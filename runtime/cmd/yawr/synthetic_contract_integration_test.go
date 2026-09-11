package main

// synthetic_contract_integration_test.go — CLI integration proof for the
// ops-synthetic contract (Phase 1B Item 4).
//
// Scope of this file:
//
//  1. Runs the ops-synthetic mcp-http binding through the full runRun() path
//     with BOTH --package-map AND --profile active in a single execution.
//     Both flags are genuinely load-bearing:
//
//     --package-map: the project config has no requires; without the flag
//     the tool is not found (PKG-001 → exitValidation).
//
//     --profile: supplies the endpoint override to the live mock MCP server.
//     Without it the static placeholder URL https://127.0.0.1/ is used and
//     the TLS dial fails (connection refused → exitFailure).
//
//  2. Exercises BOTH outcome paths of Action B (pattern-search): match-found
//     and no-match, through the mcp-http binding.
//
//  3. Proves managed-identity token acquisition from a mock IMDS server and
//     attachment to the MCP server's Authorization header, through runRun().
//
//  4. Asserts Invariant 3 (endpoint outside allowed_hosts fails at plan time,
//     PLAN-013) for the synthetic contract's mcp-http binding shape.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// writeOpsSyntheticMCPHTTPPackage writes a yawr-package.yaml and tool YAML for
// the ops-synthetic mcp-http binding.
//
// The static transport.url is set to https://127.0.0.1/ — a placeholder that
// passes HTTPS-URL validation at scan time. The profile endpoint override
// redirects actual calls to the live mock MCP server at runtime. allowed_hosts
// is restricted to 127.0.0.1 so the PLAN-013 preflight accepts the override
// (same host) while rejecting any other host.
func writeOpsSyntheticMCPHTTPPackage(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: yawr.ops-synthetic
  version: "1.0.0"
exports:
  tools:
    - id: ops-synthetic
      path: tools/ops-synthetic.tool.yaml
`)
	writeFile(t, filepath.Join(root, "tools", "ops-synthetic.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: ops-synthetic
  version: "1.0.0"
transport:
  mode: mcp-http
  url: https://127.0.0.1/
  auth:
    provider: managed-identity
    scope: api://ops-synthetic/mcp.tools
    allowed_hosts:
      - 127.0.0.1
actions:
  - name: status-query
    description: "Query status for a target (Action A: 5-field typed output)"
    classification: read-only
    args:
      target: {type: string, required: true}
  - name: pattern-search
    description: "Search for a pattern (Action B: match-found or no-match)"
    classification: read-only
    args:
      query: {type: string, required: true}
`)
}

// newOpsSyntheticCLIMCPServer starts an httptest.Server implementing the
// ops-synthetic MCP protocol. It records the first Authorization header it
// observes in *gotAuth. The caller must close the server when done.
func newOpsSyntheticCLIMCPServer(t *testing.T, gotAuth *string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	initialized := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		method, _ := msg["method"].(string)
		id := msg["id"]

		if auth := r.Header.Get("Authorization"); auth != "" {
			mu.Lock()
			if *gotAuth == "" {
				*gotAuth = auth
			}
			mu.Unlock()
		}

		writeJSONResponse := func(payload any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(payload)
		}

		switch method {
		case "initialize":
			initialized = true
			writeJSONResponse(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"serverInfo":      map[string]any{"name": "ops-synthetic-cli-mock"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "shutdown":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if !initialized {
				http.Error(w, "not initialized", http.StatusPreconditionFailed)
				return
			}
			params, _ := msg["params"].(map[string]any)
			name, _ := params["name"].(string)
			args, _ := params["arguments"].(map[string]any)

			var outText string
			switch name {
			case "status-query":
				target, _ := args["target"].(string)
				b, _ := json.Marshal(map[string]any{
					"id": "svc-" + target, "name": target,
					"status": "operational", "region": "eastus", "health_score": 98,
				})
				outText = string(b)
			case "pattern-search":
				query, _ := args["query"].(string)
				var out map[string]any
				if strings.HasPrefix(query, "FOUND:") {
					out = map[string]any{
						"matched": true,
						"pattern": strings.TrimPrefix(query, "FOUND:"),
						"count":   1,
					}
				} else {
					out = map[string]any{"matched": false}
				}
				b, _ := json.Marshal(out)
				outText = string(b)
			default:
				http.Error(w, "unknown tool "+name, http.StatusBadRequest)
				return
			}
			writeJSONResponse(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": outText}},
				},
			})
		default:
			http.Error(w, "unknown method "+method, http.StatusBadRequest)
		}
	}))
}

// ─── primary composition proof ────────────────────────────────────────────────

// TestSyntheticContract_CLI_PackageMap_Profile_MCPHTTPBinding is the primary
// CLI integration test for the ops-synthetic contract. It proves that
// --package-map and --profile compose correctly in a single runRun() execution
// through the mcp-http binding with managed-identity token attachment.
//
// Both flags are genuinely load-bearing:
//   - Without --package-map: the project config has no requires, so
//     yawr.ops-synthetic is not found → PKG-001 → exitValidation (not exitSuccess).
//   - Without --profile: the mcp-http transport uses the static placeholder URL
//     https://127.0.0.1/ (port 443). TLS dial to that non-existent port fails →
//     step error → exitFailure (not exitSuccess).
//
// Managed-identity token attachment is proven by asserting that the mock MCP
// server received an Authorization: Bearer <token> header matching the token
// returned by the mock IMDS server.
func TestSyntheticContract_CLI_PackageMap_Profile_MCPHTTPBinding(t *testing.T) {
	// ── 1. Mock IMDS server ───────────────────────────────────────────────
	// Returns a synthetic access token. SetIMDSEndpointForTest redirects the
	// ManagedIdentityAuthProvider's HTTP client to this server for the duration
	// of the test. Must not be called with t.Parallel().
	const syntheticToken = "SYNTHETIC_MI_TOKEN_CLI_PROOF"
	imdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": syntheticToken,
			"expires_in":   "3600",
			"token_type":   "Bearer",
		})
	}))
	defer imdsSrv.Close()
	t.Cleanup(internaltool.SetIMDSEndpointForTest(imdsSrv.URL))

	// ── 2. Mock MCP server ────────────────────────────────────────────────
	var gotAuth string
	var authMu sync.Mutex
	mcpSrv := newOpsSyntheticCLIMCPServer(t, &gotAuth, &authMu)
	defer mcpSrv.Close()

	// ── 3. mcp-http package ───────────────────────────────────────────────
	mcpPkgDir := t.TempDir()
	writeOpsSyntheticMCPHTTPPackage(t, mcpPkgDir)

	// ── 4. Work directory ─────────────────────────────────────────────────
	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	// Project config: deliberately empty requires. Without --package-map
	// there is no binding for yawr.ops-synthetic and runRun exits with
	// exitValidation (PKG-001). This makes --package-map load-bearing.
	writeFile(t, filepath.Join(absDir, ".yawr", "config.yaml"),
		"apiVersion: yawr.config/v1\nrequires: []\n")

	relMCP := relForwardSlash(t, absDir, mcpPkgDir)
	writeFile(t, filepath.Join(absDir, "package-map.yaml"),
		"apiVersion: yawr.config/v1\nrequires:\n  - package: yawr.ops-synthetic\n    version: \"^1.0.0\"\n    path: "+relMCP+"\n")

	// Profile: supplies the endpoint override to the live mock MCP server.
	// Without --profile the static placeholder URL https://127.0.0.1/ is
	// used. Port 443 is not listening → TLS connection refused → exitFailure.
	// This makes --profile load-bearing.
	writeFile(t, filepath.Join(absDir, "headless.profile.yaml"), "apiVersion: yawr.runtime-profile/v1\n"+
		"id: headless-ops\n"+
		"context: headless-server\n"+
		"attendance: unattended\n"+
		"approval:\n"+
		"  scope:\n"+
		"    allow_read: true\n"+
		"    allow_mutating: false\n"+
		"    allow_destructive: false\n"+
		"tools:\n"+
		"  ops-synthetic:\n"+
		"    endpoint: \""+mcpSrv.URL+"/\"\n"+
		"    provider: managed-identity\n")

	// Runbook: exercises status-query (Action A) and both pattern-search
	// outcomes (Action B: match-found + no-match).
	writeFile(t, filepath.Join(absDir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: ops-synthetic-proof
name: ops-synthetic contract proof
toolRefs:
  - name: ops-synthetic
    package: yawr.ops-synthetic
flow:
  - step:
      id: query
      type: tool
      tool:
        name: ops-synthetic
        action: status-query
        args:
          target: api-gateway
  - step:
      id: search-hit
      type: tool
      tool:
        name: ops-synthetic
        action: pattern-search
        args:
          query: "FOUND:active-pattern"
  - step:
      id: search-miss
      type: tool
      tool:
        name: ops-synthetic
        action: pattern-search
        args:
          query: not-found-query
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	chdirForTest(t, absDir)

	out := runCaptureStdout(t, []string{
		"runbook.yaml",
		"--package-map", "package-map.yaml",
		"--profile", "headless.profile.yaml",
		"--trace", "trace.jsonl",
		"--output", "json",
	})

	// ── Action A: 5-field typed output ────────────────────────────────────
	queryStdout := capturedResult(t, out, "query")
	for _, want := range []string{"\"id\"", "\"name\"", "\"status\"", "\"region\"", "\"health_score\""} {
		if !strings.Contains(queryStdout, want) {
			t.Errorf("status-query (Action A): output missing field %s\n  got: %s", want, queryStdout)
		}
	}

	// ── Action B: match-found outcome ─────────────────────────────────────
	hitStdout := capturedResult(t, out, "search-hit")
	if !strings.Contains(hitStdout, `"matched":true`) {
		t.Errorf("pattern-search match-found: expected \"matched\":true\n  got: %s", hitStdout)
	}
	if !strings.Contains(hitStdout, `"pattern"`) {
		t.Errorf("pattern-search match-found: expected \"pattern\" field in payload\n  got: %s", hitStdout)
	}

	// ── Action B: no-match outcome ────────────────────────────────────────
	missStdout := capturedResult(t, out, "search-miss")
	if !strings.Contains(missStdout, `"matched":false`) {
		t.Errorf("pattern-search no-match: expected \"matched\":false\n  got: %s", missStdout)
	}
	if strings.Contains(missStdout, `"pattern"`) {
		t.Errorf("pattern-search no-match: unexpected \"pattern\" field in no-match payload\n  got: %s", missStdout)
	}

	// ── Managed-identity token attachment ─────────────────────────────────
	// The mock MCP server must have received an Authorization header carrying
	// the token returned by the mock IMDS server. This proves the full path:
	// profile selects provider:managed-identity → runtime acquires token from
	// IMDS → TokenGate attaches it as Bearer to the outgoing MCP request.
	authMu.Lock()
	observed := gotAuth
	authMu.Unlock()
	if observed != "Bearer "+syntheticToken {
		t.Errorf("managed-identity token not attached to MCP request\n  want: Bearer %s\n  got:  %s",
			syntheticToken, observed)
	}
}

// ─── Invariant 3 (PLAN-013) for the synthetic contract ───────────────────────

// TestSyntheticContract_CLI_Invariant_PLAN013_EndpointOutsideAllowedHosts
// proves that an endpoint override in the profile resolving to a host OUTSIDE
// the mcp-http tool's allowed_hosts fails at planning time (Tier 0 preflight),
// not midway through execution. This is the synthetic-contract-specific PLAN-013
// assertion; the generic proof already lives in
// testCLI_ProfileToolOverrideEndpoint_Reachable (reachability_probes_test.go).
//
// Invariant: an endpoint override targeting a host NOT in allowed_hosts MUST
// surface as a PLAN-013 error (exitValidation), not as a runtime MCP-012
// mid-execution failure.
func TestSyntheticContract_CLI_Invariant_PLAN013_EndpointOutsideAllowedHosts(t *testing.T) {
	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	chdirForTest(t, absDir)

	// mcp-http binding for ops-synthetic: allowed_hosts restricts to
	// ops-synthetic.example.net. The profile override below targets a
	// different host (attacker.example.com).
	writeFile(t, filepath.Join(absDir, "ops-synthetic.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: ops-synthetic
  version: "1.0.0"
transport:
  mode: mcp-http
  url: https://ops-synthetic.example.net/v1/
  auth:
    provider: managed-identity
    scope: api://ops-synthetic/mcp.tools
    allowed_hosts:
      - ops-synthetic.example.net
actions:
  - name: status-query
    description: "Query status for a target"
    classification: read-only
  - name: pattern-search
    description: "Search for a pattern"
    classification: read-only
`)
	writeFile(t, filepath.Join(absDir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: plan013-synthetic
name: PLAN-013 synthetic contract probe
flow:
  - step:
      id: q
      type: tool
      tool:
        name: ops-synthetic
        action: status-query
`)
	writeFile(t, filepath.Join(absDir, "bad.profile.yaml"), `apiVersion: yawr.runtime-profile/v1
id: bad-endpoint-profile
context: headless-server
attendance: unattended
approval:
  scope:
    allow_read: true
tools:
  ops-synthetic:
    endpoint: https://attacker.example.com/v1/
`)

	stderr := captureStderr(t, func() int {
		return runRun([]string{
			"runbook.yaml",
			"--profile", "bad.profile.yaml",
			"--trace", "trace.jsonl",
			"--output", "quiet",
		})
	})

	if runLast == exitSuccess {
		t.Fatal("PLAN-013 invariant FAILED: an endpoint override outside allowed_hosts " +
			"should have been rejected at plan time (exitValidation), but the run succeeded. " +
			"Check internal/planner/preflight.go checkToolEnvironmentPreflight for the PLAN-013 check.")
	}
	if runLast != exitValidation {
		t.Errorf("expected exitValidation (%d) for PLAN-013, got %d (stderr: %s)",
			exitValidation, runLast, stderr)
	}
	if !strings.Contains(stderr, "PLAN-013") {
		t.Errorf("expected PLAN-013 sentinel in stderr, got: %s", stderr)
	}
}
