package tool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type boundTestRegistry struct{ lookups atomic.Int64 }

func (r *boundTestRegistry) Lookup(string) (*toolpkg.ToolDef, bool) {
	r.lookups.Add(1)
	return nil, false
}
func (*boundTestRegistry) All() []toolpkg.ToolDef { return nil }
func (*boundTestRegistry) Register(toolpkg.ToolDef) error {
	return fmt.Errorf("unexpected registry mutation")
}

type boundTestTransport struct {
	marker string
	calls  atomic.Int64
}

func (transport *boundTestTransport) Invoke(_ context.Context, definition toolpkg.ToolDef, _ string, _ map[string]any) (*toolpkg.ToolResult, error) {
	transport.calls.Add(1)
	if definition.Command != transport.marker {
		return nil, fmt.Errorf("wrong lexical definition: got %s, want %s", definition.Command, transport.marker)
	}
	return &toolpkg.ToolResult{Output: map[string]any{"marker": transport.marker}}, nil
}
func (*boundTestTransport) Close() error { return nil }

func boundRuntimeInvocation(t *testing.T, scope string, definition toolpkg.ToolDef) toolpkg.BoundInvocation {
	t.Helper()
	bound := toolpkg.BoundDefinition{Runtime: definition}
	id, err := toolpkg.DefinitionID(bound)
	if err != nil {
		t.Fatal(err)
	}
	return toolpkg.BoundInvocation{ScopeID: scope, LogicalName: "query", Action: "run",
		DefinitionID: id, BindingID: toolpkg.BindingID(scope, "query", id), Definition: bound}
}

func TestBoundRuntimeParallelAliasesNeverConsultGlobalRegistry(t *testing.T) {
	registry := &boundTestRegistry{}
	runtime := NewDefaultToolRuntime(registry)
	t.Cleanup(func() { _ = runtime.Close() })
	invocations := []toolpkg.BoundInvocation{}
	transports := []*boundTestTransport{}
	for _, marker := range []string{"left", "right"} {
		invocation := boundRuntimeInvocation(t, marker, toolpkg.ToolDef{Name: "query", Command: marker,
			Transport: toolpkg.TransportJSONRPC, Actions: map[string]*toolpkg.ToolAction{"run": {}}})
		transport := &boundTestTransport{marker: marker}
		runtime.persistent[invocation.DefinitionID] = transport
		invocations = append(invocations, invocation)
		transports = append(transports, transport)
	}
	var workers sync.WaitGroup
	failures := make(chan error, 200)
	for repeat := 0; repeat < 100; repeat++ {
		for _, invocation := range invocations {
			workers.Add(1)
			go func(invocation toolpkg.BoundInvocation) {
				defer workers.Done()
				result, err := runtime.InvokeBound(context.Background(), invocation, nil)
				if err != nil {
					failures <- err
				} else if result == nil || result.Output["marker"] != invocation.ScopeID {
					failures <- fmt.Errorf("incorrect result for scope %s: %+v", invocation.ScopeID, result)
				}
			}(invocation)
		}
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	for _, transport := range transports {
		if transport.calls.Load() != 100 {
			t.Errorf("%s dispatched %d times, want 100", transport.marker, transport.calls.Load())
		}
	}
	if registry.lookups.Load() != 0 {
		t.Fatalf("bound invocation consulted global registry %d times", registry.lookups.Load())
	}
}

func TestBoundRuntimeRejectsTamperingBeforeTransport(t *testing.T) {
	registry := &boundTestRegistry{}
	runtime := NewDefaultToolRuntime(registry)
	invocation := boundRuntimeInvocation(t, "scope", toolpkg.ToolDef{Name: "query", Command: "fixture",
		Transport: toolpkg.TransportJSONRPC, Actions: map[string]*toolpkg.ToolAction{"run": {}}})
	transport := &boundTestTransport{marker: "fixture"}
	runtime.persistent[invocation.DefinitionID] = transport
	invocation.Definition.Runtime.Command = "changed"
	if _, err := runtime.InvokeBound(context.Background(), invocation, nil); err == nil {
		t.Fatal("tampered definition was dispatched")
	}
	if transport.calls.Load() != 0 || registry.lookups.Load() != 0 {
		t.Fatal("identity refusal occurred after transport or registry access")
	}
}

func TestBoundRuntimeUsesFrozenEndpointNotLegacyProfile(t *testing.T) {
	var frozenHits, otherHits atomic.Int64
	server := &fakeMCPServer{}
	frozen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		frozenHits.Add(1)
		server.ServeHTTP(w, request)
	}))
	defer frozen.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		otherHits.Add(1)
		http.Error(w, "must not reach changed profile endpoint", http.StatusInternalServerError)
	}))
	defer other.Close()
	runtime := NewDefaultToolRuntime(nil)
	defer runtime.Close()
	runtime.SetProfile(&schema.RuntimeProfile{Tools: map[string]*schema.ProfileToolOverride{"query": {Endpoint: other.URL}}})
	invocation := boundRuntimeInvocation(t, "scope", toolpkg.ToolDef{Name: "query",
		Transport: toolpkg.TransportMCPHTTP, URL: frozen.URL, Actions: map[string]*toolpkg.ToolAction{"run": {}}})
	result, err := runtime.InvokeBound(context.Background(), invocation, nil)
	if err != nil || result == nil {
		t.Fatalf("bound dispatch failed: %v", err)
	}
	if frozenHits.Load() == 0 || otherHits.Load() != 0 {
		t.Fatalf("bound endpoint changed: frozen=%d other=%d", frozenHits.Load(), otherHits.Load())
	}
}

func TestBoundRuntimeRefusesSubstitutionTransportFallback(t *testing.T) {
	runtime := NewDefaultToolRuntime(nil)
	invocation := boundRuntimeInvocation(t, "scope", toolpkg.ToolDef{Name: "query", Command: "fixture",
		Transport: toolpkg.TransportJSONRPC, Actions: map[string]*toolpkg.ToolAction{
			"run": {Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.runbook.yaml"}},
		}})
	transport := &boundTestTransport{marker: "fixture"}
	runtime.persistent[invocation.DefinitionID] = transport
	if _, err := runtime.InvokeBound(context.Background(), invocation, nil); !errors.Is(err, errkit.ErrSCOPE002) {
		t.Fatalf("substitution dispatched as a process-backed action: %v", err)
	}
	if transport.calls.Load() != 0 {
		t.Fatal("substitution refusal occurred after dispatch")
	}
}

func TestBoundRuntimeCancellationPreventsDispatch(t *testing.T) {
	runtime := NewDefaultToolRuntime(nil)
	invocation := boundRuntimeInvocation(t, "scope", toolpkg.ToolDef{Name: "query", Command: "fixture",
		Transport: toolpkg.TransportJSONRPC, Actions: map[string]*toolpkg.ToolAction{"run": {}}})
	transport := &boundTestTransport{marker: "fixture"}
	runtime.persistent[invocation.DefinitionID] = transport
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.InvokeBound(ctx, invocation, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bound invocation returned %v", err)
	}
	if transport.calls.Load() != 0 {
		t.Fatal("canceled invocation reached transport")
	}
}
