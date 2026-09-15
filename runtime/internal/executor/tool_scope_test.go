package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

type scopedExecutorRuntime struct {
	legacy  atomic.Int64
	lookups atomic.Int64
	bound   atomic.Int64
}

func (runtime *scopedExecutorRuntime) Invoke(context.Context, string, string, map[string]any) (*tool.ToolResult, error) {
	runtime.legacy.Add(1)
	return nil, errors.New("name-only dispatch must not run")
}

func (runtime *scopedExecutorRuntime) LookupDef(string) (*tool.ToolDef, bool) {
	runtime.lookups.Add(1)
	return nil, false
}

func (runtime *scopedExecutorRuntime) InvokeBound(_ context.Context, invocation tool.BoundInvocation, args map[string]any) (*tool.ToolResult, error) {
	runtime.bound.Add(1)
	if err := tool.ValidateBoundInvocation(invocation); err != nil {
		return nil, err
	}
	marker := invocation.Definition.Runtime.Command
	if args["marker"] != marker {
		return nil, fmt.Errorf("wrong scope defaults: %v, want %s", args, marker)
	}
	return &tool.ToolResult{Output: map[string]any{"marker": marker}}, nil
}

type legacyOnlyScopeRuntime struct{ calls int }

func (runtime *legacyOnlyScopeRuntime) Invoke(context.Context, string, string, map[string]any) (*tool.ToolResult, error) {
	runtime.calls++
	return &tool.ToolResult{}, nil
}

func scopedExecutorFixture(t *testing.T) (*toolscope.Set, []engine.ResolvedStep) {
	t.Helper()
	snapshot := toolscope.Snapshot{Version: toolscope.Version, CatalogDigest: "catalog-fixture",
		ProfileDigest: toolscope.ProfileDigest(nil), Documents: map[string]toolscope.Document{},
		Scopes: map[string]toolscope.Scope{}, Bindings: map[string]toolscope.Binding{},
		Definitions: map[string]tool.BoundDefinition{}}
	var steps []engine.ResolvedStep
	for _, marker := range []string{"left", "right"} {
		document := toolscope.Document{SourceIdentity: marker + ".runbook.yaml", SourceDigest: marker, PackageIdentity: "workspace"}
		documentID := toolscope.DocumentID(document)
		scopeID := toolscope.ScopeID(documentID, snapshot.CatalogDigest, snapshot.ProfileDigest)
		document.ScopeID = scopeID
		authored := &schema.ToolAction{Args: map[string]*schema.ArgDef{"marker": {Type: "string", Default: marker}},
			Outputs: map[string]*schema.ArgDef{"marker": {Type: "string"}}}
		definition := tool.BoundDefinition{
			Runtime: tool.ToolDef{Name: marker + "-implementation", Command: marker, Transport: tool.TransportNative,
				Actions: map[string]*tool.ToolAction{"run": (&tool.ToolAction{
					Args: map[string]*tool.ArgDef{"marker": {Type: "string", Default: marker}}, Outputs: authored.Outputs,
				}).WithSchemaAction(authored)}},
			Declaration: &schema.ToolDef{Name: marker + "-implementation", Actions: map[string]*schema.ToolAction{"run": authored}},
		}
		definitionID, err := tool.DefinitionID(definition)
		if err != nil {
			t.Fatal(err)
		}
		bindingID := tool.BindingID(scopeID, "query", definitionID)
		snapshot.Documents[documentID] = document
		snapshot.Scopes[scopeID] = toolscope.Scope{DocumentID: documentID, Bindings: map[string]string{"query": bindingID}}
		snapshot.Bindings[bindingID] = toolscope.Binding{ScopeID: scopeID, LogicalName: "query", DefinitionID: definitionID}
		snapshot.Definitions[definitionID] = definition
		steps = append(steps, engine.ResolvedStep{ID: marker, Kind: "tool", LexicalScopeID: scopeID, ToolBindingID: bindingID,
			Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "query", Action: "run"}}})
	}
	set, err := toolscope.New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return set, steps
}

func TestScopedExecutorParallelDefaultsAndContractsUseOneBinding(t *testing.T) {
	scopes, steps := scopedExecutorFixture(t)
	runtime := &scopedExecutorRuntime{}
	executor := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
	ctx := engine.WithToolScopes(context.Background(), scopes)
	var workers sync.WaitGroup
	failures := make(chan error, 200)
	for repeat := 0; repeat < 100; repeat++ {
		for _, step := range steps {
			workers.Add(1)
			go func(step engine.ResolvedStep) {
				defer workers.Done()
				result, err := executor.Execute(ctx, step, nil)
				if err != nil {
					failures <- err
				} else if result.Status != engine.StepStatusCompleted || result.Output["marker"] != step.ID {
					failures <- fmt.Errorf("%s received another scope's result: %+v", step.ID, result)
				}
			}(step)
		}
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if runtime.bound.Load() != 200 || runtime.legacy.Load() != 0 || runtime.lookups.Load() != 0 {
		t.Fatalf("dispatch counts: bound=%d legacy=%d lookup=%d", runtime.bound.Load(), runtime.legacy.Load(), runtime.lookups.Load())
	}
}

func TestScopedExecutorMissingReferencesNeverFallBack(t *testing.T) {
	for _, mutation := range []string{"context", "scope", "binding", "cross-scope"} {
		t.Run(mutation, func(t *testing.T) {
			scopes, steps := scopedExecutorFixture(t)
			runtime := &scopedExecutorRuntime{}
			executor := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
			ctx := engine.WithToolScopes(context.Background(), scopes)
			step := steps[0]
			switch mutation {
			case "context":
				ctx = context.Background()
			case "scope":
				step.LexicalScopeID = ""
			case "binding":
				step.ToolBindingID = ""
			case "cross-scope":
				step.ToolBindingID = steps[1].ToolBindingID
			}

			if _, err := executor.Execute(ctx, step, nil); !errors.Is(err, errkit.ErrSCOPE002) {
				t.Fatalf("bad binding was not rejected: %v", err)
			}
			if runtime.bound.Load() != 0 || runtime.legacy.Load() != 0 || runtime.lookups.Load() != 0 {
				t.Fatal("invalid binding reached a runtime")
			}
		})
	}
}

func TestScopedPresentationCaptureUsesExactInvocationWithoutOverwritingParent(t *testing.T) {
	scopes, steps := scopedExecutorFixture(t)
	runtime := &scopedExecutorRuntime{}
	executor := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
	parent := engine.WithToolPresentationCapture(engine.WithToolScopes(context.Background(), scopes), "snapshot")
	if _, err := executor.Execute(parent, steps[0], nil); err != nil {
		t.Fatal(err)
	}
	child := engine.WithToolPresentationCapture(parent, "snapshot")
	if _, err := executor.Execute(child, steps[1], nil); err != nil {
		t.Fatal(err)
	}
	for index, ctx := range []context.Context{parent, child} {
		capture := engine.ToolPresentationFromContext(ctx)
		invocation, err := scopes.Resolve(steps[index].LexicalScopeID, "query", "run")
		if err != nil {
			t.Fatal(err)
		}
		if capture.ScopeID != invocation.ScopeID || capture.BindingID != invocation.BindingID ||
			capture.DefinitionID != invocation.DefinitionID || capture.ToolID != "query" ||
			capture.Definition.Name != invocation.Definition.Declaration.Name {
			t.Fatalf("wrong presentation owner: %+v", capture)
		}
	}
}

func TestScopedExecutorRejectsNameOnlySDKRuntime(t *testing.T) {
	scopes, steps := scopedExecutorFixture(t)
	runtime := &legacyOnlyScopeRuntime{}
	executor := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
	if _, err := executor.Execute(engine.WithToolScopes(context.Background(), scopes), steps[0], nil); !errors.Is(err, errkit.ErrSCOPE002) {
		t.Fatalf("unsupported SDK runtime was not rejected: %v", err)
	}
	if runtime.calls != 0 {
		t.Fatal("scoped invocation fell back to name-only SDK API")
	}
}

func TestScopedExecutorSavedResultUsesFrozenContract(t *testing.T) {
	scopes, steps := scopedExecutorFixture(t)
	runtime := &scopedExecutorRuntime{}
	executor := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
	ctx := engine.WithToolScopes(context.Background(), scopes)
	result, err := executor.ValidateSavedResult(ctx, steps[0], nil, &engine.StepResult{
		StepID: steps[0].ID, Status: engine.StepStatusCompleted, Output: map[string]any{"marker": 42},
	})
	if err != nil || result == nil || result.Status != engine.StepStatusFailed || result.Error == nil {
		t.Fatalf("saved result bypassed frozen output contract: %+v %v", result, err)
	}
	if runtime.bound.Load() != 0 || runtime.legacy.Load() != 0 || runtime.lookups.Load() != 0 {
		t.Fatal("saved result validation consulted or invoked runtime")
	}
	pure, err := executor.IsPureRunbookSubstitution(ctx, steps[0], nil)
	if err != nil || pure || runtime.lookups.Load() != 0 {
		t.Fatalf("classification did not use bound definition: pure=%v err=%v lookup=%d", pure, err, runtime.lookups.Load())
	}
}
