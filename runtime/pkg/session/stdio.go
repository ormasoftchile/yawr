package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const StdioProtocolV1 = "yawr.session-stdio/v1"

const (
	MaxStdioFrameBytes           = 8 << 20
	MaxStdioFrameProjectionBytes = 4 << 20
)

type FrameType string

const (
	FrameSessionStarted        FrameType = "session.started"
	FrameSessionSnapshot       FrameType = "session.snapshot"
	FrameSegmentAdded          FrameType = "segment.added"
	FrameSegmentGraph          FrameType = "segment.graph"
	FrameSegmentGraphAvailable FrameType = "segment.graph.available"
	FrameTransitionPrepared    FrameType = "transition.prepared"
	FrameTransitionCommitted   FrameType = "transition.committed"
	FrameAttemptStarted        FrameType = "attempt.started"
	FrameRunEvent              FrameType = "run.event"
	FrameInteractionPending    FrameType = "interaction.pending"
	FrameInteractionResolved   FrameType = "interaction.resolved"
	FrameAttemptPaused         FrameType = "attempt.paused"
	FrameAttemptFinished       FrameType = "attempt.finished"
	FrameSegmentFinished       FrameType = "segment.finished"
	FrameSessionFinished       FrameType = "session.finished"
	FrameProtocolError         FrameType = "protocol.error"
)

type StdioFrame struct {
	Version         string          `json:"version"`
	Type            FrameType       `json:"type"`
	FrameID         string          `json:"frameID"`
	SessionID       string          `json:"sessionID"`
	SessionSequence int64           `json:"sessionSequence"`
	SequenceIndex   int             `json:"sequenceIndex"`
	SequenceCount   int             `json:"sequenceCount"`
	WriterEpoch     uint64          `json:"writerEpoch,omitempty"`
	SegmentID       string          `json:"segmentID,omitempty"`
	RunID           string          `json:"runID,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

type GraphChunkPayload struct {
	GraphRevision int64  `json:"graphRevision"`
	ChunkIndex    int    `json:"chunkIndex"`
	ChunkCount    int    `json:"chunkCount"`
	WholeBlobHash string `json:"wholeBlobHash"`
	Data          []byte `json:"data"`
}

type GraphAvailablePayload struct {
	GraphRevision int64  `json:"graphRevision"`
	WholeBlobHash string `json:"wholeBlobHash"`
}

type CommandType string

const (
	CommandSessionConfigure    CommandType = "session.configure"
	CommandInteractionAnswer   CommandType = "interaction.answer"
	CommandSessionCancel       CommandType = "session.cancel"
	CommandSessionDetach       CommandType = "session.detach"
	CommandSessionResume       CommandType = "session.resume"
	CommandSessionContinueLive CommandType = "session.continue_live"
	CommandSessionClose        CommandType = "session.close"
)

type StdioCommand struct {
	Version          string          `json:"version"`
	Type             CommandType     `json:"type"`
	CommandID        string          `json:"commandID"`
	SessionID        string          `json:"sessionID"`
	WriterEpoch      uint64          `json:"writerEpoch,omitempty"`
	ExpectedSequence int64           `json:"expectedSequence,omitempty"`
	SegmentID        string          `json:"segmentID,omitempty"`
	RunID            string          `json:"runID,omitempty"`
	TurnID           string          `json:"turnID,omitempty"`
	Payload          json.RawMessage `json:"payload,omitempty"`
}

func StdioCommandDigest(command StdioCommand) (string, error) {
	var payload any
	if len(command.Payload) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(command.Payload))
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			return "", err
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return "", errors.New("session: command payload must contain one JSON value")
		}
	}
	encoded, err := json.Marshal(struct {
		Version   string      `json:"version"`
		Type      CommandType `json:"type"`
		CommandID string      `json:"command_id"`
		SessionID string      `json:"session_id"`
		SegmentID string      `json:"segment_id,omitempty"`
		RunID     string      `json:"run_id,omitempty"`
		TurnID    string      `json:"turn_id,omitempty"`
		Payload   any         `json:"payload,omitempty"`
	}{
		Version: command.Version, Type: command.Type, CommandID: command.CommandID,
		SessionID: command.SessionID, SegmentID: command.SegmentID, RunID: command.RunID,
		TurnID: command.TurnID, Payload: payload,
	})
	if err != nil {
		return "", err
	}
	return DigestBytes(encoded), nil
}
