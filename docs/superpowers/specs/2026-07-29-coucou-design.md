# Coucou — Design

**Date:** 2026-07-29
**Status:** Approved, pending implementation plan

## 1. What Coucou is

A TUI scheduler that runs shell commands on a schedule, scoped to a single
project directory. It lives in a tmux or zellij pane, runs all day, and is
glanced at rather than opened.

That framing drives most of what follows. Coucou is not a cron replacement,
not a daemon, and not a task runner. It schedules a project's recurring
commands while you are working on that project.

### Non-goals for v1

Background execution when the TUI is closed. Desktop notifications. Retries
and backoff. Configurable success exit codes. One-shot (`at`-style) tasks.
Per-task time zones. Capturing, storing, or rendering command output. Remote
or multi-machine operation.

## 2. Decisions

### 2.1 Process model

The scheduler runs inside the TUI process. Closing the TUI stops scheduling.

`engine` is deliberately isolated from the terminal so a future daemon can
subscribe to the same event stream without the core changing. That seam is
part of the design; building the daemon is not.

### 2.2 Config discovery

`.coucou.yaml` (or `.coucou.yml`) in the **current directory only**. No
ancestor search.

Precedence: `--config PATH` > `$COUCOU_CONFIG` > `./.coucou.yaml`. Nothing
found is an error exit naming the directory searched — never a fall back to a
central config, which would let `coucou` in an unrelated directory start
unrelated tasks.

No ancestor search is a deliberate rejection of git-style discovery. The
decisive reason is path resolution: with CWD-only, the config directory and
the invocation directory are the same directory, so `log:`, `cwd`, and
relative paths inside commands have exactly one meaning. Walk-up splits them
and both possible resolutions surprise someone. In a monorepo it would also
mean standing in `frontend/` and firing the backend's tasks.

### 2.3 Schedule grammar

One `schedule:` key, three families:

```
"30 18 * * 1"              five-field cron
"@daily"                   POSIX macro, midnight
"@daily at 17:00"          macro + time
"@weekly on mon at 18:30"  macro + day + time
"@monthly on 1 at 09:00"
"@hourly at :15"
"@every 90s"               interval
"@every 2h30m"
```

Everything desugars to a common internal representation before reaching the
scheduler. Anything outside the grammar is a config error quoting the
expression back.

Cron follows POSIX exactly, including the day-of-month/day-of-week OR quirk,
so expressions copy-paste to and from a crontab unchanged.

`@every` measures from when the previous run **finished**, not started, so a
slow task cannot pile up on itself. It has no wall-clock due time.

Interval floor is 1s; below that is a config error.

### 2.4 Time and DST

Local wall clock, year round. No per-task time zone.

- Spring forward: a time that doesn't exist runs once at the next valid minute
- Fall back: a time that occurs twice runs once

The scheduler ticks at 1Hz and re-derives what is due by comparing against the
current clock. It never sleeps for a precomputed duration — that breaks across
suspend and NTP steps, both routine for a laptop pane.

### 2.5 Missed runs

Per task: `catch_up`, `report` (default), or `ignore`.

**The policy applies wherever a due time passes without a run** — at startup,
after suspend, after a clock step. Startup is the most visible case, not a
special one.

- `catch_up` — one run shortly after, then resume. A week away with a daily
  task means one run, not seven. Runs are staggered a couple of seconds apart
  so several overdue tasks don't stampede.
- `report` — mark the row overdue, run nothing
- `ignore` — nothing

A task is overdue if its last run finished before the most recent due time;
for `@every`, if more than one interval has elapsed since it last finished.

There is no staleness window. Catch-up fires once whether you were away an
hour or a month, so a window would add a knob that changes nothing.

### 2.6 Output

**Coucou does not capture command output.** Output can be anything from a
one-line result to reams of raw events; interpreting, storing, rotating, and
capping it is not a scheduler's job, and the shell already does it better.

`log:` is optional. When set, Coucou appends the command's stdout and stderr
to that path and displays the path. When unset, both go to `/dev/null` —
never inherited, since a child writing to the pane's fd would corrupt the UI.

`L` execs `$PAGER` (falling back to `less`, then `more`) on the log file and
restores the TUI on exit. Coucou never reads or renders the contents.

A failed run on a task with no `log:` shows a `no log` marker, so the fix is
discoverable when it matters.

**Known cost, accepted:** a failure on a task with no log leaves only an exit
code. Cron's answer was mailing output; Coucou's default is less helpful. The
marker plus the pager handoff is the mitigation.

A narrow "capture stderr only, on failure only" exception was considered and
rejected: `curl` writes its progress meter to stderr, so the assumption that
stderr is small diagnostics has a counterexample in the project's own README.

### 2.7 Execution

Three knobs; everything else fixed.

| | |
|---|---|
| `shell:` | Defaults to `$SHELL`, falls back to `/bin/sh`. A fish-syntax command must not be handed to `sh` |
| `timeout:` | Optional. SIGTERM the process group, 5s grace, then SIGKILL |
| `on_overlap:` | `skip` (default), `queue` (depth 1), `replace` |

Fixed: environment inherited from Coucou's own process, so the classic cron
`PATH` trap cannot occur; working directory is the config directory.

Documented, not solved: `$SHELL -c` is non-interactive, so shell functions and
aliases may be unavailable. A child that calls `setsid` escapes the process
group and survives timeout.

### 2.8 State and identity

Coucou never writes to the config file. Mutable state lives in `state_dir:`,
defaulting to `.coucou/` beside the config.

Task `name` is the state key. Renaming starts fresh history; duplicate names
are a config error.

```json
{ "version": 1,
  "tasks": {
    "API Refresh": {
      "disabled": false,
      "last_run": { "started_at": "...", "finished_at": "...",
                    "exit_code": 0, "outcome": "ok" },
      "history": [ { "at": "...", "outcome": "ok", "duration_ms": 1400 } ]
    } } }
```

`outcome`: `ok` | `fail` | `timeout` | `replaced` | `skipped`. History is a
12-entry ring; skips are recorded, since a task repeatedly overrunning its own
schedule is worth seeing. Writes are atomic (temp file, rename).

### 2.9 Instance lock

One Coucou per config. Two instances mean two schedulers and every task firing
twice — a correctness bug no amount of file locking fixes.

`.coucou/lock` holds pid, start time, and config path. A second launch exits
non-zero naming the holder. A lock whose pid is dead (signal-0 check) is
broken automatically. `--force` breaks a live one. Network filesystems where
liveness checks are unreliable are unsupported.

### 2.10 Reload

The config file is watched. On write: debounce 200ms, parse, then diff by name
— added tasks get a `next_at`, removed tasks stop scheduling but keep their
state, changed tasks recompute. An in-flight run finishes under the definition
it started with and records under its old name.

A parse or validation error keeps the last-good config scheduling and shows
the error in the Status line. There is no `e` key; you edit in your own editor.

### 2.11 CLI

```
coucou                  TUI
coucou validate         parse + check; exit 0/1 with all errors
coucou run NAME         foreground, streams to terminal
coucou list             tasks and next run times
coucou next             what fires soonest
--config PATH  --ascii  --force  --version
```

`coucou run` takes no lock and records nothing — it is you testing a command,
not an occurrence of the schedule.

### 2.12 UI

No drawn border; the multiplexer already draws one, and the space is scarce.

Five regions: Header (name, clock), Status (`Idle` / running task / errors),
task list, detail pane, key hints.

**Responsive by column shedding**, in priority order: `NAME` and `STATUS`
always survive, then `NEXT RUN`, then `SCHEDULE`, then `LAST RUN`. Names
ellipsize only after every optional column is gone. The detail pane collapses
when height is short. Below ~20 columns, a minimal message.

Spinner: braille frames at ~10fps, **only while something runs**; otherwise
the display updates at 1Hz. One animation clock drives every spinner so they
stay in step. Idle cost matters — this process runs all day.

Quiet by construction: no bells, no focus stealing.

Non-UTF-8 locale, `TERM` unset or `dumb`, or `--ascii` drops to ASCII glyphs.
`NO_COLOR` honored. Not a TTY refuses the TUI and points at `coucou list`.

## 3. Architecture

```
        cli  ──────────────┐
         │                 │
        tui ──────► engine ├──► schedule ──┐
                      │  │                 ├──► config
                      │  └──► runner ──────┘
                      └────► state
```

| Package | Responsibility |
|---|---|
| `config` | Parse and validate `.coucou.yaml`; precise, quotable errors |
| `schedule` | Parse `schedule:`; answer `NextAfter(t)` |
| `runner` | Execute one command; own its process group, timeout, signals |
| `state` | Atomic load/save of `state.json`; instance lock; history ring |
| `engine` | Tick; decide what's due; apply `on_overlap` and `missed_runs`; emit events |
| `tui` | Bubble Tea model over engine events; pure `View()` |
| `cli` | Flags and subcommands; wiring per command |

Dependencies point one direction; no cycles. `engine` has no terminal
dependency and communicates by event channel — the daemon seam.

`validate` is `config` alone. `list` and `next` are `config` + `schedule`.
`run NAME` is `config` + `runner`. Only the TUI needs all seven.

### Stack

Go with Bubble Tea. One static binary with no runtime, which matters for a
project-scoped tool you want on `$PATH` everywhere; negligible idle footprint
for an all-day pane; `robfig/cron` already implements five-field cron and the
`@every` spelling; `fsnotify` for reload; `Setpgid` for real process-group
timeouts; a pure `View()` that makes width degradation unit-testable.

```
coucou/
  main.go
  internal/{config,schedule,runner,state,engine,tui,cli}/
```

## 4. Run lifecycle

**Startup:** resolve config → load and validate → acquire lock (or exit) →
load state (absent = fresh) → compute `next_at` per task → apply `missed_runs`
→ start ticker and watcher.

**Tick (1Hz):** for each enabled, not-running task, is `now >= next_at`?

**Run:** dispatch with a fresh process group → row and Status line spin →
optional timeout timer → SIGTERM group, 5s grace, SIGKILL → record outcome, save
state, recompute `next_at` (from `finished_at` for `@every`, from
`NextAfter(now)` for cron).

## 5. Error handling

**Config.** Startup reports *all* validation failures with line numbers and
exits non-zero; reload keeps the last-good config and shows the error in the
Status line.

Missing config → exit 1 naming the directory. Duplicate names → error.
Unparseable schedule → quote it back. **Unknown keys are errors** — a silently
ignored `on_overlaps:` is the bug that costs an afternoon. Invalid enum →
list valid values. `@every` under 1s → error. Zero tasks → valid, empty state.
`log:` parent directories are created on demand; only permission failure is an
error. Unwritable `state_dir` → startup error.

**State.** Missing → fresh. Corrupt or a future schema version → rename to
`state.json.bak`, start fresh, note in Status; losing history beats refusing to
start. Write failure (full or read-only disk) → **keep scheduling**, show a
persistent error, retry next write.

**Execution.** Shell not found, fork failure, or unopenable `log:` → the run
fails before starting with the reason recorded. Task removed from config
mid-run → finishes, records under its old name. Disabled mid-run → current run
finishes, no new ones.

**Shutdown.** `q` with a run in flight confirms, then terminates the group and
releases the lock. SIGTERM/SIGHUP take the same path. SIGKILL leaves a stale
lock the next launch breaks.

**Time.** A large backward clock step invalidates and recomputes every cached
`next_at` rather than leaving tasks stalled.

**Terminal.** SIGWINCH relayouts. Pager exit triggers a full redraw. `L` on a
task with no `log:` is inert.

## 6. Testing

Three seams: `Clock` (injected everywhere time is read), `Runner` (an
interface, so engine tests never spawn a process), and `t.TempDir()` for the
filesystem — no FS abstraction layer.

| Package | Approach |
|---|---|
| `config` | Fixture YAML tables; golden files for error *messages* — wording is a feature |
| `schedule` | Pure tables on a fixed clock: cron fields, macro desugaring, `@every`, the dom/dow OR quirk, both DST transitions against a real zone |
| `runner` | Real subprocesses. Timeout escalation against a child that ignores SIGTERM; process-group kill verified via a grandchild; log append and `/dev/null` default observed |
| `state` | Atomic write under interruption, corrupt recovery, ring eviction, lock staleness by forking and killing a process |
| `engine` | Fake clock + fake runner, fully deterministic. All `on_overlap` and `missed_runs` policies, simulated suspend, reload diffs, disable-mid-run. No sleeps |
| `tui` | Golden `View()` snapshots at 32/48/72 columns; `teatest` for keys and the pager suspend/restore cycle |
| `cli` | End-to-end in a temp project: `validate` exit codes, `run` streaming and non-recording, `list` output |

Engine tests run under `-race` — the ticker, watcher, and child reaping are the
only real concurrency.

## 7. Open items for the implementation plan

- Whether `queue` depth 1 needs a UI affordance beyond the row marker
- ASCII glyph substitutions for `▸ ✓ ✗ ·` and the spinner
