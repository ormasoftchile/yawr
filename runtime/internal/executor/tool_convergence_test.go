package executor

import (
	"context"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func stringPtr(s string) *string {
	return &s
}

func TestToolExecutionConvergence_Parity(t *testing.T) {
	// Register a shared native tool in registry
	toolDef := toolpkg.ToolDef{
		Name:      "parity-tool",
		Transport: toolpkg.TransportNative,
		Command:   "echo",
		Actions: map[string]*toolpkg.ToolAction{
			"say": {
				Classification: stringPtr("read-only"),
				Description:    "Echo a message",
				Argv:           []string{"--message", "PARITY: ${text}"},
				Args: map[string]*toolpkg.ArgDef{
					"text": {Type: "string", Required: true},
				},
			},
		},
	}

	registry := internaltool.NewMapRegistry([]toolpkg.ToolDef{toolDef})
	rt := internaltool.NewDefaultToolRuntime(registry)

	// Create shared ToolInvoker
	invoker := internaltool.NewInvoker(internaltool.InvokerConfig{
		Registry: registry,
		Runtime:  rt,
	})

	// Create ToolExecutor equipped with the shared invoker
	executor := NewToolExecutor(rt, &internalexpr.TemplateEvaluator{})
	executor.SetInvoker(invoker)

	ctx := context.Background()

	// 1. Execute via Workflow ToolExecutor
	wfStep := engine.ResolvedStep{
		ID:   "step-parity",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{
			Tool: schema.ToolInvocation{
				Name:   "parity-tool",
				Action: "say",
				Args: map[string]any{
					"text": "hello from workflow",
				},
			},
		},
	}

	wfResult, err := executor.Execute(ctx, wfStep, map[string]any{})
	if err != nil {
		t.Fatalf("workflow execution failed: %v", err)
	}
	if wfResult.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed status, got %s", wfResult.Status)
	}
	wfStdout, _ := wfResult.Output["stdout"].(string)

	// 2. Execute via Ad-Hoc ToolInvoker
	adHocReq := toolpkg.InvocationRequest{
		Tool:   "parity-tool",
		Action: "say",
		Arguments: map[string]any{
			"text": "hello from ad-hoc",
		},
		Authority: toolpkg.InvocationAuthority{
			Actor:     "ad-hoc-caller",
			AllowRead: true,
		},
	}

	adHocResult, err := invoker.Invoke(ctx, adHocReq)
	if err != nil {
		t.Fatalf("ad-hoc invocation failed: %v", err)
	}
	if adHocResult.Status != toolpkg.InvocationStatusCompleted {
		t.Fatalf("expected completed status, got %s", adHocResult.Status)
	}

	// 3. Verify parity of execution
	if !strings.Contains(wfStdout, "PARITY: hello from workflow") {
		t.Errorf("workflow output mismatch: %q", wfStdout)
	}
	if !strings.Contains(adHocResult.Stdout, "PARITY: hello from ad-hoc") {
		t.Errorf("ad-hoc output mismatch: %q", adHocResult.Stdout)
	}
	if adHocResult.Classification != "read-only" {
		t.Errorf("ad-hoc classification mismatch: %s", adHocResult.Classification)
	}
}
