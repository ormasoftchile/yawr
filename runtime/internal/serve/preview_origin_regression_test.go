package serve

// C-2 regression tests for preview.html's previewParentTargetOrigin().
//
// Contract: barbara-served-sterling-recovery-contract.md §C-2
//
// The three tests here prove:
//
//  1. TestServedPreviewEmptyReferrer — positive proof: an iframe-like context
//     with an agreed vscode-webview:// ancestor and parentOrigin resolves to
//     that trusted origin even when document.referrer is empty.
//
//  2. TestServedPreviewOriginTiers — table-driven coverage of all three tiers
//     and the disagreement safe-failure path.
//
//  3. TestPreviewHTMLHasNoWildcardPostMessage — static proof that no code path
//     in preview.html calls postMessage with the wildcard target origin '*'.
//
// Run: go test -run TestServedPreviewEmptyReferrer ./internal/serve/
//      go test -run TestServedPreviewOriginTiers   ./internal/serve/
//      go test -run TestPreviewHTMLHasNoWildcardPostMessage ./internal/serve/
// Or all at once:
//      go test ./internal/serve/ -run "TestServedPreviewEmpty|TestServedPreviewOriginTiers|TestPreviewHTMLHasNoWildcard"

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// previewHTMLAbsPath returns the absolute path to the real static/preview.html
// source file so the Node.js helper can read it directly (the embedded FS
// cannot be passed as a file path).
func previewHTMLAbsPath(t *testing.T) string {
	t.Helper()
	// This test file lives at internal/serve/; preview.html is at
	// internal/serve/static/preview.html relative to the repo root.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate preview.html")
	}
	// file = .../internal/serve/preview_origin_regression_test.go
	dir := filepath.Dir(file)
	p, err := filepath.Abs(filepath.Join(dir, "static", "preview.html"))
	if err != nil {
		t.Fatalf("abs path for preview.html: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("preview.html not found at %s: %v", p, err)
	}
	return p
}

// previewHelperAbsPath returns the absolute path to testdata/previewOriginHelper.js.
func previewHelperAbsPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate testdata/previewOriginHelper.js")
	}
	p, err := filepath.Abs(filepath.Join(filepath.Dir(file), "testdata", "previewOriginHelper.js"))
	if err != nil {
		t.Fatalf("abs path for previewOriginHelper.js: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("previewOriginHelper.js not found at %s: %v", p, err)
	}
	return p
}

// requireNode skips the test if node is not available.
func requireNode(t *testing.T) string {
	t.Helper()
	nodeExe := os.Getenv("YAWR_NODE_PATH")
	if nodeExe == "" {
		nodeExe = "node"
	}
	if _, err := exec.LookPath(nodeExe); err != nil {
		t.Skipf("skipping: node not found in PATH (set YAWR_NODE_PATH): %v", err)
	}
	return nodeExe
}

// runOriginHelper invokes previewOriginHelper.js with the given scenario and
// returns the result string (the resolved origin or "null").
func runOriginHelper(t *testing.T, nodeExe, helperPath, htmlPath string, scenario map[string]any) string {
	t.Helper()

	scenarioJSON, err := json.Marshal(scenario)
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(nodeExe, helperPath, string(scenarioJSON), htmlPath)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("previewOriginHelper.js failed: %v\nstderr: %s\nstdout: %s",
			err, stderr.String(), stdout.String())
	}

	var out struct {
		Result *string `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("parse helper output: %v (raw: %s)", err, stdout.String())
	}
	if out.Result == nil {
		return "null"
	}
	return *out.Result
}

// TestServedPreviewEmptyReferrer — C-2 positive proof.
//
// Scenario: the preview is loaded as an iframe inside a vscode-webview://
// wrapper. document.referrer is empty (the standard behaviour for cross-origin
// Electron webviews), but location.ancestorOrigins[0] is populated.
//
// Expected: previewParentTargetOrigin() returns the ancestor origin, NOT null.
// A null return would cause the calling code to self-ack execution-not-started
// instead of dispatching yawr.host-action.request to window.parent.
func TestServedPreviewEmptyReferrer(t *testing.T) {
	nodeExe := requireNode(t)
	helperPath := previewHelperAbsPath(t)
	htmlPath := previewHTMLAbsPath(t)

	const vsCodeOrigin = "vscode-webview://1a2b3c4d5e6f7890abcdef1234567890"

	result := runOriginHelper(t, nodeExe, helperPath, htmlPath, map[string]any{
		"isTopLevel":      false,
		"ancestorOrigins": []string{vsCodeOrigin},
		"referrer":        "", // empty — the bug trigger
		"search":          "?parentOrigin=" + vsCodeOrigin,
		"origin":          "http://127.0.0.1:8080",
	})

	if result == "null" {
		t.Fatalf(
			"previewParentTargetOrigin() returned null with ancestorOrigins[0]=%q and empty referrer — "+
				"the preview would self-ack execution-not-started instead of dispatching "+
				"yawr.host-action.request; this is the F-1 regression",
			vsCodeOrigin,
		)
	}
	if result != vsCodeOrigin {
		t.Fatalf("previewParentTargetOrigin() = %q, want %q", result, vsCodeOrigin)
	}
}

// TestServedPreviewOriginTiers — table-driven coverage of all resolution tiers
// and the disagreement safe-failure path.
func TestServedPreviewOriginTiers(t *testing.T) {
	nodeExe := requireNode(t)
	helperPath := previewHelperAbsPath(t)
	htmlPath := previewHTMLAbsPath(t)

	const vsCodeOrigin = "vscode-webview://deadbeefcafe"
	const previewOrigin = "http://127.0.0.1:9999"

	cases := []struct {
		name            string
		scenario        map[string]any
		wantResult      string // empty string means "null"
		wantNotWildcard bool   // always true; belt-and-suspenders
	}{
		{
			name: "ancestor-without-explicit-agreement",
			scenario: map[string]any{
				"isTopLevel":      false,
				"ancestorOrigins": []string{vsCodeOrigin},
				"referrer":        "",
				"search":          "",
				"origin":          previewOrigin,
			},
			wantResult: "",
		},
		{
			name: "parameter-without-browser-proof",
			scenario: map[string]any{
				"isTopLevel":      false,
				"ancestorOrigins": []string{},
				"referrer":        "",
				"search":          "?parentOrigin=" + previewOrigin,
				"origin":          previewOrigin,
			},
			wantResult: "",
		},
		{
			name: "trusted-vscode-ancestor-and-parameter-agree",
			scenario: map[string]any{
				"isTopLevel":      false,
				"ancestorOrigins": []string{vsCodeOrigin},
				"referrer":        "",
				"search":          "?parentOrigin=" + vsCodeOrigin,
				"origin":          previewOrigin,
			},
			wantResult: vsCodeOrigin,
		},
		{
			name: "tier1-and-tier2-disagree-safe-failure",
			scenario: map[string]any{
				"isTopLevel":      false,
				"ancestorOrigins": []string{vsCodeOrigin},
				"referrer":        "",
				"search":          "?parentOrigin=https://untrusted.example.com",
				"origin":          previewOrigin,
			},
			wantResult: "", // must return null — mismatch is a safe failure
		},
		{
			name: "trusted-same-origin-referrer-and-parameter-agree",
			scenario: map[string]any{
				"isTopLevel":      false,
				"ancestorOrigins": []string{},
				"referrer":        previewOrigin + "/wrapper",
				"search":          "?parentOrigin=" + previewOrigin,
				"origin":          previewOrigin,
			},
			wantResult: previewOrigin,
		},
		{
			name: "hostile-parent-cannot-authorize-itself",
			scenario: map[string]any{
				"isTopLevel":      false,
				"ancestorOrigins": []string{"https://hostile.example"},
				"referrer":        "https://hostile.example/embed",
				"search":          "?parentOrigin=https://hostile.example",
				"origin":          previewOrigin,
			},
			wantResult: "",
		},
		{
			name: "all-empty-self-acks",
			scenario: map[string]any{
				"isTopLevel":      false,
				"ancestorOrigins": []string{},
				"referrer":        "",
				"search":          "",
				"origin":          previewOrigin,
			},
			wantResult: "", // null → calling code self-acks, correct for standalone
		},
		{
			name: "top-level-always-null",
			scenario: map[string]any{
				"isTopLevel":      true,
				"ancestorOrigins": []string{vsCodeOrigin},
				"referrer":        previewOrigin + "/",
				"search":          "?parentOrigin=" + vsCodeOrigin,
				"origin":          previewOrigin,
			},
			wantResult: "", // window.parent === window → always null
		},
		{
			name: "malformed-param-origin-ignored",
			scenario: map[string]any{
				"isTopLevel":      false,
				"ancestorOrigins": []string{},
				"referrer":        "",
				"search":          "?parentOrigin=not-a-url",
				"origin":          previewOrigin,
			},
			wantResult: "", // malformed param falls through to tier 3 → null (no referrer)
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runOriginHelper(t, nodeExe, helperPath, htmlPath, tc.scenario)
			want := tc.wantResult
			if want == "" {
				want = "null"
			}
			if got != want {
				t.Errorf("previewParentTargetOrigin() = %q, want %q (scenario: %v)",
					got, want, tc.scenario)
			}
			if got == "*" {
				t.Errorf("previewParentTargetOrigin() returned wildcard '*' — never allowed")
			}
		})
	}
}

// TestPreviewHTMLHasNoWildcardPostMessage — static negative proof.
//
// Verifies that no postMessage call in preview.html uses the wildcard target
// origin '*'. Every postMessage must use a resolved targetOrigin from
// previewParentTargetOrigin(), and the function must never return '*'.
func TestPreviewHTMLHasNoWildcardPostMessage(t *testing.T) {
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		t.Fatalf("read preview.html: %v", err)
	}
	html := string(data)

	// No postMessage with wildcard.
	if strings.Contains(html, `postMessage(`) {
		// Collect all postMessage call sites and verify none use '*'.
		idx := 0
		for {
			pos := strings.Index(html[idx:], "postMessage(")
			if pos < 0 {
				break
			}
			abs := idx + pos
			// Grab a ~200-char window starting at the call site.
			end := abs + 200
			if end > len(html) {
				end = len(html)
			}
			site := html[abs:end]
			if strings.Contains(site, `'*'`) || strings.Contains(site, `"*"`) {
				t.Errorf("postMessage call at offset %d uses wildcard target origin '*':\n%s", abs, site)
			}
			idx = abs + len("postMessage(")
		}
	}

	// The function must be present and must not contain a literal '*' return.
	fnStart := strings.Index(html, "function previewParentTargetOrigin()")
	if fnStart < 0 {
		t.Fatal("previewParentTargetOrigin() not found in preview.html")
	}
	// Find function end by brace counting.
	braceStart := strings.Index(html[fnStart:], "{") + fnStart
	depth, pos := 0, braceStart
	for pos < len(html) {
		switch html[pos] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				pos++
				goto done
			}
		}
		pos++
	}
done:
	fnBody := html[fnStart:pos]
	if strings.Contains(fnBody, `return '*'`) || strings.Contains(fnBody, `return "*"`) {
		t.Errorf("previewParentTargetOrigin() contains a literal wildcard '*' return:\n%s", fnBody)
	}

	// Must require browser proof, explicit agreement, and trusted origins.
	for _, want := range []string{
		"ancestorOrigins",
		"parentOrigin",
		"document.referrer",
		"isTrustedPreviewParentOrigin",
	} {
		if !strings.Contains(fnBody, want) {
			t.Errorf("previewParentTargetOrigin() missing %q — three-tier algorithm not present", want)
		}
	}
}
