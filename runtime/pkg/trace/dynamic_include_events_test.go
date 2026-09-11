package trace_test

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestEventKindConstants_DynamicInclude(t *testing.T) {
	// Verify the three dynamic-include event kinds are defined with the
	// exact wire-format strings.
	cases := []struct {
		kind     trace.EventKind
		expected string
	}{
		{trace.EventKindIncludeResolved, "include/resolved"},
		{trace.EventKindIncludeNotFound, "include/notFound"},
		{trace.EventKindReplayDynamicIncludeDrift, "replay/dynamicIncludeDrift"},
	}
	for _, tc := range cases {
		if string(tc.kind) != tc.expected {
			t.Errorf("EventKind %q: expected wire value %q, got %q", tc.kind, tc.expected, string(tc.kind))
		}
	}
}

func TestIncludeResolvedPayload_Fields(t *testing.T) {
	p := trace.IncludeResolvedPayload{
		StepID:         "s1",
		RenderedRef:    "pkg/tsg-disk",
		QualifiedID:    "pkg/tsg-disk",
		AbsPath:        "/resolved/tsg-disk.runbook.yaml",
		PackageName:    "pkg",
		PackageVersion: "1.0.0",
		FileDigest:     "sha256:abc",
		PackageDigest:  "sha256:def",
		Depth:          2,
		EffectiveGovernance: trace.EffectiveGovernancePayload{
			RequireApproval: true,
		},
	}
	if p.StepID != "s1" || p.Depth != 2 || !p.EffectiveGovernance.RequireApproval {
		t.Errorf("unexpected payload: %+v", p)
	}
}

func TestIncludeNotFoundPayload_Fields(t *testing.T) {
	p := trace.IncludeNotFoundPayload{
		StepID:      "s1",
		RenderedRef: "missing-ref",
		ErrorCode:   "DINC-002",
		Reason:      "not found",
	}
	if p.ErrorCode != "DINC-002" {
		t.Errorf("unexpected error code: %q", p.ErrorCode)
	}
}

func TestReplayDynamicIncludeDriftPayload_Fields(t *testing.T) {
	p := trace.ReplayDynamicIncludeDriftPayload{
		StepID:         "s1",
		QualifiedID:    "pkg/tsg",
		ExpectedDigest: "sha256:aaa",
		ActualDigest:   "sha256:bbb",
	}
	if p.ExpectedDigest == p.ActualDigest {
		t.Error("expected digests to differ in drift payload")
	}
}
