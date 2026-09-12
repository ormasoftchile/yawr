package engine

import (
	"context"
	"errors"
	"io"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

// The adapter preserves context when creating a nested engine. This private
// link follows the existing forwarder without changing the public observer API.
type publicationParentKey struct{}

type callbackPublication struct {
	event enginepkg.Event
	done  chan struct{}
}

var errPublicationCancelled = errors.New("engine: cancelled at publication boundary")

func (h *runHandle) executionErrorLocked(ctx context.Context) error {
	if h.traceErr != nil {
		return h.traceErr
	}
	if h.done.Load() {
		if h.run.Error != nil {
			return h.run.Error
		}
		if h.run.Status == enginepkg.RunStatusCancelled {
			return errPublicationCancelled
		}
		return io.EOF
	}
	if err := h.runCtx.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// Only execution boundaries wait. Cancel, State, forwarding and ordinary event
// drains must never wait for a callback that may currently be calling them.
// Next remains caller-driven and sequential, not callback-recursive.
func (h *runHandle) publishBoundaryLocked(ctx context.Context) error {
	publication := h.lastCallback
	h.mu.Unlock()
	err := h.awaitPublication(ctx, publication)
	h.mu.Lock()
	if stateErr := h.executionErrorLocked(ctx); stateErr != nil {
		return stateErr
	}
	return err
}

func (h *runHandle) awaitPublication(ctx context.Context, publication *callbackPublication) error {
	var eventID string
	if publication != nil {
		eventID = publication.event.EventID
		if err := h.awaitOwnPublication(ctx, publication); err != nil {
			return err
		}
	}
	// Child callback return acknowledges the forwarder, not a parent's queued
	// observer. Follow the exact event up the chain, outside every run mutex.
	for parent, _ := h.traceCtx.Value(publicationParentKey{}).(*runHandle); parent != nil; {
		parent.mu.Lock()
		err := parent.executionErrorLocked(ctx)
		parent.mu.Unlock()
		if err != nil {
			return err
		}
		parent.callbackMu.Lock()
		pending := parent.callbackPending[eventID]
		parent.callbackMu.Unlock()
		if pending != nil {
			if err := parent.awaitOwnPublication(ctx, pending); err != nil {
				return err
			}
		}
		parent.mu.Lock()
		err = parent.executionErrorLocked(ctx)
		parent.mu.Unlock()
		if err != nil {
			return err
		}
		parent, _ = parent.traceCtx.Value(publicationParentKey{}).(*runHandle)
	}
	return nil
}

func (h *runHandle) awaitOwnPublication(ctx context.Context, publication *callbackPublication) error {
	for {
		h.drainCallbacksThrough(publication)
		h.callbackMu.Lock()
		idle, running := h.callbackIdle, h.callbacksRunning
		h.callbackMu.Unlock()
		select {
		case <-publication.done:
			return nil
		default:
		}
		if !running {
			continue
		}
		select {
		case <-publication.done:
			return nil
		case <-idle:
			// The previous owner stopped at its own event, not at ours.
		case <-ctx.Done():
			return ctx.Err()
		case <-h.runCtx.Done():
			return h.runCtx.Err()
		}
	}
}
