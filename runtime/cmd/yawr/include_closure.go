package main

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// checkIncludeClosureLexicalScoping implements the fail-closed fallback
// Barbara's gate review explicitly sanctions for §5 ("(b) Fail closed. ...
// make an included runbook that declares requires: or toolRefs: a hard,
// typed, diagnosable error, and document the single-file restriction.
// Silently dynamic-scoped resolution is the one outcome I will not
// accept"): design/yawr/sections/06-tool-runtime.tex §Includes, Lexical
// Scoping, and Global Package Set rejects dynamic scoping as "the
// decisive call" because it makes a child runbook's meaning depend on who
// included it. This runtime revision does not yet implement full
// per-file lexical toolRefs binding (option (a) in the review); rather
// than silently resolving an included file's own requires:/toolRefs:
// against the root's dynamically-scoped, process-global tool registry
// (the exact failure mode the ruling forbids), it refuses to run at all
// -- with PKG-017 (late package binding after catalog freeze), which is
// the closest existing sentinel to "a package/tool binding declared
// somewhere the frozen, root-only resolution model cannot honor."
//
// Every runbook reached transitively through an include is checked as
// soon as it is loaded (EnterInclude receives the already-parsed child);
// only the root runbook itself (never observed via EnterInclude) may
// declare requires:/toolRefs:.
func checkIncludeClosureLexicalScoping(ctx context.Context, parserImpl parser.Parser, rb *parser.ParsedRunbook) []error {
	if rb == nil || rb.Runbook == nil {
		return nil
	}
	v := &closureLexicalVisitor{}
	w := &flowwalk.Walker{Loader: &cliLoader{p: parserImpl}}
	if err := w.Walk(ctx, rb, v); err != nil {
		v.errs = append(v.errs, err)
	}
	return v.errs
}

type closureLexicalVisitor struct {
	flowwalk.Base
	errs []error
}

func (v *closureLexicalVisitor) BeforeInclude(_ flowwalk.Ctx, step *schema.Step) (bool, error) {
	// Dynamic includes have no statically-known path; there is nothing for
	// lexical-scoping analysis to load or check at this site. Skip rather
	// than fabricating a path from the empty Runbook field.
	if step.IncludeSpec != nil && step.IncludeSpec.Include.IsDynamic() {
		return false, nil
	}
	return true, nil
}

func (v *closureLexicalVisitor) EnterInclude(_ flowwalk.Ctx, step *schema.Step, childRb *parser.ParsedRunbook) (bool, error) {
	if childRb != nil && childRb.Runbook != nil {
		if len(childRb.Runbook.Requires) > 0 {
			v.errs = append(v.errs, errkit.New("PKG-017", fmt.Sprintf(
				"included runbook %q declares requires: (%d entries); per-file package requirements are not supported by this runtime -- declare every required package on the root runbook or project config instead", childRb.Source, len(childRb.Runbook.Requires))))
		}
		if len(childRb.Runbook.ToolRefs) > 0 {
			v.errs = append(v.errs, errkit.New("PKG-017", fmt.Sprintf(
				"included runbook %q declares toolRefs: (%d entries); lexical per-file tool binding for included runbooks is not supported by this runtime -- declare every tool binding on the root runbook instead", childRb.Source, len(childRb.Runbook.ToolRefs))))
		}
	}
	return childRb != nil, nil
}
