package planner_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerPkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// fakeLoader is a test double for RunbookLoader.
type fakeLoader struct {
	runbooks map[string]*parser.ParsedRunbook
}

func (f *fakeLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	if rb, ok := f.runbooks[path]; ok {
		return rb, nil
	}
	path = normalizeTestPath(path)
	for key, rb := range f.runbooks {
		if normalizeTestPath(key) == path {
			return rb, nil
		}
	}
	return nil, &plannerPkg.PlanError{
		Code: plannerPkg.ErrRunbookNotFound,
		Path: path,
	}
}

func normalizeTestPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// fakeRegistry is a test double for ToolRegistry.
type fakeRegistry struct {
	tools map[string]*schema.ToolDef
}

func (f *fakeRegistry) Lookup(ctx context.Context, name, action string) (*schema.ToolDef, error) {
	key := name + "/" + action
	if tool, ok := f.tools[key]; ok {
		return tool, nil
	}
	// Check if tool exists without action
	for k, tool := range f.tools {
		if len(k) > len(name) && k[:len(name)] == name && k[len(name)] == '/' {
			return nil, plannerPkg.ErrActionNotFound
		}
		_ = tool
	}
	return nil, plannerPkg.ErrToolNotFound
}

// minimalCLIRunbook builds a ParsedRunbook with a single CLI step.
func minimalCLIRunbook(source, id string, stepID string) *parser.ParsedRunbook {
	return &parser.ParsedRunbook{
		Source: source,
		Runbook: &schema.Runbook{
			ID:   id,
			Name: id,
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   stepID,
					Type: schema.StepTypeCLI,
					CLI:  &schema.CLISpec{Command: "echo", Args: []string{"hello"}},
				}},
			},
		},
	}
}

// newPlanner creates a planner with the given loader and registry.
//
// Defaults to eager expansion: most tests in this file exercise plan-time
// behavior that only happens when includes are loaded eagerly (cycle
// detection, max-depth enforcement, transitive resolution). The
// production default is lazy; tests that want lazy override Default via
// opts or use a separate Config literal.
func newPlanner(loader plannerPkg.RunbookLoader, registry plannerPkg.ToolRegistry, opts ...func(*plannerPkg.Config)) plannerPkg.Planner {
	cfg := plannerPkg.Config{
		Loader:       loader,
		Tools:        registry,
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	}
	for _, o := range opts {
		o(&cfg)
	}
	return planner.New(cfg)
}

func withBaseDir(dir string) func(*plannerPkg.Config) {
	return func(c *plannerPkg.Config) { c.BaseDir = dir }
}

func withMaxDepth(n int) func(*plannerPkg.Config) {
	return func(c *plannerPkg.Config) { c.MaxIncludeDepth = n }
}

// --- Canonical test names requested by task spec ---

func TestPlanner_MinimalRunbook(t *testing.T) {
	ctx := context.Background()
	loader := &fakeLoader{runbooks: make(map[string]*parser.ParsedRunbook)}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}

	p := newPlanner(loader, registry)
	rb := minimalCLIRunbook("/test/runbook.yaml", "test-runbook", "step-1")

	plan, err := p.Plan(ctx, rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if plan.RunbookPath != rb.Source {
		t.Errorf("RunbookPath = %q; want %q", plan.RunbookPath, rb.Source)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("len(Steps) = %d; want 1", len(plan.Steps))
	}
	if plan.Steps[0].ID != "step-1" {
		t.Errorf("Steps[0].ID = %q; want step-1", plan.Steps[0].ID)
	}
	if plan.Metadata.RunbookID != "test-runbook" {
		t.Errorf("RunbookID = %q; want test-runbook", plan.Metadata.RunbookID)
	}
}

func TestPlanner_ImportResolution(t *testing.T) {
	ctx := context.Background()

	childRb := minimalCLIRunbook("/test/child.yaml", "child-runbook", "child-step-1")

	parentRb := &parser.ParsedRunbook{
		Source: "/test/parent.yaml",
		Runbook: &schema.Runbook{
			ID:   "parent-runbook",
			Name: "Parent Runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-child",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "child.yaml"},
					},
				}},
			},
		},
	}

	loader := &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{"/test/child.yaml": childRb}}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}
	p := newPlanner(loader, registry, withBaseDir("/test"))

	plan, err := p.Plan(ctx, parentRb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("len(Steps) = %d; want 2 (include-child parent + child-step-1)", len(plan.Steps))
	}
	if plan.Steps[0].ID != "include-child" {
		t.Errorf("Steps[0].ID = %q; want include-child", plan.Steps[0].ID)
	}
	if plan.Steps[1].ID != "child-step-1" {
		t.Errorf("Steps[1].ID = %q; want child-step-1", plan.Steps[1].ID)
	}
	wantOrigin := normalizeTestPath("/test/child.yaml")
	if plan.Steps[1].Origin != wantOrigin {
		t.Errorf("Steps[1].Origin = %q; want %q", plan.Steps[1].Origin, wantOrigin)
	}
}

func TestPlanner_ImportCycleDetected(t *testing.T) {
	ctx := context.Background()

	rbA := &parser.ParsedRunbook{
		Source: "/test/a.yaml",
		Runbook: &schema.Runbook{
			ID: "runbook-a",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-b",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "b.yaml"},
					},
				}},
			},
		},
	}
	rbB := &parser.ParsedRunbook{
		Source: "/test/b.yaml",
		Runbook: &schema.Runbook{
			ID: "runbook-b",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-a",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "a.yaml"},
					},
				}},
			},
		},
	}

	loader := &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{
		"/test/a.yaml": rbA,
		"/test/b.yaml": rbB,
	}}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}
	p := newPlanner(loader, registry, withBaseDir("/test"))

	_, err := p.Plan(ctx, rbA)
	if err == nil {
		t.Fatal("Plan should fail with import cycle")
	}
	if !errors.Is(err, plannerPkg.ErrImportCycle) {
		t.Errorf("error = %v; want ErrImportCycle", err)
	}
}

func TestPlanner_MaxDepthExceeded(t *testing.T) {
	ctx := context.Background()

	// Build a linear include chain: r1 → r2 → … → r12 (11 includes, max=10).
	runbooks := make(map[string]*parser.ParsedRunbook)
	for i := 1; i <= 12; i++ {
		path := fmt.Sprintf("/test/r%d.yaml", i)
		rb := &parser.ParsedRunbook{
			Source:  path,
			Runbook: &schema.Runbook{ID: fmt.Sprintf("runbook-%d", i)},
		}
		if i < 12 {
			rb.Runbook.Flow = []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-next",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: fmt.Sprintf("r%d.yaml", i+1)},
					},
				}},
			}
		}
		runbooks[path] = rb
	}

	loader := &fakeLoader{runbooks: runbooks}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}
	p := newPlanner(loader, registry, withBaseDir("/test"), withMaxDepth(10))

	_, err := p.Plan(ctx, runbooks["/test/r1.yaml"])
	if err == nil {
		t.Fatal("Plan should fail with max depth exceeded")
	}
	if !errors.Is(err, plannerPkg.ErrMaxDepthExceeded) {
		t.Errorf("error = %v; want ErrMaxDepthExceeded", err)
	}
}

func TestPlanner_ToolDiscovery(t *testing.T) {
	ctx := context.Background()

	rb := &parser.ParsedRunbook{
		Source: "/test/runbook.yaml",
		Runbook: &schema.Runbook{
			ID: "tool-runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "call-kubectl",
					Type: schema.StepTypeTool,
					ToolCall: &schema.ToolCallSpec{
						Tool: schema.ToolInvocation{
							Name:   "kubectl",
							Action: "get-pods",
							Args:   map[string]any{"namespace": "default"},
						},
					},
				}},
			},
		},
	}

	kubectlDef := &schema.ToolDef{
		Name:    "kubectl",
		Version: "1.0.0",
		Actions: map[string]*schema.ToolAction{
			"get-pods": {
				Description: "List pods",
				Args:        map[string]*schema.ArgDef{"namespace": {Type: "string", Required: true}},
			},
		},
	}

	loader := &fakeLoader{runbooks: make(map[string]*parser.ParsedRunbook)}
	registry := &fakeRegistry{tools: map[string]*schema.ToolDef{"kubectl/get-pods": kubectlDef}}
	p := newPlanner(loader, registry)

	plan, err := p.Plan(ctx, rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("len(Steps) = %d; want 1", len(plan.Steps))
	}
	if plan.Steps[0].Kind != "tool" {
		t.Errorf("Kind = %q; want tool", plan.Steps[0].Kind)
	}
	if _, ok := plan.Tools["kubectl"]; !ok {
		t.Error("Tools map missing 'kubectl'")
	}
}

func TestPlanner_UnknownTool(t *testing.T) {
	ctx := context.Background()

	rb := &parser.ParsedRunbook{
		Source: "/test/runbook.yaml",
		Runbook: &schema.Runbook{
			ID: "tool-runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "call-missing",
					Type: schema.StepTypeTool,
					ToolCall: &schema.ToolCallSpec{
						Tool: schema.ToolInvocation{Name: "nonexistent-tool", Action: "action"},
					},
				}},
			},
		},
	}

	loader := &fakeLoader{runbooks: make(map[string]*parser.ParsedRunbook)}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}
	p := newPlanner(loader, registry)

	_, err := p.Plan(ctx, rb)
	if err == nil {
		t.Fatal("Plan should fail with tool not found")
	}
	if !errors.Is(err, plannerPkg.ErrToolNotFound) {
		t.Errorf("error = %v; want ErrToolNotFound", err)
	}
}

func TestPlanner_TopoSort(t *testing.T) {
	ctx := context.Background()

	// Three-step linear runbook: step-a → step-b → step-c
	rb := &parser.ParsedRunbook{
		Source: "/test/runbook.yaml",
		Runbook: &schema.Runbook{
			ID: "linear-runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{ID: "step-a", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}},
				{Step: &schema.Step{ID: "step-b", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}},
				{Step: &schema.Step{ID: "step-c", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}},
			},
		},
	}

	loader := &fakeLoader{runbooks: make(map[string]*parser.ParsedRunbook)}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}
	p := newPlanner(loader, registry)

	plan, err := p.Plan(ctx, rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Steps) != 3 {
		t.Fatalf("len(Steps) = %d; want 3", len(plan.Steps))
	}
	wantOrder := []string{"step-a", "step-b", "step-c"}
	for i, want := range wantOrder {
		if plan.Steps[i].ID != want {
			t.Errorf("Steps[%d].ID = %q; want %q", i, plan.Steps[i].ID, want)
		}
	}
}

// --- Ken's original skeleton tests (t.Skip removed) ---

func TestPlanner_DiamondDependency(t *testing.T) {
	ctx := context.Background()

	// Diamond: A includes B and C; both B and C include D.
	// Should resolve without cycle error.
	rbD := minimalCLIRunbook("/test/d.yaml", "runbook-d", "d-step")

	rbB := &parser.ParsedRunbook{
		Source: "/test/b.yaml",
		Runbook: &schema.Runbook{
			ID: "runbook-b",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-d",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "d.yaml"},
					},
				}},
			},
		},
	}

	rbC := &parser.ParsedRunbook{
		Source: "/test/c.yaml",
		Runbook: &schema.Runbook{
			ID: "runbook-c",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-d",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "d.yaml"},
					},
				}},
			},
		},
	}

	rbA := &parser.ParsedRunbook{
		Source: "/test/a.yaml",
		Runbook: &schema.Runbook{
			ID: "runbook-a",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-b",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "b.yaml"},
					},
				}},
				{Step: &schema.Step{
					ID:   "include-c",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "c.yaml"},
					},
				}},
			},
		},
	}

	loader := &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{
		"/test/b.yaml": rbB,
		"/test/c.yaml": rbC,
		"/test/d.yaml": rbD,
	}}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}
	p := newPlanner(loader, registry, withBaseDir("/test"))

	plan, err := p.Plan(ctx, rbA)
	if err != nil {
		t.Fatalf("Plan failed on diamond dependency: %v", err)
	}
	// B→D gives [d-step, include-d, include-b]; C→D gives [d-step, include-d, include-c].
	// Total: 6 steps (d-step inlined twice, each include step emitted post-order).
	if len(plan.Steps) != 6 {
		t.Errorf("len(Steps) = %d; want 6 (include steps emitted post-order)", len(plan.Steps))
	}
}

func TestPlan_BasicRunbook(t *testing.T) {
	ctx := context.Background()
	loader := &fakeLoader{runbooks: make(map[string]*parser.ParsedRunbook)}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}

	p := planner.New(plannerPkg.Config{
		Loader: loader,
		Tools:  registry,
	})

	rb := &parser.ParsedRunbook{
		Source: "/test/runbook.yaml",
		Runbook: &schema.Runbook{
			ID:   "test-runbook",
			Name: "Test Runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "step-1",
					Type: schema.StepTypeCLI,
					CLI:  &schema.CLISpec{Command: "echo", Args: []string{"hello"}},
				}},
			},
		},
	}

	plan, err := p.Plan(ctx, rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if plan.RunbookPath != rb.Source {
		t.Errorf("RunbookPath = %q; want %q", plan.RunbookPath, rb.Source)
	}
	if len(plan.Steps) != 1 {
		t.Errorf("len(Steps) = %d; want 1", len(plan.Steps))
	}
	if plan.Metadata.RunbookID != "test-runbook" {
		t.Errorf("RunbookID = %q; want 'test-runbook'", plan.Metadata.RunbookID)
	}
}

func TestPlan_IncludeResolution(t *testing.T) {
	ctx := context.Background()

	childRb := &parser.ParsedRunbook{
		Source: "/test/child.yaml",
		Runbook: &schema.Runbook{
			ID:   "child-runbook",
			Name: "Child Runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "child-step-1",
					Type: schema.StepTypeCLI,
					CLI:  &schema.CLISpec{Command: "echo", Args: []string{"child"}},
				}},
			},
		},
	}

	parentRb := &parser.ParsedRunbook{
		Source: "/test/parent.yaml",
		Runbook: &schema.Runbook{
			ID:   "parent-runbook",
			Name: "Parent Runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-child",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "child.yaml"},
					},
				}},
			},
		},
	}

	loader := &fakeLoader{
		runbooks: map[string]*parser.ParsedRunbook{"/test/child.yaml": childRb},
	}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}

	p := planner.New(plannerPkg.Config{
		Loader:       loader,
		Tools:        registry,
		BaseDir:      "/test",
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})

	plan, err := p.Plan(ctx, parentRb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Errorf("len(Steps) = %d; want 2 (include-child + child-step-1)", len(plan.Steps))
	}
	if plan.Steps[0].ID != "include-child" {
		t.Errorf("Steps[0].ID = %q; want \"include-child\"", plan.Steps[0].ID)
	}
	if plan.Steps[1].ID != "child-step-1" {
		t.Errorf("Steps[1].ID = %q; want \"child-step-1\"", plan.Steps[1].ID)
	}
}

func TestPlan_ToolResolution(t *testing.T) {
	ctx := context.Background()

	rb := &parser.ParsedRunbook{
		Source: "/test/runbook.yaml",
		Runbook: &schema.Runbook{
			ID:   "tool-runbook",
			Name: "Tool Runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "call-kubectl",
					Type: schema.StepTypeTool,
					ToolCall: &schema.ToolCallSpec{
						Tool: schema.ToolInvocation{
							Name:   "kubectl",
							Action: "get-pods",
							Args:   map[string]any{"namespace": "default"},
						},
					},
				}},
			},
		},
	}

	kubectlTool := &schema.ToolDef{
		Name:    "kubectl",
		Version: "1.0.0",
		Actions: map[string]*schema.ToolAction{
			"get-pods": {
				Description: "List pods in a namespace",
				Args: map[string]*schema.ArgDef{
					"namespace": {Type: "string", Required: true},
				},
			},
		},
	}

	loader := &fakeLoader{runbooks: make(map[string]*parser.ParsedRunbook)}
	registry := &fakeRegistry{
		tools: map[string]*schema.ToolDef{"kubectl/get-pods": kubectlTool},
	}

	p := planner.New(plannerPkg.Config{Loader: loader, Tools: registry})

	plan, err := p.Plan(ctx, rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if _, ok := plan.Tools["kubectl"]; !ok {
		t.Error("Tools map missing 'kubectl'")
	}
}

func TestPlan_ImportCycleDetection(t *testing.T) {
	ctx := context.Background()

	rbA := &parser.ParsedRunbook{
		Source: "/test/a.yaml",
		Runbook: &schema.Runbook{
			ID: "runbook-a",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-b",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "b.yaml"},
					},
				}},
			},
		},
	}

	rbB := &parser.ParsedRunbook{
		Source: "/test/b.yaml",
		Runbook: &schema.Runbook{
			ID: "runbook-b",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "include-a",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: "a.yaml"},
					},
				}},
			},
		},
	}

	loader := &fakeLoader{
		runbooks: map[string]*parser.ParsedRunbook{
			"/test/a.yaml": rbA,
			"/test/b.yaml": rbB,
		},
	}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}

	p := planner.New(plannerPkg.Config{
		Loader:       loader,
		Tools:        registry,
		BaseDir:      "/test",
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})

	_, err := p.Plan(ctx, rbA)
	if err == nil {
		t.Fatal("Plan should fail with import cycle")
	}
	if !errors.Is(err, plannerPkg.ErrImportCycle) {
		t.Errorf("error = %v; want ErrImportCycle", err)
	}
}

func TestPlan_ToolNotFound(t *testing.T) {
	ctx := context.Background()

	rb := &parser.ParsedRunbook{
		Source: "/test/runbook.yaml",
		Runbook: &schema.Runbook{
			ID: "tool-runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "call-missing",
					Type: schema.StepTypeTool,
					ToolCall: &schema.ToolCallSpec{
						Tool: schema.ToolInvocation{
							Name:   "nonexistent-tool",
							Action: "action",
						},
					},
				}},
			},
		},
	}

	loader := &fakeLoader{runbooks: make(map[string]*parser.ParsedRunbook)}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}

	p := planner.New(plannerPkg.Config{Loader: loader, Tools: registry})

	_, err := p.Plan(ctx, rb)
	if err == nil {
		t.Fatal("Plan should fail with tool not found")
	}
	if !errors.Is(err, plannerPkg.ErrToolNotFound) {
		t.Errorf("error = %v; want ErrToolNotFound", err)
	}
}

func TestPlan_MaxDepthExceeded(t *testing.T) {
	ctx := context.Background()

	// Create a deep include chain: r1 → r2 → r3 → ... → r12 (11 includes, max=10)
	runbooks := make(map[string]*parser.ParsedRunbook)
	for i := 1; i <= 12; i++ {
		path := fmt.Sprintf("/test/r%d.yaml", i)
		rb := &parser.ParsedRunbook{
			Source:  path,
			Runbook: &schema.Runbook{ID: fmt.Sprintf("runbook-%d", i), Flow: []schema.FlowNode{}},
		}
		if i < 12 {
			rb.Runbook.Flow = append(rb.Runbook.Flow, schema.FlowNode{
				Step: &schema.Step{
					ID:   "include-next",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{Runbook: fmt.Sprintf("r%d.yaml", i+1)},
					},
				},
			})
		}
		runbooks[path] = rb
	}

	loader := &fakeLoader{runbooks: runbooks}
	registry := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}

	p := planner.New(plannerPkg.Config{
		Loader:          loader,
		Tools:           registry,
		BaseDir:         "/test",
		MaxIncludeDepth: 10,
		ExpandPolicy:    expand.Policy{Default: expand.ModeEager},
	})

	_, err := p.Plan(ctx, runbooks["/test/r1.yaml"])
	if err == nil {
		t.Fatal("Plan should fail with max depth exceeded")
	}
	if !errors.Is(err, plannerPkg.ErrMaxDepthExceeded) {
		t.Errorf("error = %v; want ErrMaxDepthExceeded", err)
	}
}
