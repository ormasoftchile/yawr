package graphdoc

import (
	"os"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
)

func DetailsForFrozenStep(step engine.ResolvedStep, plan *engine.ExecutionPlan, snapshotDigest string) (*StepDetails, error) {
	d := DetailsForResolvedStep(step)
	if d == nil || d.Kind != "tool" {
		return d, nil
	}
	definition, err := engine.FrozenToolDefinition(plan, step)
	if err != nil {
		return nil, err
	}
	d.CodePresentation = presentation.ForAction(d.Tool, d.Action, "frozen", snapshotDigest, definition)
	maskDeclaredSecrets(d)
	return d, nil
}

func maskDeclaredSecrets(d *StepDetails) {
	defer d.PruneExpressionPresentation()
	if d.CodePresentation == nil {
		return
	}

	for _, field := range d.CodePresentation.Arguments {
		if field.ValueType != "secret" {
			continue
		}
		for i := range d.Arguments {
			if d.Arguments[i].Name == field.Name {
				d.Arguments[i].Value = nil
				d.Arguments[i].Redacted = true
			}
		}
	}
}

func (d *StepDetails) SetCodePresentation(e *presentation.Envelope) {
	d.CodePresentation = e
	maskDeclaredSecrets(d)
}

// AddCurrentPresentation uses the same lexical metadata resolver as authoring.
// Resolution failure affects metadata only; the structural graph still renders.
func AddCurrentPresentation(doc *Document, context presentation.Context) {
	if context.ProjectRoot == "" {
		context.ProjectRoot = filepath.Dir(doc.Runbook.Path)
	}
	if context.EntrypointPath == "" {
		context.EntrypointPath = doc.Runbook.Path
	}
	byFrame := map[string]map[string]presentation.Binding{}
	for _, frame := range doc.Frames {
		data, err := os.ReadFile(frame.RunbookPath)
		if err != nil {
			continue
		}
		reply := presentation.Resolve(presentation.Request{SchemaVersion: presentation.SchemaVersion, RequestID: "preview", Context: context, Document: presentation.Buffer{URI: presentation.FileURI(frame.RunbookPath), Path: frame.RunbookPath, Text: string(data)}, Overlays: []presentation.Buffer{}})
		bindings := map[string]presentation.Binding{}
		for _, b := range reply.Bindings {
			bindings[b.Name] = b
		}
		id := frame.ID
		if frame.QualifiedID != "" {
			id = frame.QualifiedID
		}
		byFrame[id] = bindings
	}
	for i := range doc.Nodes {
		node := &doc.Nodes[i]
		d := node.Details
		if d == nil || d.Kind != "tool" {
			continue
		}
		e := &presentation.Envelope{Version: 1, Status: "unavailable", Reason: "missing-dependency", Origin: "current", Arguments: []presentation.Field{}, Outputs: []presentation.Field{}}
		id := node.FrameID
		if node.QualifiedFrameID != "" {
			id = node.QualifiedFrameID
		}
		if b, ok := byFrame[id][d.Tool]; ok {
			e.Status, e.Reason = b.Status, b.Reason
			if b.Status == "resolved" {
				e.Status, e.Reason = "unavailable", "missing-dependency"
				for _, a := range b.Actions {
					if a.Name == d.Action {
						e.Status, e.Reason, e.ToolID, e.ToolDigest, e.Action = "resolved", "", b.ToolID, b.ToolDigest, a.Name
						e.Arguments, e.Outputs = a.Arguments, a.Outputs
					}
				}
			}
		}
		d.CodePresentation = e
		maskDeclaredSecrets(d)
	}
	if hash, err := doc.ContentHash(); err == nil {
		doc.Hash = hash
	}
}
