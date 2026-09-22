package serve

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

// previewRunbookPath returns the absolute path to the collect-health
// example runbook used as input to the format=... tests.
func previewRunbookPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "examples", "collect-health", "collect-health.yawr"))
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	return abs
}

// newPreviewTestServer wires a real parser + fake engine/planner so the
// /preview/document endpoint can build a graphdoc.Document from a real
// runbook file on disk.
func newPreviewTestServer(t *testing.T) *Server {
	t.Helper()
	p, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunID:       "run-1",
		RunbookPath: previewRunbookPath(t),
		Metadata: engine.PlanMetadata{
			RunbookID:   "collect-health",
			RunbookName: "collect-health",
			PlannedAt:   time.Now(),
		},
		Tools: make(map[string]*schema.ToolDef),
	}
	planner := &fakePlanner{plan: plan}
	handle := newFakeRunHandle("run-1")
	eng := &fakeEngine{startFunc: func(_ context.Context, _ *engine.ExecutionPlan, _ engine.RunOptions) (engine.RunHandle, error) {
		return handle, nil
	}}
	cfg := servepkg.ServerConfig{
		Engine:          eng,
		EngineConfig:    engine.EngineConfig{Store: runstore.NewDirRunStore(makeWorkDir(t))},
		Parser:          p,
		Planner:         planner,
		EventBufferSize: 4,
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func TestPreviewDocument_FormatVariants(t *testing.T) {
	ts := newHTTPTestServer(t, newPreviewTestServer(t))
	defer ts.Close()

	tests := []struct {
		format      string
		wantType    string
		mustContain []string
	}{
		{"", "application/json", []string{`"schema_version"`, `"runbook"`, `"check_loop"`}},
		{"graphjson", "application/json", []string{`"schema_version"`, `"check_loop"`}},
		{"prose", "text/markdown", []string{"# collect-health", "**Loop**", "check_loop"}},
		{"mermaid", "text/plain", []string{"flowchart TD", "check_loop", "show_result"}},
		{"asciigraph", "text/plain", []string{"collect-health", "├─", "if All checks passed"}},
	}

	rbPath := previewRunbookPath(t)
	for _, tt := range tests {
		t.Run("format="+tt.format, func(t *testing.T) {
			url := ts.URL + "/preview/document?runbookPath=" + rbPath
			if tt.format != "" {
				url += "&format=" + tt.format
			}
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d, want 200", resp.StatusCode)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, tt.wantType) {
				t.Errorf("Content-Type: got %q, want prefix %q", ct, tt.wantType)
			}
			body, _ := io.ReadAll(resp.Body)
			for _, sub := range tt.mustContain {
				if !strings.Contains(string(body), sub) {
					t.Errorf("body missing %q in:\n%s", sub, body)
				}
			}
		})
	}
}

// TestPreviewDocument_GraphJSON_CarriesDeclaredInputs is a B-1 regression
// test (barbara-client-enum-parity-gate-review.md): the SAME transport
// the web GUI's InputsForm consumes (/preview/document?format=graphjson)
// must carry the declared inputs[] DTO end-to-end through the real HTTP
// handler, not merely through the Go graphjson.Render function in
// isolation.
func TestPreviewDocument_GraphJSON_CarriesDeclaredInputs(t *testing.T) {
	dir := t.TempDir()
	rbPath := filepath.Join(dir, "enum.runbook.yaml")
	if err := writeTestFile(rbPath, `apiVersion: yawr.runbook/v1
id: enum-fixture
name: enum-fixture
inputs:
  env_name:
    type: string
    required: true
    enum: ["staging", "prod"]
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ts := newHTTPTestServer(t, newPreviewTestServer(t))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/preview/document?format=graphjson&runbookPath=" + rbPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"inputs"`) {
		t.Fatalf("/preview/document graphjson output has no \"inputs\" key at all: %s", body)
	}
	if !strings.Contains(string(body), `"enum": [\n      "staging",\n      "prod"\n    ]`) &&
		!strings.Contains(string(body), `"enum":["staging","prod"]`) {
		// graphjson.Render uses json.MarshalIndent; be tolerant of exact
		// spacing but assert the declared order survives in the raw text.
		if !(strings.Contains(string(body), `"staging"`) && strings.Contains(string(body), `"prod"`) &&
			strings.Index(string(body), `"staging"`) < strings.Index(string(body), `"prod"`)) {
			t.Fatalf("declared enum order not preserved end-to-end: %s", body)
		}
	}
}

func TestRunDocument_ETagReturnsNotModified(t *testing.T) {
	srv := newPreviewTestServer(t)
	srv.registry.Add(&RunEntry{
		ID:          "run-etag",
		RunbookPath: previewRunbookPath(t),
		State:       engine.RunStatusCompleted,
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
	})
	ts := newHTTPTestServer(t, srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/runs/run-etag/document")
	if err != nil {
		t.Fatalf("GET document: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("missing ETag on run document response")
	}
	if len(body) == 0 {
		t.Fatalf("initial document response was empty")
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/runs/run-etag/document", nil)
	req.Header.Set("If-None-Match", etag)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET document: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional status: got %d, want 304; body=%s", resp.StatusCode, body)
	}
	if len(body) != 0 {
		t.Fatalf("304 response body length: got %d, want 0", len(body))
	}
}

func TestPreviewReadEndpointsThrottleFaultyTerminalClient(t *testing.T) {
	srv := newPreviewTestServer(t)
	srv.registry.Add(&RunEntry{
		ID:          "run-hot-loop",
		RunbookPath: previewRunbookPath(t),
		State:       engine.RunStatusCompleted,
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
	})
	ts := newHTTPTestServer(t, srv)
	defer ts.Close()

	const allowedRapidReads = 60
	tooMany := 0
	noContent := 0
	ok200 := 0
	other := 0
	for i := 0; i < allowedRapidReads+20; i++ {
		resp, err := http.Get(ts.URL + "/runs/run-hot-loop/interactions")
		if err != nil {
			t.Fatalf("GET interactions: %v", err)
		}
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNoContent:
			noContent++
		case http.StatusTooManyRequests:
			tooMany++
		case http.StatusOK:
			ok200++
		default:
			other++
		}
	}
	if ok200 != 0 || noContent > allowedRapidReads || tooMany == 0 || other != 0 {
		t.Fatalf("rapid terminal reads: 204=%d 200=%d 429=%d other=%d; want <=%d 204s, zero 200s, and at least one 429", noContent, ok200, tooMany, other, allowedRapidReads)
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestPreviewDocument_MissingPathParam(t *testing.T) {
	ts := newHTTPTestServer(t, newPreviewTestServer(t))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/preview/document")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestPreviewDocument_BadRunbookPath(t *testing.T) {
	ts := newHTTPTestServer(t, newPreviewTestServer(t))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/preview/document?runbookPath=/no/such/file.yaml")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestRunDocument_NotFound(t *testing.T) {
	ts := newHTTPTestServer(t, newPreviewTestServer(t))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/runs/no-such-run/document")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}
