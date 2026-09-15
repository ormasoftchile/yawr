package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

const BoundDefinitionVersion = "yawr.bound-tool-definition/v1"

type BoundDefinition struct {
	Runtime        ToolDef         `json:"runtime"`
	Declaration    *schema.ToolDef `json:"declaration,omitempty"`
	SourceDigest   string          `json:"source_digest,omitempty"`
	PackageVersion string          `json:"package_version,omitempty"`
}

type BoundInvocation struct {
	ScopeID      string          `json:"scope_id"`
	BindingID    string          `json:"binding_id"`
	DefinitionID string          `json:"definition_id"`
	LogicalName  string          `json:"logical_name"`
	Action       string          `json:"action"`
	Definition   BoundDefinition `json:"definition"`
}

type BoundToolRuntime interface {
	InvokeBound(context.Context, BoundInvocation, map[string]any) (*ToolResult, error)
}

type boundDefinitionAlias BoundDefinition

type boundDefinitionRecord struct {
	Version       string                        `json:"version"`
	Definition    boundDefinitionAlias          `json:"definition"`
	SchemaActions map[string]*schema.ToolAction `json:"schema_actions,omitempty"`
}

func boundRecord(definition BoundDefinition) boundDefinitionRecord {
	record := boundDefinitionRecord{Version: BoundDefinitionVersion, Definition: boundDefinitionAlias(definition),
		SchemaActions: map[string]*schema.ToolAction{}}
	for name, action := range definition.Runtime.Actions {
		if declaration := action.SchemaAction(); declaration != nil {
			record.SchemaActions[name] = declaration
		}
	}
	return record
}

func (definition BoundDefinition) MarshalJSON() ([]byte, error) {
	return json.Marshal(boundRecord(definition))
}

func (definition *BoundDefinition) UnmarshalJSON(body []byte) error {
	record, err := decodeBoundRecord(body)
	if err != nil {
		return err
	}
	*definition = BoundDefinition(record.Definition)
	return nil
}

func decodeBoundRecord(body []byte) (boundDefinitionRecord, error) {
	var record boundDefinitionRecord
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return boundDefinitionRecord{}, errkit.Wrap("SCOPE-002", "tool definition cannot be restored", err)
	}
	if record.Version != BoundDefinitionVersion {
		return boundDefinitionRecord{}, errkit.New("SCOPE-002", fmt.Sprintf("unsupported bound definition version %q", record.Version))
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return boundDefinitionRecord{}, errkit.New("SCOPE-002", "unexpected trailing bound definition data")
	}
	for name := range record.SchemaActions {
		if record.Definition.Runtime.Actions[name] == nil {
			return boundDefinitionRecord{}, errkit.New("SCOPE-002", fmt.Sprintf("retained schema action %q has no runtime action", name))
		}
	}
	for name, action := range record.Definition.Runtime.Actions {
		if action != nil {
			action.WithSchemaAction(record.SchemaActions[name])
		}
	}
	return record, nil
}

func cloneBoundRecord(definition BoundDefinition) (boundDefinitionRecord, error) {
	body, err := json.Marshal(boundRecord(definition))
	if err != nil {
		return boundDefinitionRecord{}, errkit.Wrap("SCOPE-002", "tool definition cannot be frozen", err)
	}
	return decodeBoundRecord(body)
}

func CloneBoundDefinition(definition BoundDefinition) (BoundDefinition, error) {
	record, err := cloneBoundRecord(definition)
	return BoundDefinition(record.Definition), err
}

// DefinitionID excludes generated substitution closures to avoid recursive
// definition -> scope -> substitution -> definition hashes. The containing
// scope snapshot must hash and validate those executable closures separately.
func DefinitionID(definition BoundDefinition) (string, error) {
	record, err := cloneBoundRecord(definition)
	if err != nil {
		return "", err
	}
	for _, action := range record.SchemaActions {
		if action != nil {
			action.FrozenSubstitution = nil
		}
	}
	if record.Definition.Declaration != nil {
		for _, action := range record.Definition.Declaration.Actions {
			if action != nil {
				action.FrozenSubstitution = nil
			}
		}
	}
	body, err := json.Marshal(record)
	if err != nil {
		return "", errkit.Wrap("SCOPE-002", "tool definition identity cannot be encoded", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body)), nil
}

func BindingID(scopeID, logicalName, definitionID string) string {
	body, _ := json.Marshal([4]string{"yawr.tool-binding/v1", scopeID, logicalName, definitionID})
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}

func ValidateBoundInvocation(invocation BoundInvocation) error {
	if invocation.ScopeID == "" || invocation.LogicalName == "" || invocation.Action == "" {
		return errkit.New("SCOPE-002", "bound invocation requires scope, local tool name, and action")
	}
	definitionID, err := DefinitionID(invocation.Definition)
	if err != nil {
		return err
	}
	if invocation.DefinitionID != definitionID ||
		invocation.BindingID != BindingID(invocation.ScopeID, invocation.LogicalName, definitionID) {
		return errkit.New("SCOPE-002", "bound invocation identity does not match its scope and definition")
	}
	if invocation.Definition.Runtime.Actions[invocation.Action] == nil {
		return errkit.New("SCOPE-002", fmt.Sprintf("bound tool %q does not declare action %q", invocation.LogicalName, invocation.Action))
	}
	return nil
}
