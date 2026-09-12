package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

type publicResultsCounter struct{ count *atomic.Int32 }

func (c publicResultsCounter) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{Classification: "read-only", EndpointIdentity: "synthetic-public-counter", RenderedRequest: map[string]any{"counter": true}}); err != nil {
		return nil, err
	}
	n := c.count.Add(1)
	return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess, Vars: map[string]any{"count": int(n)}}, nil
}

func TestPublicResultsHTTPActiveReopenedNoRepeat(t *testing.T) {
	ctx := context.Background()
	dir := makeWorkDir(t)
	var counter atomic.Int32
	cfg, shutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	cfg.Executors.(*executor.MapRegistry).Register("cli", publicResultsCounter{&counter})
	native := map[string]any{"false": false, "zero": 0, "null": nil, "empty": []any{}, "unicode": "á😀", "large": strings.Repeat("😀", 20000)}
	plan := &engine.ExecutionPlan{RunID: "public-http", RunbookPath: filepath.Join(dir, "synthetic.yaml"),
		Outputs: map[string]*schema.Output{"result": {Type: "object", ValueTreePresent: true, ValueTree: map[string]any{"native": native, "count": "${count}"}}},
		Steps:   []engine.ResolvedStep{{ID: "produce", Kind: "cli", Spec: &schema.CLISpec{Command: "never-dispatched"}}, {ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}}}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	eng := internalengine.New(cfg)
	handle, err := eng.Start(ctx, plan, engine.RunOptions{Store: cfg.Store})
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(store engine.RunStore) *Server {
		srv, err := NewServer(servepkg.ServerConfig{Engine: eng, EngineConfig: engine.EngineConfig{Store: store}, Parser: &fakeParser{}, Planner: &fakePlanner{}, BearerToken: "synthetic-public-auth"})
		if err != nil {
			t.Fatal(err)
		}
		return srv
	}
	srv := newServer(cfg.Store)
	srv.registry.Add(&RunEntry{ID: handle.State().RunID, Handle: handle})
	server := newHTTPTestServer(t, srv)
	defer server.Close()
	get := func(url, token string) (int, map[string]json.RawMessage, []byte) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "run.get", "params": map[string]any{"runID": handle.State().RunID}})
		req, _ := http.NewRequest("POST", url+"/rpc", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Result map[string]json.RawMessage `json:"result"`
		}
		json.Unmarshal(raw, &envelope)
		return response.StatusCode, envelope.Result, raw
	}
	_, pending, _ := get(server.URL, "synthetic-public-auth")
	if string(pending["results"]) != "null" {
		t.Fatal("pending publication invented")
	}
	for {
		_, err = handle.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	record, err := handle.State().CloneResults()
	if err != nil || record == nil {
		t.Fatal("no committed root", err)
	}
	expected, _ := engine.CanonicalResultsJSON(record, true)
	for _, token := range []string{"", "wrong"} {
		status, _, raw := get(server.URL, token)
		if status != 401 && status != 403 {
			t.Fatal("authorization bypass")
		}
		if bytes.Contains(raw, []byte("publication_id")) || bytes.Contains(raw, []byte("large")) {
			t.Fatal("unauthorized content")
		}
	}
	_, active, _ := get(server.URL, "synthetic-public-auth")
	if !bytes.Equal(active["results"], expected) {
		t.Fatal("active canonical result differs")
	}
	reopened := runstore.NewDirRunStore(filepath.Join(dir, "runs"))
	second := newHTTPTestServer(t, newServer(reopened))
	defer second.Close()
	_, persisted, raw := get(second.URL, "synthetic-public-auth")
	if !bytes.Equal(persisted["results"], expected) {
		t.Fatalf("reopened canonical result differs: %.400s", raw)
	}
	if string(persisted["state"]) != `"completed"` || string(persisted["source"]) != `"persisted"` || counter.Load() != 1 {
		t.Fatal("reopen changed status or repeated producer")
	}
	if bytes.Contains(raw, []byte("TypedState")) || bytes.Contains(raw, []byte("binding_scope")) {
		t.Fatal("private state exposed")
	}
	if _, err := os.Stat(filepath.Join(dir, "runs")); err != nil {
		t.Fatal(err)
	}
}

func TestPublicResultsHTTPUnavailableProtection(t *testing.T) {
	h := newTestServerHarness(t)
	record := &engine.RunResults{SchemaVersion: engine.RunResultsSchemaV1, PublicationID: "pub", PlanSnapshotDigest: "sha256:" + strings.Repeat("a", 64), CheckpointSequence: 1,
		Origin: engine.ResultsOrigin{NodeID: "results", Invocation: 1}, Outputs: map[string]engine.NamedResultValue{"result": {Type: "string", Value: "synthetic-private"}}}
	if err := record.Seal(); err != nil {
		t.Fatal(err)
	}

	plan := &engine.ExecutionPlan{Inputs: map[string]*schema.Input{"credential": {Type: "secret"}}, Steps: []engine.ResolvedStep{{Kind: "results"}}}
	h.handle.state = engine.RunState{RunID: "run-1", Status: engine.RunStatusCompleted, Plan: plan, Results: record, Vars: map[string]any{"credential": "synthetic-private"}}
	h.server.registry.Add(&RunEntry{ID: "run-1", Handle: h.handle})
	response := doRPC(t, h.server, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "run.get", "params": map[string]any{"runID": "run-1"}})
	data, _ := json.Marshal(response)
	result := response.Result.(map[string]any)
	if result["results"] != nil || !bytes.Contains(data, []byte("protected-content")) || bytes.Contains(data, []byte("synthetic-private")) {
		t.Fatal("protected canonical data leaked")
	}
	h.handle.state.Status = engine.RunStatusFailed
	h.handle.state.Vars = nil
	response = doRPC(t, h.server, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "run.get", "params": map[string]any{"runID": "run-1"}})
	result = response.Result.(map[string]any)
	if result["state"] != "failed" || result["results"] != nil {
		t.Fatal("presentation masked execution failure")
	}
}

type unavailableProtectionStore struct {
	engine.DurableRunStore
	err error
}

func (s unavailableProtectionStore) LoadPlan(context.Context, string) (*engine.ExecutionPlan, error) {
	return nil, s.err
}

func TestPublicResultsUnpublishedProtectionParity(t *testing.T) {
	for _, protected := range []bool{true, false} {
		t.Run(fmt.Sprintf("protected=%v", protected), func(t *testing.T) {
			ctx := context.Background()
			dir := makeWorkDir(t)
			cfg, shutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
				Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer shutdown()
			inputType := "string"
			if protected {
				inputType = "secret"
			}
			const canary = "DAVID-SYNTHETIC-NOT-A-REAL-CREDENTIAL"
			plan := &engine.ExecutionPlan{RunID: "unpublished-parity", RunbookPath: filepath.Join(dir, "synthetic.yaml"),
				Inputs:  map[string]*schema.Input{"credential": {Type: inputType}},
				Outputs: map[string]*schema.Output{"result": {Type: "bool", ValueExpr: "credential"}},
				Steps:   []engine.ResolvedStep{{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
			}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			eng := internalengine.New(cfg)
			handle, err := eng.Start(ctx, plan, engine.RunOptions{Store: cfg.Store, RuntimeVars: map[string]any{"credential": canary}})
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Store.SaveState(ctx, handle.State()); err != nil {
				t.Fatal(err)
			}
			for _, failed := range []bool{false, true} {
				if failed {
					_, _ = handle.Next(ctx)
					if handle.State().Status != engine.RunStatusFailed {
						t.Fatal("expected failed publication")
					}
				}
				for _, mode := range []string{"active", "reopened", "missing", "corrupt", "nil-plan"} {
					t.Run(fmt.Sprintf("failed=%v/%s", failed, mode), func(t *testing.T) {
						var store engine.RunStore = runstore.NewDirRunStore(filepath.Join(dir, "runs"))
						missing := mode == "missing" || mode == "corrupt" || mode == "nil-plan"
						if missing {
							var loadErr error
							if mode == "missing" {
								loadErr = os.ErrNotExist
							}
							if mode == "corrupt" {
								loadErr = errors.New("invalid frozen protection")
							}
							store = unavailableProtectionStore{store.(engine.DurableRunStore), loadErr}
						}
						srv, err := NewServer(servepkg.ServerConfig{Engine: eng, EngineConfig: engine.EngineConfig{Store: store}, Parser: &fakeParser{}, Planner: &fakePlanner{}})
						if err != nil {
							t.Fatal(err)
						}
						if mode == "active" {
							srv.registry.Add(&RunEntry{ID: handle.State().RunID, Handle: handle})
						}
						reply := doRPC(t, srv, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "run.get", "params": map[string]any{"runID": handle.State().RunID}})
						result, ok := reply.Result.(map[string]any)
						if !ok {
							t.Fatalf("missing public response: %+v", reply)
						}
						raw, _ := json.Marshal(result)
						if result["results"] != nil || result["state"] != string(handle.State().Status) {
							t.Fatal("status/publication changed")
						}
						if protected || missing {
							if bytes.Contains(raw, []byte(canary)) || result["vars"] != nil || result["vars_unavailable"] == nil {
								t.Fatal("protected Vars not withheld")
							}
						} else if !bytes.Contains(raw, []byte(canary)) || result["vars_unavailable"] != nil {
							t.Fatal("unprotected Vars contract changed")
						}
						if missing && !bytes.Contains(raw, []byte("protection-unavailable")) {
							t.Fatal("missing explicit protection status")
						}
					})
				}
			}
		})
	}
}
