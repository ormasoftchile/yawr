package run

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestStart_Enum008_CallerInputRejected covers ENUM-008 (S3): a
// caller-supplied (--var) value bound to an enum-constrained runbook
// input must be a declared member; Start must fail before the engine
// begins execution.
func TestStart_Enum008_CallerInputRejected(t *testing.T) {
	runbookYAML := `
apiVersion: yawr.runbook/v1
id: test-enum-input
name: Test Enum Input
inputs:
  env_name:
    type: string
    required: true
    enum: ["dev", "staging"]
flow:
  - step:
      id: display_env
      type: display
      title: Show env
      content: "Env: ${env_name}"
`
	cfg := Config{
		RunbookPath: createTempRunbook(t, runbookYAML),
		Client:      "test",
		Output:      io.Discard,
		Variables:   map[string]string{"env_name": "prod"},
	}

	_, err := Start(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected ENUM-008 error for non-member caller-supplied input")
	}
	if !strings.Contains(err.Error(), "ENUM-008") {
		t.Fatalf("expected ENUM-008 in error, got: %v", err)
	}
}

// TestStartWithWarnings_EnumW001Visible covers AR-CE-4 §6 (T-RUN-WARNINGS):
// a non-fatal ENUM-W001 (case-only-distinct enum members) must reach
// Result.Warnings from StartWithWarnings without blocking the run, and
// Start (the pre-existing signature) must remain unaffected.
func TestStartWithWarnings_EnumW001Visible(t *testing.T) {
	runbookYAML := `
apiVersion: yawr.runbook/v1
id: test-enum-w001
name: Test Enum W001
inputs:
  env_name:
    type: string
    required: true
    enum: ["Prod", "prod"]
flow:
  - step:
      id: display_env
      type: display
      title: Show env
      content: "Env: ${env_name}"
`
	cfg := Config{
		RunbookPath: createTempRunbook(t, runbookYAML),
		Client:      "test",
		Output:      io.Discard,
		Variables:   map[string]string{"env_name": "prod"},
	}

	res, err := StartWithWarnings(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartWithWarnings(): non-fatal warning must not block the run, got: %v", err)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "ENUM-W001") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ENUM-W001 warning in Result.Warnings, got: %+v", res.Warnings)
	}

	ctx := context.Background()
	for {
		_, nerr := res.Handle.Next(ctx)
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			t.Fatalf("Next(): %v", nerr)
		}
	}
}

// TestStart_Enum008_CallerInputAccepted is the corresponding happy path.
func TestStart_Enum008_CallerInputAccepted(t *testing.T) {
	runbookYAML := `
apiVersion: yawr.runbook/v1
id: test-enum-input-ok
name: Test Enum Input OK
inputs:
  env_name:
    type: string
    required: true
    enum: ["dev", "staging"]
flow:
  - step:
      id: display_env
      type: display
      title: Show env
      content: "Env: ${env_name}"
`
	cfg := Config{
		RunbookPath: createTempRunbook(t, runbookYAML),
		Client:      "test",
		Output:      io.Discard,
		Variables:   map[string]string{"env_name": "dev"},
	}

	handle, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	ctx := context.Background()
	for {
		_, nerr := handle.Next(ctx)
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			t.Fatalf("Next(): %v", nerr)
		}
	}
}
