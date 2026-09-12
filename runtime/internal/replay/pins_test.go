package replay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// --- buildPinIndex ---

func TestBuildPinIndex_LookupByStepID(t *testing.T) {
	pins := []schema.LockedDynamicInclude{
		{StepID: "step-a", QualifiedID: "pkg/tsg-a", AbsPath: "/path/a.yaml", FileDigest: "aaa"},
		{StepID: "step-b", QualifiedID: "pkg/tsg-b", AbsPath: "/path/b.yaml", FileDigest: "bbb"},
	}
	idx := buildPinIndex(pins)

	if len(idx) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(idx))
	}
	got, ok := idx["step-a"]
	if !ok || len(got) != 1 || got[0].QualifiedID != "pkg/tsg-a" {
		t.Errorf("step-a: unexpected entry %+v, ok=%v", got, ok)
	}
	if _, ok := idx["step-c"]; ok {
		t.Error("step-c should not be in index")
	}
}

func TestBuildPinIndex_EmptySlice(t *testing.T) {
	idx := buildPinIndex(nil)
	if len(idx) != 0 {
		t.Errorf("expected empty index, got %d entries", len(idx))
	}
}

func TestBuildPinIndexPreservesFirstRepeatedOccurrence(t *testing.T) {
	pins := []schema.LockedDynamicInclude{
		{StepID: "child", QualifiedID: "pkg/first", AbsPath: "/path/first.yaml"},
		{StepID: "child", QualifiedID: "pkg/second", AbsPath: "/path/second.yaml"},
	}
	index := buildPinIndex(pins)
	if got := index["child"]; len(got) != 2 || got[0].QualifiedID != "pkg/first" || got[1].QualifiedID != "pkg/second" {
		t.Fatalf("repeated pins = %#v, want first then second", got)
	}
}

// --- buildPinsByPath ---

func TestBuildPinsByPath_LookupByAbsPath(t *testing.T) {
	pins := []schema.LockedDynamicInclude{
		{StepID: "step-a", AbsPath: "/catalog/yawr/v1/tsg.yaml", FileDigest: "abc"},
		{StepID: "step-b", AbsPath: "/catalog/v2/tsg.yaml", FileDigest: "def"},
	}
	idx := buildPinsByPath(pins)

	got, ok := idx["/catalog/yawr/v1/tsg.yaml"]
	if !ok || got.StepID != "step-a" {
		t.Errorf("path lookup: got %+v ok=%v", got, ok)
	}
}

// --- fileDigestSHA256 ---

func TestFileDigestSHA256_KnownContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile.txt")
	content := []byte("hello yawr")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	digest, err := fileDigestSHA256(path)
	if err != nil {
		t.Fatalf("fileDigestSHA256: %v", err)
	}
	// SHA-256 of "hello yawr"
	want := "sha256:d17f5a891ec3916c45fe8561065ead0cade95c766ff9e3aae1d91da6eefca61d"
	if digest != want {
		t.Errorf("digest: got %s, want %s", digest, want)
	}
}

func TestFileDigestSHA256_MissingFile(t *testing.T) {
	_, err := fileDigestSHA256(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

// --- PinBasedIncludeLoader ---

// fakeBaseLoader is a test implementation of LazyRunbookLoader that
// records the path it was asked to load and returns a fixed flow.
type fakeBaseLoader struct {
	loadedPath string
	err        error
}

func (f *fakeBaseLoader) Load(_ context.Context, absPath string) (*internalexecutor.LoadedRunbook, error) {
	f.loadedPath = absPath
	if f.err != nil {
		return nil, f.err
	}
	return &internalexecutor.LoadedRunbook{}, nil
}

// captureWriter records trace events for assertions in tests.
type captureWriter struct {
	events []tracepkg.TraceEvent
}

func (w *captureWriter) Append(ev tracepkg.TraceEvent) error {
	w.events = append(w.events, ev)
	return nil
}
func (w *captureWriter) Close() error { return nil }

func TestPinBasedIncludeLoader_NoPin_DelegatesDirectly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tsg.yaml")
	_ = os.WriteFile(path, []byte("steps: []"), 0o644)

	base := &fakeBaseLoader{}
	loader := NewPinBasedIncludeLoader(base, map[string]schema.LockedDynamicInclude{}, nil, "run-1")

	_, err := loader.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if base.loadedPath != path {
		t.Errorf("base loader: got path %q, want %q", base.loadedPath, path)
	}
}

func TestPinBasedIncludeLoader_MatchingDigest_NoDriftEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tsg.yaml")
	_ = os.WriteFile(path, []byte("steps: []"), 0o644)

	digest, _ := fileDigestSHA256(path)
	pins := map[string]schema.LockedDynamicInclude{
		path: {StepID: "step-1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: digest},
	}
	writer := &captureWriter{}
	base := &fakeBaseLoader{}
	loader := NewPinBasedIncludeLoader(base, pins, writer, "run-1")

	if _, err := loader.Load(context.Background(), path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, ev := range writer.events {
		if ev.Kind == tracepkg.EventKindReplayDynamicIncludeDrift {
			t.Error("unexpected drift event when digests match")
		}
	}
}

func TestPinBasedIncludeLoader_ChangedDigestFailsBeforeDelegation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tsg.yaml")
	_ = os.WriteFile(path, []byte("steps: []"), 0o644)

	pins := map[string]schema.LockedDynamicInclude{
		path: {StepID: "step-1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: "stale-digest"},
	}
	writer := &captureWriter{}
	base := &fakeBaseLoader{}
	loader := NewPinBasedIncludeLoader(base, pins, writer, "run-1")

	if _, err := loader.Load(context.Background(), path); err == nil || !strings.Contains(err.Error(), "digest changed") {
		t.Fatalf("Load error = %v, want digest changed", err)
	}
	var found bool
	for _, ev := range writer.events {
		if ev.Kind == tracepkg.EventKindReplayDynamicIncludeDrift {
			found = true
		}
	}
	if !found {
		t.Error("expected replay/dynamicIncludeDrift event, none emitted")
	}
	if base.loadedPath != "" {
		t.Fatalf("changed pinned content reached base loader: %s", base.loadedPath)
	}
}

// TestPinBasedIncludeLoader_DeterministicReplay is the determinism proof:
// after a "catalog mutation" that would resolve the same ref to a different
// path, the pin-based loader still loads from the original pinned path.
func TestPinBasedIncludeLoader_DeterministicReplay(t *testing.T) {
	dir := t.TempDir()

	// Original runbook file (what the first run resolved to).
	originalPath := filepath.Join(dir, "tsg-v1.yaml")
	_ = os.WriteFile(originalPath, []byte("# v1 runbook"), 0o644)

	// Mutated file representing what a live re-resolution would pick
	// (catalog now points here instead). We do NOT put this in the pins.
	mutatedPath := filepath.Join(dir, "tsg-v2.yaml")
	_ = os.WriteFile(mutatedPath, []byte("# v2 runbook — catalog mutated"), 0o644)

	digest, _ := fileDigestSHA256(originalPath)
	pins := map[string]schema.LockedDynamicInclude{
		originalPath: {StepID: "call-tsg", QualifiedID: "contoso-tsgs/tsg-disk", AbsPath: originalPath, FileDigest: digest},
	}
	base := &fakeBaseLoader{}
	writer := &captureWriter{}
	loader := NewPinBasedIncludeLoader(base, pins, writer, "run-replay-1")

	// Replay supplies the pinned AbsPath (not the mutated path from
	// the current catalog). The loader must use it, not re-resolve.
	_, err := loader.Load(context.Background(), originalPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if base.loadedPath != originalPath {
		t.Errorf("replay re-binding: loaded %q, want pinned %q", base.loadedPath, originalPath)
	}
	// No drift event — digest matches the pin.
	for _, ev := range writer.events {
		if ev.Kind == tracepkg.EventKindReplayDynamicIncludeDrift {
			t.Errorf("unexpected drift event for unchanged file: %+v", ev)
		}
	}
}

func TestPinBasedIncludeLoader_NilBase_ReturnsError(t *testing.T) {
	loader := NewPinBasedIncludeLoader(nil, map[string]schema.LockedDynamicInclude{}, nil, "run-1")
	_, err := loader.Load(context.Background(), "/some/path.yaml")
	if err == nil {
		t.Error("expected error with nil base loader")
	}
	if !errors.Is(err, err) { // always true, just check non-nil
		t.Error("err should be non-nil")
	}
}

// TestReplayEngine_WithPins_Roundtrip verifies that WithPins attaches
// pins to the engine without modifying the returned engine pointer or
// panicking.
func TestReplayEngine_WithPins_Roundtrip(t *testing.T) {
	eng := NewReplayEngine(engine_config_for_test(), nil, nil)
	pins := []schema.LockedDynamicInclude{
		{StepID: "s1", AbsPath: "/p/a.yaml"},
	}
	ret := eng.WithPins(pins)
	if ret != eng {
		t.Error("WithPins should return the same *ReplayEngine")
	}
	if len(eng.pins) != 1 || eng.pins[0].StepID != "s1" {
		t.Errorf("pins not attached: %+v", eng.pins)
	}
}

// engine_config_for_test returns a minimal EngineConfig for structural
// tests that do not drive the engine through its full lifecycle.
func engine_config_for_test() engine.EngineConfig {
	return engine.EngineConfig{}
}
