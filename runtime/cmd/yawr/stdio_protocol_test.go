package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/serve"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestStdioProtocolChoiceRoundTrip(t *testing.T) {
	commandReader, commandWriter := io.Pipe()
	frameReader, frameWriter := io.Pipe()
	broker := serve.NewPromptBroker(16)
	broker.Register("run-1")
	t.Cleanup(func() {
		broker.Unregister("run-1")
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})

	session := newStdioProtocol(commandReader, frameWriter)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := session.attach(ctx, "run-1", broker, nil); err != nil {
		t.Fatalf("attach: %v", err)
	}

	response := make(chan *input.ChoiceResponse, 1)
	errors := make(chan error, 1)
	go func() {
		got, err := broker.PromptChoice(
			engine.WithRunID(ctx, "run-1"),
			input.ChoiceRequest{
				StepID: "choose-route",
				Prompt: "Choose a route",
				Options: []input.Option{
					{Label: "Primary", Value: "primary"},
					{Label: "Fallback", Value: "fallback"},
				},
			},
		)
		if err != nil {
			errors <- err
			return
		}
		response <- got
	}()

	scanner := bufio.NewScanner(frameReader)
	pending := readProtocolFrame(t, scanner, "interaction.pending")
	interaction := pending["interaction"].(map[string]any)
	if got := interaction["kind"]; got != "choice" {
		t.Fatalf("kind = %v, want choice", got)
	}
	turnID, _ := interaction["turnID"].(string)
	options := interaction["options"].([]any)
	fallbackToken := options[1].(map[string]any)["value"].(string)

	writeProtocolCommand(t, commandWriter, map[string]any{
		"type":   "interaction.answer",
		"runID":  "run-1",
		"turnID": turnID,
		"answer": map[string]any{
			"kind":     "choice",
			"selected": []string{fallbackToken},
		},
	})
	resolved := readProtocolFrame(t, scanner, "interaction.resolved")
	if got := resolved["turnID"]; got != turnID {
		t.Fatalf("resolved turnID = %v, want %s", got, turnID)
	}

	select {
	case err := <-errors:
		t.Fatalf("PromptChoice: %v", err)
	case got := <-response:
		if len(got.Selected) != 1 || got.Selected[0] != "fallback" {
			t.Fatalf("selected = %#v, want fallback", got.Selected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for choice response")
	}
}

func TestStdioProtocolApprovalRoundTrip(t *testing.T) {
	rig := newStdioBrokerRig(t, "run-approval")
	records := make(chan governance.ApprovalRecord, 1)
	errors := make(chan error, 1)
	go func() {
		record, err := rig.broker.RequestApproval(
			engine.WithRunID(rig.ctx, rig.runID),
			"mitigate",
			"governance policy requires approval",
		)
		if err != nil {
			errors <- err
			return
		}
		records <- record
	}()

	pending := readProtocolFrame(t, rig.scanner, "interaction.pending")
	interaction := pending["interaction"].(map[string]any)
	if interaction["kind"] != "approval" || interaction["stepID"] != "mitigate" {
		t.Fatalf("approval interaction = %#v", interaction)
	}
	writeProtocolCommand(t, rig.commands, map[string]any{
		"type": "interaction.answer", "runID": rig.runID, "turnID": interaction["turnID"],
		"answer": map[string]any{"kind": "approval", "approved": true, "approver": "operator-id"},
	})

	select {
	case err := <-errors:
		t.Fatalf("RequestApproval: %v", err)
	case record := <-records:
		if record.Approver != "operator-id" || record.Token == "" || record.ApprovedAt == "" {
			t.Fatalf("approval record = %#v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approval record")
	}
}

func TestStdioProtocolCancelInvokesActiveHandle(t *testing.T) {
	commandReader, commandWriter := io.Pipe()
	frameReader, frameWriter := io.Pipe()
	handle := &stdioFakeRunHandle{cancelled: make(chan struct{}), events: make(chan engine.Event)}
	session := newStdioProtocol(commandReader, frameWriter)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})

	if err := session.attach(ctx, "run-2", nil, handle); err != nil {
		t.Fatalf("attach: %v", err)
	}
	writeProtocolCommand(t, commandWriter, map[string]any{
		"type":   "run.cancel",
		"runID":  "run-2",
		"reason": "operator cancelled",
	})

	select {
	case <-handle.cancelled:
		if handle.cancelReason != "operator cancelled" {
			t.Fatalf("cancel reason = %q", handle.cancelReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancellation")
	}
}

func TestStdioProtocolRejectsRuntimeCommandWithoutRunID(t *testing.T) {
	protocol := newStdioProtocol(strings.NewReader(""), io.Discard)
	protocol.runID = "run-required"
	protocol.handle = &stdioFakeRunHandle{cancelled: make(chan struct{}), events: make(chan engine.Event)}
	if err := protocol.dispatchCommand(context.Background(), stdioCommand{Type: "run.cancel"}); err == nil || !strings.Contains(err.Error(), "runID is required") {
		t.Fatalf("dispatchCommand error = %v, want required runID", err)
	}
}

func TestStdioProtocolEOFInvokesActiveHandleCancellation(t *testing.T) {
	commandReader, commandWriter := io.Pipe()
	frameReader, frameWriter := io.Pipe()
	handle := &stdioFakeRunHandle{cancelled: make(chan struct{}), events: make(chan engine.Event)}
	session := newStdioProtocol(commandReader, frameWriter)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = commandReader.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})
	if err := session.attach(ctx, "run-eof", nil, handle); err != nil {
		t.Fatalf("attach: %v", err)
	}
	_ = commandWriter.Close()
	select {
	case <-handle.cancelled:
		if handle.cancelReason != "stdio input closed" {
			t.Fatalf("cancel reason = %q", handle.cancelReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for EOF cancellation")
	}
}

func TestStdioProtocolMalformedCommandCancelsActiveHandle(t *testing.T) {
	commandReader, commandWriter := io.Pipe()
	frameReader, frameWriter := io.Pipe()
	handle := &stdioFakeRunHandle{cancelled: make(chan struct{}), events: make(chan engine.Event)}
	protocol := newStdioProtocol(commandReader, frameWriter)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})
	if err := protocol.attach(ctx, "run-malformed", nil, handle); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := io.WriteString(commandWriter, "not-json\n"); err != nil {
		t.Fatalf("write malformed command: %v", err)
	}
	scanner := bufio.NewScanner(frameReader)
	if !scanner.Scan() {
		t.Fatalf("protocol error frame missing: %v", scanner.Err())
	}
	var frame map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
		t.Fatalf("decode protocol error: %v", err)
	}
	if frame["error"].(map[string]any)["code"] != "INVALID_COMMAND" {
		t.Fatalf("protocol error = %#v", frame["error"])
	}
	select {
	case <-handle.cancelled:
		if handle.cancelReason != "stdio protocol command failed" {
			t.Fatalf("cancel reason = %q", handle.cancelReason)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed command left active run uncancelled")
	}
}

func TestStdioProtocolDebugConfigRejectsOversizedFrame(t *testing.T) {
	oversized := `{"type":"run.configure","debug":{"enabled":true,"watches":["` + strings.Repeat("x", maxStdioCommandBytes) + `"]}}` + "\n"
	protocol := newStdioProtocol(strings.NewReader(oversized), io.Discard)
	_, err := protocol.readDebugConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("readDebugConfig error = %v, want size rejection", err)
	}
}

func TestStdioProtocolDebugConfigWaitHonorsCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	protocol := newStdioProtocol(reader, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := protocol.readDebugConfig(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readDebugConfig error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("readDebugConfig remained blocked after cancellation")
	}
	if _, err := io.WriteString(writer, "{}\n"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("debug config reader remained active after cancellation: write error = %v", err)
	}
}

func TestStdioProtocolDecisionRoundTrip(t *testing.T) {
	rig := newStdioBrokerRig(t, "run-decision")
	response := make(chan *input.DecisionResponse, 1)
	errors := make(chan error, 1)
	go func() {
		got, err := rig.broker.PromptDecision(
			engine.WithRunID(rig.ctx, rig.runID),
			input.DecisionRequest{
				StepID: "route",
				Prompt: "Choose route",
				Routes: []input.Route{{Label: "Primary"}, {Label: "Fallback"}},
			},
		)
		if err != nil {
			errors <- err
			return
		}
		response <- got
	}()
	pending := readProtocolFrame(t, rig.scanner, "interaction.pending")
	interaction := pending["interaction"].(map[string]any)
	routes := interaction["routes"].([]any)
	fallbackToken := routes[1].(map[string]any)["label"].(string)
	writeProtocolCommand(t, rig.commands, map[string]any{
		"type": "interaction.answer", "runID": rig.runID,
		"turnID": interaction["turnID"],
		"answer": map[string]any{"kind": "decision", "label": fallbackToken},
	})
	readProtocolFrame(t, rig.scanner, "interaction.resolved")
	select {
	case err := <-errors:
		t.Fatal(err)
	case got := <-response:
		if got.Label != "Fallback" {
			t.Fatalf("label = %q", got.Label)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decision response timeout")
	}
}

func TestStdioProtocolCollectorRoundTrip(t *testing.T) {
	rig := newStdioBrokerRig(t, "run-collector")
	response := make(chan *input.FormResponse, 1)
	errors := make(chan error, 1)
	go func() {
		got, err := rig.broker.PromptForm(
			engine.WithRunID(rig.ctx, rig.runID),
			input.FormRequest{
				StepID: "collect",
				Prompt: "Record findings",
				Fields: []input.FormField{
					{Name: "severity", Type: "choice", Options: []input.Option{{Label: "Low", Value: "low"}, {Label: "High", Value: "high"}}},
					{Name: "notes", Type: "text"},
				},
			},
		)
		if err != nil {
			errors <- err
			return
		}
		response <- got
	}()
	pending := readProtocolFrame(t, rig.scanner, "interaction.pending")
	interaction := pending["interaction"].(map[string]any)
	fields := interaction["fields"].([]any)
	severity := fields[0].(map[string]any)
	notes := fields[1].(map[string]any)
	options := severity["options"].([]any)
	highToken := options[1].(map[string]any)["value"].(string)
	writeProtocolCommand(t, rig.commands, map[string]any{
		"type": "interaction.answer", "runID": rig.runID,
		"turnID": interaction["turnID"],
		"answer": map[string]any{
			"kind": "collector",
			"values": map[string]any{
				severity["name"].(string): highToken,
				notes["name"].(string):    "details",
			},
		},
	})
	readProtocolFrame(t, rig.scanner, "interaction.resolved")
	select {
	case err := <-errors:
		t.Fatal(err)
	case got := <-response:
		if got.Values["severity"] != "high" || got.Values["notes"] != "details" {
			t.Fatalf("values = %#v", got.Values)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("collector response timeout")
	}
}

func TestStdioProtocolHostActionRoundTrip(t *testing.T) {
	rig := newStdioBrokerRig(t, "run-host")
	response := make(chan hostaction.Response, 1)
	errors := make(chan error, 1)
	go func() {
		got, err := rig.broker.ExecuteHostAction(
			engine.WithRunID(rig.ctx, rig.runID),
			hostaction.Request{
				Capability: hostaction.Capability("test.echo"),
				Payload:    map[string]any{"echo": "hello"},
			},
		)
		if err != nil {
			errors <- err
			return
		}
		response <- got
	}()
	pending := readProtocolFrame(t, rig.scanner, "interaction.pending")
	interaction := pending["interaction"].(map[string]any)
	hostRequest := interaction["host_action"].(map[string]any)
	writeProtocolCommand(t, rig.commands, map[string]any{
		"type": "interaction.answer", "runID": rig.runID,
		"turnID": interaction["turnID"],
		"answer": map[string]any{
			"kind":          "host_action",
			"runID":         rig.runID,
			"turnID":        interaction["turnID"],
			"correlationID": interaction["correlationID"],
			"capability":    hostRequest["capability"],
			"status":        "completed",
			"result":        map[string]any{"echo": "hello"},
		},
	})
	readProtocolFrame(t, rig.scanner, "interaction.resolved")
	select {
	case err := <-errors:
		t.Fatal(err)
	case got := <-response:
		if got.Status != hostaction.StatusCompleted || got.Result["echo"] != "hello" {
			t.Fatalf("response = %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("host action response timeout")
	}
}

type stdioBrokerRig struct {
	ctx      context.Context
	runID    string
	broker   *serve.PromptBroker
	commands io.Writer
	scanner  *bufio.Scanner
}

func newStdioBrokerRig(t *testing.T, runID string) *stdioBrokerRig {
	t.Helper()
	commandReader, commandWriter := io.Pipe()
	frameReader, frameWriter := io.Pipe()
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	ctx, cancel := context.WithCancel(context.Background())
	session := newStdioProtocol(commandReader, frameWriter)
	if err := session.attach(ctx, runID, broker, nil); err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		broker.Unregister(runID)
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})
	return &stdioBrokerRig{
		ctx:      ctx,
		runID:    runID,
		broker:   broker,
		commands: commandWriter,
		scanner:  bufio.NewScanner(frameReader),
	}
}

func TestRunStdioChoiceEndToEnd(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "choice.runbook.yaml")
	runbook := `apiVersion: yawr.runbook/v1
id: stdio-choice
name: stdio-choice
kind: reference
flow:
  - step:
      id: choose-route
      type: choice
      prompt: Choose a route
      variable: selected_route
      options:
        - value: primary
          label: Primary
        - value: fallback
          label: Fallback
`
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	t.Chdir(dir)

	oldStdin, oldStdout := os.Stdin, os.Stdout
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	frameReader, frameWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdin, os.Stdout = commandReader, frameWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})

	frames := make(chan map[string]any, 32)
	go func() {
		scanner := bufio.NewScanner(frameReader)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
		close(frames)
	}()

	exitCodes := make(chan int, 1)
	go func() {
		code := runRun([]string{
			"--stdio",
			"--trace", filepath.Join(dir, "trace.jsonl"),
			runbookPath,
		})
		_ = frameWriter.Close()
		exitCodes <- code
	}()

	started := waitForProtocolType(t, frames, "run.started")
	runID, _ := started["runID"].(string)
	if runID == "" {
		t.Fatal("run.started missing runID")
	}
	sawStepEvent := false
	var pending map[string]any
	for pending == nil {
		frame := waitForAnyProtocolFrame(t, frames)
		if frame["type"] == "run.event" {
			event, _ := frame["event"].(map[string]any)
			kind, _ := event["kind"].(string)
			if kind == "step/started" || kind == "step/completed" {
				sawStepEvent = true
			}
		}
		if frame["type"] == "interaction.pending" {
			pending = frame
		}
	}
	interaction := pending["interaction"].(map[string]any)
	turnID := interaction["turnID"].(string)
	options := interaction["options"].([]any)
	selected := options[1].(map[string]any)["value"].(string)
	writeProtocolCommand(t, commandWriter, map[string]any{
		"type":   "interaction.answer",
		"runID":  runID,
		"turnID": turnID,
		"answer": map[string]any{"kind": "choice", "selected": []string{selected}},
	})

	for {
		frame := waitForAnyProtocolFrame(t, frames)
		if frame["type"] == "run.event" {
			event, _ := frame["event"].(map[string]any)
			kind, _ := event["kind"].(string)
			if kind == "step/started" || kind == "step/completed" {
				sawStepEvent = true
			}
		}
		if frame["type"] == "run.finished" {
			if frame["status"] != "completed" {
				t.Fatalf("run status = %v, want completed", frame["status"])
			}
			break
		}
	}
	if !sawStepEvent {
		t.Fatal("stdio stream contained no live step event")
	}

	select {
	case code := <-exitCodes:
		if code != exitSuccess {
			t.Fatalf("runRun exit code = %d, want %d", code, exitSuccess)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for runRun to exit")
	}
}

func TestRunStdioGovernanceApprovalEndToEnd(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "approval.runbook.yaml")
	runbook := `apiVersion: yawr.runbook/v1
id: stdio-approval
name: stdio-approval
kind: reference
governance:
  require_approval: true
flow:
  - step:
      id: governed-step
      type: noop
`
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	t.Chdir(dir)

	oldStdin, oldStdout := os.Stdin, os.Stdout
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	frameReader, frameWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdin, os.Stdout = commandReader, frameWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})

	frames := make(chan map[string]any, 32)
	go func() {
		scanner := bufio.NewScanner(frameReader)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
		close(frames)
	}()
	exitCodes := make(chan int, 1)
	go func() {
		code := runRun([]string{"--stdio", "--trace", filepath.Join(dir, "trace.jsonl"), runbookPath})
		_ = frameWriter.Close()
		exitCodes <- code
	}()

	started := waitForProtocolType(t, frames, "run.started")
	runID := started["runID"].(string)
	pending := waitForProtocolType(t, frames, "interaction.pending")
	interaction := pending["interaction"].(map[string]any)
	if interaction["kind"] != "approval" {
		t.Fatalf("interaction kind = %v, want approval", interaction["kind"])
	}
	writeProtocolCommand(t, commandWriter, map[string]any{
		"type": "interaction.answer", "runID": runID, "turnID": interaction["turnID"],
		"answer": map[string]any{"kind": "approval", "approved": true, "approver": "operator-id"},
	})
	finished := waitForProtocolType(t, frames, "run.finished")
	if finished["status"] != "completed" {
		t.Fatalf("run status = %v, want completed", finished["status"])
	}
	select {
	case code := <-exitCodes:
		if code != exitSuccess {
			t.Fatalf("exit code = %d, want %d", code, exitSuccess)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for governed run to exit")
	}
}

func TestRunStdioRedactsDeclaredSecretAcrossFrames(t *testing.T) {
	const secret = "SENTINEL-stdio-secret-7f3a"
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "secret.runbook.yaml")
	runbook := `apiVersion: yawr.runbook/v1
id: stdio-secret
name: stdio-secret
kind: reference
inputs:
  access_token:
    type: secret
    required: true
flow:
  - step:
      id: show-secret
      type: display
      display:
        content: "Bearer ${access_token}"
  - step:
      id: copy-secret
      type: noop
      capture:
        copied_secret: "prefix-${access_token}"
`
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	t.Chdir(dir)

	oldStdin, oldStdout := os.Stdin, os.Stdout
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	frameReader, frameWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdin, os.Stdout = commandReader, frameWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})

	frames := make(chan map[string]any, 64)
	go func() {
		scanner := bufio.NewScanner(frameReader)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
		close(frames)
	}()
	exitCodes := make(chan int, 1)
	go func() {
		code := runRun([]string{
			"--stdio", "--var", "access_token=" + secret,
			"--trace", filepath.Join(dir, "trace.jsonl"), runbookPath,
		})
		_ = frameWriter.Close()
		exitCodes <- code
	}()

	var observed []map[string]any
	for {
		frame := waitForAnyProtocolFrame(t, frames)
		observed = append(observed, frame)
		if frame["type"] == "run.finished" {
			break
		}
	}
	encoded, err := json.Marshal(observed)
	if err != nil {
		t.Fatalf("marshal observed frames: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("declared secret leaked in stdio frames: %s", encoded)
	}
	if !strings.Contains(string(encoded), "redacted") {
		t.Fatalf("stdio frames did not mark secret redaction: %s", encoded)
	}
	select {
	case code := <-exitCodes:
		if code != exitSuccess {
			t.Fatalf("exit code = %d, want %d", code, exitSuccess)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for secret run to exit")
	}
	traceBytes, err := os.ReadFile(filepath.Join(dir, "trace.jsonl"))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if strings.Contains(string(traceBytes), secret) {
		t.Fatal("declared secret leaked into persisted trace")
	}
}

func TestRunStdioDynamicIncludeRedactsChildSecret(t *testing.T) {
	const secret = "SENTINEL-dynamic-stdio-secret-9d2e"
	dir := makeWorkDir(t)
	runbookPath := writeDynamicSecretIncludeFixture(t, dir, secret)
	t.Chdir(dir)

	oldStdin, oldStdout := os.Stdin, os.Stdout
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	frameReader, frameWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdin, os.Stdout = commandReader, frameWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})

	frames := make(chan map[string]any, 64)
	go func() {
		scanner := bufio.NewScanner(frameReader)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
		close(frames)
	}()
	exitCodes := make(chan int, 1)
	go func() {
		exitCodes <- runRun([]string{
			"--stdio", "--var", "child_ref=acme-dynsecret/child-secret",
			"--trace", filepath.Join(filepath.Dir(runbookPath), "trace.jsonl"), runbookPath,
		})
		_ = frameWriter.Close()
	}()

	var observed []map[string]any
	for {
		frame := waitForAnyProtocolFrame(t, frames)
		observed = append(observed, frame)
		if frame["type"] == "run.finished" {
			if frame["status"] != "completed" {
				t.Fatalf("run status = %v, want completed", frame["status"])
			}
			break
		}
	}
	encoded, err := json.Marshal(observed)
	if err != nil {
		t.Fatalf("marshal frames: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("dynamic child secret %q leaked in stdio frames", secret)
	}
	if !strings.Contains(string(encoded), `"step_id":"copy-secret"`) || !strings.Contains(string(encoded), `"status":"completed"`) {
		t.Fatal("stdio redaction removed step_id or status")
	}
	select {
	case code := <-exitCodes:
		if code != exitSuccess {
			t.Fatalf("exit code = %d, want %d", code, exitSuccess)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dynamic include run")
	}
}

func TestRunStdioAcceptsPrivateInputsFromConfigure(t *testing.T) {
	const secret = "SENTINEL-private-input"
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "private-input.runbook.yaml")
	runbook := `apiVersion: yawr.runbook/v1
id: stdio-private-input
name: stdio-private-input
kind: reference
inputs:
  access_token:
    type: secret
    required: true
flow:
  - step:
      id: done
      type: noop
`
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	t.Chdir(dir)

	oldStdin, oldStdout := os.Stdin, os.Stdout
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	frameReader, frameWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdin, os.Stdout = commandReader, frameWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})
	if err := json.NewEncoder(commandWriter).Encode(map[string]any{
		"type": "run.configure", "inputs": map[string]string{"access_token": secret},
	}); err != nil {
		t.Fatalf("write run.configure: %v", err)
	}

	exitCodes := make(chan int, 1)
	go func() {
		exitCodes <- runRun([]string{"--stdio", "--configure", "--trace", filepath.Join(dir, "trace.jsonl"), runbookPath})
		_ = frameWriter.Close()
	}()
	scanner := bufio.NewScanner(frameReader)
	var stream strings.Builder
	for scanner.Scan() {
		stream.Write(scanner.Bytes())
		stream.WriteByte('\n')
		var frame map[string]any
		if json.Unmarshal(scanner.Bytes(), &frame) == nil && frame["type"] == "run.finished" {
			break
		}
	}
	if strings.Contains(stream.String(), secret) {
		t.Fatalf("private input leaked in frames: %s", stream.String())
	}
	select {
	case code := <-exitCodes:
		if code != exitSuccess {
			t.Fatalf("exit code = %d, want %d", code, exitSuccess)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for private-input run")
	}
}

func TestStdioRedactionPreservesProtocolStructure(t *testing.T) {
	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	if err := protocol.configureRedaction([]string{"run", "yawr", "type", "run-structure"}, nil); err != nil {
		t.Fatalf("configure redaction: %v", err)
	}
	if err := protocol.send(map[string]any{
		"type":   "run.started",
		"runID":  "run-structure",
		"status": "running",
		"payload": map[string]any{
			"type": "value contains run and yawr",
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(output.Bytes(), &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if frame["type"] != "run.started" || frame["version"] != stdioProtocolVersion || frame["runID"] != "run-structure" || frame["status"] != "running" {
		t.Fatalf("protocol structure was redacted: %#v", frame)
	}
	payload := frame["payload"].(map[string]any)
	if _, ok := payload["type"]; !ok || strings.Contains(payload["type"].(string), "run") || strings.Contains(payload["type"].(string), "yawr") {
		t.Fatalf("payload was not safely redacted: %#v", payload)
	}

	output.Reset()
	if err := protocol.send(map[string]any{
		"type": "run.event", "runID": "run-structure",
		"event": engine.Event{EventID: "event-run", RunID: "run-structure", RunbookID: "yawr", Kind: "run/started", Payload: map[string]any{"line": "run yawr"}},
	}); err != nil {
		t.Fatalf("send event: %v", err)
	}
	if err := json.Unmarshal(output.Bytes(), &frame); err != nil {
		t.Fatalf("decode event frame: %v", err)
	}
	event := frame["event"].(map[string]any)
	if event["kind"] != "run/started" || event["run_id"] != "run-structure" || event["runbook_id"] != "yawr" || event["event_id"] != "event-run" {
		t.Fatalf("event structure was redacted: %#v", event)
	}
	if strings.Contains(event["payload"].(map[string]any)["line"].(string), "run") {
		t.Fatalf("event payload was not redacted: %#v", event["payload"])
	}
}

func TestStdioRouteTestSanitizationPreservesStatus(t *testing.T) {
	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	if err := protocol.configureRedaction([]string{"reached"}, nil); err != nil {
		t.Fatalf("configure redaction: %v", err)
	}
	if err := protocol.send(map[string]any{
		"type": "run.finished", "runID": "route-test-run", "status": "completed",
		"routeTest": map[string]any{
			"passed": true, "targetReached": true, "externalDispatches": 0,
			"status": "reached", "message": "target reached with secret reached",
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(output.Bytes(), &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	routeTest := frame["routeTest"].(map[string]any)
	if routeTest["status"] != "reached" {
		t.Fatalf("routeTest.status was redacted: %#v", routeTest)
	}
	if message := routeTest["message"].(string); strings.Contains(message, "reached") {
		t.Fatalf("routeTest message was not redacted: %q", message)
	}
}

func TestStdioTerminalRedactionPreservesExactNodeIdentity(t *testing.T) {
	const secret = "leaf"
	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	if err := protocol.configureRedaction([]string{secret}, nil); err != nil {
		t.Fatalf("configure redaction: %v", err)
	}
	if err := protocol.send(map[string]any{
		"type": "run.finished", "runID": "run-identity", "status": "completed",
		"steps": []stepSummary{{
			StepID: "leaf", NodeID: "inspect-primary/leaf", Kind: "tool", Status: "completed",
			Output: map[string]any{"message": "result for leaf"},
		}},
	}); err != nil {
		t.Fatalf("send terminal frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(output.Bytes(), &frame); err != nil {
		t.Fatalf("decode terminal frame: %v", err)
	}
	step := frame["steps"].([]any)[0].(map[string]any)
	if step["step_id"] != "leaf" || step["node_id"] != "inspect-primary/leaf" {
		t.Fatalf("terminal identity was redacted: %#v", step)
	}
	if message := step["output"].(map[string]any)["message"].(string); strings.Contains(message, secret) {
		t.Fatalf("terminal output was not redacted: %q", message)
	}
}

func TestStdioProtectionTraversesNestedIncludesWithoutRewritingEventStructure(t *testing.T) {
	registry := internalexecutor.NewMapRegistry()
	registry.Register("include", internalexecutor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, nil, nil))
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{{
		ID: "outer", Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{Runbook: "outer.runbook.yaml"},
			ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{
				ID: "inner", Type: schema.StepTypeInclude,
				IncludeSpec: &schema.IncludeSpec{
					Include:            schema.IncludeConfig{Runbook: "inner.runbook.yaml", With: map[string]string{"credential": "${nested_secret}"}},
					ResolvedInputs:     map[string]*schema.Input{"credential": {Type: "secret"}},
					ResolvedGovernance: &schema.GovernanceConfig{Redact: []schema.RedactRule{{Pattern: "token-[a-z]+", Replace: "<redacted>"}}},
				},
			}}},
		},
	}}}
	protection, complete := stdioDeclaredProtection(
		context.Background(), registry, plan, map[string]any{"nested_secret": "completed"},
	)
	if !complete {
		t.Fatal("static nested protection was incomplete")
	}
	if len(protection.SecretValues) == 0 || len(protection.RedactionPatterns) == 0 {
		t.Fatalf("nested protection = %#v", protection)
	}

	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	if err := protocol.configureRedaction(protection.SecretValues, protection.RedactionPatterns); err != nil {
		t.Fatalf("configure redaction: %v", err)
	}
	if err := protocol.send(map[string]any{
		"type": "run.event", "runID": "run-nested",
		"event": engine.Event{Kind: "step/completed", Payload: map[string]any{
			"node_id": "completed", "status": "completed", "line": "completed token-alpha",
		}},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(output.Bytes(), &frame); err != nil {
		t.Fatalf("decode: %v", err)
	}
	payload := frame["event"].(map[string]any)["payload"].(map[string]any)
	if payload["node_id"] != "completed" || payload["status"] != "completed" {
		t.Fatalf("event structure was redacted: %#v", payload)
	}
	if line := payload["line"].(string); strings.Contains(line, "completed") || strings.Contains(line, "token-alpha") {
		t.Fatalf("nested secret or governance pattern leaked: %q", line)
	}
}

func TestStdioRedactsDebugWatchContent(t *testing.T) {
	const secret = "SENTINEL-debug-watch"
	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	if err := protocol.configureRedaction([]string{secret}, nil); err != nil {
		t.Fatalf("configure redaction: %v", err)
	}
	if err := protocol.send(map[string]any{
		"type": "interaction.pending", "runID": "run-debug", "turnID": "turn-debug",
		"interaction": map[string]any{
			"type": "pending", "runID": "run-debug", "turnID": "turn-debug", "stepID": "inspect", "kind": "debug_break",
			"debug": map[string]any{
				"phase": "after", "invocation": 1, "attempt": 1,
				"watches": []any{map[string]any{"expression": secret, "value": "prefix-" + secret}},
			},
		},
	}); err != nil {
		t.Fatalf("send debug interaction: %v", err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("debug watch leaked declared secret: %s", output.String())
	}
	if !strings.Contains(output.String(), "redacted") {
		t.Fatalf("debug watch did not mark redaction: %s", output.String())
	}
}

func TestRunNonStdioDoesNotInstallTypedNilInteractionProviders(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "ordinary.runbook.yaml")
	runbook := `apiVersion: yawr.runbook/v1
id: ordinary-run
name: ordinary-run
kind: reference
governance:
  require_approval: true
flow:
  - step:
      id: governed-step
      type: noop
`
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	t.Chdir(dir)
	if code := runWithMode([]string{"--trace", filepath.Join(dir, "trace.jsonl"), runbookPath}, engine.RunModeDryRun); code != exitSuccess && code != exitFailure {
		t.Fatalf("runWithMode exit = %d, want a normal terminal exit", code)
	}
}

func TestRunStdioDebugOverrideEndToEnd(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "debug.runbook.yaml")
	runbook := strings.Join([]string{
		"apiVersion: yawr.runbook/v1",
		"id: stdio-debug",
		"name: stdio-debug",
		"kind: reference",
		"flow:",
		"  - step:",
		"      id: get-incident",
		"      type: cli",
		"      command: go",
		"      args: [version]",
		"      capture:",
		"        incident_status: stdout",
		"  - step:",
		"      id: route-incident",
		"      type: branch",
		"      branches:",
		"        - condition: 'incident_status == \"Active\"'",
		"          label: Active",
		"          steps:",
		"            - step:",
		"                id: active-path",
		"                type: noop",
		"        - condition: 'incident_status != \"Active\"'",
		"          label: Other",
		"          steps:",
		"            - step:",
		"                id: other-path",
		"                type: noop",
	}, "\n") + "\n"
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	t.Chdir(dir)

	oldStdin, oldStdout := os.Stdin, os.Stdout
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	frameReader, frameWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdin, os.Stdout = commandReader, frameWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})

	frames := make(chan map[string]any, 32)
	go func() {
		scanner := bufio.NewScanner(frameReader)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
		close(frames)
	}()

	exitCodes := make(chan int, 1)
	go func() {
		code := runRun([]string{
			"--stdio",
			"--debug",
			"--trace", filepath.Join(dir, "trace.jsonl"),
			runbookPath,
		})
		_ = frameWriter.Close()
		exitCodes <- code
	}()

	writeProtocolCommand(t, commandWriter, map[string]any{
		"type": "run.configure",
		"debug": map[string]any{
			"enabled": true,
			"breakpoints": []map[string]any{{
				"step":  "get-incident",
				"phase": "after",
			}},
		},
	})

	started := waitForProtocolType(t, frames, "run.started")
	runID := started["runID"].(string)
	pending := waitForProtocolType(t, frames, "interaction.pending")
	interaction := pending["interaction"].(map[string]any)
	if interaction["kind"] != "debug_break" {
		t.Fatalf("interaction kind = %v, want debug_break", interaction["kind"])
	}
	debug := interaction["debug"].(map[string]any)
	if debug["phase"] != "after" {
		t.Fatalf("debug phase = %v, want after", debug["phase"])
	}

	writeProtocolCommand(t, commandWriter, map[string]any{
		"type":   "interaction.answer",
		"runID":  runID,
		"turnID": interaction["turnID"],
		"answer": map[string]any{
			"kind":   "debug_break",
			"action": "continue",
			"set": map[string]any{
				"status": "completed",
				"output_patch": map[string]any{
					"stdout": "Active",
				},
			},
		},
	})

	finished := waitForProtocolType(t, frames, "run.finished")
	if finished["status"] != "completed" {
		t.Fatalf("run status = %v, want completed", finished["status"])
	}
	steps := finished["steps"].([]any)
	output := steps[0].(map[string]any)["output"].(map[string]any)
	if output["stdout_excerpt"] != "Active" {
		t.Fatalf("effective output = %v, want Active", output["stdout_excerpt"])
	}
	branchOutput := steps[1].(map[string]any)["output"].(map[string]any)
	if branchOutput["matched_arm"] != "Active" {
		t.Fatalf("matched arm = %v, want Active", branchOutput["matched_arm"])
	}

	select {
	case code := <-exitCodes:
		if code != exitSuccess {
			t.Fatalf("runRun exit code = %d, want %d", code, exitSuccess)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for debug run to exit")
	}
}

func TestStdioFinishedSummaryIsBounded(t *testing.T) {
	result := &engine.StepResult{
		StepID: "large-output",
		Status: engine.StepStatusCompleted,
		Output: map[string]any{"stdout": strings.Repeat("x", 2*maxStdioFrameBytes)},
	}
	summary := summarizeStdioStep(result, "cli")
	if summary.NodeID != "large-output" {
		t.Fatalf("terminal summary node_id = %q, want large-output", summary.NodeID)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if len(encoded) >= maxStdioFrameBytes/2 {
		t.Fatalf("bounded summary = %d bytes, want less than %d", len(encoded), maxStdioFrameBytes/2)
	}
	if summary.Output["stdout_truncated"] != true {
		t.Fatalf("bounded summary did not mark stdout truncation: %#v", summary.Output)
	}
}

func TestStdioFinishedSummaryMarksOmittedStructuredOutput(t *testing.T) {
	result := &engine.StepResult{
		StepID: "structured-output",
		Status: engine.StepStatusCompleted,
		Output: map[string]any{"incident": map[string]any{"status": "Active"}},
	}
	summary := summarizeStdioStep(result, "tool")
	if summary.Output["preview_fields_omitted"] != 1 || summary.Output["preview_truncated"] != true {
		t.Fatalf("bounded structured output did not mark omission: %#v", summary.Output)
	}
}

func TestStdioFinishedStepsFitProtocolFrame(t *testing.T) {
	results := make([]stepSummary, 1024)
	for index := range results {
		results[index] = stepSummary{
			StepID: fmt.Sprintf("step-%04d", index),
			Kind:   "cli",
			Status: string(engine.StepStatusCompleted),
			Output: map[string]any{"stdout": strings.Repeat("x", 16*1024)},
		}
	}
	bounded, omitted := boundedStdioStepSummaries(results)
	if omitted == 0 {
		t.Fatal("large terminal summary did not report omitted steps")
	}
	frame, err := json.Marshal(map[string]any{
		"type": "run.finished", "version": stdioProtocolVersion,
		"runID": "run-large", "status": "completed", "steps": bounded,
		"stepsOmitted": omitted,
	})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if len(frame)+1 > maxStdioFrameBytes {
		t.Fatalf("run.finished = %d bytes, exceeds %d", len(frame)+1, maxStdioFrameBytes)
	}
}

func TestStdioProtocolBudgetsFrameAfterRedactionExpansion(t *testing.T) {
	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	if err := protocol.configureRedaction([]string{"x"}, nil); err != nil {
		t.Fatalf("configure redaction: %v", err)
	}
	err := protocol.send(map[string]any{
		"type": "run.event", "runID": "run-redaction-budget",
		"event": engine.Event{Kind: "step/output", Payload: map[string]any{
			"line": strings.Repeat("x", maxStdioFrameBytes/8),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("send error = %v, want post-redaction size rejection", err)
	}
	if output.Len() != 0 {
		t.Fatalf("oversized redacted frame was partially written: %d bytes", output.Len())
	}
}

func TestStdioProtocolDropsFramesAfterRunFinished(t *testing.T) {
	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	if err := protocol.send(map[string]any{"type": "run.finished", "runID": "run-terminal", "status": "completed"}); err != nil {
		t.Fatalf("send run.finished: %v", err)
	}
	if err := protocol.send(map[string]any{"type": "run.event", "runID": "run-terminal", "event": map[string]any{"kind": "step/completed"}}); err != nil {
		t.Fatalf("send trailing event: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("protocol wrote %d frames after terminal frame: %q", len(lines), output.String())
	}
}

func TestStdioProtocolDrainsEventsBeforeRunFinished(t *testing.T) {
	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	events := make(chan engine.Event, 1)
	handle := &stdioFakeRunHandle{events: events, cancelled: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := protocol.attach(ctx, "run-order", nil, handle); err != nil {
		t.Fatalf("attach: %v", err)
	}
	events <- engine.Event{Kind: "debug/override_applied", RunID: "run-order", Payload: map[string]any{"step_id": "one"}}
	close(events)
	if err := protocol.waitForEvents(ctx); err != nil {
		t.Fatalf("waitForEvents: %v", err)
	}
	if err := protocol.send(map[string]any{"type": "run.finished", "runID": "run-order", "status": "completed"}); err != nil {
		t.Fatalf("send run.finished: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"type":"run.event"`) || !strings.Contains(lines[1], `"type":"run.finished"`) {
		t.Fatalf("protocol frame order = %q", output.String())
	}
}

func TestStdioProtocolDrainsResolvedInteractionsBeforeRunFinished(t *testing.T) {
	var output bytes.Buffer
	protocol := newStdioProtocol(strings.NewReader(""), &output)
	broker := serve.NewPromptBroker(16)
	const runID = "run-interaction-order"
	broker.Register(runID)
	observer, err := broker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("subscribe observer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := protocol.attach(ctx, runID, broker, nil); err != nil {
		t.Fatalf("attach: %v", err)
	}
	answered := make(chan error, 1)
	go func() {
		_, promptErr := broker.PromptDecision(engine.WithRunID(ctx, runID), input.DecisionRequest{
			StepID: "choose", Prompt: "Choose", Routes: []input.Route{{Label: "continue"}},
		})
		answered <- promptErr
	}()
	pending, ok := (<-observer).(serve.PendingInteraction)
	if !ok {
		t.Fatal("observer did not receive pending interaction")
	}
	if err := broker.Answer(runID, pending.TurnID, serve.AnswerEnvelope{Kind: "decision", Label: "r:0"}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("prompt: %v", err)
	}
	broker.Unregister(runID)
	if err := protocol.waitForInteractions(ctx); err != nil {
		t.Fatalf("waitForInteractions: %v", err)
	}
	if err := protocol.send(map[string]any{"type": "run.finished", "runID": runID, "status": "completed"}); err != nil {
		t.Fatalf("send run.finished: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 ||
		!strings.Contains(lines[0], `"type":"interaction.pending"`) ||
		!strings.Contains(lines[1], `"type":"interaction.resolved"`) ||
		!strings.Contains(lines[2], `"type":"run.finished"`) {
		t.Fatalf("protocol frame order = %q", output.String())
	}
}

func TestStdioEventWriteFailureCancelsActiveRun(t *testing.T) {
	commandReader, commandWriter := io.Pipe()
	t.Cleanup(func() { _ = commandWriter.Close() })
	protocol := newStdioProtocol(commandReader, failingProtocolWriter{})
	events := make(chan engine.Event, 1)
	handle := &stdioFakeRunHandle{events: events, cancelled: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := protocol.attach(ctx, "run-write-failure", nil, handle); err != nil {
		t.Fatalf("attach: %v", err)
	}
	events <- engine.Event{Kind: "step/started", RunID: "run-write-failure", Payload: map[string]any{"step_id": "one"}}
	select {
	case <-handle.cancelled:
		if handle.cancelReason != "stdio protocol output failed" {
			t.Fatalf("cancel reason = %q", handle.cancelReason)
		}
	case <-time.After(time.Second):
		t.Fatal("event write failure left active run uncancelled")
	}
	if err := protocol.waitForEvents(ctx); err == nil {
		t.Fatal("event write failure was not reported")
	}
}

func TestStdioInteractionWriteFailureCancelsActiveRun(t *testing.T) {
	commandReader, commandWriter := io.Pipe()
	t.Cleanup(func() { _ = commandWriter.Close() })
	protocol := newStdioProtocol(commandReader, failingProtocolWriter{})
	broker := serve.NewPromptBroker(4)
	broker.Register("run-interaction-write-failure")
	t.Cleanup(func() { broker.Unregister("run-interaction-write-failure") })
	handle := &stdioFakeRunHandle{cancelled: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := protocol.attach(ctx, "run-interaction-write-failure", broker, handle); err != nil {
		t.Fatalf("attach: %v", err)
	}
	go func() {
		promptCtx := engine.WithRunID(ctx, "run-interaction-write-failure")
		_, _ = broker.PromptChoice(promptCtx, input.ChoiceRequest{StepID: "choose", Prompt: "Choose"})
	}()
	select {
	case <-handle.cancelled:
		if handle.cancelReason != "stdio protocol output failed" {
			t.Errorf("cancel reason = %q", handle.cancelReason)
		}
	case <-time.After(time.Second):
		t.Fatal("interaction write failure left active run uncancelled")
	}
}

func TestRunStdioFailsWhenLifecycleFrameCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "broken-output.runbook.yaml")
	if err := os.WriteFile(runbookPath, []byte("apiVersion: yawr.runbook/v1\nid: broken-output\nname: broken-output\nkind: reference\nflow:\n  - step:\n      id: done\n      type: noop\n"), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	t.Chdir(dir)

	oldStdin, oldStdout := os.Stdin, os.Stdout
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	frameReader, frameWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	_ = frameReader.Close()
	os.Stdin, os.Stdout = commandReader, frameWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameWriter.Close()
	})

	if code := runRun([]string{"--stdio", "--trace", filepath.Join(dir, "trace.jsonl"), runbookPath}); code != exitRuntime {
		t.Fatalf("runRun exit = %d, want %d after lifecycle write failure", code, exitRuntime)
	}
}

func TestRunStdioCancelEndToEnd(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "cancel.runbook.yaml")
	runbook := `apiVersion: yawr.runbook/v1
id: stdio-cancel
name: stdio-cancel
kind: reference
flow:
  - step:
      id: wait-for-choice
      type: choice
      prompt: Wait for operator
      variable: selected
      options:
        - value: continue
          label: Continue
`
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	t.Chdir(dir)

	oldStdin, oldStdout := os.Stdin, os.Stdout
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	frameReader, frameWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdin, os.Stdout = commandReader, frameWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = commandReader.Close()
		_ = commandWriter.Close()
		_ = frameReader.Close()
		_ = frameWriter.Close()
	})

	frames := make(chan map[string]any, 32)
	go func() {
		scanner := bufio.NewScanner(frameReader)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
		close(frames)
	}()
	exitCodes := make(chan int, 1)
	go func() {
		code := runRun([]string{"--stdio", "--trace", filepath.Join(dir, "trace.jsonl"), runbookPath})
		_ = frameWriter.Close()
		exitCodes <- code
	}()

	started := waitForProtocolType(t, frames, "run.started")
	runID := started["runID"].(string)
	waitForProtocolType(t, frames, "interaction.pending")
	writeProtocolCommand(t, commandWriter, map[string]any{
		"type": "run.cancel", "runID": runID, "reason": "operator cancelled",
	})
	finished := waitForProtocolType(t, frames, "run.finished")
	if finished["status"] != "cancelled" {
		t.Fatalf("status = %v, want cancelled", finished["status"])
	}
	select {
	case code := <-exitCodes:
		if code != exitFailure {
			t.Fatalf("exit code = %d, want %d", code, exitFailure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancelled run to exit")
	}
}

func waitForProtocolType(t *testing.T, frames <-chan map[string]any, want string) map[string]any {
	t.Helper()
	for {
		frame := waitForAnyProtocolFrame(t, frames)
		if frame["type"] == "protocol.error" {
			t.Fatalf("protocol error while waiting for %s: %#v", want, frame["error"])
		}
		if frame["type"] == want {
			return frame
		}
	}
}

func waitForAnyProtocolFrame(t *testing.T, frames <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("protocol stream closed")
		}
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for protocol frame")
		return nil
	}
}

func readProtocolFrame(t *testing.T, scanner *bufio.Scanner, wantType string) map[string]any {
	t.Helper()
	for scanner.Scan() {
		var frame map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatalf("decode frame %q: %v", scanner.Text(), err)
		}
		if frame["type"] == "protocol.error" {
			t.Fatalf("protocol error while waiting for %s: %#v", wantType, frame["error"])
		}
		if frame["type"] == wantType {
			return frame
		}
	}
	t.Fatalf("stream closed before %s: %v", wantType, scanner.Err())
	return nil
}

func writeProtocolCommand(t *testing.T, writer io.Writer, command map[string]any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(command); err != nil {
		t.Fatalf("encode command: %v", err)
	}
}

type stdioFakeRunHandle struct {
	mu           sync.Mutex
	cancelled    chan struct{}
	cancelReason string
	events       chan engine.Event
}

type failingProtocolWriter struct{}

func (failingProtocolWriter) Write([]byte) (int, error) {
	return 0, errors.New("protocol output closed")
}

func (h *stdioFakeRunHandle) Next(context.Context) (*engine.StepResult, error) {
	return nil, io.EOF
}

func (h *stdioFakeRunHandle) Approve(context.Context, engine.ApprovalDecision) error {
	return nil
}

func (h *stdioFakeRunHandle) SubmitEvidence(context.Context, string, map[string]*engine.EvidenceValue) error {
	return nil
}

func (h *stdioFakeRunHandle) Cancel(_ context.Context, reason string) error {
	h.mu.Lock()
	h.cancelReason = reason
	if h.cancelled == nil {
		h.cancelled = make(chan struct{})
	}
	select {
	case <-h.cancelled:
	default:
		close(h.cancelled)
	}
	h.mu.Unlock()
	return nil
}

func (h *stdioFakeRunHandle) State() engine.RunState {
	return engine.RunState{RunID: "run-2", Status: engine.RunStatusRunning}
}

func (h *stdioFakeRunHandle) Events() <-chan engine.Event {
	return h.events
}
