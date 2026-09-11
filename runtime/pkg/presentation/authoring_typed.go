package presentation

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/schemas"
	"gopkg.in/yaml.v3"
)

var typedAuthoringDefinitions = sync.OnceValue(func() map[string]json.RawMessage {
	var root struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if json.Unmarshal(schemas.RunbookSchema, &root) != nil {
		return nil
	}
	return root.Defs
})

func typedAuthoringSchema(name string) *includeAuthoringSchema {
	definitions := typedAuthoringDefinitions()
	var spec includeAuthoringSchema
	if json.Unmarshal(definitions[name], &spec) != nil {
		return nil
	}
	if name == "Step" {
		var discriminator includeAuthoringSchema
		if json.Unmarshal(definitions["StepType"], &discriminator) != nil {
			return nil
		}
		spec.Properties["type"] = &discriminator
	}
	return &spec
}

func completeTypedBindings(s *authoringSource, target *authoringTarget, reply *AuthoringReply, expression string, start, caret int, edit Range, encode func(string) string) {
	if start < 0 || caret < start || caret > len(expression) {
		return
	}
	// GXL's native authoring result supplies the replacement span. Do not offer
	// root bindings at member sites or parse a competing expression language.
	if start > 0 && expression[start-1] == '.' {
		return
	}
	prefix := expression[start:caret]
	limit := int(^uint(0) >> 1)
	if strings.HasPrefix(target.site.YAMLPath, "/bindings/") {
		part := strings.Split(strings.TrimPrefix(target.site.YAMLPath, "/bindings/"), "/")[0]
		if index, err := strconv.Atoi(part); err == nil {
			limit = index
		}
	}
	bindings := mapValue(s.root, "bindings")
	if bindings == nil || bindings.Kind != yaml.SequenceNode {
		return
	}
	for index, binding := range bindings.Content {
		if index >= limit || !uniqueMappingKeys(binding) {
			continue
		}
		name, typ := stringValue(mapValue(binding, "name")), stringValue(mapValue(binding, "type"))
		if name == "" || !strings.HasPrefix(name, prefix) {
			continue
		}
		reply.Items = append(reply.Items, AuthoringItem{ID: "binding:" + name, Kind: "variable", Name: name, ValueType: typ,
			Description: "Invocation binding", Edit: AuthoringEdit{Range: edit, NewText: encode(name)}})
	}
}
