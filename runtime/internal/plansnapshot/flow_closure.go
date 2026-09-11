package plansnapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

const FlowClosureSchemaV1 = "yawr.execution-flow-closure/v1"
const FlowClosureSchemaV2 = "execution-flow-closure/v2"
const FlowClosureSchemaV3 = "execution-flow-closure/v3"

type flowClosureV1 struct {
	SchemaVersion string                     `json:"schema_version"`
	ClosureDigest string                     `json:"closure_digest"`
	Nodes         []flowNodeSnapshotV1       `json:"nodes"`
	Tools         map[string]*schema.ToolDef `json:"tools,omitempty"`
}

func EncodeFlowClosure(nodes []schema.FlowNode, tools ...map[string]*schema.ToolDef) (json.RawMessage, error) {
	snapshot, err := snapshotFlowNodes(nodes)
	if err != nil {
		return nil, fmt.Errorf("plan snapshot: encode flow closure: %w", err)
	}
	closure := flowClosureV1{SchemaVersion: FlowClosureSchemaV1, Nodes: snapshot}
	if len(tools) > 0 && (HasPresentation(tools[0]) || hasTypedMetadata(tools[0])) {
		closure.SchemaVersion = FlowClosureSchemaV2
		closure.Tools = tools[0]
		if hasTypedMetadata(tools[0]) {
			closure.SchemaVersion = FlowClosureSchemaV3
		}
	}
	for _, node := range snapshot {
		encoded, err := json.Marshal(node)
		if err != nil {
			return nil, err
		}
		var value any
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, err
		}
		if typedJSONMetadata(value) {
			closure.SchemaVersion = FlowClosureSchemaV3
		}
	}
	digest, err := flowClosureDigest(closure)
	if err != nil {
		return nil, err
	}
	closure.ClosureDigest = digest
	encoded, err := json.Marshal(closure)
	if err != nil {
		return nil, fmt.Errorf("plan snapshot: encode flow closure: %w", err)
	}
	return encoded, nil
}

func RestoreFlowClosure(encoded json.RawMessage) ([]schema.FlowNode, error) {
	if len(encoded) == 0 || !json.Valid(encoded) {
		return nil, errors.New("plan snapshot: flow closure is required")
	}
	var closure flowClosureV1
	if err := decodeStrictJSON(encoded, &closure); err != nil {
		return nil, fmt.Errorf("plan snapshot: decode flow closure: %w", err)
	}
	if (closure.SchemaVersion != FlowClosureSchemaV1 && closure.SchemaVersion != FlowClosureSchemaV2 && closure.SchemaVersion != FlowClosureSchemaV3) || closure.ClosureDigest == "" ||
		(closure.SchemaVersion == FlowClosureSchemaV1 && len(closure.Tools) != 0) {
		return nil, errors.New("plan snapshot: invalid flow closure header")
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	if closure.SchemaVersion != FlowClosureSchemaV3 && typedJSONMetadata(value) {
		return nil, errors.New("plan snapshot: typed features require execution-flow-closure/v3")
	}
	digest, err := flowClosureDigest(closure)
	if err != nil {
		return nil, err
	}
	if digest != closure.ClosureDigest {
		return nil, errors.New("plan snapshot: flow closure digest mismatch")
	}
	nodes, err := restoreFlowNodes(closure.Nodes)
	if err != nil {
		return nil, fmt.Errorf("plan snapshot: restore flow closure: %w", err)
	}
	return nodes, nil
}

// RestoreFlowTools reads only definitions committed with this exact closure.
// It never consults today's runtime registry or source catalog.
func RestoreFlowTools(encoded json.RawMessage) (map[string]*schema.ToolDef, error) {
	if _, err := RestoreFlowClosure(encoded); err != nil {
		return nil, err
	}
	var closure flowClosureV1
	if err := decodeStrictJSON(encoded, &closure); err != nil {
		return nil, err
	}
	return closure.Tools, nil
}

func ValidateFlowClosure(encoded json.RawMessage) error {
	_, err := RestoreFlowClosure(encoded)
	return err
}

func ValidateDynamicIncludePin(pin schema.LockedDynamicInclude) error {
	if pin.StepID == "" || pin.RenderedRef == "" || pin.QualifiedID == "" || pin.AbsPath == "" ||
		!validSHA256Digest(pin.FileDigest) || !validSHA256Digest(pin.PackageDigest) {
		return errors.New("plan snapshot: invalid dynamic include pin")
	}
	if len(pin.ExecutableClosure) == 0 {
		if pin.ResolvedInputs != nil || pin.ResolvedOutputs != nil || pin.ResolvedGovernance != nil {
			return errors.New("plan snapshot: dynamic include metadata has no executable closure")
		}
		return nil
	}
	if pin.RunbookID == "" || pin.RunbookName == "" || !validRawSHA256Digest(pin.RunbookContentHash) {
		return errors.New("plan snapshot: dynamic include closure has no runbook identity")
	}
	if err := ValidateFlowClosure(pin.ExecutableClosure); err != nil {
		return fmt.Errorf("plan snapshot: invalid dynamic include closure: %w", err)
	}
	return nil
}

func ValidateFrozenToolSubstitution(frozen *schema.FrozenToolSubstitution) error {
	if frozen == nil || frozen.RunbookPath == "" || frozen.RunbookID == "" ||
		!validRawSHA256Digest(frozen.RunbookContentHash) {
		return errors.New("plan snapshot: invalid frozen tool substitution")
	}
	flow, err := RestoreFlowClosure(frozen.ExecutableClosure)
	if err != nil {
		return fmt.Errorf("plan snapshot: invalid frozen tool substitution closure: %w", err)
	}
	if err := validateFlowResumeSafety(flow, "tool-substitution", true); err != nil {
		return fmt.Errorf("plan snapshot: unsafe frozen tool substitution closure: %w", err)
	}
	if err := schema.ValidateTypedRunbook(&schema.Runbook{Inputs: frozen.Inputs, Outputs: frozen.Outputs, Bindings: frozen.Bindings, Flow: flow}); err != nil {
		return fmt.Errorf("plan snapshot: invalid typed tool substitution declarations: %w", err)
	}
	return nil
}

func validRawSHA256Digest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validSHA256Digest(value string) bool {
	hexDigest := strings.TrimPrefix(value, "sha256:")
	if hexDigest == value || len(hexDigest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hexDigest)
	return err == nil
}

func flowClosureDigest(closure flowClosureV1) (string, error) {
	closure.ClosureDigest = ""
	encoded, err := json.Marshal(closure)
	if err != nil {
		return "", fmt.Errorf("plan snapshot: digest flow closure: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest), nil
}
