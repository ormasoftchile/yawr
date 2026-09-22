package sessioncoordinator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
)

func TestScopedHandoffGraphValidatesCompleteInvocationProjection(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(cwd)), "examples", "dependency-scopes")
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := adapter.PrepareScopedRun(context.Background(), adapter.ScopedRunOptions{
		Catalog:    pkgcatalog.BuildOptions{WorkspaceRoot: root},
		Entrypoint: filepath.Join(root, "static-and-lazy.yawr"), Parser: parser,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := prepared.Plan(context.Background(), expand.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := BuildExecutionPlanGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeHandoffGraph(plan, graph); err != nil {
		t.Fatalf("complete scoped graph rejected: %v", err)
	}
	var document graphjson.Document
	if err := json.Unmarshal(graph, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Nodes) != 14 || len(document.Frames) != 7 {
		t.Fatalf("invocation graph incomplete: %d nodes, %d frames", len(document.Nodes), len(document.Frames))
	}
	nodeFrames := map[string]string{}
	frameDepths := map[string]int{}
	for _, node := range document.Nodes {
		nodeFrames[node.ID] = graphDataString(node.Data, "frame_id")
	}
	for _, frame := range document.Frames {
		frameDepths[frame.ID] = frame.Depth
	}
	for _, frame := range document.Frames {
		if frame.ParentIncludeNodeID == "" {
			continue
		}
		parentFrame, ok := nodeFrames[frame.ParentIncludeNodeID]
		if !ok || frame.Depth != frameDepths[parentFrame]+1 {
			t.Fatalf("frame %s lost its caller depth: parent=%s depth=%d", frame.ID, parentFrame, frame.Depth)
		}
	}
	document.Nodes[0].Data["runtime_node_id"] = "different-owner"
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeHandoffGraph(plan, tampered); err == nil {
		t.Fatal("scoped graph accepted a forged runtime binding")
	}
}
