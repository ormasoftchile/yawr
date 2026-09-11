// Package mermaid renders a graphdoc.Document as a Mermaid flowchart.
//
// Output is a `flowchart TD` graph where:
//   - Each Node becomes a node with a shape chosen by Kind:
//   - branch / decision / choice → diamond  ({…})
//   - iterate / parallel         → trapezoid ([/…/])
//   - approve                    → stadium  ([… ])
//   - end                        → rounded   (((…)))
//   - everything else            → rectangle ([…])
//   - Each Edge becomes a typed arrow:
//   - sequence       → -->
//   - branch-arm     → -->|label|
//   - parallel-branch→ -->|label|
//   - iterate-body   → -. body .->
//   - include        → ==>
//   - compensate     → -.->|on failure|
//   - Frames map to subgraphs (root frame is the outer flowchart; each
//     included frame is a `subgraph frame_<id> ... end` block grouping
//     its nodes for visual nesting).
//
// Output is deterministic for a given Document (Node.Order is preserved).
package mermaid

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

// Render returns the Mermaid source for doc with no runtime overlay.
func Render(doc *graphdoc.Document) string {
	return RenderWithState(doc, nil)
}

// RenderWithState returns Mermaid source for doc, optionally annotated
// with a runtime state overlay. Each Node gets a `class:<status>`
// directive (running / completed / failed / skipped / cancelled /
// pending), and the file ends with `classDef` blocks defining the
// matching colors.
func RenderWithState(doc *graphdoc.Document, state *runstate.State) string {
	if doc == nil {
		return "flowchart TD\n"
	}
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	identity := newRenderIdentity(doc)

	// Group nodes by FrameID, preserving Node.Order.
	frameNodes := make(map[string][]*graphdoc.Node)
	for i := range doc.Nodes {
		n := &doc.Nodes[i]
		frameNodes[identity.nodeFrameID(n)] = append(frameNodes[identity.nodeFrameID(n)], n)
	}
	for k := range frameNodes {
		sort.SliceStable(frameNodes[k], func(i, j int) bool {
			return frameNodes[k][i].Order < frameNodes[k][j].Order
		})
	}

	// Emit frames in document order; root frame's nodes go at top level,
	// every other frame becomes a subgraph.
	for i := range doc.Frames {
		f := &doc.Frames[i]
		frameID := identity.frameID(f)
		nodes := frameNodes[frameID]
		if len(nodes) == 0 {
			continue
		}
		if f.Depth == 0 {
			emitNodes(&b, nodes, "  ", identity)
			continue
		}
		fmt.Fprintf(&b, "  subgraph %s [included: %s]\n",
			identity.renderFrameID(frameID), escapeLabel(f.RunbookID))
		emitNodes(&b, nodes, "    ", identity)
		b.WriteString("  end\n")
	}

	for _, e := range doc.Edges {
		fmt.Fprintf(&b, "  %s\n", edgeLine(e, identity))
	}

	if state != nil {
		writeStatusAnnotations(&b, doc, state, identity)
	}
	return b.String()
}

// emitNodes writes node declarations at the given indent.
func emitNodes(b *strings.Builder, nodes []*graphdoc.Node, indent string, identity renderIdentity) {
	for _, n := range nodes {
		fmt.Fprintf(b, "%s%s\n", indent, nodeDecl(n, identity))
	}
}

// nodeDecl returns the `id["label"]` declaration with the right shape.
func nodeDecl(n *graphdoc.Node, identity renderIdentity) string {
	id := identity.renderNodeID(identity.nodeID(n))
	label := escapeLabel(nodeLabel(n))
	switch n.Kind {
	case "branch", "decision", "choice":
		return fmt.Sprintf("%s{%q}", id, label)
	case "iterate":
		return fmt.Sprintf("%s[/%q/]", id, label)
	case "parallel":
		return fmt.Sprintf("%s[/%q\\]", id, label)
	case "approve":
		return fmt.Sprintf("%s([%q])", id, label)
	case "end":
		return fmt.Sprintf("%s(((%q)))", id, label)
	case "include":
		return fmt.Sprintf("%s[[%q]]", id, label)
	case "noop", "display":
		return fmt.Sprintf("%s>%q]", id, label)
	default:
		return fmt.Sprintf("%s[%q]", id, label)
	}
}

// edgeLine returns one Mermaid edge line.
func edgeLine(e graphdoc.Edge, identity renderIdentity) string {
	from, to := identity.renderNodeID(identity.edgeFrom(e)), identity.renderNodeID(identity.edgeTo(e))
	switch e.Kind {
	case graphdoc.EdgeSequence:
		return fmt.Sprintf("%s --> %s", from, to)
	case graphdoc.EdgeBranchArm, graphdoc.EdgeParallelBranch:
		if e.Label == "" {
			return fmt.Sprintf("%s --> %s", from, to)
		}
		return fmt.Sprintf("%s -->|%s| %s", from, escapeArrowLabel(e.Label), to)
	case graphdoc.EdgeIterateBody:
		return fmt.Sprintf("%s -. body .-> %s", from, to)
	case graphdoc.EdgeInclude:
		return fmt.Sprintf("%s ==> %s", from, to)
	case graphdoc.EdgeCompensate:
		label := e.Label
		if label == "" {
			label = "on failure"
		}
		return fmt.Sprintf("%s -.->|%s| %s", from, escapeArrowLabel(label), to)
	}
	// Unknown kinds fall through to a plain edge so output remains valid.
	return fmt.Sprintf("%s --> %s", from, to)
}

// nodeLabel returns the human-readable label shown inside a node.
func nodeLabel(n *graphdoc.Node) string {
	if n.Title != "" && n.Title != n.ID {
		return n.Title
	}
	return n.ID
}

// safeID makes an ID safe to use as a Mermaid node identifier.
// Mermaid IDs must not contain spaces or punctuation that overlaps with
// its arrow / shape syntax. We replace anything not [A-Za-z0-9_] with _.
func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "n_"
	}
	return b.String()
}

// escapeLabel escapes characters that would break a Mermaid quoted label.
// Inside "..." Mermaid does not interpret most punctuation; the only
// characters we need to neutralize are " and embedded newlines.
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// escapeArrowLabel escapes characters illegal in an unquoted arrow label
// `-->|label|`. Pipes terminate the label, so we replace them.
func escapeArrowLabel(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// frameSubgraphBase returns the preferred Mermaid identifier for a frame.
func frameSubgraphBase(frameID string) string {
	return "frame_" + safeID(strings.TrimPrefix(frameID, "frame:"))
}

type renderIdentity struct {
	nodeCounts     map[string]int
	frameCounts    map[string]int
	renderedNodes  map[string]string
	renderedFrames map[string]string
}

func newRenderIdentity(doc *graphdoc.Document) renderIdentity {
	identity := renderIdentity{
		nodeCounts: map[string]int{}, frameCounts: map[string]int{},
		renderedNodes: map[string]string{}, renderedFrames: map[string]string{},
	}
	for i := range doc.Nodes {
		identity.nodeCounts[doc.Nodes[i].ID]++
	}
	for i := range doc.Frames {
		identity.frameCounts[doc.Frames[i].ID]++
	}
	used := make(map[string]bool, len(doc.Nodes)+len(doc.Frames))
	for i := range doc.Nodes {
		logicalID := identity.nodeID(&doc.Nodes[i])
		if _, exists := identity.renderedNodes[logicalID]; !exists {
			identity.renderedNodes[logicalID] = allocateMermaidID(safeID(logicalID), used)
		}
	}
	for i := range doc.Frames {
		frame := &doc.Frames[i]
		if frame.Depth == 0 {
			continue
		}
		logicalID := identity.frameID(frame)
		if _, exists := identity.renderedFrames[logicalID]; !exists {
			identity.renderedFrames[logicalID] = allocateMermaidID(frameSubgraphBase(logicalID), used)
		}
	}
	return identity
}

func allocateMermaidID(base string, used map[string]bool) string {
	candidate := base
	for suffix := 2; used[candidate]; suffix++ {
		candidate = fmt.Sprintf("%s_%d", base, suffix)
	}
	used[candidate] = true
	return candidate
}

func (identity renderIdentity) renderNodeID(logicalID string) string {
	if rendered := identity.renderedNodes[logicalID]; rendered != "" {
		return rendered
	}
	return safeID(logicalID)
}

func (identity renderIdentity) renderFrameID(logicalID string) string {
	if rendered := identity.renderedFrames[logicalID]; rendered != "" {
		return rendered
	}
	return frameSubgraphBase(logicalID)
}

func (identity renderIdentity) nodeID(node *graphdoc.Node) string {
	if identity.nodeCounts[node.ID] > 1 && node.QualifiedID != "" {
		return node.QualifiedID
	}
	return node.ID
}

func (identity renderIdentity) nodeFrameID(node *graphdoc.Node) string {
	if identity.frameCounts[node.FrameID] > 1 && node.QualifiedFrameID != "" {
		return node.QualifiedFrameID
	}
	return node.FrameID
}

func (identity renderIdentity) frameID(frame *graphdoc.Frame) string {
	if identity.frameCounts[frame.ID] > 1 && frame.QualifiedID != "" {
		return frame.QualifiedID
	}
	return frame.ID
}

func (identity renderIdentity) edgeFrom(edge graphdoc.Edge) string {
	if identity.nodeCounts[edge.From] > 1 && edge.QualifiedFrom != "" {
		return edge.QualifiedFrom
	}
	return edge.From
}

func (identity renderIdentity) edgeTo(edge graphdoc.Edge) string {
	if identity.nodeCounts[edge.To] > 1 && edge.QualifiedTo != "" {
		return edge.QualifiedTo
	}
	return edge.To
}

// writeStatusAnnotations appends `class` directives per node and a set of
// `classDef` blocks defining the visual styles for each runstate status.
func writeStatusAnnotations(b *strings.Builder, doc *graphdoc.Document, state *runstate.State, identity renderIdentity) {
	for i := range doc.Nodes {
		n := &doc.Nodes[i]
		st := state.GetForNode(n.QualifiedID, n.ID)
		fmt.Fprintf(b, "  class %s status_%s\n", identity.renderNodeID(identity.nodeID(n)), st.Status)
	}
	b.WriteString("  classDef status_pending fill:#f5f5f5,stroke:#999,color:#666\n")
	b.WriteString("  classDef status_running fill:#fff8c5,stroke:#bf8700,color:#1f1f1f,stroke-width:2px\n")
	b.WriteString("  classDef status_completed fill:#dafbe1,stroke:#1a7f37,color:#1f1f1f\n")
	b.WriteString("  classDef status_failed fill:#ffebe9,stroke:#cf222e,color:#1f1f1f\n")
	b.WriteString("  classDef status_skipped fill:#eaeaea,stroke:#777,color:#555,stroke-dasharray: 4 2\n")
	b.WriteString("  classDef status_cancelled fill:#eaeaea,stroke:#777,color:#555,stroke-dasharray: 2 2\n")
}
