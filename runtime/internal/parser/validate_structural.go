package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	schemasdata "github.com/ormasoftchile/yawr/runtime/schemas"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const schemaID = "https://github.com/ormasoftchile/yawr/runtime/schemas/runbook.schema.json"

// compiledSchema is the compiled JSON Schema, loaded once at parser construction.
type compiledSchema = jsonschema.Schema

// compileSchema parses and compiles the embedded runbook JSON Schema.
func compileSchema() (*jsonschema.Schema, error) {
	// jsonschema/v6 requires AddResource to receive a parsed Go value, not a reader.
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemasdata.RunbookSchema))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema JSON: %w", err)
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaID, schemaDoc); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}
	sch, err := c.Compile(schemaID)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return sch, nil
}

// validateStructural validates raw YAML bytes against the compiled JSON Schema.
// It converts YAML → Go map → JSON → UnmarshalJSON for schema validation.
func validateStructural(sch *jsonschema.Schema, yamlData []byte) ValidationErrors {
	// Decode YAML to a generic Go value.
	var yamlObj interface{}
	if err := yaml.Unmarshal(yamlData, &yamlObj); err != nil {
		return ValidationErrors{verr("yaml/parse", "", err.Error())}
	}

	// Round-trip through JSON to normalise yaml.v3 types (e.g. map[interface{}]interface{})
	// to JSON-compatible types (map[string]interface{}).
	jsonBytes, err := json.Marshal(normalizeYAML(yamlObj))
	if err != nil {
		return ValidationErrors{verr("json/marshal", "", err.Error())}
	}

	// Use jsonschema.UnmarshalJSON so the value is in the form the validator expects.
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(jsonBytes))
	if err != nil {
		return ValidationErrors{verr("json/decode", "", err.Error())}
	}

	if err := sch.Validate(inst); err != nil {
		return schemaErrorsToValidation(err)
	}
	return nil
}

// normalizeYAML recursively converts yaml.v3's types to JSON-compatible types.
func normalizeYAML(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, v2 := range t {
			out[k] = normalizeYAML(v2)
		}
		return out
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, v2 := range t {
			out[fmt.Sprintf("%v", k)] = normalizeYAML(v2)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, v2 := range t {
			out[i] = normalizeYAML(v2)
		}
		return out
	default:
		return v
	}
}

// enumAwareCode maps a leaf jsonschema.ValidationError to the correct
// ENUM-00x code (AR-ENUM-3/§12 error catalog) when the failure occurs
// underneath an `enum:` keyword in the schema (any InstanceLocation segment
// equal to "enum"); otherwise falls back to the generic
// "schema/structural" code used for every other structural violation.
// keyword->code: type/minItems (enum itself not an array, empty, or an
// item not of type string) -> ENUM-002; pattern/minLength (whitespace
// boundary) -> ENUM-003; uniqueItems (literal duplicate) -> ENUM-004.
func enumAwareCode(e *jsonschema.ValidationError) string {
	underEnum := false
	for _, seg := range e.InstanceLocation {
		if seg == "enum" {
			underEnum = true
			break
		}
	}
	if !underEnum {
		return "schema/structural"
	}
	kp := e.ErrorKind.KeywordPath()
	if len(kp) == 0 {
		return "schema/structural"
	}
	switch kp[len(kp)-1] {
	case "type", "minItems":
		return "ENUM-002"
	case "pattern", "minLength":
		return "ENUM-003"
	case "uniqueItems":
		return "ENUM-004"
	}
	return "schema/structural"
}

// schemaErrorsToValidation converts a jsonschema validation error into
// a slice of *ValidationError. jsonschema/v6 returns a *jsonschema.ValidationError
// which may contain nested errors.
func schemaErrorsToValidation(err error) ValidationErrors {
	if err == nil {
		return nil
	}
	var ve ValidationErrors

	var flatten func(e *jsonschema.ValidationError)
	flatten = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			// Leaf error — extract location and message.
			field := strings.Join(e.InstanceLocation, ".")
			msg := e.Error()
			// Strip the verbose prefix that jsonschema/v6 adds.
			if idx := strings.Index(msg, ": "); idx >= 0 {
				msg = msg[idx+2:]
			}
			ve = append(ve, verr(enumAwareCode(e), field, msg))
		}
		for _, c := range e.Causes {
			flatten(c)
		}
	}

	if jse, ok := err.(*jsonschema.ValidationError); ok {
		flatten(jse)
	} else {
		ve = append(ve, verr("schema/structural", "", err.Error()))
	}
	return ve
}
