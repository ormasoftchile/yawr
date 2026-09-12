// Package regions builds a domain-shaped view over a runbook's yawr
// structure by composing graphdoc.Document (structure), runstate.State
// (live status), and the runbook's region manifest.
//
// The view is read-only. It is the input renderers consume to draw
// regions as collapsible domain nodes while leaving unregioned yawr
// nodes to render as yawr.
//
// The contract is defined in specs/regions-v1.md.
package regions

import (
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	regschema "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// Status is the aggregated status of a region. Mirrors runstate.Status
// values 1:1 so renderers can use a single status vocabulary for both
// region-collapsed and yawr-expanded views.
type Status = runstate.Status

// RegionView is the per-region overlay produced by Build.
type RegionView struct {
	// Region is the source manifest entry. Always non-nil.
	Region *regschema.Region

	// Members are the node IDs that belong to this region (copied from
	// Region.Members for convenience).
	Members []string

	// Status is the aggregated runtime status. Always set; defaults to
	// StatusPending when no member has reported.
	Status Status

	// MemberStates maps node id → live NodeState for each member that
	// has produced runtime events. Members with no events are omitted.
	MemberStates map[string]runstate.NodeState
}

// EdgeView is a graphdoc.Edge annotated with the regions of its
// endpoints. FromRegion / ToRegion are empty when an endpoint is not
// in any region.
type EdgeView struct {
	From       string
	To         string
	Kind       graphdoc.EdgeKind
	Label      string
	FromRegion string // region id of From, or ""
	ToRegion   string // region id of To, or ""
}

// CrossRegionEdge is one logical inbound or outbound boundary edge
// surfaced when a region is rendered collapsed.
type CrossRegionEdge struct {
	// FromRegion is the source region id, or "" when the edge enters
	// from an unregioned node or from the runbook entry.
	FromRegion string

	// FromNode is the underlying yawr source node id. Useful to a
	// renderer that wants to label or follow the edge.
	FromNode string

	// ToRegion is the destination region id, or "" when the edge exits
	// to an unregioned node or to the runbook end.
	ToRegion string

	// ToNode is the underlying yawr destination node id.
	ToNode string

	// Kind is the underlying graphdoc edge kind.
	Kind graphdoc.EdgeKind
}

// View is the assembled overlay over a runbook.
type View struct {
	// Doc is the structural document used to build the view. Renderers
	// read frame/group/node geometry from here.
	Doc *graphdoc.Document

	// State is the runtime overlay used to build the view, or nil when
	// no live state was provided (preview-only mode).
	State *runstate.State

	// Regions maps region id → RegionView. Empty when the runbook has
	// no manifest.
	Regions map[string]*RegionView

	// NodeRegion maps node id → region id, empty for unregioned nodes.
	NodeRegion map[string]string

	// Edges is the full edge set with region annotations. Renderers
	// that draw the expanded ("yawr") view consume this directly.
	Edges []EdgeView

	// CrossRegionEdges is the deduplicated set of edges that cross at
	// least one region boundary. A renderer that draws regions
	// collapsed iterates this slice. Edges entirely inside a single
	// region (FromRegion == ToRegion and non-empty) are excluded.
	CrossRegionEdges []CrossRegionEdge
}

// Build assembles a View from the trio of inputs.
//
//   - manifest may be nil; the resulting View has no Regions and only
//     the structural overlay.
//   - state may be nil; member statuses default to StatusPending.
//   - doc must be non-nil.
//
// Build does not mutate any input.
func Build(doc *graphdoc.Document, manifest *regschema.Manifest, state *runstate.State) *View {
	v := &View{
		Doc:        doc,
		State:      state,
		Regions:    map[string]*RegionView{},
		NodeRegion: map[string]string{},
	}
	if doc == nil {
		return v
	}

	// Index region membership.
	if manifest != nil {
		for i := range manifest.Regions {
			r := &manifest.Regions[i]
			rv := &RegionView{
				Region:       r,
				Members:      append([]string(nil), r.Members...),
				MemberStates: map[string]runstate.NodeState{},
			}
			for _, id := range r.Members {
				v.NodeRegion[id] = r.ID
			}
			v.Regions[r.ID] = rv
		}
	}

	// Populate per-member runtime state and aggregate per-region
	// status.
	for _, rv := range v.Regions {
		statuses := make([]runstate.Status, 0, len(rv.Members))
		for _, id := range rv.Members {
			if state != nil {
				if ns, ok := lookupNode(state, id); ok {
					rv.MemberStates[id] = ns
					statuses = append(statuses, ns.Status)
					continue
				}
			}
			statuses = append(statuses, runstate.StatusPending)
		}
		rv.Status = aggregate(rv.Region, statuses)
	}

	// Annotate edges with region ids of both endpoints.
	v.Edges = make([]EdgeView, 0, len(doc.Edges))
	for _, e := range doc.Edges {
		v.Edges = append(v.Edges, EdgeView{
			From:       e.From,
			To:         e.To,
			Kind:       e.Kind,
			Label:      e.Label,
			FromRegion: v.NodeRegion[e.From],
			ToRegion:   v.NodeRegion[e.To],
		})
	}

	// Derive cross-region edges (deduplicated by region pair + node
	// pair to keep the surface minimal).
	type key struct {
		fromRegion, toRegion string
		fromNode, toNode     string
		kind                 graphdoc.EdgeKind
	}
	seen := map[key]bool{}
	for _, e := range v.Edges {
		// Skip edges entirely inside one region.
		if e.FromRegion != "" && e.FromRegion == e.ToRegion {
			continue
		}
		// Skip edges entirely outside any region (pure yawr flow).
		if e.FromRegion == "" && e.ToRegion == "" {
			continue
		}
		k := key{e.FromRegion, e.ToRegion, e.From, e.To, e.Kind}
		if seen[k] {
			continue
		}
		seen[k] = true
		v.CrossRegionEdges = append(v.CrossRegionEdges, CrossRegionEdge{
			FromRegion: e.FromRegion,
			FromNode:   e.From,
			ToRegion:   e.ToRegion,
			ToNode:     e.To,
			Kind:       e.Kind,
		})
	}

	return v
}

// lookupNode returns the runtime NodeState for id from state. The
// concrete State.Get returns a default with StatusPending for unseen
// nodes; we only want to count "real" reports here, so we use the
// underlying Nodes map directly via Snapshot.
func lookupNode(state *runstate.State, id string) (runstate.NodeState, bool) {
	snap := state.Snapshot()
	n, ok := snap.Nodes[id]
	return n, ok
}
