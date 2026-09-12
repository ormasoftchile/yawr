package trace

import "context"

// TraceReader reads events from a JSONL trace file or run store.
// Implementations MUST be safe for concurrent use.
type TraceReader interface {
	// ReadAll returns all events in sequence order.
	ReadAll(ctx context.Context) ([]TraceEvent, error)

	// ReadSince returns events with sequence > afterSeq.
	ReadSince(ctx context.Context, afterSeq int64) ([]TraceEvent, error)

	// ReadFiltered returns events matching the filter predicate.
	ReadFiltered(ctx context.Context, filter TraceFilter) ([]TraceEvent, error)
}
