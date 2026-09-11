package runstate_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

func jsonSizeForRunstateTest(value any) int {
	b, err := json.Marshal(value)
	if err != nil {
		return len(strings.TrimSpace(strings.ReplaceAll(err.Error(), "\n", " ")))
	}
	return len(b)
}

func ev(kind string, seq int64, ts string, payload map[string]any) engine.Event {
	return engine.Event{
		Kind:      kind,
		Sequence:  seq,
		Timestamp: ts,
		RunID:     "run-1",
		RunbookID: "rb-1",
		Payload:   payload,
	}
}

func TestApply_HappyPath(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("run/started", 1, "2026-05-03T12:00:00Z", nil))
	s.Apply(ev("step/started", 2, "2026-05-03T12:00:01Z", map[string]any{"step_id": "a"}))
	s.Apply(ev("step/completed", 3, "2026-05-03T12:00:02Z", map[string]any{
		"step_id":     "a",
		"duration_ms": int64(1000),
	}))
	s.Apply(ev("step/started", 4, "2026-05-03T12:00:02Z", map[string]any{"step_id": "b"}))
	s.Apply(ev("step/failed", 5, "2026-05-03T12:00:03Z", map[string]any{
		"step_id":     "b",
		"error":       "boom",
		"duration_ms": int64(500),
	}))
	s.Apply(ev("run/failed", 6, "2026-05-03T12:00:04Z", nil))

	if s.Status != runstate.RunFailed {
		t.Errorf("run status: got %q want failed", s.Status)
	}
	if got := s.Get("a").Status; got != runstate.StatusCompleted {
		t.Errorf("a status: got %q want completed", got)
	}
	if got := s.Get("a").DurationMs; got != 1000 {
		t.Errorf("a duration: got %d", got)
	}
	if got := s.Get("a").Attempt; got != 1 {
		t.Errorf("a attempt: got %d want 1", got)
	}
	bn := s.Get("b")
	if bn.Status != runstate.StatusFailed || bn.Error != "boom" {
		t.Errorf("b unexpected: %+v", bn)
	}
	if s.CurrentNodeID != "" {
		t.Errorf("CurrentNodeID after failure: got %q want empty", s.CurrentNodeID)
	}
	if s.Sequence != 6 {
		t.Errorf("sequence: got %d want 6", s.Sequence)
	}
}

func TestApply_PendingDefault(t *testing.T) {
	s := runstate.New()
	if got := s.Get("nope").Status; got != runstate.StatusPending {
		t.Errorf("unknown node: got %q want pending", got)
	}
}

func TestApply_StepFailedStoresOutputForPreviewDiagnostics(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/failed", 1, "", map[string]any{
		"step_id": "query",
		"error":   "native tool queryer action query: result: stdout must be exactly one JSON object",
		"output":  map[string]any{"stdout": "not-json", "stderr": "diagnostic", "exit_code": 0},
	}))

	n := s.Get("query")
	if n.Output == nil || n.Output["stdout_excerpt"] != "not-json" {
		t.Fatalf("failed step output diagnostics excerpt not retained: %#v", n.Output)
	}
	if _, exists := n.Output["stdout"]; exists {
		t.Fatalf("failed preview output carried full stdout: %#v", n.Output)
	}
	if n.Error == "" {
		t.Fatal("expected failed step error")
	}
}

func TestApply_StepCompletedStoresOutputForPreview(t *testing.T) {
	s := runstate.New()
	output := map[string]any{
		"success":   true,
		"row_count": int64(1),
		"columns":   []any{"Column1"},
		"rows":      []any{map[string]any{"Column1": int64(1)}},
		"stdout":    `{"success":true,"rowCount":1,"columns":["Column1"],"data":[{"Column1":1}]}`,
	}
	s.Apply(ev("step/completed", 1, "", map[string]any{
		"step_id": "query",
		"output":  output,
	}))

	n := s.Get("query")
	if n.Output == nil {
		t.Fatal("expected node output for preview, got nil")
	}
	if got := n.Output["row_count"]; got != int64(1) {
		t.Fatalf("row_count: got %#v want 1", got)
	}
	if _, exists := n.Output["stdout"]; exists {
		t.Fatalf("preview output carried full stdout: %#v", n.Output)
	}
	if got := n.Output["stdout_excerpt"]; got != output["stdout"] {
		t.Fatalf("stdout excerpt: got %#v want %#v", got, output["stdout"])
	}
}

func TestApply_BranchArmIndexPreservedForPreviewRouting(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/completed", 1, "", map[string]any{
		"step_id": "choose",
		"output":  map[string]any{"matched_arm": "duplicate label", "matched_arm_index": 2},
	}))

	output := s.Get("choose").Output
	if output["matched_arm_index"] != int64(2) {
		t.Fatalf("matched_arm_index not preserved: %#v", output)
	}
}

func TestApply_QueryResultPreviewOutputIsBoundedBeforeSerialization(t *testing.T) {
	columns := make([]any, 96)
	rows := make([]any, 300)
	for i := range columns {
		columns[i] = "Column" + strings.Repeat("X", 160) + string(rune('A'+i%26))
	}
	for i := range rows {
		row := make(map[string]any, len(columns))
		for _, col := range columns {
			row[col.(string)] = strings.Repeat("cell", 200)
		}
		rows[i] = row
	}
	s := runstate.New()
	s.Apply(ev("step/completed", 1, "", map[string]any{
		"step_id": "query",
		"output": map[string]any{
			"success":   true,
			"row_count": int64(300),
			"columns":   columns,
			"rows":      rows,
			"stdout":    strings.Repeat("stdout", 20000),
		},
	}))

	n := s.Get("query")
	if _, exists := n.Output["stdout"]; exists {
		t.Fatalf("preview output carried full stdout")
	}
	if n.Output["preview_truncated"] != true || n.Output["stdout_truncated"] != true {
		t.Fatalf("expected truncation metadata: %#v", n.Output)
	}
	if got := len(n.Output["stdout_excerpt"].(string)); got > 4096 {
		t.Fatalf("stdout excerpt too large: got %d", got)
	}
	body, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 30000 {
		t.Fatalf("preview state JSON is unbounded: got %d bytes, want <= 30000", len(body))
	}
}

func TestApply_CapturesAreBoundedForPreview(t *testing.T) {
	rows := make([]any, 300)
	for i := range rows {
		rows[i] = map[string]any{"Column1": strings.Repeat("row", 500)}
	}
	s := runstate.New()
	s.Apply(ev("step/completed", 1, "", map[string]any{
		"step_id": "query",
		"captures": map[string]any{
			"saved_count": int64(300),
			"saved_rows":  rows,
		},
	}))

	if got := s.Snapshot().Vars["saved_count"]; got != int64(300) {
		t.Fatalf("small capture not preserved: %#v", got)
	}
	if s.Snapshot().Vars["saved_rows_preview_truncated"] != true {
		t.Fatalf("expected capture truncation marker: %#v", s.Snapshot().Vars)
	}
	if jsonSizeForRunstateTest(s.Snapshot().Vars["saved_rows"]) > 4096 {
		t.Fatalf("captured rows exceed preview budget")
	}
	body, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 12000 {
		t.Fatalf("preview state JSON with captures is unbounded: got %d bytes", len(body))
	}
}

func TestApply_ManyCapturesAreAggregateBoundedForPreview(t *testing.T) {
	captures := make(map[string]any, 200)
	for i := 0; i < 200; i++ {
		captures[fmt.Sprintf("cap_%03d", i)] = strings.Repeat("x", 3900)
	}
	s := runstate.New()
	s.Apply(ev("step/completed", 1, "", map[string]any{"step_id": "query", "captures": captures}))
	snap := s.Snapshot()
	if snap.Vars["_preview_captures_truncated"] != true {
		t.Fatalf("expected aggregate truncation marker: %#v", snap.Vars)
	}
	if _, ok := snap.Vars["cap_000"]; !ok {
		t.Fatalf("deterministic first key was not retained: %#v", snap.Vars)
	}
	if _, ok := snap.Vars["cap_199"]; ok {
		t.Fatalf("late key retained despite aggregate budget")
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 20000 {
		t.Fatalf("preview state JSON with many captures is unbounded: got %d bytes", len(body))
	}
}

func TestApply_InitialVarsAreAggregateBoundedForPreview(t *testing.T) {
	vars := make(map[string]any, 200)
	for i := 0; i < 200; i++ {
		vars[fmt.Sprintf("input_%03d", i)] = strings.Repeat("x", 3900)
	}
	s := runstate.New()
	s.Apply(ev("run/started", 1, "", map[string]any{"vars": vars}))
	snap := s.Snapshot()
	if snap.Vars["_preview_captures_truncated"] != true {
		t.Fatalf("expected initial vars aggregate truncation marker: %#v", snap.Vars)
	}
	if _, ok := snap.Vars["input_000"]; !ok {
		t.Fatalf("deterministic first key missing: %#v", snap.Vars)
	}
	if _, ok := snap.Vars["input_199"]; ok {
		t.Fatalf("late key retained despite aggregate budget")
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 24000 {
		t.Fatalf("initial preview vars are unbounded: got %d bytes", len(body))
	}
}

func TestPreviewEventPayload_BoundsLineErrorAndResiduals(t *testing.T) {
	payload := runstate.PreviewEventPayload(map[string]any{
		"step_id": "query",
		"line":    strings.Repeat("{\"rows\":[", 2000),
		"error":   strings.Repeat("failed", 2000),
		"future":  map[string]any{"blob": strings.Repeat("z", 20000)},
	})
	if len(payload["line"].(string)) > runstate.PreviewDiagnosticExcerptCharBudget || payload["line_preview_truncated"] != true {
		t.Fatalf("line was not bounded/marked: %#v", payload)
	}
	if len(payload["error"].(string)) > runstate.PreviewDiagnosticExcerptCharBudget || payload["error_preview_truncated"] != true {
		t.Fatalf("error was not bounded/marked: %#v", payload)
	}
	future, ok := payload["future"].(map[string]any)
	if !ok || future["_preview_truncated"] != true || jsonSizeForRunstateTest(future) > runstate.PreviewCaptureValueCharBudget+512 {
		t.Fatalf("residual field was not bounded/marked: %#v", payload["future"])
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 14000 {
		t.Fatalf("preview event payload is unbounded: got %d", len(body))
	}
}

func TestPreviewEventPayload_CaptureOmissionCountIsIdempotent(t *testing.T) {
	captures := make(map[string]any, 200)
	for i := 0; i < 200; i++ {
		captures[fmt.Sprintf("cap_%03d", i)] = strings.Repeat("x", 3900)
	}
	first := runstate.PreviewEventPayload(map[string]any{"captures": captures})
	firstCaps := first["captures"].(map[string]any)
	omitted1 := firstCaps["_preview_captures_omitted"]
	if firstCaps["_preview_captures_truncated"] != true || omitted1 == nil || omitted1 == 0 {
		t.Fatalf("expected first projection omission count: %#v", firstCaps)
	}
	second := runstate.PreviewEventPayload(first)
	secondCaps := second["captures"].(map[string]any)
	if secondCaps["_preview_captures_omitted"] != omitted1 {
		t.Fatalf("omission count changed after second projection: first=%#v second=%#v", omitted1, secondCaps["_preview_captures_omitted"])
	}
	if secondCaps["_preview_captures_truncated"] != true {
		t.Fatalf("truncation marker lost after second projection: %#v", secondCaps)
	}
}

func TestApply_InteractionOutputPreservedAndBoundedForPreview(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/completed", 1, "", map[string]any{
		"step_id": "collect",
		"output": map[string]any{"interaction": map[string]any{
			"kind":   "collector",
			"prompt": strings.Repeat("prompt", 2000),
			"answer": map[string]any{"field": strings.Repeat("answer", 2000)},
		}},
	}))
	n := s.Get("collect")
	if n.Interaction == nil || n.Interaction.Kind != "collector" {
		t.Fatalf("interaction not preserved: %#v", n.Interaction)
	}
	if len(n.Interaction.Prompt) > 4096 {
		t.Fatalf("prompt not bounded: %d", len(n.Interaction.Prompt))
	}
	body, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "preview_truncated") {
		t.Fatalf("expected interaction truncation marker in preview JSON: %s", body)
	}
	if len(body) > 12000 {
		t.Fatalf("interaction preview state is unbounded: got %d", len(body))
	}
}

func TestApply_FailedQueryResultDiagnosticsAreBoundedBeforeSerialization(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/failed", 1, "", map[string]any{
		"step_id": "query",
		"error":   "native tool queryer action query: result: stdout must be exactly one JSON object",
		"output": map[string]any{
			"stdout":    strings.Repeat("not-json", 20000),
			"stderr":    strings.Repeat("diagnostic", 20000),
			"exit_code": 0,
		},
	}))

	n := s.Get("query")
	if _, exists := n.Output["stdout"]; exists {
		t.Fatalf("failed preview output carried full stdout")
	}
	if _, exists := n.Output["stderr"]; exists {
		t.Fatalf("failed preview output carried full stderr")
	}
	if n.Output["stdout_truncated"] != true || n.Output["stderr_truncated"] != true || n.Output["preview_truncated"] != true {
		t.Fatalf("expected diagnostic truncation metadata: %#v", n.Output)
	}
	body, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 12000 {
		t.Fatalf("failed preview state JSON is unbounded: got %d bytes, want <= 12000", len(body))
	}
}

func TestApply_IterateProgress(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 1, "", map[string]any{"step_id": "loop"}))
	s.Apply(ev("iterate/iteration_started", 2, "", map[string]any{
		"step_id":         "loop",
		"iteration_index": int64(1),
		"iteration_total": int64(3),
	}))
	s.Apply(ev("iterate/iteration_completed", 3, "", map[string]any{
		"step_id":         "loop",
		"iteration_index": int64(1),
		"iteration_total": int64(3),
	}))
	s.Apply(ev("iterate/iteration_started", 4, "", map[string]any{
		"step_id":         "loop",
		"iteration_index": int64(2),
		"iteration_total": int64(3),
	}))

	n := s.Get("loop")
	if n.Iteration == nil || n.Iteration.Index != 2 || n.Iteration.Total != 3 {
		t.Errorf("iteration progress: got %+v", n.Iteration)
	}
}

func TestApply_RetrySetsAttempt(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 1, "", map[string]any{"step_id": "x"}))
	s.Apply(ev("step/failed", 2, "", map[string]any{"step_id": "x", "error": "boom"}))
	s.Apply(ev("step/started", 3, "", map[string]any{"step_id": "x"}))
	if got := s.Get("x").Attempt; got != 2 {
		t.Errorf("attempt count: got %d want 2", got)
	}
	if got := s.Get("x").Status; got != runstate.StatusRunning {
		t.Errorf("after retry: got %q want running", got)
	}
}

func TestApply_DebugOverridePersistsActualAndEffectiveResult(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 1, "", map[string]any{"step_id": "get_incident"}))
	s.Apply(ev("debug/override_applied", 2, "", map[string]any{
		"step_id": "get_incident",
		"phase":   "after",
		"call_path": []any{
			map[string]any{"step_id": "inspect_primary_icm"},
		},
		"actual": map[string]any{
			"status": "completed",
			"output": map[string]any{"incident": map[string]any{"status": "Mitigated"}},
		},
		"effective": map[string]any{
			"status": "completed",
			"output": map[string]any{"incident": map[string]any{"status": "Active"}},
		},
		"actual_vars":    map[string]any{"incident_status": "Mitigated"},
		"effective_vars": map[string]any{"incident_status": "Active"},
	}))
	s.Apply(ev("step/completed", 3, "", map[string]any{
		"step_id": "get_incident",
		"output":  map[string]any{"incident": map[string]any{"status": "Active"}},
	}))

	node := s.Get("get_incident")
	if node.DebugOverride == nil || node.DebugOverride.Phase != "after" {
		t.Fatalf("debug override = %#v", node.DebugOverride)
	}
	actual := node.DebugOverride.Actual["output"].(map[string]any)["incident"].(map[string]any)["status"]
	effective := node.DebugOverride.Effective["output"].(map[string]any)["incident"].(map[string]any)["status"]
	if actual != "Mitigated" || effective != "Active" {
		t.Fatalf("actual/effective = %v/%v", actual, effective)
	}
	if node.DebugOverride.ActualVars["incident_status"] != "Mitigated" || node.DebugOverride.EffectiveVars["incident_status"] != "Active" {
		t.Fatalf("actual/effective vars = %#v/%#v", node.DebugOverride.ActualVars, node.DebugOverride.EffectiveVars)
	}
	body, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(body) > 16000 {
		t.Fatalf("debug override preview is unbounded: %d bytes", len(body))
	}
}

func TestSnapshot_IsIndependent(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 1, "", map[string]any{"step_id": "a"}))
	snap := s.Snapshot()
	s.Apply(ev("step/completed", 2, "", map[string]any{"step_id": "a", "duration_ms": int64(7)}))
	if snap.Get("a").Status != runstate.StatusRunning {
		t.Errorf("snapshot mutated by later Apply")
	}
}

func TestGetForNode_PrefersQualifiedAndFallsBackToRaw(t *testing.T) {
	state := runstate.New()
	state.Apply(ev("step/completed", 1, "", map[string]any{
		"step_id": "child", "node_id": "include/child",
	}))
	if got := state.GetForNode("include/child", "child"); got.Status != runstate.StatusCompleted || got.ID != "include/child" {
		t.Fatalf("qualified lookup = %#v", got)
	}

	legacy := runstate.New()
	legacy.Apply(ev("step/completed", 1, "", map[string]any{"step_id": "child"}))
	if got := legacy.GetForNode("include/child", "child"); got.Status != runstate.StatusCompleted || got.ID != "child" {
		t.Fatalf("legacy fallback = %#v", got)
	}
}

func TestApply_IterationRecords(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 1, "", map[string]any{"step_id": "loop"}))
	s.Apply(ev("iterate/iteration_started", 2, "2026-05-09T12:00:00Z", map[string]any{
		"step_id":         "loop",
		"iteration_index": int64(1),
		"iteration_total": int64(2),
		"as":              "svc",
		"value":           "alpha",
	}))
	s.Apply(ev("iterate/iteration_started", 3, "2026-05-09T12:00:00Z", map[string]any{
		"step_id":         "loop",
		"iteration_index": int64(2),
		"iteration_total": int64(2),
		"as":              "svc",
		"value":           "beta",
	}))
	// Concurrent: index 2 finishes first.
	s.Apply(ev("iterate/iteration_completed", 4, "2026-05-09T12:00:01Z", map[string]any{
		"step_id":         "loop",
		"iteration_index": int64(2),
		"iteration_total": int64(2),
		"as":              "svc",
		"value":           "beta",
		"status":          "completed",
		"duration_ms":     int64(180),
	}))
	s.Apply(ev("iterate/iteration_completed", 5, "2026-05-09T12:00:02Z", map[string]any{
		"step_id":         "loop",
		"iteration_index": int64(1),
		"iteration_total": int64(2),
		"as":              "svc",
		"value":           "alpha",
		"status":          "failed",
		"duration_ms":     int64(420),
	}))

	n := s.Get("loop")
	if n.Iteration == nil {
		t.Fatalf("iteration nil")
	}
	if got := len(n.Iteration.Records); got != 2 {
		t.Fatalf("records: got %d want 2", got)
	}
	byIdx := map[int]runstate.IterationRecord{}
	for _, r := range n.Iteration.Records {
		byIdx[r.Index] = r
	}
	if r := byIdx[1]; r.Status != runstate.StatusFailed || r.DurationMs != 420 || r.Value != "alpha" || r.As != "svc" {
		t.Errorf("record 1 wrong: %+v", r)
	}
	if r := byIdx[2]; r.Status != runstate.StatusCompleted || r.DurationMs != 180 || r.Value != "beta" {
		t.Errorf("record 2 wrong: %+v", r)
	}
	if byIdx[1].StartedAt == nil || byIdx[2].StartedAt == nil {
		t.Errorf("started_at not preserved through completed event")
	}
}
