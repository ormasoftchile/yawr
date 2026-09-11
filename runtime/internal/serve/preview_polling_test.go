package serve

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestPreviewClient_NoRecommendationTerminalBoundsReadTraffic(t *testing.T) {
	fixedDoc, fixedInteractions := runNoRecommendationPreviewClient(t, false, 200*time.Millisecond)
	if fixedDoc > 1 || fixedInteractions > 1 {
		t.Fatalf("fixed no-recommendation terminal polling: document=%d interactions=%d, want <=1 each", fixedDoc, fixedInteractions)
	}

	unboundedDoc, unboundedInteractions := runNoRecommendationPreviewClient(t, true, 200*time.Millisecond)
	if unboundedDoc <= 2 || unboundedInteractions <= 2 {
		t.Fatalf("test harness is not sensitive to repeated polling: document=%d interactions=%d, want both >2", unboundedDoc, unboundedInteractions)
	}
}

func runNoRecommendationPreviewClient(t *testing.T, unbounded bool, window time.Duration) (int64, int64) {
	t.Helper()
	srv := newPreviewTestServer(t)
	runID := "run-no-recommendation"
	srv.registry.Add(&RunEntry{
		ID:          runID,
		RunbookPath: previewRunbookPath(t),
		State:       engine.RunStatusCompleted,
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
	})

	var docRequests atomic.Int64
	var interactionRequests atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/document") {
			docRequests.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/interactions") {
			interactionRequests.Add(1)
		}
		srv.handler.ServeHTTP(w, r)
	}))
	defer ts.Close()

	client := &deterministicPreviewClient{
		baseURL:   ts.URL,
		runID:     runID,
		client:    &http.Client{Timeout: 500 * time.Millisecond},
		unbounded: unbounded,
	}
	client.loadDoc(t, true)
	// Count only the post-terminal observation window requested by the report.
	docRequests.Store(0)
	interactionRequests.Store(0)

	client.observeNoRecommendationTerminal()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		client.frontierEffect(t)
		client.pollTick(t)
		client.interactionsEffect(t)
		time.Sleep(5 * time.Millisecond)
	}
	return docRequests.Load(), interactionRequests.Load()
}

type deterministicPreviewClient struct {
	baseURL   string
	runID     string
	client    *http.Client
	unbounded bool
	terminal  bool
	etag      string
	knownIDs  map[string]bool
	missing   string
}

func (c *deterministicPreviewClient) loadDoc(t *testing.T, manual bool) {
	t.Helper()
	if !manual && c.terminal && !c.unbounded {
		return
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/runs/"+c.runID+"/document", nil)
	if err != nil {
		t.Fatalf("new document request: %v", err)
	}
	if c.etag != "" {
		req.Header.Set("If-None-Match", c.etag)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("GET document: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return
	}
	if resp.StatusCode != http.StatusOK {
		if c.unbounded {
			return
		}
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET document status: got %d; body=%s", resp.StatusCode, body)
	}
	c.etag = resp.Header.Get("ETag")
	var doc struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	c.knownIDs = make(map[string]bool, len(doc.Nodes))
	for _, n := range doc.Nodes {
		c.knownIDs[n.ID] = true
	}
}

func (c *deterministicPreviewClient) observeNoRecommendationTerminal() {
	c.terminal = true
	c.missing = "no_recommendation_terminal"
}

func (c *deterministicPreviewClient) frontierEffect(t *testing.T) {
	t.Helper()
	if c.missing == "" || c.knownIDs[c.missing] {
		return
	}
	if c.terminal && !c.unbounded {
		return
	}
	c.loadDoc(t, false)
}

func (c *deterministicPreviewClient) pollTick(t *testing.T) {
	t.Helper()
	if c.terminal && !c.unbounded {
		return
	}
	c.loadDoc(t, false)
}

func (c *deterministicPreviewClient) interactionsEffect(t *testing.T) {
	t.Helper()
	if c.terminal && !c.unbounded {
		return
	}
	resp, err := c.client.Get(c.baseURL + "/runs/" + c.runID + "/interactions")
	if err != nil {
		t.Fatalf("GET interactions: %v", err)
	}
	resp.Body.Close()
}
