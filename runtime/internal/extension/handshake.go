package extension

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type handshakeResult struct {
	Tools     []extension.ContributedTool
	Providers []extension.ContributedProvider
	Policy    []extension.ContributedPolicyRule
}

type initParams struct {
	HostVersion string   `json:"hostVersion"`
	Grants      []string `json:"grants"`
}

type initResult struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

type contributionsResult struct {
	Tools               []schema.ToolDef       `json:"tools,omitempty"`
	Providers           []providerContribution `json:"providers,omitempty"`
	PolicyContributions []policyContribution   `json:"policyContributions,omitempty"`
}

type providerContribution struct {
	Name     string   `json:"name"`
	Prefixes []string `json:"prefixes"`
}

type policyContribution struct {
	ID          string   `json:"id"`
	Description string   `json:"description,omitempty"`
	Allow       []string `json:"allow,omitempty"`
	Deny        []string `json:"deny,omitempty"`
	DenyEnvVars []string `json:"denyEnvVars,omitempty"`
}

// performHandshake sends extension/initialize and contributions/list.
func performHandshake(ctx context.Context, proc *extensionProcess, grants extension.CapabilitySet) (*handshakeResult, error) {
	if proc == nil || proc.codec == nil {
		return nil, fmt.Errorf("handshake: missing process codec")
	}
	initReq := initParams{HostVersion: hostVersion, Grants: capabilitySlice(grants)}
	var initResp initResult
	if err := proc.codec.call(ctx, "extension/initialize", initReq, &initResp); err != nil {
		return nil, err
	}
	if initResp.Name == "" || initResp.Version == "" {
		return nil, fmt.Errorf("handshake: missing extension identity")
	}
	for _, cap := range initResp.Capabilities {
		if !grants.Has(cap) {
			return nil, ErrCapabilityViolation
		}
	}
	var contribResp contributionsResult
	if err := proc.codec.call(ctx, "contributions/list", nil, &contribResp); err != nil {
		return nil, err
	}
	result := &handshakeResult{}
	for i := range contribResp.Tools {
		toolDef := contribResp.Tools[i]
		result.Tools = append(result.Tools, extension.ContributedTool{
			ExtensionName: initResp.Name,
			Def:           &toolDef,
		})
	}
	for _, prov := range contribResp.Providers {
		def := &schema.ProviderDef{Name: prov.Name}
		result.Providers = append(result.Providers, extension.ContributedProvider{
			ExtensionName: initResp.Name,
			Def:           def,
			Prefixes:      prov.Prefixes,
		})
	}
	for _, rule := range contribResp.PolicyContributions {
		result.Policy = append(result.Policy, extension.ContributedPolicyRule{
			ExtensionName: initResp.Name,
			Rule: governance.PolicyRule{
				ID:          rule.ID,
				Description: rule.Description,
				Allow:       governance.AllowList{Patterns: rule.Allow},
				Deny:        governance.DenyList{Patterns: rule.Deny},
				DenyEnvVars: rule.DenyEnvVars,
			},
		})
	}
	return result, nil
}

func capabilitySlice(caps extension.CapabilitySet) []string {
	if len(caps) == 0 {
		return nil
	}
	out := make([]string, 0, len(caps))
	for cap := range caps {
		out = append(out, cap)
	}
	return out
}
