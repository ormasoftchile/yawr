package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalserve "github.com/ormasoftchile/yawr/runtime/internal/serve"
	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

func TestServe_ToolDefaultsAndDefinitionIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	pkgDir := filepath.Join(dir, "package")
	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: synthetic.isolation, version: "1.0.0"}
exports:
  tools: [{id: public-parent, path: parent.tool.yaml}]
`)
	writeFile(t, filepath.Join(pkgDir, "parent.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: public-parent, version: "1.0.0"}
transport: {mode: native, command: must-not-dispatch}
actions:
  - name: cpu
    args: {}
    outputs: {result: {type: object}}
    execute: {kind: runbook, path: parent.runbook.yaml}
  - name: memory
    args: {}
    outputs: {result: {type: object}}
    execute: {kind: runbook, path: parent.runbook.yaml}
`)
	writeFile(t, filepath.Join(pkgDir, "helper.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: private-helper, version: "1.0.0"}
transport: {mode: native, command: must-not-dispatch}
actions:
  - name: high
    args:
      hash: {type: string, required: false, default: ""}
      verified: {type: boolean, required: false, default: false}
    outputs: {result: {type: object}}
    execute: {kind: runbook, path: helper.runbook.yaml}
  - name: low
    args:
      hash: {type: string, required: false, default: ""}
      verified: {type: boolean, required: false, default: false}
    outputs: {result: {type: object}}
    execute: {kind: runbook, path: helper.runbook.yaml}
`)
	writeFile(t, filepath.Join(pkgDir, "parent.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: parent
name: Parent
toolRefs: [{name: private-helper}]
outputs: {result: {type: object, value_expr: child_result}}
flow:
  - step:
      id: high_child
      type: tool
      tool: {name: private-helper, action: high, args: {}}
      capture: {high_result: outputs.result}
  - step:
      id: low_child
      type: tool
      tool: {name: private-helper, action: low, args: {}}
      capture: {child_result: outputs.result}
`)
	writeFile(t, filepath.Join(pkgDir, "helper.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: helper
name: Helper
inputs:
  hash: {type: string, required: false, default: ""}
  verified: {type: boolean, required: false, default: false}
outputs:
  result: {type: object, value_expr: vars}
flow:
  - step: {id: marker, type: noop, capture: {synthetic: no-provider}}
  - step:
      id: strict_defaults
      type: assert
      assert:
        - {type: eq, subject: '${hash == "" and verified == false}', expected: "true"}
`)
	for _, action := range []string{"cpu", "memory"} {
		writeFile(t, filepath.Join(dir, action+".runbook.yaml"), fmt.Sprintf(`apiVersion: yawr.runbook/v1
id: %s
name: Synthetic caller
toolRefs: [{name: public-parent, package: synthetic.isolation}]
flow:
  - step:
      id: first_call
      type: tool
      tool: {name: public-parent, action: %s}
      capture: {result: outputs.result}
  - step:
      id: repeated_call
      type: tool
      tool: {name: public-parent, action: %s}
      capture: {result: outputs.result}
  - step:
      id: verify_output
      type: assert
      assert:
        - {type: eq, subject: '${result.hash == "" and result.verified == false and result.synthetic == "no-provider"}', expected: "true"}
`, action, action, action))
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := newServingToolRegistry(dir, pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(registry.tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.tools) != 4 {
		t.Fatalf("unexpected provider tools in synthetic registry: %d", len(registry.tools))
	}
	plannerImpl := internalplanner.New(planner.Config{Loader: &fileRunbookLoader{parser: parserImpl}, Tools: registry})
	cfg, shutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		RunDir: filepath.Join(dir, "runs"), ToolScanDir: dir, ExtraToolScanPaths: []string{pkgDir},
		SubstitutionParser: parserImpl, LazyRunbookLoader: adapter.NewParserLazyLoader(parserImpl),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server, err := internalserve.NewServer(serve.ServerConfig{
		Addr: addr, Engine: internalengine.New(cfg), EngineConfig: cfg, Parser: parserImpl, Planner: plannerImpl,
		WorkspaceRoot: dir, ProjectToolPaths: []string{"package"},
		ProjectRequires: []*schema.PackageRequirement{{Package: "synthetic.isolation", Version: "^1.0.0", Path: "package"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: 20 * time.Second}
	base := "http://" + addr
	healthy := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get(base + "/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("synthetic server did not become healthy")
	}
	run := func(action string) error {
		body, _ := json.Marshal(map[string]any{"runbookPath": filepath.Join(dir, action+".runbook.yaml"), "mode": "real", "actor": "synthetic", "inputs": map[string]string{}})
		response, err := client.Post(base+"/runs", "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusCreated {
			return fmt.Errorf("POST: %d %s", response.StatusCode, data)
		}
		var created struct {
			RunID string `json:"runID"`
		}
		if err := json.Unmarshal(data, &created); err != nil {
			return err
		}
		if created.RunID == "" {
			return fmt.Errorf("missing run ID: %s", data)
		}
		response, err = client.Get(base + "/runs/" + created.RunID + "/state")
		if err != nil {
			return err
		}
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var state struct {
				Status string         `json:"status"`
				Vars   map[string]any `json:"vars"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &state); err != nil {
				return err
			}
			switch state.Status {
			case "completed":
				result, ok := state.Vars["result"].(map[string]any)
				if !ok || result["hash"] != "" || result["verified"] != false || result["synthetic"] != "no-provider" {
					return fmt.Errorf("invalid typed result: %#v", state.Vars)
				}
				return nil
			case "failed", "cancelled":
				return fmt.Errorf("%s run: %s", action, line)
			}
		}
		return fmt.Errorf("no terminal state: %v", scanner.Err())
	}
	// Both orders and same-action repetitions share the startup registry.
	for _, action := range []string{"cpu", "memory", "memory", "cpu"} {
		if err := run(action); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(action string) {
			defer wg.Done()
			if err := run(action); err != nil {
				errors <- err
			}
		}([]string{"cpu", "memory"}[i%2])
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	after, err := json.Marshal(registry.tools)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("served requests mutated shared registry definitions")
	}
}
