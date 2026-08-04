# Coucou Supervisor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `internal/supervisor`, the package that holds the instance lock, drives the engine's 1Hz tick, and shuts both down cleanly — the wiring that makes Coucou actually run tasks on a schedule.

**Architecture:** One blocking function, `supervisor.Run(ctx, opts, observe)`. It acquires the lock, constructs and starts an engine, ticks it until the context is cancelled, then stops the engine and releases the lock. Clock, runner, and tick source are all injected so every test is deterministic. Signals are handled by callers, not here.

**Tech Stack:** Go 1.26.2, standard library only. No new dependencies.

## Global Constraints

- Design spec: `docs/superpowers/specs/2026-07-30-coucou-supervisor-design.md`
- Go version: 1.26.2 (pinned in `.go-version`)
- No new third-party dependencies. Standard library only.
- No new user-facing CLI command or flag. `internal/cli` is not modified by this plan.
- Config hot-reload is out of scope.
- The whole `supervisor` package must pass `go test -race ./internal/supervisor/...`.
- Run `task check` before every commit (build, test, vet, gofmt).
- Existing test helper names in `internal/engine/engine_test.go` (`fakeRunner`, `newCfg`) are unexported. `supervisor` defines its own; do not export the engine's.

---

### Task 1: Fix the `dispatch` / `Stop` WaitGroup race in the engine

`dispatch` releases `e.mu` before calling `wg.Add(1)`. `Stop()` cancels under the lock, unlocks, then `wg.Wait()`s. A `Tick` caught between those two points means `Wait` sees a zero counter and returns, so `Stop()` reports that everything finished while a run is starting. Go may also panic with "WaitGroup is reused before previous Wait returned".

This must land first: the supervisor's entire purpose is owning clean shutdown, and every later task builds on `Stop()` being trustworthy.

**Files:**
- Modify: `internal/engine/engine.go:268-291` (the `dispatch` function)
- Test: `internal/engine/engine_test.go` (append)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: no signature changes. `(*engine.Engine).Stop()` keeps its exact behavior contract but becomes correct under concurrency. Later tasks rely on `Stop()` genuinely waiting for all in-flight runs.

- [ ] **Step 1: Write the failing test**

Append to `internal/engine/engine_test.go`:

```go
// TestStopDoesNotReturnWhileARunIsStarting hammers the window between
// dispatch releasing e.mu and dispatch calling wg.Add(1). If Stop's
// wg.Wait() observes a zero counter inside that window it returns early,
// and a run then starts with nothing waiting on it.
//
// This is probabilistic by nature. The primary signal is the race detector;
// the callCount assertion catches the coarser "run started after Stop
// returned" case.
func TestStopDoesNotReturnWhileARunIsStarting(t *testing.T) {
	for i := 0; i < 200; i++ {
		// Start the clock one minute BEFORE the fire time. Start computes
		// next_at with schedule.Next, which returns the first instant
		// strictly after now — so a clock already sitting on 17:00 would
		// schedule tomorrow's 17:00 and the task would never be due.
		clk := clock.NewFake(time.Date(2026, 7, 31, 16, 59, 0, 0, time.UTC))
		fr := &fakeRunner{}
		cfg := newCfg(t, config.Task{
			Name: "x", Command: "true", Schedule: "0 17 * * *",
		})
		st := &state.State{Version: state.Version, Tasks: map[string]*state.TaskState{}}

		e := New(cfg, st, fr, clk)
		e.Start()
		clk.Advance(time.Minute) // now 17:00 — the task is due

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); e.Tick() }()
		go func() { defer wg.Done(); e.Stop() }()
		wg.Wait()

		settled := fr.callCount()
		time.Sleep(2 * time.Millisecond)
		if fr.callCount() != settled {
			t.Fatalf("iteration %d: a run started after Stop returned", i)
		}
	}
}
```

- [ ] **Step 2: Run the test and confirm it FAILS**

Run: `go test -race ./internal/engine/ -run TestStopDoesNotReturnWhileARunIsStarting -v`

Expected: FAIL. Either a `DATA RACE` report naming `sync.WaitGroup`, a panic reading `WaitGroup is reused before previous Wait returned`, or the `a run started after Stop returned` assertion.

**Do not proceed until you have seen it fail.** If it passes, increase the iteration count to 1000 and re-run. A race fix nobody watched fail first is a guess. If it still passes at 1000, stop and report that — do not apply the fix and claim success.

- [ ] **Step 3a: Add a `stopping` flag and set it at the top of `Stop`**

Moving `wg.Add(1)` under the mutex is necessary but not sufficient. `Stop` sets
`e.stopped` only *after* `wg.Wait()` returns, so a `stopped` check inside
`dispatch` would be dead code during the exact window it is meant to guard: if
`Stop` releases the mutex and `dispatch` acquires it before `Wait` runs, the run
still starts.

`stopped` cannot simply be moved earlier — `emit` returns early when it is set,
so hoisting it would suppress the `EventFinished` of every run still shutting
down. A separate flag keeps the two concerns apart.

Add the field to the `Engine` struct in `internal/engine/engine.go` (the block
at lines 61-74), directly below `stopped`:

```go
	stopped  bool
	// stopping is set at the top of Stop, before the mutex is released, so
	// dispatch cannot start a new run once shutdown has begun. It is
	// distinct from stopped, which gates emit and is set only after every
	// run has finished; setting stopped this early would swallow the
	// EventFinished of runs still shutting down.
	stopping bool
```

Then change `Stop` (lines 405-418) so the flag is set inside the first critical
section:

```go
func (e *Engine) Stop() {
	e.mu.Lock()
	e.stopping = true
	for _, rt := range e.rt {
		if rt.cancel != nil {
			rt.cancel()
		}
	}
	e.mu.Unlock()
	e.wg.Wait()
	e.mu.Lock()
	e.stopped = true
	close(e.events)
	e.mu.Unlock()
}
```

- [ ] **Step 3b: Apply the `dispatch` fix**

In `internal/engine/engine.go`, replace the opening of `dispatch` (currently lines 268-291):

```go
// dispatch starts a run in its own goroutine.
func (e *Engine) dispatch(t *config.Task, now time.Time) {
	ctx, cancel := context.WithCancel(context.Background())

	e.mu.Lock()
	rt := e.rt[t.Name]
	// Both checks and the wg.Add below must happen under e.mu. Stop sets
	// stopping under the same mutex and only then calls wg.Wait, so once
	// stopping is observed no new run can start, and any wg.Add that beat
	// it is already counted before Wait can see zero. Adding to the
	// WaitGroup after unlocking would let Wait return while this run is
	// still starting.
	if e.stopping || rt == nil || rt.running {
		e.mu.Unlock()
		cancel()
		return
	}
	rt.running = true
	rt.overdue = false
	rt.staggering = false
	rt.cancel = cancel
	e.scheduleNextLocked(t, now)
	e.wg.Add(1)
	e.mu.Unlock()

	e.emit(Event{Kind: EventStarted, Task: t.Name})

	go func() {
		defer e.wg.Done()
		defer cancel()
```

Leave the rest of the goroutine body unchanged. Note that the original `e.wg.Add(1)` line that sat between `e.emit(...)` and `go func() {` must be **removed** — it has moved inside the lock.

- [ ] **Step 4: Run the test and confirm it PASSES**

Run: `go test -race ./internal/engine/ -run TestStopDoesNotReturnWhileARunIsStarting -v`
Expected: PASS.

- [ ] **Step 5: Run the whole engine suite under race**

Run: `go test -race ./internal/engine/ -count=3`
Expected: PASS. The tail of `dispatch` re-dispatches a queued run (`if queued { e.dispatch(...) }`); confirm the new `stopping` check does not break `TestOverlapQueue` or any other overlap test. If one fails, the correct resolution is that a queued re-dispatch after `Stop` should be dropped — that is the intended behavior, so update the test's expectation only if it asserted a post-Stop re-dispatch.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "fix(engine): refuse and count dispatches under the mutex Stop holds

dispatch called wg.Add(1) after unlocking e.mu, so Stop's wg.Wait() could
observe a zero counter and return while a run was still starting. That run
then executed with nothing waiting on it and recorded state after
shutdown; Go could also panic with 'WaitGroup is reused'.

Moving wg.Add under the lock is not enough on its own: Stop set stopped
only after wg.Wait returned, so a gate on it would be dead code during
the very window it guards. Add a separate stopping flag, set at the top
of Stop while the mutex is still held, and check it in dispatch. stopped
stays where it is because emit keys off it, and hoisting it would swallow
the EventFinished of runs still shutting down."
```

---

### Task 2: `supervisor.Run` — lock, engine, tick loop, shutdown

The core deliverable. After this task Coucou can, for the first time, run a task on a schedule.

**Files:**
- Create: `internal/supervisor/supervisor.go`
- Test: `internal/supervisor/supervisor_test.go`

**Interfaces:**
- Consumes: `engine.New(cfg, st, r, clk) *engine.Engine`, `(*engine.Engine).Start()`, `.Tick()`, `.Stop()`; `state.AcquireLock(dir, configPath string, force bool) (*state.Lock, error)`, `(*state.Lock).Release() error`, `state.Load(dir string) (*state.State, error)`; `config.(*Config).StateDirPath() string`; `clock.Real() clock.Clock`; `runner.New(grace time.Duration) runner.Runner`, `runner.DefaultGrace`.
- Produces: `supervisor.Options` struct and `supervisor.Run(ctx context.Context, opts Options, observe func(*engine.Engine)) error`. Tasks 3-7 extend this same function and test file.

- [ ] **Step 1: Write the test helper and the failing test**

Create `internal/supervisor/supervisor_test.go`:

```go
package supervisor

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/schedule"
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
		parsed, err := schedule.Parse(cfg.Tasks[i].Schedule)
		if err != nil {
			t.Fatalf("bad schedule %q in fixture: %v", cfg.Tasks[i].Schedule, err)
		}
		cfg.Tasks[i].Parsed = parsed
	}
	return cfg
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
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/supervisor/ -run TestTickDrivesRuns -v`
Expected: FAIL to build — `undefined: Run`, `undefined: Options`.

- [ ] **Step 3: Write the implementation**

Create `internal/supervisor/supervisor.go`:

```go
// Package supervisor owns one Coucou scheduling session: it holds the
// instance lock, drives the engine's tick, and shuts both down cleanly.
//
// It is not a daemon. The process is foreground and single-instance; the
// TUI will embed Run rather than reimplement it. Signal handling
// deliberately lives in the caller, because signal.Notify is process-global
// and a package that grabs it cannot be tested or reused cleanly. The
// supervisor understands only context cancellation.
package supervisor

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/engine"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/state"
)

// Options configures one scheduling session. Only Config is required; the
// remaining fields exist so tests can drive time, execution, and ticks
// deterministically.
type Options struct {
	Config *config.Config
	Clock  clock.Clock      // defaults to clock.Real()
	Runner runner.Runner    // defaults to runner.New(runner.DefaultGrace)
	Ticks  <-chan time.Time // defaults to a 1Hz ticker
	Force  bool             // break a live lock
}

// Run blocks until ctx is cancelled or Ticks closes, then shuts down and
// returns. observe, when non-nil, is called once with the started engine
// before the first tick, which is the window a TUI needs to subscribe to
// Events() without missing any.
//
// Cancellation before the session starts returns ctx.Err(): nothing ran, so
// it is an abort. Cancellation during the tick loop returns nil: that is
// the documented way to shut down.
func Run(ctx context.Context, opts Options, observe func(*engine.Engine)) (err error) {
	if opts.Config == nil {
		return fmt.Errorf("supervisor: Options.Config is required")
	}

	clk := opts.Clock
	if clk == nil {
		clk = clock.Real()
	}
	r := opts.Runner
	if r == nil {
		r = runner.New(runner.DefaultGrace)
	}
	ticks := opts.Ticks
	if ticks == nil {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		ticks = tk.C
	}

	dir := opts.Config.StateDirPath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create state directory %s: %w", dir, err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	lock, err := state.AcquireLock(dir, opts.Config.Path, opts.Force)
	if err != nil {
		return err
	}
	// Registered before the engine's defer so it runs after it: scheduling
	// must be fully stopped before the lock is handed to the next process.
	defer func() {
		if rerr := lock.Release(); rerr != nil && err == nil {
			err = fmt.Errorf("cannot release lock: %w", rerr)
		}
	}()

	st, err := state.Load(dir)
	if err != nil {
		return err
	}

	eng := engine.New(opts.Config, st, r, clk)
	eng.Start()
	if observe != nil {
		observe(eng)
	}
	// Stop cancels every in-flight run, waits for each to record its
	// outcome, and closes the event channel. No separate Wait is needed,
	// and no final state.Save: the engine saves after every run.
	defer eng.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			// A receive from a closed channel returns immediately and
			// forever. Without this check the select becomes a hot loop
			// pegging a core and calling Tick millions of times.
			if !ok {
				return nil
			}
			eng.Tick()
		}
	}
}
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test -race ./internal/supervisor/ -run TestTickDrivesRuns -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/
git commit -m "feat(supervisor): drive the engine from a tick source

Adds supervisor.Run: acquire the instance lock, start an engine, tick it
until the context is cancelled, then stop it and release. Clock, runner,
and tick source are injected so the whole lifecycle is testable without a
terminal or a real second of wall clock.

Nothing imported internal/engine before this; Coucou could not actually
run anything on a schedule."
```

---

### Task 3: Lock acquisition failure modes

**Files:**
- Test: `internal/supervisor/supervisor_test.go` (append)

**Interfaces:**
- Consumes: `supervisor.Run`, `Options` from Task 2. `state.AcquireLock`, `(*state.Lock).Release`.
- Produces: no new exported surface. Confirms `Run` refuses to schedule without the lock and never leaks one.

- [ ] **Step 1: Write the failing tests**

Append to `internal/supervisor/supervisor_test.go`:

```go
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
```

Add these imports to the existing block in `supervisor_test.go`:

```go
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/joelhelbling/coucou/internal/state"
```

- [ ] **Step 2: Run the tests**

Run: `go test -race ./internal/supervisor/ -run 'TestSecondInstance|TestStaleLock|TestLockIsReleased|TestCancelledBeforeStart' -v`

Expected: PASS. Task 2's implementation already covers all five paths — these tests exist to pin the behavior, not to drive new code.

Two things that may need adjusting rather than "fixing":

- If `TestSecondInstanceIsRefused` fails on the pid assertion, read the actual error text from `state.AcquireLock` and align the assertion with it. Do not change the `state` package to satisfy the test.
- If `TestLockIsReleasedWhenStartupFails` fails because `Run` returned `nil`, check whether `state.Load` really propagates an EISDIR read error. Read `internal/state/state.go:52-80` and confirm. If `Load` swallows it, pick a different post-acquisition failure to force — but do not weaken the test to "startup succeeded".

- [ ] **Step 3: Commit**

```bash
git add internal/supervisor/supervisor_test.go
git commit -m "test(supervisor): pin lock acquisition and release behavior

Second instance refused, stale lock broken, lock released on clean exit
and on a failed startup, and no lock taken when cancelled before start."
```

---

### Task 4: Shutdown terminates in-flight runs; a closed tick channel exits

**Files:**
- Test: `internal/supervisor/supervisor_test.go` (append)

**Interfaces:**
- Consumes: `supervisor.Run`, `Options`, `fakeRunner` (its `block` and `ctxs` fields) from Task 2.
- Produces: no new exported surface.

- [ ] **Step 1: Write the failing tests**

Append to `internal/supervisor/supervisor_test.go`:

```go
func TestCancelTerminatesInFlightRun(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC))
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
```

- [ ] **Step 2: Run the tests**

Run: `go test -race ./internal/supervisor/ -run 'TestCancelTerminates|TestClosedTick' -v -timeout 60s`
Expected: PASS.

If `TestClosedTickChannelExitsCleanly` hangs or the suite pegs a CPU core, the `if !ok` guard is missing from the tick loop — that is exactly the hot-loop failure the design calls out.

- [ ] **Step 3: Commit**

```bash
git add internal/supervisor/supervisor_test.go
git commit -m "test(supervisor): cover shutdown paths

Cancellation terminates an in-flight run via the engine's Stop, and a
closed tick channel exits rather than spinning on an always-ready recv."
```

---

### Task 5: The `observe` callback fires after Start and before the first tick

The TUI's correctness depends on this ordering: subscribing after the first tick would drop the events of any task that fired immediately on startup.

**Files:**
- Test: `internal/supervisor/supervisor_test.go` (append)

**Interfaces:**
- Consumes: `supervisor.Run`'s third parameter, `observe func(*engine.Engine)`.
- Produces: no new exported surface.

- [ ] **Step 1: Write the failing test**

Append to `internal/supervisor/supervisor_test.go`:

```go
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
```

Add this helper above the tests, in the same file:

```go
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
```

Add to the import block:

```go
	"github.com/joelhelbling/coucou/internal/engine"
```

- [ ] **Step 2: Run the tests**

Run: `go test -race ./internal/supervisor/ -run 'TestObserveFires|TestNilObserve' -v`
Expected: PASS — Task 2 already calls `observe` in the right place. These tests lock the ordering contract in so a later refactor cannot quietly move the call.

- [ ] **Step 3: Commit**

```bash
git add internal/supervisor/supervisor_test.go
git commit -m "test(supervisor): pin observe ordering

observe must fire after engine.Start and before the first tick; that is
the window a TUI needs to subscribe without dropping startup events."
```

---

### Task 6: Age-guarded orphan sweep for `lock.tmp.*`

`AcquireLock` writes `lock.tmp.<token>` and links it into place, removing it via `defer` — so a temp file normally lives for microseconds, and only a SIGKILL in that window orphans one. An unguarded sweep would delete a concurrently-acquiring process's temp file between its write and its link, turning a rare orphan into a reproducible `ENOENT` acquisition failure. The age guard is the whole point of this task.

**Files:**
- Modify: `internal/supervisor/supervisor.go`
- Test: `internal/supervisor/supervisor_test.go` (append)

**Interfaces:**
- Consumes: `supervisor.Run` from Task 2.
- Produces: unexported `sweepOrphanTemps(dir string, now time.Time)` and `tmpOrphanAge` constant, both package-private.

- [ ] **Step 1: Write the failing tests**

Append to `internal/supervisor/supervisor_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./internal/supervisor/ -run TestSweep -v`
Expected: FAIL to build — `undefined: sweepOrphanTemps`.

- [ ] **Step 3: Write the implementation**

Add to `internal/supervisor/supervisor.go`, after the `Run` function:

```go
// tmpOrphanAge is how old a lock temp file must be before the sweep will
// remove it. AcquireLock writes lock.tmp.<token> and links it into place,
// removing it via defer, so a temp file normally lives for microseconds and
// only a SIGKILL in that window orphans one. Deleting a temp file belonging
// to a live acquirer would make its os.Link fail with ENOENT, converting a
// rare orphan into a reproducible acquisition failure. A genuine orphan
// persists forever, so waiting a minute costs nothing.
const tmpOrphanAge = time.Minute

// sweepOrphanTemps removes lock temp files left behind by a killed process.
// Failures are ignored: a stray temp file is cosmetic, and refusing to
// schedule because of one would be worse than leaving it.
func sweepOrphanTemps(dir string, now time.Time) {
	matches, err := filepath.Glob(filepath.Join(dir, "lock.tmp.*"))
	if err != nil {
		return
	}
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) < tmpOrphanAge {
			continue
		}
		os.Remove(p)
	}
}
```

Add `"path/filepath"` to the import block in `supervisor.go`.

Then call it in `Run`, immediately after the `os.MkdirAll` block and before the `ctx.Err()` check:

```go
	sweepOrphanTemps(dir, clk.Now())
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -race ./internal/supervisor/ -run 'TestSweep|TestRunSweeps' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/
git commit -m "feat(supervisor): sweep orphaned lock temp files on startup

A SIGKILL between AcquireLock's temp write and its link leaves a
lock.tmp.* file with nothing to reap it. Sweep them at startup, but only
past tmpOrphanAge: an unguarded sweep would delete a live acquirer's temp
file between its write and its link and fail that acquisition with
ENOENT."
```

---

### Task 7: Real-clock smoke test and default wiring

Every other test injects fakes, so nothing yet proves the production defaults — the real ticker and the real runner — are wired correctly.

**Files:**
- Test: `internal/supervisor/supervisor_test.go` (append)

**Interfaces:**
- Consumes: `supervisor.Run` with `Options{Config: cfg}` only — all other fields defaulted.
- Produces: no new exported surface.

- [ ] **Step 1: Write the failing test**

Append to `internal/supervisor/supervisor_test.go`:

```go
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
```

Note: `newCfg` calls `t.TempDir()` internally, and this test then overrides `cfg.Dir` and `cfg.Path` so the task's `touch` target and the state dir share one directory. That override is deliberate — leave it.

- [ ] **Step 2: Run the test**

Run: `go test ./internal/supervisor/ -run TestDefaultsRunARealTask -v -timeout 60s`
Expected: PASS, taking roughly 1-2 seconds.

If it fails with the task never firing, check that `@every 1s` is accepted by `schedule.Parse`. Run `go test ./internal/schedule/ -run Interval -v` to confirm the grammar, and adjust the fixture's schedule string to whatever the parser accepts rather than changing the parser.

- [ ] **Step 3: Verify the short flag skips it**

Run: `go test -short ./internal/supervisor/ -run TestDefaultsRunARealTask -v`
Expected: SKIP.

- [ ] **Step 4: Run the full suite under race**

Run: `go test -race ./... -count=2`
Expected: PASS.

- [ ] **Step 5: Run the full gate**

Run: `task check`
Expected: exit 0. This runs build, test, vet, and the gofmt check that CI enforces.

- [ ] **Step 6: Commit**

```bash
git add internal/supervisor/supervisor_test.go
git commit -m "test(supervisor): smoke-test the production defaults

One real-clock test with a real ticker, runner, and subprocess. Every
other test injects fakes, so nothing else would catch the default wiring
being wrong. Skipped under -short."
```

---

## Done criteria

- `go test -race ./... -count=2` passes.
- `task check` exits 0.
- `internal/engine` no longer has the `dispatch`/`Stop` WaitGroup race.
- `internal/supervisor` is imported by nothing yet — that is expected. The TUI is its first consumer.
- `internal/cli` is unchanged. No new command, no new flag.

## Follow-ups this plan deliberately does not do

- Wire `supervisor.Run` into a command. There is no headless command by design (see spec §2).
- Config hot-reload and the `e.cfg` race it exposes.
- The remaining carried findings in `docs/superpowers/plans/2026-07-29-coucou-core-followups.md`, notably the lossy event stream and the missing run-outcome accessor. Both bite when the TUI lands, not before.
