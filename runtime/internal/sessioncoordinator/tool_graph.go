package sessioncoordinator

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
)

func appendFrozenToolGraphs(plan *engine.ExecutionPlan, document *graphjson.Document, nodes []handoffGraphNodeBinding) error {
	if plan.ToolScopes == nil {
		return nil
	}
	for index, step := range plan.Steps {
		projection, err := frozenToolProjection(plan, step)
		if err != nil {
			return err
		}
		if projection == nil {
			continue
		}
		if err := applyFrozenToolProjectionPins(plan, index, projection, plan.Metadata.DynamicIncludes); err != nil {
			return err
		}
		encoded, err := executionPlanGraphCurrentVersion(projection, nil)
		if err != nil {
			return err
		}
		var child graphjson.Document
		if err := json.Unmarshal(encoded, &child); err != nil {
			return err
		}
		runtimePath, err := handoffRuntimeStepCallPath(plan, index)
		if err != nil {
			return err
		}
		if err := appendToolInvocationGraph(document, child, nodes[index].ID, engine.DebugNodeID(runtimePath, step.ID)); err != nil {
			return err
		}
	}
	return nil
}

func appendToolInvocationGraph(document *graphjson.Document, child graphjson.Document, parentID, runtimeParentID string) error {
	parentFrameID := ""
	for _, node := range document.Nodes {
		if node.ID == parentID {
			parentFrameID = graphDataString(node.Data, "frame_id")
			break
		}
	}
	parentDepth := -1
	for _, frame := range document.Frames {
		if frame.ID == parentFrameID {
			parentDepth = frame.Depth
			break
		}
	}
	if parentDepth < 0 {
		return fmt.Errorf("session coordinator: frozen tool graph parent frame is unavailable for %s", parentID)
	}
	nodeID := func(id string) string { return parentID + "/" + id }
	frameID := func(id string) string {
		if id == "frame:root" {
			return "frame:" + parentID
		}
		return "frame:" + nodeID(strings.TrimPrefix(id, "frame:"))
	}
	entryGroup := "group:" + parentID + ":include_frame:0"
	groupID := func(id string) string {
		if id == "" {
			return entryGroup
		}
		return "group:" + nodeID(strings.TrimPrefix(id, "group:"))
	}
	document.Groups = append(document.Groups, graphdoc.Group{
		ID: entryGroup, Kind: graphdoc.GroupIncludeFrame, ParentNodeID: parentID,
		FrameID: frameID("frame:root"), QualifiedID: entryGroup,
		QualifiedParentNodeID: parentID, QualifiedFrameID: frameID("frame:root"),
	})
	entered := false
	for _, node := range child.Nodes {
		data := cloneGraphNodeData(node.Data)
		oldGroup := graphDataString(data, "group_id")
		if !entered && oldGroup == "" {
			document.Edges = append(document.Edges, graphjson.Edge{
				ID: "tool-entry:" + parentID, Source: parentID, Target: nodeID(node.ID), Type: string(graphdoc.EdgeInclude),
			})
			entered = true
		}
		node.ID = nodeID(node.ID)
		node.ParentNode, node.Extent = groupID(oldGroup), "parent"
		data["id"], data["group_id"], data["frame_id"] = node.ID, node.ParentNode, frameID(graphDataString(data, "frame_id"))
		data["runtime_node_id"] = runtimeParentID + "/" + graphDataString(data, "runtime_node_id")
		path := append([]string(nil), strings.Split(parentID, "/")...)
		if callPath, ok := data["call_path"].([]any); ok {
			for _, value := range callPath {
				path = append(path, fmt.Sprint(value))
			}

		}
		data["call_path"], data["order"] = path, len(document.Nodes)
		node.Data = data
		document.Nodes = append(document.Nodes, node)
	}
	for _, frame := range child.Frames {
		root := frame.ID == "frame:root"
		frame.ID = frameID(frame.ID)
		frame.QualifiedID = frame.ID
		if root {
			frame.ParentIncludeNodeID = parentID
		} else {
			frame.ParentIncludeNodeID = nodeID(frame.ParentIncludeNodeID)
		}
		frame.QualifiedParentIncludeNodeID = frame.ParentIncludeNodeID
		frame.Depth += parentDepth + 1
		document.Frames = append(document.Frames, frame)
	}
	for _, group := range child.Groups {
		group.ID, group.ParentNodeID, group.FrameID = groupID(group.ID), nodeID(group.ParentNodeID), frameID(group.FrameID)
		group.QualifiedID, group.QualifiedParentNodeID, group.QualifiedFrameID = group.ID, group.ParentNodeID, group.FrameID
		document.Groups = append(document.Groups, group)
	}
	for _, edge := range child.Edges {
		edge.ID, edge.Source, edge.Target = "tool-edge:"+parentID+":"+edge.ID, nodeID(edge.Source), nodeID(edge.Target)
		document.Edges = append(document.Edges, edge)
	}
	if child.SchemaVersion == "3" {
		document.SchemaVersion = "3"
	}
	return nil
}
