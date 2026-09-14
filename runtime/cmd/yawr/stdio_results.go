package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/internal/resultsdelivery"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

func stdioExecutionError(message string) map[string]any {
	bounded := runstate.PreviewEventPayload(map[string]any{"error": message})
	text, _ := bounded["error"].(string)
	result := map[string]any{"code": "execution-failed", "message": text}
	if text != message {
		result["messageTruncated"] = true
	}
	return result
}

func (p *stdioProtocol) sendFinished(frame map[string]any, state engine.RunState) error {
	frame["version"] = stdioProtocolVersion
	result := state.StepResults[state.CurrentStep]
	if state.Results != nil && state.Results.Origin.FrameID == "" {
		result = state.StepResults[state.Results.Origin.NodeID]
	}
	if state.Status == engine.RunStatusCompleted &&
		result != nil && result.Status == engine.StepStatusCompleted && result.Output["terminal"] == true {
		for _, name := range []string{"outcome_category", "outcome_code"} {
			if value, exists := result.Output[name]; exists {
				frame[name] = value
			}
		}
	}
	safe, err := p.sanitizeFrame(frame)
	if err != nil {
		return err
	}
	p.mu.RLock()
	protection := p.protect
	p.mu.RUnlock()
	record, cloneErr := state.CloneResults()
	body, unavailable := resultsdelivery.Prepare(record, cloneErr, state.Status, protection)
	if unavailable != nil {
		safe["results"] = nil
		safe["results_unavailable"] = unavailable
		return p.sendResultsFrame(safe)
	}
	base, err := json.Marshal(safe)
	if err != nil {
		return err
	}
	// Adding one member costs its comma, quoted key and colon; no giant trial frame.
	if len(base)+len(`,"results":`)+len(body)+1 <= maxStdioFrameBytes {
		safe["results"] = body
		return p.sendResultsFrame(safe)
	}
	safe["results_ref"] = resultsdelivery.Reference{SchemaVersion: record.SchemaVersion,
		PublicationID: record.PublicationID, Digest: record.Digest, TotalBytes: len(body)}
	terminal, err := json.Marshal(safe)
	if err != nil {
		return err
	}
	if len(terminal)+1 > maxStdioFrameBytes {
		return fmt.Errorf("stdio results terminal exceeds frame budget")
	}
	for offset := 0; offset < len(body); offset += resultsdelivery.ChunkBytes {
		end := min(offset+resultsdelivery.ChunkBytes, len(body))
		chunk := map[string]any{"version": stdioProtocolVersion, "type": "run.results.chunk", "runID": state.RunID,
			"publicationID": record.PublicationID, "digest": record.Digest, "offset": offset, "totalBytes": len(body),
			"data": base64.StdEncoding.EncodeToString(body[offset:end])}
		if err := p.sendResultsFrame(chunk); err != nil {
			return fmt.Errorf("stdio results delivery failed at byte %d (publication remains durable): %w", offset, err)
		}
	}
	return p.sendResultsFrame(safe)
}

// Only prevalidated canonical content reaches this writer. Redacting base64 or
// record values would falsely retain the original publication's digest.
func (p *stdioProtocol) sendResultsFrame(frame map[string]any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.terminalSent {
		return fmt.Errorf("stdio results delivery after terminal")
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(encoded)+1 > maxStdioFrameBytes {
		return fmt.Errorf("stdio protocol frame exceeds %d bytes", maxStdioFrameBytes)
	}
	if err := p.encoder.Encode(json.RawMessage(encoded)); err != nil {
		return err
	}
	if frame["type"] == "run.finished" {
		p.terminalSent = true
	}
	return nil
}
