package serve

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRegistry_ConcurrentAddGet(t *testing.T) {
	reg := NewRunRegistry()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			reg.Add(&RunEntry{ID: strconv.Itoa(idx)})
		}(i)
	}

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = reg.Get(strconv.Itoa(idx))
		}(i)
	}

	wg.Wait()
}

func TestRegistry_GC_RemovesOldRuns(t *testing.T) {
	reg := NewRunRegistry()
	old := time.Now().Add(-2 * time.Hour)
	reg.Add(&RunEntry{ID: "old", CompletedAt: old})
	reg.Add(&RunEntry{ID: "fresh", CompletedAt: time.Now()})
	reg.Add(&RunEntry{ID: "active"})

	removed := reg.GC(time.Hour)
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	if _, ok := reg.Get("old"); ok {
		t.Fatal("expected old run to be removed")
	}
	if _, ok := reg.Get("fresh"); !ok {
		t.Fatal("expected fresh run to remain")
	}
	if _, ok := reg.Get("active"); !ok {
		t.Fatal("expected active run to remain")
	}
}
