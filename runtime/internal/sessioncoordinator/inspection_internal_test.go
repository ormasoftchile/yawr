package sessioncoordinator

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

func TestLatestAttemptIDUsesSegmentThenAttemptOrdinal(t *testing.T) {
	manifest := session.Manifest{
		Segments: map[string]session.SegmentRecord{
			"source": {SegmentID: "source", Ordinal: 1},
			"target": {SegmentID: "target", Ordinal: 2},
		},
		Attempts: map[string]session.RunAttemptRecord{
			"source-run": {RunID: "source-run", SegmentID: "source", Ordinal: 1},
			"target-run": {RunID: "target-run", SegmentID: "target", Ordinal: 1},
		},
	}

	if got := latestAttemptID(manifest); got != "target-run" {
		t.Fatalf("latestAttemptID = %q, want target-run", got)
	}
}
