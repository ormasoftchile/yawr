package extension

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
)

func TestHealthLoop_PingSuccess(t *testing.T) {
	reset := setPingTimings(10*time.Millisecond, 50*time.Millisecond)
	defer reset()
	proc, cleanup := pingProcess(t, "pong")
	defer cleanup()
	proc.setState(extension.StateLoaded)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startHealthLoop(ctx, proc, nil)
	time.Sleep(30 * time.Millisecond)
	if proc.getState() != extension.StateLoaded {
		t.Fatalf("expected state loaded")
	}
}

func TestHealthLoop_PingFailure_SetsStateFailed(t *testing.T) {
	reset := setPingTimings(10*time.Millisecond, 15*time.Millisecond)
	defer reset()
	proc, cleanup := pingProcess(t, "nope")
	defer cleanup()
	proc.setState(extension.StateLoaded)
	failure := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startHealthLoop(ctx, proc, func(_ string, err error) {
		failure <- err
	})
	select {
	case <-failure:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected failure callback")
	}
	if proc.getState() != extension.StateFailed {
		t.Fatalf("expected state failed")
	}
}

func TestHealthLoop_ContextCancel_Stops(t *testing.T) {
	reset := setPingTimings(10*time.Millisecond, 50*time.Millisecond)
	defer reset()
	proc, cleanup := pingProcess(t, "pong")
	defer cleanup()
	proc.setState(extension.StateLoaded)
	failure := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	startHealthLoop(ctx, proc, func(_ string, err error) {
		failure <- err
	})
	cancel()
	select {
	case <-failure:
		t.Fatalf("did not expect failure callback")
	case <-time.After(30 * time.Millisecond):
	}
}

func pingProcess(t *testing.T, response string) (*extensionProcess, func()) {
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
			if req.Method == "extension/ping" {
				_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage([]byte("\"" + response + "\""))})
			}
		}
	}()
	proc := &extensionProcess{codec: codec, decl: extension.ExtensionDecl{Name: "health-ext"}}
	cleanup := func() {
		_ = client.Close()
		_ = server.Close()
	}
	return proc, cleanup
}

func setPingTimings(interval, timeout time.Duration) func() {
	prevInterval := time.Duration(atomic.LoadInt64(&pingInterval))
	prevTimeout := time.Duration(atomic.LoadInt64(&pingTimeout))
	atomic.StoreInt64(&pingInterval, int64(interval))
	atomic.StoreInt64(&pingTimeout, int64(timeout))
	return func() {
		atomic.StoreInt64(&pingInterval, int64(prevInterval))
		atomic.StoreInt64(&pingTimeout, int64(prevTimeout))
	}
}
