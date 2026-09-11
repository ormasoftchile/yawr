package runstore

import (
	"encoding/json"
	"fmt"
)

func validateOldTypedEnvelope(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range []string{"TypedState", "ResultsID"} {
		if _, exists := raw[key]; exists {
			return fmt.Errorf("runstore: typed state fields require run-state/v3")
		}
	}
	checkSteps := func(data json.RawMessage) error {
		var steps map[string]map[string]json.RawMessage
		if len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, &steps); err != nil {
			return err
		}
		for _, step := range steps {
			for _, key := range []string{"ResultsID", "PublicOutputs", "RequiredFailure"} {
				if _, exists := step[key]; exists {
					return fmt.Errorf("runstore: typed step fields require run-state/v3")
				}
			}
		}
		return nil
	}
	if err := checkSteps(raw["DurableStepResults"]); err != nil {
		return err
	}
	var frames map[string]map[string]json.RawMessage
	if data := raw["DurableExecutionFrames"]; len(data) > 0 {
		if err := json.Unmarshal(data, &frames); err != nil {
			return err
		}
	}
	for _, frame := range frames {
		if _, exists := frame["run_results_id"]; exists {
			return fmt.Errorf("runstore: typed frame fields require run-state/v3")
		}
		if err := checkSteps(frame["results"]); err != nil {
			return err
		}
	}
	var resolutions map[string]struct {
		Pin map[string]json.RawMessage `json:"pin"`
	}
	if data := raw["DynamicIncludes"]; len(data) > 0 {
		if err := json.Unmarshal(data, &resolutions); err != nil {
			return err
		}
	}
	for _, resolution := range resolutions {
		if _, exists := resolution.Pin["resolved_bindings"]; exists {
			return fmt.Errorf("runstore: typed pin fields require run-state/v3")
		}
		var outputs map[string]map[string]json.RawMessage
		if data := resolution.Pin["resolved_outputs"]; len(data) > 0 {
			if err := json.Unmarshal(data, &outputs); err != nil {
				return err
			}
			for _, output := range outputs {
				for _, key := range []string{"value_tree", "value_tree_present"} {
					if _, exists := output[key]; exists {
						return fmt.Errorf("runstore: typed output fields require run-state/v3")
					}
				}
			}
		}
	}
	return nil
}
