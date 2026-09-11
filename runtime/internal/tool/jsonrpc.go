package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// JSONRPCTransport maintains a persistent JSON-RPC process.
type JSONRPCTransport struct {
	mu     sync.Mutex
	proc   *ProcessHandle
	reader *bufio.Reader
	writer *bufio.Writer
	nextID int
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// Invoke sends a tools/call JSON-RPC request and waits for the response.
func (t *JSONRPCTransport) Invoke(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.proc == nil {
		proc, err := StartProcess(ctx, def.Command, def.Args, def.Env)
		if err != nil {
			return nil, err
		}
		t.proc = proc
		t.reader = bufio.NewReader(proc.stdout)
		t.writer = bufio.NewWriter(proc.stdin)
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
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := t.writer.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	if err := t.writer.Flush(); err != nil {
		return nil, err
	}

	respLine, err := readLine(ctx, t.reader, t.proc)
	if err != nil {
		return nil, err
	}

	var resp jsonrpcResponse
	if err := json.Unmarshal(respLine, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("jsonrpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	result := &toolpkg.ToolResult{ExitCode: 0}
	if len(resp.Result) > 0 {
		var parsed map[string]any
		if json.Unmarshal(resp.Result, &parsed) == nil {
			result.Output = parsed
		} else {
			result.Stdout = string(resp.Result)
		}
	}
	return result, nil
}

// Close shuts down the persistent JSON-RPC process.
func (t *JSONRPCTransport) Close() error {
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
	if payload, err := json.Marshal(req); err == nil {
		_, _ = t.writer.Write(append(payload, '\n'))
		_ = t.writer.Flush()
	}
	_ = t.proc.Kill()
	t.proc = nil
	return nil
}

func readLine(ctx context.Context, reader *bufio.Reader, proc *ProcessHandle) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := reader.ReadBytes('\n')
		ch <- result{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		if proc != nil {
			_ = proc.Kill()
		}
		return nil, ctx.Err()
	case res := <-ch:
		return res.line, res.err
	}
}
