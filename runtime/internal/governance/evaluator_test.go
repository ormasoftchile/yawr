package governance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestEvaluate_AllowedCommand(t *testing.T) {
	cfg := &schema.GovernanceConfig{AllowCommands: []string{"kubectl"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, err := eval.Evaluate(context.Background(), governance.StepInfo{
		ID:      "step-1",
		Kind:    "cli",
		Command: "kubectl",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed || result.Denied {
		t.Errorf("expected Allowed=true, Denied=false")
	}
}

func TestEvaluate_DeniedCommand(t *testing.T) {
	cfg := &schema.GovernanceConfig{DenyCommands: []string{"rm"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, err := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "rm",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed || !result.Denied || result.DenyReason == "" {
		t.Errorf("expected Denied=true with DenyReason")
	}
}

func TestEvaluate_DenyOverridesAllow(t *testing.T) {
	cfg := &schema.GovernanceConfig{
		AllowCommands: []string{"kubectl"},
		DenyCommands:  []string{"kubectl"},
	}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "kubectl",
	})

	if !result.Denied {
		t.Errorf("deny-wins: expected Denied=true")
	}
}

func TestEvaluate_EmptyAllowlist_AllAllowed(t *testing.T) {
	pol := BuildPolicy(&schema.GovernanceConfig{})
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "any-command",
	})

	if !result.Allowed || result.Denied {
		t.Errorf("permissive mode: expected Allowed=true")
	}
}

func TestEvaluate_NonEmptyAllowlist_UnlistedDenied(t *testing.T) {
	cfg := &schema.GovernanceConfig{AllowCommands: []string{"kubectl"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "curl",
	})

	if !result.Denied {
		t.Errorf("expected Denied=true (not in allowlist)")
	}
}

func TestEvaluate_GlobPatternMatching(t *testing.T) {
	cfg := &schema.GovernanceConfig{AllowCommands: []string{"kube*"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "kubectl",
	})

	if !result.Allowed {
		t.Errorf("glob match: expected Allowed=true")
	}
}

func TestEvaluate_EnvVarBlocking(t *testing.T) {
	cfg := &schema.GovernanceConfig{DenyEnvVars: []string{"*_SECRET"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID:   "step-1",
		Kind: "cli",
		EnvVars: map[string]string{
			"API_SECRET": "secret-value",
			"API_KEY":    "key-value",
		},
	})

	if len(result.BlockedEnvVars) != 1 || result.BlockedEnvVars[0] != "API_SECRET" {
		t.Errorf("expected API_SECRET blocked, got %v", result.BlockedEnvVars)
	}
	if _, exists := result.FilteredEnvVars["API_SECRET"]; exists {
		t.Errorf("API_SECRET should not be in filtered vars")
	}
	if _, exists := result.FilteredEnvVars["API_KEY"]; !exists {
		t.Errorf("API_KEY should be in filtered vars")
	}
}

func TestEvaluate_EnvVarBlockingMultiplePatterns(t *testing.T) {
	cfg := &schema.GovernanceConfig{DenyEnvVars: []string{"*_SECRET", "*_PASSWORD"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID:   "step-1",
		Kind: "cli",
		EnvVars: map[string]string{
			"DB_PASSWORD": "pass",
			"API_SECRET":  "secret",
			"PUBLIC_KEY":  "key",
		},
	})

	if len(result.BlockedEnvVars) != 2 || len(result.FilteredEnvVars) != 1 {
		t.Errorf("expected 2 blocked, 1 filtered, got %d blocked, %d filtered",
			len(result.BlockedEnvVars), len(result.FilteredEnvVars))
	}
}

func TestEvaluate_RequiresApproval(t *testing.T) {
	cfg := &schema.GovernanceConfig{RequireApproval: true}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "kubectl",
	})

	if !result.RequiresApproval || result.Denied {
		t.Errorf("expected RequiresApproval=true, Denied=false")
	}
}

func TestEvaluate_DeniedOverridesApproval(t *testing.T) {
	cfg := &schema.GovernanceConfig{
		RequireApproval: true,
		DenyCommands:    []string{"rm"},
	}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "rm",
	})

	if !result.Denied {
		t.Errorf("deny takes precedence: expected Denied=true")
	}
}

func TestEvaluate_EvidencePopulated(t *testing.T) {
	cfg := &schema.GovernanceConfig{AllowCommands: []string{"kubectl"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "kubectl",
	})

	if result.Evidence.StepID != "step-1" {
		t.Errorf("expected StepID=step-1, got %s", result.Evidence.StepID)
	}
	if result.Evidence.EvaluatedAt == "" {
		t.Errorf("expected non-empty EvaluatedAt")
	}
	if result.Evidence.Outcome != "allowed" {
		t.Errorf("expected Outcome=allowed, got %s", result.Evidence.Outcome)
	}
}

func TestEvaluate_NonCLIStep_AlwaysAllowed(t *testing.T) {
	cfg := &schema.GovernanceConfig{AllowCommands: []string{"kubectl"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID:      "step-1",
		Kind:    "tool",
		Command: "", // Non-CLI
	})

	if !result.Allowed || result.Denied {
		t.Errorf("non-CLI step: expected Allowed=true, Denied=false")
	}
}

func TestEvaluate_NilPolicy_AlwaysAllowed(t *testing.T) {
	pol := BuildPolicy() // No configs
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "any-command",
	})

	if !result.Allowed {
		t.Errorf("empty policy: expected Allowed=true")
	}
}

func TestEvaluate_EvidenceJSON_Roundtrip(t *testing.T) {
	cfg := &schema.GovernanceConfig{AllowCommands: []string{"kubectl"}}
	pol := BuildPolicy(cfg)
	eval := NewEvaluator(pol, nil)

	result, _ := eval.Evaluate(context.Background(), governance.StepInfo{
		ID: "step-1", Kind: "cli", Command: "kubectl",
	})

	data, err := json.Marshal(result.Evidence)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled governance.Evidence
	err = json.Unmarshal(data, &unmarshaled)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.StepID != result.Evidence.StepID {
		t.Errorf("StepID mismatch after roundtrip")
	}
}
