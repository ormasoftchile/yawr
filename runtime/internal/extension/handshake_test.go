package extension

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
)

func TestHandshake_Success(t *testing.T) {
	proc := startTestProcess(t)
	defer func() { _ = proc.stop(context.Background()) }()
	grants := extension.NewCapabilitySet([]string{extension.CapabilityToolRegistration})
	if _, err := performHandshake(context.Background(), proc, grants); err != nil {
		t.Fatalf("expected handshake success: %v", err)
	}
}

func TestHandshake_CapabilityViolation(t *testing.T) {
	proc := startTestProcess(t)
	defer func() { _ = proc.stop(context.Background()) }()
	grants := extension.NewCapabilitySet(nil)
	if _, err := performHandshake(context.Background(), proc, grants); err != ErrCapabilityViolation {
		t.Fatalf("expected capability violation, got %v", err)
	}
}

func TestHandshake_MalformedResponse(t *testing.T) {
	client, server := net.Pipe()
	codec := newRPCCodec(client, client)
	go func() {
		dec := json.NewDecoder(server)
		enc := json.NewEncoder(server)
		var req rpcRequest
		_ = dec.Decode(&req)
		_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage([]byte("\"bad\""))})
	}()
	proc := &extensionProcess{codec: codec, decl: extension.ExtensionDecl{Name: "bad-ext"}}
	grants := extension.NewCapabilitySet([]string{extension.CapabilityToolRegistration})
	if _, err := performHandshake(context.Background(), proc, grants); err == nil {
		t.Fatalf("expected malformed response error")
	}
	_ = client.Close()
	_ = server.Close()
}

func TestHandshake_ContributionsListed(t *testing.T) {
	proc := startTestProcess(t)
	defer func() { _ = proc.stop(context.Background()) }()
	grants := extension.NewCapabilitySet([]string{extension.CapabilityToolRegistration})
	result, err := performHandshake(context.Background(), proc, grants)
	if err != nil {
		t.Fatalf("expected handshake success: %v", err)
	}
	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result.Tools))
	}
	if result.Tools[0].Def == nil || result.Tools[0].Def.Name != "hello" {
		t.Fatalf("expected hello tool")
	}
}

func startTestProcess(t *testing.T) *extensionProcess {
	t.Helper()
	decl := testExtensionDecl(t)
	proc, err := startProcess(context.Background(), decl)
	if err != nil {
		t.Fatalf("failed to start process: %v", err)
	}
	return proc
}

func testExtensionDecl(t *testing.T) extension.ExtensionDecl {
	t.Helper()
	root := os.Getenv("YAWR_EXT_DIR")
	if root == "" {
		t.Fatalf("YAWR_EXT_DIR not set")
	}
	path := filepath.Join(root)
	return extension.ExtensionDecl{
		Name:       "hello-ext",
		Path:       path,
		Entrypoint: "hello-ext",
		Grants:     []string{extension.CapabilityToolRegistration},
	}
}
