package capture

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestServiceEventSource(t *testing.T) {
	step := engine.ResolvedStep{ID: "s", Capture: map[string]string{
		"id":     "event.id",
		"kind":   "event.body.type",
		"header": "event.headers.x-github-event",
		"absent": "event.headers.x-missing",
	}}
	result := &engine.StepResult{StepID: "s", Output: map[string]any{}}
	svc := New(nil, nil, &Event{ID: "evt-1", Body: `{"type":"push"}`, Headers: map[string]string{"X-GitHub-Event": "push"}})

	got, err := svc.CaptureStep(nil, step, result)
	if err != nil {
		t.Fatalf("CaptureStep: %v", err)
	}
	assertPJVM(t, got["id"], "evt-1")
	assertPJVM(t, got["kind"], "push")
	assertPJVM(t, got["header"], "push")
	assertPJVM(t, got["absent"], "null")
}

func TestServiceSourceUnavailable(t *testing.T) {
	step := engine.ResolvedStep{ID: "s", Capture: map[string]string{"prior": "step.scan.stdout"}}
	_, err := New(nil, nil, nil).CaptureStep(nil, step, &engine.StepResult{StepID: "s", Output: map[string]any{}})
	if codeOf(err) != "GCP-RESOLVE-001" {
		t.Fatalf("code=%q err=%v, want GCP-RESOLVE-001", codeOf(err), err)
	}
}
