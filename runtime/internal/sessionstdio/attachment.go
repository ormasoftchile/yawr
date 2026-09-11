package sessionstdio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

const ErrorSessionAlreadyActive = "session-already-active"

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	cause   error
}

func (protocolErr *ProtocolError) Error() string {
	if protocolErr == nil {
		return "session stdio: protocol error"
	}
	return fmt.Sprintf("session stdio: %s: %s", protocolErr.Code, protocolErr.Message)
}

func (protocolErr *ProtocolError) Unwrap() error {
	if protocolErr == nil {
		return nil
	}
	return protocolErr.cause
}

type AttachmentStore interface {
	FrameStore
	SessionUpdates(string) <-chan struct{}
	AcquireSessionLease(context.Context, string) (session.Lease, error)
	LoadManifest(context.Context, string) (session.Manifest, error)
	PauseSession(context.Context, session.PauseRequest) (session.Manifest, error)
	RecordDetach(context.Context, session.DetachRequest) (session.Manifest, error)
}

type Attachment struct {
	store            AttachmentStore
	sessionID        string
	lease            session.Lease
	projector        *Projector
	output           io.Writer
	replayMu         sync.Mutex
	deferReplay      bool
	writeMu          sync.Mutex
	sequenceMu       sync.Mutex
	lastSequence     int64
	operationMu      sync.Mutex
	release          sync.Once
	releaseErr       error
	detachCommandID  string
	detachedManifest session.Manifest
	commandHandler   CommandHandler
	updates          <-chan struct{}
}

type CommandHandler interface {
	HandleSessionCommand(context.Context, *Attachment, session.StdioCommand, string) error
}

func (attachment *Attachment) WithCommandHandler(handler CommandHandler) *Attachment {
	attachment.operationMu.Lock()
	attachment.commandHandler = handler
	attachment.operationMu.Unlock()
	return attachment
}

func Attach(
	ctx context.Context,
	store AttachmentStore,
	sessionID string,
	afterSequence int64,
	output io.Writer,
) (*Attachment, error) {
	return AttachReconciled(ctx, store, sessionID, afterSequence, output, nil)
}

func AttachReconciled(
	ctx context.Context,
	store AttachmentStore,
	sessionID string,
	afterSequence int64,
	output io.Writer,
	reconcile func(*Attachment) error,
) (*Attachment, error) {
	if store == nil || output == nil {
		return nil, errors.New("session stdio: store and output are required")
	}
	lease, err := store.AcquireSessionLease(ctx, sessionID)
	if err != nil {
		if errors.Is(err, session.ErrSessionLeaseHeld) {
			return nil, &ProtocolError{
				Code: ErrorSessionAlreadyActive, Message: "another writer owns the session", cause: err,
			}
		}
		return nil, err
	}
	manifest, err := store.LoadManifest(ctx, sessionID)
	if err != nil {
		return nil, errors.Join(err, lease.Release())
	}
	if manifest.SchemaVersion != session.ManifestSchemaV1 || manifest.Session.SessionID != sessionID {
		return nil, errors.Join(
			protocolError(ErrorInvalidCommand, "session manifest is invalid for attach", nil),
			lease.Release(),
		)
	}
	attachment := &Attachment{
		store: store, sessionID: sessionID, lease: lease,
		projector: NewProjector(store, 0), output: output,
		updates: store.SessionUpdates(sessionID), lastSequence: afterSequence, deferReplay: true,
	}
	if reconcile != nil {
		if err := reconcile(attachment); err != nil {
			return nil, errors.Join(err, attachment.Release())
		}
	}
	manifest, err = store.LoadManifest(ctx, sessionID)
	if err != nil {
		return nil, errors.Join(err, attachment.Release())
	}
	graphSegmentID := attachmentGraphSegmentID(manifest)
	graphRevision := manifest.Segments[graphSegmentID].GraphRevision
	attachment.projector.WithGraphSegmentFilter(func(segmentID string, revision int64) bool {
		return segmentID == graphSegmentID && revision == graphRevision
	})
	attachment.replayMu.Lock()
	attachment.deferReplay = false
	attachment.replayMu.Unlock()
	if err := attachment.Replay(ctx, afterSequence); err != nil {
		return nil, errors.Join(err, attachment.Release())
	}
	manifest, err = store.LoadManifest(ctx, sessionID)
	if err != nil {
		return nil, errors.Join(err, attachment.Release())
	}
	if attachment.LastSequence() == afterSequence {
		if err := attachment.sendAttachmentSnapshot(manifest); err != nil {
			return nil, errors.Join(err, attachment.Release())
		}
	}
	return attachment, nil
}

func attachmentGraphSegmentID(manifest session.Manifest) string {
	if manifest.Session.ActiveSegmentID != "" {
		return manifest.Session.ActiveSegmentID
	}
	segmentID, ordinal := "", 0
	for candidateID, segment := range manifest.Segments {
		if segment.Ordinal > ordinal || segment.Ordinal == ordinal && candidateID > segmentID {
			segmentID, ordinal = candidateID, segment.Ordinal
		}
	}
	return segmentID
}

func Adopt(
	ctx context.Context,
	store AttachmentStore,
	sessionID string,
	afterSequence int64,
	output io.Writer,
	lease session.Lease,
) (*Attachment, error) {
	if store == nil || output == nil || lease == nil || lease.Epoch() == 0 {
		return nil, errors.New("session stdio: adopted store, output, and lease are required")
	}
	attachment := &Attachment{
		store: store, sessionID: sessionID, lease: lease,
		projector: NewProjector(store, 0), output: output,
		updates: store.SessionUpdates(sessionID), lastSequence: afterSequence,
	}
	if err := attachment.Replay(ctx, afterSequence); err != nil {
		return attachment, err
	}
	manifest, err := store.LoadManifest(ctx, sessionID)
	if err != nil {
		return attachment, err
	}
	if attachment.LastSequence() != manifest.Session.Sequence {
		return attachment, errors.New("session stdio: startup replay did not reach the journal head")
	}
	if err := attachment.sendAttachmentSnapshot(manifest); err != nil {
		return attachment, err
	}
	return attachment, nil
}

func (attachment *Attachment) WriterEpoch() uint64 {
	if attachment == nil || attachment.lease == nil {
		return 0
	}
	return attachment.lease.Epoch()
}

func (attachment *Attachment) SessionID() string {
	if attachment == nil {
		return ""
	}
	return attachment.sessionID
}

func (attachment *Attachment) Updates() <-chan struct{} {
	if attachment == nil {
		return nil
	}
	return attachment.updates
}

func (attachment *Attachment) sendAttachmentSnapshot(manifest session.Manifest) error {
	encoded, err := json.Marshal(session.ProjectManifest(manifest))
	if err != nil {
		return err
	}
	frame := session.StdioFrame{
		Version: session.StdioProtocolV1, Type: session.FrameSessionSnapshot,
		FrameID: session.DigestJSON(struct {
			SessionID   string `json:"session_id"`
			Sequence    int64  `json:"sequence"`
			WriterEpoch uint64 `json:"writer_epoch"`
		}{attachment.sessionID, manifest.Session.Sequence, attachment.WriterEpoch()}),
		SessionID: attachment.sessionID, SessionSequence: manifest.Session.Sequence,
		SequenceIndex: 0, SequenceCount: 1, WriterEpoch: attachment.WriterEpoch(),
		SegmentID: manifest.Session.ActiveSegmentID, RunID: manifest.Session.ActiveRunID,
		Payload: encoded,
	}
	attachment.writeMu.Lock()
	defer attachment.writeMu.Unlock()
	return WriteFrame(attachment.output, frame)
}

func (attachment *Attachment) advanceWriterEpoch() error {
	attachment.operationMu.Lock()
	defer attachment.operationMu.Unlock()
	lease, ok := attachment.lease.(interface {
		Advance() (uint64, error)
	})
	if !ok {
		return errors.New("session stdio: attachment lease cannot advance its writer epoch")
	}
	_, err := lease.Advance()
	return err
}

func (attachment *Attachment) Replay(ctx context.Context, _ int64) error {
	if attachment == nil || attachment.projector == nil || attachment.output == nil {
		return errors.New("session stdio: attachment is not initialized")
	}
	attachment.replayMu.Lock()
	defer attachment.replayMu.Unlock()
	if attachment.deferReplay {
		return nil
	}
	attachment.sequenceMu.Lock()
	afterSequence := attachment.lastSequence
	attachment.sequenceMu.Unlock()
	attachment.writeMu.Lock()
	defer attachment.writeMu.Unlock()
	lastSequence, err := attachment.projector.Project(
		ctx, attachment.sessionID, afterSequence, func(group []session.StdioFrame) error {
			for _, frame := range group {
				frame.WriterEpoch = attachment.WriterEpoch()
				if err := WriteFrame(attachment.output, frame); err != nil {
					return err
				}
			}
			return nil
		},
	)
	attachment.sequenceMu.Lock()
	if lastSequence > attachment.lastSequence {
		attachment.lastSequence = lastSequence
	}
	attachment.sequenceMu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (attachment *Attachment) LastSequence() int64 {
	if attachment == nil {
		return 0
	}
	attachment.sequenceMu.Lock()
	defer attachment.sequenceMu.Unlock()
	return attachment.lastSequence
}

func (attachment *Attachment) SendProtocolError(ctx context.Context, protocolErr *ProtocolError) error {
	if attachment == nil || protocolErr == nil {
		return errors.New("session stdio: protocol error is required")
	}
	payload, err := json.Marshal(protocolErr)
	if err != nil {
		return err
	}
	sequence := attachment.LastSequence()
	frame := session.StdioFrame{
		Version: session.StdioProtocolV1, Type: session.FrameProtocolError,
		FrameID: session.DigestJSON(struct {
			SessionID string `json:"session_id"`
			Sequence  int64  `json:"sequence"`
			Code      string `json:"code"`
		}{attachment.sessionID, sequence, protocolErr.Code}),
		SessionID: attachment.sessionID, SessionSequence: sequence,
		SequenceIndex: 0, SequenceCount: 1, WriterEpoch: attachment.WriterEpoch(), Payload: payload,
	}
	attachment.writeMu.Lock()
	defer attachment.writeMu.Unlock()
	return WriteFrame(attachment.output, frame)
}

func WriteFrame(output io.Writer, frame session.StdioFrame) error {
	if output == nil {
		return errors.New("session stdio: frame output is required")
	}
	encoded, err := marshalStdioFrame(frame)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	written, err := output.Write(encoded)
	if err == nil && written != len(encoded) {
		return io.ErrShortWrite
	}
	return err
}

func marshalStdioFrame(frame session.StdioFrame) ([]byte, error) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if len(encoded)+1 > session.MaxStdioFrameBytes {
		return nil, fmt.Errorf("session stdio: serialized frame exceeds %d bytes", session.MaxStdioFrameBytes)
	}
	return encoded, nil
}

func (attachment *Attachment) Release() error {
	if attachment == nil {
		return nil
	}
	attachment.release.Do(func() {
		if attachment.lease != nil {
			attachment.releaseErr = attachment.lease.Release()
		}
	})
	return attachment.releaseErr
}

func (attachment *Attachment) Detach(
	ctx context.Context,
	commandID string,
	reason string,
) (session.Manifest, error) {
	return attachment.detach(ctx, commandID, "", reason)
}

func (attachment *Attachment) detach(
	ctx context.Context,
	commandID string,
	clientCommandDigest string,
	reason string,
) (session.Manifest, error) {
	if attachment == nil || attachment.store == nil || attachment.lease == nil {
		return session.Manifest{}, errors.New("session stdio: attachment is not initialized")
	}
	attachment.operationMu.Lock()
	defer attachment.operationMu.Unlock()
	if attachment.detachCommandID != "" {
		if attachment.detachCommandID != commandID {
			return session.Manifest{}, errors.New("session stdio: attachment is already detached")
		}
		return attachment.detachedManifest, attachment.releaseErr
	}
	manifest, err := attachment.store.LoadManifest(ctx, attachment.sessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	if manifest.Session.Status == session.StatusIndeterminate || manifest.Session.ActiveRunID == "" {
		detached, detachErr := attachment.store.RecordDetach(context.WithoutCancel(ctx), session.DetachRequest{
			SessionID: attachment.sessionID, CommandID: commandID, WriterEpoch: attachment.lease.Epoch(),
			ExpectedSequence: manifest.Session.Sequence, ClientCommandDigest: clientCommandDigest, Reason: reason,
		})
		if detachErr != nil {
			if !errors.Is(detachErr, session.ErrProjection) {
				return session.Manifest{}, detachErr
			}
			detached, detachErr = attachment.store.LoadManifest(context.WithoutCancel(ctx), attachment.sessionID)
			receipt, found := detached.AcceptedCommands[commandID]
			if detachErr != nil || !found || receipt.CommandDigest != session.DetachDigest(reason) ||
				receipt.ClientCommandDigest != clientCommandDigest || receipt.EventKind != session.EventSessionDetached {
				return session.Manifest{}, errors.Join(session.ErrProjection, detachErr)
			}
		}
		attachment.detachCommandID = commandID
		attachment.detachedManifest = detached
		replayErr := attachment.Replay(context.WithoutCancel(ctx), manifest.Session.Sequence)
		return detached, errors.Join(replayErr, attachment.Release())
	}
	paused, err := attachment.store.PauseSession(context.WithoutCancel(ctx), session.PauseRequest{
		SessionID: attachment.sessionID, CommandID: commandID, WriterEpoch: attachment.lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, ClientCommandDigest: clientCommandDigest, Reason: reason,
	})
	if err != nil {
		return session.Manifest{}, err
	}
	attachment.detachCommandID = commandID
	attachment.detachedManifest = paused
	replayErr := attachment.Replay(context.WithoutCancel(ctx), manifest.Session.Sequence)
	if err := attachment.Release(); err != nil {
		return paused, errors.Join(replayErr, err)
	}
	return paused, replayErr
}
