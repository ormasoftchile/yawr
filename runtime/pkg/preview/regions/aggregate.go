package regions

import (
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	regschema "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// aggregate computes a region's overall status from its members'
// statuses according to the rule named on the region. Only the
// "default" rule is implemented in v1; any other (kit-defined) rule
// falls back to default.
//
// The default rule (specs/regions-v1.md):
//
//	any failed                         → failed
//	any running                        → running
//	any cancelled (none of above)      → cancelled
//	any pending  (none of above)       → pending
//	all skipped                        → skipped
//	all completed-or-skipped           → completed
//
// An empty members list returns StatusPending unless the region has a
// SkipReason, in which case it returns StatusSkipped.
func aggregate(r *regschema.Region, statuses []runstate.Status) runstate.Status {
	if len(statuses) == 0 {
		if r != nil && r.SkipReason != "" {
			return runstate.StatusSkipped
		}
		return runstate.StatusPending
	}

	var anyFailed, anyRunning, anyCancelled, anyPending bool
	allSkipped := true
	allCompletedOrSkipped := true

	for _, s := range statuses {
		switch s {
		case runstate.StatusFailed:
			anyFailed = true
		case runstate.StatusRunning:
			anyRunning = true
		case runstate.StatusCancelled:
			anyCancelled = true
		case runstate.StatusPending:
			anyPending = true
		}
		if s != runstate.StatusSkipped {
			allSkipped = false
		}
		if s != runstate.StatusCompleted && s != runstate.StatusSkipped {
			allCompletedOrSkipped = false
		}
	}

	switch {
	case anyFailed:
		return runstate.StatusFailed
	case anyRunning:
		return runstate.StatusRunning
	case anyCancelled:
		return runstate.StatusCancelled
	case anyPending:
		return runstate.StatusPending
	case allSkipped:
		return runstate.StatusSkipped
	case allCompletedOrSkipped:
		return runstate.StatusCompleted
	default:
		// Unreachable in practice (every status is covered above), but
		// keep the door open for future additions to runstate.Status.
		return runstate.StatusPending
	}
}
