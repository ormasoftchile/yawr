package sessionstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/serve"
	"github.com/ormasoftchile/yawr/runtime/internal/sessioncoordinator"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

type CoordinatorRuntime struct {
	coordinator *sessioncoordinator.Coordinator
	broker      *serve.PromptBroker
	runOptions  engine.RunOptions

	mu                          sync.Mutex
	handle                      *sessioncoordinator.Handle
	runID                       string
	started                     bool
	driving                     bool
	stopped                     bool
	requireStartupConfiguration bool
	runCtx                      context.Context
	cancel                      context.CancelFunc
	done                        chan error
}

func NewCoordinatorRuntime(
	coordinator *sessioncoordinator.Coordinator,
	broker *serve.PromptBroker,
	runOptions engine.RunOptions,
) (*CoordinatorRuntime, error) {
	if coordinator == nil || broker == nil {
		return nil, errors.New("session stdio: coordinator and interaction broker are required")
	}
	return &CoordinatorRuntime{
		coordinator: coordinator, broker: broker, runOptions: runOptions, done: make(chan error, 1),
	}, nil
}

func (runtime *CoordinatorRuntime) Done() <-chan error { return runtime.done }

func (runtime *CoordinatorRuntime) RequireStartupConfiguration() {
	runtime.mu.Lock()
	if !runtime.started {
		runtime.requireStartupConfiguration = true
	}
	runtime.mu.Unlock()
}

func (runtime *CoordinatorRuntime) ReconcileAttached(ctx context.Context, attachment *Attachment) error {
	if attachment == nil || attachment.WriterEpoch() == 0 {
		return errors.New("session stdio: attachment lease is required for reconciliation")
	}
	manifest, err := attachment.store.LoadManifest(ctx, attachment.sessionID)
	if err != nil {
		return err
	}
	runID := manifest.Session.ActiveRunID
	if runID == "" {
		return nil
	}
	attempt, found := manifest.Attempts[runID]
	if !found {
		return errors.New("session stdio: active attempt is unavailable")
	}
	switch attempt.Status {
	case session.AttemptStatusStarting, session.AttemptStatusRunning, session.AttemptStatusWaiting:
	case session.AttemptStatusPausedAtBoundary, session.AttemptStatusHandoffPending,
		session.AttemptStatusCompleted, session.AttemptStatusFailed,
		session.AttemptStatusCancelled, session.AttemptStatusIndeterminate:
		return nil
	default:
		return errors.New("session stdio: active attempt has invalid status")
	}
	runtime.broker.Register(runID)
	defer runtime.broker.Unregister(runID)
	resumeCommandID := uuid.NewSHA1(uuid.MustParse(attachment.sessionID), []byte(fmt.Sprintf(
		"attach-reconcile-resume:%d", attachment.WriterEpoch(),
	))).String()
	handle, err := runtime.coordinator.ResumeAttached(ctx, sessioncoordinator.ResumeRequest{
		SessionID: attachment.sessionID, CommandID: resumeCommandID, RunOptions: runtime.runOptions,
	}, attachment.lease)
	if err != nil {
		if errors.Is(err, engine.ErrIndeterminateAcknowledgmentRequired) {
			manifest, loadErr := attachment.store.LoadManifest(context.WithoutCancel(ctx), attachment.sessionID)
			if loadErr != nil {
				return errors.Join(err, loadErr)
			}
			attempt := manifest.Attempts[manifest.Session.ActiveRunID]
			if manifest.Session.Status != session.StatusIndeterminate ||
				attempt.Status != session.AttemptStatusIndeterminate {
				return err
			}
			if advanceErr := attachment.advanceWriterEpoch(); advanceErr != nil {
				return errors.Join(err, advanceErr)
			}
			return attachment.Replay(context.WithoutCancel(ctx), attachment.LastSequence())
		}
		return err
	}
	pauseCommandID := uuid.NewSHA1(uuid.MustParse(attachment.sessionID), []byte(fmt.Sprintf(
		"attach-reconcile-pause:%d", attachment.WriterEpoch(),
	))).String()
	_, pauseErr := handle.PauseAttached(
		context.WithoutCancel(ctx), pauseCommandID, "recovered orphaned stdio attachment",
	)
	if pauseErr != nil && !errors.Is(pauseErr, engine.ErrIndeterminate) {
		return pauseErr
	}
	if err := attachment.advanceWriterEpoch(); err != nil {
		return err
	}
	return attachment.Replay(context.WithoutCancel(ctx), attachment.LastSequence())
}

func (runtime *CoordinatorRuntime) StartAttached(
	ctx context.Context,
	attachment *Attachment,
	handle *sessioncoordinator.Handle,
) error {
	if attachment == nil || handle == nil || handle.SessionID() != attachment.sessionID ||
		handle.WriterEpoch() != attachment.WriterEpoch() {
		return errors.New("session stdio: attached coordinator handle does not own the transport lease")
	}
	manifest := handle.Manifest()
	runID := manifest.Session.ActiveRunID
	if runID == "" {
		return errors.New("session stdio: attached coordinator handle has no active run")
	}
	runtime.mu.Lock()
	if runtime.started && !runtime.stopped {
		runtime.mu.Unlock()
		return errors.New("session stdio: coordinator runtime already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	runtime.handle = handle
	runtime.runID = runID
	runtime.started = true
	runtime.driving = !runtime.requireStartupConfiguration
	runtime.stopped = false
	runtime.runCtx = runCtx
	runtime.cancel = cancel
	drive := runtime.driving
	runtime.mu.Unlock()
	go runtime.followJournal(runCtx, attachment)
	if drive {
		go runtime.drive(runCtx, attachment, handle)
	}
	return nil
}

func (runtime *CoordinatorRuntime) HandleSessionCommand(
	ctx context.Context,
	attachment *Attachment,
	command session.StdioCommand,
	clientCommandDigest string,
) error {
	switch command.Type {
	case session.CommandSessionResume:
		return runtime.resume(ctx, attachment, command, clientCommandDigest)
	case session.CommandInteractionAnswer:
		return runtime.answer(command, clientCommandDigest)
	case session.CommandSessionDetach:
		return runtime.detach(ctx, attachment, command, clientCommandDigest)
	case session.CommandSessionCancel:
		return runtime.cancelSession(ctx, command, clientCommandDigest)
	case session.CommandSessionClose:
		return runtime.closeSession(ctx, attachment, command, clientCommandDigest)
	case session.CommandSessionConfigure:
		return runtime.configure(ctx, attachment, command, clientCommandDigest)
	case session.CommandSessionContinueLive:
		return protocolError(ErrorUnsupported, "session.continue_live requires a replay-to-live boundary", nil)
	default:
		return protocolError(ErrorUnsupported, fmt.Sprintf("command %q is not implemented", command.Type), nil)
	}
}

type configurePayload struct {
	Inputs map[string]string `json:"inputs,omitempty"`
}

func (runtime *CoordinatorRuntime) configure(
	ctx context.Context,
	attachment *Attachment,
	command session.StdioCommand,
	clientCommandDigest string,
) error {
	var payload configurePayload
	if err := decodeStrictPayload(command.Payload, &payload); err != nil {
		return protocolError(ErrorInvalidCommand, "session configure payload is invalid", err)
	}
	if len(payload.Inputs) > 64 {
		return protocolError(ErrorInvalidCommand, "session configure inputs exceed 64 fields", nil)
	}
	for name, value := range payload.Inputs {
		if name == "" || len([]byte(name)) > 256 || len([]byte(value)) > 64*1024 {
			return protocolError(ErrorInvalidCommand, "session configure contains an invalid private input", nil)
		}
	}
	manifest, err := attachment.store.LoadManifest(ctx, attachment.sessionID)
	if err != nil {
		return err
	}
	if receipt, found := manifest.AcceptedCommands[command.CommandID]; found {
		if receipt.ClientCommandDigest != clientCommandDigest || receipt.EventKind != session.EventExecutionCommitted {
			return session.ErrCommandConflict
		}
		return runtime.startConfiguredDrive(attachment)
	}

	runtime.mu.Lock()
	handle, runID := runtime.handle, runtime.runID
	valid := runtime.requireStartupConfiguration && runtime.started && !runtime.driving && !runtime.stopped &&
		handle != nil && (command.RunID == "" || command.RunID == runID)
	runtime.mu.Unlock()
	if !valid {
		return protocolError(ErrorInvalidCommand, "session startup configuration is not available", nil)
	}
	if err := handle.ConfigureStartupCommand(ctx, command.CommandID, clientCommandDigest, payload.Inputs); err != nil {
		return protocolError(ErrorInvalidCommand, "session startup configuration was rejected", err)
	}
	return runtime.startConfiguredDrive(attachment)
}

func (runtime *CoordinatorRuntime) startConfiguredDrive(attachment *Attachment) error {
	runtime.mu.Lock()
	if runtime.driving || runtime.stopped {
		runtime.mu.Unlock()
		return nil
	}
	if !runtime.started || runtime.handle == nil || runtime.runCtx == nil {
		runtime.mu.Unlock()
		return protocolError(ErrorInvalidCommand, "session runtime is unavailable", nil)
	}
	runtime.driving = true
	runCtx, handle := runtime.runCtx, runtime.handle
	runtime.mu.Unlock()
	go runtime.drive(runCtx, attachment, handle)
	return nil
}

type cancelPayload struct {
	Reason string `json:"reason,omitempty"`
}

func (runtime *CoordinatorRuntime) cancelSession(
	ctx context.Context,
	command session.StdioCommand,
	clientCommandDigest string,
) error {
	runtime.mu.Lock()
	handle, runID := runtime.handle, runtime.runID
	runtime.mu.Unlock()
	if handle == nil || command.RunID != "" && command.RunID != runID {
		return protocolError(ErrorInvalidCommand, "session cancel does not target the active run", nil)
	}
	var payload cancelPayload
	if err := decodeStrictPayload(command.Payload, &payload); err != nil {
		return protocolError(ErrorInvalidCommand, "session cancel payload is invalid", err)
	}
	if payload.Reason == "" {
		payload.Reason = "operator cancelled"
	}
	if len(payload.Reason) > 1024 || strings.ContainsAny(payload.Reason, "\r\n\x00") {
		return protocolError(ErrorInvalidCommand, "session cancel reason is invalid", nil)
	}
	runtime.mu.Lock()
	runtime.stopped = true
	runtime.mu.Unlock()
	_, err := handle.CancelCommand(ctx, command.CommandID, clientCommandDigest, payload.Reason)
	if err != nil && !errors.Is(err, engine.ErrIndeterminate) {
		return err
	}
	runtime.mu.Lock()
	if runtime.cancel != nil {
		runtime.cancel()
	}
	runtime.mu.Unlock()
	runtime.broker.Unregister(runID)
	return nil
}

type closePayload struct {
	Status session.Status `json:"status"`
}

func (runtime *CoordinatorRuntime) closeSession(
	ctx context.Context,
	attachment *Attachment,
	command session.StdioCommand,
	clientCommandDigest string,
) error {
	runtime.mu.Lock()
	handle := runtime.handle
	runtime.mu.Unlock()
	if command.RunID != "" {
		return protocolError(ErrorInvalidCommand, "session close does not target the investigation", nil)
	}
	var payload closePayload
	if err := decodeStrictPayload(command.Payload, &payload); err != nil {
		return protocolError(ErrorInvalidCommand, "session close payload is invalid", err)
	}
	switch payload.Status {
	case session.StatusResolved, session.StatusEscalated, session.StatusCancelled, session.StatusAbandoned:
	default:
		return protocolError(ErrorInvalidCommand, "session close status is invalid", nil)
	}
	if handle == nil {
		_, err := runtime.coordinator.CloseAttached(ctx, session.CloseRequest{
			SessionID: attachment.sessionID, CommandID: command.CommandID,
			ExpectedSequence: command.ExpectedSequence, ClientCommandDigest: clientCommandDigest,
			Status: payload.Status,
		}, attachment.lease)
		return err
	}
	_, err := handle.CloseInvestigationCommand(ctx, command.CommandID, clientCommandDigest, payload.Status)
	return err
}

func (runtime *CoordinatorRuntime) resume(
	ctx context.Context,
	attachment *Attachment,
	command session.StdioCommand,
	clientCommandDigest string,
) error {
	runtime.mu.Lock()
	if runtime.started && !runtime.stopped {
		runtime.mu.Unlock()
		manifest, err := attachment.store.LoadManifest(ctx, attachment.sessionID)
		if err != nil {
			return err
		}
		receipt, found := manifest.AcceptedCommands[command.CommandID]
		if found && receipt.ClientCommandDigest == clientCommandDigest {
			return nil
		}
		return protocolError(ErrorInvalidCommand, "session is already resumed", nil)
	}
	manifest, err := attachment.store.LoadManifest(ctx, attachment.sessionID)
	if err != nil {
		runtime.mu.Unlock()
		return err
	}
	runID := manifest.Session.ActiveRunID
	if runID == "" {
		runtime.mu.Unlock()
		return protocolError(ErrorInvalidCommand, "session has no active run", nil)
	}
	resumeCommandID := command.CommandID
	resumeCommandDigest := clientCommandDigest
	if receipt, found := manifest.AcceptedCommands[command.CommandID]; found {
		if receipt.ClientCommandDigest != clientCommandDigest {
			runtime.mu.Unlock()
			return session.ErrCommandConflict
		}
		resumeCommandID = uuid.NewSHA1(uuid.MustParse(attachment.sessionID), []byte(fmt.Sprintf(
			"accepted-resume-recovery:%s:%d", command.CommandID, attachment.WriterEpoch(),
		))).String()
		resumeCommandDigest = ""
	}
	runtime.broker.Register(runID)
	runCtx, cancel := context.WithCancel(ctx)
	handle, err := runtime.coordinator.ResumeAttached(runCtx, sessioncoordinator.ResumeRequest{
		SessionID: attachment.sessionID, CommandID: resumeCommandID,
		ClientCommandDigest: resumeCommandDigest, RunOptions: runtime.runOptions,
	}, attachment.lease)
	if err != nil {
		cancel()
		runtime.broker.Unregister(runID)
		runtime.mu.Unlock()
		return err
	}
	activeRunID := handle.Manifest().Session.ActiveRunID
	if activeRunID == "" {
		cancel()
		runtime.broker.Unregister(runID)
		runtime.mu.Unlock()
		return errors.New("session stdio: resumed coordinator has no active run")
	}
	if activeRunID != runID {
		runtime.broker.Register(activeRunID)
		runtime.broker.Unregister(runID)
	}
	runtime.handle = handle
	runtime.runID = activeRunID
	runtime.started = true
	runtime.driving = true
	runtime.stopped = false
	runtime.runCtx = runCtx
	runtime.cancel = cancel
	runtime.mu.Unlock()

	go runtime.followJournal(runCtx, attachment)
	go runtime.drive(runCtx, attachment, handle)
	return nil
}

func (runtime *CoordinatorRuntime) answer(command session.StdioCommand, clientCommandDigest string) error {
	runtime.mu.Lock()
	handle, runID, stopped := runtime.handle, runtime.runID, runtime.stopped
	runtime.mu.Unlock()
	if handle == nil || stopped || command.RunID != runID || command.TurnID == "" {
		return protocolError(ErrorInvalidCommand, "interaction answer does not target the active run", nil)
	}
	var answer serve.AnswerEnvelope
	if err := decodeStrictPayload(command.Payload, &answer); err != nil {
		return protocolError(ErrorInvalidCommand, "interaction answer payload is invalid", err)
	}
	return runtime.broker.AnswerCommand(runID, command.TurnID, command.CommandID, clientCommandDigest, answer)
}

func (runtime *CoordinatorRuntime) detach(
	ctx context.Context,
	attachment *Attachment,
	command session.StdioCommand,
	clientCommandDigest string,
) error {
	runtime.mu.Lock()
	handle, runID := runtime.handle, runtime.runID
	runtime.stopped = true
	runtime.mu.Unlock()
	if handle == nil {
		_, err := attachment.detach(ctx, command.CommandID, clientCommandDigest, "session.detach")
		return err
	}
	_, err := handle.PauseAttachedCommand(ctx, command.CommandID, clientCommandDigest, "session.detach")
	runtime.mu.Lock()
	if runtime.cancel != nil {
		runtime.cancel()
	}
	runtime.mu.Unlock()
	runtime.broker.Unregister(runID)
	if errors.Is(err, engine.ErrIndeterminate) {
		return nil
	}
	return err
}

func (runtime *CoordinatorRuntime) drive(
	ctx context.Context,
	attachment *Attachment,
	handle *sessioncoordinator.Handle,
) {
	var terminalErr error
	for {
		_, err := handle.Next(ctx)
		if syncErr := runtime.synchronizeActiveRun(handle); syncErr != nil {
			terminalErr = syncErr
			break
		}
		if replayErr := attachment.Replay(context.WithoutCancel(ctx), attachment.LastSequence()); replayErr != nil {
			runtime.failTransport(attachment, replayErr)
			return
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			terminalErr = err
			break
		}
	}
	runtime.mu.Lock()
	alreadyStopped := runtime.stopped
	if !alreadyStopped {
		runtime.stopped = true
	}
	runID := runtime.runID
	cancel := runtime.cancel
	runtime.mu.Unlock()
	runtime.broker.Unregister(runID)
	if alreadyStopped {
		return
	}
	if cancel != nil {
		cancel()
	}
	select {
	case runtime.done <- terminalErr:
	default:
	}
}

func (runtime *CoordinatorRuntime) synchronizeActiveRun(handle *sessioncoordinator.Handle) error {
	manifest := handle.Manifest()
	nextRunID := manifest.Session.ActiveRunID
	if nextRunID == "" {
		return nil
	}
	runtime.mu.Lock()
	previousRunID := runtime.runID
	stopped := runtime.stopped
	runtime.mu.Unlock()
	if stopped || nextRunID == previousRunID {
		return nil
	}
	runtime.broker.Register(nextRunID)
	runtime.mu.Lock()
	if runtime.stopped {
		runtime.mu.Unlock()
		runtime.broker.Unregister(nextRunID)
		return context.Canceled
	}
	runtime.runID = nextRunID
	runtime.mu.Unlock()
	if previousRunID != "" {
		runtime.broker.Unregister(previousRunID)
	}
	return nil
}

func (runtime *CoordinatorRuntime) followJournal(ctx context.Context, attachment *Attachment) {
	updates := attachment.Updates()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-updates:
			if !ok {
				return
			}
			if err := attachment.Replay(ctx, attachment.LastSequence()); err != nil {
				runtime.failTransport(attachment, err)
				return
			}
		}
	}
}

func (runtime *CoordinatorRuntime) failTransport(attachment *Attachment, transportErr error) {
	runtime.mu.Lock()
	handle := runtime.handle
	runID := runtime.runID
	if runtime.stopped {
		runtime.mu.Unlock()
		return
	}
	runtime.stopped = true
	if runtime.cancel != nil {
		runtime.cancel()
	}
	runtime.mu.Unlock()
	var detachErr error
	if handle != nil {
		commandID := uuid.NewSHA1(uuid.MustParse(attachment.sessionID), []byte(fmt.Sprintf(
			"transport-loss:%d:%s", attachment.WriterEpoch(), transportErr,
		))).String()
		_, detachErr = handle.Detach(context.Background(), commandID, "stdio transport lost")
	}
	runtime.broker.Unregister(runID)
	releaseErr := attachment.Release()
	terminalErr := errors.Join(transportErr, detachErr, releaseErr)
	select {
	case runtime.done <- terminalErr:
	default:
	}
}

func decodeStrictPayload(encoded json.RawMessage, target any) error {
	if len(encoded) == 0 {
		return errors.New("payload is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("payload must contain one JSON value")
	}
	return nil
}
