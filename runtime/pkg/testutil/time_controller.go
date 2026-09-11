package testutil

import (
	"sync"
	"time"
)

// fakeTimer is a timer whose deadline is controlled by TimeController.Advance.
type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
	fired    bool
}

// C returns the channel that receives the fire-time when the timer fires.
func (ft *fakeTimer) C() <-chan time.Time {
	return ft.ch
}

// TimeController provides fake time control for deterministic tests.
// Advance() moves time forward, triggering any registered timers whose
// deadline has been reached.
type TimeController struct {
	now    time.Time
	timers []*fakeTimer
	mu     sync.Mutex
}

// NewTimeController returns a TimeController starting at a fixed epoch.
func NewTimeController() *TimeController {
	return &TimeController{
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Now returns the current fake time.
func (tc *TimeController) Now() time.Time {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.now
}

// Advance moves fake time forward by d and fires all timers whose deadline has
// been reached by the new time.
func (tc *TimeController) Advance(d time.Duration) {
	tc.mu.Lock()
	tc.now = tc.now.Add(d)
	now := tc.now
	var toFire []*fakeTimer
	for _, t := range tc.timers {
		if !t.fired && !t.deadline.After(now) {
			t.fired = true
			toFire = append(toFire, t)
		}
	}
	tc.mu.Unlock()

	for _, t := range toFire {
		t.ch <- now
	}
}

// NewTimer returns a fakeTimer that fires when Advance() moves time past deadline.
// The returned timer's channel (C()) will receive the fire-time exactly once.
func (tc *TimeController) NewTimer(d time.Duration) *fakeTimer {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	ft := &fakeTimer{
		deadline: tc.now.Add(d),
		ch:       make(chan time.Time, 1),
	}
	tc.timers = append(tc.timers, ft)
	return ft
}

// Since returns the fake duration elapsed since t.
func (tc *TimeController) Since(t time.Time) time.Duration {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.now.Sub(t)
}
