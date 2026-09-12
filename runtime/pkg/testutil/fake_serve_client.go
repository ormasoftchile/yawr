package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// ServeClient is a helper for yawr serve integration tests.
type ServeClient struct {
	BaseURL string
	Client  *http.Client
}

// ServeRPCError mirrors the JSON-RPC error shape.
type ServeRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type serveRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *ServeRPCError  `json:"error"`
}

// NewServeClient constructs a ServeClient for baseURL.
func NewServeClient(baseURL string) *ServeClient {
	return &ServeClient{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Client:  http.DefaultClient,
	}
}

// Call issues a JSON-RPC request to /rpc.
func (c *ServeClient) Call(ctx context.Context, id any, method string, params any) (json.RawMessage, *ServeRPCError, error) {
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/rpc", bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	var parsed serveRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, nil, err
	}
	if parsed.Error != nil {
		return nil, parsed.Error, nil
	}
	return parsed.Result, nil, nil
}

// DialWS connects to the /ws endpoint with an optional runID filter.
func (c *ServeClient) DialWS(ctx context.Context, runID string) (*websocket.Conn, error) {
	url := c.BaseURL + "/ws"
	if runID != "" {
		url = fmt.Sprintf("%s?runID=%s", url, runID)
	}
	wsURL := "ws" + strings.TrimPrefix(url, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// OpenSSE opens an SSE connection to /events with optional runID and lastEventID.
func (c *ServeClient) OpenSSE(ctx context.Context, runID string, lastEventID string) (*http.Response, error) {
	url := c.BaseURL + "/events"
	if runID != "" {
		url = fmt.Sprintf("%s?runID=%s", url, runID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	return c.Client.Do(req)
}
