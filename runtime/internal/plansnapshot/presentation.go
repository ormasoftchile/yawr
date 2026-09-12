package plansnapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// HasPresentation traverses definition tables, whose keys are arbitrary names,
// and frozen executable closures. Argument/default/output values are opaque.
func HasPresentation(value any) bool {
	var tools map[string]*schema.ToolDef
	switch v := value.(type) {
	case map[string]*schema.ToolDef:
		tools = v
	case *schema.ToolDef:
		tools = map[string]*schema.ToolDef{"": v}
	case []schema.LockedDynamicInclude:
		return hasDynamicPresentation(v)
	default:
		return false
	}
	data, err := json.Marshal(tools)
	if err != nil {
		return false
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return false
	}
	return toolsHavePresentation(decoded)
}

func jsonObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

// encoding/json matches schema field names case-insensitively. Inspect every
// matching spelling so an ambiguous schema object cannot conceal metadata.
// This matching never applies to user-defined table keys.
func schemaFields(value any, name string) []any {
	var fields []any
	for key, field := range jsonObject(value) {
		if strings.EqualFold(key, name) {
			fields = append(fields, field)
		}
	}
	return fields
}

func toolsHavePresentation(value any) bool {
	for _, tool := range jsonObject(value) {
		for _, actions := range schemaFields(tool, "actions") {
			for _, action := range jsonObject(actions) {
				for _, site := range []string{"args", "outputs"} {
					for _, fields := range schemaFields(action, site) {
						for _, field := range jsonObject(fields) {
							if len(schemaFields(field, "presentation")) > 0 {
								return true
							}
						}
					}
				}
				for _, frozen := range schemaFields(action, "frozen_substitution") {
					for _, closure := range schemaFields(frozen, "executable_closure") {
						if closureHasPresentation(closure) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func closureHasPresentation(value any) bool {
	for _, tools := range schemaFields(value, "tools") {
		if toolsHavePresentation(tools) {
			return true
		}
	}
	return false
}

func dynamicIncludesHavePresentation(value any) bool {
	pins, _ := value.([]any)
	for _, pin := range pins {
		for _, closure := range schemaFields(pin, "executable_closure") {
			if closureHasPresentation(closure) {
				return true
			}
		}
	}
	return false
}

func hasDynamicPresentation(pins []schema.LockedDynamicInclude) bool {
	data, err := json.Marshal(pins)
	if err != nil {
		return false
	}
	var decoded any
	return json.Unmarshal(data, &decoded) == nil && dynamicIncludesHavePresentation(decoded)
}

// Preflight rejects ambiguous version envelopes before any state mutation.
func Preflight(data []byte) error {
	if len(data) > 64<<20 {
		return errors.New("plan snapshot: size limit exceeded")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	value, err := uniqueJSON(d, 0)
	if err != nil {
		return err
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("plan snapshot: trailing JSON")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("plan snapshot: invalid envelope")
	}
	version, ok := object["schema_version"].(string)
	if !ok || version != SchemaVersionV3 {
		return errors.New("plan snapshot: unsupported schema version")
	}
	return nil
}
func ValidateUniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if _, err := uniqueJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("snapshot: trailing JSON")
	}
	return nil
}

func uniqueJSON(d *json.Decoder, depth int) (any, error) {
	if depth > 256 {
		return nil, errors.New("plan snapshot: nesting limit")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			s, ok := k.(string)
			if !ok {
				return nil, errors.New("plan snapshot: invalid key")
			}
			if _, ok := m[s]; ok {
				return nil, errors.New("plan snapshot: duplicate key")
			}
			v, err := uniqueJSON(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[s] = v
		}
		_, err = d.Token()
		return m, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := uniqueJSON(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token()
		return a, err
	default:
		return t, nil
	}
}
