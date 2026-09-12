package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRun_EntryGovernance_DenyCommandBlocksChild proves DEF-006 is fixed:
// a deny_commands rule declared on the *entry* runbook's governance: block
// must block a matching command inside a dynamically included child. Before
// the fix, ComposeGovernance received nil as the parent, so the entry
// runbook's deny list was silently dropped and the command ran.
func TestRun_EntryGovernance_DenyCommandBlocksChild(t *testing.T) {
	pkgDir := t.TempDir()

	// Child runbook runs a command that the parent forbids.
	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme-gov-test
  version: "1.0.0"
exports:
  runbooks:
    - id: risky-op
      path: runbooks/risky-op.runbook.yaml
`)
	writeFile(t, filepath.Join(pkgDir, "runbooks", "risky-op.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: risky-op
name: Risky Operation
flow:
  - step:
      id: exec
      type: cli
      command: "yawr-blocked-by-governance"
`)

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	relPkg := relForwardSlash(t, absDir, pkgDir)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"),
		"apiVersion: yawr.config/v1\nrequires:\n"+
			"  - package: acme-gov-test\n"+
			"    version: \"^1.0.0\"\n"+
			"    path: "+relPkg+"\n")

	// Parent runbook denies the exact command the child tries to run.
	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-gov-entry-deny
name: entry governance deny test
governance:
  deny_commands:
    - "yawr-blocked-by-governance"
flow:
  - step:
      id: dyn
      type: include
      include:
        runbook_ref: "acme-gov-test/risky-op"
        resolve_from: catalog
`)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	code := runRun([]string{"runbook.yaml", "--output", "quiet",
		"--trace", filepath.Join(dir, "trace.jsonl")})
	if code == exitSuccess {
		t.Fatalf("expected non-success: entry governance deny_commands must block the child's command, got exitSuccess")
	}
}

// TestRun_EntryGovernance_AllowedCommandPassesThrough verifies the positive
// case: a command NOT in the entry runbook's deny list still runs normally.
func TestRun_EntryGovernance_AllowedCommandPassesThrough(t *testing.T) {
	pkgDir := t.TempDir()

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme-gov-allow
  version: "1.0.0"
exports:
  runbooks:
    - id: simple-op
      path: runbooks/simple-op.runbook.yaml
`)
	writeFile(t, filepath.Join(pkgDir, "runbooks", "simple-op.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: simple-op
name: Simple Op
flow:
  - step:
      id: done
      type: end
      outcome: {category: success, code: ok}
`)

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	relPkg := relForwardSlash(t, absDir, pkgDir)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"),
		"apiVersion: yawr.config/v1\nrequires:\n"+
			"  - package: acme-gov-allow\n"+
			"    version: \"^1.0.0\"\n"+
			"    path: "+relPkg+"\n")

	// Parent denies a command the child never runs.
	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-gov-entry-allow
name: entry governance allow test
governance:
  deny_commands:
    - "yawr-different-blocked-command"
flow:
  - step:
      id: dyn
      type: include
      include:
        runbook_ref: "acme-gov-allow/simple-op"
        resolve_from: catalog
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	code := runRun([]string{"runbook.yaml", "--output", "quiet",
		"--trace", filepath.Join(dir, "trace.jsonl")})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess when denied command is not used, got %d", code)
	}
}
