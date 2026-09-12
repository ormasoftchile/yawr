package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"gopkg.in/yaml.v3"
)

// PresentationDescriptor describes display syntax, never execution semantics.
type PresentationDescriptor struct {
	Version  int64  `yaml:"version" json:"version"`
	Kind     string `yaml:"kind" json:"kind"`
	Language string `yaml:"language" json:"language"`
}

var presentationLanguage = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func (p *PresentationDescriptor) Validate() error {
	if p == nil || p.Version < 1 || p.Version > 9007199254740991 || p.Kind != "code" || !presentationLanguage.MatchString(p.Language) {
		return errors.New("invalid presentation descriptor")
	}
	return nil
}

func (p *PresentationDescriptor) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode || len(n.Content) != 6 {
		return errors.New("invalid presentation descriptor")
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		if seen[k] || (k != "version" && k != "kind" && k != "language") || (k == "version" && v.Tag != "!!int") || (k != "version" && v.Tag != "!!str") {
			return errors.New("invalid presentation descriptor")
		}
		seen[k] = true
	}
	type plain PresentationDescriptor
	var out plain
	if err := n.Decode(&out); err != nil {
		return errors.New("invalid presentation descriptor")
	}
	*p = PresentationDescriptor(out)
	return p.Validate()
}

func (p *PresentationDescriptor) UnmarshalJSON(data []byte) error {
	type plain PresentationDescriptor
	var out plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return errors.New("invalid presentation descriptor")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid presentation descriptor")
	}
	// Count keys independently so duplicate declarations cannot be collapsed.
	d = json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return errors.New("invalid presentation descriptor")
	}
	seen := map[string]bool{}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return errors.New("invalid presentation descriptor")
		}
		s, ok := k.(string)
		if !ok || seen[s] {
			return errors.New("invalid presentation descriptor")
		}
		seen[s] = true
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return errors.New("invalid presentation descriptor")
		}
	}
	if len(seen) != 3 {
		return errors.New("invalid presentation descriptor")
	}
	*p = PresentationDescriptor(out)
	return p.Validate()
}

func (a *ArgDef) UnmarshalYAML(n *yaml.Node) error {
	type plain ArgDef
	var out plain
	if err := n.Decode(&out); err != nil {
		return err
	}
	*a = ArgDef(out)
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "presentation" && a.Presentation == nil {
			return errors.New("invalid presentation descriptor")
		}
	}
	return a.ValidatePresentation()
}

func (a *ArgDef) ValidatePresentation() error {
	if a.Presentation == nil {
		return nil
	}
	if a.Type != "string" {
		return errors.New("presentation requires string type")
	}
	return a.Presentation.Validate()
}

func (a *ArgDef) UnmarshalJSON(data []byte) error {
	type plain ArgDef
	var out plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid argument declaration")
	}
	*a = ArgDef(out)
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return errors.New("invalid argument declaration")
	}
	if _, ok := fields["presentation"]; ok && a.Presentation == nil {
		return errors.New("invalid presentation descriptor")
	}
	return a.ValidatePresentation()
}

func (p *PresentationDescriptor) Availability() (string, string) {
	if p == nil {
		return "unavailable", "missing-descriptor"
	}
	if p.Validate() != nil {
		return "unavailable", "invalid-descriptor"
	}
	if p.Version != 1 {
		return "unsupported", "unsupported-version"
	}
	switch p.Language {
	case "sql", "kql", "powershell":
		return "resolved", ""
	default:
		return "unsupported", "unsupported-language"
	}

}

func (t *ToolDef) ValidatePresentations() error {
	if t == nil {
		return nil
	}
	for _, action := range t.Actions {
		if action == nil {
			continue
		}
		for _, fields := range []map[string]*ArgDef{action.Args, action.Outputs} {
			for _, field := range fields {
				if field != nil {
					if err := field.ValidatePresentation(); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
