package engine

import (
	"context"
	"reflect"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestPlanProfileContextPreservesOwnedFrozenPolicy(t *testing.T) {
	profile := &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion, ID: "frozen-profile",
		Context: schema.ProfileContextCI, Attendance: schema.ProfileAttendanceUnattended,
		Tools: map[string]*schema.ProfileToolOverride{"query": {Endpoint: "https://original.invalid", Provider: "original"}, "unset": nil},
	}
	ctx := WithPlanProfile(context.Background(), profile)
	expected := PlanProfileFromContext(ctx)
	profile.ID = "mutated"
	profile.Tools["query"].Endpoint = "https://changed.invalid"
	delete(profile.Tools, "unset")
	if got := PlanProfileFromContext(ctx); !reflect.DeepEqual(got, expected) {
		t.Fatal("caller mutation changed context profile")
	}
	child := PlanProfileFromContext(ctx)
	child.Tools["query"].Provider = "changed"
	child.Tools["other"] = &schema.ProfileToolOverride{}
	if got := PlanProfileFromContext(ctx); !reflect.DeepEqual(got, expected) {
		t.Fatal("subplan mutation changed frozen context profile")
	}
	nested := WithPlanProfile(ctx, nil)
	if PlanProfileFromContext(nested) != nil {
		t.Fatal("explicit nil profile inherited ancestor profile")
	}
	if PlanProfileFromContext(context.Background()) != nil {
		t.Fatal("absent profile must remain nil")
	}
}
