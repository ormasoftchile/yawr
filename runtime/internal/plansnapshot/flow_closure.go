package plansnapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

const FlowClosureSchemaV3 = "execution-flow-closure/v3"
const FlowClosureSchemaV4 = schema.ScopedFlowClosureVersion
const DynamicIncludePinSchemaV2 = "yawr.dynamic-include-pin/v2"
const FrozenToolSubstitutionSchemaV2 = "yawr.frozen-tool-substitution/v2"

type flowClosureV1 struct {
	RootScopeID        string                     `json:"root_scope_id,omitempty"`
	ScopeContextDigest string                     `json:"scope_context_digest,omitempty"`
	SchemaVersion      string                     `json:"schema_version"`
	ClosureDigest      string                     `json:"closure_digest"`
	Nodes              []flowNodeSnapshotV1       `json:"nodes"`
	Tools              map[string]*schema.ToolDef `json:"tools,omitempty"`
	Invocation         *schema.RunbookInvocation  `json:"invocation,omitempty"`
}

func EncodeFlowClosure(nodes []schema.FlowNode, tools ...map[string]*schema.ToolDef) (json.RawMessage, error) {
	if err := planner.ValidateScopedFlow(nodes, nil, ""); err != nil {
		return nil, err
	}
	snapshot, err := snapshotFlowNodes(nodes)
	if err != nil {
		return nil, fmt.Errorf("plan snapshot: encode flow closure: %w", err)
	}
	closure := flowClosureV1{SchemaVersion: FlowClosureSchemaV3, Nodes: snapshot}
	if len(tools) > 0 {
		closure.Tools = tools[0]
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
	if closure.SchemaVersion != FlowClosureSchemaV3 || closure.ClosureDigest == "" {
		return nil, errors.New("plan snapshot: invalid flow closure header")
	}
	if closure.RootScopeID != "" || closure.ScopeContextDigest != "" || closure.Invocation != nil {
		return nil, errors.New("plan snapshot: v3 closure cannot carry lexical scopes")
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
	if err := planner.ValidateScopedFlow(nodes, nil, ""); err != nil {
		return nil, err
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

func ValidateDynamicIncludePin(pin schema.LockedDynamicInclude, sets ...*toolscope.Set) error {
	var scopes *toolscope.Set
	if len(sets) > 0 {
		scopes = sets[0]
	}
	if scopes != nil {
		if pin.SchemaVersion != DynamicIncludePinSchemaV2 || !scopes.HasScope(pin.TargetScopeID) {
			return errors.New("plan snapshot: invalid scoped dynamic pin version or target")
		}
		target, targetErr := scopes.Target(pin.QualifiedID)
		if targetErr != nil || target.ScopeID != pin.TargetScopeID || target.SourceDigest != pin.FileDigest {
			return errors.New("plan snapshot: dynamic target identity mismatch")
		}
		if _, err := RestoreScopedFlowClosure(pin.ExecutableClosure, scopes, pin.TargetScopeID); err != nil {
			return err
		}
		var closure flowClosureV1
		if err := decodeStrictJSON(pin.ExecutableClosure, &closure); err != nil {
			return err
		}
		if closure.ClosureDigest != target.ClosureDigest {
			return errors.New("plan snapshot: dynamic target closure differs from frozen catalog")
		}
		if pin.AbsPath != target.AbsPath || pin.RunbookID != target.RunbookID || pin.RunbookName != target.RunbookName ||
			pin.RunbookContentHash != target.ContentHash || pin.PackageName != target.PackageName ||
			pin.PackageVersion != target.PackageVersion || pin.PackageDigest != target.PackageDigest {
			return errors.New("plan snapshot: dynamic pin identity differs from captured target")
		}
		type metadata struct {
			Inputs     map[string]*schema.Input  `json:"inputs,omitempty"`
			Bindings   []schema.Binding          `json:"bindings,omitempty"`
			Outputs    map[string]*schema.Output `json:"outputs,omitempty"`
			Governance *schema.GovernanceConfig  `json:"governance,omitempty"`
		}
		pinMetadata, err := json.Marshal(metadata{pin.ResolvedInputs, pin.ResolvedBindings, pin.ResolvedOutputs, pin.ResolvedGovernance})
		if err != nil {
			return err
		}
		targetMetadata, err := json.Marshal(metadata{target.Inputs, target.Bindings, target.Outputs, target.Governance})
		if err != nil {
			return err
		}
		if string(pinMetadata) != string(targetMetadata) {
			return errors.New("plan snapshot: dynamic pin metadata differs from captured target")
		}
	} else if pin.SchemaVersion != "" || pin.TargetScopeID != "" {
		return errors.New("plan snapshot: scoped dynamic pin requires immutable scopes")
	}
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
	if scopes == nil {
		if err := ValidateFlowClosure(pin.ExecutableClosure); err != nil {
			return fmt.Errorf("plan snapshot: invalid dynamic include closure: %w", err)
		}
	}
	return nil
}

func ValidateFrozenToolSubstitution(frozen *schema.FrozenToolSubstitution, sets ...*toolscope.Set) error {
	if frozen == nil || frozen.RunbookPath == "" || frozen.RunbookID == "" ||
		!validRawSHA256Digest(frozen.RunbookContentHash) {
		return errors.New("plan snapshot: invalid frozen tool substitution")
	}
	var scopes *toolscope.Set
	if len(sets) > 0 {
		scopes = sets[0]
	}
	var flow []schema.FlowNode
	var err error
	if scopes != nil {
		if frozen.SchemaVersion != FrozenToolSubstitutionSchemaV2 || !scopes.HasScope(frozen.TargetScopeID) {
			return errors.New("plan snapshot: invalid scoped substitution version or target")
		}
		flow, err = RestoreScopedFlowClosure(frozen.ExecutableClosure, scopes, frozen.TargetScopeID)
		if err == nil {
			invocation, invocationErr := ScopedInvocationFromClosure(frozen.ExecutableClosure, scopes, frozen.TargetScopeID)
			if invocationErr != nil {
				return invocationErr
			}
			if !invocationDeclarationsMatch(invocation, frozen.Bindings, frozen.Outputs) {
				return errors.New("plan snapshot: substitution invocation differs from frozen declarations")
			}
		}
	} else {
		if frozen.SchemaVersion != "" || frozen.TargetScopeID != "" {
			return errors.New("plan snapshot: scoped substitution requires immutable scopes")
		}
		flow, err = RestoreFlowClosure(frozen.ExecutableClosure)
	}
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
	if closure.SchemaVersion == FlowClosureSchemaV4 {
		nodes, err := json.Marshal(closure.Nodes)
		if err != nil {
			return "", err
		}
		if len(closure.Tools) != 0 {
			return "", errors.New("plan snapshot: scoped closure cannot contain name-only tools")
		}
		return schema.ScopedFlowClosureDigest(schema.ScopedFlowClosure{
			RootScopeID: closure.RootScopeID, ScopeContextDigest: closure.ScopeContextDigest,
			SchemaVersion: closure.SchemaVersion, Nodes: nodes, Invocation: closure.Invocation,
		})
	}
	closure.ClosureDigest = ""
	encoded, err := json.Marshal(closure)
	if err != nil {
		return "", fmt.Errorf("plan snapshot: digest flow closure: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest), nil
}
