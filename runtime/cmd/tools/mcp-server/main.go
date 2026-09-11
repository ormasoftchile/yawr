package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params,omitempty"`
}

func main() {
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for {
		body, err := readMCPMessage(reader)
		if err != nil {
			if err == io.EOF {
				return
			}
			return
		}
		var req mcpRequest
		if json.Unmarshal(body, &req) != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "yawr-mcp", "version": "1.0.0"},
				},
			}
			writeMCPMessage(writer, resp)
		case "initialized", "notifications/initialized":
			continue
		case "tools/list":
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "echo", "description": "Echo", "inputSchema": map[string]any{}},
						{"name": "fail", "description": "Fail", "inputSchema": map[string]any{}},
						{"name": "slow", "description": "Slow", "inputSchema": map[string]any{}},
					},
				},
			}
			writeMCPMessage(writer, resp)
		case "tools/call":
			handleCall(writer, req)
		case "shutdown":
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]any{"ok": true},
			}
			writeMCPMessage(writer, resp)
			return
		default:
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			}
			writeMCPMessage(writer, resp)
		}
	}
}

func handleCall(writer *bufio.Writer, req mcpRequest) {
	name, _ := req.Params["name"].(string)
	arguments, _ := req.Params["arguments"].(map[string]any)

	switch name {
	case "echo":
		msg, _ := arguments["message"].(string)
		text := fmt.Sprintf("{\"message\":%q}", msg)
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			},
		}
		writeMCPMessage(writer, resp)
	case "slow":
		seconds := toInt(arguments["seconds"], 0)
		if seconds == 0 {
			seconds = toInt(arguments["delay_seconds"], 0)
		}
		if seconds > 0 {
			time.Sleep(time.Duration(seconds) * time.Second)
		}
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "{\"status\":\"ok\"}"}},
			},
		}
		writeMCPMessage(writer, resp)
	case "fail":
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"result": map[string]any{
				"isError": true,
				"content": []map[string]any{{"type": "text", "text": "forced failure"}},
			},
		}
		writeMCPMessage(writer, resp)
	default:
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"error":   map[string]any{"code": -32602, "message": "unknown tool"},
		}
		writeMCPMessage(writer, resp)
	}
}

func writeMCPMessage(w *bufio.Writer, payload any) {
	body, _ := json.Marshal(payload)
	_, _ = w.WriteString(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)))
	_, _ = w.Write(body)
	_ = w.Flush()
}

func readMCPMessage(r *bufio.Reader) ([]byte, error) {
	length, err := readContentLength(r)
	if err != nil {
		return nil, err
	}
	body := make([]byte, length)
	_, err = io.ReadFull(r, body)
	return body, err
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

func toInt(v any, fallback int) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case int64:
		return int(val)
	default:
		return fallback
	}
}
