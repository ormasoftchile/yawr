package engine

import (
	"fmt"
	"time"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func validatePreStartEvents(runID string, events []enginepkg.Event) error {
	ids := make(map[string]bool, len(events))
	for index, event := range events {
		kind := trace.EventKindPackageResolved
		if index == len(events)-1 {
			kind = trace.EventKindCatalogFrozen
		}
		if event.RunID != runID || event.Sequence != int64(index+1) ||
			event.EventID == "" || ids[event.EventID] ||
			event.Kind != string(kind) {
			return fmt.Errorf("%w: invalid pre-start event identity at %d", enginepkg.ErrTraceCommit, index)
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
			return fmt.Errorf("%w: invalid pre-start timestamp: %w", enginepkg.ErrTraceCommit, err)
		}
		ids[event.EventID] = true
	}
	return nil
}
