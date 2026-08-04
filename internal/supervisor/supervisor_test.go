package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/engine"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/schedule"
	"github.com/joelhelbling/coucou/internal/state"
)

// fakeRunner records what it was asked to run and returns a canned result.
// The engine package has its own unexported copy; duplicating it here keeps
// the two packages' tests uncoupled.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	block chan struct{} // when non-nil, Run blocks until closed or ctx done
	ctxs  []context.Context
}

func (f *fakeRunner) Run(ctx context.Context, cfg *config.Config, t *config.Task) runner.Result {
	f.mu.Lock()
	f.calls = append(f.calls, t.Name)
	f.ctxs = append(f.ctxs, ctx)
	block := f.block
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return runner.Result{Outcome: runner.OutcomeReplaced, ExitCode: -1}
		}
	}
	return runner.Result{
		StartedAt: time.Now(), FinishedAt: time.Now(),
		Outcome: runner.OutcomeOK,
	}
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newCfg builds an in-memory config with a real temp directory. StateDir is
// set explicitly: config.Load would default it to ".coucou", but a
// hand-built Config leaves it empty, which would put the lock and state file
// directly in Dir.
func newCfg(t *testing.T, tasks ...config.Task) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Dir:      dir,
		Path:     filepath.Join(dir, ".coucou.yaml"),
		StateDir: ".coucou",
		Tasks:    tasks,
	}
	for i := range cfg.Tasks {
		if cfg.Tasks[i].OnOverlap == "" {
			cfg.Tasks[i].OnOverlap = config.OverlapSkip
		}
		if cfg.Tasks[i].MissedRuns == "" {
			cfg.Tasks[i].MissedRuns = config.MissedReport
		}
		// config.applyDefaults never runs on a hand-built Config, and the
		// real runner execs Shell directly with no fallback, so without this
		// the fixture emits a Config that fails with "exec: no command".
		if cfg.Tasks[i].Shell == "" {
			cfg.Tasks[i].Shell = "/bin/sh"
		}
		parsed, err := schedule.Parse(cfg.Tasks[i].Schedule)
		if err != nil {
			t.Fatalf("bad schedule %q in fixture: %v", cfg.Tasks[i].Schedule, err)
		}
		cfg.Tasks[i].Parsed = parsed
	}
	return cfg
}

// recordingRunner wraps a fakeRunner to note when a run begins, so a test
// can assert ordering against the observe callback.
type recordingRunner struct {
	fake  *fakeRunner
	onRun func()
}

func (r *recordingRunner) Run(ctx context.Context, cfg *config.Config, t *config.Task) runner.Result {
	r.onRun()
	return r.fake.Run(ctx, cfg, t)
}

func TestObserveFiresBeforeFirstTick(t *testing.T) {
	// One minute before the fire time: engine.Start computes next_at with
	// schedule.Next, which is strictly-after, so a clock already sitting on
	// 17:00 would schedule tomorrow and the task would never run — leaving
	// the order slice with only "observe" and failing the length check.
	clk := clock.NewFake(time.Date(2026, 7, 31, 16, 59, 0, 0, time.UTC))
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})

	var mu sync.Mutex
	var order []string
	observed := make(chan *engine.Engine, 1)

	fr2 := &recordingRunner{fake: fr, onRun: func() {
		mu.Lock()
		order = append(order, "run")
		mu.Unlock()
	}}

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg, Clock: clk, Runner: fr2, Ticks: ticks,
		}, func(e *engine.Engine) {
			mu.Lock()
			order = append(order, "observe")
			mu.Unlock()
			observed <- e
		})
	}()

	var eng *engine.Engine
	select {
	case eng = <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("observe was never called")
	}
	if eng == nil {
		t.Fatal("observe received a nil engine")
	}
	// The engine is started, so next_at is already computed.
	if eng.NextAt("xkcd").IsZero() {
		t.Error("observe fired before engine.Start computed next_at")
	}

	clk.Advance(time.Minute) // now 17:00 — the task is due
	ticks <- time.Time{}
	ticks <- time.Time{}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "observe" {
		t.Errorf("order = %v, want observe first", order)
	}
}

func TestNilObserveIsFine(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg,
			Clock:  clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)),
			Runner: &fakeRunner{},
			Ticks:  ticks,
		}, nil)
	}()
	ticks <- time.Time{}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run with nil observe: %v", err)
	}
}

func TestTickDrivesRuns(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 31, 16, 59, 0, 0, time.UTC))
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg, Clock: clk, Runner: fr, Ticks: ticks,
		}, nil)
	}()

	// Not due yet.
	ticks <- time.Time{}
	if fr.callCount() != 0 {
		t.Fatalf("ran before it was due")
	}

	clk.Advance(time.Minute) // now 17:00
	ticks <- time.Time{}
	// The next send is only accepted once the previous Tick returned, so
	// the dispatch has happened by now; the run itself is a goroutine.
	ticks <- time.Time{}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fr.callCount() != 1 {
		t.Errorf("ran %d times, want 1", fr.callCount())
	}

	// Run deliberately performs no final state.Save, on the stated grounds
	// that the engine saves after every run. Nothing else asserts that, so
	// this pins it: if the per-run save is ever dropped, the session would
	// lose its entire history on exit and only this check would notice.
	st, err := state.Load(cfg.StateDirPath())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if st.Get("xkcd").LastRun == nil {
		t.Error("the run was not durably saved; state.json has no LastRun")
	}
}

// TestObserveSeesStartupEvents proves observe's stated purpose: a subscriber
// that grabs Events() inside the callback does not miss events emitted from
// the first tick onward.
func TestObserveSeesStartupEvents(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 31, 16, 59, 0, 0, time.UTC))
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})

	started := make(chan string, 8)
	subscribed := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg, Clock: clk, Runner: &fakeRunner{}, Ticks: ticks,
		}, func(e *engine.Engine) {
			events := e.Events()
			// Drained on its own goroutine: the channel holds 64 events and
			// the engine drops rather than blocks, but reading inline would
			// block Run itself, which observe must never do.
			go func() {
				for ev := range events {
					if ev.Kind == engine.EventStarted {
						select {
						case started <- ev.Task:
						default:
						}
					}
				}
			}()
			close(subscribed)
		})
	}()

	<-subscribed
	clk.Advance(time.Minute) // now 17:00 — due
	ticks <- time.Time{}

	select {
	case name := <-started:
		if name != "xkcd" {
			t.Errorf("EventStarted for %q, want xkcd", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber never saw an EventStarted")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestForceTakesOverALiveLock is the only exercise of Options.Force, the
// branch that deliberately overrides the single-instance interlock.
func TestForceTakesOverALiveLock(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})
	dir := cfg.StateDirPath()

	held, err := state.AcquireLock(dir, cfg.Path, false)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg,
			Clock:  clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)),
			Runner: &fakeRunner{},
			Ticks:  ticks,
			Force:  true,
		}, nil)
	}()

	// A refused Run would have returned before ever reading a tick, so an
	// accepted send is the proof that Force got us into the loop.
	select {
	case ticks <- time.Time{}:
	case err := <-done:
		t.Fatalf("Force did not take over the live lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run never reached its tick loop")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestSecondInstanceIsRefused(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})
	dir := cfg.StateDirPath()

	held, err := state.AcquireLock(dir, cfg.Path, false)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer held.Release()

	fr := &fakeRunner{}
	ticks := make(chan time.Time)
	err = Run(context.Background(), Options{
		Config: cfg,
		Clock:  clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)),
		Runner: fr,
		Ticks:  ticks,
	}, nil)

	if err == nil {
		t.Fatal("second instance was allowed to start")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("error should name the holding pid, got: %v", err)
	}
	if fr.callCount() != 0 {
		t.Error("scheduled a run without holding the lock")
	}
}

func TestStaleLockIsBroken(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})
	dir := cfg.StateDirPath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// PID 0x7FFFFFFF is not a live process on any supported platform.
	stale := `{"pid":2147483647,"started_at":"2020-01-01T00:00:00Z",` +
		`"config_path":"` + cfg.Path + `","token":"deadbeef"}`
	if err := os.WriteFile(filepath.Join(dir, "lock"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg,
			Clock:  clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)),
			Runner: &fakeRunner{},
			Ticks:  ticks,
		}, nil)
	}()

	ticks <- time.Time{} // proves Run got past the lock and into the loop
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run should have broken the stale lock, got: %v", err)
	}
}

func TestLockIsReleasedOnExit(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg,
			Clock:  clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)),
			Runner: &fakeRunner{},
			Ticks:  ticks,
		}, nil)
	}()

	ticks <- time.Time{}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cfg.StateDirPath(), "lock")); !os.IsNotExist(err) {
		t.Error("lock file survived a clean shutdown")
	}
}

// TestLockIsReleasedWhenStartupFails forces a failure *after* the lock is
// taken, so the deferred Release is the only thing that can clean up. A
// directory where state.json belongs makes os.ReadFile fail with EISDIR,
// which state.Load surfaces rather than swallowing (it only recovers from
// unparseable *content*).
func TestLockIsReleasedWhenStartupFails(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})
	dir := cfg.StateDirPath()
	if err := os.MkdirAll(filepath.Join(dir, "state.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	fr := &fakeRunner{}
	err := Run(context.Background(), Options{
		Config: cfg,
		Clock:  clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)),
		Runner: fr,
		Ticks:  make(chan time.Time),
	}, nil)

	if err == nil {
		t.Fatal("Run should have failed to load state")
	}
	if fr.callCount() != 0 {
		t.Error("scheduled a run despite failing startup")
	}
	if _, serr := os.Stat(filepath.Join(dir, "lock")); !os.IsNotExist(serr) {
		t.Error("lock leaked after a failed startup")
	}
}

func TestCancelledBeforeStartTakesNoLock(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	fr := &fakeRunner{}
	err := Run(ctx, Options{
		Config: cfg,
		Clock:  clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)),
		Runner: fr,
		Ticks:  make(chan time.Time),
	}, nil)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if fr.callCount() != 0 {
		t.Error("ran a task despite being cancelled before start")
	}
	if _, serr := os.Stat(filepath.Join(cfg.StateDirPath(), "lock")); !os.IsNotExist(serr) {
		t.Error("took a lock despite being cancelled before start")
	}
}

func TestCancelTerminatesInFlightRun(t *testing.T) {
	// Started a minute before the due time: engine.Start computes next_at
	// strictly after "now" and never treats a task that has never run as
	// already overdue, so starting the fake clock exactly on the due
	// instant would push next_at to the following day and the task would
	// never dispatch. Advancing to the due instant afterward, as
	// TestTickDrivesRuns does, is what actually triggers it.
	clk := clock.NewFake(time.Date(2026, 7, 31, 16, 59, 0, 0, time.UTC))
	// block is never closed: the only way this run ends is ctx cancellation.
	fr := &fakeRunner{block: make(chan struct{})}
	cfg := newCfg(t, config.Task{
		Name: "slow", Command: "sleep 999", Schedule: "0 17 * * *",
	})

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg, Clock: clk, Runner: fr, Ticks: ticks,
		}, nil)
	}()

	ticks <- time.Time{} // not due yet
	clk.Advance(time.Minute)
	ticks <- time.Time{} // due now, dispatches
	ticks <- time.Time{} // accepted only after the previous Tick returned

	// Wait for the run to actually be inside fakeRunner.Run.
	deadline := time.Now().Add(2 * time.Second)
	for fr.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("run never started")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return; Stop is not terminating the in-flight run")
	}

	fr.mu.Lock()
	runCtx := fr.ctxs[0]
	fr.mu.Unlock()
	if runCtx.Err() == nil {
		t.Error("the run's context was not cancelled")
	}
}

func TestClosedTickChannelExitsCleanly(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})

	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Config: cfg,
			Clock:  clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)),
			Runner: &fakeRunner{},
			Ticks:  ticks,
		}, nil)
	}()

	ticks <- time.Time{}
	close(ticks)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run spun on a closed tick channel instead of returning")
	}

	if _, err := os.Stat(filepath.Join(cfg.StateDirPath(), "lock")); !os.IsNotExist(err) {
		t.Error("lock survived shutdown via closed tick channel")
	}
}

func TestSweepRemovesOldOrphanTemp(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)
	orphan := filepath.Join(dir, "lock.tmp.deadbeef")
	if err := os.WriteFile(orphan, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * time.Minute)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	sweepOrphanTemps(dir, now)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("old orphan temp file survived the sweep")
	}
}

func TestSweepSparesFreshTemp(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)
	fresh := filepath.Join(dir, "lock.tmp.cafebabe")
	if err := os.WriteFile(fresh, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	recent := now.Add(-time.Second)
	if err := os.Chtimes(fresh, recent, recent); err != nil {
		t.Fatal(err)
	}

	sweepOrphanTemps(dir, now)

	if _, err := os.Stat(fresh); err != nil {
		t.Error("sweep deleted a temp file that could belong to a live acquirer")
	}
}

func TestSweepSparesLockAndMutex(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)
	old := now.Add(-10 * time.Minute)

	for _, name := range []string{"lock", "lock.mutex", "state.json"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	sweepOrphanTemps(dir, now)

	for _, name := range []string{"lock", "lock.mutex", "state.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("sweep deleted %s, which it must never touch", name)
		}
	}
}

// TestSweepHandlesGlobMetacharsInDirName guards the reason the sweep scans
// the directory instead of globbing it: filepath.Glob treats the directory
// portion as a pattern too, so an unterminated [ in a real config path
// returned ErrBadPattern and disabled the sweep entirely.
func TestSweepHandlesGlobMetacharsInDirName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj[1-9]*?")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)
	orphan := filepath.Join(dir, "lock.tmp.deadbeef")
	if err := os.WriteFile(orphan, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * time.Minute)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	sweepOrphanTemps(dir, now)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan survived the sweep in a directory containing glob metacharacters")
	}
}

func TestRunSweepsOnStartup(t *testing.T) {
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})
	dir := cfg.StateDirPath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC)
	orphan := filepath.Join(dir, "lock.tmp.deadbeef")
	if err := os.WriteFile(orphan, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * time.Minute)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg, Clock: clock.NewFake(now),
			Runner: &fakeRunner{}, Ticks: ticks,
		}, nil)
	}()
	ticks <- time.Time{}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("Run did not sweep the orphan on startup")
	}
}

// TestDefaultsRunARealTask exercises the production wiring: real clock,
// real ticker, real runner, real subprocess. Every other test injects
// fakes, so without this nothing would catch time.NewTicker or
// runner.New being hooked up wrong.
func TestDefaultsRunARealTask(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real wall-clock time")
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "fired")
	cfg := newCfg(t, config.Task{
		Name:     "touch",
		Command:  "touch " + marker,
		Schedule: "@every 1s",
	})
	cfg.Dir = dir
	cfg.Path = filepath.Join(dir, ".coucou.yaml")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: cfg}, nil)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("task never fired with default (real) clock, ticker, and runner")
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
