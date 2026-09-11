package capture

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func TestServiceCaptureSources(t *testing.T) {
	prior := map[string]*engine.StepResult{
		"scan": {StepID: "scan", Output: map[string]any{"stdout": `{"result":"clean"}`, "exit_code": 0}},
	}
	result := &engine.StepResult{StepID: "cur", Output: map[string]any{
		"stdout":    `{"foo":{"bar":42}}`,
		"stderr":    `{"warning":"disk"}`,
		"exit_code": 7,
		"http": map[string]any{
			"status":  202,
			"body":    `{"data":{"id":"req-7"}}`,
			"headers": map[string]any{"Content-Type": "application/json"},
		},
	}}
	step := engine.ResolvedStep{ID: "cur", Capture: map[string]string{
		"code":   "exit_code",
		"json":   "json.foo.bar",
		"stderr": "stderr.warning",
		"http":   "http.body.data.id",
		"header": "http.headers.content-type",
		"prior":  "step.scan.json.result",
	}}

	got, err := New(nil, prior, nil).CaptureStep(nil, step, result)
	if err != nil {
		t.Fatalf("CaptureStep: %v", err)
	}
	assertPJVM(t, got["code"], "7")
	assertPJVM(t, got["json"], "42")
	assertPJVM(t, got["stderr"], "disk")
	assertPJVM(t, got["http"], "req-7")
	assertPJVM(t, got["header"], "application/json")
	assertPJVM(t, got["prior"], "clean")
}

func TestServiceCaptureDefaults(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		def     any
		stdout  string
		want    string
		wantErr string
	}{
		{name: "missing scalar uses default", path: "json.missing", def: "fallback", stdout: `{"present":true}`, want: "fallback"},
		{name: "subtree default rejected", path: "json.config", def: map[string]any{}, stdout: `{"config":{"region":"us"}}`, wantErr: "GCP-DEFAULT-SUBTREE"},
		{name: "exit code type mismatch", path: "exit_code", def: "not_run", stdout: `{}`, wantErr: "GCP-TYPE-001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := engine.ResolvedStep{ID: "s", Capture: map[string]string{"out": tt.path}, CaptureDefaults: map[string]any{"out": tt.def}}
			result := &engine.StepResult{StepID: "s", Output: map[string]any{"stdout": tt.stdout, "exit_code": 3}}
			got, err := New(nil, nil, nil).CaptureStep(nil, step, result)
			if tt.wantErr != "" {
				if codeOf(err) != tt.wantErr {
					t.Fatalf("code=%q err=%v, want %s", codeOf(err), err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CaptureStep: %v", err)
			}
			assertPJVM(t, got["out"], tt.want)
		})
	}
}

func TestServiceCaptureNestedOutputField(t *testing.T) {
	step := engine.ResolvedStep{
		ID:      "host_action",
		Capture: map[string]string{"launch_status": "outputs.result.status"},
	}
	result := &engine.StepResult{
		StepID: "host_action",
		Output: map[string]any{"result": map[string]any{"status": "opened"}},
	}

	got, err := New(nil, nil, nil).CaptureStep(nil, step, result)
	if err != nil {
		t.Fatalf("CaptureStep: %v", err)
	}
	assertPJVM(t, got["launch_status"], "opened")
}

func assertPJVM(t *testing.T, got pjvm.Value, want string) {
	t.Helper()
	if got.String() != want {
		t.Fatalf("got %s, want %s", got.String(), want)
	}
}

func codeOf(err error) string {
	for err != nil {
		if c, ok := err.(interface{ Code() string }); ok {
			return c.Code()
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			err = u.Unwrap()
			continue
		}
		return ""
	}
	return ""
}
