package testutil

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// update is set via -update flag to regenerate golden traces.
var update = flag.Bool("update", false, "regenerate golden trace files")

// goldenDir is the directory where golden trace files are stored.
const goldenDir = "testdata/golden"

// AssertGoldenTrace compares actualEvents against the golden file testdata/golden/<name>.jsonl.
// When run with -update, the golden file is written instead of compared.
// Call NormalizeTrace before passing events to remove non-deterministic fields.
func AssertGoldenTrace(t testing.TB, name string, events []trace.TraceEvent) {
	t.Helper()

	path := filepath.Join(goldenDir, name+".jsonl")

	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("golden: mkdir %s: %v", goldenDir, err)
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("golden: create %s: %v", path, err)
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				t.Fatalf("golden: encode event: %v", err)
			}
		}
		t.Logf("golden: updated %s (%d events)", path, len(events))
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden: read %s: %v (run with -update to create)", path, err)
	}

	var want []trace.TraceEvent
	dec := json.NewDecoder(bytes.NewReader(raw))
	for dec.More() {
		var ev trace.TraceEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("golden: decode %s: %v", path, err)
		}
		want = append(want, ev)
	}

	if len(want) != len(events) {
		t.Errorf("golden %s: got %d events, want %d", name, len(events), len(want))
		return
	}
	for i, got := range events {
		gotB, _ := json.Marshal(got)
		wantB, _ := json.Marshal(want[i])
		if string(gotB) != string(wantB) {
			t.Errorf("golden %s event[%d]:\n  got:  %s\n  want: %s", name, i, gotB, wantB)
		}
	}
}

// NormalizeTrace replaces non-deterministic fields (UUIDs, timestamps, PIDs)
// with stable placeholders for golden comparison.
func NormalizeTrace(events []trace.TraceEvent) []trace.TraceEvent {
	out := make([]trace.TraceEvent, len(events))
	for i, ev := range events {
		ev.Timestamp = "<timestamp>"
		if ev.Sequence != 0 {
			ev.Sequence = int64(i + 1) // rebase sequences to 1-based index
		}
		out[i] = ev
	}
	return out
}
