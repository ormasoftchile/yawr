package replay

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	sharedreplay "github.com/ormasoftchile/yawr/runtime/pkg/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"gopkg.in/yaml.v3"
)

// Scenario holds pre-recorded responses for replay mode.
type Scenario struct {
	Commands               []CommandFixture                      `yaml:"commands"`
	Evidence               map[string]map[string]EvidenceFixture `yaml:"evidence"`
	Tools                  map[string]ToolFixture                `yaml:"tools"`
	SourceRunID            string                                `yaml:"source_run_id,omitempty"`
	DependencyLock         sharedreplay.DependencyLock           `yaml:"dependency_lock,omitempty"`
	ExpectedTransitions    []sharedreplay.ExpectedTransition     `yaml:"expected_transitions,omitempty"`
	StepResponses          []sharedreplay.StepBinding            `yaml:"step_responses,omitempty"`
	HostActionResponses    []sharedreplay.HostActionBinding      `yaml:"host_action_responses,omitempty"`
	InteractionAnswers     []sharedreplay.InteractionBinding     `yaml:"interaction_answers,omitempty"`
	Approvals              []sharedreplay.ApprovalBinding        `yaml:"approvals,omitempty"`
	WaitEvents             []WaitEventBinding                    `yaml:"wait_events,omitempty"`
	DynamicIncludeNotFound []DynamicIncludeNotFoundFixture       `yaml:"dynamic_include_not_found,omitempty"`
	AllowUnmatched         bool                                  `yaml:"allow_unmatched"`
	PatternFixtures        bool                                  `yaml:"-"`
	StrictExactFixtures    bool                                  `yaml:"-"`
	mu                     *sync.Mutex
}

type WaitEventBinding struct {
	At            sharedreplay.Selector `yaml:"at"`
	EventID       string                `yaml:"event_id"`
	Source        string                `yaml:"source"`
	Channel       string                `yaml:"channel,omitempty"`
	Payload       map[string]any        `yaml:"payload,omitempty"`
	FilterMatched string                `yaml:"filter_matched,omitempty"`
	Provenance    sharedreplay.Source   `yaml:"provenance"`
	Review        sharedreplay.Review   `yaml:"review"`
}

type DynamicIncludeNotFoundFixture struct {
	StepID          string                               `yaml:"step_id"`
	QualifiedNodeID string                               `yaml:"qualified_node_id"`
	Invocation      int                                  `yaml:"invocation"`
	StructuralPath  []schema.DynamicIncludeFrameIdentity `yaml:"structural_path,omitempty"`
	RenderedRef     string                               `yaml:"rendered_ref"`
	Reason          string                               `yaml:"reason"`
	matched         bool
}

// CommandFixture is a recorded CLI invocation output.
type CommandFixture struct {
	Argv     []string `yaml:"argv"`
	Stdout   string   `yaml:"stdout"`
	Stderr   string   `yaml:"stderr"`
	ExitCode int      `yaml:"exit_code"`
	matched  bool
}

// EvidenceFixture captures manual evidence for replay.
type EvidenceFixture struct {
	Kind  string            `yaml:"kind"`
	Value string            `yaml:"value,omitempty"`
	Items map[string]string `yaml:"items,omitempty"`
}

// ToolFixture captures tool response outputs for replay.
type ToolFixture struct {
	Response string `yaml:"response"`
	ExitCode int    `yaml:"exit_code"`
}

// LoadScenario reads and parses a scenario YAML file.
func LoadScenario(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var s Scenario
	if err := decoder.Decode(&s); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("replay: scenario must contain exactly one document")
		}
		return nil, err
	}
	if err := validatePersistedReplayFixtures(s); err != nil {
		return nil, err
	}
	hasExact := len(s.StepResponses) > 0 || len(s.HostActionResponses) > 0 ||
		len(s.InteractionAnswers) > 0 || len(s.Approvals) > 0 || len(s.WaitEvents) > 0
	s.PatternFixtures = !hasExact && (len(s.Commands) > 0 || len(s.Tools) > 0 || s.AllowUnmatched)
	s.StrictExactFixtures = !s.PatternFixtures
	s.ensureMutex()
	return &s, nil
}

func validatePersistedReplayFixtures(scenario Scenario) error {
	hasExact := len(scenario.StepResponses) > 0 || len(scenario.HostActionResponses) > 0 ||
		len(scenario.InteractionAnswers) > 0 || len(scenario.Approvals) > 0 || len(scenario.WaitEvents) > 0 ||
		len(scenario.ExpectedTransitions) > 0
	if hasExact && (scenario.DependencyLock.PlanHash == "" || scenario.DependencyLock.GraphHash == "" ||
		scenario.DependencyLock.CatalogDigest == "" || scenario.DependencyLock.PackageLockDigest == "" ||
		scenario.DependencyLock.ToolDigest == "" || scenario.DependencyLock.ProfileDigest == "") {
		return errors.New("replay: exact scenario requires complete plan, graph, catalog, package, tool, and profile dependency locks")
	}
	validate := func(name string, source sharedreplay.Source, review sharedreplay.Review) error {
		if source.Kind != "prior-run" || source.RunID == "" {
			return errors.New("replay: " + name + " requires prior-run provenance")
		}
		if scenario.SourceRunID != "" && source.RunID != scenario.SourceRunID {
			return errors.New("replay: " + name + " source run does not match scenario")
		}
		if review.State != "reviewed" || review.ReviewedBy == "" || !review.SensitivityReviewed {
			return errors.New("replay: " + name + " must be sensitivity-reviewed")
		}
		if _, err := time.Parse(time.RFC3339, review.ReviewedAt); err != nil {
			return errors.New("replay: " + name + " review timestamp is invalid")
		}
		return nil
	}
	for _, fixture := range scenario.StepResponses {
		if err := validate("step response", fixture.Source, fixture.Review); err != nil {
			return err
		}
	}
	for _, fixture := range scenario.HostActionResponses {
		if fixture.Capability == "" {
			return errors.New("replay: host-action response capability is required")
		}
		if err := validate("host-action response", fixture.Source, fixture.Review); err != nil {
			return err
		}
	}
	for _, fixture := range scenario.InteractionAnswers {
		if err := validate("interaction answer", fixture.Source, fixture.Review); err != nil {
			return err
		}
	}
	for _, fixture := range scenario.Approvals {
		if err := validate("approval", fixture.Source, fixture.Review); err != nil {
			return err
		}
	}
	for _, fixture := range scenario.WaitEvents {
		if err := validate("wait event", fixture.Provenance, fixture.Review); err != nil {
			return err
		}
	}
	for _, transition := range scenario.ExpectedTransitions {
		if err := validateReplaySelector(transition.At); err != nil ||
			transition.TargetRunbook == "" || transition.ReasonCode == "" {
			return errors.New("replay: expected transition is invalid")
		}
	}
	return nil
}

// MatchCommand returns the first unmatched fixture whose argv matches.
// Commands are matched in declaration order (first match wins).
func (s *Scenario) MatchCommand(argv []string) (*CommandFixture, bool) {
	mutex := s.ensureMutex()
	mutex.Lock()
	defer mutex.Unlock()
	for i := range s.Commands {
		if s.Commands[i].matched {
			continue
		}
		if argvEqual(s.Commands[i].Argv, argv) {
			s.Commands[i].matched = true
			return &s.Commands[i], true
		}
	}
	return nil, false
}

func (s *Scenario) MatchDynamicIncludeNotFound(
	stepID string,
	qualifiedNodeID string,
	invocation int,
	renderedRef string,
	structuralPath []schema.DynamicIncludeFrameIdentity,
) (*DynamicIncludeNotFoundFixture, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	mutex := s.ensureMutex()
	mutex.Lock()
	defer mutex.Unlock()
	for index := range s.DynamicIncludeNotFound {
		fixture := &s.DynamicIncludeNotFound[index]
		if fixture.matched || fixture.StepID != stepID || fixture.QualifiedNodeID != qualifiedNodeID ||
			fixture.Invocation != invocation || fixture.RenderedRef != renderedRef ||
			!sameDynamicStructuralPath(fixture.StructuralPath, structuralPath) {
			continue
		}
		if fixture.StepID == "" || fixture.QualifiedNodeID == "" || fixture.Invocation < 1 ||
			fixture.RenderedRef == "" || fixture.Reason == "" {
			return nil, false, errors.New("replay: dynamic include not-found fixture is incomplete")
		}
		fixture.matched = true
		return fixture, true, nil
	}
	return nil, false, nil
}

func (s *Scenario) ensureMutex() *sync.Mutex {
	if s.mu == nil {
		s.mu = &sync.Mutex{}
	}
	return s.mu
}

func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
