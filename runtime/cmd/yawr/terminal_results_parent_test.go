package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func terminalParentCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	if binary := os.Getenv("YAWR_TERMINAL_TEST_EXE"); binary != "" {
		return exec.Command(binary, append([]string{"run"}, args...)...)
	}
	return y1Command(t, "run", args...)
}

func terminalParentFixture(t *testing.T, category, code, gate, before string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	child := fmt.Sprintf(`apiVersion: yawr.runbook/v1
id: child
name: Child
outputs:
  result:
    type: object
    value_tree:
      status: %q
      code: %q
flow:
%s  - step:
      id: child_branch
      type: branch
      branches:
        - else: true
          steps:
            - step: {id: child_end, type: end, publish_results: true, outcome: {category: %q, code: %q}}
            - step: {id: child_tail, type: noop}
`, category, code, before, category, code)
	parent := `apiVersion: yawr.runbook/v1
id: parent
name: Parent
outputs:
  result: {type: object, value_expr: child_result}
flow:
  - step:
      id: parent_branch
      type: branch
      branches:
        - else: true
          steps:
            - step:
                id: invoke
                type: include
                capture: {child_result: outputs.result}
                include:
                  runbook: child.runbook.yaml
                  expand: eager
` + gate + `            - step:
                id: parent_end
                type: end
                publish_results: true
                outcome:
                  category: '${__run_outcome_category}'
                  code: '${__run_outcome_code}'
            - step: {id: parent_inner_tail, type: noop}
  - step: {id: parent_tail, type: noop}
`
	for name, text := range map[string]string{"child.runbook.yaml": child, "parent.runbook.yaml": parent} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "parent.runbook.yaml"), filepath.Join(dir, "runs")
}

func TestTerminalResultsParentForwarding(t *testing.T) {
	for _, category := range []string{"resolved", "escalated", "no_action", "previously-unknown-category"} {
		for _, gated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/gate-%v", category, gated), func(t *testing.T) {
				code := "Failover_Exact." + category
				gate := ""
				if gated {
					gate = "                  gate: {stop_if: [other-category]}\n"
				}
				path, runDir := terminalParentFixture(t, category, code, gate, "")
				cmd := terminalParentCommand(t, path, "--stdio", "--run-dir", runDir)
				stdin, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer stdin.Close()
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("run: %v\n%s\n%s", err, stderr.String(), stdout.String())
				}
				var finished map[string]json.RawMessage
				finishedCount, rootCompleted := 0, 0
				scanner := bufio.NewScanner(&stdout)
				scanner.Buffer(make([]byte, 65536), maxStdioFrameBytes)
				for scanner.Scan() {
					var frame map[string]json.RawMessage
					if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
						t.Fatal(err)
					}
					if string(frame["type"]) == `"run.finished"` {
						finished, finishedCount = frame, finishedCount+1
					}
					if string(frame["type"]) != `"run.event"` {
						continue
					}
					var event struct {
						Kind    string         `json:"kind"`
						Payload map[string]any `json:"payload"`
					}
					if err := json.Unmarshal(frame["event"], &event); err != nil {
						t.Fatal(err)
					}
					if event.Kind == "step/started" && strings.Contains(fmt.Sprint(event.Payload["step_id"]), "tail") {
						t.Fatalf("tail dispatched: %#v", event.Payload)
					}
					if event.Kind == "run/completed" {
						publication, _ := event.Payload["results_publication"].(map[string]any)
						origin, _ := publication["origin"].(map[string]any)
						if publication != nil && origin["frame_id"] == nil {
							rootCompleted++
							if event.Payload["outcome_category"] != category || event.Payload["outcome_code"] != code {
								t.Fatalf("root completion lost child outcome: %#v", event.Payload)
							}
						}
					}
				}
				if err := scanner.Err(); err != nil {
					t.Fatal(err)
				}
				if finishedCount != 1 || rootCompleted != 1 || string(finished["status"]) != `"completed"` {
					t.Fatalf("completion count/status: %d/%d %s", finishedCount, rootCompleted, finished["status"])
				}
				var record engine.RunResults
				if err := json.Unmarshal(finished["results"], &record); err != nil {
					t.Fatal(err)
				}
				if err := record.Validate(); err != nil {
					t.Fatal(err)
				}
				value := record.Outputs["result"].Value.(map[string]any)
				var terminalCategory, terminalCode string
				if err := json.Unmarshal(finished["outcome_category"], &terminalCategory); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(finished["outcome_code"], &terminalCode); err != nil {
					t.Fatal(err)
				}
				if terminalCategory != category || terminalCode != code ||
					value["status"] != category || value["code"] != code || record.Origin.FrameID != "" {
					t.Fatalf("parent run.finished outcome/Results mismatch: %s", mustJSON(finished))
				}
				var runID string
				if err := json.Unmarshal(finished["runID"], &runID); err != nil {
					t.Fatal(err)
				}
				store := runstore.NewDirRunStore(runDir)
				defer store.Close()
				state, err := store.LoadState(context.Background(), runID)
				if err != nil {
					t.Fatal(err)
				}
				if state.Results == nil || state.Results.Digest != record.Digest {
					t.Fatal("parent Results not durable")
				}
				childPublications := 0
				for _, frame := range state.ExecutionFrames {
					if frame.RunResults == nil {
						continue
					}
					childPublications++
					child := frame.RunResults.Outputs["result"].Value.(map[string]any)
					if child["status"] != value["status"] || child["code"] != value["code"] {
						t.Fatal("parent does not match child's committed Results")
					}
				}
				if childPublications != 1 {
					t.Fatalf("child publications=%d", childPublications)
				}
				rootPublications := 0
				for _, result := range state.StepResults {
					if result.Results != nil {
						rootPublications++
						if result.Results.Digest != record.Digest {
							t.Fatal("multiple root records")
						}
					}
				}
				if rootPublications != 1 {
					t.Fatalf("root publications=%d", rootPublications)
				}
				repeat := terminalParentCommand(t, "--resume", runID, "--output", "json", "--run-dir", runDir)
				body, err := repeat.Output()
				if err != nil {
					t.Fatal(err)
				}
				var resumed struct {
					Results *engine.RunResults `json:"results"`
				}
				if err := json.Unmarshal(body, &resumed); err != nil {
					t.Fatal(err)
				}
				if resumed.Results == nil || resumed.Results.Digest != record.Digest || resumed.Results.CheckpointSequence != record.CheckpointSequence {
					t.Fatal("completed parent resume republished")
				}
			})
		}
	}
}

func mustJSON(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		return err.Error()
	}
	return string(body)
}

func TestTerminalResultsParentGateAndFailure(t *testing.T) {
	for _, mode := range []string{"matching-gate", "failure"} {
		t.Run(mode, func(t *testing.T) {
			gate, before := "", ""
			if mode == "matching-gate" {
				gate = "                  gate: {stop_if: [escalated]}\n"
			}
			if mode == "failure" {
				before = "  - step: {id: required, type: assert, assert: [{type: eq, subject: actual, expected: different}], on_error: continue}\n"
			}
			path, runDir := terminalParentFixture(t, "escalated", "exact-child-code", gate, before)
			cmd := terminalParentCommand(t, path, "--stdio", "--run-dir", runDir)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			runErr := cmd.Run()
			if (runErr != nil) != (mode == "failure") {
				t.Fatalf("run: %v: %s", runErr, stderr.String())
			}
			frames := resultsFrames(t, &stdout)
			terminal := frames[len(frames)-1]
			if string(terminal["results"]) != "null" {
				t.Fatal("gate/failed parent published")
			}
			if mode == "matching-gate" {
				if string(terminal["status"]) != `"completed"` {
					t.Fatal("gate status changed")
				}
				if string(terminal["outcome_category"]) != `"escalated"` || string(terminal["outcome_code"]) != `"exact-child-code"` {
					t.Fatal("gate outcome changed")
				}
			} else if string(terminal["status"]) != `"failed"` {
				t.Fatal("failed child masked")
			}
			var runID string
			if err := json.Unmarshal(terminal["runID"], &runID); err != nil {
				t.Fatal(err)
			}
			store := runstore.NewDirRunStore(runDir)
			defer store.Close()
			state, err := store.LoadState(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Results != nil {
				t.Fatal("nonpublication parent has durable results")
			}
			if _, captured := state.Vars["child_result"]; mode == "matching-gate" && captured {
				t.Fatal("matching gate performed captures")
			}
			for _, frame := range state.ExecutionFrames {
				for _, result := range frame.Results {
					if (strings.Contains(result.StepID, "tail") || result.StepID == "parent_end") && result.Status == engine.StepStatusCompleted {
						t.Fatalf("gate/failure dispatched %s", result.StepID)
					}
				}
			}
		})
	}
}

func TestTerminalResultsParentCancellationAndIncomplete(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel-%v", cancel), func(t *testing.T) {
			before := "  - step: {id: choose, type: choice, prompt: Continue, variable: answer, options: [{label: Yes, value: yes}]}\n"
			path, runDir := terminalParentFixture(t, "resolved", "exact-child-code", "", before)
			cmd := terminalParentCommand(t, path, "--stdio", "--run-dir", runDir)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			timer := time.AfterFunc(20*time.Second, func() { _ = cmd.Process.Kill() })
			defer timer.Stop()
			defer stdin.Close()
			defer cmd.Process.Kill()
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 65536), maxStdioFrameBytes)
			started := readProtocolFrame(t, scanner, "run.started")
			runID := started["runID"].(string)
			readProtocolFrame(t, scanner, "interaction.pending")
			store := runstore.NewDirRunStore(runDir)
			defer store.Close()
			state, err := store.LoadState(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Results != nil {
				t.Fatal("incomplete parent published")
			}
			for _, frame := range state.ExecutionFrames {
				if frame.RunResults != nil {
					t.Fatal("incomplete child published")
				}
			}
			if !cancel {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				return
			}
			writeProtocolCommand(t, stdin, map[string]any{"type": "run.cancel", "runID": runID, "reason": "test cancellation"})
			finished := readProtocolFrame(t, scanner, "run.finished")
			if finished["results"] != nil || finished["status"] != "cancelled" {
				t.Fatalf("cancelled parent: %#v", finished)
			}
			_ = cmd.Wait()
			state, err = store.LoadState(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Results != nil {
				t.Fatal("cancelled parent persisted Results")
			}
		})
	}
}
