package schema

import "reflect"

// RunbookInvocation is the declaring scope, never a structural flow suffix.
type RunbookInvocation struct {
	Bindings []Binding          `json:"bindings,omitempty"`
	Outputs  map[string]*Output `json:"outputs,omitempty"`
	Results  bool               `json:"results,omitempty"`
}

func InvocationForRunbook(bindings []Binding, outputs map[string]*Output, flow []FlowNode) *RunbookInvocation {
	return &RunbookInvocation{Bindings: bindings, Outputs: outputs, Results: HasResults(flow)}
}

func CloneInvocation(invocation *RunbookInvocation) *RunbookInvocation {
	if invocation == nil {
		return nil
	}
	return cloneToolValue(reflect.ValueOf(invocation)).Interface().(*RunbookInvocation)
}
