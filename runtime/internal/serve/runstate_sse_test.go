package serve

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

// TestRunState_SSE_StreamsSnapshots verifies that GET /runs/{id}/state
// emits a runstate snapshot as the initial frame and one further
// snapshot per engine.Event broadcast on the bridge. The test asserts
// that the per-node status transitions Pending → Running → Completed.
func TestRunState_SSE_StreamsSnapshots(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{
		ID:          "run-1",
		Handle:      h.handle,
		State:       engine.RunStatusRunning,
		RunbookPath: previewRunbookPath(t),
	})
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	// Open the SSE stream.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/runs/run-1/state", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)

	// Frame 1: initial snapshot — Status=pending, no nodes touched.
	first := readStateFrame(t, br)
	if first["status"] != "pending" && first["status"] != "running" {
		t.Errorf("initial status: got %v, want pending|running", first["status"])
	}

	// Allow the handler goroutine to register its subscriber.
	if err := h.server.bridge.WaitForSubscriber(t.Context(), 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}

	// Broadcast step/started.
	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/started",
		RunID:    "run-1",
		Sequence: 1,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{"step_id": "check_loop"},
	})
	frame := readStateFrame(t, br)
	if got := nodeStatus(frame, "check_loop"); got != "running" {
		t.Errorf("after step/started: got %q, want running", got)
	}

	// Broadcast step/completed with duration.
	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/completed",
		RunID:    "run-1",
		Sequence: 2,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{"step_id": "check_loop", "duration_ms": int64(123)},
	})
	frame = readStateFrame(t, br)
	if got := nodeStatus(frame, "check_loop"); got != "completed" {
		t.Errorf("after step/completed: got %q, want completed", got)
	}
}

func TestRunStatePreviewCapturesAggregateBoundedInEventsAndState(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{ID: "run-1", Handle: h.handle, State: engine.RunStatusRunning, RunbookPath: previewRunbookPath(t)})
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	eventsResp, err := http.Get(ts.URL + "/events?runID=run-1")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer eventsResp.Body.Close()
	stateResp, err := http.Get(ts.URL + "/runs/run-1/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer stateResp.Body.Close()
	stateBR := bufio.NewReader(stateResp.Body)
	_ = readStateFrame(t, stateBR)
	if err := h.server.bridge.WaitForSubscriber(t.Context(), 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	captures := make(map[string]any, 200)
	for i := 0; i < 200; i++ {
		captures[fmt.Sprintf("cap_%03d", i)] = strings.Repeat("x", 3900)
	}
	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type: "step/completed", RunID: "run-1", Sequence: 1, TS: time.Now().Format(time.RFC3339Nano),
		Payload: map[string]any{"step_id": "query", "captures": captures},
	})

	eventFrame := waitForStepCompletedEventFrame(t, eventsResp.Body, "run-1")
	if got := len(eventFrame.data); got > 30000 {
		t.Fatalf("/events capture payload is unbounded: got %d", got)
	}
	if !strings.Contains(eventFrame.data, "_preview_captures_truncated") || !strings.Contains(eventFrame.data, "cap_000") {
		t.Fatalf("/events missing deterministic subset/truncation marker: %s", eventFrame.data)
	}
	if strings.Contains(eventFrame.data, "cap_199") {
		t.Fatalf("/events retained late key despite aggregate budget")
	}
	var event struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(eventFrame.data), &event); err != nil {
		t.Fatalf("decode event frame: %v", err)
	}
	eventCaps := event.Payload["captures"].(map[string]any)
	eventOmitted, _ := eventCaps["_preview_captures_omitted"].(float64)
	if eventOmitted <= 0 {
		t.Fatalf("/events omission count was lost through repeated projection: %#v", eventCaps)
	}

	stateFrame := readStateFrame(t, stateBR)
	body, _ := json.Marshal(stateFrame)
	if len(body) > 30000 {
		t.Fatalf("/state capture payload is unbounded: got %d", len(body))
	}
	vars := stateFrame["vars"].(map[string]any)
	if vars["_preview_captures_truncated"] != true || vars["cap_000"] == nil {
		t.Fatalf("/state missing deterministic subset/truncation marker: %#v", vars)
	}
	if vars["_preview_captures_omitted"] != eventOmitted {
		t.Fatalf("omission count changed between /events and /state: event=%#v state=%#v", eventOmitted, vars["_preview_captures_omitted"])
	}
	if _, ok := vars["cap_199"]; ok {
		t.Fatalf("/state retained late key despite aggregate budget")
	}
}

func TestRunStatePreviewInitialVarsBoundedInEventsAndState(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{ID: "run-1", Handle: h.handle, State: engine.RunStatusRunning, RunbookPath: previewRunbookPath(t)})
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	eventsResp, err := http.Get(ts.URL + "/events?runID=run-1")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer eventsResp.Body.Close()
	stateResp, err := http.Get(ts.URL + "/runs/run-1/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer stateResp.Body.Close()
	stateBR := bufio.NewReader(stateResp.Body)
	_ = readStateFrame(t, stateBR)
	if err := h.server.bridge.WaitForSubscriber(t.Context(), 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	vars := make(map[string]any, 200)
	for i := 0; i < 200; i++ {
		vars[fmt.Sprintf("input_%03d", i)] = strings.Repeat("x", 3900)
	}
	h.server.bridge.Broadcast(servepkg.RunEvent{Type: "run/started", RunID: "run-1", Sequence: 1, TS: time.Now().Format(time.RFC3339Nano), Payload: map[string]any{"vars": vars}})

	eventFrame := waitForEventFrame(t, eventsResp.Body, "run/started", "run-1")
	if got := len(eventFrame.data); got > 30000 {
		t.Fatalf("/events initial vars payload is unbounded: got %d", got)
	}
	if !strings.Contains(eventFrame.data, "_preview_captures_truncated") || !strings.Contains(eventFrame.data, "input_000") || strings.Contains(eventFrame.data, "input_199") {
		t.Fatalf("/events did not retain deterministic bounded vars with marker: %s", eventFrame.data)
	}
	stateFrame := readStateFrame(t, stateBR)
	body, _ := json.Marshal(stateFrame)
	if len(body) > 30000 {
		t.Fatalf("/state initial vars payload is unbounded: got %d", len(body))
	}
	varsOut := stateFrame["vars"].(map[string]any)
	if varsOut["_preview_captures_truncated"] != true || varsOut["input_000"] == nil {
		t.Fatalf("/state initial vars missing bounded marker/subset: %#v", varsOut)
	}
	if _, ok := varsOut["input_199"]; ok {
		t.Fatalf("/state retained late initial var despite aggregate budget")
	}
}

func TestRunStatePreviewLineAndErrorBoundedInEventsAndState(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{ID: "run-1", Handle: h.handle, State: engine.RunStatusRunning, RunbookPath: previewRunbookPath(t)})
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	eventsResp, err := http.Get(ts.URL + "/events?runID=run-1")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer eventsResp.Body.Close()
	stateResp, err := http.Get(ts.URL + "/runs/run-1/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer stateResp.Body.Close()
	stateBR := bufio.NewReader(stateResp.Body)
	_ = readStateFrame(t, stateBR)
	if err := h.server.bridge.WaitForSubscriber(t.Context(), 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	line := strings.Repeat("{\"success\":true,\"rows\":[", 2000)
	errorText := strings.Repeat("native failed ", 2000)
	h.server.bridge.Broadcast(servepkg.RunEvent{Type: "step/output", RunID: "run-1", Sequence: 1, TS: time.Now().Format(time.RFC3339Nano), Payload: map[string]any{"step_id": "query", "line": line}})
	lineFrame := waitForEventFrame(t, eventsResp.Body, "step/output", "run-1")
	if got := len(lineFrame.data); got > 12000 || !strings.Contains(lineFrame.data, "line_preview_truncated") {
		t.Fatalf("/events step/output line not bounded/marked: len=%d data=%s", got, lineFrame.data)
	}
	h.server.bridge.Broadcast(servepkg.RunEvent{Type: "step/failed", RunID: "run-1", Sequence: 2, TS: time.Now().Format(time.RFC3339Nano), Payload: map[string]any{"step_id": "query", "error": errorText}})
	errorFrame := waitForEventFrame(t, eventsResp.Body, "step/failed", "run-1")
	if got := len(errorFrame.data); got > 12000 || !strings.Contains(errorFrame.data, "error_preview_truncated") {
		t.Fatalf("/events step/failed error not bounded/marked: len=%d data=%s", got, errorFrame.data)
	}
	_ = readStateFrame(t, stateBR)
	stateFrame := readStateFrame(t, stateBR)
	body, _ := json.Marshal(stateFrame)
	if len(body) > 12000 {
		t.Fatalf("/state failed error payload is unbounded: got %d", len(body))
	}
	nodes := stateFrame["nodes"].(map[string]any)
	query := nodes["query"].(map[string]any)
	if len(query["error"].(string)) > 4096 {
		t.Fatalf("/state node error not bounded")
	}
}

func TestRunStatePreviewEvidenceKeepsStructuredRecordsAndBoundsLargeValues(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{ID: "run-1", Handle: h.handle, State: engine.RunStatusRunning, RunbookPath: previewRunbookPath(t)})
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	eventsResp, err := http.Get(ts.URL + "/events?runID=run-1")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer eventsResp.Body.Close()
	if err := h.server.bridge.WaitForSubscriber(t.Context(), 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}
	small := []evidence.EvidenceRecord{{Name: "observation", Kind: evidence.EvidenceKindText, Value: "ok", CapturedAt: time.Date(2026, 8, 21, 1, 2, 3, 0, time.UTC)}}
	h.server.bridge.Broadcast(toRunEvent(engine.Event{Kind: "step/completed", RunID: "run-1", Sequence: 1, Timestamp: time.Now().Format(time.RFC3339Nano), Payload: map[string]any{"step_id": "query", "evidence": small}}))
	frame := waitForEventFrame(t, eventsResp.Body, "step/completed", "run-1")
	var ev struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(frame.data), &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	records, ok := ev.Payload["evidence"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("evidence lost array shape: %#v", ev.Payload["evidence"])
	}
	record := records[0].(map[string]any)
	if record["name"] != "observation" || record["kind"] != "text" || record["value"] != "ok" {
		t.Fatalf("evidence record lost structured fields: %#v", record)
	}
	large := make([]evidence.EvidenceRecord, 200)
	for i := range large {
		large[i] = evidence.EvidenceRecord{Name: fmt.Sprintf("record-%03d", i), Kind: evidence.EvidenceKindText, Value: strings.Repeat("v", 1000), CapturedAt: time.Now()}
	}
	h.server.bridge.Broadcast(toRunEvent(engine.Event{Kind: "step/completed", RunID: "run-1", Sequence: 2, Timestamp: time.Now().Format(time.RFC3339Nano), Payload: map[string]any{"step_id": "query", "evidence": large}}))
	largeFrame := waitForEventFrame(t, eventsResp.Body, "step/completed", "run-1")
	if len(largeFrame.data) > 12000 || !strings.Contains(largeFrame.data, "evidence_preview_truncated") || !strings.Contains(largeFrame.data, "_preview_omitted") {
		t.Fatalf("large evidence not bounded/marked: len=%d data=%s", len(largeFrame.data), largeFrame.data)
	}
}

func TestRunStatePreviewGenericArrayOmissionCountSurvivesToRunEventAndBroadcast(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{ID: "run-1", Handle: h.handle, State: engine.RunStatusRunning, RunbookPath: previewRunbookPath(t)})
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	eventsResp, err := http.Get(ts.URL + "/events?runID=run-1")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer eventsResp.Body.Close()
	if err := h.server.bridge.WaitForSubscriber(t.Context(), 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}
	items := make([]any, 200)
	for i := range items {
		items[i] = map[string]any{"name": fmt.Sprintf("item-%03d", i), "value": strings.Repeat("x", 1000)}
	}
	h.server.bridge.Broadcast(toRunEvent(engine.Event{Kind: "step/completed", RunID: "run-1", Sequence: 1, Timestamp: time.Now().Format(time.RFC3339Nano), Payload: map[string]any{"step_id": "query", "future_array": items}}))
	frame := waitForEventFrame(t, eventsResp.Body, "step/completed", "run-1")
	var ev struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(frame.data), &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	projected := ev.Payload["future_array"].([]any)
	sentinel := projected[len(projected)-1].(map[string]any)
	omitted, _ := sentinel["_preview_omitted"].(float64)
	if sentinel["_preview_truncated"] != true || omitted <= 0 {
		t.Fatalf("generic array omission count was lost through repeated projection: %#v", projected)
	}
}

func TestRunStatePreviewVarsOmissionCountMergesInitialAndCaptures(t *testing.T) {
	s := runstate.New()
	initial := make(map[string]any, 200)
	captures := make(map[string]any, 200)
	for i := 0; i < 200; i++ {
		initial[fmt.Sprintf("input_%03d", i)] = strings.Repeat("i", 3900)
		captures[fmt.Sprintf("cap_%03d", i)] = strings.Repeat("c", 3900)
	}
	s.Apply(engine.Event{Kind: "run/started", RunID: "run-1", RunbookID: "rb-1", Sequence: 1, Payload: map[string]any{"vars": initial}})
	firstOmitted, _ := s.Snapshot().Vars["_preview_captures_omitted"].(int)
	if firstOmitted <= 0 {
		t.Fatalf("expected initial omission count, got %#v", s.Snapshot().Vars)
	}
	s.Apply(engine.Event{Kind: "step/completed", RunID: "run-1", RunbookID: "rb-1", Sequence: 2, Payload: map[string]any{"step_id": "query", "captures": captures}})
	snap := s.Snapshot()
	combinedOmitted, _ := snap.Vars["_preview_captures_omitted"].(int)
	if combinedOmitted <= firstOmitted {
		t.Fatalf("combined omission count did not include initial+captures: first=%d vars=%#v", firstOmitted, snap.Vars)
	}
	body, _ := json.Marshal(snap)
	if len(body) > 30000 || snap.Vars["_preview_captures_truncated"] != true || snap.Vars["input_000"] == nil || snap.Vars["cap_000"] == nil {
		t.Fatalf("merged vars not bounded with truthful marker/subset: len=%d vars=%#v", len(body), snap.Vars)
	}
}

func TestRunStatePreviewInteractionPreservedAndBounded(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.registry.Add(&RunEntry{ID: "run-1", Handle: h.handle, State: engine.RunStatusRunning, RunbookPath: previewRunbookPath(t)})
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/runs/run-1/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	_ = readStateFrame(t, br)
	if err := h.server.bridge.WaitForSubscriber(t.Context(), 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}

	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type: "step/completed", RunID: "run-1", Sequence: 1, TS: time.Now().Format(time.RFC3339Nano),
		Payload: map[string]any{"step_id": "collect", "output": map[string]any{"interaction": map[string]any{
			"kind": "collector", "prompt": strings.Repeat("prompt", 2000), "answer": map[string]any{"field": strings.Repeat("answer", 2000)},
		}}},
	})
	frame := readStateFrame(t, br)
	nodes := frame["nodes"].(map[string]any)
	collect := nodes["collect"].(map[string]any)
	interaction := collect["interaction"].(map[string]any)
	if interaction["kind"] != "collector" || interaction["preview_truncated"] != true {
		t.Fatalf("interaction missing or not marked truncated: %#v", interaction)
	}
	body, _ := json.Marshal(frame)
	if len(body) > 30000 {
		t.Fatalf("/state interaction payload is unbounded: got %d", len(body))
	}
}

// TestRunState_NotFound returns 404 for unknown run IDs.
func TestRunState_NotFound(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/runs/no-such/state")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// readStateFrame parses one SSE frame from a /runs/{id}/state stream
// and returns the JSON payload as a generic map.
func readStateFrame(t *testing.T, br *bufio.Reader) map[string]any {
	t.Helper()
	var dataLines []string
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for SSE frame")
		}
		line, err := br.ReadString('\n')
		if err == io.EOF {
			t.Fatalf("EOF before complete frame")
		}
		if err != nil {
			t.Fatalf("ReadString: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(dataLines) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
		// Ignore event:, id: and comment lines.
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &out); err != nil {
		t.Fatalf("unmarshal frame: %v\nraw: %q", err, dataLines)
	}
	return out
}

func waitForEventFrame(t *testing.T, body io.Reader, eventType, runID string) sseFrame {
	t.Helper()
	br := bufio.NewReader(body)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := readRawSSEFrame(br, 10*time.Second)
		if err != nil {
			t.Fatalf("read /events frame: %v", err)
		}
		if frame.event != eventType {
			continue
		}
		var ev struct {
			RunID string `json:"runID"`
		}
		if err := json.Unmarshal([]byte(frame.data), &ev); err != nil {
			t.Fatalf("decode /events runID: %v\n%s", err, frame.data)
		}
		if ev.RunID == runID {
			return frame
		}
	}
	t.Fatalf("timed out waiting for /events %s frame", eventType)
	return sseFrame{}
}

// nodeStatus extracts state.nodes[id].status from a parsed state frame.
func nodeStatus(frame map[string]any, id string) string {
	nodes, _ := frame["nodes"].(map[string]any)
	if nodes == nil {
		return ""
	}
	n, _ := nodes[id].(map[string]any)
	if n == nil {
		return ""
	}
	s, _ := n["status"].(string)
	return s
}
