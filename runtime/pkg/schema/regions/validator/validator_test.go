package validator_test

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema/regions/validator"
)

// rb constructs a minimal runbook with the given flow and an optional manifest.
func rb(flow []schema.FlowNode, m *regions.Manifest) *schema.Runbook {
	return &schema.Runbook{
		APIVersion: "yawr.runbook/v1",
		ID:         "rb",
		Name:       "rb",
		Flow:       flow,
		Regions:    m,
	}
}

// step returns a FlowNode wrapping a Step with the given id and type.
func step(id string, t schema.StepType) schema.FlowNode {
	return schema.FlowNode{Step: &schema.Step{ID: id, Type: t}}
}

// stepHuman is a convenience for cli steps (validator is type-agnostic).
func stepHuman(id string) schema.FlowNode { return step(id, schema.StepTypeCLI) }

// region returns a Region with sensible defaults filled in for tests.
func region(id string, members, entries, exits []string) regions.Region {
	return regions.Region{
		ID:      id,
		Kit:     "test",
		OpType:  "test.op",
		OpID:    id + "_op",
		Label:   "Test " + id,
		Members: members,
		Entries: entries,
		Exits:   exits,
	}
}

func manifest(rs ...regions.Region) *regions.Manifest {
	return &regions.Manifest{SchemaVersion: regions.SchemaVersion, Regions: rs}
}

// codes extracts just the Error.Code values for assertion convenience.
func codes(errs []validator.Error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Code
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func assertNoErrors(t *testing.T, errs []validator.Error) {
	t.Helper()
	if len(errs) != 0 {
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		t.Fatalf("expected no errors, got %d:\n  %s", len(errs), strings.Join(msgs, "\n  "))
	}
}

func assertHasCode(t *testing.T, errs []validator.Error, want string) {
	t.Helper()
	if !contains(codes(errs), want) {
		t.Fatalf("expected error code %q in %v", want, codes(errs))
	}
}

// --- Positive cases ---

func TestValidate_NilManifest_NoErrors(t *testing.T) {
	r := rb([]schema.FlowNode{stepHuman("a")}, nil)
	assertNoErrors(t, validator.Validate(r))
}

func TestValidate_SimpleSiblingRegion(t *testing.T) {
	r := rb(
		[]schema.FlowNode{stepHuman("a"), stepHuman("b")},
		manifest(region("r1", []string{"a", "b"}, []string{"a"}, []string{"b"})),
	)
	assertNoErrors(t, validator.Validate(r))
}

func TestValidate_SingleNodeRegion(t *testing.T) {
	r := rb(
		[]schema.FlowNode{stepHuman("a")},
		manifest(region("r1", []string{"a"}, []string{"a"}, []string{"a"})),
	)
	assertNoErrors(t, validator.Validate(r))
}

func TestValidate_RegionContainingIterate(t *testing.T) {
	// iterate "loop" with "inner" inside; both members of one region.
	flow := []schema.FlowNode{
		{Iterate: &schema.IterateNode{
			ID:    "loop",
			Steps: []schema.FlowNode{stepHuman("inner")},
		}},
	}
	r := rb(flow, manifest(region("r1",
		[]string{"loop", "inner"},
		[]string{"loop"},
		[]string{"loop"},
	)))
	assertNoErrors(t, validator.Validate(r))
}

func TestValidate_EmptyRegionWithSkipReason(t *testing.T) {
	m := manifest(regions.Region{
		ID: "r1", Kit: "k", OpType: "t", OpID: "o", Label: "L",
		Members: nil, Entries: nil, Exits: nil,
		SkipReason: "severity=emergency",
	})
	r := rb([]schema.FlowNode{stepHuman("a")}, m)
	assertNoErrors(t, validator.Validate(r))
}

// --- Negative cases ---

func TestValidate_UnknownSchemaVersion(t *testing.T) {
	m := &regions.Manifest{SchemaVersion: "99", Regions: []regions.Region{
		region("r1", []string{"a"}, []string{"a"}, []string{"a"}),
	}}
	r := rb([]schema.FlowNode{stepHuman("a")}, m)
	assertHasCode(t, validator.Validate(r), "schema-version-unsupported")
}

func TestValidate_DuplicateRegionID(t *testing.T) {
	r := rb(
		[]schema.FlowNode{stepHuman("a"), stepHuman("b")},
		manifest(
			region("r1", []string{"a"}, []string{"a"}, []string{"a"}),
			region("r1", []string{"b"}, []string{"b"}, []string{"b"}),
		),
	)
	assertHasCode(t, validator.Validate(r), "region-duplicate-id")
}

func TestValidate_UnknownDisplayValue(t *testing.T) {
	rg := region("r1", []string{"a"}, []string{"a"}, []string{"a"})
	rg.Display = "weird"
	r := rb([]schema.FlowNode{stepHuman("a")}, manifest(rg))
	assertHasCode(t, validator.Validate(r), "region-display-unknown")
}

func TestValidate_EmptyRegionWithoutSkipReason(t *testing.T) {
	rg := regions.Region{
		ID: "r1", Kit: "k", OpType: "t", OpID: "o", Label: "L",
	}
	r := rb([]schema.FlowNode{stepHuman("a")}, manifest(rg))
	assertHasCode(t, validator.Validate(r), "region-empty-without-skip-reason")
}

func TestValidate_SkipReasonWithMembers(t *testing.T) {
	rg := region("r1", []string{"a"}, []string{"a"}, []string{"a"})
	rg.SkipReason = "should-not-be-set"
	r := rb([]schema.FlowNode{stepHuman("a")}, manifest(rg))
	assertHasCode(t, validator.Validate(r), "region-skip-reason-with-members")
}

func TestValidate_MemberUnknown(t *testing.T) {
	r := rb(
		[]schema.FlowNode{stepHuman("a")},
		manifest(region("r1", []string{"ghost"}, []string{"ghost"}, []string{"ghost"})),
	)
	assertHasCode(t, validator.Validate(r), "region-member-unknown")
}

func TestValidate_EntryNotMember(t *testing.T) {
	r := rb(
		[]schema.FlowNode{stepHuman("a"), stepHuman("b")},
		manifest(region("r1", []string{"a"}, []string{"b"}, []string{"a"})),
	)
	assertHasCode(t, validator.Validate(r), "region-entry-not-member")
}

func TestValidate_ExitNotMember(t *testing.T) {
	r := rb(
		[]schema.FlowNode{stepHuman("a"), stepHuman("b")},
		manifest(region("r1", []string{"a"}, []string{"a"}, []string{"b"})),
	)
	assertHasCode(t, validator.Validate(r), "region-exit-not-member")
}

func TestValidate_MemberSharedAcrossRegions(t *testing.T) {
	r := rb(
		[]schema.FlowNode{stepHuman("a"), stepHuman("b")},
		manifest(
			region("r1", []string{"a", "b"}, []string{"a"}, []string{"b"}),
			region("r2", []string{"b"}, []string{"b"}, []string{"b"}),
		),
	)
	assertHasCode(t, validator.Validate(r), "region-member-shared")
}

func TestValidate_NotContiguous_DifferentParents(t *testing.T) {
	// Region r1 claims top-level "a" plus "inner" inside an iterate that
	// is NOT a member. That gives r1 two roots ("a" with parent ""; "inner"
	// with parent "loop") that don't share a parent → not contiguous.
	flow := []schema.FlowNode{
		stepHuman("a"),
		{Iterate: &schema.IterateNode{
			ID:    "loop",
			Steps: []schema.FlowNode{stepHuman("inner")},
		}},
	}
	r := rb(flow, manifest(region("r1",
		[]string{"a", "inner"},
		[]string{"a"},
		[]string{"inner"},
	)))
	assertHasCode(t, validator.Validate(r), "region-not-contiguous")
}

func TestValidate_SiblingsSameParent_AcceptedByContiguity(t *testing.T) {
	// Two top-level siblings; both share parent "" — this passes the
	// Phase 1 contiguity check. (Sibling-interleaving with another
	// region is intentionally not detected here; deferred to Phase 2.)
	r := rb(
		[]schema.FlowNode{stepHuman("a"), stepHuman("b")},
		manifest(region("r1", []string{"a", "b"}, []string{"a"}, []string{"b"})),
	)
	assertNoErrors(t, validator.Validate(r))
}
