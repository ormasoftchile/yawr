package tool

import (
	"context"
	"testing"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestMCPTransport_Handshake(t *testing.T) {
	transport := &MCPTransport{}
	t.Cleanup(func() { _ = transport.Close() })
	def := toolpkg.ToolDef{
		Name:      "mcp",
		Transport: toolpkg.TransportMCP,
		Command:   toolPath("mcp-server"),
	}

	if _, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"message": "hi"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !transport.initialized {
		t.Fatalf("expected transport initialized")
	}
}

func TestMCPTransport_Echo(t *testing.T) {
	transport := &MCPTransport{}
	t.Cleanup(func() { _ = transport.Close() })
	def := toolpkg.ToolDef{
		Name:      "mcp",
		Transport: toolpkg.TransportMCP,
		Command:   toolPath("mcp-server"),
	}

	res, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"message": "pong"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Output["message"] != "pong" {
		t.Fatalf("expected message pong, got %v", res.Output["message"])
	}
}

func TestMCPTransport_Fail(t *testing.T) {
	transport := &MCPTransport{}
	t.Cleanup(func() { _ = transport.Close() })
	def := toolpkg.ToolDef{
		Name:      "mcp",
		Transport: toolpkg.TransportMCP,
		Command:   toolPath("mcp-server"),
	}

	if _, err := transport.Invoke(context.Background(), def, "fail", map[string]any{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestMCPTransport_ToolsList(t *testing.T) {
	transport := &MCPTransport{}
	t.Cleanup(func() { _ = transport.Close() })
	def := toolpkg.ToolDef{
		Name:      "mcp",
		Transport: toolpkg.TransportMCP,
		Command:   toolPath("mcp-server"),
	}

	tools, err := transport.ListTools(context.Background(), def)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}
}
