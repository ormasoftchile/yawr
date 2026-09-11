package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestStdioTransport_Echo(t *testing.T) {
	transport := &StdioTransport{}
	def := toolpkg.ToolDef{
		Name:      "echo",
		Transport: toolpkg.TransportStdio,
		Command:   toolPath("echo"),
	}

	res, err := transport.Invoke(context.Background(), def, "echo", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("expected stdout hello, got %q", res.Stdout)
	}
}

func TestStdioTransport_Fail(t *testing.T) {
	transport := &StdioTransport{}
	def := toolpkg.ToolDef{
		Name:      "fail",
		Transport: toolpkg.TransportStdio,
		Command:   toolPath("fail"),
	}

	res, err := transport.Invoke(context.Background(), def, "fail", map[string]any{"exit_code": 7})
	if err == nil {
		t.Fatal("expected error")
	}
	if res == nil {
		t.Fatalf("expected failed tool result, got nil (err=%v)", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "error: forced failure") {
		t.Fatalf("expected stderr message, got %q", res.Stderr)
	}
}

func TestStdioTransport_Timeout(t *testing.T) {
	transport := &StdioTransport{}
	def := toolpkg.ToolDef{
		Name:      "slow",
		Transport: toolpkg.TransportStdio,
		Command:   toolPath("slow"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := transport.Invoke(ctx, def, "run", map[string]any{"delay_seconds": 1})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected context error, got %v", err)
	}
}

func TestStdioTransport_JSONOutput(t *testing.T) {
	transport := &StdioTransport{}
	def := toolpkg.ToolDef{
		Name:      "json-emitter",
		Transport: toolpkg.TransportStdio,
		Command:   toolPath("json-emitter"),
	}

	res, err := transport.Invoke(context.Background(), def, "emit", map[string]any{"foo": "bar"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Output == nil || res.Output["result"] != "ok" {
		t.Fatalf("expected JSON output, got %v", res.Output)
	}
}
