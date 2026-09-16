// Package toolscope owns immutable, file-local tool bindings. It never consults
// a registry or source reader while resolving a frozen invocation.
package toolscope

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

const Version = "yawr.lexical-tool-scopes/v1"
const TargetVersion = "yawr.scoped-dynamic-target/v1"

type Document struct {
	SourceIdentity  string `json:"source_identity"`
	SourceDigest    string `json:"source_digest"`
	PackageIdentity string `json:"package_identity"`
	ScopeID         string `json:"scope_id"`
	StaticEdges     []Edge `json:"static_edges,omitempty"`
}

type Edge struct {
	StepID     string `json:"step_id"`
	DocumentID string `json:"document_id"`
}

type Scope struct {
	DocumentID string            `json:"document_id"`
	Bindings   map[string]string `json:"bindings"`
}

type Binding struct {
	ScopeID             string `json:"scope_id"`
	LogicalName         string `json:"logical_name"`
	DefinitionID        string `json:"definition_id"`
	DeclarationSite     string `json:"declaration_site,omitempty"`
	SelectionProvenance string `json:"selection_provenance,omitempty"`
}

type Target struct {
	SchemaVersion     string                    `json:"schema_version"`
	DocumentID        string                    `json:"document_id"`
	ScopeID           string                    `json:"scope_id"`
	SourceDigest      string                    `json:"source_digest"`
	ClosureDigest     string                    `json:"closure_digest"`
	ExecutableClosure json.RawMessage           `json:"executable_closure"`
	RunbookID         string                    `json:"runbook_id"`
	RunbookName       string                    `json:"runbook_name"`
	ContentHash       string                    `json:"content_hash"`
	AbsPath           string                    `json:"abs_path"`
	PackageName       string                    `json:"package_name"`
	PackageVersion    string                    `json:"package_version"`
	PackageDigest     string                    `json:"package_digest"`
	Inputs            map[string]*schema.Input  `json:"inputs,omitempty"`
	Bindings          []schema.Binding          `json:"bindings,omitempty"`
	Outputs           map[string]*schema.Output `json:"outputs,omitempty"`
	Governance        *schema.GovernanceConfig  `json:"governance,omitempty"`
}

type Snapshot struct {
	Version        string                          `json:"version"`
	Digest         string                          `json:"digest"`
	CatalogDigest  string                          `json:"catalog_digest"`
	ProfileDigest  string                          `json:"profile_digest"`
	Documents      map[string]Document             `json:"documents"`
	Scopes         map[string]Scope                `json:"scopes"`
	Bindings       map[string]Binding              `json:"bindings"`
	Definitions    map[string]tool.BoundDefinition `json:"definitions"`
	DynamicTargets map[string]Target               `json:"dynamic_targets,omitempty"`
}

type Set struct{ snapshot Snapshot }

func digest(value any) string {
	body, _ := json.Marshal(value)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}

func DocumentID(document Document) string {
	return digest([4]string{Version, document.SourceIdentity, document.SourceDigest, document.PackageIdentity})
}

func ScopeID(documentID, catalogDigest, profileDigest string) string {
	return digest([4]string{Version, documentID, catalogDigest, profileDigest})
}

func ProfileDigest(profile *schema.RuntimeProfile) string { return digest(profile) }

func (set *Set) ValidateInclude(owner, stepID, target string) error {
	if !set.HasScope(owner) || !set.HasScope(target) {
		return fmt.Errorf("tool scope: missing include owner")
	}
	document := set.snapshot.Documents[set.snapshot.Scopes[owner].DocumentID]
	for _, edge := range document.StaticEdges {
		if edge.StepID == stepID && edge.DocumentID == set.snapshot.Scopes[target].DocumentID {
			return nil
		}
	}
	return fmt.Errorf("tool scope: include %q crosses an unrecorded source edge", stepID)
}

// ContextDigest excludes generated executable bodies to break the substitution
// -> scope -> definition cycle. Snapshot.Digest covers those bodies in full.
func (set *Set) ContextDigest() string {
	if set == nil {
		return ""
	}
	return digest([3]string{Version, set.snapshot.CatalogDigest, set.snapshot.ProfileDigest})
}

func clone(snapshot Snapshot) (Snapshot, error) {
	body, err := json.Marshal(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	var result Snapshot
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

// New seals a complete caller-built table; IDs must already be canonical.
func New(snapshot Snapshot) (*Set, error) {
	if snapshot.Version != Version {
		return nil, fmt.Errorf("tool scope: unsupported version %q", snapshot.Version)
	}
	owned, err := clone(snapshot)
	if err != nil {
		return nil, err
	}
	if err := validate(owned); err != nil {
		return nil, err
	}
	owned.Digest = ""
	body, err := json.Marshal(owned)
	if err != nil {
		return nil, err
	}
	owned.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	return &Set{snapshot: owned}, nil
}

func Restore(snapshot Snapshot) (*Set, error) {
	set, err := New(snapshot)
	if err != nil {
		return nil, err
	}
	if snapshot.Digest == "" || set.snapshot.Digest != snapshot.Digest {
		return nil, fmt.Errorf("tool scope: digest mismatch")
	}
	return set, nil
}

func (set *Set) Export() Snapshot {
	if set == nil {
		return Snapshot{}
	}
	result, _ := clone(set.snapshot)
	return result
}

// Target returns captured candidate content, not a filesystem/catalog handle.
func (set *Set) Target(qualifiedID string) (Target, error) {
	if set == nil {
		return Target{}, fmt.Errorf("tool scope: immutable set required")
	}
	target, ok := set.snapshot.DynamicTargets[qualifiedID]
	if !ok {
		return Target{}, fmt.Errorf("tool scope: no captured dynamic target %q", qualifiedID)
	}
	body, err := json.Marshal(target)
	if err != nil {
		return Target{}, err
	}
	var owned Target
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&owned); err != nil {
		return Target{}, err
	}
	return owned, nil
}

func (set *Set) MarshalJSON() ([]byte, error) {
	if set == nil {
		return []byte("null"), nil
	}
	return json.Marshal(set.snapshot)
}

func (set *Set) UnmarshalJSON(body []byte) error {
	var snapshot Snapshot
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&snapshot); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("tool scope: trailing data")
	}
	restored, err := Restore(snapshot)
	if err != nil {
		return err
	}
	*set = *restored
	return nil
}

func (set *Set) HasScope(id string) bool {
	if set == nil {
		return false
	}
	_, ok := set.snapshot.Scopes[id]
	return ok
}

// Declarations returns an owned, file-local view for validation/presentation.
// It does not merge sibling scopes or fall back to a runtime registry.
func (set *Set) Declarations(scopeID string) (map[string]*schema.ToolDef, error) {
	if !set.HasScope(scopeID) {
		return nil, fmt.Errorf("tool scope: missing scope %q", scopeID)
	}
	result := make(map[string]*schema.ToolDef)
	for name, bindingID := range set.snapshot.Scopes[scopeID].Bindings {
		binding := set.snapshot.Bindings[bindingID]
		definition, err := tool.CloneBoundDefinition(set.snapshot.Definitions[binding.DefinitionID])
		if err != nil {
			return nil, err
		}
		if definition.Declaration == nil {
			return nil, fmt.Errorf("tool scope: frozen declaration missing for %q", name)
		}
		result[name] = definition.Declaration
	}
	return result, nil
}

func (set *Set) Resolve(scopeID, name, action string) (tool.BoundInvocation, error) {
	if !set.HasScope(scopeID) {
		return tool.BoundInvocation{}, fmt.Errorf("tool scope: missing scope %q", scopeID)
	}
	bindingID := set.snapshot.Scopes[scopeID].Bindings[name]
	binding, ok := set.snapshot.Bindings[bindingID]
	if !ok {
		return tool.BoundInvocation{}, fmt.Errorf("tool scope: no local binding %q in %q", name, scopeID)
	}
	definition, err := tool.CloneBoundDefinition(set.snapshot.Definitions[binding.DefinitionID])
	if err != nil {
		return tool.BoundInvocation{}, err
	}
	invocation := tool.BoundInvocation{ScopeID: scopeID, BindingID: bindingID, DefinitionID: binding.DefinitionID, LogicalName: name, Action: action, Definition: definition}
	return invocation, tool.ValidateBoundInvocation(invocation)
}

func validate(snapshot Snapshot) error {
	fail := func() error { return fmt.Errorf("tool scope: incomplete or inconsistent immutable tables") }
	if snapshot.CatalogDigest == "" || snapshot.ProfileDigest == "" || len(snapshot.Documents) == 0 || len(snapshot.Scopes) == 0 {
		return fail()
	}
	for id, document := range snapshot.Documents {
		if document.SourceIdentity == "" || document.SourceDigest == "" || document.PackageIdentity == "" || id != DocumentID(document) {
			return fail()
		}
		scope, ok := snapshot.Scopes[document.ScopeID]
		if !ok || scope.DocumentID != id {
			return fail()
		}
		for _, edge := range document.StaticEdges {
			if _, ok := snapshot.Documents[edge.DocumentID]; !ok || edge.StepID == "" {
				return fail()
			}
		}
	}
	for id, scope := range snapshot.Scopes {
		document, ok := snapshot.Documents[scope.DocumentID]
		if !ok || document.ScopeID != id || id != ScopeID(scope.DocumentID, snapshot.CatalogDigest, snapshot.ProfileDigest) {
			return fail()
		}
		for name, bindingID := range scope.Bindings {
			binding, ok := snapshot.Bindings[bindingID]
			if !ok || name == "" || binding.ScopeID != id || binding.LogicalName != name {
				return fail()
			}
		}
	}
	for id, binding := range snapshot.Bindings {
		scope, ok := snapshot.Scopes[binding.ScopeID]
		if !ok || scope.Bindings[binding.LogicalName] != id || id != tool.BindingID(binding.ScopeID, binding.LogicalName, binding.DefinitionID) {
			return fail()
		}
		if _, ok := snapshot.Definitions[binding.DefinitionID]; !ok {
			return fail()
		}
	}
	for id, definition := range snapshot.Definitions {
		actual, err := tool.DefinitionID(definition)
		if err != nil {
			return err
		}
		if actual != id {
			return fail()
		}
	}
	for name, target := range snapshot.DynamicTargets {
		document, ok := snapshot.Documents[target.DocumentID]
		if !ok || name == "" || document.ScopeID != target.ScopeID || document.SourceDigest != target.SourceDigest || target.ClosureDigest == "" {
			return fail()
		}
		if err := validateTarget(target, document, digest([3]string{Version, snapshot.CatalogDigest, snapshot.ProfileDigest})); err != nil {
			return fmt.Errorf("tool scope: target %q: %w", name, err)
		}
	}
	return nil
}
