package cmd_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/cmd/internal/testworkspace"
)

func TestAuthoringWorkspaceRejectsSecondOwnerForArbitraryDirectoryName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared-authoring-state")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := testworkspace.Claim(root, "cmd/yawr"); err != nil {
		t.Fatal(err)
	}
	if err := testworkspace.Claim(root, "cmd/yawr"); !errors.Is(err, testworkspace.ErrAlreadyOwned) {
		t.Fatalf("second owner error = %v, want %v", err, testworkspace.ErrAlreadyOwned)
	}
}

func TestAuthoringWorkspaceDoesNotClassifySharedReadOnlyFixtures(t *testing.T) {
	sandbox := t.TempDir()
	fixture := filepath.Join(sandbox, "fixtures")
	workspace := filepath.Join(sandbox, "workspace")
	if err := os.MkdirAll(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "canonical.json"), []byte("{}\n"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := testworkspace.Claim(workspace, "cmd/yawr"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture, testworkspace.MarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only fixture was classified as an owned workspace: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(fixture, "canonical.json"))
	if err != nil || string(data) != "{}\n" {
		t.Fatalf("shared fixture changed: %q, %v", data, err)
	}
}
