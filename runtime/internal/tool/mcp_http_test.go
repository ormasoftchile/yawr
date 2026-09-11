package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	schemapkg "github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// fakeMCPServer is an in-process httptest server that implements a minimal
// Streamable HTTP MCP endpoint for testing MCPHTTPTransport.
type fakeMCPServer struct {
	// respondWithSSE controls whether tools/call responds via SSE or JSON.
	respondWithSSE bool
	// toolError makes the next tools/call respond with isError: true.
	toolError bool
	// serverError makes the server return HTTP 5xx.
	serverError bool
	// sessionID is issued on initialize.
	sessionID string
	// capturedHeaders records the headers sent by the transport on each request.
	capturedHeaders []http.Header
	// initCount tracks how many times initialize was called.
	initCount int
}

func (s *fakeMCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Capture headers for inspection.
	headers := r.Header.Clone()
	s.capturedHeaders = append(s.capturedHeaders, headers)

	if s.serverError {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	method, _ := req["method"].(string)
	id := req["id"] // may be nil for notifications

	switch method {
	case "initialize":
		s.initCount++
		if s.sessionID != "" {
			w.Header().Set("Mcp-Session-Id", s.sessionID)
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"serverInfo":      map[string]any{"name": "test-server"},
			},
		}
		json.NewEncoder(w).Encode(resp)

	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)

	case "shutdown":
		w.WriteHeader(http.StatusAccepted)

	case "tools/list":
		w.Header().Set("Content-Type", "application/json")
		idInt := int(id.(float64))
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      idInt,
			"result": map[string]any{
				"tools": []map[string]any{
					{"name": "echo", "description": "echoes input"},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)

	case "tools/call":
		idInt := int(id.(float64))
		params, _ := req["params"].(map[string]any)
		args, _ := params["arguments"].(map[string]any)
		inputText, _ := args["text"].(string)

		var result map[string]any
		if s.toolError {
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "tool failure"}},
				"isError": true,
			}
		} else {
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": inputText}},
			}
		}
		rpc := map[string]any{
			"jsonrpc": "2.0",
			"id":      idInt,
			"result":  result,
		}

		if s.respondWithSSE {
			// Respond with SSE containing optional notification then the response.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			// Emit a notification first (no id).
			notif := map[string]any{
				"jsonrpc": "2.0",
				"method":  "notifications/progress",
				"params":  map[string]any{"status": "running"},
			}
			notifBytes, _ := json.Marshal(notif)
			fmt.Fprintf(w, "data: %s\n\n", notifBytes)

			// Then emit the real response.
			rpcBytes, _ := json.Marshal(rpc)
			fmt.Fprintf(w, "data: %s\n\n", rpcBytes)

			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(rpc)
		}

	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

// TestMCPHTTPTransport_MappedToolAndInputOnWire proves mcp-http sends the
// declared remote MCP tool name and adapted provider arguments, not the logical
// action name/args. This is the ICM shape: get-incident + incident_id string →
// get_incident_details_by_id + incidentId integer.
func TestMCPHTTPTransport_MappedToolAndInputOnWire(t *testing.T) {
	var capturedName string
	var capturedArgs map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": mcpProtocolVersion}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			params, _ := req["params"].(map[string]any)
			capturedName, _ = params["name"].(string)
			capturedArgs, _ = params["arguments"].(map[string]any)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  map[string]any{"content": []map[string]any{{"type": "text", "text": `{"title":"ok"}`}}},
			})
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{
		Name:      "icm",
		Transport: toolpkg.TransportMCPHTTP,
		URL:       ts.URL,
		Actions: map[string]*toolpkg.ToolAction{
			"get-incident": {
				MCPTool: "get_incident_details_by_id",
				MCPInput: map[string]*schemapkg.VSCodeInputMapping{
					"incidentId": {From: "incident_id", Coerce: "integer", Required: true},
				},
			},
		},
	}

	_, err := transport.Invoke(context.Background(), def, "get-incident", map[string]any{"incident_id": "852896186"})
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	if capturedName != "get_incident_details_by_id" {
		t.Fatalf("MCP tool name on wire = %q, want get_incident_details_by_id", capturedName)
	}
	if _, ok := capturedArgs["incident_id"]; ok {
		t.Fatalf("logical arg incident_id leaked onto MCP wire: %#v", capturedArgs)
	}
	if got := capturedArgs["incidentId"]; got != float64(852896186) {
		t.Fatalf("incidentId on wire = %T %#v, want JSON number 852896186", got, got)
	}
}

// TestMCPHTTPTransport_JSONResponse tests the happy path with a plain JSON response.
func TestMCPHTTPTransport_JSONResponse(t *testing.T) {
	srv := &fakeMCPServer{sessionID: "sess-001"}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "test-tool", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	if result.Stdout != "hello" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}

	// Session ID should be echoed on subsequent calls.
	if transport.sessionID != "sess-001" {
		t.Errorf("sessionID = %q, want %q", transport.sessionID, "sess-001")
	}
}

// TestMCPHTTPTransport_SSEResponse tests SSE framing, including that a
// notification before the response is skipped and the real response is returned.
func TestMCPHTTPTransport_SSEResponse(t *testing.T) {
	srv := &fakeMCPServer{respondWithSSE: true}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "test-tool", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"text": "sse-test"})
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	if result.Stdout != "sse-test" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "sse-test")
	}
}

// TestMCPHTTPTransport_SSEMultiLineData tests multi-line SSE events: data split
// across two data: lines joined by \n, with SSE comment (heartbeat) interleaved.
// This tests that the parser handles the blank-line event boundary, skips
// ': hb' comment lines, and correctly assembles the payload.
func TestMCPHTTPTransport_SSEMultiLineData(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": mcpProtocolVersion}}
			json.NewEncoder(w).Encode(resp)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// Read the actual request ID to correlate in the response.
			reqID := req["id"]
			idFloat, _ := reqID.(float64)
			idInt := int(idFloat)

			// Build split JSON at a token boundary.
			part1 := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d`, idInt)
			part2 := `,"result":{"content":[{"type":"text","text":"multiline"}]}}`

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Heartbeat comment + multi-line data event.
			fmt.Fprintf(w, ": hb\ndata: %s\ndata: %s\n\n", part1, part2)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	if result.Stdout != "multiline" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "multiline")
	}
}

// TestMCPHTTPTransport_ToolError verifies that isError:true in the MCP response
// produces a non-zero ExitCode and an error — identical to stdio MCP behaviour.
func TestMCPHTTPTransport_ToolError(t *testing.T) {
	srv := &fakeMCPServer{toolError: true}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "fail", nil)
	if err == nil {
		t.Fatal("expected error for isError tool response")
	}
	if !strings.Contains(err.Error(), "mcp tool error") {
		t.Errorf("error %q should contain 'mcp tool error'", err.Error())
	}
}

// TestMCPHTTPTransport_SessionID verifies that the session ID from initialize
// is sent on subsequent requests, and that a server that omits it is handled.
func TestMCPHTTPTransport_SessionID(t *testing.T) {
	// Server with no session ID.
	srv := &fakeMCPServer{sessionID: ""}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"text": "x"})
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	// No panic, no error — omitted session ID is acceptable.
	if transport.sessionID != "" {
		t.Errorf("expected empty sessionID when server omits it")
	}
}

// TestMCPHTTPTransport_UnexpectedContentType verifies MCP-005 is emitted.
func TestMCPHTTPTransport_UnexpectedContentType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": mcpProtocolVersion}}
			json.NewEncoder(w).Encode(resp)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "unexpected")
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected MCP-005 error for unexpected content-type")
	}
	if !strings.Contains(err.Error(), "MCP-005") {
		t.Errorf("error %q should contain MCP-005", err.Error())
	}
}

// TestMCPHTTPTransport_ProtocolVersionHeader verifies that MCP-Protocol-Version
// is sent on every request.
func TestMCPHTTPTransport_ProtocolVersionHeader(t *testing.T) {
	srv := &fakeMCPServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}
	_, _ = transport.Invoke(context.Background(), def, "echo", map[string]any{"text": "x"})

	for i, h := range srv.capturedHeaders {
		if h.Get("MCP-Protocol-Version") != mcpProtocolVersion {
			t.Errorf("request %d missing MCP-Protocol-Version header", i)
		}
	}
}

// TestMCPHTTPTransport_RuntimeWiring proves that a tool declared with
// mode: mcp-http actually reaches MCPHTTPTransport through DefaultToolRuntime,
// rather than via direct construction. This is the "dead-code" guard.
func TestMCPHTTPTransport_RuntimeWiring(t *testing.T) {
	srv := &fakeMCPServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Build a ToolRegistry with a single mcp-http tool.
	reg := newTestRegistry(t, toolpkg.ToolDef{
		Name:      "mcp-http-tool",
		Transport: toolpkg.TransportMCPHTTP,
		URL:       ts.URL,
		Actions: map[string]*toolpkg.ToolAction{
			"echo": {Description: "echo", Args: map[string]*toolpkg.ArgDef{"text": {Type: "string"}}},
		},
	})

	runtime := NewDefaultToolRuntime(reg)
	defer runtime.Close()

	result, err := runtime.Invoke(context.Background(), "mcp-http-tool", "echo", map[string]any{"text": "wired"})
	if err != nil {
		t.Fatalf("runtime.Invoke: unexpected error: %v", err)
	}
	if result.Stdout != "wired" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "wired")
	}
}

// TestMCPHTTPTransport_SessionExpiry verifies that a 404 mid-run triggers
// re-initialization and retries the request.
func TestMCPHTTPTransport_SessionExpiry(t *testing.T) {
	callCount := 0
	initCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			initCount++
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": mcpProtocolVersion}}
			json.NewEncoder(w).Encode(resp)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			callCount++
			if callCount == 1 {
				// First call: simulate expired session.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Second call (after re-init): succeed.
			idInt := int(id.(float64))
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      idInt,
				"result":  map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	result, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	if result.Stdout != "ok" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "ok")
	}
	if initCount != 2 {
		t.Errorf("expected 2 init calls (initial + re-init), got %d", initCount)
	}
}

// TestMCPHTTPTransport_ListTools verifies the tools/list response parsing.
func TestMCPHTTPTransport_ListTools(t *testing.T) {
	srv := &fakeMCPServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	tools, err := transport.ListTools(context.Background(), def)
	if err != nil {
		t.Fatalf("ListTools: unexpected error: %v", err)
	}
	if len(tools) != 1 || tools[0]["name"] != "echo" {
		t.Errorf("unexpected tools: %v", tools)
	}
}

// TestMCPHTTPTransport_InitializeError verifies Correction 4 from Ken's recon:
// when the server returns a JSON-RPC error on initialize, the transport must
// fail loudly and NOT mark the session as initialized. This is the defect
// present in the stdio MCPTransport (which does not inspect the init response)
// that we explicitly do not replicate.
func TestMCPHTTPTransport_InitializeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := req["id"]
		w.Header().Set("Content-Type", "application/json")
		// Return a JSON-RPC error instead of a valid initialize response.
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32600, "message": "unsupported protocol version"},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	transport := NewMCPHTTPTransport(ts.URL, nil)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: ts.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected error when server returns JSON-RPC error on initialize")
	}
	if !strings.Contains(err.Error(), "initialize failed") {
		t.Errorf("error %q should mention 'initialize failed'", err.Error())
	}
	// The session must NOT be marked initialized.
	if transport.initialized {
		t.Error("transport.initialized must be false after a failed initialize")
	}
}

// ─── test helpers ─────────────────────────────────────────────────────────

type testRegistry struct {
	defs map[string]*toolpkg.ToolDef
}

func newTestRegistry(t *testing.T, defs ...toolpkg.ToolDef) *testRegistry {
	t.Helper()
	r := &testRegistry{defs: make(map[string]*toolpkg.ToolDef)}
	for i := range defs {
		d := defs[i]
		r.defs[d.Name] = &d
	}
	return r
}

func (r *testRegistry) Lookup(name string) (*toolpkg.ToolDef, bool) {
	d, ok := r.defs[name]
	return d, ok
}

func (r *testRegistry) All() []toolpkg.ToolDef {
	out := make([]toolpkg.ToolDef, 0, len(r.defs))
	for _, d := range r.defs {
		out = append(out, *d)
	}
	return out
}

func (r *testRegistry) Register(def toolpkg.ToolDef) error {
	r.defs[def.Name] = &def
	return nil
}

// TestMCPHTTPTransport_SchemaRuntimeWiring proves the full path from
// schema.TransportMCPHTTP (the YAML constant) through RuntimeToolDef →
// mapTransport → toolpkg.TransportMCPHTTP → DefaultToolRuntime → MCPHTTPTransport.
// This closes the gap that TestMCPHTTPTransport_RuntimeWiring leaves open:
// that test constructs toolpkg.TransportMCPHTTP directly; this one converts
// from the schema layer first, the same path ScanDir takes from a .tool.yaml file.
func TestMCPHTTPTransport_SchemaRuntimeWiring(t *testing.T) {
	srv := &fakeMCPServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Build a schema.ToolDef as the YAML parser produces (Transport.Type = "mcp-http").
	schemaDef := &schemapkg.ToolDef{
		Name: "schema-mcp-http-tool",
		Transport: schemapkg.TransportConfig{
			Type: schemapkg.TransportMCPHTTP,
			URL:  ts.URL, // http:// is fine here; we skip ValidateTransportConfig
		},
		Actions: map[string]*schemapkg.ToolAction{
			"echo": {Description: "echo", Args: map[string]*schemapkg.ArgDef{"text": {Type: "string"}}},
		},
	}

	runtimeDef, err := RuntimeToolDef(schemaDef)
	if err != nil {
		t.Fatalf("RuntimeToolDef: unexpected error: %v", err)
	}
	if runtimeDef.Transport != toolpkg.TransportMCPHTTP {
		t.Fatalf("RuntimeToolDef: Transport = %q, want %q", runtimeDef.Transport, toolpkg.TransportMCPHTTP)
	}
	if runtimeDef.URL != ts.URL {
		t.Fatalf("RuntimeToolDef: URL = %q, want %q", runtimeDef.URL, ts.URL)
	}

	// Now prove DefaultToolRuntime routes it to MCPHTTPTransport.
	reg := newTestRegistry(t, runtimeDef)
	runtime := NewDefaultToolRuntime(reg)
	defer runtime.Close()

	result, err := runtime.Invoke(context.Background(), "schema-mcp-http-tool", "echo", map[string]any{"text": "schema-wired"})
	if err != nil {
		t.Fatalf("runtime.Invoke: unexpected error: %v", err)
	}
	if result.Stdout != "schema-wired" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "schema-wired")
	}
}

// TestMCPHTTPTransport_RedirectBlocked_Authenticated verifies MCP-013:
// an authenticated transport must not follow redirects. The bearer token
// must never be forwarded to the redirect target.
func TestMCPHTTPTransport_RedirectBlocked_Authenticated(t *testing.T) {
	// Redirect target: a valid MCP server.
	target := httptest.NewServer(&fakeMCPServer{})
	defer target.Close()

	// Redirecting front-end (issues 307 to preserve POST method).
	redirecter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer redirecter.Close()

	redirectHost := redirecter.Listener.Addr().(*net.TCPAddr).IP.String()

	// Gate with the redirecter's host in allowed_hosts so AttachToken succeeds
	// for the initial request; CheckRedirect fires before the token reaches
	// the redirect target.
	gate := NewTokenGate(makeStaticProvider("secret-tok"), "", []string{redirectHost})
	transport := NewMCPHTTPTransport(redirecter.URL, gate)
	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: redirecter.URL}

	_, err := transport.Invoke(context.Background(), def, "echo", nil)
	if err == nil {
		t.Fatal("expected MCP-013 error for redirect on authenticated transport")
	}
	if !errors.Is(err, errkit.ErrMCP013) {
		t.Errorf("error %v should unwrap to ErrMCP013", err)
	}
}

// TestMCPHTTPTransport_NoCheckRedirectOnUnauthenticated verifies that
// NewMCPHTTPTransport does not install CheckRedirect when gate is nil,
// so unauthenticated transports may follow redirects.
func TestMCPHTTPTransport_NoCheckRedirectOnUnauthenticated(t *testing.T) {
	transport := NewMCPHTTPTransport("https://example.com/v1/", nil)
	if transport.httpClient.CheckRedirect != nil {
		t.Error("unauthenticated transport must not set CheckRedirect")
	}
}

// TestMCPHTTPTransport_CheckRedirectInstalledOnAuthenticated verifies that
// NewMCPHTTPTransport installs CheckRedirect when a gate is provided, and
// that it returns http.ErrUseLastResponse (so send() sees the redirect
// response and emits MCP-013 with the destination, rather than a generic
// transport error).
func TestMCPHTTPTransport_CheckRedirectInstalledOnAuthenticated(t *testing.T) {
	gate := NewTokenGate(makeStaticProvider("tok"), "", []string{"example.com"})
	transport := NewMCPHTTPTransport("https://example.com/v1/", gate)
	if transport.httpClient.CheckRedirect == nil {
		t.Fatal("authenticated transport must set CheckRedirect")
	}
	// CheckRedirect must return http.ErrUseLastResponse so the redirect
	// response is surfaced to send() for clean MCP-013 emission.
	req, _ := http.NewRequest("POST", "https://example.com/v1/", nil)
	err := transport.httpClient.CheckRedirect(req, []*http.Request{req})
	if !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect must return http.ErrUseLastResponse, got %v", err)
	}
}

// TestMCPHTTPTransport_TokenNeverReachesRedirectTarget is the security proof
// for MCP-013 / B-32: a sentinel bearer token must never appear in a request
// to the redirect destination. The redirecting server is in allowed_hosts;
// the redirect target is not — and even if it were, the redirect is blocked
// by CheckRedirect before any further request is sent.
func TestMCPHTTPTransport_TokenNeverReachesRedirectTarget(t *testing.T) {
	const sentinelToken = "sentinel-bearer-never-forward-99887766"

	// Evil server — must never receive any Authorization header.
	evilReceived := false
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			evilReceived = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()

	// Redirecting front-end: 302 to evil on every POST.
	redirecter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer redirecter.Close()

	redirectHost := redirecter.Listener.Addr().(*net.TCPAddr).IP.String()
	gate := NewTokenGate(makeStaticProvider(sentinelToken), "api://scope", []string{redirectHost})
	transport := NewMCPHTTPTransport(redirecter.URL, gate)

	def := toolpkg.ToolDef{Name: "t", Transport: toolpkg.TransportMCPHTTP, URL: redirecter.URL}
	_, err := transport.Invoke(context.Background(), def, "echo", nil)

	if err == nil {
		t.Fatal("expected MCP-013 error when redirect blocked, got nil")
	}
	if !errors.Is(err, errkit.ErrMCP013) {
		t.Errorf("expected MCP-013, got: %v", err)
	}
	// The core security assertion: the token must never have left for the evil server.
	if evilReceived {
		t.Error("SECURITY FAILURE: bearer token was forwarded to the redirect target")
	}
}
