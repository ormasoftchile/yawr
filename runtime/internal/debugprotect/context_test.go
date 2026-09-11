package debugprotect

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

func TestProtectionContextOmitsSecretsAndDeepCopiesPatterns(t *testing.T) {
	pattern := &governance.RedactionPattern{Pattern: "token-[a-z]+", Replacement: "<redacted>"}
	ctx := WithProtection(context.Background(), engine.DebugProtection{
		ProtectedVars: []string{"api_secret"}, SecretValues: []string{"secret-value"},
		RedactionPatterns: []*governance.RedactionPattern{pattern},
	})
	pattern.Pattern = "mutated"

	first := ProtectionFromContext(ctx)
	if len(first.SecretValues) != 0 || len(first.ProtectedVars) != 1 || first.ProtectedVars[0] != "api_secret" {
		t.Fatalf("transported protection = %#v", first)
	}
	if len(first.RedactionPatterns) != 1 || first.RedactionPatterns[0].Pattern != "token-[a-z]+" {
		t.Fatalf("transported patterns = %#v", first.RedactionPatterns)
	}
	first.RedactionPatterns[0].Pattern = "also-mutated"
	if second := ProtectionFromContext(ctx); second.RedactionPatterns[0].Pattern != "token-[a-z]+" {
		t.Fatalf("context returned shared pattern pointer: %#v", second.RedactionPatterns)
	}
}
