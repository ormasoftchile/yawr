package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// MCPTransport maintains a persistent MCP process over stdio.
type MCPTransport struct {
	mu          sync.Mutex
	proc        *ProcessHandle
	reader      *bufio.Reader
	writer      *bufio.Writer
	nextID      int
	initialized bool
}

// Invoke sends tools/call to the MCP server.
func (t *MCPTransport) Invoke(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.ensureStarted(ctx, def); err != nil {
		return nil, err
	}

	id := t.nextID
	t.nextID++

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      action,
			"arguments": args,
		},
	}
	if err := writeMCPMessage(t.writer, req); err != nil {
		return nil, err
	}
	if err := t.writer.Flush(); err != nil {
		return nil, err
	}

	body, err := readMCPMessage(ctx, t.reader, t.proc)
	if err != nil {
		return nil, err
	}

	var resp mcpResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	cr, err := resp.callResult()
	if err != nil || cr == nil || len(cr.Content) == 0 {
		return nil, fmt.Errorf("mcp response missing content")
	}
	if cr.IsError {
		return nil, fmt.Errorf("mcp tool error: %s", cr.Content[0].Text)
	}

	text := cr.Content[0].Text
	result := &toolpkg.ToolResult{ExitCode: 0, Stdout: text}
	var parsed map[string]any
	if json.Unmarshal([]byte(text), &parsed) == nil {
		result.Output = parsed
	}
	return result, nil
}

// ListTools requests tools/list from the MCP server.
func (t *MCPTransport) ListTools(ctx context.Context, def toolpkg.ToolDef) ([]map[string]any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.ensureStarted(ctx, def); err != nil {
		return nil, err
	}

	id := t.nextID
	t.nextID++
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	if err := writeMCPMessage(t.writer, req); err != nil {
		return nil, err
	}
	if err := t.writer.Flush(); err != nil {
		return nil, err
	}

	body, err := readMCPMessage(ctx, t.reader, t.proc)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
		Error *jsonrpcError `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result.Tools, nil
}

// Close shuts down the MCP process.
func (t *MCPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proc == nil {
		return nil
	}
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      t.nextID,
		"method":  "shutdown",
		"params":  map[string]any{},
	}
	t.nextID++
	if err := writeMCPMessage(t.writer, req); err == nil {
		_ = t.writer.Flush()
	}
	_ = t.proc.Kill()
	t.proc = nil
	return nil
}

func (t *MCPTransport) ensureStarted(ctx context.Context, def toolpkg.ToolDef) error {
	if t.proc != nil {
		return nil
	}
	proc, err := StartProcess(ctx, def.Command, def.Args, def.Env)
	if err != nil {
		return err
	}
	t.proc = proc
	t.reader = bufio.NewReader(proc.stdout)
	t.writer = bufio.NewWriter(proc.stdin)

	initID := t.nextID
	t.nextID++
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      initID,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "yawr",
				"version": "2.0.0",
			},
		},
	}
	if err := writeMCPMessage(t.writer, initReq); err != nil {
		return err
	}
	if err := t.writer.Flush(); err != nil {
		return err
	}
	if _, err := readMCPMessage(ctx, t.reader, t.proc); err != nil {
		return err
	}

	initialized := map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	}
	if err := writeMCPMessage(t.writer, initialized); err != nil {
		return err
	}
	if err := t.writer.Flush(); err != nil {
		return err
	}
	t.initialized = true
	return nil
}

func writeMCPMessage(w *bufio.Writer, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.WriteString(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readMCPMessage(ctx context.Context, r *bufio.Reader, proc *ProcessHandle) ([]byte, error) {
	type result struct {
		body []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		length, err := readContentLength(r)
		if err != nil {
			ch <- result{err: err}
			return
		}
		body := make([]byte, length)
		_, err = io.ReadFull(r, body)
		ch <- result{body: body, err: err}
	}()

	select {
	case <-ctx.Done():
		if proc != nil {
			_ = proc.Kill()
		}
		return nil, ctx.Err()
	case res := <-ch:
		return res.body, res.err
	}
}

func readContentLength(r *bufio.Reader) (int, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				return 0, fmt.Errorf("invalid content-length header")
			}
			val, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return 0, err
			}
			length = val
		}
	}
	if length < 0 {
		return 0, fmt.Errorf("content-length header not found")
	}
	return length, nil
}
