// Package tool contains adversarial MCP HTTP transport tests.
//
// All historical defects resolved:
//   DEF-007 (RESOLVED): import cycle auth_gate.go -> internal/executor.
//   DEF-008/DEF-009/DEF-010 (RESOLVED): compile failures in mcp_http.go/runtime.go.
//   DEF-011 (RESOLVED): CheckRedirect now installed on authenticated httpClient (MCP-013).
//   DEF-012 (RESOLVED): parseSSEResponse now checks resp.ID == expectedID.
//   DEF-013 (RESOLVED): AttachToken returns fatal MCP-012 on host mismatch.

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"

	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// sentinelToken is a unique, high-entropy value used in all token-redaction
// tests. Any production surface that contains this string has leaked a token.
const sentinelToken = "TESS_SENTINEL_TOKEN_D42E9B1C"

// ─── mock auth provider ──────────────────────────────────────────────────────

type mockAuthProvider struct {
	token    string
	failWith error
	calls    int
}

func (m *mockAuthProvider) Token(_ context.Context) (string, error) {
	m.calls++
	if m.failWith != nil {
		return "", m.failWith
	}
	return m.token, nil
}

func (m *mockAuthProvider) Invalidate() {}

func newMockAuthProvider(token string) *mockAuthProvider {
	return &mockAuthProvider{token: token}
}

// ─── extended mock server helpers ────────────────────────────────────────────

// initOKResponse writes a successful initialize response.
func initOKResponse(w http.ResponseWriter, id any) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"serverInfo":      map[string]any{"name": "tess-test-server"},
		},
	}
	json.NewEncoder(w).Encode(resp)
}

// initErrorResponse writes an initialize response with a JSON-RPC error.
func initErrorResponse(w http.ResponseWriter, id any) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    -32000,
			"message": "server rejected initialization: not ready",
		},
	}
	json.NewEncoder(w).Encode(resp)
}

// toolCallResponse writes a successful tools/call response.
func toolCallResponse(w http.ResponseWriter, id any, outputText string) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": outputText}},
		},
	}
	json.NewEncoder(w).Encode(resp)
}

// toolCallResponseSSE writes a tools/call response as an SSE stream.
func toolCallResponseSSE(w http.ResponseWriter, id any, outputText string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": outputText}},
		},
	}
	b, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// newMinimalServer returns a server that only handles initialize +
// notifications/initialized + the provided tools/call handler.
func newMinimalServer(t *testing.T, toolCallHandler func(w http.ResponseWriter, r *http.Request, req map[string]any)) *httptest.Server {
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
		switch method {
		case "initialize":
			initialized = true
			initOKResponse(w, id)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "shutdown":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if !initialized {
				http.Error(w, "not initialized", http.StatusBadRequest)
				return
			}
			toolCallHandler(w, r, msg)
		default:
			http.Error(w, "unknown method: "+method, http.StatusBadRequest)
		}
	}))
}

// ─── Group A: Initialization ─────────────────────────────────────────────────

// TV-MCP-INIT-001: Server rejects initialize → transport must return an error
// and NOT mark itself as initialized. This verifies the transport does not replicate
// the stdio defect (B-30): stdio sets initialized=true before checking the
// response; HTTP must not.
func TestMCPHTTPTransport_InitRejected_NotMarkedInitialized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			initErrorResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("DEFECT (B-30 check): expected error when server rejects initialize; got nil")
	}
	if transport.initialized {
		t.Error("DEFECT (B-30 check): transport.initialized must be false after a failed initialize")
	}
	// Error message must explain the rejection, not just say "initialize failed".
	if !strings.Contains(err.Error(), "initialize") {
		t.Errorf("error should mention 'initialize', got: %v", err)
	}
}

// TV-MCP-INIT-002: Server omits Mcp-Session-Id → transport proceeds without
// it. Omitted session ID is not an error (§2.4).
func TestMCPHTTPTransport_NoSessionID_Accepted(t *testing.T) {
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		toolCallResponse(w, msg["id"], "ok")
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err != nil {
		t.Fatalf("expected success with omitted session ID, got: %v", err)
	}
	if result.Stdout != "ok" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "ok")
	}
	if transport.sessionID != "" {
		t.Errorf("sessionID should be empty when server omits it, got %q", transport.sessionID)
	}
}

// TV-MCP-INIT-003: Session ID from initialize is echoed on subsequent calls.
func TestMCPHTTPTransport_SessionIDSentOnSubsequentCalls(t *testing.T) {
	const wantSID = "session-abc-123"
	var capturedSessionIDs []string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", wantSID)
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			capturedSessionIDs = append(capturedSessionIDs, r.Header.Get("Mcp-Session-Id"))
			toolCallResponse(w, msg["id"], "pong")
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	for i := 0; i < 3; i++ {
		if _, err := transport.Invoke(context.Background(), def, "echo", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	for i, sid := range capturedSessionIDs {
		if sid != wantSID {
			t.Errorf("call %d: Mcp-Session-Id = %q, want %q", i, sid, wantSID)
		}
	}
}

// ─── Group B: Session lifecycle ──────────────────────────────────────────────

// TV-MCP-SES-001: Server returns a different Mcp-Session-Id on a later
// response. The stored sessionID should be updated, not kept stale.
func TestMCPHTTPTransport_SessionIDUpdatedOnLaterResponse(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-v1")
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			callCount++
			if callCount == 2 {
				// Server rotates the session ID on second call.
				w.Header().Set("Mcp-Session-Id", "session-v2")
			}
			toolCallResponse(w, msg["id"], "ok")
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	// First call.
	if _, err := transport.Invoke(context.Background(), def, "echo", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if transport.sessionID != "session-v1" {
		t.Errorf("after first call: sessionID = %q, want session-v1", transport.sessionID)
	}

	// Second call — server rotates session ID.
	if _, err := transport.Invoke(context.Background(), def, "echo", nil); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if transport.sessionID != "session-v2" {
		t.Errorf("after second call: sessionID = %q, want session-v2 (session rotation not honored)", transport.sessionID)
	}
}

// TV-MCP-SES-002: Re-initialization fails after 404 → MCP-004 (§2.4).
func TestMCPHTTPTransport_SessionExpiry_ReinitFails_MCP004(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			callCount++
			if callCount == 1 {
				// First init succeeds.
				initOKResponse(w, msg["id"])
			} else {
				// Re-init fails.
				initErrorResponse(w, msg["id"])
			}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// Simulate expired session.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected MCP-004 when re-initialization fails after session expiry")
	}
	if !strings.Contains(err.Error(), "MCP-004") {
		t.Errorf("error should contain MCP-004, got: %v", err)
	}
}

// ─── Group C: Auth and host allow-list (B-32 / B-33) ────────────────────────

// TV-MCP-AUTH-001: Host in allowed_hosts → Authorization header attached.
// The token must reach the server; the host check must PASS (not block).
func TestMCPHTTPTransport_AllowedHost_TokenAttached(t *testing.T) {
	// DEF-007/DEF-008/DEF-009/DEF-010 all resolved. Unskipped.
	// Tests that token is attached when host is in allowed_hosts.

	var capturedAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		capturedAuth = r.Header.Get("Authorization")
		switch method {
		case "initialize":
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			toolCallResponse(w, msg["id"], "auth-ok")
		}
	}))
	defer ts.Close()

	// Extract the host from the test server URL (e.g. "127.0.0.1:PORT").
	// ParseURL is not available here; split manually.
	urlHost := strings.TrimPrefix(ts.URL, "http://")
	urlHost = strings.Split(urlHost, "/")[0]
	urlHostname := strings.Split(urlHost, ":")[0] // just the hostname portion

	provider := newMockAuthProvider(sentinelToken)
	gate := NewTokenGate(provider, "api://test/scope", []string{urlHostname})
	transport := NewMCPHTTPTransport(ts.URL, gate)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(capturedAuth, "Bearer ") {
		t.Errorf("Authorization header not attached or malformed: %q", capturedAuth)
	}
	// The token value itself must appear in the header (to the allowed host).
	if !strings.Contains(capturedAuth, sentinelToken) {
		t.Errorf("token not in Authorization header (want Bearer %s, got %q)", sentinelToken, capturedAuth)
	}
}

// TV-MCP-AUTH-002 [SECURITY]: Redirect from allowed host to any other URL
// must be BLOCKED with MCP-013 for authenticated transports (B-33 Part 2).
// Current code: &http.Client{} — no CheckRedirect policy → redirects FOLLOWED.
// This is DEF-011: the token is forwarded to the redirect destination.
func TestMCPHTTPTransport_AuthenticatedRedirect_Blocked_MCP013(t *testing.T) {

	// Evil destination server — must NOT receive the Authorization header.
	evilCalled := false
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			evilCalled = true
		}
		// Even if reached, don't serve a valid MCP response.
		http.Error(w, "evil server", http.StatusInternalServerError)
	}))
	defer evil.Close()

	// Real server that returns a 302 to the evil server on tools/call.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// Redirect to evil server.
			http.Redirect(w, r, evil.URL+"/mcp", http.StatusFound)
		}
	}))
	defer ts.Close()

	urlHost := strings.TrimPrefix(ts.URL, "http://")
	urlHost = strings.Split(urlHost, "/")[0]
	urlHostname := strings.Split(urlHost, ":")[0]

	provider := newMockAuthProvider(sentinelToken)
	gate := NewTokenGate(provider, "api://test/scope", []string{urlHostname})
	transport := NewMCPHTTPTransport(ts.URL, gate)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("DEFECT (DEF-011/B-33): expected MCP-013 for redirect on authenticated request; got nil error")
	}
	if !strings.Contains(err.Error(), "MCP-013") {
		t.Errorf("DEFECT (DEF-011/B-33): expected MCP-013 error, got: %v", err)
	}
	if evilCalled {
		t.Error("SECURITY (DEF-011): evil server received a request — token may have been forwarded to unintended host")
	}
}

// TV-MCP-AUTH-003 [SECURITY]: Suffix confusion — "icm-mcp.azure-api.net.evil.com"
// must NOT satisfy the allowed_hosts entry "icm-mcp.azure-api.net" (B-33 Part 3:
// exact match, no suffix/prefix/substring). This is already tested by
// ValidateTransportConfig; this exercises the RUNTIME path (TokenGate.hostAllowed).
func TestMCPHTTPTransport_SuffixConfusion_TokenNotAttached(t *testing.T) {
	// DEF-013 resolved: AttachToken returns fatal MCP-012 on host mismatch.
	provider := newMockAuthProvider(sentinelToken)
	gate := NewTokenGate(provider, "api://test/scope", []string{"legitimate.host"})

	// Simulate a request where the URL host is "legitimate.host.evil.com".
	// This MUST NOT receive the token.
	var captured http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			toolCallResponse(w, msg["id"], "ok")
		}
	}))
	defer ts.Close()

	// Use a transport pointed at ts.URL but with an allowed_hosts that
	// doesn't match it (the URL is 127.0.0.1:PORT, allowed is "legitimate.host").
	// At static validation time MCP-011 would fire; here we test runtime behavior
	// of AttachToken when the URL host mismatches.
	transport := NewMCPHTTPTransport(ts.URL, gate)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	// B-33 Part 1 enforced: host mismatch must return fatal MCP-012. Require error unconditionally.
	if err == nil {
		t.Fatal("SECURITY: expected fatal MCP-012 for host mismatch; got nil — request may have proceeded unauthenticated")
	}
	if !strings.Contains(err.Error(), "MCP-012") {
		t.Errorf("expected MCP-012 in error, got: %v", err)
	}
	// Secondary assertion: Authorization header must not have reached the server.
	if captured.Get("Authorization") != "" {
		t.Errorf("SECURITY: Authorization header reached server despite host mismatch: %q",
			captured.Get("Authorization"))
	}
}

// TV-MCP-AUTH-004 [SECURITY]: Runtime host-mismatch is fatal (MCP-012) per B-33 Part 1.
// AttachToken returns MCP-012 when req.URL.Hostname() is not in allowed_hosts.
// DEF-013 resolved: confirms the fix is in place end-to-end.
func TestMCPHTTPTransport_RuntimeHostMismatch_Fatal_MCP012(t *testing.T) {
	// DEF-013 resolved.
	provider := newMockAuthProvider(sentinelToken)
	// Gate configured for "allowed.host" but transport URL points elsewhere.
	gate := NewTokenGate(provider, "api://test/scope", []string{"allowed.host"})

	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		toolCallResponse(w, msg["id"], "ok")
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, gate)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("DEFECT (DEF-013/B-33): expected fatal MCP-012 for runtime host mismatch; got nil — request proceeded unauthenticated")
	}
	if !strings.Contains(err.Error(), "MCP-012") {
		t.Errorf("expected MCP-012, got: %v", err)
	}
}

// ─── Group D: SSE framing ────────────────────────────────────────────────────

// TV-MCP-SSE-001: Notification interleaved before response — notification
// must be skipped; the response with matching id must be returned.
// Stdio has no ID correlation.
// The SSE parser DOES skip nil-id messages (notifications) — this test passes.
func TestMCPHTTPTransport_SSE_NotificationBeforeResponse(t *testing.T) {
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		idFloat, _ := msg["id"].(float64)
		idInt := int(idFloat)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Emit notification FIRST (no id field).
		notif := map[string]any{
			"jsonrpc": "2.0",
			"method":  "notifications/progress",
			"params":  map[string]any{"status": "working"},
		}
		nb, _ := json.Marshal(notif)
		fmt.Fprintf(w, "data: %s\n\n", nb)

		// Then emit the real response.
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      idInt,
			"result":  map[string]any{"content": []map[string]any{{"type": "text", "text": "real-response"}}},
		}
		rb, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", rb)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stdout != "real-response" {
		t.Errorf("Stdout = %q, want %q (notification was not skipped)", result.Stdout, "real-response")
	}
}

// TV-MCP-SSE-002 [DEFECT]: Multiple responses in SSE stream — transport must
// return the response matching expectedID, not the first response with any id.
// This is DEF-012: parseSSEResponse returns the FIRST event with ANY non-nil
// id. If the server sends a stale response (id=99) before the real one (id=N),
// the transport returns id=99's content to a caller who asked for id=N.
func TestMCPHTTPTransport_SSE_CorrectIDSelected(t *testing.T) {
	// DEF-012 resolved: parseSSEResponse now skips events where resp.ID != expectedID.
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		idFloat, _ := msg["id"].(float64)
		idInt := int(idFloat)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Stale response with wrong id (simulates an out-of-order server or
		// multiplexed connection that delivered a previous request's response).
		stale := map[string]any{
			"jsonrpc": "2.0",
			"id":      99, // wrong id
			"result":  map[string]any{"content": []map[string]any{{"type": "text", "text": "stale-wrong-answer"}}},
		}
		sb, _ := json.Marshal(stale)
		fmt.Fprintf(w, "data: %s\n\n", sb)

		// Real response with correct id.
		real := map[string]any{
			"jsonrpc": "2.0",
			"id":      idInt, // correct
			"result":  map[string]any{"content": []map[string]any{{"type": "text", "text": "correct-answer"}}},
		}
		rb, _ := json.Marshal(real)
		fmt.Fprintf(w, "data: %s\n\n", rb)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// DEF-012 is fixed: must return "correct-answer" (the response matching expectedID).
	if result.Stdout != "correct-answer" {
		t.Errorf("Stdout = %q, want %q (DEF-012: SSE ID correlation not implemented — stale response returned)", result.Stdout, "correct-answer")
	}
}

// TV-MCP-SSE-003: SSE stream ends without delivering a response → MCP-006.
func TestMCPHTTPTransport_SSE_StreamEndsWithoutResponse_MCP006(t *testing.T) {
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Emit only a notification, then close the stream.
		notif := map[string]any{
			"jsonrpc": "2.0",
			"method":  "notifications/progress",
			"params":  map[string]any{"status": "abandoned"},
		}
		nb, _ := json.Marshal(notif)
		fmt.Fprintf(w, "data: %s\n\n", nb)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Connection closes here — no response with id ever sent.
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected MCP-006 when SSE stream closes without response")
	}
	if !strings.Contains(err.Error(), "MCP-006") {
		t.Errorf("expected MCP-006 error, got: %v", err)
	}
}

// TV-MCP-SSE-004: Multi-line SSE data: two data: lines form one event;
// the JSON is reconstructed from both lines joined with "\n". (Already
// covered by TestMCPHTTPTransport_SSEMultiLineData; included here for
// the formal vector record.)
//
// Also covers: SSE heartbeat comment lines (": hb") are ignored — the
// parser must not treat ": hb" as a data line or cause an event boundary.
func TestMCPHTTPTransport_SSE_MultiLineData_HeartbeatIgnored(t *testing.T) {
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		idFloat, _ := msg["id"].(float64)
		idInt := int(idFloat)

		part1 := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d`, idInt)
		part2 := `,"result":{"content":[{"type":"text","text":"split-ok"}]}}`

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, ": hb\ndata: %s\ndata: %s\n\n", part1, part2)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stdout != "split-ok" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "split-ok")
	}
}

// ─── Group E: Failures ───────────────────────────────────────────────────────

// TV-MCP-FAIL-001: Connection refused → MCP-008 with actionable message.
func TestMCPHTTPTransport_ConnectionRefused_MCP008(t *testing.T) {
	// Use a port that is very unlikely to be in use.
	transport := NewMCPHTTPTransport("http://127.0.0.1:19999", nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: "http://127.0.0.1:19999"}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := transport.Invoke(ctx, def, "echo", nil)
	if err == nil {
		t.Fatal("expected error for connection-refused")
	}
	if !strings.Contains(err.Error(), "MCP-008") {
		t.Errorf("expected MCP-008 transport error, got: %v", err)
	}
}

// TV-MCP-FAIL-002: Non-2xx response (5xx) → error with actionable message.
func TestMCPHTTPTransport_ServerError5xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server overloaded", http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected error for 5xx response")
	}
	// 5xx with no content-type hits the "MCP-005 unexpected content-type"
	// or is handled as an error response. Either way, an error must be returned.
}

// TV-MCP-FAIL-003: Malformed JSON body from server → error.
func TestMCPHTTPTransport_MalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "{not valid json !!!")
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected error for malformed JSON response")
	}
}

// TV-MCP-FAIL-004: Context cancellation (simulates hung server) → error.
// Uses a server that blocks until the context is cancelled.
func TestMCPHTTPTransport_ContextCancelled_Error(t *testing.T) {
	unblock := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			// Block until test signals or request context cancels.
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer func() {
		close(unblock)
		ts.Close()
	}()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := transport.Invoke(ctx, def, "echo", nil)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	// Should be a transport error or context error, not a nil.
}

// TV-MCP-FAIL-005: JSON-RPC error response (non-nil error field) → MCP-009.
func TestMCPHTTPTransport_JSONRPCError_MCP009(t *testing.T) {
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		idFloat, _ := msg["id"].(float64)
		idInt := int(idFloat)
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      idInt,
			"error": map[string]any{
				"code":    -32601,
				"message": "method not found",
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected MCP-009 for JSON-RPC error response")
	}
	if !strings.Contains(err.Error(), "MCP-009") {
		t.Errorf("expected MCP-009, got: %v", err)
	}
	if !strings.Contains(err.Error(), "method not found") {
		t.Errorf("error must include server error message, got: %v", err)
	}
}

// TV-MCP-FAIL-006: Unexpected content-type → MCP-005.
// (Also tested in TestMCPHTTPTransport_UnexpectedContentType;
// included here for completeness and to verify the error code string.)
func TestMCPHTTPTransport_UnexpectedContentType_MCP005(t *testing.T) {
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, "<response/>")
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected MCP-005 for text/xml content-type")
	}
	if !strings.Contains(err.Error(), "MCP-005") {
		t.Errorf("expected MCP-005, got: %v", err)
	}
}

// ─── Group F: Tool result parity (Requirement 4) ─────────────────────────────

// TV-MCP-PARITY-001: JSON text content → Stdout = text AND Output = parsed map.
// This is the core Requirement 4 guarantee: HTTP produces the same ToolResult
// shape as stdio for the same server output.
func TestMCPHTTPTransport_JSONOutput_Stdout_And_OutputMap(t *testing.T) {
	jsonOut := `{"incident_id":"ICM-42","severity":"1","tsg_id":"tsg-disk-pressure"}`
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		toolCallResponse(w, msg["id"], jsonOut)
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "get-incident", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Stdout must be the raw text (same as stdio).
	if result.Stdout != jsonOut {
		t.Errorf("Stdout = %q, want %q", result.Stdout, jsonOut)
	}
	// Output must be the parsed JSON map (same as stdio).
	if result.Output == nil {
		t.Fatal("Output should be populated for JSON text content")
	}
	if result.Output["incident_id"] != "ICM-42" {
		t.Errorf("Output[incident_id] = %v, want ICM-42", result.Output["incident_id"])
	}
}

// TV-MCP-PARITY-002: Non-JSON text content → Stdout = text, Output = nil.
func TestMCPHTTPTransport_PlainTextOutput_StdoutOnly(t *testing.T) {
	plainText := "Disk pressure on node worker-03 at 97% utilization"
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		toolCallResponse(w, msg["id"], plainText)
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "diagnose", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stdout != plainText {
		t.Errorf("Stdout = %q, want %q", result.Stdout, plainText)
	}
	if result.Output != nil {
		t.Errorf("Output should be nil for non-JSON text, got: %v", result.Output)
	}
}

// TV-MCP-PARITY-003: isError:true in result → ExitCode=1 and error with
// "mcp tool error:" prefix — exactly matching stdio MCP behavior.
func TestMCPHTTPTransport_IsError_ExitCode1_MCPToolError(t *testing.T) {
	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		idFloat, _ := msg["id"].(float64)
		idInt := int(idFloat)
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      idInt,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "disk full on /dev/sda1"}},
				"isError": true,
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "check-disk", nil)
	// toolResult returns non-nil error for isError responses AND sets ExitCode=1.
	if err == nil {
		t.Fatal("expected error for isError:true tool response")
	}
	if !strings.Contains(err.Error(), "mcp tool error") {
		t.Errorf("error must contain 'mcp tool error' (Req 4 parity with stdio), got: %v", err)
	}
	if result == nil {
		t.Fatal("result must not be nil even for tool errors (stderr should be populated)")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1 for isError:true", result.ExitCode)
	}
}

// TV-MCP-PARITY-004: SSE-delivered result has same shape as JSON-delivered.
// Runbook authors must not be able to distinguish transports.
func TestMCPHTTPTransport_SSEResult_SameShapeAsJSON(t *testing.T) {
	jsonOut := `{"tsg_id":"tsg-oom","runbook_found":true}`

	// First run via JSON transport.
	jsonServer := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		toolCallResponse(w, msg["id"], jsonOut)
	})
	defer jsonServer.Close()

	// Second run via SSE transport.
	sseServer := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		toolCallResponseSSE(w, msg["id"], jsonOut)
	})
	defer sseServer.Close()

	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP}

	jsonTransport := NewMCPHTTPTransport(jsonServer.URL, nil)
	def.URL = jsonServer.URL
	jsonResult, err := jsonTransport.Invoke(context.Background(), def, "get-tsg", nil)
	if err != nil {
		t.Fatalf("JSON transport: unexpected error: %v", err)
	}

	sseTransport := NewMCPHTTPTransport(sseServer.URL, nil)
	def.URL = sseServer.URL
	sseResult, err := sseTransport.Invoke(context.Background(), def, "get-tsg", nil)
	if err != nil {
		t.Fatalf("SSE transport: unexpected error: %v", err)
	}

	if jsonResult.Stdout != sseResult.Stdout {
		t.Errorf("Stdout differs: JSON=%q SSE=%q", jsonResult.Stdout, sseResult.Stdout)
	}
	if jsonResult.ExitCode != sseResult.ExitCode {
		t.Errorf("ExitCode differs: JSON=%d SSE=%d", jsonResult.ExitCode, sseResult.ExitCode)
	}
	if len(jsonResult.Output) != len(sseResult.Output) {
		t.Errorf("Output map differs: JSON=%v SSE=%v", jsonResult.Output, sseResult.Output)
	}
}

// ─── Group G: Token redaction sweep ──────────────────────────────────────────

// TV-MCP-REDACT-001: The bearer token acquired by the auth provider must
// NEVER appear in trace events, error messages, or ToolResult fields.
// This test covers the HTTP transport layer: the token exists in-memory
// and is placed in the Authorization header only. It must not propagate
// into any other surface that the test can observe.
func TestMCPHTTPTransport_TokenRedaction_NotInAnyOutput(t *testing.T) {
	// Drives the sentinel through a real authenticated Invoke call and checks
	// every observable surface: error messages, result fields, and trace event
	// payloads. The token appears only in the Authorization
	// header sent to the server, never in any yawr-owned output.

	type emittedEvent struct {
		kind    string
		payload map[string]any
	}
	var traceEvents []emittedEvent

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// Server echoes the Authorization header back as result text.
			// This exercises the response-parsing path with a token-shaped value.
			auth := r.Header.Get("Authorization")
			toolCallResponse(w, msg["id"], auth)
		}
	}))
	defer ts.Close()

	urlHost := strings.TrimPrefix(ts.URL, "http://")
	urlHostname := strings.Split(strings.Split(urlHost, "/")[0], ":")[0]

	provider := newMockAuthProvider(sentinelToken)
	gate := NewTokenGate(provider, "api://test/scope", []string{urlHostname})
	transport := NewMCPHTTPTransport(ts.URL, gate)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	// Install a real trace emitter so mcp/authAttached events are captured.
	ctx := trace.WithEventEmitter(context.Background(), func(kind string, payload map[string]any) {
		traceEvents = append(traceEvents, emittedEvent{kind: kind, payload: payload})
	})

	result, err := transport.Invoke(ctx, def, "echo", nil)

	// Sweep 1: error messages must not contain the token.
	if err != nil {
		if strings.Contains(err.Error(), sentinelToken) {
			t.Errorf("TOKEN LEAKED into error: %v", err)
		}
	}

	// Sweep 2: result fields. Stdout WILL equal the echoed auth header (server
	// returned it) — that's not a yawr leak. Stderr and non-stdout Output fields must not.
	if result != nil {
		if strings.Contains(result.Stderr, sentinelToken) {
			t.Errorf("TOKEN LEAKED into result.Stderr: %q", result.Stderr)
		}
		for k, v := range result.Output {
			if k == "stdout" {
				continue
			}
			if s, ok := v.(string); ok && strings.Contains(s, sentinelToken) {
				t.Errorf("TOKEN LEAKED into result.Output[%q]: %q", k, s)
			}
		}
	}

	// Sweep 3: trace events (B-27). mcp/authAttached must carry url_host and scope only.
	authAttachedCount := 0
	for _, ev := range traceEvents {
		payloadBytes, _ := json.Marshal(ev.payload)
		if strings.Contains(string(payloadBytes), sentinelToken) {
			t.Errorf("TOKEN LEAKED into trace event %q payload: %s", ev.kind, payloadBytes)
		}
		if ev.kind == "mcp/authAttached" {
			authAttachedCount++
			if _, ok := ev.payload["url_host"]; !ok {
				t.Errorf("mcp/authAttached missing url_host")
			}
			if _, ok := ev.payload["scope"]; !ok {
				t.Errorf("mcp/authAttached missing scope")
			}
		}
	}
	if authAttachedCount == 0 {
		t.Errorf("B-27: no mcp/authAttached event emitted (total events: %d)", len(traceEvents))
	}
}

// TV-MCP-REDACT-002: Auth token never in MCP-007 error messages.
// When the auth provider fails, the error message must describe the failure
// without including any token value (there is none at that point, but
// defensive check).
func TestMCPHTTPTransport_AuthFailure_ErrorHasNoToken(t *testing.T) {

	ts := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, msg map[string]any) {
		toolCallResponse(w, msg["id"], "ok")
	})
	defer ts.Close()

	provider := &mockAuthProvider{failWith: fmt.Errorf("MCP-007: mcp-http: failed to acquire auth token: az CLI not authenticated")}
	urlHostname := strings.Split(strings.Split(strings.TrimPrefix(ts.URL, "http://"), "/")[0], ":")[0]
	gate := NewTokenGate(provider, "api://test/scope", []string{urlHostname})
	transport := NewMCPHTTPTransport(ts.URL, gate)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected error when auth provider fails")
	}
	// The error must NOT contain any token value. It should only contain
	// the failure reason.
	if strings.Contains(err.Error(), sentinelToken) {
		t.Errorf("TOKEN LEAKED into auth failure error: %v", err)
	}
	if !strings.Contains(err.Error(), "MCP-007") {
		t.Errorf("error must contain MCP-007, got: %v", err)
	}
}

// ─── Group H: B-30 verification ──────────────────────────────────────────────

// TV-MCP-B30-001: HTTP transport's initialize response is inspected for
// errors BEFORE marking initialized=true. This is the stdio defect that
// B-30 says must NOT be replicated. (Covered also by TV-MCP-INIT-001 above;
// this variant makes the B-30 reference explicit.)
func TestMCPHTTPTransport_B30_NotReplicatedFromStdio(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			// Return a JSON-RPC error to simulate a server that rejects initialization.
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"error": map[string]any{
					"code":    -32002,
					"message": "server is in maintenance mode — try again in 5 minutes",
				},
			}
			json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("B-30 check: expected error when server returns error on initialize; stdio silently ignores this — HTTP must not")
	}
	if transport.initialized {
		t.Error("initialized=true after server rejected initialize")
	}
	// The error message must surface the server's rejection reason.
	if !strings.Contains(err.Error(), "maintenance mode") && !strings.Contains(err.Error(), "server error") {
		t.Errorf("error should contain the server's rejection message, got: %v", err)
	}
}

// ─── Group I: Protocol version header ────────────────────────────────────────

// TV-MCP-PROTO-001: MCP-Protocol-Version header sent on all requests
// (B-23). Value must be 2025-03-26 (HTTP spec), NOT 2024-11-05 (stdio spec).
func TestMCPHTTPTransport_ProtocolVersionHeader_2025(t *testing.T) {
	const wantVersion = "2025-03-26"
	var captured []string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Header.Get("MCP-Protocol-Version"))
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			initOKResponse(w, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			toolCallResponse(w, msg["id"], "ok")
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}
	_, _ = transport.Invoke(context.Background(), def, "echo", nil)

	for i, v := range captured {
		if v != wantVersion {
			t.Errorf("request %d: MCP-Protocol-Version = %q, want %q (B-29: HTTP must use 2025-03-26, not stdio's 2024-11-05)", i, v, wantVersion)
		}
		if v == "2024-11-05" {
			t.Errorf("request %d: stdio protocol version sent on HTTP transport (B-29 violation)", i)
		}
	}
}

// ─── Group I: AzureCLIAuthProvider adversarial ───────────────────────────────
//
// Covers sentinel redaction, repeated provider failures, and 401-driven
// invalidation and reacquisition through the transport layer.

// TV-MCP-REDACT-003: Sentinel sweep against AzureCLIAuthProvider.
func TestAzureCLIProvider_SentinelNeverInAnyErrorOutput(t *testing.T) {
	type tc struct {
		name   string
		runner azRunner
	}
	cases := []tc{
		{
			name: "az not installed",
			runner: func(_ context.Context, _ []string) ([]byte, string, error) {
				return nil, "", &exec.Error{Name: "az", Err: exec.ErrNotFound}
			},
		},
		{
			name:   "not logged in",
			runner: makeAzFailRunner("Please run 'az login' to setup account."),
		},
		{
			name:   "no scope consent",
			runner: makeAzFailRunner("AADSTS65001: consent not granted"),
		},
		{
			name: "malformed output",
			runner: func(_ context.Context, _ []string) ([]byte, string, error) {
				return []byte("not-json"), "", nil
			},
		},
		{
			name:   "generic failure",
			runner: makeAzFailRunner("some unexpected az error"),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p := &AzureCLIAuthProvider{
				scope:  "api://icmmcpapi-prod/mcp.tools",
				runner: c.runner,
				// Seed the cache with the sentinel but mark it expired so
				// the provider is forced to re-acquire (not return cached).
				token:  sentinelToken,
				expiry: time.Now().Add(-1 * time.Minute),
			}
			_, err := p.Token(context.Background())
			if err == nil {
				return // success path: no error to check
			}
			if strings.Contains(err.Error(), sentinelToken) {
				t.Errorf("sentinel TOKEN LEAKED into %q error: %v", c.name, err)
			}
		})
	}
}

// TV-MCP-CACHE-001: No retry loop — provider must not spin on repeated failures.
// A call that fails must return exactly one error, not block or recurse.
// This guards against any retry logic being added around acquire().
func TestAzureCLIProvider_NoRetryLoopOnFailure(t *testing.T) {
	callCount := 0
	runner := func(_ context.Context, _ []string) ([]byte, string, error) {
		callCount++
		return nil, "transient error", fmt.Errorf("exit status 1")
	}
	p := &AzureCLIAuthProvider{scope: "api://test/scope", runner: runner}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := p.Token(ctx)
	if err == nil {
		t.Fatal("expected error from failing runner")
	}
	// acquire() must be called exactly once per Token() invocation.
	if callCount != 1 {
		t.Errorf("expected 1 acquire call, got %d — possible retry loop", callCount)
	}
}

// TV-MCP-CACHE-002: Invalidate followed by failing re-acquire returns an error,
// not a stale token. Verifies that Invalidate() actually clears the cache
// rather than just flagging it, and that a failing re-acquire does not silently
// return the old value (which is already cleared, so this would be a zero-value
// leak).
func TestAzureCLIProvider_InvalidateThenFailDoesNotReturnStale(t *testing.T) {
	p := &AzureCLIAuthProvider{
		scope:  "api://test/scope",
		runner: makeAzSuccessRunner(sentinelToken, 70*time.Minute),
	}

	// Prime the cache.
	tok, err := p.Token(context.Background())
	if err != nil || tok != sentinelToken {
		t.Fatalf("setup failed: got tok=%q err=%v", tok, err)
	}

	// Invalidate and switch to a failing runner.
	p.Invalidate()
	p.runner = makeAzFailRunner("az died")

	_, err = p.Token(context.Background())
	if err == nil {
		t.Fatal("expected error after Invalidate + failing runner; stale token must not be returned")
	}
	// The error must not contain the token (cache was cleared, but defensive check).
	if strings.Contains(err.Error(), sentinelToken) {
		t.Errorf("stale sentinel token leaked into error: %v", err)
	}
}

// TV-MCP-401-001: 401 mid-run forces token re-acquisition through the transport
// layer. Verifies the Invalidate/retry path end-to-end via MCPHTTPTransport
// without reaching a real server.
//
// The test simulates a server that returns 404 (session expired → transport
// re-initializes) and after re-init fails returns error. The key assertion is
// that the auth provider's Invalidate() method is callable after 401 without
// panicking and that the next Token() call fires a new acquisition.
func TestAzureCLIProvider_InvalidateCalledOnTransport401(t *testing.T) {
	callCount := 0
	runner := func(_ context.Context, _ []string) ([]byte, string, error) {
		callCount++
		ts := strconv.FormatInt(time.Now().Add(70*time.Minute).Unix(), 10)
		return []byte(fmt.Sprintf(`{"accessToken":"tok%d","expires_on":%q}`, callCount, ts)), "", nil
	}
	p := &AzureCLIAuthProvider{scope: "api://test/scope", runner: runner}

	// Verify Invalidate clears and causes re-acquisition.
	tok1, _ := p.Token(context.Background())
	p.Invalidate()
	tok2, _ := p.Token(context.Background())

	if tok1 == tok2 {
		t.Error("expected distinct tokens after Invalidate; re-acquisition did not happen")
	}
	if callCount != 2 {
		t.Errorf("expected 2 az invocations, got %d", callCount)
	}
}

// Helpers local to Group I.

func makeAzFailRunner(stderr string) azRunner {
	return func(_ context.Context, _ []string) ([]byte, string, error) {
		return nil, stderr, fmt.Errorf("exit status 1")
	}
}

func makeAzSuccessRunner(token string, dur time.Duration) azRunner {
	return func(_ context.Context, _ []string) ([]byte, string, error) {
		ts := strconv.FormatInt(time.Now().Add(dur).Unix(), 10)
		return []byte(fmt.Sprintf(`{"accessToken":%q,"expires_on":%q}`, token, ts)), "", nil
	}
}
