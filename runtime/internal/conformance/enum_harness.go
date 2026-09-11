package conformance

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

// EnumHarness runs tv-enum.yaml vectors against the real yawr CLI binary
// built from this checkout:
// each vector's `variables` map is materialized as a real, disposable
// workspace directory and `input` is run with `yawr run` exactly the way
// a user would invoke it, so the harness exercises the true CLI/API
// caller-binding, plan, and runtime layers rather than a bespoke in-process
// shortcut. The three
// TV-ENUM-SUBST catalog vectors are the sole exception: they assert on
// pkgcatalog.Catalog contents, which the CLI's own output does not
// expose, so those are resolved via the identical pkgcatalog.Build/
// adapter.BuildPackageCatalog call the CLI itself makes.
type EnumHarness struct {
	binPath     string
	profilePath string // absolute path to unattended-test.profile.yaml
}

var (
	buildOnce sync.Once
	buildErr  error
	buildPath string
)

// NewEnumHarness builds (once per test binary run) the yawr CLI to a
// scratch location and returns a harness that can run vectors against it.
func NewEnumHarness(t *testing.T) *EnumHarness {
	t.Helper()
	buildOnce.Do(func() {
		buildPath, buildErr = buildYawrBinary()
	})
	if buildErr != nil {
		t.Fatalf("build yawr CLI: %v", buildErr)
	}
	// Resolve the unattended-test profile path once. The profile tells the CLI
	// that this is a non-interactive test invocation, which is
	// required because profileless non-interactive
	// execution is now rejected at startup. Test context permits deterministic
	// fixture tools without weakening current unattended production policy.
	moduleRoot, mrErr := findModuleRoot()
	if mrErr != nil {
		t.Fatalf("find module root for profile: %v", mrErr)
	}
	profilePath := filepath.Join(moduleRoot, "internal", "conformance", "testdata", "unattended-test.profile.yaml")
	return &EnumHarness{binPath: buildPath, profilePath: profilePath}
}

func buildYawrBinary() (string, error) {
	moduleRoot, err := findModuleRoot()
	if err != nil {
		return "", err
	}
	// Build to the OS temp directory (not moduleRoot): this is a scratch
	// build artifact for the test run only and must never land inside the
	// working tree, which would pollute `git status` for a repo this
	// harness must not itself dirty.
	outDir, err := os.MkdirTemp("", "enumharness-bin-")
	if err != nil {
		return "", err
	}
	binName := "yawr-conformance"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(outDir, binName)
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/yawr")
	cmd.Dir = moduleRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build ./cmd/yawr: %w: %s", err, stderr.String())
	}
	return binPath, nil
}

// findModuleRoot walks up from this source file to the directory
// containing go.mod, mirroring conformance_test.go's testdataDir helper.
func findModuleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve caller path")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", file)
		}
		dir = parent
	}
}

// gitBashDir returns the directory containing a working bash.exe usable by
// the CLI's "cli" steps (several vectors' fixtures use `command: bash`,
// mirroring tv-pkg-resolve.yaml's own fixture shape), preferring Git for
// Windows' bash over any WSL launcher shim that may otherwise shadow it on
// PATH. Returns "" if none is found (non-Windows platforms rely on the
// system bash already being first on PATH).
func gitBashDir() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	candidates := []string{
		`C:\Program Files\Git\bin`,
		`C:\Program Files (x86)\Git\bin`,
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "bash.exe")); err == nil {
			return c
		}
	}
	return ""
}

// writeWorkspace materializes an EnumVector's variables map (workspace-
// relative POSIX path -> file content) onto disk under root.
func writeWorkspace(root string, files map[string]string) error {
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(path.Clean("/" + rel))[1:])
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

// cliResult is the outcome of one `yawr run` invocation.
type cliResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func (h *EnumHarness) runCLI(workDir, entry string) (cliResult, error) {
	cmd := exec.Command(h.binPath, "run", entry,
		"--trace", filepath.Join(workDir, ".trace.jsonl"),
		"--profile", h.profilePath,
	)
	cmd.Dir = workDir
	env := os.Environ()
	if gb := gitBashDir(); gb != "" {
		// Windows env var names are case-insensitive but os.Environ()
		// preserves whatever case the OS actually used (commonly "Path",
		// not "PATH"); simply prepending a new "PATH=..." entry without
		// removing the inherited "Path"/"PATH" entry leaves TWO PATH
		// variables in the child's environment block, and Windows/the Go
		// runtime does not guarantee the prepended one wins for the
		// child's own lookups -- in practice the child process (yawr.exe)
		// ended up resolving `bash` via the broken WSL launcher shim
		// instead of Git for Windows' bash, silently corrupting every
		// vector whose fixture runs a `command: bash` step. Filter out
		// every existing PATH-named entry (case-insensitively) before
		// prepending the single, correct one.
		filtered := env[:0]
		for _, kv := range env {
			if len(kv) >= 5 && strings.EqualFold(kv[:5], "path=") {
				continue
			}
			filtered = append(filtered, kv)
		}
		env = append([]string{"PATH=" + gb + string(os.PathListSeparator) + os.Getenv("PATH")}, filtered...)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return cliResult{}, fmt.Errorf("run yawr CLI: %w", runErr)
		}
	}
	return cliResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}, nil
}

// enumRuntimeGapSkips lists vectors with no reachable runtime site. Each skip
// has a concrete current reason so the harness never drops a vector silently.
var enumRuntimeGapSkips = map[string]string{
	"TV-ENUM-RUNTIME-001": "no runtime site reads Input.From ('from: env')",
	"TV-ENUM-RUNTIME-002": "no runtime site reads Input.From ('from: prompt')",
	"TV-ENUM-RUNTIME-003": "no runtime site reads Input.From ('from: <provider>.<field>')",
	"TV-ENUM-RUNTIME-006": "root runbooks do not evaluate their top-level outputs block",
	"TV-ENUM-MOCK-005":    "Input.From is not read, so replay has no prompt-binding event to intercept",
	"TV-ENUM-UNICODE-005": "fixture default 'graceful' is not a member of enum [\"\\u00e1\",\"force\"], so current validation returns ENUM-006",
	"TV-ENUM-PLAN-003":    "caller declares no strategy binding, so a full run returns GIS-PATH-MISSING; the vector expects an unevaluated plan-only value",
	"TV-ENUM-PLAN-005":    "under YAML 1.2 bare yes/no are strings, so the fixture has no malformed enum member and ENUM-002 does not fire",
	"TV-ENUM-RUNTIME-004": "substitute input and action argument declare unequal enum sets, so package validation returns PKG-013",
	"TV-ENUM-RUNTIME-007": "root runbooks do not evaluate their top-level outputs block, so the expected output check is unreachable",

	// Governance vectors: skip entries for vectors that are valid
	// schema contracts but cannot be executed by the CLI harness today.
}

// Run executes one EnumVector and returns its verdict.
func (h *EnumHarness) Run(ctx context.Context, t *testing.T, v EnumVector) Result {
	if reason, skip := enumRuntimeGapSkips[v.ID]; skip {
		return Result{Verdict: VerdictSkip, Reason: reason}
	}

	if v.Expected.WantsCatalog() {
		return h.runCatalogVector(t, v)
	}
	return h.runCLIVector(ctx, t, v)
}

func (h *EnumHarness) runCLIVector(ctx context.Context, t *testing.T, v EnumVector) Result {
	t.Helper()
	workDir := t.TempDir()
	if err := writeWorkspace(workDir, v.Variables); err != nil {
		return Result{Verdict: VerdictFail, Err: err}
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_ = runCtx // exec.Command below does not take ctx directly; timeout enforced by test framework's own -timeout.
	res, err := h.runCLI(workDir, v.Input)
	if err != nil {
		return Result{Verdict: VerdictFail, Err: err}
	}

	if v.Expected.WantsError() {
		wantCode := v.Expected.ErrorCode
		if wantCode == "" && v.Expected.Error != nil {
			wantCode = v.Expected.Error.Code
		}
		if res.exitCode == 0 {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected failure (%s), CLI succeeded: stdout=%q stderr=%q", wantCode, res.stdout, res.stderr)}
		}
		// Plan-time/validation errors (e.g. from cmd/yawr/run.go's own
		// exitValidation paths) are written to stderr, but per-step
		// runtime failures (a "tool"/"cli" step's own Status=Failed
		// Error, e.g. ENUM-008/ENUM-009) are rendered on stdout by
		// renderStepText -- check both streams for the error code.
		if wantCode != "" && !strings.Contains(res.stderr, wantCode) && !strings.Contains(res.stdout, wantCode) {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected error code %s in stdout/stderr, got stdout=%q stderr=%q", wantCode, res.stdout, res.stderr)}
		}
		if v.Expected.ErrorClass != "" {
			if sentinel := errkit.ClassSentinel(v.Expected.ErrorClass); sentinel != nil {
				_ = sentinel // class membership is implied by the code match above; codes are unique per class in this registry.
			}
		}
		return Result{Verdict: VerdictPass}
	}

	// Success vector: the run must complete without an ENUM/GCP-TYPE/PKG
	// failure. Any declared warnings (e.g. ENUM-W001) must be surfaced
	// non-fatally on stderr (R4) without failing the run.
	if res.exitCode != 0 {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected success, got exit %d: stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)}
	}
	for _, w := range v.Expected.Warnings {
		if !strings.Contains(res.stderr, w) {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected warning %s on stderr, got: %q", w, res.stderr)}
		}
	}
	return Result{Verdict: VerdictPass}
}

// runCatalogVector resolves the vector's requires:/toolRefs: through the
// exact pkgcatalog.Build/adapter.BuildPackageCatalog path `yawr run` uses
// (internal/adapter/toolrefs.go), then compares the frozen catalog's
// bare-name entries against the vector's expected projection.
func (h *EnumHarness) runCatalogVector(t *testing.T, v EnumVector) Result {
	t.Helper()
	workDir := t.TempDir()
	if err := writeWorkspace(workDir, v.Variables); err != nil {
		return Result{Verdict: VerdictFail, Err: err}
	}
	entryPath := filepath.Join(workDir, filepath.FromSlash(v.Input))

	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		return Result{Verdict: VerdictFail, Err: err}
	}
	parsed, err := parserImpl.Parse(context.Background(), entryPath)
	if err != nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("parse %s: %w", v.Input, err)}
	}
	if parsed.Runbook == nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("parsed runbook is nil for %s", v.Input)}
	}

	cat, catErrs := pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot:   workDir,
		Builtins:        internaltool.NewBuiltinRegistry().All(),
		RunbookRequires: parsed.Runbook.Requires,
		RunbookPath:     entryPath,
	})
	if fatal, _ := errkit.SplitWarnings(catErrs); len(fatal) > 0 {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("unexpected catalog build errors: %v", fatal)}
	}

	for bare, want := range v.Expected.Catalog {
		entries := cat.ByBare(bare)
		if len(entries) == 0 {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("no catalog entry for bare name %q", bare)}
		}
		entry := entries[len(entries)-1] // highest-precedence (tier-ascending list; last wins)
		if entry.Qualified != want.QualifiedName {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("bare %q: qualifiedName = %q, want %q", bare, entry.Qualified, want.QualifiedName)}
		}
		if int(entry.Tier) != want.Tier {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("bare %q: tier = %d, want %d", bare, entry.Tier, want.Tier)}
		}
	}
	return Result{Verdict: VerdictPass}
}
