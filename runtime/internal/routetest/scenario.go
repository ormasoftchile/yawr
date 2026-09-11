package routetest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	replaypkg "github.com/ormasoftchile/yawr/runtime/pkg/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/sensitive"
	"gopkg.in/yaml.v3"
)

const APIVersion = "yawr.route-test/v1"

// Scenario is the internal route-test artifact consumed by the scheduler.
type Scenario struct {
	APIVersion          string               `yaml:"apiVersion,omitempty" json:"apiVersion,omitempty"`
	ID                  string               `yaml:"id,omitempty" json:"id,omitempty"`
	Name                string               `yaml:"name,omitempty" json:"name,omitempty"`
	Runbook             string               `yaml:"runbook,omitempty" json:"runbook,omitempty"`
	PlanHash            string               `yaml:"plan_hash,omitempty" json:"plan_hash,omitempty"`
	SensitivityReviewed bool                 `yaml:"sensitivity_reviewed" json:"sensitivity_reviewed"`
	Target              Selector             `yaml:"target" json:"target"`
	Inputs              map[string]string    `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	StepResponses       []StepBinding        `yaml:"step_responses,omitempty" json:"step_responses,omitempty"`
	HostActionResponses []HostActionBinding  `yaml:"host_action_responses,omitempty" json:"host_action_responses,omitempty"`
	InteractionAnswers  []InteractionBinding `yaml:"interaction_answers,omitempty" json:"interaction_answers,omitempty"`
	TestApprovals       []ApprovalBinding    `yaml:"test_approvals,omitempty" json:"test_approvals,omitempty"`
	LastResult          *SavedResult         `yaml:"last_result,omitempty" json:"last_result,omitempty"`
}

type SavedResult struct {
	Status             string `yaml:"status" json:"status"`
	TargetReached      bool   `yaml:"target_reached" json:"target_reached"`
	ExternalDispatches int    `yaml:"external_dispatches" json:"external_dispatches"`
	RanAt              string `yaml:"ran_at" json:"ran_at"`
	ConditionsDigest   string `yaml:"conditions_digest" json:"conditions_digest"`
}

// Route tests use the scheduler's shared reviewed replay contracts.
type Selector = replaypkg.Selector
type Review = replaypkg.Review
type Source = replaypkg.Source
type HostActionResponse = replaypkg.HostActionResponse
type HostActionBinding = replaypkg.HostActionBinding
type InteractionBinding = replaypkg.InteractionBinding
type StepBinding = replaypkg.StepBinding
type ApprovalBinding = replaypkg.ApprovalBinding

// LoadScenario reads and validates one route-test artifact.
func LoadScenario(path string) (Scenario, error) {
	file, err := os.Open(path)
	if err != nil {
		return Scenario{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil {
		return Scenario{}, err
	}
	if len(data) > 1024*1024 {
		return Scenario{}, fmt.Errorf("route test: artifact exceeds 1 MiB")
	}
	return ParseScenario(data)
}

// ParseScenario parses one strict yawr.route-test/v1 document.
func ParseScenario(data []byte) (Scenario, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var scenario Scenario
	if err := decoder.Decode(&scenario); err != nil {
		return Scenario{}, fmt.Errorf("route test: parse artifact: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Scenario{}, fmt.Errorf("route test: artifact must contain one document")
		}
		return Scenario{}, fmt.Errorf("route test: parse artifact: %w", err)
	}
	if scenario.APIVersion != APIVersion {
		return Scenario{}, fmt.Errorf("route test: apiVersion must be %q", APIVersion)
	}
	if scenario.Runbook == "" {
		return Scenario{}, fmt.Errorf("route test: runbook is required")
	}
	if scenario.PlanHash == "" {
		return Scenario{}, fmt.Errorf("route test: plan_hash is required")
	}
	if !scenario.SensitivityReviewed {
		return Scenario{}, fmt.Errorf("route test: sensitivity_reviewed must be true before persistence")
	}
	if scenario.LastResult != nil {
		if _, err := time.Parse(time.RFC3339, scenario.LastResult.RanAt); err != nil {
			return Scenario{}, fmt.Errorf("route test: last_result.ran_at must be RFC3339")
		}
		if scenario.LastResult.ExternalDispatches < 0 {
			return Scenario{}, fmt.Errorf("route test: last_result.external_dispatches cannot be negative")
		}
		if scenario.LastResult.ConditionsDigest == "" {
			return Scenario{}, fmt.Errorf("route test: last_result.conditions_digest is required")
		}
		digest, err := conditionsDigest(scenario)
		if err != nil {
			return Scenario{}, err
		}
		if scenario.LastResult.ConditionsDigest != digest {
			return Scenario{}, fmt.Errorf("route test: last_result conditions digest %q does not match %q", scenario.LastResult.ConditionsDigest, digest)
		}
		switch scenario.LastResult.Status {
		case "reached":
			if !scenario.LastResult.TargetReached || scenario.LastResult.ExternalDispatches != 0 {
				return Scenario{}, fmt.Errorf("route test: reached result requires target_reached and zero external dispatches")
			}
		case "route-changed", "runtime-failed", "safety-failed", "stopped":
		default:
			return Scenario{}, fmt.Errorf("route test: last_result.status is not supported")
		}
	}
	for _, binding := range scenario.StepResponses {
		if err := validatePersistedSource("step response", binding.Source, false); err != nil {
			return Scenario{}, err
		}
	}
	for _, binding := range scenario.HostActionResponses {
		if err := validatePersistedSource("host action response", binding.Source, false); err != nil {
			return Scenario{}, err
		}
	}
	for _, binding := range scenario.InteractionAnswers {
		if err := validatePersistedSource("interaction answer", binding.Source, binding.Kind == "collector"); err != nil {
			return Scenario{}, err
		}
	}
	for _, binding := range scenario.TestApprovals {
		if err := validatePersistedSource("test approval", binding.Source, false); err != nil {
			return Scenario{}, err
		}
	}
	if err := rejectSensitiveRouteTestData(scenario); err != nil {
		return Scenario{}, err
	}
	if _, err := NewScheduler(scenario); err != nil {
		return Scenario{}, err
	}
	return scenario, nil
}

func conditionsDigest(scenario Scenario) (string, error) {
	scenario.LastResult = nil
	encoded, err := json.Marshal(scenario)
	if err != nil {
		return "", fmt.Errorf("route test: encode conditions: %w", err)
	}
	var canonical any
	if err := json.Unmarshal(encoded, &canonical); err != nil {
		return "", fmt.Errorf("route test: canonicalize conditions: %w", err)
	}
	encoded, err = json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("route test: encode canonical conditions: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest), nil
}

func rejectSensitiveRouteTestData(scenario Scenario) error {
	for name := range scenario.Inputs {
		if sensitiveRouteTestKey(name) {
			return fmt.Errorf("route test: sensitive input key %q cannot be persisted", name)
		}
	}
	for _, binding := range scenario.StepResponses {
		if name := sensitiveKeyInValue(binding.Output); name != "" {
			return fmt.Errorf("route test: sensitive result key %q cannot be persisted", name)
		}
		if err := validatePersistedValue(binding.Output); err != nil {
			return fmt.Errorf("route test: saved step result: %w", err)
		}
	}
	for _, binding := range scenario.HostActionResponses {
		if name := sensitiveKeyInValue(binding.Response.Result); name != "" {
			return fmt.Errorf("route test: sensitive host result key %q cannot be persisted", name)
		}
		if err := validatePersistedValue(binding.Response.Result); err != nil {
			return fmt.Errorf("route test: saved host result: %w", err)
		}
	}
	for _, binding := range scenario.InteractionAnswers {
		if name := sensitiveKeyInValue(binding.Values); name != "" {
			return fmt.Errorf("route test: sensitive answer key %q cannot be persisted", name)
		}
		if err := validatePersistedValue(binding.Values); err != nil {
			return fmt.Errorf("route test: saved interaction answer: %w", err)
		}
	}
	return nil
}

func validatePersistedValue(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("must be JSON-compatible: %w", err)
	}
	if len(encoded) > 64*1024 {
		return fmt.Errorf("exceeds 64 KiB")
	}
	nodes := 0
	return validatePersistedNode(value, 1, &nodes)
}

func validatePersistedNode(value any, depth int, nodes *int) error {
	if depth > 8 {
		return fmt.Errorf("exceeds depth 8")
	}
	*nodes++
	if *nodes > 512 {
		return fmt.Errorf("exceeds 512 values")
	}
	switch typed := value.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return nil
	case string:
		if len(typed) > 4096 {
			return fmt.Errorf("contains a string over 4096 bytes")
		}
		if strings.ContainsAny(typed, "\r\n") {
			return fmt.Errorf("contains multiline raw output; store an evidence reference instead")
		}
		return nil
	case []any:
		if len(typed) > 64 {
			return fmt.Errorf("contains an array over 64 items")
		}
		for _, item := range typed {
			if err := validatePersistedNode(item, depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if len(typed) > 64 {
			return fmt.Errorf("contains an object over 64 properties")
		}
		for _, item := range typed {
			if err := validatePersistedNode(item, depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("contains unsupported value %T", value)
	}
}

func sensitiveKeyInValue(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if sensitiveRouteTestKey(key) {
				return key
			}
			if nested := sensitiveKeyInValue(item); nested != "" {
				return nested
			}
		}
	case []any:
		for _, item := range typed {
			if nested := sensitiveKeyInValue(item); nested != "" {
				return nested
			}
		}
	}
	return ""
}

func sensitiveRouteTestKey(key string) bool {
	return sensitive.Name(key)
}

func validatePersistedSource(name string, source Source, answerDigest bool) error {
	switch source.Kind {
	case "manual":
		return nil
	case "prior-run":
		if source.RunID == "" || source.InteractionID == "" || source.CopiedAt == "" {
			return fmt.Errorf("route test: %s prior-run source requires run_id, interaction_id, and copied_at", name)
		}
		if _, err := time.Parse(time.RFC3339, source.CopiedAt); err != nil {
			return fmt.Errorf("route test: %s copied_at must be RFC3339", name)
		}
		if answerDigest && source.AnswerDigest == "" {
			return fmt.Errorf("route test: %s prior-run source requires answer_digest", name)
		}
		return nil
	default:
		return fmt.Errorf("route test: %s source kind must be manual or prior-run", name)
	}
}
