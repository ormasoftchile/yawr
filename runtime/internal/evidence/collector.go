package evidence

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
)

// DefaultCollector implements engine.EvidenceHook.
// It extracts evidence from StepResult.Output following step-kind conventions.
type DefaultCollector struct {
	mu     sync.Mutex
	stores map[string]*AttachmentStore
}

// NewDefaultCollector constructs a DefaultCollector.
func NewDefaultCollector() *DefaultCollector {
	return &DefaultCollector{
		stores: make(map[string]*AttachmentStore),
	}
}

func (c *DefaultCollector) storeFor(runDir string) *AttachmentStore {
	c.mu.Lock()
	defer c.mu.Unlock()
	if store, ok := c.stores[runDir]; ok {
		return store
	}
	store := NewAttachmentStore(runDir)
	c.stores[runDir] = store
	return store
}

// Collect examines the step result and returns evidence records.
func (c *DefaultCollector) Collect(
	ctx context.Context,
	step engine.ResolvedStep,
	result *engine.StepResult,
	runDir string,
) ([]evidencepkg.EvidenceRecord, error) {
	_ = ctx
	_ = step
	if result == nil || result.Output == nil {
		return nil, nil
	}

	var records []evidencepkg.EvidenceRecord
	now := time.Now()

	if stdout, ok := result.Output["stdout"].(string); ok && stdout != "" {
		records = append(records, evidencepkg.EvidenceRecord{
			Name:       "stdout",
			Kind:       evidencepkg.EvidenceKindText,
			Value:      stdout,
			CapturedAt: now,
		})
	}
	if stderr, ok := result.Output["stderr"].(string); ok && stderr != "" {
		records = append(records, evidencepkg.EvidenceRecord{
			Name:       "stderr",
			Kind:       evidencepkg.EvidenceKindText,
			Value:      stderr,
			CapturedAt: now,
		})
	}
	if response, ok := result.Output["response"].(string); ok && response != "" {
		records = append(records, evidencepkg.EvidenceRecord{
			Name:       "tool_response",
			Kind:       evidencepkg.EvidenceKindText,
			Value:      response,
			CapturedAt: now,
		})
	}

	if attachments, ok := result.Output["attachments"].([]any); ok {
		store := c.storeFor(runDir)
		for _, att := range attachments {
			path, ok := att.(string)
			if !ok {
				continue
			}
			rec, err := store.Store(path)
			if err != nil {
				log.Printf("[WARN] evidence: failed to store attachment %s: %v", path, err)
				continue
			}
			rec.CapturedAt = now
			records = append(records, *rec)
		}
	}
	if attachments, ok := result.Output["attachments"].([]string); ok {
		store := c.storeFor(runDir)
		for _, path := range attachments {
			rec, err := store.Store(path)
			if err != nil {
				log.Printf("[WARN] evidence: failed to store attachment %s: %v", path, err)
				continue
			}
			rec.CapturedAt = now
			records = append(records, *rec)
		}
	}

	if checklist, ok := result.Output["checklist"].(map[string]any); ok {
		items := make(map[string]string, len(checklist))
		for k, v := range checklist {
			if s, ok := v.(string); ok {
				items[k] = s
			}
		}
		if len(items) > 0 {
			records = append(records, evidencepkg.EvidenceRecord{
				Name:       "checklist",
				Kind:       evidencepkg.EvidenceKindChecklist,
				Items:      items,
				CapturedAt: now,
			})
		}
	}

	return records, nil
}
