package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type preStartFailingStore struct {
	*runstore.DirRunStore
	err error
}

func (s *preStartFailingStore) WriteTrace(ctx context.Context, runID string, event enginepkg.Event) error {
	if event.Kind == "catalog/frozen" {
		return s.err
	}
	return s.DirRunStore.WriteTrace(ctx, runID, event)
}

func TestPreStartTracePersistenceAndOwnership(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "write-failure"}[fail], func(t *testing.T) {
			ctx := context.Background()
			store := runstore.NewDirRunStore(t.TempDir())
			defer store.Close()
			cfg := makeTestConfig()
			cfg.Store = store
			registry := newFakeExecutorRegistry()
			registry.Register("noop", &passThroughExecutor{})
			cfg.Executors = registry
			writer := &fakeTraceWriter{}
			cfg.TraceWriter = writer
			prefix := []enginepkg.Event{{RunID: "prestart", Sequence: 1, EventID: "catalog-event",
				Kind: "catalog/frozen", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				Payload: map[string]any{"catalogDigest": "frozen"}}}
			plan := &enginepkg.ExecutionPlan{RunID: "prestart", RunbookPath: "root.runbook.yaml",
				Metadata: enginepkg.PlanMetadata{RunbookID: "root"},
				Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}}}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			writeErr := errors.New("prestart append failed")
			if fail {
				cfg.Store = &preStartFailingStore{DirRunStore: store, err: writeErr}
			}
			eng := New(cfg)
			handle, err := eng.Start(ctx, plan, enginepkg.RunOptions{PreStartEvents: prefix})
			if fail {
				if handle != nil || !errors.Is(err, enginepkg.ErrTraceCommit) || !errors.Is(err, writeErr) {
					t.Fatalf("write error swallowed: %v", err)
				}
				lease, err := store.AcquireRunLease(ctx, "prestart")
				if err != nil {
					t.Fatalf("failed start leaked writer lease: %v", err)
				}
				if err := lease.Release(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// The caller's prefix must not be retained by reference or emitted
			// again to the configured live writer.
			prefix[0].Payload["catalogDigest"] = "mutated"
			for {
				_, err := handle.Next(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			events, err := internaltrace.NewJSONLReader(store.TracePath("prestart")).ReadAllStrict(ctx)
			if err != nil || len(events) < 2 || events[0].EventID != "catalog-event" || events[1].Sequence != 2 {
				t.Fatalf("incomplete prefix: %v %v", events, err)
			}
			if string(events[0].Payload) != `{"catalogDigest":"frozen"}` {
				t.Fatal("prefix shares caller mutation")
			}
			for _, event := range writer.collect() {
				if event.EventID == "catalog-event" {
					t.Fatal("prestart duplicated to live sink")
				}
			}
			if _, err := eng.Resume(ctx, "prestart", enginepkg.RunOptions{PreStartEvents: prefix}); !errors.Is(err, enginepkg.ErrTraceCommit) {
				t.Fatalf("resume accepted prestart replay: %v", err)
			}
		})
	}
}

func TestPreStartTraceRejectsInvalidPrefix(t *testing.T) {
	valid := enginepkg.Event{RunID: "run", EventID: "event", Sequence: 1, Kind: "catalog/frozen",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, mutate := range []func(*enginepkg.Event){
		func(e *enginepkg.Event) { e.Sequence = 2 },
		func(e *enginepkg.Event) { e.RunID = "other" },
		func(e *enginepkg.Event) { e.EventID = "" },
		func(e *enginepkg.Event) { e.Timestamp = "" },
		func(e *enginepkg.Event) { e.Kind = "provider/diagnostic" },
		func(e *enginepkg.Event) { e.Kind = "package/resolved" },
	} {
		event := valid
		mutate(&event)
		if !errors.Is(validatePreStartEvents("run", []enginepkg.Event{event}), enginepkg.ErrTraceCommit) {
			t.Fatalf("invalid prefix accepted: %+v", event)
		}
	}
	second := valid
	second.Sequence = 2
	valid.Kind = "package/resolved"
	if validatePreStartEvents("run", []enginepkg.Event{valid, second}) == nil {
		t.Fatal("duplicate identity accepted")
	}
}

type preStartAheadWriter struct{ fakeTraceWriter }

func (*preStartAheadWriter) LastSequence(string) int64 { return 2 }

func TestPreStartTraceRejectsMissingLinkBeforeLease(t *testing.T) {
	root := t.TempDir()
	cfg := makeTestConfig()
	cfg.Store = runstore.NewDirRunStore(root)
	cfg.TraceWriter = &preStartAheadWriter{}
	plan := &enginepkg.ExecutionPlan{RunID: "run", RunbookPath: "root.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{ID: "noop", Kind: "noop", Spec: &schema.NoopSpec{}}}}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	prefix := []enginepkg.Event{{RunID: "run", EventID: "one", Sequence: 1, Kind: "catalog/frozen",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}}
	if _, err := New(cfg).Start(context.Background(), plan, enginepkg.RunOptions{PreStartEvents: prefix}); !errors.Is(err, enginepkg.ErrTraceCommit) {
		t.Fatalf("missing prefix linkage accepted: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid prefix acquired lease/created artifacts: %v %v", entries, err)
	}
}
