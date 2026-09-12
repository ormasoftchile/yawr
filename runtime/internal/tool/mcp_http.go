package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

const (
	mcpProtocolVersion = "2025-03-26"
	mcpDefaultTimeout  = 120 * time.Second
)

// MCPHTTPTransport implements pkg/tool.ToolTransport over Streamable HTTP MCP
// (MCP spec 2025-03-26). A single instance is pooled per tool name inside
// DefaultToolRuntime.persistent and is therefore session-scoped — the MCP
// session ID survives across multiple tool calls within one run.
type MCPHTTPTransport struct {
	mu          sync.Mutex
	url         string
	gate        *TokenGate // nil → no Authorization header (unauthenticated)
	sessionID   string     // from Mcp-Session-Id response header
	initialized bool
	httpClient  *http.Client
	nextID      int
}

// NewMCPHTTPTransport constructs an MCPHTTPTransport.
// gate may be nil when the server uses no bearer-token authentication (§4.5).
// When non-nil, gate.AttachToken is the single chokepoint for all token
// attachment and host-allow-list enforcement (B-32).
//
// Authenticated requests (gate != nil) never follow HTTP redirects (MCP-013 /
// B-32). CheckRedirect returns http.ErrUseLastResponse so the redirect
// response is surfaced to send(), which converts it to a clear operator-
// actionable MCP-013 error naming the redirect destination. Unauthenticated
// transports follow redirects normally.
func NewMCPHTTPTransport(url string, gate *TokenGate) *MCPHTTPTransport {
	client := &http.Client{}
	if gate != nil {
		// Return http.ErrUseLastResponse so Do() returns the redirect response
		// with err=nil. send() then detects the 3xx status and emits MCP-013
		// with the redirect destination, giving the operator an actionable message.
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return &MCPHTTPTransport{
		url:        url,
		gate:       gate,
		httpClient: client,
	}
}

// Invoke sends a tools/call request and returns the result.
// Output shape is identical to MCPTransport (stdio): text content → Stdout;
// JSON text → also Output map. Tool-level errors (isError) → non-zero ExitCode.
func (t *MCPHTTPTransport) Invoke(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.ensureInitialized(ctx); err != nil {
		return nil, err
	}

	actionDef := def.Actions[action]
	remoteAction := action
	if actionDef != nil && actionDef.MCPTool != "" {
		remoteAction = actionDef.MCPTool
	}
	adaptedArgs, adaptErr := applyMCPInputAdaptation(actionDef, args)
	if adaptErr != nil {
		return nil, adaptErr
	}

	id := t.allocID()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      remoteAction,
			"arguments": adaptedArgs,
		},
	}

	resp, err := t.doRequest(ctx, req, id)
	if err != nil {
		return nil, err
	}
	return t.toolResult(resp)
}

// ListTools retrieves the tool list from the server.
func (t *MCPHTTPTransport) ListTools(ctx context.Context, def toolpkg.ToolDef) ([]map[string]any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.ensureInitialized(ctx); err != nil {
		return nil, err
	}

	id := t.allocID()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/list",
		"params":  map[string]any{},
	}

	resp, err := t.doRequest(ctx, req, id)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP-009: mcp-http: server error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if len(resp.Result) == 0 {
		return nil, fmt.Errorf("mcp-http: tools/list: missing result")
	}

	var listResult struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &listResult); err != nil {
		return nil, fmt.Errorf("mcp-http: tools/list: malformed result: %w", err)
	}
	return listResult.Tools, nil
}

// Close sends a best-effort shutdown notification, then releases the HTTP client.
func (t *MCPHTTPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.initialized {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shutdownNotif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "shutdown",
		"params":  map[string]any{},
	}
	// Best-effort: ignore errors (process may already be gone).
	body, _ := json.Marshal(shutdownNotif)
	req, err := t.buildHTTPRequest(ctx, body)
	if err == nil {
		resp, err2 := t.httpClient.Do(req)
		if err2 == nil {
			resp.Body.Close()
		}
	}

	t.initialized = false
	t.sessionID = ""
	return nil
}

// ─── internal helpers ───────────────────────────────────────────────────────

func (t *MCPHTTPTransport) allocID() int {
	t.nextID++
	return t.nextID
}

// ensureInitialized performs the MCP initialize handshake if not yet done.
// Caller must hold t.mu.
func (t *MCPHTTPTransport) ensureInitialized(ctx context.Context) error {
	if t.initialized {
		return nil
	}
	return t.initialize(ctx)
}

func (t *MCPHTTPTransport) initialize(ctx context.Context) error {
	id := t.allocID()
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "yawr",
				"version": "2.0.0",
			},
		},
	}

	// Unlike stdio MCPTransport (which marks initialized before inspecting the
	// response, we check
	// the response for a protocol error before marking the session live.
	initResp, err := t.doRequest(ctx, initReq, id)
	if err != nil {
		return fmt.Errorf("mcp-http: initialize failed: %w", err)
	}
	if initResp.Error != nil {
		return fmt.Errorf("mcp-http: initialize failed: server error %d: %s", initResp.Error.Code, initResp.Error.Message)
	}

	// Send notifications/initialized — no id, 202/204 expected, no response body.
	initializedNotif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	}
	body, err := json.Marshal(initializedNotif)
	if err != nil {
		return fmt.Errorf("mcp-http: notifications/initialized marshal: %w", err)
	}
	req, err := t.buildHTTPRequest(ctx, body)
	if err != nil {
		return err
	}
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("MCP-008: mcp-http: transport error: %w", err)
	}
	resp.Body.Close()
	// 202 and 204 are both acceptable.

	t.initialized = true
	return nil
}

// doRequest sends one JSON-RPC request and returns the matching response.
// It handles the 404-session-expired → re-initialize → retry logic (§2.4).
func (t *MCPHTTPTransport) doRequest(ctx context.Context, req map[string]any, id int) (*mcpResponse, error) {
	resp, err := t.send(ctx, req, id)
	if err != nil {
		// 404 may mean session expired.
		if isHTTP404(err) && t.initialized {
			t.sessionID = ""
			t.initialized = false
			if reinitErr := t.initialize(ctx); reinitErr != nil {
				return nil, fmt.Errorf("MCP-004: mcp-http: session expired; re-initialization failed: %v", reinitErr)
			}
			resp, err = t.send(ctx, req, id)
			if err != nil {
				return nil, fmt.Errorf("MCP-004: mcp-http: session expired; re-initialization failed: %v", err)
			}
		} else {
			return nil, err
		}
	}
	return resp, nil
}

// send posts a JSON-RPC message and parses the HTTP response (JSON or SSE).
// expectedID is the JSON-RPC id from the request: it is used to correlate
// the response in SSE streams, skipping any response whose id does not match
// and any notification (no id). For a plain JSON response, the id is checked
// after parsing.
func (t *MCPHTTPTransport) send(ctx context.Context, payload any, expectedID int) (*mcpResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("mcp-http: marshal request: %w", err)
	}

	// Apply default timeout if context has no deadline.
	reqCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, mcpDefaultTimeout)
		defer cancel()
	}

	httpReq, err := t.buildHTTPRequest(reqCtx, body)
	if err != nil {
		return nil, err
	}

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("MCP-008: mcp-http: transport error: %w", err)
	}

	// 401: token may have expired mid-run. Invalidate cached token and retry
	// once (the gate re-acquires on the next Token() call).
	if httpResp.StatusCode == http.StatusUnauthorized && t.gate != nil {
		httpResp.Body.Close()
		t.gate.Invalidate()
		httpReq, err = t.buildHTTPRequest(reqCtx, body)
		if err != nil {
			return nil, err
		}
		httpResp, err = t.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("MCP-008: mcp-http: transport error on auth retry: %w", err)
		}
	}

	// Do not drain an SSE stream after a matching response or a budget error:
	// the server may keep streaming indefinitely. Closing also cancels reads.
	defer httpResp.Body.Close()

	// 3xx: redirect surfaced but not followed on authenticated transports.
	// CheckRedirect returns http.ErrUseLastResponse so Do() delivers the
	// redirect response here with err=nil. Convert to a clear MCP-013 error
	// naming the redirect destination so the operator knows to update url:.
	if t.gate != nil && httpResp.StatusCode >= 300 && httpResp.StatusCode < 400 {
		location := httpResp.Header.Get("Location")
		if location == "" {
			location = "(no Location header)"
		}
		return nil, errkit.New("MCP-013",
			fmt.Sprintf("mcp-http: authenticated request was redirected to %q — redirects are blocked to prevent token forwarding to an unreviewed host; update url: to %q", location, location))
	}

	// B-23: version mismatch.
	if httpResp.StatusCode >= 400 && httpResp.StatusCode < 500 &&
		httpResp.StatusCode != http.StatusNotFound {
		// Could be a version rejection. Emit MCP-003.
		return nil, fmt.Errorf("MCP-003: mcp-http: server rejected protocol version (HTTP %d)", httpResp.StatusCode)
	}

	if httpResp.StatusCode == http.StatusNotFound {
		return nil, &http404Error{}
	}

	// Capture session ID if server provides one.
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.sessionID = sid
	}

	ct := httpResp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		return t.parseJSONResponse(httpResp.Body, expectedID)
	case strings.HasPrefix(ct, "text/event-stream"):
		return t.parseSSEResponse(httpResp.Body, expectedID)
	case httpResp.StatusCode == http.StatusNoContent || httpResp.StatusCode == http.StatusAccepted:
		// Notification responses — no body.
		return &mcpResponse{}, nil
	default:
		return nil, fmt.Errorf("MCP-005: mcp-http: unexpected content-type %q (expected application/json or text/event-stream)", ct)
	}
}

// buildHTTPRequest assembles the POST request with all required headers.
// Token attachment goes through gate.AttachToken — the single chokepoint
// for the host allow-list check, token acquisition, and mcp/authAttached
// audit emission. The token value is never logged (B-24).
func (t *MCPHTTPTransport) buildHTTPRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("mcp-http: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	if err := t.gate.AttachToken(ctx, req); err != nil {
		return nil, err // MCP-007 from auth provider; token not in error msg
	}
	return req, nil
}

// parseJSONResponse reads a plain JSON body as a single mcpResponse.
// It verifies the response ID matches expectedID.
func (t *MCPHTTPTransport) parseJSONResponse(r io.Reader, expectedID int) (*mcpResponse, error) {
	data, err := io.ReadAll(io.LimitReader(r, mcpHTTPMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("MCP-008: mcp-http: transport error reading JSON body: %w", err)
	}
	if len(data) > mcpHTTPMaxResponseBytes {
		return nil, mcpHTTPBudgetError("response bytes", mcpHTTPMaxResponseBytes)
	}
	var resp mcpResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("mcp-http: malformed JSON response: %w", err)
	}
	// ID correlation: responses must have an id that matches what we sent.
	// A mismatch indicates the server is confused or we have a routing bug.
	if resp.ID == nil || *resp.ID != expectedID {
		gotID := -1
		if resp.ID != nil {
			gotID = *resp.ID
		}
		return nil, fmt.Errorf("mcp-http: response id mismatch: want %d got %d", expectedID, gotID)
	}
	return &resp, nil
}

// parseSSEResponse reads an SSE stream and returns the response whose id
// matches expectedID. Notifications (absent id) and responses for other ids
// are skipped — the server may interleave progress notifications before the
// terminal response, and we must not mis-associate them.
// If the stream closes before a matching response is found, returns MCP-006.
func (t *MCPHTTPTransport) parseSSEResponse(r io.Reader, expectedID int) (*mcpResponse, error) {
	return parseBoundedMCPSSE(r, expectedID)
}

// toolResult converts an mcpResponse into a ToolResult, matching stdio MCP
// behaviour exactly (Requirement 4 / §5).
func (t *MCPHTTPTransport) toolResult(resp *mcpResponse) (*toolpkg.ToolResult, error) {
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP-009: mcp-http: server error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	cr, err := resp.callResult()
	if err != nil || cr == nil || len(cr.Content) == 0 {
		return nil, fmt.Errorf("mcp-http: response missing content")
	}
	if cr.IsError {
		return &toolpkg.ToolResult{
			ExitCode: 1,
			Stderr:   cr.Content[0].Text,
		}, fmt.Errorf("mcp tool error: %s", cr.Content[0].Text)
	}

	text := cr.Content[0].Text
	result := &toolpkg.ToolResult{ExitCode: 0, Stdout: text}
	var parsed map[string]any
	if json.Unmarshal([]byte(text), &parsed) == nil {
		result.Output = parsed
	}
	return result, nil
}

// ─── HTTP 404 sentinel ───────────────────────────────────────────────────────

type http404Error struct{}

func (e *http404Error) Error() string { return "http 404 not found" }

func isHTTP404(err error) bool {
	_, ok := err.(*http404Error)
	return ok
}
