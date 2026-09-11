package pkgsubst

import (
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// TestPlan_InputEnumMismatch_PKG013 covers AR-ENUM-8: a substitute input's
// enum: set must exactly match the action arg's enum: set (order-
// insensitive, NFC-normalized) -- a narrower or wider substitute set is
// PKG-013, the same code as any other signature mismatch (not a new
// ENUM-0xx code).
func TestPlan_InputEnumMismatch_PKG013(t *testing.T) {
	action := baseAction()
	action.Args["node"].Enum = schema.EnumConstraint{"node-a", "node-b"}
	sub := baseSubstitute()
	sub.Inputs["node"].Enum = schema.EnumConstraint{"node-a"} // narrower: missing node-b

	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	subPath := filepath.Join(pkgRoot, "runbooks", "drain-node.yaml")
	opts := PlanOptions{
		ToolFilePath:  filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml"),
		PackageRoot:   pkgRoot,
		WorkspaceRoot: ws,
		Parser:        &fakeParser{byPath: map[string]*schema.Runbook{subPath: sub}},
	}
	_, errs := Plan(action, "drain-node", "acme.incident-tools", "kubectl", opts)
	if !hasCode(errs, "PKG-013") {
		t.Fatalf("expected PKG-013 for enum set mismatch, got %v", errs)
	}
}

// TestPlan_InputEnumMatch_Succeeds is the corresponding happy path: an
// exact (order-insensitive) enum set match on both sides is not an error.
func TestPlan_InputEnumMatch_Succeeds(t *testing.T) {
	action := baseAction()
	action.Args["node"].Enum = schema.EnumConstraint{"node-a", "node-b"}
	sub := baseSubstitute()
	sub.Inputs["node"].Enum = schema.EnumConstraint{"node-b", "node-a"} // reordered, still equal as a set

	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	subPath := filepath.Join(pkgRoot, "runbooks", "drain-node.yaml")
	opts := PlanOptions{
		ToolFilePath:  filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml"),
		PackageRoot:   pkgRoot,
		WorkspaceRoot: ws,
		Parser:        &fakeParser{byPath: map[string]*schema.Runbook{subPath: sub}},
	}
	_, errs := Plan(action, "drain-node", "acme.incident-tools", "kubectl", opts)
	if hasCode(errs, "PKG-013") {
		t.Fatalf("expected no PKG-013 for equal enum sets, got %v", errs)
	}
}

// TestPlan_OutputEnumMismatch_PKG013 mirrors the input case for the
// outputs.<name> contract (S2 vs S4).
func TestPlan_OutputEnumMismatch_PKG013(t *testing.T) {
	action := baseAction()
	action.Outputs["drained"].Type = "string"
	action.Outputs["drained"].Enum = schema.EnumConstraint{"true", "false"}
	sub := baseSubstitute()
	sub.Outputs["drained"].Type = "string"
	sub.Outputs["drained"].Enum = schema.EnumConstraint{"true", "false", "unknown"} // superset

	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	subPath := filepath.Join(pkgRoot, "runbooks", "drain-node.yaml")
	opts := PlanOptions{
		ToolFilePath:  filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml"),
		PackageRoot:   pkgRoot,
		WorkspaceRoot: ws,
		Parser:        &fakeParser{byPath: map[string]*schema.Runbook{subPath: sub}},
	}
	_, errs := Plan(action, "drain-node", "acme.incident-tools", "kubectl", opts)
	if !hasCode(errs, "PKG-013") {
		t.Fatalf("expected PKG-013 for output enum set mismatch, got %v", errs)
	}
}
