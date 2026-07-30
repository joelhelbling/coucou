# Coucou supervisor — design

The headless scheduling session: hold the instance lock, drive the engine's
tick, shut both down cleanly.

## 1. Why

`internal/engine` is complete and heavily tested, but nothing imports it. No
process ever constructs an `Engine`, so Coucou cannot currently run anything on
a schedule. `coucou list` computes next-run times directly from the config.

The supervisor is the missing wiring. It is not a daemon.

## 2. Relationship to the approved design

The design spec (§2.1) says the scheduler runs inside the TUI process and that
building a daemon is out of scope. This work does not change that.

The supervisor is the lifecycle code the TUI needs regardless — lock, ticker,
shutdown — factored into its own package so it can be tested without a
terminal. The TUI will embed it. **No new user-facing command is added.** The
CLI surface stays exactly as specced.

Config hot-reload (§2.10) is explicitly out of scope here. It needs a
file-watch dependency, debounce, task diffing, and a fix for `Tick` iterating
`e.cfg.Tasks` outside `e.mu` — which is safe only while config is immutable.
Reload becomes its own unit of work with that race fix bundled in.

## 3. Interface

```go
// Package supervisor owns one Coucou scheduling session.
package supervisor

type Options struct {
    Config *config.Config
    Clock  clock.Clock       // defaults to clock.Real()
    Runner runner.Runner     // defaults to runner.New(runner.DefaultGrace)
    Ticks  <-chan time.Time  // defaults to time.NewTicker(time.Second).C
    Force  bool              // break a live lock
}

// Run blocks until ctx is cancelled, then shuts down and returns.
func Run(ctx context.Context, opts Options, observe func(*engine.Engine)) error
```

### Why these shapes

**`Ticks` is a channel, not a `Ticker` interface.** `clock.Clock` offers only
`Now()`, so tick delivery needs its own seam. A bare channel lets tests push
ticks with no new abstraction and no sleeping. An interface wrapping one
channel would be ceremony.

**Signals are not handled here.** `signal.Notify` is process-global; a package
that grabs it cannot be used twice or tested cleanly. `cli` translates
SIGINT/SIGTERM/SIGHUP into a `context.CancelFunc`. The supervisor understands
only cancellation.

**`observe` is a callback, not a return value.** `Run` blocks, so it cannot
return the engine while the engine is still useful. The callback fires once,
after `engine.Start()` and before the first tick — the window a TUI needs to
subscribe to `Events()` without missing any. `nil` gives headless behavior.

## 4. Sequence

1. `os.MkdirAll(cfg.StateDirPath())`
2. Sweep orphaned `lock.tmp.*` files older than `tmpOrphanAge` (1 minute)
3. `state.AcquireLock(dir, configPath, opts.Force)` — on failure return an
   error naming the holder; no lock, no scheduling
4. `defer lock.Release()`
5. `state.Load(dir)`
6. `engine.New(...)` → `engine.Start()` → `observe(eng)`
7. `for { select { case <-ctx.Done(): ...; case _, ok := <-ticks: ... } }`
8. On cancel: `eng.Stop()`, then the deferred `Release`

`Stop()` already cancels in-flight runs, waits for each, and closes the event
channel, so step 8 needs no separate `eng.Wait()`.

## 5. Error handling

| Failure | Behavior |
|---|---|
| Lock held by a live process | Error naming holder pid and start time. No scheduling. |
| Lock held by a dead pid | `AcquireLock` breaks it already. No supervisor logic. |
| `MkdirAll` fails | Return the error. No state dir, no scheduling. |
| `state.Load` | Never fails on bad content; corrupt files move to `state.json.bak`. Only genuine I/O errors propagate. |
| Run failures | Not the supervisor's concern. The engine records the outcome and keeps scheduling. |
| `ctx` cancelled before lock acquisition | Return `ctx.Err()` without touching the lock. |
| Lock release fails | Report, but do not mask an earlier error. |

**The orphan sweep must be age-guarded.** `AcquireLock` writes
`lock.tmp.<token>` and then links it into place, removing it via `defer` — so a
temp file normally lives for microseconds, and only a SIGKILL in that window
orphans one. An unguarded sweep would delete a *concurrently acquiring*
process's temp file between its write and its link, making that link fail with
ENOENT. That converts a rare orphan into a reproducible acquisition failure.

The sweep therefore ignores anything whose mtime is newer than `tmpOrphanAge`
(1 minute). A genuine orphan persists forever, so waiting a minute costs
nothing; no live acquirer holds a temp file for anywhere near that long.

**A closed `Ticks` channel must not spin.** Receiving from a closed channel
returns immediately and forever, turning the select into a hot loop that pegs a
core and calls `Tick()` millions of times. The receive uses the two-value form
and treats a closed channel as shutdown.

**No final `state.Save`.** The engine saves after every run, and `Stop()` waits
for every in-flight run to record. State is durable by the time `Stop()`
returns. This changes the day the TUI can toggle `disabled`, which is state the
engine does not write on a run boundary.

## 6. Engine fix landing with this

`dispatch` releases `e.mu` before calling `wg.Add(1)`. `Stop()` cancels under
the lock, unlocks, then `wg.Wait()`s. A `Tick` caught between those points
means `Wait` sees a zero counter and returns — so `Stop()` reports that
everything finished while a run is starting. That run then executes with
nothing waiting on it and records state after shutdown. Go may also panic with
"WaitGroup is reused before previous Wait returned".

Fix: check `stopped` and call `wg.Add(1)` inside the lock, before `Unlock`.
`Stop()` sets `stopped` under the same mutex, so once observed no new run
starts, and any `wg.Add` that beat it is counted before `Wait` can see zero.

This was a carried finding deferred to the TUI phase. It comes forward because
owning clean shutdown is the supervisor's entire purpose.

## 7. Testing

Every test injects `Clock`, `Runner`, and `Ticks`. None sleeps.

| Test | Method |
|---|---|
| Ticks drive runs | Advance fake clock, push a tick, assert the fake runner was called |
| Second instance refused | Hold the lock, then `Run` → error naming the holder, runner never called |
| Stale lock broken | Plant a lock with a dead pid, assert `Run` starts |
| Cancel terminates in-flight run | Fake runner blocks on `ctx.Done()`; cancel; assert return and cancellation |
| Lock released on exit | `.coucou/lock` gone after `Run` returns |
| Lock released on error paths | Force a post-acquisition failure; assert no lock left behind |
| Closed `Ticks` exits cleanly | Close it; assert `Run` returns rather than spinning |
| `observe` fires before first tick | Record call order |
| Orphan sweep | Plant an old-mtime `lock.tmp.*`, assert it is gone after startup |
| Sweep spares fresh temps | Plant a current-mtime `lock.tmp.*`, assert it survives |

`fakeRunner` exists in `engine_test.go` but is unexported. The supervisor gets
its own local double rather than coupling two packages' tests.

**The package runs under `-race`**, including a concurrent Tick/cancel hammer
aimed at the window §6 closes. That hammer must be observed failing against
the current code before the fix lands. A race fix nobody watched fail first is
a guess.

**One real-clock smoke test**, marked so `-short` skips it: default `Options`
with a trivial `echo` task, run ~2s, assert it fired. Every other test injects
fakes, so nothing else would catch `time.NewTicker` being wired up wrong.

## 8. Out of scope

- Config hot-reload and the file watcher (§2.10)
- Any new CLI command or flag
- The TUI itself
- Remaining carried findings not named above
