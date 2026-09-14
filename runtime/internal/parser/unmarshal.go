package parser

import (
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
	"gopkg.in/yaml.v3"
)

// parseRunbook decodes raw YAML bytes into a *schema.Runbook.
//
// Design note: schema.Step has multiple inline spec structs with overlapping
// YAML field names (e.g. "prompt" in ChoiceSpec, CollectorSpec, DecisionSpec).
// yaml.v3 panics with "duplicated key" if it ever encounters schema.Step during
// a Decode call, even transitively (via FlowNode.Step → schema.Step).
//
// Therefore, this package NEVER calls node.Decode() on any type that transitively
// contains schema.Step or schema.FlowNode. All nested-flow structures are decoded
// via raw intermediates that capture steps as raw *yaml.Node children, then
// dispatched through parseFlowNodes.
func parseRunbook(data []byte) (*schema.Runbook, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	if doc.Kind == 0 {
		return nil, fmt.Errorf("empty YAML document")
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, fmt.Errorf("empty YAML document")
		}
		root = doc.Content[0]
	}

	// Decode top-level scalar/object fields (flow omitted — extracted separately).
	var raw rawRunbook
	if err := root.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode runbook fields: %w", err)
	}

	rb := &schema.Runbook{
		Schema:      raw.Schema,
		APIVersion:  raw.APIVersion,
		ID:          raw.ID,
		Name:        raw.Name,
		Kind:        raw.Kind,
		Description: raw.Description,
		Vars:        raw.Vars,
		Bindings:    raw.Bindings,
		Inputs:      raw.Inputs,
		Outputs:     raw.Outputs,
		ToolRefs:    raw.ToolRefs,
		Requires:    raw.Requires,
		Imports:     raw.Imports,
		Defaults:    raw.Defaults,
		Governance:  raw.Governance,
		Prose:       raw.Prose,
		Metadata:    raw.Metadata,
		Regions:     raw.Regions,
	}

	if flowNode := findKey(root, "flow"); flowNode != nil {
		nodes, err := parseFlowNodes(flowNode)
		if err != nil {
			return nil, err
		}
		rb.Flow = nodes
	}

	return rb, nil
}

// rawRunbook mirrors schema.Runbook top-level fields.
// The 'flow' field is intentionally absent — it is extracted separately
// as a raw yaml.Node to avoid schema.Step's duplicate-key panic.
type rawRunbook struct {
	Schema      string                       `yaml:"$schema,omitempty"`
	APIVersion  string                       `yaml:"apiVersion"`
	ID          string                       `yaml:"id"`
	Name        string                       `yaml:"name"`
	Kind        schema.RunbookKind           `yaml:"kind,omitempty"`
	Description string                       `yaml:"description,omitempty"`
	Vars        map[string]any               `yaml:"vars,omitempty"`
	Bindings    []schema.Binding             `yaml:"bindings,omitempty"`
	Inputs      map[string]*schema.Input     `yaml:"inputs,omitempty"`
	Outputs     map[string]*schema.Output    `yaml:"outputs,omitempty"`
	ToolRefs    []*schema.ToolRef            `yaml:"toolRefs,omitempty"`
	Requires    []*schema.PackageRequirement `yaml:"requires,omitempty"`
	Imports     map[string]string            `yaml:"imports,omitempty"`
	Defaults    *schema.StepDefaults         `yaml:"defaults,omitempty"`
	Governance  *schema.GovernanceConfig     `yaml:"governance,omitempty"`
	Prose       map[string]string            `yaml:"prose,omitempty"`
	Metadata    map[string]string            `yaml:"metadata,omitempty"`
	Regions     *regions.Manifest            `yaml:"regions,omitempty"`
}

// ─── Flow node parsing ────────────────────────────────────────────────────────

func parseFlowNodes(seq *yaml.Node) ([]schema.FlowNode, error) {
	if seq.Kind == yaml.DocumentNode {
		if len(seq.Content) == 0 {
			return nil, nil
		}
		seq = seq.Content[0]
	}
	if seq.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("flow must be a sequence, got kind %d", seq.Kind)
	}
	out := make([]schema.FlowNode, 0, len(seq.Content))
	for i, item := range seq.Content {
		fn, err := parseFlowNode(item)
		if err != nil {
			return nil, fmt.Errorf("flow[%d]: %w", i, err)
		}
		out = append(out, fn)
	}
	return out, nil
}

func parseFlowNode(node *yaml.Node) (schema.FlowNode, error) {
	if node.Kind != yaml.MappingNode {
		return schema.FlowNode{}, fmt.Errorf("flow node must be a mapping, got kind %d", node.Kind)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "step":
			s, err := parseStep(node.Content[i+1])
			if err != nil {
				return schema.FlowNode{}, err
			}
			return schema.FlowNode{Step: s}, nil
		case "iterate":
			it, err := parseIterateNode(node.Content[i+1])
			if err != nil {
				return schema.FlowNode{}, fmt.Errorf("iterate: %w", err)
			}
			return schema.FlowNode{Iterate: it}, nil
		case "parallel":
			pn, err := parseParallelNode(node.Content[i+1])
			if err != nil {
				return schema.FlowNode{}, fmt.Errorf("parallel: %w", err)
			}
			return schema.FlowNode{Parallel: pn}, nil
		}
	}
	return schema.FlowNode{}, fmt.Errorf("flow node has no 'step', 'iterate', or 'parallel' key")
}

// ─── Step parsing ──────────────────────────────────────────────────────────────

// parseStep decodes a step yaml.Node using per-type dispatch.
// It never calls node.Decode on any type that transitively contains schema.Step.
func parseStep(node *yaml.Node) (*schema.Step, error) {
	removedFailurePolicy := "continue_" + "on_fail"
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == removedFailurePolicy {
			return nil, fmt.Errorf("step uses removed field %q; use on_error", removedFailurePolicy)
		}
	}
	var raw rawStep
	if err := node.Decode(&raw); err != nil {
		return nil, fmt.Errorf("step common fields: %w", err)
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == "publish_results" {
			if raw.Type != schema.StepTypeEnd || node.Content[index+1].Tag != "!!bool" {
				return nil, fmt.Errorf("step %q: publish_results requires an end step and a boolean", raw.ID)
			}
		}
	}
	if raw.Type == schema.StepTypeAssign || raw.Type == schema.StepTypeResults {
		for index := 0; index+1 < len(node.Content); index += 2 {
			name := node.Content[index].Value
			switch name {
			case "id", "type", "title", "subtitle", "when", "timeout":
				continue
			case "assign":
				if raw.Type == schema.StepTypeAssign {
					continue
				}
			}
			return nil, fmt.Errorf("step %q: %s does not accept %q", raw.ID, raw.Type, name)
		}
	}

	step := &schema.Step{
		ID:              raw.ID,
		Type:            raw.Type,
		Title:           raw.Title,
		Subtitle:        raw.Subtitle,
		When:            raw.When,
		Timeout:         raw.Timeout,
		Delay:           raw.Delay,
		Retry:           raw.Retry,
		Scope:           raw.Scope,
		Export:          raw.Export,
		Capture:         raw.Capture,
		CaptureDefaults: raw.CaptureDefaults,
		Contract:        raw.Contract,
		OnError:         raw.OnError,
	}

	var err error
	switch raw.Type {
	case schema.StepTypeAssign:
		spec := &schema.AssignSpec{}
		if err = node.Decode(spec); err == nil {
			step.AssignSpec = spec
		}
	case schema.StepTypeResults:
		step.ResultsSpec = &schema.ResultsSpec{}
	case schema.StepTypeCLI:
		step.CLI, err = decodeCLISpec(node)

	case schema.StepTypeTool:
		spec := &schema.ToolCallSpec{}
		if err = node.Decode(spec); err == nil {
			step.ToolCall = spec
		}

	case schema.StepTypeInclude:
		spec := &schema.IncludeSpec{}
		if err = node.Decode(spec); err == nil {
			step.IncludeSpec = spec
		}

	case schema.StepTypeChoice:
		step.ChoiceSpec, err = decodeChoiceSpec(node)

	case schema.StepTypeDecision:
		step.DecisionSpec, err = decodeDecisionSpec(node)

	case schema.StepTypeCollector:
		step.CollectorSpec, err = decodeCollectorSpec(node)

	case schema.StepTypeHostAction:
		spec := &schema.HostActionSpec{}
		if err = node.Decode(spec); err == nil {
			step.HostActionSpec = spec
		}

	case schema.StepTypeHandoff:
		spec := &schema.HandoffSpec{}
		if err = node.Decode(spec); err == nil {
			step.HandoffSpec = spec
		}

	case schema.StepTypeBranch:
		step.BranchSpec, err = decodeBranchSpec(node)

	case schema.StepTypeParallel:
		step.ParallelSpec, err = parseParallelNode(node)

	case schema.StepTypeApprove:
		step.ApproveSpec, err = decodeApproveSpec(node)

	case schema.StepTypeAssert:
		spec := &schema.AssertSpec{}
		if err = node.Decode(spec); err == nil {
			step.AssertSpec = spec
		}

	case schema.StepTypeCompensate:
		step.CompensateSpec, err = decodeCompensateSpec(node)

	case schema.StepTypeWaitForEvent:
		spec := &schema.WaitForEventSpec{}
		if err = node.Decode(spec); err == nil {
			step.WaitForEventSpec = spec
		}

	case schema.StepTypeEnd:
		spec := &schema.EndSpec{}
		if err = node.Decode(spec); err == nil {
			step.EndSpec = spec
		}

	case schema.StepTypeDisplay:
		spec := &schema.DisplaySpec{}
		if err = node.Decode(spec); err == nil {
			step.DisplaySpec = spec
		}

	case schema.StepTypeExtension:
		// Extension steps are validated structurally only; their body is opaque to the parser.
	}

	if err != nil {
		return nil, fmt.Errorf("step %q (type %s): %w", raw.ID, raw.Type, err)
	}
	return step, nil
}

// rawStep captures only common step fields.
// Deliberately has no inline specs so yaml.v3 never hits schema.Step's duplicate keys.
type rawStep struct {
	ID              string              `yaml:"id"`
	Type            schema.StepType     `yaml:"type"`
	Title           string              `yaml:"title,omitempty"`
	Subtitle        string              `yaml:"subtitle,omitempty"`
	When            string              `yaml:"when,omitempty"`
	Timeout         string              `yaml:"timeout,omitempty"`
	Delay           string              `yaml:"delay,omitempty"`
	Retry           *schema.RetryConfig `yaml:"retry,omitempty"`
	Scope           string              `yaml:"scope,omitempty"`
	Export          []string            `yaml:"export,omitempty"`
	Capture         map[string]string   `yaml:"capture,omitempty"`
	CaptureDefaults map[string]any      `yaml:"capture_defaults,omitempty"`
	Contract        *schema.Contract    `yaml:"contract,omitempty"`
	OnError         string              `yaml:"on_error,omitempty"`
}

// ─── Type-specific spec decoders ─────────────────────────────────────────────

func decodeCLISpec(node *yaml.Node) (*schema.CLISpec, error) {
	spec := &schema.CLISpec{}
	if err := node.Decode(spec); err != nil {
		return nil, err
	}
	return spec, nil
}

func decodeChoiceSpec(node *yaml.Node) (*schema.ChoiceSpec, error) {
	var r struct {
		Prompt        string                `yaml:"prompt"`
		Options       []schema.ChoiceOption `yaml:"options"`
		Variable      string                `yaml:"variable"`
		Default       string                `yaml:"default,omitempty"`
		Multiple      bool                  `yaml:"multiple,omitempty"`
		MinSelections int                   `yaml:"min_selections,omitempty"`
		MaxSelections int                   `yaml:"max_selections,omitempty"`
		Approvals     *schema.ApprovalGate  `yaml:"approvals,omitempty"`
	}
	if err := node.Decode(&r); err != nil {
		return nil, err
	}
	return &schema.ChoiceSpec{
		Prompt: r.Prompt, Options: r.Options, Variable: r.Variable,
		Default: r.Default, Multiple: r.Multiple,
		MinSelections: r.MinSelections, MaxSelections: r.MaxSelections,
		Approvals: r.Approvals,
	}, nil
}

func decodeDecisionSpec(node *yaml.Node) (*schema.DecisionSpec, error) {
	var r struct {
		Prompt   string                 `yaml:"prompt"`
		Routes   []schema.DecisionRoute `yaml:"routes"`
		Variable string                 `yaml:"variable,omitempty"`
	}
	if err := node.Decode(&r); err != nil {
		return nil, err
	}
	return &schema.DecisionSpec{Prompt: r.Prompt, Routes: r.Routes, Variable: r.Variable}, nil
}

func decodeCollectorSpec(node *yaml.Node) (*schema.CollectorSpec, error) {
	var r struct {
		Prompt    string                  `yaml:"prompt"`
		Fields    []schema.CollectorField `yaml:"fields"`
		Approvals *schema.ApprovalGate    `yaml:"approvals,omitempty"`
	}
	if err := node.Decode(&r); err != nil {
		return nil, err
	}
	return &schema.CollectorSpec{Prompt: r.Prompt, Fields: r.Fields, Approvals: r.Approvals}, nil
}

// decodeBranchSpec decodes a BranchSpec (for branch OR parallel steps).
// Arms are decoded without FlowNode/schema.Step to avoid duplicate-key panic.
func decodeBranchSpec(node *yaml.Node) (*schema.BranchSpec, error) {
	arms, err := decodeBranchArms(node)
	if err != nil {
		return nil, err
	}
	return &schema.BranchSpec{Branches: arms}, nil
}

func decodeBranchArms(stepNode *yaml.Node) ([]schema.BranchArm, error) {
	branchesNode := findKey(stepNode, "branches")
	if branchesNode == nil || branchesNode.Kind != yaml.SequenceNode {
		return nil, nil
	}
	arms := make([]schema.BranchArm, 0, len(branchesNode.Content))
	for i, armNode := range branchesNode.Content {
		arm, err := decodeBranchArm(armNode, i)
		if err != nil {
			return nil, err
		}
		arms = append(arms, arm)
	}
	return arms, nil
}

// rawBranchArm decodes scalar branch arm fields only (no Steps).
type rawBranchArm struct {
	Condition string `yaml:"condition,omitempty"`
	Else      bool   `yaml:"else,omitempty"`
	Label     string `yaml:"label,omitempty"`
}

func decodeBranchArm(node *yaml.Node, idx int) (schema.BranchArm, error) {
	var r rawBranchArm
	if err := node.Decode(&r); err != nil {
		return schema.BranchArm{}, fmt.Errorf("arm[%d]: %w", idx, err)
	}
	if elseNode := findKey(node, "else"); elseNode != nil && elseNode.Tag == "!!null" {
		r.Else = true
	}
	arm := schema.BranchArm{Condition: r.Condition, Else: r.Else, Label: r.Label}
	if stepsNode := findKey(node, "steps"); stepsNode != nil {
		steps, err := parseFlowNodes(stepsNode)
		if err != nil {
			return schema.BranchArm{}, fmt.Errorf("arm[%d].steps: %w", idx, err)
		}
		arm.Steps = steps
	}
	return arm, nil
}

func decodeApproveSpec(node *yaml.Node) (*schema.ApproveSpec, error) {
	var r struct {
		Approvals           schema.ApprovalGate `yaml:"approvals"`
		TimeoutBusinessDays int                 `yaml:"timeout_business_days,omitempty"`
		Timezone            string              `yaml:"timezone,omitempty"`
		BusinessCalendar    string              `yaml:"business_calendar,omitempty"`
		OnTimeout           string              `yaml:"on_timeout,omitempty"`
	}
	if err := node.Decode(&r); err != nil {
		return nil, err
	}
	return &schema.ApproveSpec{
		Approvals: r.Approvals, TimeoutBusinessDays: r.TimeoutBusinessDays,
		Timezone: r.Timezone, BusinessCalendar: r.BusinessCalendar, OnTimeout: r.OnTimeout,
	}, nil
}

func decodeCompensateSpec(node *yaml.Node) (*schema.CompensateSpec, error) {
	compNode := findKey(node, "compensate")
	if compNode == nil {
		return &schema.CompensateSpec{}, nil
	}
	var r struct {
		On string `yaml:"on,omitempty"`
	}
	if err := compNode.Decode(&r); err != nil {
		return nil, err
	}
	cfg := schema.CompensateConfig{On: r.On}
	if stepsNode := findKey(compNode, "steps"); stepsNode != nil {
		steps, err := parseFlowNodes(stepsNode)
		if err != nil {
			return nil, fmt.Errorf("compensate.steps: %w", err)
		}
		cfg.Steps = steps
	}
	return &schema.CompensateSpec{Compensate: cfg}, nil
}

// ─── Iterate and Parallel node parsers ────────────────────────────────────────

// rawIterateNode decodes iterate scalar fields without Steps.
type rawIterateNode struct {
	ID            string            `yaml:"id"`
	Over          string            `yaml:"over,omitempty"`
	As            string            `yaml:"as,omitempty"`
	Max           int               `yaml:"max,omitempty"`
	Until         string            `yaml:"until,omitempty"`
	Concurrency   int               `yaml:"concurrency,omitempty"`
	Collect       map[string]string `yaml:"collect,omitempty"`
	CollectValues map[string]any    `yaml:"collect_values,omitempty"`
}

func parseIterateNode(node *yaml.Node) (*schema.IterateNode, error) {
	var r rawIterateNode
	if err := node.Decode(&r); err != nil {
		return nil, err
	}
	it := &schema.IterateNode{
		ID: r.ID, Over: r.Over, As: r.As, Max: r.Max, Until: r.Until,
		Concurrency: r.Concurrency, Collect: r.Collect, CollectValues: r.CollectValues,
	}
	if stepsNode := findKey(node, "steps"); stepsNode != nil {
		steps, err := parseFlowNodes(stepsNode)
		if err != nil {
			return nil, fmt.Errorf("iterate.steps: %w", err)
		}
		it.Steps = steps
	}
	return it, nil
}

// rawParallelBranch decodes parallel branch label only (no Steps).
type rawParallelBranch struct {
	Label string `yaml:"label,omitempty"`
}

func parseParallelNode(node *yaml.Node) (*schema.ParallelNode, error) {
	var r struct {
		ID   string               `yaml:"id"`
		Join *schema.ParallelJoin `yaml:"join,omitempty"`
	}
	if err := node.Decode(&r); err != nil {
		return nil, err
	}
	pn := &schema.ParallelNode{ID: r.ID, Join: r.Join}

	branchesNode := findKey(node, "branches")
	if branchesNode == nil || branchesNode.Kind != yaml.SequenceNode {
		return pn, nil
	}
	pn.Branches = make([]schema.ParallelBranch, 0, len(branchesNode.Content))
	for i, branchNode := range branchesNode.Content {
		var rawB rawParallelBranch
		if err := branchNode.Decode(&rawB); err != nil {
			return nil, fmt.Errorf("parallel.branches[%d]: %w", i, err)
		}
		branch := schema.ParallelBranch{Label: rawB.Label}
		if stepsNode := findKey(branchNode, "steps"); stepsNode != nil {
			steps, err := parseFlowNodes(stepsNode)
			if err != nil {
				return nil, fmt.Errorf("parallel.branches[%d].steps: %w", i, err)
			}
			branch.Steps = steps
		}
		pn.Branches = append(pn.Branches, branch)
	}
	return pn, nil
}

// ─── Utility ───────────────────────────────────────────────────────────────────

// findKey locates a value node by key in a yaml MappingNode.
func findKey(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
