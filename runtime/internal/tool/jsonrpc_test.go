package tool

import (
	"context"
	"testing"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestJSONRPCTransport_Echo(t *testing.T) {
	transport := &JSONRPCTransport{}
	t.Cleanup(func() { _ = transport.Close() })
	def := toolpkg.ToolDef{
		Name:      "jsonrpc",
		Transport: toolpkg.TransportJSONRPC,
		Command:   toolPath("jsonrpc-server"),
	}

	res, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"message": "hi"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Output["message"] != "hi" {
		t.Fatalf("expected message hi, got %v", res.Output["message"])
	}
}

func TestJSONRPCTransport_MultiCall(t *testing.T) {
	transport := &JSONRPCTransport{}
	t.Cleanup(func() { _ = transport.Close() })
	def := toolpkg.ToolDef{
		Name:      "jsonrpc",
		Transport: toolpkg.TransportJSONRPC,
		Command:   toolPath("jsonrpc-server"),
	}

	if _, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"message": "one"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	first := transport.proc
	if _, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"message": "two"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if transport.proc != first {
		t.Fatalf("expected persistent process reuse")
	}
}

func TestJSONRPCTransport_Fail(t *testing.T) {
	transport := &JSONRPCTransport{}
	t.Cleanup(func() { _ = transport.Close() })
	def := toolpkg.ToolDef{
		Name:      "jsonrpc",
		Transport: toolpkg.TransportJSONRPC,
		Command:   toolPath("jsonrpc-server"),
	}

	if _, err := transport.Invoke(context.Background(), def, "fail", map[string]any{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestJSONRPCTransport_Shutdown(t *testing.T) {
	transport := &JSONRPCTransport{}
	def := toolpkg.ToolDef{
		Name:      "jsonrpc",
		Transport: toolpkg.TransportJSONRPC,
		Command:   toolPath("jsonrpc-server"),
	}

	if _, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"message": "hi"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if transport.proc != nil {
		t.Fatalf("expected process closed")
	}
}
