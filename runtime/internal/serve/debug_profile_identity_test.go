package serve

import (
	"os"
	"path/filepath"
	"testing"

	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

func TestDebugProfileStateUsesYawrDirectory(t *testing.T) {
	const profile = `version: yawr.debug-profile/v1
name: Saved
root: {ref: root.runbook.yaml, id: root}
created_against: plan-hash
overrides:
  - target: {step: inspect, phase: after}
    set: {status: completed}
`
	metadata := debugProfileMetadata{rootRef: "root.runbook.yaml", rootID: "root"}
	t.Run("reads saved profile", func(t *testing.T) {
		root := t.TempDir()
		writeDebugIdentityFile(t, filepath.Join(root, ".yawr", "debug-profiles", "saved.yaml"), profile)
		server := &Server{cfg: servepkg.ServerConfig{WorkspaceRoot: root}}
		got, err := server.readDebugProfiles(metadata)
		if err != nil || len(got) != 1 {
			t.Fatalf("readDebugProfiles() = %#v, %v", got, err)
		}
	})
	t.Run("writes yawr profile", func(t *testing.T) {
		root := t.TempDir()
		server := &Server{cfg: servepkg.ServerConfig{WorkspaceRoot: root}}
		if err := server.writeDebugProfile("saved", []byte(profile)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, ".yawr", "debug-profiles", "saved.yaml")); err != nil {
			t.Fatal(err)
		}
	})
}

func writeDebugIdentityFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
