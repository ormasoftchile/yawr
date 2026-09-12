package replay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadScenario_Valid(t *testing.T) {
	dir := makeScenarioDir(t)
	path := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(path, []byte(sampleScenarioYAML()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	scenario, err := LoadScenario(path)
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if len(scenario.Commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(scenario.Commands))
	}
	if !scenario.AllowUnmatched {
		t.Fatalf("expected allow_unmatched true")
	}
	if scenario.Tools["tool/action"].Response != "{\"ok\":true}" {
		t.Fatalf("unexpected tool response: %#v", scenario.Tools)
	}
}

func TestLoadScenario_Invalid(t *testing.T) {
	dir := makeScenarioDir(t)
	path := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(path, []byte("bad: ["), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadScenario(path); err == nil {
		t.Fatalf("expected error")
	}
}

func TestLoadScenarioRejectsExactHostFixtureWithoutCapability(t *testing.T) {
	dir := makeScenarioDir(t)
	path := filepath.Join(dir, "scenario.yaml")
	data := `source_run_id: source-run
host_action_responses:
	- at:
			qualified_node_id: open
			step: open
			phase: execute
			invocation: 1
			attempt: 1
		response:
			status: completed
			result: { ok: true }
		source:
			kind: prior-run
			run_id: source-run
		review:
			state: reviewed
			reviewed_by: reviewer-id
			reviewed_at: "2026-09-01T00:00:00Z"
			sensitivity_reviewed: true
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadScenario(path); err == nil {
		t.Fatal("LoadScenario accepted an exact host fixture without capability")
	}
}

func sampleScenarioYAML() string {
	return `commands:
  - argv: ["echo", "hi"]
    stdout: "hi\n"
    stderr: ""
    exit_code: 0
evidence:
  step-1:
    note:
      kind: text
      value: "ok"
tools:
  tool/action:
    response: "{\"ok\":true}"
    exit_code: 0
allow_unmatched: true
`
}

func makeScenarioDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "replay-scenario-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
