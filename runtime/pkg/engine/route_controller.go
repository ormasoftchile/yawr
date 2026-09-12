package engine

import (
	"context"
	"errors"
)

// ErrRouteTestTargetReached propagates a nested target boundary to the root
// engine without converting the route-test result into cancellation or failure.
var ErrRouteTestTargetReached = errors.New("engine: route-test target reached")

// RouteTestTargetError carries the exact nested target location to the root
// run handle, which alone owns the durable paused-at-boundary checkpoint.
type RouteTestTargetError struct {
	Location DebugLocation
}

func (err *RouteTestTargetError) Error() string { return ErrRouteTestTargetReached.Error() }
func (err *RouteTestTargetError) Unwrap() error { return ErrRouteTestTargetReached }

func NewRouteTestTargetError(location DebugLocation) error {
	return &RouteTestTargetError{Location: location}
}

func RouteTestTargetLocation(err error) (DebugLocation, bool) {
	var target *RouteTestTargetError
	if !errors.As(err, &target) {
		return DebugLocation{}, false
	}
	return target.Location, true
}

// RouteTestBoundaryError marks a failed exact response binding or safety gate.
type RouteTestBoundaryError struct{ Err error }

func (err *RouteTestBoundaryError) Error() string { return err.Err.Error() }
func (err *RouteTestBoundaryError) Unwrap() error { return err.Err }

func NewRouteTestBoundaryError(err error) error {
	if err == nil {
		return nil
	}
	return &RouteTestBoundaryError{Err: err}
}

func IsRouteTestBoundaryError(err error) bool {
	var boundary *RouteTestBoundaryError
	return errors.As(err, &boundary)
}

// RouteTestDecision controls execution at a route-test step boundary.
type RouteTestDecision string

const (
	RouteTestContinue      RouteTestDecision = "continue"
	RouteTestTargetReached RouteTestDecision = "target_reached"
)

// RouteTestController enforces the target and zero-dispatch boundary before a
// step delay, governance check, special engine path, or executor lookup.
type RouteTestController interface {
	BeforeStep(context.Context, DebugLocation, ResolvedStep) (RouteTestDecision, error)
}

type routeTestControllerContextKey struct{}

func WithRouteTestController(ctx context.Context, controller RouteTestController) context.Context {
	if controller == nil {
		return ctx
	}
	return context.WithValue(ctx, routeTestControllerContextKey{}, controller)
}

func RouteTestControllerFromContext(ctx context.Context) RouteTestController {
	controller, _ := ctx.Value(routeTestControllerContextKey{}).(RouteTestController)
	return controller
}

// RecordRouteTestExternalDispatch records a real external boundary call when
// route-test instrumentation is present in ctx.
func RecordRouteTestExternalDispatch(ctx context.Context) {
	recorder, _ := RouteTestControllerFromContext(ctx).(interface{ RecordExternalDispatch() })
	if recorder != nil {
		recorder.RecordExternalDispatch()
	}
}

func RouteTestTargetReachedFromContext(ctx context.Context) bool {
	controller := RouteTestControllerFromContext(ctx)
	state, ok := controller.(interface{ TargetReached() bool })
	return ok && state.TargetReached()
}
