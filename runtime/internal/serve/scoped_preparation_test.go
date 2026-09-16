package serve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

func TestPrepareRunUsesCapturedRootAndPreservesErrors(t *testing.T) {
	plan := &engine.ExecutionPlan{}
	root := &parser.ParsedRunbook{Runbook: &schema.Runbook{
		ID: "captured", Vars: map[string]any{"source": "captured"},
	}}
	for _, name := range []string{"captured", "nil-plan", "nil-root", "nil-runbook", "failure"} {
		t.Run(name, func(t *testing.T) {
			expectedErr := errors.New("dependency conflict")
			server := &Server{cfg: servepkg.ServerConfig{
				PrepareRun: func(_ context.Context, path string) (*engine.ExecutionPlan, *parser.ParsedRunbook, error) {
					if path != "root.yaml" {
						t.Fatalf("preparation path = %q", path)
					}
					switch name {
					case "nil-plan":
						return nil, root, nil
					case "nil-root":
						return plan, nil, nil
					case "nil-runbook":
						return plan, &parser.ParsedRunbook{}, nil
					case "failure":
						return nil, nil, expectedErr
					}
					return plan, root, nil
				},
			}}
			gotPlan, gotRoot, vars, _, err := server.loadPlanAndSeed(context.Background(), "root.yaml", nil)
			switch name {
			case "captured":
				if err != nil || gotPlan != plan || gotRoot != root || vars["source"] != "captured" {
					t.Fatalf("captured preparation not used: plan=%p root=%p vars=%v err=%v", gotPlan, gotRoot, vars, err)
				}
			case "failure":
				if !errors.Is(err, expectedErr) {
					t.Fatalf("lost preparation failure: %v", err)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), "captured runbook") {
					t.Fatalf("invalid preparation accepted: %v", err)
				}
			}
		})
	}
}
