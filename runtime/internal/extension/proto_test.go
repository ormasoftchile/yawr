package extension

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

func TestRPCCodec_Call_Success(t *testing.T) {
	codec, cleanup := setupCodec(t, func(req rpcRequest) rpcResponse {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage([]byte("{\"ok\":true}"))}
	})
	defer cleanup()
	var resp map[string]bool
	if err := codec.call(context.Background(), "test/success", map[string]any{"value": "ok"}, &resp); err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if !resp["ok"] {
		t.Fatalf("expected ok response")
	}
}

func TestRPCCodec_Call_RPCError(t *testing.T) {
	codec, cleanup := setupCodec(t, func(req rpcRequest) rpcResponse {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32000, Message: "boom"}}
	})
	defer cleanup()
	if err := codec.call(context.Background(), "test/error", nil, nil); err == nil {
		t.Fatalf("expected error")
	}
}

func TestRPCCodec_Call_Timeout(t *testing.T) {
	client, server := net.Pipe()
	codec := newRPCCodec(client, client)
	defer client.Close()
	defer server.Close()
	go func() {
		var req rpcRequest
		_ = json.NewDecoder(server).Decode(&req)
		select {}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := codec.call(ctx, "test/timeout", nil, nil); err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestRPCCodec_Call_Concurrent(t *testing.T) {
	codec, cleanup := setupCodec(t, func(req rpcRequest) rpcResponse {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage([]byte("{\"ok\":true}"))}
	})
	defer cleanup()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := codec.call(context.Background(), "test/concurrent", nil, nil); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
}

func setupCodec(t *testing.T, handler func(rpcRequest) rpcResponse) (*rpcCodec, func()) {
	t.Helper()
	client, server := net.Pipe()
	codec := newRPCCodec(client, client)
	go func() {
		dec := json.NewDecoder(server)
		enc := json.NewEncoder(server)
		for {
			var req rpcRequest
			if err := dec.Decode(&req); err != nil {
				return
			}
			resp := handler(req)
			_ = enc.Encode(resp)
		}
	}()
	return codec, func() {
		_ = client.Close()
		_ = server.Close()
	}
}
