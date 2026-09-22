package schema_test

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestIsStaticHandoffTarget(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"child.runbook.yaml", true},
		{"sub/nested.runbook.yaml", true},
		{"child.yawr", true},
		{"sub/nested.yawr", true},
		{"UPPER.YAWR", true},
		{"UPPER.RUNBOOK.YAML", true},
		{"", false},
		{"   ", false},
		{"/root.yawr", false},
		{"/root.runbook.yaml", false},
		{"../parent.yawr", false},
		{"../parent.runbook.yaml", false},
		{"${dynamic}.yawr", false},
		{"${dynamic}.runbook.yaml", false},
		{"not-a-runbook.txt", false},
		{"tool.yawt", false},
		{"tool.yaml", false},
		{"child.runbook.yaml ", false},
		{" child.yawr", false},
	}

	for _, tt := range tests {
		got := schema.IsStaticHandoffTarget(tt.target)
		if got != tt.want {
			t.Errorf("IsStaticHandoffTarget(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
}
