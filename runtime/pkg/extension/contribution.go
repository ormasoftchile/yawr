package extension

import (
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ContributedTool is a tool contributed by an extension at runtime.
type ContributedTool struct {
	ExtensionName string
	Def           *schema.ToolDef
}

// ContributedProvider is a provider contributed by an extension.
type ContributedProvider struct {
	ExtensionName string
	Def           *schema.ProviderDef
	Prefixes      []string
}

// ContributedPolicyRule is a governance rule contributed by an extension.
type ContributedPolicyRule struct {
	ExtensionName string
	Rule          governance.PolicyRule
}
