// Package markdown renders a graphdoc.Document as a Markdown report
// organised around kit-declared regions (see specs/regions-v1.md).
//
// The report-style output differs from the prose package's
// guide-style numbered list:
//
//   - The runbook becomes a top-level document with a "## Overview"
//     section listing the regions it contains.
//   - Each region becomes a "## <op_type>: <label>" section that lists
//     its members and key metadata (kind, ID, region role).
//   - Steps that are not part of any region are collected under a
//     final "## Steps" section so nothing is dropped.
//
// When no regions are declared the output collapses to a single
// "## Steps" listing — equivalent to a flat markdown summary.
//
// Output is deterministic; Node.Order is the only source of ordering.
package markdown

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	regschema "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// Render returns the Markdown report for doc.
func Render(doc *graphdoc.Document) string {
	if doc == nil {
		return ""
	}
	var b strings.Builder

	title := doc.Runbook.Name
	if title == "" {
		title = doc.Runbook.ID
	}
	if title != "" {
		fmt.Fprintf(&b, "# %s\n\n", title)
	}

	regions := nonEmptyRegions(doc)
	if len(regions) > 0 {
		writeOverview(&b, regions)
		nodesByID := indexNodes(doc)
		for _, reg := range regions {
			writeRegionSection(&b, reg, nodesByID)
		}
	}

	if leftovers := nodesNotInAnyRegion(doc); len(leftovers) > 0 {
		writeStepsSection(&b, leftovers)
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

func writeOverview(b *strings.Builder, regions []*regschema.Region) {
	fmt.Fprintf(b, "## Overview\n\n")
	for _, reg := range regions {
		fmt.Fprintf(b, "- **%s** — %s (%d step%s)\n",
			regionTitle(reg), reg.OpID, len(reg.Members), plural(len(reg.Members)))
	}
	b.WriteString("\n")
}

func writeRegionSection(b *strings.Builder, reg *regschema.Region, nodesByID map[string]*graphdoc.Node) {
	fmt.Fprintf(b, "## %s\n\n", regionTitle(reg))
	if reg.Kit != "" {
		fmt.Fprintf(b, "- Kit: `%s`\n", reg.Kit)
	}
	if reg.OpType != "" {
		fmt.Fprintf(b, "- Op type: `%s`\n", reg.OpType)
	}
	if reg.OpID != "" {
		fmt.Fprintf(b, "- Op id: `%s`\n", reg.OpID)
	}
	b.WriteString("\n")

	entries := setOf(reg.Entries)
	exits := setOf(reg.Exits)

	fmt.Fprintf(b, "### Steps\n\n")
	for _, id := range reg.Members {
		n := nodesByID[id]
		if n == nil {
			fmt.Fprintf(b, "- `%s` — _unknown step_\n", id)
			continue
		}
		role := []string{}
		if entries[id] {
			role = append(role, "entry")
		}
		if exits[id] {
			role = append(role, "exit")
		}
		suffix := ""
		if len(role) > 0 {
			suffix = fmt.Sprintf(" _(%s)_", strings.Join(role, ", "))
		}
		fmt.Fprintf(b, "- **%s** [`%s`] (`%s`)%s\n",
			titleOrID(n), n.Kind, n.ID, suffix)
	}
	b.WriteString("\n")
}

func writeStepsSection(b *strings.Builder, nodes []*graphdoc.Node) {
	fmt.Fprintf(b, "## Steps\n\n")
	for _, n := range nodes {
		fmt.Fprintf(b, "- **%s** [`%s`] (`%s`)\n", titleOrID(n), n.Kind, n.ID)
	}
	b.WriteString("\n")
}

// regionTitle returns "<op_type>: <label>", "<op_type>", or "<label>"
// depending on which fields the region populated.
func regionTitle(reg *regschema.Region) string {
	switch {
	case reg.OpType != "" && reg.Label != "" && reg.Label != reg.OpType:
		return fmt.Sprintf("%s: %s", reg.OpType, reg.Label)
	case reg.Label != "":
		return reg.Label
	case reg.OpType != "":
		return reg.OpType
	default:
		return reg.ID
	}
}

func titleOrID(n *graphdoc.Node) string {
	if n.Title != "" && n.Title != n.ID {
		return n.Title
	}
	return n.ID
}

func nonEmptyRegions(doc *graphdoc.Document) []*regschema.Region {
	if doc.Regions == nil {
		return nil
	}
	out := make([]*regschema.Region, 0, len(doc.Regions.Regions))
	for i := range doc.Regions.Regions {
		reg := &doc.Regions.Regions[i]
		if len(reg.Members) > 0 {
			out = append(out, reg)
		}
	}
	return out
}

func indexNodes(doc *graphdoc.Document) map[string]*graphdoc.Node {
	m := make(map[string]*graphdoc.Node, len(doc.Nodes))
	for i := range doc.Nodes {
		m[doc.Nodes[i].ID] = &doc.Nodes[i]
	}
	return m
}

// nodesNotInAnyRegion returns top-level frame nodes (in deterministic
// order) that are not members of any kit-declared region.
func nodesNotInAnyRegion(doc *graphdoc.Document) []*graphdoc.Node {
	inRegion := map[string]bool{}
	if doc.Regions != nil {
		for _, r := range doc.Regions.Regions {
			for _, id := range r.Members {
				inRegion[id] = true
			}
		}
	}
	root := rootFrame(doc)
	if root == nil {
		return nil
	}
	out := []*graphdoc.Node{}
	for i := range doc.Nodes {
		n := &doc.Nodes[i]
		if n.GroupID != "" || n.FrameID != root.ID {
			continue
		}
		if inRegion[n.ID] {
			continue
		}
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

func rootFrame(doc *graphdoc.Document) *graphdoc.Frame {
	for i := range doc.Frames {
		if doc.Frames[i].Depth == 0 {
			return &doc.Frames[i]
		}
	}
	return nil
}

func setOf(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
