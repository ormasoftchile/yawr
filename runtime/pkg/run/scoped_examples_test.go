package run

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestScopedSDKExamplesRunFromCapturedEmbeddedSources(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(cwd)), "examples", "dependency-scopes")
	for _, name := range []string{"static-and-lazy", "parallel", "dynamic", "dynamic-parallel", "dynamic-through-tool"} {
		t.Run(name, func(t *testing.T) {
			for _, profile := range []*schema.RuntimeProfile{nil, {
				APIVersion: "yawr.runtime-profile/v1", ID: "frozen-profile", Context: "test", Attendance: "unattended",
			}} {
				t.Run(map[bool]string{false: "default", true: "frozen-profile"}[profile != nil], func(t *testing.T) {
					files := fstest.MapFS{}
					if err := fs.WalkDir(os.DirFS(root), ".", func(path string, entry fs.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if entry.IsDir() {
							return nil
						}
						data, err := fs.ReadFile(os.DirFS(root), path)
						if err != nil {
							return err
						}
						files[path] = &fstest.MapFile{Data: data}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					var mu sync.Mutex
					observed := map[string]int{}
					onEvent := func(event engine.Event) {
						if event.Kind != "step/completed" {
							return
						}
						id, _ := event.Payload["step_id"].(string)
						mu.Lock()
						observed[id]++
						mu.Unlock()
					}
					handle, err := Start(context.Background(), Config{
						RunbookFS: files, RunbookName: name + ".runbook.yaml", WorkspaceRoot: t.TempDir(),
						Output: io.Discard, OnEvent: onEvent, OnSubEvent: onEvent, Profile: profile,
					})
					if err != nil {
						t.Fatal(err)
					}
					for path := range files {
						delete(files, path)
					}
					for {
						_, err := handle.Next(context.Background())
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					if handle.State().Status != engine.RunStatusCompleted {
						t.Fatalf("run status = %s", handle.State().Status)
					}
					mu.Lock()
					defer mu.Unlock()
					if observed["verify_left"] == 0 || observed["verify_right"] == 0 {
						t.Fatalf("children did not execute their own marker assertions: %v", observed)
					}
					wantLeft := 1
					if name == "dynamic" {
						wantLeft = 2
					}
					total := 0
					for _, count := range observed {
						total += count
					}
					expected := map[string]int{"static-and-lazy": 14, "parallel": 11, "dynamic": 15, "dynamic-parallel": 11, "dynamic-through-tool": 11}[name]
					if observed["verify_left"] != wantLeft || observed["verify_right"] != 1 || total != expected {
						t.Fatalf("completion stream lost or duplicated scoped steps: %v (total %d, want %d)", observed, total, expected)
					}
				})
			}
		})
	}
}
