package serve

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

type testServerHarness struct {
	server  *Server
	parser  *fakeParser
	planner *fakePlanner
	engine  *fakeEngine
	handle  *fakeRunHandle
	store   *runstore.DirRunStore
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerHarness(t).server
}

func newHTTPTestServer(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	return httptest.NewServer(srv.handler)
}

func newTestServerHarness(t *testing.T) *testServerHarness {
	t.Helper()
	parser := &fakeParser{
		result: &parser.ParsedRunbook{Source: "test"},
	}
	plan := &engine.ExecutionPlan{
		RunID:       "run-1",
		RunbookPath: "runbook.yaml",
		Metadata: engine.PlanMetadata{
			RunbookID:   "rb-1",
			RunbookName: "Runbook",
			PlannedAt:   time.Now(),
		},
		Tools: make(map[string]*schema.ToolDef),
	}
	planner := &fakePlanner{plan: plan}
	handle := newFakeRunHandle("run-1")
	fakeEng := &fakeEngine{
		startFunc: func(_ context.Context, _ *engine.ExecutionPlan, _ engine.RunOptions) (engine.RunHandle, error) {
			return handle, nil
		},
	}
	store := runstore.NewDirRunStore(makeWorkDir(t))
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close test run store: %v", err)
		}
	})

	cfg := servepkg.ServerConfig{
		Engine:          fakeEng,
		EngineConfig:    engine.EngineConfig{Store: store},
		Parser:          parser,
		Planner:         planner,
		EventBufferSize: 4,
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return &testServerHarness{
		server:  srv,
		parser:  parser,
		planner: planner,
		engine:  fakeEng,
		handle:  handle,
		store:   store,
	}
}

type fakeParser struct {
	result *parser.ParsedRunbook
	err    error
}

func (f *fakeParser) Parse(_ context.Context, _ string) (*parser.ParsedRunbook, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeParser) ParseBytes(_ context.Context, _ []byte) (*parser.ParsedRunbook, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakePlanner struct {
	plan *engine.ExecutionPlan
	err  error
}

func (f *fakePlanner) Plan(_ context.Context, _ *parser.ParsedRunbook) (*engine.ExecutionPlan, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.plan, nil
}

type fakeEngine struct {
	startFunc  func(context.Context, *engine.ExecutionPlan, engine.RunOptions) (engine.RunHandle, error)
	resumeFunc func(context.Context, string, engine.RunOptions) (engine.RunHandle, error)
}

func (f *fakeEngine) Start(ctx context.Context, plan *engine.ExecutionPlan, opts engine.RunOptions) (engine.RunHandle, error) {
	if f.startFunc != nil {
		return f.startFunc(ctx, plan, opts)
	}
	return nil, engine.ErrNotImplemented
}

func (f *fakeEngine) Resume(ctx context.Context, runID string, opts engine.RunOptions) (engine.RunHandle, error) {
	if f.resumeFunc != nil {
		return f.resumeFunc(ctx, runID, opts)
	}
	return nil, engine.ErrNotImplemented
}

type fakeRunHandle struct {
	mu          sync.Mutex
	id          string
	results     []*engine.StepResult
	nextErr     error
	eofStatus   engine.RunStatus
	state       engine.RunState
	events      chan engine.Event
	closed      bool
	cancelError error
}

func newFakeRunHandle(runID string) *fakeRunHandle {
	return &fakeRunHandle{
		id:     runID,
		state:  engine.RunState{RunID: runID, Status: engine.RunStatusRunning, StartedAt: time.Now(), UpdatedAt: time.Now()},
		events: make(chan engine.Event, 16),
	}
}

func (f *fakeRunHandle) Next(_ context.Context) (*engine.StepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.results) == 0 {
		if f.eofStatus != "" {
			f.state.Status = f.eofStatus
			f.state.CompletedAt = time.Time{}
			f.state.UpdatedAt = time.Now()
		}
		if f.nextErr != nil {
			return nil, f.nextErr
		}
		return nil, io.EOF
	}
	res := f.results[0]
	f.results = f.results[1:]
	if res != nil {
		f.state.CurrentStep = res.StepID
	}
	if len(f.results) == 0 && f.nextErr == nil {
		f.state.Status = engine.RunStatusCompleted
		f.state.UpdatedAt = time.Now()
	}
	return res, nil
}

func (f *fakeRunHandle) Approve(_ context.Context, _ engine.ApprovalDecision) error {
	return engine.ErrNotImplemented
}

func (f *fakeRunHandle) SubmitEvidence(_ context.Context, _ string, _ map[string]*engine.EvidenceValue) error {
	return engine.ErrNotImplemented
}

func (f *fakeRunHandle) Cancel(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelError != nil {
		return f.cancelError
	}
	f.state.Status = engine.RunStatusCancelled
	f.state.UpdatedAt = time.Now()
	f.closeEventsLocked()
	return nil
}

func (f *fakeRunHandle) State() engine.RunState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeRunHandle) Events() <-chan engine.Event {
	return f.events
}

func (f *fakeRunHandle) pushEvent(ev engine.Event) {
	f.events <- ev
}

func (f *fakeRunHandle) closeEvents() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeEventsLocked()
}

func (f *fakeRunHandle) closeEventsLocked() {
	if f.closed {
		return
	}
	close(f.events)
	f.closed = true
}

func makeWorkDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

type sentinelError struct {
	msg string
}

func (s sentinelError) Error() string {
	return s.msg
}

var errSentinel = sentinelError{msg: "sentinel"}

func isSentinel(err error) bool {
	return errors.Is(err, errSentinel)
}
