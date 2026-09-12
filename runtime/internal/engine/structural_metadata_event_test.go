package engine_test

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/run"
)

// TestStepStartedEvent_StructuralMetadata_CollectHealth asserts that the
// engine's step/started events carry the structural metadata fields
// (parent_step_id, parent_kind, include_alias, branch_label) for the canonical
// collect-health runbook. This is the wire-format contract for downstream
// consumers (TUI, harness, SDKs).
func TestStepStartedEvent_StructuralMetadata_CollectHealth(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The collect-health runbook invokes ping/nslookup with POSIX-style
		// flags ("-c"); Windows ping uses "-n", so the captured errors do not
		// match the runbook's success branch. Tracked in yawr#34.
		t.Skip("collect-health runbook uses POSIX-only ping flags; not portable to Windows")
	}
	var (
		mu     sync.Mutex
		events []engine.Event
	)
	collect := func(e engine.Event) {
		mu.Lock()
		defer mu.Unlock()
		// Copy payload so concurrent mutation in the engine (if any) doesn't race.
		copyP := make(map[string]any, len(e.Payload))
		for k, v := range e.Payload {
			copyP[k] = v
		}
		events = append(events, engine.Event{Kind: e.Kind, RunID: e.RunID, Payload: copyP})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	handle, err := run.Start(ctx, run.Config{
		RunbookPath: collectHealthPath(),
		Client:      "engine-contract-test",
		OnEvent:     collect,
		OnSubEvent:  collect,
	})
	if err != nil {
		t.Fatalf("run.Start: %v", err)
	}

	// Drive the run to completion.
	for {
		_, nextErr := handle.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("Next: %v", nextErr)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	// Index step/started by step ID; keep first occurrence (sub-engine iterations
	// re-emit the same step IDs for each iteration with identical structural metadata).
	starts := map[string]map[string]any{}
	for _, e := range events {
		if e.Kind != "step/started" {
			continue
		}
		stepID, _ := e.Payload["step_id"].(string)
		if _, seen := starts[stepID]; seen {
			continue
		}
		starts[stepID] = e.Payload
	}

	t.Logf("step/started payloads (%d unique step IDs):", len(starts))
	for id, p := range starts {
		t.Logf("  %-20s parent=%-15v parent_kind=%-10v alias=%-6v branch=%v",
			id, p["parent_step_id"], p["parent_kind"], p["include_alias"], p["branch_label"])
	}

	// Diagnostic: list ALL step/started events in order (no dedup) to spot duplicates / cross-arm leakage.
	t.Logf("--- all step/started events (in order) ---")
	for _, e := range events {
		if e.Kind != "step/started" {
			continue
		}
		t.Logf("  %v", e.Payload)
	}

	type want struct {
		parentID     string
		parentKind   string
		includeAlias string
		branchLabel  string
	}
	expectations := map[string]want{
		"check_loop":  {parentID: "", parentKind: "", includeAlias: "", branchLabel: ""},
		"check_host":  {parentID: "check_loop", parentKind: "iterate", includeAlias: "check", branchLabel: ""},
		"dns_lookup":  {parentID: "check_host", parentKind: "include", includeAlias: "", branchLabel: ""},
		"ping_host":   {parentID: "check_host", parentKind: "include", includeAlias: "", branchLabel: ""},
		"summarize":   {parentID: "check_host", parentKind: "include", includeAlias: "", branchLabel: ""},
		"accumulate":  {parentID: "check_loop", parentKind: "iterate", includeAlias: "", branchLabel: ""},
		"show_result": {parentID: "", parentKind: "", includeAlias: "", branchLabel: ""},
		"go_ahead":    {parentID: "show_result", parentKind: "branch", includeAlias: "", branchLabel: "All checks passed"},
		// Note: include is now a real composite parent step (like branch /
		// iterate / parallel), so check_host fires step/started directly
		// and its children reference it as parent_step_id=check_host.
		// no_go is the not-taken branch arm — never runs, never fires step/started.
		"done": {parentID: "", parentKind: "", includeAlias: "", branchLabel: ""},
	}

	getStr := func(p map[string]any, key string) string {
		v, _ := p[key].(string)
		return v
	}

	for id, w := range expectations {
		p, ok := starts[id]
		if !ok {
			t.Errorf("step %q: no step/started event observed", id)
			continue
		}
		if got := getStr(p, "parent_step_id"); got != w.parentID {
			t.Errorf("step %q: parent_step_id = %q, want %q", id, got, w.parentID)
		}
		if got := getStr(p, "parent_kind"); got != w.parentKind {
			t.Errorf("step %q: parent_kind = %q, want %q", id, got, w.parentKind)
		}
		if got := getStr(p, "include_alias"); got != w.includeAlias {
			t.Errorf("step %q: include_alias = %q, want %q", id, got, w.includeAlias)
		}
		if got := getStr(p, "branch_label"); got != w.branchLabel {
			t.Errorf("step %q: branch_label = %q, want %q", id, got, w.branchLabel)
		}
	}
}
