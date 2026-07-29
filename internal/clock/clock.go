package clock

import (
	"sync"
	"time"
)

// Clock is the only source of time in Coucou. Injecting it makes every
// scheduling decision deterministically testable.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Real returns the system clock.
func Real() Clock { return realClock{} }

// Fake is a manually controlled clock for tests. It is safe for concurrent
// use because the engine reads it from its ticker goroutine.
type Fake struct {
	mu  sync.RWMutex
	now time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{now: t} }

func (f *Fake) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set jumps the clock to t, forward or backward. Use it to simulate suspend
// and resume or an NTP step.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}
