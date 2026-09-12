package runstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestCheckpointBudgetPendingTraceBoundary(t *testing.T) {
	for _, withFrame := range []bool{false, true} {
		t.Run(fmt.Sprintf("frame-%v", withFrame), func(t *testing.T) {
			store := NewDirRunStore(t.TempDir())
			ctx := context.Background()
			const runID = "budget-roundtrip"
			lease, err := store.AcquireRunLease(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			large := strings.Repeat("x", 70<<10)
			state := engine.RunState{
				RunID: runID, WriterEpoch: lease.Epoch(), Status: engine.RunStatusRunning,
				CheckpointSequence: 1, CommittedTraceSequence: 1,
				Vars: map[string]any{},
				StepResults: map[string]*engine.StepResult{
					"root": {StepID: "root", Status: engine.StepStatusCompleted,
						Output: map[string]any{"blob": large}, Vars: map[string]any{"captured": false}},
				},
				PendingTraceEvents: []engine.Event{{
					EventID: "commit-1", RunID: runID, Sequence: 1, Kind: string(trace.EventKindExecutionCommitted),
					Timestamp: "2026-09-05T00:00:00Z",
					Payload:   map[string]any{"blob": large, "zero": 0},
				}},
			}
			extra := 4 // root output/capture and two pending trace payload entries
			if withFrame {
				id := engine.InteractionPayloadDigest([]byte("budget-frame"))
				state.ExecutionFrames = map[string]*engine.ExecutionFrameState{
					id: {
						SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: id,
						WriterEpoch: lease.Epoch(), ParentQualifiedNodeID: "choose", ParentStepID: "choose",
						Kind: "branch", CallPath: []engine.DebugCallFrame{{StepID: "choose"}},
						BranchLabel: "active", Invocation: 1,
						DefinitionDigest: engine.InteractionPayloadDigest([]byte("child")),
						StepCount:        1, StepIDs: []string{"child"}, NextStepIndex: 1,
						WorkingVars: map[string]any{"blob": large},
						Results: map[string]*engine.StepResult{
							"0": {StepID: "child", Status: engine.StepStatusCompleted,
								Output: map[string]any{"blob": large}, Vars: map[string]any{"captured": false}},
						},
						Status: engine.ExecutionFrameStatusActive, StartedAt: "2026-09-05T00:00:00Z",
					},
				}
				extra += 3
			}
			for i := 0; i < maxStoredValueEntries-extra; i++ {
				state.Vars[fmt.Sprintf("v-%04d", i)] = nil
			}
			if err := store.SaveState(ctx, state); err != nil {
				t.Fatalf("4096 entries must save: %v", err)
			}
			before, err := store.LoadState(ctx, runID)
			if err != nil {
				t.Fatalf("4096 entries must reload: %v", err)
			}
			if before.PendingTraceEvents[0].Payload["blob"] != large ||
				before.PendingTraceEvents[0].Payload["zero"] != json.Number("0") ||
				before.StepResults["root"].Vars["captured"] != false {
				t.Fatal("typed payload changed")
			}
			blobDir := filepath.Join(store.RunDir(runID), "blobs")
			blobsBefore, err := os.ReadDir(blobDir)
			if err != nil || len(blobsBefore) != 1 {
				t.Fatalf("expected one deduplicated blob: %v %v", blobsBefore, err)
			}
			state.CheckpointSequence++
			state.PendingTraceEvents[0].Payload["overflow"] = strings.Repeat("new-blob", 10000)
			if err := store.SaveState(ctx, state); err == nil || !strings.Contains(err.Error(), "value count") {
				t.Fatalf("4097 including pending event must fail before publication: %v", err)
			}
			after, err := store.LoadState(ctx, runID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected save changed previous checkpoint: %v", err)
			}
			blobsAfter, err := os.ReadDir(blobDir)
			if err != nil || len(blobsAfter) != len(blobsBefore) {
				t.Fatalf("rejected save wrote blobs: %v %v", blobsAfter, err)
			}
			if _, err := os.Stat(filepath.Join(store.RunDir(runID), "snapshots", "checkpoint-00000000000000000002.json")); !os.IsNotExist(err) {
				t.Fatalf("rejected checkpoint published: %v", err)
			}

			// Model a checkpoint produced by the old save counter: Load must
			// retain its existing inclusive limit, not grandfather unsafe data.
			path := filepath.Join(store.RunDir(runID), "snapshots", "checkpoint-00000000000000000001.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot runStateSnapshotV1
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot.DurablePendingTraceEvents[0].DurablePayload["overflow"] = storedJSONValueV1{Inline: json.RawMessage(`0`)}
			data, err = json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadState(ctx, runID); err == nil || !strings.Contains(err.Error(), "value count") {
				t.Fatalf("load accepted 4097 entries: %v", err)
			}
		})
	}
}

func TestCheckpointBudgetSharedByteBoundary(t *testing.T) {
	budget := stateValueBudget{}
	if err := budget.addEntries(4096); err != nil {
		t.Fatal(err)
	}
	if err := budget.addEntries(1); err == nil {
		t.Fatal("accepted 4097 entries")
	}
	if err := budget.addBytes(maxCheckpointExpandedBytes); err != nil {
		t.Fatal(err)
	}
	if err := budget.addBytes(1); err == nil {
		t.Fatal("accepted expanded bytes above limit")
	}
}

func TestCheckpointBudgetPendingTraceBlobExpansion(t *testing.T) {
	// Preflight charges each reference, even when trace and variables share
	// the same content-addressed blob. It need not inflate the blob to reject.
	value := storedJSONValueV1{Blob: &stateBlobRefV1{
		Digest: "sha256:" + strings.Repeat("a", 64), Encoding: "json+gzip",
		UncompressedSize: maxCheckpointExpandedBytes / 2,
	}}
	loader := newStateValueLoader(NewDirRunStore(t.TempDir()), "budget-blobs")
	vars := map[string]storedJSONValueV1{"blob": value}
	events := []pendingTraceEventSnapshotV1{{DurablePayload: map[string]storedJSONValueV1{"blob": value}}}
	if err := loader.preflight(vars, nil, nil, events); err != nil {
		t.Fatalf("exact expanded-byte boundary: %v", err)
	}
	events[0].DurablePayload["one-more-byte"] = storedJSONValueV1{Inline: json.RawMessage(`0`)}
	if err := loader.preflight(vars, nil, nil, events); err == nil || !strings.Contains(err.Error(), "expanded data") {
		t.Fatalf("pending payload byte expansion not charged: %v", err)
	}
}
