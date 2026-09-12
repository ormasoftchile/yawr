package sessioncoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

const (
	sessionProjectionChunkSchemaV1 = "yawr.session-run-projection-chunk/v1"
	sessionProjectionChunkBytes    = 8 << 20
	maxSessionProjectionFiles      = 10_000
	maxSessionProjectionBytes      = 512 << 20
)

type runProjectionArchive struct {
	SchemaVersion string                     `json:"schema_version"`
	RunID         string                     `json:"run_id"`
	Files         []runProjectionArchiveFile `json:"files"`
}

type runProjectionArchiveFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Data   []byte `json:"data"`
}

type sessionRunProjection struct {
	SchemaVersion      string                     `json:"schema_version"`
	RunID              string                     `json:"run_id"`
	CheckpointSequence int64                      `json:"checkpoint_sequence"`
	Files              []sessionRunProjectionFile `json:"files"`
}

type sessionRunProjectionFile struct {
	Path         string   `json:"path"`
	Digest       string   `json:"digest"`
	Size         int64    `json:"size"`
	ChunkDigests []string `json:"chunk_digests,omitempty"`
}

type sessionRunProjectionChunk struct {
	SchemaVersion string `json:"schema_version"`
	Data          []byte `json:"data"`
}

func encodeExecutionFrameProjection(state engine.RunState) (session.JSONBlob, []session.JSONBlob, error) {
	contentData, err := json.Marshal(session.ExecutionFrameProjectionContent{
		Interactions: state.Interactions, PendingTraceEvents: state.PendingTraceEvents,
	})
	if err != nil {
		return session.JSONBlob{}, nil, fmt.Errorf("session coordinator: encode execution frame projection: %w", err)
	}
	if len(contentData) == 0 || len(contentData) > session.MaxStdioFrameProjectionBytes {
		return session.JSONBlob{}, nil, errors.New("session coordinator: execution frame projection exceeds limits")
	}
	projection := session.ExecutionFrameProjection{
		SchemaVersion: session.ExecutionFrameProjectionSchemaV1, RunID: state.RunID,
		CheckpointSequence: state.CheckpointSequence, CommittedTraceSequence: state.CommittedTraceSequence,
		ContentDigest: session.DigestBytes(contentData), ContentSize: int64(len(contentData)),
	}
	chunks := make([]session.JSONBlob, 0, (len(contentData)+sessionProjectionChunkBytes-1)/sessionProjectionChunkBytes)
	for offset := 0; offset < len(contentData); offset += sessionProjectionChunkBytes {
		end := offset + sessionProjectionChunkBytes
		if end > len(contentData) {
			end = len(contentData)
		}
		chunkData, err := json.Marshal(session.ExecutionFrameProjectionChunk{
			SchemaVersion: session.ExecutionFrameProjectionChunkSchemaV1, Data: contentData[offset:end],
		})
		if err != nil {
			return session.JSONBlob{}, nil, err
		}
		chunk := session.NewJSONBlob(chunkData)
		projection.ChunkDigests = append(projection.ChunkDigests, chunk.Digest)
		chunks = append(chunks, chunk)
	}
	rootData, err := json.Marshal(projection)
	if err != nil {
		return session.JSONBlob{}, nil, err
	}
	return session.NewJSONBlob(rootData), chunks, nil
}

type sessionTraceCommitPayload struct {
	RunID         string `json:"run_id"`
	TraceHash     string `json:"trace_hash"`
	TraceSequence int64  `json:"trace_sequence"`
}

type runTraceRecord struct {
	Sequence  int64           `json:"seq"`
	Timestamp string          `json:"ts"`
	Kind      string          `json:"kind"`
	RunID     string          `json:"run_id"`
	RunbookID string          `json:"runbook_id,omitempty"`
	EventID   string          `json:"event_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"sig,omitempty"`
}

func encodeSessionRunProjection(
	encoded json.RawMessage,
	state engine.RunState,
) (session.JSONBlob, []session.JSONBlob, error) {
	var archive runProjectionArchive
	if err := decodeStrictDocument(encoded, &archive); err != nil {
		return session.JSONBlob{}, nil, fmt.Errorf("session coordinator: decode run projection: %w", err)
	}
	if archive.SchemaVersion != "yawr.run-projection/v1" || archive.RunID != state.RunID ||
		len(archive.Files) == 0 || len(archive.Files) > maxSessionProjectionFiles {
		return session.JSONBlob{}, nil, errors.New("session coordinator: invalid run projection")
	}
	currentSnapshot := fmt.Sprintf("snapshots/checkpoint-%020d.json", state.CheckpointSequence)
	referencedStateBlobs := make(map[string]bool)
	snapshotFound := false
	for _, file := range archive.Files {
		if file.Path != currentSnapshot {
			continue
		}
		snapshotFound = true
		var snapshot any
		if err := json.Unmarshal(file.Data, &snapshot); err != nil {
			return session.JSONBlob{}, nil, errors.New("session coordinator: current run snapshot is invalid")
		}
		collectStateBlobPaths(snapshot, referencedStateBlobs)
		break
	}
	if !snapshotFound {
		return session.JSONBlob{}, nil, errors.New("session coordinator: current run snapshot is missing from projection")
	}
	projection := sessionRunProjection{
		SchemaVersion: session.RunProjectionSchemaV1, RunID: state.RunID,
		CheckpointSequence: state.CheckpointSequence,
	}
	chunksByDigest := make(map[string]session.JSONBlob)
	total := int64(0)
	previousPath := ""
	for _, file := range archive.Files {
		if file.Path <= previousPath || file.Path == "" || session.DigestBytes(file.Data) != file.Digest {
			return session.JSONBlob{}, nil, errors.New("session coordinator: invalid run projection file")
		}
		previousPath = file.Path
		if strings.HasPrefix(file.Path, "snapshots/") && file.Path != currentSnapshot ||
			strings.HasPrefix(file.Path, "blobs/") && !referencedStateBlobs[file.Path] {
			continue
		}
		total += int64(len(file.Data))
		if total > maxSessionProjectionBytes {
			return session.JSONBlob{}, nil, errors.New("session coordinator: run projection exceeds session limits")
		}
		reference := sessionRunProjectionFile{Path: file.Path, Digest: file.Digest, Size: int64(len(file.Data))}
		for offset := 0; offset < len(file.Data); offset += sessionProjectionChunkBytes {
			end := offset + sessionProjectionChunkBytes
			if end > len(file.Data) {
				end = len(file.Data)
			}
			encodedChunk, err := json.Marshal(sessionRunProjectionChunk{
				SchemaVersion: sessionProjectionChunkSchemaV1, Data: file.Data[offset:end],
			})
			if err != nil {
				return session.JSONBlob{}, nil, err
			}
			chunk := session.NewJSONBlob(encodedChunk)
			chunksByDigest[chunk.Digest] = chunk
			reference.ChunkDigests = append(reference.ChunkDigests, chunk.Digest)
		}
		projection.Files = append(projection.Files, reference)
	}
	rootData, err := json.Marshal(projection)
	if err != nil {
		return session.JSONBlob{}, nil, err
	}
	root := session.NewJSONBlob(rootData)
	chunks := make([]session.JSONBlob, 0, len(chunksByDigest))
	for _, file := range projection.Files {
		for _, digest := range file.ChunkDigests {
			if chunk, found := chunksByDigest[digest]; found {
				chunks = append(chunks, chunk)
				delete(chunksByDigest, digest)
			}
		}
	}
	return root, chunks, nil
}

func alignRunProjectionWithPlan(
	encoded json.RawMessage,
	plan *engine.ExecutionPlan,
	dynamicIncludes map[string]*engine.DynamicIncludeResolutionState,
	settledDispatches map[string]*engine.DispatchState,
) (json.RawMessage, error) {
	if plan == nil || plan.Validation == nil {
		return nil, errors.New("session coordinator: revised projection plan is invalid")
	}
	var archive runProjectionArchive
	if err := decodeStrictDocument(encoded, &archive); err != nil {
		return nil, fmt.Errorf("session coordinator: decode revised run projection: %w", err)
	}
	snapshotPlan := *plan
	snapshotPlan.RunID = archive.RunID
	snapshot, err := plansnapshot.FromExecutionPlan(&snapshotPlan)
	if err != nil {
		return nil, fmt.Errorf("session coordinator: snapshot revised run projection plan: %w", err)
	}
	planData, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	planFound := false
	stateFound := false
	for index := range archive.Files {
		file := &archive.Files[index]
		switch {
		case file.Path == "plan.v1.json":
			file.Data = planData
			file.Digest = session.DigestBytes(planData)
			planFound = true
		case strings.HasPrefix(file.Path, "snapshots/"):
			data, err := alignRunStateSnapshotWithPlan(
				file.Data, plan, snapshot.SnapshotDigest, dynamicIncludes, settledDispatches,
			)
			if err != nil {
				return nil, err
			}
			file.Data = data
			file.Digest = session.DigestBytes(data)
			stateFound = true
		}
	}
	if !planFound || !stateFound {
		return nil, errors.New("session coordinator: revised run projection is incomplete")
	}
	return json.Marshal(archive)
}

func alignRunStateSnapshotWithPlan(
	encoded []byte,
	plan *engine.ExecutionPlan,
	planDigest string,
	dynamicIncludes map[string]*engine.DynamicIncludeResolutionState,
	settledDispatches map[string]*engine.DispatchState,
) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var snapshot map[string]any
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, errors.New("session coordinator: revised run state snapshot is invalid")
	}
	snapshot["PlanSnapshotDigest"] = planDigest
	mergedDynamicIncludes := make(map[string]*engine.DynamicIncludeResolutionState, len(dynamicIncludes))
	for resolutionID, resolution := range dynamicIncludes {
		mergedDynamicIncludes[resolutionID] = resolution
	}
	if encodedExisting, err := json.Marshal(snapshot["DynamicIncludes"]); err == nil && string(encodedExisting) != "null" {
		var existing map[string]*engine.DynamicIncludeResolutionState
		if err := decodeStrictDocument(encodedExisting, &existing); err != nil {
			return nil, errors.New("session coordinator: revised dynamic include state is invalid")
		}
		for resolutionID, resolution := range existing {
			journaled := mergedDynamicIncludes[resolutionID]
			if journaled != nil && dynamicResolutionIdentityDigest(*journaled) != dynamicResolutionIdentityDigest(*resolution) {
				return nil, errors.New("session coordinator: revised dynamic include identity conflicts with execution state")
			}
			mergedDynamicIncludes[resolutionID] = resolution
		}
	}
	snapshot["DynamicIncludes"] = mergedDynamicIncludes
	if len(settledDispatches) > 0 {
		encodedExisting, err := json.Marshal(snapshot["Dispatches"])
		if err != nil {
			return nil, errors.New("session coordinator: revised dispatch state is invalid")
		}
		existing := make(map[string]*engine.DispatchState)
		if string(encodedExisting) != "null" {
			if err := decodeStrictDocument(encodedExisting, &existing); err != nil {
				return nil, errors.New("session coordinator: revised dispatch state is invalid")
			}
		}
		for occurrenceID, settled := range settledDispatches {
			prepared := existing[occurrenceID]
			if prepared == nil || settled == nil ||
				dispatchIntentIdentityDigest(*prepared) != dispatchIntentIdentityDigest(*settled) ||
				prepared.Status != engine.DispatchStatusPrepared && prepared.Status != engine.DispatchStatusSettled {
				return nil, errors.New("session coordinator: revised resolver dispatch conflicts with execution state")
			}
			if prepared.Status == engine.DispatchStatusSettled && prepared.ResultDigest != settled.ResultDigest {
				return nil, errors.New("session coordinator: revised resolver result conflicts with execution state")
			}
			existing[occurrenceID] = settled
		}
		snapshot["Dispatches"] = existing
	}
	if currentStep, _ := snapshot["CurrentStep"].(string); currentStep != "" {
		index, err := revisedPlanStepIndex(plan, nil, currentStep)
		if err != nil {
			return nil, err
		}
		snapshot["CurrentStepIndex"] = index
	}
	if encodedCursors, err := json.Marshal(snapshot["CursorSet"]); err == nil && string(encodedCursors) != "null" {
		var cursors engine.ExecutionCursorSet
		if err := decodeStrictDocument(encodedCursors, &cursors); err != nil {
			return nil, errors.New("session coordinator: revised run cursor set is invalid")
		}
		for index := range cursors.Cursors {
			cursor := &cursors.Cursors[index]
			if cursor.Phase == engine.ExecutionPhaseExecute &&
				cursorHasJournaledDynamicResolution(*cursor, mergedDynamicIncludes, settledDispatches) {
				cursor.Phase = engine.ExecutionPhaseBefore
			}
			if cursor.FrameID != "" {
				continue
			}
			if cursor.AtEnd {
				cursor.StepIndex = len(plan.Steps)
				continue
			}
			stepIndex, err := revisedPlanStepIndex(plan, cursor.CallPath, cursor.StepID)
			if err != nil {
				return nil, err
			}
			cursor.StepIndex = stepIndex
		}
		snapshot["CursorSet"] = cursors
	}
	return json.Marshal(snapshot)
}

func cursorHasJournaledDynamicResolution(
	cursor engine.ExecutionCursor,
	resolutions map[string]*engine.DynamicIncludeResolutionState,
	dispatches map[string]*engine.DispatchState,
) bool {
	for _, resolution := range resolutions {
		if resolution == nil || resolution.QualifiedNodeID != cursor.QualifiedNodeID ||
			resolution.StepID != cursor.StepID || resolution.FrameID != cursor.FrameID ||
			!sameHandoffCallPath(resolution.CallPath, cursor.CallPath) {
			continue
		}
		for _, dispatch := range dispatches {
			if dispatch != nil && dispatch.Status == engine.DispatchStatusSettled &&
				dispatch.QualifiedNodeID == cursor.QualifiedNodeID && dispatch.FrameID == cursor.FrameID {
				return true
			}
		}
	}
	return false
}

func dispatchIntentIdentityDigest(dispatch engine.DispatchState) string {
	return session.DigestJSON(struct {
		SchemaVersion      string                  `json:"schema_version"`
		OccurrenceID       string                  `json:"occurrence_id"`
		QualifiedNodeID    string                  `json:"qualified_node_id"`
		CallPath           []engine.DebugCallFrame `json:"call_path"`
		StepID             string                  `json:"step_id"`
		FrameID            string                  `json:"frame_id"`
		FrameStepIndex     int                     `json:"frame_step_index"`
		Phase              engine.ExecutionPhase   `json:"phase"`
		Invocation         int                     `json:"invocation"`
		RetryAttempt       int                     `json:"retry_attempt"`
		OccurrenceSequence int64                   `json:"occurrence_sequence"`
		Classification     string                  `json:"classification"`
		EndpointIdentity   string                  `json:"endpoint_identity"`
		RequestDigest      string                  `json:"request_digest"`
		IdempotencyKey     string                  `json:"idempotency_key"`
	}{
		dispatch.SchemaVersion, dispatch.OccurrenceID, dispatch.QualifiedNodeID, dispatch.CallPath,
		dispatch.StepID, dispatch.FrameID, dispatch.FrameStepIndex, dispatch.Phase,
		dispatch.Invocation, dispatch.RetryAttempt, dispatch.OccurrenceSequence,
		dispatch.Classification, dispatch.EndpointIdentity, dispatch.RequestDigest, dispatch.IdempotencyKey,
	})
}

func dynamicResolutionIdentityDigest(resolution engine.DynamicIncludeResolutionState) string {
	var closure struct {
		Digest string `json:"closure_digest"`
	}
	if err := json.Unmarshal(resolution.Pin.ExecutableClosure, &closure); err != nil {
		closure.Digest = session.DigestBytes(resolution.Pin.ExecutableClosure)
	}
	return session.DigestJSON(struct {
		SchemaVersion string                  `json:"schema_version"`
		ResolutionID  string                  `json:"resolution_id"`
		QualifiedNode string                  `json:"qualified_node_id"`
		CallPath      []engine.DebugCallFrame `json:"call_path"`
		StepID        string                  `json:"step_id"`
		FrameID       string                  `json:"frame_id"`
		FrameIndex    int                     `json:"frame_step_index"`
		Invocation    int                     `json:"invocation"`
		Revision      int64                   `json:"revision"`
		Pin           dynamicPinIdentity      `json:"pin"`
	}{
		resolution.SchemaVersion, resolution.ResolutionID, resolution.QualifiedNodeID,
		resolution.CallPath, resolution.StepID, resolution.FrameID,
		resolution.FrameStepIndex, resolution.Invocation, resolution.Revision, dynamicPinIdentity{
			StepID: resolution.Pin.StepID, RenderedRef: resolution.Pin.RenderedRef,
			QualifiedID: resolution.Pin.QualifiedID, AbsPath: resolution.Pin.AbsPath,
			StructuralPath: resolution.Pin.StructuralPath,
			RunbookID:      resolution.Pin.RunbookID, RunbookName: resolution.Pin.RunbookName,
			RunbookContentHash: resolution.Pin.RunbookContentHash,
			PackageName:        resolution.Pin.PackageName, PackageVersion: resolution.Pin.PackageVersion,
			FileDigest: resolution.Pin.FileDigest, PackageDigest: resolution.Pin.PackageDigest,
			ClosureDigest: closure.Digest, ResolvedInputs: resolution.Pin.ResolvedInputs, ResolvedBindings: resolution.Pin.ResolvedBindings,
			ResolvedOutputs: resolution.Pin.ResolvedOutputs, ResolvedGovernance: resolution.Pin.ResolvedGovernance,
		},
	})
}

type dynamicPinIdentity struct {
	StepID             string                               `json:"step_id"`
	RenderedRef        string                               `json:"rendered_ref"`
	QualifiedID        string                               `json:"qualified_id"`
	RunbookID          string                               `json:"runbook_id"`
	RunbookName        string                               `json:"runbook_name"`
	RunbookContentHash string                               `json:"runbook_content_hash"`
	AbsPath            string                               `json:"abs_path"`
	PackageName        string                               `json:"package_name"`
	PackageVersion     string                               `json:"package_version"`
	FileDigest         string                               `json:"file_digest"`
	PackageDigest      string                               `json:"package_digest"`
	ClosureDigest      string                               `json:"closure_digest"`
	StructuralPath     []schema.DynamicIncludeFrameIdentity `json:"structural_path"`
	ResolvedInputs     map[string]*schema.Input             `json:"resolved_inputs"`
	ResolvedBindings   []schema.Binding                     `json:"resolved_bindings,omitempty"`
	ResolvedOutputs    map[string]*schema.Output            `json:"resolved_outputs"`
	ResolvedGovernance *schema.GovernanceConfig             `json:"resolved_governance"`
}

func revisedPlanStepIndex(
	plan *engine.ExecutionPlan,
	callPath []engine.DebugCallFrame,
	stepID string,
) (int, error) {
	matched := -1
	for index := range plan.Steps {
		if plan.Steps[index].ID != stepID {
			continue
		}
		expectedCallPath, err := handoffPlanStepCallPath(plan, index)
		if err != nil || !sameHandoffCallPath(expectedCallPath, callPath) {
			continue
		}
		if matched >= 0 {
			return -1, errors.New("session coordinator: revised run cursor is ambiguous")
		}
		matched = index
	}
	if matched < 0 {
		return -1, errors.New("session coordinator: revised run cursor is unavailable")
	}
	return matched, nil
}

func collectStateBlobPaths(value any, paths map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == "digest" {
				if digest, ok := nested.(string); ok && strings.HasPrefix(digest, "sha256:") && len(digest) == 71 {
					paths["blobs/"+strings.TrimPrefix(digest, "sha256:")+".json.gz"] = true
				}
			}
			collectStateBlobPaths(nested, paths)
		}
	case []any:
		for _, nested := range typed {
			collectStateBlobPaths(nested, paths)
		}
	}
}

func decodeSessionRunProjection(
	ctx context.Context,
	encoded json.RawMessage,
	load func(string) (json.RawMessage, error),
) (json.RawMessage, error) {
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(encoded, &header); err != nil || header.SchemaVersion != session.RunProjectionSchemaV1 {
		return append(json.RawMessage(nil), encoded...), nil
	}
	var projection sessionRunProjection
	if err := decodeStrictDocument(encoded, &projection); err != nil {
		return nil, fmt.Errorf("session coordinator: decode session run projection: %w", err)
	}
	if projection.RunID == "" || projection.CheckpointSequence < 0 ||
		len(projection.Files) == 0 || len(projection.Files) > maxSessionProjectionFiles {
		return nil, errors.New("session coordinator: invalid session run projection")
	}
	archive := runProjectionArchive{SchemaVersion: "yawr.run-projection/v1", RunID: projection.RunID}
	total := int64(0)
	previousPath := ""
	for _, file := range projection.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file.Path <= previousPath || file.Path == "" || file.Size < 0 ||
			file.Size > maxSessionProjectionBytes-total {
			return nil, errors.New("session coordinator: invalid session run projection file")
		}
		previousPath = file.Path
		data := make([]byte, 0, file.Size)
		for _, digest := range file.ChunkDigests {
			chunkData, err := load(digest)
			if err != nil {
				return nil, fmt.Errorf("session coordinator: load projection chunk %s: %w", digest, err)
			}
			if session.DigestBytes(chunkData) != digest {
				return nil, errors.New("session coordinator: projection chunk digest mismatch")
			}
			var chunk sessionRunProjectionChunk
			if err := decodeStrictDocument(chunkData, &chunk); err != nil ||
				chunk.SchemaVersion != sessionProjectionChunkSchemaV1 || len(chunk.Data) > sessionProjectionChunkBytes {
				return nil, errors.New("session coordinator: invalid projection chunk")
			}
			if int64(len(chunk.Data)) > file.Size-int64(len(data)) {
				return nil, errors.New("session coordinator: projection chunk exceeds declared file size")
			}
			data = append(data, chunk.Data...)
		}
		if int64(len(data)) != file.Size || session.DigestBytes(data) != file.Digest {
			return nil, errors.New("session coordinator: reconstructed projection file does not match manifest")
		}
		total += file.Size
		archive.Files = append(archive.Files, runProjectionArchiveFile{
			Path: file.Path, Digest: file.Digest, Data: data,
		})
	}
	return json.Marshal(archive)
}

func appendTraceEventsToRunProjection(
	encoded json.RawMessage,
	events []engine.Event,
) (json.RawMessage, error) {
	if len(events) == 0 {
		return encoded, nil
	}
	var archive runProjectionArchive
	if err := decodeStrictDocument(encoded, &archive); err != nil {
		return nil, err
	}
	traceIndex := -1
	for index := range archive.Files {
		if archive.Files[index].Path == "trace.jsonl" {
			traceIndex = index
			break
		}
	}
	if traceIndex < 0 {
		archive.Files = append(archive.Files, runProjectionArchiveFile{Path: "trace.jsonl"})
		traceIndex = len(archive.Files) - 1
	}
	records := make(map[int64]runTraceRecord)
	lines := bytes.Split(archive.Files[traceIndex].Data, []byte{'\n'})
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record runTraceRecord
		if err := decodeStrictDocument(line, &record); err != nil || record.Sequence < 1 ||
			record.RunID != archive.RunID || record.EventID == "" || record.Kind == "" || !json.Valid(record.Payload) {
			return nil, errors.New("session coordinator: invalid existing run trace projection")
		}
		if existing, found := records[record.Sequence]; found {
			existingData, _ := json.Marshal(existing)
			recordData, _ := json.Marshal(record)
			if !bytes.Equal(existingData, recordData) {
				return nil, errors.New("session coordinator: conflicting run trace projection sequence")
			}
			continue
		}
		records[record.Sequence] = record
	}
	for _, event := range events {
		payload, err := json.Marshal(event.Payload)
		if err != nil {
			return nil, err
		}
		record := runTraceRecord{
			Sequence: event.Sequence, Timestamp: event.Timestamp, Kind: event.Kind,
			RunID: event.RunID, RunbookID: event.RunbookID, EventID: event.EventID, Payload: payload,
		}
		if record.Sequence < 1 || record.RunID != archive.RunID || record.EventID == "" || record.Kind == "" {
			return nil, errors.New("session coordinator: invalid trace event for run projection")
		}
		if existing, found := records[record.Sequence]; found {
			existingData, _ := json.Marshal(existing)
			recordData, _ := json.Marshal(record)
			if !bytes.Equal(existingData, recordData) {
				return nil, errors.New("session coordinator: conflicting trace event for run projection")
			}
			continue
		}
		records[record.Sequence] = record
	}
	traceData := make([]byte, 0, len(archive.Files[traceIndex].Data))
	sequences := make([]int64, 0, len(records))
	for sequence := range records {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(left, right int) bool { return sequences[left] < sequences[right] })
	for _, sequence := range sequences {
		record := records[sequence]
		encodedRecord, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		traceData = append(traceData, encodedRecord...)
		traceData = append(traceData, '\n')
	}
	archive.Files[traceIndex].Data = traceData
	archive.Files[traceIndex].Digest = session.DigestBytes(traceData)
	sort.Slice(archive.Files, func(left, right int) bool { return archive.Files[left].Path < archive.Files[right].Path })
	return json.Marshal(archive)
}

func sessionProjectionSnapshot(
	encoded json.RawMessage,
	runID string,
	checkpointSequence int64,
) (string, string, error) {
	var projection sessionRunProjection
	if err := decodeStrictDocument(encoded, &projection); err != nil {
		return "", "", err
	}
	if projection.SchemaVersion != session.RunProjectionSchemaV1 || projection.RunID != runID ||
		projection.CheckpointSequence != checkpointSequence {
		return "", "", errors.New("session coordinator: projection does not match execution mutation")
	}
	expectedPath := fmt.Sprintf("snapshots/checkpoint-%020d.json", checkpointSequence)
	for _, file := range projection.Files {
		if file.Path == expectedPath {
			return file.Path, file.Digest, nil
		}
	}
	return "", "", errors.New("session coordinator: projection has no authoritative state snapshot")
}

func runProjectionMatchesArchive(encoded json.RawMessage, runDir string) bool {
	var archive runProjectionArchive
	if err := decodeStrictDocument(encoded, &archive); err != nil || len(archive.Files) == 0 {
		return false
	}
	for _, file := range archive.Files {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Path)))
		if file.Path == "" || filepath.IsAbs(file.Path) || strings.Contains(file.Path, "\\") ||
			clean != file.Path || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return false
		}
		info, err := os.Stat(filepath.Join(runDir, filepath.FromSlash(file.Path)))
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(file.Data)) {
			return false
		}
		data, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(file.Path)))
		if err != nil || !bytes.Equal(data, file.Data) || session.DigestBytes(data) != file.Digest {
			return false
		}
	}
	return true
}

func validateRunProjectionTrace(encoded json.RawMessage, runID string, lastSequence int64) error {
	var archive runProjectionArchive
	if err := decodeStrictDocument(encoded, &archive); err != nil || archive.RunID != runID {
		return errors.New("session coordinator: invalid authoritative trace archive")
	}
	for _, file := range archive.Files {
		if file.Path != "trace.jsonl" {
			continue
		}
		lines := bytes.Split(file.Data, []byte{'\n'})
		nextSequence := int64(1)
		for _, line := range lines {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var record runTraceRecord
			if err := decodeStrictDocument(line, &record); err != nil || record.RunID != runID ||
				record.Sequence != nextSequence || record.EventID == "" || record.Kind == "" ||
				!json.Valid(record.Payload) {
				return errors.New("session coordinator: authoritative run trace is not contiguous")
			}
			nextSequence++
		}
		if nextSequence-1 != lastSequence {
			return errors.New("session coordinator: authoritative run trace has the wrong final sequence")
		}
		return nil
	}
	if lastSequence != 0 {
		return errors.New("session coordinator: authoritative run trace is missing")
	}
	return nil
}

func decodeStrictDocument(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON")
		}
		return err
	}
	return nil
}
