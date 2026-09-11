package parser_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

// fixturesDir is the path to the test runbook fixtures.
const fixturesDir = "testdata/runbooks"

// normalizingPlatform is a test-only Platform that replaces backslashes with
// forward slashes in paths, allowing path normalization to be tested.
type normalizingPlatform struct {
	*platform.FakePlatform
}

func (n *normalizingPlatform) NormalizePath(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

func (n *normalizingPlatform) TempDir() string          { return n.FakePlatform.TempDir() }
func (n *normalizingPlatform) AllowedSignals() []string { return n.FakePlatform.AllowedSignals() }
func (n *normalizingPlatform) OpenAppend(path string) (io.WriteCloser, error) {
	return n.FakePlatform.OpenAppend(path)
}
func (n *normalizingPlatform) NewlineNormalizer(w io.Writer) io.Writer {
	return n.FakePlatform.NewlineNormalizer(w)
}
func (n *normalizingPlatform) ExecSuffix() string   { return n.FakePlatform.ExecSuffix() }
func (n *normalizingPlatform) DefaultShell() string { return n.FakePlatform.DefaultShell() }
func (n *normalizingPlatform) Exec(ctx context.Context, req platform.ExecRequest) (*platform.ExecResult, error) {
	return n.FakePlatform.Exec(ctx, req)
}

// ─── Happy path: r01–r10 fixtures ────────────────────────────────────────────

func TestParser_Fixtures(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§2", "all r01-r10 fixtures MUST parse successfully")

	fixtures, err := filepath.Glob(filepath.Join(fixturesDir, "r*/schema.yaml"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures found — check fixturesDir path")
	}

	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}

	for _, path := range fixtures {
		path := path
		name := filepath.Base(filepath.Dir(path))
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pr, err := p.Parse(context.Background(), path)
			if err != nil {
				t.Fatalf("Parse(%s): unexpected error: %v", name, err)
			}
			if pr.Runbook == nil {
				t.Fatal("ParsedRunbook.Runbook is nil")
			}
			if pr.Runbook.APIVersion != "yawr.runbook/v1" {
				t.Errorf("APIVersion = %q; want yawr.runbook/v1", pr.Runbook.APIVersion)
			}
		})
	}
}

func TestParser_RejectsRemovedToolRefFields(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"source: obsolete", "actions: [inspect]"} {
		t.Run(strings.Split(field, ":")[0], func(t *testing.T) {
			src := "apiVersion: yawr.runbook/v1\nid: removed\nname: Removed\n" +
				"toolRefs:\n  - name: db\n    package: example.tools\n    " + field + "\nflow: []\n"
			_, err := p.ParseBytes(context.Background(), []byte(src))
			if err == nil {
				t.Fatalf("expected toolRefs field %q to fail structural validation", field)
			}
			assertErrorCode(t, err, "schema/structural")
		})
	}
}

// ─── §2.1 — apiVersion ────────────────────────────────────────────────────────

func TestParser_MissingAPIVersion(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§2.1", "runbook MUST have apiVersion: yawr.runbook/v1")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbook()
	src = strings.ReplaceAll(src, "apiVersion: yawr.runbook/v1\n", "")
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for missing apiVersion, got nil")
	}
	assertErrorCode(t, err, "schema/structural")
}

func TestParser_WrongAPIVersion(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§2.1", "parser MUST NOT silently accept wrong apiVersion")
	p, _ := parser.New(platform.NewFakePlatform())
	src := strings.ReplaceAll(minimalRunbook(), "yawr.runbook/v1", "runbook/v9")
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for wrong apiVersion, got nil")
	}
	assertErrorCode(t, err, "schema/structural")
}

// ─── §2.2 — Required top-level fields ─────────────────────────────────────────

func TestParser_MissingID(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§2.2", "runbook MUST have id (non-empty)")
	p, _ := parser.New(platform.NewFakePlatform())
	src := strings.ReplaceAll(minimalRunbook(), "id: hello-world\n", "")
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for missing id, got nil")
	}
	assertErrorCode(t, err, "schema/structural")
}

func TestParser_MissingName(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§2.2", "runbook MUST have name (non-empty)")
	p, _ := parser.New(platform.NewFakePlatform())
	src := strings.ReplaceAll(minimalRunbook(), "name: Hello World\n", "")
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	assertErrorCode(t, err, "schema/structural")
}

func TestParser_MissingFlow(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§2.2", "runbook MUST have flow")
	p, _ := parser.New(platform.NewFakePlatform())
	src := `apiVersion: yawr.runbook/v1
id: hello-world
name: Hello World
`
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for missing flow, got nil")
	}
	assertErrorCode(t, err, "schema/structural")
}

// ─── §4 — Step id and type ────────────────────────────────────────────────────

func TestParser_StepMustHaveID(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "each step MUST have a non-empty id")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbookWithStep(`
  - step:
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for step with no id, got nil")
	}
	assertErrorCode(t, err, "schema/structural")
}

func TestParser_StepMustHaveType(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4.2", "each step MUST have a type field")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbookWithStep(`
  - step:
      id: orphan
      title: No type here
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for step missing type, got nil")
	}
	assertErrorCode(t, err, "schema/structural")
}

func TestParser_StepInvalidType(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4.2", "step type MUST be one of the 14 valid types")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbookWithStep(`
  - step:
      id: s1
      type: bogus_type
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for invalid step type, got nil")
	}
	assertErrorCode(t, err, "schema/structural")
}

// ─── §4 — Step ID uniqueness ──────────────────────────────────────────────────

func TestParser_DuplicateStepID(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "step IDs MUST be unique within a runbook")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbookWithSteps(`
  - step:
      id: dup
      type: cli
      title: First
      command: echo
      args: ["a"]
  - step:
      id: dup
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for duplicate step IDs, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention 'duplicate', got: %v", err)
	}
}

// ─── §4 — ParallelNode nested in parallel branch forbidden ────────────────────

func TestParser_NestedParallelNodeForbidden(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "flow-level ParallelNode MUST NOT appear inside a parallel branch")
	p, _ := parser.New(platform.NewFakePlatform())
	// Use a flow-level `parallel:` node inside a branch of another `parallel:` node.
	src := `apiVersion: yawr.runbook/v1
id: test-runbook
name: Test Runbook
flow:
  - parallel:
      id: outer_par
      branches:
        - label: Branch A
          steps:
            - parallel:
                id: inner_par
                branches:
                  - label: Nested
                    steps:
                      - step:
                          id: leaf
                          type: cli
                          command: echo
                          args: ["x"]
`
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for nested flow-level ParallelNode, got nil")
	}
	assertErrorCode(t, err, "parallel/nested-forbidden")
}

// ─── §4 — IterateNode/ParallelNode IDs participate in uniqueness check ────────

func TestParser_IterateNodeDuplicateID(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "IterateNode.ID MUST be included in step-ID uniqueness check")
	p, _ := parser.New(platform.NewFakePlatform())
	src := `apiVersion: yawr.runbook/v1
id: test-runbook
name: Test Runbook
flow:
  - step:
      id: dup
      type: cli
      title: A step
      command: echo
      args: ["hi"]
  - iterate:
      id: dup
      over: "[1,2,3]"
      as: item
      steps:
        - step:
            id: iter_leaf
            type: cli
            command: echo
            args: ["x"]
`
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for IterateNode ID duplicating a step ID, got nil")
	}
	assertErrorCode(t, err, "step/duplicate-id")
}

func TestParser_ParallelNodeDuplicateID(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "ParallelNode.ID MUST be included in step-ID uniqueness check")
	p, _ := parser.New(platform.NewFakePlatform())
	src := `apiVersion: yawr.runbook/v1
id: test-runbook
name: Test Runbook
flow:
  - step:
      id: dup
      type: cli
      title: A step
      command: echo
      args: ["hi"]
  - parallel:
      id: dup
      branches:
        - label: Branch A
          steps:
            - step:
                id: par_leaf
                type: cli
                command: echo
                args: ["x"]
`
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for ParallelNode ID duplicating a step ID, got nil")
	}
	assertErrorCode(t, err, "step/duplicate-id")
}

// ─── §4 — Nested parallel forbidden ──────────────────────────────────────────

func TestParser_NestedParallelForbidden(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "parallel step MUST NOT appear inside a parallel branch")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbookWithSteps(`
  - step:
      id: outer_parallel
      type: parallel
      title: Outer parallel
      branches:
        - label: Branch A
          steps:
            - step:
                id: inner_parallel
                type: parallel
                title: Inner (forbidden)
                branches:
                  - label: Nested
                    steps:
                      - step:
                          id: nested_leaf
                          type: cli
                          command: echo
                          args: ["x"]
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for nested parallel, got nil")
	}
	if !strings.Contains(err.Error(), "parallel/nested-forbidden") {
		t.Errorf("error code should be 'parallel/nested-forbidden', got: %v", err)
	}
}

// ─── §4 — Signal allow-list ───────────────────────────────────────────────────

func TestParser_SignalAllowList_Valid(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "wait_for_event signal source MUST use OS-allowed signal")
	p, _ := parser.New(platform.NewFakePlatform()) // fake allows SIGTERM
	src := minimalRunbookWithSteps(`
  - step:
      id: wait_sig
      type: wait_for_event
      title: Wait for SIGTERM
      event:
        source: signal
        id: SIGTERM
  - step:
      id: done
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err != nil {
		t.Errorf("unexpected error for allowed signal SIGTERM: %v", err)
	}
}

func TestParser_SignalAllowList_Invalid(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "semantic validation MUST reject unsupported signals")
	// Use a platform that only allows SIGINT.
	fp := platform.NewFakePlatform()
	fp.Signals = []string{"SIGINT"}
	p, _ := parser.New(fp)

	src := minimalRunbookWithSteps(`
  - step:
      id: wait_sig
      type: wait_for_event
      title: Wait for SIGUSR1
      event:
        source: signal
        id: SIGUSR1
  - step:
      id: done
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for unsupported signal SIGUSR1, got nil")
	}
	if !strings.Contains(err.Error(), "signal/unsupported") {
		t.Errorf("expected signal/unsupported error, got: %v", err)
	}
}

// ─── §4 — Path normalization ──────────────────────────────────────────────────

func TestParser_PathNormalization(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "path fields MUST use / separators (normalize \\)")
	np := &normalizingPlatform{FakePlatform: platform.NewFakePlatform()}
	p, _ := parser.New(np)

	src := minimalRunbookWithSteps(`
  - step:
      id: run_cmd
      type: cli
      title: Run in windows path
      command: echo
      args: ["hi"]
      workdir: "C:\\some\\windows\\path"
  - step:
      id: done
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`)
	pr, err := p.ParseBytes(context.Background(), []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := findStep(pr.Runbook.Flow, "run_cmd")
	if step == nil || step.CLI == nil {
		t.Fatal("could not find step run_cmd with CLI spec")
	}
	if strings.Contains(step.CLI.Workdir, `\`) {
		t.Errorf("workdir still contains backslashes: %q", step.CLI.Workdir)
	}
}

// ─── §4 — branch step requires at least one arm ──────────────────────────────

func TestParser_BranchRequiresAtLeastOneArm(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "branch step branches MUST have at least one entry")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbookWithSteps(`
  - step:
      id: empty_branch
      type: branch
      title: Empty branch (should fail)
      branches: []
  - step:
      id: done
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for branch with empty arms, got nil")
	}
	assertErrorCode(t, err, "branch/no-arms")
}

// ─── §4 — approve step requires reviewers ─────────────────────────────────────

func TestParser_ApproveRequiresReviewers(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "approve step approvals MUST have at least one role or pool member")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbookWithSteps(`
  - step:
      id: approve_step
      type: approve
      title: Approve something
      approvals:
        mode: all
  - step:
      id: done
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for approve with no reviewers, got nil")
	}
	if !strings.Contains(err.Error(), "approve/no-reviewers") {
		t.Errorf("expected approve/no-reviewers error, got: %v", err)
	}
}

// ─── §4 — collector step requires at least one field ─────────────────────────

func TestParser_CollectorRequiresFields(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "collector fields MUST have at least one entry")
	p, _ := parser.New(platform.NewFakePlatform())
	src := minimalRunbookWithSteps(`
  - step:
      id: collect
      type: collector
      title: Collect
      prompt: "Enter data:"
      fields: []
  - step:
      id: done
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`)
	_, err := p.ParseBytes(context.Background(), []byte(src))
	if err == nil {
		t.Fatal("expected error for collector with empty fields, got nil")
	}
	if !strings.Contains(err.Error(), "collector/no-fields") {
		t.Errorf("expected collector/no-fields error, got: %v", err)
	}
}

// ─── Minimal valid runbook round-trip ─────────────────────────────────────────

func TestParser_MinimalValid(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§2", "minimal valid runbook MUST parse successfully")
	p, _ := parser.New(platform.NewFakePlatform())
	pr, err := p.ParseBytes(context.Background(), []byte(minimalRunbook()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rb := pr.Runbook
	if rb.APIVersion != "yawr.runbook/v1" {
		t.Errorf("APIVersion = %q", rb.APIVersion)
	}
	if rb.ID != "hello-world" {
		t.Errorf("ID = %q", rb.ID)
	}
	if rb.Name != "Hello World" {
		t.Errorf("Name = %q", rb.Name)
	}
	if len(rb.Flow) != 2 {
		t.Errorf("len(Flow) = %d; want 2", len(rb.Flow))
	}
}

// ─── Fixture round-trip: verify step IDs are parsed ──────────────────────────

func TestParser_FixtureR01_StepIDs(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "r01 fixture: step IDs must be correctly parsed")
	p, _ := parser.New(platform.NewFakePlatform())
	path := filepath.Join(fixturesDir, "r01-k8s-incident", "schema.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	pr, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse r01: %v", err)
	}
	// r01 must have detect_alert as first step.
	if len(pr.Runbook.Flow) == 0 {
		t.Fatal("flow is empty")
	}
	first := pr.Runbook.Flow[0].Step
	if first == nil || first.ID != "detect_alert" {
		t.Errorf("first step id = %q; want detect_alert", stepIDOrEmpty(pr.Runbook.Flow[0]))
	}
}

// ─── Fixture round-trip: iterate node ────────────────────────────────────────

func TestParser_FixtureR02_IterateNode(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "r02 fixture: iterate flow node must be correctly parsed")
	p, _ := parser.New(platform.NewFakePlatform())
	path := filepath.Join(fixturesDir, "r02-canary-deploy", "schema.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	pr, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse r02: %v", err)
	}
	// Find the iterate node in the flow.
	var found bool
	for _, fn := range pr.Runbook.Flow {
		if fn.Iterate != nil && fn.Iterate.ID != "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("r02: no iterate flow node found in parsed runbook")
	}
}

// ─── Fixture round-trip: r11 iterate loop ─────────────────────────────────────

func TestParser_FixtureR11_IterateLoop(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "r11 fixture: iterate FlowNode with cli steps must parse correctly")
	p, _ := parser.New(platform.NewFakePlatform())
	path := filepath.Join(fixturesDir, "r11-iterate-loop", "schema.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	pr, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse r11: %v", err)
	}

	// Find the iterate FlowNode and verify its shape.
	var iterNode *schema.IterateNode
	for _, fn := range pr.Runbook.Flow {
		if fn.Iterate != nil {
			iterNode = fn.Iterate
			break
		}
	}
	if iterNode == nil {
		t.Fatal("r11: no iterate FlowNode found in parsed runbook")
	}
	if iterNode.ID != "service_loop" {
		t.Errorf("iterate id = %q; want service_loop", iterNode.ID)
	}
	if iterNode.Over != "$.services" {
		t.Errorf("iterate over = %q; want $.services", iterNode.Over)
	}
	if iterNode.As != "svc" {
		t.Errorf("iterate as = %q; want svc", iterNode.As)
	}
	if len(iterNode.Steps) == 0 {
		t.Error("r11: iterate body steps must not be empty")
	}
	// Inner steps must be cli steps.
	for _, inner := range iterNode.Steps {
		if inner.Step == nil {
			t.Error("r11: expected all iterate body nodes to be steps")
			continue
		}
		if inner.Step.Type != schema.StepTypeCLI {
			t.Errorf("r11: inner step %q type = %q; want cli", inner.Step.ID, inner.Step.Type)
		}
	}
}

// ─── Fixture round-trip: r12 approval quorum ──────────────────────────────────

func TestParser_FixtureR12_ApprovalQuorum(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "r12 fixture: approve step with pool quorum must parse correctly")
	p, _ := parser.New(platform.NewFakePlatform())
	path := filepath.Join(fixturesDir, "r12-approval-quorum", "schema.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	pr, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse r12: %v", err)
	}

	approveStep := findStep(pr.Runbook.Flow, "get_approval")
	if approveStep == nil {
		t.Fatal("r12: step 'get_approval' not found")
	}
	if approveStep.Type != schema.StepTypeApprove {
		t.Errorf("r12: step type = %q; want approve", approveStep.Type)
	}
	if approveStep.ApproveSpec == nil {
		t.Fatal("r12: ApproveSpec is nil on approve step")
	}
	gate := approveStep.ApproveSpec.Approvals
	if len(gate.Pool) != 3 {
		t.Errorf("r12: approvals.pool len = %d; want 3", len(gate.Pool))
	}
	if gate.Required != 2 {
		t.Errorf("r12: approvals.required = %d; want 2", gate.Required)
	}
}

// ─── Fixture round-trip: r13 decision routing ─────────────────────────────────

func TestParser_FixtureR13_DecisionRouting(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "r13 fixture: decision step with routes and branch step must parse correctly")
	p, _ := parser.New(platform.NewFakePlatform())
	path := filepath.Join(fixturesDir, "r13-decision-routing", "schema.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	pr, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse r13: %v", err)
	}

	// Verify the decision step.
	decStep := findStep(pr.Runbook.Flow, "route")
	if decStep == nil {
		t.Fatal("r13: step 'route' not found")
	}
	if decStep.Type != schema.StepTypeDecision {
		t.Errorf("r13: step type = %q; want decision", decStep.Type)
	}
	if decStep.DecisionSpec == nil {
		t.Fatal("r13: DecisionSpec is nil on decision step")
	}
	if len(decStep.DecisionSpec.Routes) != 3 {
		t.Errorf("r13: decision routes len = %d; want 3", len(decStep.DecisionSpec.Routes))
	}

	// Verify the branch step.
	branchStep := findStep(pr.Runbook.Flow, "deploy_branch")
	if branchStep == nil {
		t.Fatal("r13: step 'deploy_branch' not found")
	}
	if branchStep.Type != schema.StepTypeBranch {
		t.Errorf("r13: step type = %q; want branch", branchStep.Type)
	}
	if branchStep.BranchSpec == nil {
		t.Fatal("r13: BranchSpec is nil on branch step")
	}
	if len(branchStep.BranchSpec.Branches) != 3 {
		t.Errorf("r13: branch arms len = %d; want 3", len(branchStep.BranchSpec.Branches))
	}
	// Last arm must be the else arm.
	lastArm := branchStep.BranchSpec.Branches[len(branchStep.BranchSpec.Branches)-1]
	if !lastArm.Else {
		t.Error("r13: last branch arm must be the else arm")
	}
}

func TestParser_BranchElseMarker(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	marker := `apiVersion: yawr.runbook/v1
id: else-marker
name: Else marker
flow:
  - step:
      id: choose
      type: branch
      branches:
        - else:
          label: Otherwise
          steps:
            - step:
                id: fallback
                type: noop
`
	parsed, err := p.ParseBytes(context.Background(), []byte(marker))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	branch := findStep(parsed.Runbook.Flow, "choose")
	if branch == nil || branch.BranchSpec == nil || len(branch.BranchSpec.Branches) != 1 {
		t.Fatalf("branch arm missing: %#v", branch)
	}
	arm := branch.BranchSpec.Branches[0]
	if !arm.Else || arm.Label != "Otherwise" || len(arm.Steps) != 1 || arm.Steps[0].Step.ID != "fallback" {
		t.Fatalf("fallback arm decoded incorrectly: %#v", arm)
	}
}

func TestParser_BranchFallbackRejectsAmbiguousForms(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	base := `apiVersion: yawr.runbook/v1
id: fallback-validation
name: Fallback validation
flow:
  - step:
      id: choose
      type: branch
      branches:
        - %s
          steps:
            - step:
                id: fallback
                type: noop
`
	for name, arm := range map[string]string{
		"else false":        "else: false",
		"conditionless arm": "label: Missing condition",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.ParseBytes(context.Background(), []byte(fmt.Sprintf(base, arm)))
			if err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
	for name, branches := range map[string]string{
		"fallback with condition": `
        - else:
          condition: 'true'
          steps: []`,
		"multiple fallbacks": `
        - else:
          steps: []
        - else: true
          steps: []`,
	} {
		t.Run(name, func(t *testing.T) {
			source := `apiVersion: yawr.runbook/v1
id: fallback-validation
name: Fallback validation
flow:
  - step:
      id: choose
      type: branch
      branches:` + branches + "\n"
			if _, err := p.ParseBytes(context.Background(), []byte(source)); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

// ─── Fixture round-trip: r14 assert and compensate ───────────────────────────

func TestParser_FixtureR14_AssertCompensate(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "r14 fixture: assert and compensate steps must parse correctly")
	p, _ := parser.New(platform.NewFakePlatform())
	path := filepath.Join(fixturesDir, "r14-assert-compensate", "schema.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	pr, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse r14: %v", err)
	}

	// Verify the compensate step.
	compensateStep := findStep(pr.Runbook.Flow, "register_rollback")
	if compensateStep == nil {
		t.Fatal("r14: step 'register_rollback' not found")
	}
	if compensateStep.Type != schema.StepTypeCompensate {
		t.Errorf("r14: step type = %q; want compensate", compensateStep.Type)
	}
	if compensateStep.CompensateSpec == nil {
		t.Fatal("r14: CompensateSpec is nil on compensate step")
	}
	if len(compensateStep.CompensateSpec.Compensate.Steps) != 2 {
		t.Errorf("r14: compensate steps len = %d; want 2", len(compensateStep.CompensateSpec.Compensate.Steps))
	}

	// Verify the assert step.
	assertStep := findStep(pr.Runbook.Flow, "verify_deployment")
	if assertStep == nil {
		t.Fatal("r14: step 'verify_deployment' not found")
	}
	if assertStep.Type != schema.StepTypeAssert {
		t.Errorf("r14: step type = %q; want assert", assertStep.Type)
	}
	if assertStep.AssertSpec == nil {
		t.Fatal("r14: AssertSpec is nil on assert step")
	}
	if len(assertStep.AssertSpec.Assert) != 2 {
		t.Errorf("r14: assertions len = %d; want 2", len(assertStep.AssertSpec.Assert))
	}
}

// ─── Fixture round-trip: r15 branch and collector ────────────────────────────

func TestParser_FixtureR15_BranchCollector(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "r15 fixture: branch and collector steps must parse correctly")
	p, _ := parser.New(platform.NewFakePlatform())
	path := filepath.Join(fixturesDir, "r15-branch-collector", "schema.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	pr, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse r15: %v", err)
	}

	// Verify the collector step.
	collectorStep := findStep(pr.Runbook.Flow, "collect_config")
	if collectorStep == nil {
		t.Fatal("r15: step 'collect_config' not found")
	}
	if collectorStep.Type != schema.StepTypeCollector {
		t.Errorf("r15: step type = %q; want collector", collectorStep.Type)
	}
	if collectorStep.CollectorSpec == nil {
		t.Fatal("r15: CollectorSpec is nil on collector step")
	}
	if len(collectorStep.CollectorSpec.Fields) != 4 {
		t.Errorf("r15: collector fields len = %d; want 4", len(collectorStep.CollectorSpec.Fields))
	}

	// Verify the branch step.
	branchStep := findStep(pr.Runbook.Flow, "environment_branch")
	if branchStep == nil {
		t.Fatal("r15: step 'environment_branch' not found")
	}
	if branchStep.Type != schema.StepTypeBranch {
		t.Errorf("r15: step type = %q; want branch", branchStep.Type)
	}
	if branchStep.BranchSpec == nil {
		t.Fatal("r15: BranchSpec is nil on branch step")
	}
	if len(branchStep.BranchSpec.Branches) != 3 {
		t.Errorf("r15: branch arms len = %d; want 3", len(branchStep.BranchSpec.Branches))
	}
}

// ─── Fixture round-trip: r16 end step ────────────────────────────────────────

func TestParser_FixtureR16_EndStep(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§4", "r16 fixture: end step with outcome must parse correctly")
	p, _ := parser.New(platform.NewFakePlatform())
	path := filepath.Join(fixturesDir, "r16-end-step", "schema.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	pr, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse r16: %v", err)
	}

	// Verify the end step.
	endStep := findStep(pr.Runbook.Flow, "task_complete")
	if endStep == nil {
		t.Fatal("r16: step 'task_complete' not found")
	}
	if endStep.Type != schema.StepTypeEnd {
		t.Errorf("r16: step type = %q; want end", endStep.Type)
	}
	if endStep.EndSpec == nil {
		t.Fatal("r16: EndSpec is nil on end step")
	}
	if endStep.EndSpec.Outcome == nil {
		t.Fatal("r16: Outcome is nil on end step")
	}
	if endStep.EndSpec.Outcome.Category != "resolved" {
		t.Errorf("r16: outcome.category = %q; want resolved", endStep.EndSpec.Outcome.Category)
	}
	if endStep.EndSpec.Outcome.Code != "task_succeeded" {
		t.Errorf("r16: outcome.code = %q; want task_succeeded", endStep.EndSpec.Outcome.Code)
	}
}

// ─── ParseBytes with invalid YAML ────────────────────────────────────────────

func TestParser_InvalidYAML(t *testing.T) {
	_ = testutil.Tag("03-schema-vnext.md", "§2", "invalid YAML MUST return a parse error")
	p, _ := parser.New(platform.NewFakePlatform())
	_, err := p.ParseBytes(context.Background(), []byte(":\t bad: yaml: ["))
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// minimalRunbook returns a minimal valid runbook YAML string.
func minimalRunbook() string {
	return `apiVersion: yawr.runbook/v1
id: hello-world
name: Hello World

flow:
  - step:
      id: greet
      type: cli
      title: Print greeting
      command: echo
      args: ["Hello, world!"]
      capture:
        output: stdout
  - step:
      id: done
      type: end
      title: Complete
      outcome:
        category: resolved
        code: success
`
}

// minimalRunbookWithStep wraps a single flow step block.
func minimalRunbookWithStep(stepBlock string) string {
	return `apiVersion: yawr.runbook/v1
id: test-runbook
name: Test Runbook
flow:` + stepBlock
}

// minimalRunbookWithSteps wraps a multi-step flow block.
func minimalRunbookWithSteps(steps string) string {
	return `apiVersion: yawr.runbook/v1
id: test-runbook
name: Test Runbook
flow:` + steps
}

// findStep searches a flow for a step with the given ID.
func findStep(flow []schema.FlowNode, id string) *schema.Step {
	for _, fn := range flow {
		if fn.Step != nil && fn.Step.ID == id {
			return fn.Step
		}
		if fn.Step != nil && fn.Step.BranchSpec != nil {
			for _, arm := range fn.Step.BranchSpec.Branches {
				if s := findStep(arm.Steps, id); s != nil {
					return s
				}
			}
		}
		if fn.Iterate != nil {
			if s := findStep(fn.Iterate.Steps, id); s != nil {
				return s
			}
		}
		if fn.Parallel != nil {
			for _, b := range fn.Parallel.Branches {
				if s := findStep(b.Steps, id); s != nil {
					return s
				}
			}
		}
	}
	return nil
}

// assertErrorCode verifies that err is a ValidationErrors containing at least one
// entry with the given code. Fails the test immediately if the assertion does not hold.
func assertErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var verr parser.ValidationErrors
	if !errors.As(err, &verr) {
		t.Fatalf("expected parser.ValidationErrors, got %T: %v", err, err)
	}
	for _, e := range verr {
		if e.Code == code {
			return
		}
	}
	t.Fatalf("expected error code %q not found in: %v", code, verr)
}

// stepIDOrEmpty returns the step ID from a FlowNode, or "" if it has no step.
func stepIDOrEmpty(fn schema.FlowNode) string {
	if fn.Step != nil {
		return fn.Step.ID
	}
	return ""
}

func TestParser_DisplayStep_ParsesSpec(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	src := minimalRunbookWithStep(`
  - step:
      id: show_report
      type: display
      title: Health Report
      display:
        content: "Service: {{ .svc }}"
        format: text
`)

	rb, err := p.ParseBytes(context.Background(), []byte(src))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	step := findStep(rb.Runbook.Flow, "show_report")
	if step == nil {
		t.Fatal("step show_report not found")
	}
	if step.DisplaySpec == nil {
		t.Fatal("DisplaySpec is nil — parser did not populate it")
	}
	if step.DisplaySpec.Display.Content == "" {
		t.Fatal("DisplaySpec.Display.Content is empty")
	}
	if step.DisplaySpec.Display.Format != "text" {
		t.Errorf("expected format 'text', got %q", step.DisplaySpec.Display.Format)
	}
}
