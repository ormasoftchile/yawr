package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603

	rpcRunbookNotFound  = -32000
	rpcRunbookParseErr  = -32001
	rpcRunbookInvalid   = -32002
	rpcPlanError        = -32003
	rpcRunNotFound      = -32010
	rpcRunNotActive     = -32011
	rpcRunCompleted     = -32012
	rpcRunLocked        = -32013
	rpcRunDeleteRunning = -32020
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    *rpcErrorData `json:"data,omitempty"`
}

// rpcErrorData carries structured, machine-readable error detail alongside
// the JSON-RPC numeric code (AR-CE-4 §5, T-SERVE-ENUM-ERRCODE): Code is
// the engine's own errkit code (e.g. "ENUM-008"), classified via
// errkit.Coder rather than lowercased message-substring matching (D-3).
// Never populated with a rejected value or a redacted member list.
type rpcErrorData struct {
	Code string `json:"code,omitempty"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &rpcError{Code: rpcParseError, Message: "Parse error"},
		})
		return
	}
	if err := ensureNoExtraJSON(decoder); err != nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &rpcError{Code: rpcParseError, Message: "Parse error"},
		})
		return
	}

	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidRequest, Message: "Invalid Request"},
		})
		return
	}

	switch req.Method {
	case "run.start":
		s.handleRunStart(w, r, req)
	case "run.next":
		s.handleRunNext(w, r, req)
	case "run.cancel":
		s.handleRunCancel(w, r, req)
	case "run.status":
		s.handleRunStatus(w, r, req)
	case "run.list":
		s.handleRunList(w, r, req)
	case "run.get":
		s.handleRunGet(w, r, req)
	case "run.evidence":
		s.handleRunEvidence(w, r, req)
	case "run.resume":
		s.handleRunResume(w, r, req)
	case "run.delete":
		s.handleRunDelete(w, r, req)
	default:
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcMethodNotFound, Message: "Method not found"},
		})
	}
}

func (s *Server) handleRunStart(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		RunbookPath string         `json:"runbookPath"`
		Inputs      map[string]any `json:"inputs"`
		Mode        string         `json:"mode"`
		Actor       string         `json:"actor"`
	}
	if err := decodeParams(req.Params, &params); err != nil || strings.TrimSpace(params.RunbookPath) == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
		return
	}

	vars, err := normalizeInputs(params.Inputs)
	if err != nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
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
			writeRPC(w, rpcResponse{
				JSONRPC: "2.0",
				ID:      normalizeID(req.ID),
				Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
			})
			return
		}
	}

	actor := params.Actor
	if actor == "" {
		actor = "rpc-client"
	}

	plan, parsed, vars, warnings, err := s.loadPlanAndSeed(r.Context(), params.RunbookPath, vars)
	if err != nil {
		code, message, engineCode := mapRunbookError(err)
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   rpcErrorWithCode(code, message, engineCode),
		})
		return
	}

	startCtx, catalogWarnings, err := s.contextWithPerRunCatalog(r.Context(), plan, parsed, params.RunbookPath)
	if err != nil {
		code, message, engineCode := mapRunbookError(err)
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   rpcErrorWithCode(code, message, engineCode),
		})
		return
	}
	warnings = append(warnings, catalogWarnings...)

	runCtx, cancel := context.WithCancel(context.WithoutCancel(startCtx))
	handle, err := s.engine.Start(runCtx, plan, engine.RunOptions{
		Mode:   mode,
		Actor:  actor,
		Vars:   vars,
		Client: "server",
	})
	if err != nil {
		cancel()
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInternalError, Message: "Internal error"},
		})
		return
	}

	state := handle.State()
	if state.RunID == "" {
		state.RunID = plan.RunID
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

	result := map[string]any{"runID": entry.ID}
	if len(warnings) > 0 {
		// ENUM-W001 (AR-CE-4 §6, CE-W-02): non-fatal parse warnings MUST
		// reach a human on every surface that parses a runbook, including
		// JSON-RPC run.start. The run still starts; this is advisory.
		result["warnings"] = rpcWarnings(warnings)
	}
	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Result:  result,
	})
}

func (s *Server) handleRunNext(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		RunID string `json:"runID"`
	}
	if err := decodeParams(req.Params, &params); err != nil || strings.TrimSpace(params.RunID) == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
		return
	}

	entry, ok := s.registry.Get(params.RunID)
	if !ok || entry.Handle == nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunNotFound, Message: "Run not found"},
		})
		return
	}

	state := entry.Handle.State().Status
	if isStoppedState(state) {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunNotActive, Message: "Run not active"},
		})
		return
	}

	result, err := entry.Handle.Next(r.Context())
	if errors.Is(err, io.EOF) {
		state := s.syncRunEntryState(&entry)
		if state.Status == engine.RunStatusPausedAtBoundary {
			writeRPC(w, rpcResponse{
				JSONRPC: "2.0", ID: normalizeID(req.ID),
				Result: map[string]any{"state": string(state.Status)},
			})
			return
		}
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunCompleted, Message: "Run completed"},
		})
		return
	}
	if err != nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInternalError, Message: "Internal error"},
		})
		return
	}

	s.registry.WithEntry(entry.ID, func(e *RunEntry) {
		e.State = entry.Handle.State().Status
	})

	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Result: map[string]any{
			"stepID": result.StepID,
			"state":  string(result.Status),
			"output": result.Output,
		},
	})
}

func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		RunID  string `json:"runID"`
		Reason string `json:"reason"`
	}
	if err := decodeParams(req.Params, &params); err != nil || strings.TrimSpace(params.RunID) == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
		return
	}

	entry, ok := s.registry.Get(params.RunID)
	if !ok || entry.Handle == nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunNotFound, Message: "Run not found"},
		})
		return
	}

	if isTerminalState(entry.Handle.State().Status) {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunNotActive, Message: "Run not active"},
		})
		return
	}

	_ = entry.Handle.Cancel(r.Context(), params.Reason)
	if entry.Cancel != nil {
		entry.Cancel()
	}
	if s.broker != nil {
		s.broker.Unregister(entry.ID)
	}
	s.registry.WithEntry(entry.ID, func(e *RunEntry) {
		e.State = engine.RunStatusCancelled
		e.CompletedAt = time.Now()
	})
	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Result:  map[string]any{},
	})
}

func (s *Server) handleRunStatus(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		RunID string `json:"runID"`
	}
	if err := decodeParams(req.Params, &params); err != nil || strings.TrimSpace(params.RunID) == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
		return
	}

	entry, ok := s.registry.Get(params.RunID)
	if !ok || entry.Handle == nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunNotFound, Message: "Run not found"},
		})
		return
	}

	state := entry.Handle.State()
	result := map[string]any{
		"runID":       state.RunID,
		"state":       string(state.Status),
		"currentStep": state.CurrentStep,
		"runbookPath": state.RunbookPath,
		"startedAt":   state.StartedAt.Format(time.RFC3339Nano),
		"updatedAt":   state.UpdatedAt.Format(time.RFC3339Nano),
	}
	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Result:  result,
	})
}

// handleRunList returns a list of all runs (active and persisted).
//
// Response schema:
//
//	[
//	  {
//	    "runID":       string,  // Unique run identifier
//	    "state":       string,  // "pending"|"running"|"paused"|"completed"|"failed"|"cancelled"
//	    "runbookPath": string,  // Path to runbook file
//	    "startedAt":   string,  // RFC3339Nano timestamp
//	    "source":      string   // "active" (in-memory) or "persisted" (on-disk)
//	  }
//	]
//
// Active runs from the registry take precedence over persisted runs with the same runID.
// Store errors are silently ignored (graceful degradation).
func (s *Server) handleRunList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	activeEntries := s.registry.List()
	activeIDs := make(map[string]bool, len(activeEntries))
	response := make([]map[string]any, 0, len(activeEntries)+10)

	for _, entry := range activeEntries {
		activeIDs[entry.ID] = true
		state := engine.RunStatusPending
		startedAt := entry.StartedAt
		if entry.Handle != nil {
			handleState := entry.Handle.State()
			state = handleState.Status
			if !handleState.StartedAt.IsZero() {
				startedAt = handleState.StartedAt
			}
		}
		item := map[string]any{
			"runID":       entry.ID,
			"state":       string(state),
			"runbookPath": entry.RunbookPath,
			"startedAt":   startedAt.Format(time.RFC3339Nano),
			"source":      "active",
		}
		if !entry.CompletedAt.IsZero() {
			item["completedAt"] = entry.CompletedAt.Format(time.RFC3339Nano)
		}
		response = append(response, item)
	}

	if s.store != nil {
		if lister, ok := s.store.(interface {
			ListRuns(ctx context.Context) ([]engine.RunState, error)
		}); ok {
			persisted, err := lister.ListRuns(r.Context())
			if err == nil {
				for _, run := range persisted {
					if activeIDs[run.RunID] {
						continue
					}
					item := map[string]any{
						"runID":       run.RunID,
						"state":       string(run.Status),
						"runbookPath": run.RunbookPath,
						"startedAt":   run.StartedAt.Format(time.RFC3339Nano),
						"source":      "persisted",
					}
					if !run.CompletedAt.IsZero() {
						item["completedAt"] = run.CompletedAt.Format(time.RFC3339Nano)
					}
					response = append(response, item)
				}
			}
		}
	}

	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Result:  response,
	})
}

// handleRunGet returns the full state of a single run by ID.
//
// Params:
//
//	{"runID": string}
//
// Response schema (active run):
//
//	{
//	  "runID":            string,   // Unique run identifier
//	  "state":            string,   // "pending"|"running"|"paused"|"completed"|"failed"|"cancelled"
//	  "runbookPath":      string,   // Path to runbook file
//	  "startedAt":        string,   // RFC3339Nano timestamp
//	  "currentStep":      string,   // ID of the step currently executing (empty if none)
//	  "currentStepIndex": number,   // 0-based index into plan.steps (-1 before first step)
//	  "vars":             object,   // Runtime variable map (string→string)
//	  "completedAt":      string,   // RFC3339Nano; present only when state is terminal
//	  "source":           string    // "active"
//	}
//
// Response schema (persisted run):
//
//	{
//	  "runID":            string,
//	  "state":            string,
//	  "runbookPath":      string,
//	  "startedAt":        string,
//	  "currentStep":      string,
//	  "currentStepIndex": number,
//	  "vars":             object,
//	  "completedAt":      string,   // RFC3339Nano; present when terminal and timestamp was recorded
//	  "source":           string    // "persisted"
//	}
//
// Error: -32602 (Invalid params) if runID is missing.
// Error: -32010 (Run not found) if no active or persisted run matches runID.
func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		RunID string `json:"runID"`
	}
	if err := decodeParams(req.Params, &params); err != nil || strings.TrimSpace(params.RunID) == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
		return
	}

	if entry, ok := s.registry.Get(params.RunID); ok {
		state := engine.RunStatusPending
		currentStep := ""
		currentStepIndex := 0
		vars := map[string]any{}
		var handleState engine.RunState

		if entry.Handle != nil {
			handleState = entry.Handle.State()
			state = handleState.Status
			currentStep = handleState.CurrentStep
			currentStepIndex = handleState.CurrentStepIndex
			vars = handleState.Vars
		}

		result := map[string]any{
			"runID":            entry.ID,
			"state":            string(state),
			"runbookPath":      entry.RunbookPath,
			"startedAt":        entry.StartedAt.Format(time.RFC3339Nano),
			"currentStep":      currentStep,
			"currentStepIndex": currentStepIndex,
			"vars":             vars,
			"source":           "active",
		}
		if !entry.CompletedAt.IsZero() {
			result["completedAt"] = entry.CompletedAt.Format(time.RFC3339Nano)
		}
		record, recordErr := handleState.CloneResults()
		s.addPublicResults(r.Context(), result, record, recordErr, handleState.Status, handleState.Plan, vars, params.RunID, handleState.BindingScope != nil)
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: normalizeID(req.ID), Result: result})
		return
	}

	if s.store != nil {
		runState, err := s.store.LoadState(r.Context(), params.RunID)
		if err == nil {
			result := map[string]any{
				"runID":            runState.RunID,
				"state":            string(runState.Status),
				"runbookPath":      runState.RunbookPath,
				"startedAt":        runState.StartedAt.Format(time.RFC3339Nano),
				"currentStep":      runState.CurrentStep,
				"currentStepIndex": runState.CurrentStepIndex,
				"vars":             runState.Vars,
				"source":           "persisted",
			}
			if !runState.CompletedAt.IsZero() {
				result["completedAt"] = runState.CompletedAt.Format(time.RFC3339Nano)
			}
			record, recordErr := runState.CloneResults()
			s.addPublicResults(r.Context(), result, record, recordErr, runState.Status, runState.Plan, runState.Vars, params.RunID, runState.BindingScope != nil)
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: normalizeID(req.ID), Result: result})
			return
		}
	}

	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Error:   &rpcError{Code: rpcRunNotFound, Message: "Run not found"},
	})
}

func (s *Server) handleRunEvidence(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		RunID  string   `json:"runID"`
		StepID string   `json:"stepID"`
		Kinds  []string `json:"kinds"`
	}
	if err := decodeParams(req.Params, &params); err != nil || strings.TrimSpace(params.RunID) == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
		return
	}

	reader, err := s.traceReaderForRun(params.RunID)
	if err != nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunNotFound, Message: "Run not found"},
		})
		return
	}

	filter := tracepkg.TraceFilter{
		Kinds:  []tracepkg.EventKind{tracepkg.EventKindStepCompleted},
		StepID: params.StepID,
	}
	events, err := reader.ReadFiltered(r.Context(), filter)
	if err != nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInternalError, Message: "Internal error"},
		})
		return
	}

	result := extractEvidence(events, params.Kinds)
	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Result: map[string]any{
			"runID":    params.RunID,
			"evidence": result,
		},
	})
}

func (s *Server) handleRunResume(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		RunID string `json:"runID"`
		Actor string `json:"actor"`
	}
	if err := decodeParams(req.Params, &params); err != nil || strings.TrimSpace(params.RunID) == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
		return
	}

	actor := params.Actor
	if actor == "" {
		actor = "rpc-client"
	}
	if s.store == nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInternalError, Message: "Internal error"},
		})
		return
	}

	// Reserve before Resume: nested executors must find their broker, and a
	// failed/duplicate attachment must never unregister another writer's queue.
	if _, exists := s.registry.Get(params.RunID); exists || !s.broker.registerExclusive(params.RunID) {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0", ID: normalizeID(req.ID),
			Error: &rpcError{Code: rpcRunLocked, Message: "Run already attached"},
		})
		return
	}
	// Resume retains this context for subsequent steps. The HTTP request ends
	// when attachment returns, not when the resumed run finishes.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	handle, err := s.engine.Resume(runCtx, params.RunID, engine.RunOptions{
		Mode:  engine.RunModeReal,
		Actor: actor,
		Store: s.store,
	})
	if err != nil {
		cancel()
		s.broker.Unregister(params.RunID)
		code := rpcInternalError
		msg := "Internal error"
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "not found") {
			code = rpcRunNotFound
			msg = "Run not found"
		} else if errors.Is(err, engine.ErrRunLeaseHeld) || strings.Contains(err.Error(), "locked") {
			code = rpcRunLocked
			msg = "Run locked by another process"
		}
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: code, Message: msg},
		})
		return
	}

	state := handle.State()
	entry := &RunEntry{
		ID:          params.RunID,
		RunbookPath: state.RunbookPath,
		Handle:      handle,
		Cancel:      cancel,
		State:       state.Status,
		StartedAt:   state.StartedAt,
	}
	s.registry.Add(entry)
	s.wg.Add(1)
	go s.pumpEvents(runCtx, entry)

	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Result: map[string]any{
			"runID":           params.RunID,
			"resumedFromStep": state.CurrentStep,
		},
	})
}

func (s *Server) handleRunDelete(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		RunID string `json:"runID"`
	}
	if err := decodeParams(req.Params, &params); err != nil || strings.TrimSpace(params.RunID) == "" {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInvalidParams, Message: "Invalid params"},
		})
		return
	}

	// Safety invariant: never delete a run that is actively running in memory.
	if entry, ok := s.registry.Get(params.RunID); ok {
		status := engine.RunStatusPending
		if entry.Handle != nil {
			status = entry.Handle.State().Status
		}
		if status == engine.RunStatusRunning {
			writeRPC(w, rpcResponse{
				JSONRPC: "2.0",
				ID:      normalizeID(req.ID),
				Error:   &rpcError{Code: rpcRunDeleteRunning, Message: "Cannot delete running run"},
			})
			return
		}
	}

	if s.store == nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunNotFound, Message: "Run not found"},
		})
		return
	}

	runState, err := s.store.LoadState(r.Context(), params.RunID)
	if err != nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunNotFound, Message: "Run not found"},
		})
		return
	}

	// Also guard against persisted running state (e.g. after a server crash).
	if runState.Status == engine.RunStatusRunning {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcRunDeleteRunning, Message: "Cannot delete running run"},
		})
		return
	}

	deleter, ok := s.store.(interface {
		DeleteRun(ctx context.Context, runID string) error
	})
	if !ok {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInternalError, Message: "Internal error"},
		})
		return
	}
	if err := deleter.DeleteRun(r.Context(), params.RunID); err != nil {
		writeRPC(w, rpcResponse{
			JSONRPC: "2.0",
			ID:      normalizeID(req.ID),
			Error:   &rpcError{Code: rpcInternalError, Message: err.Error()},
		})
		return
	}

	writeRPC(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(req.ID),
		Result:  map[string]bool{"deleted": true},
	})
}

func (s *Server) loadPlan(ctx context.Context, path string) (*engine.ExecutionPlan, error) {
	plan, _, _, _, err := s.loadPlanAndSeed(ctx, path, nil)
	return plan, err
}

// loadPlanAndSeed parses the runbook, plans it, and seeds runbook-level
// vars and input defaults into the supplied vars map (caller-supplied
// values win over runbook defaults). Returns the plan and the resulting
// vars (a copy if userVars was nil; otherwise userVars mutated in place
// and returned for convenience).
func (s *Server) loadPlanAndSeed(ctx context.Context, path string, userVars map[string]string) (*engine.ExecutionPlan, *parser.ParsedRunbook, map[string]string, []parser.ParseWarning, error) {
	rb, err := s.parser.Parse(ctx, path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	plan, err := s.planner.Plan(ctx, rb)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if userVars == nil {
		userVars = map[string]string{}
	}
	if rb != nil && rb.Runbook != nil {
		// ENUM-008 (AR-ENUM-7, barbara-enum-mvp-implementation-gate.md
		// R1): this is a live, unguarded caller-binding path -- client-
		// supplied params.Inputs are merged into userVars by the two RPC
		// handlers that call this function before defaults are applied
		// below. Checked here, before defaults are seeded, so only the
		// genuinely caller-supplied values are validated (defaults were
		// already checked plan-time by ENUM-006).
		if err := schema.CheckCallerInputBindings(rb.Runbook.Inputs, userVars); err != nil {
			return nil, nil, nil, nil, err
		}
		for k, v := range rb.Runbook.Vars {
			if _, ok := userVars[k]; ok {
				continue
			}
			if str, ok := v.(string); ok {
				userVars[k] = str
			} else {
				userVars[k] = fmt.Sprint(v)
			}
		}
		for name, in := range rb.Runbook.Inputs {
			if in == nil {
				continue
			}
			if _, ok := userVars[name]; ok {
				continue
			}
			if in.Default != nil {
				userVars[name] = fmt.Sprint(in.Default)
			} else if !in.Required {
				userVars[name] = ""
			}
		}
	}
	var warnings []parser.ParseWarning
	if rb != nil {
		warnings = rb.Warnings
	}
	return plan, rb, userVars, warnings, nil
}

func decodeParams(raw json.RawMessage, dest any) error {
	if len(raw) == 0 {
		return json.Unmarshal([]byte("{}"), dest)
	}
	return json.Unmarshal(raw, dest)
}

func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func ensureNoExtraJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return errors.New("extra json")
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func normalizeInputs(inputs map[string]any) (map[string]string, error) {
	if inputs == nil {
		return nil, nil
	}
	out := make(map[string]string, len(inputs))
	for k, v := range inputs {
		switch val := v.(type) {
		case string:
			out[k] = val
		default:
			return nil, errors.New("inputs must be string values")
		}
	}
	return out, nil
}

// rpcWarnings converts parser.ParseWarning values into the wire shape
// clients render (AR-CE-4 §6): field + message, non-fatal, never blocking
// the run that produced them.
func rpcWarnings(in []parser.ParseWarning) []map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(in))
	for _, w := range in {
		out = append(out, map[string]string{"field": w.Field, "message": w.Message})
	}
	return out
}

// rpcErrorWithCode builds an *rpcError, attaching Data.Code only when
// engineCode is non-empty (uncoded errors carry no data.code).
func rpcErrorWithCode(code int, message, engineCode string) *rpcError {
	e := &rpcError{Code: code, Message: message}
	if engineCode != "" {
		e.Data = &rpcErrorData{Code: engineCode}
	}
	return e
}

// mapRunbookError classifies a parse/plan/binding error into an RPC error
// code, a client-safe message, and (when the error is coded) the engine's
// own errkit code for error.data.code.
//
// D-3 (barbara-client-enum-compatibility-ruling.md): this function used to
// classify by lowercased message substring, which is why an ENUM-008 value
// rejection ("input %q value %q is not a declared enum member") fell
// through every substring branch and was reported to the client as
// "Parse error" -- the operator's *file* was fine, their *value* was
// rejected, and the two presentations must never be conflated (AR-CE-4
// §7). Message-substring classification of a *coded* error (one that
// implements errkit.Coder) is banned going forward: coded errors are
// classified by errors.As(err, &coder) and dispatched on Code(), never by
// inspecting err.Error()'s text. The substring fallback below remains only
// for the genuinely uncoded errors this runtime still produces (raw YAML
// decode errors, ValidationErrors, etc.).
func mapRunbookError(err error) (code int, message string, engineCode string) {
	if err == nil {
		return rpcInternalError, "Internal error", ""
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return rpcRunbookNotFound, "Runbook not found", ""
	}
	var planErr *planner.PlanError
	if errors.As(err, &planErr) {
		return rpcPlanError, "Plan error", ""
	}
	var coder errkit.Coder
	if errors.As(err, &coder) {
		ec := coder.Code()
		return rpcRunbookInvalid, safeMessageForCode(ec), ec
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "yaml/parse") || strings.Contains(msg, "yaml:") || strings.Contains(msg, "parser: unmarshal") {
		return rpcRunbookParseErr, "Parse error", ""
	}
	if strings.Contains(msg, "[") || strings.Contains(msg, "validation") {
		return rpcRunbookInvalid, "Validation failed", ""
	}
	return rpcRunbookParseErr, "Parse error", ""
}

// safeMessageForCode returns a client-safe, generic message for a coded
// error class. It deliberately never echoes the underlying error's own
// message text: an errkit-coded error's message may have been written for
// CLI stderr / logs, not for a wire payload an untrusted client renders
// verbatim, and in particular MUST NOT ever carry a rejected value or a
// redacted enum member list (C1; AR-CE-4 §5, AR-CE-6 §4). The client is
// expected to key its own copy off error.data.code, not off this string.
func safeMessageForCode(code string) string {
	switch {
	case code == "ENUM-008":
		return "The submitted value is not a declared enum member."
	case code == "ENUM-009":
		return "A declared output value is not a declared enum member."
	case strings.HasPrefix(code, "ENUM-W"):
		return "Non-fatal enum warning."
	case strings.HasPrefix(code, "ENUM-"):
		return "The runbook's enum declaration is invalid."
	case strings.HasPrefix(code, "PKG-"):
		return "Package resolution failed."
	case strings.HasPrefix(code, "PLAN-"):
		return "Plan error"
	default:
		return "Validation failed"
	}
}

func (s *Server) traceReaderForRun(runID string) (tracepkg.TraceReader, error) {
	if s.store == nil {
		return nil, errors.New("trace reader unavailable")
	}
	pathProvider, ok := s.store.(interface{ TracePath(string) string })
	if !ok {
		return nil, errors.New("trace reader unavailable")
	}
	tracePath := pathProvider.TracePath(runID)
	if tracePath == "" {
		return nil, errors.New("trace reader unavailable")
	}
	return internaltrace.NewJSONLReader(tracePath), nil
}

func extractEvidence(events []tracepkg.TraceEvent, kinds []string) []evidencepkg.EvidenceSet {
	allowed := make(map[evidencepkg.EvidenceKind]bool)
	for _, kind := range kinds {
		allowed[evidencepkg.EvidenceKind(kind)] = true
	}

	sets := make([]evidencepkg.EvidenceSet, 0)
	for _, ev := range events {
		var payload struct {
			StepID   string                       `json:"step_id"`
			Evidence []evidencepkg.EvidenceRecord `json:"evidence"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if payload.StepID == "" || len(payload.Evidence) == 0 {
			continue
		}
		records := payload.Evidence
		if len(allowed) > 0 {
			filtered := records[:0]
			for _, record := range records {
				if allowed[record.Kind] {
					filtered = append(filtered, record)
				}
			}
			records = filtered
		}
		if len(records) == 0 {
			continue
		}
		sets = append(sets, evidencepkg.EvidenceSet{
			StepID:  payload.StepID,
			Records: records,
		})
	}
	return sets
}

func toRunEvent(ev engine.Event) servepkg.RunEvent {
	return servepkg.RunEvent{
		Type:     ev.Kind,
		RunID:    ev.RunID,
		Sequence: ev.Sequence,
		TS:       ev.Timestamp,
		Payload:  runstate.PreviewEventPayload(ev.Payload),
	}
}
