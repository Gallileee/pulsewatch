// Package clock lets the scheduler and the checks read the time through an interface, so tests
// can move time forward by hand instead of sleeping.
package clock

import (
	"sync"
	"time"
)

// Clock is the time source.
type Clock interface {
	Now() time.Time
	// After sends the time on the returned channel once d has passed.
	After(d time.Duration) <-chan time.Time
}

// Real is the system clock.
type Real struct{}

// Now returns the current time.
func (Real) Now() time.Time { return time.Now() }

// After waits for d on the system clock.
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake is a clock that only moves when Advance is called.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
	changed chan struct{} // closed and replaced whenever a waiter is added
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewFake returns a fake clock set to now.
func NewFake(now time.Time) *Fake {
	return &Fake{now: now, changed: make(chan struct{})}
}

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel that receives the fake time once Advance has moved it past d.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, waiter{at: f.now.Add(d), ch: ch})
	close(f.changed)
	f.changed = make(chan struct{})
	return ch
}

// Advance moves the clock forward by d and wakes every waiter whose time has come.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	kept := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.at.After(f.now) {
			w.ch <- f.now
		} else {
			kept = append(kept, w)
		}
	}
	f.waiters = kept
}

// Set moves the clock to t (forward or back) without waking anyone.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

// BlockUntil waits until at least n callers are waiting in After, or the timeout passes.
// It reports whether they are.
func (f *Fake) BlockUntil(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		f.mu.Lock()
		count, changed := len(f.waiters), f.changed
		f.mu.Unlock()
		if count >= n {
			return true
		}
		select {
		case <-changed:
		case <-deadline:
			return false
		}
	}
}
