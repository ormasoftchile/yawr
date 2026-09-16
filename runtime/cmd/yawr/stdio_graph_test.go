package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
)

func TestRunStdioExecutionGraphExamples(t *testing.T) {
	root := filepath.Join(findRepoRoot(t), "examples", "execution-graph")
	for _, name := range []string{"dynamic-router", "static-debug", "repeated-dynamic", "nested-failure", "operator-review", "static-eager", "static-lazy",
		"lexical-static-and-lazy", "lexical-parallel", "lexical-dynamic", "lexical-dynamic-parallel", "lexical-dynamic-through-tool"} {
		t.Run(name, func(t *testing.T) {
			artifacts := t.TempDir()
			fixture := name
			args := []string{"--stdio", "--require-capabilities", "yawr.run-graph/v1",
				"--run-dir", filepath.Join(artifacts, "runs"), "--trace", filepath.Join(artifacts, "trace.jsonl")}
			if name == "static-debug" {
				fixture = "static-lazy"
				args = append(args, "--debug")
			}
			scenarioRoot := root
			entrypoint := filepath.Join(root, "runbooks", fixture+".runbook.yaml")
			lexical := strings.HasPrefix(name, "lexical-")
			if lexical {
				scenarioRoot = filepath.Join(findRepoRoot(t), "examples", "dependency-scopes")
				entrypoint = filepath.Join(scenarioRoot, strings.TrimPrefix(name, "lexical-")+".runbook.yaml")
			}
			preview := y1Command(t, "preview", "--format", "graphjson", "--recurse", entrypoint)
			preview.Dir = scenarioRoot
			previewBytes, err := preview.Output()
			if err != nil {
				t.Fatal(err)
			}
			var initial graphjson.Document
			if err := json.Unmarshal(previewBytes, &initial); err != nil {
				t.Fatal(err)
			}
			cmd := y1Command(t, "run", append(args, entrypoint)...)
			cmd.Dir = scenarioRoot
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
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			timer := time.AfterFunc(20*time.Second, func() { _ = cmd.Process.Kill() })
			defer timer.Stop()
			defer stdin.Close()
			if name == "static-debug" {
				if err := json.NewEncoder(stdin).Encode(map[string]any{"type": "run.configure",
					"debug": map[string]any{"enabled": true, "breakpoints": []map[string]any{
						{"step": "child", "phase": "before"},
					}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 4096), maxStdioFrameBytes)
			var body []byte
			known := map[string]bool{}
			for _, node := range initial.Nodes {
				known[node.ID] = true
			}
			childStarts := 0
			allStarts := 0
			repeatedIDs := map[string]bool{}
			prompt := false
			childDebugPauses := 0
			finished := ""
			var retainedFrames int
			for scanner.Scan() {
				var frame map[string]json.RawMessage
				if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
					t.Fatal(err)
				}
				var kind string
				json.Unmarshal(frame["type"], &kind)
				switch kind {
				case "run.graph.chunk":
					var chunk struct {
						Data   string `json:"data"`
						Offset int    `json:"offset"`
						Total  int    `json:"totalBytes"`
						Digest string `json:"digest"`
					}
					if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
						t.Fatal(err)
					}
					if chunk.Offset == 0 {
						body = nil
					}
					if chunk.Offset != len(body) {
						t.Fatal("noncontiguous graph chunks")
					}
					part, err := base64.StdEncoding.DecodeString(chunk.Data)
					if err != nil {
						t.Fatal(err)
					}
					body = append(body, part...)
					if len(body) == chunk.Total {
						if fmt.Sprintf("sha256:%x", sha256.Sum256(body)) != chunk.Digest {
							t.Fatal("graph digest mismatch")
						}
						var update struct {
							Document graphjson.Document `json:"document"`
							NodeIDs  []string           `json:"nodeIDs"`
						}
						if err := json.Unmarshal(body, &update); err != nil {
							t.Fatal(err)
						}
						if update.Document.Runbook.ID != initial.Runbook.ID || update.Document.Runbook.Path != initial.Runbook.Path {
							t.Fatalf("execution graph changed preview root identity: preview=%#v execution=%#v", initial.Runbook, update.Document.Runbook)
						}
						next := map[string]bool{}
						for _, node := range update.Document.Nodes {
							next[node.ID] = true
						}
						for id := range known {
							if !next[id] {
								t.Fatalf("prior child %s disappeared", id)
							}
						}
						for _, id := range update.NodeIDs {
							if !next[id] {
								t.Fatalf("missing bound node %s", id)
							}
						}
						known = next
						retainedFrames = len(update.Document.Frames)
					}
				case "run.event":
					var event struct {
						Kind    string `json:"kind"`
						Payload struct {
							Node  string `json:"qualified_node_id"`
							Graph string `json:"graph_node_id"`
						} `json:"payload"`
					}
					json.Unmarshal(frame["event"], &event)
					if event.Kind == "step/started" {
						allStarts++
						if lexical && event.Payload.Graph == "" && strings.Contains(event.Payload.Node, "/") {
							childStarts++
						}
						graphID := event.Payload.Graph
						if graphID == "" {
							graphID = event.Payload.Node
						}
						if lexical && !known[graphID] {
							t.Fatalf("scoped step started without a visible graph node: %s", event.Payload.Node)
						}
					}
					if event.Kind == "step/started" && event.Payload.Graph != "" {
						childStarts++
						if strings.HasSuffix(event.Payload.Node, "/collect_evidence") {
							repeatedIDs[event.Payload.Graph] = true
						}
						if !known[event.Payload.Graph] {
							t.Fatalf("child started before its graph: %s", event.Payload.Node)
						}
					}
				case "interaction.pending":
					var pending struct {
						Node    string `json:"nodeID"`
						Turn    string `json:"turnID"`
						Kind    string `json:"kind"`
						Options []struct {
							Value string `json:"value"`
						} `json:"options"`
					}
					json.Unmarshal(frame["interaction"], &pending)
					rootDebugPause := name == "static-debug" && pending.Node == "child"
					if !known[pending.Node] {
						t.Fatalf("prompt has no exact graph node: %s", pending.Node)
					}
					if len(pending.Options) == 0 && pending.Kind != "debug_break" {
						t.Fatal("expected operator choice")
					}
					prompt = true
					var answer map[string]any
					if pending.Kind == "debug_break" {
						action := "continue"
						if rootDebugPause {
							action = "step_into"
						} else {
							childDebugPauses++
						}
						answer = map[string]any{"kind": "debug_break", "action": action}
					} else {
						answer = map[string]any{"kind": "choice", "selected": []string{pending.Options[0].Value}}
					}
					var runID string
					json.Unmarshal(frame["runID"], &runID)
					if err := json.NewEncoder(stdin).Encode(map[string]any{"type": "interaction.answer",
						"runID": runID, "turnID": pending.Turn,
						"answer": answer,
					}); err != nil {
						t.Fatal(err)
					}
				case "run.finished":
					json.Unmarshal(frame["status"], &finished)
				case "protocol.error":
					t.Fatalf("runtime rejected a protocol command: %s", scanner.Bytes())
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			waitErr := cmd.Wait()
			expected := "completed"
			if name == "nested-failure" {
				expected = "failed"
			}
			if finished != expected {
				t.Fatalf("status=%q expected=%q exit=%v stderr=%s", finished, expected, waitErr, stderr.String())
			}
			if lexical {
				expectedStarts := map[string]int{
					"lexical-static-and-lazy": 14, "lexical-parallel": 11, "lexical-dynamic": 15,
					"lexical-dynamic-parallel": 11, "lexical-dynamic-through-tool": 11,
				}[name]
				if allStarts != expectedStarts {
					t.Fatalf("scoped execution lost canonical steps: got %d, want %d", allStarts, expectedStarts)
				}
			}
			if !strings.HasPrefix(name, "static-") && (childStarts < 3 || retainedFrames < 3) {
				t.Fatalf("missing nested execution: child starts=%d runbook frames=%d", childStarts, retainedFrames)
			}
			if name == "repeated-dynamic" && (retainedFrames != 5 || len(repeatedIDs) != 2) {
				t.Fatalf("same-child invocation history was lost: frames=%d distinct collect steps=%d", retainedFrames, len(repeatedIDs))
			}

			if name == "operator-review" && !prompt {
				t.Fatal("operator interaction was bypassed")
			}
			if name == "static-debug" && childDebugPauses == 0 {
				t.Fatal("Step Into did not pause in the resolved child")
			}
		})
	}
}

func TestRunStdioExecutionGraphRedactsDecodedChunks(t *testing.T) {
	const secret = "SENTINEL-graph-secret-f09e"
	dir := makeWorkDir(t)
	runbook := writeDynamicSecretIncludeFixture(t, dir, secret)
	cmd := y1Command(t, "run", "--stdio", "--require-capabilities", "yawr.run-graph/v1",
		"--var", "child_ref=acme-dynsecret/child-secret", "--trace", filepath.Join(dir, "trace.jsonl"), runbook)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("graph run: %v; stderr=%s; stdout=%s", err, stderr.String(), output)
	}
	frames := resultsFrames(t, bytes.NewBuffer(output))
	var decoded []byte
	for _, frame := range frames {
		if string(frame["type"]) != `"run.graph.chunk"` {
			continue
		}
		var encoded string
		if err := json.Unmarshal(frame["data"], &encoded); err != nil {
			t.Fatal(err)
		}
		part, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, part...)
	}
	if len(decoded) == 0 || !bytes.Contains(decoded, []byte("copy-secret")) {
		t.Fatal("expected a graph containing the real included step")
	}
	if bytes.Contains(decoded, []byte(secret)) || bytes.Contains(output, []byte(secret)) {
		t.Fatal("declared child secret leaked through graph transport")
	}
}
