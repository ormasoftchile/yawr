package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	parserInternal "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/internal/presentationview"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/asciigraph"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/markdown"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/mermaid"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/prose"
)

// runPreview implements `yawr preview <runbook> [flags]`.
//
// Flags:
//
//	--format prose|mermaid|graphjson|asciigraph|markdown  (default prose)
//	--recurse                                    inline included runbooks
//	--out <file>                                 write to file (default stdout)
func runPreview(args []string) int {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "prose", "output format: prose | mermaid | graphjson | asciigraph | markdown")
	recurse := fs.Bool("recurse", false, "inline included runbooks (otherwise opaque)")
	outPath := fs.String("out", "", "write output to file (default: stdout)")
	runDir := fs.String("run-dir", "", "read-only saved run store")
	runID := fs.String("run-id", "", "read-only saved run identity")
	packageMap := fs.String("package-map", "", "local package binding override")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitSuccess
		}
		return exitValidation
	}

	rest := fs.Args()
	if *runDir != "" || *runID != "" {
		if *runDir == "" || *runID == "" || len(rest) != 0 || *format != "graphjson" {
			fmt.Fprintln(os.Stderr, "preview: saved inspection requires --format graphjson --run-dir DIR --run-id ID")
			return exitValidation
		}
		store := runstore.NewDirRunStore(*runDir)
		defer store.Close()
		doc, err := presentationview.Inspect(context.Background(), store, *runID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "preview: saved inspection: %v\n", err)
			return exitFailure
		}
		data, err := graphjson.Render(doc)
		if err != nil {
			return exitFailure
		}
		out, err := openOutput(*outPath)
		if err != nil {
			return exitFailure
		}
		if out != os.Stdout {
			defer out.(io.Closer).Close()
		}
		if _, err = out.Write(append(data, '\n')); err != nil {
			return exitFailure
		}
		return exitSuccess
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: yawr preview <runbook.yaml> [--format prose|mermaid|graphjson] [--recurse] [--out FILE]")
		return exitValidation
	}
	rbPath := rest[0]
	abs, err := filepath.Abs(rbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: resolve path: %v\n", err)
		return exitValidation
	}

	ctx := context.Background()
	p, err := parserInternal.New(platform.Real())
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: parser init: %v\n", err)
		return exitFailure
	}
	rb, err := p.Parse(ctx, abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: parse %s: %v\n", abs, err)
		return exitFailure
	}

	doc, err := (&graphdoc.Builder{Loader: &cliLoader{p: p}, Recurse: *recurse}).Build(ctx, rb)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: build document: %v\n", err)
		return exitFailure
	}
	workspace, _ := os.Getwd()
	packageMapAbs := *packageMap
	if packageMapAbs != "" {
		packageMapAbs, _ = filepath.Abs(packageMapAbs)
	}
	graphdoc.AddCurrentPresentation(doc, presentation.Context{ProjectRoot: workspace, EntrypointPath: abs, PackageMapPath: packageMapAbs})

	out, err := openOutput(*outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: open output: %v\n", err)
		return exitFailure
	}
	defer func() {
		if c, ok := out.(io.Closer); ok && out != os.Stdout {
			_ = c.Close()
		}
	}()

	switch *format {
	case "prose":
		fmt.Fprint(out, prose.Render(doc))
	case "mermaid":
		fmt.Fprint(out, mermaid.Render(doc))
	case "asciigraph":
		fmt.Fprint(out, asciigraph.Render(doc))
	case "markdown":
		fmt.Fprint(out, markdown.Render(doc))
	case "graphjson":
		b, err := graphjson.Render(doc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "preview: render graphjson: %v\n", err)
			return exitFailure
		}
		out.Write(b)
		fmt.Fprintln(out)
	default:
		fmt.Fprintf(os.Stderr, "preview: unknown format %q (want prose|mermaid|graphjson|asciigraph|markdown)\n", *format)
		return exitValidation
	}
	return exitSuccess
}

// cliLoader adapts the internal parser to flowwalk.Loader.
type cliLoader struct{ p parser.Parser }

func (l *cliLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	return l.p.Parse(ctx, path)
}

func openOutput(path string) (io.Writer, error) {
	if path == "" || path == "-" {
		return os.Stdout, nil
	}
	return os.Create(path)
}
