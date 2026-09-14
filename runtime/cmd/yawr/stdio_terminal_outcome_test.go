package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestStdioTerminalOutcomeIsNotAPreview(t *testing.T) {
	for _, large := range []bool{false, true} {
		code := strings.Repeat("Exact-code-", 1000)
		if large {
			code = strings.Repeat("x", maxStdioFrameBytes)
		}
		state := engine.RunState{RunID: "terminal", Status: engine.RunStatusCompleted, CurrentStep: "branch",
			Results: stdioResultsRecord(t, "safe"), StepResults: map[string]*engine.StepResult{
				"branch": {Status: engine.StepStatusCompleted, Output: map[string]any{"terminal": true, "outcome_category": "no_action", "outcome_code": code}},
			}}
		state.Results.Origin.NodeID = "branch"
		if err := state.Results.Seal(); err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		protocol := newStdioProtocol(strings.NewReader(""), &buffer)
		err := protocol.sendFinished(map[string]any{"type": "run.finished", "status": "completed"}, state)
		if large {
			if err == nil || buffer.Len() != 0 || protocol.terminalSent {
				t.Fatal("oversize outcome was truncated or sent")
			}
			if state.Results.Validate() != nil {
				t.Fatal("transport failure changed durable Results")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		frames := resultsFrames(t, &buffer)
		var actual string
		if err := json.Unmarshal(frames[0]["outcome_code"], &actual); err != nil {
			t.Fatal(err)
		}
		if actual != code {
			t.Fatal("terminal outcome was preview-truncated")
		}
	}
}
