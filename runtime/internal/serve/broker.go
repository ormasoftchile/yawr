// Package serve: HTTP interaction broker.
//
// PromptBroker implements input.PromptProvider for the server and routes
// each prompt to a per-run channel keyed by the run ID extracted from the
// executor's context (engine.RunIDFromContext). Per the wire protocol
// in specs/runbook-interaction-wire-v1.md, each prompt becomes a Pending
// frame the client must answer via POST /runs/{id}/interactions/{turnID}.

package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/capture"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

// PendingInteraction is the SSE frame the server emits when an executor
// asks for input. Wire shape lives in specs/runbook-interaction-wire-v1.md.
type PendingInteraction struct {
	Type   string `json:"type"` // "pending"
	ID     int64  `json:"id"`
	TurnID string `json:"turnID"`
	RunID  string `json:"runID"`
	StepID string `json:"stepID"`
	NodeID string `json:"nodeID,omitempty"`
	Kind   string `json:"kind"` // "choice" | "decision" | "collector" | "approval" | "host_action" | "debug_break"
	// CorrelationID is server-generated and scoped to this pending turn.
	// It is intentionally opaque and must be echoed by host bridges.
	CorrelationID string `json:"correlationID,omitempty"`
	Title         string `json:"title,omitempty"`
	Prompt        string `json:"prompt,omitempty"`
	// For "choice":
	Options  []InteractionOption `json:"options,omitempty"`
	Multiple bool                `json:"multiple,omitempty"`
	Min      int                 `json:"min,omitempty"`
	Max      int                 `json:"max,omitempty"`
	// For "decision":
	Routes []InteractionRoute `json:"routes,omitempty"`
	// For "collector":
	Fields []InteractionField `json:"fields,omitempty"`
	// For "host_action":
	HostAction *hostaction.Request `json:"host_action,omitempty"`
	// For "debug_break":
	Debug *DebugBreakPayload `json:"debug,omitempty"`
}

// DebugRunConfig opts one registered run into debugger pauses.
type DebugRunConfig struct {
	Enabled           bool              `json:"enabled"`
	Breakpoints       []DebugBreakpoint `json:"breakpoints,omitempty"`
	Watches           []string          `json:"watches,omitempty"`
	Profile           *DebugProfile     `json:"profile,omitempty"`
	AllowStaleProfile bool              `json:"allowStaleProfile,omitempty"`
}

func (config *DebugRunConfig) UnmarshalJSON(data []byte) error {
	type rawDebugRunConfig DebugRunConfig
	var raw rawDebugRunConfig
	if err := decodeStrictJSON(data, &raw); err != nil {
		return err
	}
	*config = DebugRunConfig(raw)
	return nil
}

// DebugBreakpoint targets one phase at one exact nested call path.
type DebugBreakpoint struct {
	Step     string                  `json:"step"`
	Phase    engine.DebugPhase       `json:"phase"`
	CallPath []engine.DebugCallFrame `json:"callPath,omitempty"`
}

// DebugBreakPayload is the preview-safe state shown while execution is paused.
type DebugBreakPayload struct {
	Phase              engine.DebugPhase       `json:"phase"`
	CallPath           []engine.DebugCallFrame `json:"callPath,omitempty"`
	Invocation         int                     `json:"invocation"`
	Attempt            int                     `json:"attempt"`
	Variables          map[string]any          `json:"variables,omitempty"`
	ProtectedVariables []string                `json:"protectedVariables,omitempty"`
	Actual             *DebugActualResult      `json:"actual,omitempty"`
	OutputProtected    bool                    `json:"outputProtected,omitempty"`
	CanStepInto        bool                    `json:"canStepInto,omitempty"`
	Watches            []DebugWatchResult      `json:"watches,omitempty"`
}

type DebugWatchResult struct {
	Expression string `json:"expression"`
	Value      any    `json:"value,omitempty"`
	Error      string `json:"error,omitempty"`
}

// DebugActualResult preserves the executor observation separately from edits.
type DebugActualResult struct {
	Status engine.StepStatus `json:"status"`
	Output map[string]any    `json:"output,omitempty"`
	Error  string            `json:"error,omitempty"`
}

// DebugSet is the editable effective state in a debug answer.
type DebugSet struct {
	Status      engine.StepStatus `json:"status,omitempty" yaml:"status,omitempty"`
	OutputPatch map[string]any    `json:"output_patch,omitempty" yaml:"output_patch,omitempty"`
	Vars        map[string]any    `json:"vars,omitempty" yaml:"vars,omitempty"`
	Error       string            `json:"error,omitempty" yaml:"error,omitempty"`
}

func (set *DebugSet) UnmarshalJSON(data []byte) error {
	type rawDebugSet DebugSet
	var raw rawDebugSet
	if err := decodeStrictJSON(data, &raw); err != nil {
		return err
	}
	*set = DebugSet(raw)
	return nil
}

// InteractionOption mirrors input.Option on the wire.
type InteractionOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Hint  string `json:"hint,omitempty"`
}

// InteractionRoute mirrors input.Route on the wire.
type InteractionRoute struct {
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// InteractionField mirrors input.FormField on the wire.
//
// F-3 (barbara-client-enum-parity-gate-review.md, B-2): this DTO
// deliberately carries no Enum/EnumRedacted/EnumMemberCount fields. The
// only wire-level enum carriage this codebase ships is the preview
// document's declaration DTO (graphdoc.InputDecl, AR-CE-2 §2) -- there is
// no live collector/interaction producer for S3 (`inputs.<name>`) enum
// metadata, because collector steps (the only PromptForm producer) have
// no schema linkage to top-level runbook input declarations, and building
// one would mean either a new collector-to-input binding (a schema
// change, out of scope) or wiring `Input.From` prompt-sourcing (T-ENUM-
// FROM-SOURCING, explicitly out of scope for this revision). Rather than
// leave a wire field no code path populates -- exactly what the gate
// review rejected -- this field and its constructor (formerly
// `NewEnumInteractionField`) were removed. Re-add only alongside a real
// producer and a test that drives an actual interaction (see the gate
// review's F-3 fix note).
type InteractionField struct {
	Name       string                `json:"name"`
	Type       string                `json:"type"`
	Label      string                `json:"label,omitempty"`
	Required   bool                  `json:"required,omitempty"`
	Default    any                   `json:"default,omitempty"`
	Hint       string                `json:"hint,omitempty"`
	Options    []InteractionOption   `json:"options,omitempty"`
	FromStep   string                `json:"fromStep,omitempty"`
	Multiple   bool                  `json:"multiple,omitempty"`
	Validation *input.FormValidation `json:"validation,omitempty"`
	Ephemeral  bool                  `json:"ephemeral,omitempty"`
}

// ResolvedInteraction is the SSE frame emitted after an answer is
// accepted (or the prompt is cancelled).
type ResolvedInteraction struct {
	Type      string `json:"type"` // "resolved"
	ID        int64  `json:"id"`
	TurnID    string `json:"turnID"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// AnswerEnvelope is the JSON body posted to
// POST /runs/{id}/interactions/{turnID}.
type AnswerEnvelope struct {
	Kind          string                `json:"kind"`
	Selected      []string              `json:"selected,omitempty"`      // for "choice"
	Label         string                `json:"label,omitempty"`         // for "decision"
	Values        map[string]any        `json:"values,omitempty"`        // for "collector"
	RunID         string                `json:"runID,omitempty"`         // for "host_action"
	TurnID        string                `json:"turnID,omitempty"`        // for "host_action"
	CorrelationID string                `json:"correlationID,omitempty"` // for "host_action"
	Capability    hostaction.Capability `json:"capability,omitempty"`    // for "host_action"
	Status        hostaction.Status     `json:"status,omitempty"`        // for "host_action"
	Result        map[string]any        `json:"result,omitempty"`        // for "host_action"
	Action        engine.DebugAction    `json:"action,omitempty"`        // for "debug_break"
	Set           *DebugSet             `json:"set,omitempty"`           // for "debug_break"
	Approved      *bool                 `json:"approved,omitempty"`      // for "approval"
	Approver      string                `json:"approver,omitempty"`      // for "approval"
	acceptedAt    string
	auditToken    string
}

// pendingTurn is the in-memory record of a single open prompt awaiting an
// answer from the client.
type pendingTurn struct {
	frame     PendingInteraction
	answer    chan AnswerEnvelope // 1-buffered; closed on cancel
	once      sync.Once
	ctx       context.Context
	committer engine.InteractionCommitter
}

// runQueue holds the per-run state for one active run.
type runQueue struct {
	mu          sync.Mutex
	nextID      int64
	pending     map[string]*pendingTurn // turnID -> pending
	history     []any                   // recent frames (PendingInteraction or ResolvedInteraction)
	bufSize     int
	subs        map[chan any]struct{}
	debug       *DebugRunConfig
	debugStep   *debugStepPlan
	invocations *engine.InteractionInvocationTracker
	closed      bool
}

func newRunQueue(bufSize int) *runQueue {
	if bufSize <= 0 {
		bufSize = 64
	}
	return &runQueue{
		pending:     make(map[string]*pendingTurn),
		bufSize:     bufSize,
		subs:        make(map[chan any]struct{}),
		invocations: engine.NewInteractionInvocationTracker(),
	}
}

// publish records a frame in history and fans out to subscribers.
// Caller must hold q.mu.
func (q *runQueue) publishLocked(frame any) {
	q.history = append(q.history, frame)
	if len(q.history) > q.bufSize {
		q.history = q.history[len(q.history)-q.bufSize:]
	}
	for ch := range q.subs {
		select {
		case ch <- frame:
		default:
			// Slow subscriber; drop. Client can reconnect with Last-Event-ID.
		}
	}
}

// PromptBroker is the server-wide PromptProvider. It dispatches each
// Prompt* call to the queue for the run identified by ctx.
type PromptBroker struct {
	mu      sync.RWMutex
	runs    map[string]*runQueue
	bufSize int
}

func (*PromptBroker) CommitsInteractionsDurably() bool { return true }

// NewPromptBroker constructs a broker with a per-run buffer of bufSize
// frames (default 64 if <= 0).
func NewPromptBroker(bufSize int) *PromptBroker {
	return &PromptBroker{
		runs:    make(map[string]*runQueue),
		bufSize: bufSize,
	}
}

// Register creates the queue for a run. Must be called before the engine
// starts a run; otherwise the first Prompt* call would block waiting for
// a non-existent client.
func (b *PromptBroker) Register(runID string) *runQueue {
	b.mu.Lock()
	defer b.mu.Unlock()
	if q, ok := b.runs[runID]; ok {
		return q
	}
	q := newRunQueue(b.bufSize)
	b.runs[runID] = q
	return q
}

// registerExclusive reserves attachment ownership without replacing an existing
// queue (including one whose engine is still being resumed).
func (b *PromptBroker) registerExclusive(runID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.runs[runID]; exists {
		return false
	}
	b.runs[runID] = newRunQueue(b.bufSize)
	return true
}

// ConfigureDebug validates and stores debugger configuration for one run.
func (b *PromptBroker) ConfigureDebug(runID string, config DebugRunConfig) error {
	q := b.queueFor(runID)
	if q == nil {
		return errRunNotRegistered
	}
	if err := validateDebugRunConfig(config); err != nil {
		return err
	}
	clone := cloneDebugRunConfig(config)
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return errRunNotRegistered
	}
	q.debug = &clone
	q.mu.Unlock()
	return nil
}

// Unregister removes the queue for a run and cancels any open prompts.
func (b *PromptBroker) Unregister(runID string) {
	b.mu.Lock()
	q, ok := b.runs[runID]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.runs, runID)
	b.mu.Unlock()

	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	for turnID, p := range q.pending {
		p.once.Do(func() { close(p.answer) })
		q.publishLocked(ResolvedInteraction{
			Type:      "resolved",
			ID:        q.assignIDLocked(),
			TurnID:    turnID,
			Cancelled: true,
		})
	}
	q.pending = nil
	for ch := range q.subs {
		close(ch)
	}
	q.subs = nil
}

func (q *runQueue) assignIDLocked() int64 {
	q.nextID++
	return q.nextID
}

// queueFor returns the queue for a run (or nil if not registered).
func (b *PromptBroker) queueFor(runID string) *runQueue {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.runs[runID]
}

// Subscribe registers a frame channel for a run. The returned channel is
// closed when the run is unregistered. The caller receives every frame
// after Subscribe returns.
//
// since is the last frame ID the caller has already seen (0 = none).
// All frames in the queue's history with id > since are pre-loaded into
// the channel before live frames are delivered.
func (b *PromptBroker) Subscribe(runID string, since int64) (<-chan any, error) {
	q := b.queueFor(runID)
	if q == nil {
		return nil, errRunNotRegistered
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil, errRunNotRegistered
	}
	ch := make(chan any, len(q.history)+len(q.pending)+16)

	// Preload: any frame in history whose id > since.
	deliveredPending := make(map[string]bool)
	for _, f := range q.history {
		var id int64
		switch ff := f.(type) {
		case PendingInteraction:
			id = ff.ID
		case ResolvedInteraction:
			id = ff.ID
		}
		if id > since {
			ch <- f
			if pending, ok := f.(PendingInteraction); ok {
				deliveredPending[pending.TurnID] = true
			}
		}
	}
	// An unresolved turn is live state, not merely history. Re-emit it even when
	// Last-Event-ID is newer than its original pending frame or history rotated.
	pendingFrames := make([]PendingInteraction, 0, len(q.pending))
	for turnID, turn := range q.pending {
		if !deliveredPending[turnID] {
			pendingFrames = append(pendingFrames, turn.frame)
		}
	}
	sort.Slice(pendingFrames, func(i, j int) bool { return pendingFrames[i].ID < pendingFrames[j].ID })
	for _, pending := range pendingFrames {
		ch <- pending
	}
	q.subs[ch] = struct{}{}
	q.mu.Unlock()

	return ch, nil
}

// Unsubscribe removes a previously-subscribed channel and closes it.
func (b *PromptBroker) Unsubscribe(runID string, ch <-chan any) {
	q := b.queueFor(runID)
	if q == nil {
		return
	}
	q.mu.Lock()
	for c := range q.subs {
		if c == ch {
			delete(q.subs, c)
			close(c)
			break
		}
	}
	q.mu.Unlock()
}

// Answer delivers an AnswerEnvelope for a pending turn. Returns
// errUnknownTurn if the turn is not pending.
func (b *PromptBroker) Answer(runID, turnID string, env AnswerEnvelope) error {
	return b.AnswerCommand(runID, turnID, "", "", env)
}

func (b *PromptBroker) AnswerCommand(runID, turnID, commandID, commandDigest string, env AnswerEnvelope) error {
	q := b.queueFor(runID)
	if q == nil {
		return errRunNotRegistered
	}
	q.mu.Lock()
	p, ok := q.pending[turnID]
	if !ok {
		q.mu.Unlock()
		return errUnknownTurn
	}
	if p.frame.Kind != env.Kind {
		q.mu.Unlock()
		return errKindMismatch
	}
	if p.frame.Kind == "host_action" {
		if p.frame.HostAction == nil ||
			env.RunID != p.frame.RunID ||
			env.TurnID != p.frame.TurnID ||
			env.CorrelationID != p.frame.CorrelationID ||
			env.Capability != p.frame.HostAction.Capability {
			q.mu.Unlock()
			return errHostActionTupleMismatch
		}
	}
	mapped, err := mapPreviewAnswerTokens(p.frame, env)
	if err != nil {
		q.mu.Unlock()
		return err
	}
	if p.committer != nil {
		answer, marshalErr := json.Marshal(mapped)
		if marshalErr != nil {
			q.mu.Unlock()
			return marshalErr
		}
		answerDigest := digestInteractionJSON(answer)
		var committed engine.InteractionState
		var commitErr error
		if commandID != "" {
			commandCommitter, ok := p.committer.(engine.InteractionCommandCommitter)
			if !ok {
				q.mu.Unlock()
				return errors.New("interaction: durable committer does not support command identity")
			}
			committed, commitErr = commandCommitter.AcceptInteractionCommand(
				p.ctx, turnID, commandID, commandDigest, answerDigest, answer,
			)
		} else {
			committed, commitErr = p.committer.AcceptInteraction(p.ctx, turnID, answerDigest, answer)
		}
		if commitErr != nil {
			q.mu.Unlock()
			return commitErr
		}
		if committed.Status != engine.InteractionStatusAnswered || committed.TurnID != turnID {
			q.mu.Unlock()
			return errors.New("interaction: durable answer commit returned inconsistent state")
		}
		mapped.acceptedAt = committed.AcceptedAt
		mapped.auditToken = committed.AuditToken
		if metadataErr := validateDurableAnswerMetadata(mapped); metadataErr != nil {
			q.mu.Unlock()
			return metadataErr
		}
	}
	if p.frame.Kind == "debug_break" {
		q.setDebugStepPlanLocked(p.frame, mapped.Action)
	}
	delete(q.pending, turnID)
	resolved := ResolvedInteraction{
		Type:   "resolved",
		ID:     q.assignIDLocked(),
		TurnID: turnID,
	}
	q.publishLocked(resolved)
	q.mu.Unlock()

	// Deliver outside the lock so a slow Prompt* receiver does not
	// block other broker operations.
	p.once.Do(func() {
		select {
		case p.answer <- mapped:
		default:
		}
		close(p.answer)
	})
	return nil
}

// PromptChoice implements input.PromptProvider. Blocks until the client
// posts an answer or ctx is cancelled.
func (b *PromptBroker) PromptChoice(ctx context.Context, req input.ChoiceRequest) (*input.ChoiceResponse, error) {
	runID := engine.RunIDFromContext(ctx)
	q := b.queueFor(runID)
	if q == nil {
		return nil, errRunNotRegistered
	}
	frame := PendingInteraction{
		Type:     "pending",
		TurnID:   newTurnID(),
		RunID:    runID,
		StepID:   req.StepID,
		NodeID:   engine.DebugNodeID(engine.DebugCallPathFromContext(ctx), req.StepID),
		Kind:     "choice",
		Prompt:   req.Prompt,
		Multiple: req.Multiple,
		Min:      req.Min,
		Max:      req.Max,
		Options:  toOptions(req.Options),
	}
	turn, restored, err := b.prepareInteraction(ctx, q, frame)
	if err != nil {
		return nil, err
	}
	if restored != nil {
		return &input.ChoiceResponse{Selected: restored.Selected}, nil
	}

	select {
	case env, ok := <-turn.answer:
		if !ok {
			return nil, context.Canceled
		}
		return &input.ChoiceResponse{Selected: env.Selected}, nil
	case <-ctx.Done():
		// Engine cancellation; drop the pending turn so a late answer is rejected.
		q.mu.Lock()
		if _, still := q.pending[turn.frame.TurnID]; still {
			delete(q.pending, turn.frame.TurnID)
			q.publishLocked(ResolvedInteraction{
				Type: "resolved", ID: q.assignIDLocked(), TurnID: turn.frame.TurnID, Cancelled: true,
			})
		}
		q.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (b *PromptBroker) prepareInteraction(
	ctx context.Context,
	q *runQueue,
	frame PendingInteraction,
) (*pendingTurn, *AnswerEnvelope, error) {
	committer := engine.InteractionCommitterFromContext(ctx)
	if committer != nil {
		frameID := ""
		frameStepIndex := 0
		if boundary, ok := engine.DispatchExecutionBoundaryFromContext(ctx); ok {
			frameID = boundary.FrameID
			frameStepIndex = boundary.FrameStepIndex
		}
		semantic := frame
		semantic.ID = 0
		semantic.TurnID = ""
		semantic.CorrelationID = ""
		semanticRequest, err := json.Marshal(semantic)
		if err != nil {
			return nil, nil, err
		}
		tracker := engine.InteractionInvocationTrackerFromContext(ctx)
		if tracker == nil {
			tracker = q.invocations
		}
		committed, err := committer.PrepareInteraction(ctx, engine.InteractionState{
			SchemaVersion: engine.InteractionStateSchemaV1,
			TurnID:        frame.TurnID, CorrelationID: frame.CorrelationID,
			NodeID: frame.NodeID, StepID: frame.StepID,
			FrameID: frameID, FrameStepIndex: frameStepIndex, Kind: frame.Kind,
			Ordinal: tracker.NextOccurrence(
				frame.NodeID, frame.Kind, frameID, frameStepIndex,
			),
			Status:        engine.InteractionStatusPending,
			RequestDigest: engine.InteractionPayloadDigest(semanticRequest), Request: semanticRequest,
		})
		if err != nil {
			return nil, nil, err
		}
		if committed.Status == engine.InteractionStatusAnswered {
			var answer AnswerEnvelope
			if err := decodeStrictJSON(committed.Answer, &answer); err != nil {
				return nil, nil, fmt.Errorf("interaction: decode restored answer: %w", err)
			}
			answer.acceptedAt = committed.AcceptedAt
			answer.auditToken = committed.AuditToken
			answer, err = validateMappedAnswer(frame, answer)
			if err != nil {
				return nil, nil, fmt.Errorf("interaction: validate restored answer: %w", err)
			}
			if err := validateDurableAnswerMetadata(answer); err != nil {
				return nil, nil, fmt.Errorf("interaction: validate restored answer: %w", err)
			}
			return nil, &answer, nil
		}
		if committed.Status != engine.InteractionStatusPending {
			return nil, nil, errors.New("interaction: durable prepare returned unsupported status")
		}
		if len(committed.Request) > 0 {
			if err := decodeStrictJSON(committed.Request, &frame); err != nil {
				return nil, nil, fmt.Errorf("interaction: decode restored request: %w", err)
			}
		}
		frame.TurnID = committed.TurnID
		frame.CorrelationID = committed.CorrelationID
	}

	turn := &pendingTurn{
		answer: make(chan AnswerEnvelope, 1), frame: frame,
		ctx: context.WithoutCancel(ctx), committer: committer,
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil, nil, context.Canceled
	}
	turn.frame.ID = q.assignIDLocked()
	q.pending[turn.frame.TurnID] = turn
	q.publishLocked(turn.frame)
	q.mu.Unlock()
	return turn, nil, nil
}

func digestInteractionJSON(data []byte) string {
	return engine.InteractionPayloadDigest(data)
}

// PromptDecision implements input.PromptProvider.
func (b *PromptBroker) PromptDecision(ctx context.Context, req input.DecisionRequest) (*input.DecisionResponse, error) {
	runID := engine.RunIDFromContext(ctx)
	q := b.queueFor(runID)
	if q == nil {
		return nil, errRunNotRegistered
	}
	frame := PendingInteraction{
		Type:   "pending",
		TurnID: newTurnID(),
		RunID:  runID,
		StepID: req.StepID,
		NodeID: engine.DebugNodeID(engine.DebugCallPathFromContext(ctx), req.StepID),
		Kind:   "decision",
		Prompt: req.Prompt,
		Routes: toRoutes(req.Routes),
	}
	turn, restored, err := b.prepareInteraction(ctx, q, frame)
	if err != nil {
		return nil, err
	}
	if restored != nil {
		return &input.DecisionResponse{Label: restored.Label}, nil
	}

	select {
	case env, ok := <-turn.answer:
		if !ok {
			return nil, context.Canceled
		}
		return &input.DecisionResponse{Label: env.Label}, nil
	case <-ctx.Done():
		q.mu.Lock()
		if _, still := q.pending[turn.frame.TurnID]; still {
			delete(q.pending, turn.frame.TurnID)
			q.publishLocked(ResolvedInteraction{
				Type: "resolved", ID: q.assignIDLocked(), TurnID: turn.frame.TurnID, Cancelled: true,
			})
		}
		q.mu.Unlock()
		return nil, ctx.Err()
	}
}

// PromptForm implements input.PromptProvider.
func (b *PromptBroker) PromptForm(ctx context.Context, req input.FormRequest) (*input.FormResponse, error) {
	runID := engine.RunIDFromContext(ctx)
	q := b.queueFor(runID)
	if q == nil {
		return nil, errRunNotRegistered
	}
	frame := PendingInteraction{
		Type:   "pending",
		TurnID: newTurnID(),
		RunID:  runID,
		StepID: req.StepID,
		NodeID: engine.DebugNodeID(engine.DebugCallPathFromContext(ctx), req.StepID),
		Kind:   "collector",
		Prompt: req.Prompt,
		Fields: toFields(req.Fields),
	}
	turn, restored, err := b.prepareInteraction(ctx, q, frame)
	if err != nil {
		return nil, err
	}
	if restored != nil {
		return &input.FormResponse{Values: restored.Values}, nil
	}

	select {
	case env, ok := <-turn.answer:
		if !ok {
			return nil, context.Canceled
		}
		return &input.FormResponse{Values: env.Values}, nil
	case <-ctx.Done():
		q.mu.Lock()
		if _, still := q.pending[turn.frame.TurnID]; still {
			delete(q.pending, turn.frame.TurnID)
			q.publishLocked(ResolvedInteraction{
				Type: "resolved", ID: q.assignIDLocked(), TurnID: turn.frame.TurnID, Cancelled: true,
			})
		}
		q.mu.Unlock()
		return nil, ctx.Err()
	}
}

// RequestApproval implements governance.ApprovalGate. It fails closed when no
// registered operator client is available or when the operator denies approval.
func (b *PromptBroker) RequestApproval(ctx context.Context, stepID, reason string) (governance.ApprovalRecord, error) {
	runID := engine.RunIDFromContext(ctx)
	q := b.queueFor(runID)
	if q == nil {
		return governance.ApprovalRecord{}, errRunNotRegistered
	}
	frame := PendingInteraction{
		Type:   "pending",
		TurnID: newTurnID(),
		RunID:  runID,
		StepID: stepID,
		NodeID: engine.DebugNodeID(engine.DebugCallPathFromContext(ctx), stepID),
		Kind:   "approval",
		Title:  "Approval required",
		Prompt: reason,
	}
	turn, restored, err := b.prepareInteraction(ctx, q, frame)
	if err != nil {
		return governance.ApprovalRecord{}, err
	}
	if restored != nil {
		return approvalRecordFromAnswer(frame.TurnID, *restored)
	}

	select {
	case env, ok := <-turn.answer:
		if !ok {
			return governance.ApprovalRecord{}, context.Canceled
		}
		return approvalRecordFromAnswer(turn.frame.TurnID, env)
	case <-ctx.Done():
		q.mu.Lock()
		if _, still := q.pending[turn.frame.TurnID]; still {
			delete(q.pending, turn.frame.TurnID)
			q.publishLocked(ResolvedInteraction{
				Type: "resolved", ID: q.assignIDLocked(), TurnID: turn.frame.TurnID, Cancelled: true,
			})
		}
		q.mu.Unlock()
		return governance.ApprovalRecord{}, ctx.Err()
	}
}

func approvalRecordFromAnswer(turnID string, env AnswerEnvelope) (governance.ApprovalRecord, error) {
	if env.Approved == nil || !*env.Approved {
		return governance.ApprovalRecord{}, errApprovalDenied
	}
	approvedAt := env.acceptedAt
	if approvedAt == "" {
		approvedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	token := env.auditToken
	if token == "" {
		token = turnID
	}
	return governance.ApprovalRecord{Approver: env.Approver, ApprovedAt: approvedAt, Token: token}, nil
}

func mapPreviewAnswerTokens(frame PendingInteraction, env AnswerEnvelope) (AnswerEnvelope, error) {
	switch frame.Kind {
	case "choice":
		mapped := env
		mapped.Selected = make([]string, 0, len(env.Selected))
		for _, token := range env.Selected {
			idx, ok := parsePreviewToken(token, "o")
			if !ok || idx < 0 || idx >= len(frame.Options) {
				return AnswerEnvelope{}, errInvalidInteractionToken
			}
			mapped.Selected = append(mapped.Selected, frame.Options[idx].Value)
		}
		return validateMappedAnswer(frame, mapped)
	case "decision":
		idx, ok := parsePreviewToken(env.Label, "r")
		if !ok || idx < 0 || idx >= len(frame.Routes) {
			return AnswerEnvelope{}, errInvalidInteractionToken
		}
		mapped := env
		mapped.Label = frame.Routes[idx].Label
		return validateMappedAnswer(frame, mapped)
	case "collector":
		mapped := env
		mapped.Values = make(map[string]any, len(env.Values))
		for token, value := range env.Values {
			fieldIdx, ok := parsePreviewToken(token, "f")
			if !ok || fieldIdx < 0 || fieldIdx >= len(frame.Fields) {
				return AnswerEnvelope{}, errInvalidInteractionToken
			}
			field := frame.Fields[fieldIdx]
			mappedValue := value
			if len(field.Options) > 0 {
				if selected, ok := value.(string); ok && selected != "" {
					optionIdx, ok := parsePreviewToken(selected, "f"+strconv.Itoa(fieldIdx)+"o")
					if !ok || optionIdx < 0 || optionIdx >= len(field.Options) {
						return AnswerEnvelope{}, errInvalidInteractionToken
					}
					mappedValue = field.Options[optionIdx].Value
				} else if selectedList, ok := value.([]any); ok {
					out := make([]any, 0, len(selectedList))
					for _, raw := range selectedList {
						selected, ok := raw.(string)
						if !ok {
							return AnswerEnvelope{}, errInvalidInteractionToken
						}
						optionIdx, ok := parsePreviewToken(selected, "f"+strconv.Itoa(fieldIdx)+"o")
						if !ok || optionIdx < 0 || optionIdx >= len(field.Options) {
							return AnswerEnvelope{}, errInvalidInteractionToken
						}
						out = append(out, field.Options[optionIdx].Value)
					}
					mappedValue = out
				}
			}
			mapped.Values[field.Name] = mappedValue
		}
		return validateMappedAnswer(frame, mapped)
	case "approval":
		if env.Approved == nil {
			return AnswerEnvelope{}, errInvalidApprovalAnswer
		}
		mapped := AnswerEnvelope{Kind: env.Kind, Approved: env.Approved}
		if *env.Approved {
			mapped.Approver = strings.TrimSpace(env.Approver)
			if mapped.Approver == "" || len([]byte(mapped.Approver)) > 256 {
				return AnswerEnvelope{}, errInvalidApprovalAnswer
			}
		} else if env.Approver != "" {
			return AnswerEnvelope{}, errInvalidApprovalAnswer
		}
		return validateMappedAnswer(frame, mapped)
	case "host_action":
		if !hostaction.IsKnownStatus(env.Status) {
			return AnswerEnvelope{}, errInvalidHostActionStatus
		}
		if env.Status == hostaction.StatusCompleted && env.Result == nil {
			return AnswerEnvelope{}, errInvalidHostActionStatus
		}
		if env.Status != hostaction.StatusCompleted && env.Result != nil {
			return AnswerEnvelope{}, errInvalidHostActionStatus
		}
		if env.Result != nil {
			if err := hostaction.ValidatePayload(env.Result); err != nil {
				return AnswerEnvelope{}, errInvalidHostActionStatus
			}
		}
		return validateMappedAnswer(frame, AnswerEnvelope{Kind: env.Kind, Status: env.Status, Result: env.Result})
	case "debug_break":
		if env.Action != engine.DebugActionContinue && env.Action != engine.DebugActionStop &&
			env.Action != engine.DebugActionStepInto && env.Action != engine.DebugActionStepOver &&
			env.Action != engine.DebugActionStepOut {
			return AnswerEnvelope{}, errInvalidDebugAnswer
		}
		if env.Action == engine.DebugActionStop && env.Set != nil {
			return AnswerEnvelope{}, errInvalidDebugAnswer
		}
		if env.Action == engine.DebugActionStepInto && !frame.Debug.CanStepInto {
			return AnswerEnvelope{}, errInvalidDebugAnswer
		}
		if frame.Debug == nil || validateDebugSetForPhase(frame.Debug.Phase, env.Set) != nil {
			return AnswerEnvelope{}, errInvalidDebugAnswer
		}
		return validateMappedAnswer(frame, AnswerEnvelope{Kind: env.Kind, Action: env.Action, Set: env.Set})
	default:
		return env, nil
	}
}

func validateMappedAnswer(frame PendingInteraction, answer AnswerEnvelope) (AnswerEnvelope, error) {
	if answer.Kind != frame.Kind {
		return AnswerEnvelope{}, errKindMismatch
	}
	switch frame.Kind {
	case "choice":
		if !frame.Multiple && len(answer.Selected) > 1 || frame.Min > 0 && len(answer.Selected) < frame.Min ||
			frame.Max > 0 && len(answer.Selected) > frame.Max {
			return AnswerEnvelope{}, errInvalidInteractionToken
		}
		allowed := make(map[string]bool, len(frame.Options))
		for _, option := range frame.Options {
			allowed[option.Value] = true
		}
		for _, selected := range answer.Selected {
			if !allowed[selected] {
				return AnswerEnvelope{}, errInvalidInteractionToken
			}
		}
	case "decision":
		valid := false
		for _, route := range frame.Routes {
			valid = valid || route.Label == answer.Label
		}
		if !valid {
			return AnswerEnvelope{}, errInvalidInteractionToken
		}
	case "collector":
		fields := make([]input.FormField, len(frame.Fields))
		for index, field := range frame.Fields {
			options := make([]input.Option, len(field.Options))
			for optionIndex, option := range field.Options {
				options[optionIndex] = input.Option{Label: option.Label, Value: option.Value, Hint: option.Hint}
			}
			fields[index] = input.FormField{
				Name: field.Name, Type: field.Type, Label: field.Label, Required: field.Required,
				Default: field.Default, Hint: field.Hint, Options: options, Multiple: field.Multiple,
				Validation: field.Validation, Ephemeral: field.Ephemeral, FromStep: field.FromStep,
			}
		}
		if failures := input.ValidateFormResponse(
			input.FormRequest{StepID: frame.StepID, Prompt: frame.Prompt, Fields: fields},
			input.FormResponse{Values: answer.Values},
		); len(failures) > 0 {
			return AnswerEnvelope{}, fmt.Errorf("invalid collector answer: %v", failures)
		}
	case "approval":
		if answer.Approved == nil {
			return AnswerEnvelope{}, errInvalidApprovalAnswer
		}
		if *answer.Approved {
			answer.Approver = strings.TrimSpace(answer.Approver)
			if answer.Approver == "" || len([]byte(answer.Approver)) > 256 {
				return AnswerEnvelope{}, errInvalidApprovalAnswer
			}
		} else if answer.Approver != "" {
			return AnswerEnvelope{}, errInvalidApprovalAnswer
		}
	case "host_action":
		if !hostaction.IsKnownStatus(answer.Status) ||
			answer.Status == hostaction.StatusCompleted && answer.Result == nil ||
			answer.Status != hostaction.StatusCompleted && answer.Result != nil {
			return AnswerEnvelope{}, errInvalidHostActionStatus
		}
		if answer.Result != nil {
			if err := hostaction.ValidatePayload(answer.Result); err != nil {
				return AnswerEnvelope{}, errInvalidHostActionStatus
			}
		}
	case "debug_break":
		if frame.Debug == nil || answer.Action != engine.DebugActionContinue && answer.Action != engine.DebugActionStop &&
			answer.Action != engine.DebugActionStepInto && answer.Action != engine.DebugActionStepOver &&
			answer.Action != engine.DebugActionStepOut || answer.Action == engine.DebugActionStop && answer.Set != nil ||
			answer.Action == engine.DebugActionStepInto && !frame.Debug.CanStepInto ||
			validateDebugSetForPhase(frame.Debug.Phase, answer.Set) != nil {
			return AnswerEnvelope{}, errInvalidDebugAnswer
		}
	default:
		return AnswerEnvelope{}, errKindMismatch
	}
	return answer, nil
}

func validateDurableAnswerMetadata(answer AnswerEnvelope) error {
	if answer.Kind != "approval" {
		return nil
	}
	if strings.TrimSpace(answer.auditToken) == "" || len(answer.auditToken) > 4096 {
		return errInvalidApprovalAnswer
	}
	if _, err := time.Parse(time.RFC3339Nano, answer.acceptedAt); err != nil {
		return errInvalidApprovalAnswer
	}
	return nil
}

// Pause implements engine.DebugController. Unconfigured runs return immediately.
func (b *PromptBroker) Pause(ctx context.Context, snapshot engine.DebugSnapshot) (engine.DebugDecision, error) {
	runID := engine.RunIDFromContext(ctx)
	q := b.queueFor(runID)
	if q == nil {
		return engine.DebugDecision{Action: engine.DebugActionContinue}, nil
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return engine.DebugDecision{}, context.Canceled
	}
	profileSet := matchingDebugProfileSet(q.debugProfile(), snapshot)
	stepMatch := q.matchesDebugStepLocked(snapshot)
	if stepMatch {
		q.debugStep = nil
	}
	configured := q.debug != nil && q.debug.Enabled && (matchesDebugBreakpoint(q.debug.Breakpoints, snapshot) || stepMatch)
	var watches []string
	if q.debug != nil {
		watches = append([]string(nil), q.debug.Watches...)
	}
	q.mu.Unlock()
	if profileSet != nil {
		return debugDecisionFromSet(engine.DebugActionContinue, profileSet), nil
	}
	if !configured {
		return engine.DebugDecision{Action: engine.DebugActionContinue}, nil
	}

	turn := &pendingTurn{
		answer: make(chan AnswerEnvelope, 1),
		frame: PendingInteraction{
			Type:   "pending",
			TurnID: newTurnID(),
			RunID:  runID,
			StepID: snapshot.Location.StepID,
			NodeID: engine.DebugNodeID(snapshot.Location.CallPath, snapshot.Location.StepID),
			Kind:   "debug_break",
			Debug:  debugBreakPayload(snapshot, watches),
		},
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return engine.DebugDecision{}, context.Canceled
	}
	turn.frame.ID = q.assignIDLocked()
	q.pending[turn.frame.TurnID] = turn
	q.publishLocked(turn.frame)
	q.mu.Unlock()

	select {
	case env, ok := <-turn.answer:
		if !ok {
			return engine.DebugDecision{}, context.Canceled
		}
		return debugDecisionFromSet(engineActionForDebugAnswer(env.Action), env.Set), nil
	case <-ctx.Done():
		q.mu.Lock()
		if _, still := q.pending[turn.frame.TurnID]; still {
			delete(q.pending, turn.frame.TurnID)
			q.publishLocked(ResolvedInteraction{
				Type: "resolved", ID: q.assignIDLocked(), TurnID: turn.frame.TurnID, Cancelled: true,
			})
		}
		q.mu.Unlock()
		return engine.DebugDecision{}, ctx.Err()
	}
}

func validateDebugRunConfig(config DebugRunConfig) error {
	if !config.Enabled {
		return nil
	}
	if err := validateDebugProfile(config.Profile); err != nil {
		return err
	}
	if len(config.Breakpoints) > 256 {
		return errInvalidDebugConfig
	}
	for _, breakpoint := range config.Breakpoints {
		if strings.TrimSpace(breakpoint.Step) == "" {
			return errInvalidDebugConfig
		}
		if breakpoint.Phase != engine.DebugPhaseBefore && breakpoint.Phase != engine.DebugPhaseAfter {
			return errInvalidDebugConfig
		}
		if len(breakpoint.CallPath) > 32 {
			return errInvalidDebugConfig
		}
		for _, frame := range breakpoint.CallPath {
			if strings.TrimSpace(frame.StepID) == "" {
				return errInvalidDebugConfig
			}
		}
	}
	if len(config.Watches) > 32 {
		return errInvalidDebugConfig
	}
	for _, watch := range config.Watches {
		if strings.TrimSpace(watch) == "" || len(watch) > 1024 {
			return errInvalidDebugConfig
		}
		if _, err := gxlparser.Parse(watch); err != nil {
			return errInvalidDebugConfig
		}
	}
	return nil
}

func cloneDebugRunConfig(config DebugRunConfig) DebugRunConfig {
	clone := DebugRunConfig{Enabled: config.Enabled, Breakpoints: make([]DebugBreakpoint, len(config.Breakpoints)), Watches: append([]string(nil), config.Watches...), Profile: cloneDebugProfile(config.Profile), AllowStaleProfile: config.AllowStaleProfile}
	for i, breakpoint := range config.Breakpoints {
		clone.Breakpoints[i] = breakpoint
		clone.Breakpoints[i].CallPath = append([]engine.DebugCallFrame(nil), breakpoint.CallPath...)
	}
	return clone
}

func (q *runQueue) debugProfile() *DebugProfile {
	if q.debug == nil || !q.debug.Enabled {
		return nil
	}
	return q.debug.Profile
}

func debugDecisionFromSet(action engine.DebugAction, set *DebugSet) engine.DebugDecision {
	decision := engine.DebugDecision{Action: action}
	if set == nil {
		return decision
	}
	decision.Vars = cloneDebugJSONMap(set.Vars)
	if set.Status != "" || set.OutputPatch != nil || set.Error != "" {
		decision.Result = &engine.DebugResultOverride{
			Status:      set.Status,
			OutputPatch: cloneDebugJSONMap(set.OutputPatch),
			Error:       set.Error,
		}
	}
	return decision
}

type debugStepPlan struct {
	action     engine.DebugAction
	phase      engine.DebugPhase
	stepID     string
	callPath   []engine.DebugCallFrame
	invocation int
}

func (q *runQueue) setDebugStepPlanLocked(frame PendingInteraction, action engine.DebugAction) {
	if frame.Debug == nil {
		q.debugStep = nil
		return
	}
	switch action {
	case engine.DebugActionStepInto, engine.DebugActionStepOver, engine.DebugActionStepOut:
		q.debugStep = &debugStepPlan{
			action: action, phase: frame.Debug.Phase, stepID: frame.StepID,
			callPath:   append([]engine.DebugCallFrame(nil), frame.Debug.CallPath...),
			invocation: frame.Debug.Invocation,
		}
	default:
		q.debugStep = nil
	}
}

func (q *runQueue) matchesDebugStepLocked(snapshot engine.DebugSnapshot) bool {
	plan := q.debugStep
	if plan == nil {
		return false
	}
	switch plan.action {
	case engine.DebugActionStepInto:
		childPath := append(append([]engine.DebugCallFrame(nil), plan.callPath...), engine.DebugCallFrame{StepID: plan.stepID})
		if snapshot.Phase == engine.DebugPhaseBefore && sameDebugCallPath(childPath, snapshot.Location.CallPath) {
			return true
		}
		return snapshot.Phase == engine.DebugPhaseAfter && snapshot.Location.StepID == plan.stepID &&
			snapshot.Location.Invocation == plan.invocation && sameDebugCallPath(plan.callPath, snapshot.Location.CallPath)
	case engine.DebugActionStepOver:
		if plan.phase == engine.DebugPhaseBefore {
			return snapshot.Phase == engine.DebugPhaseAfter && snapshot.Location.StepID == plan.stepID &&
				snapshot.Location.Invocation == plan.invocation && sameDebugCallPath(plan.callPath, snapshot.Location.CallPath)
		}
		if snapshot.Phase == engine.DebugPhaseBefore && sameDebugCallPath(plan.callPath, snapshot.Location.CallPath) &&
			(snapshot.Location.StepID != plan.stepID || snapshot.Location.Invocation != plan.invocation) {
			return true
		}
		if len(plan.callPath) > 0 {
			parent := plan.callPath[len(plan.callPath)-1]
			return snapshot.Phase == engine.DebugPhaseAfter && snapshot.Location.StepID == parent.StepID &&
				sameDebugCallPath(plan.callPath[:len(plan.callPath)-1], snapshot.Location.CallPath)
		}
		return false
	case engine.DebugActionStepOut:
		if len(plan.callPath) == 0 {
			return false
		}
		parent := plan.callPath[len(plan.callPath)-1]
		return snapshot.Phase == engine.DebugPhaseAfter && snapshot.Location.StepID == parent.StepID &&
			sameDebugCallPath(plan.callPath[:len(plan.callPath)-1], snapshot.Location.CallPath)
	default:
		return false
	}
}

func engineActionForDebugAnswer(action engine.DebugAction) engine.DebugAction {
	switch action {
	case engine.DebugActionStepInto, engine.DebugActionStepOver, engine.DebugActionStepOut:
		return engine.DebugActionContinue
	default:
		return action
	}
}

func matchesDebugBreakpoint(breakpoints []DebugBreakpoint, snapshot engine.DebugSnapshot) bool {
	for _, breakpoint := range breakpoints {
		if breakpoint.Step != snapshot.Location.StepID || breakpoint.Phase != snapshot.Phase || len(breakpoint.CallPath) != len(snapshot.Location.CallPath) {
			continue
		}
		matches := true
		for i := range breakpoint.CallPath {
			if breakpoint.CallPath[i].StepID != snapshot.Location.CallPath[i].StepID {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func debugBreakPayload(snapshot engine.DebugSnapshot, watches []string) *DebugBreakPayload {
	payload := &DebugBreakPayload{
		Phase:              snapshot.Phase,
		CallPath:           append([]engine.DebugCallFrame(nil), snapshot.Location.CallPath...),
		Invocation:         snapshot.Location.Invocation,
		Attempt:            snapshot.Location.Attempt,
		Variables:          runstate.PreviewVars(snapshot.Vars),
		ProtectedVariables: append([]string(nil), snapshot.ProtectedVars...),
		OutputProtected:    snapshot.OutputProtected,
		CanStepInto:        snapshot.CanStepInto,
		Watches:            evaluateDebugWatches(snapshot.Vars, watches),
	}
	if snapshot.Actual != nil {
		payload.Actual = &DebugActualResult{
			Status: snapshot.Actual.Status,
			Output: runstate.PreviewCaptures(snapshot.Actual.Output),
		}
		if snapshot.Actual.Error != nil {
			payload.Actual.Error = snapshot.Actual.Error.Error()
		}
	}
	return payload
}

func evaluateDebugWatches(vars map[string]any, watches []string) []DebugWatchResult {
	if len(watches) == 0 {
		return nil
	}
	scope, scopeErr := gxleval.FromAny(vars)
	results := make([]DebugWatchResult, 0, len(watches))
	for _, source := range watches {
		result := DebugWatchResult{Expression: source}
		if scopeErr != nil {
			result.Error = scopeErr.Error()
			results = append(results, result)
			continue
		}
		expression, err := gxlparser.Parse(source)
		if err == nil {
			var value any
			parsed, evalErr := gxleval.Eval(expression, scope)
			if evalErr == nil {
				value = capture.ToAny(parsed)
				result.Value = runstate.PreviewEventPayload(map[string]any{"value": value})["value"]
			} else {
				err = evalErr
			}
		}
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
	}
	return results
}

func validDebugMap(values map[string]any) bool {
	if values == nil {
		return true
	}
	return hostaction.ValidatePayload(values) == nil
}

func validateDebugSet(set *DebugSet) error {
	if set == nil {
		return nil
	}
	switch set.Status {
	case "", engine.StepStatusCompleted, engine.StepStatusFailed, engine.StepStatusSkipped:
	default:
		return errInvalidDebugAnswer
	}
	if set.Error != "" && set.Status != engine.StepStatusFailed {
		return errInvalidDebugAnswer
	}
	if !validDebugMap(set.OutputPatch) || !validDebugMap(set.Vars) {
		return errInvalidDebugAnswer
	}
	return nil
}

func validateDebugSetForPhase(phase engine.DebugPhase, set *DebugSet) error {
	if err := validateDebugSet(set); err != nil || set == nil {
		return err
	}
	if phase == engine.DebugPhaseBefore && (set.Status != "" || set.OutputPatch != nil || set.Error != "") {
		return errInvalidDebugAnswer
	}
	return nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// ExecuteHostAction implements hostaction.Provider. It creates one pending,
// server-correlated interaction and blocks until a scoped acknowledgement is
// accepted or the run is cancelled.
func (b *PromptBroker) ExecuteHostAction(ctx context.Context, request hostaction.Request) (hostaction.Response, error) {
	if err := hostaction.ValidatePayload(request.Payload); err != nil {
		return hostaction.Response{}, err
	}
	runID := engine.RunIDFromContext(ctx)
	q := b.queueFor(runID)
	if q == nil {
		return hostaction.Response{}, errRunNotRegistered
	}
	frame := PendingInteraction{
		Type:          "pending",
		TurnID:        newTurnID(),
		CorrelationID: newTurnID(),
		RunID:         runID,
		StepID:        hostaction.StepIDFromContext(ctx),
		NodeID:        engine.DebugNodeID(engine.DebugCallPathFromContext(ctx), hostaction.StepIDFromContext(ctx)),
		Kind:          "host_action",
		HostAction:    &request,
	}
	turn, restored, err := b.prepareInteraction(ctx, q, frame)
	if err != nil {
		return hostaction.Response{}, err
	}
	if restored != nil {
		return hostaction.Response{Status: restored.Status, Result: restored.Result}, nil
	}

	select {
	case env, ok := <-turn.answer:
		if !ok {
			return hostaction.Response{}, context.Canceled
		}
		return hostaction.Response{Status: env.Status, Result: env.Result}, nil
	case <-ctx.Done():
		q.mu.Lock()
		if _, still := q.pending[turn.frame.TurnID]; still {
			delete(q.pending, turn.frame.TurnID)
			q.publishLocked(ResolvedInteraction{
				Type: "resolved", ID: q.assignIDLocked(), TurnID: turn.frame.TurnID, Cancelled: true,
			})
		}
		q.mu.Unlock()
		return hostaction.Response{}, ctx.Err()
	}
}

func parsePreviewToken(token, prefix string) (int, bool) {
	want := prefix + ":"
	if !strings.HasPrefix(token, want) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(token, want))
	return n, err == nil
}

func toOptions(in []input.Option) []InteractionOption {
	if len(in) == 0 {
		return nil
	}
	out := make([]InteractionOption, 0, len(in))
	for _, o := range in {
		out = append(out, InteractionOption{Label: o.Label, Value: o.Value, Hint: o.Hint})
	}
	return out
}

func toRoutes(in []input.Route) []InteractionRoute {
	if len(in) == 0 {
		return nil
	}
	out := make([]InteractionRoute, 0, len(in))
	for _, r := range in {
		out = append(out, InteractionRoute{Label: r.Label, Hint: r.Hint})
	}
	return out
}

func toFields(in []input.FormField) []InteractionField {
	if len(in) == 0 {
		return nil
	}
	out := make([]InteractionField, 0, len(in))
	for _, f := range in {
		field := InteractionField{
			Name: f.Name, Type: f.Type, Label: f.Label, Required: f.Required,
			Default: f.Default, Hint: f.Hint, Options: toOptions(f.Options),
			FromStep: f.FromStep, Multiple: f.Multiple, Validation: f.Validation, Ephemeral: f.Ephemeral,
		}
		out = append(out, field)
	}
	return out
}

func newTurnID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Extremely unlikely; fall back to a timestamp-based id.
		return "turn-" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(buf[:])
}

var (
	errRunNotRegistered        = errors.New("interaction: run not registered")
	errUnknownTurn             = errors.New("interaction: unknown or already-resolved turn")
	errKindMismatch            = errors.New("interaction: answer kind does not match pending turn")
	errInvalidInteractionToken = errors.New("interaction: invalid preview answer token")
	errInvalidHostActionStatus = errors.New("interaction: invalid host action status")
	errHostActionTupleMismatch = errors.New("interaction: host action acknowledgement does not match pending request")
	errInvalidDebugConfig      = errors.New("interaction: invalid debug configuration")
	errInvalidDebugAnswer      = errors.New("interaction: invalid debug answer")
	errInvalidApprovalAnswer   = errors.New("interaction: invalid approval answer")
	errApprovalDenied          = errors.New("approval denied by operator")
)
