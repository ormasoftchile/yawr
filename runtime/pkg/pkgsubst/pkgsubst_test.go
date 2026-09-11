package pkgsubst

import (
	"context"
	"path/filepath"
	"testing"

	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// fakeParser is a minimal parserpkg.Parser test double that hands back a
// pre-built *schema.Runbook without exercising the full JSON-schema
// pipeline, so these tests isolate pkgsubst's own contract-validation
// logic (input/output signature, cycle/depth, governance composition) from
// unrelated JSON-schema strictness (e.g. additionalProperties: false on
// Input, which would otherwise reject some of the corpus's illustrative
// extra fields like TV-PKG-SUBST-007's `env: NODE_NAME`).
type fakeParser struct {
	byPath map[string]*schema.Runbook
}

func (f *fakeParser) Parse(_ context.Context, path string) (*parserpkg.ParsedRunbook, error) {
	rb, ok := f.byPath[path]
	if !ok {
		return nil, errNotFound(path)
	}
	return &parserpkg.ParsedRunbook{Source: path, Runbook: rb}, nil
}

func (f *fakeParser) ParseBytes(_ context.Context, _ []byte) (*parserpkg.ParsedRunbook, error) {
	return nil, errNotFound("<bytes>")
}

type notFoundErr string

func (e notFoundErr) Error() string { return "not found: " + string(e) }
func errNotFound(path string) error { return notFoundErr(path) }

// baseAction/baseTool/baseSubstitute mirror TV-PKG-SUBST-001's kubectl
// drain-node fixture (design/yawr/conformance/tv-pkg-resolve.yaml).
func baseAction() *schema.ToolAction {
	return &schema.ToolAction{
		Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "../runbooks/drain-node.yaml"},
		Args: map[string]*schema.ArgDef{
			"node": {Type: "string", Required: true},
		},
		Outputs: map[string]*schema.ArgDef{
			"drained": {Type: "boolean"},
		},
	}
}

func baseSubstitute() *schema.Runbook {
	return &schema.Runbook{
		APIVersion: "yawr.runbook/v1",
		ID:         "acme.incident-tools/drain-node",
		Name:       "drain-node",
		Inputs: map[string]*schema.Input{
			"node": {Type: "string", Required: true, From: "context"},
		},
		Outputs: map[string]*schema.Output{
			"drained": {Type: "boolean", Value: "step.drain.json.drained"},
		},
	}
}

func planWith(t *testing.T, sub *schema.Runbook, opts PlanOptions) (*Result, []error) {
	t.Helper()
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	toolPath := filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml")
	subPath := filepath.Join(pkgRoot, "runbooks", "drain-node.yaml")
	if opts.ToolFilePath == "" {
		opts.ToolFilePath = toolPath
	}
	if opts.PackageRoot == "" {
		opts.PackageRoot = pkgRoot
	}
	if opts.WorkspaceRoot == "" {
		opts.WorkspaceRoot = ws
	}
	if opts.Parser == nil {
		opts.Parser = &fakeParser{byPath: map[string]*schema.Runbook{subPath: sub}}
	}
	return Plan(baseAction(), "drain-node", "acme.incident-tools", "kubectl", opts)
}

func hasCode(errs []error, code string) bool {
	for _, e := range errs {
		if c, ok := e.(interface{ Code() string }); ok && c.Code() == code {
			return true
		}
	}
	return false
}

// TV-PKG-SUBST-001/004: exact by-name-and-type input/output match succeeds.
func TestPlan_ContractMatch_Succeeds(t *testing.T) {
	res, errs := planWith(t, baseSubstitute(), PlanOptions{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if res == nil {
		t.Fatal("expected a non-nil Result for a substituted action")
	}
	if res.Frame.String() != "acme.incident-tools/kubectl#drain-node" {
		t.Fatalf("unexpected frame: %s", res.Frame)
	}
}

// Non-substituted actions are a no-op (nil, nil), never planned.
func TestPlan_NonSubstitutedAction_IsNoOp(t *testing.T) {
	action := &schema.ToolAction{Argv: []string{"drain"}}
	res, errs := Plan(action, "drain", "acme.incident-tools", "kubectl", PlanOptions{})
	if res != nil || errs != nil {
		t.Fatalf("expected (nil, nil) for a process-backed action, got (%v, %v)", res, errs)
	}
}

// TV-PKG-SUBST-005: substitute declares an output not in the action's
// outputs: contract (superset) is PKG-013.
func TestPlan_OutputSuperset_PKG013(t *testing.T) {
	sub := baseSubstitute()
	sub.Outputs["extra_field"] = &schema.Output{Type: "string", Value: "step.drain.json.drained"}
	_, errs := planWith(t, sub, PlanOptions{})
	if !hasCode(errs, "PKG-013") {
		t.Fatalf("expected PKG-013, got %v", errs)
	}
}

// TV-PKG-SUBST-006: substitute output type mismatch (string vs boolean) is PKG-013.
func TestPlan_OutputTypeMismatch_PKG013(t *testing.T) {
	sub := baseSubstitute()
	sub.Outputs["drained"] = &schema.Output{Type: "string", Value: "step.drain.json.drained"}
	_, errs := planWith(t, sub, PlanOptions{})
	if !hasCode(errs, "PKG-013") {
		t.Fatalf("expected PKG-013, got %v", errs)
	}
}

// A declared output with an empty value: expression can never be produced
// on any terminal path (PKG-027).
func TestPlan_UnproducibleOutput_PKG027(t *testing.T) {
	sub := baseSubstitute()
	sub.Outputs["drained"] = &schema.Output{Type: "boolean", Value: ""}
	_, errs := planWith(t, sub, PlanOptions{})
	if !hasCode(errs, "PKG-027") {
		t.Fatalf("expected PKG-027, got %v", errs)
	}
}

func TestValidateOutputs_OptionalUnproducibleOutputAllowedWhenActionOptional(t *testing.T) {
	action := baseAction()
	action.Outputs["note"] = &schema.ArgDef{Type: "string", Required: false}
	sub := baseSubstitute()
	sub.Outputs["note"] = &schema.Output{Type: "string", Optional: true}
	errs := validateOutputs(action, sub)
	if len(errs) != 0 {
		t.Fatalf("optional unproduced output should be allowed, got %v", errs)
	}
}

func TestValidateOutputs_OptionalCannotSatisfyRequiredActionOutput(t *testing.T) {
	action := baseAction()
	action.Outputs["required_note"] = &schema.ArgDef{Type: "string", Required: true}
	sub := baseSubstitute()
	sub.Outputs["required_note"] = &schema.Output{Type: "string", Optional: true}
	errs := validateOutputs(action, sub)
	if !hasCode(errs, "PKG-013") {
		t.Fatalf("expected PKG-013, got %v", errs)
	}
}

// TV-PKG-SUBST-007: substitute input declares from: env instead of the
// required from: context is PKG-026.
func TestPlan_FromEnv_PKG026(t *testing.T) {
	sub := baseSubstitute()
	sub.Inputs["node"] = &schema.Input{Type: "string", Required: true, From: "env"}
	_, errs := planWith(t, sub, PlanOptions{})
	if !hasCode(errs, "PKG-026") {
		t.Fatalf("expected PKG-026, got %v", errs)
	}
}

// from: prompt is equally forbidden (PKG-026).
func TestPlan_FromPrompt_PKG026(t *testing.T) {
	sub := baseSubstitute()
	sub.Inputs["node"] = &schema.Input{Type: "string", Required: true, From: "prompt"}
	_, errs := planWith(t, sub, PlanOptions{})
	if !hasCode(errs, "PKG-026") {
		t.Fatalf("expected PKG-026, got %v", errs)
	}
}

// Input signature mismatches: extra substitute input not in args:, and an
// action arg with no corresponding substitute input, are both PKG-013.
func TestPlan_InputSignatureMismatch_PKG013(t *testing.T) {
	sub := baseSubstitute()
	sub.Inputs["unexpected"] = &schema.Input{Type: "string", Required: true, From: "context"}
	_, errs := planWith(t, sub, PlanOptions{})
	if !hasCode(errs, "PKG-013") {
		t.Fatalf("expected PKG-013 for extra substitute input, got %v", errs)
	}

	sub2 := baseSubstitute()
	delete(sub2.Inputs, "node")
	_, errs2 := planWith(t, sub2, PlanOptions{})
	if !hasCode(errs2, "PKG-013") {
		t.Fatalf("expected PKG-013 for missing substitute input, got %v", errs2)
	}
}

// An action-required arg whose substitute input is not itself required is
// a contract mismatch (PKG-013): the substitute cannot advertise looser
// requiredness than the action it implements.
func TestPlan_RequiredNotPropagated_PKG013(t *testing.T) {
	sub := baseSubstitute()
	sub.Inputs["node"] = &schema.Input{Type: "string", Required: false, From: "context"}
	_, errs := planWith(t, sub, PlanOptions{})
	if !hasCode(errs, "PKG-013") {
		t.Fatalf("expected PKG-013, got %v", errs)
	}
}

// TV-PKG-SUBST-008: a substitution cycle (frame already active in the
// stack) is PKG-015, detected before any parse/execution.
func TestPlan_Cycle_PKG015(t *testing.T) {
	opts := PlanOptions{
		Frames: []Frame{{Package: "acme.incident-tools", Tool: "kubectl", Action: "drain-node"}},
	}
	_, errs := planWith(t, baseSubstitute(), opts)
	if !hasCode(errs, "PKG-015") {
		t.Fatalf("expected PKG-015, got %v", errs)
	}
}

// TV-PKG-SUBST-009: nesting beyond MaxDepth is PKG-028.
func TestPlan_MaxDepthExceeded_PKG028(t *testing.T) {
	opts := PlanOptions{
		Frames: []Frame{
			{Package: "p", Tool: "t1", Action: "a1"},
			{Package: "p", Tool: "t2", Action: "a2"},
			{Package: "p", Tool: "t3", Action: "a3"},
			{Package: "p", Tool: "t4", Action: "a4"},
		},
	}
	_, errs := planWith(t, baseSubstitute(), opts)
	if !hasCode(errs, "PKG-028") {
		t.Fatalf("expected PKG-028, got %v", errs)
	}
}

// Exactly MaxDepth is still legal (the boundary is > MaxDepth, not >=).
func TestPlan_MaxDepthBoundary_Legal(t *testing.T) {
	opts := PlanOptions{
		Frames: []Frame{
			{Package: "p", Tool: "t1", Action: "a1"},
			{Package: "p", Tool: "t2", Action: "a2"},
			{Package: "p", Tool: "t3", Action: "a3"},
		},
	}
	_, errs := planWith(t, baseSubstitute(), opts)
	if len(errs) != 0 {
		t.Fatalf("expected depth-4 frame to be legal, got %v", errs)
	}
}

// TV-PKG-SUBST-010: a strict-subset substitute allow_commands narrows the
// caller's set legally, no PKG-014.
func TestPlan_GovernanceNarrowing_Legal(t *testing.T) {
	sub := baseSubstitute()
	sub.Governance = &schema.GovernanceConfig{AllowCommands: []string{"bash"}}
	caller := &EffectiveGovernance{AllowCommands: []string{"kubectl", "bash"}}
	res, errs := planWith(t, sub, PlanOptions{CallerGovernance: caller})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(res.EffectiveGovernance.AllowCommands) != 1 || res.EffectiveGovernance.AllowCommands[0] != "bash" {
		t.Fatalf("expected effective allow_commands [bash], got %v", res.EffectiveGovernance.AllowCommands)
	}
}

// TV-PKG-SUBST-011: substitute governance widens allow_commands (adds
// "curl", not in the caller's set) is PKG-014.
func TestPlan_GovernanceWidening_PKG014(t *testing.T) {
	sub := baseSubstitute()
	sub.Governance = &schema.GovernanceConfig{AllowCommands: []string{"bash", "curl"}}
	caller := &EffectiveGovernance{AllowCommands: []string{"kubectl", "bash"}}
	_, errs := planWith(t, sub, PlanOptions{CallerGovernance: caller})
	if !hasCode(errs, "PKG-014") {
		t.Fatalf("expected PKG-014, got %v", errs)
	}
}

// require_approval composes via logical OR: a substitute cannot use its
// own laxer default to bypass a caller-required approval gate.
func TestPlan_RequireApproval_ORComposition(t *testing.T) {
	sub := baseSubstitute()
	sub.Governance = &schema.GovernanceConfig{RequireApproval: false}
	caller := &EffectiveGovernance{RequireApproval: true}
	res, errs := planWith(t, sub, PlanOptions{CallerGovernance: caller})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !res.EffectiveGovernance.RequireApproval {
		t.Fatal("expected effective require_approval = true (OR composition)")
	}
}

// deny_commands/deny_env_vars/redact compose via union.
func TestPlan_DenyUnionComposition(t *testing.T) {
	sub := baseSubstitute()
	sub.Governance = &schema.GovernanceConfig{
		DenyCommands: []string{"rm"},
		DenyEnvVars:  []string{"SECRET"},
	}
	caller := &EffectiveGovernance{
		DenyCommands: []string{"curl"},
		DenyEnvVars:  []string{"TOKEN"},
	}
	res, errs := planWith(t, sub, PlanOptions{CallerGovernance: caller})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(res.EffectiveGovernance.DenyCommands) != 2 || len(res.EffectiveGovernance.DenyEnvVars) != 2 {
		t.Fatalf("expected union of 2+2 deny entries, got %v / %v",
			res.EffectiveGovernance.DenyCommands, res.EffectiveGovernance.DenyEnvVars)
	}
}
