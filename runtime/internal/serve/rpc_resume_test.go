package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func resumeRPCPayload() map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "run.resume",
		"params": map[string]any{"runID": "run-1"},
	}
}

func TestRPCResumeReservesBrokerAndCleansFailure(t *testing.T) {
	h := newTestServerHarness(t)
	t.Cleanup(func() { _ = h.server.Stop(context.Background()) })
	attempts := 0
	var failedCtx context.Context
	h.engine.resumeFunc = func(ctx context.Context, _ string, _ engine.RunOptions) (engine.RunHandle, error) {
		attempts++
		if h.server.broker.queueFor("run-1") == nil {
			t.Fatal("Resume called without broker")
		}
		if attempts == 1 {
			failedCtx = ctx
			return nil, errors.New("checkpoint invalid")
		}
		return h.handle, nil
	}
	if resp := doRPC(t, h.server, resumeRPCPayload()); resp.Error == nil {
		t.Fatal("expected resume failure")
	}
	if failedCtx.Err() != context.Canceled {
		t.Fatal("failed attachment context was not released")
	}
	if h.server.broker.queueFor("run-1") != nil {
		t.Fatal("failed resume retained broker")
	}
	if _, ok := h.server.registry.Get("run-1"); ok {
		t.Fatal("failed resume registered handle")
	}
	if resp := doRPC(t, h.server, resumeRPCPayload()); resp.Error != nil {
		t.Fatalf("retry after failure: %+v", resp.Error)
	}
	queue := h.server.broker.queueFor("run-1")
	if resp := doRPC(t, h.server, resumeRPCPayload()); resp.Error == nil || resp.Error.Code != rpcRunLocked {
		t.Fatalf("duplicate attach: %+v", resp)
	}
	if attempts != 2 || h.server.broker.queueFor("run-1") != queue {
		t.Fatal("duplicate attachment changed active broker/engine")
	}
}

func TestRPCResumeRejectsInFlightBrokerReservation(t *testing.T) {
	h := newTestServerHarness(t)
	queue := h.server.broker.Register("run-1")
	h.engine.resumeFunc = func(_ context.Context, _ string, _ engine.RunOptions) (engine.RunHandle, error) {
		t.Fatal("Resume must not acquire an already reserved broker")
		return nil, nil
	}
	if resp := doRPC(t, h.server, resumeRPCPayload()); resp.Error == nil || resp.Error.Code != rpcRunLocked {
		t.Fatalf("in-flight attachment: %+v", resp)
	}
	if h.server.broker.queueFor("run-1") != queue {
		t.Fatal("in-flight queue clobbered")
	}
}

func TestRPCResumeConcurrentAttachmentAndRequestLifetime(t *testing.T) {
	h := newTestServerHarness(t)
	t.Cleanup(func() { _ = h.server.Stop(context.Background()) })
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	h.engine.resumeFunc = func(ctx context.Context, _ string, _ engine.RunOptions) (engine.RunHandle, error) {
		entered <- ctx
		<-release
		return h.handle, nil
	}
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	data, err := json.Marshal(resumeRPCPayload())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader(data)).WithContext(requestCtx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.server.handler.ServeHTTP(recorder, request)
		close(done)
	}()
	var runCtx context.Context
	select {
	case runCtx = <-entered:
	case <-time.After(time.Second):
		t.Fatal("Resume not entered")
	}
	queue := h.server.broker.queueFor("run-1")
	second := doRPC(t, h.server, resumeRPCPayload())
	close(release)
	<-done
	if second.Error == nil || second.Error.Code != rpcRunLocked {
		t.Fatalf("concurrent resume: %+v", second.Error)
	}
	cancelRequest()
	if runCtx.Err() != nil {
		t.Fatal("HTTP response lifetime canceled resumed execution")
	}
	entry, ok := h.server.registry.Get("run-1")
	if !ok || entry.Handle != h.handle || h.server.broker.queueFor("run-1") != queue {
		t.Fatal("concurrent attachment changed ownership")
	}
	entry.Cancel()
	if runCtx.Err() != context.Canceled {
		t.Fatal("run cancellation did not cancel detached context")
	}
}
