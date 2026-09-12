package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// These tests are regression guards for bugs we have actually shipped:
//
//   - State SSE used to be silently broken because the UI listened to
//     the default `message` event but the server emitted `event: state`.
//     TestE2E_StateSSE_EmitsNamedEvent locks the wire format so we can't
//     drift either side again.
//
//   - DELETE /runs/{id} was added when we wired up cancel from the UI.
//     TestE2E_RunsDelete covers happy path + 404.
//
//   - Runbook-level `vars:` and `inputs:` defaults were never seeded into
//     engine vars, so any template referencing them failed with
//     "map has no entry for key X". TestE2E_RunsCreate_SeedsRunbookVars
//     captures the engine.RunOptions handed to engine.Start and asserts
//     the merged map.

// TestE2E_StateSSE_EmitsNamedEvent locks the SSE wire format: every
// state frame must use `event: state`. The browser client uses
// addEventListener('state', …) and silently drops anything else.
func TestE2E_StateSSE_EmitsNamedEvent(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{
		ID:          "run-1",
		Handle:      h.handle,
		State:       engine.RunStatusRunning,
		RunbookPath: previewRunbookPath(t),
	})
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/runs/run-1/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	frame, err := readRawSSEFrame(br, 3*time.Second)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if frame.event != "state" {
		t.Fatalf("event name: got %q, want %q (UI listens for the named event; "+
			"unnamed frames are silently dropped)", frame.event, "state")
	}
	if !strings.Contains(frame.data, `"run_id"`) {
		t.Fatalf("data missing run_id: %q", frame.data)
	}
}

// TestE2E_RunsDelete_HappyPath verifies DELETE /runs/{id} cancels an
// active run and returns 204.
func TestE2E_RunsDelete_HappyPath(t *testing.T) {
	h := newTestServerHarness(t)

	cancelled := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	h.server.registry.Add(&RunEntry{
		ID:     "run-1",
		Handle: h.handle,
		State:  engine.RunStatusRunning,
		Cancel: func() {
			cancel()
			select {
			case cancelled <- struct{}{}:
			default:
			}
		},
	})
	_ = ctx
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/runs/run-1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel func not invoked")
	}
}

// TestE2E_RunsDelete_NotFound returns 404 for an unknown run id.
func TestE2E_RunsDelete_StopsPreviewInteractionStream(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{
		ID:          "run-cancel-preview",
		Handle:      h.handle,
		State:       engine.RunStatusRunning,
		RunbookPath: previewRunbookPath(t),
		Cancel:      func() {},
	})
	h.server.broker.Register("run-cancel-preview")
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/runs/run-cancel-preview", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status: got %d, want 204", resp.StatusCode)
	}

	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err = client.Get(ts.URL + "/runs/run-cancel-preview/interactions")
	if err != nil {
		t.Fatalf("GET interactions after cancel did not terminate cleanly: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("interactions after cancel: got %d, want 204; body=%s", resp.StatusCode, raw)
	}
}

func TestE2E_RunsDelete_NotFound(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/runs/no-such", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestE2E_RunsCreate_SeedsRunbookVars asserts that POST /runs merges
// runbook-level `vars:` and `inputs:` defaults into the engine.RunOptions
// before calling engine.Start, with caller-supplied vars winning over
// runbook defaults. Caught the "map has no entry for key X" regression.
func TestE2E_RunsCreate_SeedsRunbookVars(t *testing.T) {
	h := newTestServerHarness(t)

	// Configure the parser to return a runbook with vars + inputs.
	h.parser.result = &parser.ParsedRunbook{
		Source: "test",
		Runbook: &schema.Runbook{
			Vars: map[string]any{
				"service_url": "https://default.example.com",
				"max_retries": 3,
			},
			Inputs: map[string]*schema.Input{
				"region":      {Type: "string", Default: "us-east-1"},
				"environment": {Type: "string", Required: false}, // no default → ""
				"caller_var":  {Type: "string", Default: "should_be_overridden"},
				"required":    {Type: "string", Required: true}, // not seeded
			},
		},
	}

	// Capture RunOptions on Start.
	var (
		mu      sync.Mutex
		gotOpts engine.RunOptions
		startCh = make(chan struct{}, 1)
	)
	h.engine.startFunc = func(_ context.Context, _ *engine.ExecutionPlan, opts engine.RunOptions) (engine.RunHandle, error) {
		mu.Lock()
		gotOpts = opts
		mu.Unlock()
		select {
		case startCh <- struct{}{}:
		default:
		}
		return h.handle, nil
	}

	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	// Caller supplies caller_var — should win over runbook default.
	// The HTTP API field is `inputs` (not `vars`); the server normalizes
	// it to a string map and merges runbook defaults underneath.
	body := strings.NewReader(`{"runbookPath":"runbook.yaml","inputs":{"caller_var":"from_caller"}}`)
	resp, err := http.Post(ts.URL+"/runs", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 201; body=%s", resp.StatusCode, raw)
	}

	select {
	case <-startCh:
	case <-time.After(2 * time.Second):
		t.Fatal("engine.Start was not called")
	}

	mu.Lock()
	vars := gotOpts.Vars
	mu.Unlock()

	checks := []struct{ key, want string }{
		{"service_url", "https://default.example.com"}, // from runbook vars
		{"max_retries", "3"},                           // non-string runbook var, stringified
		{"region", "us-east-1"},                        // input default
		{"environment", ""},                            // optional input, no default
		{"caller_var", "from_caller"},                  // caller wins
	}
	for _, c := range checks {
		if got := vars[c.key]; got != c.want {
			t.Errorf("vars[%q]: got %q, want %q", c.key, got, c.want)
		}
	}
	if _, ok := vars["required"]; ok {
		t.Errorf("vars[%q]: required-with-no-default should not be seeded", "required")
	}
}

// --- helpers ---

// TestE2E_CollectorAcceptsTypedValues verifies that the broker accepts
// a collector answer whose fields are typed values (number, integer,
// boolean, multi-select array) and surfaces them to PromptForm.Values.
// Locks the wire so the UI's type-aware coercion stays compatible.
func TestE2E_CollectorAcceptsTypedValues(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	body := strings.NewReader(`{"runbookPath":"runbook.yaml"}`)
	resp, err := http.Post(ts.URL+"/runs", "application/json", body)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	var created struct{ RunID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)

	// Match the spec's field-type inventory.
	formCh := h.startCollectorPrompt(t, created.RunID, input.FormRequest{
		StepID: "rich_form",
		Prompt: "All field types",
		Fields: []input.FormField{
			{Name: "title", Type: "text", Required: true},
			{Name: "count", Type: "number"},
			{Name: "retries", Type: "integer"},
			{Name: "active", Type: "boolean"},
			{Name: "tier", Type: "select", Options: []input.Option{{Label: "A", Value: "a"}, {Label: "B", Value: "b"}}},
			{Name: "tags", Type: "select", Multiple: true, Options: []input.Option{{Label: "x", Value: "x"}, {Label: "y", Value: "y"}}},
			{Name: "secret", Type: "secret"},
		},
	})

	sseReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/runs/"+created.RunID+"/interactions", nil)
	sseResp, _ := http.DefaultClient.Do(sseReq)
	defer sseResp.Body.Close()
	br := bufio.NewReader(sseResp.Body)
	frame, err := readInteractionFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	turnID, _ := frame["turnID"].(string)
	if turnID == "" {
		t.Fatal("missing turnID")
	}

	// Verify the field types are echoed on the wire.
	fields, _ := frame["fields"].([]any)
	if len(fields) != 7 {
		t.Fatalf("fields: got %d, want 7", len(fields))
	}

	fieldByDisplay := map[string]map[string]any{}
	for _, raw := range fields {
		f := raw.(map[string]any)
		fieldByDisplay[f["display_name"].(string)] = f
	}
	optionToken := func(field map[string]any, index int) string {
		return field["options"].([]any)[index].(map[string]any)["value"].(string)
	}
	answerBody := map[string]any{"kind": "collector", "values": map[string]any{
		fieldByDisplay["title"]["name"].(string):   "hello",
		fieldByDisplay["count"]["name"].(string):   3.14,
		fieldByDisplay["retries"]["name"].(string): 5,
		fieldByDisplay["active"]["name"].(string):  true,
		fieldByDisplay["tier"]["name"].(string):    optionToken(fieldByDisplay["tier"], 0),
		fieldByDisplay["tags"]["name"].(string):    []any{optionToken(fieldByDisplay["tags"], 0), optionToken(fieldByDisplay["tags"], 1)},
		fieldByDisplay["secret"]["name"].(string):  "shhh",
	}}
	answerBytes, _ := json.Marshal(answerBody)
	answerResp, err := http.Post(ts.URL+"/runs/"+created.RunID+"/interactions/"+turnID, "application/json", bytes.NewReader(answerBytes))
	if err != nil {
		t.Fatalf("POST answer: %v", err)
	}
	defer answerResp.Body.Close()
	if answerResp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(answerResp.Body)
		t.Fatalf("answer status: got %d, want 204; body=%s", answerResp.StatusCode, raw)
	}

	select {
	case got := <-formCh:
		if got.err != nil {
			t.Fatalf("PromptForm err: %v", got.err)
		}
		v := got.resp.Values
		if v["title"] != "hello" {
			t.Errorf("title: %#v", v["title"])
		}
		if n, ok := v["count"].(float64); !ok || n != 3.14 {
			t.Errorf("count: %#v", v["count"])
		}
		if n, ok := v["retries"].(float64); !ok || n != 5 {
			// JSON numbers come back as float64 even for integers.
			t.Errorf("retries: %#v", v["retries"])
		}
		if b, ok := v["active"].(bool); !ok || !b {
			t.Errorf("active: %#v", v["active"])
		}
		if v["tier"] != "a" {
			t.Errorf("tier: %#v", v["tier"])
		}
		if arr, ok := v["tags"].([]any); !ok || len(arr) != 2 || arr[0] != "x" || arr[1] != "y" {
			t.Errorf("tags: %#v", v["tags"])
		}
		if v["secret"] != "shhh" {
			t.Errorf("secret: %#v", v["secret"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PromptForm did not return")
	}
}

type sseFrame struct {
	event string
	data  string
}

// readRawSSEFrame reads one full SSE frame and returns its event name
// and joined data lines. Unlike readStateFrame in runstate_sse_test.go
// this preserves the event name so we can assert on the wire format.
func readRawSSEFrame(br *bufio.Reader, timeout time.Duration) (sseFrame, error) {
	var (
		frame    sseFrame
		dataBuf  bytes.Buffer
		deadline = time.Now().Add(timeout)
	)
	for {
		if time.Now().After(deadline) {
			return sseFrame{}, io.EOF
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return sseFrame{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if dataBuf.Len() > 0 || frame.event != "" {
				frame.data = dataBuf.String()
				return frame, nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

// silence unused import warnings if json/encoding gets pruned later.
var _ = json.Marshal
