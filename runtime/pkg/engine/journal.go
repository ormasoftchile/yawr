package engine

import (
	"context"
	"encoding/json"

	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// ErrIdempotencyConflict reports that a caller-supplied idempotency key was
// reused with a request whose canonical digest differs from the original.
var ErrIdempotencyConflict = tool.ErrIdempotencyConflict

// SettledOutcome is the replayable result recorded when a standalone dispatch
// settles. It is what a duplicate request receives instead of a second
// external effect. Error is non-empty when the provider reported failure.
type SettledOutcome struct {
	ExitCode int             `json:"exit_code"`
	Stdout   string          `json:"stdout,omitempty"`
	Stderr   string          `json:"stderr,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// DispatchJournal is the durable intent journal used by ad-hoc tool
// invocations, which have no workflow run to own their DispatchState.
//
// It extends DispatchCommitter (the same seam workflow steps use) with the
// settlement, lookup and exclusion operations a run handle otherwise
// provides. Implementations MUST:
//
//   - persist the prepared DispatchState durably before PrepareDispatch returns;
//   - never persist the rendered request itself (only its digest);
//   - for a request carrying an idempotency key, return the existing settled
//     state on an identical retry, ErrIdempotencyConflict on a different
//     digest, and ErrIndeterminate when a previous attempt prepared but never
//     settled;
//   - make LockKey exclusive across every process sharing the journal.
type DispatchJournal interface {
	DispatchCommitter

	// SettleDispatch durably records the outcome of a prepared occurrence.
	SettleDispatch(ctx context.Context, occurrenceID string, outcome SettledOutcome) error

	// MarkIndeterminate records that the effect of a prepared occurrence is
	// unknown (for example, cancelled mid-flight). It never becomes settled
	// without explicit operator resolution.
	MarkIndeterminate(ctx context.Context, occurrenceID string) error

	// LookupDispatch returns the recorded state for an occurrence.
	LookupDispatch(ctx context.Context, occurrenceID string) (*DispatchState, bool)

	// SettledOutcomeFor returns the replayable outcome of a settled occurrence.
	SettledOutcomeFor(ctx context.Context, occurrenceID string) (*SettledOutcome, bool)

	// LockKey acquires exclusive ownership of an idempotency key for the full
	// prepare → execute → settle span. The returned release func must be
	// called exactly once. Implementations must ensure a crashed holder's
	// lock is released (so the survivor observes an indeterminate intent
	// rather than blocking forever).
	LockKey(ctx context.Context, key string) (release func(), err error)
}
