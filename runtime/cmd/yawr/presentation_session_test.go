package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/internal/presentationview"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

func TestPresentationSessionFrozenGraphAndReplay(t *testing.T) {
	for _, variant := range []string{"static", "dynamic", "dynamic-native"} {
		t.Run(variant, func(t *testing.T) {
			dynamic := variant != "static"
			dir := t.TempDir()
			files, _ := filepath.Glob(filepath.Join(findRepoRoot(t), "examples", "code-presentation", "*.yaml"))
			for _, file := range files {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(dir, filepath.Base(file)), string(data))
			}
			if variant == "dynamic-native" {
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "code.tool.yaml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				text := strings.ReplaceAll(string(data), "\r\n", "\n")
				text = strings.ReplaceAll(text, "transport: {mode: native, command: must-not-execute}",
					fmt.Sprintf("transport:\n  mode: native\n  command: '%s'\n  env: {YAWR_PRESENTATION_TEST_CHILD: '1'}", strings.ReplaceAll(executable, "'", "''")))
				text = strings.ReplaceAll(text, "    execute: {kind: runbook, path: echo.runbook.yaml}", "    argv: ['-test.run=^TestPresentationNativeProcess$']")
				text = strings.ReplaceAll(text, "      code:\n        type: string", "      code:\n        type: string\n        from: stdout")
				writeFile(t, path, text)
			}
			entry := filepath.Join(dir, "root.runbook.yaml")
			if dynamic {
				writeFile(t, filepath.Join(dir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: presentation-fixture, version: "1.0.0"}
exports:
  tools:
    - {id: code, path: code.tool.yaml}
  runbooks:
    - {id: child, path: root.runbook.yaml}
`)
				writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\nrequires:\n  - {package: presentation-fixture, version: '^1.0.0', path: '.'}\n")
				entry = filepath.Join(dir, "parent.runbook.yaml")
				writeFile(t, entry, `apiVersion: yawr.runbook/v1
id: dynamic-presentation
name: Dynamic presentation
flow:
  - step:
      id: child
      type: include
      include: {runbook_ref: "presentation-fixture/child", resolve_from: catalog}
`)
			}
			t.Chdir(dir)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sessionID, commandID := uuid.NewString(), uuid.NewString()
			runID := uuid.NewSHA1(uuid.MustParse(sessionID), []byte("run:"+commandID)).String()
			sessionDir, runDir := filepath.Join(dir, "sessions"), filepath.Join(dir, "runs")
			process := startSessionCLIProcess(t, ctx, []string{"start", entry,
				"--session-id", sessionID, "--command-id", commandID, "--stdio",
				"--profile", filepath.Join(dir, "profile.yaml"), "--tool-dir", dir,
				"--session-dir", sessionDir, "--run-dir", runDir})
			head := process.waitFrame(t, func(frame session.StdioFrame) bool {
				return frame.Type == session.FrameSessionSnapshot && frame.SequenceIndex == 0 && frame.SequenceCount == 1
			})
			process.send(t, session.StdioCommand{Version: session.StdioProtocolV1, Type: session.CommandSessionConfigure,
				CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: head.WriterEpoch,
				ExpectedSequence: head.SessionSequence, Payload: json.RawMessage(`{"inputs":{}}`)})
			languages := map[string]bool{}
			var segmentID string
			process.waitFrame(t, func(frame session.StdioFrame) bool {
				if frame.SegmentID != "" {
					segmentID = frame.SegmentID
				}
				if frame.Type == session.FrameRunEvent {
					assertPresentationFrame(t, frame, languages)
				}
				return frame.Type == session.FrameAttemptFinished
			})
			if len(languages) != 3 {
				t.Fatalf("live languages = %v", languages)
			}
			process.input.Close()
			if code := process.wait(t); code != exitSuccess {
				t.Fatalf("session exit %d: %s", code, process.stderr.String())
			}
			for _, file := range files {
				os.Remove(filepath.Join(dir, filepath.Base(file)))
			}
			if dynamic {
				os.Remove(entry)
				// Today's catalog cannot supply any old descriptor.
				writeFile(t, filepath.Join(dir, "yawr-package.yaml"), "apiVersion: yawr.tool-package/v1\nmeta: {name: changed, version: '9.0.0'}\n")
			}
			var graphOut, stderr bytes.Buffer
			revision := "1"
			if dynamic {
				revision = "2"
			}
			code := sessionMain(ctx, []string{"graph", sessionID, "--segment-id", segmentID,
				"--revision", revision, "--session-dir", sessionDir}, io.NopCloser(strings.NewReader("")), &graphOut, &stderr)
			if code != exitSuccess {
				t.Fatalf("graph: %s", stderr.String())
			}
			var response struct {
				Data      []byte `json:"data"`
				GraphHash string `json:"graph_hash"`
			}
			if err := json.Unmarshal(graphOut.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if session.DigestBytes(response.Data) != response.GraphHash {
				t.Fatal("graph revision hash mismatch")
			}
			var graph struct {
				graphjson.Document
				ExecutionPlanHash string `json:"execution_plan_hash"`
			}
			if err := json.Unmarshal(response.Data, &graph); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, node := range graph.Nodes {
				data, _ := json.Marshal(node.Data["details"])
				var details struct {
					Presentation *presentation.Envelope `json:"code_presentation"`
				}
				json.Unmarshal(data, &details)
				if e := details.Presentation; e != nil {
					count++
					if e.Origin != "frozen" || e.PlanSnapshotDigest != graph.ExecutionPlanHash || e.ToolID == "" {
						t.Fatalf("invalid graph binding: %+v", e)
					}
				}
			}
			if count != 3 {
				t.Fatalf("frozen graph metadata nodes = %d", count)
			}
			store := runstore.NewDirRunStore(runDir)
			if dynamic {
				plan, err := store.LoadPlan(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				if len(plan.Tools) != 0 {
					t.Fatalf("dynamic fixture unexpectedly has root tools: %v", plan.Tools)
				}
				state, err := store.LoadState(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				for _, resolution := range state.DynamicIncludes {
					tools, err := plansnapshot.RestoreFlowTools(resolution.Pin.ExecutableClosure)
					if err != nil || !plansnapshot.HasPresentation(tools) || resolution.Pin.Revision < 1 {
						t.Fatalf("committed dynamic descriptors missing: %v", err)
					}
				}
			}
			doc, err := presentationview.Inspect(ctx, store, runID)
			store.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.PresentationState.Occurrences) < 3 {
				t.Fatal("frozen history lost child captures")
			}
			var replay, replayErrors bytes.Buffer
			if code := sessionMain(ctx, []string{"attach", sessionID, "--stdio", "--session-dir", sessionDir, "--run-dir", runDir, "--tool-dir", dir},
				io.NopCloser(strings.NewReader("")), &replay, &replayErrors); code != exitSuccess {
				t.Fatalf("replay: %s", replayErrors.String())
			}
			languages = map[string]bool{}
			decoder := json.NewDecoder(&replay)
			for {
				var frame session.StdioFrame
				if decoder.Decode(&frame) != nil {
					break
				}
				if frame.Type == session.FrameRunEvent {
					assertPresentationFrame(t, frame, languages)
				}
			}
			if len(languages) != 3 {
				t.Fatalf("replayed languages = %v", languages)
			}
		})
	}
}

func assertPresentationFrame(t *testing.T, frame session.StdioFrame, languages map[string]bool) {
	t.Helper()
	var event engine.Event
	if json.Unmarshal(frame.Payload, &event) != nil || event.Kind != "step/completed" {
		return
	}
	var e presentation.Envelope
	data, _ := json.Marshal(event.Payload["code_presentation"])
	if json.Unmarshal(data, &e) != nil || e.ToolID == "" {
		return
	}
	output, _ := event.Payload["output"].(map[string]any)
	status, _ := event.Payload["output_value_status"].(map[string]any)
	for _, field := range e.Outputs {
		if field.Name != "code" || field.Presentation == nil {
			continue
		}
		if status[field.Name] != "available" {
			t.Fatalf("live code not approved: %v", status)
		}
		if _, ok := output[field.Name].(string); !ok {
			t.Fatal("available without actual stdio string")
		}
		if e.Origin != "frozen" || e.PlanSnapshotDigest == "" {
			t.Fatal("unfrozen live metadata")
		}
		languages[field.Presentation.Language] = true
	}
}
