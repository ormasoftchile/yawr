package tool

import (
	"context"
	"fmt"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// DefaultToolRuntime selects transports based on ToolDef.Transport.
type DefaultToolRuntime struct {
	registry toolpkg.ToolRegistry

	mu         sync.Mutex
	persistent map[string]toolpkg.ToolTransport
	profile    *schema.RuntimeProfile // may be nil; set via SetProfile before first Invoke
}

// Registry returns the underlying tool registry.
func (r *DefaultToolRuntime) Registry() toolpkg.ToolRegistry {
	return r.registry
}

// LookupDef implements toolpkg.ToolDefLookup, exposing the resolved tool
// definition (including any per-action execute.kind: runbook substitution
// declaration) to callers such as the tool executor without requiring a
// concrete-type assertion.
func (r *DefaultToolRuntime) LookupDef(name string) (*toolpkg.ToolDef, bool) {
	if r.registry == nil {
		return nil, false
	}
	return r.registry.Lookup(name)
}

// NewDefaultToolRuntime constructs a DefaultToolRuntime.
func NewDefaultToolRuntime(registry toolpkg.ToolRegistry) *DefaultToolRuntime {
	return &DefaultToolRuntime{
		registry:   registry,
		persistent: make(map[string]toolpkg.ToolTransport),
	}
}

// SetProfile attaches a runtime profile to the runtime. Per-tool endpoint and
// provider overrides in the profile are applied when transports are first
// constructed (at invocation time). Must be called before the first Invoke.
// Ratified Rule A: the profile MAY override the auth provider; it MUST NOT
// override Scope or AllowedHosts — those always come from the tool definition.
func (r *DefaultToolRuntime) SetProfile(profile *schema.RuntimeProfile) {
	r.profile = profile
}

// Invoke looks up a tool and dispatches to the appropriate transport.
func (r *DefaultToolRuntime) Invoke(ctx context.Context, toolName string, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	if r.registry == nil {
		return nil, fmt.Errorf("tool runtime: registry not configured")
	}
	def, ok := r.registry.Lookup(toolName)
	if !ok || def == nil {
		return nil, fmt.Errorf("tool runtime: tool not found: %s", toolName)
	}
	actionDef, ok := def.Actions[action]
	if !ok || actionDef == nil {
		return nil, fmt.Errorf("tool runtime: action not found: %s", action)
	}
	if err := validateToolArguments(actionDef, args); err != nil {
		return nil, err
	}

	switch def.Transport {
	case toolpkg.TransportStdio:
		return (&StdioTransport{}).Invoke(ctx, *def, action, args)
	case toolpkg.TransportJSONRPC:
		return r.invokePersistent(ctx, toolName, *def, action, args, func() toolpkg.ToolTransport {
			return &JSONRPCTransport{}
		})
	case toolpkg.TransportMCP:
		return r.invokePersistent(ctx, toolName, *def, action, args, func() toolpkg.ToolTransport {
			return &MCPTransport{}
		})
	case toolpkg.TransportMCPHTTP:
		if def.Auth != nil {
			if err := ValidateAuthConfig(def.URL, def.Auth); err != nil {
				return nil, err
			}
		}
		return r.invokePersistent(ctx, toolName, *def, action, args, func() toolpkg.ToolTransport {
			var gate *TokenGate
			if def.Auth != nil {
				// Ratified Rule A: profile MAY override the provider (who acquires
				// the token). Scope and AllowedHosts MUST always come from the tool
				// definition — a profile never changes what the token is scoped for
				// or where it may be sent.
				providerName := def.Auth.Provider
				if r.profile != nil {
					if override, ok := r.profile.Tools[toolName]; ok && override != nil && override.Provider != "" {
						providerName = override.Provider
					}
				}
				provider, _ := NewAuthProviderForTarget(providerName, def.Auth.Scope, def.Auth.Resource, "")
				targetKind := "scope"
				targetValue := def.Auth.Scope
				if def.Auth.Resource != "" {
					targetKind = "resource"
					targetValue = def.Auth.Resource
				}
				gate = NewTokenGateForTarget(provider, targetKind, targetValue, def.Auth.AllowedHosts)
			}
			// Profile endpoint override: use the profile-supplied URL if present.
			// PLAN-013 (enforced at plan time) has already verified the override
			// host is in allowed_hosts, so execution here is safe.
			effectiveURL := def.URL
			if r.profile != nil {
				if override, ok := r.profile.Tools[toolName]; ok && override != nil && override.Endpoint != "" {
					effectiveURL = override.Endpoint
				}
			}
			return NewMCPHTTPTransport(effectiveURL, gate)
		})
	case toolpkg.TransportVSCodeMCP:
		// Apply vscode_input adaptation before the args go onto the bridge wire.
		adaptedArgs, adaptErr := applyVSCodeInputAdaptation(actionDef, args)
		if adaptErr != nil {
			return nil, adaptErr
		}
		return r.invokePersistent(ctx, toolName, *def, action, adaptedArgs, func() toolpkg.ToolTransport {
			return newVSCodeMCPTransport()
		})
	case toolpkg.TransportNative:
		return (&NativeCLITransport{}).Invoke(ctx, *def, action, args)
	case toolpkg.TransportNativeFileOnly:
		return (&NativeFileOnlyTransport{}).Invoke(ctx, *def, action, args)
	default:
		return nil, fmt.Errorf("tool runtime: unsupported transport %q", def.Transport)
	}
}

// Close closes all persistent transports.
func (r *DefaultToolRuntime) Close() error {
	r.mu.Lock()
	transports := make([]toolpkg.ToolTransport, 0, len(r.persistent))
	for _, t := range r.persistent {
		transports = append(transports, t)
	}
	r.persistent = make(map[string]toolpkg.ToolTransport)
	r.mu.Unlock()

	var firstErr error
	for _, t := range transports {
		if err := t.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *DefaultToolRuntime) invokePersistent(ctx context.Context, name string, def toolpkg.ToolDef, action string, args map[string]any, factory func() toolpkg.ToolTransport) (*toolpkg.ToolResult, error) {
	r.mu.Lock()
	transport, ok := r.persistent[name]
	if !ok {
		transport = factory()
		r.persistent[name] = transport
	}
	r.mu.Unlock()
	return transport.Invoke(ctx, def, action, args)
}
