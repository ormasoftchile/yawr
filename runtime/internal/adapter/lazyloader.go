package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// NewParserLazyLoader returns a [internalexecutor.LazyRunbookLoader]
// that parses the included runbook on demand and stamps absolute lazy
// paths onto every nested include site it finds.
//
// Stamping makes laziness contagious: when a top-level include is
// deferred (expand=lazy) and the planner therefore does not walk into
// it, the IncludeExecutor loads its flow at run time via this loader,
// which then ensures any nested include inside that flow also carries a
// LazyRunbookPath. The sub-engine's IncludeExecutor will lazy-load them
// in turn, so the whole transitive subtree never costs any plan-time
// work until — and only if — execution actually reaches it.
//
// Cycle detection runs at load time: a per-load stack catches cycles
// in the same way the eager planner does, but only for the actually
// traversed subtree.
func NewParserLazyLoader(p parser.Parser) internalexecutor.LazyRunbookLoader {
	return &parserLazyLoader{parser: p}
}

type parserLazyLoader struct {
	parser parser.Parser
}

func (l *parserLazyLoader) Load(ctx context.Context, absPath string) (*internalexecutor.LoadedRunbook, error) {
	source, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	return l.LoadSnapshot(ctx, absPath, source)
}

func (l *parserLazyLoader) LoadSnapshot(ctx context.Context, absPath string, source []byte) (*internalexecutor.LoadedRunbook, error) {
	if l.parser == nil {
		return nil, fmt.Errorf("lazy loader: nil parser")
	}
	rb, err := l.parser.ParseBytes(ctx, source)
	if err != nil {
		return nil, err
	}
	if rb == nil || rb.Runbook == nil {
		return nil, fmt.Errorf("lazy loader: %s parsed empty", absPath)
	}
	rb.Source = absPath
	stampLazyIncludes(rb.Runbook.Flow, filepath.Dir(absPath))
	contentHash, err := graphdoc.RunbookContentHash(rb.Runbook)
	if err != nil {
		return nil, err
	}
	return &internalexecutor.LoadedRunbook{
		Bindings: rb.Runbook.Bindings,
		Flow:     rb.Runbook.Flow, Inputs: rb.Runbook.Inputs, Outputs: rb.Runbook.Outputs,
		Governance: rb.Runbook.Governance, ID: rb.Runbook.ID, Name: rb.Runbook.Name, ContentHash: contentHash,
	}, nil
}

// stampLazyIncludes walks nodes and sets IncludeSpec.LazyRunbookPath on
// every include site whose path is not already absolute-stamped, so the
// IncludeExecutor can find the file at run time without re-resolving
// the parent runbook's base directory.
func stampLazyIncludes(nodes []schema.FlowNode, baseDir string) {
	for i := range nodes {
		n := &nodes[i]
		switch {
		case n.Step != nil:
			s := n.Step
			if s.Type == schema.StepTypeInclude && s.IncludeSpec != nil && !s.IncludeSpec.Include.IsDynamic() &&
				s.IncludeSpec.Include.Runbook != "" && s.IncludeSpec.LazyRunbookPath == "" {
				p := s.IncludeSpec.Include.Runbook
				if !filepath.IsAbs(p) {
					p = filepath.Join(baseDir, p)
				}
				s.IncludeSpec.LazyRunbookPath = p
			}
			if s.BranchSpec != nil {
				for j := range s.BranchSpec.Branches {
					stampLazyIncludes(s.BranchSpec.Branches[j].Steps, baseDir)
				}
			}
			if s.CompensateSpec != nil {
				stampLazyIncludes(s.CompensateSpec.Compensate.Steps, baseDir)
			}
		case n.Iterate != nil:
			stampLazyIncludes(n.Iterate.Steps, baseDir)
		case n.Parallel != nil:
			for j := range n.Parallel.Branches {
				stampLazyIncludes(n.Parallel.Branches[j].Steps, baseDir)
			}
		}
	}
}
