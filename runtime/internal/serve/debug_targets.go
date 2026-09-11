package serve

import (
	"context"
	"errors"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

func (s *Server) validateDebugTargets(ctx context.Context, runbookPath string, config *DebugRunConfig) error {
	if config == nil || !config.Enabled {
		return nil
	}
	doc, err := s.buildPreviewDocument(ctx, runbookPath, true)
	if err != nil {
		return err
	}
	for _, breakpoint := range config.Breakpoints {
		if err := validateDebugTarget(doc, breakpoint.Step, breakpoint.CallPath); err != nil {
			return err
		}
	}
	if config.Profile != nil {
		for _, override := range config.Profile.Overrides {
			if err := validateDebugTarget(doc, override.Target.Step, override.Target.CallPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDebugTarget(doc *graphdoc.Document, stepID string, callPath []engine.DebugCallFrame) error {
	if doc == nil {
		return errors.New("debug target graph is unavailable")
	}
	nodes := make(map[string]*graphdoc.Node, len(doc.Nodes))
	for i := range doc.Nodes {
		node := &doc.Nodes[i]
		nodes[qualifiedDebugNodeID(node)] = node
	}
	target := nodes[engine.DebugNodeID(callPath, stepID)]
	if target == nil || target.StepID != stepID || !sameGraphCallPath(target.CallPath, callPath) {
		return errors.New("debug target does not exist at the selected call path")
	}
	if target.Kind == "parallel" || target.Kind == "wait_for_event" || target.Kind == "compensate" {
		return errors.New("debug target uses an execution path not supported by debugger v1")
	}
	groups := make(map[string]*graphdoc.Group, len(doc.Groups))
	for i := range doc.Groups {
		group := &doc.Groups[i]
		groupID := group.ID
		if group.QualifiedID != "" {
			groupID = group.QualifiedID
		}
		groups[groupID] = group
	}
	groupID := target.GroupID
	if target.QualifiedGroupID != "" {
		groupID = target.QualifiedGroupID
	}
	for groupID != "" {
		group := groups[groupID]
		if group == nil {
			break
		}
		parentID := group.ParentNodeID
		if group.QualifiedParentNodeID != "" {
			parentID = group.QualifiedParentNodeID
		}
		parent := nodes[parentID]
		if parent == nil {
			break
		}
		if parent.Kind == "parallel" || parent.Kind == "compensate" {
			return errors.New("debug target is inside an execution path not supported by debugger v1")
		}
		if parent.Kind == "iterate" && parent.Concurrent {
			return errors.New("debug target is inside a concurrent iterate not supported by debugger v1")
		}
		groupID = parent.GroupID
		if parent.QualifiedGroupID != "" {
			groupID = parent.QualifiedGroupID
		}
	}
	return nil
}

func qualifiedDebugNodeID(node *graphdoc.Node) string {
	if node != nil && node.QualifiedID != "" {
		return node.QualifiedID
	}
	if node == nil {
		return ""
	}
	return node.ID
}

func sameGraphCallPath(path []string, frames []engine.DebugCallFrame) bool {
	if len(path) != len(frames) {
		return false
	}
	for i := range path {
		if path[i] != frames[i].StepID {
			return false
		}
	}
	return true
}
