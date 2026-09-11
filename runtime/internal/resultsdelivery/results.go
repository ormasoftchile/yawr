// Package resultsdelivery authorizes complete canonical publications, never previews.
package resultsdelivery

import (
	"encoding/json"

	"github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

const ChunkBytes = 64 << 10

type Unavailable struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type Reference struct {
	SchemaVersion string `json:"schema_version"`
	PublicationID string `json:"publication_id"`
	Digest        string `json:"digest"`
	TotalBytes    int    `json:"total_bytes"`
}

func Missing(reason string) *Unavailable {
	return &Unavailable{Status: "unavailable", Reason: reason}
}

func Prepare(record *engine.RunResults, cloneErr error, status engine.RunStatus, protection engine.DebugProtection) (json.RawMessage, *Unavailable) {
	if cloneErr != nil {
		return nil, Missing("invalid-publication")
	}
	if record == nil {
		return nil, Missing("no-publication")
	}
	for _, output := range record.Outputs {
		if output.Type == "secret" {
			return nil, &Unavailable{Status: "redacted", Reason: "protected-content"}
		}
		if err := executor.ValidateStrictValue(output.Value, output.Type, nil); err != nil {
			return nil, Missing("invalid-publication")
		}
	}
	if status != engine.RunStatusCompleted {
		return nil, Missing("execution-not-completed")
	}
	if err := record.Validate(); err != nil {
		return nil, Missing("invalid-publication")
	}
	body, err := engine.CanonicalResultsJSON(record, true)
	if err != nil {
		return nil, Missing("invalid-publication")
	}
	if err := debugprotect.ValidateHandoffJSON(protection, body); err != nil {
		return nil, &Unavailable{Status: "redacted", Reason: "protected-content"}
	}
	return body, nil
}
