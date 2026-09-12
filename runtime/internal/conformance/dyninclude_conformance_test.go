package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type DynIncludeVector struct {
	ID          string             `yaml:"id"`
	Category    string             `yaml:"category"`
	Description string             `yaml:"description"`
	Input       string             `yaml:"input"`
	Variables   map[string]string  `yaml:"variables"`
	Expected    DynIncludeExpected `yaml:"expected"`
	Note        string             `yaml:"note,omitempty"`
	Tags        []string           `yaml:"tags,omitempty"`
}

type DynIncludeExpected struct {
	ErrorClass string `yaml:"error_class,omitempty"`
	ErrorCode  string `yaml:"error_code,omitempty"`
	Value      any    `yaml:"value,omitempty"`
	HasValue   bool   `yaml:"-"`
}

type dynIncludeVectorFile struct {
	Vectors []DynIncludeVector `yaml:"vectors"`
}

func (e *DynIncludeExpected) UnmarshalYAML(node *yaml.Node) error {
	type expected DynIncludeExpected
	var decoded expected
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "value" {
			decoded.HasValue = true
			break
		}
	}
	*e = DynIncludeExpected(decoded)
	return nil
}

func (e DynIncludeExpected) WantsError() bool {
	return e.ErrorClass != "" || e.ErrorCode != ""
}

func dynincludeDataDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	return filepath.Join(filepath.Dir(file), "dynincludedata")
}

func LoadDynIncludeSuite(ctx context.Context, dir string) ([]DynIncludeVector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "tv-dyninclude.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read tv-dyninclude.yaml: %w", err)
	}
	var file dynIncludeVectorFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("decode tv-dyninclude.yaml: %w", err)
	}
	return file.Vectors, nil
}

func TestLoadDynIncludeSuite(t *testing.T) {
	vectors, err := LoadDynIncludeSuite(context.Background(), dynincludeDataDir(t))
	if err != nil {
		t.Fatalf("LoadDynIncludeSuite() error = %v", err)
	}
	if got, want := len(vectors), 29; got != want {
		t.Fatalf("len(vectors) = %d, want %d", got, want)
	}
}

const (
	dynIncludeRequiresVersionReason = "DEFECT — these fixtures intentionally model dynamic catalog resolution through requires: entries without version, but the current CLI/schema layer rejects them earlier with [schema/structural] requires.*: missing property 'version'. Catalog freeze and dynamic include execution are never reached."
	dynIncludeEmptyLiteralReason    = "DEFECT — an empty literal runbook_ref is treated as the dynamic arm being absent at semantic-validation time, so the parser emits include/missing-target (exactly-one-of violation) and never reaches the dynamic loader's empty-ref DINC-002 path."
)

var dynIncludeDefectSkips = map[string]string{
	// CLI preflight vectors are covered by production-path tests.
	// REF-011 empty literals are covered by DINC-002.
	// Entry governance seeding is covered by production-path tests.

	// ── Permanent skip: B-16 (intentional defensive dead code) ──────────────
	// DINC-009 is retained as intentional dead code. expr.Evaluator.Eval() always
	// returns a string; non-string refs cannot occur in practice. B-16 closes this
	// as "not a defect" — the evaluator's string-normalizing contract is correct and
	// is not changing. The non-string path cannot fire by design.
	"TV-DYN-TMPL-005": "PERMANENT SKIP — expr.Evaluator.Eval() always returns string, so non-string refs are unreachable by design.",

	// WithEntryGovernance before eng.Start seeds the entry runbook's
	// static governance: block as the initial dynGov in context. ComposeGovernance
	// now receives the real parent policy instead of nil. TV-DYN-GOV-005 removed.

	// ── Harness limitation: GOV-001 ──────────────────────────────────────────
	// TTYOutput=true → TerminalApprovalGate (can deny → DYN-015). TTYOutput=false (all
	// non-interactive runs) → NoOpApprovalGate (silently approves). The CLI subprocess
	// harness always runs with TTYOutput=false so always gets NoOpApprovalGate.
	// require_approval IS enforceable in real interactive deployments; this is a
	// harness/subprocess limitation only, not a governance gap.
	// dynamic_include_test.go covers nil-gate and denied-gate DYN-015 paths directly.
	"TV-DYN-GOV-001": "HARNESS LIMITATION — the subprocess harness has no TTY; interactive approval enforcement is covered by unit tests.",

	// ── Permanent skip: B-17 (capabilities out of scope) ────────────────────
	// schema.GovernanceConfig has no Capabilities field; capability narrowing cannot
	// be expressed or enforced. B-17 marks capabilities as a forward-reference and
	// amends §6 of the contract accordingly. GOV-015 tests capabilities specifically.
	"TV-DYN-GOV-015": "PERMANENT SKIP — schema.GovernanceConfig has no Capabilities field, so this vector is outside the current contract.",

	// ── Production defect DEF-004 (closed by B-19) ──────────────────────────
	// when: conditions are stored in ResolvedStep.When and validated at plan time
	// but NEVER evaluated at runtime by the engine. B-19 rules DEF-004 out of scope
	// for dynamic includes; the correct pattern is branch/condition: (already what
	// the ICM e2e tests use). TV-DYN-COMPAT-001 has been rewritten to branch/condition:.
}

func mappedDynIncludeCode(want string) string {
	switch want {
	case "DYN-001":
		return "DINC-001"
	case "DYN-002":
		return "DINC-002"
	case "DYN-005":
		return "DINC-003"
	case "DYN-006":
		return "DINC-002"
	case "DYN-013":
		return "DINC-004"
	case "DYN-014":
		return "DINC-005"
	default:
		return want
	}
}

func outputContainsAny(res cliResult, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && (strings.Contains(res.stdout, needle) || strings.Contains(res.stderr, needle)) {
			return true
		}
	}
	return false
}

func assertDynIncludeResult(t *testing.T, v DynIncludeVector, res cliResult) {
	t.Helper()
	if v.Expected.WantsError() {
		if res.exitCode == 0 {
			t.Fatalf("expected failure (%s), CLI succeeded: stdout=%q stderr=%q", v.Expected.ErrorCode, res.stdout, res.stderr)
		}
		if v.Expected.ErrorCode == "SCHEMA-001" {
			if !outputContainsAny(res, "schema/structural", "include/") {
				t.Fatalf("expected schema/structural or include/* implementation code in stdout/stderr, got stdout=%q stderr=%q", res.stdout, res.stderr)
			}
			return
		}
		wantCode := mappedDynIncludeCode(v.Expected.ErrorCode)
		if wantCode != "" && !outputContainsAny(res, wantCode) {
			t.Fatalf("expected error code %s in stdout/stderr, got stdout=%q stderr=%q", wantCode, res.stdout, res.stderr)
		}
		return
	}
	if res.exitCode != 0 {
		t.Fatalf("expected success, got exit %d: stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
}

func TestDynIncludeConformance(t *testing.T) {
	vectors, err := LoadDynIncludeSuite(context.Background(), dynincludeDataDir(t))
	if err != nil {
		t.Fatalf("LoadDynIncludeSuite() error = %v", err)
	}
	harness := NewEnumHarness(t)

	passed, skipped, failed := 0, 0, 0
	for _, v := range vectors {
		v := v
		t.Run(v.ID, func(t *testing.T) {
			if reason, ok := dynIncludeDefectSkips[v.ID]; ok {
				skipped++
				t.Skipf("%s: %s", v.ID, reason)
			}

			workDir := t.TempDir()
			if err := writeWorkspace(workDir, v.Variables); err != nil {
				failed++
				t.Fatalf("write workspace: %v", err)
			}
			res, err := harness.runCLI(workDir, v.Input)
			if err != nil {
				failed++
				t.Fatalf("run CLI: %v", err)
			}
			assertDynIncludeResult(t, v, res)
			passed++
		})
	}
	failed = len(vectors) - passed - skipped
	t.Cleanup(func() {
		t.Logf("TV-DYNCLUDE conformance report: %d vectors total, %d passed, %d skipped (named reasons), %d failed", len(vectors), passed, skipped, failed)
	})
}
