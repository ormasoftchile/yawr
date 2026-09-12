package sessionstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

const maxCommandBytes = 1 << 20

const (
	ErrorInvalidCommand   = "invalid-command"
	ErrorStaleWriter      = "stale-writer"
	ErrorSequenceConflict = "sequence-conflict"
	ErrorUnsupported      = "unsupported-command"
)

type ServeResult struct {
	Detached bool
}

func (attachment *Attachment) Serve(ctx context.Context, input io.Reader) (ServeResult, error) {
	if attachment == nil || input == nil {
		return ServeResult{}, errors.New("session stdio: command input is required")
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64*1024), maxCommandBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return ServeResult{}, err
		}
		command, err := decodeCommand(scanner.Bytes())
		if err != nil {
			return ServeResult{}, err
		}
		detached, err := attachment.HandleCommand(ctx, command)
		if err != nil {
			return ServeResult{}, err
		}
		if detached {
			return ServeResult{Detached: true}, nil
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return ServeResult{}, protocolError(ErrorInvalidCommand, "command exceeds the 1 MiB limit", err)
		}
		return ServeResult{}, err
	}
	return ServeResult{}, nil
}

func decodeCommand(encoded []byte) (session.StdioCommand, error) {
	if len(bytes.TrimSpace(encoded)) == 0 {
		return session.StdioCommand{}, protocolError(ErrorInvalidCommand, "command is empty", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var command session.StdioCommand
	if err := decoder.Decode(&command); err != nil {
		return session.StdioCommand{}, protocolError(ErrorInvalidCommand, "command is malformed", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return session.StdioCommand{}, protocolError(ErrorInvalidCommand, "command must contain one JSON object", err)
	}
	return command, nil
}

func (attachment *Attachment) HandleCommand(
	ctx context.Context,
	command session.StdioCommand,
) (bool, error) {
	if command.Version != session.StdioProtocolV1 || command.SessionID != attachment.sessionID ||
		command.CommandID == "" || command.ExpectedSequence < 1 {
		return false, protocolError(ErrorInvalidCommand, "command envelope is invalid", nil)
	}
	if _, err := uuid.Parse(command.CommandID); err != nil {
		return false, protocolError(ErrorInvalidCommand, "commandID must be a UUID", err)
	}
	if command.WriterEpoch != attachment.WriterEpoch() {
		return false, protocolError(ErrorStaleWriter, "command writer epoch does not own the attachment", session.ErrSessionLeaseStale)
	}
	clientCommandDigest, err := session.StdioCommandDigest(command)
	if err != nil {
		return false, protocolError(ErrorInvalidCommand, "command payload is invalid", err)
	}
	manifest, err := attachment.store.LoadManifest(ctx, attachment.sessionID)
	if err != nil {
		return false, err
	}
	if receipt, found := manifest.AcceptedCommands[command.CommandID]; found {
		if receipt.ClientCommandDigest == "" || receipt.ClientCommandDigest != clientCommandDigest {
			return false, protocolError(ErrorInvalidCommand, "commandID was accepted with different content", session.ErrCommandConflict)
		}
		if command.Type == session.CommandSessionConfigure {
			attachment.operationMu.Lock()
			handler := attachment.commandHandler
			attachment.operationMu.Unlock()
			if handler == nil {
				return false, protocolError(ErrorUnsupported, "session configure runtime is unavailable", nil)
			}
			if err := handler.HandleSessionCommand(ctx, attachment, command, clientCommandDigest); err != nil {
				return false, err
			}
		}
		if command.Type == session.CommandSessionResume && !clientCommandReceiptSuperseded(manifest, receipt) &&
			resumeRetryNeedsRuntimeRecovery(manifest) {
			attachment.operationMu.Lock()
			handler := attachment.commandHandler
			attachment.operationMu.Unlock()
			if handler == nil {
				return false, protocolError(ErrorUnsupported, "session resume runtime is unavailable", nil)
			}
			if err := handler.HandleSessionCommand(ctx, attachment, command, clientCommandDigest); err != nil {
				return false, err
			}
		}
		if err := attachment.Replay(ctx, command.ExpectedSequence); err != nil {
			return false, err
		}
		if isTerminalAttachmentCommand(command.Type) && !clientCommandReceiptSuperseded(manifest, receipt) {
			return true, attachment.Release()
		}
		return false, nil
	}
	if manifest.Session.Sequence != command.ExpectedSequence {
		return false, protocolError(ErrorSequenceConflict, "command expected sequence is stale", session.ErrSequenceConflict)
	}
	switch command.Type {
	case session.CommandSessionDetach:
		attachment.operationMu.Lock()
		completedCommandID := attachment.detachCommandID
		handler := attachment.commandHandler
		attachment.operationMu.Unlock()
		if completedCommandID != "" {
			if completedCommandID == command.CommandID {
				return true, nil
			}
			return false, protocolError(ErrorInvalidCommand, "attachment is already detached", nil)
		}
		if handler == nil {
			_, err = attachment.detach(ctx, command.CommandID, clientCommandDigest, "session.detach")
			return err == nil, err
		}
		if err := handler.HandleSessionCommand(ctx, attachment, command, clientCommandDigest); err != nil {
			return false, err
		}
		attachment.operationMu.Lock()
		completedCommandID = attachment.detachCommandID
		attachment.operationMu.Unlock()
		if completedCommandID == command.CommandID {
			return true, nil
		}
		committed, err := attachment.store.LoadManifest(context.WithoutCancel(ctx), attachment.sessionID)
		if err != nil {
			return false, err
		}
		receipt, found := committed.AcceptedCommands[command.CommandID]
		if !found || receipt.ClientCommandDigest != clientCommandDigest {
			return false, errors.New("session stdio: detach handler returned before durable command receipt")
		}
		if err := attachment.Replay(context.WithoutCancel(ctx), command.ExpectedSequence); err != nil {
			return false, errors.Join(err, attachment.Release())
		}
		attachment.operationMu.Lock()
		attachment.detachCommandID = command.CommandID
		attachment.detachedManifest = committed
		attachment.operationMu.Unlock()
		return true, attachment.Release()
	default:
		attachment.operationMu.Lock()
		handler := attachment.commandHandler
		attachment.operationMu.Unlock()
		if handler == nil {
			return false, protocolError(ErrorUnsupported, fmt.Sprintf("command %q is not implemented", command.Type), nil)
		}
		if err := handler.HandleSessionCommand(ctx, attachment, command, clientCommandDigest); err != nil {
			return false, err
		}
		committed, err := attachment.store.LoadManifest(ctx, attachment.sessionID)
		if err != nil {
			return false, err
		}
		receipt, found := committed.AcceptedCommands[command.CommandID]
		if !found || receipt.ClientCommandDigest != clientCommandDigest {
			return false, errors.New("session stdio: command handler returned before durable command receipt")
		}
		if err := attachment.Replay(ctx, command.ExpectedSequence); err != nil {
			return false, err
		}
		if isTerminalAttachmentCommand(command.Type) {
			return true, attachment.Release()
		}
		return false, nil
	}
}

func clientCommandReceiptSuperseded(manifest session.Manifest, receipt session.CommandReceipt) bool {
	for _, candidate := range manifest.AcceptedCommands {
		if candidate.Sequence > receipt.Sequence && candidate.ClientCommandDigest != "" {
			return true
		}
	}
	return false
}

func resumeRetryNeedsRuntimeRecovery(manifest session.Manifest) bool {
	runID := manifest.Session.ActiveRunID
	if runID == "" {
		return false
	}
	attempt, found := manifest.Attempts[runID]
	if !found {
		return false
	}
	switch attempt.Status {
	case session.AttemptStatusStarting, session.AttemptStatusRunning, session.AttemptStatusWaiting,
		session.AttemptStatusPausedAtBoundary, session.AttemptStatusHandoffPending:
		return true
	default:
		return false
	}
}

func isTerminalAttachmentCommand(commandType session.CommandType) bool {
	switch commandType {
	case session.CommandSessionCancel, session.CommandSessionDetach, session.CommandSessionClose:
		return true
	default:
		return false
	}
}

func protocolError(code, message string, cause error) *ProtocolError {
	return &ProtocolError{Code: code, Message: message, cause: cause}
}
