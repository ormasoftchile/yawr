// Package pkgdrift implements the resume/replay package-integrity decision
// Package resumption
// and Substitution Events). It is a pure function of recorded vs current
// digests plus mode/override, so it can be unit-tested independently of the
// run manifest and trace-writer wiring described in the final report's
// "not wired into internal/resume" caveat.
package pkgdrift

import "github.com/ormasoftchile/yawr/runtime/pkg/errkit"

// Mode selects which of the two drift disciplines applies: resume is a
// hard refusal unless explicitly overridden; replay is always advisory.
type Mode int

const (
	ModeResume Mode = iota
	ModeReplay
)

// DigestPair is one comparison unit: a package name (or "*" for the overall
// catalog digest) plus its recorded (expected) and recomputed (actual)
// digest.
type DigestPair struct {
	Name     string
	Expected string
	Actual   string
}

// Mismatched reports whether this pair's digests differ.
func (d DigestPair) Mismatched() bool { return d.Expected != d.Actual }

// Decision is the outcome of evaluating a set of digest pairs for a given
// mode.
type Decision struct {
	// Allowed reports whether the run may proceed. Always true for
	// ModeReplay (advisory only); false for ModeResume on any mismatch
	// unless AllowDrift was set.
	Allowed bool
	// Err is the PKG-009 hard-refusal error for ModeResume when a mismatch
	// is present and AllowDrift is false. Nil otherwise.
	Err error
	// DriftAcceptedEvents are governance/packageDriftAccepted payloads to
	// emit, one per mismatched pair, when ModeResume + AllowDrift bypassed
	// a mismatch. Emitting drift acceptance is an auditable act, never a
	// silent bypass.
	DriftAcceptedEvents []DriftAcceptedEvent
	// ReplayDriftEvents are replay/packageDrift payloads to emit, one per
	// mismatched pair, in ModeReplay. Non-fatal: replay proceeds regardless.
	ReplayDriftEvents []ReplayDriftEvent
}

// DriftAcceptedEvent mirrors trace.GovernancePackageDriftAcceptedPayload
// (kept as an independent, dependency-free type here so pkgdrift does not
// need to import pkg/trace; callers translate 1:1).
type DriftAcceptedEvent struct {
	ExpectedCatalogDigest string
	ActualCatalogDigest   string
	Operator              string
}

// ReplayDriftEvent mirrors trace.ReplayPackageDriftPayload.
type ReplayDriftEvent struct {
	Name           string
	ExpectedDigest string
	ActualDigest   string
}

// Evaluate applies the Package Resumption Contract
// Package
// Resumption Contract) to pairs for the given mode. operator identifies who
// is running the resume (used only when a drift-accepted event is
// produced); it is ignored for ModeReplay.
func Evaluate(mode Mode, pairs []DigestPair, allowDrift bool, operator string) Decision {
	var mismatched []DigestPair
	for _, p := range pairs {
		if p.Mismatched() {
			mismatched = append(mismatched, p)
		}
	}

	if len(mismatched) == 0 {
		return Decision{Allowed: true}
	}

	switch mode {
	case ModeReplay:
		dec := Decision{Allowed: true}
		for _, p := range mismatched {
			dec.ReplayDriftEvents = append(dec.ReplayDriftEvents, ReplayDriftEvent{
				Name:           p.Name,
				ExpectedDigest: p.Expected,
				ActualDigest:   p.Actual,
			})
		}
		return dec
	default: // ModeResume
		if !allowDrift {
			return Decision{
				Allowed: false,
				Err: errkit.New("PKG-009", "package or catalog digest mismatch on resume: "+
					mismatchSummary(mismatched)+
					" (pass --allow-package-drift to override; the run stays resumable)"),
			}
		}
		dec := Decision{Allowed: true}
		for _, p := range mismatched {
			dec.DriftAcceptedEvents = append(dec.DriftAcceptedEvents, DriftAcceptedEvent{
				ExpectedCatalogDigest: p.Expected,
				ActualCatalogDigest:   p.Actual,
				Operator:              operator,
			})
		}
		return dec
	}
}

func mismatchSummary(pairs []DigestPair) string {
	s := ""
	for i, p := range pairs {
		if i > 0 {
			s += ", "
		}
		s += p.Name + ": expected " + p.Expected + " got " + p.Actual
	}
	return s
}
