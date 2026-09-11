package extension

// ExtensionManifest is parsed from yawr-extension.yaml.
type ExtensionManifest struct {
	Name          string        `yaml:"name"`
	Version       string        `yaml:"version"`
	Entrypoint    string        `yaml:"entrypoint"`
	Capabilities  []string      `yaml:"capabilities"`
	Compatibility Compatibility `yaml:"compatibility"`
	Tools         []ToolContrib `yaml:"tools,omitempty"`
}

// Compatibility declares the host version bounds for an extension.
type Compatibility struct {
	YawrMinVersion string `yaml:"yawr_min_version"`
	YawrMaxVersion string `yaml:"yawr_max_version,omitempty"`
}

// ToolContrib declares a tool in an extension manifest.
type ToolContrib struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}
