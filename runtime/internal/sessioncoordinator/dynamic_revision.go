package sessioncoordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

type journaledSegmentRevision struct {
	SegmentID              string                               `json:"segment_id"`
	RunID                  string                               `json:"run_id"`
	ExecutableRevision     int64                                `json:"executable_revision"`
	GraphRevision          int64                                `json:"graph_revision"`
	PlanHash               string                               `json:"plan_hash"`
	ExecutableSnapshotHash string                               `json:"executable_snapshot_hash"`
	GraphHash              string                               `json:"graph_hash"`
	Resolution             engine.DynamicIncludeResolutionState `json:"resolution"`
	Dispatch               engine.DispatchState                 `json:"dispatch"`
}

// BuildExecutionPlanGraph renders canonical GraphJSON from a finalized
// immutable plan and stamps the resulting content hash into its metadata.
func BuildExecutionPlanGraph(plan *engine.ExecutionPlan) (json.RawMessage, error) {
	return finalizeExecutionPlanGraph(plan, nil, nil)
}

func finalizeExecutionPlanGraph(
	plan *engine.ExecutionPlan,
	resolutions []*engine.DynamicIncludeResolutionState,
	supplied json.RawMessage,
) (json.RawMessage, error) {
	generated, err := executionPlanGraph(plan, resolutions)
	if err != nil {
		return nil, err
	}
	var generatedDocument graphjson.Document
	if err := decodeHandoffJSON(generated, &generatedDocument); err != nil {
		return nil, errors.New("session coordinator: generated GraphJSON is invalid")
	}
	if len(supplied) > 0 {
		var suppliedDocument graphjson.Document
		if err := decodeHandoffJSON(supplied, &suppliedDocument); err != nil {
			return nil, errors.New("session coordinator: supplied GraphJSON is invalid")
		}
		if err := validateHandoffGraphContentHash(suppliedDocument); err != nil ||
			suppliedDocument.Hash != generatedDocument.Hash {
			return nil, errors.New("session coordinator: supplied GraphJSON does not match finalized plan")
		}
	}
	plan.Metadata.GraphContentHash = generatedDocument.Hash
	return generated, nil
}

func (coordinator *Coordinator) loadJournaledSegmentResolutions(
	ctx context.Context,
	sessionID string,
	segment session.SegmentRecord,
	runID string,
) (map[string]*engine.DynamicIncludeResolutionState, map[string]*engine.DispatchState, error) {
	events, err := coordinator.sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		return nil, nil, err
	}
	result := make(map[string]*engine.DynamicIncludeResolutionState)
	dispatches := make(map[string]*engine.DispatchState)
	nextRevision := int64(2)
	lastPlanHash, lastExecutableSnapshotHash, lastGraphHash := "", "", ""
	for _, event := range events {
		if event.Kind != session.EventSegmentRevised {
			continue
		}
		var payload journaledSegmentRevision
		if err := decodeStrictDocument(event.Payload, &payload); err != nil {
			return nil, nil, errors.New("session coordinator: invalid journaled segment revision")
		}
		if payload.SegmentID != segment.SegmentID {
			continue
		}
		if payload.RunID != runID || payload.ExecutableRevision != nextRevision ||
			payload.GraphRevision != nextRevision || payload.Resolution.ResolutionID == "" ||
			result[payload.Resolution.ResolutionID] != nil {
			return nil, nil, errors.New("session coordinator: journaled segment revisions are not contiguous")
		}
		resolution := payload.Resolution
		result[resolution.ResolutionID] = &resolution
		dispatch := payload.Dispatch
		if dispatch.OccurrenceID == "" || dispatches[dispatch.OccurrenceID] != nil {
			return nil, nil, errors.New("session coordinator: journaled segment revision dispatches are invalid")
		}
		dispatches[dispatch.OccurrenceID] = &dispatch
		lastPlanHash, lastExecutableSnapshotHash, lastGraphHash =
			payload.PlanHash, payload.ExecutableSnapshotHash, payload.GraphHash
		nextRevision++
	}
	if nextRevision-1 != segment.ExecutableRevision || segment.GraphRevision != segment.ExecutableRevision ||
		lastPlanHash != segment.PlanHash || lastExecutableSnapshotHash != segment.ExecutableSnapshotHash ||
		lastGraphHash != segment.GraphHash {
		return nil, nil, errors.New("session coordinator: segment revision history does not match manifest")
	}
	return result, dispatches, nil
}

func buildDynamicSegmentRevision(
	state engine.RunState,
) (*engine.ExecutionPlan, session.JSONBlob, json.RawMessage, engine.DynamicIncludeResolutionState, error) {
	plan, resolutions, err := dynamicRevisionPlan(state)
	if err != nil {
		return nil, session.JSONBlob{}, nil, engine.DynamicIncludeResolutionState{}, err
	}
	if len(resolutions) == 0 {
		return nil, session.JSONBlob{}, nil, engine.DynamicIncludeResolutionState{},
			errors.New("session coordinator: dynamic segment revision has no resolution")
	}
	graph, err := finalizeExecutionPlanGraph(plan, resolutions, nil)
	if err != nil {
		return nil, session.JSONBlob{}, nil, engine.DynamicIncludeResolutionState{}, err
	}
	planBlob, err := snapshotBlob(plan)
	if err != nil {
		return nil, session.JSONBlob{}, nil, engine.DynamicIncludeResolutionState{}, err
	}
	boundGraph, err := bindHandoffGraph(plan, planBlob.Digest, graph)
	if err != nil {
		return nil, session.JSONBlob{}, nil, engine.DynamicIncludeResolutionState{}, err
	}
	return plan, planBlob, boundGraph, *resolutions[len(resolutions)-1], nil
}

func dynamicRevisionPlan(
	state engine.RunState,
) (*engine.ExecutionPlan, []*engine.DynamicIncludeResolutionState, error) {
	if state.Plan == nil || state.Plan.Validation == nil {
		return nil, nil, errors.New("session coordinator: dynamic revision plan is unavailable")
	}
	snapshot, err := plansnapshot.FromExecutionPlan(state.Plan)
	if err != nil {
		return nil, nil, fmt.Errorf("session coordinator: snapshot dynamic revision plan: %w", err)
	}
	plan, err := plansnapshot.Restore(snapshot)
	if err != nil {
		return nil, nil, fmt.Errorf("session coordinator: restore dynamic revision plan: %w", err)
	}
	resolutions := make([]*engine.DynamicIncludeResolutionState, 0, len(state.DynamicIncludes))
	for _, resolution := range state.DynamicIncludes {
		if resolution != nil {
			resolutions = append(resolutions, resolution)
		}
	}
	sort.Slice(resolutions, func(left, right int) bool {
		return resolutions[left].Revision < resolutions[right].Revision
	})
	for index, resolution := range resolutions {
		if resolution.Revision != int64(index+1) {
			return nil, nil, errors.New("session coordinator: dynamic include revisions are not contiguous")
		}
	}
	selected, err := selectedDynamicRevisionResolutions(resolutions)
	if err != nil {
		return nil, nil, err
	}
	plan, err = baseDynamicRevisionPlan(plan)
	if err != nil {
		return nil, nil, err
	}
	for _, resolution := range selected {
		if err := plansnapshot.ValidateDynamicIncludePin(resolution.Pin); err != nil {
			return nil, nil, errors.New("session coordinator: dynamic include revision pin is invalid")
		}
		flow, err := plansnapshot.RestoreFlowClosure(resolution.Pin.ExecutableClosure)
		if err != nil {
			return nil, nil, errors.New("session coordinator: dynamic include revision closure is invalid")
		}
		matched, err := dynamicRevisionParentIndex(plan, resolution.Pin)
		if err != nil {
			return nil, nil, err
		}
		include, ok := plan.Steps[matched].Spec.(*schema.IncludeSpec)
		if !ok || include == nil || !include.Include.IsDynamic() {
			return nil, nil, errors.New("session coordinator: dynamic include revision parent is invalid")
		}
		include.ResolvedSteps = flow
		include.ResolvedRunbookPath = resolution.Pin.AbsPath
		include.ResolvedRunbookID = resolution.Pin.RunbookID
		include.ResolvedRunbookName = resolution.Pin.RunbookName
		include.ResolvedRunbookContentHash = resolution.Pin.RunbookContentHash
		include.ResolvedInputs = resolution.Pin.ResolvedInputs
		include.ResolvedBindings = resolution.Pin.ResolvedBindings
		include.ResolvedOutputs = resolution.Pin.ResolvedOutputs
		include.ResolvedGovernance = resolution.Pin.ResolvedGovernance
		if err := attachDynamicPinToCanonicalTree(plan, resolution.Pin, flow); err != nil {
			return nil, nil, err
		}
		if err := internalplanner.FinalizeMaterializedPlan(plan); err != nil {
			return nil, nil, fmt.Errorf("session coordinator: finalize dynamic revision plan: %w", err)
		}
	}
	plan.Metadata.DynamicIncludes = make([]schema.LockedDynamicInclude, len(resolutions))
	for index, resolution := range resolutions {
		plan.Metadata.DynamicIncludes[index] = resolution.Pin
	}
	return plan, resolutions, nil
}

func selectedDynamicRevisionResolutions(
	resolutions []*engine.DynamicIncludeResolutionState,
) ([]*engine.DynamicIncludeResolutionState, error) {
	selectedBySite := make(map[string]*engine.DynamicIncludeResolutionState)
	selectedByRevision := make(map[int64]*engine.DynamicIncludeResolutionState)
	for index := len(resolutions) - 1; index >= 0; index-- {
		lineage, err := dynamicResolutionLineage(resolutions, index)
		if err != nil {
			return nil, err
		}
		compatible := true
		for _, lineageIndex := range lineage {
			candidate := resolutions[lineageIndex]
			if selected := selectedBySite[dynamicPlanSiteKey(candidate.Pin)]; selected != nil && selected.Revision != candidate.Revision {
				compatible = false
				break
			}
		}
		if !compatible {
			continue
		}
		for _, lineageIndex := range lineage {
			candidate := resolutions[lineageIndex]
			selectedBySite[dynamicPlanSiteKey(candidate.Pin)] = candidate
			selectedByRevision[candidate.Revision] = candidate
		}
	}
	selected := make([]*engine.DynamicIncludeResolutionState, 0, len(selectedByRevision))
	for _, resolution := range selectedByRevision {
		selected = append(selected, resolution)
	}
	sort.Slice(selected, func(left, right int) bool {
		return selected[left].Revision < selected[right].Revision
	})
	return selected, nil
}

func dynamicResolutionLineage(
	resolutions []*engine.DynamicIncludeResolutionState,
	index int,
) ([]int, error) {
	if index < 0 || index >= len(resolutions) {
		return nil, errors.New("session coordinator: dynamic resolution lineage index is invalid")
	}
	lineage := []int{index}
	current := index
	for {
		parent, found, err := dynamicParentResolutionIndex(resolutions[:current], resolutions[current].Pin)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		lineage = append(lineage, parent)
		current = parent
	}
	for left, right := 0, len(lineage)-1; left < right; left, right = left+1, right-1 {
		lineage[left], lineage[right] = lineage[right], lineage[left]
	}
	return lineage, nil
}

func dynamicPlanSiteKey(pin schema.LockedDynamicInclude) string {
	type structuralSite struct {
		QualifiedNodeID string `json:"qualified_node_id"`
		Kind            string `json:"kind"`
		BranchLabel     string `json:"branch_label,omitempty"`
	}
	path := make([]structuralSite, len(pin.StructuralPath))
	for index, identity := range pin.StructuralPath {
		path[index] = structuralSite{
			QualifiedNodeID: identity.QualifiedNodeID, Kind: identity.Kind, BranchLabel: identity.BranchLabel,
		}
	}
	return session.DigestJSON(struct {
		QualifiedNodeID string           `json:"qualified_node_id"`
		Path            []structuralSite `json:"path,omitempty"`
	}{pin.QualifiedNodeID, path})
}

func dynamicParentResolutionIndex(
	prior []*engine.DynamicIncludeResolutionState,
	pin schema.LockedDynamicInclude,
) (int, bool, error) {
	priorPins := make([]schema.LockedDynamicInclude, len(prior))
	for index, resolution := range prior {
		if resolution != nil {
			priorPins[index] = resolution.Pin
		}
	}
	return dynamicParentPinIndex(priorPins, pin)
}

func dynamicParentPinIndex(
	prior []schema.LockedDynamicInclude,
	pin schema.LockedDynamicInclude,
) (int, bool, error) {
	for pathIndex := len(pin.StructuralPath) - 1; pathIndex >= 0; pathIndex-- {
		identity := pin.StructuralPath[pathIndex]
		if identity.Kind != "include" {
			continue
		}
		matched := -1
		for index, candidate := range prior {
			if candidate.QualifiedNodeID != identity.QualifiedNodeID ||
				identity.Invocation > 0 && candidate.Invocation != identity.Invocation ||
				!sameDynamicOccurrencePath(candidate.StructuralPath, pin.StructuralPath[:pathIndex]) {
				continue
			}
			if matched >= 0 {
				return -1, false, errors.New("session coordinator: dynamic include ancestor occurrence is ambiguous")
			}
			matched = index
		}
		if matched >= 0 {
			return matched, true, nil
		}
	}
	longestPrefix := -1
	matched := -1
	for index, candidate := range prior {
		prefix := candidate.QualifiedNodeID + "/"
		if candidate.QualifiedNodeID == "" || !strings.HasPrefix(pin.QualifiedNodeID, prefix) || len(prefix) < longestPrefix {
			continue
		}
		if len(prefix) > longestPrefix {
			longestPrefix = len(prefix)
			matched = index
			continue
		}
		return -1, false, errors.New("session coordinator: dynamic include ancestor occurrence is ambiguous")
	}
	if matched >= 0 {
		return matched, true, nil
	}
	return -1, false, nil
}

func sameDynamicOccurrencePath(left, right []schema.DynamicIncludeFrameIdentity) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func executionPlanGraph(
	plan *engine.ExecutionPlan,
	resolutions []*engine.DynamicIncludeResolutionState,
) (json.RawMessage, error) {
	if plan == nil || len(plan.Metadata.DynamicIncludes) == 0 {
		return executionPlanGraphCurrentVersion(plan, resolutions)
	}
	base, err := baseDynamicRevisionPlan(plan)
	if err != nil {
		return nil, err
	}
	baseGraph, err := executionPlanGraphCurrentVersion(base, nil)
	if err != nil {
		return nil, err
	}
	var cumulative graphjson.Document
	if err := decodeHandoffJSON(baseGraph, &cumulative); err != nil {
		return nil, errors.New("session coordinator: base dynamic GraphJSON is invalid")
	}
	pins := append([]schema.LockedDynamicInclude(nil), plan.Metadata.DynamicIncludes...)
	sort.Slice(pins, func(left, right int) bool { return pins[left].Revision < pins[right].Revision })
	occurrenceNodes := make(map[int64]map[string]string)
	for index, pin := range pins {
		if pin.Revision != int64(index+1) {
			return nil, errors.New("session coordinator: dynamic graph revisions are not contiguous")
		}
		working, err := baseDynamicRevisionPlan(base)
		if err != nil {
			return nil, err
		}
		lineage, err := dynamicPinLineage(pins, index)
		if err != nil {
			return nil, err
		}
		for _, lineagePin := range lineage {
			if err := applyDynamicRevisionPin(working, lineagePin); err != nil {
				return nil, err
			}
		}
		currentGraph, err := executionPlanGraphCurrentVersion(working, resolutions)
		if err != nil {
			return nil, err
		}
		var current graphjson.Document
		if err := decodeHandoffJSON(currentGraph, &current); err != nil {
			return nil, errors.New("session coordinator: dynamic occurrence GraphJSON is invalid")
		}
		var parent *schema.LockedDynamicInclude
		if len(lineage) > 1 {
			parent = &lineage[len(lineage)-2]
		}
		if err := appendDynamicOccurrenceGraph(&cumulative, current, pin, parent, occurrenceNodes); err != nil {
			return nil, err
		}
	}
	canonical, err := graphDocumentFromHandoffGraph(cumulative)
	if err != nil {
		return nil, err
	}
	hash, err := canonical.ContentHash()
	if err != nil {
		return nil, err
	}
	cumulative.Hash = hash
	return json.Marshal(cumulative)
}

func dynamicPinLineage(
	pins []schema.LockedDynamicInclude,
	index int,
) ([]schema.LockedDynamicInclude, error) {
	if index < 0 || index >= len(pins) {
		return nil, errors.New("session coordinator: dynamic include lineage index is invalid")
	}
	lineage := []schema.LockedDynamicInclude{pins[index]}
	current := index
	for {
		parent, found, err := dynamicParentPinIndex(pins[:current], pins[current])
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		lineage = append(lineage, pins[parent])
		current = parent
	}
	for left, right := 0, len(lineage)-1; left < right; left, right = left+1, right-1 {
		lineage[left], lineage[right] = lineage[right], lineage[left]
	}
	return lineage, nil
}

func executionPlanGraphCurrent(
	plan *engine.ExecutionPlan,
	resolutions []*engine.DynamicIncludeResolutionState,
) (json.RawMessage, error) {
	return executionPlanGraphCurrentVersion(plan, resolutions)
}

func executionPlanGraphCurrentVersion(
	plan *engine.ExecutionPlan,
	resolutions []*engine.DynamicIncludeResolutionState,
) (json.RawMessage, error) {
	if plan == nil || plan.Validation == nil || plan.Metadata.RunbookID == "" || plan.Metadata.RunbookName == "" {
		return nil, errors.New("session coordinator: finalized plan is required for GraphJSON")
	}
	invocation := &schema.RunbookInvocation{Bindings: plan.Bindings, Outputs: plan.Outputs}
	for _, step := range plan.Steps {
		if step.Depth == 0 && step.Kind == "results" {
			invocation.Results = true
		}
	}
	frames := map[string]graphdoc.Frame{
		"frame:root": {
			ID: "frame:root", RunbookID: plan.Metadata.RunbookID, RunbookPath: plan.RunbookPath,
			Invocation:  graphdoc.InvocationDetails(invocation),
			ContentHash: plan.Metadata.RunbookContentHash, Depth: 0,
		},
	}
	renderedFrames := []graphdoc.Frame{frames["frame:root"]}
	for index := range plan.Steps {
		include, ok := plan.Steps[index].Spec.(*schema.IncludeSpec)
		if !ok || include == nil || include.ResolvedRunbookPath == "" {
			continue
		}
		callPath, err := handoffPlanStepCallPath(plan, index)
		if err != nil {
			return nil, err
		}
		parentID := engine.DebugNodeID(callPath, plan.Steps[index].ID)
		frameID := "frame:" + parentID
		if frames[frameID].ID != "" {
			continue
		}
		runbookID := include.ResolvedRunbookID
		if runbookID == "" {
			runbookID = dynamicRunbookID(resolutions, parentID, include.ResolvedRunbookPath)
		}
		contentHash := include.ResolvedRunbookContentHash
		if contentHash == "" {
			contentHash = dynamicRunbookContentHash(resolutions, parentID, include.ResolvedRunbookPath)
		}
		frame := graphdoc.Frame{
			ID: frameID, RunbookID: runbookID, RunbookPath: include.ResolvedRunbookPath,
			Invocation:          graphdoc.InvocationDetails(schema.InvocationForRunbook(include.ResolvedBindings, include.ResolvedOutputs, include.ResolvedSteps)),
			ParentIncludeNodeID: parentID, QualifiedParentIncludeNodeID: parentID,
			ContentHash: contentHash, Depth: includeDepth(callPath) + 1,
		}
		if parentID != plan.Steps[index].ID {
			frame.QualifiedID = frameID
		}
		frames[frameID] = frame
		renderedFrames = append(renderedFrames, frame)
	}
	nodeBindings := make([]handoffGraphNodeBinding, len(plan.Steps))
	for index, step := range plan.Steps {
		callPath, err := handoffPlanStepCallPath(plan, index)
		if err != nil {
			return nil, err
		}
		frameID := "frame:root"
		for callIndex := len(callPath) - 1; callIndex >= 0; callIndex-- {
			if callPath[callIndex].RunbookPath != "" {
				frameID = "frame:" + engine.DebugNodeID(callPath[:callIndex], callPath[callIndex].StepID)
				break
			}
		}
		nodeBindings[index] = handoffGraphNodeBinding{
			ID: engine.DebugNodeID(callPath, step.ID), StepID: step.ID,
			Kind: step.Kind, FrameID: frameID, Order: index,
		}
	}
	expectedGroups, err := expectedHandoffGraphGroups(plan, nodeBindings, frames)
	if err != nil {
		return nil, err
	}
	for index := range nodeBindings {
		groupID, err := expectedHandoffNodeGroup(plan, index, nodeBindings, expectedGroups)
		if err != nil {
			return nil, err
		}
		nodeBindings[index].GroupID = groupID
	}
	groups := make([]graphdoc.Group, 0, len(expectedGroups))
	groupIDs, err := orderedHandoffGraphGroupIDs(plan, nodeBindings, expectedGroups)
	if err != nil {
		return nil, err
	}
	for _, id := range groupIDs {
		group := expectedGroups[id]
		rawParent := lastDebugNodeSegment(group.ParentNodeID)
		rawFrame := "frame:" + lastDebugNodeSegment(strings.TrimPrefix(group.FrameID, "frame:"))
		rawID := fmt.Sprintf("group:%s:%s:%d", rawParent, group.Kind, group.Index)
		if group.ID != rawID {
			group.QualifiedID = group.ID
			group.QualifiedParentNodeID = group.ParentNodeID
		}
		if group.FrameID != rawFrame {
			group.QualifiedFrameID = group.FrameID
		}
		groups = append(groups, group)
	}
	nodes := make([]graphjson.Node, len(plan.Steps))
	for index, step := range plan.Steps {
		binding := nodeBindings[index]
		callPath, _ := handoffPlanStepCallPath(plan, index)
		callIDs := make([]string, len(callPath))
		for pathIndex := range callPath {
			callIDs[pathIndex] = callPath[pathIndex].StepID
		}
		title := step.Name
		if step.Kind == "results" && title == "" {
			title = "Results"
		}
		if include, ok := step.Spec.(*schema.IncludeSpec); ok && include != nil && include.Include.IsDynamic() {
			title = "⟨dynamic⟩ " + include.Include.RunbookRef
		}
		if iterate, ok := step.Spec.(*schema.IterateNode); ok && iterate != nil {
			title = iterateGraphTitle(iterate)
		}
		data := map[string]any{
			"id": binding.ID, "step_id": step.ID, "call_path": callIDs,
			"kind": step.Kind, "title": title, "group_id": binding.GroupID,
			"frame_id": binding.FrameID, "order": index,
			"details": frozenGraphDetails(index, plan),
		}
		if include, ok := step.Spec.(*schema.IncludeSpec); ok && include != nil && include.Include.IsDynamic() {
			data["dynamic"] = true
		}
		if iterate, ok := step.Spec.(*schema.IterateNode); ok && iterate != nil && iterate.Concurrency > 1 {
			data["concurrent"] = true
		}
		if toolStep, ok := step.Spec.(*schema.ToolCallSpec); ok && toolStep != nil {
			data["tool_name"] = toolStep.Tool.Name
			data["tool_action"] = toolStep.Tool.Action
		}
		nodes[index] = graphjson.Node{
			ID: binding.ID, Type: handoffReactFlowType(step.Kind), Data: data,
			Position: graphjson.Position{X: 0, Y: 0},
		}
		if binding.GroupID != "" {
			nodes[index].ParentNode = binding.GroupID
			nodes[index].Extent = "parent"
		}
	}
	edges := expectedDynamicGraphEdges(nodeBindings, expectedGroups)
	document := graphjson.Document{
		SchemaVersion: graphdoc.SchemaVersion,
		Runbook: graphdoc.RunbookRef{
			ID: plan.Metadata.RunbookID, Name: plan.Metadata.RunbookName, Path: plan.RunbookPath,
		},
		Frames: renderedFrames, Nodes: nodes, Groups: groups, Edges: edges,
		Regions: plan.Metadata.Regions, Inputs: handoffGraphInputDecls(plan),
	}
	for _, frame := range renderedFrames {
		if frame.Invocation != nil {
			document.SchemaVersion = "3"
		}
	}
	for _, step := range plan.Steps {
		if step.Kind == "assign" || step.Kind == "results" {
			document.SchemaVersion = "3"
		}
	}
	canonical, err := graphDocumentFromHandoffGraph(document)
	if err != nil {
		return nil, err
	}
	hash, err := canonical.ContentHash()
	if err != nil {
		return nil, err
	}
	document.Hash = hash
	return json.Marshal(document)
}

func baseDynamicRevisionPlan(plan *engine.ExecutionPlan) (*engine.ExecutionPlan, error) {
	if plan == nil || plan.Validation == nil {
		return nil, errors.New("session coordinator: finalized dynamic plan is required")
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		return nil, err
	}
	base, err := plansnapshot.Restore(snapshot)
	if err != nil {
		return nil, err
	}
	base.Metadata.DynamicIncludes = nil
	base.Metadata.GraphContentHash = ""
	for index := range base.Steps {
		if base.Steps[index].ParentID == "" {
			clearDynamicMaterializations(base.Steps[index].Spec)
		}
	}
	if err := internalplanner.FinalizeMaterializedPlan(base); err != nil {
		return nil, err
	}
	return base, nil
}

func clearDynamicMaterializations(spec engine.StepSpec) {
	switch typed := spec.(type) {
	case *schema.IncludeSpec:
		if typed == nil {
			return
		}
		if typed.Include.IsDynamic() {
			typed.ResolvedSteps = nil
			typed.ResolvedRunbookPath = ""
			typed.ResolvedRunbookID = ""
			typed.ResolvedRunbookName = ""
			typed.ResolvedRunbookContentHash = ""
			typed.ResolvedInputs = nil
			typed.ResolvedBindings = nil
			typed.ResolvedOutputs = nil
			typed.ResolvedGovernance = nil
			return
		}
		clearDynamicFlow(typed.ResolvedSteps)
	case *schema.BranchSpec:
		if typed != nil {
			for index := range typed.Branches {
				clearDynamicFlow(typed.Branches[index].Steps)
			}
		}
	case *schema.IterateNode:
		if typed != nil {
			clearDynamicFlow(typed.Steps)
		}
	case *schema.ParallelNode:
		if typed != nil {
			for index := range typed.Branches {
				clearDynamicFlow(typed.Branches[index].Steps)
			}
		}
	case *schema.CompensateSpec:
		if typed != nil {
			clearDynamicFlow(typed.Compensate.Steps)
		}
	}
}

func clearDynamicFlow(nodes []schema.FlowNode) {
	for index := range nodes {
		node := &nodes[index]
		if node.Step != nil {
			clearDynamicMaterializations(dynamicGraphStepSpec(node.Step))
		}
		if node.Iterate != nil {
			clearDynamicMaterializations(node.Iterate)
		}
		if node.Parallel != nil {
			clearDynamicMaterializations(node.Parallel)
		}
	}
}

func dynamicGraphStepSpec(step *schema.Step) engine.StepSpec {
	if step == nil {
		return nil
	}
	switch step.Type {
	case schema.StepTypeCLI:
		return step.CLI
	case schema.StepTypeTool:
		return step.ToolCall
	case schema.StepTypeInclude:
		return step.IncludeSpec
	case schema.StepTypeChoice:
		return step.ChoiceSpec
	case schema.StepTypeDecision:
		return step.DecisionSpec
	case schema.StepTypeCollector:
		return step.CollectorSpec
	case schema.StepTypeHostAction:
		return step.HostActionSpec
	case schema.StepTypeHandoff:
		return step.HandoffSpec
	case schema.StepTypeBranch:
		return step.BranchSpec
	case schema.StepTypeParallel:
		return step.ParallelSpec
	case schema.StepTypeApprove:
		return step.ApproveSpec
	case schema.StepTypeAssert:
		return step.AssertSpec
	case schema.StepTypeCompensate:
		return step.CompensateSpec
	case schema.StepTypeWaitForEvent:
		return step.WaitForEventSpec
	case schema.StepTypeEnd:
		return step.EndSpec
	case schema.StepTypeNoop:
		return step.NoopSpec
	case schema.StepTypeDisplay:
		return step.DisplaySpec
	default:
		return nil
	}
}

func applyDynamicRevisionPin(plan *engine.ExecutionPlan, pin schema.LockedDynamicInclude) error {
	if err := plansnapshot.ValidateDynamicIncludePin(pin); err != nil {
		return errors.New("session coordinator: dynamic include revision pin is invalid")
	}
	flow, err := plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
	if err != nil {
		return errors.New("session coordinator: dynamic include revision closure is invalid")
	}
	matched, err := dynamicRevisionParentIndex(plan, pin)
	if err != nil {
		return err
	}
	include, ok := plan.Steps[matched].Spec.(*schema.IncludeSpec)
	if !ok || include == nil || !include.Include.IsDynamic() {
		return errors.New("session coordinator: dynamic include revision parent is invalid")
	}
	include.ResolvedSteps = flow
	include.ResolvedRunbookPath = pin.AbsPath
	include.ResolvedRunbookID = pin.RunbookID
	include.ResolvedRunbookName = pin.RunbookName
	include.ResolvedRunbookContentHash = pin.RunbookContentHash
	include.ResolvedInputs = pin.ResolvedInputs
	include.ResolvedBindings = pin.ResolvedBindings
	include.ResolvedOutputs = pin.ResolvedOutputs
	include.ResolvedGovernance = pin.ResolvedGovernance
	if err := attachDynamicPinToCanonicalTree(plan, pin, flow); err != nil {
		return err
	}
	plan.Metadata.DynamicIncludes = append(plan.Metadata.DynamicIncludes, pin)
	if err := internalplanner.FinalizeMaterializedPlan(plan); err != nil {
		return fmt.Errorf("session coordinator: finalize dynamic revision plan: %w", err)
	}
	return nil
}

func dynamicRevisionParentIndex(plan *engine.ExecutionPlan, pin schema.LockedDynamicInclude) (int, error) {
	matched := -1
	for index := range plan.Steps {
		step := &plan.Steps[index]
		if step.ID != pin.StepID || step.Kind != "include" {
			continue
		}
		structuralPath, qualifiedNodeID, err := planDynamicStructuralIdentity(plan, index)
		if err != nil || pin.QualifiedNodeID != "" && pin.QualifiedNodeID != qualifiedNodeID ||
			len(pin.StructuralPath) > 0 && !sameDynamicPlanStructuralPath(pin.StructuralPath, structuralPath) {
			continue
		}
		if matched >= 0 {
			return -1, errors.New("session coordinator: dynamic include revision parent is ambiguous")
		}
		matched = index
	}
	if matched < 0 {
		return -1, errors.New("session coordinator: dynamic include revision parent is unavailable")
	}
	return matched, nil
}

func planDynamicStructuralIdentity(
	plan *engine.ExecutionPlan,
	stepIndex int,
) ([]schema.DynamicIncludeFrameIdentity, string, error) {
	if plan == nil || stepIndex < 0 || stepIndex >= len(plan.Steps) {
		return nil, "", errors.New("session coordinator: dynamic include plan site is invalid")
	}
	var reversed []int
	current := stepIndex
	for plan.Steps[current].ParentID != "" {
		parent, err := handoffPlanParentIndex(plan, current)
		if err != nil {
			return nil, "", err
		}
		reversed = append(reversed, parent)
		current = parent
	}
	ancestors := make([]int, len(reversed))
	for index := range reversed {
		ancestors[len(reversed)-1-index] = reversed[index]
	}
	path := make([]schema.DynamicIncludeFrameIdentity, 0, len(ancestors))
	callIDs := make([]string, 0, len(ancestors))
	for index, ancestorIndex := range ancestors {
		ancestor := plan.Steps[ancestorIndex]
		callIDs = append(callIDs, ancestor.ID)
		childIndex := stepIndex
		if index+1 < len(ancestors) {
			childIndex = ancestors[index+1]
		}
		path = append(path, schema.DynamicIncludeFrameIdentity{
			QualifiedNodeID: strings.Join(callIDs, "/"), Kind: ancestor.Kind,
			BranchLabel: plan.Steps[childIndex].BranchLabel,
		})
	}
	qualifiedParts := append(append([]string(nil), callIDs...), plan.Steps[stepIndex].ID)
	return path, strings.Join(qualifiedParts, "/"), nil
}

func sameDynamicPlanStructuralPath(
	actual []schema.DynamicIncludeFrameIdentity,
	expected []schema.DynamicIncludeFrameIdentity,
) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index].QualifiedNodeID != expected[index].QualifiedNodeID ||
			actual[index].Kind != expected[index].Kind ||
			actual[index].BranchLabel != expected[index].BranchLabel {
			return false
		}
	}
	return true
}

func attachDynamicPinToCanonicalTree(
	plan *engine.ExecutionPlan,
	pin schema.LockedDynamicInclude,
	flow []schema.FlowNode,
) error {
	matches := 0
	for index := range plan.Steps {
		step := &plan.Steps[index]
		if step.ParentID != "" {
			continue
		}
		if step.ID == pin.StepID && step.Kind == "include" &&
			(pin.QualifiedNodeID == "" || pin.QualifiedNodeID == step.ID) && len(pin.StructuralPath) == 0 {
			if include, ok := step.Spec.(*schema.IncludeSpec); ok && include != nil && include.Include.IsDynamic() {
				setDynamicIncludeMaterialization(include, pin, flow)
				matches++
			}
		}
		matches += attachDynamicPinInSpec(step.Spec, schema.DynamicIncludeFrameIdentity{
			QualifiedNodeID: step.ID, Kind: step.Kind, BranchLabel: step.BranchLabel,
		}, pin, flow)
	}
	if matches != 1 {
		return errors.New("session coordinator: canonical dynamic include definition is ambiguous or unavailable")
	}
	return nil
}

func attachDynamicPinInSpec(
	spec engine.StepSpec,
	identity schema.DynamicIncludeFrameIdentity,
	pin schema.LockedDynamicInclude,
	flow []schema.FlowNode,
) int {
	switch typed := spec.(type) {
	case *schema.IncludeSpec:
		if typed == nil || typed.Include.IsDynamic() && len(typed.ResolvedSteps) == 0 {
			return 0
		}
		return attachDynamicPinInFlow(typed.ResolvedSteps, []schema.DynamicIncludeFrameIdentity{identity}, "", pin, flow)
	case *schema.BranchSpec:
		matches := 0
		if typed != nil {
			for _, branch := range typed.Branches {
				branchIdentity := identity
				branchIdentity.BranchLabel = branch.Label
				matches += attachDynamicPinInFlow(branch.Steps, []schema.DynamicIncludeFrameIdentity{branchIdentity}, branch.Label, pin, flow)
			}
		}
		return matches
	case *schema.IterateNode:
		if typed != nil {
			return attachDynamicPinInFlow(typed.Steps, []schema.DynamicIncludeFrameIdentity{identity}, "", pin, flow)
		}
	case *schema.ParallelNode:
		matches := 0
		if typed != nil {
			for _, branch := range typed.Branches {
				branchIdentity := identity
				branchIdentity.BranchLabel = branch.Label
				matches += attachDynamicPinInFlow(branch.Steps, []schema.DynamicIncludeFrameIdentity{branchIdentity}, branch.Label, pin, flow)
			}
		}
		return matches
	case *schema.CompensateSpec:
		if typed != nil {
			return attachDynamicPinInFlow(typed.Compensate.Steps, []schema.DynamicIncludeFrameIdentity{identity}, "", pin, flow)
		}
	}
	return 0
}

func attachDynamicPinInFlow(
	nodes []schema.FlowNode,
	parentPath []schema.DynamicIncludeFrameIdentity,
	branchLabel string,
	pin schema.LockedDynamicInclude,
	flow []schema.FlowNode,
) int {
	matches := 0
	for index := range nodes {
		node := &nodes[index]
		stepID, kind := handoffFlowNodeIdentity(*node)
		if stepID == "" {
			continue
		}
		qualifiedNodeID := stepID
		if len(parentPath) > 0 {
			qualifiedNodeID = parentPath[len(parentPath)-1].QualifiedNodeID + "/" + stepID
		}
		if node.Step != nil && node.Step.IncludeSpec != nil && node.Step.IncludeSpec.Include.IsDynamic() &&
			stepID == pin.StepID && (pin.QualifiedNodeID == "" || pin.QualifiedNodeID == qualifiedNodeID) &&
			(len(pin.StructuralPath) == 0 || sameDynamicPlanStructuralPath(pin.StructuralPath, parentPath)) {
			setDynamicIncludeMaterialization(node.Step.IncludeSpec, pin, flow)
			matches++
		}
		identity := schema.DynamicIncludeFrameIdentity{
			QualifiedNodeID: qualifiedNodeID, Kind: kind, BranchLabel: branchLabel,
		}
		nextPath := append(append([]schema.DynamicIncludeFrameIdentity(nil), parentPath...), identity)
		switch {
		case node.Step != nil:
			matches += attachDynamicPinInNestedStep(node.Step, nextPath, pin, flow)
		case node.Iterate != nil:
			matches += attachDynamicPinInFlow(node.Iterate.Steps, nextPath, "", pin, flow)
		case node.Parallel != nil:
			for _, branch := range node.Parallel.Branches {
				branchPath := append([]schema.DynamicIncludeFrameIdentity(nil), parentPath...)
				branchIdentity := identity
				branchIdentity.BranchLabel = branch.Label
				branchPath = append(branchPath, branchIdentity)
				matches += attachDynamicPinInFlow(branch.Steps, branchPath, branch.Label, pin, flow)
			}
		}
	}
	return matches
}

func attachDynamicPinInNestedStep(
	step *schema.Step,
	path []schema.DynamicIncludeFrameIdentity,
	pin schema.LockedDynamicInclude,
	flow []schema.FlowNode,
) int {
	if step == nil {
		return 0
	}
	if step.IncludeSpec != nil && len(step.IncludeSpec.ResolvedSteps) > 0 {
		return attachDynamicPinInFlow(step.IncludeSpec.ResolvedSteps, path, "", pin, flow)
	}
	matches := 0
	if step.BranchSpec != nil {
		for _, branch := range step.BranchSpec.Branches {
			branchPath := append([]schema.DynamicIncludeFrameIdentity(nil), path[:len(path)-1]...)
			identity := path[len(path)-1]
			identity.BranchLabel = branch.Label
			branchPath = append(branchPath, identity)
			matches += attachDynamicPinInFlow(branch.Steps, branchPath, branch.Label, pin, flow)
		}
	}
	if step.ParallelSpec != nil {
		for _, branch := range step.ParallelSpec.Branches {
			branchPath := append([]schema.DynamicIncludeFrameIdentity(nil), path[:len(path)-1]...)
			identity := path[len(path)-1]
			identity.BranchLabel = branch.Label
			branchPath = append(branchPath, identity)
			matches += attachDynamicPinInFlow(branch.Steps, branchPath, branch.Label, pin, flow)
		}
	}
	if step.CompensateSpec != nil {
		matches += attachDynamicPinInFlow(step.CompensateSpec.Compensate.Steps, path, "", pin, flow)
	}
	return matches
}

func setDynamicIncludeMaterialization(
	include *schema.IncludeSpec,
	pin schema.LockedDynamicInclude,
	flow []schema.FlowNode,
) {
	include.ResolvedSteps = flow
	include.ResolvedRunbookPath = pin.AbsPath
	include.ResolvedRunbookID = pin.RunbookID
	include.ResolvedRunbookName = pin.RunbookName
	include.ResolvedRunbookContentHash = pin.RunbookContentHash
	include.ResolvedInputs = pin.ResolvedInputs
	include.ResolvedBindings = pin.ResolvedBindings
	include.ResolvedOutputs = pin.ResolvedOutputs
	include.ResolvedGovernance = pin.ResolvedGovernance
}

func appendDynamicOccurrenceGraph(
	cumulative *graphjson.Document,
	current graphjson.Document,
	pin schema.LockedDynamicInclude,
	parentPin *schema.LockedDynamicInclude,
	occurrenceNodes map[int64]map[string]string,
) error {
	if current.SchemaVersion == "3" {
		cumulative.SchemaVersion = "3"
	}
	root := pin.QualifiedNodeID
	if root == "" {
		root = pin.StepID
	}
	parent := root
	if parentPin != nil {
		parent = occurrenceNodes[parentPin.Revision][root]
		if parent == "" {
			return errors.New("session coordinator: dynamic graph parent occurrence is unavailable")
		}
	}
	usedNodes := make(map[string]bool, len(cumulative.Nodes))
	for _, node := range cumulative.Nodes {
		if node.ID == "" || usedNodes[node.ID] {
			return errors.New("session coordinator: dynamic graph contains duplicate node identity")
		}
		usedNodes[node.ID] = true
	}
	usedFrames := make(map[string]bool, len(cumulative.Frames))
	for _, frame := range cumulative.Frames {
		if frame.ID == "" || usedFrames[frame.ID] {
			return errors.New("session coordinator: dynamic graph contains duplicate frame identity")
		}
		usedFrames[frame.ID] = true
	}
	usedGroups := make(map[string]bool, len(cumulative.Groups))
	for _, group := range cumulative.Groups {
		if group.ID == "" || usedGroups[group.ID] {
			return errors.New("session coordinator: dynamic graph contains duplicate group identity")
		}
		usedGroups[group.ID] = true
	}
	nodeMap := make(map[string]string)
	for _, node := range current.Nodes {
		if strings.HasPrefix(node.ID, root+"/") {
			nodeMap[node.ID] = allocateDynamicGraphID("node", pin, node.ID, usedNodes)
		}
	}
	frameMap := make(map[string]string)
	for _, frame := range current.Frames {
		runtimeID := strings.TrimPrefix(frame.ID, "frame:")
		if runtimeID != root && !strings.HasPrefix(runtimeID, root+"/") {
			continue
		}
		mappedID := allocateDynamicGraphID("frame", pin, frame.ID, usedFrames)
		frameMap[frame.ID] = mappedID
		frame.ID = mappedID
		frame.QualifiedID = mappedID
		frame.ParentIncludeNodeID = mapDynamicOccurrenceNode(frame.ParentIncludeNodeID, root, parent, nodeMap)
		frame.QualifiedParentIncludeNodeID = frame.ParentIncludeNodeID
		cumulative.Frames = append(cumulative.Frames, frame)
	}
	groupMap := make(map[string]string)
	for _, group := range current.Groups {
		if frameMap[group.FrameID] == "" {
			continue
		}
		groupMap[group.ID] = allocateDynamicGraphID("group", pin, group.ID, usedGroups)
	}
	for _, group := range current.Groups {
		mappedID := groupMap[group.ID]
		if mappedID == "" {
			continue
		}
		group.ID = mappedID
		group.QualifiedID = mappedID
		group.ParentNodeID = mapDynamicOccurrenceNode(group.ParentNodeID, root, parent, nodeMap)
		group.QualifiedParentNodeID = group.ParentNodeID
		group.FrameID = frameMap[group.FrameID]
		group.QualifiedFrameID = group.FrameID
		cumulative.Groups = append(cumulative.Groups, group)
	}
	for _, node := range current.Nodes {
		mappedID := nodeMap[node.ID]
		if mappedID == "" {
			continue
		}
		node.ID = mappedID
		if node.ParentNode != "" {
			node.ParentNode = groupMap[node.ParentNode]
		}
		data := cloneGraphNodeData(node.Data)
		data["id"] = mappedID
		data["frame_id"] = frameMap[graphDataString(data, "frame_id")]
		if groupID := graphDataString(data, "group_id"); groupID != "" {
			data["group_id"] = groupMap[groupID]
		}
		data["order"] = len(cumulative.Nodes)
		node.Data = data
		cumulative.Nodes = append(cumulative.Nodes, node)
	}
	for _, edge := range current.Edges {
		mappedTarget := nodeMap[edge.Target]
		if mappedTarget == "" {
			continue
		}
		mappedSource := nodeMap[edge.Source]
		if edge.Source == root {
			mappedSource = parent
		}
		if mappedSource == "" {
			continue
		}
		edge.ID = "e" + fmt.Sprint(len(cumulative.Edges))
		edge.Source = mappedSource
		edge.Target = mappedTarget
		cumulative.Edges = append(cumulative.Edges, edge)
	}
	occurrenceNodes[pin.Revision] = nodeMap
	return nil
}

func mapDynamicOccurrenceNode(
	value string,
	root string,
	parent string,
	nodeMap map[string]string,
) string {
	if value == root {
		return parent
	}
	if mapped := nodeMap[value]; mapped != "" {
		return mapped
	}
	return value
}

func allocateDynamicGraphID(
	kind string,
	pin schema.LockedDynamicInclude,
	originalID string,
	used map[string]bool,
) string {
	for salt := 0; ; salt++ {
		digest := strings.TrimPrefix(session.DigestJSON(struct {
			Kind       string `json:"kind"`
			Revision   int64  `json:"revision"`
			Occurrence string `json:"occurrence"`
			OriginalID string `json:"original_id"`
			Salt       int    `json:"salt"`
		}{kind, pin.Revision, pin.QualifiedNodeID, originalID, salt}), "sha256:")
		candidate := "yawr:" + kind + ":" + digest
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

func cloneGraphNodeData(source map[string]any) map[string]any {
	encoded, _ := json.Marshal(source)
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	result := make(map[string]any)
	_ = decoder.Decode(&result)
	return result
}

func graphDataString(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

func orderedHandoffGraphGroupIDs(
	plan *engine.ExecutionPlan,
	nodes []handoffGraphNodeBinding,
	groups map[string]graphdoc.Group,
) ([]string, error) {
	children := make(map[int][]int)
	for childIndex := range plan.Steps {
		if plan.Steps[childIndex].ParentID == "" {
			continue
		}
		parentIndex, err := handoffPlanParentIndex(plan, childIndex)
		if err != nil {
			return nil, err
		}
		children[parentIndex] = append(children[parentIndex], childIndex)
	}
	result := make([]string, 0, len(groups))
	var appendParentGroups func(int) error
	appendGroup := func(parentIndex int, kind graphdoc.GroupKind, index int) error {
		id := fmt.Sprintf("group:%s:%s:%d", nodes[parentIndex].ID, kind, index)
		if _, found := groups[id]; !found {
			return errors.New("session coordinator: finalized plan graph group is unavailable")
		}
		result = append(result, id)
		return nil
	}
	appendChildren := func(parentIndex int, armIndex int, hasArm bool) error {
		for _, childIndex := range children[parentIndex] {
			if hasArm {
				index, found, err := handoffStructuralArmIndex(plan.Steps[parentIndex], plan.Steps[childIndex])
				if err != nil {
					return err
				}
				if !found || index != armIndex {
					continue
				}
			}
			if err := appendParentGroups(childIndex); err != nil {
				return err
			}
		}
		return nil
	}
	appendParentGroups = func(parentIndex int) error {
		switch typed := plan.Steps[parentIndex].Spec.(type) {
		case *schema.IncludeSpec:
			if typed != nil && typed.ResolvedRunbookPath != "" {
				if err := appendGroup(parentIndex, graphdoc.GroupIncludeFrame, 0); err != nil {
					return err
				}
				return appendChildren(parentIndex, 0, false)
			}
		case *schema.IterateNode:
			if typed != nil {
				if err := appendGroup(parentIndex, graphdoc.GroupIterateBody, 0); err != nil {
					return err
				}
				return appendChildren(parentIndex, 0, false)
			}
		case *schema.CompensateSpec:
			if typed != nil {
				if err := appendGroup(parentIndex, graphdoc.GroupCompensateBody, 0); err != nil {
					return err
				}
				return appendChildren(parentIndex, 0, false)
			}
		case *schema.BranchSpec:
			if typed != nil {
				for index := range typed.Branches {
					if err := appendGroup(parentIndex, graphdoc.GroupBranchArm, index); err != nil {
						return err
					}
					if err := appendChildren(parentIndex, index, true); err != nil {
						return err
					}
				}
			}
		case *schema.ParallelNode:
			if typed != nil {
				for index := range typed.Branches {
					if err := appendGroup(parentIndex, graphdoc.GroupParallelBranch, index); err != nil {
						return err
					}
					if err := appendChildren(parentIndex, index, true); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	for index := range plan.Steps {
		if plan.Steps[index].ParentID == "" {
			if err := appendParentGroups(index); err != nil {
				return nil, err
			}
		}
	}
	if len(result) != len(groups) {
		return nil, errors.New("session coordinator: finalized plan graph group traversal is incomplete")
	}
	return result, nil
}

func expectedDynamicGraphEdges(
	nodes []handoffGraphNodeBinding,
	groups map[string]graphdoc.Group,
) []graphjson.Edge {
	type edgeDefinition struct {
		source string
		target string
		kind   string
		label  string
	}
	definitions := make([]edgeDefinition, 0, len(nodes))
	lastByScope := make(map[string]handoffGraphNodeBinding)
	for _, node := range nodes {
		scope := "frame\x00" + node.FrameID
		if node.GroupID != "" {
			scope = "group\x00" + node.GroupID
		}
		previous, found := lastByScope[scope]
		if !found && strings.HasPrefix(scope, "group\x00") {
			group := groups[strings.TrimPrefix(scope, "group\x00")]
			kind, label := handoffGroupEntryEdge(group)
			definitions = append(definitions, edgeDefinition{group.ParentNodeID, node.ID, kind, label})
		} else if found {
			definitions = append(definitions, edgeDefinition{previous.ID, node.ID, string(graphdoc.EdgeSequence), ""})
		}
		lastByScope[scope] = node
	}
	edges := make([]graphjson.Edge, len(definitions))
	for index, definition := range definitions {
		edges[index] = graphjson.Edge{
			ID: "e" + fmt.Sprint(index), Source: definition.source, Target: definition.target,
			Type: definition.kind, Label: definition.label,
		}
	}
	return edges
}

func iterateGraphTitle(iterate *schema.IterateNode) string {
	if iterate == nil {
		return ""
	}
	if iterate.Over != "" && iterate.As != "" {
		return "over " + iterate.Over + " as " + iterate.As
	}
	if iterate.Over != "" {
		return "over " + iterate.Over
	}
	return iterate.ID
}

func dynamicRunbookID(
	resolutions []*engine.DynamicIncludeResolutionState,
	qualifiedNodeID string,
	runbookPath string,
) string {
	for _, resolution := range resolutions {
		if resolution.QualifiedNodeID == qualifiedNodeID && filepath.Clean(resolution.Pin.AbsPath) == filepath.Clean(runbookPath) {
			return resolution.Pin.QualifiedID
		}
	}
	name := filepath.Base(runbookPath)
	return strings.TrimSuffix(name, ".runbook.yaml")
}

func dynamicRunbookContentHash(
	resolutions []*engine.DynamicIncludeResolutionState,
	qualifiedNodeID string,
	runbookPath string,
) string {
	for _, resolution := range resolutions {
		if resolution.QualifiedNodeID == qualifiedNodeID &&
			filepath.Clean(resolution.Pin.AbsPath) == filepath.Clean(runbookPath) {
			return strings.TrimPrefix(resolution.Pin.FileDigest, "sha256:")
		}
	}
	return ""
}

func lastDebugNodeSegment(value string) string {
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}
