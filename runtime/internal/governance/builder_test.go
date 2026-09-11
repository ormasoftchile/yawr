package governance

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestBuildPolicy_AllowCommands(t *testing.T) {
	cfg := &schema.GovernanceConfig{AllowCommands: []string{"kubectl", "curl"}}
	pol := BuildPolicy(cfg)

	allowed, _ := pol.CheckCommand("kubectl")
	if !allowed {
		t.Errorf("expected kubectl to be allowed")
	}

	allowed, _ = pol.CheckCommand("rm")
	if allowed {
		t.Errorf("expected rm to be denied (not in allowlist)")
	}
}

func TestBuildPolicy_DenyCommands(t *testing.T) {
	cfg := &schema.GovernanceConfig{DenyCommands: []string{"rm", "dd"}}
	pol := BuildPolicy(cfg)

	allowed, _ := pol.CheckCommand("rm")
	if allowed {
		t.Errorf("expected rm to be denied")
	}

	allowed, _ = pol.CheckCommand("kubectl")
	if !allowed {
		t.Errorf("expected kubectl to be allowed (permissive mode)")
	}
}

func TestBuildPolicy_MergeRunbookAndStep(t *testing.T) {
	runbookCfg := &schema.GovernanceConfig{AllowCommands: []string{"kubectl", "curl"}}
	stepCfg := &schema.GovernanceConfig{AllowCommands: []string{"kubectl"}}
	pol := BuildPolicy(runbookCfg, stepCfg)

	allowed, _ := pol.CheckCommand("kubectl")
	if !allowed {
		t.Errorf("kubectl should be allowed")
	}

	allowed, _ = pol.CheckCommand("curl")
	if allowed {
		t.Errorf("curl should be denied (not in step allowlist)")
	}
}

func TestBuildPolicy_MergeDenyIsAdditive(t *testing.T) {
	runbookCfg := &schema.GovernanceConfig{DenyCommands: []string{"rm"}}
	stepCfg := &schema.GovernanceConfig{DenyCommands: []string{"dd"}}
	pol := BuildPolicy(runbookCfg, stepCfg)

	allowed, _ := pol.CheckCommand("rm")
	if allowed {
		t.Errorf("rm should be denied")
	}

	allowed, _ = pol.CheckCommand("dd")
	if allowed {
		t.Errorf("dd should be denied")
	}
}

func TestBuildPolicy_MergeApprovalIsOR(t *testing.T) {
	runbookCfg := &schema.GovernanceConfig{RequireApproval: false}
	stepCfg := &schema.GovernanceConfig{RequireApproval: true}
	pol := BuildPolicy(runbookCfg, stepCfg)

	eval := NewEvaluator(pol, nil)
	if !eval.(*evaluator).requireApproval {
		t.Errorf("expected requireApproval=true (OR semantics)")
	}
}

func TestBuildPolicy_RedactionPatterns(t *testing.T) {
	cfg := &schema.GovernanceConfig{
		Redact: []schema.RedactRule{{Pattern: "token", Replace: "[REDACTED]"}},
	}
	pol := BuildPolicy(cfg)

	patterns := pol.RedactionPatterns()
	if len(patterns) != 1 {
		t.Errorf("expected 1 redaction pattern, got %d", len(patterns))
	}
}

// TestScriptMatchPattern verifies that patterns ending in * match scripts
// containing path separators via prefix semantics, since path.Match's * does
// not cross /.
func TestScriptMatchPattern(t *testing.T) {
	cases := []struct {
		pattern string
		script  string
		want    bool
	}{
		{"rm -rf *", "rm -rf /tmp/scratch", true},
		{"rm -rf *", "rm -rf ./localdir", true},
		{"rm -rf *", "rm -rf somefile", true},
		{"rm", "rm file", false},
		{"echo", "echo hello", false},
		{"rm*", "rm -rf /path", true},
		{"kubectl *", "kubectl delete pod foo", true},
		{"kubectl *", "helm upgrade foo", false},
	}
	for _, tc := range cases {
		got := scriptMatchPattern(tc.pattern, tc.script)
		if got != tc.want {
			t.Errorf("scriptMatchPattern(%q, %q) = %v; want %v", tc.pattern, tc.script, got, tc.want)
		}
	}
}

// TestBuildPolicy_CheckScript verifies that CheckScript enforces deny patterns
// against the full script string so that patterns like "rm -rf *" match scripts
// that contain path separators.
func TestBuildPolicy_CheckScript(t *testing.T) {
	pol := BuildPolicy(&schema.GovernanceConfig{DenyCommands: []string{"rm -rf *"}})
	sc, ok := pol.(*policy)
	if !ok {
		t.Fatal("BuildPolicy must return *policy for CheckScript tests")
	}

	if allowed, rule := sc.CheckScript("rm -rf /tmp/scratch"); allowed {
		t.Errorf("expected rm -rf /tmp/scratch to be denied; got allowed")
		_ = rule
	}
	if allowed, _ := sc.CheckScript("echo hello"); !allowed {
		t.Errorf("expected echo hello to be allowed")
	}
	if allowed, _ := sc.CheckScript("rm -rf ./localdir"); allowed {
		t.Errorf("expected rm -rf ./localdir to be denied by rm -rf * pattern")
	}
}
