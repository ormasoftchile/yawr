package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"time"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return
			}
			return
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		switch req.Method {
		case "shutdown":
			writeResponse(writer, req.ID, map[string]any{"ok": true}, nil)
			return
		case "tools/call":
			handleCall(writer, req)
		default:
			writeResponse(writer, req.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
		}
	}
}

func handleCall(writer *bufio.Writer, req rpcRequest) {
	name, _ := req.Params["name"].(string)
	arguments, _ := req.Params["arguments"].(map[string]any)

	switch name {
	case "echo":
		msg, _ := arguments["message"].(string)
		writeResponse(writer, req.ID, map[string]any{"message": msg}, nil)
	case "slow":
		seconds := toInt(arguments["seconds"], 0)
		if seconds == 0 {
			seconds = toInt(arguments["delay_seconds"], 0)
		}
		if seconds > 0 {
			time.Sleep(time.Duration(seconds) * time.Second)
		}
		writeResponse(writer, req.ID, map[string]any{"status": "ok"}, nil)
	case "fail":
		writeResponse(writer, req.ID, nil, &rpcError{Code: -32001, Message: "forced failure"})
	default:
		writeResponse(writer, req.ID, nil, &rpcError{Code: -32602, Message: "unknown tool action"})
	}
}

func writeResponse(w *bufio.Writer, id json.RawMessage, result any, errResp *rpcError) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
	}
	if errResp != nil {
		resp["error"] = errResp
	} else {
		resp["result"] = result
	}
	payload, _ := json.Marshal(resp)
	_, _ = w.Write(append(payload, '\n'))
	_ = w.Flush()
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
