package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

func TestSSE_ConnectReceivesEvents(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	// Open SSE connection in a goroutine; the handler registers a subscriber
	// after writing headers, so we must wait for registration before broadcasting.
	connDone := make(chan *http.Response, 1)
	go func() {
		connDone <- openSSE(t, ts.URL+"/events", "")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.server.bridge.WaitForSubscriber(ctx, 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}

	resp := <-connDone
	defer resp.Body.Close()

	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/started",
		RunID:    "run-1",
		Sequence: 1,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{},
	})

	ev := readSSEEvent(t, resp.Body)
	if ev.Type != "step/started" {
		t.Fatalf("expected step/started, got %s", ev.Type)
	}
}

func TestSSE_LastEventID_Replay(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/started",
		RunID:    "run-1",
		Sequence: 1,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{},
	})
	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/completed",
		RunID:    "run-1",
		Sequence: 2,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{},
	})

	resp := openSSE(t, ts.URL+"/events?runID=run-1", "1")
	defer resp.Body.Close()

	ev := readSSEEvent(t, resp.Body)
	if ev.Sequence != 2 {
		t.Fatalf("expected replay sequence 2, got %d", ev.Sequence)
	}
}

func TestSSE_FilterByRunID(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	// Open SSE connection in a goroutine; the handler registers a subscriber
	// after writing headers, so we must wait for registration before broadcasting.
	connDone := make(chan *http.Response, 1)
	go func() {
		connDone <- openSSE(t, ts.URL+"/events?runID=run-2", "")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.server.bridge.WaitForSubscriber(ctx, 2*time.Second); err != nil {
		t.Fatalf("subscriber not registered: %v", err)
	}

	resp := <-connDone
	defer resp.Body.Close()

	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/started",
		RunID:    "run-1",
		Sequence: 1,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{},
	})

	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/started",
		RunID:    "run-2",
		Sequence: 2,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{},
	})

	ev := readSSEEvent(t, resp.Body)
	if ev.RunID != "run-2" {
		t.Fatalf("expected run-2, got %s", ev.RunID)
	}
}

func openSSE(t *testing.T, url string, lastEventID string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
	return resp
}

func readSSEEvent(t *testing.T, body io.Reader) servepkg.RunEvent {
	t.Helper()
	type result struct {
		ev  servepkg.RunEvent
		err error
	}
	ch := make(chan result, 1)
	go func() {
		reader := bufio.NewReader(body)
		var ev servepkg.RunEvent
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				if ev.Type != "" {
					ch <- result{ev: ev}
					return
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if err := json.Unmarshal([]byte(payload), &ev); err != nil {
					ch <- result{err: err}
					return
				}
			}
		}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read sse: %v", res.err)
		}
		return res.ev
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for sse event")
		return servepkg.RunEvent{}
	}
}

// TestSSE_HeartbeatKeepsConnectionAlive is a regression guard for the
// SSE heartbeat: idle connections must emit a comment frame so proxies
// and the browser do not declare the stream dead. Shrinks the interval
// for fast assertion.
func TestSSE_HeartbeatKeepsConnectionAlive(t *testing.T) {
	prev := sseHeartbeatInterval
	sseHeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { sseHeartbeatInterval = prev })

	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	resp := openSSE(t, ts.URL+"/events", "")
	defer resp.Body.Close()

	// Read until a heartbeat comment line (":hb") appears.
	deadline := time.Now().Add(2 * time.Second)
	reader := bufio.NewReader(resp.Body)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, ":") {
			return
		}
	}
	t.Fatal("no heartbeat received within deadline")
}
