package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

func TestWS_ConnectReceivesEvents(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	conn := dialWS(t, ts.URL+"/ws")
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	waitForWSSubscriber(t, h)

	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/started",
		RunID:    "run-1",
		Sequence: 1,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{},
	})

	ev := readWSEvent(t, conn)
	if ev.Type != "step/started" {
		t.Fatalf("expected step/started, got %s", ev.Type)
	}
}

func TestWS_FilterByRunID(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	conn := dialWS(t, ts.URL+"/ws?runID=run-2")
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	waitForWSSubscriber(t, h)

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

	ev := readWSEvent(t, conn)
	if ev.RunID != "run-2" {
		t.Fatalf("expected run-2, got %s", ev.RunID)
	}
}

func TestWS_PreviewPayloadIsBounded(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	conn := dialWS(t, ts.URL+"/ws?runID=run-1")
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	waitForWSSubscriber(t, h)

	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "step/output",
		RunID:    "run-1",
		Sequence: 1,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{"step_id": "query", "line": strings.Repeat("large-json", 3000)},
	})
	ev := readWSEvent(t, conn)
	line, _ := ev.Payload["line"].(string)
	if len(line) > 4096 || ev.Payload["line_preview_truncated"] != true {
		t.Fatalf("ws payload line was not bounded/marked: %#v", ev.Payload)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 12000 {
		t.Fatalf("ws frame remains unbounded: got %d", len(body))
	}
}

func TestWS_RunCompleted_ReceivesTerminal(t *testing.T) {
	h := newTestServerHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	conn := dialWS(t, ts.URL+"/ws?runID=run-1")
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	waitForWSSubscriber(t, h)

	h.server.bridge.Broadcast(servepkg.RunEvent{
		Type:     "run/completed",
		RunID:    "run-1",
		Sequence: 9,
		TS:       time.Now().Format(time.RFC3339Nano),
		Payload:  map[string]any{},
	})

	ev := readWSEvent(t, conn)
	if ev.Type != "run/completed" {
		t.Fatalf("expected run/completed, got %s", ev.Type)
	}
}

func TestWS_SlowConsumer_DropsEvents(t *testing.T) {
	h := newTestServerHarness(t)
	h.server.cfg.EventBufferSize = 1
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	conn := dialWS(t, ts.URL+"/ws?runID=run-1")
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	waitForWSSubscriber(t, h)

	payload := strings.Repeat("x", 256*1024)
	for i := 0; i < 1000; i++ {
		h.server.bridge.Broadcast(servepkg.RunEvent{
			Type:     "step/started",
			RunID:    "run-1",
			Sequence: int64(i + 1),
			TS:       time.Now().Format(time.RFC3339Nano),
			Payload:  map[string]any{"blob": payload},
		})
	}

	time.Sleep(50 * time.Millisecond)

	received := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, _, err := conn.Read(ctx)
		cancel()
		if err != nil {
			break
		}
		received++
		if received > 1000 {
			break
		}
	}
	if received >= 1000 {
		t.Fatalf("expected dropped events, received %d", received)
	}
}

func waitForWSSubscriber(t *testing.T, h *testServerHarness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.server.bridge.WaitForSubscriber(ctx, 2*time.Second); err != nil {
		t.Fatalf("WaitForSubscriber: %v", err)
	}
}

func dialWS(t *testing.T, baseURL string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	return conn
}

func readWSEvent(t *testing.T, conn *websocket.Conn) servepkg.RunEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read ws: %v", err)
	}
	var ev servepkg.RunEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal ws event: %v", err)
	}
	return ev
}
