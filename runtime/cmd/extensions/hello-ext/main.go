package main

import (
	"bufio"
	"encoding/json"
	"os"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolInvokeParams struct {
	Tool   string         `json:"tool"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		resp := handleRequest(req)
		_ = encoder.Encode(resp)
		if req.Method == "extension/shutdown" {
			return
		}
	}
}

func handleRequest(req rpcRequest) rpcResponse {
	switch req.Method {
	case "extension/initialize":
		return initResponse(req)
	case "contributions/list":
		return contributionsResponse(req)
	case "extension/ping":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: "pong"}
	case "extension/shutdown":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: "ok"}
	case "tools/invoke":
		return invokeResponse(req)
	default:
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found"}}
	}
}

func initResponse(req rpcRequest) rpcResponse {
	return rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"name":         "hello-ext",
			"version":      "0.1.0",
			"capabilities": []string{extension.CapabilityToolRegistration},
		},
	}
}

func contributionsResponse(req rpcRequest) rpcResponse {
	toolDef := schema.ToolDef{
		Name:        "hello",
		Description: "Says hello",
		Transport: schema.TransportConfig{
			Type:    schema.TransportStdio,
			Command: "hello-ext",
		},
		Actions: map[string]*schema.ToolAction{
			"hello": {
				Description: "Says hello",
				Args: map[string]*schema.ArgDef{
					"name": {Type: "string", Required: false},
				},
				Returns: "string",
			},
		},
	}
	return rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"tools": []schema.ToolDef{toolDef},
		},
	}
}

func invokeResponse(req rpcRequest) rpcResponse {
	var params toolInvokeParams
	_ = json.Unmarshal(req.Params, &params)
	return rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"exitCode": 0,
			"output":   params.Args,
		},
	}
}
