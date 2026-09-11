package tool

import "encoding/json"

// mcpContent is a single content item in an MCP tool result.
type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mcpCallResult is the result payload of a tools/call response.
type mcpCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// mcpResponse is the top-level JSON-RPC response envelope for MCP calls.
// Result is kept as json.RawMessage so callers can decode it into the
// appropriate shape (mcpCallResult for tools/call, tools-list struct for
// tools/list). ID is a pointer to distinguish responses (numeric id) from
// notifications (absent id field).
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// callResult decodes the Result field as an mcpCallResult (tools/call shape).
func (r *mcpResponse) callResult() (*mcpCallResult, error) {
	if len(r.Result) == 0 {
		return nil, nil
	}
	var cr mcpCallResult
	if err := json.Unmarshal(r.Result, &cr); err != nil {
		return nil, err
	}
	return &cr, nil
}
