package serve

// Tests for AR-CE-4 (T-SERVE-ENUM-ERRCODE): coded errors map to
// rpc.error.data.code via errkit.Coder classification, never message
// substring matching; the safe message never leaks the rejected value or
// redacted members. Covers CE-V-02/CE-U-01 (no leakage) from
// barbara-client-enum-compatibility-ruling.md's acceptance matrix (§9).

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// CE-V-02: an ENUM-008 caller-binding rejection maps to rpcRunbookInvalid
// (not rpcRunbookParseErr) with engineCode == "ENUM-008", classified by
// errkit.Coder, and the safe message contains neither the rejected value
// nor any generic "parse error" wording.
func TestMapRunbookError_Enum008_CodedNotSubstring(t *testing.T) {
	inputs := map[string]*schema.Input{
		"env_name": {Enum: schema.EnumConstraint{"prod", "staging"}},
	}
	err := schema.CheckCallerInputBindings(inputs, map[string]string{"env_name": "totally-not-a-member"})
	if err == nil {
		t.Fatal("expected ENUM-008 binding error")
	}

	code, message, engineCode := mapRunbookError(err)
	if code != rpcRunbookInvalid {
		t.Fatalf("code = %d, want rpcRunbookInvalid (%d)", code, rpcRunbookInvalid)
	}
	if engineCode != "ENUM-008" {
		t.Fatalf("engineCode = %q, want ENUM-008", engineCode)
	}
	if strings.Contains(message, "totally-not-a-member") {
		t.Fatalf("safe message must never leak the rejected value, got: %q", message)
	}
	if strings.Contains(strings.ToLower(message), "parse") {
		t.Fatalf("safe message must not present a coded ENUM-008 as a parse error, got: %q", message)
	}
}

// The rpcError.Data.Code wire field carries the engine's own code, so a
// client can key its own copy off error.data.code without parsing prose.
func TestRpcErrorWithCode_AttachesDataOnlyWhenCoded(t *testing.T) {
	coded := rpcErrorWithCode(rpcRunbookInvalid, "safe message", "ENUM-008")
	if coded.Data == nil || coded.Data.Code != "ENUM-008" {
		t.Fatalf("expected Data.Code=ENUM-008, got %+v", coded.Data)
	}

	uncoded := rpcErrorWithCode(rpcRunbookParseErr, "Parse error", "")
	if uncoded.Data != nil {
		t.Fatalf("expected nil Data for an uncoded error, got %+v", uncoded.Data)
	}
}

// No-leakage: safeMessageForCode never echoes a raw error message, only
// generic per-code prose, for every enum-family code this runtime emits.
func TestSafeMessageForCode_NoLeakage(t *testing.T) {
	for _, code := range []string{"ENUM-001", "ENUM-008", "ENUM-009", "ENUM-W001"} {
		msg := safeMessageForCode(code)
		if msg == "" {
			t.Fatalf("safeMessageForCode(%q) returned empty message", code)
		}
		if strings.Contains(msg, "<redacted>") {
			t.Fatalf("safeMessageForCode(%q) must never mention a redaction placeholder, got: %q", code, msg)
		}
	}
}

// A generic, uncoded error (no errkit.Coder in its chain) still falls back
// to substring classification -- this runtime's pre-existing YAML/plan
// error paths are unaffected by the D-3 fix.
func TestMapRunbookError_UncodedFallsBackToSubstring(t *testing.T) {
	plain := plainError("yaml: line 3: mapping values are not allowed in this context")
	code, message, engineCode := mapRunbookError(plain)
	if code != rpcRunbookParseErr {
		t.Fatalf("code = %d, want rpcRunbookParseErr (%d)", code, rpcRunbookParseErr)
	}
	if engineCode != "" {
		t.Fatalf("engineCode = %q, want empty for an uncoded error", engineCode)
	}
	if message != "Parse error" {
		t.Fatalf("message = %q, want \"Parse error\"", message)
	}
}

type plainError string

func (e plainError) Error() string { return string(e) }
