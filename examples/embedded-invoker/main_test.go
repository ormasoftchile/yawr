package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestEmbeddedInvoker_FullLifecycle(t *testing.T) {
	if err := runEmbeddedExample(); err != nil {
		t.Fatalf("runEmbeddedExample failed: %v", err)
	}
}

func TestEmbeddedInvoker_ZeroInternalImports(t *testing.T) {
	cmd := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", ".")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed: %v\n%s", err, string(out))
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "github.com/ormasoftchile/yawr") && strings.Contains(line, "/internal/") {
			t.Errorf("FORBIDDEN: external module directly imported YAWR internal package: %s", line)
		}
	}
}
