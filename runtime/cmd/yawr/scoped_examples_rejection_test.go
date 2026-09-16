package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopedExampleRejectionsPrecedeEveryDispatch(t *testing.T) {
	root := filepath.Join(findRepoRoot(t), "examples", "dependency-scopes")
	for _, fixture := range []struct {
		name string
		code string
	}{
		{"parent-private-alias", "SCOPE-001"},
		{"conflicting-requirements", "PKG-002"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			artifacts := t.TempDir()
			command := y1Command(t, "run", "--stdio", "--run-dir", filepath.Join(artifacts, "runs"),
				"--trace", filepath.Join(artifacts, "trace.jsonl"),
				filepath.Join(root, "rejections", fixture.name+".runbook.yaml"))
			command.Dir = root
			var stderr bytes.Buffer
			command.Stderr = &stderr
			output, err := command.Output()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != exitValidation || !strings.Contains(stderr.String(), fixture.code) {
				t.Fatalf("expected preflight %s: exit=%v stderr=%s", fixture.code, err, stderr.String())
			}
			for _, line := range bytes.Split(bytes.TrimSpace(output), []byte{'\n'}) {
				if len(line) == 0 {
					continue
				}
				var frame struct {
					Event *struct {
						Kind string `json:"kind"`
					} `json:"event"`
				}
				if err := json.Unmarshal(line, &frame); err != nil {
					t.Fatal(err)
				}
				if frame.Event != nil && frame.Event.Kind == "step/started" {
					t.Fatal("invalid dependency example dispatched before preflight refusal")
				}
			}
		})
	}
}
