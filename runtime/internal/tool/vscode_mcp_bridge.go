package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"

	"github.com/google/uuid"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

const (
	vscodeBridgeVersion     = "yawr.vscode-mcp-bridge/v1"
	vscodeBridgeDefaultURL  = "http://127.0.0.1:7779"
	vscodeBridgeEnvVar      = "YAWR_VSCODE_BRIDGE_URL"
	vscodeBridgeTokenEnvVar = "YAWR_VSCODE_BRIDGE_TOKEN"
)

type bridgeRequest struct {
	Version         string         `json:"version"`
	RequestID       string         `json:"request_id"`
	Tool            string         `json:"tool"`
	Action          string         `json:"action"`
	Args            map[string]any `json:"args"`
	DeadlineUnixMS  int64          `json:"deadline_unix_ms,omitempty"`
	CapabilityProof string         `json:"capability_proof"`
}

type bridgeErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type bridgeResponse struct {
	Version   string             `json:"version"`
	RequestID string             `json:"request_id"`
	Result    map[string]any     `json:"result,omitempty"`
	Error     *bridgeErrorDetail `json:"error,omitempty"`
}

type pendingRequest struct {
	result *toolpkg.ToolResult
	err    error
	done   chan struct{}
}

// VSCodeMCPTransport implements toolpkg.ToolTransport over the VS Code
// loopback bridge. Instances are pooled per tool name in
// DefaultToolRuntime.persistent, so the capability secret is session-scoped.
//
// Security model:
//   - Loopback only: bridge URL MUST target http://127.0.0.1.
//   - Capability secret: provisioned by the VS Code extension per session and
//     passed to Yawr via YAWR_VSCODE_BRIDGE_TOKEN. Yawr reads and presents
//     it; it never mints this value.
//   - No bearer token ever reaches Yawr; authorization stays inside VS Code.
type VSCodeMCPTransport struct {
	mu         sync.Mutex
	bridgeURL  string
	capSecret  string // NEVER log or return this value.
	initErr    error  // construction error surfaced on first Invoke
	httpClient *http.Client
	pending    map[string]*pendingRequest
}

func newVSCodeMCPTransport() *VSCodeMCPTransport {
	bridgeURL := os.Getenv(vscodeBridgeEnvVar)
	if bridgeURL == "" {
		bridgeURL = vscodeBridgeDefaultURL
	}

	if err := validateLoopback(bridgeURL); err != nil {
		return &VSCodeMCPTransport{initErr: err}
	}

	token := os.Getenv(vscodeBridgeTokenEnvVar)
	if token == "" {
		return &VSCodeMCPTransport{
			initErr: fmt.Errorf("vscode-mcp: no bridge capability token available; " +
				"this transport can only run under the Yawr VS Code extension " +
				"(YAWR_VSCODE_BRIDGE_TOKEN not set)"),
		}
	}

	return &VSCodeMCPTransport{
		bridgeURL:  bridgeURL,
		capSecret:  token, // NEVER log or return this value.
		httpClient: &http.Client{},
		pending:    make(map[string]*pendingRequest),
	}
}

func (t *VSCodeMCPTransport) Invoke(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	if t.initErr != nil {
		return nil, t.initErr
	}
	return t.invokeWithRequestID(ctx, def, action, args, newRequestID())
}

func (t *VSCodeMCPTransport) invokeWithRequestID(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any, requestID string) (*toolpkg.ToolResult, error) {
	if err := validateLoopback(t.bridgeURL); err != nil {
		return nil, err
	}
	if requestID == "" {
		requestID = newRequestID()
	}

	pending, waitForExisting := t.registerPending(requestID)
	if waitForExisting {
		select {
		case <-pending.done:
			return cloneToolResult(pending.result), pending.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	result, err := t.invokeBridge(ctx, def, action, args, requestID)
	t.finishPending(requestID, result, err)
	return cloneToolResult(result), err
}

func (t *VSCodeMCPTransport) invokeBridge(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any, requestID string) (*toolpkg.ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}

	var deadlineMS int64
	if deadline, ok := ctx.Deadline(); ok {
		deadlineMS = deadline.UnixMilli()
	}

	payload := bridgeRequest{
		Version:         vscodeBridgeVersion,
		RequestID:       requestID,
		Tool:            def.Name,
		Action:          action,
		Args:            args,
		DeadlineUnixMS:  deadlineMS,
		CapabilityProof: t.capSecret,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("vscode-mcp: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.bridgeURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vscode-mcp: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vscode-mcp: transport error: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, httpResp.Body)
		httpResp.Body.Close()
	}()

	switch httpResp.StatusCode {
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("vscode-mcp: VS Code extension is not connected or has been closed")
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("vscode-mcp: capability secret was rejected by bridge — session may have expired")
	}

	rawBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("vscode-mcp: read response body: %w", err)
	}

	var resp bridgeResponse
	if err := json.Unmarshal(rawBody, &resp); err != nil {
		return nil, fmt.Errorf("vscode-mcp: malformed bridge response: %w", err)
	}
	if resp.Version == "" || resp.RequestID == "" {
		return nil, fmt.Errorf("vscode-mcp: malformed bridge response: missing required fields")
	}
	if resp.Version != vscodeBridgeVersion {
		return nil, fmt.Errorf("vscode-mcp: bridge version mismatch: client expects %q, bridge returned %q", vscodeBridgeVersion, resp.Version)
	}
	if resp.RequestID != requestID {
		return nil, fmt.Errorf("vscode-mcp: malformed bridge response: request_id mismatch: sent %q, got %q", requestID, resp.RequestID)
	}
	if resp.Error == nil && resp.Result == nil {
		return nil, fmt.Errorf("vscode-mcp: malformed bridge response: missing result and error")
	}
	if resp.Error != nil {
		if resp.Error.Code == "" || resp.Error.Message == "" {
			return nil, fmt.Errorf("vscode-mcp: malformed bridge response: error.code and error.message are required")
		}
		switch resp.Error.Code {
		case "bridge_disconnected":
			return nil, fmt.Errorf("vscode-mcp: VS Code extension is not connected or has been closed")
		case "capability_rejected":
			return nil, fmt.Errorf("vscode-mcp: capability secret was rejected by bridge — session may have expired")
		default:
			return nil, fmt.Errorf("vscode-mcp: bridge error [%s]: %s", resp.Error.Code, resp.Error.Message)
		}
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vscode-mcp: bridge HTTP status %d", httpResp.StatusCode)
	}

	return &toolpkg.ToolResult{ExitCode: 0, Output: cloneMap(resp.Result)}, nil
}

func (t *VSCodeMCPTransport) Close() error {
	t.mu.Lock()
	t.pending = make(map[string]*pendingRequest)
	t.mu.Unlock()
	return nil
}

func (t *VSCodeMCPTransport) registerPending(requestID string) (*pendingRequest, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if pending, ok := t.pending[requestID]; ok {
		return pending, true
	}
	pending := &pendingRequest{done: make(chan struct{})}
	t.pending[requestID] = pending
	return pending, false
}

func (t *VSCodeMCPTransport) finishPending(requestID string, result *toolpkg.ToolResult, err error) {
	t.mu.Lock()
	pending := t.pending[requestID]
	pending.result = cloneToolResult(result)
	pending.err = err
	close(pending.done)
	t.mu.Unlock()
}

func validateLoopback(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("vscode-mcp: malformed bridge URL %q: %w", sanitizeURL(rawURL), err)
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" {
		return fmt.Errorf("vscode-mcp: bridge URL must target 127.0.0.1 over http (loopback only); got %q", sanitizeURL(rawURL))
	}
	return nil
}

func sanitizeURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if parsed.Scheme == "" && parsed.Host == "" {
		return rawURL
	}
	return parsed.Scheme + "://" + parsed.Host
}

func newRequestID() string {
	return uuid.NewString()
}

func cloneToolResult(result *toolpkg.ToolResult) *toolpkg.ToolResult {
	if result == nil {
		return nil
	}
	cloned := *result
	cloned.Output = cloneMap(result.Output)
	return &cloned
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
