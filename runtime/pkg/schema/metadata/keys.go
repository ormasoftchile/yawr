// Package metadata defines the canonical metadata keys emitted by
// compilers into the `metadata:` block of a yawr.runbook/v1 document.
// Compilers MUST emit using these constants so that SDKs (Swift,
// Kotlin) can decode the metadata uniformly without hard-coding
// strings.
//
// New keys may be added freely; existing keys must never change
// meaning. SDKs that don't recognise a key MUST ignore it (the
// "be liberal in what you accept" rule keeps old SDK builds
// forward-compatible with new compilers).
package metadata

// Core metadata keys emitted on every routine runbook.
const (
	// Type is "routine" for cadence-driven runbooks and
	// "incident" for ad-hoc runs.
	KeyType = "type"

	// RoutineID is the kit-local identifier (kebab-case).
	KeyRoutineID = "routine_id"

	// PropertyID is the kit-local property identifier the routine
	// belongs to.
	KeyPropertyID = "property_id"

	// Interval is the cadence interval as a Duration string
	// (e.g. "7d", "1M"). Empty when the routine has no recurring
	// cadence (one-shots).
	KeyInterval = "interval"

	// Zone and Asset are optional context for the routine target.
	KeyZone  = "zone"
	KeyAsset = "asset"
)

// Cadence-anchor metadata keys. At most one of AnchorWeekday /
// AnchorDayOfMonth / AnchorDate is emitted per runbook. AnchorTime
// is emitted independently when a time-of-day is declared.
const (
	KeyAnchorWeekday    = "anchor_weekday"
	KeyAnchorDayOfMonth = "anchor_day_of_month"
	KeyAnchorDate       = "anchor_date"
	KeyAnchorTime       = "anchor_time"
)

// Notification metadata keys. Both are Duration strings (e.g.
// "15m", "2h"); see github.com/ormasoftchile/yawr/runtime-core/pkg/duration.
const (
	KeyRemindBefore  = "remind_before"
	KeyEscalateAfter = "escalate_after"
)
