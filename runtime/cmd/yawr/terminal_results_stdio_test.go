package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestTerminalResultsStdioChoiceRestart(t *testing.T) {
	testTerminalResultsStdioChoiceRestart(t, false)
}

func TestTerminalResultsParentChoiceRestart(t *testing.T) {
	testTerminalResultsStdioChoiceRestart(t, true)
}

func testTerminalResultsStdioChoiceRestart(t *testing.T, parent bool) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "runs")
	path := filepath.Join(dir, "terminal.runbook.yaml")
	source := `apiVersion: yawr.runbook/v1
id: terminal-stdio
name: Terminal stdio
outputs:
  selected: {type: string, value_expr: selected_route}
flow:
  - step:
      id: outer
      type: branch
      branches:
        - else: true
          steps:
            - step:
                id: inner
                type: branch
                branches:
                  - else: true
                    steps:
                      - step:
                          id: choose
                          type: choice
                          prompt: Choose a route
                          variable: selected_route
                          options:
                            - {value: primary, label: Primary}
                            - {value: fallback, label: Fallback}
                      - step: {id: end, type: end, publish_results: true, outcome: {category: no_action, code: GeoDR-Exact_Code}}
                      - step: {id: unreachable_inner, type: noop}
            - step: {id: unreachable_outer, type: noop}
  - step: {id: unreachable_root, type: noop}
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if parent {
		path, runDir = terminalParentFixture(t, "no_action", "GeoDR-Exact_Code", "",
			"  - step: {id: choose, type: choice, prompt: Choose, variable: selected_route, options: [{label: Primary, value: primary}, {label: Fallback, value: fallback}]}\n")
	}
	start := func(args ...string) (*exec.Cmd, io.WriteCloser, <-chan map[string]any, *bytes.Buffer) {
		t.Helper()
		args = append(args, "--stdio", "--run-dir", runDir, "--require-capabilities", "yawr.terminal-results/v1")
		cmd := y1Command(t, "run", args...)
		if binary := os.Getenv("YAWR_TERMINAL_TEST_EXE"); binary != "" {
			cmd = exec.Command(binary, append([]string{"run"}, args...)...)
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr := &bytes.Buffer{}
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		timer := time.AfterFunc(30*time.Second, func() { _ = cmd.Process.Kill() })
		t.Cleanup(func() { timer.Stop(); _ = stdin.Close(); _ = cmd.Process.Kill() })
		frames := make(chan map[string]any, 256)
		go func() {
			defer close(frames)
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 65536), maxStdioFrameBytes)
			for scanner.Scan() {
				var frame map[string]any
				if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
					return
				}
				frames <- frame
			}
		}()
		return cmd, stdin, frames, stderr
	}
	first, _, frames, _ := start(path)
	started := waitForProtocolType(t, frames, "run.started")
	runID := started["runID"].(string)
	pending := waitForProtocolType(t, frames, "interaction.pending")
	turnID := pending["interaction"].(map[string]any)["turnID"].(string)
	store := runstore.NewDirRunStore(runDir)
	defer store.Close()
	waiting, err := store.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Results != nil {
		t.Fatal("published before answering choice")
	}
	for _, frame := range waiting.ExecutionFrames {
		if frame.RunResults != nil {
			t.Fatal("structural frame published while waiting")
		}
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	for range frames {
	}

	second, stdin, frames, stderr := start("--resume", runID)
	pending = waitForProtocolType(t, frames, "interaction.pending")
	interaction := pending["interaction"].(map[string]any)
	if interaction["turnID"] != turnID {
		t.Fatal("resume regenerated the pending choice")
	}
	options := interaction["options"].([]any)
	selected := options[1].(map[string]any)["value"].(string)
	writeProtocolCommand(t, stdin, map[string]any{"type": "interaction.answer", "runID": runID, "turnID": turnID,
		"answer": map[string]any{"kind": "choice", "selected": []string{selected}}})
	finishedCount, completedCount := 0, 0
	var published engine.RunResults
	for frame := range frames {
		if frame["version"] != stdioProtocolVersion {
			t.Fatal("wrong stdio version")
		}
		if frame["type"] == "run.event" {
			event := frame["event"].(map[string]any)
			payload, _ := event["payload"].(map[string]any)
			if event["kind"] == "step/started" {
				if id, _ := payload["step_id"].(string); strings.Contains(id, "unreachable") || strings.Contains(id, "tail") {
					t.Fatalf("terminal tail dispatched: %s", id)
				}
			}
			publication, _ := payload["results_publication"].(map[string]any)
			origin, _ := publication["origin"].(map[string]any)
			if event["kind"] == "run/completed" && publication != nil && origin["frame_id"] == nil {
				completedCount++
				if payload["outcome_category"] != "no_action" || payload["outcome_code"] != "GeoDR-Exact_Code" {
					t.Fatalf("terminal outcome changed: %#v", payload)
				}
			}
		}
		if frame["type"] == "run.finished" {
			finishedCount++
			if frame["status"] != "completed" || frame["results_unavailable"] != nil {
				t.Fatalf("invalid terminal: %#v", frame)
			}
			if frame["outcome_category"] != "no_action" || frame["outcome_code"] != "GeoDR-Exact_Code" {
				t.Fatalf("run.finished lost exact terminal outcome: %#v", frame)
			}
			body, err := json.Marshal(frame["results"])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &published); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("resume: %v: %s", err, stderr.String())
	}
	if finishedCount != 1 || completedCount != 1 || published.Validate() != nil {
		t.Fatalf("publication/terminal counts=%d/%d record=%#v", finishedCount, completedCount, published)
	}
	if parent {
		result := published.Outputs["result"].Value.(map[string]any)
		if result["status"] != "no_action" || result["code"] != "GeoDR-Exact_Code" {
			t.Fatal("parent did not forward child Results")
		}
	} else if published.Outputs["selected"].Value != "fallback" {
		t.Fatal("choice value changed")
	}
	durable, err := store.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Results == nil || durable.Results.Digest != published.Digest {
		t.Fatal("stdio is not the durable record")
	}
	childPublications := 0
	for _, frame := range durable.ExecutionFrames {
		if frame.RunResults != nil {
			childPublications++
			if !parent || frame.Kind != "include" {
				t.Fatal("structural frame produced an extra record")
			}
		}
	}
	if parent && childPublications != 1 {
		t.Fatalf("child publications = %d", childPublications)
	}
	for attempt := 0; attempt < 2; attempt++ {
		cmd, _, frames, stderr := start("--resume", runID)
		count := 0
		for frame := range frames {
			if frame["type"] != "run.finished" {
				continue
			}
			count++
			if frame["outcome_category"] != "no_action" || frame["outcome_code"] != "GeoDR-Exact_Code" {
				t.Fatal("completed resume lost terminal outcome")
			}
			body, _ := json.Marshal(frame["results"])
			var record engine.RunResults
			if err := json.Unmarshal(body, &record); err != nil {
				t.Fatal(err)
			}
			if record.Digest != published.Digest || record.PublicationID != published.PublicationID ||
				record.CheckpointSequence != published.CheckpointSequence {
				t.Fatal("completed resume regenerated publication")
			}
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("completed resume: %v: %s", err, stderr.String())
		}
		if count != 1 {
			t.Fatalf("terminal count = %d", count)
		}
	}
}
