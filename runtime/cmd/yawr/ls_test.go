package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func makeRunDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

func seedRun(t *testing.T, runDir string, state engine.RunState) {
	t.Helper()
	ctx := context.Background()
	store := runstore.NewDirRunStore(runDir)
	if err := store.SaveState(ctx, state); err != nil {
		t.Fatalf("seedRun SaveState: %v", err)
	}
}

func TestLs_Empty(t *testing.T) {
	dir := makeRunDir(t)
	var buf bytes.Buffer
	code := lsMain([]string{}, dir, &buf)
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d", code)
	}
	got := buf.String()
	if got != "no runs found\n" {
		t.Errorf("expected 'no runs found', got %q", got)
	}
}

func TestLs_WithRuns(t *testing.T) {
	dir := makeRunDir(t)
	now := time.Now().Truncate(time.Second)
	seedRun(t, dir, engine.RunState{
		RunID:       "run-001",
		RunbookPath: "deploy.yaml",
		Status:      engine.RunStatusCompleted,
		StartedAt:   now.Add(-10 * time.Minute),
		UpdatedAt:   now.Add(-5 * time.Minute),
	})

	var buf bytes.Buffer
	code := lsMain([]string{}, dir, &buf)
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	got := buf.String()
	if got == "no runs found\n" {
		t.Fatal("expected run listing, got 'no runs found'")
	}
	if !contains(got, "run-001") {
		t.Errorf("expected run-001 in output, got: %s", got)
	}
	if !contains(got, "completed") {
		t.Errorf("expected 'completed' in output, got: %s", got)
	}
}

func TestLs_FilterStatus(t *testing.T) {
	dir := makeRunDir(t)
	now := time.Now().Truncate(time.Second)
	seedRun(t, dir, engine.RunState{
		RunID:     "run-completed",
		Status:    engine.RunStatusCompleted,
		StartedAt: now.Add(-10 * time.Minute),
	})
	seedRun(t, dir, engine.RunState{
		RunID:     "run-failed",
		Status:    engine.RunStatusFailed,
		StartedAt: now.Add(-10 * time.Minute),
	})

	var buf bytes.Buffer
	code := lsMain([]string{"-status=completed"}, dir, &buf)
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d", code)
	}
	got := buf.String()
	if !contains(got, "run-completed") {
		t.Errorf("expected run-completed in output, got: %s", got)
	}
	if contains(got, "run-failed") {
		t.Errorf("expected run-failed to be filtered out, got: %s", got)
	}
}

func TestLs_OutputJSON(t *testing.T) {
	dir := makeRunDir(t)
	now := time.Now().Truncate(time.Second)
	seedRun(t, dir, engine.RunState{
		RunID:     "run-json",
		Status:    engine.RunStatusCompleted,
		StartedAt: now.Add(-10 * time.Minute),
	})

	var buf bytes.Buffer
	code := lsMain([]string{"-output=json"}, dir, &buf)
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d", code)
	}
	var out []engine.RunState
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("json parse: %v; output: %s", err, buf.String())
	}
	if len(out) != 1 || out[0].RunID != "run-json" {
		t.Errorf("unexpected JSON output: %v", out)
	}
}

func TestLs_SinceFilter(t *testing.T) {
	dir := makeRunDir(t)
	now := time.Now().Truncate(time.Second)
	// Old run — should be filtered out.
	seedRun(t, dir, engine.RunState{
		RunID:     "run-old",
		Status:    engine.RunStatusCompleted,
		StartedAt: now.Add(-48 * time.Hour),
	})
	// Recent run — should appear.
	seedRun(t, dir, engine.RunState{
		RunID:     "run-new",
		Status:    engine.RunStatusCompleted,
		StartedAt: now.Add(-10 * time.Minute),
	})

	var buf bytes.Buffer
	code := lsMain([]string{"-since=24h"}, dir, &buf)
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d", code)
	}
	got := buf.String()
	if contains(got, "run-old") {
		t.Errorf("expected run-old to be filtered by --since, got: %s", got)
	}
	if !contains(got, "run-new") {
		t.Errorf("expected run-new in output, got: %s", got)
	}
}

func TestLs_InvalidOutput(t *testing.T) {
	dir := makeRunDir(t)
	var buf bytes.Buffer
	code := lsMain([]string{"-output=xml"}, dir, &buf)
	if code == exitSuccess {
		t.Fatal("expected non-zero exit for invalid output format")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
