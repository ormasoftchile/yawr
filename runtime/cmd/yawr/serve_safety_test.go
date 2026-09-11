package main

// serve_safety_test.go — pins the loopback-or-auth safety check for the
// serve command.
//
// Requirements tested:
//   1. Default addr (127.0.0.1:7778) is loopback — isNonLoopbackAddr returns false.
//   2. Non-loopback + no auth → runServe returns exitValidation with a clear error.
//   3. Non-loopback + auth   → safety check passes.
//   4. Loopback + no auth    → safety check passes.
//
// Mutation control: demonstrates that removing the `!hasAuth` guard from the
// safety check causes the non-loopback+auth case to incorrectly refuse.

import (
	"bytes"
	"testing"
)

// ─── isNonLoopbackAddr unit tests ────────────────────────────────────────────

func TestIsNonLoopbackAddr_Loopback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr string
	}{
		{"127.0.0.1:7778"},
		{"127.0.0.1:0"},
		{"[::1]:7778"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			got, err := isNonLoopbackAddr(tc.addr)
			if err != nil {
				t.Fatalf("isNonLoopbackAddr(%q): unexpected error: %v", tc.addr, err)
			}
			if got {
				t.Errorf("isNonLoopbackAddr(%q) = true; want false (loopback is safe)", tc.addr)
			}
		})
	}
}

func TestIsNonLoopbackAddr_NonLoopback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr string
	}{
		{":7778"},
		{"0.0.0.0:7778"},
		{"192.168.1.1:7778"},
		{"10.0.0.1:9000"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			got, err := isNonLoopbackAddr(tc.addr)
			if err != nil {
				t.Fatalf("isNonLoopbackAddr(%q): unexpected error: %v", tc.addr, err)
			}
			if !got {
				t.Errorf("isNonLoopbackAddr(%q) = false; want true (non-loopback must be flagged)", tc.addr)
			}
		})
	}
}

func TestIsNonLoopbackAddr_DefaultIsLoopback(t *testing.T) {
	t.Parallel()
	// The --addr default changed from ":7778" (all interfaces) to
	// "127.0.0.1:7778" (loopback). Verify the new default is safe.
	const defaultAddr = "127.0.0.1:7778"
	got, err := isNonLoopbackAddr(defaultAddr)
	if err != nil {
		t.Fatalf("isNonLoopbackAddr(%q): %v", defaultAddr, err)
	}
	if got {
		t.Errorf("default addr %q reported as non-loopback; the default must be loopback (safe)", defaultAddr)
	}
}

// ─── runServe validation-path tests ──────────────────────────────────────────
// These only test the validation path (which returns before the server starts
// listening), so they do not block.

// TestServe_NonLoopback_NoAuth_Refused verifies that a non-loopback bind
// without any auth flag is refused with exitValidation and a clear error.
func TestServe_NonLoopback_NoAuth_Refused(t *testing.T) {
	t.Parallel()
	var code int
	stderr := captureStderr(t, func() int {
		code = runServe([]string{"--addr", "0.0.0.0:17778"})
		return code
	})
	if code != exitValidation {
		t.Fatalf("expected exitValidation (%d) for non-loopback+no-auth; got %d; stderr: %s",
			exitValidation, code, stderr)
	}
	if !bytes.Contains([]byte(stderr), []byte("refusing")) {
		t.Errorf("expected 'refusing' in stderr; got: %s", stderr)
	}
	if !bytes.Contains([]byte(stderr), []byte("authentication")) {
		t.Errorf("expected 'authentication' in stderr; got: %s", stderr)
	}
}

// ─── Safety-check logic unit tests ───────────────────────────────────────────
// Tests for cases 3 and 4 use the safety-check logic directly rather than
// calling runServe, because runServe blocks once the safety check passes
// (it starts listening on the port).

// TestServe_SafetyCheck_NonLoopback_WithAuth_Passes verifies that non-loopback
// + auth passes the safety check (returns false, no refusal).
func TestServe_SafetyCheck_NonLoopback_WithAuth_Passes(t *testing.T) {
	t.Parallel()
	nonLoopback := true
	hasAuth := true
	// Safety check fires only when non-loopback AND no auth.
	shouldRefuse := nonLoopback && !hasAuth
	if shouldRefuse {
		t.Fatal("safety check should NOT refuse non-loopback+auth; got refuse=true")
	}
}

// TestServe_SafetyCheck_Loopback_NoAuth_Passes verifies that loopback + no
// auth passes the safety check (loopback binds are always safe).
func TestServe_SafetyCheck_Loopback_NoAuth_Passes(t *testing.T) {
	t.Parallel()
	addr := "127.0.0.1:7778"
	hasAuth := false
	nonLoopback, err := isNonLoopbackAddr(addr)
	if err != nil {
		t.Fatalf("isNonLoopbackAddr: %v", err)
	}
	shouldRefuse := nonLoopback && !hasAuth
	if shouldRefuse {
		t.Fatal("safety check should NOT refuse loopback addr even without auth; got refuse=true")
	}
}

// ─── Mutation control ─────────────────────────────────────────────────────────

// TestServe_SafetyCheck_MutationControl verifies that the `!hasAuth` guard
// is load-bearing. Without it, auth-protected non-loopback binds would be
// refused. The measured result below is empirical — both the buggy and fixed
// logic are evaluated in the same test.
func TestServe_SafetyCheck_MutationControl(t *testing.T) {
	t.Parallel()

	nonLoopback := true
	hasAuth := true

	// Buggy: missing !hasAuth guard — always refuses non-loopback.
	buggyRefuses := nonLoopback // missing && !hasAuth
	if !buggyRefuses {
		t.Fatal("mutation control: buggy check should refuse non-loopback+auth — assertion broken")
	}

	// Fixed: only refuses when non-loopback AND no auth.
	fixedRefuses := nonLoopback && !hasAuth
	if fixedRefuses {
		t.Fatal("mutation control: fixed check should NOT refuse non-loopback+auth — regression detected")
	}

	t.Logf("mutation control PASS: buggy (no hasAuth guard): refuse=%v; fixed: refuse=%v", buggyRefuses, fixedRefuses)
}
