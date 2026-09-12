package extension

// ExtensionManifest is parsed from yawr-extension.yaml.
type ExtensionManifest struct {
	Name         string        `yaml:"name"`
	Version      string        `yaml:"version"`
	Entrypoint   string        `yaml:"entrypoint"`
	Capabilities []string      `yaml:"capabilities"`
	Tools        []ToolContrib `yaml:"tools,omitempty"`
}

// ToolContrib declares a tool in an extension manifest.
type ToolContrib struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}
