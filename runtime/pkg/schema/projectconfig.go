package schema

// PackageRequirement is a single "requires:" entry — the sole, canonical
// mechanism by which a project or a runbook declares a dependency on a tool
// package (design/yawr/sections/06-tool-runtime.tex §requires: is
// canonical). It may appear at the top level of .yawr/config.yaml (project
// scope, where path is REQUIRED in the MVP) or at the top level of any
// runbook (runbook scope, where path is optional if the project already
// supplies it).
type PackageRequirement struct {
	Package string `yaml:"package"        json:"package"`
	Version string `yaml:"version"        json:"version"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
}

// ProjectConfig is the parsed representation of .yawr/config.yaml
// (apiVersion: yawr.config/v1). See
// spec/schema-candidates/yawr-private-1990a6c/project-config.v1.schema.json and
// design/yawr/sections/06-tool-runtime.tex §Project binding.
type ProjectConfig struct {
	Schema     string                `yaml:"$schema,omitempty" json:"$schema,omitempty"`
	APIVersion string                `yaml:"apiVersion"        json:"apiVersion"`
	Requires   []*PackageRequirement `yaml:"requires,omitempty" json:"requires,omitempty"`
	ToolPaths  []string              `yaml:"tool-paths,omitempty" json:"tool-paths,omitempty"`
	Defaults   map[string]any        `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Tools      map[string]any        `yaml:"tools,omitempty"    json:"tools,omitempty"`
}

// ProjectConfigAPIVersion is the required apiVersion discriminator for
// .yawr/config.yaml.
const ProjectConfigAPIVersion = "yawr.config/v1"
