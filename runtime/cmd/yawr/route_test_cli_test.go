package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestRunRouteTest_ReachesTargetWithoutDispatch(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "route.runbook.yaml")
	artifactPath := filepath.Join(dir, "route.route-test.yaml")
	tracePath := filepath.Join(dir, "trace.jsonl")
	runbookSource := `apiVersion: yawr.runbook/v1
id: route-test-cli
name: Route test CLI
kind: mitigation
flow:
  - step:
      id: load_context
      type: host_action
      host_action:
        capability: test.saved-context
        request: { key: route }
      capture: { route_value: outputs.result.value }
  - step:
      id: choose_route
      type: branch
      branches:
        - condition: route_value == "go"
          steps:
            - step:
                id: dangerous_command
                type: cli
                run: must-not-run
        - else: true
          steps:
            - step:
                id: wrong_route
                type: end
                outcome: { category: blocked, code: wrong-route }
`
	if err := os.WriteFile(runbookPath, []byte(runbookSource), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("new parser: %v", err)
	}
	parsed, err := parserImpl.Parse(context.Background(), runbookPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	document, err := (&graphdoc.Builder{Loader: &cliLoader{p: parserImpl}, Recurse: true}).Build(context.Background(), parsed)
	if err != nil {
		t.Fatalf("graph document: %v", err)
	}
	registry, err := newToolRegistry(dir)
	if err != nil {
		t.Fatalf("tool registry: %v", err)
	}
	plan, err := internalplanner.New(plannerpkg.Config{Loader: &fileRunbookLoader{parser: parserImpl}, Tools: registry, ExpandPolicy: expand.Policy{Default: expand.ModeEager}}).Plan(context.Background(), parsed)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planHash, err := routeTestPlanHash(document.Hash, plan, buildRouteTestCLICatalog(t, runbookPath, parsed.Runbook.Requires), nil)
	if err != nil {
		t.Fatalf("route test hash: %v", err)
	}
	artifact := fmt.Sprintf(`apiVersion: yawr.route-test/v1
runbook: %q
plan_hash: %s
sensitivity_reviewed: true
target: { call_path: [choose_route], step: dangerous_command, phase: before, invocation: 1, attempt: 1 }
host_action_responses:
  - at: { step: load_context, phase: execute, invocation: 1, attempt: 1 }
    capability: test.saved-context
    response: { status: completed, result: { value: go } }
    source: { kind: manual }
    review: { state: reviewed, reviewed_by: operator, reviewed_at: "2026-08-28T12:05:00Z", sensitivity_reviewed: true }
  `, runbookPath, planHash)
	if err := os.WriteFile(artifactPath, []byte(artifact), 0o600); err != nil {
		t.Fatalf("write route test: %v", err)
	}

	output := runCaptureStdout(t, []string{
		runbookPath, "--route-test", artifactPath, "--trace", tracePath, "--output", "text",
	})
	if !strings.Contains(output, "Step reached - command not run") {
		t.Fatalf("output = %q", output)
	}
}

func TestRunRouteTest_TargetAfterToleratedFailureSucceeds(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "route.runbook.yaml")
	artifactPath := filepath.Join(dir, "route.route-test.yaml")
	runbookSource := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: route-test-tolerated-failure
name: Route test tolerated failure
kind: mitigation
flow:
	- step: { id: tolerated_failure, type: cli, run: must-not-run, on_error: continue }
	- step: { id: target, type: noop }
`, "\t", "  ")
	if err := os.WriteFile(runbookPath, []byte(runbookSource), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("new parser: %v", err)
	}
	parsed, err := parserImpl.Parse(context.Background(), runbookPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	document, err := (&graphdoc.Builder{Loader: &cliLoader{p: parserImpl}, Recurse: true}).Build(context.Background(), parsed)
	if err != nil {
		t.Fatalf("graph document: %v", err)
	}
	registry, err := newToolRegistry(dir)
	if err != nil {
		t.Fatalf("tool registry: %v", err)
	}
	plan, err := internalplanner.New(plannerpkg.Config{Loader: &fileRunbookLoader{parser: parserImpl}, Tools: registry, ExpandPolicy: expand.Policy{Default: expand.ModeEager}}).Plan(context.Background(), parsed)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planHash, err := routeTestPlanHash(document.Hash, plan, buildRouteTestCLICatalog(t, runbookPath, parsed.Runbook.Requires), nil)
	if err != nil {
		t.Fatalf("route test hash: %v", err)
	}
	artifact := fmt.Sprintf(`apiVersion: yawr.route-test/v1
runbook: %q
plan_hash: %s
sensitivity_reviewed: true
target: { step: target, phase: before, invocation: 1, attempt: 1 }
step_responses:
  - at: { step: tolerated_failure, phase: execute, invocation: 1, attempt: 1 }
    kind: cli
    status: failed
    outcome: failed
    output: { stderr: expected }
    source: { kind: manual }
    review: { state: reviewed, reviewed_by: operator, reviewed_at: "2026-08-28T12:05:00Z", sensitivity_reviewed: true }
`, runbookPath, planHash)
	if err := os.WriteFile(artifactPath, []byte(artifact), 0o600); err != nil {
		t.Fatalf("write route test: %v", err)
	}

	output := runCaptureStdout(t, []string{
		runbookPath, "--route-test", artifactPath, "--trace", filepath.Join(dir, "trace.jsonl"), "--output", "text",
	})
	if !strings.Contains(output, "Step reached - command not run") {
		t.Fatalf("output = %q", output)
	}
}

func TestRunRouteTest_RejectsStaleArtifactIdentity(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "route.runbook.yaml")
	artifactPath := filepath.Join(dir, "route.route-test.yaml")
	if err := os.WriteFile(runbookPath, []byte(`apiVersion: yawr.runbook/v1
id: stale-route
name: Stale route
kind: mitigation
flow:
  - step: { id: target, type: noop }
`), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	if err := os.WriteFile(artifactPath, []byte(fmt.Sprintf(`apiVersion: yawr.route-test/v1
runbook: %q
plan_hash: sha256:stale
sensitivity_reviewed: true
target: { step: target, phase: before, invocation: 1, attempt: 1 }
`, runbookPath)), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	stderr := captureStderr(t, func() int {
		return runRun([]string{
			runbookPath, "--route-test", artifactPath,
			"--trace", filepath.Join(dir, "trace.jsonl"), "--output", "text",
		})
	})
	if runLast != exitValidation || !strings.Contains(stderr, "plan changed") {
		t.Fatalf("exit=%d stderr=%q", runLast, stderr)
	}
}

func buildRouteTestCLICatalog(
	t *testing.T,
	runbookPath string,
	runbookRequires []*schema.PackageRequirement,
) *pkgcatalog.Catalog {
	t.Helper()
	workspaceRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	project, err := loadProjectConfig(workspaceRoot)
	if err != nil {
		t.Fatalf("loadProjectConfig: %v", err)
	}
	options := adapter.PackageCatalogOptions{
		WorkspaceRoot: workspaceRoot, Builtins: internaltool.NewBuiltinRegistry().All(),
	}
	if project != nil {
		options.ProjectRequires = project.Requires
		options.ProjectToolPaths = project.ToolPaths
	}
	catalog, catalogErrs := adapter.BuildPackageCatalog(options, runbookPath, runbookRequires)
	fatal, _ := errkit.SplitWarnings(catalogErrs)
	if len(fatal) > 0 {
		t.Fatalf("BuildPackageCatalog: %v", fatal)
	}
	return catalog
}

func TestSameFilePathUsesPlatformCaseSemantics(t *testing.T) {
	upper := filepath.Join("workspace", "Runbooks", "Route.runbook.yaml")
	lower := filepath.Join("workspace", "runbooks", "route.runbook.yaml")
	if !sameFilePathForOS("windows", upper, lower) {
		t.Fatal("Windows path identity must be case-insensitive")
	}
	if sameFilePathForOS("linux", upper, lower) {
		t.Fatal("Linux path identity must be case-sensitive")
	}
	if sameFilePathForOS("darwin", upper, lower) {
		t.Fatal("non-Windows path identity must not unconditionally fold case")
	}
}

func TestSameFilePathFollowsFilesystemCaseBehavior(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "Route.runbook.yaml")
	caseVariant := filepath.Join(dir, "route.runbook.yaml")
	if err := os.WriteFile(canonical, []byte("runbook"), 0o600); err != nil {
		t.Fatalf("write canonical path: %v", err)
	}
	canonicalInfo, err := os.Stat(canonical)
	if err != nil {
		t.Fatalf("stat canonical path: %v", err)
	}
	variantInfo, variantErr := os.Stat(caseVariant)
	want := false
	if variantErr == nil {
		want = os.SameFile(canonicalInfo, variantInfo)
	} else if !os.IsNotExist(variantErr) {
		t.Fatalf("stat case variant: %v", variantErr)
	}
	if got := sameFilePath(canonical, caseVariant); got != want {
		t.Fatalf("sameFilePath case result = %v, want filesystem identity %v", got, want)
	}
}
