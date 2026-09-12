package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

type noToolRegistry struct{}

func (noToolRegistry) Lookup(context.Context, string, string) (*schema.ToolDef, error) {
	return nil, plannerpkg.ErrToolNotFound
}

type testRunbookLoader struct{ parser parser.Parser }

func (l testRunbookLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	return l.parser.Parse(ctx, path)
}

func newCatalogServeTestServer(t *testing.T, root string) (*Server, string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir(%s): %v", root, err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader:       testRunbookLoader{parser: parserImpl},
		Tools:        noToolRegistry{},
		ExpandPolicy: expand.Policy{Default: expand.ModeLazy},
	})
	broker := NewPromptBroker(16)
	runDir := filepath.Join(root, ".runbook", "runs")
	ecfg, shutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		RunDir:                 runDir,
		ToolScanDir:            root,
		ExcludeTestTools:       true,
		PromptProviderOverride: broker,
		LazyRunbookLoader:      adapter.NewParserLazyLoader(parserImpl),
		SubstitutionParser:     parserImpl,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	t.Cleanup(shutdown)
	srv, err := NewServerWithBroker(servepkg.ServerConfig{
		Engine:          internalengine.New(ecfg),
		EngineConfig:    ecfg,
		Parser:          parserImpl,
		Planner:         plannerImpl,
		EventBufferSize: 32,
	}, broker)
	if err != nil {
		t.Fatalf("NewServerWithBroker: %v", err)
	}
	return srv, runDir
}

func TestRunsCreate_CatalogIncludeResolvesPerRun(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "remediation-pkg")
	writeCatalogPackage(t, pkgDir, "demo.remediation", "remediate", "REMEDIATION BODY EXECUTED for target=${target}")
	caller := filepath.Join(root, "caller.runbook.yaml")
	writeCallerRunbookWithPathAndTarget(t, caller, "demo.remediation", "^1.0.0", "./remediation-pkg", "remediate", "prod-db-01")

	srv, runDir := newCatalogServeTestServer(t, root)
	runID, status, body := postRun(t, srv, caller, nil)
	if status != http.StatusCreated {
		t.Fatalf("POST /runs status = %d, want 201; body=%s", status, body)
	}
	waitRunTerminal(t, srv, runID, engine.RunStatusCompleted)
	trace := readTrace(t, runDir, runID)
	if !strings.Contains(trace, "REMEDIATION BODY EXECUTED for target=prod-db-01") {
		t.Fatalf("served dynamic include did not execute child body; trace:\n%s", trace)
	}
	if strings.Contains(trace, "no DynamicIncludeResolver configured") || strings.Contains(trace, "SERVE-W001") {
		t.Fatalf("serve still reports unresolved catalog gap; trace:\n%s", trace)
	}
}

func TestRunsCreate_CatalogBuildFailureIsSynchronous(t *testing.T) {
	root := t.TempDir()
	caller := filepath.Join(root, "caller.runbook.yaml")
	writeCallerRunbook(t, caller, "missing.pkg", "^1.0.0", "remediate", "prod-db-01")

	srv, _ := newCatalogServeTestServer(t, root)
	_, status, body := postRun(t, srv, caller, nil)
	if status < 400 || status >= 500 {
		t.Fatalf("POST /runs status = %d, want synchronous 4xx; body=%s", status, body)
	}
	if !strings.Contains(body, "\"code\":\"PKG-") {
		t.Fatalf("expected PKG-* code in synchronous error body, got: %s", body)
	}
}

func TestRunsCreate_ConcurrentRunsUseIsolatedCatalogs(t *testing.T) {
	root := t.TempDir()
	writeCatalogPackage(t, filepath.Join(root, "pkg-a"), "demo.a", "remediate", "BODY A EXECUTED")
	writeCatalogPackage(t, filepath.Join(root, "pkg-b"), "demo.b", "remediate", "BODY B EXECUTED")
	callerA := filepath.Join(root, "caller-a.runbook.yaml")
	callerB := filepath.Join(root, "caller-b.runbook.yaml")
	writeCallerRunbookWithPath(t, callerA, "demo.a", "^1.0.0", "./pkg-a", "remediate")
	writeCallerRunbookWithPath(t, callerB, "demo.b", "^1.0.0", "./pkg-b", "remediate")

	srv, runDir := newCatalogServeTestServer(t, root)
	type outcome struct {
		runID  string
		status int
		body   string
	}
	ch := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, p := range []string{callerA, callerB} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			runID, status, body := postRun(t, srv, path, nil)
			ch <- outcome{runID: runID, status: status, body: body}
		}(p)
	}
	wg.Wait()
	close(ch)

	for got := range ch {
		if got.status != http.StatusCreated {
			t.Fatalf("POST /runs status = %d, want 201; body=%s", got.status, got.body)
		}
		waitRunTerminal(t, srv, got.runID, engine.RunStatusCompleted)
	}

	traceA := readTraceForRunbook(t, runDir, "caller-a")
	traceB := readTraceForRunbook(t, runDir, "caller-b")
	if !strings.Contains(traceA, "BODY A EXECUTED") || strings.Contains(traceA, "BODY B EXECUTED") {
		t.Fatalf("caller A saw wrong catalog; trace:\n%s", traceA)
	}
	if !strings.Contains(traceB, "BODY B EXECUTED") || strings.Contains(traceB, "BODY A EXECUTED") {
		t.Fatalf("caller B saw wrong catalog; trace:\n%s", traceB)
	}
}

func postRun(t *testing.T, srv *Server, runbookPath string, inputs map[string]any) (string, int, string) {
	t.Helper()
	payload := map[string]any{"runbookPath": runbookPath, "inputs": inputs, "mode": "real", "actor": "test"}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/runs", bytes.NewReader(data))
	srv.handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusCreated {
		return "", rec.Code, body
	}
	var resp struct {
		RunID string `json:"runID"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode run response: %v; body=%s", err, body)
	}
	if resp.RunID == "" {
		t.Fatalf("empty runID in response: %s", body)
	}
	return resp.RunID, rec.Code, body
}

func waitRunTerminal(t *testing.T, srv *Server, runID string, want engine.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entry, ok := srv.registry.Get(runID)
		if ok && isTerminalState(entry.State) {
			if entry.State != want {
				t.Fatalf("run %s state = %s, want %s", runID, entry.State, want)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for run %s to reach %s", runID, want)
}

func readTrace(t *testing.T, runDir, runID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runDir, runID, "trace.jsonl"))
	if err != nil {
		t.Fatalf("read trace for %s: %v", runID, err)
	}
	return string(data)
}

func readTraceForRunbook(t *testing.T, runDir, runbookID string) string {
	t.Helper()
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", runDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			trace := readTrace(t, runDir, e.Name())
			if strings.Contains(trace, "\"runbook_id\":\""+runbookID+"\"") {
				return trace
			}
		}
	}
	t.Fatalf("no trace found for runbook %q", runbookID)
	return ""
}

func writeCatalogPackage(t *testing.T, pkgDir, pkgName, runbookID, body string) {
	t.Helper()
	writeCatalogTestFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), "apiVersion: yawr.tool-package/v1\nmeta:\n  name: "+pkgName+"\n  version: \"1.0.0\"\nexports:\n  runbooks:\n    - id: "+runbookID+"\n      path: runbooks/"+runbookID+".runbook.yaml\n")
	writeCatalogTestFile(t, filepath.Join(pkgDir, "runbooks", runbookID+".runbook.yaml"), "apiVersion: yawr.runbook/v1\nid: "+runbookID+"\nname: Child\nkind: composable\ninputs:\n  target:\n    type: string\n    required: false\n    from: context\n    default: \"(none)\"\nflow:\n  - step:\n      id: do_remediation\n      type: display\n      display:\n        format: text\n        content: \""+body+"\"\n  - step:\n      id: done\n      type: end\n      outcome:\n        category: success\n        code: remediated\n")
}

func writeCallerRunbook(t *testing.T, path, pkgName, version, ref, target string) {
	t.Helper()
	writeCatalogTestFile(t, path, "apiVersion: yawr.runbook/v1\nid: caller\nname: Caller\nkind: mitigation\nrequires:\n  - package: "+pkgName+"\n    version: \""+version+"\"\ninputs:\n  suggested_id:\n    type: string\n    required: false\n    from: context\n    default: \""+ref+"\"\nflow:\n  - step:\n      id: invoke_suggested\n      type: include\n      include:\n        runbook_ref: \"${suggested_id}\"\n        resolve_from: catalog\n        on_not_found: continue\n        with:\n          target: \""+target+"\"\n  - step:\n      id: done\n      type: end\n      outcome:\n        category: success\n        code: routed\n")
}

func writeCallerRunbookWithPath(t *testing.T, path, pkgName, version, pkgPath, ref string) {
	t.Helper()
	writeCallerRunbookWithPathAndTarget(t, path, pkgName, version, pkgPath, ref, "")
}

func writeCallerRunbookWithPathAndTarget(t *testing.T, path, pkgName, version, pkgPath, ref, target string) {
	t.Helper()
	id := strings.TrimSuffix(filepath.Base(path), ".runbook.yaml")
	withBlock := ""
	if target != "" {
		withBlock = "\n        with:\n          target: \"" + target + "\""
	}
	writeCatalogTestFile(t, path, "apiVersion: yawr.runbook/v1\nid: "+id+"\nname: Caller\nkind: mitigation\nrequires:\n  - package: "+pkgName+"\n    version: \""+version+"\"\n    path: "+pkgPath+"\nflow:\n  - step:\n      id: invoke_suggested\n      type: include\n      include:\n        runbook_ref: \""+ref+"\"\n        resolve_from: catalog\n        on_not_found: fail"+withBlock+"\n  - step:\n      id: done\n      type: end\n      outcome:\n        category: success\n        code: routed\n")
}

func writeCatalogTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
