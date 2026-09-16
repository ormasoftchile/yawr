package sessionstore

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func validateScopedSegmentArtifact(resolution engine.DynamicIncludeResolutionState, data json.RawMessage) error {
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("sessionstore: invalid segment snapshot: %w", err)
	}
	if resolution.SchemaVersion == engine.DynamicIncludeResolutionStateSchemaV1 {
		if header.SchemaVersion == "execution-plan/v4" {
			return errors.New("sessionstore: scoped segment requires resolution v2")
		}
		return nil
	}
	var snapshot plansnapshot.SnapshotV1
	if err := decodeStrictJSON(data, &snapshot); err != nil {
		return fmt.Errorf("sessionstore: invalid scoped segment snapshot: %w", err)
	}
	plan, err := plansnapshot.Restore(snapshot)
	if err != nil {
		return fmt.Errorf("sessionstore: invalid scoped segment snapshot: %w", err)
	}
	if plan.ToolScopes == nil {
		return errors.New("sessionstore: resolution v2 requires its scoped segment snapshot")
	}
	if err := plansnapshot.ValidateDynamicIncludePin(resolution.Pin, plan.ToolScopes); err != nil {
		return fmt.Errorf("sessionstore: invalid scoped segment pin: %w", err)
	}
	return nil
}
