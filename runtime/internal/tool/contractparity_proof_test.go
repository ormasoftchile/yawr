package tool

// contractparity_proof_test.go — Tess's parity proof for the ops-synthetic
// contract (Phase 1B Item 4, Tess's contribution).
//
// This file uses the exported pkg/contractparity API to assert that the
// native binding and the mcp-http binding of ops-synthetic are
// contract-identical for:
//
//  1. Action A (status-query) — 5-field typed output
//  2. Action B match-found (pattern-search, query="FOUND:…") — matched=true shape
//  3. Action B no-match (pattern-search, any other query) — matched=false shape
//  4. Declared metadata — both bindings agree on classification=read-only
//
// One negative control at this level (TestSyntheticContractParity_NegativeControl)
// deliberately compares a known-divergent stub binding against the native
// binding and asserts that the harness reports the violation.
//
// Why this file is in package tool (not contractparity_test or cmd/yawr):
//
//   - newOpsSyntheticMCPServer (from synthetic_contract_proof_test.go) is
//     unexported; it is only accessible from within package tool.
//   - NativeCLITransport is also unexported.
//   - The harness itself (pkg/contractparity) is fully exported — this file
//     is the "external consumer" proof that the harness is usable from
//     outside contractparity WITHOUT touching unexported harness internals.
//
// Placement justification: the task permits "place your test in the same
// package as a NEW file (that is fine, same package, different file)".
//
// ops-mock binary: built once per test run via sync.Once so the full-suite
// overhead is one go build call. The path is stored in opsMockBinOnce so
// each test can reference it without rebuilding.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/contractparity"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// ─── ops-mock binary (built once per test run) ────────────────────────────────

var (
	opsMockBinOnce sync.Once
	opsMockBinPath string
	opsMockBinErr  error
)

func opsMockBin(t *testing.T) string {
	t.Helper()
	opsMockBinOnce.Do(func() {
		root := repoRoot()
		dir := filepath.Join(root, ".testtools", "contractparity-proof")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			opsMockBinErr = fmt.Errorf("mkdir .testtools/contractparity-proof: %w", err)
			return
		}
		out, err := testutil.BuildGoBinary(root, filepath.Join(dir, "ops-mock"), "./cmd/tools/ops-mock")
		if err != nil {
			opsMockBinErr = fmt.Errorf("build ops-mock: %w", err)
			return
		}
		opsMockBinPath = out
	})
	if opsMockBinErr != nil {
		t.Fatalf("ops-mock binary unavailable: %v", opsMockBinErr)
	}
	return opsMockBinPath
}

// ─── binding constructors ─────────────────────────────────────────────────────

// readOnlyMeta is the declared metadata for every ops-synthetic action.
var readOnlyMeta = contractparity.ActionMeta{
	Classification: strPtr("read-only"),
}

func strPtr(s string) *string { return &s }

// nativeInvokeFor returns a contractparity.Binding.Invoke function that runs
// ops-mock via NativeCLITransport for the given action and argv template.
//
// NativeCLITransport does not parse stdout as JSON — it leaves Output nil.
// The invoke function parses stdout explicitly so the harness receives a
// map[string]any, the same shape the mcp-http binding returns via Output.
func nativeInvokeFor(binPath, action string, argv []string) func(context.Context, map[string]any) (map[string]any, error) {
	return func(ctx context.Context, args map[string]any) (map[string]any, error) {
		transport := &NativeCLITransport{}
		def := toolpkg.ToolDef{
			Name:    "ops-synthetic",
			Command: binPath,
			Actions: map[string]*toolpkg.ToolAction{
				action: {Argv: argv},
			},
		}
		result, err := transport.Invoke(ctx, def, action, args)
		if err != nil {
			return nil, err
		}
		// Parse stdout as JSON (ops-mock always emits JSON on stdout).
		var out map[string]any
		if err := json.Unmarshal([]byte(result.Stdout), &out); err != nil {
			return nil, fmt.Errorf("native binding: parse stdout JSON: %w (stdout: %q)", err, result.Stdout)
		}
		return out, nil
	}
}

// mcpHTTPInvokeFor returns a contractparity.Binding.Invoke function that calls
// the given MCPHTTPTransport URL for the given action.
//
// MCPHTTPTransport.toolResult already parses the content text as JSON into
// result.Output, so we return that directly.
func mcpHTTPInvokeFor(srvURL, action string) func(context.Context, map[string]any) (map[string]any, error) {
	return func(ctx context.Context, args map[string]any) (map[string]any, error) {
		transport := NewMCPHTTPTransport(srvURL, nil)
		defer transport.Close()
		def := toolpkg.ToolDef{
			Name:      "ops-synthetic",
			Transport: toolpkg.TransportMCPHTTP,
			URL:       srvURL,
			Actions:   map[string]*toolpkg.ToolAction{action: {}},
		}
		result, err := transport.Invoke(ctx, def, action, args)
		if err != nil {
			return nil, err
		}
		if result.Output == nil {
			return nil, fmt.Errorf("mcp-http binding: Output is nil (server did not return JSON)")
		}
		return result.Output, nil
	}
}

// ─── Action A: status-query ───────────────────────────────────────────────────

// TestSyntheticContractParity_ActionA_StatusQuery asserts that the native and
// mcp-http bindings of status-query are contract-identical: same 5 output keys
// (id, name, status, region, health_score) with matching Go reflect.Types, and
// both declare classification=read-only.
func TestSyntheticContractParity_ActionA_StatusQuery(t *testing.T) {
	bin := opsMockBin(t)
	srv := newOpsSyntheticMCPServer(t)
	defer srv.Close()

	native := contractparity.Binding{
		Name:   "native",
		Invoke: nativeInvokeFor(bin, "status-query", []string{"-action", "status-query", "-target", "${target}"}),
		Meta:   readOnlyMeta,
	}
	mcpHTTP := contractparity.Binding{
		Name:   "mcp-http",
		Invoke: mcpHTTPInvokeFor(srv.URL, "status-query"),
		Meta:   readOnlyMeta,
	}

	contractparity.AssertParity(t, context.Background(),
		map[string]any{"target": "api-gateway"},
		native, mcpHTTP,
	)
}

// ─── Action B: pattern-search match-found ────────────────────────────────────

// TestSyntheticContractParity_ActionB_MatchFound asserts that the native and
// mcp-http bindings produce contract-identical output for the match-found
// outcome of pattern-search (query starts with "FOUND:").
//
// Both must return: matched (bool), pattern (string), count (numeric).
func TestSyntheticContractParity_ActionB_MatchFound(t *testing.T) {
	bin := opsMockBin(t)
	srv := newOpsSyntheticMCPServer(t)
	defer srv.Close()

	native := contractparity.Binding{
		Name:   "native",
		Invoke: nativeInvokeFor(bin, "pattern-search", []string{"-action", "pattern-search", "-query", "${query}"}),
		Meta:   readOnlyMeta,
	}
	mcpHTTP := contractparity.Binding{
		Name:   "mcp-http",
		Invoke: mcpHTTPInvokeFor(srv.URL, "pattern-search"),
		Meta:   readOnlyMeta,
	}

	contractparity.AssertParity(t, context.Background(),
		map[string]any{"query": "FOUND:active-pattern"},
		native, mcpHTTP,
	)
}

// ─── Action B: pattern-search no-match ───────────────────────────────────────

// TestSyntheticContractParity_ActionB_NoMatch asserts that the native and
// mcp-http bindings produce contract-identical output for the no-match
// outcome of pattern-search (any query without the "FOUND:" prefix).
//
// Both must return: matched (bool) only — no "pattern" or "count" key.
//
// The no-match shape is compared against the no-match shape, not against
// match-found. Two bindings that disagree about which outcome they returned
// is itself a parity violation worth catching.
func TestSyntheticContractParity_ActionB_NoMatch(t *testing.T) {
	bin := opsMockBin(t)
	srv := newOpsSyntheticMCPServer(t)
	defer srv.Close()

	native := contractparity.Binding{
		Name:   "native",
		Invoke: nativeInvokeFor(bin, "pattern-search", []string{"-action", "pattern-search", "-query", "${query}"}),
		Meta:   readOnlyMeta,
	}
	mcpHTTP := contractparity.Binding{
		Name:   "mcp-http",
		Invoke: mcpHTTPInvokeFor(srv.URL, "pattern-search"),
		Meta:   readOnlyMeta,
	}

	contractparity.AssertParity(t, context.Background(),
		map[string]any{"query": "no-match-query"},
		native, mcpHTTP,
	)
}

// ─── NEGATIVE CONTROL ─────────────────────────────────────────────────────────

// TestSyntheticContractParity_NegativeControl deliberately compares the
// real native binding against a knowingly-divergent stub that adds an extra
// output key ("internal_debug_flag") the native binding never returns.
//
// The harness MUST report ViolationExtraKey. If it does not, the harness is
// broken and every passing parity proof above is untrustworthy.
//
// This mirrors the discipline of TestCredentialLeak_SweepDetectsIntentionalLeak:
// a parity test that has never been observed to fail is not evidence.
func TestSyntheticContractParity_NegativeControl(t *testing.T) {
	bin := opsMockBin(t)

	// Real native binding for status-query.
	native := contractparity.Binding{
		Name:   "native",
		Invoke: nativeInvokeFor(bin, "status-query", []string{"-action", "status-query", "-target", "${target}"}),
		Meta:   readOnlyMeta,
	}

	// Divergent stub: returns the correct 5-field output PLUS an extra key that
	// is absent from the real contract. This simulates a binding that leaks an
	// internal field (e.g. a debugging artefact that crept into production output).
	divergentStub := contractparity.Binding{
		Name: "divergent-stub",
		Invoke: func(_ context.Context, _ map[string]any) (map[string]any, error) {
			return map[string]any{
				"id":                  "svc-api-gateway",
				"name":                "api-gateway",
				"status":              "operational",
				"region":              "eastus",
				"health_score":        float64(98),
				"internal_debug_flag": true, // extra key — deliberate parity violation
			}, nil
		},
		Meta: readOnlyMeta,
	}

	r, err := contractparity.Check(context.Background(), map[string]any{"target": "api-gateway"}, native, divergentStub)
	if err != nil {
		t.Fatalf("Check: unexpected error: %v", err)
	}
	if r.Equal() {
		t.Fatal("NEGATIVE CONTROL FAILED: harness did NOT detect extra key 'internal_debug_flag' in divergent stub — harness is broken and all parity proofs in this file are untrustworthy")
	}

	// Verify the violation is specifically the extra key, not some other error.
	found := false
	for _, v := range r.Violations {
		if v.Kind == contractparity.ViolationExtraKey && v.Field == "internal_debug_flag" {
			found = true
		}
	}
	if !found {
		t.Errorf("NEGATIVE CONTROL FAILED: expected ViolationExtraKey for field 'internal_debug_flag', got: %s", r.Summary())
	}

	// Log the violation summary so a reader can see exactly what was detected.
	t.Logf("negative control correctly reported: %s", r.Summary())
}
