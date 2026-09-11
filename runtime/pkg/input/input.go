package input

import "context"

// InputRequest describes what value is being requested.
type InputRequest struct {
	StepID    string            // step that needs the value
	VarName   string            // variable name being resolved
	Schema    map[string]any    // JSON Schema for validation (optional)
	Sensitive bool              // if true, mask in logs
	Metadata  map[string]string // arbitrary k/v for providers
}

// InputResponse is the resolved value with provenance.
type InputResponse struct {
	Value     any    // resolved value
	Source    string // which provider resolved it (for logging/replay)
	CacheKey  string // opaque key for replay (StepID+VarName by default)
	Sensitive bool   // propagated from request
}

// InputProvider resolves a single input request.
type InputProvider interface {
	// Provide resolves the request. Returns (nil, nil) if this provider
	// cannot satisfy the request (allows chain fallthrough).
	Provide(ctx context.Context, req InputRequest) (*InputResponse, error)
	// Name returns the provider's identifier for logging.
	Name() string
}

// InputRegistry holds multiple providers and resolves via priority chain.
type InputRegistry interface {
	// Register adds a provider. Lower priority number = higher precedence.
	Register(priority int, p InputProvider)
	// Resolve tries providers in priority order; returns error if none resolve.
	Resolve(ctx context.Context, req InputRequest) (*InputResponse, error)
	// SetDefault sets a provider of last resort (used when chain is empty).
	SetDefault(p InputProvider)
}
