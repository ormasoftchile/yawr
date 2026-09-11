package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

var ErrInteractionConflict = errors.New("engine: durable interaction state conflicts with current execution")

// InteractionCommitter durably records an interaction before publication and
// its accepted answer before the blocked executor is released.
type InteractionCommitter interface {
	PrepareInteraction(ctx context.Context, proposed InteractionState) (InteractionState, error)
	AcceptInteraction(ctx context.Context, turnID, answerDigest string, answer json.RawMessage) (InteractionState, error)
}

type InteractionCommandCommitter interface {
	InteractionCommitter
	AcceptInteractionCommand(
		ctx context.Context,
		turnID string,
		commandID string,
		commandDigest string,
		answerDigest string,
		answer json.RawMessage,
	) (InteractionState, error)
}

type interactionCommitterContextKey struct{}
type interactionInvocationTrackerContextKey struct{}

type InteractionInvocationTracker struct {
	mu     sync.Mutex
	counts map[string]int
	reuses map[string]int
}

func NewInteractionInvocationTracker() *InteractionInvocationTracker {
	return &InteractionInvocationTracker{counts: make(map[string]int), reuses: make(map[string]int)}
}

func NewInteractionInvocationTrackerFromCounts(counts map[string]int) *InteractionInvocationTracker {
	tracker := NewInteractionInvocationTracker()
	for key, count := range counts {
		if key != "" && count > 0 {
			tracker.counts[key] = count
		}
	}
	return tracker
}

func (tracker *InteractionInvocationTracker) Next(nodeID, kind string) int {
	return tracker.NextOccurrence(nodeID, kind, "", 0)
}

func (tracker *InteractionInvocationTracker) NextOccurrence(nodeID, kind, frameID string, frameStepIndex int) int {
	if tracker == nil {
		return 1
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.counts == nil {
		tracker.counts = make(map[string]int)
	}
	key := InteractionInvocationCountKey(nodeID, kind, frameID, frameStepIndex)
	if remaining := tracker.reuses[key]; remaining > 0 {
		ordinal := tracker.counts[key] - remaining + 1
		if remaining == 1 {
			delete(tracker.reuses, key)
		} else {
			tracker.reuses[key] = remaining - 1
		}
		return ordinal
	}
	tracker.counts[key]++
	return tracker.counts[key]
}

func (tracker *InteractionInvocationTracker) Snapshot() map[string]int {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	counts := make(map[string]int, len(tracker.counts))
	for key, count := range tracker.counts {
		counts[key] = count
	}
	return counts
}

func (tracker *InteractionInvocationTracker) Rewind(nodeID, kind string) {
	tracker.RewindOccurrence(nodeID, kind, "", 0)
}

func (tracker *InteractionInvocationTracker) RewindOccurrence(nodeID, kind, frameID string, frameStepIndex int) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	key := InteractionInvocationCountKey(nodeID, kind, frameID, frameStepIndex)
	if tracker.reuses == nil {
		tracker.reuses = make(map[string]int)
	}
	if tracker.reuses[key] < tracker.counts[key] {
		tracker.reuses[key]++
	}
	tracker.mu.Unlock()
}

func InteractionInvocationCountKey(nodeID, kind, frameID string, frameStepIndex int) string {
	key := nodeID + "\x00" + kind
	if frameID != "" {
		key += "\x00" + frameID + "\x00" + strconv.Itoa(frameStepIndex)
	}
	return key
}

func WithInteractionCommitter(ctx context.Context, committer InteractionCommitter) context.Context {
	if committer == nil {
		return ctx
	}
	return context.WithValue(ctx, interactionCommitterContextKey{}, committer)
}

func InteractionCommitterFromContext(ctx context.Context) InteractionCommitter {
	committer, _ := ctx.Value(interactionCommitterContextKey{}).(InteractionCommitter)
	return committer
}

func WithInteractionInvocationTracker(ctx context.Context, tracker *InteractionInvocationTracker) context.Context {
	if tracker == nil {
		return ctx
	}
	return context.WithValue(ctx, interactionInvocationTrackerContextKey{}, tracker)
}

func InteractionInvocationTrackerFromContext(ctx context.Context) *InteractionInvocationTracker {
	tracker, _ := ctx.Value(interactionInvocationTrackerContextKey{}).(*InteractionInvocationTracker)
	return tracker
}

func InteractionPayloadDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest)
}
