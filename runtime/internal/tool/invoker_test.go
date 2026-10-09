package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pkgGov "github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// fakeApprovalGate provides a controllable ApprovalGate for testing.
type fakeApprovalGate struct {
	shouldApprove bool
	token         string
	err           error
	calls         []string
}

func (g *fakeApprovalGate) RequestApproval(ctx context.Context, stepID string, reason string) (pkgGov.ApprovalRecord, error) {
	g.calls = append(g.calls, stepID)
	if !g.shouldApprove {
		if g.err != nil {
			return pkgGov.ApprovalRecord{}, g.err
		}
		return pkgGov.ApprovalRecord{}, errors.New("approval rejected by reviewer")
	}
	return pkgGov.ApprovalRecord{
		Approver:   "operator@example.com",
		ApprovedAt: time.Now().UTC().Format(time.RFC3339),
		Token:      g.token,
	}, nil
}

func stringPtr(s string) *string {
	return &s
}

func boolPtr(b bool) *bool {
	return &b
}

func setupTestInvoker(defs ...toolpkg.ToolDef) (toolpkg.ToolInvoker, *MapRegistry, *InMemoryDispatchStore) {
	registry := NewMapRegistry(defs)
	rt := NewDefaultToolRuntime(registry)
	store := NewInMemoryDispatchStore()
	inv := NewInvoker(InvokerConfig{
		Registry:      registry,
		Runtime:       rt,
		DispatchStore: store,
	})
	return inv, registry, store
}

// Scenario A: Read-only ad-hoc invocation
func TestInvoker_ScenarioA_ReadOnly(t *testing.T) {
	readTool := toolpkg.ToolDef{
		Name:      "cat-reader",
		Transport: toolpkg.TransportNative,
		Command:   "echo",
		Actions: map[string]*toolpkg.ToolAction{
			"read": {
				Classification: stringPtr("read-only"),
				Description:    "Read file contents",
				Argv:           []string{"--message", "Reading: ${path}"},
				Args: map[string]*toolpkg.ArgDef{
					"path": {
						Type:     "string",
						Required: true,
					},
				},
			},
		},
	}

	inv, _, _ := setupTestInvoker(readTool)
	ctx := context.Background()

	t.Run("Valid read-only invocation succeeds with evidence", func(t *testing.T) {
		req := toolpkg.InvocationRequest{
			Tool:   "cat-reader",
			Action: "read",
			Arguments: map[string]any{
				"path": "/etc/hosts",
			},
			Authority: toolpkg.InvocationAuthority{
				Actor:     "a4c:copilot",
				AllowRead: true,
			},
		}

		res, err := inv.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("unexpected invocation error: %v", err)
		}
		if res.Status != toolpkg.InvocationStatusCompleted {
			t.Fatalf("expected completed status, got %s (error: %s)", res.Status, res.ErrorMessage)
		}
		if res.Classification != "read-only" {
			t.Errorf("expected classification read-only, got %s", res.Classification)
		}
		if !strings.Contains(res.Stdout, "Reading: /etc/hosts") {
			t.Errorf("stdout does not contain expected output: %q", res.Stdout)
		}
		if len(res.Evidence) == 0 {
			t.Errorf("expected evidence records to be captured, got none")
		}
	})

	t.Run("Argument validation failure rejects before execution", func(t *testing.T) {
		req := toolpkg.InvocationRequest{
			Tool:   "cat-reader",
			Action: "read",
			Arguments: map[string]any{
				// Missing required "path" argument
			},
			Authority: toolpkg.InvocationAuthority{
				Actor:     "a4c:copilot",
				AllowRead: true,
			},
		}

		res, err := inv.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("unexpected go error: %v", err)
		}
		if res.Status != toolpkg.InvocationStatusFailed {
			t.Fatalf("expected failed status, got %s", res.Status)
		}
		if !strings.Contains(res.ErrorMessage, "required argument") {
			t.Errorf("expected required argument error, got: %s", res.ErrorMessage)
		}
	})
}

// Scenario B: Governed effectful invocation (approval gates & scope restriction)
func TestInvoker_ScenarioB_GovernedEffectful(t *testing.T) {
	mutatingTool := toolpkg.ToolDef{
		Name:      "git-patch",
		Transport: toolpkg.TransportNative,
		Command:   "echo",
		Governance: &schema.ToolGovernance{
			RequiresApproval: boolPtr(true),
		},
		Actions: map[string]*toolpkg.ToolAction{
			"apply": {
				Classification: stringPtr("mutating"),
				Description:    "Apply a git patch",
				Argv:           []string{"--message", "Applied patch: ${patch}"},
				Args: map[string]*toolpkg.ArgDef{
					"patch": {
						Type:     "string",
						Required: true,
					},
				},
			},
		},
	}

	inv, _, _ := setupTestInvoker(mutatingTool)
	ctx := context.Background()

	t.Run("Attended mode with approval granted proceeds to execution", func(t *testing.T) {
		gate := &fakeApprovalGate{
			shouldApprove: true,
			token:         "tok-appr-999",
		}

		req := toolpkg.InvocationRequest{
			Tool:   "git-patch",
			Action: "apply",
			Arguments: map[string]any{
				"patch": "diff --git a/foo b/foo",
			},
			Authority: toolpkg.InvocationAuthority{
				Actor:         "a4c:opencode",
				Attendance:    schema.ProfileAttendanceAttended,
				AllowMutating: true,
				ApprovalGate:  gate,
			},
		}

		res, err := inv.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != toolpkg.InvocationStatusCompleted {
			t.Fatalf("expected status completed, got %s: %s", res.Status, res.ErrorMessage)
		}
		if res.ApprovalRecord == nil || res.ApprovalRecord.Token != "tok-appr-999" {
			t.Errorf("expected approval record with token tok-appr-999, got %+v", res.ApprovalRecord)
		}
		if !strings.Contains(res.Stdout, "Applied patch:") {
			t.Errorf("expected tool output in stdout, got %q", res.Stdout)
		}
		if res.OccurrenceID == "" {
			t.Errorf("expected occurrence ID to be populated")
		}

		// Verify evidence contains approval
		hasApprovalEvidence := false
		for _, ev := range res.Evidence {
			if ev.Name == "approval_record" && ev.Value == "tok-appr-999" {
				hasApprovalEvidence = true
			}
		}
		if !hasApprovalEvidence {
			t.Errorf("expected approval record evidence, got %+v", res.Evidence)
		}
	})

	t.Run("Attended mode with approval denied blocks execution", func(t *testing.T) {
		gate := &fakeApprovalGate{
			shouldApprove: false,
			err:           errors.New("denied by security reviewer"),
		}

		req := toolpkg.InvocationRequest{
			Tool:   "git-patch",
			Action: "apply",
			Arguments: map[string]any{
				"patch": "malicious patch",
			},
			Authority: toolpkg.InvocationAuthority{
				Actor:         "a4c:opencode",
				Attendance:    schema.ProfileAttendanceAttended,
				AllowMutating: true,
				ApprovalGate:  gate,
			},
		}

		res, err := inv.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != toolpkg.InvocationStatusDenied {
			t.Fatalf("expected status denied, got %s", res.Status)
		}
		if !strings.Contains(res.ErrorMessage, "denied by security reviewer") {
			t.Errorf("expected denial message, got %s", res.ErrorMessage)
		}
		if res.Stdout != "" {
			t.Errorf("tool executed despite denial! stdout: %s", res.Stdout)
		}
	})

	t.Run("Unattended mode without mutating authorization is denied by policy", func(t *testing.T) {
		req := toolpkg.InvocationRequest{
			Tool:   "git-patch",
			Action: "apply",
			Arguments: map[string]any{
				"patch": "diff --git",
			},
			Authority: toolpkg.InvocationAuthority{
				Actor:         "a4c:agent-unattended",
				Attendance:    schema.ProfileAttendanceUnattended,
				AllowRead:     true,
				AllowMutating: false, // Mutating forbidden in unattended scope
			},
		}

		res, err := inv.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != toolpkg.InvocationStatusDenied {
			t.Fatalf("expected status denied, got %s", res.Status)
		}
		if !strings.Contains(res.ErrorMessage, "allow_mutating=false") {
			t.Errorf("expected deny message, got %s", res.ErrorMessage)
		}
	})
}

// Scenario C: Recovery, Idempotency, and Indeterminate Outcomes
func TestInvoker_ScenarioC_RecoveryAndIdempotency(t *testing.T) {
	mutTool := toolpkg.ToolDef{
		Name:      "deploy-apply",
		Transport: toolpkg.TransportNative,
		Command:   "echo",
		Actions: map[string]*toolpkg.ToolAction{
			"apply": {
				Classification: stringPtr("mutating"),
				Description:    "Deploy resources",
				Argv:           []string{"--message", "Deployed: ${id}"},
				Args: map[string]*toolpkg.ArgDef{
					"id": {Type: "string", Required: true},
				},
			},
		},
	}

	t.Run("Idempotent replay returns cached settlement without duplicate execution", func(t *testing.T) {
		inv, _, store := setupTestInvoker(mutTool)
		ctx := context.Background()

		req := toolpkg.InvocationRequest{
			Tool:           "deploy-apply",
			Action:         "apply",
			IdempotencyKey: "deploy-cluster-001",
			Arguments: map[string]any{
				"id": "cluster-001",
			},
			Authority: toolpkg.InvocationAuthority{
				Actor:         "operator",
				AllowMutating: true,
			},
		}

		// First call: executes and settles
		res1, err := inv.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("first invoke error: %v", err)
		}
		if res1.Status != toolpkg.InvocationStatusCompleted {
			t.Fatalf("first status: %s", res1.Status)
		}
		occID := res1.OccurrenceID
		if occID == "" {
			t.Fatalf("expected non-empty occurrence ID")
		}

		// Verify state in dispatch store is settled
		disp, ok := store.LookupDispatch(ctx, occID)
		if !ok || disp.Status != "settled" {
			t.Fatalf("expected settled dispatch in store, got %+v", disp)
		}

		// Second call: duplicate request with identical idempotency key
		res2, err := inv.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("replay invoke error: %v", err)
		}
		if res2.Status != toolpkg.InvocationStatusCompleted {
			t.Fatalf("replay status: %s", res2.Status)
		}
		if res2.OccurrenceID != occID {
			t.Errorf("expected same occurrence ID on replay: want %s, got %s", occID, res2.OccurrenceID)
		}
	})

	t.Run("Interrupted crash intent produces indeterminate outcome and prevents unsafe replay", func(t *testing.T) {
		inv, _, store := setupTestInvoker(mutTool)
		ctx := context.Background()

		// Simulate crash: pre-seed a dispatch intent stuck in DispatchStatusPrepared
		// (e.g. host died while external provider was executing)
		fakeReq := toolpkg.InvocationRequest{
			Tool:           "deploy-apply",
			Action:         "apply",
			IdempotencyKey: "crashed-job-999",
			Arguments: map[string]any{
				"id": "cluster-999",
			},
			Authority: toolpkg.InvocationAuthority{
				Actor:         "operator",
				AllowMutating: true,
			},
		}

		// First, prepare intent directly in store without settling it (simulating crash)
		renderedMap := map[string]any{
			"tool":            fakeReq.Tool,
			"action":          fakeReq.Action,
			"args":            fakeReq.Arguments,
			"idempotency_key": fakeReq.IdempotencyKey,
		}
		_, prepErr := store.PrepareDispatch(ctx, engine.DispatchRequest{
			Classification:   "mutating",
			EndpointIdentity: "tool:native",
			RenderedRequest:  renderedMap,
		})
		if prepErr != nil {
			t.Fatalf("failed to simulate prepared intent: %v", prepErr)
		}

		// Now invoke via Invoker with the same idempotency key (recovery/replay attempt)
		res, err := inv.Invoke(ctx, fakeReq)
		if err != nil {
			t.Fatalf("unexpected Go error (should be wrapped in result): %v", err)
		}
		if res.Status != toolpkg.InvocationStatusIndeterminate {
			t.Fatalf("expected indeterminate status on unmatched crash intent, got %s (err: %s)", res.Status, res.ErrorMessage)
		}
		if !strings.Contains(res.ErrorMessage, "unmatched dispatch intent") {
			t.Errorf("expected unmatched dispatch intent error, got: %s", res.ErrorMessage)
		}
	})
}

// Scenario D: Authority allowlist enforcement
func TestInvoker_ScenarioD_AuthorityAllowlist(t *testing.T) {
	toolA := toolpkg.ToolDef{
		Name:      "allowed-tool",
		Transport: toolpkg.TransportNative,
		Command:   "echo",
		Actions: map[string]*toolpkg.ToolAction{
			"ping": {Classification: stringPtr("read-only")},
		},
	}
	toolB := toolpkg.ToolDef{
		Name:      "restricted-tool",
		Transport: toolpkg.TransportNative,
		Command:   "echo",
		Actions: map[string]*toolpkg.ToolAction{
			"ping": {Classification: stringPtr("read-only")},
		},
	}

	inv, _, _ := setupTestInvoker(toolA, toolB)
	ctx := context.Background()

	authority := toolpkg.InvocationAuthority{
		Actor:        "scoped-agent",
		AllowRead:    true,
		AllowedTools: []string{"allowed-tool"}, // only toolA permitted
	}

	t.Run("Permitted tool executes", func(t *testing.T) {
		res, err := inv.Invoke(ctx, toolpkg.InvocationRequest{
			Tool:      "allowed-tool",
			Action:    "ping",
			Authority: authority,
		})
		if err != nil || res.Status != toolpkg.InvocationStatusCompleted {
			t.Fatalf("expected allowed tool to complete, got %s: %v", res.Status, err)
		}
	})

	t.Run("Non-permitted tool is denied immediately", func(t *testing.T) {
		res, err := inv.Invoke(ctx, toolpkg.InvocationRequest{
			Tool:      "restricted-tool",
			Action:    "ping",
			Authority: authority,
		})
		if err != nil {
			t.Fatalf("unexpected go error: %v", err)
		}
		if res.Status != toolpkg.InvocationStatusDenied {
			t.Fatalf("expected denied status, got %s", res.Status)
		}
		if !strings.Contains(res.ErrorMessage, "not permitted by caller authority") {
			t.Errorf("expected authority denial error, got %s", res.ErrorMessage)
		}
	})
}

func TestInvoker_EnumEnforcement(t *testing.T) {
	enumTool := toolpkg.ToolDef{
		Name:      "cloud-manager",
		Transport: toolpkg.TransportNative,
		Command:   "echo",
		Actions: map[string]*toolpkg.ToolAction{
			"provision": {
				Classification: stringPtr("mutating"),
				Argv:           []string{"--message", "${region}"},
				Args: map[string]*toolpkg.ArgDef{
					"region": {
						Type:     "string",
						Required: true,
						Enum:     schema.EnumConstraint{"us-east-1", "eu-west-1", "ap-southeast-1"},
					},
				},
			},
		},
	}

	inv, _, _ := setupTestInvoker(enumTool)
	ctx := context.Background()
	auth := toolpkg.InvocationAuthority{
		Actor:         "test-actor",
		AllowMutating: true,
	}

	t.Run("Valid enum member succeeds", func(t *testing.T) {
		res, err := inv.Invoke(ctx, toolpkg.InvocationRequest{
			Tool:      "cloud-manager",
			Action:    "provision",
			Arguments: map[string]any{"region": "us-east-1"},
			Authority: auth,
		})
		if err != nil || res.Status != toolpkg.InvocationStatusCompleted {
			t.Fatalf("expected completed with valid enum, got %s (err: %v, errMsg: %q)", res.Status, err, res.ErrorMessage)
		}
	})

	t.Run("Invalid enum member is rejected before execution", func(t *testing.T) {
		res, err := inv.Invoke(ctx, toolpkg.InvocationRequest{
			Tool:      "cloud-manager",
			Action:    "provision",
			Arguments: map[string]any{"region": "invalid-region"},
			Authority: auth,
		})
		if err != nil {
			t.Fatalf("unexpected go error: %v", err)
		}
		if res.Status != toolpkg.InvocationStatusFailed {
			t.Fatalf("expected failed status, got %s", res.Status)
		}
		if !strings.Contains(res.ErrorMessage, "ENUM-008") {
			t.Fatalf("expected ENUM-008 validation error, got: %s", res.ErrorMessage)
		}
	})
}

func TestInvoker_PersistentStore_ConcurrentAndReplay(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "yawr-invoker-concurrent-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	mockTool := toolpkg.ToolDef{
		Name:      "safe-echo",
		Transport: toolpkg.TransportNative,
		Command:   "echo",
		Actions: map[string]*toolpkg.ToolAction{
			"run": {
				Classification: stringPtr("mutating"),
				Argv:           []string{"--message", "${msg}"},
				Args: map[string]*toolpkg.ArgDef{
					"msg": {Type: "string", Required: true},
				},
			},
		},
	}

	reg := NewMapRegistry([]toolpkg.ToolDef{mockTool})
	rt := NewDefaultToolRuntime(reg)
	fs, err := NewFileDispatchStore(filepath.Join(tempDir, "state"))
	if err != nil {
		t.Fatalf("NewFileDispatchStore: %v", err)
	}

	inv := NewInvoker(InvokerConfig{
		Registry:      reg,
		Runtime:       rt,
		DispatchStore: fs,
	})

	const goroutines = 6
	var wg sync.WaitGroup
	wg.Add(goroutines)

	results := make([]*toolpkg.InvocationResult, goroutines)
	errorsList := make([]error, goroutines)

	req := toolpkg.InvocationRequest{
		Tool:           "safe-echo",
		Action:         "run",
		Arguments:      map[string]any{"msg": "hello-world"},
		IdempotencyKey: "concurrent-run-key-1",
		Authority: toolpkg.InvocationAuthority{
			Actor:         "test-client",
			AllowMutating: true,
		},
	}

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			res, err := inv.Invoke(context.Background(), req)
			results[idx] = res
			errorsList[idx] = err
		}(i)
	}

	wg.Wait()

	replayedCount := 0
	completedCount := 0
	for i := 0; i < goroutines; i++ {
		if errorsList[i] != nil {
			t.Errorf("goroutine %d failed with error: %v", i, errorsList[i])
		}
		if results[i] == nil {
			t.Fatalf("goroutine %d returned nil result", i)
		}
		if results[i].Status != toolpkg.InvocationStatusCompleted {
			t.Errorf("goroutine %d status got %s (errMsg: %q), want completed", i, results[i].Status, results[i].ErrorMessage)
		} else {
			completedCount++
		}
		if results[i].Replayed {
			replayedCount++
		}
	}

	if completedCount != goroutines {
		t.Fatalf("expected all %d goroutines to complete, got %d", goroutines, completedCount)
	}
	if replayedCount != goroutines-1 {
		t.Fatalf("expected exactly %d replayed outcomes, got %d", goroutines-1, replayedCount)
	}
}

