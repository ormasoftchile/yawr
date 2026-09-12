// Package anchor defines CadenceAnchor, the YAML shape that pins a
// routine's recurrence to an absolute reference (a weekday, a day
// of the month, an absolute date, or just a time of day) instead
// of relying on a relative offset from the previous completion.
//
// The type is intentionally a passive value — it does NOT compute
// "next occurrence" by itself; SDKs (Swift, Kotlin) own that logic.
// Yawr standardises the schema shape and metadata
// keys emitted by domain compilers.
package anchor

// CadenceAnchor pins recurrence to an absolute reference. At most
// one of Weekday, DayOfMonth, or Date should be set; when all three
// are empty, Time alone forms a "time-of-day" anchor (only valid
// with daily cadence).
type CadenceAnchor struct {
	// Weekday is a lower-case English day name ("monday".."sunday").
	// Combine with daily or weekly cadence.
	Weekday string `yaml:"weekday,omitempty"`

	// DayOfMonth is the day of the month (1..31). Values exceeding
	// the month's length clamp to the last day. Combine with
	// monthly or yearly cadence.
	DayOfMonth int `yaml:"day_of_month,omitempty"`

	// Date is a one-time anchor in YYYY-MM-DD format, evaluated in
	// the property's (or trip's) local time zone.
	Date string `yaml:"date,omitempty"`

	// Time is the local time of day in HH:MM (24h) format.
	// Defaults to "00:00" when omitted.
	Time string `yaml:"time,omitempty"`
}
