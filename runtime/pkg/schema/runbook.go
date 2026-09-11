package schema

import (
	"gopkg.in/yaml.v3"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// Runbook is the top-level parsed representation of a yawr runbook document.
type Runbook struct {
	Schema      string                `yaml:"$schema,omitempty"     json:"$schema,omitempty"`
	APIVersion  string                `yaml:"apiVersion"            json:"apiVersion"`
	ID          string                `yaml:"id"                    json:"id"`
	Name        string                `yaml:"name"                  json:"name"`
	Kind        RunbookKind           `yaml:"kind,omitempty"        json:"kind,omitempty"`
	Description string                `yaml:"description,omitempty" json:"description,omitempty"`
	Vars        map[string]any        `yaml:"vars,omitempty"        json:"vars,omitempty"`
	Bindings    []Binding             `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	Inputs      map[string]*Input     `yaml:"inputs,omitempty"      json:"inputs,omitempty"`
	Outputs     map[string]*Output    `yaml:"outputs,omitempty"    json:"outputs,omitempty"`
	ToolRefs    []*ToolRef            `yaml:"toolRefs,omitempty"    json:"toolRefs,omitempty"`
	Requires    []*PackageRequirement `yaml:"requires,omitempty" json:"requires,omitempty"`
	Extensions  []*ExtensionRef       `yaml:"extensions,omitempty"  json:"extensions,omitempty"`
	Imports     map[string]string     `yaml:"imports,omitempty"     json:"imports,omitempty"`
	Defaults    *StepDefaults         `yaml:"defaults,omitempty"    json:"defaults,omitempty"`
	Governance  *GovernanceConfig     `yaml:"governance,omitempty"  json:"governance,omitempty"`
	Prose       map[string]string     `yaml:"prose,omitempty"       json:"prose,omitempty"`
	Metadata    map[string]string     `yaml:"metadata,omitempty"    json:"metadata,omitempty"`
	Flow        []FlowNode            `yaml:"flow"                  json:"flow"`

	// Expand selects how this runbook's include sites are materialized
	// into the execution plan: "" (inherit), "eager", "lazy", or "auto".
	// See pkg/expand for semantics. Per-include `expand:` overrides this.
	Expand string `yaml:"expand,omitempty" json:"expand,omitempty"`

	// Regions is the optional region manifest. When present, validators
	// enforce specs/regions-v1.md. The engine ignores this field.
	Regions *regions.Manifest `yaml:"regions,omitempty" json:"regions,omitempty"`
}

// RunbookKind classifies the runbook's operational purpose.

// ExtensionRef references an extension root directory.
type ExtensionRef struct {
	Name   string   `yaml:"name,omitempty" json:"name,omitempty"`
	Path   string   `yaml:"path" json:"path"`
	Grants []string `yaml:"grants,omitempty" json:"grants,omitempty"`
}

type RunbookKind string

const (
	KindMitigation RunbookKind = "mitigation"
	KindReference  RunbookKind = "reference"
	KindComposable RunbookKind = "composable"
	KindRCA        RunbookKind = "rca"
)

// Input declares a runbook input variable.
type Input struct {
	Type        string `yaml:"type"                 json:"type"`
	Required    bool   `yaml:"required,omitempty"   json:"required,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Default     any    `yaml:"default,omitempty"    json:"default,omitempty"`
	From        string `yaml:"from,omitempty"       json:"from,omitempty"`
	// Enum declares S3's value-domain constraint (AR-ENUM-1..15): valid
	// only when this input resolves to type: string (type absent defaults
	// to string per this struct's existing convention); forbidden on
	// type: secret (ENUM-001, C1).
	Enum EnumConstraint `yaml:"enum,omitempty" json:"enum,omitempty"`
}

// Output declares a runbook output variable.
type Output struct {
	Type        string `yaml:"type"                 json:"type"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Value       string `yaml:"value,omitempty"      json:"value,omitempty"`
	// ValueExpr is an opt-in typed GXL expression, mutually exclusive with Value.
	ValueExpr        string `yaml:"value_expr,omitempty" json:"value_expr,omitempty"`
	ValueTree        any    `yaml:"value_tree,omitempty" json:"value_tree,omitempty"`
	ValueTreePresent bool   `yaml:"-" json:"value_tree_present,omitempty"`
	// Optional marks a substituted runbook output as intentionally absent on
	// some paths. It may only satisfy an action output whose required flag is false.
	Optional bool `yaml:"optional,omitempty" json:"optional,omitempty"`
	// Enum declares S4's value-domain constraint (AR-ENUM-1..15), checked
	// at production time against the resolved outputs.<name>.value (ENUM-009).
	Enum EnumConstraint `yaml:"enum,omitempty" json:"enum,omitempty"`
}

// StepDefaults holds default settings applied to all steps.
type StepDefaults struct {
	Timeout        string `yaml:"timeout,omitempty"          json:"timeout,omitempty"`
	RetryMax       int    `yaml:"retry_max,omitempty"        json:"retry_max,omitempty"`
	ContinueOnFail bool   `yaml:"continue_on_fail,omitempty" json:"continue_on_fail,omitempty"`
}

// GovernanceConfig is the runbook-level governance policy block.
type GovernanceConfig struct {
	RequireApproval bool             `yaml:"require_approval,omitempty" json:"require_approval,omitempty"`
	Rules           []GovernanceRule `yaml:"rules,omitempty"            json:"rules,omitempty"`
	AllowCommands   []string         `yaml:"allow_commands,omitempty"   json:"allow_commands,omitempty"`
	DenyCommands    []string         `yaml:"deny_commands,omitempty"    json:"deny_commands,omitempty"`
	DenyEnvVars     []string         `yaml:"deny_env_vars,omitempty"    json:"deny_env_vars,omitempty"`
	Redact          []RedactRule     `yaml:"redact,omitempty"           json:"redact,omitempty"`
}

// UnmarshalYAML accepts current snake_case spellings and the hyphenated
// spellings used by package-substitution documents. Snake_case wins if both
// are present.
func (g *GovernanceConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain GovernanceConfig
	var current plain
	if err := node.Decode(&current); err != nil {
		return err
	}
	var alt struct {
		RequireApproval bool     `yaml:"require-approval,omitempty"`
		AllowCommands   []string `yaml:"allow-commands,omitempty"`
		DenyCommands    []string `yaml:"deny-commands,omitempty"`
		DenyEnvVars     []string `yaml:"deny-env-vars,omitempty"`
	}
	if err := node.Decode(&alt); err != nil {
		return err
	}
	*g = GovernanceConfig(current)
	if !g.RequireApproval && alt.RequireApproval {
		g.RequireApproval = alt.RequireApproval
	}
	if len(g.AllowCommands) == 0 && len(alt.AllowCommands) > 0 {
		g.AllowCommands = alt.AllowCommands
	}
	if len(g.DenyCommands) == 0 && len(alt.DenyCommands) > 0 {
		g.DenyCommands = alt.DenyCommands
	}
	if len(g.DenyEnvVars) == 0 && len(alt.DenyEnvVars) > 0 {
		g.DenyEnvVars = alt.DenyEnvVars
	}
	return nil
}

// GovernanceRule is a single governance rule entry (action allow/deny/require-approval).
type GovernanceRule struct {
	Effects      []string `yaml:"effects,omitempty"       json:"effects,omitempty"`
	Action       string   `yaml:"action,omitempty"        json:"action,omitempty"`
	MinApprovers int      `yaml:"min_approvers,omitempty" json:"min_approvers,omitempty"`
}

// RedactRule defines a single redaction rule for sensitive output.
// Pattern is a RE2 regex; Replace is the substitution string.
type RedactRule struct {
	Pattern string `yaml:"pattern" json:"pattern"`
	Replace string `yaml:"replace" json:"replace"`
}

// FlowNode is an element in the runbook flow array.
// Exactly one of Step, Iterate, or Parallel will be non-nil.
type FlowNode struct {
	Step     *Step         `yaml:"step,omitempty"     json:"step,omitempty"`
	Iterate  *IterateNode  `yaml:"iterate,omitempty"  json:"iterate,omitempty"`
	Parallel *ParallelNode `yaml:"parallel,omitempty" json:"parallel,omitempty"`
}
