package contractparity_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/contractparity"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// staticBinding returns a Binding whose Invoke always returns the supplied
// output map.
func staticBinding(name string, out map[string]any, meta contractparity.ActionMeta) contractparity.Binding {
	return contractparity.Binding{
		Name:   name,
		Invoke: func(_ context.Context, _ map[string]any) (map[string]any, error) { return out, nil },
		Meta:   meta,
	}
}

// errorBinding returns a Binding whose Invoke always returns the supplied error.
func errorBinding(name string, err error) contractparity.Binding {
	return contractparity.Binding{
		Name:   name,
		Invoke: func(_ context.Context, _ map[string]any) (map[string]any, error) { return nil, err },
	}
}

// ─── CompareOutputs — positive controls (parity holds) ───────────────────────

func TestCompareOutputs_IdenticalOutputs_NilMaps(t *testing.T) {
	r := contractparity.CompareOutputs("A", nil, "B", nil)
	if !r.Equal() {
		t.Errorf("expected parity for nil maps, got violations: %s", r.Summary())
	}
}

func TestCompareOutputs_IdenticalOutputs_Empty(t *testing.T) {
	r := contractparity.CompareOutputs("A", map[string]any{}, "B", map[string]any{})
	if !r.Equal() {
		t.Errorf("expected parity for empty maps, got violations: %s", r.Summary())
	}
}

func TestCompareOutputs_IdenticalOutputs_SameKeysAndTypes(t *testing.T) {
	// Values differ deliberately — parity is about shape, not values.
	a := map[string]any{"id": "abc", "count": 42, "active": true}
	b := map[string]any{"id": "xyz", "count": 99, "active": false}
	r := contractparity.CompareOutputs("A", a, "B", b)
	if !r.Equal() {
		t.Errorf("expected parity (values differ but shapes match), got: %s", r.Summary())
	}
}

func TestCompareOutputs_NilValues_BothSides(t *testing.T) {
	// Both sides have the key; both values are nil — shapes agree.
	a := map[string]any{"x": nil}
	b := map[string]any{"x": nil}
	r := contractparity.CompareOutputs("A", a, "B", b)
	if !r.Equal() {
		t.Errorf("expected parity for matching nil values: %s", r.Summary())
	}
}

func TestCompareOutputs_NestedMaps_SameShape(t *testing.T) {
	// Top-level type is map[string]any on both sides — shape matches.
	a := map[string]any{"detail": map[string]any{"code": 1}}
	b := map[string]any{"detail": map[string]any{"code": 2}}
	r := contractparity.CompareOutputs("A", a, "B", b)
	if !r.Equal() {
		t.Errorf("expected parity for nested maps with same top-level type: %s", r.Summary())
	}
}

// ─── CompareOutputs — NEGATIVE CONTROLS ──────────────────────────────────────
//
// CRITICAL: each test below deliberately injects a parity violation and asserts
// that the harness reports it.  If the harness fails to detect the violation the
// test itself fails with "NEGATIVE CONTROL FAILED".  This mirrors the pattern of
// TestCredentialLeak_SweepDetectsIntentionalLeak in internal/tool.

func TestCompareOutputs_NegativeControl_MissingKey(t *testing.T) {
	// Binding A returns key "status"; binding B deliberately omits it.
	a := map[string]any{"status": "ok", "id": "1"}
	b := map[string]any{"id": "2"} // "status" missing

	r := contractparity.CompareOutputs("A", a, "B", b)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect missing output key 'status' — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationMissingKey, "status") {
		t.Errorf("expected ViolationMissingKey for field 'status', got: %s", r.Summary())
	}
}

func TestCompareOutputs_NegativeControl_ExtraKey(t *testing.T) {
	// Binding B returns an extra key "debug_trace" that A does not.
	a := map[string]any{"id": "1"}
	b := map[string]any{"id": "2", "debug_trace": "internal-detail"} // extra key

	r := contractparity.CompareOutputs("A", a, "B", b)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect extra output key 'debug_trace' — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationExtraKey, "debug_trace") {
		t.Errorf("expected ViolationExtraKey for field 'debug_trace', got: %s", r.Summary())
	}
}

func TestCompareOutputs_NegativeControl_TypeMismatch_StringVsInt(t *testing.T) {
	// A returns "count" as string; B returns it as int.
	a := map[string]any{"count": "42"} // string
	b := map[string]any{"count": 42}   // int

	r := contractparity.CompareOutputs("A", a, "B", b)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect type mismatch for 'count' (string vs int) — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationTypeMismatch, "count") {
		t.Errorf("expected ViolationTypeMismatch for field 'count', got: %s", r.Summary())
	}
}

func TestCompareOutputs_NegativeControl_TypeMismatch_BoolVsString(t *testing.T) {
	a := map[string]any{"active": true}
	b := map[string]any{"active": "true"} // string, not bool

	r := contractparity.CompareOutputs("A", a, "B", b)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect type mismatch for 'active' (bool vs string) — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationTypeMismatch, "active") {
		t.Errorf("expected ViolationTypeMismatch for field 'active', got: %s", r.Summary())
	}
}

func TestCompareOutputs_NegativeControl_TypeMismatch_NilVsString(t *testing.T) {
	// A returns nil for key "x"; B returns a string.
	a := map[string]any{"x": nil}
	b := map[string]any{"x": "something"}

	r := contractparity.CompareOutputs("A", a, "B", b)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect type mismatch for 'x' (nil vs string) — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationTypeMismatch, "x") {
		t.Errorf("expected ViolationTypeMismatch for field 'x', got: %s", r.Summary())
	}
}

func TestCompareOutputs_NegativeControl_MultipleViolations(t *testing.T) {
	// Inject two separate violations; harness MUST report both.
	a := map[string]any{"id": "1", "status": "ok"}
	b := map[string]any{"id": 99} // type mismatch on "id", missing "status"

	r := contractparity.CompareOutputs("A", a, "B", b)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect any violation in multi-violation case — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationTypeMismatch, "id") {
		t.Errorf("expected ViolationTypeMismatch for 'id', got: %s", r.Summary())
	}
	if !hasViolation(r, contractparity.ViolationMissingKey, "status") {
		t.Errorf("expected ViolationMissingKey for 'status', got: %s", r.Summary())
	}
}

// ─── CompareMeta — positive controls ─────────────────────────────────────────

func TestCompareMeta_IdenticalClassification(t *testing.T) {
	metaA := contractparity.ActionMeta{Classification: strPtr("read-only")}
	metaB := contractparity.ActionMeta{Classification: strPtr("read-only")}
	r := contractparity.CompareMeta(nil, "A", metaA, "B", metaB)
	if !r.Equal() {
		t.Errorf("expected parity for identical classification, got: %s", r.Summary())
	}
}

func TestCompareMeta_OneNilClassification_Skipped(t *testing.T) {
	// Only one side declares classification — field should be skipped.
	metaA := contractparity.ActionMeta{Classification: strPtr("read-only")}
	metaB := contractparity.ActionMeta{} // nil
	r := contractparity.CompareMeta(nil, "A", metaA, "B", metaB)
	if !r.Equal() {
		t.Errorf("expected no violation when only one side declares classification, got: %s", r.Summary())
	}
}

func TestCompareMeta_AppendsToExistingReport(t *testing.T) {
	// Verify that when r is non-nil, CompareMeta appends rather than replacing.
	existing := contractparity.CompareOutputs("A", map[string]any{"a": 1}, "B", map[string]any{})
	if existing.Equal() {
		t.Fatal("setup: expected existing violation")
	}
	metaA := contractparity.ActionMeta{Classification: strPtr("read-only")}
	metaB := contractparity.ActionMeta{Classification: strPtr("mutating")}
	updated := contractparity.CompareMeta(existing, "A", metaA, "B", metaB)
	if len(updated.Violations) < 2 {
		t.Errorf("expected at least 2 violations after append, got %d", len(updated.Violations))
	}
}

// ─── CompareMeta — NEGATIVE CONTROLS ─────────────────────────────────────────

func TestCompareMeta_NegativeControl_DivergentClassification(t *testing.T) {
	metaA := contractparity.ActionMeta{Classification: strPtr("read-only")}
	metaB := contractparity.ActionMeta{Classification: strPtr("mutating")} // deliberate divergence

	r := contractparity.CompareMeta(nil, "A", metaA, "B", metaB)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect divergent classification ('read-only' vs 'mutating') — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationMetaMismatch, "meta.classification") {
		t.Errorf("expected ViolationMetaMismatch for meta.classification, got: %s", r.Summary())
	}
}

func TestCompareMeta_NegativeControl_DivergentRequiresApproval(t *testing.T) {
	metaA := contractparity.ActionMeta{RequiresApproval: boolPtr(false)}
	metaB := contractparity.ActionMeta{RequiresApproval: boolPtr(true)} // deliberate divergence

	r := contractparity.CompareMeta(nil, "A", metaA, "B", metaB)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect divergent requires_approval — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationMetaMismatch, "meta.requires_approval") {
		t.Errorf("expected ViolationMetaMismatch for meta.requires_approval, got: %s", r.Summary())
	}
}

func TestCompareMeta_NegativeControl_DivergentIdempotent(t *testing.T) {
	metaA := contractparity.ActionMeta{Idempotent: boolPtr(true)}
	metaB := contractparity.ActionMeta{Idempotent: boolPtr(false)} // deliberate divergence

	r := contractparity.CompareMeta(nil, "A", metaA, "B", metaB)
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect divergent idempotent flag — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationMetaMismatch, "meta.idempotent") {
		t.Errorf("expected ViolationMetaMismatch for meta.idempotent, got: %s", r.Summary())
	}
}

// ─── Check — positive controls ────────────────────────────────────────────────

func TestCheck_ParityHolds(t *testing.T) {
	meta := contractparity.ActionMeta{Classification: strPtr("read-only")}
	a := staticBinding("native", map[string]any{"id": "fixture", "score": 0.9}, meta)
	b := staticBinding("mcp-http", map[string]any{"id": "live", "score": 1.1}, meta)

	r, err := contractparity.Check(context.Background(), map[string]any{"input": "x"}, a, b)
	if err != nil {
		t.Fatalf("unexpected error from Check: %v", err)
	}
	if !r.Equal() {
		t.Errorf("expected parity, got: %s", r.Summary())
	}
}

func TestCheck_EmptyOutputs(t *testing.T) {
	a := staticBinding("native", nil, contractparity.ActionMeta{})
	b := staticBinding("mcp-http", nil, contractparity.ActionMeta{})

	r, err := contractparity.Check(context.Background(), nil, a, b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Equal() {
		t.Errorf("expected parity for empty outputs: %s", r.Summary())
	}
}

// ─── Check — NEGATIVE CONTROLS ────────────────────────────────────────────────

func TestCheck_NegativeControl_MissingKey(t *testing.T) {
	a := staticBinding("native", map[string]any{"result": "yes", "code": 200}, contractparity.ActionMeta{})
	b := staticBinding("mcp-http", map[string]any{"code": 200}, contractparity.ActionMeta{}) // missing "result"

	r, err := contractparity.Check(context.Background(), nil, a, b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: Check did NOT detect missing key 'result' — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationMissingKey, "result") {
		t.Errorf("expected ViolationMissingKey for 'result', got: %s", r.Summary())
	}
}

func TestCheck_NegativeControl_ClassificationDiverges(t *testing.T) {
	metaA := contractparity.ActionMeta{Classification: strPtr("read-only")}
	metaB := contractparity.ActionMeta{Classification: strPtr("destructive")} // deliberate divergence

	a := staticBinding("native", map[string]any{"ok": true}, metaA)
	b := staticBinding("mcp-http", map[string]any{"ok": true}, metaB)

	r, err := contractparity.Check(context.Background(), nil, a, b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: Check did NOT detect classification mismatch ('read-only' vs 'destructive') — harness is broken")
	}
	if !hasViolation(r, contractparity.ViolationMetaMismatch, "meta.classification") {
		t.Errorf("expected ViolationMetaMismatch for meta.classification, got: %s", r.Summary())
	}
}

func TestCheck_NegativeControl_InvokeError_BindingA(t *testing.T) {
	// When binding A errors, Check must propagate the error (not return a report).
	a := errorBinding("native", errors.New("connection refused"))
	b := staticBinding("mcp-http", map[string]any{"x": 1}, contractparity.ActionMeta{})

	r, err := contractparity.Check(context.Background(), nil, a, b)
	if err == nil {
		t.Fatal("NEGATIVE CONTROL FAILED: Check did NOT propagate invoke error from binding A — harness is broken")
	}
	if r != nil {
		t.Error("expected nil report when Check returns error, got non-nil")
	}
}

func TestCheck_NegativeControl_InvokeError_BindingB(t *testing.T) {
	a := staticBinding("native", map[string]any{"x": 1}, contractparity.ActionMeta{})
	b := errorBinding("mcp-http", errors.New("timeout"))

	r, err := contractparity.Check(context.Background(), nil, a, b)
	if err == nil {
		t.Fatal("NEGATIVE CONTROL FAILED: Check did NOT propagate invoke error from binding B — harness is broken")
	}
	if r != nil {
		t.Error("expected nil report when Check returns error, got non-nil")
	}
}

// ─── AssertParity / RequireParity — testing adapter ──────────────────────────

func TestAssertParity_ParityHolds_NoTestFailure(t *testing.T) {
	meta := contractparity.ActionMeta{Classification: strPtr("read-only")}
	a := staticBinding("native", map[string]any{"id": "a"}, meta)
	b := staticBinding("mcp-http", map[string]any{"id": "b"}, meta)
	contractparity.AssertParity(t, context.Background(), nil, a, b)
}

// TestAssertParity_NegativeControl_MarksFailed uses a spy testing.T to confirm
// that AssertParity actually calls t.Error when parity is violated.
// An adapter that swallows failures would make every consumer test vacuously pass.
func TestAssertParity_NegativeControl_MarksFailed(t *testing.T) {
	spy := &spyTB{TB: t}

	a := staticBinding("native", map[string]any{"id": "a", "extra": "yes"}, contractparity.ActionMeta{})
	b := staticBinding("mcp-http", map[string]any{"id": "a"}, contractparity.ActionMeta{}) // missing "extra"

	contractparity.AssertParity(spy, context.Background(), nil, a, b)

	if !spy.failed {
		t.Fatal("NEGATIVE CONTROL FAILED: AssertParity did NOT call t.Error when parity was violated — testing adapter is broken")
	}
	combined := strings.Join(spy.messages, "\n")
	if !strings.Contains(combined, "missing_output_key") {
		t.Errorf("expected diagnostic to name violation kind 'missing_output_key', got: %q", combined)
	}
	if !strings.Contains(combined, "extra") {
		t.Errorf("expected diagnostic to name the field 'extra', got: %q", combined)
	}
}

// TestRequireParity_NegativeControl_MarksFailed mirrors the above for RequireParity.
func TestRequireParity_NegativeControl_MarksFailed(t *testing.T) {
	spy := &spyTB{TB: t}

	a := staticBinding("native", map[string]any{"count": 1}, contractparity.ActionMeta{})
	b := staticBinding("mcp-http", map[string]any{"count": "one"}, contractparity.ActionMeta{}) // type mismatch

	contractparity.RequireParity(spy, context.Background(), nil, a, b)

	if !spy.failed {
		t.Fatal("NEGATIVE CONTROL FAILED: RequireParity did NOT call t.Fatal when parity was violated — testing adapter is broken")
	}
}

// ─── Report.Summary ───────────────────────────────────────────────────────────

func TestReport_Summary_EmptyWhenEqual(t *testing.T) {
	r := &contractparity.Report{BindingA: "A", BindingB: "B"}
	if r.Summary() != "" {
		t.Errorf("expected empty summary for equal report, got: %q", r.Summary())
	}
}

func TestReport_Summary_ContainsFieldName(t *testing.T) {
	r := contractparity.CompareOutputs("A", map[string]any{"foo": 1}, "B", map[string]any{})
	if r.Equal() {
		t.Fatal("expected violation, got none")
	}
	if !strings.Contains(r.Summary(), "foo") {
		t.Errorf("expected summary to name the missing field 'foo', got: %s", r.Summary())
	}
}

func TestReport_Summary_NamesBindings(t *testing.T) {
	r := contractparity.CompareOutputs("binding-alpha", map[string]any{"k": 1}, "binding-beta", map[string]any{})
	if !strings.Contains(r.Summary(), "binding-alpha") || !strings.Contains(r.Summary(), "binding-beta") {
		t.Errorf("expected summary to name both bindings, got: %s", r.Summary())
	}
}

// ─── Violation.String ─────────────────────────────────────────────────────────

func TestViolation_String_ContainsKindAndField(t *testing.T) {
	v := contractparity.Violation{
		Kind:  contractparity.ViolationMissingKey,
		Field: "some_field",
		DescA: "string",
		DescB: "<absent>",
	}
	s := v.String()
	if !strings.Contains(s, "missing_output_key") {
		t.Errorf("expected violation string to contain kind, got: %q", s)
	}
	if !strings.Contains(s, "some_field") {
		t.Errorf("expected violation string to contain field name, got: %q", s)
	}
}

// ─── Spy testing.TB ───────────────────────────────────────────────────────────

// spyTB wraps a testing.TB to intercept Error/Errorf/Fatal/Fatalf calls without
// propagating them to the real test, so the caller can assert that failures were
// recorded.
type spyTB struct {
	testing.TB
	failed   bool
	messages []string
}

func (s *spyTB) Helper() { s.TB.Helper() }

func (s *spyTB) Error(args ...any) {
	s.failed = true
	s.messages = append(s.messages, fmt.Sprint(args...))
}

func (s *spyTB) Errorf(format string, args ...any) {
	s.failed = true
	s.messages = append(s.messages, fmt.Sprintf(format, args...))
}

// Fatal does NOT call runtime.Goexit so the test can continue and inspect
// s.failed.
func (s *spyTB) Fatal(args ...any) {
	s.failed = true
	s.messages = append(s.messages, fmt.Sprint(args...))
}

func (s *spyTB) Fatalf(format string, args ...any) {
	s.failed = true
	s.messages = append(s.messages, fmt.Sprintf(format, args...))
}

// ─── Assertion helper ─────────────────────────────────────────────────────────

// hasViolation returns true when r contains at least one Violation with the
// given kind and field.
func hasViolation(r *contractparity.Report, kind contractparity.ViolationKind, field string) bool {
	for _, v := range r.Violations {
		if v.Kind == kind && v.Field == field {
			return true
		}
	}
	return false
}
