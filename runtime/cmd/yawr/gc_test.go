package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestGc_NothingToDelete(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	code := gcMain([]string{"--force", "--older-than=0s"}, dir, &buf, strings.NewReader(""))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	if !contains(buf.String(), "nothing to delete") {
		t.Errorf("expected 'nothing to delete', got: %s", buf.String())
	}
}

func TestGc_DryRun(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-old",
		Status:    engine.RunStatusCompleted,
		StartedAt: now.Add(-10 * 24 * time.Hour),
		UpdatedAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var buf bytes.Buffer
	code := gcMain([]string{"--dry-run", "--older-than=1h"}, dir, &buf, strings.NewReader(""))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	if !contains(buf.String(), "dry-run") {
		t.Errorf("expected 'dry-run' in output, got: %s", buf.String())
	}
	if !contains(buf.String(), "run-old") {
		t.Errorf("expected run-old in dry-run output, got: %s", buf.String())
	}

	// Verify run directory still exists.
	runPath := filepath.Join(dir, "run-old")
	if _, err := os.Stat(runPath); os.IsNotExist(err) {
		t.Error("dry-run should not have deleted the run directory")
	}
}

func TestGc_DeletesOldRuns(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-stale",
		Status:    engine.RunStatusCompleted,
		StartedAt: now.Add(-10 * 24 * time.Hour),
		UpdatedAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var buf bytes.Buffer
	code := gcMain([]string{"--force", "--older-than=1h"}, dir, &buf, strings.NewReader(""))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	if !contains(buf.String(), "deleted 1 run(s)") {
		t.Errorf("expected 'deleted 1 run(s)', got: %s", buf.String())
	}

	// Verify run directory was deleted.
	runPath := filepath.Join(dir, "run-stale")
	if _, err := os.Stat(runPath); !os.IsNotExist(err) {
		t.Error("expected run directory to be deleted")
	}
}

func TestGc_PreservesRunning(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)
	// Running run — must never be deleted.
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-active",
		Status:    engine.RunStatusRunning,
		StartedAt: now.Add(-10 * 24 * time.Hour),
		UpdatedAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var buf bytes.Buffer
	code := gcMain([]string{"--force", "--older-than=0s"}, dir, &buf, strings.NewReader(""))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	// Nothing should be deleted since running status is excluded.
	if !contains(buf.String(), "nothing to delete") {
		t.Errorf("expected 'nothing to delete' (running must be preserved), got: %s", buf.String())
	}

	// Confirm run directory still exists.
	runPath := filepath.Join(dir, "run-active")
	if _, err := os.Stat(runPath); os.IsNotExist(err) {
		t.Error("running run must never be deleted by gc")
	}
}

func TestGc_Confirmation_Abort(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-confirm",
		Status:    engine.RunStatusCompleted,
		StartedAt: now.Add(-10 * 24 * time.Hour),
		UpdatedAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var buf bytes.Buffer
	// Send "n" to abort.
	code := gcMain([]string{"--older-than=1h"}, dir, &buf, strings.NewReader("n\n"))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	if !contains(buf.String(), "aborted") {
		t.Errorf("expected 'aborted', got: %s", buf.String())
	}

	// Run must still exist.
	runPath := filepath.Join(dir, "run-confirm")
	if _, err := os.Stat(runPath); os.IsNotExist(err) {
		t.Error("expected run to survive after abort")
	}
}

func TestGc_Confirmation_Yes(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-yes",
		Status:    engine.RunStatusFailed,
		StartedAt: now.Add(-10 * 24 * time.Hour),
		UpdatedAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var buf bytes.Buffer
	code := gcMain([]string{"--older-than=1h"}, dir, &buf, strings.NewReader("y\n"))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	if !contains(buf.String(), "deleted 1 run(s)") {
		t.Errorf("expected 'deleted 1 run(s)', got: %s", buf.String())
	}
}

// TestGc_RunningStatus_NeverDeleted explicitly verifies D-13-03: a run with
// status=running is never deleted, even when --older-than=0s and --force are both set.
func TestGc_RunningStatus_NeverDeleted(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-active",
		Status:    engine.RunStatusRunning,
		StartedAt: now.Add(-10 * 24 * time.Hour),
		UpdatedAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var buf bytes.Buffer
	code := gcMain([]string{"--force", "--older-than=0s"}, dir, &buf, strings.NewReader(""))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	if !contains(buf.String(), "nothing to delete") {
		t.Errorf("expected 'nothing to delete' when only running runs exist, got: %s", buf.String())
	}

	runPath := filepath.Join(dir, "run-active")
	if _, err := os.Stat(runPath); os.IsNotExist(err) {
		t.Error("running run must never be deleted by gc")
	}
}

// TestGc_MixedStatuses verifies that only terminal-status runs are deleted when
// a mix of completed, failed, cancelled, and running runs are present.
func TestGc_MixedStatuses(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)
	old := now.Add(-10 * 24 * time.Hour)

	runs := []engine.RunState{
		{RunID: "run-completed", Status: engine.RunStatusCompleted, StartedAt: old, UpdatedAt: old},
		{RunID: "run-failed", Status: engine.RunStatusFailed, StartedAt: old, UpdatedAt: old},
		{RunID: "run-cancelled", Status: engine.RunStatusCancelled, StartedAt: old, UpdatedAt: old},
		{RunID: "run-running", Status: engine.RunStatusRunning, StartedAt: old, UpdatedAt: old},
	}
	for _, r := range runs {
		if err := store.SaveState(ctx, r); err != nil {
			t.Fatalf("SaveState %s: %v", r.RunID, err)
		}
	}

	var buf bytes.Buffer
	code := gcMain([]string{"--force", "--older-than=1h"}, dir, &buf, strings.NewReader(""))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}
	if !contains(buf.String(), "deleted 3 run(s)") {
		t.Errorf("expected 'deleted 3 run(s)', got: %s", buf.String())
	}

	// Running run must survive.
	runPath := filepath.Join(dir, "run-running")
	if _, err := os.Stat(runPath); os.IsNotExist(err) {
		t.Error("running run must survive gc")
	}
	// Terminal runs must be deleted.
	for _, id := range []string{"run-completed", "run-failed", "run-cancelled"} {
		if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted", id)
		}
	}
}

// TestGc_StatusFilter_Running_Rejected verifies that --status=running is
// silently excluded (safety invariant), causing gc to return exitValidation
// because no valid statuses remain.
func TestGc_StatusFilter_Running_Rejected(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)
	old := now.Add(-10 * 24 * time.Hour)
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-old",
		Status:    engine.RunStatusCompleted,
		StartedAt: old,
		UpdatedAt: old,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var buf bytes.Buffer
	code := gcMain([]string{"--force", "--older-than=0s", "--status=running"}, dir, &buf, strings.NewReader(""))
	// --status=running is silently stripped; with no valid statuses left,
	// gc returns exitValidation.
	if code != exitValidation {
		t.Fatalf("expected exitValidation (%d) when --status=running, got %d; output: %s", exitValidation, code, buf.String())
	}

	// The run must be untouched.
	if _, err := os.Stat(filepath.Join(dir, "run-old")); os.IsNotExist(err) {
		t.Error("expected run-old to survive since no valid status filter remained")
	}
}

// TestGc_OlderThan_EdgeCase verifies the exclusive boundary invariant:
// only runs strictly older than the cutoff (UpdatedAt < cutoff) are deleted.
// A run that is younger than the cutoff — even by a small margin — must survive.
//
// Implementation note: we cannot reliably test UpdatedAt == cutoff because
// gcMain computes its own time.Now() independently. Instead, we test both sides:
//   - A run updated 1h ago with --older-than=2h is NEWER than the cutoff → NOT deleted.
//   - A run updated 3h ago with --older-than=2h is OLDER than the cutoff → IS deleted.
func TestGc_OlderThan_EdgeCase(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := runstore.NewDirRunStore(dir)

	now := time.Now().Truncate(time.Second)

	// "younger" run: 1h old; cutoff is 2h → younger than cutoff → NOT deleted.
	youngUpdatedAt := now.Add(-1 * time.Hour)
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-young",
		Status:    engine.RunStatusCompleted,
		StartedAt: youngUpdatedAt,
		UpdatedAt: youngUpdatedAt,
	}); err != nil {
		t.Fatalf("SaveState run-young: %v", err)
	}

	// "older" run: 3h old; cutoff is 2h → older than cutoff → IS deleted.
	oldUpdatedAt := now.Add(-3 * time.Hour)
	if err := store.SaveState(ctx, engine.RunState{
		RunID:     "run-old",
		Status:    engine.RunStatusCompleted,
		StartedAt: oldUpdatedAt,
		UpdatedAt: oldUpdatedAt,
	}); err != nil {
		t.Fatalf("SaveState run-old: %v", err)
	}

	var buf bytes.Buffer
	code := gcMain([]string{"--force", "--older-than=2h"}, dir, &buf, strings.NewReader(""))
	if code != exitSuccess {
		t.Fatalf("expected exit 0, got %d; output: %s", code, buf.String())
	}

	// Only the old run should be deleted.
	if !contains(buf.String(), "deleted 1 run(s)") {
		t.Errorf("expected 'deleted 1 run(s)', got: %s", buf.String())
	}

	// Young run (newer than cutoff) must survive.
	if _, err := os.Stat(filepath.Join(dir, "run-young")); os.IsNotExist(err) {
		t.Error("run-young (newer than cutoff) must not be deleted")
	}

	// Old run (older than cutoff) must be gone.
	if _, err := os.Stat(filepath.Join(dir, "run-old")); !os.IsNotExist(err) {
		t.Error("run-old (older than cutoff) must be deleted")
	}
}
