package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

// fakeResumeHandle is a minimal engine.RunHandle stub exposing only the
// State() this test needs; all other methods are unused by
// checkResumePackageDrift and simply satisfy the interface.
type fakeResumeHandle struct {
	state engine.RunState
}

func (f *fakeResumeHandle) Next(ctx context.Context) (*engine.StepResult, error) { return nil, nil }
func (f *fakeResumeHandle) Approve(ctx context.Context, decision engine.ApprovalDecision) error {
	return nil
}
func (f *fakeResumeHandle) SubmitEvidence(ctx context.Context, stepID string, ev map[string]*engine.EvidenceValue) error {
	return nil
}
func (f *fakeResumeHandle) Cancel(ctx context.Context, reason string) error { return nil }
func (f *fakeResumeHandle) State() engine.RunState                          { return f.state }
func (f *fakeResumeHandle) Events() <-chan engine.Event                     { return nil }

// TestCheckResumePackageDrift_MatchingDigests_NoOp verifies that when the
// recorded catalog digest still matches what a fresh rebuild computes, the
// resume proceeds silently (no error, no drift-accepted trace event).
func TestCheckResumePackageDrift_MatchingDigests_NoOp(t *testing.T) {
	dir, cat, plan := setupDriftTestFixture(t)
	ctx := context.Background()

	tracePath := filepath.Join(dir, "trace.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	defer writer.Close()
	ecfg := engine.EngineConfig{TraceWriter: writer}

	plan.Metadata.CatalogDigest = cat.CatalogDigest()
	handle := &fakeResumeHandle{state: engine.RunState{RunID: "run-1", Plan: plan}}

	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	err = checkResumePackageDrift(ctx, ecfg, mustParser(t), handle, "run-1", "", false)
	if err != nil {
		t.Fatalf("checkResumePackageDrift with matching digest: unexpected error: %v", err)
	}
}

// TestCheckResumePackageDrift_Mismatch_RefusedWithoutFlag verifies the hard
// PKG-009 refusal on a catalog digest mismatch when --allow-package-drift
// was not passed.
func TestCheckResumePackageDrift_Mismatch_RefusedWithoutFlag(t *testing.T) {
	dir, _, plan := setupDriftTestFixture(t)
	ctx := context.Background()

	tracePath := filepath.Join(dir, "trace.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	defer writer.Close()
	ecfg := engine.EngineConfig{TraceWriter: writer}

	plan.Metadata.CatalogDigest = "sha256:deadbeef-not-the-real-digest"
	handle := &fakeResumeHandle{state: engine.RunState{RunID: "run-1", Plan: plan}}

	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	err = checkResumePackageDrift(ctx, ecfg, mustParser(t), handle, "run-1", "", false)
	if err == nil {
		t.Fatalf("expected PKG-009 error on digest mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "PKG-009") {
		t.Fatalf("expected PKG-009 in error, got: %v", err)
	}
}

// TestCheckResumePackageDrift_Mismatch_AcceptedWithFlag verifies that
// --allow-package-drift bypasses the refusal and records an auditable
// governance/packageDriftAccepted trace event rather than silently
// swallowing the mismatch.
func TestCheckResumePackageDrift_Mismatch_AcceptedWithFlag(t *testing.T) {
	dir, _, plan := setupDriftTestFixture(t)
	ctx := context.Background()

	tracePath := filepath.Join(dir, "trace.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	ecfg := engine.EngineConfig{TraceWriter: writer}

	plan.Metadata.CatalogDigest = "sha256:deadbeef-not-the-real-digest"
	handle := &fakeResumeHandle{state: engine.RunState{RunID: "run-1", Plan: plan}}

	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	err = checkResumePackageDrift(ctx, ecfg, mustParser(t), handle, "run-1", "", true)
	if err != nil {
		t.Fatalf("checkResumePackageDrift with --allow-package-drift: unexpected error: %v", err)
	}
	writer.Close()

	traceBytes, rerr := os.ReadFile(tracePath)
	if rerr != nil {
		t.Fatalf("read trace.jsonl: %v", rerr)
	}
	var sawDriftAccepted bool
	for _, line := range splitLines(traceBytes) {
		if len(line) == 0 {
			continue
		}
		var evt struct {
			Kind    string          `json:"kind"`
			RunID   string          `json:"run_id"`
			Payload json.RawMessage `json:"payload"`
		}
		if jerr := json.Unmarshal(line, &evt); jerr != nil {
			continue
		}
		if evt.Kind == "governance/packageDriftAccepted" {
			sawDriftAccepted = true
			if evt.RunID != "run-1" {
				t.Fatalf("drift-accepted event run_id = %q, want %q", evt.RunID, "run-1")
			}
		}
	}
	if !sawDriftAccepted {
		t.Fatalf("trace.jsonl does not contain governance/packageDriftAccepted; raw: %s", traceBytes)
	}
}

// setupDriftTestFixture builds a workspace directory with one required
// package and a calling runbook, then plans it once to obtain the
// ExecutionPlan the way runWithMode's fresh-run branch would (RunbookPath
// set, ready for checkResumePackageDrift to re-parse and rebuild the
// catalog against).
func setupDriftTestFixture(t *testing.T) (dir string, cat *pkgcatalog.Catalog, plan *engine.ExecutionPlan) {
	t.Helper()
	pkgsRoot := t.TempDir()
	pkgDir := filepath.Join(pkgsRoot, "drift-pkg")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.drift-tools
  version: "1.0.0"
exports:
  tools:
    - id: pingtool
      path: tools/ping.tool.yaml
`)
	writeFile(t, filepath.Join(pkgDir, "tools", "ping.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: pingtool
  version: "1.0.0"
transport:
  mode: native
  command: does-not-run
actions:
  - name: ping
    description: no-op action used only to force catalog resolution
    args: {}
`)

	dir = makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	relPkg := relForwardSlash(t, absDir, pkgDir)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\n"+
		"requires:\n"+
		"  - package: acme.drift-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relPkg+"\n")

	runbookPath := filepath.Join(absDir, "runbook.yaml")
	writeFile(t, runbookPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-drift-live
name: drift live CLI test
toolRefs:
  - name: pingtool
    package: acme.drift-tools
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	cat = buildDriftCatalog(t, absDir, runbookPath)
	plan = &engine.ExecutionPlan{RunID: "run-1", RunbookPath: runbookPath}
	// Mirror the real fresh-run branch (cmd/yawr/run.go), which always
	// freezes PackageDigests into the plan alongside CatalogDigest: the
	// manifest's PackageDigests set is the authoritative record B4 checks
	// resolved-now packages against, so a realistic fixture must populate
	// it the same way production does.
	pkgDigests := make(map[string]string, len(cat.Packages))
	for _, p := range cat.Packages {
		pkgDigests[p.Name] = p.Digest
	}
	plan.Metadata.PackageDigests = pkgDigests
	return absDir, cat, plan
}

// buildDriftCatalog rebuilds the package catalog for dir/runbookPath the
// same way runWithMode's fresh-run branch does, for use as the "recorded at
// plan time" baseline in drift tests.
func buildDriftCatalog(t *testing.T, dir, runbookPath string) *pkgcatalog.Catalog {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	parserImpl := mustParser(t)
	parsed, err := parserImpl.Parse(context.Background(), runbookPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	projCfg, cfgErr := loadProjectConfig(dir)
	if cfgErr != nil {
		t.Fatalf("loadProjectConfig: %v", cfgErr)
	}
	catOpts := adapter.PackageCatalogOptions{
		WorkspaceRoot:    dir,
		Builtins:         internaltool.NewBuiltinRegistry().All(),
		ProjectRequires:  projCfg.Requires,
		ProjectToolPaths: projCfg.ToolPaths,
	}
	cat, catErrs := adapter.BuildPackageCatalog(catOpts, runbookPath, parsed.Runbook.Requires)
	fatal, _ := errkit.SplitWarnings(catErrs)
	if len(fatal) > 0 {
		t.Fatalf("BuildPackageCatalog: %v", fatal)
	}
	return cat
}

func mustParser(t *testing.T) parser.Parser {
	t.Helper()
	p, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("internalparser.New: %v", err)
	}
	return p
}
