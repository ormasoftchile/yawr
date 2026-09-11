package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The subprocess enters production main(), trusting only the synthetic
// loopback server's certificate in addition to its normal runtime wiring.
func TestMCPHTTPStdioCLIProcess(t *testing.T) {
	certPath := os.Getenv("YAWR_SSE_TEST_CERT")
	if certPath == "" {
		return
	}
	cert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		t.Fatal("invalid synthetic server certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = transport
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing CLI arguments")
}

func TestMCPHTTPStdioLargeAndOversize(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversize=%v", oversize), func(t *testing.T) {
			dir := t.TempDir()
			var calls atomic.Int32
			fields := map[string]any{}
			var descriptors, captures, assertions strings.Builder
			for i := 0; i < 42; i++ {
				name := fmt.Sprintf("field_%02d", i)
				value := strings.Repeat(fmt.Sprintf("synthetic-%02d-", i), 512)
				fields[name] = value
				fmt.Fprintf(&descriptors, "      %s: {type: string}\n", name)
				fmt.Fprintf(&captures, "        %s: outputs.%s\n", name, name)
				fmt.Fprintf(&assertions, "        - {type: eq, subject: '${%s}', expected: '%s'}\n", name, value)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, "bad request", 400)
					return
				}
				switch req["method"] {
				case "initialize":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"protocolVersion": "2025-03-26"}})
				case "notifications/initialized", "shutdown":
					w.WriteHeader(http.StatusAccepted)
				case "tools/call":
					calls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					if oversize {
						fmt.Fprint(w, "data: ")
						chunk := strings.Repeat("x", 32<<10)
						for i := 0; i <= 512; i++ {
							if _, err := fmt.Fprint(w, chunk); err != nil {
								return
							}
							w.(http.Flusher).Flush()
						}
						return
					}
					text, _ := json.Marshal(fields)
					wire, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req["id"],
						"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}})
					fmt.Fprint(w, ": synthetic heartbeat\r\ndata: ")
					for len(wire) > 0 {
						n := min(997, len(wire))
						if _, err := w.Write(wire[:n]); err != nil {
							return
						}
						w.(http.Flusher).Flush()
						wire = wire[n:]
					}
					fmt.Fprint(w, "\r\n\r\n")
				default:
					http.Error(w, "unexpected method", 400)
				}
			}))
			defer server.Close()
			certPath := filepath.Join(dir, "synthetic-cert.pem")
			writeFile(t, certPath, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})))
			writeFile(t, filepath.Join(dir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: sse.synthetic, version: "1.0.0"}
exports:
  tools: [{id: synthetic-incident, path: incident.tool.yaml}]
`)
			writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\nrequires:\n  - {package: sse.synthetic, version: '^1.0.0', path: '.'}\n")
			writeFile(t, filepath.Join(dir, "incident.tool.yaml"), fmt.Sprintf(`apiVersion: yawr.tool/v1
meta: {name: synthetic-incident, version: "1.0.0"}
transport: {mode: mcp-http, url: '%s'}
actions:
  - name: get
    classification: read-only
    args: {}
    outputs:
%s`, server.URL, descriptors.String()))
			writeFile(t, filepath.Join(dir, "profile.yaml"), `apiVersion: yawr.runtime-profile/v1
id: synthetic
context: test
attendance: unattended
approval:
  scope: {allow_read: true, allow_mutating: false, allow_destructive: false}
`)
			writeFile(t, filepath.Join(dir, "root.runbook.yaml"), fmt.Sprintf(`apiVersion: yawr.runbook/v1
id: synthetic-sse
name: Synthetic SSE complete-field preservation
toolRefs: [{name: synthetic-incident, package: sse.synthetic}]
flow:
  - step:
      id: get_incident
      type: tool
      tool: {name: synthetic-incident, action: get, args: {}}
      capture:
%s  - step:
      id: verify_all_fields
      type: assert
      assert:
%s`, captures.String(), assertions.String()))
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestMCPHTTPStdioCLIProcess$", "--",
				"run", "root.runbook.yaml", "--stdio", "--profile", "profile.yaml", "--run-dir", "runs", "--trace", "trace.jsonl")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "YAWR_SSE_TEST_CERT="+certPath)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close() // Hold stdin open until the actual run exits.
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			if calls.Load() != 1 {
				t.Fatalf("actual MCP dispatch count=%d, exit=%v stderr=%s stdout=%s", calls.Load(), err, stderr.String(), stdout.String())
			}
			if (err != nil) != oversize {
				t.Fatalf("unexpected CLI exit: %v stderr=%s stdout=%s", err, stderr.String(), stdout.String())
			}
			scanner := bufio.NewScanner(&stdout)
			scanner.Buffer(make([]byte, 64<<10), 2<<20)
			var finished, verified, failed, budgetError bool
			for scanner.Scan() {
				var frame map[string]any
				if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
					t.Fatal(err)
				}
				if frame["type"] == "run.finished" {
					finished = true
				}
				event, _ := frame["event"].(map[string]any)
				payload, _ := event["payload"].(map[string]any)
				if event["kind"] == "step/completed" && payload["step_id"] == "verify_all_fields" {
					verified = true
				}
				if event["kind"] == "step/failed" && payload["step_id"] == "get_incident" {
					failed = true
					budgetError = strings.Contains(scanner.Text(), "MCP-008") && strings.Contains(scanner.Text(), "16777216")
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if !finished || (!oversize && !verified) || (oversize && (!failed || !budgetError || verified)) {
				t.Fatalf("stdio lifecycle: finished=%v verified=%v failed=%v budgetError=%v", finished, verified, failed, budgetError)
			}
		})
	}
}
