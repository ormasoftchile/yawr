package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstdio"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

var (
	errSessionStartPrimary = errors.New("primary output failure")
	errSessionStartDetach  = errors.New("durable detach failure")
	errSessionStartRelease = errors.New("lease release failure")
)

type failingSessionStartCleanup struct {
	order []string
}

func (cleanup *failingSessionStartCleanup) Detach(
	context.Context,
	string,
	string,
) (session.Manifest, error) {
	cleanup.order = append(cleanup.order, "detach")
	return session.Manifest{}, errSessionStartDetach
}

func (cleanup *failingSessionStartCleanup) Release() error {
	cleanup.order = append(cleanup.order, "release")
	return errSessionStartRelease
}

func TestCleanupFailedSessionStartJoinsAllErrorsInOrder(t *testing.T) {
	cleanup := &failingSessionStartCleanup{}
	err := cleanupFailedSessionStart(
		context.Background(), errSessionStartPrimary, cleanup, cleanup, "startup failed",
	)
	if !errors.Is(err, errSessionStartPrimary) || !errors.Is(err, errSessionStartDetach) ||
		!errors.Is(err, errSessionStartRelease) {
		t.Fatalf("cleanup error = %v", err)
	}
	if len(cleanup.order) != 2 || cleanup.order[0] != "detach" || cleanup.order[1] != "release" {
		t.Fatalf("cleanup order = %#v", cleanup.order)
	}
}

func TestSessionAttachCLIReplaysFramesAndReleasesOnEOF(t *testing.T) {
	base := t.TempDir()
	store := sessionstore.NewDirStore(base)
	request := createCLITestSession(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var output bytes.Buffer
	code := sessionMain(
		context.Background(),
		[]string{"attach", request.SessionID, "--stdio", "--after-sequence", "0", "--session-dir", base,
			"--run-dir", filepath.Join(base, "runs"), "--tool-dir", base},
		io.NopCloser(strings.NewReader("")), &output, &bytes.Buffer{},
	)
	if code != exitSuccess {
		t.Fatalf("sessionMain code = %d", code)
	}
	decoder := json.NewDecoder(&output)
	frames := 0
	for {
		var frame session.StdioFrame
		if err := decoder.Decode(&frame); err != nil {
			break
		}
		if frame.Version != session.StdioProtocolV1 || frame.SessionID != request.SessionID {
			t.Fatalf("frame = %#v", frame)
		}
		frames++
	}
	if frames == 0 {
		t.Fatal("attach emitted no frames")
	}
	reopened := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = reopened.Close() })
	manifest, err := reopened.LoadManifest(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.Session.Status != session.StatusPaused ||
		manifest.Attempts[manifest.Session.ActiveRunID].Status != session.AttemptStatusPausedAtBoundary {
		t.Fatalf("EOF did not pause session: %#v", manifest)
	}
	lease, err := reopened.AcquireSessionLease(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("lease remained held after EOF: %v", err)
	}
	_ = lease.Release()
}

func TestSessionGraphCLIReadsImmutableRevisionWithoutWriterLease(t *testing.T) {
	base := t.TempDir()
	store := sessionstore.NewDirStore(base)
	request := createCLITestSession(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var output, errorOutput bytes.Buffer
	code := sessionMain(
		context.Background(),
		[]string{"graph", request.SessionID, "--segment-id", request.Segment.SegmentID,
			"--revision", "1", "--session-dir", base},
		io.NopCloser(strings.NewReader("")), &output, &errorOutput,
	)
	if code != exitSuccess {
		t.Fatalf("session graph code = %d, stderr = %s", code, errorOutput.String())
	}
	var response struct {
		SchemaVersion string                `json:"schema_version"`
		SessionID     string                `json:"session_id"`
		Segment       session.SegmentRecord `json:"segment"`
		GraphRevision int64                 `json:"graph_revision"`
		GraphHash     string                `json:"graph_hash"`
		Data          []byte                `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("decode graph response: %v", err)
	}
	if response.SchemaVersion != "yawr.session-graph-revision/v1" || response.SessionID != request.SessionID ||
		response.Segment.SegmentID != request.Segment.SegmentID || response.GraphRevision != 1 ||
		response.GraphHash != session.DigestBytes(response.Data) || !json.Valid(response.Data) {
		t.Fatalf("graph response = %#v", response)
	}
}

func TestSessionAttachCLIEmitsStructuredActiveWriterError(t *testing.T) {
	base := t.TempDir()
	store := sessionstore.NewDirStore(base)
	request := createCLITestSession(t, store)
	held, err := store.AcquireSessionLease(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("Acquire held lease: %v", err)
	}
	defer held.Release()
	var output bytes.Buffer
	code := sessionMain(
		context.Background(),
		[]string{"attach", request.SessionID, "--stdio", "--session-dir", base,
			"--run-dir", filepath.Join(base, "runs"), "--tool-dir", base, "--after-sequence", "7"},
		io.NopCloser(strings.NewReader("")), &output, &bytes.Buffer{},
	)
	if code != exitRuntime {
		t.Fatalf("sessionMain code = %d", code)
	}
	var frame session.StdioFrame
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &frame); err != nil {
		t.Fatalf("decode protocol error: %v", err)
	}
	if frame.Type != session.FrameProtocolError {
		t.Fatalf("protocol error frame = %#v", frame)
	}
	if frame.SessionSequence != 7 || frame.SequenceIndex != 0 || frame.SequenceCount != 1 || frame.WriterEpoch != 0 {
		t.Fatalf("pre-lease protocol error envelope = %#v", frame)
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil || payload.Code != "session-already-active" {
		t.Fatalf("protocol error payload = %#v, %v", payload, err)
	}
}

func TestSessionAttachCLIProcessesExplicitIdempotentDetach(t *testing.T) {
	base := t.TempDir()
	store := sessionstore.NewDirStore(base)
	request := createCLITestSession(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	commandID := uuid.NewString()
	command, _ := json.Marshal(session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: commandID, SessionID: request.SessionID, WriterEpoch: 2, ExpectedSequence: 2,
	})
	var output bytes.Buffer
	code := sessionMain(
		context.Background(),
		[]string{"attach", request.SessionID, "--stdio", "--after-sequence", "0", "--session-dir", base,
			"--run-dir", filepath.Join(base, "runs"), "--tool-dir", base},
		io.NopCloser(bytes.NewReader(append(command, '\n'))), &output, &bytes.Buffer{},
	)
	if code != exitSuccess {
		t.Fatalf("sessionMain code = %d", code)
	}
	reopened := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = reopened.Close() })
	manifest, err := reopened.LoadManifest(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	receipt := manifest.AcceptedCommands[commandID]
	if manifest.Session.Sequence != 3 || manifest.Session.Status != session.StatusPaused ||
		receipt.EventKind != session.EventSessionPaused || receipt.Sequence != 3 {
		t.Fatalf("explicit detach manifest/receipt = %#v/%#v", manifest.Session, receipt)
	}
	decoder := json.NewDecoder(&output)
	foundPause := false
	for {
		var frame session.StdioFrame
		if err := decoder.Decode(&frame); err != nil {
			break
		}
		foundPause = foundPause || frame.Type == session.FrameAttemptPaused && frame.SessionSequence == 3
	}
	if !foundPause {
		t.Fatalf("explicit detach output omitted pause frame: %s", output.String())
	}
}

func TestSessionAttachCLIReleasesIndeterminateSessionWithoutStateChange(t *testing.T) {
	for _, test := range []struct {
		name     string
		explicit bool
	}{
		{name: "stdin EOF"},
		{name: "explicit detach", explicit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			sessionDir := filepath.Join(base, "sessions")
			store := sessionstore.NewDirStore(sessionDir)
			request := createCLITestSession(t, store)
			lease, err := store.AcquireSessionLease(context.Background(), request.SessionID)
			if err != nil {
				t.Fatalf("AcquireSessionLease: %v", err)
			}
			manifest, err := store.LoadManifest(context.Background(), request.SessionID)
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			indeterminate, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
				SessionID: request.SessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
				ExpectedSequence: manifest.Session.Sequence, RunID: manifest.Session.ActiveRunID,
				Status: session.AttemptStatusIndeterminate,
			})
			if err != nil {
				t.Fatalf("UpdateAttempt indeterminate: %v", err)
			}
			if err := lease.Release(); err != nil {
				t.Fatalf("Release setup lease: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("Close setup store: %v", err)
			}
			var input io.ReadCloser = io.NopCloser(strings.NewReader(""))
			if test.explicit {
				command, _ := json.Marshal(session.StdioCommand{
					Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
					CommandID: uuid.NewString(), SessionID: request.SessionID,
					WriterEpoch: 3, ExpectedSequence: indeterminate.Session.Sequence,
				})
				input = io.NopCloser(bytes.NewReader(append(command, '\n')))
			}
			var output bytes.Buffer
			var errorOutput bytes.Buffer
			code := sessionMain(context.Background(), []string{
				"attach", request.SessionID, "--stdio", "--after-sequence", "3",
				"--session-dir", sessionDir, "--run-dir", filepath.Join(base, "runs"), "--tool-dir", base,
			}, input, &output, &errorOutput)
			if code != exitSuccess {
				t.Fatalf("sessionMain code = %d: %s", code, errorOutput.String())
			}
			reopened := sessionstore.NewDirStore(sessionDir)
			t.Cleanup(func() { _ = reopened.Close() })
			unchanged, err := reopened.LoadManifest(context.Background(), request.SessionID)
			if err != nil {
				t.Fatalf("LoadManifest unchanged: %v", err)
			}
			if unchanged.Session.Status != session.StatusIndeterminate ||
				unchanged.Session.Sequence != indeterminate.Session.Sequence+1 ||
				unchanged.Attempts[indeterminate.Session.ActiveRunID].Status != session.AttemptStatusIndeterminate {
				t.Fatalf("indeterminate session changed = %#v", unchanged.Session)
			}
			events, err := reopened.ReadEvents(context.Background(), request.SessionID, indeterminate.Session.Sequence)
			if err != nil || len(events) != 1 || events[0].Kind != session.EventSessionDetached ||
				events[0].ClientCommandDigest == "" {
				t.Fatalf("detach receipt events = %#v, %v", events, err)
			}
			newLease, err := reopened.AcquireSessionLease(context.Background(), request.SessionID)
			if err != nil {
				t.Fatalf("attachment lease remained held: %v", err)
			}
			_ = newLease.Release()
		})
	}
}

func TestSessionStartCLICreatesClientIdentifiedSessionAndStreamsFrames(t *testing.T) {
	base := t.TempDir()
	runbookPath := filepath.Join(base, "start.runbook.yaml")
	if err := os.WriteFile(runbookPath, []byte(`$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: session-start
name: Session start
inputs:
  region:
    type: string
    required: true
  environment:
    type: string
    default: prod
flow:
  - step:
      id: done
      type: noop
`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sessionID := uuid.NewString()
	commandID := uuid.NewString()
	parsedSessionID := uuid.MustParse(sessionID)
	runID := uuid.NewSHA1(parsedSessionID, []byte("run:"+commandID)).String()
	runDir := filepath.Join(base, "runs")
	var output bytes.Buffer
	var errorOutput bytes.Buffer
	code := sessionMain(context.Background(), []string{
		"start", runbookPath, "--session-id", sessionID, "--command-id", commandID, "--stdio",
		"--var", "region=westus",
		"--session-dir", filepath.Join(base, "sessions"), "--run-dir", runDir,
		"--tool-dir", base,
	}, io.NopCloser(strings.NewReader("")), &output, &errorOutput)
	if code != exitSuccess {
		runs := internalrunstore.NewDirRunStore(runDir)
		state, stateErr := runs.LoadState(context.Background(), runID)
		_ = runs.Close()
		t.Fatalf("sessionMain code = %d: %s; run status=%s dispatches=%d load=%v",
			code, errorOutput.String(), state.Status, len(state.Dispatches), stateErr)
	}
	store := sessionstore.NewDirStore(filepath.Join(base, "sessions"))
	t.Cleanup(func() { _ = store.Close() })
	manifest, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	wantRunID := runID
	wantSegmentID := uuid.NewSHA1(parsedSessionID, []byte("segment:"+commandID)).String()
	if manifest.Session.CreationCommandID != commandID || manifest.Session.RootSegmentID != wantSegmentID ||
		manifest.Attempts[wantRunID].SegmentID != wantSegmentID || len(manifest.Segments) != 1 {
		t.Fatalf("created session = %#v", manifest)
	}
	runs := internalrunstore.NewDirRunStore(runDir)
	t.Cleanup(func() { _ = runs.Close() })
	state, err := runs.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Vars["region"] != "westus" {
		t.Fatalf("durable region input = %#v", state.Vars["region"])
	}
	if state.Vars["environment"] != "prod" {
		t.Fatalf("durable default input = %#v", state.Vars["environment"])
	}
	decoder := json.NewDecoder(&output)
	foundStarted := false
	for {
		var frame session.StdioFrame
		if err := decoder.Decode(&frame); err != nil {
			break
		}
		foundStarted = foundStarted || frame.Type == session.FrameSessionStarted && frame.SessionID == sessionID
	}
	if !foundStarted {
		t.Fatalf("session.started frame missing: %s", output.String())
	}
}

func TestSessionStartCLIUsesPackageMapCatalogBinding(t *testing.T) {
	packageRoot := t.TempDir()
	writeAcmeIncidentToolsPackage(t, packageRoot, os.Args[0], "session-package-binding")
	workspace := t.TempDir()
	relativePackageRoot := relForwardSlash(t, workspace, packageRoot)
	packageMapPath := filepath.Join(workspace, "package-map.yaml")
	writeFile(t, packageMapPath, "apiVersion: yawr.config/v1\n"+
		"requires:\n"+
		"  - package: acme.incident-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relativePackageRoot+"\n")
	profilePath := filepath.Join(workspace, "session.profile.yaml")
	writeFile(t, profilePath, profileYAML("vscode-operator", "attended"))
	runbookPath := filepath.Join(workspace, "catalog.runbook.yaml")
	writeFile(t, runbookPath, `apiVersion: yawr.runbook/v1
id: session-catalog
name: Session catalog
toolRefs:
  - name: kubectl
    package: acme.incident-tools
flow:
  - step:
      id: identify
      type: tool
      tool:
        name: kubectl
        action: identify
`)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	sessionID, commandID := uuid.NewString(), uuid.NewString()
	parsedSessionID := uuid.MustParse(sessionID)
	runID := uuid.NewSHA1(parsedSessionID, []byte("run:"+commandID)).String()
	runDir := filepath.Join(workspace, "runs")
	var output, errorOutput bytes.Buffer
	code := sessionMain(context.Background(), []string{
		"start", runbookPath, "--session-id", sessionID, "--command-id", commandID, "--stdio",
		"--package-map", packageMapPath, "--profile", profilePath,
		"--session-dir", filepath.Join(workspace, "sessions"), "--run-dir", runDir,
		"--tool-dir", workspace,
	}, io.NopCloser(strings.NewReader("")), &output, &errorOutput)
	if code != exitSuccess {
		t.Fatalf("sessionMain code = %d: %s", code, errorOutput.String())
	}
	runs := internalrunstore.NewDirRunStore(runDir)
	t.Cleanup(func() { _ = runs.Close() })
	plan, err := runs.LoadPlan(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if plan.Tools["kubectl"] == nil || plan.Metadata.CatalogDigest == "" || plan.Metadata.Profile == nil ||
		plan.Metadata.Profile.ID != "test-profile" ||
		plan.Metadata.PackageDigests["acme.incident-tools"] == "" {
		t.Fatalf("session plan omitted catalog binding/provenance: tools=%v metadata=%#v", plan.Tools, plan.Metadata)
	}
}

type sessionFailWriter struct{}

func (sessionFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type sessionMatchingFailWriter struct {
	mu      sync.Mutex
	match   []byte
	matched chan struct{}
	frames  chan session.StdioFrame
	once    sync.Once
}

func (writer *sessionMatchingFailWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if bytes.Contains(data, writer.match) {
		writer.once.Do(func() { close(writer.matched) })
		return 0, io.ErrClosedPipe
	}
	if writer.frames != nil {
		var frame session.StdioFrame
		if err := json.Unmarshal(bytes.TrimSpace(data), &frame); err == nil {
			writer.frames <- frame
		}
	}
	return len(data), nil
}

type blockingSessionInput struct {
	started      chan struct{}
	closed       chan struct{}
	readReturned chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once
	returnOnce   sync.Once
}

func newBlockingSessionInput() *blockingSessionInput {
	return &blockingSessionInput{
		started: make(chan struct{}), closed: make(chan struct{}), readReturned: make(chan struct{}),
	}
}

func (input *blockingSessionInput) Read([]byte) (int, error) {
	input.startOnce.Do(func() { close(input.started) })
	<-input.closed
	input.returnOnce.Do(func() { close(input.readReturned) })
	return 0, io.ErrClosedPipe
}

func (input *blockingSessionInput) Close() error {
	input.closeOnce.Do(func() { close(input.closed) })
	return nil
}

type sessionRuntimeSignal struct {
	done chan error
}

func (runtime *sessionRuntimeSignal) Done() <-chan error { return runtime.done }

func TestServeSessionAttachmentClosesAndJoinsCommandReaderOnRuntimeFailure(t *testing.T) {
	input := newBlockingSessionInput()
	runtimeErr := errors.New("runtime failed")
	runtime := &sessionRuntimeSignal{done: make(chan error, 1)}
	var errorOutput bytes.Buffer
	finished := make(chan int, 1)
	go func() {
		finished <- serveSessionAttachment(
			context.Background(), nil, &sessionstdio.Attachment{}, runtime, input, &errorOutput,
		)
	}()
	select {
	case <-input.started:
	case <-time.After(time.Second):
		t.Fatal("command reader did not start")
	}
	runtime.done <- runtimeErr
	select {
	case code := <-finished:
		if code != exitRuntime || !strings.Contains(errorOutput.String(), runtimeErr.Error()) {
			t.Fatalf("serve result = %d/%q", code, errorOutput.String())
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not stop after runtime failure")
	}
	select {
	case <-input.readReturned:
	default:
		t.Fatal("serve returned before the command reader exited")
	}
}

func TestSessionStartCLIInitialOutputFailurePausesBeforeLeaseRelease(t *testing.T) {
	base := t.TempDir()
	runbookPath := filepath.Join(base, "output-failure.runbook.yaml")
	if err := os.WriteFile(runbookPath, []byte(`$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: session-output-failure
name: Session output failure
flow:
  - step:
      id: done
      type: noop
`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sessionID := uuid.NewString()
	sessionDir := filepath.Join(base, "sessions")
	var errorOutput bytes.Buffer
	code := sessionMain(context.Background(), []string{
		"start", runbookPath, "--session-id", sessionID, "--command-id", uuid.NewString(), "--stdio",
		"--session-dir", sessionDir, "--run-dir", filepath.Join(base, "runs"), "--tool-dir", base,
	}, io.NopCloser(strings.NewReader("")), sessionFailWriter{}, &errorOutput)
	if code != exitRuntime || !strings.Contains(errorOutput.String(), io.ErrClosedPipe.Error()) {
		t.Fatalf("sessionMain code/error = %d/%q", code, errorOutput.String())
	}
	store := sessionstore.NewDirStore(sessionDir)
	t.Cleanup(func() { _ = store.Close() })
	manifest, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	attempt := manifest.Attempts[manifest.Session.ActiveRunID]
	if manifest.Session.Status != session.StatusPaused || attempt.Status != session.AttemptStatusPausedAtBoundary {
		t.Fatalf("output failure manifest = %#v/%#v", manifest.Session, attempt)
	}
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("session lease remained held after output failure: %v", err)
	}
	_ = lease.Release()
}

func TestSessionStartCLIRuntimeOutputFailureExitsWithInputOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base := t.TempDir()
	runbookPath := filepath.Join(base, "runtime-output-failure.runbook.yaml")
	if err := os.WriteFile(runbookPath, []byte(`$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: session-runtime-output-failure
name: Session runtime output failure
flow:
  - step:
      id: done
      type: noop
`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sessionID := uuid.NewString()
	sessionDir := filepath.Join(base, "sessions")
	inputReader, inputWriter := io.Pipe()
	defer inputWriter.Close()
	writer := &sessionMatchingFailWriter{
		match: []byte("attempt.finished"), matched: make(chan struct{}), frames: make(chan session.StdioFrame, 32),
	}
	var errorOutput bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- sessionMain(ctx, []string{
			"start", runbookPath, "--session-id", sessionID, "--command-id", uuid.NewString(), "--stdio",
			"--session-dir", sessionDir, "--run-dir", filepath.Join(base, "runs"), "--tool-dir", base,
		}, inputReader, writer, &errorOutput)
	}()
	head := waitSessionHeadFrame(t, writer.frames)
	json.NewEncoder(inputWriter).Encode(session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionConfigure,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: head.WriterEpoch,
		ExpectedSequence: head.SessionSequence, Payload: json.RawMessage(`{"inputs":{}}`),
	})
	select {
	case <-writer.matched:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime output failure was not reached")
	}
	select {
	case code := <-done:
		if code != exitRuntime || !strings.Contains(errorOutput.String(), io.ErrClosedPipe.Error()) {
			t.Fatalf("sessionMain code/error = %d/%q", code, errorOutput.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session CLI did not exit after runtime output failure with stdin open")
	}
	store := sessionstore.NewDirStore(sessionDir)
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.AcquireSessionLease(ctx, sessionID); err != nil {
		t.Fatalf("session lease remained held after runtime output failure: %v", err)
	}
}

func TestSessionAttachCLIResumesAnswersAndRetriesWithoutDuplicateJournalEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := t.TempDir()
	sessionDir := filepath.Join(base, "sessions")
	runDir := filepath.Join(base, "runs")
	runbookPath := filepath.Join(base, "choice.runbook.yaml")
	if err := os.WriteFile(runbookPath, []byte(`$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: session-choice
name: Session choice
flow:
  - step:
      id: choose
      type: choice
      prompt: Choose one
      variable: selected
      options:
        - value: one
          label: One
  - step:
      id: done
      type: noop
`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sessionID := uuid.NewString()
	creationCommandID := uuid.NewString()
	start := startSessionCLIProcess(t, ctx, []string{
		"start", runbookPath, "--session-id", sessionID, "--command-id", creationCommandID, "--stdio",
		"--session-dir", sessionDir, "--run-dir", runDir, "--tool-dir", base,
	})
	configureSessionCLIProcess(t, start, sessionID)
	firstPending, firstPause := waitPendingBoundary(t, start, 0)
	detachCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: firstPause.WriterEpoch,
		ExpectedSequence: firstPause.SessionSequence,
	}
	start.send(t, detachCommand)
	if code := start.wait(t); code != exitSuccess {
		t.Fatalf("start code = %d: %s", code, start.stderr.String())
	}

	store := sessionstore.NewDirStore(sessionDir)
	t.Cleanup(func() { _ = store.Close() })
	paused, err := store.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest paused: %v", err)
	}
	if paused.Session.Status != session.StatusPaused || paused.Session.ActiveRunID == "" {
		t.Fatalf("paused session = %#v", paused.Session)
	}
	runID := paused.Session.ActiveRunID

	attached := startSessionCLIProcess(t, ctx, []string{
		"attach", sessionID, "--stdio", "--after-sequence", "0",
		"--session-dir", sessionDir, "--run-dir", runDir, "--tool-dir", base,
	})
	replayedPending, replayedPause := waitPendingBoundary(t, attached, paused.Session.Sequence)
	if replayedPending.TurnID != firstPending.TurnID || replayedPause.WriterEpoch <= firstPause.WriterEpoch {
		t.Fatalf("replayed turn/epoch = %s/%d, want %s/>%d",
			replayedPending.TurnID, replayedPause.WriterEpoch, firstPending.TurnID, firstPause.WriterEpoch)
	}
	resumeCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionResume,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: replayedPause.WriterEpoch,
		ExpectedSequence: paused.Session.Sequence,
	}
	attached.send(t, resumeCommand)
	resumedPending, _ := waitPendingBoundary(t, attached, paused.Session.Sequence+1)
	if resumedPending.TurnID != firstPending.TurnID {
		t.Fatalf("resumed turn = %s, want %s", resumedPending.TurnID, firstPending.TurnID)
	}
	waiting, err := store.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest waiting: %v", err)
	}
	answerPayload, _ := json.Marshal(map[string]any{"kind": "choice", "selected": []string{"o:0"}})
	answerCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandInteractionAnswer,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: replayedPause.WriterEpoch,
		ExpectedSequence: waiting.Session.Sequence, RunID: runID, TurnID: firstPending.TurnID,
		Payload: answerPayload,
	}
	attached.send(t, answerCommand)
	finished := attached.waitFrame(t, func(frame session.StdioFrame) bool {
		return frame.Type == session.FrameAttemptFinished && frame.RunID == runID &&
			frame.SessionSequence > answerCommand.ExpectedSequence
	})
	attached.waitFrame(t, func(frame session.StdioFrame) bool {
		if frame.Type != session.FrameRunEvent || frame.RunID != runID ||
			frame.SessionSequence <= finished.SessionSequence {
			return false
		}
		var event struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(frame.Payload, &event)
		return event.Kind == "checkpoint"
	})
	eventsBeforeRetry, err := store.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents before retry: %v", err)
	}
	attached.send(t, answerCommand)
	beforeClose, err := store.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest before close: %v", err)
	}
	closePayload, _ := json.Marshal(map[string]any{"status": session.StatusResolved})
	closeCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionClose,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: replayedPause.WriterEpoch,
		ExpectedSequence: beforeClose.Session.Sequence, Payload: closePayload,
	}
	attached.send(t, closeCommand)
	attached.waitFrame(t, func(frame session.StdioFrame) bool {
		return frame.Type == session.FrameSessionFinished && frame.SessionSequence > beforeClose.Session.Sequence
	})
	eventsAfterRetry, err := store.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents after retry: %v", err)
	}
	if len(eventsAfterRetry) != len(eventsBeforeRetry)+1 {
		t.Fatalf("answer retry and close events = %d -> %d", len(eventsBeforeRetry), len(eventsAfterRetry))
	}
	answerEvents := 0
	closeEvents := 0
	for _, event := range eventsAfterRetry {
		if event.CommandID == answerCommand.CommandID {
			answerEvents++
			if event.ClientCommandDigest == "" {
				t.Fatalf("answer event omitted client command digest: %#v", event)
			}
		}
		if event.CommandID == closeCommand.CommandID && event.Kind == session.EventSessionClosed {
			closeEvents++
		}
	}
	if answerEvents != 1 || closeEvents != 1 {
		t.Fatalf("answer/close event counts = %d/%d", answerEvents, closeEvents)
	}
	if code := attached.wait(t); code != exitSuccess {
		t.Fatalf("attach code = %d: %s", code, attached.stderr.String())
	}
	completed, err := store.LoadManifest(ctx, sessionID)
	if err != nil || completed.Session.Status != session.StatusResolved || completed.Session.ActiveRunID != "" ||
		completed.Attempts[runID].Status != session.AttemptStatusCompleted {
		t.Fatalf("completed session = %#v, %v", completed, err)
	}
}

type sessionCLIProcess struct {
	input       *io.PipeWriter
	output      *io.PipeReader
	frames      chan session.StdioFrame
	frameErrors chan error
	done        chan int
	stderr      bytes.Buffer
}

type pendingFrame struct {
	TurnID string `json:"turn_id"`
}

func startSessionCLIProcess(t *testing.T, ctx context.Context, args []string) *sessionCLIProcess {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	process := &sessionCLIProcess{
		input: inputWriter, output: outputReader, frames: make(chan session.StdioFrame, 512),
		frameErrors: make(chan error, 1), done: make(chan int, 1),
	}
	go func() {
		process.done <- sessionMain(ctx, args, inputReader, outputWriter, &process.stderr)
		_ = outputWriter.Close()
		_ = inputReader.Close()
	}()
	go func() {
		decoder := json.NewDecoder(outputReader)
		for {
			var frame session.StdioFrame
			if err := decoder.Decode(&frame); err != nil {
				process.frameErrors <- err
				close(process.frames)
				return
			}
			process.frames <- frame
		}
	}()
	t.Cleanup(func() {
		_ = inputWriter.Close()
		_ = outputReader.Close()
	})
	return process
}

func (process *sessionCLIProcess) send(t *testing.T, command session.StdioCommand) {
	t.Helper()
	if err := json.NewEncoder(process.input).Encode(command); err != nil {
		select {
		case code := <-process.done:
			process.done <- code
			t.Fatalf("send %s: %v; process code=%d stderr=%s", command.Type, err, code, process.stderr.String())
		default:
			t.Fatalf("send %s: %v; stderr=%s", command.Type, err, process.stderr.String())
		}
	}
}

func (process *sessionCLIProcess) waitFrame(t *testing.T, match func(session.StdioFrame) bool) session.StdioFrame {
	t.Helper()
	for {
		select {
		case frame, ok := <-process.frames:
			if !ok {
				err := <-process.frameErrors
				t.Fatalf("session output ended before expected frame: %v; stderr: %s", err, process.stderr.String())
			}
			if match(frame) {
				return frame
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for session frame; stderr: %s", process.stderr.String())
		}
	}
}

func (process *sessionCLIProcess) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-process.done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for session CLI; stderr: %s", process.stderr.String())
		return -1
	}
}

func configureSessionCLIProcess(t *testing.T, process *sessionCLIProcess, sessionID string) {
	t.Helper()
	head := waitSessionHeadFrame(t, process.frames)
	process.send(t, session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionConfigure,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: head.WriterEpoch,
		ExpectedSequence: head.SessionSequence, Payload: json.RawMessage(`{"inputs":{}}`),
	})
}

func waitSessionHeadFrame(t *testing.T, frames <-chan session.StdioFrame) session.StdioFrame {
	t.Helper()
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("session output ended before startup head handshake")
			}
			if frame.Type == session.FrameSessionSnapshot && frame.SequenceIndex == 0 && frame.SequenceCount == 1 {
				return frame
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for session startup head handshake")
		}
	}
}

func waitPendingBoundary(t *testing.T, process *sessionCLIProcess, minimumSequence int64) (pendingFrame, session.StdioFrame) {
	t.Helper()
	var pending pendingFrame
	var pendingSequence int64
	for {
		frame := process.waitFrame(t, func(session.StdioFrame) bool { return true })
		if frame.Type == session.FrameInteractionPending {
			if err := json.Unmarshal(frame.Payload, &pending); err != nil || pending.TurnID == "" {
				t.Fatalf("decode pending interaction: %#v, %v", frame, err)
			}
			pendingSequence = frame.SessionSequence
		}
		if pending.TurnID != "" && frame.Type == session.FrameAttemptPaused &&
			frame.SessionSequence >= pendingSequence && frame.SessionSequence >= minimumSequence {
			return pending, frame
		}
	}
}

func createCLITestSession(t *testing.T, store *sessionstore.DirStore) session.CreateRequest {
	t.Helper()
	sessionID := uuid.NewString()
	segmentID := uuid.NewString()
	runID := uuid.NewString()
	planBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"execution-plan/v3"}`))
	graphBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"yawr.graph-json/v1","nodes":[],"edges":[]}`))
	request := session.CreateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(),
		Session: session.SessionRecord{Status: session.StatusActive},
		Segment: session.SegmentRecord{
			SegmentID: segmentID, Ordinal: 1, RunbookID: "root", RunbookName: "Root",
			Status: session.SegmentStatusActive, PlanHash: planBlob.Digest, GraphHash: graphBlob.Digest,
			ExecutableSnapshotHash: planBlob.Digest, AttemptRunIDs: []string{runID},
		},
		Attempt: session.RunAttemptRecord{
			RunID: runID, SegmentID: segmentID, Ordinal: 1, Mode: "real", Status: session.AttemptStatusRunning,
		},
		Blobs: []session.JSONBlob{planBlob, graphBlob},
	}
	identity, err := session.NewCreationIdentity(
		sessionID, request.Session, request.Segment, request.Attempt, nil, nil, "",
	)
	if err != nil {
		t.Fatalf("NewCreationIdentity: %v", err)
	}
	request.Identity = identity
	request.EntryDigest = session.CreationDigest(identity)
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	manifest, err := store.CreateSession(context.Background(), lease.Epoch(), request)
	if err != nil {
		_ = lease.Release()
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: runID, Status: session.AttemptStatusPausedAtBoundary,
	}); err != nil {
		_ = lease.Release()
		t.Fatalf("pause attempt: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	return request
}
