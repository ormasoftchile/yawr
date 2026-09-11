// Package asciigraph renders a graphdoc.Document as an ASCII tree
// suitable for a fixed-width terminal display (the yawr-tui Preview pane).
//
// The output uses Unicode box-drawing characters (├─, │, └─) the same
// way `tree(1)` does. It is intentionally not a true "graph" — it is a
// hierarchical tree view where each Node lives under its enclosing
// Group, and Groups under their parent Node. This matches what users
// expect to see in a terminal and aligns with the prose output's mental
// model.
//
// When a runstate.State is provided, each node line is prefixed with a
// status glyph and (where available) a duration / iteration suffix.
package asciigraph

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	renderidentity "github.com/ormasoftchile/yawr/runtime/pkg/preview/render/internal/identity"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	regschema "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// Render returns the ASCII tree for doc.
func Render(doc *graphdoc.Document) string {
	return RenderWithState(doc, nil)
}

// RenderWithState renders the tree with optional runtime overlay.
func RenderWithState(doc *graphdoc.Document, state *runstate.State) string {
	if doc == nil {
		return ""
	}
	r := &renderer{
		doc:          doc,
		state:        state,
		nodesByGroup: groupChildNodes(doc),
		groupsByNode: nodeChildGroups(doc),
		nodesByFrame: frameTopNodes(doc),
		regionByNode: nodeRegionIndex(doc),
		regionOpened: map[string]bool{},
	}
	var b strings.Builder
	title := doc.Runbook.Name
	if title == "" {
		title = doc.Runbook.ID
	}
	if title != "" {
		fmt.Fprintf(&b, "%s\n", title)
	}
	root := rootFrame(doc)
	if root != nil {
		nodes := r.nodesByFrame[renderidentity.Frame(root)]
		r.renderSiblings(&b, nodes, "")
	}
	return b.String()
}

// renderer carries the precomputed indexes.
type renderer struct {
	doc   *graphdoc.Document
	state *runstate.State

	nodesByGroup map[string][]*graphdoc.Node
	groupsByNode map[string][]*graphdoc.Group
	nodesByFrame map[string][]*graphdoc.Node
	regionByNode map[string]string // nodeID -> regionID for kit-declared regions
	regionOpened map[string]bool   // regionID -> already wrapped at an ancestor level
}

// renderSiblings renders an ordered list of sibling nodes at one indent
// level, grouping consecutive nodes that belong to the same kit-declared
// region into a labelled region block.
func (r *renderer) renderSiblings(b *strings.Builder, nodes []*graphdoc.Node, prefix string) {
	if len(nodes) == 0 {
		return
	}
	// Build runs: each run is either a single non-region node (rid=="")
	// or a maximal contiguous span of nodes sharing the same region ID.
	type run struct {
		rid   string
		items []*graphdoc.Node
	}
	var runs []run
	for _, n := range nodes {
		rid := r.regionByNode[n.ID]
		// Suppress region wrapping for nodes whose region was already
		// opened at an ancestor level (e.g. a region member nested
		// inside one of the region's own branch arms).
		if rid != "" && r.regionOpened[rid] {
			rid = ""
		}
		if len(runs) > 0 && runs[len(runs)-1].rid == rid && rid != "" {
			runs[len(runs)-1].items = append(runs[len(runs)-1].items, n)
			continue
		}
		runs = append(runs, run{rid: rid, items: []*graphdoc.Node{n}})
	}
	for ri, ru := range runs {
		isLast := ri == len(runs)-1
		if ru.rid == "" {
			// Plain sibling — render each child at this indent level.
			for ni, n := range ru.items {
				lastInRun := isLast && ni == len(ru.items)-1
				r.renderNode(b, n, prefix, lastInRun)
			}
			continue
		}
		// Region wrapper: header line then nested members.
		reg := r.findRegion(ru.rid)
		connector := "├─ "
		childPrefix := prefix + "│  "
		if isLast {
			connector = "└─ "
			childPrefix = prefix + "   "
		}
		fmt.Fprintf(b, "%s%s%s\n", prefix, connector, regionHeader(reg))
		r.regionOpened[ru.rid] = true
		for ni, n := range ru.items {
			r.renderNode(b, n, childPrefix, ni == len(ru.items)-1)
		}
		delete(r.regionOpened, ru.rid)
	}
}

func (r *renderer) findRegion(id string) *regschema.Region {
	if r.doc.Regions == nil {
		return nil
	}
	for i := range r.doc.Regions.Regions {
		reg := &r.doc.Regions.Regions[i]
		if reg.ID == id {
			return reg
		}
	}
	return nil
}

func regionHeader(reg *regschema.Region) string {
	if reg == nil {
		return "◇ region"
	}
	label := reg.Label
	if label == "" {
		label = reg.OpType
	}
	if reg.OpType != "" && reg.OpType != label {
		return fmt.Sprintf("◇ %s: %s", reg.OpType, label)
	}
	return fmt.Sprintf("◇ %s", label)
}

// renderNode emits a single node line and recurses into its child
// groups. prefix is the ancestor indentation prefix; isLast controls
// the connector ("└─" vs "├─") and the new prefix passed to children.
func (r *renderer) renderNode(b *strings.Builder, n *graphdoc.Node, prefix string, isLast bool) {
	connector := "├─ "
	childPrefix := prefix + "│  "
	if isLast {
		connector = "└─ "
		childPrefix = prefix + "   "
	}
	fmt.Fprintf(b, "%s%s%s%s%s\n",
		prefix, connector, r.statusPrefix(n), nodeLabel(n), r.statusSuffix(n))

	groups := r.groupsByNode[renderidentity.Node(n)]
	for gi, g := range groups {
		gIsLast := gi == len(groups)-1
		r.renderGroup(b, g, childPrefix, gIsLast)
	}
}

// renderGroup emits a group label (when meaningful) and its children.
func (r *renderer) renderGroup(b *strings.Builder, g *graphdoc.Group, prefix string, isLast bool) {
	connector := "├─ "
	childPrefix := prefix + "│  "
	if isLast {
		connector = "└─ "
		childPrefix = prefix + "   "
	}
	header := groupHeader(g)
	if header != "" {
		fmt.Fprintf(b, "%s%s%s\n", prefix, connector, header)
	} else {
		// Transparent group: children hang directly under the parent
		// node. Reuse the same indent prefix.
		childPrefix = prefix
	}
	r.renderSiblings(b, r.nodesByGroup[renderidentity.Group(g)], childPrefix)
}

// statusPrefix returns the leading status glyph for n, or "" when no
// state overlay is attached.
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

// statusSuffix returns " — iter k/N, Xms, error: …" when applicable.
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

// nodeLabel formats a node title plus its kind tag.
func nodeLabel(n *graphdoc.Node) string {
	title := n.Title
	if title == "" || title == n.ID {
		title = n.ID
	}
	return fmt.Sprintf("%s [%s] (%s)", title, n.Kind, n.ID)
}

// groupHeader returns the visible label for a group, or "" when
// the group should be rendered transparently.
func groupHeader(g *graphdoc.Group) string {
	switch g.Kind {
	case graphdoc.GroupBranchArm:
		if g.Fallback {
			if g.Label != "" {
				return fmt.Sprintf("otherwise — %s", g.Label)
			}
			return "otherwise"
		}
		if g.Label != "" {
			return fmt.Sprintf("if %s", g.Label)
		}
		return "if"
	case graphdoc.GroupParallelBranch:
		if g.Label != "" {
			return fmt.Sprintf("branch %q", g.Label)
		}
		return fmt.Sprintf("branch %d", g.Index+1)
	case graphdoc.GroupCompensateBody:
		return "on failure"
	case graphdoc.GroupIterateBody, graphdoc.GroupIncludeFrame:
		return ""
	}
	return ""
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

// nodeChildGroups returns Node.ID -> ordered child groups.
func nodeChildGroups(doc *graphdoc.Document) map[string][]*graphdoc.Group {
	m := make(map[string][]*graphdoc.Group)
	for i := range doc.Groups {
		g := &doc.Groups[i]
		m[renderidentity.GroupParentNode(g)] = append(m[renderidentity.GroupParentNode(g)], g)
	}
	return m
}

// frameTopNodes returns Frame.ID -> ordered top-level (no group) nodes.
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

func rootFrame(doc *graphdoc.Document) *graphdoc.Frame {
	for i := range doc.Frames {
		if doc.Frames[i].Depth == 0 {
			return &doc.Frames[i]
		}
	}
	return nil
}

// nodeRegionIndex returns nodeID -> regionID for every node that is a
// member of a kit-declared region (see specs/regions-v1.md). Nodes not
// in any region map to "".
func nodeRegionIndex(doc *graphdoc.Document) map[string]string {
	m := make(map[string]string)
	if doc.Regions == nil {
		return m
	}
	for _, r := range doc.Regions.Regions {
		for _, id := range r.Members {
			m[id] = r.ID
		}
	}
	return m
}
