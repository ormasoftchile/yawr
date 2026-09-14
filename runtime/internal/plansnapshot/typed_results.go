package plansnapshot

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func hasTypedFeatures(bindings []schema.Binding, outputs map[string]*schema.Output, steps []ResolvedStepSnapshotV1) bool {
	if len(bindings) > 0 {
		return true
	}
	for _, output := range outputs {
		if output != nil && output.ValueTreePresent {
			return true
		}
	}
	for _, step := range steps {
		for _, source := range step.Capture {
			if strings.TrimSpace(source) == "outputs" {
				return true
			}
		}
		if step.Kind == "assign" || step.Kind == "results" {
			return true
		}
		var spec any
		decoder := json.NewDecoder(bytes.NewReader(step.Spec))
		decoder.UseNumber()
		if decoder.Decode(&spec) == nil && typedJSONMetadata(spec) {
			return true
		}
	}
	return false
}

func typedJSONMetadata(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		for name, value := range current {
			if name == "publish_results" && value == true {
				return true
			}
			switch name {
			case "value", "args", "default", "collect_values", "request", "with", "vars", "Vars", "enum", "metadata":
				continue
			}
			if name == "value_tree_present" {
				return true
			}
			if name == "value_tree" {
				if _, declaration := current["type"]; declaration {
					return true
				}
			}
			if name == "capture" {
				if captures, ok := value.(map[string]any); ok {
					for _, source := range captures {
						if source, ok := source.(string); ok && strings.TrimSpace(source) == "outputs" {
							return true
						}
					}
				}
				continue
			}
			if (name == "spec_kind" || name == "kind" || name == "type") && (value == "assign" || value == "results") {
				return true
			}

			if name == "bindings" || name == "resolved_bindings" {
				if _, ok := value.([]any); ok || value == nil {
					return true
				}
			}
			if typedJSONMetadata(value) {
				return true
			}
		}
	case []any:
		for _, value := range current {
			if typedJSONMetadata(value) {
				return true
			}
		}
	}
	return false
}

func hasTypedMetadata(value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var native any
	if json.Unmarshal(data, &native) != nil {
		return false
	}
	return typedJSONMetadata(native)
}
