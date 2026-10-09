package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/run"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type hostApprovalGate struct {
	invoked bool
}

func (g *hostApprovalGate) RequestApproval(ctx context.Context, stepID string, reason string) (governance.ApprovalRecord, error) {
	g.invoked = true
	fmt.Printf("  [ApprovalGate] Host Gate Prompt: Step %q requires approval (Reason: %s)\n", stepID, reason)
	return governance.ApprovalRecord{
		Approver:   "operator@host.local",
		ApprovedAt: time.Now().UTC().Format(time.RFC3339),
		Token:      "tok-host-approved-42",
	}, nil
}

func stringPtr(s string) *string {
	return &s
}

func boolPtr(b bool) *bool {
	return &b
}

func main() {
	if err := runEmbeddedExample(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runEmbeddedExample() error {
	ctx := context.Background()
	stateDir, err := os.MkdirTemp("", "yawr-embedded-invoker-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(stateDir)

	dispatchDir := filepath.Join(stateDir, "dispatches")
	fmt.Println("=== YAWR Embedded Tool Invoker Example ===")
	fmt.Printf("Durable Dispatch Directory: %s\n\n", dispatchDir)

	// 1. Construct Registry and Define Real Tools via Public Types
	readTool := tool.ToolDef{
		Name:      "system-info",
		Transport: tool.TransportNative,
		Command:   "echo",
		Actions: map[string]*tool.ToolAction{
			"version": {
				Classification: stringPtr("read-only"),
				Description:    "Print system version",
				Argv:           []string{"--message", "YAWR Embedded SDK v1.0.0 (${target})"},
				Args: map[string]*tool.ArgDef{
					"target": {Type: "string", Required: true},
				},
			},
		},
	}

	mutatingTool := tool.ToolDef{
		Name:      "cluster-deployer",
		Transport: tool.TransportNative,
		Command:   "echo",
		Governance: &schema.ToolGovernance{
			RequiresApproval: boolPtr(true),
		},
		Actions: map[string]*tool.ToolAction{
			"apply": {
				Classification: stringPtr("mutating"),
				Description:    "Deploy cluster revision",
				Argv:           []string{"--message", "Deployed revision: ${rev}"},
				Args: map[string]*tool.ArgDef{
					"rev": {Type: "string", Required: true},
				},
			},
		},
	}

	reg := tool.NewRegistry(readTool, mutatingTool)
	gate := &hostApprovalGate{}

	// 2. Construct Invoker via Public SDK (pkg/run and pkg/tool)
	// ZERO imports of YAWR internal packages!
	invoker, err := run.NewToolInvoker(tool.InvokerOptions{
		Registry:     reg,
		StateDir:     dispatchDir,
		ApprovalGate: gate,
	})
	if err != nil {
		return fmt.Errorf("failed to construct invoker: %w", err)
	}

	// 3. Executing Read-Only Operation
	fmt.Println("--- 1. Executing Read-Only Operation ---")
	readReq := tool.InvocationRequest{
		Tool:   "system-info",
		Action: "version",
		Arguments: map[string]any{
			"target": "darwin-arm64",
		},
		Authority: tool.InvocationAuthority{
			Actor:     "a4c:github-copilot",
			AllowRead: true,
		},
	}
	readRes, err := invoker.Invoke(ctx, readReq)
	if err != nil {
		return fmt.Errorf("read-only invoke go error: %w", err)
	}
	if readRes.Status != tool.InvocationStatusCompleted {
		return fmt.Errorf("read-only invocation did not complete: status=%s, err=%s", readRes.Status, readRes.ErrorMessage)
	}
	fmt.Printf("Read-only succeeded: stdout=%q\n\n", readRes.Stdout)

	// 4. Unauthorized Mutating Call (Denied without execution)
	fmt.Println("--- 2. Attempting Unauthorized Mutating Call ---")
	unauthReq := tool.InvocationRequest{
		Tool:   "cluster-deployer",
		Action: "apply",
		Arguments: map[string]any{
			"rev": "v2.1.0",
		},
		Authority: tool.InvocationAuthority{
			Actor:         "untrusted-agent",
			AllowRead:     true,
			AllowMutating: false, // caller denied mutation capability
		},
	}
	unauthRes, err := invoker.Invoke(ctx, unauthReq)
	if err != nil {
		return fmt.Errorf("unauth invoke go error: %w", err)
	}
	if unauthRes.Status != tool.InvocationStatusDenied {
		return fmt.Errorf("expected denial, got status=%s", unauthRes.Status)
	}
	fmt.Printf("Unauthorized call correctly denied: status=%s, error=%q\n\n", unauthRes.Status, unauthRes.ErrorMessage)

	// 5. Governed Mutating Call with Host Approval Gate
	fmt.Println("--- 3. Executing Governed Mutating Call (Approval Gate) ---")
	mutReq := tool.InvocationRequest{
		Tool:   "cluster-deployer",
		Action: "apply",
		Arguments: map[string]any{
			"rev": "v2.1.0",
		},
		IdempotencyKey: "deploy-rev-210",
		Authority: tool.InvocationAuthority{
			Actor:         "authorized-operator",
			AllowMutating: true,
			ApprovalGate:  gate,
		},
	}
	mutRes, err := invoker.Invoke(ctx, mutReq)
	if err != nil {
		return fmt.Errorf("mutating invoke go error: %w", err)
	}
	if mutRes.Status != tool.InvocationStatusCompleted {
		return fmt.Errorf("mutating invocation failed: status=%s, err=%s", mutRes.Status, mutRes.ErrorMessage)
	}
	if !gate.invoked {
		return errors.New("expected host approval gate to be invoked")
	}
	if mutRes.ApprovalRecord == nil || mutRes.ApprovalRecord.Token != "tok-host-approved-42" {
		return fmt.Errorf("expected approval record in result, got: %+v", mutRes.ApprovalRecord)
	}
	fmt.Printf("Governed mutation completed: stdout=%q, approvalToken=%s\n\n", mutRes.Stdout, mutRes.ApprovalRecord.Token)

	// 6. Idempotent Replay (Identical Key and Request)
	fmt.Println("--- 4. Replaying Same Request with Idempotency Key ---")
	replayRes, err := invoker.Invoke(ctx, mutReq)
	if err != nil {
		return fmt.Errorf("replay invoke go error: %w", err)
	}
	if replayRes.Status != tool.InvocationStatusCompleted {
		return fmt.Errorf("replay did not complete: status=%s", replayRes.Status)
	}
	if !replayRes.Replayed {
		return errors.New("expected Replayed == true on identical retry")
	}
	fmt.Printf("Replay succeeded without re-execution: replayed=%v, occurrenceID=%s\n\n", replayRes.Replayed, replayRes.OccurrenceID)

	// 7. Cross-Instance Crash Recovery and Idempotency Conflict Detection
	fmt.Println("--- 5. Verifying Cross-Instance State Sharing & Conflict Detection ---")
	invoker2, err := run.NewToolInvoker(tool.InvokerOptions{
		Registry:     reg,
		StateDir:     dispatchDir,
		ApprovalGate: gate,
	})
	if err != nil {
		return fmt.Errorf("failed to construct second invoker instance: %w", err)
	}

	// Replay from new process/instance returns settled outcome
	restartedReplay, err := invoker2.Invoke(ctx, mutReq)
	if err != nil {
		return fmt.Errorf("restarted replay error: %w", err)
	}
	if !restartedReplay.Replayed {
		return errors.New("expected second invoker instance to return replayed=true from persistent journal")
	}
	fmt.Printf("New invoker instance replayed settled dispatch from disk: occurrenceID=%s\n", restartedReplay.OccurrenceID)

	// Reusing same idempotency key with conflicting arguments MUST fail
	conflictReq := tool.InvocationRequest{
		Tool:   "cluster-deployer",
		Action: "apply",
		Arguments: map[string]any{
			"rev": "v9.9.9", // Different arguments!
		},
		IdempotencyKey: "deploy-rev-210", // Same key!
		Authority: tool.InvocationAuthority{
			Actor:         "authorized-operator",
			AllowMutating: true,
		},
	}
	conflictRes, err := invoker2.Invoke(ctx, conflictReq)
	if err != nil {
		return fmt.Errorf("conflict invoke go error: %w", err)
	}
	if conflictRes.Status != tool.InvocationStatusFailed {
		return fmt.Errorf("expected failed status on idempotency conflict, got %s", conflictRes.Status)
	}
	if !strings.Contains(conflictRes.ErrorMessage, "idempotency key") || !strings.Contains(conflictRes.ErrorMessage, "different request") {
		return fmt.Errorf("expected ErrIdempotencyConflict, got: %s", conflictRes.ErrorMessage)
	}
	fmt.Printf("Conflicting key reuse correctly rejected: %s\n\n", conflictRes.ErrorMessage)

	fmt.Println("=== All 7 Lifecycle Steps Succeeded via Public SDK ===")
	return nil
}
