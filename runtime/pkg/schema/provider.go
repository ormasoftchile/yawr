package schema

// ProviderDef is the parsed definition of a .provider.yaml file.
type ProviderDef struct {
	APIVersion  string                    `yaml:"apiVersion"            json:"apiVersion"`
	Name        string                    `yaml:"name"                  json:"name"`
	Description string                    `yaml:"description,omitempty" json:"description,omitempty"`
	Transport   TransportConfig           `yaml:"transport"             json:"transport"`
	Fields      map[string]*ProviderField `yaml:"fields,omitempty"      json:"fields,omitempty"`
	Metadata    map[string]string         `yaml:"metadata,omitempty"    json:"metadata,omitempty"`
}

// ProviderField declares a single field that a provider can supply.
type ProviderField struct {
	Type        string `yaml:"type"                 json:"type"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty"   json:"required,omitempty"`
}
