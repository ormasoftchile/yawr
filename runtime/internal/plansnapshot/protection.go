package plansnapshot

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// CanonicalProtectionArtifact removes only redaction definitions from a
// validated immutable execution-plan snapshot before protected-value scans.
func CanonicalProtectionArtifact(encoded []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var snapshot any
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, err
	}
	root, ok := snapshot.(map[string]any)
	if !ok {
		return nil, errors.New("plan snapshot: protection artifact is not an object")
	}
	stripRedactRules(root["governance"])
	if steps, ok := root["steps"].([]any); ok {
		for _, value := range steps {
			step, _ := value.(map[string]any)
			kind, _ := step["kind"].(string)
			stripSpecRedactions(kind, step["spec"])
		}
	}
	if metadata, ok := root["metadata"].(map[string]any); ok {
		if resolutions, ok := metadata["DynamicIncludes"].([]any); ok {
			for _, value := range resolutions {
				resolution, _ := value.(map[string]any)
				stripRedactRules(resolution["resolved_governance"])
				if closure, ok := resolution["executable_closure"].(map[string]any); ok {
					stripFlowRedactions(closure["nodes"])
				}
			}
		}
	}
	return json.Marshal(snapshot)
}

// CanonicalDynamicIncludeProtectionArtifact removes only policy definitions
// from a typed dynamic-resolution pin before protected-value scans.
func CanonicalDynamicIncludeProtectionArtifact(pin schema.LockedDynamicInclude) (json.RawMessage, error) {
	encoded, err := json.Marshal(pin)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var artifact any
	if err := decoder.Decode(&artifact); err != nil {
		return nil, err
	}
	root, ok := artifact.(map[string]any)
	if !ok {
		return nil, errors.New("plan snapshot: dynamic include protection artifact is not an object")
	}
	stripRedactRules(root["resolved_governance"])
	if closure, ok := root["executable_closure"].(map[string]any); ok {
		stripFlowRedactions(closure["nodes"])
	}
	return json.Marshal(artifact)
}

func stripRedactRules(value any) {
	if governance, ok := value.(map[string]any); ok {
		delete(governance, "redact")
	}
}

func stripSpecRedactions(kind string, value any) {
	spec, ok := value.(map[string]any)
	if !ok {
		return
	}
	switch kind {
	case "include":
		stripRedactRules(spec["resolved_governance"])
		stripFlowRedactions(spec["resolved_steps"])
	case "branch":
		stripBranchRedactions(spec["branches"])
	case "iterate", "compensate":
		stripFlowRedactions(spec["steps"])
	case "parallel":
		stripBranchRedactions(spec["branches"])
	}
}

func stripBranchRedactions(value any) {
	branches, _ := value.([]any)
	for _, branchValue := range branches {
		branch, _ := branchValue.(map[string]any)
		stripFlowRedactions(branch["steps"])
	}
}

func stripFlowRedactions(value any) {
	nodes, _ := value.([]any)
	for _, nodeValue := range nodes {
		node, _ := nodeValue.(map[string]any)
		if step, ok := node["step"].(map[string]any); ok {
			kind, _ := step["spec_kind"].(string)
			stripSpecRedactions(kind, step["spec"])
		}
		if iterate, ok := node["iterate"].(map[string]any); ok {
			stripSpecRedactions("iterate", iterate)
		}
		if parallel, ok := node["parallel"].(map[string]any); ok {
			stripSpecRedactions("parallel", parallel)
		}
	}
}
