package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type sseFragmentReader struct {
	r io.Reader
	n int
}

func (r sseFragmentReader) Read(p []byte) (int, error) {
	if len(p) > r.n {
		p = p[:r.n]
	}
	return r.r.Read(p)
}

// A generated reader exercises wire limits without allocating an oversized
// fixture or embedding provider data.
type sseRepeatReader byte

func (r sseRepeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestMCPHTTPTransport_SSELargeIncidentCompleteFields(t *testing.T) {
	incident := map[string]any{}
	for i := 0; i < 42; i++ {
		incident[fmt.Sprintf("field_%02d", i)] = strings.Repeat(fmt.Sprintf("synthetic-%02d-λ-", i), 2048)
	}
	incidentBytes, err := json.Marshal(incident)
	if err != nil {
		t.Fatal(err)
	}
	server := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, req map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		wire, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": req["id"],
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(incidentBytes)}}},
		})
		fmt.Fprint(w, ": heartbeat\r\ndata: ")
		for len(wire) > 0 {
			n := min(997, len(wire))
			if _, err := w.Write(wire[:n]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			wire = wire[n:]
		}
		fmt.Fprint(w, "\r\n\r\n")
		w.(http.Flusher).Flush()
		// A terminal response must return without draining an open stream.
		<-r.Context().Done()
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport := NewMCPHTTPTransport(server.URL, nil)
	got, err := transport.Invoke(ctx, toolpkg.ToolDef{}, "synthetic-incident", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 || got.Stdout != string(incidentBytes) || !reflect.DeepEqual(got.Output, incident) {
		t.Fatal("large incident lost or altered fields")
	}
}

func TestMCPHTTPTransport_SSEFramingAndFragmentation(t *testing.T) {
	const response = `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"λ-preserved"}]}}`
	for name, wire := range map[string]string{
		"crlf":      ": comment\r\nevent: message\r\nid: ignored\r\nretry: 100\r\ndata: " + response + "\r\n\r\n",
		"multiline": "data: {\"jsonrpc\":\"2.0\",\ndata: \"id\":7,\ndata: \"result\":\ndata: {\"content\":[{\"type\":\"text\",\"text\":\"λ-preserved\"}]}}\n\n",
		"eof":       "data: " + response,
		"eof-line":  "data: " + response + "\n",
		"skipped":   "data: malformed\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\ndata: {\"id\":2,\"result\":{}}\n\ndata: " + response + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, size := range []int{1, 7, 32768} {
				resp, err := parseBoundedMCPSSE(sseFragmentReader{strings.NewReader(wire), size}, 7)
				if err != nil {
					t.Fatalf("fragment size %d: %v", size, err)
				}
				got, err := (&MCPHTTPTransport{}).toolResult(resp)
				if err != nil || got.Stdout != "λ-preserved" {
					t.Fatalf("fragment size %d: framing changed result", size)
				}
			}
		})
	}
}

func TestMCPHTTPTransport_SSEEventBoundaries(t *testing.T) {
	const prefix = "data: {\"id\":7,\"result\":{\"padding\":\""
	const suffix = "\"}}\n"
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
			padding := mcpHTTPMaxEventBytes - len(prefix) - len(suffix) + delta
			wire := io.MultiReader(strings.NewReader(prefix), io.LimitReader(sseRepeatReader('x'), int64(padding)), strings.NewReader(suffix+"\n"))
			resp, err := parseBoundedMCPSSE(wire, 7)
			if delta <= 0 {
				if err != nil || resp == nil {
					t.Fatalf("within budget: %v", err)
				}
			} else if resp != nil || err == nil || !strings.Contains(err.Error(), "SSE event bytes limit 16777216") {
				t.Fatalf("oversize must fail explicitly: %v", err)
			}
		})
	}
}

func TestMCPHTTPTransport_SSEAggregateBudgets(t *testing.T) {
	for name, reader := range map[string]io.Reader{
		"multiline":      strings.NewReader(strings.Repeat("data: "+strings.Repeat(" ", 1024)+"\n", mcpHTTPMaxEventBytes/1031+1)),
		"ignored-field":  io.MultiReader(strings.NewReader("ignored:"), io.LimitReader(sseRepeatReader('x'), mcpHTTPMaxEventBytes)),
		"comment":        io.MultiReader(strings.NewReader(":"), io.LimitReader(sseRepeatReader('x'), mcpHTTPMaxEventBytes)),
		"many-comments":  strings.NewReader(strings.Repeat(": heartbeat\n", mcpHTTPMaxEventBytes/12+1)),
		"event-count":    strings.NewReader(strings.Repeat("data: {}\n\n", mcpHTTPMaxEvents+1)),
		"response-bytes": io.LimitReader(sseRepeatReader('\n'), mcpHTTPMaxResponseBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := parseBoundedMCPSSE(reader, 7)
			if resp != nil || err == nil || !strings.Contains(err.Error(), "MCP-008") || !strings.Contains(err.Error(), "not truncated") {
				t.Fatalf("budget must reject entire result: %v", err)
			}
		})
	}
	// Exactly the event-count limit is allowed, including the matching event.
	wire := strings.Repeat("data: malformed\n\n", mcpHTTPMaxEvents-1) + "data: {\"id\":7,\"result\":{}}\n\n"
	if _, err := parseBoundedMCPSSE(strings.NewReader(wire), 7); err != nil {
		t.Fatal(err)
	}
}

type sseErrorReader struct{ err error }

func (r sseErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestMCPHTTPTransport_SSEMalformedEOFAndReadFailure(t *testing.T) {
	for _, wire := range []string{"", ": heartbeat\n\n", "data: {broken}\n\n", "data: {\"id\":7", "data\n\n"} {
		resp, err := parseBoundedMCPSSE(strings.NewReader(wire), 7)
		if resp != nil || err == nil || !strings.Contains(err.Error(), "MCP-006") {
			t.Fatalf("missing/malformed response must fail: %v", err)
		}
	}
	boom := errors.New("synthetic network failure")
	reader := io.MultiReader(strings.NewReader("data: {\"id\":7,\"result\":{}}"), sseErrorReader{boom})
	if resp, err := parseBoundedMCPSSE(reader, 7); resp != nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), "MCP-008") {
		t.Fatalf("network failure must not flush success: %v", err)
	}
}

func TestMCPHTTPTransport_SSECancelDuringPartialEvent(t *testing.T) {
	started := make(chan struct{})
	server := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewMCPHTTPTransport(server.URL, nil).Invoke(ctx, toolpkg.ToolDef{}, "synthetic", nil)
		done <- err
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("no partial SSE event received")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "MCP-008") {
			t.Fatalf("cancellation not preserved: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop SSE read")
	}
}

func TestMCPHTTPTransport_JSONResponseBounded(t *testing.T) {
	transport := &MCPHTTPTransport{}
	resp, err := transport.parseJSONResponse(io.LimitReader(sseRepeatReader(' '), mcpHTTPMaxResponseBytes+1), 7)
	if resp != nil || err == nil || !strings.Contains(err.Error(), "response bytes limit 67108864") {
		t.Fatalf("oversize JSON must be rejected: %v", err)
	}
}

func TestMCPHTTPTransport_SSEInitializationListAndSessionRetry(t *testing.T) {
	initializations, calls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}

		var result any
		switch req["method"] {
		case "initialize":
			initializations++
			result = map[string]any{"protocolVersion": mcpProtocolVersion, "padding": strings.Repeat("x", 128<<10)}
		case "notifications/initialized", "shutdown":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "synthetic", "description": strings.Repeat("x", 128<<10)}}}
		case "tools/call":
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 128<<10)}}}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result})
		fmt.Fprintf(w, "data: %s\n\n", payload)
	}))
	defer server.Close()
	transport := NewMCPHTTPTransport(server.URL, nil)
	tools, err := transport.ListTools(context.Background(), toolpkg.ToolDef{})
	if err != nil || len(tools) != 1 {
		t.Fatalf("SSE initialization/list failed: %v", err)
	}
	got, err := transport.Invoke(context.Background(), toolpkg.ToolDef{}, "synthetic", nil)
	if err != nil || got == nil || len(got.Stdout) != 128<<10 || initializations != 2 || calls != 2 {
		t.Fatalf("SSE session retry failed: initializations=%d calls=%d err=%v", initializations, calls, err)
	}
}

func TestMCPHTTPTransport_SSEAuthRetry(t *testing.T) {
	calls := 0
	server := newMinimalServer(t, func(w http.ResponseWriter, r *http.Request, req map[string]any) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.(http.Flusher).Flush()
			// Auth retry must close this body rather than drain it.
			<-r.Context().Done()
			return
		}
		toolCallResponseSSE(w, req["id"], strings.Repeat("x", 128<<10))
	})
	defer server.Close()
	gate := NewTokenGate(newMockAuthProvider("synthetic-test-token"), "synthetic-scope", []string{"127.0.0.1"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := NewMCPHTTPTransport(server.URL, gate).Invoke(ctx, toolpkg.ToolDef{}, "synthetic", nil)
	if err != nil || result == nil || len(result.Stdout) != 128<<10 || calls != 2 {
		t.Fatalf("SSE auth retry failed: calls=%d err=%v", calls, err)
	}
}
