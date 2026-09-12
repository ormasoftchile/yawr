package expr

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
)

// Engine identifies the runtime engine selected for one expression operation.
type Engine int

const (
	// EngineNative uses the native GXL/GIS/GCP engines. It is the only engine after P8.
	EngineNative Engine = iota
)

func (e Engine) String() string { return "native" }

// OperationKind identifies the subsystem routed through the expression shim.
type OperationKind int

const (
	// EvalCondition is boolean condition evaluation for when/branch/iterate.
	EvalCondition OperationKind = iota
	// Interpolate is GIS/string interpolation for runtime fields.
	Interpolate
	// ResolveCapturePath is GCP/capture-path resolution.
	ResolveCapturePath
)

func (k OperationKind) String() string {
	switch k {
	case EvalCondition:
		return "eval_condition"
	case Interpolate:
		return "interpolate"
	case ResolveCapturePath:
		return "resolve_capture_path"
	default:
		return "unknown"
	}
}

// Operation describes one expression operation crossing the selection seam.
type Operation struct {
	Kind   OperationKind
	Source string
}

// Selector chooses which engine handles an expression operation.
type Selector interface {
	EngineFor(op Operation) Engine
}

type nativeSelector struct{}

func newDefaultSelector() Selector { return nativeSelector{} }

func (nativeSelector) EngineFor(Operation) Engine { return EngineNative }

func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}

var (
	selectorMu      sync.RWMutex
	defaultSelector = newDefaultSelector()
	auditHook       func(Operation, Engine)
)

func selectedEngine(op Operation) Engine {
	selectorMu.RLock()
	sel := defaultSelector
	hook := auditHook
	selectorMu.RUnlock()
	engine := sel.EngineFor(op)
	if hook != nil {
		hook(op, engine)
	}
	trace := os.Getenv("YAWR_EXPR_TRACE")
	if truthyEnv(trace) {
		writeSelectionAudit(op, engine)
	}
	return engine
}

func writeSelectionAudit(op Operation, engine Engine) {
	record := struct {
		Event  string `json:"event"`
		Kind   string `json:"kind"`
		Engine string `json:"engine"`
		Source string `json:"source,omitempty"`
	}{
		Event:  "expr.engine.selected",
		Kind:   op.Kind.String(),
		Engine: engine.String(),
		Source: op.Source,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	log.Printf("yawr: %s", data)
}

func setSelectorForTest(sel Selector) func() {
	selectorMu.Lock()
	prev := defaultSelector
	defaultSelector = sel
	selectorMu.Unlock()
	return func() {
		selectorMu.Lock()
		defaultSelector = prev
		selectorMu.Unlock()
	}
}

func setAuditHookForTest(hook func(Operation, Engine)) func() {
	selectorMu.Lock()
	prev := auditHook
	auditHook = hook
	selectorMu.Unlock()
	return func() {
		selectorMu.Lock()
		auditHook = prev
		selectorMu.Unlock()
	}
}
