package tool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Fixed transport policy, not tool-output projection limits. See
// docs/mcp-http-response-budgets.md for wire-byte accounting and rationale.
const (
	mcpHTTPMaxEventBytes    = 16 << 20
	mcpHTTPMaxResponseBytes = 64 << 20
	mcpHTTPMaxEvents        = 4096
)

func mcpHTTPBudgetError(budget string, limit int) error {
	return fmt.Errorf("MCP-008: mcp-http: response budget exceeded: %s limit %d; response rejected (not truncated)", budget, limit)
}

// parseBoundedMCPSSE retains only the current event, not the event history.
// ReadSlice handles arbitrary network fragmentation without Scanner's 64 KiB
// token ceiling. Fragments are checked before accumulating either a line or
// an event; many short data lines cannot bypass the event budget.
func parseBoundedMCPSSE(r io.Reader, expectedID int) (*mcpResponse, error) {
	reader := bufio.NewReaderSize(io.LimitReader(r, mcpHTTPMaxResponseBytes+1), 32<<10)
	var data bytes.Buffer
	var line []byte
	var responseBytes, eventBytes, events int
	hasData := false

	dispatch := func() (*mcpResponse, error) {
		if eventBytes == 0 {
			return nil, nil
		}
		events++
		if events > mcpHTTPMaxEvents {
			return nil, mcpHTTPBudgetError("SSE events", mcpHTTPMaxEvents)
		}
		eventBytes = 0
		var resp mcpResponse
		err := json.Unmarshal(data.Bytes(), &resp)
		data.Reset()
		hasData = false
		// Preserve existing behavior: malformed events, notifications, and
		// unrelated IDs are skipped, but still consume all relevant budgets.
		if err != nil || resp.ID == nil || *resp.ID != expectedID {
			return nil, nil
		}
		return &resp, nil
	}

	for {
		fragment, err := reader.ReadSlice('\n')
		responseBytes += len(fragment)
		if responseBytes > mcpHTTPMaxResponseBytes {
			return nil, mcpHTTPBudgetError("response bytes", mcpHTTPMaxResponseBytes)
		}
		// Blank separator lines count toward response bytes, not event bytes.
		blank := len(line) == 0 && (bytes.Equal(fragment, []byte("\n")) || bytes.Equal(fragment, []byte("\r\n")))
		if !blank {
			if len(fragment) > mcpHTTPMaxEventBytes-eventBytes {
				return nil, mcpHTTPBudgetError("SSE event bytes", mcpHTTPMaxEventBytes)
			}
			eventBytes += len(fragment)
		}
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return nil, fmt.Errorf("MCP-008: mcp-http: transport error reading SSE stream: %w", err)
		}
		line = append(line, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		text := bytes.TrimSuffix(line, []byte("\n"))
		text = bytes.TrimSuffix(text, []byte("\r"))
		if len(text) == 0 {
			if resp, dispatchErr := dispatch(); resp != nil || dispatchErr != nil {
				return resp, dispatchErr
			}
		} else if bytes.HasPrefix(text, []byte("data:")) || bytes.Equal(text, []byte("data")) {
			value := bytes.TrimPrefix(text, []byte("data"))
			value = bytes.TrimPrefix(value, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			hasData = true
		}
		// Comments and event/id/retry/unknown fields are intentionally ignored.
		line = line[:0]
		if err == io.EOF {
			// Preserve accepting a final event without a trailing blank line.
			if resp, dispatchErr := dispatch(); resp != nil || dispatchErr != nil {
				return resp, dispatchErr
			}
			return nil, fmt.Errorf("MCP-006: mcp-http: SSE stream closed without response for request id %d", expectedID)
		}
	}
}
