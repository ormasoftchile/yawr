package serve

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

// TestFailStop_HTTP_PostRuns_FailedStepTerminatesRun exercises the real
// HTTP POST /runs → advanceRun path. A step that fails without recovery
// must result in a terminal failed state accessible via the server.
func TestFailStop_HTTP_PostRuns_FailedStepTerminatesRun(t *testing.T) {
	h := newTestServerHarness(t)

	// Configure the fake handle to return a failed step then EOF.
	h.handle.mu.Lock()
	h.handle.results = []*engine.StepResult{
		{StepID: "invoke-tool", Status: engine.StepStatusFailed, Outcome: engine.StepOutcomeFailed},
	}
	h.handle.state.Status = engine.RunStatusFailed
	h.handle.mu.Unlock()

	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	// POST /runs to start the run.
	body := strings.NewReader(`{"runbookPath":"runbook.yaml"}`)
	resp, err := http.Post(ts.URL+"/runs", "application/json", body)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 201; body=%s", resp.StatusCode, raw)
	}
	var created struct{ RunID string }
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.RunID == "" {
		t.Fatal("empty runID")
	}

	// Poll until the run entry is terminal (advanceRun runs async).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for run to reach terminal state")
		}
		entry, found := h.server.registry.Get(created.RunID)
		if found && (entry.State == engine.RunStatusFailed ||
			entry.State == engine.RunStatusCompleted ||
			entry.State == engine.RunStatusCancelled) {
			if entry.State != engine.RunStatusFailed {
				t.Errorf("expected RunStatusFailed, got %s", entry.State)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
