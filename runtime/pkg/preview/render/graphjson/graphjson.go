// Package graphjson renders a graphdoc.Document (with optional runstate
// overlay) into the JSON shape consumed directly by React Flow.
//
// Output schema:
//
//	{
//	  "schema_version": "1",
//	  "hash":           "<doc.Hash>",
//	  "runbook":        { id, name, path },
//	  "frames":         [ { id, runbook_id, runbook_path, parent_include_node_id, depth } ],
//	  "nodes": [ {
//	    "id":   string,
//	    "type": "step" | "iterate" | "parallel" | "branch" | "include" | …,
//	    "data": { id, kind, title, group_id, frame_id, order,
//	              status, attempt, duration_ms, error, iteration{index,total} },
//	    "parentNode": "<group:…>" | "<frame:…>" | "",
//	    "extent":     "parent" | "",
//	    "position":   { x, y }   // 0,0 — positions left to layout engine
//	  } ],
//	  "groups": [ { id, kind, parent_node_id, frame_id, label, index } ],
//	  "edges": [ { id, source, target, type, label } ],
//	  "inputs": [ { name, type, required, default, description,
//	                enum, enumRedacted, enumMemberCount } ]  // omitempty
//	}
//
// Position is always (0,0); the consumer (e.g. dagre, elkjs) is
// responsible for layout. parentNode points at the group ID for nested
// nodes, enabling React Flow's compound-node grouping out of the box.
package graphjson

import (
	"encoding/json"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	regschema "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// Document is the top-level JSON shape.
type Document struct {
	PresentationState *graphdoc.PresentationState `json:"presentation_state,omitempty"`
	SchemaVersion     string                      `json:"schema_version"`
	Hash              string                      `json:"hash"`
	Runbook           graphdoc.RunbookRef         `json:"runbook"`
	Frames            []graphdoc.Frame            `json:"frames"`
	Nodes             []Node                      `json:"nodes"`
	Groups            []graphdoc.Group            `json:"groups"`
	Edges             []Edge                      `json:"edges"`
	Regions           *regschema.Manifest         `json:"regions,omitempty"`
	// Inputs carries the root runbook's declared top-level input metadata
	// (AR-CE-2, F-1). Copied verbatim from doc.Inputs: no re-sorting of
	// the list, no reordering of enum members, `enum` omitted (not `[]`)
	// when absent or redacted (see graphdoc.InputDecl).
	Inputs []graphdoc.InputDecl `json:"inputs,omitempty"`
}

// Node is the React Flow node shape.
type Node struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Data       map[string]any `json:"data"`
	ParentNode string         `json:"parentNode,omitempty"`
	Extent     string         `json:"extent,omitempty"`
	Position   Position       `json:"position"`
}

// Position is React Flow's required {x, y}; layout is delegated.
type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Edge is the React Flow edge shape.
type Edge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type,omitempty"`
	Label  string `json:"label,omitempty"`
}

// Render returns the React-Flow JSON for doc.
func Render(doc *graphdoc.Document) ([]byte, error) {
	return RenderWithState(doc, nil)
}

// RenderWithState returns the React-Flow JSON, with runtime status
// overlaid into each node's data when state is non-nil.
func RenderWithState(doc *graphdoc.Document, state *runstate.State) ([]byte, error) {
	if doc == nil {
		return json.Marshal(&Document{SchemaVersion: graphdoc.SchemaVersion})
	}
	doc = doc.ForExpressionRendering()

	out := &Document{
		PresentationState: doc.PresentationState,
		SchemaVersion:     doc.SchemaVersion,
		Hash:              doc.Hash,
		Runbook:           doc.Runbook,
		Frames:            qualifiedFrames(doc.Frames),
		Groups:            qualifiedGroups(doc.Groups),
		Nodes:             make([]Node, 0, len(doc.Nodes)),
		Edges:             make([]Edge, 0, len(doc.Edges)),
		Regions:           doc.Regions,
		Inputs:            doc.Inputs,
	}

	for i := range doc.Nodes {
		n := &doc.Nodes[i]
		nodeID := n.ID
		if n.QualifiedID != "" {
			nodeID = n.QualifiedID
		}
		frameID := n.FrameID
		if n.QualifiedFrameID != "" {
			frameID = n.QualifiedFrameID
		}
		groupID := n.GroupID
		if n.QualifiedGroupID != "" {
			groupID = n.QualifiedGroupID
		}
		data := map[string]any{
			"id":        nodeID,
			"step_id":   n.StepID,
			"call_path": n.CallPath,
			"kind":      n.Kind,
			"title":     n.Title,
			"group_id":  groupID,
			"frame_id":  frameID,
			"order":     n.Order,
		}
		if n.Dynamic {
			data["dynamic"] = true
		}
		if n.Concurrent {
			data["concurrent"] = true
		}
		if n.ToolName != "" {
			data["tool_name"] = n.ToolName
		}
		if n.ToolAction != "" {
			data["tool_action"] = n.ToolAction
		}
		if n.Details != nil {
			data["details"] = n.Details
		}
		if state != nil {
			st := state.Get(nodeID)
			data["status"] = string(st.Status)
			if st.Attempt > 0 {
				data["attempt"] = st.Attempt
			}
			if st.DurationMs > 0 {
				data["duration_ms"] = st.DurationMs
			}
			if st.Error != "" {
				data["error"] = st.Error
			}
			if st.Iteration != nil {
				data["iteration"] = map[string]any{
					"index": st.Iteration.Index,
					"total": st.Iteration.Total,
				}
			}
		}
		parent := groupID
		if parent == "" {
			// React Flow only nests inside compound nodes (groups).
			// Top-level frame membership is preserved on data.frame_id
			// and the Frames array.
			parent = ""
		}
		extent := ""
		if parent != "" {
			extent = "parent"
		}
		out.Nodes = append(out.Nodes, Node{
			ID:         nodeID,
			Type:       reactFlowType(n.Kind),
			Data:       data,
			ParentNode: parent,
			Extent:     extent,
			Position:   Position{X: 0, Y: 0},
		})
	}

	for i, e := range doc.Edges {
		source, target := e.From, e.To
		if e.QualifiedFrom != "" {
			source = e.QualifiedFrom
		}
		if e.QualifiedTo != "" {
			target = e.QualifiedTo
		}
		out.Edges = append(out.Edges, Edge{
			ID:     fmt.Sprintf("e%d", i),
			Source: source,
			Target: target,
			Type:   string(e.Kind),
			Label:  e.Label,
		})
	}

	return json.MarshalIndent(out, "", "  ")
}

func qualifiedFrames(frames []graphdoc.Frame) []graphdoc.Frame {
	out := make([]graphdoc.Frame, len(frames))
	for i, frame := range frames {
		out[i] = frame
		if frame.QualifiedID != "" {
			out[i].ID = frame.QualifiedID
		}
		if frame.QualifiedParentIncludeNodeID != "" {
			out[i].ParentIncludeNodeID = frame.QualifiedParentIncludeNodeID
		}
	}
	return out
}

func qualifiedGroups(groups []graphdoc.Group) []graphdoc.Group {
	out := make([]graphdoc.Group, len(groups))
	for i, group := range groups {
		out[i] = group
		if group.QualifiedID != "" {
			out[i].ID = group.QualifiedID
		}
		if group.QualifiedParentNodeID != "" {
			out[i].ParentNodeID = group.QualifiedParentNodeID
		}
		if group.QualifiedFrameID != "" {
			out[i].FrameID = group.QualifiedFrameID
		}
	}
	return out
}

// reactFlowType maps a graphdoc Node.Kind to a custom React Flow type.
// Consumers register node renderers under these names. We use generic
// names (decision, group, terminal, action) so a single renderer can
// cover several semantic kinds.
func reactFlowType(kind string) string {
	switch kind {
	case "branch", "decision", "choice":
		return "decision"
	case "iterate", "parallel":
		return "group"
	case "include":
		return "include"
	case "end", "results":
		return "terminal"
	case "approve":
		return "approval"
	case "compensate":
		return "compensate"
	case "noop", "display":
		return "note"
	default:
		return "action"
	}
}
