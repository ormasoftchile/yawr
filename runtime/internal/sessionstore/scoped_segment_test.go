package sessionstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestScopedSegmentArtifactPreservesVersionAndFrozenIdentity(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(cwd)), "examples", "dependency-scopes")
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prepared, err := adapter.PrepareScopedRun(ctx, adapter.ScopedRunOptions{
		Catalog: pkgcatalog.BuildOptions{WorkspaceRoot: root}, Parser: parser,
		Entrypoint: filepath.Join(root, "dynamic.runbook.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := prepared.Plan(ctx, expand.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	target, err := prepared.Resolver.Resolve(engine.WithToolScopes(ctx, plan.ToolScopes), "scope-children/left")
	if err != nil {
		t.Fatal(err)
	}
	invocation := schema.InvocationForRunbook(target.ChildBindings, target.ChildOutputs, target.Flow)
	closure, err := plansnapshot.EncodeScopedInvocationFlowClosure(target.Flow, invocation, plan.ToolScopes, target.TargetScopeID)
	if err != nil {
		t.Fatal(err)
	}
	resolution := engine.DynamicIncludeResolutionState{
		SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV2,
		Pin: schema.LockedDynamicInclude{
			SchemaVersion: "yawr.dynamic-include-pin/v2", TargetScopeID: target.TargetScopeID,
			StepID: "enter_child", RenderedRef: "scope-children/left", QualifiedID: target.QualifiedID,
			RunbookID: target.RunbookID, RunbookName: target.RunbookName, RunbookContentHash: target.ContentHash,
			AbsPath: target.AbsPath, PackageName: target.PackageName, PackageVersion: target.PackageVersion,
			FileDigest: target.FileDigest, PackageDigest: target.PackageDigest, ExecutableClosure: closure,
			ResolvedInputs: target.ChildInputs, ResolvedBindings: target.ChildBindings,
			ResolvedOutputs: target.ChildOutputs, ResolvedGovernance: target.ChildGovernance,
		},
	}
	if err := validateScopedSegmentArtifact(resolution, data); err != nil {
		t.Fatalf("valid frozen scoped segment rejected: %v", err)
	}
	for _, name := range []string{"legacy-envelope", "legacy-snapshot", "unknown-scope", "changed-package", "changed-closure", "changed-snapshot"} {
		t.Run(name, func(t *testing.T) {
			candidate := resolution
			artifact := append(json.RawMessage(nil), data...)
			switch name {
			case "legacy-envelope":
				candidate.SchemaVersion = engine.DynamicIncludeResolutionStateSchemaV1
			case "legacy-snapshot":
				artifact = json.RawMessage(`{"schema_version":"execution-plan/v3"}`)
			case "unknown-scope":
				candidate.Pin.TargetScopeID = "different-authoring-scope"
			case "changed-package":
				candidate.Pin.PackageVersion = "9.9.9"
			case "changed-closure":
				candidate.Pin.ExecutableClosure = json.RawMessage(`{}`)
			case "changed-snapshot":
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(artifact, &fields); err != nil {
					t.Fatal(err)
				}
				fields["plan_hash"] = json.RawMessage(`"forged"`)
				var err error
				artifact, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := validateScopedSegmentArtifact(candidate, artifact); err == nil {
				t.Fatal("accepted incompatible or tampered scoped segment")
			}
		})
	}
}
