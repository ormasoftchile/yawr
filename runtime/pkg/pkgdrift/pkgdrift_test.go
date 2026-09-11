package pkgdrift

import "testing"

func codeOf(err error) string {
	if c, ok := err.(interface{ Code() string }); ok {
		return c.Code()
	}
	return ""
}

func TestEvaluate_NoMismatch_AlwaysAllowed(t *testing.T) {
	pairs := []DigestPair{{Name: "acme.incident-tools", Expected: "sha256:aa", Actual: "sha256:aa"}}
	for _, mode := range []Mode{ModeResume, ModeReplay} {
		dec := Evaluate(mode, pairs, false, "alice")
		if !dec.Allowed || dec.Err != nil {
			t.Fatalf("mode %v: expected no-drift decision to be allowed with no error, got %+v", mode, dec)
		}
	}
}

// design/yawr/sections/13-evidence-tracing-resumption.tex §Package
// Resumption Contract: mismatch on resume without override is a hard
// refusal, PKG-009.
func TestEvaluate_Resume_Mismatch_HardFail_PKG009(t *testing.T) {
	pairs := []DigestPair{{Name: "acme.incident-tools", Expected: "sha256:aa", Actual: "sha256:bb"}}
	dec := Evaluate(ModeResume, pairs, false, "alice")
	if dec.Allowed {
		t.Fatal("expected resume with mismatch and no override to be refused")
	}
	if codeOf(dec.Err) != "PKG-009" {
		t.Fatalf("expected PKG-009, got %v", dec.Err)
	}
}

// --allow-package-drift overrides the refusal and MUST emit an auditable
// governance/packageDriftAccepted event recording both digests and the
// operator identity.
func TestEvaluate_Resume_Mismatch_AllowDrift_EmitsAuditEvent(t *testing.T) {
	pairs := []DigestPair{{Name: "acme.incident-tools", Expected: "sha256:aa", Actual: "sha256:bb"}}
	dec := Evaluate(ModeResume, pairs, true, "alice")
	if !dec.Allowed || dec.Err != nil {
		t.Fatalf("expected allow-drift override to permit resume, got %+v", dec)
	}
	if len(dec.DriftAcceptedEvents) != 1 {
		t.Fatalf("expected exactly one drift-accepted event, got %d", len(dec.DriftAcceptedEvents))
	}
	ev := dec.DriftAcceptedEvents[0]
	if ev.ExpectedCatalogDigest != "sha256:aa" || ev.ActualCatalogDigest != "sha256:bb" || ev.Operator != "alice" {
		t.Fatalf("unexpected drift-accepted event: %+v", ev)
	}
}

// Replay mode is advisory/non-fatal: a mismatch emits replay/packageDrift
// and replay proceeds regardless of any override flag.
func TestEvaluate_Replay_Mismatch_NonFatal(t *testing.T) {
	pairs := []DigestPair{{Name: "*", Expected: "sha256:cc", Actual: "sha256:dd"}}
	dec := Evaluate(ModeReplay, pairs, false, "")
	if !dec.Allowed || dec.Err != nil {
		t.Fatalf("expected replay mismatch to be non-fatal, got %+v", dec)
	}
	if len(dec.ReplayDriftEvents) != 1 {
		t.Fatalf("expected exactly one replay-drift event, got %d", len(dec.ReplayDriftEvents))
	}
	ev := dec.ReplayDriftEvents[0]
	if ev.Name != "*" || ev.ExpectedDigest != "sha256:cc" || ev.ActualDigest != "sha256:dd" {
		t.Fatalf("unexpected replay-drift event: %+v", ev)
	}
}

// Missing packages on resume are PKG-001, never a fallback to a different
// tier -- modeled here as an empty actual digest, still a mismatch subject
// to the same hard-refusal/override discipline (the PKG-001-vs-PKG-009
// distinction is made by the caller based on why the actual digest could
// not be computed at all, not by this pure comparison function).
func TestEvaluate_MultipleMismatches_AllReported(t *testing.T) {
	pairs := []DigestPair{
		{Name: "acme.a", Expected: "sha256:1", Actual: "sha256:2"},
		{Name: "acme.b", Expected: "sha256:3", Actual: "sha256:3"},
		{Name: "acme.c", Expected: "sha256:5", Actual: "sha256:6"},
	}
	dec := Evaluate(ModeResume, pairs, true, "bob")
	if len(dec.DriftAcceptedEvents) != 2 {
		t.Fatalf("expected 2 drift-accepted events (acme.b matches), got %d: %+v", len(dec.DriftAcceptedEvents), dec.DriftAcceptedEvents)
	}
}
