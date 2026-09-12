package testutil

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// FakeExtensionHost is an injectable test double.
type FakeExtensionHost struct {
	LoadErr       error
	ShutdownErr   error
	Tools         []*schema.ToolDef
	Providers     []*schema.ProviderDef
	PolicyRules   []governance.PolicyRule
	LoadCallCount int
}

func (f *FakeExtensionHost) Load(ctx context.Context, manifest *extension.ProjectManifest) error {
	_ = ctx
	_ = manifest
	f.LoadCallCount++
	return f.LoadErr
}

func (f *FakeExtensionHost) Shutdown(ctx context.Context) error {
	_ = ctx
	return f.ShutdownErr
}

func (f *FakeExtensionHost) ContributedTools() []*schema.ToolDef {
	return f.Tools
}

func (f *FakeExtensionHost) ContributedProviders() []*schema.ProviderDef {
	return f.Providers
}

func (f *FakeExtensionHost) ContributedPolicyRules() []governance.PolicyRule {
	return f.PolicyRules
}

func (f *FakeExtensionHost) Status() []extension.ExtensionStatus {
	return nil
}
