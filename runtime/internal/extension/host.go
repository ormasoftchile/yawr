package extension

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

const handshakeTimeout = 30 * time.Second

// concreteHost implements extension.ExtensionHost.
type concreteHost struct {
	mu           sync.RWMutex
	procs        []*extensionProcess
	registry     tool.ToolRegistry
	tools        []*schema.ToolDef
	providers    []*schema.ProviderDef
	policyRules  []governance.PolicyRule
	statuses     map[string]extension.ExtensionStatus
	healthCancel context.CancelFunc
}

// NewHost constructs a concrete extension host.
func NewHost(registry tool.ToolRegistry) *concreteHost {
	return &concreteHost{
		registry: registry,
		statuses: make(map[string]extension.ExtensionStatus),
	}
}

// Load discovers and launches all extensions declared in the manifest.
func (h *concreteHost) Load(ctx context.Context, manifest *extension.ProjectManifest) error {
	workdir, err := os.Getwd()
	if err != nil {
		return err
	}
	decls, err := discover(ctx, manifest, workdir)
	if err != nil {
		return err
	}
	if len(decls) == 0 {
		return nil
	}
	healthCtx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.healthCancel = cancel
	h.mu.Unlock()
	var loadErr error
	for _, decl := range decls {
		h.setStatus(decl.Name, extension.StateDiscovered, nil)
		proc, err := startProcess(ctx, decl)
		if err != nil {
			h.setStatus(decl.Name, extension.StateFailed, err)
			if loadErr == nil {
				loadErr = err
			}
			continue
		}
		proc.setState(extension.StateStarting)
		h.setStatus(decl.Name, extension.StateStarting, nil)
		parsed, _, err := readManifestDir(decl.Path)
		if err != nil {
			_ = proc.stop(ctx)
			proc.setState(extension.StateFailed)
			h.setStatus(decl.Name, extension.StateFailed, err)
			if loadErr == nil {
				loadErr = err
			}
			continue
		}
		grants := intersectGrants(parsed.Capabilities, decl.Grants)
		proc.setState(extension.StateHandshaking)
		h.setStatus(decl.Name, extension.StateHandshaking, nil)
		handshakeCtx, cancelHandshake := context.WithTimeout(ctx, handshakeTimeout)
		res, err := performHandshake(handshakeCtx, proc, grants)
		cancelHandshake()
		if err != nil {
			_ = proc.stop(ctx)
			proc.setState(extension.StateFailed)
			h.setStatus(decl.Name, extension.StateFailed, err)
			if loadErr == nil {
				loadErr = err
			}
			continue
		}
		if err := h.registerContributions(res, grants); err != nil {
			_ = proc.stop(ctx)
			proc.setState(extension.StateFailed)
			h.setStatus(decl.Name, extension.StateFailed, err)
			if loadErr == nil {
				loadErr = err
			}
			continue
		}
		proc.setState(extension.StateLoaded)
		h.setStatus(decl.Name, extension.StateLoaded, nil)
		h.mu.Lock()
		h.procs = append(h.procs, proc)
		h.mu.Unlock()
		startHealthLoop(healthCtx, proc, func(name string, err error) {
			h.setStatus(name, extension.StateFailed, err)
		})
	}
	return loadErr
}

// Shutdown gracefully stops all running extensions.
func (h *concreteHost) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	if h.healthCancel != nil {
		h.healthCancel()
		h.healthCancel = nil
	}
	procs := append([]*extensionProcess(nil), h.procs...)
	h.mu.Unlock()
	var firstErr error
	for _, proc := range procs {
		if err := proc.stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		h.setStatus(proc.decl.Name, extension.StateShutdown, nil)
	}
	return firstErr
}

// ContributedTools returns tools from all loaded extensions.
func (h *concreteHost) ContributedTools() []*schema.ToolDef {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]*schema.ToolDef(nil), h.tools...)
}

// ContributedProviders returns providers from all loaded extensions.
func (h *concreteHost) ContributedProviders() []*schema.ProviderDef {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]*schema.ProviderDef(nil), h.providers...)
}

// ContributedPolicyRules returns governance rules from all loaded extensions.
func (h *concreteHost) ContributedPolicyRules() []governance.PolicyRule {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]governance.PolicyRule(nil), h.policyRules...)
}

// Status returns the current state of each loaded extension.
func (h *concreteHost) Status() []extension.ExtensionStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	statuses := make([]extension.ExtensionStatus, 0, len(h.statuses))
	for _, status := range h.statuses {
		statuses = append(statuses, status)
	}
	return statuses
}

func (h *concreteHost) registerContributions(res *handshakeResult, grants extension.CapabilitySet) error {
	if res == nil {
		return nil
	}
	if len(res.Tools) > 0 {
		if err := enforce(grants, extension.CapabilityToolRegistration); err != nil {
			return err
		}
		if h.registry == nil {
			return fmt.Errorf("tool registry not configured")
		}
		for _, toolContrib := range res.Tools {
			if toolContrib.Def == nil {
				continue
			}
			runtimeDef, err := toolDefFromSchema(toolContrib.ExtensionName, toolContrib.Def)
			if err != nil {
				return err
			}
			if err := h.registry.Register(runtimeDef); err != nil {
				return err
			}
			h.mu.Lock()
			h.tools = append(h.tools, toolContrib.Def)
			h.mu.Unlock()
		}
	}
	if len(res.Providers) > 0 {
		if err := enforce(grants, extension.CapabilityProviderRegistration); err != nil {
			return err
		}
		h.mu.Lock()
		for _, provider := range res.Providers {
			if provider.Def == nil {
				continue
			}
			h.providers = append(h.providers, provider.Def)
		}
		h.mu.Unlock()
	}
	if len(res.Policy) > 0 {
		if err := enforce(grants, extension.CapabilityPolicyContribution); err != nil {
			return err
		}
		h.mu.Lock()
		for _, rule := range res.Policy {
			h.policyRules = append(h.policyRules, rule.Rule)
		}
		h.mu.Unlock()
	}
	return nil
}

func (h *concreteHost) setStatus(name string, state extension.ExtensionState, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	status := extension.ExtensionStatus{Name: name, State: state}
	if state == extension.StateFailed {
		status.Err = err
	}
	h.statuses[name] = status
}

func toolDefFromSchema(extName string, def *schema.ToolDef) (tool.ToolDef, error) {
	if def == nil {
		return tool.ToolDef{}, fmt.Errorf("nil tool definition")
	}
	transport, err := mapTransport(def.Transport.Type)
	if err != nil {
		return tool.ToolDef{}, err
	}
	source := fmt.Sprintf("extension://%s", extName)
	return tool.ToolDef{
		Name:      def.Name,
		Source:    source,
		Transport: transport,
		Command:   def.Transport.Command,
		Args:      def.Transport.Args,
		Env:       def.Transport.Env,
	}, nil
}

func mapTransport(t schema.Transport) (tool.TransportType, error) {
	switch t {
	case schema.TransportStdio:
		return tool.TransportStdio, nil
	case schema.TransportJSONRPC:
		return tool.TransportJSONRPC, nil
	case schema.TransportMCP:
		return tool.TransportMCP, nil
	default:
		return "", fmt.Errorf("unsupported transport %q", t)
	}
}
