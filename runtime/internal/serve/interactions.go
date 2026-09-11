// HTTP handlers for the runbook interaction wire protocol v1.
// See specs/runbook-interaction-wire-v1.md.

package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

// handleRunsCreate handles POST /runs and starts a new run.
//
//	{"runbookPath":"...", "inputs":{...}, "mode":"real", "actor":"web-ui"}
//
// Returns 201 with {"runID":"..."}.
func (s *Server) handleRunsCreate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if r.Body == nil {
		http.Error(w, "request body is required", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var params struct {
		RunbookPath string          `json:"runbookPath"`
		Inputs      map[string]any  `json:"inputs"`
		Mode        string          `json:"mode"`
		Actor       string          `json:"actor"`
		Debug       *DebugRunConfig `json:"debug"`
	}
	if err := json.Unmarshal(body, &params); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(params.RunbookPath) == "" {
		http.Error(w, "runbookPath is required", http.StatusBadRequest)
		return
	}

	vars, err := normalizeInputs(params.Inputs)
	if err != nil {
		http.Error(w, "inputs: "+err.Error(), http.StatusBadRequest)
		return
	}

	mode := engine.RunModeReal
	if params.Mode != "" {
		switch params.Mode {
		case string(engine.RunModeReal):
			mode = engine.RunModeReal
		case string(engine.RunModeDryRun):
			mode = engine.RunModeDryRun
		case string(engine.RunModeReplay):
			mode = engine.RunModeReplay
		default:
			http.Error(w, "invalid mode: "+params.Mode, http.StatusBadRequest)
			return
		}
	}

	actor := params.Actor
	if actor == "" {
		actor = "http"
	}

	plan, parsed, vars, warnings, err := s.loadPlanAndSeed(r.Context(), params.RunbookPath, vars)
	if err != nil {
		writeRunbookError(w, err)
		return
	}
	if params.Debug != nil && params.Debug.Enabled && params.Debug.Profile != nil {
		metadata, metadataErr := s.debugProfileMetadataForPlan(r.Context(), params.RunbookPath, plan)
		if metadataErr != nil {
			http.Error(w, metadataErr.Error(), http.StatusBadRequest)
			return
		}
		if bindingErr := validateDebugProfileBinding(params.Debug.Profile, metadata, params.Debug.AllowStaleProfile); bindingErr != nil {
			http.Error(w, bindingErr.Error(), http.StatusBadRequest)
			return
		}
	}
	if targetErr := s.validateDebugTargets(r.Context(), params.RunbookPath, params.Debug); targetErr != nil {
		http.Error(w, targetErr.Error(), http.StatusBadRequest)
		return
	}

	// Pre-register the broker queue so the engine cannot race ahead of the
	// client and drop a prompt.
	runID := plan.RunID
	if runID == "" {
		// Engine generates one if empty; we install our own so we can
		// register the broker before Start.
		runID = newRunID()
		plan.RunID = runID
	}
	if s.broker != nil {
		s.broker.Register(runID)
		if params.Debug != nil {
			if err := s.broker.ConfigureDebug(runID, *params.Debug); err != nil {
				s.broker.Unregister(runID)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
	}

	// Use a detached context for engine.Start. The request context is
	// canceled as soon as the 201 response is written, which would
	// propagate into every prompt the engine makes. The run's own
	// runCtx (created below) governs cancellation.
	startCtx := context.Background()
	startCtx, catalogWarnings, err := s.contextWithPerRunCatalog(startCtx, plan, parsed, params.RunbookPath)
	if err != nil {
		if s.broker != nil {
			s.broker.Unregister(runID)
		}
		writeRunbookError(w, err)
		return
	}
	warnings = append(warnings, catalogWarnings...)
	var debugger engine.DebugController
	if params.Debug != nil && params.Debug.Enabled {
		debugger = s.broker
	}
	handle, err := s.engine.Start(startCtx, plan, engine.RunOptions{
		Mode:     mode,
		Actor:    actor,
		Vars:     vars,
		Client:   "server",
		Debugger: debugger,
	})
	if err != nil {
		if s.broker != nil {
			s.broker.Unregister(runID)
		}
		http.Error(w, "start: "+err.Error(), http.StatusInternalServerError)
		return
	}

	runCtx, cancel := context.WithCancel(context.Background())
	state := handle.State()
	if state.RunID == "" {
		state.RunID = runID
	}
	entry := &RunEntry{
		ID:          state.RunID,
		RunbookPath: plan.RunbookPath,
		Handle:      handle,
		Cancel:      cancel,
		State:       state.Status,
		StartedAt:   state.StartedAt,
	}
	if entry.StartedAt.IsZero() {
		entry.StartedAt = time.Now()
	}
	s.registry.Add(entry)
	s.wg.Add(1)
	go s.pumpEvents(runCtx, entry)
	// Auto-advance the run to completion. Unlike the JSON-RPC `runs.next`
	// flow (which is turn-based), the web UI expects the engine to drive
	// itself; the broker handles any prompts mid-run.
	s.wg.Add(1)
	go s.advanceRun(runCtx, entry)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	respBody := map[string]any{"runID": entry.ID}
	if len(warnings) > 0 {
		// ENUM-W001 (AR-CE-4 §6, CE-W-02): non-fatal parse warnings MUST
		// reach a human on every surface that parses a runbook, including
		// POST /runs. The run still starts; this is advisory.
		respBody["warnings"] = rpcWarnings(warnings)
	}
	_ = json.NewEncoder(w).Encode(respBody)
}

// advanceRun calls handle.Next in a loop until the run completes, fails,
// is cancelled, or ctx is done. Each Next call drives one step; the
// PromptBroker blocks Next inside any interactive step until an answer
// arrives via POST /runs/{id}/interactions/{turnID}.
func (s *Server) advanceRun(ctx context.Context, entry *RunEntry) {
	defer s.wg.Done()
	if entry == nil || entry.Handle == nil {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		state := entry.Handle.State().Status
		if isStoppedState(state) {
			return
		}
		_, err := entry.Handle.Next(ctx)
		if errors.Is(err, io.EOF) {
			s.syncRunEntryState(entry)
			return
		}
		if err != nil {
			s.syncRunEntryState(entry)
			return
		}
	}
}

func (s *Server) syncRunEntryState(entry *RunEntry) engine.RunState {
	state := entry.Handle.State()
	s.registry.WithEntry(entry.ID, func(current *RunEntry) {
		current.State = state.Status
		current.CompletedAt = state.CompletedAt
		if isTerminalState(state.Status) && current.CompletedAt.IsZero() {
			current.CompletedAt = time.Now()
		}
	})
	return state
}

// handleRunsDelete handles DELETE /runs/{id} and cancels an active run.
// Idempotent: returns 204 even if the run already completed. Returns 404
// only if the run was never registered.
func (s *Server) handleRunsDelete(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if runID == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	entry, ok := s.registry.Get(runID)
	if !ok {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	// Cancel the engine handle first so it can emit run/cancelled and
	// mark its own status. Just cancelling the context (entry.Cancel)
	// alone unblocks the broker but never produces a terminal event,
	// so the UI's SSE state stream stays at "running" forever and the
	// "Running…" / "Cancel" buttons never go away.
	if entry.Handle != nil {
		_ = entry.Handle.Cancel(r.Context(), "user requested cancel")
	}
	if entry.Cancel != nil {
		entry.Cancel()
	}
	if s.broker != nil {
		s.broker.Unregister(runID)
	}
	s.registry.WithEntry(runID, func(e *RunEntry) {
		e.State = engine.RunStatusCancelled
		if e.CompletedAt.IsZero() {
			e.CompletedAt = time.Now()
		}
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleInteractionsStream handles GET /runs/{id}/interactions and
// streams pending/resolved frames as SSE.
func (s *Server) handleInteractionsStream(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if runID == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	if _, ok := s.registry.Get(runID); !ok {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if !s.registry.AllowPreviewRead(runID, time.Now()) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "preview read rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if s.broker == nil {
		http.Error(w, "interaction broker not configured", http.StatusServiceUnavailable)
		return
	}
	since := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	}

	ch, err := s.broker.Subscribe(runID, since)
	if err != nil {
		// Run completed and queue was torn down. HTTP 204 tells EventSource
		// clients not to reconnect, avoiding terminal hot loops.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer s.broker.Unsubscribe(runID, ch)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	clearSSEDeadlines(w)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	hbTicker := time.NewTicker(sseHeartbeatInterval)
	defer hbTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-hbTicker.C:
			if err := writeSSEHeartbeat(w); err != nil {
				return
			}
			flusher.Flush()
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if err := writeInteractionFrame(w, frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleInteractionAnswer handles POST /runs/{id}/interactions/{turnID}.
func (s *Server) handleInteractionAnswer(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	turnID := r.PathValue("turnID")
	if runID == "" || turnID == "" {
		http.Error(w, "missing run id or turn id", http.StatusBadRequest)
		return
	}
	if s.broker == nil {
		http.Error(w, "interaction broker not configured", http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var env AnswerEnvelope
	if err := decodeStrictJSON(body, &env); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch env.Kind {
	case "choice", "decision", "collector", "host_action", "debug_break":
		// ok
	default:
		http.Error(w, "invalid kind: "+env.Kind, http.StatusBadRequest)
		return
	}

	if err := s.broker.Answer(runID, turnID, env); err != nil {
		switch {
		case errors.Is(err, errRunNotRegistered):
			if _, registered := s.registry.Get(runID); registered {
				http.Error(w, "run not active", http.StatusConflict)
			} else {
				http.Error(w, "run not found", http.StatusNotFound)
			}
		case errors.Is(err, errUnknownTurn):
			http.Error(w, "turn not found or already answered", http.StatusConflict)
		case errors.Is(err, errKindMismatch), errors.Is(err, errInvalidInteractionToken), errors.Is(err, errInvalidHostActionStatus), errors.Is(err, errHostActionTupleMismatch), errors.Is(err, errInvalidDebugAnswer):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const previewPendingInteractionTotalCharBudget = 12000

// PreviewInteractionFrame returns the bounded, tokenized interaction shape
// safe for an operator-facing client. HTTP/SSE and stdio transports share it.
func PreviewInteractionFrame(frame any) any {
	return previewInteractionFrame(frame)
}

func previewInteractionFrame(frame any) any {
	pending, ok := frame.(PendingInteraction)
	if !ok {
		return frame
	}
	bounded := map[string]any{
		"type":   pending.Type,
		"id":     pending.ID,
		"turnID": pending.TurnID,
		"runID":  pending.RunID,
		"stepID": pending.StepID,
		"kind":   pending.Kind,
	}
	if pending.NodeID != "" {
		bounded["nodeID"] = pending.NodeID
	}
	truncated := false
	putString := func(key, value string, budget int) {
		if value == "" {
			return
		}
		bounded[key] = truncateInteractionPreviewString(value, budget)
		if len(value) > budget {
			bounded[key+"_preview_truncated"] = true
			truncated = true
		}
	}
	putString("title", pending.Title, runstate.PreviewQueryResultCellCharLimit)
	putString("prompt", pending.Prompt, runstate.PreviewInteractionPromptCharBudget)
	if pending.Multiple {
		bounded["multiple"] = true
	}
	if pending.HostAction != nil {
		// ExecuteHostAction validates generic depth/node/string/container/byte
		// limits before a request reaches this preview frame.
		bounded["host_action"] = pending.HostAction
	}
	if pending.Kind == "host_action" {
		bounded["correlationID"] = pending.CorrelationID
	}
	if pending.Debug != nil {
		debugPayload, debugTruncated := previewDebugBreakPayload(pending.Debug)
		bounded["debug"] = debugPayload
		if debugTruncated {
			bounded["debug_preview_truncated"] = true
			truncated = true
		}
	}
	if pending.Min != 0 {
		bounded["min"] = pending.Min
	}
	if pending.Max != 0 {
		bounded["max"] = pending.Max
	}
	if len(pending.Options) > 0 {
		options := make([]map[string]any, 0, min(len(pending.Options), runstate.PreviewCapturesMaxEntries))
		omitted := 0
		for i, option := range pending.Options {
			item := map[string]any{"value": fmt.Sprintf("o:%d", i)}
			displayValue := truncateInteractionPreviewString(option.Value, runstate.PreviewQueryResultCellCharLimit)
			if option.Value != displayValue {
				item["display_value"] = displayValue
				item["value_preview_truncated"] = true
				truncated = true
			}
			if option.Label != "" {
				item["label"] = truncateInteractionPreviewString(option.Label, runstate.PreviewQueryResultCellCharLimit)
				if option.Label != item["label"] {
					item["label_preview_truncated"] = true
					truncated = true
				}
			}
			if option.Hint != "" {
				item["hint"] = truncateInteractionPreviewString(option.Hint, runstate.PreviewQueryResultCellCharLimit)
				if option.Hint != item["hint"] {
					item["hint_preview_truncated"] = true
					truncated = true
				}
			}
			candidate := append(options, item)
			bounded["options"] = candidate
			if jsonSizeForInteractionPreview(bounded) > previewPendingInteractionTotalCharBudget || len(candidate) > runstate.PreviewCapturesMaxEntries {
				omitted++
				bounded["options"] = options
				truncated = true
				continue
			}
			options = candidate
		}
		if omitted > 0 || len(options) < len(pending.Options) {
			bounded["options_preview_truncated"] = true
			bounded["options_preview_omitted"] = len(pending.Options) - len(options)
			truncated = true
		}
	}
	if len(pending.Routes) > 0 {
		routes := make([]map[string]any, 0, min(len(pending.Routes), runstate.PreviewCapturesMaxEntries))
		for i, route := range pending.Routes {
			item := map[string]any{"label": fmt.Sprintf("r:%d", i)}
			displayLabel := truncateInteractionPreviewString(route.Label, runstate.PreviewQueryResultCellCharLimit)
			item["display_label"] = displayLabel
			if route.Label != displayLabel {
				item["label_preview_truncated"] = true
				truncated = true
			}
			if route.Hint != "" {
				item["hint"] = truncateInteractionPreviewString(route.Hint, runstate.PreviewQueryResultCellCharLimit)
				if route.Hint != item["hint"] {
					item["hint_preview_truncated"] = true
					truncated = true
				}
			}
			candidate := append(routes, item)
			bounded["routes"] = candidate
			if jsonSizeForInteractionPreview(bounded) > previewPendingInteractionTotalCharBudget || len(candidate) > runstate.PreviewCapturesMaxEntries {
				bounded["routes"] = routes
				truncated = true
				continue
			}
			routes = candidate
		}
		if len(routes) < len(pending.Routes) {
			bounded["routes_preview_truncated"] = true
			bounded["routes_preview_omitted"] = len(pending.Routes) - len(routes)
			truncated = true
		}
	}
	if len(pending.Fields) > 0 {
		fields := make([]map[string]any, 0, min(len(pending.Fields), runstate.PreviewCapturesMaxEntries))
		for fieldIdx, field := range pending.Fields {
			item := map[string]any{
				"name": fmt.Sprintf("f:%d", fieldIdx),
				"type": truncateInteractionPreviewString(field.Type, runstate.PreviewQueryResultCellCharLimit),
			}
			displayName := truncateInteractionPreviewString(field.Name, runstate.PreviewQueryResultCellCharLimit)
			item["display_name"] = displayName
			if field.Name != displayName {
				item["name_preview_truncated"] = true
				truncated = true
			}
			if field.Type != item["type"] {
				item["type_preview_truncated"] = true
				truncated = true
			}
			if field.Required {
				item["required"] = true
			}
			if field.Multiple {
				item["multiple"] = true
			}
			if field.Ephemeral {
				item["ephemeral"] = true
			}
			if field.Validation != nil {
				item["validation"] = runstate.PreviewEventPayload(map[string]any{"value": field.Validation})["value"]
			}
			if field.FromStep != "" {
				item["fromStep"] = truncateInteractionPreviewString(field.FromStep, runstate.PreviewQueryResultCellCharLimit)
				if field.FromStep != item["fromStep"] {
					item["fromStep_preview_truncated"] = true
					truncated = true
				}
			}
			if field.Label != "" {
				item["label"] = truncateInteractionPreviewString(field.Label, runstate.PreviewQueryResultCellCharLimit)
				if field.Label != item["label"] {
					item["label_preview_truncated"] = true
					truncated = true
				}
			}
			if field.Hint != "" {
				item["hint"] = truncateInteractionPreviewString(field.Hint, runstate.PreviewQueryResultCellCharLimit)
				if field.Hint != item["hint"] {
					item["hint_preview_truncated"] = true
					truncated = true
				}
			}
			if field.Default != nil {
				item["default"] = runstate.PreviewEventPayload(map[string]any{"value": field.Default})["value"]
				if jsonSizeForInteractionPreview(field.Default) > runstate.PreviewCaptureValueCharBudget {
					item["default_preview_truncated"] = true
					truncated = true
				}
			}
			if len(field.Options) > 0 {
				fieldOptions := make([]map[string]any, 0, min(len(field.Options), runstate.PreviewCapturesMaxEntries))
				for optionIdx, option := range field.Options {
					opt := map[string]any{"value": fmt.Sprintf("f%do:%d", fieldIdx, optionIdx)}
					displayValue := truncateInteractionPreviewString(option.Value, runstate.PreviewQueryResultCellCharLimit)
					if option.Value != displayValue {
						opt["display_value"] = displayValue
						opt["value_preview_truncated"] = true
						truncated = true
					}
					if option.Label != "" {
						opt["label"] = truncateInteractionPreviewString(option.Label, runstate.PreviewQueryResultCellCharLimit)
						if option.Label != opt["label"] {
							opt["label_preview_truncated"] = true
							truncated = true
						}
					}
					if option.Hint != "" {
						opt["hint"] = truncateInteractionPreviewString(option.Hint, runstate.PreviewQueryResultCellCharLimit)
						if option.Hint != opt["hint"] {
							opt["hint_preview_truncated"] = true
							truncated = true
						}
					}
					candidateOptions := append(fieldOptions, opt)
					item["options"] = candidateOptions
					if jsonSizeForInteractionPreview(item) > runstate.PreviewCaptureValueCharBudget || len(candidateOptions) > runstate.PreviewCapturesMaxEntries {
						item["options"] = fieldOptions
						truncated = true
						continue
					}
					fieldOptions = candidateOptions
				}
				if len(fieldOptions) < len(field.Options) {
					item["options_preview_truncated"] = true
					item["options_preview_omitted"] = len(field.Options) - len(fieldOptions)
					truncated = true
				}
			}
			candidate := append(fields, item)
			bounded["fields"] = candidate
			if jsonSizeForInteractionPreview(bounded) > previewPendingInteractionTotalCharBudget || len(candidate) > runstate.PreviewCapturesMaxEntries {
				bounded["fields"] = fields
				truncated = true
				continue
			}
			fields = candidate
		}
		if len(fields) < len(pending.Fields) {
			bounded["fields_preview_truncated"] = true
			bounded["fields_preview_omitted"] = len(pending.Fields) - len(fields)
			truncated = true
		}
	}
	if truncated {
		bounded["preview_truncated"] = true
	}
	return bounded
}

func previewDebugBreakPayload(debug *DebugBreakPayload) (map[string]any, bool) {
	if debug == nil {
		return nil, false
	}
	payload := map[string]any{
		"phase":      debug.Phase,
		"invocation": debug.Invocation,
		"attempt":    debug.Attempt,
	}
	if len(debug.CallPath) > 0 {
		payload["callPath"] = debug.CallPath
	}
	if len(debug.ProtectedVariables) > 0 {
		payload["protectedVariables"] = debug.ProtectedVariables
	}
	if debug.OutputProtected {
		payload["outputProtected"] = true
	}
	if debug.CanStepInto {
		payload["canStepInto"] = true
	}
	truncated := false
	if debug.Variables != nil {
		value, valueTruncated := previewDebugValue(debug.Variables)
		payload["variables"] = value
		truncated = truncated || valueTruncated
	}
	if debug.Actual != nil {
		actual := map[string]any{"status": debug.Actual.Status}
		if debug.Actual.Error != "" {
			value, valueTruncated := previewDebugValue(debug.Actual.Error)
			actual["error"] = value
			truncated = truncated || valueTruncated
		}
		if debug.Actual.Output != nil {
			value, valueTruncated := previewDebugValue(debug.Actual.Output)
			actual["output"] = value
			truncated = truncated || valueTruncated
		}
		payload["actual"] = actual
	}
	if len(debug.Watches) > 0 {
		value, valueTruncated := previewDebugValue(debug.Watches)
		payload["watches"] = value
		truncated = truncated || valueTruncated
	}
	return payload, truncated
}

func previewDebugValue(value any) (any, bool) {
	projected := runstate.PreviewEventPayload(map[string]any{"value": value})
	return projected["value"], projected["value_preview_truncated"] == true
}

func truncateInteractionPreviewString(text string, budget int) string {
	if len(text) <= budget {
		return text
	}
	if budget <= 3 {
		return strings.Repeat(".", budget)
	}
	return text[:budget-3] + "..."
}

func jsonSizeForInteractionPreview(value any) int {
	b, err := json.Marshal(value)
	if err != nil {
		return len(fmt.Sprint(value))
	}
	return len(b)
}

func writeInteractionFrame(w io.Writer, frame any) error {
	var id int64
	switch f := frame.(type) {
	case PendingInteraction:
		id = f.ID
	case ResolvedInteraction:
		id = f.ID
	}
	data, err := json.Marshal(previewInteractionFrame(frame))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, data); err != nil {
		return err
	}
	return nil
}

// writeRunbookError translates a parser/planner error into a REST HTTP
// error response. The body is JSON so a client can key off "code" (the
// engine's errkit code, e.g. "ENUM-008") rather than parsing prose
// (CE-V-03: "Non-2xx with the code present in the body"; AR-CE-4 §5). For
// coded errors, "error" is the safe, generic message from mapRunbookError
// -- never the raw err.Error() text, which for ENUM-008 would otherwise
// carry the rejected value. Uncoded errors (plain parse/plan failures)
// still surface their full detail: nothing sensitive travels through
// those paths today, and the detail is useful for debugging a malformed
// runbook.
func writeRunbookError(w http.ResponseWriter, err error) {
	code, msg, engineCode := mapRunbookError(err)
	httpStatus := http.StatusInternalServerError
	switch code {
	case rpcInvalidParams, rpcRunbookParseErr, rpcRunbookInvalid, rpcPlanError:
		httpStatus = http.StatusBadRequest
	case rpcRunbookNotFound:
		httpStatus = http.StatusNotFound
	}
	body := map[string]string{"error": msg}
	if engineCode != "" {
		body["code"] = engineCode
	} else {
		body["error"] = msg + ": " + err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(body)
}

// newRunID generates a UUID-shaped run id for use when a plan does not
// already carry one. Reuses the broker's turn-id generator format.
func newRunID() string {
	return "run-" + newTurnID()
}
