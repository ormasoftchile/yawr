package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	schemapkg "github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type fakeBridgeServer struct {
	mu           sync.Mutex
	version      string
	statusCode   int
	result       map[string]any
	errDetail    *bridgeErrorDetail
	hang         bool
	malformed    string
	lastReq      *bridgeRequest
	requestCount int
}

func (s *fakeBridgeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.hang {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		return
	}

	var req bridgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.requestCount++
	captured := req
	s.lastReq = &captured
	s.mu.Unlock()

	if s.malformed != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.malformed))
		return
	}

	if s.statusCode != 0 && s.statusCode != http.StatusOK {
		w.WriteHeader(s.statusCode)
		return
	}

	version := s.version
	if version == "" {
		version = vscodeBridgeVersion
	}
	resp := bridgeResponse{
		Version:   version,
		RequestID: req.RequestID,
		Result:    s.result,
		Error:     s.errDetail,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func newLoopbackBridgeServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(handler)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	ts.Listener = listener
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func newBridgeTransport(t *testing.T, bridgeURL string) *VSCodeMCPTransport {
	t.Helper()
	t.Setenv(vscodeBridgeTokenEnvVar, "test-sentinel-token-for-bridge-tests")
	transport := newVSCodeMCPTransport()
	transport.bridgeURL = bridgeURL
	return transport
}

func sweepToolResult(s *credentialSweeper, result *toolpkg.ToolResult) {
	if result == nil {
		return
	}
	s.add("result.Stdout", result.Stdout)
	s.add("result.Stderr", result.Stderr)
	s.addJSON("result(json)", result)
	for k, v := range result.Output {
		s.add(fmt.Sprintf("result.Output[%s]", k), fmt.Sprintf("%v", v))
	}
}

func collectErrChain(s *credentialSweeper, label string, err error) {
	for i, cur := 0, err; cur != nil; i, cur = i+1, errors.Unwrap(cur) {
		s.add(fmt.Sprintf("%s[%d]", label, i), cur.Error())
	}
}

func TestVSCodeMCP_Success(t *testing.T) {
	srv := &fakeBridgeServer{result: map[string]any{"foo": "bar"}}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := transport.Invoke(ctx, toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", map[string]any{"target": "api"})
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	if result == nil || result.Output == nil {
		t.Fatalf("Output is nil but expected map with key foo")
	}
	if got := result.Output["foo"]; got != "bar" {
		t.Fatalf("Output[foo] = %#v, want %q", got, "bar")
	}

	// Mutation control: if we remove the output mapping (replace "resp.Result" with nil),
	// this assertion fails with: "Output is nil but expected map with key foo".
	// MEASURED: test fails as expected when production wiring is removed.

	srv.mu.Lock()
	lastReq := srv.lastReq
	srv.mu.Unlock()
	if lastReq == nil {
		t.Fatal("bridge did not capture request")
	}
	if lastReq.Version != vscodeBridgeVersion {
		t.Fatalf("request version = %q, want %q", lastReq.Version, vscodeBridgeVersion)
	}
	if lastReq.Tool != "ops-synthetic" || lastReq.Action != "status-query" {
		t.Fatalf("request routing = %s/%s, want ops-synthetic/status-query", lastReq.Tool, lastReq.Action)
	}
	if lastReq.CapabilityProof != transport.capSecret {
		t.Fatal("capability proof mismatch")
	}
	if lastReq.DeadlineUnixMS == 0 {
		t.Fatal("deadline_unix_ms was not propagated from context deadline")
	}
}

func TestVSCodeMCP_TypedOutputMismatch(t *testing.T) {
	srv := &fakeBridgeServer{result: map[string]any{"count": "not-a-number"}}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)

	result, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "pattern-search", map[string]any{"target": "api", "query": "x"})
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}
	got, ok := result.Output["count"].(string)
	if !ok || got != "not-a-number" {
		t.Fatalf("Output[count] = %#v (%T), want raw string pass-through", result.Output["count"], result.Output["count"])
	}
}

func TestVSCodeMCP_Timeout(t *testing.T) {
	srv := &fakeBridgeServer{hang: true}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := transport.Invoke(ctx, toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
}

func TestVSCodeMCP_Cancellation(t *testing.T) {
	srv := &fakeBridgeServer{hang: true}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := transport.Invoke(ctx, toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}

func TestVSCodeMCP_DuplicateRequest(t *testing.T) {
	srv := &fakeBridgeServer{result: map[string]any{"ok": true}}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)
	def := toolpkg.ToolDef{Name: "ops-synthetic"}
	args := map[string]any{"target": "api"}

	result1, err := transport.Invoke(context.Background(), def, "status-query", args)
	if err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
	result2, err := transport.Invoke(context.Background(), def, "status-query", args)
	if err != nil {
		t.Fatalf("second Invoke: %v", err)
	}
	if result1.Output["ok"] != true || result2.Output["ok"] != true {
		t.Fatalf("duplicate invocations did not both succeed: first=%v second=%v", result1.Output, result2.Output)
	}

	// Mutation control: if request dispatch skips the second response mapping,
	// the second assertion above fails because result2.Output["ok"] is not true.
	// MEASURED: test fails as expected when the second call is not wired through.

	srv.mu.Lock()
	count := srv.requestCount
	srv.mu.Unlock()
	if count != 2 {
		t.Fatalf("requestCount = %d, want 2 independent bridge requests", count)
	}
}

func TestVSCodeMCP_MalformedResponse(t *testing.T) {
	srv := &fakeBridgeServer{malformed: `{"version":`}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)

	_, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
	if err == nil {
		t.Fatal("expected malformed response error, got nil")
	}
	if !strings.Contains(err.Error(), "malformed bridge response") {
		t.Fatalf("error = %q, want malformed bridge response", err.Error())
	}
}

func TestVSCodeMCP_CapabilityRejection(t *testing.T) {
	srv := &fakeBridgeServer{statusCode: http.StatusUnauthorized}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)

	_, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
	if err == nil {
		t.Fatal("expected capability rejection error, got nil")
	}
	if !strings.Contains(err.Error(), "capability secret was rejected") {
		t.Fatalf("error = %q, want capability rejection", err.Error())
	}
}

func TestVSCodeMCP_ExtensionDisconnect(t *testing.T) {
	srv := &fakeBridgeServer{statusCode: http.StatusServiceUnavailable}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)

	_, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
	if err == nil {
		t.Fatal("expected extension disconnect error, got nil")
	}
	if !strings.Contains(err.Error(), "not connected or has been closed") {
		t.Fatalf("error = %q, want disconnect message", err.Error())
	}
}

func TestVSCodeMCP_VersionMismatch(t *testing.T) {
	srv := &fakeBridgeServer{version: "vscode-mcp-bridge/v2", result: map[string]any{"ok": true}}
	ts := newLoopbackBridgeServer(t, srv)
	transport := newBridgeTransport(t, ts.URL)

	_, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
	if err == nil {
		t.Fatal("expected version mismatch error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, vscodeBridgeVersion) || !strings.Contains(msg, "vscode-mcp-bridge/v2") {
		t.Fatalf("error = %q, want both client and bridge versions named", msg)
	}
}

func TestVSCodeMCP_CapabilitySecretRedaction(t *testing.T) {
	tests := []struct {
		name    string
		server  *fakeBridgeServer
		wantErr string
	}{
		{name: "non-200 capability rejection", server: &fakeBridgeServer{statusCode: http.StatusUnauthorized}, wantErr: "capability secret was rejected"},
		{name: "malformed", server: &fakeBridgeServer{malformed: `not-json`}, wantErr: "malformed bridge response"},
		{name: "disconnected", server: &fakeBridgeServer{statusCode: http.StatusServiceUnavailable}, wantErr: "not connected or has been closed"},
		{name: "version mismatch", server: &fakeBridgeServer{version: "vscode-mcp-bridge/v2", result: map[string]any{"ok": true}}, wantErr: "bridge version mismatch"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newLoopbackBridgeServer(t, tc.server)
			transport := newBridgeTransport(t, ts.URL)
			sentinel := transport.capSecret
			sweeper := &credentialSweeper{}

			result, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
			sweepToolResult(sweeper, result)
			collectErrChain(sweeper, "invokeErr", err)
			if scanErr := sweeper.scan(sentinel); scanErr != nil {
				t.Fatalf("capability secret leaked on failure path %q:\n%v", tc.name, scanErr)
			}
		})
	}

	t.Run("success result surfaces", func(t *testing.T) {
		srv := &fakeBridgeServer{result: map[string]any{"foo": "bar"}}
		ts := newLoopbackBridgeServer(t, srv)
		transport := newBridgeTransport(t, ts.URL)
		sentinel := transport.capSecret
		sweeper := &credentialSweeper{}

		result, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		sweepToolResult(sweeper, result)
		if scanErr := sweeper.scan(sentinel); scanErr != nil {
			t.Fatalf("capability secret leaked on success surfaces:\n%v", scanErr)
		}
	})
}

func TestVSCodeMCP_CapabilitySecretSweepDetectsLeak(t *testing.T) {
	t.Setenv(vscodeBridgeTokenEnvVar, "sweep-detection-test-token-abc123")
	s := &credentialSweeper{}
	transport := newVSCodeMCPTransport()
	s.add("intentional-leak", "oops "+transport.capSecret)
	if err := s.scan(transport.capSecret); err == nil {
		t.Fatal("sweep FAILED to detect intentional capability-secret leak")
	}
}

func TestVSCodeMCP_RuntimeWiring(t *testing.T) {
	srv := &fakeBridgeServer{result: map[string]any{"ok": true}}
	ts := newLoopbackBridgeServer(t, srv)
	t.Setenv(vscodeBridgeEnvVar, ts.URL)
	t.Setenv(vscodeBridgeTokenEnvVar, "runtime-wiring-test-token")

	reg := newTestRegistry(t, toolpkg.ToolDef{
		Name:      "vscode-tool",
		Transport: toolpkg.TransportVSCodeMCP,
		Actions:   map[string]*toolpkg.ToolAction{"run": {Description: "run"}},
	})
	runtime := NewDefaultToolRuntime(reg)
	defer runtime.Close()

	result, err := runtime.Invoke(context.Background(), "vscode-tool", "run", map[string]any{"arg": "value"})
	if err != nil {
		t.Fatalf("runtime.Invoke: %v", err)
	}
	if result.Output["ok"] != true {
		t.Fatalf("runtime output = %v, want ok=true", result.Output)
	}
}

func TestVSCodeMCP_TestdataFixture_Parses(t *testing.T) {
	fixture := filepath.Join(repoRoot(), "internal", "tool", "testdata", "vscode-mcp-ops-synthetic.tool.yaml")
	def, err := ParseToolFile(fixture)
	if err != nil {
		t.Fatalf("ParseToolFile: %v", err)
	}
	if def.Transport.Type != schemapkg.TransportVSCodeMCP {
		t.Fatalf("transport type = %q, want %q", def.Transport.Type, schemapkg.TransportVSCodeMCP)
	}
	runtimeDef, err := RuntimeToolDef(def)
	if err != nil {
		t.Fatalf("RuntimeToolDef: %v", err)
	}
	if runtimeDef.Transport != toolpkg.TransportVSCodeMCP {
		t.Fatalf("runtime transport = %q, want %q", runtimeDef.Transport, toolpkg.TransportVSCodeMCP)
	}
}

// TestVSCodeMCP_TokenAbsent verifies that when YAWR_VSCODE_BRIDGE_TOKEN is
// unset, Invoke returns the actionable error and performs zero HTTP requests.
func TestVSCodeMCP_TokenAbsent(t *testing.T) {
	srv := &fakeBridgeServer{result: map[string]any{"ok": true}}
	ts := newLoopbackBridgeServer(t, srv)

	t.Setenv(vscodeBridgeTokenEnvVar, "")
	t.Setenv(vscodeBridgeEnvVar, ts.URL)

	transport := newVSCodeMCPTransport()

	_, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
	if err == nil {
		t.Fatal("expected error when token is absent, got nil")
	}
	want := "YAWR_VSCODE_BRIDGE_TOKEN not set"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err.Error(), want)
	}

	srv.mu.Lock()
	count := srv.requestCount
	srv.mu.Unlock()
	if count != 0 {
		t.Fatalf("bridge received %d requests, want 0 (no request should be sent without a token)", count)
	}
}

// TestVSCodeMCP_TokenPresentVerbatim verifies the token is transmitted
// verbatim as capability_proof, as asserted server-side in the fake bridge.
func TestVSCodeMCP_TokenPresentVerbatim(t *testing.T) {
	const sentinelToken = "my-verbatim-test-token-xyz987"

	srv := &fakeBridgeServer{result: map[string]any{"ok": true}}
	ts := newLoopbackBridgeServer(t, srv)

	t.Setenv(vscodeBridgeTokenEnvVar, sentinelToken)
	t.Setenv(vscodeBridgeEnvVar, ts.URL)

	transport := newVSCodeMCPTransport()

	_, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
	if err != nil {
		t.Fatalf("Invoke: unexpected error: %v", err)
	}

	srv.mu.Lock()
	lastReq := srv.lastReq
	srv.mu.Unlock()
	if lastReq == nil {
		t.Fatal("bridge did not capture request")
	}
	if lastReq.CapabilityProof != sentinelToken {
		t.Fatalf("capability_proof = %q, want verbatim token %q", lastReq.CapabilityProof, sentinelToken)
	}
}

// TestVSCodeMCP_MalformedBridgeURL verifies that a non-loopback or garbage
// bridge URL returns an error and does NOT panic.
func TestVSCodeMCP_MalformedBridgeURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"non-loopback IP", "http://10.0.0.5:7779"},
		{"garbage", "not-a-valid-url-at-all"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(vscodeBridgeTokenEnvVar, "some-token")
			t.Setenv(vscodeBridgeEnvVar, tc.url)

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("constructor panicked with %v for URL %q", r, tc.url)
				}
			}()

			transport := newVSCodeMCPTransport()

			_, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
			if err == nil {
				t.Fatalf("expected error for malformed URL %q, got nil", tc.url)
			}
		})
	}
}

// TestVSCodeMCP_TokenRedaction verifies the token never appears in the
// returned error text or ToolResult.
func TestVSCodeMCP_TokenRedaction(t *testing.T) {
	const sentinelToken = "super-secret-redaction-test-token-111"

	tests := []struct {
		name   string
		server *fakeBridgeServer
	}{
		{"capability rejection", &fakeBridgeServer{statusCode: http.StatusUnauthorized}},
		{"malformed response", &fakeBridgeServer{malformed: `not-json`}},
		{"disconnected", &fakeBridgeServer{statusCode: http.StatusServiceUnavailable}},
		{"version mismatch", &fakeBridgeServer{version: "vscode-mcp-bridge/v2", result: map[string]any{"ok": true}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newLoopbackBridgeServer(t, tc.server)
			t.Setenv(vscodeBridgeTokenEnvVar, sentinelToken)
			t.Setenv(vscodeBridgeEnvVar, ts.URL)

			transport := newVSCodeMCPTransport()
			sweeper := &credentialSweeper{}

			result, err := transport.Invoke(context.Background(), toolpkg.ToolDef{Name: "ops-synthetic"}, "status-query", nil)
			sweepToolResult(sweeper, result)
			if err != nil {
				collectErrChain(sweeper, "invokeErr", err)
			}
			if scanErr := sweeper.scan(sentinelToken); scanErr != nil {
				t.Fatalf("token leaked in %q:\n%v", tc.name, scanErr)
			}
		})
	}
}
