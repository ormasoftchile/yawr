package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	internalgov "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	pkgGov "github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// StandaloneDispatchStore records and manages durable dispatch intents independently of a workflow run.
type StandaloneDispatchStore interface {
	engine.DispatchCommitter
	SettleDispatch(ctx context.Context, occurrenceID string, result any) error
	LookupDispatch(ctx context.Context, occurrenceID string) (*engine.DispatchState, bool)
	LookupByIdempotencyKey(ctx context.Context, key string) (*engine.DispatchState, bool)
	MarkIndeterminate(ctx context.Context, occurrenceID string) error
}

// InMemoryDispatchStore provides an in-memory implementation of StandaloneDispatchStore.
type InMemoryDispatchStore struct {
	mu         sync.Mutex
	dispatches map[string]*engine.DispatchState
	results    map[string]any
	byKey      map[string]string // IdempotencyKey -> OccurrenceID
}

// NewInMemoryDispatchStore constructs a new InMemoryDispatchStore.
func NewInMemoryDispatchStore() *InMemoryDispatchStore {
	return &InMemoryDispatchStore{
		dispatches: make(map[string]*engine.DispatchState),
		results:    make(map[string]any),
		byKey:      make(map[string]string),
	}
}

// PrepareDispatch records a durable intent before provider I/O.
func (s *InMemoryDispatchStore) PrepareDispatch(ctx context.Context, request engine.DispatchRequest) (engine.DispatchState, error) {
	if err := ctx.Err(); err != nil {
		return engine.DispatchState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	classification := request.Classification
	if classification == "" {
		classification = "unspecified"
	}

	rendered, err := json.Marshal(request.RenderedRequest)
	if err != nil {
		return engine.DispatchState{}, fmt.Errorf("dispatch: encode rendered request: %w", err)
	}
	requestDigest := engine.InteractionPayloadDigest(rendered)

	// Derive or accept idempotency key
	idempotencyKey := ""
	if reqMap, ok := request.RenderedRequest.(map[string]any); ok {
		if rawKey, hasKey := reqMap["idempotency_key"].(string); hasKey && rawKey != "" {
			idempotencyKey = rawKey
		}
	}
	if idempotencyKey == "" {
		idempotencyKey = engine.InteractionPayloadDigest([]byte("adhoc.dispatch/v1\x00" + requestDigest))
	}

	// Check if this idempotency key was previously seen
	if prevOccID, exists := s.byKey[idempotencyKey]; exists {
		prev := s.dispatches[prevOccID]
		if prev != nil {
			if prev.RequestDigest != requestDigest {
				return *prev, fmt.Errorf("%w: idempotency key %q was previously used with different request parameters", toolpkg.ErrIdempotencyConflict, idempotencyKey)
			}
			if prev.Status == engine.DispatchStatusPrepared {
				// Interrupted prior execution: prevent unsafe unconfirmed replay
				prev.Status = engine.DispatchStatusIndeterminate
				return *prev, fmt.Errorf("%w: unmatched dispatch intent %s", engine.ErrIndeterminate, prev.OccurrenceID)
			}
			if prev.Status == engine.DispatchStatusSettled {
				// Already settled: return existing state for replay
				return *prev, nil
			}
			if prev.Status == engine.DispatchStatusIndeterminate {
				return *prev, fmt.Errorf("%w: unmatched dispatch intent %s", engine.ErrIndeterminate, prev.OccurrenceID)
			}
		}
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	occIDBytes := sha256.Sum256([]byte(idempotencyKey + "\x00" + nowStr))
	occID := hex.EncodeToString(occIDBytes[:16])

	state := engine.DispatchState{
		SchemaVersion:                  engine.DispatchStateSchemaV1,
		OccurrenceID:                   occID,
		Classification:                 classification,
		EndpointIdentity:               request.EndpointIdentity,
		RequestDigest:                  requestDigest,
		IdempotencyKey:                 idempotencyKey,
		ProviderSupportsReconciliation: request.ProviderSupportsReconciliation,
		Status:                         engine.DispatchStatusPrepared,
		PreparedAt:                     nowStr,
	}

	s.dispatches[occID] = &state
	s.byKey[idempotencyKey] = occID
	return state, nil
}

// SettleDispatch updates a dispatch to settled with the result digest.
func (s *InMemoryDispatchStore) SettleDispatch(ctx context.Context, occurrenceID string, result any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	disp, ok := s.dispatches[occurrenceID]
	if !ok {
		return fmt.Errorf("dispatch: occurrence %s not found", occurrenceID)
	}

	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("dispatch: marshal result for settle: %w", err)
	}

	disp.Status = engine.DispatchStatusSettled
	disp.ResultDigest = engine.InteractionPayloadDigest(payload)
	disp.SettledAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.results[occurrenceID] = result
	return nil
}

// LookupDispatch returns a recorded dispatch by occurrence ID.
func (s *InMemoryDispatchStore) LookupDispatch(ctx context.Context, occurrenceID string) (*engine.DispatchState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	disp, ok := s.dispatches[occurrenceID]
	if !ok {
		return nil, false
	}
	out := *disp
	return &out, true
}

// LookupByIdempotencyKey returns a recorded dispatch by idempotency key.
func (s *InMemoryDispatchStore) LookupByIdempotencyKey(ctx context.Context, key string) (*engine.DispatchState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	occID, ok := s.byKey[key]
	if !ok {
		return nil, false
	}
	disp := s.dispatches[occID]
	if disp == nil {
		return nil, false
	}
	out := *disp
	return &out, true
}

// MarkIndeterminate flags a dispatch occurrence as indeterminate.
func (s *InMemoryDispatchStore) MarkIndeterminate(ctx context.Context, occurrenceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	disp, ok := s.dispatches[occurrenceID]
	if !ok {
		return fmt.Errorf("dispatch: occurrence %s not found", occurrenceID)
	}
	disp.Status = engine.DispatchStatusIndeterminate
	return nil
}

// InvokerConfig configures a DefaultInvoker.
type InvokerConfig struct {
	Registry          toolpkg.ToolRegistry
	Runtime           toolpkg.ToolRuntime
	ApprovalGate      pkgGov.ApprovalGate
	EvidenceCollector engine.EvidenceHook
	DispatchStore     StandaloneDispatchStore
}

// DefaultInvoker implements toolpkg.ToolInvoker.
type DefaultInvoker struct {
	registry          toolpkg.ToolRegistry
	runtime           toolpkg.ToolRuntime
	approvalGate      pkgGov.ApprovalGate
	evidenceCollector engine.EvidenceHook
	dispatchStore     StandaloneDispatchStore
}

// NewInvoker constructs a new DefaultInvoker.
func NewInvoker(cfg InvokerConfig) toolpkg.ToolInvoker {
	reg := cfg.Registry
	if reg == nil {
		reg = NewBuiltinRegistry()
	}
	rt := cfg.Runtime
	if rt == nil {
		rt = NewDefaultToolRuntime(reg)
	}
	gate := cfg.ApprovalGate
	if gate == nil {
		gate = internalgov.NewNoOpApprovalGate()
	}
	store := cfg.DispatchStore
	if store == nil {
		store = NewInMemoryDispatchStore()
	}
	return &DefaultInvoker{
		registry:          reg,
		runtime:           rt,
		approvalGate:      gate,
		evidenceCollector: cfg.EvidenceCollector,
		dispatchStore:     store,
	}
}

func init() {
	toolpkg.RegisterInvokerFactory(func(opts toolpkg.InvokerOptions) (toolpkg.ToolInvoker, error) {
		var store StandaloneDispatchStore
		if opts.DispatchStore != nil {
			if s, ok := opts.DispatchStore.(StandaloneDispatchStore); ok {
				store = s
			}
		} else if opts.StateDir != "" {
			fs, err := NewFileDispatchStore(opts.StateDir)
			if err != nil {
				return nil, err
			}
			store = fs
		} else {
			store = NewInMemoryDispatchStore()
		}

		reg := opts.Registry
		if reg == nil && opts.Runtime != nil {
			if rtReg, ok := opts.Runtime.(interface{ Registry() toolpkg.ToolRegistry }); ok && rtReg != nil {
				reg = rtReg.Registry()
			}
		}
		if reg == nil {
			reg = NewBuiltinRegistry()
		}
		rt := opts.Runtime
		if rt == nil {
			rt = NewDefaultToolRuntime(reg)
		}

		return NewInvoker(InvokerConfig{
			Registry:      reg,
			Runtime:       rt,
			ApprovalGate:  opts.ApprovalGate,
			DispatchStore: store,
		}), nil
	})
}

// Invoke executes a tool invocation with full governance, authorization, durable intent, and evidence guarantees.
func (inv *DefaultInvoker) Invoke(ctx context.Context, req toolpkg.InvocationRequest) (*toolpkg.InvocationResult, error) {
	startedAt := time.Now()
	if req.Tool == "" {
		return nil, errors.New("tool invoker: tool name is required")
	}
	if req.Action == "" {
		return nil, errors.New("tool invoker: action name is required")
	}

	// 1. Tool and Action Resolution
	def, found := inv.registry.Lookup(req.Tool)
	if !found || def == nil {
		return &toolpkg.InvocationResult{
			Status:       toolpkg.InvocationStatusFailed,
			Error:        fmt.Errorf("tool invoker: tool %q not found in registry", req.Tool),
			ErrorMessage: fmt.Sprintf("tool %q not found in registry", req.Tool),
			DurationMs:   time.Since(startedAt).Milliseconds(),
		}, nil
	}

	actionDef, actionFound := def.Actions[req.Action]
	if !actionFound || actionDef == nil {
		return &toolpkg.InvocationResult{
			Status:       toolpkg.InvocationStatusFailed,
			Error:        fmt.Errorf("tool invoker: action %q not found on tool %q", req.Action, req.Tool),
			ErrorMessage: fmt.Sprintf("action %q not found on tool %q", req.Action, req.Tool),
			DurationMs:   time.Since(startedAt).Milliseconds(),
		}, nil
	}

	// 2. Authority tool allowlist check
	if len(req.Authority.AllowedTools) > 0 {
		allowed := false
		for _, pattern := range req.Authority.AllowedTools {
			if pattern == req.Tool || pattern == "*" || (strings.HasSuffix(pattern, "*") && strings.HasPrefix(req.Tool, strings.TrimSuffix(pattern, "*"))) {
				allowed = true
				break
			}
		}
		if !allowed {
			err := fmt.Errorf("tool invoker: tool %q is not permitted by caller authority", req.Tool)
			return &toolpkg.InvocationResult{
				Status:       toolpkg.InvocationStatusDenied,
				Error:        err,
				ErrorMessage: err.Error(),
				DurationMs:   time.Since(startedAt).Milliseconds(),
			}, nil
		}
	}

	// 3. Arguments preparation & validation
	args := make(map[string]any, len(req.Arguments))
	for k, v := range req.Arguments {
		args[k] = v
	}
	// Apply action argument defaults
	for name, arg := range actionDef.Args {
		if _, supplied := args[name]; !supplied && arg != nil && arg.Default != nil {
			args[name] = arg.Default
		}
	}

	if err := validateToolArguments(actionDef, args); err != nil {
		return &toolpkg.InvocationResult{
			Status:       toolpkg.InvocationStatusFailed,
			Error:        err,
			ErrorMessage: err.Error(),
			DurationMs:   time.Since(startedAt).Milliseconds(),
		}, nil
	}

	// 4. Action classification
	classification := "unspecified"
	if actionDef.Classification != nil && *actionDef.Classification != "" {
		classification = *actionDef.Classification
	} else if sa := actionDef.SchemaAction(); sa != nil && sa.Classification != nil && *sa.Classification != "" {
		classification = *sa.Classification
	}
	// Authority classification capability check
	switch classification {
	case "read-only":
		if !req.Authority.AllowRead {
			err := fmt.Errorf("tool invoker: read-only action %q not permitted by caller authority (allow_read=false)", req.Tool+"/"+req.Action)
			return &toolpkg.InvocationResult{
				Status:         toolpkg.InvocationStatusDenied,
				Classification: classification,
				Error:          err,
				ErrorMessage:   err.Error(),
				DurationMs:     time.Since(startedAt).Milliseconds(),
			}, nil
		}
	case "mutating":
		if !req.Authority.AllowMutating {
			err := fmt.Errorf("tool invoker: mutating action %q not permitted by caller authority (allow_mutating=false)", req.Tool+"/"+req.Action)
			return &toolpkg.InvocationResult{
				Status:         toolpkg.InvocationStatusDenied,
				Classification: classification,
				Error:          err,
				ErrorMessage:   err.Error(),
				DurationMs:     time.Since(startedAt).Milliseconds(),
			}, nil
		}
	case "destructive":
		if !req.Authority.AllowDestructive {
			err := fmt.Errorf("tool invoker: destructive action %q not permitted by caller authority (allow_destructive=false)", req.Tool+"/"+req.Action)
			return &toolpkg.InvocationResult{
				Status:         toolpkg.InvocationStatusDenied,
				Classification: classification,
				Error:          err,
				ErrorMessage:   err.Error(),
				DurationMs:     time.Since(startedAt).Milliseconds(),
			}, nil
		}
	}

	// 5. Governance Evaluation
	stepInfo := pkgGov.StepInfo{
		ID:                   req.Tool + "/" + req.Action,
		Kind:                 "tool",
		Command:              def.Command,
		ToolRequiresApproval: def.Governance != nil && def.Governance.RequiresApproval != nil && *def.Governance.RequiresApproval,
		ToolClassification:   &classification,
	}
	if def.Governance != nil {
		stepInfo.ToolApprovalTriState = def.Governance.RequiresApproval
	}

	evaluator := req.Authority.PolicyEvaluator
	if evaluator == nil {
		gate := req.Authority.ApprovalGate
		if gate == nil {
			gate = inv.approvalGate
		}
		base := internalgov.BuildEvaluator(gate)
		if req.Authority.Context != "" || req.Authority.Attendance != "" {
			profile := &schema.RuntimeProfile{
				Context:    req.Authority.Context,
				Attendance: req.Authority.Attendance,
				Approval: schema.ProfileApproval{
					Scope: schema.ProfileApprovalScope{
						AllowRead:        req.Authority.AllowRead,
						AllowMutating:    req.Authority.AllowMutating,
						AllowDestructive: req.Authority.AllowDestructive,
					},
				},
			}
			evaluator = internalgov.NewProfileEvaluator(base, profile)
		} else {
			evaluator = base
		}
	}

	evalResult, evalErr := evaluator.Evaluate(ctx, stepInfo)
	if evalErr != nil {
		return &toolpkg.InvocationResult{
			Status:         toolpkg.InvocationStatusFailed,
			Classification: classification,
			Error:          evalErr,
			ErrorMessage:   evalErr.Error(),
			DurationMs:     time.Since(startedAt).Milliseconds(),
		}, nil
	}

	if evalResult.Denied {
		err := fmt.Errorf("governance: invocation denied: %s", evalResult.DenyReason)
		evRecords := []evidence.EvidenceRecord{
			{
				Name:       "governance_evaluation",
				Kind:       evidence.EvidenceKindText,
				Value:      evalResult.DenyReason,
				CapturedAt: time.Now(),
			},
		}
		return &toolpkg.InvocationResult{
			Status:         toolpkg.InvocationStatusDenied,
			Classification: classification,
			Error:          err,
			ErrorMessage:   evalResult.DenyReason,
			Evidence:       evRecords,
			DurationMs:     time.Since(startedAt).Milliseconds(),
		}, nil
	}

	// 6. Approval Gating
	var approvalRecord *pkgGov.ApprovalRecord
	if evalResult.RequiresApproval {
		gate := req.Authority.ApprovalGate
		if gate == nil {
			gate = inv.approvalGate
		}
		if gate == nil {
			err := fmt.Errorf("action %s requires approval but no ApprovalGate is configured", stepInfo.ID)
			return &toolpkg.InvocationResult{
				Status:         toolpkg.InvocationStatusFailed,
				Classification: classification,
				Error:          err,
				ErrorMessage:   err.Error(),
				DurationMs:     time.Since(startedAt).Milliseconds(),
			}, nil
		}

		rec, err := gate.RequestApproval(ctx, stepInfo.ID, "governance policy requires approval")
		if err != nil {
			return &toolpkg.InvocationResult{
				Status:         toolpkg.InvocationStatusDenied,
				Classification: classification,
				Error:          fmt.Errorf("approval denied: %w", err),
				ErrorMessage:   err.Error(),
				DurationMs:     time.Since(startedAt).Milliseconds(),
			}, nil
		}
		approvalRecord = &rec
	}

	// 7. Durable Dispatch Intent & Idempotency
	var dispState engine.DispatchState
	var dispatchErr error
	var committer engine.DispatchCommitter = inv.dispatchStore
	if ctxCommitter, hasCtx := engine.DispatchCommitterFromContext(ctx); hasCtx && ctxCommitter != nil {
		committer = ctxCommitter
	}
	if committer != nil {
		if req.IdempotencyKey != "" {
			if locker, ok := committer.(interface {
				LockKey(ctx context.Context, key string) (func(), error)
			}); ok {
				release, lockErr := locker.LockKey(ctx, req.IdempotencyKey)
				if lockErr != nil {
					return nil, lockErr
				}
				defer release()
			}
		}

		renderedMap := map[string]any{
			"tool":   req.Tool,
			"action": req.Action,
			"args":   args,
		}
		if req.IdempotencyKey != "" {
			renderedMap["idempotency_key"] = req.IdempotencyKey
		}

		dispReq := engine.DispatchRequest{
			Classification:   classification,
			EndpointIdentity: "tool:" + string(def.Transport),
			RenderedRequest:  renderedMap,
		}

		dispState, dispatchErr = committer.PrepareDispatch(ctx, dispReq)
		if dispatchErr != nil {
			status := toolpkg.InvocationStatusFailed
			if errors.Is(dispatchErr, engine.ErrIndeterminate) {
				status = toolpkg.InvocationStatusIndeterminate
			}
			return &toolpkg.InvocationResult{
				Status:         status,
				Classification: classification,
				Error:          dispatchErr,
				ErrorMessage:   dispatchErr.Error(),
				DurationMs:     time.Since(startedAt).Milliseconds(),
			}, nil
		}

		// Replay check: if already settled, return existing result without re-executing
		if dispState.Status == engine.DispatchStatusSettled {
			replayedRes := &toolpkg.InvocationResult{
				Status:         toolpkg.InvocationStatusCompleted,
				Classification: classification,
				OccurrenceID:   dispState.OccurrenceID,
				IdempotencyKey: dispState.IdempotencyKey,
				CorrelationID:  req.CorrelationID,
				Replayed:       true,
				DurationMs:     time.Since(startedAt).Milliseconds(),
			}
			if getter, ok := committer.(interface {
				SettledOutcomeFor(ctx context.Context, occurrenceID string) (*engine.SettledOutcome, bool)
			}); ok {
				if outcome, ok := getter.SettledOutcomeFor(ctx, dispState.OccurrenceID); ok && outcome != nil {
					replayedRes.ExitCode = outcome.ExitCode
					replayedRes.Stdout = outcome.Stdout
					replayedRes.Stderr = outcome.Stderr
					if outcome.Error != "" {
						replayedRes.ErrorMessage = outcome.Error
						replayedRes.Error = errors.New(outcome.Error)
						replayedRes.Status = toolpkg.InvocationStatusFailed
					}
					if len(outcome.Output) > 0 {
						var parsed map[string]any
						if err := json.Unmarshal(outcome.Output, &parsed); err == nil {
							replayedRes.Output = parsed
						}
					}
				}
			}
			return replayedRes, nil
		}

		ctx = engine.WithDispatchCommitter(ctx, committer)
		ctx = engine.WithPreparedDispatch(ctx, dispState)
	}

	// 8. Tool Execution
	execRes, execErr := inv.runtime.Invoke(ctx, req.Tool, req.Action, args)

	// 9. Settle Durable Intent
	if inv.dispatchStore != nil && dispState.OccurrenceID != "" {
		if execErr != nil {
			_ = inv.dispatchStore.SettleDispatch(ctx, dispState.OccurrenceID, map[string]any{"error": execErr.Error()})
		} else {
			_ = inv.dispatchStore.SettleDispatch(ctx, dispState.OccurrenceID, execRes)
		}
	}

	durationMs := time.Since(startedAt).Milliseconds()

	// 10. Assemble Result & Collect Evidence
	var evidenceRecords []evidence.EvidenceRecord
	if approvalRecord != nil {
		capturedAt := time.Now()
		if t, err := time.Parse(time.RFC3339, approvalRecord.ApprovedAt); err == nil {
			capturedAt = t
		}
		evidenceRecords = append(evidenceRecords, evidence.EvidenceRecord{
			Name:       "approval_record",
			Kind:       evidence.EvidenceKindText,
			Value:      approvalRecord.Token,
			CapturedAt: capturedAt,
		})
	}

	if execErr != nil {
		res := &toolpkg.InvocationResult{
			Status:         toolpkg.InvocationStatusFailed,
			Classification: classification,
			OccurrenceID:   dispState.OccurrenceID,
			IdempotencyKey: dispState.IdempotencyKey,
			ApprovalRecord: approvalRecord,
			Evidence:       evidenceRecords,
			Error:          execErr,
			ErrorMessage:   execErr.Error(),
			DurationMs:     durationMs,
		}
		if execRes != nil {
			res.Stdout = execRes.Stdout
			res.Stderr = execRes.Stderr
			res.ExitCode = execRes.ExitCode
			res.Output = execRes.Output
		}
		return res, nil
	}

	if execRes != nil {
		if execRes.Stdout != "" {
			evidenceRecords = append(evidenceRecords, evidence.EvidenceRecord{
				Name:       "stdout",
				Kind:       evidence.EvidenceKindText,
				Value:      execRes.Stdout,
				CapturedAt: time.Now(),
			})
		}
		if execRes.Stderr != "" {
			evidenceRecords = append(evidenceRecords, evidence.EvidenceRecord{
				Name:       "stderr",
				Kind:       evidence.EvidenceKindText,
				Value:      execRes.Stderr,
				CapturedAt: time.Now(),
			})
		}
	}

	outputMap := make(map[string]any)
	stdout := ""
	stderr := ""
	exitCode := 0
	if execRes != nil {
		stdout = execRes.Stdout
		stderr = execRes.Stderr
		exitCode = execRes.ExitCode
		for k, v := range execRes.Output {
			outputMap[k] = v
		}
	}

	return &toolpkg.InvocationResult{
		Status:         toolpkg.InvocationStatusCompleted,
		Classification: classification,
		Output:         outputMap,
		Stdout:         stdout,
		Stderr:         stderr,
		ExitCode:       exitCode,
		OccurrenceID:   dispState.OccurrenceID,
		IdempotencyKey: dispState.IdempotencyKey,
		ApprovalRecord: approvalRecord,
		Evidence:       evidenceRecords,
		DurationMs:     durationMs,
	}, nil
}
