package presentation

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPresentationNeverInitializesMCPOrAuth(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "must not call", 500) }))
	defer server.Close()
	req, _, _ := contract(t)
	req.Overlays[0].Text = strings.Replace(req.Overlays[0].Text,
		"transport: {type: native, command: never-execute}",
		"transport: {mode: mcp-http, url: '"+server.URL+"', auth: {provider: never-start-auth, scope: never-read-scope}}", 1)
	reply := Resolve(req)
	if reply.Status != "resolved" || len(reply.Regions) != 1 {
		t.Fatalf("metadata required transport/auth initialization: %#v", reply)
	}
	if calls.Load() != 0 {
		t.Fatal("metadata resolver called a transport")
	}
	data, _ := json.Marshal(reply)
	if strings.Contains(string(data), "never-start-auth") || strings.Contains(string(data), "never-read-scope") || strings.Contains(string(data), server.URL) {
		t.Fatal("transport/auth values leaked")
	}
}
