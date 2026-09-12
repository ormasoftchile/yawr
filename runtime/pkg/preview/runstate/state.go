// Package runstate models the runtime status of a runbook execution as a
// flat overlay keyed by node ID, fed by engine.Event records.
//
// runstate.State is intentionally separate from graphdoc.Document:
//   - Document  is structural, content-hashable, stable across runs.
//   - State     is mutable, per-run, streamed live during execution.
//
// Renderers consume both: structure from Document, status from State,
// joining on node ID. This separation lets us cache the document, cheap-
// diff the state, and stream state changes over SSE without re-sending
// the structure each time.
package runstate

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// Status enumerates the lifecycle phases of a node.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusDelaying  Status = "delaying"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusSkipped   Status = "skipped"
	StatusCancelled Status = "cancelled"
)

const (
	PreviewQueryResultSampleRows       = 10
	PreviewQueryResultCellCharLimit    = 120
	PreviewQueryResultTotalCharBudget  = 12000
	PreviewQueryResultUITextReserve    = 5120
	PreviewDiagnosticExcerptCharBudget = 4096
	PreviewCaptureValueCharBudget      = 4096
	PreviewCapturesTotalCharBudget     = 12000
	PreviewCapturesMaxEntries          = 64
	PreviewVarsTotalCharBudget         = 16000
	PreviewInteractionPromptCharBudget = 4096
	PreviewInteractionAnswerCharBudget = 4096
	previewMetadataCharBudget          = 2048
)

// RunStatus enumerates the lifecycle phases of the run as a whole.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// NodeState is the overlay record for a single node.
type NodeState struct {
	ID         string     `json:"id"`
	Status     Status     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMs int64      `json:"duration_ms,omitempty"`
	DelayMs    int64      `json:"delay_ms,omitempty"`
	Attempt    int        `json:"attempt,omitempty"`
	Error      string     `json:"error,omitempty"`

	// Iteration tracks live progress for an iterate node. nil for non-iterate.
	Iteration *IterationProgress `json:"iteration,omitempty"`

	// Interaction is the resolved prompt + answer for completed interactive
	// steps (collector, decision, choice). nil for non-interactive steps.
	Interaction *InteractionRecord `json:"interaction,omitempty"`

	// Output is the completed step's structured result output, when present.
	// It is used by preview renderers for opt-in typed results such as
	// yawr.query-result/v1; process diagnostics remain under stdout/stderr/exit_code.
	Output map[string]any `json:"output,omitempty"`

	// DebugOverride preserves the immutable executor observation beside the
	// effective result used by downstream execution.
	DebugOverride *DebugOverrideRecord `json:"debug_override,omitempty"`
}

// DebugOverrideRecord is the bounded preview projection of one applied override.
type DebugOverrideRecord struct {
	Phase         string         `json:"phase"`
	CallPath      []any          `json:"call_path,omitempty"`
	Actual        map[string]any `json:"actual,omitempty"`
	Effective     map[string]any `json:"effective,omitempty"`
	ActualVars    map[string]any `json:"actual_vars,omitempty"`
	EffectiveVars map[string]any `json:"effective_vars,omitempty"`
}

// InteractionRecord captures what an interactive step asked and what the
// user answered, for post-completion display in renderers.
type InteractionRecord struct {
	Kind             string         `json:"kind"`                        // collector | decision | choice
	Prompt           string         `json:"prompt,omitempty"`            // resolved (templated) prompt text
	Answer           map[string]any `json:"answer,omitempty"`            // shape depends on kind
	PreviewTruncated bool           `json:"preview_truncated,omitempty"` // true when preview prompt/answer/options were bounded
}

// IterationProgress is the per-iterate-node overlay tracking how many
// loop bodies have started/completed.
type IterationProgress struct {
	Index   int               `json:"index"`             // 1-based; the latest iteration to have started or completed
	Total   int               `json:"total"`             // -1 when unknown (open iterators)
	Records []IterationRecord `json:"records,omitempty"` // per-iteration history; appended on iterate/iteration_started
}

// IterationRecord captures the per-iteration outcome of a single pass
// through an iterate's body. Renderers use this to show a table of all
// iterations after the loop completes (or while it is in progress).
type IterationRecord struct {
	Index      int        `json:"index"` // 1-based
	As         string     `json:"as,omitempty"`
	Value      any        `json:"value,omitempty"`
	Status     Status     `json:"status,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMs int64      `json:"duration_ms,omitempty"`
}

// State is the runtime overlay for a Document. It is safe for concurrent
// readers as long as updates are serialized through Apply (or callers
// hold a single writer goroutine).
type State struct {
	mu sync.RWMutex

	RunID     string               `json:"run_id"`
	RunbookID string               `json:"runbook_id"`
	Status    RunStatus            `json:"status"`
	StartedAt *time.Time           `json:"started_at,omitempty"`
	EndedAt   *time.Time           `json:"ended_at,omitempty"`
	Nodes     map[string]NodeState `json:"nodes"`
	Sequence  int64                `json:"sequence"`

	// Vars is the runtime variable scope: initial runbook vars + inputs
	// (seeded from run/started) plus any captures merged from completed
	// steps. Renderers can show this to surface what the engine sees.
	Vars map[string]any `json:"vars,omitempty"`

	// CurrentNodeID is the most recently-started, not-yet-finished node.
	// Useful for the TUI's "you are here" cursor.
	CurrentNodeID string `json:"current_node_id,omitempty"`

	previewVarsOmitted int `json:"-"`
}

// New returns a fresh empty State.
func New() *State {
	return &State{
		Status: RunPending,
		Nodes:  make(map[string]NodeState),
	}
}

// Snapshot returns a deep-enough copy of the state for safe consumption
// by a renderer. The Nodes map is cloned; pointer fields inside NodeState
// (StartedAt, FinishedAt, Iteration) are shared.
func (s *State) Snapshot() *State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := &State{
		RunID:              s.RunID,
		RunbookID:          s.RunbookID,
		Status:             s.Status,
		StartedAt:          s.StartedAt,
		EndedAt:            s.EndedAt,
		Nodes:              make(map[string]NodeState, len(s.Nodes)),
		Sequence:           s.Sequence,
		CurrentNodeID:      s.CurrentNodeID,
		previewVarsOmitted: s.previewVarsOmitted,
	}
	if s.Vars != nil {
		out.Vars = make(map[string]any, len(s.Vars))
		for k, v := range s.Vars {
			out.Vars[k] = v
		}
	}
	for k, v := range s.Nodes {
		if v.Output != nil {
			cloned := make(map[string]any, len(v.Output))
			for ok, ov := range v.Output {
				cloned[ok] = ov
			}
			v.Output = cloned
		}
		out.Nodes[k] = v
	}
	return out
}

// Get returns the NodeState for id, or a zero-value state with id set if
// no events for that node have been seen yet.
func (s *State) Get(id string) NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n, ok := s.Nodes[id]; ok {
		return n
	}
	return NodeState{ID: id, Status: StatusPending}
}

// GetForNode returns qualified runtime state when present, falling back to the
// semantic v1 step ID for older traces and top-level nodes.
func (s *State) GetForNode(qualifiedID, rawID string) NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if qualifiedID != "" {
		if node, ok := s.Nodes[qualifiedID]; ok {
			return node
		}
	}
	if node, ok := s.Nodes[rawID]; ok {
		return node
	}
	if qualifiedID != "" {
		return NodeState{ID: qualifiedID, Status: StatusPending}
	}
	return NodeState{ID: rawID, Status: StatusPending}
}

// Apply ingests a single engine.Event and mutates the state accordingly.
// Out-of-order or unknown events are ignored.
func (s *State) Apply(ev engine.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ev.Sequence > s.Sequence {
		s.Sequence = ev.Sequence
	}

	ts := parseTimestamp(ev.Timestamp)

	switch trace.EventKind(ev.Kind) {
	case trace.EventKindRunStarted:
		s.RunID = ev.RunID
		s.RunbookID = ev.RunbookID
		s.Status = RunRunning
		if !ts.IsZero() {
			s.StartedAt = &ts
		}
		if raw, ok := ev.Payload["vars"].(map[string]any); ok {
			s.Vars = PreviewVars(raw)
			s.previewVarsOmitted = previewOmittedCount(s.Vars)
		}
	case trace.EventKindRunCompleted:
		s.Status = RunCompleted
		if !ts.IsZero() {
			s.EndedAt = &ts
		}
		s.CurrentNodeID = ""
	case trace.EventKindRunFailed:
		s.Status = RunFailed
		if !ts.IsZero() {
			s.EndedAt = &ts
		}
		s.CurrentNodeID = ""
	case trace.EventKindRunCancelled:
		s.Status = RunCancelled
		if !ts.IsZero() {
			s.EndedAt = &ts
		}
		s.CurrentNodeID = ""

	case trace.EventKindStepStarted:
		id := stepID(ev)
		if id == "" {
			return
		}
		n := s.nodeOrNew(id)
		n.Status = StatusRunning
		if !ts.IsZero() {
			n.StartedAt = &ts
		}
		n.Attempt++
		s.Nodes[id] = n
		s.CurrentNodeID = id

	case trace.EventKindStepResumed:
		id := stepID(ev)
		if id == "" {
			return
		}
		n := s.nodeOrNew(id)
		n.Status = StatusRunning
		if !ts.IsZero() {
			n.StartedAt = &ts
		}
		s.Nodes[id] = n
		s.CurrentNodeID = id

	case trace.EventKindStepDelaying:
		// step/delaying fires after step/started but before the engine
		// sleeps for the configured `delay:` duration. We override the
		// node's status so the UI can show a distinct "delaying" badge
		// and skip auto-selecting it (keeping the previous step's output
		// visible while the new step is paused).
		id := stepID(ev)
		if id == "" {
			return
		}
		n := s.nodeOrNew(id)
		n.Status = StatusDelaying
		if d, ok := durationField(ev, "delay"); ok {
			n.DelayMs = d.Milliseconds()
		}
		s.Nodes[id] = n
		s.CurrentNodeID = id

	case trace.EventKind("debug/override_applied"):
		id := stepID(ev)
		if id == "" {
			return
		}
		n := s.nodeOrNew(id)
		phase, _ := stringField(ev, "phase")
		record := &DebugOverrideRecord{Phase: truncatePreviewString(phase, 32)}
		if path, ok := anySlice(ev.Payload["call_path"]); ok {
			if projected, ok := previewGenericValue(path, PreviewCaptureValueCharBudget, 0).([]any); ok {
				record.CallPath = projected
			}
		}
		if actual, ok := ev.Payload["actual"].(map[string]any); ok {
			record.Actual = previewDebugResult(actual)
		}
		if effective, ok := ev.Payload["effective"].(map[string]any); ok {
			record.Effective = previewDebugResult(effective)
		}
		if actualVars, ok := ev.Payload["actual_vars"].(map[string]any); ok {
			record.ActualVars = PreviewVars(actualVars)
		}
		if effectiveVars, ok := ev.Payload["effective_vars"].(map[string]any); ok {
			record.EffectiveVars = PreviewVars(effectiveVars)
		}
		n.DebugOverride = record
		s.Nodes[id] = n

	case trace.EventKindStepCompleted:
		id := stepID(ev)
		if id == "" {
			return
		}
		n := s.nodeOrNew(id)
		n.Status = StatusCompleted
		if !ts.IsZero() {
			n.FinishedAt = &ts
		}
		if d, ok := intField(ev, "duration_ms"); ok {
			n.DurationMs = d
		}
		if out, ok := ev.Payload["output"].(map[string]any); ok && len(out) > 0 {
			n.Output = PreviewOutput(out)
		}
		if ir := interactionFromOutput(n.Output); ir != nil {
			n.Interaction = ir
		}
		if caps, ok := ev.Payload["captures"].(map[string]any); ok && len(caps) > 0 {
			if len(s.Vars) == 0 {
				s.Vars = PreviewVars(caps)
				s.previewVarsOmitted = previewOmittedCount(s.Vars)
			} else {
				current := previewCapturesWithBudget(s.Vars, PreviewCapturesMaxEntries/2, PreviewVarsTotalCharBudget/2)
				incoming := previewCapturesWithBudget(caps, PreviewCapturesMaxEntries/2, PreviewVarsTotalCharBudget/2)
				priorOmitted := previewOmittedCount(current) + previewOmittedCount(incoming)
				combined := make(map[string]any, len(current)+len(incoming)+2)
				for k, v := range current {
					if isPreviewAggregateMarker(k) {
						continue
					}
					combined[k] = v
				}
				for k, v := range incoming {
					if isPreviewAggregateMarker(k) {
						continue
					}
					combined[k] = v
				}
				if priorOmitted > 0 {
					combined["_preview_captures_truncated"] = true
					combined["_preview_captures_omitted"] = priorOmitted
				}
				s.Vars = PreviewVars(combined)
				s.previewVarsOmitted = previewOmittedCount(s.Vars)
			}
		}
		s.Nodes[id] = n
		if s.CurrentNodeID == id {
			s.CurrentNodeID = ""
		}

	case trace.EventKindStepFailed:
		id := stepID(ev)
		if id == "" {
			return
		}
		n := s.nodeOrNew(id)
		n.Status = StatusFailed
		if !ts.IsZero() {
			n.FinishedAt = &ts
		}
		if d, ok := intField(ev, "duration_ms"); ok {
			n.DurationMs = d
		}
		if e, ok := stringField(ev, "error"); ok {
			n.Error = truncatePreviewString(e, PreviewDiagnosticExcerptCharBudget)
		}
		if out, ok := ev.Payload["output"].(map[string]any); ok && len(out) > 0 {
			n.Output = PreviewOutput(out)
		}
		s.Nodes[id] = n
		if s.CurrentNodeID == id {
			s.CurrentNodeID = ""
		}

	case trace.EventKindStepSkipped:
		id := stepID(ev)
		if id == "" {
			return
		}
		n := s.nodeOrNew(id)
		n.Status = StatusSkipped
		s.Nodes[id] = n

	case trace.EventKindIterateIterationStarted:
		id := stepID(ev)
		if id == "" {
			return
		}
		idx, _ := intField(ev, "iteration_index")
		total, _ := intField(ev, "iteration_total")
		as, _ := stringField(ev, "as")
		n := s.nodeOrNew(id)
		prev := n.Iteration
		prog := &IterationProgress{Index: int(idx), Total: int(total)}
		if prev != nil {
			prog.Records = prev.Records
		}
		rec := IterationRecord{Index: int(idx), As: as, Status: StatusRunning}
		if v, ok := ev.Payload["value"]; ok {
			rec.Value = v
		}
		if !ts.IsZero() {
			rec.StartedAt = &ts
		}
		prog.Records = upsertIterationRecord(prog.Records, rec)
		n.Iteration = prog
		s.Nodes[id] = n

	case trace.EventKindIterateIterationCompleted:
		// We keep Iteration set to the last completed index so renderers
		// can show "5 of 10" after the step finishes; the iterate node's
		// own status is updated by step/completed.
		id := stepID(ev)
		if id == "" {
			return
		}
		idx, _ := intField(ev, "iteration_index")
		total, _ := intField(ev, "iteration_total")
		as, _ := stringField(ev, "as")
		statusStr, _ := stringField(ev, "status")
		durMs, _ := intField(ev, "duration_ms")
		n := s.nodeOrNew(id)
		prev := n.Iteration
		prog := &IterationProgress{Index: int(idx), Total: int(total)}
		if prev != nil {
			prog.Records = prev.Records
		}
		rec := IterationRecord{Index: int(idx), As: as, DurationMs: durMs}
		if v, ok := ev.Payload["value"]; ok {
			rec.Value = v
		}
		if statusStr != "" {
			rec.Status = Status(statusStr)
		} else {
			rec.Status = StatusCompleted
		}
		if !ts.IsZero() {
			rec.FinishedAt = &ts
		}
		// Preserve started_at from the matching iteration_started record.
		for _, existing := range prog.Records {
			if existing.Index == rec.Index && existing.StartedAt != nil {
				rec.StartedAt = existing.StartedAt
				break
			}
		}
		prog.Records = upsertIterationRecord(prog.Records, rec)
		n.Iteration = prog
		s.Nodes[id] = n
	}
}

func previewDebugResult(result map[string]any) map[string]any {
	projected, ok := previewGenericValue(result, PreviewCaptureValueCharBudget, 0).(map[string]any)
	if !ok {
		return map[string]any{"preview_truncated": true}
	}
	if jsonSize(result) > PreviewCaptureValueCharBudget {
		projected["preview_truncated"] = true
	}
	return projected
}

// nodeOrNew returns a copy of the node by id, or a fresh one with
// pending status if absent.
func (s *State) nodeOrNew(id string) NodeState {
	if n, ok := s.Nodes[id]; ok {
		return n
	}
	return NodeState{ID: id, Status: StatusPending}
}

// upsertIterationRecord replaces the record with the same Index, or
// appends rec when no matching index exists. Records are kept in
// insertion order; concurrent iterate workers may complete out of
// order, so callers should not assume rec.Index == len(records)+1.
func upsertIterationRecord(records []IterationRecord, rec IterationRecord) []IterationRecord {
	for i, existing := range records {
		if existing.Index == rec.Index {
			// Merge: prefer non-zero fields from the incoming record,
			// but never lose StartedAt that was set earlier.
			if rec.StartedAt == nil && existing.StartedAt != nil {
				rec.StartedAt = existing.StartedAt
			}
			if rec.As == "" {
				rec.As = existing.As
			}
			if rec.Value == nil {
				rec.Value = existing.Value
			}
			records[i] = rec
			return records
		}
	}
	return append(records, rec)
}

// stepID extracts the qualified runtime node identity when available and uses
// the raw step ID for top-level events.
func stepID(ev engine.Event) string {
	if v, ok := ev.Payload["node_id"]; ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	if v, ok := ev.Payload["step_id"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func stringField(ev engine.Event, k string) (string, bool) {
	if v, ok := ev.Payload[k]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

func intField(ev engine.Event, k string) (int64, bool) {
	v, ok := ev.Payload[k]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// PreviewEventPayload returns a payload safe for preview event buffering and
// SSE serialization. yawr.query-result/v1 outputs keep semantic totals plus a
// bounded sample/excerpts; full diagnostics remain in the engine trace, not in
// every preview state frame.
func PreviewEventPayload(payload map[string]any) map[string]any {
	if len(payload) == 0 {
		return payload
	}
	bounded := make(map[string]any, len(payload)+4)
	keys := make([]string, 0, len(payload))
	for k := range payload {
		if strings.HasSuffix(k, "_preview_truncated") || strings.HasPrefix(k, "_preview_") || strings.HasSuffix(k, "_omitted") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := payload[k]
		switch k {
		case "code_presentation", "output_value_status":
			// Typed metadata is projected atomically, never with generic sentinels.
			continue
		case "output":
			if out, ok := v.(map[string]any); ok && len(out) > 0 {
				bounded[k] = PreviewOutput(out)
			} else {
				bounded[k] = previewGenericValue(v, PreviewCaptureValueCharBudget, 0)
				bounded[k+"_preview_truncated"] = true
			}
		case "captures":
			if caps, ok := v.(map[string]any); ok && len(caps) > 0 {
				bounded[k] = PreviewCaptures(caps)
			} else {
				bounded[k] = previewGenericValue(v, PreviewCaptureValueCharBudget, 0)
				bounded[k+"_preview_truncated"] = true
			}
		case "vars":
			if vars, ok := v.(map[string]any); ok && len(vars) > 0 {
				bounded[k] = PreviewVars(vars)
			} else {
				bounded[k] = previewGenericValue(v, PreviewCaptureValueCharBudget, 0)
				bounded[k+"_preview_truncated"] = true
			}
		case "evidence":
			bounded[k] = previewGenericValue(v, PreviewCaptureValueCharBudget, 0)
			if jsonSize(v) > PreviewCaptureValueCharBudget || truthy(payload[k+"_preview_truncated"]) {
				bounded[k+"_preview_truncated"] = true
			}
		case "line", "error":
			bounded[k] = previewGenericValue(v, PreviewDiagnosticExcerptCharBudget, 0)
			if jsonSize(v) > PreviewDiagnosticExcerptCharBudget || truthy(payload[k+"_preview_truncated"]) {
				bounded[k+"_preview_truncated"] = true
			}
		default:
			bounded[k] = previewGenericValue(v, PreviewCaptureValueCharBudget, 0)
			if jsonSize(v) > PreviewCaptureValueCharBudget || truthy(payload[k+"_preview_truncated"]) {
				bounded[k+"_preview_truncated"] = true
			}
		}
	}
	previewPresentation(payload, bounded)
	return bounded
}

// PreviewOutput returns a bounded preview-specific representation of a tool
// output. It never carries full stdout/stderr or full query rows into preview
// state; large diagnostics stay available through the underlying run trace.
func PreviewOutput(output map[string]any) map[string]any {
	if len(output) == 0 {
		return output
	}
	if isQueryResultOutput(output) {
		return previewQueryResultOutput(output)
	}
	return previewDiagnosticOutput(output, true)
}

func previewInteraction(raw any) map[string]any {
	m, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{"preview_truncated": true}
	}
	bounded := make(map[string]any, len(m)+1)
	if kind, ok := m["kind"].(string); ok {
		bounded["kind"] = truncatePreviewString(kind, PreviewQueryResultCellCharLimit)
	}
	if prompt, ok := m["prompt"].(string); ok {
		bounded["prompt"] = truncatePreviewString(prompt, PreviewInteractionPromptCharBudget)
		if len(prompt) > PreviewInteractionPromptCharBudget || truthy(m["prompt_preview_truncated"]) {
			bounded["prompt_preview_truncated"] = true
			bounded["preview_truncated"] = true
		}
	}
	if answer, ok := m["answer"].(map[string]any); ok {
		bounded["answer"] = previewCaptureValue(answer, PreviewInteractionAnswerCharBudget)
		if jsonSize(answer) > PreviewInteractionAnswerCharBudget || truthy(m["answer_preview_truncated"]) {
			bounded["answer_preview_truncated"] = true
			bounded["preview_truncated"] = true
		}
	}
	if options, ok := m["options"]; ok {
		bounded["options"] = previewCaptureValue(options, PreviewInteractionAnswerCharBudget)
		if jsonSize(options) > PreviewInteractionAnswerCharBudget || truthy(m["options_preview_truncated"]) {
			bounded["options_preview_truncated"] = true
			bounded["preview_truncated"] = true
		}
	}
	if truthy(m["preview_truncated"]) {
		bounded["preview_truncated"] = true
	}
	return bounded
}

// PreviewCaptures returns preview-safe captures for a single event. Keys are
// retained in lexicographic order up to deterministic entry and total-byte
// budgets; omitted entries and sampled values are marked explicitly.
func PreviewCaptures(captures map[string]any) map[string]any {
	return previewCapturesWithBudget(captures, PreviewCapturesMaxEntries, PreviewCapturesTotalCharBudget)
}

// PreviewVars returns preview-safe cumulative variables for state snapshots.
func PreviewVars(vars map[string]any) map[string]any {
	return previewCapturesWithBudget(vars, PreviewCapturesMaxEntries, PreviewVarsTotalCharBudget)
}

func isPreviewAggregateMarker(key string) bool {
	return key == "_preview_captures_truncated" || key == "_preview_captures_omitted"
}

func previewOmittedCount(values map[string]any) int {
	if values == nil {
		return 0
	}
	if n, ok := integerValue(values["_preview_captures_omitted"]); ok && n > 0 {
		return int(n)
	}
	return 0
}

func previewCapturesWithBudget(captures map[string]any, maxEntries, totalBudget int) map[string]any {
	if len(captures) == 0 {
		return captures
	}
	keys := make([]string, 0, len(captures))
	for k := range captures {
		if strings.HasSuffix(k, "_preview_truncated") || k == "_preview_captures_truncated" || k == "_preview_captures_omitted" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	bounded := make(map[string]any, min(len(captures)+2, maxEntries+2))
	used := jsonSize(map[string]any{"_preview_captures_truncated": true, "_preview_captures_omitted": 0})
	retained := 0
	omitted := 0
	if prior, ok := integerValue(captures["_preview_captures_omitted"]); ok && prior > 0 {
		omitted = int(prior)
	}
	truncatedAggregate := false
	for _, k := range keys {
		if retained >= maxEntries {
			omitted++
			truncatedAggregate = true
			continue
		}
		v := captures[k]
		valueTruncated := false
		_, jsonErr := json.Marshal(v)
		if jsonErr != nil || jsonSize(v) > PreviewCaptureValueCharBudget {
			v = previewCaptureValue(v, PreviewCaptureValueCharBudget)
			valueTruncated = true
		}
		entryCost := jsonSize(map[string]any{k: v})
		markerCost := 0
		if valueTruncated || truthy(captures[k+"_preview_truncated"]) {
			markerCost = jsonSize(map[string]any{k + "_preview_truncated": true})
		}
		if used+entryCost+markerCost > totalBudget {
			omitted++
			truncatedAggregate = true
			continue
		}
		bounded[k] = v
		used += entryCost
		retained++
		if valueTruncated || truthy(captures[k+"_preview_truncated"]) {
			bounded[k+"_preview_truncated"] = true
			used += markerCost
		}
	}
	if truncatedAggregate || omitted > 0 || truthy(captures["_preview_captures_truncated"]) {
		bounded["_preview_captures_truncated"] = true
		bounded["_preview_captures_omitted"] = omitted
	}
	return bounded
}

func previewGenericValue(value any, budget, depth int) any {
	if budget <= 0 {
		return nil
	}
	switch v := value.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		if jsonSize(v) <= budget {
			return v
		}
		return truncatePreviewString(fmt.Sprint(v), budget)
	case float32:
		value := float64(v)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return truncatePreviewString(fmt.Sprint(v), budget)
		}
		return v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return truncatePreviewString(fmt.Sprint(v), budget)
		}
		return v
	case string:
		return truncatePreviewString(v, budget)
	case map[string]any:
		priorOmitted := 0
		if prior, ok := integerValue(v["_preview_omitted"]); ok && prior > 0 {
			priorOmitted = int(prior)
		}
		if depth >= 4 {
			return map[string]any{"_preview_truncated": true}
		}
		out := make(map[string]any)
		keys := make([]string, 0, len(v))
		for key := range v {
			if strings.HasSuffix(key, "_preview_truncated") || strings.HasPrefix(key, "_preview_") {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		remaining := budget
		omitted := priorOmitted
		for _, key := range keys {
			candidateBudget := min(remaining-len(key), PreviewCaptureValueCharBudget)
			if candidateBudget <= 0 {
				omitted++
				continue
			}
			candidate := previewGenericValue(v[key], candidateBudget, depth+1)
			cost := len(key) + jsonSize(candidate)
			if cost > remaining {
				omitted++
				continue
			}
			out[key] = candidate
			remaining -= cost
			if jsonSize(v[key]) > candidateBudget || truthy(v[key+"_preview_truncated"]) {
				out[key+"_preview_truncated"] = true
			}
		}
		if omitted > 0 || truthy(v["_preview_truncated"]) {
			out["_preview_truncated"] = true
			out["_preview_omitted"] = omitted
		}
		return out
	case map[string]string:
		converted := make(map[string]any, len(v))
		for key, val := range v {
			converted[key] = val
		}
		return previewGenericValue(converted, budget, depth)
	default:
		if slice, ok := anySlice(value); ok {
			if depth >= 4 {
				return []any{map[string]any{"_preview_truncated": true}}
			}
			priorOmitted := 0
			items := slice
			if len(slice) > 0 {
				if sentinel, ok := slice[len(slice)-1].(map[string]any); ok && truthy(sentinel["_preview_truncated"]) {
					if prior, ok := integerValue(sentinel["_preview_omitted"]); ok && prior > 0 {
						priorOmitted = int(prior)
					}
					items = slice[:len(slice)-1]
				}
			}
			out := make([]any, 0, min(len(items), PreviewQueryResultSampleRows)+1)
			remaining := budget
			for _, item := range items {
				if len(out) >= PreviewQueryResultSampleRows || remaining <= 0 {
					break
				}
				candidate := previewGenericValue(item, min(remaining, PreviewCaptureValueCharBudget), depth+1)
				cost := jsonSize(candidate)
				if cost > remaining {
					break
				}
				out = append(out, candidate)
				remaining -= cost
			}
			omitted := priorOmitted + len(items) - len(out)
			if omitted > 0 {
				out = append(out, map[string]any{"_preview_truncated": true, "_preview_omitted": omitted})
			}
			return out
		}
		if converted, ok := jsonCompatibleStruct(value); ok {
			return previewGenericValue(converted, budget, depth)
		}
		return truncatePreviewString(fmt.Sprint(value), budget)
	}
}

func jsonCompatibleStruct(value any) (any, bool) {
	if value == nil {
		return nil, false
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false
	}
	return out, true
}

func previewCaptureValue(value any, budget int) any {
	if budget <= 0 {
		return nil
	}
	switch v := value.(type) {
	case string:
		return truncatePreviewString(v, budget)
	case []any:
		out := make([]any, 0, min(len(v), PreviewQueryResultSampleRows))
		remaining := budget
		for _, item := range v {
			if len(out) >= PreviewQueryResultSampleRows || remaining <= 0 {
				break
			}
			candidate := previewCaptureValue(item, min(remaining, PreviewQueryResultCellCharLimit))
			cost := jsonSize(candidate)
			if cost > remaining {
				break
			}
			out = append(out, candidate)
			remaining -= cost
		}
		return out
	case map[string]any:
		out := make(map[string]any)
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		remaining := budget
		for _, key := range keys {
			if remaining <= 0 {
				break
			}
			candidate := previewCaptureValue(v[key], min(remaining-len(key), PreviewQueryResultCellCharLimit))
			cost := len(key) + jsonSize(candidate)
			if cost > remaining {
				break
			}
			out[key] = candidate
			remaining -= cost
		}
		return out
	case map[string]string:
		converted := make(map[string]any, len(v))
		for key, val := range v {
			converted[key] = val
		}
		return previewCaptureValue(converted, budget)
	default:
		if slice, ok := anySlice(value); ok {
			return previewCaptureValue(slice, budget)
		}
		text := fmt.Sprint(value)
		return truncatePreviewString(text, budget)
	}
}

func jsonSize(value any) int {
	b, err := json.Marshal(value)
	if err != nil {
		return len(fmt.Sprint(value))
	}
	return len(b)
}

func anySlice(value any) ([]any, bool) {
	if value == nil {
		return nil, false
	}
	if v, ok := value.([]any); ok {
		return v, true
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

func isQueryResultOutput(output map[string]any) bool {
	if _, ok := output["success"].(bool); !ok {
		return false
	}
	if _, ok := integerValue(output["row_count"]); !ok {
		return false
	}
	_, colsOK := anySlice(output["columns"])
	_, rowsOK := anySlice(output["rows"])
	return colsOK && rowsOK
}

func previewDiagnosticOutput(output map[string]any, markOmitted ...bool) map[string]any {
	bounded := make(map[string]any, 7)
	if v, ok := output["exit_code"]; ok {
		bounded["exit_code"] = v
	}
	if v, ok := integerValue(output["matched_arm_index"]); ok {
		bounded["matched_arm_index"] = v
	}
	if v, ok := output["matched_arm"].(string); ok {
		bounded["matched_arm"] = truncatePreviewString(v, previewMetadataCharBudget)
	}
	if raw, ok := output["interaction"]; ok {
		bounded["interaction"] = previewInteraction(raw)
	}
	copyDiagnosticExcerpt(bounded, output, "stdout")
	copyDiagnosticExcerpt(bounded, output, "stderr")
	if len(markOmitted) > 0 && markOmitted[0] {
		known := map[string]bool{
			"exit_code": true, "matched_arm_index": true, "matched_arm": true, "interaction": true,
			"stdout": true, "stdout_excerpt": true, "stdout_truncated": true,
			"stderr": true, "stderr_excerpt": true, "stderr_truncated": true,
			"preview_truncated":      true,
			"preview_fields_omitted": true,
		}
		omitted := 0
		if prior, ok := integerValue(output["preview_fields_omitted"]); ok && prior > 0 {
			omitted = int(prior)
		}
		for key := range output {
			if !known[key] {
				omitted++
			}
		}
		if omitted > 0 {
			bounded["preview_fields_omitted"] = omitted
			bounded["preview_truncated"] = true
		}
	}
	return bounded
}

func previewQueryResultOutput(output map[string]any) map[string]any {
	bounded := previewDiagnosticOutput(output)
	bounded["success"] = output["success"]
	bounded["row_count"] = output["row_count"]

	columns, _ := anySlice(output["columns"])
	rows, _ := anySlice(output["rows"])
	sampledColumns, sampledRows, columnsTruncated, rowsTruncated, budgetExhausted := sampleQueryRowsForPreview(columns, rows)
	columnsTruncated = columnsTruncated || truthy(output["columns_truncated"])
	rowsTruncated = rowsTruncated || truthy(output["rows_truncated"])
	budgetExhausted = budgetExhausted || truthy(output["budget_exhausted"])
	bounded["columns"] = sampledColumns
	bounded["rows"] = sampledRows
	bounded["columns_truncated"] = columnsTruncated
	bounded["rows_truncated"] = rowsTruncated
	bounded["budget_exhausted"] = budgetExhausted
	bounded["preview_row_sample_count"] = len(sampledRows)
	if columnsTruncated || rowsTruncated || budgetExhausted || truthy(bounded["stdout_truncated"]) || truthy(bounded["stderr_truncated"]) || truthy(output["preview_truncated"]) {
		bounded["preview_truncated"] = true
	}
	if metadata, ok := output["metadata"].(map[string]any); ok {
		if encoded, err := json.Marshal(metadata); err == nil && len(encoded) <= previewMetadataCharBudget {
			bounded["metadata"] = metadata
		} else {
			bounded["metadata_truncated"] = true
			bounded["preview_truncated"] = true
		}
	} else if truthy(output["metadata_truncated"]) {
		bounded["metadata_truncated"] = true
		bounded["preview_truncated"] = true
	}
	return bounded
}

func sampleQueryRowsForPreview(columns, rows []any) ([]any, []any, bool, bool, bool) {
	budget := PreviewQueryResultTotalCharBudget - PreviewQueryResultUITextReserve
	if budget < 0 {
		budget = 0
	}
	visibleColumns := make([]any, 0, len(columns))
	visibleSources := make([]string, 0, len(columns))
	budgetExhausted := false
	for _, col := range columns {
		source := fmt.Sprint(col)
		label := formatPreviewCell(col)
		cost := len(label) * 2
		if len(visibleColumns) > 0 {
			cost += 2
		}
		if cost > budget {
			if len(visibleColumns) == 0 && budget > 2 {
				label = truncatePreviewString(label, budget/2)
				visibleColumns = append(visibleColumns, label)
				visibleSources = append(visibleSources, source)
				budget -= len(label) * 2
			}
			budgetExhausted = true
			break
		}
		visibleColumns = append(visibleColumns, label)
		visibleSources = append(visibleSources, source)
		budget -= cost
	}

	limit := len(rows)
	if limit > PreviewQueryResultSampleRows {
		limit = PreviewQueryResultSampleRows
	}
	sampledRows := make([]any, 0, limit)
	if len(visibleColumns) > 0 {
		for _, row := range rows[:limit] {
			cells := make([]any, 0, len(visibleColumns))
			rowCost := 0
			for idx, source := range visibleSources {
				cell := formatPreviewCell(queryPreviewCellValue(row, source, idx))
				cells = append(cells, cell)
				rowCost += len(cell)
			}
			if rowCost > budget {
				if len(sampledRows) == 0 && budget > 0 {
					cells = fitPreviewCellsToBudget(cells, budget)
					sampledRows = append(sampledRows, cells)
				}
				budgetExhausted = true
				break
			}
			budget -= rowCost
			sampledRows = append(sampledRows, cells)
		}
	}
	return visibleColumns, sampledRows, len(visibleColumns) < len(columns), len(sampledRows) < len(rows), budgetExhausted
}

func copyDiagnosticExcerpt(dst, src map[string]any, name string) {
	excerptKey := name + "_excerpt"
	truncatedKey := name + "_truncated"
	if existing, ok := src[excerptKey]; ok {
		excerpt := fmt.Sprint(existing)
		truncated := truthy(src[truncatedKey])
		if len(excerpt) > PreviewDiagnosticExcerptCharBudget {
			excerpt = truncatePreviewString(excerpt, PreviewDiagnosticExcerptCharBudget)
			truncated = true
		}
		dst[excerptKey] = excerpt
		dst[truncatedKey] = truncated
		if truncated {
			dst["preview_truncated"] = true
		}
		return
	}
	v, ok := src[name]
	if !ok {
		return
	}
	text, ok := v.(string)
	if !ok {
		text = fmt.Sprint(v)
	}
	excerpt, truncated := truncatePreviewDiagnostic(text)
	dst[excerptKey] = excerpt
	dst[truncatedKey] = truncated
	if truncated {
		dst["preview_truncated"] = true
	}
}

func truncatePreviewDiagnostic(text string) (string, bool) {
	if len(text) <= PreviewDiagnosticExcerptCharBudget {
		return text, false
	}
	return truncatePreviewString(text, PreviewDiagnosticExcerptCharBudget), true
}

func queryPreviewCellValue(row any, column string, index int) any {
	switch r := row.(type) {
	case []any:
		if index >= 0 && index < len(r) {
			return r[index]
		}
	case map[string]any:
		return r[column]
	case map[string]string:
		return r[column]
	}
	return row
}

func formatPreviewCell(value any) string {
	var text string
	switch v := value.(type) {
	case nil:
		text = ""
	case string:
		text = v
	default:
		if b, err := json.Marshal(v); err == nil {
			text = string(b)
		} else {
			text = fmt.Sprint(v)
		}
	}
	return truncatePreviewString(text, PreviewQueryResultCellCharLimit)
}

func fitPreviewCellsToBudget(cells []any, budget int) []any {
	out := make([]any, len(cells))
	for i, cell := range cells {
		if budget <= 0 {
			out[i] = ""
			continue
		}
		text := fmt.Sprint(cell)
		text = truncatePreviewString(text, budget)
		out[i] = text
		budget -= len(text)
	}
	return out
}

func truncatePreviewString(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(text) <= budget {
		return text
	}
	if budget <= 3 {
		return strings.Repeat(".", budget)
	}
	return text[:budget-3] + "..."
}

func integerValue(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case float64:
		if v == float64(int64(v)) {
			return int64(v), true
		}
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

func truthy(value any) bool {
	v, _ := value.(bool)
	return v
}

// interactionFromOutput extracts a typed InteractionRecord from
// output.interaction inside a step/completed event payload, when
// present. Returns nil when the step is not an interactive step or
// the payload does not carry an interaction echo.
func interactionFromOutput(out map[string]any) *InteractionRecord {
	if out == nil {
		return nil
	}
	raw, ok := out["interaction"].(map[string]any)
	if !ok {
		return nil
	}
	rec := &InteractionRecord{}
	if v, ok := raw["kind"].(string); ok {
		rec.Kind = v
	}
	if v, ok := raw["prompt"].(string); ok {
		rec.Prompt = v
	}
	if v, ok := raw["answer"].(map[string]any); ok {
		rec.Answer = v
	}
	if truthy(raw["preview_truncated"]) || truthy(raw["prompt_preview_truncated"]) || truthy(raw["answer_preview_truncated"]) || truthy(raw["options_preview_truncated"]) {
		rec.PreviewTruncated = true
	}
	if rec.Kind == "" && rec.Prompt == "" && rec.Answer == nil {
		return nil
	}
	return rec
}

// durationField parses a Go-style duration string field (e.g. "3s",
// "1.5m") from an event payload. Returns zero/false when missing or
// unparseable.
func durationField(ev engine.Event, k string) (time.Duration, bool) {
	s, ok := stringField(ev, k)
	if !ok || s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d, true
}

// parseTimestamp parses an RFC3339(.micro) timestamp; returns zero on error.
func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
