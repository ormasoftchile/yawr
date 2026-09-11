// Package prose renders a graphdoc.Document as Markdown prose, suitable
// for display next to a runbook source file the way a Markdown preview is
// shown next to a .md file.
//
// The rendering treats the runbook as a Troubleshooting Guide: a numbered
// list of steps, with nested lists under iterate / parallel / branch /
// include parents. Output is deterministic — Node.Order is the only source
// of ordering — so two equal Documents always produce identical Markdown.
package prose

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	renderidentity "github.com/ormasoftchile/yawr/runtime/pkg/preview/render/internal/identity"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

// Render returns the Markdown prose for doc with no runtime overlay.
func Render(doc *graphdoc.Document) string {
	return RenderWithState(doc, nil)
}

// RenderWithState returns the Markdown prose for doc, optionally annotated
// with a runtime state overlay. When state is non-nil, each node line is
// prefixed with a status glyph (✓ / ✗ / ▶ / ⏸ / ·) and, when applicable,
// suffixed with timing info or an error message.
func RenderWithState(doc *graphdoc.Document, state *runstate.State) string {
	if doc == nil {
		return ""
	}
	r := &renderer{
		doc:          doc,
		state:        state,
		nodesByID:    indexNodes(doc),
		nodesByGroup: groupChildNodes(doc),
		groupsByNode: nodeChildGroups(doc),
		nodesByFrame: frameTopNodes(doc),
		framesByNode: frameByIncludeNode(doc),
	}

	var b strings.Builder
	title := doc.Runbook.Name
	if title == "" {
		title = doc.Runbook.ID
	}
	if title != "" {
		fmt.Fprintf(&b, "# %s\n\n", title)
	}

	root := rootFrame(doc)
	if root != nil {
		r.renderNodes(&b, r.nodesByFrame[renderidentity.Frame(root)], 0)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// renderer holds precomputed indexes and emits Markdown.
type renderer struct {
	doc   *graphdoc.Document
	state *runstate.State

	nodesByID    map[string]*graphdoc.Node
	nodesByGroup map[string][]*graphdoc.Node  // group ID -> child nodes (ordered)
	groupsByNode map[string][]*graphdoc.Group // parent node ID -> child groups (ordered)
	nodesByFrame map[string][]*graphdoc.Node  // frame ID -> top-level (group="") nodes (ordered)
	framesByNode map[string]*graphdoc.Frame   // include node ID -> child frame (recurse mode)
}

// renderNodes emits an ordered Markdown list of nodes at the given indent.
func (r *renderer) renderNodes(b *strings.Builder, nodes []*graphdoc.Node, indent int) {
	for i, n := range nodes {
		r.renderNode(b, n, indent, i+1)
	}
}

// renderNode emits one list item plus any nested groups under it.
func (r *renderer) renderNode(b *strings.Builder, n *graphdoc.Node, indent, ordinal int) {
	pad := strings.Repeat("    ", indent)
	prefix := r.statusPrefix(n)
	suffix := r.statusSuffix(n)
	fmt.Fprintf(b, "%s%d. %s%s%s\n", pad, ordinal, prefix, headline(n), suffix)

	for _, g := range r.groupsByNode[renderidentity.Node(n)] {
		r.renderGroup(b, g, indent+1)
	}

	// In recurse mode, an include node also has a child frame whose
	// top-level scope is rendered. The include-frame Group already covers
	// this case via groupsByNode, so nothing extra to do here.
	_ = r.framesByNode
}

// statusPrefix returns the leading status glyph (with trailing space)
// for n, or "" when no overlay is attached.
func (r *renderer) statusPrefix(n *graphdoc.Node) string {
	if r.state == nil {
		return ""
	}
	st := r.state.GetForNode(n.QualifiedID, n.ID)
	switch st.Status {
	case runstate.StatusRunning:
		return "▶ "
	case runstate.StatusCompleted:
		return "✓ "
	case runstate.StatusFailed:
		return "✗ "
	case runstate.StatusSkipped:
		return "⏸ "
	case runstate.StatusCancelled:
		return "⊘ "
	}
	return "· "
}

// statusSuffix returns trailing annotations (timing, error, iteration
// progress) when state is attached, else "".
func (r *renderer) statusSuffix(n *graphdoc.Node) string {
	if r.state == nil {
		return ""
	}
	st := r.state.GetForNode(n.QualifiedID, n.ID)
	parts := []string{}
	if st.Iteration != nil {
		if st.Iteration.Total > 0 {
			parts = append(parts, fmt.Sprintf("iter %d/%d", st.Iteration.Index, st.Iteration.Total))
		} else if st.Iteration.Index > 0 {
			parts = append(parts, fmt.Sprintf("iter %d", st.Iteration.Index))
		}
	}
	if st.DurationMs > 0 {
		parts = append(parts, fmt.Sprintf("%dms", st.DurationMs))
	}
	if st.Error != "" {
		parts = append(parts, fmt.Sprintf("error: %s", st.Error))
	}
	if len(parts) == 0 {
		return ""
	}
	return " — " + strings.Join(parts, ", ")
}

// renderGroup emits the group's heading line (if any) followed by its
// nodes as a nested list at indent+1, or directly at indent for groups
// that don't introduce a heading line of their own.
func (r *renderer) renderGroup(b *strings.Builder, g *graphdoc.Group, indent int) {
	pad := strings.Repeat("    ", indent)
	header := groupHeader(g)
	childIndent := indent
	if header != "" {
		fmt.Fprintf(b, "%s- %s\n", pad, header)
		childIndent = indent + 1
	}
	r.renderNodes(b, r.nodesByGroup[renderidentity.Group(g)], childIndent)
}

// headline returns the bold title line for a node.
func headline(n *graphdoc.Node) string {
	switch n.Kind {
	case "iterate":
		return fmt.Sprintf("**Loop** %s", titleOrID(n))
	case "parallel":
		return fmt.Sprintf("**In parallel** (%s)", n.ID)
	case "branch":
		return fmt.Sprintf("**Decide** %s", titleOrID(n))
	case "include":
		return fmt.Sprintf("**Include** %s", titleOrID(n))
	case "end":
		return fmt.Sprintf("**End** %s", titleOrID(n))
	case "approve":
		return fmt.Sprintf("**Approve** %s", titleOrID(n))
	case "compensate":
		return fmt.Sprintf("**Compensate** %s", titleOrID(n))
	case "tool":
		return fmt.Sprintf("**Tool** %s", titleOrID(n))
	case "cli":
		return fmt.Sprintf("**CLI** %s", titleOrID(n))
	case "display":
		return titleOrID(n)
	case "noop":
		return fmt.Sprintf("_%s_", titleOrID(n))
	default:
		return fmt.Sprintf("%s _(%s)_", titleOrID(n), n.Kind)
	}
}

// groupHeader returns the bullet-line prefix for a group, or "" when the
// group should be rendered transparently (its nodes inline at the same
// indent as their siblings under the parent).
func groupHeader(g *graphdoc.Group) string {
	switch g.Kind {
	case graphdoc.GroupBranchArm:
		if g.Fallback {
			if g.Label != "" {
				return fmt.Sprintf("Otherwise — %s:", g.Label)
			}
			return "Otherwise:"
		}
		if g.Label != "" {
			return fmt.Sprintf("If %s:", g.Label)
		}
		return "Else:"
	case graphdoc.GroupParallelBranch:
		if g.Label != "" {
			return fmt.Sprintf("Branch %q:", g.Label)
		}
		return fmt.Sprintf("Branch %d:", g.Index+1)
	case graphdoc.GroupCompensateBody:
		return "On failure:"
	case graphdoc.GroupIterateBody, graphdoc.GroupIncludeFrame:
		// Iterate body and include frame don't need their own bullet —
		// the parent line ("Loop over X" / "Include path") already
		// announces the nesting.
		return ""
	}
	return ""
}

// titleOrID returns the node title falling back to its ID.
func titleOrID(n *graphdoc.Node) string {
	if n.Title != "" && n.Title != n.ID {
		return fmt.Sprintf("%s (`%s`)", n.Title, n.ID)
	}
	return fmt.Sprintf("`%s`", n.ID)
}

// indexNodes builds an ID→Node map.
func indexNodes(doc *graphdoc.Document) map[string]*graphdoc.Node {
	m := make(map[string]*graphdoc.Node, len(doc.Nodes))
	for i := range doc.Nodes {
		node := &doc.Nodes[i]
		m[renderidentity.Node(node)] = node
	}
	return m
}

// groupChildNodes returns Group.ID -> ordered child nodes.
func groupChildNodes(doc *graphdoc.Document) map[string][]*graphdoc.Node {
	m := make(map[string][]*graphdoc.Node)
	for i := range doc.Nodes {
		n := &doc.Nodes[i]
		if n.GroupID == "" {
			continue
		}
		m[renderidentity.NodeGroup(n)] = append(m[renderidentity.NodeGroup(n)], n)
	}
	for k := range m {
		sort.SliceStable(m[k], func(i, j int) bool { return m[k][i].Order < m[k][j].Order })
	}
	return m
}

// nodeChildGroups returns parent Node.ID -> ordered child groups.
// Order is index-then-insertion to keep parallel branches and branch arms
// in declaration order.
func nodeChildGroups(doc *graphdoc.Document) map[string][]*graphdoc.Group {
	m := make(map[string][]*graphdoc.Group)
	for i := range doc.Groups {
		g := &doc.Groups[i]
		m[renderidentity.GroupParentNode(g)] = append(m[renderidentity.GroupParentNode(g)], g)
	}
	return m
}

// frameTopNodes returns Frame.ID -> ordered top-level (GroupID=="") nodes.
func frameTopNodes(doc *graphdoc.Document) map[string][]*graphdoc.Node {
	m := make(map[string][]*graphdoc.Node)
	for i := range doc.Nodes {
		n := &doc.Nodes[i]
		if n.GroupID != "" {
			continue
		}
		m[renderidentity.NodeFrame(n)] = append(m[renderidentity.NodeFrame(n)], n)
	}
	for k := range m {
		sort.SliceStable(m[k], func(i, j int) bool { return m[k][i].Order < m[k][j].Order })
	}
	return m
}

// frameByIncludeNode returns include-node-ID -> child Frame (recurse mode).
func frameByIncludeNode(doc *graphdoc.Document) map[string]*graphdoc.Frame {
	m := make(map[string]*graphdoc.Frame)
	for i := range doc.Frames {
		f := &doc.Frames[i]
		if renderidentity.FrameParentNode(f) != "" {
			m[renderidentity.FrameParentNode(f)] = f
		}
	}
	return m
}

// rootFrame returns the depth-0 frame, or nil if the document is empty.
func rootFrame(doc *graphdoc.Document) *graphdoc.Frame {
	for i := range doc.Frames {
		if doc.Frames[i].Depth == 0 {
			return &doc.Frames[i]
		}
	}
	return nil
}
