package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFakeAdvanceAndSet(t *testing.T) {
	base := time.Date(2026, 7, 29, 14, 32, 0, 0, time.UTC)
	f := NewFake(base)

	if !f.Now().Equal(base) {
		t.Fatalf("Now() = %v, want %v", f.Now(), base)
	}

	f.Advance(90 * time.Second)
	if want := base.Add(90 * time.Second); !f.Now().Equal(want) {
		t.Errorf("after Advance, Now() = %v, want %v", f.Now(), want)
	}

	jump := base.Add(-24 * time.Hour)
	f.Set(jump)
	if !f.Now().Equal(jump) {
		t.Errorf("after Set, Now() = %v, want %v", f.Now(), jump)
	}
}

func TestFakeIsRaceSafe(t *testing.T) {
	f := NewFake(time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); f.Advance(time.Second) }()
		go func() { defer wg.Done(); _ = f.Now() }()
	}
	wg.Wait()
}

func TestRealClockAdvances(t *testing.T) {
	c := Real()
	first := c.Now()
	if first.IsZero() {
		t.Error("real clock returned the zero time")
	}
}
