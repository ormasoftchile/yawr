package markdown_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/markdown"
)

func collectHealthPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	return filepath.Join(pkgDir, "testdata", "examples", "collect-health", "collect-health.runbook.yaml")
}

func driChangeRequestPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	path := filepath.Join(pkgDir, "testdata", "yawr-domain-dri", "pkg", "compiler", "testdata", "change-request.golden.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skip("fixture missing: yawr-domain-dri/pkg/compiler/testdata/change-request.golden.yaml")
	}
	return path
}

type fileLoader struct{ p parserPkg.Parser }

func (fl *fileLoader) Load(ctx context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	return fl.p.Parse(ctx, path)
}

func buildDoc(t *testing.T, path string) *graphdoc.Document {
	t.Helper()
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	doc, err := (&graphdoc.Builder{Loader: &fileLoader{p: p}}).Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return doc
}

// TestRender_NoRegions: a runbook without a regions manifest should
// produce a single "## Steps" section listing every top-level node.
func TestRender_NoRegions(t *testing.T) {
	got := markdown.Render(buildDoc(t, collectHealthPath()))
	for _, sub := range []string{
		"# collect-health",
		"## Steps",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("expected %q in:\n%s", sub, got)
		}
	}
	if strings.Contains(got, "## Overview") {
		t.Errorf("did not expect Overview section without regions:\n%s", got)
	}
}

// TestRender_Regions: a runbook with a regions manifest should produce
// an Overview section and one heading per region.
func TestRender_Regions(t *testing.T) {
	doc := buildDoc(t, driChangeRequestPath(t))
	got := markdown.Render(doc)

	for _, sub := range []string{
		"## Overview",
		"## ops.change-request: Deploy service",
		"### Steps",
		"- Op type: `ops.change-request`",
		"_(entry)_",
		"_(exit)_",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("expected %q in:\n%s", sub, got)
		}
	}

	// Overview must precede the per-region section.
	if strings.Index(got, "## Overview") >= strings.Index(got, "## ops.change-request") {
		t.Errorf("Overview should precede region sections:\n%s", got)
	}
}
