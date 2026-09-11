// Package regions defines the region manifest schema embedded in a
// runbook as a top-level `regions:` block.
//
// A region is a kit-declared grouping of yawr nodes that together
// represent one domain operation (e.g. an `ops.approval` from the DRI
// kit). The engine ignores regions; renderers and the report
// generator consume them to display domain-shaped views over the
// underlying yawr structure.
//
// The contract is defined in specs/regions-v1.md.
package regions

// SchemaVersion is the current region manifest schema version.
// Bump on breaking changes.
const SchemaVersion = "1"

// Display controls how a renderer should present a region by default.
type Display string

const (
	// DisplayCollapsed renders the region as a single domain node;
	// members are hidden until the user expands. Default.
	DisplayCollapsed Display = "collapsed"

	// DisplayExpanded renders the region's members inline but keeps a
	// region boundary the user can collapse.
	DisplayExpanded Display = "expanded"

	// DisplayAlwaysExpanded renders the region's members inline with no
	// collapse affordance. Useful for thin regions that exist only for
	// reporting.
	DisplayAlwaysExpanded Display = "always-expanded"
)

// StatusRule names a function that aggregates member statuses into a
// region status. "default" uses the table in the spec; kit-defined
// names are resolved by the renderer at display time.
type StatusRule string

const (
	StatusRuleDefault StatusRule = "default"
)

// Manifest is the top-level `regions:` block embedded in a runbook.
type Manifest struct {
	SchemaVersion string   `yaml:"schema_version" json:"schema_version"`
	Regions       []Region `yaml:"regions"        json:"regions"`
}

// Region is one kit-declared grouping of yawr nodes.
//
// All node-id fields refer to yawr step IDs in the same runbook. The
// validator (pkg/schema/regions/validator) enforces that every ID
// resolves and that the four constraints in specs/regions-v1.md hold.
type Region struct {
	// ID is unique within the manifest.
	ID string `yaml:"id" json:"id"`

	// Kit is the slug of the source kit (e.g. "dri", "home").
	Kit string `yaml:"kit" json:"kit"`

	// OpType is the kit-defined op type (e.g. "ops.approval").
	OpType string `yaml:"op_type" json:"op_type"`

	// OpID is the kit-side identifier of the source op.
	OpID string `yaml:"op_id" json:"op_id"`

	// Label is the human-readable, kit-localized title.
	Label string `yaml:"label" json:"label"`

	// Members lists the yawr step IDs belonging to this region.
	// May be empty only when SkipReason is set.
	Members []string `yaml:"members" json:"members"`

	// Entries lists members reachable from outside the region (or from
	// the runbook's entry point). May be empty for dead-code regions.
	Entries []string `yaml:"entries" json:"entries"`

	// Exits lists members with at least one outbound edge leaving the
	// region (or terminating the runbook). Plural is normal — branches
	// commonly produce multiple exits.
	Exits []string `yaml:"exits" json:"exits"`

	// Display controls default rendering. Empty means DisplayCollapsed.
	Display Display `yaml:"display,omitempty" json:"display,omitempty"`

	// StatusRule names the aggregation function. Empty means
	// StatusRuleDefault.
	StatusRule StatusRule `yaml:"status_rule,omitempty" json:"status_rule,omitempty"`

	// SkipReason explains why Members is empty (e.g. "severity=emergency").
	// Required when Members is empty; forbidden otherwise.
	SkipReason string `yaml:"skip_reason,omitempty" json:"skip_reason,omitempty"`
}
