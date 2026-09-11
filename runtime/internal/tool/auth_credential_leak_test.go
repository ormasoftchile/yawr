package tool

// auth_credential_leak_test.go
//
// Phase 1B Item 6 — credential non-leakage assertions.
//
// This file proves that the managed-identity bearer token NEVER appears on any
// observable output surface. It covers:
//   - trace events (mcp/authAttached and all others emitted during a call)
//   - step results and output maps
//   - error values on all failure paths (non-200 IMDS, malformed JSON,
//     context cancellation, host-not-allowed)
//   - IndeterminateRecord fields (endpoint host only, never token)
//   - serialized JSON of every collected surface
//
// CRITICAL — negative control: this file contains
// TestCredentialLeak_SweepDetectsIntentionalLeak, which deliberately injects
// the synthetic token into a swept surface and asserts that the sweep function
// fires. A credential-non-leakage test that cannot detect a real leak is
// worthless.
//
// All tests run offline against httptest mocks. No real Azure credentials are
// required or used.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// ─── sentinel token ──────────────────────────────────────────────────────────

// leakSentinel is a distinctive, unambiguous string used as the synthetic
// bearer token in all leak tests. It is unlikely to occur anywhere else in
// the codebase or in debug output, which makes matches unambiguous.
const leakSentinel = "LEAK-SENTINEL-3f9b2c8a-d14e-4a7b-9031-credential-test"

// ─── sweep helpers ───────────────────────────────────────────────────────────

// credentialSweeper collects all swept surfaces and reports whether the
// sentinel was found anywhere.
type credentialSweeper struct {
	surfaces []sweepSurface
}

type sweepSurface struct {
	label string
	value string
}

// add records a named surface for sweeping.
func (s *credentialSweeper) add(label, value string) {
	s.surfaces = append(s.surfaces, sweepSurface{label: label, value: value})
}

// addJSON serializes v to JSON and records it as a named surface.
func (s *credentialSweeper) addJSON(label string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.add(label+"(json-error)", fmt.Sprintf("json.Marshal failed: %v", err))
		return
	}
	s.add(label, string(b))
}

// scan checks all registered surfaces for the sentinel. Returns a non-nil
// error naming every surface that contains the sentinel.
func (s *credentialSweeper) scan(sentinel string) error {
	var hits []string
	for _, surf := range s.surfaces {
		if strings.Contains(surf.value, sentinel) {
			hits = append(hits, fmt.Sprintf("  surface %q contains sentinel: %s…",
				surf.label, truncate(surf.value, 200)))
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return fmt.Errorf("bearer token leaked on %d surface(s):\n%s", len(hits), strings.Join(hits, "\n"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ─── trace collector ─────────────────────────────────────────────────────────

// collectedEvent records a single trace event emitted during a test.
type collectedEvent struct {
	kind    string
	payload map[string]any
}

// eventCollector returns a trace.EventEmitter that appends to a slice and a
// pointer to that slice for later inspection.
func eventCollector() (trace.EventEmitter, *[]collectedEvent) {
	var events []collectedEvent
	emitter := func(kind string, payload map[string]any) {
		events = append(events, collectedEvent{kind: kind, payload: payload})
	}
	return emitter, &events
}

// sweepEvents adds every trace event (kind + all field values) to the sweeper.
func sweepEvents(s *credentialSweeper, events []collectedEvent) {
	for i, ev := range events {
		label := fmt.Sprintf("trace[%d].kind=%s", i, ev.kind)
		s.add(label, ev.kind)
		for k, v := range ev.payload {
			s.add(fmt.Sprintf("trace[%d].%s", i, k), fmt.Sprintf("%v", v))
		}
		s.addJSON(fmt.Sprintf("trace[%d](json)", i), ev)
	}
}

// ─── IMDS mock helpers ────────────────────────────────────────────────────────

// imdsOKServer returns an httptest.Server that responds with a valid IMDS token
// containing the given access token.
func imdsOKServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			http.Error(w, "missing Metadata header", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := fmt.Sprintf("%d", time.Now().Add(70*time.Minute).Unix())
		resp := map[string]string{
			"access_token": token,
			"expires_on":   ts,
			"token_type":   "Bearer",
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

// imdsErrorServer returns a server that responds with an HTTP error and a body
// that deliberately contains the token — to prove the error path does NOT leak
// the response body.
func imdsErrorServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Body contains the sentinel to ensure non-200 error path strips it.
		fmt.Fprintf(w, `{"error":"bad_request","debug_token":"%s"}`, token)
	}))
}

// imdsClientFor wraps an httptest.Server into an imdsHTTPClient.
func imdsClientFor(srv *httptest.Server) imdsHTTPClient {
	return func(req *http.Request) (*http.Response, error) {
		newURL := srv.URL + req.URL.RequestURI()
		newReq, err := http.NewRequestWithContext(req.Context(), req.Method, newURL, nil)
		if err != nil {
			return nil, err
		}
		for k, vs := range req.Header {
			for _, v := range vs {
				newReq.Header.Add(k, v)
			}
		}
		return srv.Client().Do(newReq)
	}
}

// ─── MCP server that captures auth headers ────────────────────────────────────

// authCapturingMCPServer is a minimal MCP server that records the Authorization
// header from each request but DOES NOT echo it back in responses. The
// Authorization header value (containing the bearer token) is stored for the
// test to inspect separately — but the field is never put into a ToolResult.
type authCapturingMCPServer struct {
	capturedAuthHeaders []string
}

func (s *authCapturingMCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Capture the auth header for the test to verify correct delivery —
	// but do NOT include it in any response body.
	s.capturedAuthHeaders = append(s.capturedAuthHeaders, r.Header.Get("Authorization"))

	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	method, _ := req["method"].(string)
	id := req["id"]

	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "initialize":
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"protocolVersion": "2025-03-26",
				"serverInfo":      map[string]any{"name": "test-mcp"},
			},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok"}},
			},
		})
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

// ─── Test: success path — no leak ────────────────────────────────────────────

// TestCredentialLeak_SuccessPath_TokenNotOnAnyOutputSurface proves that after
// a complete successful managed-identity-authenticated MCP call:
//   - the bearer token is NOT in any trace event (kind, payload field, or JSON)
//   - the bearer token is NOT in the ToolResult (stdout, stderr, output map, JSON)
//   - the bearer token is NOT in any error value (there should be none)
//   - the bearer token is NOT in any IndeterminateRecord
func TestCredentialLeak_SuccessPath_TokenNotOnAnyOutputSurface(t *testing.T) {
	imdsSrv := imdsOKServer(t, leakSentinel)
	defer imdsSrv.Close()

	mcpSrv := &authCapturingMCPServer{}
	mcpTS := httptest.NewServer(mcpSrv)
	defer mcpTS.Close()

	// Build provider pointing at the mock IMDS.
	provider := NewManagedIdentityAuthProvider("api://test/scope", "")
	provider.httpClient = imdsClientFor(imdsSrv)

	// Extract hostname from the plain-HTTP MCP server URL.
	mcpHostname := mustParseURL(mcpTS.URL).Hostname()
	gate := NewTokenGate(provider, "api://test/scope", []string{mcpHostname})
	transport := NewMCPHTTPTransport(mcpTS.URL, gate)

	def := toolpkg.ToolDef{
		Name:      "test-tool",
		Transport: toolpkg.TransportMCPHTTP,
		URL:       mcpTS.URL,
	}

	sweeper := &credentialSweeper{}
	emitter, events := eventCollector()
	ctx := trace.WithEventEmitter(context.Background(), emitter)

	result, invokeErr := transport.Invoke(ctx, def, "some-action", map[string]any{"key": "val"})

	// Sweep trace events.
	sweepEvents(sweeper, *events)

	// Sweep result surfaces.
	if result != nil {
		sweeper.add("result.Stdout", result.Stdout)
		sweeper.add("result.Stderr", result.Stderr)
		sweeper.addJSON("result(json)", result)
		for k, v := range result.Output {
			sweeper.add(fmt.Sprintf("result.Output[%s]", k), fmt.Sprintf("%v", v))
		}
	}

	// Sweep error value.
	if invokeErr != nil {
		sweeper.add("invokeErr.Error()", invokeErr.Error())
		var wrapped error = invokeErr
		depth := 0
		for wrapped != nil {
			sweeper.add(fmt.Sprintf("invokeErr.Unwrap[%d]", depth), wrapped.Error())
			wrapped = errors.Unwrap(wrapped)
			depth++
		}
	}

	// The auth header IS expected to contain the token (that's correct — the
	// token was delivered to the allowed host). We are asserting it does NOT
	// leak into OUTPUT surfaces. The captured auth header is the control that
	// proves the token was actually used.
	if len(mcpSrv.capturedAuthHeaders) == 0 {
		t.Fatal("no auth headers captured — token was never attached; test is not exercising the auth path")
	}
	tokenDelivered := false
	for _, h := range mcpSrv.capturedAuthHeaders {
		if strings.Contains(h, leakSentinel) {
			tokenDelivered = true
			break
		}
	}
	if !tokenDelivered {
		t.Fatal("sentinel token was NOT found in Authorization headers — managed-identity provider did not attach it; test is vacuous")
	}

	// Now sweep: the token must not appear on any output surface.
	if err := sweeper.scan(leakSentinel); err != nil {
		t.Errorf("CREDENTIAL LEAK on success path:\n%v", err)
	}
}

// ─── Test: failure paths — errors must not embed the token or response body ──

// TestCredentialLeak_Non200IMDS_ErrorDoesNotLeakToken runs the managed-identity
// provider against a mock IMDS that returns HTTP 400 with a body that contains
// the sentinel. The error message must NOT include the sentinel.
func TestCredentialLeak_Non200IMDS_ErrorDoesNotLeakToken(t *testing.T) {
	imdsSrv := imdsErrorServer(t, leakSentinel)
	defer imdsSrv.Close()

	provider := NewManagedIdentityAuthProvider("api://test/scope", "")
	provider.httpClient = imdsClientFor(imdsSrv)

	sweeper := &credentialSweeper{}
	emitter, events := eventCollector()
	ctx := trace.WithEventEmitter(context.Background(), emitter)

	_, err := provider.Token(ctx)
	if err == nil {
		t.Fatal("expected error from non-200 IMDS response, got nil")
	}

	sweeper.add("provider.Token error", err.Error())
	sweepEvents(sweeper, *events)

	if scanErr := sweeper.scan(leakSentinel); scanErr != nil {
		t.Errorf("CREDENTIAL LEAK on non-200 IMDS path:\n%v", scanErr)
	}
}

// TestCredentialLeak_MalformedJSON_ErrorDoesNotLeakToken checks that malformed
// JSON from IMDS (which might contain a partial token) is not reflected in the
// error. We embed the sentinel in the raw bytes that fail JSON parsing.
func TestCredentialLeak_MalformedJSON_ErrorDoesNotLeakToken(t *testing.T) {
	// IMDS returns 200 OK with a body that is not valid JSON but contains the
	// sentinel — exercises the json.Unmarshal failure path.
	malformedBody := fmt.Sprintf(`{"access_token":"%s","expires_on":NOT-VALID-JSON}`, leakSentinel)

	imdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(malformedBody))
	}))
	defer imdsSrv.Close()

	provider := NewManagedIdentityAuthProvider("api://test/scope", "")
	provider.httpClient = imdsClientFor(imdsSrv)

	sweeper := &credentialSweeper{}
	emitter, events := eventCollector()
	ctx := trace.WithEventEmitter(context.Background(), emitter)

	_, err := provider.Token(ctx)
	if err == nil {
		t.Fatal("expected JSON parse error, got nil")
	}

	sweeper.add("Token error (malformed JSON)", err.Error())
	sweepEvents(sweeper, *events)

	if scanErr := sweeper.scan(leakSentinel); scanErr != nil {
		t.Errorf("CREDENTIAL LEAK on malformed JSON path:\n%v", scanErr)
	}
}

// TestCredentialLeak_ContextCancellation_ErrorDoesNotLeakToken exercises the
// path where the IMDS request is cancelled. Even in the error, no token should
// appear (there is no token yet — but we verify the sentinel from the request
// URL is not echoed either).
func TestCredentialLeak_ContextCancellation_ErrorDoesNotLeakToken(t *testing.T) {
	release := make(chan struct{})
	imdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer imdsSrv.Close()
	defer close(release)

	provider := NewManagedIdentityAuthProvider("api://test/scope", "")
	provider.httpClient = imdsClientFor(imdsSrv)

	ctx, cancel := context.WithCancel(context.Background())
	sweeper := &credentialSweeper{}
	emitter, events := eventCollector()
	ctx = trace.WithEventEmitter(ctx, emitter)

	errc := make(chan error, 1)
	go func() {
		_, err := provider.Token(ctx)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	err := <-errc
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}

	sweeper.add("Token error (context cancelled)", err.Error())
	sweepEvents(sweeper, *events)

	if scanErr := sweeper.scan(leakSentinel); scanErr != nil {
		t.Errorf("CREDENTIAL LEAK on context cancellation path:\n%v", scanErr)
	}
}

// TestCredentialLeak_MCP012_ErrorDoesNotLeakToken exercises the TokenGate
// host-not-allowed path. The provider has the sentinel cached; the error
// returned by AttachToken (MCP-012) must not contain the token.
func TestCredentialLeak_MCP012_ErrorDoesNotLeakToken(t *testing.T) {
	// Pre-seed the provider's cache with the sentinel so Token() would return
	// it — but the gate will block before calling Token().
	provider := &ManagedIdentityAuthProvider{
		resource: "api://test/scope",
	}
	provider.mu.Lock()
	provider.token = leakSentinel
	provider.expiry = time.Now().Add(70 * time.Minute)
	provider.mu.Unlock()

	// Gate configured to allow "allowed.example.com" only.
	gate := NewTokenGate(provider, "api://test/scope", []string{"allowed.example.com"})

	// Request targets a disallowed host.
	req, _ := http.NewRequest(http.MethodGet, "https://evil.example.com/api", nil)
	req.Header = make(http.Header)

	sweeper := &credentialSweeper{}
	emitter, events := eventCollector()
	ctx := trace.WithEventEmitter(context.Background(), emitter)

	err := gate.AttachToken(ctx, req)
	if err == nil {
		t.Fatal("expected MCP-012 error, got nil")
	}

	sweeper.add("gate.AttachToken error (MCP-012)", err.Error())
	sweepEvents(sweeper, *events)

	if scanErr := sweeper.scan(leakSentinel); scanErr != nil {
		t.Errorf("CREDENTIAL LEAK on MCP-012 host rejection path:\n%v", scanErr)
	}
}

// TestCredentialLeak_TraceEvent_NoTokenInAuthAttached exercises the happy path
// of the mcp/authAttached trace event. The event must carry url_host and scope
// — but NOT the token.
func TestCredentialLeak_TraceEvent_NoTokenInAuthAttached(t *testing.T) {
	provider := &ManagedIdentityAuthProvider{
		resource: "api://test/scope",
	}
	provider.mu.Lock()
	provider.token = leakSentinel
	provider.expiry = time.Now().Add(70 * time.Minute)
	provider.mu.Unlock()

	gate := NewTokenGate(provider, "api://test/scope", []string{"example.com"})
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/api", nil)
	req.Header = make(http.Header)

	sweeper := &credentialSweeper{}
	emitter, events := eventCollector()
	ctx := trace.WithEventEmitter(context.Background(), emitter)

	if err := gate.AttachToken(ctx, req); err != nil {
		t.Fatalf("AttachToken unexpected error: %v", err)
	}

	sweepEvents(sweeper, *events)

	// Verify the auth event was actually emitted (non-vacuity check).
	var authEventFound bool
	for _, ev := range *events {
		if ev.kind == string(trace.EventKindMCPAuthAttached) {
			authEventFound = true
		}
	}
	if !authEventFound {
		t.Fatal("mcp/authAttached event was never emitted — test is vacuous")
	}

	if scanErr := sweeper.scan(leakSentinel); scanErr != nil {
		t.Errorf("CREDENTIAL LEAK in mcp/authAttached trace event:\n%v", scanErr)
	}
}

// TestCredentialLeak_IndeterminateRecord_NoToken constructs an IndeterminateRecord
// that carries an endpoint host and verifies the sentinel does not appear in it.
// (IndeterminateRecord.EndpointHost must be ONLY the parsed hostname — never
// a token, full URL, or query parameters.)
func TestCredentialLeak_IndeterminateRecord_NoToken(t *testing.T) {
	// Simulate what the engine would construct for an indeterminate step whose
	// tool had the sentinel as its token. The EndpointHost must be a bare
	// hostname — this test verifies that a correctly-constructed record is clean
	// and that the sweeper would catch a mistake.
	record := &enginepkg.IndeterminateRecord{
		RunID:                "run-001",
		StepID:               "step-invoke",
		ToolName:             "icm",
		ActionName:           "get-incident",
		Classification:       "read-only",
		EndpointHost:         "icm-mcp-prod.azure-api.net", // host only, no token
		AttemptNumber:        1,
		Deadline:             time.Now().Add(30 * time.Second),
		FailureTime:          time.Now(),
		TransportErrCategory: "context-deadline-exceeded",
	}

	sweeper := &credentialSweeper{}
	sweeper.addJSON("IndeterminateRecord", record)
	sweeper.add("IndeterminateRecord.EndpointHost", record.EndpointHost)
	sweeper.add("IndeterminateRecord.ToolName", record.ToolName)

	if scanErr := sweeper.scan(leakSentinel); scanErr != nil {
		// This would mean the record itself (as correctly constructed above)
		// contains the sentinel — impossible unless the test is misconfigured.
		t.Errorf("unexpected sentinel in IndeterminateRecord: %v", scanErr)
	}

	// Prove that if someone accidentally put the token in EndpointHost, the
	// sweeper would catch it (negative control for this specific surface).
	badRecord := &enginepkg.IndeterminateRecord{
		RunID:        "run-002",
		StepID:       "step-invoke",
		EndpointHost: "example.com?token=" + leakSentinel, // deliberate mistake
	}
	badSweeper := &credentialSweeper{}
	badSweeper.addJSON("BadIndeterminateRecord", badRecord)
	badSweeper.add("BadIndeterminateRecord.EndpointHost", badRecord.EndpointHost)

	if badSweeper.scan(leakSentinel) == nil {
		t.Error("negative control failed: sweeper did NOT detect sentinel in IndeterminateRecord.EndpointHost — sweep is broken")
	}
}

// ─── CRITICAL: negative control ──────────────────────────────────────────────

// TestCredentialLeak_SweepDetectsIntentionalLeak is the negative control.
//
// It deliberately injects the sentinel into each of the swept surface types
// and asserts that credentialSweeper.scan returns a non-nil error in every
// case. If any sub-case does NOT fire, the corresponding sweep is broken and
// the entire Item 6 acceptance criterion is void.
//
// This test must always run and must never be skipped or deleted.
func TestCredentialLeak_SweepDetectsIntentionalLeak(t *testing.T) {
	t.Run("plain string surface", func(t *testing.T) {
		s := &credentialSweeper{}
		s.add("error.Error()", "some prefix "+leakSentinel+" some suffix")
		if err := s.scan(leakSentinel); err == nil {
			t.Error("sweep FAILED to detect sentinel in plain string surface — sweep is broken")
		}
	})

	t.Run("trace event kind", func(t *testing.T) {
		s := &credentialSweeper{}
		events := []collectedEvent{{kind: leakSentinel, payload: map[string]any{}}}
		sweepEvents(s, events)
		if err := s.scan(leakSentinel); err == nil {
			t.Error("sweep FAILED to detect sentinel in trace event kind — sweep is broken")
		}
	})

	t.Run("trace event payload field value", func(t *testing.T) {
		s := &credentialSweeper{}
		events := []collectedEvent{{
			kind:    "mcp/authAttached",
			payload: map[string]any{"url_host": leakSentinel},
		}}
		sweepEvents(s, events)
		if err := s.scan(leakSentinel); err == nil {
			t.Error("sweep FAILED to detect sentinel in trace payload field value — sweep is broken")
		}
	})

	t.Run("trace event JSON serialisation", func(t *testing.T) {
		s := &credentialSweeper{}
		events := []collectedEvent{{
			kind:    "mcp/authAttached",
			payload: map[string]any{"scope": "api://test/scope", "nested_tok": leakSentinel},
		}}
		sweepEvents(s, events)
		if err := s.scan(leakSentinel); err == nil {
			t.Error("sweep FAILED to detect sentinel in JSON-serialised trace event — sweep is broken")
		}
	})

	t.Run("ToolResult stdout", func(t *testing.T) {
		s := &credentialSweeper{}
		result := &toolpkg.ToolResult{Stdout: leakSentinel}
		s.add("result.Stdout", result.Stdout)
		if err := s.scan(leakSentinel); err == nil {
			t.Error("sweep FAILED to detect sentinel in ToolResult.Stdout — sweep is broken")
		}
	})

	t.Run("ToolResult JSON", func(t *testing.T) {
		s := &credentialSweeper{}
		result := &toolpkg.ToolResult{Stdout: leakSentinel}
		s.addJSON("result", result)
		if err := s.scan(leakSentinel); err == nil {
			t.Error("sweep FAILED to detect sentinel in JSON-serialised ToolResult — sweep is broken")
		}
	})

	t.Run("IndeterminateRecord endpoint host", func(t *testing.T) {
		s := &credentialSweeper{}
		record := &enginepkg.IndeterminateRecord{
			EndpointHost: leakSentinel,
		}
		s.addJSON("record", record)
		if err := s.scan(leakSentinel); err == nil {
			t.Error("sweep FAILED to detect sentinel in IndeterminateRecord JSON — sweep is broken")
		}
	})

	t.Run("error string", func(t *testing.T) {
		s := &credentialSweeper{}
		err := fmt.Errorf("request failed: token=%s", leakSentinel)
		s.add("error", err.Error())
		if scanErr := s.scan(leakSentinel); scanErr == nil {
			t.Error("sweep FAILED to detect sentinel in error string — sweep is broken")
		}
	})
}

// ─── Test: token not in serialized step output ───────────────────────────────

// TestCredentialLeak_TokenNotInStepOutput exercises the full transport invoke
// path (provider → gate → MCPHTTPTransport) and confirms the ToolResult carries
// no trace of the bearer token in any serializable form.
func TestCredentialLeak_TokenNotInStepOutput(t *testing.T) {
	imdsSrv := imdsOKServer(t, leakSentinel)
	defer imdsSrv.Close()

	// An MCP server whose tools/call response echoes back the call arguments —
	// this is the worst case for leakage: if the token somehow ended up in args,
	// it would appear in the result.
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		method, _ := req["method"].(string)
		id := req["id"]
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"protocolVersion": "2025-03-26",
					"serverInfo":      map[string]any{"name": "echo-mcp"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// Echo the arguments back in the response content — worst case for leakage.
			params, _ := req["params"].(map[string]any)
			argsBytes, _ := json.Marshal(params)
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "args-echo: " + string(argsBytes)},
					},
				},
			})
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}))
	defer mcpSrv.Close()

	provider := NewManagedIdentityAuthProvider("api://test/scope", "")
	provider.httpClient = imdsClientFor(imdsSrv)

	// Extract hostname from mcpSrv URL.
	mcpURL := mcpSrv.URL
	u := mustParseURL(mcpURL)
	hostname := u.Hostname()

	gate := NewTokenGate(provider, "api://test/scope", []string{hostname})
	transport := NewMCPHTTPTransport(mcpURL, gate)

	def := toolpkg.ToolDef{
		Name:      "echo-tool",
		Transport: toolpkg.TransportMCPHTTP,
		URL:       mcpURL,
	}

	sweeper := &credentialSweeper{}
	emitter, events := eventCollector()
	ctx := trace.WithEventEmitter(context.Background(), emitter)

	result, err := transport.Invoke(ctx, def, "run", map[string]any{"input": "safe-value"})

	sweepEvents(sweeper, *events)
	if result != nil {
		sweeper.add("result.Stdout", result.Stdout)
		sweeper.add("result.Stderr", result.Stderr)
		sweeper.addJSON("result(json)", result)
	}
	if err != nil {
		sweeper.add("invoke error", err.Error())
	}

	if scanErr := sweeper.scan(leakSentinel); scanErr != nil {
		t.Errorf("CREDENTIAL LEAK in step output:\n%v", scanErr)
	}
}

// ─── Test: retry-after-401 — Invalidate does not expose cached token ─────────

// TestCredentialLeak_Invalidate_DoesNotExposeToken proves that Invalidate()
// clears the token from the provider cache and that no subsequent error
// (from a failing re-acquire) exposes the previous token value.
func TestCredentialLeak_Invalidate_DoesNotExposeToken(t *testing.T) {
	callCount := 0
	imdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// First call: return the sentinel as the token.
			w.WriteHeader(http.StatusOK)
			ts := fmt.Sprintf("%d", time.Now().Add(70*time.Minute).Unix())
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": leakSentinel,
				"expires_on":   ts,
				"token_type":   "Bearer",
			})
			return
		}
		// Subsequent calls: return an error body that contains the old token —
		// this simulates a server that helpfully echoes back the bad token in
		// diagnostics.
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"token_expired","old_token":"%s"}`, leakSentinel)
	}))
	defer imdsSrv.Close()

	provider := NewManagedIdentityAuthProvider("api://test/scope", "")
	provider.httpClient = imdsClientFor(imdsSrv)

	// Acquire the initial token.
	tok, err := provider.Token(context.Background())
	if err != nil || tok != leakSentinel {
		t.Fatalf("initial Token() failed or returned wrong value: tok=%q err=%v", tok, err)
	}

	// Invalidate to simulate HTTP 401 handling.
	provider.Invalidate()

	// Next Token() call must fail (IMDS returns 401) and must NOT include the
	// previous token in the error.
	_, err = provider.Token(context.Background())
	if err == nil {
		t.Fatal("expected error after Invalidate + failing IMDS, got nil")
	}

	sweeper := &credentialSweeper{}
	sweeper.add("re-acquire error after Invalidate", err.Error())

	if scanErr := sweeper.scan(leakSentinel); scanErr != nil {
		t.Errorf("CREDENTIAL LEAK: cached token appeared in error after Invalidate:\n%v", scanErr)
	}
}

// ─── Synthetic contract: ops-synthetic mcp-http + managed-identity leak test ──

// TestCredentialLeak_SyntheticContract_MCPHTTPWithManagedIdentity is the
// credential-non-leakage assertion for the ops-synthetic contract's mcp-http
// binding with a managed-identity auth provider (Phase 1B Item 4, Invariant 4).
//
// It exercises the exact same path as the production managed-identity +
// mcp-http combination: mock IMDS → token acquisition → TokenGate attachment
// → MCP server request. The credentialSweeper then verifies the token NEVER
// appears on any observable surface:
//   - trace events (mcp/authAttached and others)
//   - ToolResult stdout/stderr
//   - serialised JSON of the result
//   - error messages (if any)
//
// Reuses the credentialSweeper infrastructure rather than writing a weaker
// standalone check. The negative control in
// TestCredentialLeak_SweepDetectsIntentionalLeak remains the authoritative
// proof that the sweeper actually detects real leaks.
func TestCredentialLeak_SyntheticContract_MCPHTTPWithManagedIdentity(t *testing.T) {
	const syntheticToken = "SYNTH-LEAK-SENTINEL-ops-synthetic-7b3f2e1a"

	// Mock IMDS returns the synthetic token.
	imdsSrv := imdsOKServer(t, syntheticToken)
	defer imdsSrv.Close()

	// Non-vacuity: verify token was actually acquired.
	tokenAcquired := false

	// Mock MCP server: accepts status-query, records whether auth header arrived.
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		method, _ := msg["method"].(string)
		id := msg["id"]
		switch method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": mcpProtocolVersion,
					"serverInfo":      map[string]any{"name": "synthetic-test"},
				},
			}
			json.NewEncoder(w).Encode(resp)
		case "notifications/initialized", "shutdown":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			tokenAcquired = true // proof that Invoke actually ran and token was used
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content": []map[string]any{{
						"type": "text",
						"text": `{"id":"svc-api-gw","name":"api-gw","status":"operational","region":"eastus","health_score":98}`,
					}},
				},
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}))
	defer mcpSrv.Close()

	// Build the managed-identity provider with the mock IMDS client.
	provider := NewManagedIdentityAuthProvider("api://ops-synthetic/mcp.tools", "")
	provider.httpClient = imdsClientFor(imdsSrv)

	mcpHost := mustParseURL(mcpSrv.URL).Hostname()
	gate := NewTokenGate(provider, "api://ops-synthetic/mcp.tools", []string{mcpHost})
	transport := NewMCPHTTPTransport(mcpSrv.URL, gate)

	def := toolpkg.ToolDef{
		Name:      "ops-synthetic",
		Transport: toolpkg.TransportMCPHTTP,
		URL:       mcpSrv.URL,
		Actions:   map[string]*toolpkg.ToolAction{"status-query": {}},
	}

	sweeper := &credentialSweeper{}
	emitter, events := eventCollector()
	ctx := trace.WithEventEmitter(context.Background(), emitter)

	result, err := transport.Invoke(ctx, def, "status-query", map[string]any{"target": "api-gw"})

	sweepEvents(sweeper, *events)
	if result != nil {
		sweeper.add("result.Stdout", result.Stdout)
		sweeper.add("result.Stderr", result.Stderr)
		sweeper.addJSON("result(json)", result)
	}
	if err != nil {
		sweeper.add("invoke error", err.Error())
	}

	// Non-vacuity: the test is only valid if the token was actually acquired
	// and attached during the invocation.
	if !tokenAcquired {
		t.Fatal("non-vacuity FAILED: tools/call handler was never reached — " +
			"the token was not acquired or attached. The sweep result is meaningless.")
	}

	if scanErr := sweeper.scan(syntheticToken); scanErr != nil {
		t.Errorf("CREDENTIAL LEAK in ops-synthetic mcp-http + managed-identity path:\n%v", scanErr)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// mustParseURL parses a URL string, panicking if it fails.
func mustParseURL(raw string) *urlHelper {
	// Inline minimal URL parse to avoid importing net/url at package level.
	// We only need hostname extraction.
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[:j]
		}
		if k := strings.LastIndex(rest, ":"); k >= 0 {
			return &urlHelper{host: rest[:k]}
		}
		return &urlHelper{host: rest}
	}
	return &urlHelper{host: raw}
}

type urlHelper struct{ host string }

func (u *urlHelper) Hostname() string { return u.host }

// ─── compile-time import use check ───────────────────────────────────────────

// These ensure imported packages are referenced even if only used in one test.
var (
	_ = json.Marshal
	_ = errors.Unwrap
)
