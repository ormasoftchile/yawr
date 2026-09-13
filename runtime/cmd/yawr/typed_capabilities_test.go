package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestTypedPublicCapabilityQueries(t *testing.T) {
	root := findRepoRoot(t)
	for _, test := range []struct {
		args    []string
		version string
	}{
		{[]string{"presentation", "capabilities", "--v3"}, "presentation-capabilities/v3"},
		{[]string{"authoring", "capabilities", "--v3"}, "authoring-capabilities/v3"},
	} {
		body, stderr, err := presentationDirectCLI(t, root, test.args...)
		if err != nil || stderr != "" {
			t.Fatal(err, stderr)
		}

		var caps map[string]any
		if json.Unmarshal(body, &caps) != nil || caps["schema_version"] != test.version {
			t.Fatal("wrong strict capability envelope")
		}
	}

	body, stderr, err := presentationDirectCLI(t, root, "presentation", "capabilities", "--v99")
	if err == nil || len(body) != 0 || !strings.Contains(stderr, "unsupported-version") {
		t.Fatal("unknown version did not fail closed")
	}
	body, stderr, err = presentationDirectCLI(t, root, "run", "--stdio", "--require-capabilities", "not-supported/v1", "must-not-be-read.yaml")
	if err == nil || len(body) != 0 || !strings.Contains(stderr, "unsupported-capability") {
		t.Fatal("unsupported capability reached runbook execution")
	}
}

func TestFileOnlyCapabilityMatchesBackend(t *testing.T) {
	root := findRepoRoot(t)
	body, stderr, err := presentationDirectCLI(t, root, "presentation", "capabilities", "--v3")
	if err != nil || stderr != "" {
		t.Fatal(err, stderr)
	}
	advertised := bytes.Contains(body, []byte(`"yawr.file-only-subprocess/v1"`))
	if advertised != toolpkg.FileOnlySubprocessAvailable() {
		t.Fatalf("advertised=%v backend=%v body=%s", advertised, toolpkg.FileOnlySubprocessAvailable(), body)
	}
}
