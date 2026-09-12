// Package preview offers high-level entry points for building structural
// previews of runbooks. It hides the parser → graphdoc → renderer pipeline
// behind small functions that other modules (yawr-tui, server endpoints,
// extensions) can call without depending on internal packages.
package preview

import (
	"context"

	internalParser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/asciigraph"
)

// ASCIIGraph parses the runbook at path and returns its structural ASCII
// preview. Includes are kept opaque (Recurse=false). On error a one-line
// human-readable message is returned in place of the tree so callers
// rendering into a UI pane can show it directly.
func ASCIIGraph(ctx context.Context, path string) string {
	doc, err := BuildDocument(ctx, path, false)
	if err != nil {
		return "preview error: " + err.Error()
	}
	return asciigraph.Render(doc)
}

// BuildDocument parses the runbook at path and builds a graphdoc.Document.
// Set recurse=true to inline included runbooks.
func BuildDocument(ctx context.Context, path string, recurse bool) (*graphdoc.Document, error) {
	p, err := internalParser.New(platform.Real())
	if err != nil {
		return nil, err
	}
	rb, err := p.Parse(ctx, path)
	if err != nil {
		return nil, err
	}
	return (&graphdoc.Builder{Loader: &fileLoader{p: p}, Recurse: recurse}).Build(ctx, rb)
}

type fileLoader struct{ p parserPkg.Parser }

func (l *fileLoader) Load(ctx context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	return l.p.Parse(ctx, path)
}
