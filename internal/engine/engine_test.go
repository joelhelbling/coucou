package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/schedule"
	"github.com/joelhelbling/coucou/internal/state"
)

// fakeRunner records what it was asked to run and returns a canned result.
type fakeRunner struct {
	mu       sync.Mutex
	calls    []string
	block    chan struct{} // when non-nil, Run blocks until closed
	outcome  runner.Outcome
	exitCode int
}

func (f *fakeRunner) Run(ctx context.Context, cfg *config.Config, t *config.Task) runner.Result {
	f.mu.Lock()
	f.calls = append(f.calls, t.Name)
	block := f.block
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return runner.Result{Outcome: runner.OutcomeReplaced, ExitCode: -1}
		}
	}
	out := f.outcome
	if out == "" {
		out = runner.OutcomeOK
	}
	return runner.Result{
		StartedAt: time.Now(), FinishedAt: time.Now(),
		Outcome: out, ExitCode: f.exitCode,
	}
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newCfg(t *testing.T, tasks ...config.Task) *config.Config {
	t.Helper()
	cfg := &config.Config{Dir: t.TempDir(), Tasks: tasks}
	for i := range cfg.Tasks {
		if cfg.Tasks[i].OnOverlap == "" {
			cfg.Tasks[i].OnOverlap = config.OverlapSkip
		}
		if cfg.Tasks[i].MissedRuns == "" {
			cfg.Tasks[i].MissedRuns = config.MissedReport
		}
		parsed, err := schedule.Parse(cfg.Tasks[i].Schedule)
		if err != nil {
			t.Fatalf("bad schedule %q in fixture: %v", cfg.Tasks[i].Schedule, err)
		}
		cfg.Tasks[i].Parsed = parsed
	}
	return cfg
}

func TestFiresWhenDue(t *testing.T) {
	now := time.Date(2026, 7, 29, 16, 59, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()

	e.Tick()
	if fr.callCount() != 0 {
		t.Fatal("ran before it was due")
	}

	clk.Advance(time.Minute) // now 17:00
	e.Tick()
	e.Wait()

	if fr.callCount() != 1 {
		t.Errorf("ran %d times, want 1", fr.callCount())
	}
	if st.Get("xkcd").LastRun == nil {
		t.Error("run was not recorded in state")
	}
}

func TestDisabledTaskNeverRuns(t *testing.T) {
	now := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{Name: "x", Command: "true", Schedule: "* * * * *"})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	st.Get("x").Disabled = true

	e := New(cfg, st, fr, clk)
	e.Start()
	clk.Advance(2 * time.Minute)
	e.Tick()
	e.Wait()

	if fr.callCount() != 0 {
		t.Errorf("disabled task ran %d times", fr.callCount())
	}
}

func TestOverlapSkip(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	fr := &fakeRunner{block: make(chan struct{})}
	cfg := newCfg(t, config.Task{
		Name: "slow", Command: "true", Schedule: "* * * * *",
		OnOverlap: config.OverlapSkip,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()

	clk.Advance(time.Minute)
	e.Tick() // starts, and blocks

	clk.Advance(time.Minute)
	e.Tick() // due again while still running

	if got := fr.callCount(); got != 1 {
		t.Errorf("ran %d times, want 1 (the second occurrence must be skipped)", got)
	}

	close(fr.block)
	e.Wait()

	// The skip must be visible in history.
	found := false
	for _, h := range st.Get("slow").History {
		if h.Outcome == string(OutcomeSkipped) {
			found = true
		}
	}
	if !found {
		t.Error("skipped occurrence was not recorded in history")
	}
}

func TestOverlapReplaceKillsTheRunningOne(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	fr := &fakeRunner{block: make(chan struct{})}
	cfg := newCfg(t, config.Task{
		Name: "replaceable", Command: "true", Schedule: "* * * * *",
		OnOverlap: config.OverlapReplace,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()

	clk.Advance(time.Minute)
	e.Tick() // first run blocks

	clk.Advance(time.Minute)
	e.Tick() // must cancel the first and start a second

	deadline := time.After(2 * time.Second)
	for fr.callCount() < 2 {
		select {
		case <-deadline:
			t.Fatalf("replace did not start a second run (calls: %d)", fr.callCount())
		case <-time.After(5 * time.Millisecond):
		}
	}

	close(fr.block)
	e.Wait()
}

func TestMissedRunsCatchUp(t *testing.T) {
	// Last ran yesterday at 17:00; it is now 20:00 the next day, so today's
	// 17:00 was missed.
	last := time.Date(2026, 7, 28, 17, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)

	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
		MissedRuns: config.MissedCatchUp,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	st.Get("xkcd").LastRun = &state.LastRun{FinishedAt: last, Outcome: "ok"}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if fr.callCount() != 1 {
		t.Errorf("catch_up ran %d times, want exactly 1", fr.callCount())
	}
}

func TestMissedRunsCatchUpRunsOnlyOnceForManyMissed(t *testing.T) {
	// Away for a week with a daily task: still exactly one run.
	last := time.Date(2026, 7, 22, 17, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)

	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
		MissedRuns: config.MissedCatchUp,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	st.Get("xkcd").LastRun = &state.LastRun{FinishedAt: last, Outcome: "ok"}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if fr.callCount() != 1 {
		t.Errorf("ran %d times, want 1 (never backfill every occurrence)", fr.callCount())
	}
}

func TestMissedRunsReportMarksOverdueWithoutRunning(t *testing.T) {
	last := time.Date(2026, 7, 28, 17, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)

	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
		MissedRuns: config.MissedReport,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	st.Get("xkcd").LastRun = &state.LastRun{FinishedAt: last, Outcome: "ok"}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if fr.callCount() != 0 {
		t.Errorf("report ran %d times, want 0", fr.callCount())
	}
	if !e.Overdue("xkcd") {
		t.Error("report should mark the task overdue")
	}
}

func TestMissedRunsIgnoreDoesNothing(t *testing.T) {
	last := time.Date(2026, 7, 28, 17, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)

	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
		MissedRuns: config.MissedIgnore,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	st.Get("xkcd").LastRun = &state.LastRun{FinishedAt: last, Outcome: "ok"}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if fr.callCount() != 0 {
		t.Errorf("ignore ran %d times, want 0", fr.callCount())
	}
	if e.Overdue("xkcd") {
		t.Error("ignore should not mark the task overdue")
	}
}

// The policy is not startup-only: a suspend that skips a due time must be
// handled by the same rules.
func TestMissedRunsPolicyAppliesAfterSuspend(t *testing.T) {
	now := time.Date(2026, 7, 29, 16, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "quiet", Command: "true", Schedule: "0 17 * * *",
		MissedRuns: config.MissedIgnore,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()

	// Sleep through 17:00 and wake at 23:00 the same day.
	clk.Set(time.Date(2026, 7, 29, 23, 0, 0, 0, time.UTC))
	e.Tick()
	e.Wait()

	if fr.callCount() != 0 {
		t.Errorf("ran %d times after suspend; ignore must suppress it", fr.callCount())
	}
}

// A task that has never run is not overdue. A fresh config must not fire
// everything the first time it is loaded, even under catch_up.
func TestCatchUpDoesNotFireOnFirstEverLoad(t *testing.T) {
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "brand new", Command: "true", Schedule: "0 17 * * *",
		MissedRuns: config.MissedCatchUp,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if fr.callCount() != 0 {
		t.Errorf("ran %d times; a never-run task must not be treated as overdue",
			fr.callCount())
	}
}

func TestIntervalAnchorsOnFinish(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "poll", Command: "true", Schedule: "@every 30m",
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()

	if got, want := e.NextAt("poll"), now.Add(30*time.Minute); !got.Equal(want) {
		t.Errorf("first NextAt = %v, want %v", got, want)
	}

	clk.Advance(30 * time.Minute)
	e.Tick()
	e.Wait()

	if fr.callCount() != 1 {
		t.Fatalf("ran %d times, want 1", fr.callCount())
	}
	if e.NextAt("poll").Before(clk.Now()) {
		t.Error("next run should be scheduled after the finish time")
	}
}

func TestBackwardClockStepRecomputes(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "x", Command: "true", Schedule: "0 17 * * *",
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()
	before := e.NextAt("x")

	clk.Set(now.Add(-48 * time.Hour))
	e.Tick()

	if after := e.NextAt("x"); !after.Before(before) {
		t.Errorf("NextAt = %v; a backward clock step must recompute it (was %v)",
			after, before)
	}
}

// TestIntervalMissedRunsReportDoesNotLoopBack is the regression test for the
// finding that @every's Next ignores now when prev is non-zero: rescheduling
// with the stale prev left next_at in the past, so report silently behaved
// like catch_up on the very next tick.
func TestIntervalMissedRunsReportDoesNotLoopBack(t *testing.T) {
	last := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC) // 24h ago
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)

	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "poll", Command: "true", Schedule: "@every 30m",
		MissedRuns: config.MissedReport,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	st.Get("poll").LastRun = &state.LastRun{FinishedAt: last, Outcome: "ok"}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if fr.callCount() != 0 {
		t.Errorf("report ran %d times, want 0", fr.callCount())
	}
	if !e.Overdue("poll") {
		t.Error("report should mark the interval task overdue")
	}
	if next := e.NextAt("poll"); !next.After(now) {
		t.Errorf("NextAt = %v, want strictly after now (%v)", next, now)
	}
}

func TestIntervalMissedRunsIgnoreDoesNotLoopBack(t *testing.T) {
	last := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC) // 24h ago
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)

	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "poll", Command: "true", Schedule: "@every 30m",
		MissedRuns: config.MissedIgnore,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	st.Get("poll").LastRun = &state.LastRun{FinishedAt: last, Outcome: "ok"}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if fr.callCount() != 0 {
		t.Errorf("ignore ran %d times, want 0", fr.callCount())
	}
	if e.Overdue("poll") {
		t.Error("ignore should not mark the task overdue")
	}
	if next := e.NextAt("poll"); !next.After(now) {
		t.Errorf("NextAt = %v, want strictly after now (%v)", next, now)
	}
}

func TestIntervalMissedRunsCatchUpStillRunsExactlyOnce(t *testing.T) {
	last := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC) // 24h ago
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)

	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "poll", Command: "true", Schedule: "@every 30m",
		MissedRuns: config.MissedCatchUp,
	})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	st.Get("poll").LastRun = &state.LastRun{FinishedAt: last, Outcome: "ok"}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if fr.callCount() != 1 {
		t.Errorf("catch_up ran %d times, want exactly 1", fr.callCount())
	}
}

// TestStopDoesNotPanicConcurrentTick is the regression test for Stop closing
// e.events while emit could still be sending on it. Run with -race.
func TestStopDoesNotPanicConcurrentTick(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{Name: "x", Command: "true", Schedule: "* * * * *"})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()

	// Drain events so emit's non-blocking send has somewhere to go.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range e.Events() {
		}
	}()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				clk.Advance(time.Second)
				e.Tick()
			}
		}
	}()

	time.Sleep(5 * time.Millisecond)
	e.Stop()
	close(stop)
	wg.Wait()
	<-drainDone
}

// TestCatchUpIsStaggered is the regression test for spec section 2.5:
// several overdue catch_up tasks in one pass must not stampede at once.
func TestCatchUpIsStaggered(t *testing.T) {
	last := time.Date(2026, 7, 28, 17, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)

	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t,
		config.Task{Name: "a", Command: "true", Schedule: "0 17 * * *", MissedRuns: config.MissedCatchUp},
		config.Task{Name: "b", Command: "true", Schedule: "0 17 * * *", MissedRuns: config.MissedCatchUp},
		config.Task{Name: "c", Command: "true", Schedule: "0 17 * * *", MissedRuns: config.MissedCatchUp},
	)
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}
	for _, name := range []string{"a", "b", "c"} {
		st.Get(name).LastRun = &state.LastRun{FinishedAt: last, Outcome: "ok"}
	}

	e := New(cfg, st, fr, clk)
	e.Start()
	e.Tick()
	e.Wait()

	if got := fr.callCount(); got != 1 {
		t.Fatalf("ran %d times on the first tick, want exactly 1", got)
	}

	fr.mu.Lock()
	ranImmediately := fr.calls[0]
	fr.mu.Unlock()

	// The two tasks that did NOT run immediately must be staggered into the
	// near future, spaced roughly 2s apart, rather than left due right now.
	var future []time.Time
	for _, name := range []string{"a", "b", "c"} {
		if name == ranImmediately {
			continue
		}
		future = append(future, e.NextAt(name))
	}
	if len(future) != 2 {
		t.Fatalf("expected 2 staggered tasks, got %d", len(future))
	}
	if future[0].Equal(future[1]) {
		t.Error("staggered tasks should not share the same next_at")
	}
	for _, next := range future {
		if !next.After(now) {
			t.Errorf("staggered next_at %v should be after now %v", next, now)
		}
		if next.Before(now.Add(1 * time.Second)) {
			t.Errorf("staggered next_at %v too close to now %v", next, now)
		}
		if next.After(now.Add(10 * time.Second)) {
			t.Errorf("staggered next_at %v too far from now %v", next, now)
		}
	}

	// Advancing past the stagger window lets the rest run.
	clk.Advance(10 * time.Second)
	e.Tick()
	e.Wait()

	if got := fr.callCount(); got != 3 {
		t.Errorf("after advancing past the stagger window, ran %d times, want 3", got)
	}
}

func TestEventsAreEmitted(t *testing.T) {
	now := time.Date(2026, 7, 29, 16, 59, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{Name: "e", Command: "true", Schedule: "0 17 * * *"})
	st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

	e := New(cfg, st, fr, clk)
	e.Start()

	var got []EventKind
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range e.Events() {
			got = append(got, ev.Kind)
			if ev.Kind == EventFinished {
				return
			}
		}
	}()

	clk.Advance(time.Minute)
	e.Tick()
	e.Wait()
	wg.Wait()

	if len(got) < 2 || got[0] != EventStarted || got[len(got)-1] != EventFinished {
		t.Errorf("events = %v, want started then finished", got)
	}
}
