package testutil

import (
	"context"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// FakeInputProvider is a controllable InputProvider for use in tests.
// Set Responses to return specific values keyed by StepID+":"+VarName,
// or set Default to return the same value for every request.
// A nil response means "cannot satisfy" (chain fallthrough).
type FakeInputProvider struct {
	// Responses maps "stepID:varName" → response. Checked before Default.
	Responses map[string]*input.InputResponse
	// Default is returned when Responses has no match. nil = fallthrough.
	Default *input.InputResponse

	Calls []InputCall
	mu    sync.Mutex
}

// InputCall records a single Provide invocation.
type InputCall struct {
	StepID  string
	VarName string
	At      time.Time
}

// Ensure FakeInputProvider implements input.InputProvider.
var _ input.InputProvider = (*FakeInputProvider)(nil)

func (f *FakeInputProvider) Name() string { return "fake" }

// Provide resolves the request. Returns (nil, nil) if no response is configured.
func (f *FakeInputProvider) Provide(ctx context.Context, req input.InputRequest) (*input.InputResponse, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, InputCall{StepID: req.StepID, VarName: req.VarName, At: time.Now()})
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := req.StepID + ":" + req.VarName
	if f.Responses != nil {
		if resp, ok := f.Responses[key]; ok {
			return resp, nil
		}
	}
	return f.Default, nil
}
