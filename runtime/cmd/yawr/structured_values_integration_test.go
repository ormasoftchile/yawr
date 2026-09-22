package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func copyStructuredValuesExample(t *testing.T) string {
	t.Helper()
	source := filepath.Join(findRepoRoot(t), "examples", "structured-values")
	dir := t.TempDir()
	entries, err := os.ReadDir(source)
	if err != nil || len(entries) == 0 {
		t.Fatalf("example files: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, entry.Name()), string(data))
	}
	return dir
}

func TestRun_Substitution_StructuredCollectionAndExports(t *testing.T) {
	dir := copyStructuredValuesExample(t)
	artifacts := t.TempDir()
	t.Chdir(dir)
	output := runCaptureStdout(t, []string{
		"root.yawr", "--package-map", "package-map.yaml", "--profile", "profile.yaml",
		"--trace", filepath.Join(artifacts, "trace.jsonl"), "--run-dir", filepath.Join(artifacts, "runs"), "--output", "json",
	})
	var summary jsonSummary
	if err := json.Unmarshal([]byte(output), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "completed" || len(summary.Steps) != 2 {
		t.Fatalf("example did not complete: %s", output)
	}
	records, ok := summary.Steps[0].Output["records"].([]any)
	if !ok || len(records) != 4 {
		t.Fatalf("public output is not a typed four-item array: %s", output)
	}
	if _, err := os.Stat(filepath.Join(dir, ".runbook")); !os.IsNotExist(err) {
		t.Fatalf("isolated run wrote to the caller's default store: %v", err)
	}
	runs, err := os.ReadDir(filepath.Join(artifacts, "runs"))
	if err != nil || len(runs) == 0 {
		t.Fatalf("explicit durable run store was not written: %v", err)
	}
	orderingOutput := runCaptureStdout(t, []string{
		"ordering-root.yawr", "--package-map", "package-map.yaml", "--profile", "profile.yaml",
		"--trace", "ordering-trace.jsonl", "--output", "json",
	})
	if err := json.Unmarshal([]byte(orderingOutput), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "completed" || len(summary.Steps) != 6 {
		t.Fatalf("ordering example did not complete: %s", orderingOutput)
	}
}

func TestRun_StructuredDirectNestedParity(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "observed-no-data-missing", true: "failed"}[failed], func(t *testing.T) {
			dir := copyStructuredValuesExample(t)
			t.Chdir(dir)
			relay, err := os.ReadFile("relay.yawr")
			if err != nil {
				t.Fatal(err)
			}
			if failed {
				relay = []byte(strings.ReplaceAll(string(relay), "{id: beta, count: 1}", "{id: failed, count: 0}"))
				writeFile(t, "relay.yawr", string(relay))
			}
			// Invoke the identical gather action directly and through relay.
			direct := strings.Replace(string(relay), "id: relay", "id: direct", 1)
			writeFile(t, "direct.yawr", direct)
			var outputs []map[string]any
			for _, path := range []string{"direct.yawr", "root.yawr"} {
				output := runCaptureStdoutAnyExit(t, []string{
					path, "--package-map", "package-map.yaml", "--profile", "profile.yaml", "--output", "json",
				})
				var summary jsonSummary
				if err := json.Unmarshal([]byte(output), &summary); err != nil {
					t.Fatal(err)
				}
				want := "completed"
				if failed {
					want = "failed"
				}
				if summary.Status != want || len(summary.Steps) == 0 {
					t.Fatalf("%s: %s", path, output)
				}
				outputs = append(outputs, summary.Steps[0].Output)
			}
			if !reflect.DeepEqual(outputs[0], outputs[1]) {
				t.Fatalf("direct/nested differ: %#v / %#v", outputs[0], outputs[1])
			}
			records := outputs[0]["records"].([]any)
			if failed {
				record := records[1].(map[string]any)
				if record["status"] != "failed" || len(record["rows"].([]any)) != 0 {
					t.Fatalf("failure association lost: %#v", record)
				}
			}
			if record := records[2].(map[string]any); record["status"] != "missing" || len(record["rows"].([]any)) != 0 {
				t.Fatalf("skipped child inherited prior rows: %#v", record)
			}
		})
	}
}

func TestRun_Substitution_MaterializationFailureReturnsError(t *testing.T) {
	for _, expression := range []string{"observations +", `list.order(observations, "left +")`} {
		t.Run(expression, func(t *testing.T) {
			dir := copyStructuredValuesExample(t)
			writeFile(t, filepath.Join(dir, "order.yawr"), `apiVersion: yawr.runbook/v1
id: invalid-order
name: Invalid expression reached while freezing the package
inputs:
  observations: {type: array, required: true}
outputs:
  ordering: {type: object, value_expr: '`+expression+`'}
flow:
  - step: {id: never, type: noop}
`)
			t.Chdir(dir)
			stderr := captureStderr(t, func() int {
				return runRun([]string{
					"root.yawr", "--package-map", "package-map.yaml", "--profile", "profile.yaml",
					"--trace", "trace.jsonl", "--output", "quiet",
				})
			})
			if runLast != exitValidation || !strings.Contains(stderr, "output/value-expr") ||
				!strings.Contains(stderr, "GXL-PARSE-001") {
				t.Fatalf("materialization failure: exit=%d stderr=%s", runLast, stderr)
			}
			assertNoStructuredDispatch(t)
		})
	}
}

func TestRun_StructuredInvalidAuthoredValuesRejectBeforeDispatch(t *testing.T) {
	for _, value := range []string{
		`'${rows +}'`,
		`{status: observed, nested: {rows: '${rows +}'}}`,
		`[1, {rows: '${rows +}'}]`,
		`'${list.order(items, "left +")}'`,
	} {
		t.Run(value, func(t *testing.T) {
			dir := copyStructuredValuesExample(t)
			writeFile(t, filepath.Join(dir, "gather.yawr"), `apiVersion: yawr.runbook/v1
id: gather
name: Reject before any child work
toolRefs:
  - {name: records, package: acme.structured-values}
inputs: {configurations: {type: array, required: true}}
outputs:
  records: {type: array, value_expr: records}
  labels: {type: array, value_expr: labels}
flow:
  - iterate:
      id: invalid
      over: configurations
      steps:
        - step:
            id: must_not_dispatch
            type: tool
            tool: {name: records, action: rows, args: {name: probe, count: "1"}}
      collect_values:
        records: `+value+`
        labels: label
`)
			t.Chdir(dir)
			stderr := captureStderr(t, func() int {
				return runRun([]string{
					"root.yawr", "--package-map", "package-map.yaml", "--profile", "profile.yaml",
					"--trace", "trace.jsonl", "--output", "quiet",
				})
			})
			if runLast != exitValidation || !strings.Contains(stderr, "collect_values") {
				t.Fatalf("expected collection plan error, exit=%d stderr=%s", runLast, stderr)
			}
			assertNoStructuredDispatch(t)
		})
	}
}

func assertNoStructuredDispatch(t *testing.T) {
	t.Helper()
	trace, err := os.ReadFile("trace.jsonl")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, line := range splitLines(trace) {
		var event struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == "step/started" {
			t.Fatalf("invalid plan executed steps: %s", trace)
		}
	}
}
