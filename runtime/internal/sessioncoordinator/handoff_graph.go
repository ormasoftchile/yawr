package sessioncoordinator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

type boundHandoffGraph struct {
	graphjson.Document
	ExecutionPlanHash string `json:"execution_plan_hash"`
	BoundContentHash  string `json:"bound_content_hash"`
}

type handoffGraphNodeBinding struct {
	ID      string
	StepID  string
	Kind    string
	FrameID string
	GroupID string
	Order   int
}

func bindHandoffGraph(
	plan *engine.ExecutionPlan,
	planSnapshotHash string,
	encoded json.RawMessage,
) (json.RawMessage, error) {
	document, err := decodeHandoffGraph(plan, encoded)
	if err != nil {
		return nil, err
	}
	if !validSHA256Reference(planSnapshotHash) {
		return nil, errors.New("session coordinator: handoff target snapshot hash is invalid")
	}
	bindGraphPresentation(&document, planSnapshotHash)
	bound := boundHandoffGraph{
		Document: document, ExecutionPlanHash: planSnapshotHash,
		BoundContentHash: session.DigestJSON(document),
	}
	encodedBound, err := json.Marshal(bound)
	if err != nil {
		return nil, fmt.Errorf("session coordinator: encode bound handoff GraphJSON: %w", err)
	}
	return encodedBound, nil
}

func validateBoundHandoffGraph(
	plan *engine.ExecutionPlan,
	planSnapshotHash string,
	encoded json.RawMessage,
) error {
	var bound boundHandoffGraph
	if err := decodeHandoffJSON(encoded, &bound); err != nil {
		return fmt.Errorf("session coordinator: decode bound handoff GraphJSON: %w", err)
	}
	if bound.ExecutionPlanHash != planSnapshotHash || !validSHA256Reference(bound.ExecutionPlanHash) {
		return errors.New("session coordinator: handoff GraphJSON plan binding is invalid")
	}
	if bound.BoundContentHash != session.DigestJSON(bound.Document) {
		return errors.New("session coordinator: handoff GraphJSON content hash is invalid")
	}
	if err := validateGraphPresentationBinding(bound.Document, planSnapshotHash); err != nil {
		return err
	}
	canonicalDocument, err := json.Marshal(bound.Document)
	if err != nil {
		return err
	}
	if _, err := decodeHandoffGraph(plan, canonicalDocument); err != nil {
		return err
	}
	return nil
}

func decodeHandoffGraph(plan *engine.ExecutionPlan, encoded json.RawMessage) (graphjson.Document, error) {
	if plan == nil || !json.Valid(encoded) {
		return graphjson.Document{}, errors.New("session coordinator: handoff resolver returned invalid GraphJSON")
	}
	var document graphjson.Document
	if err := decodeHandoffJSON(encoded, &document); err != nil {
		return graphjson.Document{}, fmt.Errorf("session coordinator: decode handoff GraphJSON: %w", err)
	}
	if (document.SchemaVersion != graphdoc.SchemaVersion && document.SchemaVersion != "3") || !validRawSHA256(document.Hash) ||
		plan.Metadata.GraphContentHash != document.Hash ||
		document.Runbook.ID != plan.Metadata.RunbookID || document.Runbook.Name != plan.Metadata.RunbookName ||
		filepath.Clean(document.Runbook.Path) != filepath.Clean(plan.RunbookPath) || len(document.Frames) == 0 ||
		document.Frames[0].RunbookID != plan.Metadata.RunbookID ||
		filepath.Clean(document.Frames[0].RunbookPath) != filepath.Clean(plan.RunbookPath) {
		return graphjson.Document{}, errors.New("session coordinator: handoff GraphJSON does not match its target plan")
	}
	expectedGraph, err := executionPlanGraph(plan, nil)
	if err != nil {
		return graphjson.Document{}, err
	}
	var expected graphjson.Document
	if err := decodeHandoffJSON(expectedGraph, &expected); err != nil {
		return graphjson.Document{}, err
	}
	// Only the final self-referential binding differs from the pre-snapshot
	// canonical graph. All descriptor and executable identity fields must match.
	unbound := document
	unbound.Nodes = append([]graphjson.Node(nil), document.Nodes...)
	bindGraphPresentation(&unbound, "")
	if expected.Hash != document.Hash || session.DigestJSON(expected) != session.DigestJSON(unbound) {
		return graphjson.Document{}, errors.New("session coordinator: handoff GraphJSON definition does not match its target plan")
	}

	if err := validateHandoffGraphContentHash(document); err != nil {
		return graphjson.Document{}, err
	}
	if len(plan.Metadata.DynamicIncludes) == 0 {
		if err := validateHandoffGraphPlanEquivalence(plan, document); err != nil {
			return graphjson.Document{}, err
		}
	}
	return document, nil
}

func bindGraphPresentation(document *graphjson.Document, digest string) {
	for i := range document.Nodes {
		node := &document.Nodes[i]
		encoded, _ := json.Marshal(node.Data["details"])
		var details graphdoc.StepDetails
		if decodeHandoffJSON(encoded, &details) != nil || details.CodePresentation == nil {
			continue
		}
		details.CodePresentation.PlanSnapshotDigest = digest
		data := make(map[string]any, len(node.Data))
		for key, value := range node.Data {
			data[key] = value
		}
		encodedDetails, _ := json.Marshal(&details)
		var normalized any
		_ = decodeHandoffJSON(encodedDetails, &normalized)
		data["details"] = normalized
		node.Data = data
	}
}

func validateGraphPresentationBinding(document graphjson.Document, digest string) error {
	for _, node := range document.Nodes {
		encoded, _ := json.Marshal(node.Data["details"])
		var details graphdoc.StepDetails
		if decodeHandoffJSON(encoded, &details) == nil && details.CodePresentation != nil {
			if details.CodePresentation.Origin != "frozen" || details.CodePresentation.PlanSnapshotDigest != digest {
				return errors.New("session coordinator: presentation snapshot binding is invalid")
			}
		}
	}
	return nil
}
func validateHandoffGraphContentHash(document graphjson.Document) error {
	canonical, err := graphDocumentFromHandoffGraph(document)
	if err != nil {
		return err
	}
	hash, err := canonical.ContentHash()
	if err != nil || hash != document.Hash {
		return errors.New("session coordinator: handoff GraphJSON content hash is invalid")
	}
	return nil
}

func graphDocumentFromHandoffGraph(document graphjson.Document) (*graphdoc.Document, error) {
	if document.SchemaVersion != graphdoc.SchemaVersion && document.SchemaVersion != "3" {
		return nil, errors.New("session coordinator: unsupported GraphJSON version")
	}
	if document.SchemaVersion != "3" {
		for _, frame := range document.Frames {
			if frame.Invocation != nil {
				return nil, errors.New("session coordinator: typed invocation requires GraphJSON v3")
			}
		}
		for _, node := range document.Nodes {
			if node.Data["kind"] == "assign" || node.Data["kind"] == "results" {
				return nil, errors.New("session coordinator: typed operation requires GraphJSON v3")
			}
		}
	}
	type nodeData struct {
		ID         string                `json:"id"`
		StepID     string                `json:"step_id"`
		CallPath   []string              `json:"call_path"`
		Kind       string                `json:"kind"`
		Title      string                `json:"title"`
		GroupID    string                `json:"group_id"`
		FrameID    string                `json:"frame_id"`
		Order      int                   `json:"order"`
		Dynamic    bool                  `json:"dynamic"`
		Concurrent bool                  `json:"concurrent"`
		ToolName   string                `json:"tool_name"`
		ToolAction string                `json:"tool_action"`
		Details    *graphdoc.StepDetails `json:"details"`
	}
	dataByNode := make(map[string]nodeData, len(document.Nodes))
	rawNodeID := make(map[string]string, len(document.Nodes))
	for _, node := range document.Nodes {
		encoded, err := json.Marshal(node.Data)
		if err != nil {
			return nil, errors.New("session coordinator: handoff GraphJSON node data cannot be hashed")
		}
		var data nodeData
		if err := decodeHandoffJSON(encoded, &data); err != nil || data.StepID == "" {
			return nil, errors.New("session coordinator: handoff GraphJSON node data cannot be hashed")
		}
		dataByNode[node.ID] = data
		rawNodeID[node.ID] = data.StepID
	}
	rawFrameID := make(map[string]string, len(document.Frames))
	frames := make([]graphdoc.Frame, len(document.Frames))
	for index, frame := range document.Frames {
		frames[index] = frame
		rawID := frame.ID
		if frame.ParentIncludeNodeID != "" {
			parentID := rawNodeID[frame.ParentIncludeNodeID]
			if parentID == "" {
				return nil, errors.New("session coordinator: handoff GraphJSON frame hash identity is invalid")
			}
			rawID = "frame:" + parentID
			frames[index].ParentIncludeNodeID = parentID
		}
		frames[index].ID = rawID
		rawFrameID[frame.ID] = rawID
	}
	rawGroupID := make(map[string]string, len(document.Groups))
	groups := make([]graphdoc.Group, len(document.Groups))
	for index, group := range document.Groups {
		parentID := rawNodeID[group.ParentNodeID]
		frameID := rawFrameID[group.FrameID]
		if parentID == "" || frameID == "" {
			return nil, errors.New("session coordinator: handoff GraphJSON group hash identity is invalid")
		}
		rawID := fmt.Sprintf("group:%s:%s:%d", parentID, group.Kind, group.Index)
		groups[index] = group
		groups[index].ID = rawID
		groups[index].ParentNodeID = parentID
		groups[index].FrameID = frameID
		rawGroupID[group.ID] = rawID
	}
	nodes := make([]graphdoc.Node, len(document.Nodes))
	for index, node := range document.Nodes {
		data := dataByNode[node.ID]
		rawFrame := rawFrameID[data.FrameID]
		if rawFrame == "" {
			return nil, errors.New("session coordinator: handoff GraphJSON node hash frame is invalid")
		}
		rawGroup := ""
		if data.GroupID != "" {
			rawGroup = rawGroupID[data.GroupID]
			if rawGroup == "" {
				return nil, errors.New("session coordinator: handoff GraphJSON node hash group is invalid")
			}
		}
		nodes[index] = graphdoc.Node{
			ID: data.StepID, StepID: data.StepID, CallPath: data.CallPath,
			Kind: data.Kind, Title: data.Title, FrameID: rawFrame, GroupID: rawGroup,
			Order: data.Order, Dynamic: data.Dynamic, Concurrent: data.Concurrent,
			ToolName: data.ToolName, ToolAction: data.ToolAction, Details: data.Details,
		}
		if node.ID != data.StepID {
			nodes[index].QualifiedID = node.ID
		}
		if data.FrameID != rawFrame {
			nodes[index].QualifiedFrameID = data.FrameID
		}
		if data.GroupID != rawGroup {
			nodes[index].QualifiedGroupID = data.GroupID
		}
	}
	edges := make([]graphdoc.Edge, len(document.Edges))
	for index, edge := range document.Edges {
		from, to := rawNodeID[edge.Source], rawNodeID[edge.Target]
		if from == "" || to == "" {
			return nil, errors.New("session coordinator: handoff GraphJSON edge hash identity is invalid")
		}
		edges[index] = graphdoc.Edge{From: from, To: to, Kind: graphdoc.EdgeKind(edge.Type), Label: edge.Label}
		if edge.Source != from {
			edges[index].QualifiedFrom = edge.Source
		}
		if edge.Target != to {
			edges[index].QualifiedTo = edge.Target
		}
	}
	return &graphdoc.Document{
		SchemaVersion: document.SchemaVersion, Hash: document.Hash, Runbook: document.Runbook,
		Frames: frames, Groups: groups, Nodes: nodes, Edges: edges,
		Regions: document.Regions, Inputs: document.Inputs,
	}, nil
}

func validateHandoffGraphPlanEquivalence(plan *engine.ExecutionPlan, document graphjson.Document) error {
	if session.DigestJSON(document.Inputs) != session.DigestJSON(handoffGraphInputDecls(plan)) {
		return errors.New("session coordinator: handoff GraphJSON inputs do not match target plan")
	}
	frames := make(map[string]bool, len(document.Frames))
	framePaths := make(map[string]bool, len(document.Frames))
	frameByID := make(map[string]graphdoc.Frame, len(document.Frames))
	for _, frame := range document.Frames {
		if frame.ID == "" || frames[frame.ID] || frame.RunbookID == "" || frame.RunbookPath == "" || frame.Depth < 0 {
			return errors.New("session coordinator: handoff GraphJSON has invalid frame topology")
		}
		frames[frame.ID] = true
		cleanPath := filepath.Clean(frame.RunbookPath)
		framePaths[cleanPath] = true
		frameByID[frame.ID] = frame
	}
	rootFrame := frameByID["frame:root"]
	if rootFrame.ID != "frame:root" || rootFrame.Depth != 0 || rootFrame.ParentIncludeNodeID != "" {
		return errors.New("session coordinator: handoff GraphJSON root frame is not executable")
	}
	expectedFrames := map[string]graphdoc.Frame{"frame:root": rootFrame}
	for index := range plan.Steps {
		callPath, err := handoffPlanStepCallPath(plan, index)
		if err != nil {
			return err
		}
		for callIndex, callFrame := range callPath {
			if callFrame.RunbookPath == "" {
				continue
			}
			includeNodeID := engine.DebugNodeID(callPath[:callIndex], callFrame.StepID)
			frameID := "frame:" + includeNodeID
			expectedFrames[frameID] = graphdoc.Frame{
				ID: frameID, RunbookPath: filepath.Clean(callFrame.RunbookPath),
				ParentIncludeNodeID: includeNodeID, Depth: includeDepth(callPath[:callIndex]) + 1,
			}
		}
	}
	if len(frameByID) != len(expectedFrames) {
		return errors.New("session coordinator: handoff GraphJSON frame count does not match target plan")
	}
	for frameID, expected := range expectedFrames {
		frame, found := frameByID[frameID]
		if !found || filepath.Clean(frame.RunbookPath) != filepath.Clean(expected.RunbookPath) ||
			frame.ParentIncludeNodeID != expected.ParentIncludeNodeID || frame.Depth != expected.Depth {
			return errors.New("session coordinator: handoff GraphJSON frame ancestry is not executable")
		}
	}
	expectedPaths := map[string]bool{filepath.Clean(plan.RunbookPath): true}
	expectedSteps := make(map[string]map[string]int, len(plan.Steps))
	for _, step := range plan.Steps {
		origin := filepath.Clean(plan.RunbookPath)
		if step.Origin != "" {
			origin = filepath.Clean(step.Origin)
			expectedPaths[origin] = true
		}
		key := step.ID + "\x00" + step.Kind
		if expectedSteps[key] == nil {
			expectedSteps[key] = make(map[string]int)
		}
		expectedSteps[key][origin]++
	}
	for path := range expectedPaths {
		if !framePaths[path] {
			return errors.New("session coordinator: handoff GraphJSON is missing an executable frame")
		}
	}
	if len(document.Nodes) != len(plan.Steps) {
		return errors.New("session coordinator: handoff GraphJSON node count does not match target plan")
	}
	nodeIDs := make(map[string]bool, len(document.Nodes))
	orders := make(map[int]bool, len(document.Nodes))
	nodeBindings := make([]handoffGraphNodeBinding, len(plan.Steps))
	for _, node := range document.Nodes {
		if node.ID == "" || node.Type == "" || nodeIDs[node.ID] {
			return errors.New("session coordinator: handoff GraphJSON has duplicate node identity")
		}
		nodeIDs[node.ID] = true
		data, err := json.Marshal(node.Data)
		if err != nil {
			return errors.New("session coordinator: handoff GraphJSON has invalid node data")
		}
		var identity struct {
			ID       string   `json:"id"`
			StepID   string   `json:"step_id"`
			CallPath []string `json:"call_path"`
			Kind     string   `json:"kind"`
			FrameID  string   `json:"frame_id"`
			GroupID  string   `json:"group_id"`
			Order    int      `json:"order"`
		}
		if err := json.Unmarshal(data, &identity); err != nil || identity.StepID == "" || identity.Kind == "" ||
			identity.ID == "" || identity.FrameID == "" || !frames[identity.FrameID] || identity.Order < 0 ||
			identity.Order >= len(plan.Steps) || orders[identity.Order] {
			return errors.New("session coordinator: handoff GraphJSON node is not bound to a valid frame")
		}
		orders[identity.Order] = true
		callPath := make([]engine.DebugCallFrame, len(identity.CallPath))
		for index, stepID := range identity.CallPath {
			if stepID == "" {
				return errors.New("session coordinator: handoff GraphJSON node has an invalid call path")
			}
			callPath[index].StepID = stepID
		}
		if expectedID := engine.DebugNodeID(callPath, identity.StepID); node.ID != identity.ID || identity.ID != expectedID {
			return errors.New("session coordinator: handoff GraphJSON node identity is not executable")
		}
		expectedStep := plan.Steps[identity.Order]
		if expectedStep.ID != identity.StepID || expectedStep.Kind != identity.Kind {
			return errors.New("session coordinator: handoff GraphJSON node order does not match target plan")
		}
		expectedCallPath, err := handoffPlanStepCallPath(plan, identity.Order)
		if err != nil || !sameHandoffGraphCallPath(identity.CallPath, expectedCallPath) ||
			identity.ID != engine.DebugNodeID(expectedCallPath, expectedStep.ID) {
			return errors.New("session coordinator: handoff GraphJSON node call path does not match target plan")
		}
		expectedFrameID := "frame:root"
		for index := len(expectedCallPath) - 1; index >= 0; index-- {
			if expectedCallPath[index].RunbookPath != "" {
				expectedFrameID = "frame:" + engine.DebugNodeID(expectedCallPath[:index], expectedCallPath[index].StepID)
				break
			}
		}
		if identity.FrameID != expectedFrameID {
			return errors.New("session coordinator: handoff GraphJSON node frame does not match target plan")
		}
		if node.Type != handoffReactFlowType(identity.Kind) || node.Position.X != 0 || node.Position.Y != 0 ||
			node.ParentNode != identity.GroupID || identity.GroupID == "" && node.Extent != "" ||
			identity.GroupID != "" && node.Extent != "parent" {
			return errors.New("session coordinator: handoff GraphJSON node container is not executable")
		}
		for key := range node.Data {
			switch key {
			case "id", "step_id", "call_path", "kind", "title", "group_id", "frame_id", "order",
				"dynamic", "concurrent", "tool_name", "tool_action", "details":
			default:
				return errors.New("session coordinator: handoff GraphJSON contains runtime or unknown node data")
			}
		}
		key := identity.StepID + "\x00" + identity.Kind
		framePath := ""
		for _, frame := range document.Frames {
			if frame.ID == identity.FrameID {
				framePath = filepath.Clean(frame.RunbookPath)
				break
			}
		}
		if expectedSteps[key] == nil || expectedSteps[key][framePath] == 0 {
			return errors.New("session coordinator: handoff GraphJSON contains a node outside the target plan")
		}
		expectedSteps[key][framePath]--
		nodeBindings[identity.Order] = handoffGraphNodeBinding{
			ID: node.ID, StepID: identity.StepID, Kind: identity.Kind,
			FrameID: identity.FrameID, GroupID: identity.GroupID, Order: identity.Order,
		}
	}
	for _, paths := range expectedSteps {
		for _, remaining := range paths {
			if remaining != 0 {
				return errors.New("session coordinator: handoff GraphJSON omits a target plan step")
			}
		}
	}
	if err := validateHandoffGraphTopology(plan, document, nodeBindings, frameByID); err != nil {
		return err
	}
	return nil
}

func validateHandoffGraphTopology(
	plan *engine.ExecutionPlan,
	document graphjson.Document,
	nodes []handoffGraphNodeBinding,
	frames map[string]graphdoc.Frame,
) error {
	expectedGroups, err := expectedHandoffGraphGroups(plan, nodes, frames)
	if err != nil {
		return err
	}
	if len(document.Groups) != len(expectedGroups) {
		return errors.New("session coordinator: handoff GraphJSON group count does not match target plan")
	}
	groups := make(map[string]graphdoc.Group, len(document.Groups))
	for _, group := range document.Groups {
		expected, found := expectedGroups[group.ID]
		if !found || groups[group.ID].ID != "" || group.Kind != expected.Kind ||
			group.ParentNodeID != expected.ParentNodeID || group.FrameID != expected.FrameID ||
			group.Label != expected.Label || group.Index != expected.Index || group.Fallback != expected.Fallback {
			return errors.New("session coordinator: handoff GraphJSON group topology does not match target plan")
		}
		groups[group.ID] = group
	}
	for index, node := range nodes {
		expectedGroupID, groupErr := expectedHandoffNodeGroup(plan, index, nodes, groups)
		if groupErr != nil || node.GroupID != expectedGroupID {
			return errors.New("session coordinator: handoff GraphJSON node group does not match target plan")
		}
	}
	expectedEdges := make(map[string]bool)
	scopes := make(map[string][]handoffGraphNodeBinding)
	for _, node := range nodes {
		scope := "frame\x00" + node.FrameID
		if node.GroupID != "" {
			scope = "group\x00" + node.GroupID
		}
		scopes[scope] = append(scopes[scope], node)
	}
	for scope, scopedNodes := range scopes {
		sort.Slice(scopedNodes, func(left, right int) bool { return scopedNodes[left].Order < scopedNodes[right].Order })
		if strings.HasPrefix(scope, "group\x00") && len(scopedNodes) > 0 {
			group := groups[strings.TrimPrefix(scope, "group\x00")]
			kind, label := handoffGroupEntryEdge(group)
			expectedEdges[handoffEdgeKey(group.ParentNodeID, scopedNodes[0].ID, kind, label)] = true
		}
		for index := 1; index < len(scopedNodes); index++ {
			expectedEdges[handoffEdgeKey(
				scopedNodes[index-1].ID, scopedNodes[index].ID, string(graphdoc.EdgeSequence), "",
			)] = true
		}
	}
	if len(document.Edges) != len(expectedEdges) {
		return errors.New("session coordinator: handoff GraphJSON edge count does not match target plan")
	}
	seenEdges := make(map[string]bool, len(document.Edges))
	seenEdgeIndexes := make(map[int]bool, len(document.Edges))
	for _, edge := range document.Edges {
		index, parseErr := strconv.Atoi(strings.TrimPrefix(edge.ID, "e"))
		key := handoffEdgeKey(edge.Source, edge.Target, edge.Type, edge.Label)
		if parseErr != nil || edge.ID != "e"+strconv.Itoa(index) || index < 0 || index >= len(document.Edges) ||
			seenEdgeIndexes[index] || seenEdges[key] || !expectedEdges[key] {
			return errors.New("session coordinator: handoff GraphJSON edge topology does not match target plan")
		}
		seenEdgeIndexes[index] = true
		seenEdges[key] = true
	}
	return nil
}

func expectedHandoffGraphGroups(
	plan *engine.ExecutionPlan,
	nodes []handoffGraphNodeBinding,
	frames map[string]graphdoc.Frame,
) (map[string]graphdoc.Group, error) {
	result := make(map[string]graphdoc.Group)
	add := func(parent handoffGraphNodeBinding, kind graphdoc.GroupKind, frameID, label string, index int, fallback bool) {
		id := fmt.Sprintf("group:%s:%s:%d", parent.ID, kind, index)
		result[id] = graphdoc.Group{
			ID: id, Kind: kind, ParentNodeID: parent.ID, FrameID: frameID,
			Label: label, Index: index, Fallback: fallback,
		}
	}
	for index, step := range plan.Steps {
		parent := nodes[index]
		switch typed := step.Spec.(type) {
		case *schema.IncludeSpec:
			frameID := "frame:" + parent.ID
			if typed != nil && frames[frameID].ID != "" {
				add(parent, graphdoc.GroupIncludeFrame, frameID, "", 0, false)
			}
		case *schema.BranchSpec:
			if typed != nil {
				for branchIndex, branch := range typed.Branches {
					label := branch.Label
					if label == "" {
						label = branch.Condition
					}
					add(parent, graphdoc.GroupBranchArm, parent.FrameID, label, branchIndex, branch.Else)
				}
			}
		case *schema.IterateNode:
			if typed != nil {
				label := typed.ID
				if typed.Over != "" && typed.As != "" {
					label = "over " + typed.Over + " as " + typed.As
				} else if typed.Over != "" {
					label = "over " + typed.Over
				}
				add(parent, graphdoc.GroupIterateBody, parent.FrameID, label, 0, false)
			}
		case *schema.ParallelNode:
			if typed != nil {
				for branchIndex, branch := range typed.Branches {
					add(parent, graphdoc.GroupParallelBranch, parent.FrameID, branch.Label, branchIndex, false)
				}
			}
		case *schema.CompensateSpec:
			if typed != nil {
				add(parent, graphdoc.GroupCompensateBody, parent.FrameID, "", 0, false)
			}
		}
	}
	return result, nil
}

func expectedHandoffNodeGroup(
	plan *engine.ExecutionPlan,
	stepIndex int,
	nodes []handoffGraphNodeBinding,
	groups map[string]graphdoc.Group,
) (string, error) {
	step := &plan.Steps[stepIndex]
	if step.ParentID == "" {
		return "", nil
	}
	parentIndex, err := handoffPlanParentIndex(plan, stepIndex)
	if err != nil {
		return "", err
	}
	parent := nodes[parentIndex]
	armIndex, hasArmIndex, err := handoffStructuralArmIndex(plan.Steps[parentIndex], *step)
	if err != nil {
		return "", err
	}
	kind := graphdoc.GroupKind("")
	switch step.ParentKind {
	case "include":
		kind = graphdoc.GroupIncludeFrame
	case "iterate":
		kind = graphdoc.GroupIterateBody
	case "branch":
		kind = graphdoc.GroupBranchArm
	case "parallel":
		kind = graphdoc.GroupParallelBranch
	case "compensate":
		kind = graphdoc.GroupCompensateBody
	default:
		return "", errors.New("session coordinator: target plan has an unknown structural parent")
	}
	var matched string
	for id, group := range groups {
		if group.ParentNodeID != parent.ID || group.Kind != kind {
			continue
		}
		if kind == graphdoc.GroupBranchArm || kind == graphdoc.GroupParallelBranch {
			if hasArmIndex {
				if group.Index != armIndex {
					continue
				}
			} else if step.BranchLabel != "" && group.Label != step.BranchLabel {
				continue
			}
		}
		if matched != "" {
			return "", errors.New("session coordinator: target plan child group is ambiguous")
		}
		matched = id
	}
	if matched == "" {
		return "", errors.New("session coordinator: target plan child group is unavailable")
	}
	return matched, nil
}

func handoffStructuralArmIndex(parent engine.ResolvedStep, child engine.ResolvedStep) (int, bool, error) {
	var branches [][]schema.FlowNode
	switch typed := parent.Spec.(type) {
	case *schema.BranchSpec:
		if typed != nil {
			for _, branch := range typed.Branches {
				branches = append(branches, branch.Steps)
			}
		}
	case *schema.ParallelNode:
		if typed != nil {
			for _, branch := range typed.Branches {
				branches = append(branches, branch.Steps)
			}
		}
	default:
		return 0, false, nil
	}
	matched := -1
	for index, nodes := range branches {
		for nodeIndex := range nodes {
			stepID, kind := handoffFlowNodeIdentity(nodes[nodeIndex])
			if stepID != child.ID || kind != child.Kind {
				continue
			}
			if matched >= 0 && matched != index {
				return 0, false, errors.New("session coordinator: target plan child arm is ambiguous")
			}
			matched = index
		}
	}
	return matched, matched >= 0, nil
}

func handoffFlowNodeIdentity(node schema.FlowNode) (string, string) {
	switch {
	case node.Step != nil:
		return node.Step.ID, string(node.Step.Type)
	case node.Iterate != nil:
		return node.Iterate.ID, "iterate"
	case node.Parallel != nil:
		return node.Parallel.ID, "parallel"
	default:
		return "", ""
	}
}

func handoffPlanParentIndex(plan *engine.ExecutionPlan, stepIndex int) (int, error) {
	if plan == nil || stepIndex < 0 || stepIndex >= len(plan.Steps) || plan.Steps[stepIndex].ParentID == "" {
		return -1, errors.New("session coordinator: target plan parent is unavailable")
	}
	current := plan.Steps[stepIndex]
	parentIndex := -1
	for index := 0; index < stepIndex; index++ {
		candidate := plan.Steps[index]
		if candidate.ID != current.ParentID || candidate.Depth >= current.Depth {
			continue
		}
		if parentIndex < 0 || candidate.Depth > plan.Steps[parentIndex].Depth ||
			candidate.Depth == plan.Steps[parentIndex].Depth && index > parentIndex {
			parentIndex = index
		}
	}
	if parentIndex < 0 {
		return -1, errors.New("session coordinator: target plan has ambiguous step ancestry")
	}
	return parentIndex, nil
}

func handoffGroupEntryEdge(group graphdoc.Group) (string, string) {
	switch group.Kind {
	case graphdoc.GroupIncludeFrame:
		return string(graphdoc.EdgeInclude), ""
	case graphdoc.GroupIterateBody:
		return string(graphdoc.EdgeIterateBody), ""
	case graphdoc.GroupParallelBranch:
		return string(graphdoc.EdgeParallelBranch), group.Label
	case graphdoc.GroupBranchArm:
		label := group.Label
		if group.Fallback {
			label = "Otherwise"
			if group.Label != "" {
				label += " — " + group.Label
			}
		}
		return string(graphdoc.EdgeBranchArm), label
	case graphdoc.GroupCompensateBody:
		return string(graphdoc.EdgeCompensate), ""
	default:
		return "", ""
	}
}

func handoffEdgeKey(source, target, kind, label string) string {
	return source + "\x00" + target + "\x00" + kind + "\x00" + label
}

func handoffReactFlowType(kind string) string {
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

func includeDepth(callPath []engine.DebugCallFrame) int {
	depth := 0
	for _, frame := range callPath {
		if frame.RunbookPath != "" {
			depth++
		}
	}
	return depth
}

func handoffPlanStepCallPath(plan *engine.ExecutionPlan, stepIndex int) ([]engine.DebugCallFrame, error) {
	if plan == nil || stepIndex < 0 || stepIndex >= len(plan.Steps) {
		return nil, errors.New("session coordinator: invalid target plan step index")
	}
	byID := make(map[string][]int)
	for index := range plan.Steps {
		byID[plan.Steps[index].ID] = append(byID[plan.Steps[index].ID], index)
	}
	var reversed []engine.DebugCallFrame
	current := &plan.Steps[stepIndex]
	seen := map[int]bool{stepIndex: true}
	for current.ParentID != "" {
		candidates := byID[current.ParentID]
		parentIndex := -1
		for _, candidate := range candidates {
			if candidate >= stepIndex || plan.Steps[candidate].Depth >= current.Depth {
				continue
			}
			if parentIndex < 0 || plan.Steps[candidate].Depth > plan.Steps[parentIndex].Depth ||
				plan.Steps[candidate].Depth == plan.Steps[parentIndex].Depth && candidate > parentIndex {
				parentIndex = candidate
			}
		}
		if parentIndex < 0 || seen[parentIndex] {
			return nil, errors.New("session coordinator: target plan has ambiguous step ancestry")
		}
		seen[parentIndex] = true
		parent := &plan.Steps[parentIndex]
		if parent.Kind != "parallel" && parent.Kind != "compensate" {
			frame := engine.DebugCallFrame{StepID: parent.ID}
			if parent.Kind == "include" {
				if include, ok := parent.Spec.(*schema.IncludeSpec); ok && include != nil {
					frame.RunbookPath = include.ResolvedRunbookPath
				}
			}
			reversed = append(reversed, frame)
		}
		current = parent
	}
	result := make([]engine.DebugCallFrame, len(reversed))
	for index := range reversed {
		result[len(reversed)-1-index] = reversed[index]
	}
	return result, nil
}

func sameHandoffGraphCallPath(actual []string, expected []engine.DebugCallFrame) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index].StepID {
			return false
		}
	}
	return true
}

func decodeHandoffJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("session coordinator: trailing handoff GraphJSON content")
		}
		return err
	}
	return nil
}

func handoffGraphInputDecls(plan *engine.ExecutionPlan) []graphdoc.InputDecl {
	if plan == nil || len(plan.Inputs) == 0 {
		return nil
	}
	names := make([]string, 0, len(plan.Inputs))
	for name := range plan.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]graphdoc.InputDecl, 0, len(names))
	for _, name := range names {
		input := plan.Inputs[name]
		if input == nil {
			continue
		}
		inputType := input.Type
		if inputType == "" {
			inputType = "string"
		}
		declaration := graphdoc.InputDecl{
			Name: name, Type: inputType, Required: input.Required, Description: input.Description,
		}
		redacted := schema.IsRedactedDeclName("inputs."+name, plan.GovernanceSource)
		if len(input.Enum) > 0 && redacted {
			declaration.EnumRedacted = true
			declaration.EnumMemberCount = len(input.Enum)
		} else {
			declaration.Default = input.Default
			declaration.Enum = append([]string(nil), input.Enum...)
		}
		result = append(result, declaration)
	}
	return result
}

func validRawSHA256(value string) bool {
	return len(value) == 64 && validSHA256Reference("sha256:"+value)
}

func validSHA256Reference(value string) bool {
	if len(value) != 71 || value[:7] != "sha256:" {
		return false
	}
	for _, current := range value[7:] {
		if current >= '0' && current <= '9' || current >= 'a' && current <= 'f' {
			continue
		}
		return false
	}
	return true
}
