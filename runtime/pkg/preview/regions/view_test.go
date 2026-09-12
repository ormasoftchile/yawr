package regions_test

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/regions"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	regschema "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// doc builds a minimal graphdoc.Document from a list of node ids and a
// list of edges (from, to). All nodes share the root frame.
func doc(nodeIDs []string, edges [][2]string) *graphdoc.Document {
	d := &graphdoc.Document{
		SchemaVersion: graphdoc.SchemaVersion,
		Runbook:       graphdoc.RunbookRef{ID: "rb"},
		Frames:        []graphdoc.Frame{{ID: "f0", RunbookID: "rb"}},
	}
	for i, id := range nodeIDs {
		d.Nodes = append(d.Nodes, graphdoc.Node{
			ID: id, Kind: "cli", FrameID: "f0", Order: i,
		})
	}
	for _, e := range edges {
		d.Edges = append(d.Edges, graphdoc.Edge{
			From: e[0], To: e[1], Kind: graphdoc.EdgeSequence,
		})
	}
	return d
}

func region(id string, members []string) regschema.Region {
	return regschema.Region{
		ID: id, Kit: "test", OpType: "test.op", OpID: id + "_op",
		Label:   "Region " + id,
		Members: members,
		Entries: []string{members[0]},
		Exits:   []string{members[len(members)-1]},
	}
}

func emptyRegion(id, reason string) regschema.Region {
	return regschema.Region{
		ID: id, Kit: "test", OpType: "test.op", OpID: id + "_op",
		Label: "Region " + id, SkipReason: reason,
	}
}

func manifest(rs ...regschema.Region) *regschema.Manifest {
	return &regschema.Manifest{SchemaVersion: regschema.SchemaVersion, Regions: rs}
}

func stateWith(nodeStatus map[string]runstate.Status) *runstate.State {
	s := runstate.New()
	for id, st := range nodeStatus {
		s.Nodes[id] = runstate.NodeState{ID: id, Status: st}
	}
	return s
}

// --- Build basics ---

func TestBuild_NilDocReturnsEmptyView(t *testing.T) {
	v := regions.Build(nil, nil, nil)
	if v == nil {
		t.Fatal("expected non-nil view")
	}
	if len(v.Regions) != 0 || len(v.Edges) != 0 || len(v.CrossRegionEdges) != 0 {
		t.Fatalf("expected empty view, got %+v", v)
	}
}

func TestBuild_NoManifest_StillAnnotatesEdges(t *testing.T) {
	d := doc([]string{"a", "b"}, [][2]string{{"a", "b"}})
	v := regions.Build(d, nil, nil)
	if len(v.Regions) != 0 {
		t.Fatalf("expected no regions, got %d", len(v.Regions))
	}
	if len(v.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(v.Edges))
	}
	if v.Edges[0].FromRegion != "" || v.Edges[0].ToRegion != "" {
		t.Fatalf("expected unregioned edge, got %+v", v.Edges[0])
	}
	if len(v.CrossRegionEdges) != 0 {
		t.Fatalf("unregioned edges must not appear in CrossRegionEdges")
	}
}

func TestBuild_MembersIndexed(t *testing.T) {
	d := doc([]string{"a", "b"}, nil)
	m := manifest(region("r1", []string{"a", "b"}))
	v := regions.Build(d, m, nil)
	if v.NodeRegion["a"] != "r1" || v.NodeRegion["b"] != "r1" {
		t.Fatalf("expected both nodes mapped to r1, got %+v", v.NodeRegion)
	}
	if rv := v.Regions["r1"]; rv == nil || len(rv.Members) != 2 {
		t.Fatalf("expected r1 with 2 members, got %+v", rv)
	}
}

// --- Status aggregation (stress cases from the spec) ---

func TestAggregate_NoState_DefaultsToPending(t *testing.T) {
	d := doc([]string{"a", "b"}, nil)
	m := manifest(region("r1", []string{"a", "b"}))
	v := regions.Build(d, m, nil)
	if v.Regions["r1"].Status != runstate.StatusPending {
		t.Fatalf("expected pending, got %s", v.Regions["r1"].Status)
	}
}

func TestAggregate_AnyFailedWins(t *testing.T) {
	d := doc([]string{"a", "b", "c"}, nil)
	m := manifest(region("r1", []string{"a", "b", "c"}))
	s := stateWith(map[string]runstate.Status{
		"a": runstate.StatusCompleted,
		"b": runstate.StatusFailed,
		"c": runstate.StatusRunning,
	})
	v := regions.Build(d, m, s)
	if v.Regions["r1"].Status != runstate.StatusFailed {
		t.Fatalf("expected failed, got %s", v.Regions["r1"].Status)
	}
}

func TestAggregate_RunningBeatsPendingAndCompleted(t *testing.T) {
	d := doc([]string{"a", "b"}, nil)
	m := manifest(region("r1", []string{"a", "b"}))
	s := stateWith(map[string]runstate.Status{
		"a": runstate.StatusCompleted,
		"b": runstate.StatusRunning,
	})
	v := regions.Build(d, m, s)
	if v.Regions["r1"].Status != runstate.StatusRunning {
		t.Fatalf("expected running, got %s", v.Regions["r1"].Status)
	}
}

func TestAggregate_AllSkipped(t *testing.T) {
	d := doc([]string{"a", "b"}, nil)
	m := manifest(region("r1", []string{"a", "b"}))
	s := stateWith(map[string]runstate.Status{
		"a": runstate.StatusSkipped,
		"b": runstate.StatusSkipped,
	})
	v := regions.Build(d, m, s)
	if v.Regions["r1"].Status != runstate.StatusSkipped {
		t.Fatalf("expected skipped, got %s", v.Regions["r1"].Status)
	}
}

func TestAggregate_AllCompletedOrSkipped(t *testing.T) {
	d := doc([]string{"a", "b", "c"}, nil)
	m := manifest(region("r1", []string{"a", "b", "c"}))
	s := stateWith(map[string]runstate.Status{
		"a": runstate.StatusCompleted,
		"b": runstate.StatusSkipped,
		"c": runstate.StatusCompleted,
	})
	v := regions.Build(d, m, s)
	if v.Regions["r1"].Status != runstate.StatusCompleted {
		t.Fatalf("expected completed, got %s", v.Regions["r1"].Status)
	}
}

func TestAggregate_EmptyRegionWithSkipReason(t *testing.T) {
	d := doc([]string{"a"}, nil)
	m := manifest(emptyRegion("r0", "severity=emergency"))
	v := regions.Build(d, m, nil)
	if v.Regions["r0"].Status != runstate.StatusSkipped {
		t.Fatalf("expected skipped (empty region with skip_reason), got %s", v.Regions["r0"].Status)
	}
}

// --- Cross-region edges ---

func TestCrossRegionEdges_AggregateBetweenTwoRegions(t *testing.T) {
	// r1 = {a,b}, r2 = {c,d}; edge b→c crosses; a→b internal; c→d internal.
	d := doc(
		[]string{"a", "b", "c", "d"},
		[][2]string{{"a", "b"}, {"b", "c"}, {"c", "d"}},
	)
	m := manifest(region("r1", []string{"a", "b"}), region("r2", []string{"c", "d"}))
	v := regions.Build(d, m, nil)
	if len(v.CrossRegionEdges) != 1 {
		t.Fatalf("expected 1 cross-region edge, got %d (%+v)", len(v.CrossRegionEdges), v.CrossRegionEdges)
	}
	e := v.CrossRegionEdges[0]
	if e.FromRegion != "r1" || e.ToRegion != "r2" || e.FromNode != "b" || e.ToNode != "c" {
		t.Fatalf("unexpected boundary edge: %+v", e)
	}
}

func TestCrossRegionEdges_RegionToUnregioned(t *testing.T) {
	// r1 = {a}; b is unregioned; edge a→b crosses out of r1.
	d := doc([]string{"a", "b"}, [][2]string{{"a", "b"}})
	m := manifest(region("r1", []string{"a"}))
	v := regions.Build(d, m, nil)
	if len(v.CrossRegionEdges) != 1 {
		t.Fatalf("expected 1 cross-region edge, got %d", len(v.CrossRegionEdges))
	}
	if v.CrossRegionEdges[0].FromRegion != "r1" || v.CrossRegionEdges[0].ToRegion != "" {
		t.Fatalf("expected exit-to-unregioned edge, got %+v", v.CrossRegionEdges[0])
	}
}

func TestCrossRegionEdges_InternalEdgesExcluded(t *testing.T) {
	// r1 = {a,b,c}; all edges internal; expect no cross-region edges.
	d := doc(
		[]string{"a", "b", "c"},
		[][2]string{{"a", "b"}, {"b", "c"}},
	)
	m := manifest(region("r1", []string{"a", "b", "c"}))
	v := regions.Build(d, m, nil)
	if len(v.CrossRegionEdges) != 0 {
		t.Fatalf("expected no cross-region edges for internal-only flow, got %+v", v.CrossRegionEdges)
	}
}

func TestCrossRegionEdges_DeduplicatedByEndpointPair(t *testing.T) {
	// Two parallel edges b→c (different kinds count as different keys
	// in v1; same kind dedups). We use the same kind twice via the
	// helper which always emits EdgeSequence.
	d := doc(
		[]string{"a", "b", "c"},
		[][2]string{{"b", "c"}, {"b", "c"}},
	)
	m := manifest(region("r1", []string{"a", "b"}), region("r2", []string{"c"}))
	v := regions.Build(d, m, nil)
	if len(v.CrossRegionEdges) != 1 {
		t.Fatalf("expected duplicate edges to dedup, got %d", len(v.CrossRegionEdges))
	}
}
