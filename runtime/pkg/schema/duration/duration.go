// Package duration defines the canonical time-interval type shared
// across all yawr domains. Authors write durations as "<int><unit>"
// strings in YAML (e.g. "15m", "2h", "7d"). The supported units are:
//
//	m  minutes
//	h  hours
//	d  days
//	w  weeks   (= 7 days)
//	M  months  (~ 30 days, approximate)
//	y  years   (~ 365 days, approximate)
//
// Months and years are approximate by design; consumers that need
// calendar-aware arithmetic should use a Calendar/time-zone-aware
// path on top of [Duration.ToSeconds] rather than treating the
// approximation as authoritative.
package duration

import (
	"fmt"
	"strconv"
	"strings"
)

// Duration is a value+unit pair. The zero value is invalid (Value=0
// is treated as "unset" by callers that test with Value > 0).
type Duration struct {
	Value int
	Unit  string
}

// Parse parses a string of the form "<int><unit>", e.g. "15m" or
// "7d". It rejects empty values, unknown units, and signs.
func Parse(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return Duration{}, fmt.Errorf("invalid duration format: %q (expected format: <number><unit>)", s)
	}

	unitIdx := -1
	for i, ch := range s {
		if ch < '0' || ch > '9' {
			unitIdx = i
			break
		}
	}
	if unitIdx <= 0 {
		return Duration{}, fmt.Errorf("invalid duration format: %q (expected format: <number><unit>)", s)
	}

	value, err := strconv.Atoi(s[:unitIdx])
	if err != nil {
		return Duration{}, fmt.Errorf("invalid duration value: %q", s[:unitIdx])
	}

	unit := s[unitIdx:]
	switch unit {
	case "m", "h", "d", "w", "M", "y":
	default:
		return Duration{}, fmt.Errorf("invalid duration unit: %q (must be m, h, d, w, M, or y)", unit)
	}
	return Duration{Value: value, Unit: unit}, nil
}

// ToDays returns the approximate number of whole days. Sub-day units
// truncate toward zero (15m → 0, 23h → 0, 24h → 1).
func (d Duration) ToDays() int {
	switch d.Unit {
	case "m":
		return d.Value / (60 * 24)
	case "h":
		return d.Value / 24
	case "d":
		return d.Value
	case "w":
		return d.Value * 7
	case "M":
		return d.Value * 30
	case "y":
		return d.Value * 365
	default:
		return 0
	}
}

// ToSeconds returns the approximate number of seconds. Use this for
// sub-day-precision callers (notifications, reminders).
func (d Duration) ToSeconds() int64 {
	switch d.Unit {
	case "m":
		return int64(d.Value) * 60
	case "h":
		return int64(d.Value) * 3600
	case "d":
		return int64(d.Value) * 86_400
	case "w":
		return int64(d.Value) * 7 * 86_400
	case "M":
		return int64(d.Value) * 30 * 86_400
	case "y":
		return int64(d.Value) * 365 * 86_400
	default:
		return 0
	}
}

// String returns the canonical "<value><unit>" form.
func (d Duration) String() string {
	return fmt.Sprintf("%d%s", d.Value, d.Unit)
}

// UnmarshalYAML lets gopkg.in/yaml.v3 (and v2) decode a Duration from
// a scalar string node.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := Parse(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalYAML emits the canonical string form.
func (d Duration) MarshalYAML() (interface{}, error) {
	return d.String(), nil
}
