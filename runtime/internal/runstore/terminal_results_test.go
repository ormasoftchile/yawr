package runstore

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestTerminalResultsIntentRoundTripAndSingleRecord(t *testing.T) {
	store := NewDirRunStore(t.TempDir())
	defer store.Close()
	result := &engine.StepResult{StepID: "end", Status: engine.StepStatusCompleted, TerminalResults: true,
		Output: map[string]any{"terminal": true, "outcome_category": "escalated", "outcome_code": "Failover-Exact_Code"}}
	snapshots, err := store.snapshotStepResultsV2("terminal", map[string]*engine.StepResult{"0": result})
	if err != nil {
		t.Fatal(err)
	}
	loader := &stateValueLoader{}
	restored, err := loader.restoreStepResults(snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if !restored["0"].TerminalResults || restored["0"].Output["outcome_code"] != "Failover-Exact_Code" {
		t.Fatal("terminal publication request/outcome lost at checkpoint")
	}
	state := engine.RunState{StepResults: restored}
	if len(typedStateValues(state)) != 0 {
		t.Fatal("request persisted as a publication")
	}
	record := &engine.RunResults{PublicationID: "one-publication"}
	result.Results = record
	state.Results = record
	state.StepResults = map[string]*engine.StepResult{"end": result}
	if len(typedStateValues(state)) != 1 {
		t.Fatal("root and step duplicated durable record")
	}
}

func TestTerminalResultsFrameOriginRequiresCommittedReturn(t *testing.T) {
	record := &engine.RunResults{Digest: "sealed", Origin: engine.ResultsOrigin{NodeID: "end"}}
	result := &engine.StepResult{StepID: "end", Status: engine.StepStatusCompleted, Results: record,
		TerminalResults: true, Output: map[string]any{"terminal": true}}
	frame := &engine.ExecutionFrameState{StepCount: 2, StepIDs: []string{"end", "unreachable"},
		Results: map[string]*engine.StepResult{"0": result, "1": {StepID: "unreachable", Status: engine.StepStatusSkipped}}}
	if !validFramePublicationOrigin(frame, record) {
		t.Fatal("terminal return origin rejected")
	}
	result.TerminalResults = false
	if validFramePublicationOrigin(frame, record) {
		t.Fatal("non-final ordinary Results accepted")
	}
	result.TerminalResults = true
	result.Status = engine.StepStatusFailed
	if validFramePublicationOrigin(frame, record) {
		t.Fatal("failed terminal return accepted")
	}
	result.Status = engine.StepStatusCompleted
	result.Results = nil
	if validFramePublicationOrigin(frame, record) {
		t.Fatal("uncommitted terminal return accepted")
	}
}
