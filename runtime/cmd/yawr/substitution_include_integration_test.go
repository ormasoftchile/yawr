package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRun_Substitution_FrozenStaticIncludesExecute(t *testing.T) {
	for _, action := range []string{"run", "nested"} {
		for _, scenario := range []string{"success", "failure"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				dir := t.TempDir()
				pkgDir := filepath.Join(dir, "package")
				writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: acme.includes, version: "1.0.0"}
exports:
  tools:
    - {id: composition, path: composition.tool.yaml}
`)
				writeFile(t, filepath.Join(pkgDir, "composition.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: composition, version: "1.0.0"}
transport: {mode: native, command: must-not-dispatch}
actions:
  - name: run
    args: {scenario: {type: string, required: true}}
    outputs: {summary: {type: string}}
    execute: {kind: runbook, path: implementation.runbook.yaml}
  - name: nested
    args: {scenario: {type: string, required: true}}
    outputs: {summary: {type: string}}
    execute: {kind: runbook, path: nested.runbook.yaml}
`)
				writeFile(t, filepath.Join(pkgDir, "implementation.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: implementation
name: Static include implementation
inputs: {scenario: {type: string, required: true}}
outputs: {summary: {type: string, value: '${included_value}'}}
flow:
  - step: {id: seed, type: noop, capture: {included_value: not-run}}
  - step:
      id: include_middle
      type: include
      include: {runbook: middle.runbook.yaml}
`)
				writeFile(t, filepath.Join(pkgDir, "middle.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: middle
name: Nested include
flow:
  - step:
      id: child_branch
      type: branch
      branches:
        - condition: "true"
          steps:
            - step:
                id: include_leaf
                type: include
                include: {runbook: leaf.runbook.yaml}
`)
				writeFile(t, filepath.Join(pkgDir, "leaf.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: leaf
name: Actual child operations
flow:
  - step: {id: leaf_operation, type: noop, capture: {included_value: executed-child}}
  - step:
      id: leaf_guard
      type: assert
      assert:
        - {type: eq, subject: '${scenario}', expected: success}
`)
				writeFile(t, filepath.Join(pkgDir, "nested.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: nested
name: Nested substituted tool
inputs: {scenario: {type: string, required: true}}
outputs: {summary: {type: string, value: '${child_summary}'}}
flow:
  - step:
      id: inner_tool
      type: tool
      tool: {name: composition, action: run, args: {scenario: '${scenario}'}}
      capture: {child_summary: outputs.summary}
`)
				writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), `apiVersion: yawr.config/v1
requires:
  - {package: acme.includes, version: "^1.0.0", path: package}
`)
				writeFile(t, filepath.Join(dir, "root.runbook.yaml"), fmt.Sprintf(`apiVersion: yawr.runbook/v1
id: root
name: Public substituted include caller
toolRefs:
  - {name: composition, package: acme.includes}
flow:
  - step:
      id: public_call
      type: tool
      tool: {name: composition, action: %s, args: {scenario: %s}}
      capture: {summary: outputs.summary}
  - step:
      id: verify_child_output
      type: assert
      assert:
        - {type: eq, subject: '${summary}', expected: executed-child}
`, action, scenario))
				t.Chdir(dir)
				output := runCaptureStdoutAnyExit(t, []string{"root.runbook.yaml", "--trace", "trace.jsonl", "--output", "json"})
				wantExit, wantStatus, guardEvent := exitSuccess, "completed", "step/completed"
				if scenario == "failure" {
					wantExit, wantStatus, guardEvent = exitFailure, "failed", "step/failed"
				}
				if runLast != wantExit {
					t.Fatalf("exit=%d want=%d output=%s", runLast, wantExit, output)
				}
				var summary jsonSummary
				if err := json.Unmarshal([]byte(output), &summary); err != nil {
					t.Fatalf("summary: %v: %s", err, output)
				}
				if summary.Status != wantStatus {
					t.Fatalf("status=%s want=%s", summary.Status, wantStatus)
				}
				data, err := os.ReadFile("trace.jsonl")
				if err != nil {
					t.Fatal(err)
				}
				operationCount, guardCount := 0, 0
				prefix := "public_call/"
				if action == "nested" {
					prefix += "inner_tool/"
				}
				prefix += "include_middle/child_branch/include_leaf/"
				for _, line := range splitLines(data) {
					var event struct {
						Kind    string `json:"kind"`
						Payload struct {
							StepID string `json:"step_id"`
							NodeID string `json:"qualified_node_id"`
						} `json:"payload"`
					}
					if err := json.Unmarshal(line, &event); err != nil {
						t.Fatal(err)
					}
					if event.Kind == "step/completed" && event.Payload.StepID == "leaf_operation" {
						operationCount++
						if event.Payload.NodeID != prefix+"leaf_operation" {
							t.Fatalf("operation namespace = %q, want %q", event.Payload.NodeID, prefix+"leaf_operation")
						}
					}
					if event.Kind == guardEvent && event.Payload.StepID == "leaf_guard" {
						guardCount++
						if event.Payload.NodeID != prefix+"leaf_guard" {
							t.Fatalf("guard namespace = %q", event.Payload.NodeID)
						}
					}
					if scenario == "failure" && event.Kind == "step/started" && event.Payload.StepID == "verify_child_output" {
						t.Fatal("caller continued after child failure")
					}
				}
				if operationCount != 1 || guardCount != 1 {
					t.Fatalf("child execution counts: operation=%d guard=%d output=%s", operationCount, guardCount, output)
				}
				if summary.Steps[0].Output["summary"] != "executed-child" {
					t.Fatalf("terminal output missing even on failure: %s", output)
				}
			})
		}
	}
}
