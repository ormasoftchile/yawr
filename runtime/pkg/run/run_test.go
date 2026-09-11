package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// TestStart_MinimalRunbook tests that Start successfully wires a minimal runbook.
func TestStart_MinimalRunbook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell utilities (echo, false, /bin/sh)")
	}
	runbookYAML := `
apiVersion: yawr.runbook/v1
id: test-runbook
name: Test Runbook
flow:
  - step:
      id: step1
      type: cli
      run:
        command: echo hello
`

	// Create a test FS with the runbook
	cfg := Config{
		RunbookPath: createTempRunbook(t, runbookYAML),
		Client:      "test",
	}

	ctx := context.Background()
	handle, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if handle == nil {
		t.Fatal("Start() returned nil handle")
	}

	// Verify we can call Next() and it returns a result
	result, err := handle.Next(ctx)
	if err != nil && err != io.EOF {
		t.Fatalf("Next() failed: %v", err)
	}

	if result == nil && err != io.EOF {
		t.Fatal("Next() returned nil result without EOF")
	}

	// If we got a result, verify it has the expected step ID
	if result != nil && result.StepID != "step1" {
		t.Errorf("Expected step ID 'step1', got %q", result.StepID)
	}
}

func TestStart_InvalidLateExpressionBlocksExecution(t *testing.T) {
	var steps strings.Builder
	for i := 1; i <= 26; i++ {
		steps.WriteString("  - step:\n")
		steps.WriteString("      id: early")
		steps.WriteString(string(rune('a' + i%26)))
		steps.WriteString("\n      type: display\n      display:\n        content: ok\n")
	}
	runbookYAML := "apiVersion: yawr.runbook/v1\nid: parse-gate\nname: Parse Gate\nflow:\n" + steps.String() + `  - step:
      id: late_bad
      type: branch
      branches:
        - condition: '!ready'
          steps:
            - step:
                id: never
                type: display
                display:
                  content: never
`
	tw := &capturingTraceWriter{}
	_, err := Start(context.Background(), Config{RunbookPath: createTempRunbook(t, runbookYAML), Client: "test", TraceWriter: tw, Output: io.Discard})
	if err == nil {
		t.Fatal("expected parse-gate error")
	}
	if !strings.Contains(err.Error(), "late_bad") || !strings.Contains(err.Error(), "GXL-PARSE-007") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tw.events) != 0 {
		t.Fatalf("expected no runtime events before validation failure, got %d", len(tw.events))
	}
}

type capturingTraceWriter struct{ events []trace.TraceEvent }

func (w *capturingTraceWriter) Append(ev trace.TraceEvent) error {
	w.events = append(w.events, ev)
	return nil
}
func (w *capturingTraceWriter) Close() error { return nil }

// TestStart_MissingRunbook tests error handling when no runbook is provided.
func TestStart_MissingRunbook(t *testing.T) {
	cfg := Config{
		Client: "test",
	}

	ctx := context.Background()
	_, err := Start(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for missing runbook, got nil")
	}

	if !strings.Contains(err.Error(), "RunbookPath") {
		t.Errorf("Expected error mentioning RunbookPath, got: %v", err)
	}
}

// TestStart_InvalidRunbook tests error handling for malformed YAML.
func TestStart_InvalidRunbook(t *testing.T) {
	runbookYAML := `
id: test
flow: [this is invalid yaml structure
`

	cfg := Config{
		RunbookPath: createTempRunbook(t, runbookYAML),
		Client:      "test",
	}

	ctx := context.Background()
	_, err := Start(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for invalid runbook, got nil")
	}
}

// TestStart_DisplayStep verifies that a display step is parsed and its spec
// is non-nil so the executor doesn't reject it.
func TestStart_DisplayStep(t *testing.T) {
	runbookYAML := `
apiVersion: yawr.runbook/v1
id: test-display
name: Test Display
flow:
  - step:
      id: show
      type: display
      title: Show something
      display:
        content: "Hello world"
        format: text
  - step:
      id: done
      type: end
      title: Done
      outcome:
        category: resolved
        code: ok
`

	cfg := Config{
		RunbookPath: createTempRunbook(t, runbookYAML),
		Client:      "test",
	}

	ctx := context.Background()
	handle, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// The display step should execute without error — if DisplaySpec is nil
	// the executor returns "invalid spec" and Next() fails.
	result, err := handle.Next(ctx)
	if err != nil && err != io.EOF {
		t.Fatalf("Next() failed (display spec likely nil): %v", err)
	}
	if result != nil && result.StepID != "show" {
		t.Errorf("expected step 'show', got %q", result.StepID)
	}
}

// createTempRunbook creates a temporary runbook file for testing.
func createTempRunbook(t *testing.T, content string) string {
	t.Helper()
	tmpFile := filepath.Join(t.TempDir(), "test-runbook.yaml")
	err := writeFile(tmpFile, []byte(content))
	if err != nil {
		t.Fatalf("Failed to create temp runbook: %v", err)
	}
	return tmpFile
}

// writeFile is a helper to write files in tests.
func writeFile(path string, data []byte) error {
	// Use os.WriteFile
	return os.WriteFile(path, data, 0644)
}

// TestStart_InputVarsSeeded verifies that declared runbook inputs are seeded into
// the variable context so that GIS expressions like ${deploy_id} don't fail when
// the input is optional and no value is provided.
func TestStart_InputVarsSeeded(t *testing.T) {
	runbookYAML := `
apiVersion: yawr.runbook/v1
id: test-inputs
name: Test Input Seeding
inputs:
  deploy_id:
    type: string
    required: false
    description: Deployment ID
    from: prompt
flow:
  - step:
      id: display_deploy
      type: display
      title: Show deploy ID
      content: "Deployment: ${deploy_id}"
`
	cfg := Config{
		RunbookPath: createTempRunbook(t, runbookYAML),
		Client:      "test",
		Output:      io.Discard,
	}

	ctx := context.Background()
	handle, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start() failed (template should not error on optional input): %v", err)
	}
	if handle == nil {
		t.Fatal("Start() returned nil handle")
	}

	// Drive the run to completion — no step should fail due to missing deploy_id.
	for {
		result, err := handle.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if result != nil && result.Error != nil && strings.Contains(result.Error.Error(), "map has no entry") {
			t.Errorf("Got template error for optional input: %s", result.Error)
		}
	}
}

// TestStart_AllExamples verifies that every example runbook can be parsed and
// started without error. This is a smoke test — it catches template variable
// errors, parse errors, and planning failures across all examples.
func TestStart_AllExamples(t *testing.T) {
	// Resolve the repo root relative to this file.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "../..")

	examples := []string{
		"examples/collect-health/collect-health.runbook.yaml",
		"examples/collect-health-parallel/collect-health-parallel.runbook.yaml",
		"examples/incident-triage/incident-triage.runbook.yaml",
		"examples/multi-region-rollout/multi-region-rollout.runbook.yaml",
		"examples/nav-test/nav-test.runbook.yaml",
		"examples/nested-chain/chain-level-1.runbook.yaml",
		"examples/service-health-branching/service-health-branching.runbook.yaml",
		"examples/simple-health-check/simple-health-check.runbook.yaml",
		"examples/edge-cases/edge-case-branch.runbook.yaml",
		"examples/edge-cases/edge-case-single-step.runbook.yaml",
	}

	for _, ex := range examples {
		ex := ex
		t.Run(filepath.Base(filepath.Dir(ex)), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(repoRoot, ex)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				t.Skipf("example not found: %s", path)
			}
			cfg := Config{
				RunbookPath: path,
				Client:      "test",
				Output:      io.Discard,
			}
			ctx := context.Background()
			handle, err := Start(ctx, cfg)
			if err != nil {
				t.Fatalf("Start() failed: %v", err)
			}
			if handle == nil {
				t.Fatal("Start() returned nil handle")
			}
		})
	}
}
