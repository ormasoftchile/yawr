package schema

import "reflect"

// RunbookInvocation is the declaring scope, never a structural flow suffix.
type RunbookInvocation struct {
	Bindings []Binding          `json:"bindings,omitempty"`
	Outputs  map[string]*Output `json:"outputs,omitempty"`
	Results  bool               `json:"results,omitempty"`
}

func InvocationForRunbook(bindings []Binding, outputs map[string]*Output, flow []FlowNode) *RunbookInvocation {
	publishes := false
	for _, node := range flow {
		if node.Step != nil && node.Step.Type == StepTypeResults {
			publishes = true
		}
	}
	return &RunbookInvocation{Bindings: bindings, Outputs: outputs, Results: publishes}
}

func CloneInvocation(invocation *RunbookInvocation) *RunbookInvocation {
	if invocation == nil {
		return nil
	}
	return cloneToolValue(reflect.ValueOf(invocation)).Interface().(*RunbookInvocation)
}
