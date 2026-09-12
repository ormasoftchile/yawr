package serve

// Test for AR-CE-4 §6 (T-RUN-WARNINGS, serve surface): a parsed runbook's
// non-fatal warnings (e.g. ENUM-W001) reach the run.start RPC result so a
// client can display them, without blocking the run. Covers CE-W-02 from
// barbara-client-enum-compatibility-ruling.md's acceptance matrix (§9).

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestRPC_RunStart_SurfacesParseWarnings(t *testing.T) {
	h := newTestServerHarness(t)
	h.parser.result.Warnings = []parser.ParseWarning{
		{Field: "inputs.env_name.enum", Message: "ENUM-W001: enum members differ only by case"},
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})

	if resp.Error != nil {
		t.Fatalf("non-fatal warnings must never block the run, got error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["runID"] != "run-1" {
		t.Fatalf("expected runID run-1, got %v", result["runID"])
	}
	warnings, ok := result["warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("expected 1 warning in run.start result, got %v", result["warnings"])
	}
	w, ok := warnings[0].(map[string]any)
	if !ok {
		t.Fatalf("expected warning entry to be an object, got %T", warnings[0])
	}
	if w["field"] != "inputs.env_name.enum" {
		t.Fatalf("field = %v, want inputs.env_name.enum", w["field"])
	}
	msg, _ := w["message"].(string)
	if msg == "" || !containsSubstring(msg, "ENUM-W001") {
		t.Fatalf("expected message to carry ENUM-W001, got %q", msg)
	}
}

func TestRPC_RunStart_RequiresCatalogFailureIsSynchronous(t *testing.T) {
	h := newTestServerHarness(t)
	h.parser.result.Runbook = &schema.Runbook{
		Requires: []*schema.PackageRequirement{{Package: "sql-livesite-tsgs", Version: "^1.0.0"}},
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})
	if resp.Error == nil {
		t.Fatalf("expected synchronous package-resolution error, got success: %+v", resp.Result)
	}
	if resp.Error.Code != rpcRunbookInvalid {
		t.Fatalf("expected rpcRunbookInvalid, got %+v", resp.Error)
	}
	if resp.Error.Data == nil || !strings.HasPrefix(resp.Error.Data.Code, "PKG-") {
		t.Fatalf("expected PKG-* code in error data, got %+v", resp.Error.Data)
	}
}

func TestRPC_RunStart_NoWarnings_OmitsKey(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if _, ok := result["warnings"]; ok {
		t.Fatalf("expected no warnings key when there are no parse warnings, got %v", result["warnings"])
	}
}

func containsSubstring(s, substr string) bool {
	return strings.Contains(s, substr)
}
