# Coucou core — carried findings

Findings raised during Plan 1 (the headless core) that were reviewed,
adjudicated, and deliberately not fixed before merge. None blocks the core.
Several bite the moment the TUI lands, and those are marked.

## Fix before or during Plan 2 (the TUI)

**The event stream is lossy, and nothing says so.** `engine.emit` drops events
when the 64-slot buffer is full. A dropped `EventFinished` after a delivered
`EventStarted` leaves an events-only consumer showing a task as running
forever. The TUI must reconcile against `Running()` / `Overdue()` rather than
trusting the stream alone. Add that caveat to `Events()`' doc comment.

**`dispatch`'s `wg.Add(1)` races with `Stop`'s `wg.Wait()`.** The race detector
reports it under a concurrent Tick/Stop hammer. `Wait` can return early or
panic with "WaitGroup is reused", and a run can start after `Stop` returns.
Fix: check the `stopped` flag inside `dispatch` before `wg.Add`. This is
pre-existing, not introduced by the final fix wave.

**The engine exposes no run-outcome accessor.** `NextAt`, `Running`, and
`Overdue` are all the TUI has. Last-run outcome, exit code, and history live in
`*state.State`, which is unsynchronized and mutated by the engine under `e.mu`
— so a TUI reading it directly races immediately. Add engine accessors before
the TUI reads state.

**`Tick` iterates `e.cfg.Tasks` outside `e.mu`.** Safe only because config is
immutable in Plan 1. Config hot-reload makes this a race.

## Performance

**`state.Save`'s fsync happens under the engine's main mutex.** The fix for a
genuine data race put disk I/O inside the lock that also guards `NextAt`,
`Running`, `Overdue`, and `Tick`. On a healthy SSD this is a few milliseconds
per completed run; on a slow or contended disk it stalls the ticker and every
reader. Fix: a separate `stateMu` guarding only `state.State`, with a fixed
lock-acquisition order. Worth doing when the TUI starts polling at 10fps.

## Correctness, narrow

**A two-winner window remains under `--force` specifically.** Holder A's
`Release` removes the lock file while forcing racer B is inside the mutex,
letting a third acquirer C link in before B's rename clobbers it. Requires
`--force` concurrent with a graceful release and a third acquirer. `--force` is
an explicit human override, so this was judged not to gate merge.

**The post-run interval re-anchor bypasses `scheduleNextLocked`** and therefore
the strictly-future invariant added in the final fix wave. Benign today: a run
longer than its own interval simply reruns promptly.

**A `SIGKILL` between the lock's temp-file write and its `os.Link` orphans a
`.tmp.*` file** in the state directory with nothing to reap it. Tiny and rare;
a startup sweep would clear it.

## Config validation

**Decode-stage errors carry no line number and are a different error type**
than `ValidationErrors`. An unknown key mixed with a type error also aborts the
decoder after the first, dropping the other. The spec promises all errors with
line numbers; this is a bounded shortfall inherent to `yaml.v3` strict
decoding. Rule-validation errors (the majority) do report all failures with
lines.

## Cosmetic

- `OutcomeSkipped` lives in `engine` while the other four outcomes live in
  `runner`, so a consumer must import both to switch on an outcome string.
- `handleOverlap`'s skip records a `LastRun` with zero timestamps, transiently
  blanking the interval anchor. Self-corrects when the in-flight run records.
- The leading-dash argument scan rejects a task literally named with a leading
  dash (`coucou run -weird`).
- `schedule.parseField` does not trim whitespace before its `*` comparison, so
  `" */15"` errors rather than being read as a wildcard.
- No compile-time `var _ Clock = (*Fake)(nil)` assertion.
- `Engine.Stop()` has no direct test beyond the concurrency hammer.
- No test covers the day-search loop crossing a December–January boundary
  (verified correct by hand, just untested).
- `runner`'s timing tests use real sleeps with roughly 5x margins.
  `TestRunKillsGrandchildren` flaked once under full-suite parallel load and
  has been clean since, including at `-count=5`.
