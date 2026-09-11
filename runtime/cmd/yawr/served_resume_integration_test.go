package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

// Run the real CLI server in a separate process so abrupt restart exercises
// writer leases, on-disk recovery, and HTTP wiring rather than engine mocks.
func TestServedResumeProcess(t *testing.T) {
	if os.Getenv("YAWR_SERVED_RESUME_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(runServe(os.Args[i+1:]))
		}
	}
	os.Exit(2)
}

type resumeTestRPC struct {
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func resumeTestRequest(base, method, path string, value any) (int, []byte, error) {
	var body io.Reader
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, base+path, body)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func resumeTestCall(base, method, runID string) (resumeTestRPC, error) {
	status, data, err := resumeTestRequest(base, http.MethodPost, "/rpc", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method,
		"params": map[string]any{"runID": runID, "actor": "synthetic-http-resume"},
	})
	var result resumeTestRPC
	if err != nil {
		return result, err
	}
	if status != http.StatusOK {
		return result, fmt.Errorf("RPC status %d: %s", status, data)
	}
	err = json.Unmarshal(data, &result)
	return result, err
}

func startResumeTestServer(t *testing.T, dir string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestServedResumeProcess$", "--",
		"--addr", addr, "--run-dir", "runs", "--package-map", "probe.package-map.yaml")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "YAWR_SERVED_RESUME_PROCESS=1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if t.Failed() {
				t.Logf("serve PID %d output:\n%s", cmd.Process.Pid, output.String())
			}
		})
	}
	t.Cleanup(stop)
	base := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, _, err := resumeTestRequest(base, http.MethodGet, "/health", nil)
		if err == nil && status == http.StatusOK {
			t.Logf("healthy real serve process PID %d at %s", cmd.Process.Pid, base)
			return base, stop
		}
		time.Sleep(25 * time.Millisecond)
	}
	stop()
	t.Fatalf("server not healthy: %s", output.String())
	return "", nil
}

func resumeTestStream(t *testing.T, base, runID string) (<-chan map[string]any, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/runs/"+runID+"/interactions", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	// No Last-Event-ID: a restarted broker has a new queue-local sequence.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("SSE status: %d", resp.StatusCode)
	}
	frames := make(chan map[string]any, 16)
	go func() {
		defer close(frames)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				var frame map[string]any
				if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &frame) == nil {
					select {
					case frames <- frame:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	t.Cleanup(cancel)
	return frames, cancel
}

func resumeTestFrame(t *testing.T, frames <-chan map[string]any, kind string) map[string]any {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok || frame["type"] != kind {
			t.Fatalf("expected %s frame, got %#v", kind, frame)
		}
		return frame
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", kind)
		return nil
	}
}

func resumeTestAnswer(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	values := map[string]any{}
	for _, field := range frame["fields"].([]any) {
		f := field.(map[string]any)
		switch f["label"] {
		case "Status":
			values[f["name"].(string)] = "blocked"
		case "Confirmed":
			values[f["name"].(string)] = false
		case "Count":
			values[f["name"].(string)] = 0
		}
	}
	if len(values) != 3 {
		t.Fatalf("unexpected field tokens: %#v", frame)
	}
	return map[string]any{"kind": "collector", "values": values}
}

func TestServedResumeNestedCollectorPublicHTTP(t *testing.T) {
	dir := t.TempDir()
	fixtures := map[string]string{
		"probe.package-map.yaml": "apiVersion: yawr.config/v1\ntool-paths: [ . ]\n",
		"restart-probe.tool.yaml": `apiVersion: yawr.tool/v1
meta: {name: restart-probe, version: "1.0.0", description: Safe synthetic nested collector}
transport: {mode: native, command: does-not-run}
actions:
  - name: outer
    classification: read-only
    outputs: {answer: {type: object}}
    execute: {kind: runbook, path: outer.runbook.yaml}
  - name: inner
    classification: read-only
    outputs: {answer: {type: object}}
    execute: {kind: runbook, path: inner.runbook.yaml}
`,
		"public.runbook.yaml": `apiVersion: yawr.runbook/v1
id: public-restart
name: Safe nested collector restart probe
toolRefs: [{name: restart-probe}]
flow:
  - step:
      id: public_call
      type: tool
      tool: {name: restart-probe, action: outer}
      capture: {answer: outputs.answer}
  - step:
      id: verify_answer
      type: assert
      assert:
        - {type: eq, subject: '${answer.status == "blocked" and answer.confirmed == false and answer.count == 0}', expected: "true"}
`,
		"outer.runbook.yaml": `apiVersion: yawr.runbook/v1
id: outer-restart
name: Outer collector tool
kind: composable
toolRefs: [{name: restart-probe}]
outputs:
  answer: {type: object, value_expr: answer}
flow:
  - step:
      id: inner_call
      type: tool
      tool: {name: restart-probe, action: inner}
      capture: {answer: outputs.answer}
`,
		"inner.runbook.yaml": `apiVersion: yawr.runbook/v1
id: inner-restart
name: Inner collector tool
kind: composable
outputs:
  answer: {type: object, value_expr: 'answers[0]'}
flow:
  - step:
      id: inspect
      type: collector
      prompt: Submit synthetic operator evidence
      fields:
        - {name: status, type: text, label: Status, required: true}
        - {name: confirmed, type: boolean, label: Confirmed, required: true}
        - {name: count, type: number, label: Count, required: true}
  - iterate:
      id: pack
      over: once
      as: unused
      steps:
        - step: {id: record, type: noop}
      collect_values:
        answers: {status: '${status}', confirmed: '${confirmed}', count: '${count}'}
`,
	}
	for name, data := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first, stopFirst := startResumeTestServer(t, dir)
	create := func(base string) string {
		t.Helper()
		status, data, err := resumeTestRequest(base, http.MethodPost, "/runs", map[string]any{
			"runbookPath": "public.runbook.yaml", "mode": "real", "actor": "synthetic-http-resume",
		})
		if err != nil || status != http.StatusCreated {
			t.Fatalf("create: %d %s %v", status, data, err)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result["runID"].(string)
	}
	submit := func(base, runID, turnID string, body any, want int) {
		t.Helper()
		status, data, err := resumeTestRequest(base, http.MethodPost, "/runs/"+runID+"/interactions/"+turnID, body)
		if err != nil || status != want {
			t.Fatalf("answer: want %d got %d %s %v", want, status, data, err)
		}
	}
	store := runstore.NewDirRunStore(filepath.Join(dir, "runs"))
	completed := func(runID string) engine.RunState {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			state, err := store.LoadState(context.Background(), runID)
			if err == nil && state.Status == engine.RunStatusCompleted {
				answer, ok := state.Vars["answer"].(map[string]any)
				if !ok || answer["status"] != "blocked" || answer["confirmed"] != false || answer["count"] != json.Number("0") {
					t.Fatalf("typed public answer: %#v", state.Vars)
				}
				if state.CurrentStepIndex != 2 || len(state.StepResults) != 2 {
					t.Fatalf("caller completion: %#v", state.StepResults)
				}
				for _, result := range state.StepResults {
					if result.Status != engine.StepStatusCompleted || result.Outcome != engine.StepOutcomeSuccess {
						t.Fatalf("caller assertion failed: %#v", result)
					}
				}
				return state
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("run never completed")
		return engine.RunState{}
	}

	// Fresh-run control proves the exact fixture and token encoding.
	control := create(first)
	controlFrames, closeControl := resumeTestStream(t, first, control)
	controlPending := resumeTestFrame(t, controlFrames, "pending")
	submit(first, control, controlPending["turnID"].(string), resumeTestAnswer(t, controlPending), 204)
	resumeTestFrame(t, controlFrames, "resolved")
	completed(control)
	closeControl()

	runID := create(first)
	initialFrames, closeInitial := resumeTestStream(t, first, runID)
	pending := resumeTestFrame(t, initialFrames, "pending")
	turnID := pending["turnID"].(string)
	before, err := store.LoadState(context.Background(), runID)
	if err != nil || before.Interactions[turnID] == nil || len(before.StepResults) != 0 || before.Vars["answer"] != nil {
		t.Fatalf("pending not durable or fabricated pre-answer result: %#v %v", before, err)
	}
	second, stopSecond := startResumeTestServer(t, dir)
	for attempt := 0; attempt < 2; attempt++ {
		missing, err := resumeTestCall(second, "run.resume", "missing-run")
		if err != nil || missing.Error == nil || missing.Error.Code != -32010 {
			t.Fatalf("failed resume did not clean up for retry: error=%+v transport=%v", missing.Error, err)
		}
	}
	failedLease, err := store.AcquireRunLease(context.Background(), "missing-run")
	if err != nil {
		t.Fatalf("failed resume retained writer lease: %v", err)
	}
	if err := failedLease.Release(); err != nil {
		t.Fatal(err)
	}
	status, _, err := resumeTestRequest(second, http.MethodGet, "/runs/"+runID+"/interactions", nil)
	if err != nil || status != 404 {
		t.Fatalf("GET must not autoattach: %d %v", status, err)
	}
	locked, err := resumeTestCall(second, "run.resume", runID)
	if err != nil || locked.Error == nil || locked.Error.Code != -32013 {
		t.Fatalf("active writer not rejected: %#v %v", locked, err)
	}
	// The failed second-process attach must not harm the live first queue.
	if duplicate, err := resumeTestCall(first, "run.resume", runID); err != nil || duplicate.Error == nil || duplicate.Error.Code != -32013 {
		t.Fatalf("double attach not rejected: %#v %v", duplicate, err)
	}
	closeInitial()
	stopFirst()
	resumed, err := resumeTestCall(second, "run.resume", runID)
	if err != nil || resumed.Error != nil || resumed.Result["runID"] != runID {
		t.Fatalf("resume after crash: %#v %v", resumed, err)
	}
	frames, closeStream := resumeTestStream(t, second, runID)
	defer closeStream()
	// RPC resume is step-driven; Next blocks inside the pending collector.
	nextDone := make(chan resumeTestRPC, 1)
	nextErr := make(chan error, 1)
	go func() {
		result, err := resumeTestCall(second, "run.next", runID)
		if err != nil {
			nextErr <- err
		} else {
			nextDone <- result
		}
	}()
	var regenerated map[string]any
	select {
	case regenerated = <-frames:
		if regenerated == nil || regenerated["type"] != "pending" {
			state, loadErr := store.LoadState(context.Background(), runID)
			t.Fatalf("resume stream closed: status=%s results=%#v load=%v", state.Status, state.StepResults, loadErr)
		}
	case err := <-nextErr:
		t.Fatal(err)
	case result := <-nextDone:
		state, _ := store.LoadState(context.Background(), runID)
		for id, step := range state.StepResults {
			t.Logf("failed step %s: %+v", id, step)
		}
		trace, _ := os.ReadFile(filepath.Join(dir, "runs", runID, "trace.jsonl"))
		t.Logf("resume trace: %s", trace)
		t.Fatalf("Next returned without pending: %#v error=%+v", result.Result, result.Error)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out regenerating pending")
	}
	if regenerated["turnID"] != turnID || regenerated["runID"] != runID || regenerated["nodeID"] != "public_call/inner_call/inspect" {
		t.Fatalf("pending identity changed: %#v", regenerated)
	}
	waiting, err := store.LoadState(context.Background(), runID)
	if err != nil || waiting.WriterEpoch <= before.WriterEpoch || len(waiting.StepResults) != 0 || waiting.Vars["answer"] != nil {
		t.Fatalf("resume fencing/pre-answer invariant failed: %#v %v", waiting, err)
	}
	select {
	case result := <-nextDone:
		t.Fatalf("Next completed before a real answer: %#v", result)
	case err := <-nextErr:
		t.Fatal(err)
	default:
	}
	answer := resumeTestAnswer(t, regenerated)
	submit(second, "unknown-run", turnID, answer, 404)
	submit(second, runID, "stale-turn", answer, 409)
	submit(second, runID, turnID, map[string]any{"kind": "choice", "value": "bad"}, 400)
	submit(second, runID, turnID, map[string]any{"kind": "collector", "values": map[string]any{"not-a-token": false}}, 400)
	submit(second, runID, turnID, answer, 204)
	resolved := resumeTestFrame(t, frames, "resolved")
	if resolved["turnID"] != turnID {
		t.Fatalf("resolved wrong turn: %#v", resolved)
	}
	submit(second, runID, turnID, answer, 409)
	select {
	case result := <-nextDone:
		if result.Error != nil || result.Result["stepID"] != "public_call" {
			t.Fatalf("resumed caller failed: %#v", result)
		}
	case err := <-nextErr:
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		t.Fatal("answer did not release Next")
	}
	if result, err := resumeTestCall(second, "run.next", runID); err != nil || result.Error != nil || result.Result["stepID"] != "verify_answer" {
		t.Fatalf("caller assertion: %#v %v", result, err)
	}
	if result, err := resumeTestCall(second, "run.next", runID); err != nil || result.Error == nil || result.Error.Code != -32012 {
		t.Fatalf("completion: %#v %v", result, err)
	}
	final := completed(runID)
	if result, err := resumeTestCall(second, "run.get", runID); err != nil || result.Error != nil || result.Result["state"] != "completed" {
		t.Fatalf("public completed state: %#v %v", result, err)
	}
	traceData, err := os.ReadFile(filepath.Join(dir, "runs", runID, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	completionCount := 0
	stepCounts := map[string]int{}
	for _, line := range bytes.Split(traceData, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event struct {
			Kind    string         `json:"kind"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == "run/completed" {
			completionCount++
		}
		if event.Kind == "step/completed" {
			stepCounts[fmt.Sprint(event.Payload["step_id"])]++
		}
	}
	if completionCount != 1 || stepCounts["public_call"] != 1 || stepCounts["verify_answer"] != 1 {
		t.Fatalf("completion was not exactly once: run=%d steps=%v", completionCount, stepCounts)
	}
	select {
	case frame, ok := <-frames:
		if ok {
			t.Fatalf("unexpected additional interaction: %#v", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal SSE did not close")
	}
	// Further Next calls must not execute/assert/commit again.
	if result, err := resumeTestCall(second, "run.next", runID); err != nil || result.Error == nil {
		t.Fatalf("terminal Next: %#v %v", result, err)
	}
	unchanged, err := store.LoadState(context.Background(), runID)
	if err != nil || unchanged.CheckpointSequence != final.CheckpointSequence {
		t.Fatalf("terminal Next wrote checkpoint again: %v", err)
	}
	closeStream()
	stopSecond()
	lease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("writer lease not released after owned process exit: %v", err)
	}
	lease.Release()
	t.Logf("public restart completed original run %s turn %s, epoch %d -> %d, checkpoint %d -> %d",
		runID, turnID, before.WriterEpoch, final.WriterEpoch, before.CheckpointSequence, final.CheckpointSequence)
}
