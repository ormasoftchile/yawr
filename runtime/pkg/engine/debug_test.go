package engine

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

func TestMergeDebugProtectionDeepCopiesPatterns(t *testing.T) {
	pattern := &governance.RedactionPattern{Pattern: "token-[a-z]+", Replacement: "<redacted>"}
	base := DebugProtection{RedactionPatterns: []*governance.RedactionPattern{pattern}}
	merged := MergeDebugProtection(base, DebugProtection{})
	pattern.Pattern = "mutated"
	if merged.RedactionPatterns[0].Pattern != "token-[a-z]+" {
		t.Fatalf("merge retained caller pattern pointer: %#v", merged.RedactionPatterns)
	}
	merged.RedactionPatterns[0].Pattern = "also-mutated"
	if base.RedactionPatterns[0].Pattern != "mutated" {
		t.Fatalf("merged pattern mutated source: %#v", base.RedactionPatterns)
	}
}
