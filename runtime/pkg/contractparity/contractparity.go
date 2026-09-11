// Package contractparity provides a reusable, fixture-agnostic harness for
// asserting that two bindings of the same logical tool action are
// contract-identical.
//
// "Contract-identical" means: for the same inputs the two bindings agree on
// the set of output keys, the Go reflect.Type of each output value, and the
// declared action metadata (classification, governance-relevant fields).  They
// need NOT agree on the concrete values — a mock returns fixture data, a real
// endpoint returns live data.
//
// # Minimal usage (from a foreign repo's go test)
//
//	import "github.com/ormasoftchile/yawr/runtime/pkg/contractparity"
//
//	native := contractparity.Binding{
//	    Name:   "native",
//	    Invoke: func(ctx context.Context, args map[string]any) (map[string]any, error) { ... },
//	    Meta:   contractparity.ActionMeta{Classification: ptr("read-only")},
//	}
//	remote := contractparity.Binding{
//	    Name:   "mcp-http",
//	    Invoke: func(ctx context.Context, args map[string]any) (map[string]any, error) { ... },
//	    Meta:   contractparity.ActionMeta{Classification: ptr("read-only")},
//	}
//	contractparity.AssertParity(t, context.Background(), map[string]any{"id": "1"}, native, remote)
package contractparity

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ─── Domain types ─────────────────────────────────────────────────────────────

// ViolationKind names the category of a contract divergence.
type ViolationKind string

const (
	// ViolationMissingKey means binding B is missing a key that binding A returned.
	ViolationMissingKey ViolationKind = "missing_output_key"
	// ViolationExtraKey means binding B returned a key that binding A did not.
	ViolationExtraKey ViolationKind = "extra_output_key"
	// ViolationTypeMismatch means both bindings returned the same key but with
	// different Go reflect.Types.
	ViolationTypeMismatch ViolationKind = "output_type_mismatch"
	// ViolationMetaMismatch means a declared action-metadata field differs
	// between the two bindings (e.g. classification).
	ViolationMetaMismatch ViolationKind = "action_meta_mismatch"
)

// Violation describes one contract divergence between two bindings.
type Violation struct {
	// Kind is the category of divergence.
	Kind ViolationKind
	// Field names the output key or metadata field that diverged.
	Field string
	// DescA is a human-readable description of what binding A had.
	DescA string
	// DescB is a human-readable description of what binding B had.
	DescB string
}

// String returns a one-line diagnostic suitable for display in a test failure.
func (v Violation) String() string {
	return fmt.Sprintf("[%s] field=%q: A=%s  B=%s", v.Kind, v.Field, v.DescA, v.DescB)
}

// Report is the structured result of a parity comparison.  It is usable
// outside tests — callers that do not want *testing.T can inspect Violations
// directly.
type Report struct {
	// BindingA and BindingB are the human labels supplied by the caller.
	BindingA   string
	BindingB   string
	Violations []Violation
}

// Equal reports whether the two bindings are contract-identical.
func (r *Report) Equal() bool { return len(r.Violations) == 0 }

// Summary returns a multi-line human-readable diagnostic.  Empty string when
// Equal() is true.
func (r *Report) Summary() string {
	if r.Equal() {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "contract parity violated between binding %q and binding %q (%d violation(s)):\n",
		r.BindingA, r.BindingB, len(r.Violations))
	for i, v := range r.Violations {
		fmt.Fprintf(&sb, "  %d. %s\n", i+1, v)
	}
	return sb.String()
}

// ─── ActionMeta ───────────────────────────────────────────────────────────────

// ActionMeta captures the declared contract metadata for a logical action.
// All fields are pointers so that "absent" is distinguishable from "set to zero
// value".  A consumer that does not want to assert on a field should leave the
// pointer nil — the harness skips that field.
type ActionMeta struct {
	// Classification is the action's declared classification string, e.g.
	// "read-only", "mutating", "destructive".  Nil means "not declared".
	Classification *string
	// RequiresApproval is the governance explicit opt-in/opt-out flag.
	// Nil means "not declared".
	RequiresApproval *bool
	// Idempotent is the declared idempotency assertion.
	// Nil means "not declared".
	Idempotent *bool
}

// ─── Binding ──────────────────────────────────────────────────────────────────

// Binding represents one transport binding of a logical action.
type Binding struct {
	// Name is the human label used in diagnostics (e.g. "native", "mcp-http").
	Name string
	// Invoke calls the action with the supplied arguments and returns the
	// structured output map.  Returning a non-nil error causes Check / AssertParity
	// to fail immediately with that error, without producing a parity report.
	Invoke func(ctx context.Context, args map[string]any) (map[string]any, error)
	// Meta is the declared action metadata for this binding.
	Meta ActionMeta
}

// ─── Core comparison ──────────────────────────────────────────────────────────

// CompareOutputs compares two output maps for shape/type parity and returns a
// Report.  It does NOT compare values — only key presence and reflect.Type.
//
// The function is deterministic: violations are reported in sorted key order.
func CompareOutputs(nameA string, outputA map[string]any, nameB string, outputB map[string]any) *Report {
	r := &Report{BindingA: nameA, BindingB: nameB}

	// Collect all keys from A.
	keysA := sortedKeys(outputA)
	for _, k := range keysA {
		vA := outputA[k]
		vB, ok := outputB[k]
		if !ok {
			r.Violations = append(r.Violations, Violation{
				Kind:  ViolationMissingKey,
				Field: k,
				DescA: typeName(vA),
				DescB: "<absent>",
			})
			continue
		}
		tA, tB := reflect.TypeOf(vA), reflect.TypeOf(vB)
		if tA != tB {
			r.Violations = append(r.Violations, Violation{
				Kind:  ViolationTypeMismatch,
				Field: k,
				DescA: typeName(vA),
				DescB: typeName(vB),
			})
		}
	}

	// Extra keys present in B but not in A.
	for _, k := range sortedKeys(outputB) {
		if _, ok := outputA[k]; !ok {
			r.Violations = append(r.Violations, Violation{
				Kind:  ViolationExtraKey,
				Field: k,
				DescA: "<absent>",
				DescB: typeName(outputB[k]),
			})
		}
	}

	// Sort violations for deterministic output.
	sort.Slice(r.Violations, func(i, j int) bool {
		if r.Violations[i].Field != r.Violations[j].Field {
			return r.Violations[i].Field < r.Violations[j].Field
		}
		return string(r.Violations[i].Kind) < string(r.Violations[j].Kind)
	})

	return r
}

// CompareMeta compares ActionMeta fields for parity and appends any violations
// to the supplied Report (or returns a fresh one when r is nil).
//
// A metadata field is only compared when BOTH bindings declare it (non-nil
// pointer on both sides).  A field that is nil on one side is skipped, because
// the consumer may legitimately choose not to assert on it.
func CompareMeta(r *Report, nameA string, metaA ActionMeta, nameB string, metaB ActionMeta) *Report {
	if r == nil {
		r = &Report{BindingA: nameA, BindingB: nameB}
	}
	if metaA.Classification != nil && metaB.Classification != nil {
		if *metaA.Classification != *metaB.Classification {
			r.Violations = append(r.Violations, Violation{
				Kind:  ViolationMetaMismatch,
				Field: "meta.classification",
				DescA: fmt.Sprintf("%q", *metaA.Classification),
				DescB: fmt.Sprintf("%q", *metaB.Classification),
			})
		}
	}
	if metaA.RequiresApproval != nil && metaB.RequiresApproval != nil {
		if *metaA.RequiresApproval != *metaB.RequiresApproval {
			r.Violations = append(r.Violations, Violation{
				Kind:  ViolationMetaMismatch,
				Field: "meta.requires_approval",
				DescA: fmt.Sprintf("%v", *metaA.RequiresApproval),
				DescB: fmt.Sprintf("%v", *metaB.RequiresApproval),
			})
		}
	}
	if metaA.Idempotent != nil && metaB.Idempotent != nil {
		if *metaA.Idempotent != *metaB.Idempotent {
			r.Violations = append(r.Violations, Violation{
				Kind:  ViolationMetaMismatch,
				Field: "meta.idempotent",
				DescA: fmt.Sprintf("%v", *metaA.Idempotent),
				DescB: fmt.Sprintf("%v", *metaB.Idempotent),
			})
		}
	}
	return r
}

// Check invokes both bindings with the given args and returns a full parity
// Report (outputs + metadata).  It returns (nil, err) immediately when either
// Invoke function returns a non-nil error.
//
// Check is a pure function with respect to parity logic: it does not call t.
// Use AssertParity for the testing.T adapter.
func Check(ctx context.Context, args map[string]any, a, b Binding) (*Report, error) {
	outA, err := a.Invoke(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("binding %q invoke failed: %w", a.Name, err)
	}
	outB, err := b.Invoke(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("binding %q invoke failed: %w", b.Name, err)
	}

	r := CompareOutputs(a.Name, outA, b.Name, outB)
	CompareMeta(r, a.Name, a.Meta, b.Name, b.Meta)
	return r, nil
}

// ─── Testing adapter ──────────────────────────────────────────────────────────

// AssertParity calls Check and records any violations via t.Error (non-fatal,
// so all violations are reported together).  Invoke errors are fatal.
//
// This is the primary entry point for callers inside go test.
func AssertParity(t testing.TB, ctx context.Context, args map[string]any, a, b Binding) {
	t.Helper()
	r, err := Check(ctx, args, a, b)
	if err != nil {
		t.Fatalf("contractparity.AssertParity: %v", err)
	}
	if !r.Equal() {
		t.Errorf("contractparity.AssertParity: bindings are NOT contract-identical\n%s", r.Summary())
	}
}

// RequireParity is like AssertParity but uses t.Fatal (test stops on first
// parity failure).
func RequireParity(t testing.TB, ctx context.Context, args map[string]any, a, b Binding) {
	t.Helper()
	r, err := Check(ctx, args, a, b)
	if err != nil {
		t.Fatalf("contractparity.RequireParity: %v", err)
	}
	if !r.Equal() {
		t.Fatalf("contractparity.RequireParity: bindings are NOT contract-identical\n%s", r.Summary())
	}
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

func sortedKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// typeName returns a human-readable type description for diagnostic messages.
// For nil values it returns "<nil>".
func typeName(v any) string {
	if v == nil {
		return "<nil>"
	}
	t := reflect.TypeOf(v)
	if t == nil {
		return "<nil>"
	}
	return t.String()
}
