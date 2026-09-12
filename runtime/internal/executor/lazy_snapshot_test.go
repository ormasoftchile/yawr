package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type pathMutatingLazyLoader struct {
	replacement []byte
}

func (loader pathMutatingLazyLoader) Load(_ context.Context, path string) (*LoadedRunbook, error) {
	if err := os.WriteFile(path, loader.replacement, 0o600); err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &LoadedRunbook{Flow: []schema.FlowNode{{Step: &schema.Step{
		ID: strings.TrimSpace(string(contents)), Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}}}, nil
}

func TestDigestBoundLazyIncludeNeverReopensPathForParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.runbook.yaml")
	if err := os.WriteFile(path, []byte("bound-child"), 0o600); err != nil {
		t.Fatalf("write bound child: %v", err)
	}
	digest, err := pkgcatalog.FileDigest(path)
	if err != nil {
		t.Fatalf("digest bound child: %v", err)
	}
	var executed string
	runner := func(_ context.Context, _ SubStepParent, nodes []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		executed = nodes[0].Step.ID
		return nil, nil
	}
	executorImpl := NewIncludeExecutor(nil, runner, pathMutatingLazyLoader{replacement: []byte("changed-child")})
	step := engine.ResolvedStep{ID: "include", Kind: "include", Spec: &schema.IncludeSpec{
		Include:         schema.IncludeConfig{Runbook: "child.runbook.yaml", Expand: "lazy"},
		LazyRunbookPath: path, LazyRunbookDigest: digest,
	}}
	if _, executeErr := executorImpl.Execute(context.Background(), step, nil); executeErr == nil {
		t.Fatal("digest-bound lazy include accepted a path-only loader")
	}
	if executed != "" {
		t.Fatalf("changed child executed after digest binding: %s", executed)
	}
}
