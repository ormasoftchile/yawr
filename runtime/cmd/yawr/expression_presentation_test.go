package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
)

func TestExpressionCLIProtocol(t *testing.T) {
	root := findRepoRoot(t)
	path := filepath.Join(root, "not-on-disk.runbook.yaml")
	req := presentation.ExpressionResolveRequest{SchemaVersion: presentation.ExpressionSchemaVersion, RequestID: "expression-cli", Context: presentation.Context{ProjectRoot: root, Generation: 7}, Document: presentation.Buffer{URI: presentation.FileURI(path), Path: path, Version: 3, Text: "flow: [{step: {type: noop, when: 'count >= 2'}}]"}, Overlays: []presentation.Buffer{}}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{presentation.ExpressionSchemaVersion, "expression-resolve/v99"} {
		req.SchemaVersion = version
		input, _ := json.Marshal(req)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPresentationDirectCLIProcess$", "--", "presentation", "expressions", "resolve", "--stdio")
		cmd.Env = append(os.Environ(), "YAWR_PRESENTATION_DIRECT_CHILD=1")
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		cancel()
		if version != presentation.ExpressionSchemaVersion {
			if err == nil || len(out) != 0 {
				t.Fatal("future request accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err, stderr.String())
		}
		var reply presentation.ExpressionResolveReply
		if json.Unmarshal(out, &reply) != nil || reply.Status != "resolved" || len(reply.Regions) != 1 || reply.Regions[0].YAMLPath != "/flow/0/step/when" {
			t.Fatal(string(out))
		}
	}
	data, stderr, err := presentationDirectCLI(t, root, "presentation", "expressions", "capabilities")
	if err != nil {
		t.Fatal(err, stderr)
	}
	var actual presentation.ExpressionCapabilities
	if json.Unmarshal(data, &actual) != nil || actual.GrammarVersion != presentation.ExpressionGrammarVersion || actual.MaxValueCodeUnits != 32768 || actual.MaxRegions != 4096 || actual.MaxTokens != 65536 {
		t.Fatal(string(data))
	}
	old, stderr, err := presentationDirectCLI(t, root, "presentation", "capabilities")
	if err != nil || bytes.Contains(old, []byte("grammar_version")) {
		t.Fatal("old capabilities changed", err, stderr, string(old))
	}
}

func TestExpressionStaticAndDynamicSourceDeleted(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: expressions, version: "1.0.0"}
exports:
  runbooks: [{id: child, path: child.runbook.yaml}]
`)
	writeFile(t, filepath.Join(root, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\nrequires: [{package: expressions, version: '^1.0.0', path: '.'}]\n")
	writeFile(t, filepath.Join(root, "profile.yaml"), `apiVersion: yawr.runtime-profile/v1
id: expressions
context: test
attendance: unattended
approval:
  scope: {allow_read: true, allow_mutating: false, allow_destructive: false}
`)
	writeFile(t, filepath.Join(root, "parent.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: expression-parent
name: Expression parent
flow:
  - step:
      id: child
      type: include
      include: {runbook_ref: 'expressions/child', resolve_from: catalog}
`)
	writeFile(t, filepath.Join(root, "child.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: expression-child
name: Expression child
flow:
  - step: {id: gate, type: noop, when: '2 >= 1', capture: {value: '${"kept"}'}}
  - step: {id: message, type: display, display: {content: 'Hi ${"world"}!'}}
`)
	saved := map[string][]byte{}
	previews := map[string][]string{}
	for _, mode := range []string{"static", "dynamic"} {
		file := "child.runbook.yaml"
		if mode == "dynamic" {
			file = "parent.runbook.yaml"
		}
		runs := filepath.Join(root, mode+"-runs")
		out, stderr, err := presentationDirectCLI(t, root, "run", file, "--profile", "profile.yaml", "--run-dir", runs, "--output", "json")
		if err != nil {
			t.Fatal(mode, err, stderr, string(out))
		}
		var summary jsonSummary
		summaryJSON := out
		if start := bytes.LastIndex(out, []byte("\n{")); start >= 0 {
			summaryJSON = out[start+1:]
		}
		if json.Unmarshal(summaryJSON, &summary) != nil || summary.Status != "completed" {
			t.Fatal(mode, string(out))
		}
		plans, err := filepath.Glob(filepath.Join(runs, "*", "plan.v1.json"))
		if err != nil || len(plans) != 1 {
			t.Fatal(mode, plans, err)
		}
		args := []string{"preview", "--format", "graphjson", "--run-dir", runs, "--run-id", filepath.Base(filepath.Dir(plans[0]))}
		before, stderr, err := presentationDirectCLI(t, root, args...)
		if err != nil {
			t.Fatal(mode, err, stderr)
		}
		var doc graphjson.Document
		if json.Unmarshal(before, &doc) != nil {
			t.Fatal("graph JSON")
		}
		metadata := 0
		for _, node := range doc.Nodes {
			d, _ := json.Marshal(node.Data["details"])
			if bytes.Contains(d, []byte("expression_presentation")) {
				metadata++
			}
		}
		if metadata != 2 {
			t.Fatal(mode, "non-tool historical metadata count", metadata)
		}
		if doc.PresentationState != nil && len(doc.PresentationState.Occurrences) != 0 {
			t.Fatal("non-tool occurrence authority invented")
		}
		saved[mode] = before
		previews[mode] = args
	}
	for _, name := range []string{"child.runbook.yaml", "parent.runbook.yaml", "yawr-package.yaml", "profile.yaml"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for mode, args := range previews {
		after, stderr, err := presentationDirectCLI(t, root, args...)
		if err != nil || !bytes.Equal(saved[mode], after) {
			t.Fatal(mode, "frozen inspection changed after deletion", err, stderr)
		}
		if strings.Contains(string(after), "changed current text") {
			t.Fatal("current source fallback")
		}
	}
}
