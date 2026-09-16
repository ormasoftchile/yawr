package runstore

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestDynamicResolutionVersionCannotAcquireScopedMeaning(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	id := engine.InteractionPayloadDigest([]byte("resolution"))
	for _, version := range []string{"legacy", "legacy-with-scope", "v2-with-legacy-pin", "v2-without-plan"} {
		t.Run(version, func(t *testing.T) {
			resolution := &engine.DynamicIncludeResolutionState{
				SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV1, ResolutionID: id,
				WriterEpoch: 1, QualifiedNodeID: "include", StepID: "include", Invocation: 1, Revision: 1,
				Status: engine.DynamicIncludeResolutionStatusActive, CommittedAt: "2026-09-15T00:00:00Z",
				Pin: schema.LockedDynamicInclude{
					StepID: "include", QualifiedNodeID: "include", Invocation: 1, Revision: 1,
					RenderedRef: "example/child", QualifiedID: "example/child", AbsPath: "child.yaml",
					PackageName: "example", PackageVersion: "1.0.0", FileDigest: id, PackageDigest: id,
					ExecutableClosure: closure,
				},
			}
			want := ""
			switch version {
			case "legacy-with-scope":
				resolution.Pin.TargetScopeID = "injected-scope"
				want = "requires dynamic resolution v2"
			case "v2-with-legacy-pin":
				resolution.SchemaVersion = engine.DynamicIncludeResolutionStateSchemaV2
				want = "requires scoped pin"
			case "v2-without-plan":
				resolution.SchemaVersion = engine.DynamicIncludeResolutionStateSchemaV2
				resolution.Pin.SchemaVersion = "yawr.dynamic-include-pin/v2"
				resolution.Pin.TargetScopeID = "injected-scope"
				want = "requires its immutable plan"
			}
			err := validateDynamicIncludeResolutions(1, map[string]*engine.DynamicIncludeResolutionState{id: resolution})
			if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
				t.Fatalf("validation = %v, want %q", err, want)
			}
		})
	}
}
