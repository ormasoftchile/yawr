package serve

import (
	"context"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

// RunEntry tracks an active or completed run.
type RunEntry struct {
	ID          string
	RunbookPath string
	Handle      engine.RunHandle
	Cancel      context.CancelFunc
	State       engine.RunStatus
	StartedAt   time.Time
	CompletedAt time.Time

	previewReadWindowStart time.Time
	previewReadWindowCount int
}

// RunRegistry stores active and completed runs in memory.
type RunRegistry struct {
	mu      sync.RWMutex
	entries map[string]*RunEntry
}

// NewRunRegistry constructs an empty registry.
func NewRunRegistry() *RunRegistry {
	return &RunRegistry{
		entries: make(map[string]*RunEntry),
	}
}

// Add registers a run entry.
func (r *RunRegistry) Add(entry *RunEntry) {
	if entry == nil {
		return
	}
	r.mu.Lock()
	r.entries[entry.ID] = entry
	r.mu.Unlock()
}

// Get returns a copy of the run entry for runID.
func (r *RunRegistry) Get(runID string) (RunEntry, bool) {
	r.mu.RLock()
	entry, ok := r.entries[runID]
	if !ok {
		r.mu.RUnlock()
		return RunEntry{}, false
	}
	copy := *entry
	r.mu.RUnlock()
	return copy, true
}

// WithEntry executes fn with the live entry while holding the registry lock.
func (r *RunRegistry) WithEntry(runID string, fn func(entry *RunEntry)) bool {
	r.mu.Lock()
	entry, ok := r.entries[runID]
	if ok && fn != nil {
		fn(entry)
	}
	r.mu.Unlock()
	return ok
}

// Remove deletes a run entry.
func (r *RunRegistry) Remove(runID string) {
	r.mu.Lock()
	delete(r.entries, runID)
	r.mu.Unlock()
}

const (
	previewReadRateWindow = time.Second
	previewReadRateLimit  = 60
)

// AllowPreviewRead applies a per-run guardrail for hot-looping preview clients.
// Normal clients keep one SSE per stream and revalidate documents every 3s;
// anything near 100+ read requests/sec is treated as a faulty client.
func (r *RunRegistry) AllowPreviewRead(runID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[runID]
	if !ok {
		return false
	}
	if entry.previewReadWindowStart.IsZero() || now.Sub(entry.previewReadWindowStart) >= previewReadRateWindow {
		entry.previewReadWindowStart = now
		entry.previewReadWindowCount = 1
		return true
	}
	entry.previewReadWindowCount++
	return entry.previewReadWindowCount <= previewReadRateLimit
}

// List returns a snapshot copy of all entries.
func (r *RunRegistry) List() []RunEntry {
	r.mu.RLock()
	entries := make([]RunEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, *entry)
	}
	r.mu.RUnlock()
	return entries
}

// GC removes completed runs older than maxAge and returns the count removed.
func (r *RunRegistry) GC(maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}
	now := time.Now()
	removed := 0
	r.mu.Lock()
	for id, entry := range r.entries {
		if entry.CompletedAt.IsZero() {
			continue
		}
		if now.Sub(entry.CompletedAt) > maxAge {
			delete(r.entries, id)
			removed++
		}
	}
	r.mu.Unlock()
	return removed
}
