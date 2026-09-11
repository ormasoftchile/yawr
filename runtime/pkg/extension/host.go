package extension

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ExtensionHost manages the lifecycle of out-of-process extensions.
type ExtensionHost interface {
	// Load discovers and launches all extensions declared in the project manifest.
	Load(ctx context.Context, manifest *ProjectManifest) error

	// Shutdown gracefully stops all running extensions.
	Shutdown(ctx context.Context) error

	// ContributedTools returns all tools contributed by loaded extensions.
	ContributedTools() []*schema.ToolDef

	// ContributedProviders returns all providers contributed by loaded extensions.
	ContributedProviders() []*schema.ProviderDef

	// ContributedPolicyRules returns governance rules contributed by extensions.
	ContributedPolicyRules() []governance.PolicyRule

	// Status returns the runtime status of each loaded extension.
	Status() []ExtensionStatus
}

// ExtensionStatus reports the runtime status of a loaded extension.
type ExtensionStatus struct {
	Name  string
	State ExtensionState
	Err   error // non-nil if State == StateFailed
}

// ProjectManifest declares the extensions used by a project.
type ProjectManifest struct {
	Extensions []ExtensionDecl `yaml:"extensions,omitempty" json:"extensions,omitempty"`
}

// ExtensionDecl is a single extension declared in the project manifest.
type ExtensionDecl struct {
	Name       string   `yaml:"name"                 json:"name"`
	Path       string   `yaml:"path,omitempty"       json:"path,omitempty"`
	Entrypoint string   `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
	Grants     []string `yaml:"grants,omitempty"     json:"grants,omitempty"`
}
