// Package presentation projects tool-owned syntax metadata without executing tools.
package presentation

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

const SchemaVersion = "yawr.presentation-resolve/v1"
const ResolverVersion = "yawr.core-binding/v1"
const MaxBytes = 8 << 20
const MaxEntries = 4096

type Context struct {
	ProjectRoot    string `json:"project_root"`
	Generation     int64  `json:"generation"`
	PackageMapPath string `json:"package_map_path,omitempty"`
	EntrypointPath string `json:"entrypoint_path,omitempty"`
	PackageRoot    string `json:"package_root,omitempty"`
}
type Buffer struct {
	URI     string `json:"uri"`
	Path    string `json:"path"`
	Version int64  `json:"version"`
	Text    string `json:"text"`
}
type Request struct {
	SchemaVersion string   `json:"schema_version"`
	RequestID     string   `json:"request_id"`
	Context       Context  `json:"context"`
	Document      Buffer   `json:"document"`
	Overlays      []Buffer `json:"overlays"`
}
type Document struct {
	URI     string `json:"uri"`
	Version int64  `json:"version"`
}
type Reply struct {
	SchemaVersion   string       `json:"schema_version"`
	ResolverVersion string       `json:"resolver_version"`
	RequestID       string       `json:"request_id"`
	Context         Context      `json:"context"`
	Document        Document     `json:"document"`
	Status          string       `json:"status"`
	Reason          string       `json:"reason,omitempty"`
	Bindings        []Binding    `json:"bindings"`
	Regions         []Region     `json:"regions"`
	Dependencies    []Dependency `json:"dependencies"`
}
type Field struct {
	Name         string                         `json:"name"`
	ValueType    string                         `json:"value_type"`
	Status       string                         `json:"status"`
	Reason       string                         `json:"reason,omitempty"`
	Presentation *schema.PresentationDescriptor `json:"presentation,omitempty"`
}
type Action struct {
	Name      string  `json:"name"`
	Arguments []Field `json:"arguments"`
	Outputs   []Field `json:"outputs"`
}
type Binding struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Reason     string   `json:"reason,omitempty"`
	ToolID     string   `json:"tool_id,omitempty"`
	ToolDigest string   `json:"tool_digest,omitempty"`
	SourceURI  string   `json:"source_uri,omitempty"`
	Actions    []Action `json:"actions"`
}
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
type Region struct {
	BindingID string `json:"binding_id"`
	Action    string `json:"action"`
	Direction string `json:"direction"`
	Field     string `json:"field"`
	YAMLPath  string `json:"yaml_path"`
	Range     Range  `json:"range"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}
type Dependency struct {
	URI     string `json:"uri"`
	Version *int64 `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}
type Envelope struct {
	Version            int     `json:"version"`
	Status             string  `json:"status"`
	Reason             string  `json:"reason,omitempty"`
	Origin             string  `json:"origin"`
	ToolID             string  `json:"tool_id,omitempty"`
	ToolDigest         string  `json:"tool_digest,omitempty"`
	Action             string  `json:"action,omitempty"`
	PlanSnapshotDigest string  `json:"plan_snapshot_digest,omitempty"`
	Arguments          []Field `json:"arguments"`
	Outputs            []Field `json:"outputs"`
}

func Fields(defs map[string]*schema.ArgDef) []Field {
	out := make([]Field, 0, len(defs))
	for name, def := range defs {
		if def == nil {
			continue
		}
		status, reason := def.Presentation.Availability()
		if def.ValidatePresentation() != nil {
			status, reason = "unavailable", "invalid-descriptor"
		}
		var descriptor *schema.PresentationDescriptor
		if def.Presentation != nil {
			copy := *def.Presentation
			descriptor = &copy
		}
		out = append(out, Field{Name: name, ValueType: def.Type, Status: status, Reason: reason, Presentation: descriptor})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func ForAction(toolID, action, origin, snapshotDigest string, def *schema.ToolDef) *Envelope {
	e := &Envelope{Version: 1, Status: "unavailable", Reason: "missing-dependency", Origin: origin, PlanSnapshotDigest: snapshotDigest, Arguments: []Field{}, Outputs: []Field{}}
	if def == nil {
		return e
	}
	a := def.Actions[action]
	if a == nil {
		return e
	}
	data, err := json.Marshal(def)
	if err != nil {
		return e
	}
	e.Status, e.Reason, e.ToolID, e.Action = "resolved", "", toolID, action
	e.ToolDigest = Digest(data)
	e.Arguments, e.Outputs = Fields(a.Args), Fields(a.Outputs)
	return e
}

func Digest(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }
