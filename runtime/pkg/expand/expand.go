// Package expand controls when included sub-runbooks are materialized
// into the execution plan.
//
// Each include site can be expanded eagerly (resolved at plan time) or
// lazily (deferred until the include step actually executes). The choice
// is configurable at three scopes, in order of precedence:
//
//  1. Per-include site: an `expand:` field on the include block.
//  2. Per-runbook: a top-level `expand:` field on the runbook.
//  3. Global default: typically supplied by the CLI (`--expand=...`)
//     or the embedding application.
//
// Lazy expansion is the right default for orchestrator runbooks that
// reference many sub-runbooks of which only a few will actually run on
// any given path. Eager expansion is the right default for small,
// linear runbooks where up-front validation of the whole tree is
// inexpensive and valuable.
package expand

import "fmt"

// Mode names a single expansion strategy.
type Mode string

const (
	// ModeUnset means the field was not specified. Resolution proceeds
	// to the next level of precedence.
	ModeUnset Mode = ""

	// ModeEager loads, parses, validates, and expands every include
	// transitively at plan time. Plan-time errors include broken
	// references in branches that may never run.
	ModeEager Mode = "eager"

	// ModeLazy defers loading and expansion until an include step
	// actually executes. Plan time is fast and only walks the local
	// runbook; broken references in unentered branches surface only
	// when they are entered (or via a separate `yawr lint` pass).
	ModeLazy Mode = "lazy"

	// ModeAuto picks eager or lazy based on a static heuristic. Today
	// it resolves to lazy when the runbook contains more than a small
	// number of include sites; otherwise eager. Callers materialize
	// auto via [Policy.Materialize].
	ModeAuto Mode = "auto"
)

// Parse returns the Mode for s, accepting empty / "eager" / "lazy" /
// "auto". Unknown values are rejected.
func Parse(s string) (Mode, error) {
	switch Mode(s) {
	case ModeUnset, ModeEager, ModeLazy, ModeAuto:
		return Mode(s), nil
	}
	return ModeUnset, fmt.Errorf("expand: invalid mode %q (want one of: eager, lazy, auto)", s)
}

// IsValid reports whether m is a recognized mode (including unset).
func (m Mode) IsValid() bool {
	switch m {
	case ModeUnset, ModeEager, ModeLazy, ModeAuto:
		return true
	}
	return false
}

// DefaultAutoIncludeThreshold is the default static include-count above
// which [ModeAuto] resolves to [ModeLazy]. Tunable via [Policy].
const DefaultAutoIncludeThreshold = 5

// Policy resolves the effective expansion Mode for an include site
// from the layered configuration described in the package doc.
type Policy struct {
	// Default applies when neither the runbook nor the include site
	// specifies a mode. Zero value (ModeUnset) means [ModeLazy] is
	// used — see [Policy.Resolve] for the rationale.
	Default Mode

	// AutoIncludeThreshold tunes [ModeAuto]: when the static include
	// count is strictly greater than this threshold, auto resolves to
	// lazy. Zero falls back to [DefaultAutoIncludeThreshold].
	AutoIncludeThreshold int
}

// Resolve picks the effective mode given the include site's mode and
// its parent runbook's mode. The result may still be [ModeAuto]; call
// [Policy.Materialize] to collapse auto to a concrete mode.
//
// Precedence (highest first):
//
//	siteMode > runbookMode > policy.Default > ModeLazy
//
// Lazy is the baked-in fallback because real-world orchestrator
// runbooks fan out into many sub-runbooks of which only a few execute
// on any given path; eager loading punishes the common case to
// validate branches that will never run. Callers that want eager
// validation can opt in via a `expand: eager` field or
// `--expand=eager`.
func (p Policy) Resolve(siteMode, runbookMode Mode) Mode {
	if siteMode != ModeUnset {
		return siteMode
	}
	if runbookMode != ModeUnset {
		return runbookMode
	}
	if p.Default != ModeUnset {
		return p.Default
	}
	return ModeLazy
}

// Materialize collapses [ModeAuto] to either [ModeEager] or [ModeLazy]
// based on includeCount. Other modes pass through unchanged.
//
// includeCount should be the number of include sites visible from the
// current runbook (direct children, not transitive) — a cheap measure
// the caller can compute without loading any sub-runbooks.
func (p Policy) Materialize(m Mode, includeCount int) Mode {
	if m != ModeAuto {
		return m
	}
	threshold := p.AutoIncludeThreshold
	if threshold <= 0 {
		threshold = DefaultAutoIncludeThreshold
	}
	if includeCount > threshold {
		return ModeLazy
	}
	return ModeEager
}
