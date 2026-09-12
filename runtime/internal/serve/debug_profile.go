package serve

import (
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

const debugProfileVersion = "yawr.debug-profile/v1"

// DebugProfile is a root-runbook-scoped collection of reproducible overrides.
type DebugProfile struct {
	Version        string                 `json:"version" yaml:"version"`
	Name           string                 `json:"name" yaml:"name"`
	Root           DebugProfileRoot       `json:"root" yaml:"root"`
	CreatedAgainst string                 `json:"created_against,omitempty" yaml:"created_against,omitempty"`
	Overrides      []DebugProfileOverride `json:"overrides" yaml:"overrides"`
}

type DebugProfileRoot struct {
	Ref string `json:"ref" yaml:"ref"`
	ID  string `json:"id" yaml:"id"`
}

type DebugProfileOverride struct {
	Target DebugProfileTarget `json:"target" yaml:"target"`
	Set    DebugSet           `json:"set" yaml:"set"`
}

type DebugProfileTarget struct {
	CallPath []engine.DebugCallFrame `json:"call_path,omitempty" yaml:"call_path,omitempty"`
	Step     string                  `json:"step" yaml:"step"`
	Phase    engine.DebugPhase       `json:"phase" yaml:"phase"`
}

func validateDebugProfile(profile *DebugProfile) error {
	if profile == nil {
		return nil
	}
	if profile.Version != debugProfileVersion || strings.TrimSpace(profile.Name) == "" ||
		strings.TrimSpace(profile.Root.Ref) == "" || strings.TrimSpace(profile.Root.ID) == "" ||
		strings.TrimSpace(profile.CreatedAgainst) == "" ||
		len(profile.Overrides) == 0 || len(profile.Overrides) > 256 {
		return errInvalidDebugConfig
	}
	for _, override := range profile.Overrides {
		breakpoint := DebugBreakpoint{Step: override.Target.Step, Phase: override.Target.Phase, CallPath: override.Target.CallPath}
		if err := validateDebugRunConfig(DebugRunConfig{Enabled: true, Breakpoints: []DebugBreakpoint{breakpoint}}); err != nil {
			return err
		}
		if override.Target.Phase == engine.DebugPhaseBefore &&
			(override.Set.Status != "" || override.Set.OutputPatch != nil || override.Set.Error != "") {
			return errInvalidDebugConfig
		}
		if err := validateDebugSetForPhase(override.Target.Phase, &override.Set); err != nil {
			return err
		}
	}
	return nil
}

func matchingDebugProfileSet(profile *DebugProfile, snapshot engine.DebugSnapshot) *DebugSet {
	if profile == nil {
		return nil
	}
	for _, override := range profile.Overrides {
		if override.Target.Step != snapshot.Location.StepID || override.Target.Phase != snapshot.Phase ||
			!sameDebugCallPath(override.Target.CallPath, snapshot.Location.CallPath) {
			continue
		}
		set := cloneDebugSet(override.Set)
		return &set
	}
	return nil
}

func sameDebugCallPath(left, right []engine.DebugCallFrame) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].StepID != right[i].StepID {
			return false
		}
	}
	return true
}

func cloneDebugProfile(profile *DebugProfile) *DebugProfile {
	if profile == nil {
		return nil
	}
	clone := *profile
	clone.Overrides = make([]DebugProfileOverride, len(profile.Overrides))
	for i, override := range profile.Overrides {
		clone.Overrides[i] = override
		clone.Overrides[i].Target.CallPath = append([]engine.DebugCallFrame(nil), override.Target.CallPath...)
		clone.Overrides[i].Set = cloneDebugSet(override.Set)
	}
	return &clone
}

func cloneDebugSet(set DebugSet) DebugSet {
	return DebugSet{
		Status:      set.Status,
		OutputPatch: cloneDebugJSONMap(set.OutputPatch),
		Vars:        cloneDebugJSONMap(set.Vars),
		Error:       set.Error,
	}
}

func cloneDebugJSONMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	clone := make(map[string]any, len(values))
	for key, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			clone[key] = cloneDebugJSONMap(typed)
		case []any:
			items := make([]any, len(typed))
			for i, item := range typed {
				if object, ok := item.(map[string]any); ok {
					items[i] = cloneDebugJSONMap(object)
				} else {
					items[i] = item
				}
			}
			clone[key] = items
		default:
			clone[key] = value
		}
	}
	return clone
}
