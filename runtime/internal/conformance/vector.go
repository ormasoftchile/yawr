package conformance

import (
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
	"gopkg.in/yaml.v3"
)

// FileNames is the ordered Phase P1 conformance corpus.
var FileNames = []string{
	"tv-gxl-eval.yaml",
	"tv-gxl-parse.yaml",
	"tv-gxl-path.yaml",
	"tv-gis-path.yaml",
	"tv-gcp-path.yaml",
}

// Suite is a loaded set of conformance vectors.
type Suite struct {
	Files []VectorFile
}

// VectorFile contains vectors loaded from one corpus file.
type VectorFile struct {
	Name    string
	Vectors []Vector
}

// Vector is one schema-validated conformance test vector.
type Vector struct {
	ID          string         `yaml:"id" json:"id"`
	Category    string         `yaml:"category" json:"category"`
	Description string         `yaml:"description" json:"description"`
	Input       string         `yaml:"input" json:"input"`
	Variables   map[string]any `yaml:"variables" json:"variables"`
	Expected    Expected       `yaml:"expected" json:"expected"`
	Note        string         `yaml:"note,omitempty" json:"note,omitempty"`
	Tags        []string       `yaml:"tags,omitempty" json:"tags,omitempty"`
	SourceFile  string         `yaml:"-" json:"-"`
}

// Expected describes either a success or error expectation.
type Expected struct {
	Value      any    `yaml:"value,omitempty" json:"value,omitempty"`
	Type       string `yaml:"type,omitempty" json:"type,omitempty"`
	Assert     string `yaml:"assert,omitempty" json:"assert,omitempty"`
	Pattern    string `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	ErrorClass string `yaml:"error_class,omitempty" json:"error_class,omitempty"`
	ErrorCode  string `yaml:"error_code,omitempty" json:"error_code,omitempty"`
	Timing     string `yaml:"timing,omitempty" json:"timing,omitempty"`
	HasValue   bool   `yaml:"-" json:"-"`
	PJVMValue  *pjvm.Value
}

// UnmarshalYAML decodes Expected while preserving explicit value:null.
func (e *Expected) UnmarshalYAML(node *yaml.Node) error {
	type expected Expected
	var decoded expected
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "value" {
			decoded.HasValue = true
			break
		}
	}
	*e = Expected(decoded)
	return nil
}

// WantsError reports whether the vector expects an error.
func (e Expected) WantsError() bool { return e.ErrorClass != "" || e.ErrorCode != "" }

// Count returns the total vector count in the suite.
func (s Suite) Count() int {
	total := 0
	for _, file := range s.Files {
		total += len(file.Vectors)
	}
	return total
}
