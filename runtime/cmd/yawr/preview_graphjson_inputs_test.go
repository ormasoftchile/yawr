package main

// GraphJSON input parity regression coverage.
// regression test: `yawr preview --format graphjson` must emit the
// declared inputs[] DTO (AR-CE-2) on the real CLI path, not merely on the
// graphdoc.Document Go type. This exercises cmd/yawr/preview.go's actual
// `case "graphjson"` branch end-to-end, which is exactly what the
// rejected revision never did (B-1: "the DTO never reaches any client").

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func captureStdoutForPreview(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String(), code
}

// CE-D-01/CE-D-02/CE-D-03 against the real `yawr preview --format
// graphjson` binary path.
func TestPreview_GraphJSON_CarriesDeclaredInputs(t *testing.T) {
	dir := t.TempDir()
	rbPath := filepath.Join(dir, "enum.runbook.yaml")
	writeFile(t, rbPath, `apiVersion: yawr.runbook/v1
id: enum-fixture
name: enum-fixture
inputs:
  env_name:
    type: string
    required: true
    enum: ["staging", "prod"]
  free_text:
    type: string
    required: false
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	out, code := captureStdoutForPreview(t, func() int {
		return runPreview([]string{"--format", "graphjson", rbPath})
	})
	if code != exitSuccess {
		t.Fatalf("runPreview exit=%d, want %d; output: %s", code, exitSuccess, out)
	}

	var doc struct {
		Inputs []struct {
			Name            string   `json:"name"`
			Type            string   `json:"type"`
			Required        bool     `json:"required"`
			Enum            []string `json:"enum"`
			EnumRedacted    bool     `json:"enumRedacted"`
			EnumMemberCount int      `json:"enumMemberCount"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal graphjson output: %v\noutput: %s", err, out)
	}
	if len(doc.Inputs) == 0 {
		t.Fatalf("graphjson output has no \"inputs\" key at all; raw output: %s", out)
	}

	var envDecl, freeDecl = -1, -1
	for i, d := range doc.Inputs {
		if d.Name == "env_name" {
			envDecl = i
		}
		if d.Name == "free_text" {
			freeDecl = i
		}
	}
	if envDecl == -1 {
		t.Fatalf("env_name decl missing from graphjson inputs[]: %s", out)
	}
	if freeDecl == -1 {
		t.Fatalf("free_text decl missing from graphjson inputs[]: %s", out)
	}

	// CE-D-01 / CE-D-02: declared order preserved verbatim, never sorted.
	if got, want := doc.Inputs[envDecl].Enum, []string{"staging", "prod"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("env_name.enum = %v, want %v (declared order)", got, want)
	}

	// CE-D-03: unconstrained input carries no enum key at all.
	if !bytes.Contains([]byte(out), []byte(`"free_text"`)) {
		t.Fatalf("expected free_text present: %s", out)
	}
	if doc.Inputs[freeDecl].Enum != nil {
		t.Fatalf("free_text.enum = %v, want nil/absent", doc.Inputs[freeDecl].Enum)
	}
	// Assert the raw text has no "enum" key on the free_text object
	// specifically (not just absent from the decoded struct, which
	// would also be true for `"enum":[]`).
	if bytes.Contains([]byte(out), []byte(`"enum": []`)) || bytes.Contains([]byte(out), []byte(`"enum":[]`)) {
		t.Fatalf("graphjson output must never encode enum as [], got: %s", out)
	}
}

// Redacted enum inputs must reach the CLI's graphjson output with the
// member list omitted and enumMemberCount present (C1, CE-T-03 analogue
// for the CLI transport).
func TestPreview_GraphJSON_RedactedInputs_NoMemberLeak(t *testing.T) {
	dir := t.TempDir()
	rbPath := filepath.Join(dir, "enum-redacted.runbook.yaml")
	writeFile(t, rbPath, `apiVersion: yawr.runbook/v1
id: enum-redacted-fixture
name: enum-redacted-fixture
governance:
  redact:
    - pattern: "inputs\\.secret_env"
      replace: "<redacted>"
inputs:
  secret_env:
    type: string
    required: true
    default: prod
    enum: ["prod", "staging", "canary"]
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	out, code := captureStdoutForPreview(t, func() int {
		return runPreview([]string{"--format", "graphjson", rbPath})
	})
	if code != exitSuccess {
		t.Fatalf("runPreview exit=%d, want %d; output: %s", code, exitSuccess, out)
	}

	for _, member := range []string{"prod", "staging", "canary"} {
		if bytes.Contains([]byte(out), []byte(`"`+member+`"`)) {
			t.Fatalf("redacted graphjson output must never leak member %q: %s", member, out)
		}
	}
	if !bytes.Contains([]byte(out), []byte(`"enumRedacted": true`)) && !bytes.Contains([]byte(out), []byte(`"enumRedacted":true`)) {
		t.Fatalf("expected enumRedacted:true in redacted output: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte(`"enumMemberCount": 3`)) && !bytes.Contains([]byte(out), []byte(`"enumMemberCount":3`)) {
		t.Fatalf("expected enumMemberCount:3 in redacted output: %s", out)
	}
}
