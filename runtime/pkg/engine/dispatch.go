package engine

import "context"

// DispatchRequest describes a fully rendered external request at the last
// safe boundary before provider I/O. RenderedRequest is hashed, never stored.
type DispatchRequest struct {
	Classification                 string
	EndpointIdentity               string
	RenderedRequest                any
	ProviderSupportsReconciliation bool
}

// DispatchCommitter durably prepares an external occurrence before dispatch.
type DispatchCommitter interface {
	PrepareDispatch(ctx context.Context, request DispatchRequest) (DispatchState, error)
}

// DispatchExecutionBoundary identifies the exact step currently approaching
// provider I/O, including its containing structural frame when nested.
type DispatchExecutionBoundary struct {
	QualifiedNodeID    string
	CallPath           []DebugCallFrame
	StepID             string
	StepIndex          int
	FrameID            string
	FrameStepIndex     int
	Phase              ExecutionPhase
	Invocation         int
	RetryAttempt       int
	OccurrenceSequence int64
}

type dispatchCommitterContextKey struct{}
type preparedDispatchContextKey struct{}
type dispatchExecutionBoundaryContextKey struct{}

func WithDispatchCommitter(ctx context.Context, committer DispatchCommitter) context.Context {
	if committer == nil {
		return ctx
	}
	return context.WithValue(ctx, dispatchCommitterContextKey{}, committer)
}

// PrepareExternalDispatch commits a rendered external request before provider
// I/O. An empty state means the current run has no durable store.
func PrepareExternalDispatch(ctx context.Context, request DispatchRequest) (DispatchState, error) {
	committer, _ := ctx.Value(dispatchCommitterContextKey{}).(DispatchCommitter)
	if committer == nil {
		return DispatchState{}, nil
	}
	return committer.PrepareDispatch(ctx, request)
}

// WithPreparedDispatch exposes committed occurrence metadata to a provider.
func WithPreparedDispatch(ctx context.Context, dispatch DispatchState) context.Context {
	if dispatch.OccurrenceID == "" {
		return ctx
	}
	return context.WithValue(ctx, preparedDispatchContextKey{}, dispatch)
}

func PreparedDispatchFromContext(ctx context.Context) (DispatchState, bool) {
	dispatch, ok := ctx.Value(preparedDispatchContextKey{}).(DispatchState)
	return dispatch, ok
}

func WithDispatchExecutionBoundary(ctx context.Context, boundary DispatchExecutionBoundary) context.Context {
	if boundary.StepID == "" {
		return ctx
	}
	boundary.CallPath = append([]DebugCallFrame(nil), boundary.CallPath...)
	return context.WithValue(ctx, dispatchExecutionBoundaryContextKey{}, boundary)
}

func DispatchExecutionBoundaryFromContext(ctx context.Context) (DispatchExecutionBoundary, bool) {
	boundary, ok := ctx.Value(dispatchExecutionBoundaryContextKey{}).(DispatchExecutionBoundary)
	boundary.CallPath = append([]DebugCallFrame(nil), boundary.CallPath...)
	return boundary, ok
}
