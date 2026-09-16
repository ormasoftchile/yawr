package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/internal/sessioncoordinator"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

const maxExecutionGraphBytes = 32 * 1024 * 1024
const executionGraphChunkBytes = 192 * 1024

func sanitizeExecutionGraph(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	document := value.(map[string]any)
	safe := sanitizeStdioValue(document, secrets, redactor).(map[string]any)
	restore := func(target, source map[string]any, keys ...string) {
		for _, key := range keys {
			if value, ok := source[key]; ok {
				target[key] = value
			}
		}
	}
	restore(safe, document, "schema_version", "hash")
	restore(safe["runbook"].(map[string]any), document["runbook"].(map[string]any), "id", "path")
	for _, key := range []string{"nodes", "edges", "groups", "frames"} {
		items, _ := document[key].([]any)
		protected, _ := safe[key].([]any)
		for index, item := range items {
			source, target := item.(map[string]any), protected[index].(map[string]any)
			restore(target, source, "id", "type", "parentNode", "extent", "position", "source", "target",
				"kind", "frame_id", "parent_node_id", "parent_include_node_id", "runbook_id", "depth")
			if key == "nodes" {
				restore(target["data"].(map[string]any), source["data"].(map[string]any),
					"id", "kind", "step_id", "frame_id", "group_id", "call_path", "order", "runtime_node_id")
			}
		}
	}
	return safe
}

// Called under graphMu: graph chunks precede the event/interaction using them.
// This runs in the protocol consumer, never in the execution scheduler.
func (p *stdioProtocol) publishExecutionGraph(state engine.RunState) error {
	if p.graphPublished && len(state.DynamicIncludes) <= p.graphRevision {
		return nil
	}
	digest, ok := p.graphStore.PlanDigest(state.RunID)
	if !ok {
		return fmt.Errorf("execution graph: frozen plan digest is unavailable")
	}
	doc, bindings, err := sessioncoordinator.LiveExecutionGraph(state, digest)
	if err != nil {
		return fmt.Errorf("execution graph: %w", err)
	}
	encoded, err := graphjson.Render(doc)
	if err != nil {
		return err
	}
	if len(encoded) > maxExecutionGraphBytes {
		return fmt.Errorf("execution graph exceeds %d bytes", maxExecutionGraphBytes)
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	p.mu.RLock()
	secrets, redactor := append([]string(nil), p.secrets...), p.redactor
	p.mu.RUnlock()
	// Protect readable content before encoding chunks, not their base64 text.
	safeDocument := sanitizeExecutionGraph(document, secrets, redactor)
	nodeIDs := make([]string, len(bindings))
	for index, binding := range bindings {
		nodeIDs[index] = binding.NodeID
	}
	body, err := json.Marshal(map[string]any{"document": safeDocument, "nodeIDs": nodeIDs})
	if err != nil {
		return err
	}
	if len(body) > maxExecutionGraphBytes {
		return fmt.Errorf("execution graph exceeds %d bytes", maxExecutionGraphBytes)
	}
	hash := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	revision := len(state.DynamicIncludes) + 1
	for offset := 0; offset < len(body); offset += executionGraphChunkBytes {
		end := min(offset+executionGraphChunkBytes, len(body))
		if err := p.sendResultsFrame(map[string]any{
			"version": stdioProtocolVersion, "type": "run.graph.chunk", "runID": state.RunID,
			"revision": revision, "digest": hash, "offset": offset, "totalBytes": len(body),
			"data": base64.StdEncoding.EncodeToString(body[offset:end]),
		}); err != nil {
			return err
		}
	}
	p.graphRevision, p.graphBindings = len(state.DynamicIncludes), bindings
	p.graphPublished = true
	return nil
}

func (p *stdioProtocol) graphNodeID(nodeID string, path []schema.DynamicIncludeFrameIdentity) string {
	mapped := nodeID
	for _, binding := range p.graphBindings {
		if binding.QualifiedNodeID == nodeID && len(path) >= len(binding.StructuralPath) &&
			slices.Equal(path[:len(binding.StructuralPath)], binding.StructuralPath) {
			mapped = binding.NodeID
		}
	}
	return mapped
}

func executionFramePath(state engine.RunState, frameID string) []schema.DynamicIncludeFrameIdentity {
	var result []schema.DynamicIncludeFrameIdentity
	seen := map[string]bool{}
	for frameID != "" && !seen[frameID] {
		seen[frameID] = true
		frame := state.ExecutionFrames[frameID]
		if frame == nil {
			break
		}
		result = append(result, schema.DynamicIncludeFrameIdentity{
			QualifiedNodeID: frame.ParentQualifiedNodeID, Kind: frame.Kind, Invocation: frame.Invocation,
			BranchLabel: frame.BranchLabel, IterationIndex: frame.IterationIndex,
		})
		frameID = frame.ParentFrameID
	}
	slices.Reverse(result)
	return result
}

func (p *stdioProtocol) sendGraphEvent(runID string, event engine.Event) error {
	p.graphMu.Lock()
	defer p.graphMu.Unlock()
	if p.graphStore != nil {
		if event.Kind == "include/resolved" || event.Kind == "step/started" || event.Kind == "step/failed" || event.Kind == "debug/paused" {
			if err := p.publishExecutionGraph(p.handle.State()); err != nil {
				return err
			}
		}
		data, err := json.Marshal(event.Payload)
		if err != nil {
			return err
		}
		var identity struct {
			NodeID string                               `json:"qualified_node_id"`
			Path   []schema.DynamicIncludeFrameIdentity `json:"structural_path"`
		}
		if err := json.Unmarshal(data, &identity); err != nil {
			return err
		}
		if mapped := p.graphNodeID(identity.NodeID, identity.Path); mapped != identity.NodeID {
			var payload map[string]any
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			if err := decoder.Decode(&payload); err != nil {
				return err
			}
			payload["graph_node_id"] = mapped
			event.Payload = payload
		}
	}
	return p.send(map[string]any{"type": "run.event", "runID": runID, "event": event})
}
