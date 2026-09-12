package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// buildPinIndex retains every dynamic include occurrence in durable revision
// order, keyed by qualified node identity when available.
func buildPinIndex(pins []schema.LockedDynamicInclude) map[string][]schema.LockedDynamicInclude {
	index := make(map[string][]schema.LockedDynamicInclude, len(pins))
	for _, pin := range pins {
		key := pin.QualifiedNodeID
		if key == "" {
			key = pin.StepID
		}
		index[key] = append(index[key], pin)
	}
	for key := range index {
		sort.SliceStable(index[key], func(left, right int) bool {
			leftRevision, rightRevision := index[key][left].Revision, index[key][right].Revision
			return leftRevision > 0 && rightRevision > 0 && leftRevision < rightRevision
		})
	}
	return index
}

// fileDigestSHA256 returns the canonical sha256-prefixed digest used by
// durable dynamic-include pins.
func fileDigestSHA256(path string) (string, error) {
	return pkgcatalog.FileDigest(path)
}

// PinBasedIncludeLoader is a LazyRunbookLoader for replay. It wraps a
// base loader (parser-backed) and, for each path that matches a pin
// record, verifies the on-disk file digest against the recorded value.
// A mismatch emits replay/dynamicIncludeDrift and fails before the base
// loader can observe changed content.
type PinBasedIncludeLoader struct {
	// pinsByPath maps AbsPath → LockedDynamicInclude for O(1) digest lookup.
	pinsByPath  map[string]schema.LockedDynamicInclude
	base        internalexecutor.LazyRunbookLoader
	traceWriter tracepkg.TraceWriter
	runID       string
}

// NewPinBasedIncludeLoader constructs a loader backed by the given base
// loader and an AbsPath-keyed pin index. traceWriter and runID are used
// to emit replay/dynamicIncludeDrift events when a digest mismatch is
// detected (traceWriter may be nil, in which case drift events are
// silently dropped — useful in tests).
func NewPinBasedIncludeLoader(
	base internalexecutor.LazyRunbookLoader,
	pinsByPath map[string]schema.LockedDynamicInclude,
	traceWriter tracepkg.TraceWriter,
	runID string,
) *PinBasedIncludeLoader {
	return &PinBasedIncludeLoader{
		pinsByPath:  pinsByPath,
		base:        base,
		traceWriter: traceWriter,
		runID:       runID,
	}
}

// buildPinsByPath converts the step-keyed pin index into an AbsPath-keyed
// index so the loader can look up pins by the path it receives.
func buildPinsByPath(pins []schema.LockedDynamicInclude) map[string]schema.LockedDynamicInclude {
	idx := make(map[string]schema.LockedDynamicInclude, len(pins))
	for _, p := range pins {
		idx[p.AbsPath] = p
	}
	return idx
}

// Load loads the runbook at absPath. If absPath is
// recorded in the pin index, the on-disk digest is verified first; a
// mismatch emits replay/dynamicIncludeDrift and fails closed. The current
// catalog is never consulted.
func (l *PinBasedIncludeLoader) Load(ctx context.Context, absPath string) (*internalexecutor.LoadedRunbook, error) {
	if pin, ok := l.pinsByPath[absPath]; ok {
		if len(pin.ExecutableClosure) > 0 {
			flow, err := plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
			if err != nil {
				return nil, fmt.Errorf("replay: restore pinned dynamic include %q: %w", pin.QualifiedID, err)
			}
			return &internalexecutor.LoadedRunbook{
				Flow: flow, Inputs: pin.ResolvedInputs, Outputs: pin.ResolvedOutputs,
				Governance: pin.ResolvedGovernance, ID: pin.RunbookID,
				Name: pin.RunbookName, ContentHash: pin.RunbookContentHash,
			}, nil
		}
		actual, err := fileDigestSHA256(absPath)
		if err != nil {
			return nil, fmt.Errorf("replay: read pinned dynamic include %q: %w", pin.QualifiedID, err)
		}
		if actual != pin.FileDigest {
			l.emitDriftEvent(pin, actual)
			return nil, fmt.Errorf("replay: pinned dynamic include %q digest changed", pin.QualifiedID)
		}
	}
	if l.base == nil {
		return nil, fmt.Errorf("replay: pin-based loader has no base loader for %s", absPath)
	}
	return l.base.Load(ctx, absPath)
}

func (l *PinBasedIncludeLoader) emitDriftEvent(pin schema.LockedDynamicInclude, actualDigest string) {
	if l.traceWriter == nil {
		return
	}
	payload, err := json.Marshal(tracepkg.ReplayDynamicIncludeDriftPayload{
		StepID:         pin.StepID,
		QualifiedID:    pin.QualifiedID,
		ExpectedDigest: pin.FileDigest,
		ActualDigest:   actualDigest,
	})
	if err != nil {
		return
	}
	_ = l.traceWriter.Append(tracepkg.TraceEvent{
		RunID:     l.runID,
		Kind:      tracepkg.EventKindReplayDynamicIncludeDrift,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Payload:   payload,
	})
}
