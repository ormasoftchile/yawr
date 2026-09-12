package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const schemaID = "https://yawr.internal/conformance/yawr.vector-schema/v1"

// LoadSuite loads and schema-validates the Phase P1 corpus from dir.
func LoadSuite(ctx context.Context, dir string) (Suite, error) {
	if err := ctx.Err(); err != nil {
		return Suite{}, err
	}
	schema, err := compileSchema(filepath.Join(filepath.Dir(dir), "vector.schema.json"))
	if err != nil {
		return Suite{}, err
	}
	out := Suite{Files: make([]VectorFile, 0, len(FileNames))}
	for _, name := range FileNames {
		if err := ctx.Err(); err != nil {
			return Suite{}, err
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return Suite{}, fmt.Errorf("read vector file %s: %w", name, err)
		}
		if err := validateYAML(schema, data); err != nil {
			return Suite{}, fmt.Errorf("schema-validate %s: %w", name, err)
		}
		vf, err := decodeVectorFile(name, data)
		if err != nil {
			return Suite{}, err
		}
		out.Files = append(out.Files, vf)
	}
	return out, nil
}

func compileSchema(path string) (*jsonschema.Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaID, doc); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}
	sch, err := c.Compile(schemaID)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return sch, nil
}

func validateYAML(schema *jsonschema.Schema, data []byte) error {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	jsonBytes, err := json.Marshal(normalizeYAML(raw))
	if err != nil {
		return fmt.Errorf("marshal YAML as JSON: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("unmarshal JSON instance: %w", err)
	}
	if err := schema.Validate(inst); err != nil {
		return err
	}
	return nil
}

func decodeVectorFile(name string, data []byte) (VectorFile, error) {
	var wrapper struct {
		Vectors []Vector `yaml:"vectors"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return VectorFile{}, fmt.Errorf("decode vector file %s: %w", name, err)
	}
	for i := range wrapper.Vectors {
		wrapper.Vectors[i].SourceFile = name
		if !wrapper.Vectors[i].Expected.WantsError() && wrapper.Vectors[i].Expected.HasValue {
			val, err := pjvm.FromAny(wrapper.Vectors[i].Expected.Value)
			if err != nil {
				return VectorFile{}, fmt.Errorf("decode expected PJVM value for %s: %w", wrapper.Vectors[i].ID, err)
			}
			wrapper.Vectors[i].Expected.PJVMValue = &val
		}
	}
	return VectorFile{Name: name, Vectors: wrapper.Vectors}, nil
}

func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = normalizeYAML(v)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[fmt.Sprintf("%v", k)] = normalizeYAML(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = normalizeYAML(v)
		}
		return out
	default:
		return v
	}
}
