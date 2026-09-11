package planner_test

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerPkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// failingLoader fails on any Load call. Used to assert that a lazy
// include site does not trigger any load at plan time.
type failingLoader struct {
	t *testing.T
}

func (f *failingLoader) Load(_ context.Context, path string) (*parser.ParsedRunbook, error) {
	f.t.Errorf("loader called for %q; expected lazy plan to skip loads", path)
	return nil, &plannerPkg.PlanError{Code: plannerPkg.ErrRunbookNotFound, Path: path}
}

func parentRunbookWithIncludes(includeRunbooks ...string) *parser.ParsedRunbook {
	flow := make([]schema.FlowNode, 0, len(includeRunbooks))
	for i, p := range includeRunbooks {
		flow = append(flow, schema.FlowNode{Step: &schema.Step{
			ID:   "inc" + string(rune('0'+i)),
			Type: schema.StepTypeInclude,
			IncludeSpec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: p},
			},
		}})
	}
	return &parser.ParsedRunbook{
		Source: "/abs/parent.runbook.yaml",
		Runbook: &schema.Runbook{
			ID:   "parent",
			Name: "parent",
			Flow: flow,
		},
	}
}

// TestLazy_PolicyDefaultDoesNotLoad ensures that when the planner's
// policy default is lazy, the loader is never invoked at plan time and
// each include is emitted as a deferred step carrying the absolute
// child path.
func TestLazy_PolicyDefaultDoesNotLoad(t *testing.T) {
	rb := parentRunbookWithIncludes("./a.runbook.yaml", "./b.runbook.yaml", "./c.runbook.yaml")
	p := planner.New(plannerPkg.Config{
		Loader:       &failingLoader{t: t},
		Tools:        &fakeRegistry{tools: map[string]*schema.ToolDef{}},
		ExpandPolicy: expand.Policy{Default: expand.ModeLazy},
	})

	plan, err := p.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Steps) != 3 {
		t.Fatalf("expected 3 include steps, got %d", len(plan.Steps))
	}
	for i, s := range plan.Steps {
		spec, ok := s.Spec.(*schema.IncludeSpec)
		if !ok {
			t.Fatalf("step %d: spec is not *IncludeSpec: %T", i, s.Spec)
		}
		if len(spec.ResolvedSteps) != 0 {
			t.Errorf("step %d: ResolvedSteps should be empty for lazy, got %d", i, len(spec.ResolvedSteps))
		}
		if spec.LazyRunbookPath == "" {
			t.Errorf("step %d: LazyRunbookPath should be set", i)
		}
	}
}

// TestLazy_RunbookFieldOverridesPolicyEager: when the planner default is
// eager but the runbook says lazy, includes are deferred.
func TestLazy_RunbookFieldOverridesPolicyEager(t *testing.T) {
	rb := parentRunbookWithIncludes("./a.runbook.yaml")
	rb.Runbook.Expand = "lazy"
	p := planner.New(plannerPkg.Config{
		Loader:       &failingLoader{t: t},
		Tools:        &fakeRegistry{tools: map[string]*schema.ToolDef{}},
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})
	plan, err := p.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(plan.Steps))
	}
	if spec, ok := plan.Steps[0].Spec.(*schema.IncludeSpec); !ok || spec.LazyRunbookPath == "" {
		t.Errorf("expected lazy include, got %#v", plan.Steps[0].Spec)
	}
}

// TestLazy_SiteFieldOverridesRunbookLazy: the site-level eager flag
// forces the include to load even when the runbook default is lazy.
func TestLazy_SiteFieldOverridesRunbookLazy(t *testing.T) {
	child := minimalCLIRunbook("/abs/child.runbook.yaml", "child", "child-cli")
	loader := &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{
		"/abs/child.runbook.yaml": child,
	}}
	rb := parentRunbookWithIncludes("./child.runbook.yaml")
	rb.Source = "/abs/parent.runbook.yaml"
	rb.Runbook.Expand = "lazy"
	rb.Runbook.Flow[0].Step.IncludeSpec.Include.Expand = "eager"
	p := planner.New(plannerPkg.Config{
		Loader: loader,
		Tools:  &fakeRegistry{tools: map[string]*schema.ToolDef{}},
	})
	plan, err := p.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// Eager: the include emits its parent step + the child's CLI step.
	if len(plan.Steps) < 2 {
		t.Fatalf("expected eager expansion to emit child steps, got %d", len(plan.Steps))
	}
	spec, ok := plan.Steps[0].Spec.(*schema.IncludeSpec)
	if !ok {
		t.Fatalf("step 0 not include")
	}
	if spec.LazyRunbookPath != "" {
		t.Errorf("eager include must not have LazyRunbookPath; got %q", spec.LazyRunbookPath)
	}
	if len(spec.ResolvedSteps) == 0 {
		t.Error("eager include must have ResolvedSteps")
	}
}

// TestLazy_AutoMaterializeAboveThreshold: ModeAuto + many includes ->
// lazy.
func TestLazy_AutoMaterializeAboveThreshold(t *testing.T) {
	includes := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		includes = append(includes, "./n.runbook.yaml")
	}
	rb := parentRunbookWithIncludes(includes...)
	rb.Runbook.Expand = "auto"
	p := planner.New(plannerPkg.Config{
		Loader:       &failingLoader{t: t},
		Tools:        &fakeRegistry{tools: map[string]*schema.ToolDef{}},
		ExpandPolicy: expand.Policy{AutoIncludeThreshold: 3},
	})
	plan, err := p.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for i, s := range plan.Steps {
		if spec, ok := s.Spec.(*schema.IncludeSpec); ok && spec.LazyRunbookPath == "" {
			t.Errorf("step %d expected lazy under auto+10>3, got eager", i)
		}
	}
}
