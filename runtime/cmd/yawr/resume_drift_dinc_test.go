package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// isDINC012 reports whether err is (or wraps) a DINC-012 error.
func isDINC012(err error) bool {
	var e *errkit.Error
	return errors.As(err, &e) && e.Code() == "DINC-012"
}

// fakeDriftHandle wraps a RunState for checkResumeDynamicIncludeDrift
// without requiring a live engine.
type fakeDriftHandle struct {
	state engine.RunState
}

func (h *fakeDriftHandle) Next(_ context.Context) (*engine.StepResult, error) { return nil, nil }
func (h *fakeDriftHandle) Approve(_ context.Context, _ engine.ApprovalDecision) error {
	return nil
}
func (h *fakeDriftHandle) SubmitEvidence(_ context.Context, _ string, _ map[string]*engine.EvidenceValue) error {
	return nil
}
func (h *fakeDriftHandle) Cancel(_ context.Context, _ string) error { return nil }
func (h *fakeDriftHandle) State() engine.RunState                   { return h.state }
func (h *fakeDriftHandle) Events() <-chan engine.Event              { return nil }

// captureTraceWriter captures events written via TraceWriter.Append.
type dincCaptureWriter struct {
	events []trace.TraceEvent
}

func (w *dincCaptureWriter) Append(ev trace.TraceEvent) error {
	w.events = append(w.events, ev)
	return nil
}
func (w *dincCaptureWriter) Close() error { return nil }

// writeTempRunbook writes content to a temp file and returns the path.
func writeTempRunbook(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
	return path
}

// computeDigest returns the SHA-256 digest of a file.
func computeDigest(t *testing.T, path string) string {
	t.Helper()
	d, err := dincFileDigestSHA256(path)
	if err != nil {
		t.Fatalf("dincFileDigestSHA256(%s): %v", path, err)
	}
	return d
}

// TestCheckResumeDynamicIncludeDrift_NoPins verifies that when no dynamic
// include pins are present in the plan, the function returns nil immediately.
func TestCheckResumeDynamicIncludeDrift_NoPins(t *testing.T) {
	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{DynamicIncludes: nil},
		},
	}}
	err := checkResumeDynamicIncludeDrift(context.Background(), engine.EngineConfig{}, handle, "run-1", false)
	if err != nil {
		t.Fatalf("expected nil error with no pins, got: %v", err)
	}
}

// TestCheckResumeDynamicIncludeDrift_NilPlan verifies that a nil plan is
// handled gracefully.
func TestCheckResumeDynamicIncludeDrift_NilPlan(t *testing.T) {
	handle := &fakeDriftHandle{state: engine.RunState{Plan: nil}}
	err := checkResumeDynamicIncludeDrift(context.Background(), engine.EngineConfig{}, handle, "run-1", false)
	if err != nil {
		t.Fatalf("expected nil error with nil plan, got: %v", err)
	}
}

// TestCheckResumeDynamicIncludeDrift_Match verifies that matching digests
// produce no error and no trace events.
func TestCheckResumeDynamicIncludeDrift_Match(t *testing.T) {
	dir := t.TempDir()
	path := writeTempRunbook(t, dir, "tsg.yaml", "steps: []")
	digest := computeDigest(t, path)

	writer := &dincCaptureWriter{}
	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{
				DynamicIncludes: []schema.LockedDynamicInclude{
					{StepID: "s1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: digest},
				},
			},
		},
	}}
	ecfg := engine.EngineConfig{TraceWriter: writer}
	if err := checkResumeDynamicIncludeDrift(context.Background(), ecfg, handle, "run-1", false); err != nil {
		t.Fatalf("unexpected error with matching digest: %v", err)
	}
	if len(writer.events) != 0 {
		t.Errorf("expected no events, got %d", len(writer.events))
	}
}

// TestCheckResumeDynamicIncludeDrift_DigestMismatch_NoAllowDrift verifies
// the hard DINC-012 refusal when the on-disk file digest has changed.
func TestCheckResumeDynamicIncludeDrift_DigestMismatch_NoAllowDrift(t *testing.T) {
	dir := t.TempDir()
	path := writeTempRunbook(t, dir, "tsg.yaml", "steps: []")

	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{
				DynamicIncludes: []schema.LockedDynamicInclude{
					{StepID: "s1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: "stale-digest-abc"},
				},
			},
		},
	}}
	err := checkResumeDynamicIncludeDrift(context.Background(), engine.EngineConfig{}, handle, "run-1", false)
	if err == nil {
		t.Fatal("expected DINC-012 error, got nil")
	}
	if !isDINC012(err) {
		t.Errorf("expected DINC-012, got: %v", err)
	}
}

// TestCheckResumeDynamicIncludeDrift_VanishedEntry verifies that a pinned
// file that no longer exists on disk triggers DINC-012.
func TestCheckResumeDynamicIncludeDrift_VanishedEntry(t *testing.T) {
	dir := t.TempDir()
	vanishedPath := filepath.Join(dir, "vanished.yaml")
	// File is never written — it doesn't exist.

	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{
				DynamicIncludes: []schema.LockedDynamicInclude{
					{StepID: "s1", QualifiedID: "pkg/tsg", AbsPath: vanishedPath, FileDigest: "any-digest"},
				},
			},
		},
	}}
	err := checkResumeDynamicIncludeDrift(context.Background(), engine.EngineConfig{}, handle, "run-1", false)
	if err == nil {
		t.Fatal("expected DINC-012 error for vanished file, got nil")
	}
	if !isDINC012(err) {
		t.Errorf("expected DINC-012, got: %v", err)
	}
}

// TestCheckResumeDynamicIncludeDrift_PackageVersionChanged verifies that
// updating a runbook file (simulating a package version bump that changes
// the file content) also triggers drift detection.
func TestCheckResumeDynamicIncludeDrift_PackageVersionChanged(t *testing.T) {
	dir := t.TempDir()
	path := writeTempRunbook(t, dir, "tsg.yaml", "# original v1.0 content")
	originalDigest := computeDigest(t, path)

	// Simulate package version bump: file content changes.
	if err := os.WriteFile(path, []byte("# updated v2.0 content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{
				DynamicIncludes: []schema.LockedDynamicInclude{
					{
						StepID:         "s1",
						QualifiedID:    "pkg/tsg",
						AbsPath:        path,
						FileDigest:     originalDigest,
						PackageVersion: "1.0.0",
					},
				},
			},
		},
	}}
	err := checkResumeDynamicIncludeDrift(context.Background(), engine.EngineConfig{}, handle, "run-1", false)
	if err == nil {
		t.Fatal("expected DINC-012 after package version bump changed file, got nil")
	}
	if !isDINC012(err) {
		t.Errorf("expected DINC-012, got: %v", err)
	}
}

// TestCheckResumeDynamicIncludeDrift_AllowDrift_ReturnsNil verifies that
// --allow-package-drift suppresses the DINC-012 error.
func TestCheckResumeDynamicIncludeDrift_AllowDrift_ReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := writeTempRunbook(t, dir, "tsg.yaml", "steps: []")

	writer := &dincCaptureWriter{}
	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{
				DynamicIncludes: []schema.LockedDynamicInclude{
					{StepID: "s1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: "stale"},
				},
			},
		},
	}}
	ecfg := engine.EngineConfig{TraceWriter: writer}
	if err := checkResumeDynamicIncludeDrift(context.Background(), ecfg, handle, "run-1", true); err != nil {
		t.Fatalf("expected nil with allowDrift=true, got: %v", err)
	}
}

// TestCheckResumeDynamicIncludeDrift_DriftAcceptedIsAuditable verifies
// that accepting drift emits a governance/packageDriftAccepted trace event
// (B-12: drift acceptance must always be auditable, never silent).
func TestCheckResumeDynamicIncludeDrift_DriftAcceptedIsAuditable(t *testing.T) {
	dir := t.TempDir()
	path := writeTempRunbook(t, dir, "tsg.yaml", "steps: []")
	actualDigest := computeDigest(t, path)

	writer := &dincCaptureWriter{}
	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{
				DynamicIncludes: []schema.LockedDynamicInclude{
					{StepID: "s1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: "stale-digest"},
				},
			},
		},
	}}
	ecfg := engine.EngineConfig{TraceWriter: writer}
	if err := checkResumeDynamicIncludeDrift(context.Background(), ecfg, handle, "run-1", true); err != nil {
		t.Fatalf("expected nil with allowDrift=true, got: %v", err)
	}

	var found bool
	for _, ev := range writer.events {
		if ev.Kind != trace.EventKindGovernancePackageDriftAccepted {
			continue
		}
		found = true
		var payload trace.GovernancePackageDriftAcceptedPayload
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("unmarshal audit payload: %v", err)
		}
		if payload.ExpectedCatalogDigest != "stale-digest" {
			t.Errorf("expected recorded digest in audit event, got %q", payload.ExpectedCatalogDigest)
		}
		if payload.ActualCatalogDigest != actualDigest {
			t.Errorf("expected current digest in audit event, got %q", payload.ActualCatalogDigest)
		}
	}
	if !found {
		t.Error("no governance/packageDriftAccepted event emitted — drift acceptance is not auditable")
	}
}

// TestCheckResumeDynamicIncludeDrift_AmbiguityStateChange_PinAlwaysWins
// demonstrates that even if the catalog now resolves the ref differently
// (B-13: ambiguity state change), the pin record is what matters for
// resume drift — the file digest is checked against the pinned AbsPath only.
// If the pinned file is unchanged, resume succeeds regardless of catalog state.
func TestCheckResumeDynamicIncludeDrift_AmbiguityStateChange_PinAlwaysWins(t *testing.T) {
	dir := t.TempDir()
	// Original pinned file — unchanged on disk.
	path := writeTempRunbook(t, dir, "tsg-original.yaml", "# tsg original, unique resolution")
	digest := computeDigest(t, path)

	// A second file exists (simulating catalog now having two matches for
	// the same ref, i.e. what-was-unique-is-now-ambiguous). The resume
	// check does not consult the catalog; it only checks pin.AbsPath.
	_ = writeTempRunbook(t, dir, "tsg-ambiguous.yaml", "# tsg duplicate — catalog ambiguous now")

	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{
				DynamicIncludes: []schema.LockedDynamicInclude{
					{StepID: "s1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: digest},
				},
			},
		},
	}}
	// Pin file is unchanged; resume must succeed (catalog state is irrelevant).
	err := checkResumeDynamicIncludeDrift(context.Background(), engine.EngineConfig{}, handle, "run-1", false)
	if err != nil {
		t.Fatalf("B-13: pinned file unchanged; resume should succeed regardless of catalog: %v", err)
	}
}
