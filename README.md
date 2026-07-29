# Coucou

Coucou is a TUI task scheduler for running any shell command.

It is scoped to a **project**: the tasks it runs are the ones defined in the
directory you launch it from. It's built to live in a tmux or zellij pane and
be glanced at, not opened to answer a question.

## What it looks like

At 68 columns, with the detail pane:

```
Coucou                                          2026-07-29 14:32:07
Status  Idle
────────────────────────────────────────────────────────────────────
  NAME          SCHEDULE      NEXT RUN      LAST RUN      STATUS
▸ API Refresh   @weekly mon   Aug  3 18:30  Jul 27 18:30  ✓ ok
  xkcd          @daily 17:00  Jul 29 17:00  Jul 28 17:00  ✗ fail
  fetch deps    @every 30m    14:44         14:14         ✓ ok
────────────────────────────────────────────────────────────────────
API Refresh                                      enabled · 12 runs
Fetch latest API changelog

Command   curl -s https://api.github.com/repos/joelhelbling/coucou/
          commits.json
Run as    /opt/homebrew/bin/fish   in .   timeout 30s   overlap skip
Schedule  @weekly on mon at 18:30   (at 18:30 every Monday)
Next run  Aug  3 18:30   in 4d 3h 57m
Last run  Jul 27 18:30   exit 0   1.4s
Missed    catch_up
History   ✓ ✓ ✓ ✗ ✓ ✓ ✓ ✓ ✓ ✓ ✓ ✓   (oldest → newest)
Log       log/api.log
────────────────────────────────────────────────────────────────────
↑/↓ select   ⏎ run now   d disable   L log   q quit
```

There is no drawn border: in a dedicated pane the multiplexer already draws
one, and the two columns and two rows are better spent on content.

### While a task is running

The Status line names the task and spins; the same spinner frame appears in
that task's row, and `NEXT RUN` reads `running now`.

```
Coucou                          14:32:07
Status  ⠹ Running  xkcd   0:00:03
────────────────────────────────────────────────
  NAME          NEXT RUN      STATUS
  API Refresh   Aug  3 18:30  ✓ ok
▸ xkcd          running now   ⠹ 0:03
  fetch deps    14:44         ✓ ok
────────────────────────────────────────────────
xkcd                       enabled · 40 runs
Fetch latest xkcd comic

Command   curl -s https://xkcd.com/info.0.json
Schedule  @daily at 17:00
Next run  Jul 30 17:00   in 20h 27m
Last run  Jul 28 17:00   exit 7   0.9s   no log
History   ✓ ✓ ✗ ✓ · ✓ ✓ ✓
────────────────────────────────────────────────
↑/↓ select   ⏎ run   d disable   q quit
```

The spinner cycles the braille frames `⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏` at about 10 frames
per second, and only while something is running — the rest of the time the
display updates once a second. One animation clock drives every spinner on
screen, so they stay in step.

Two details visible above: `exit 7 · no log` marks a failure on a task with no
`log:` configured, so the fix is discoverable at the moment you need it. And
`·` in the History sparkline is a *skipped* occurrence — the task was still
running when its next turn came around.

### Narrow panes

Columns are shed in a fixed priority order as the pane narrows. `NAME` and
`STATUS` always survive; then `NEXT RUN`, then `SCHEDULE`, then `LAST RUN`.
The detail pane collapses when there isn't the height for it.

```
Coucou                  14:32:07
Status  Idle
────────────────────────────────
  NAME          STATUS
▸ API Refresh   ✓ ok
  xkcd          ✗ fail
  fetch deps    ✓ ok
────────────────────────────────
↑/↓  ⏎ run  d off  L log  q quit
```

### The regions

- **Header** — app name and a live clock.
- **Status** — `Idle`, or the running task with elapsed time. Also where
  config errors and other warnings appear.
- **Task list** — one row per task, `▸` marking the selection.
- **Detail pane** — everything about the selected task the list can't hold.
- **Key hints** — what you can do to the selected task right now.

`L` hands the `log:` file to `$PAGER` and restores the TUI when you quit it.
Coucou never reads, parses, or renders command output itself.

## How it's configured

Coucou reads `.coucou.yaml` from the **current directory**. It does not search
parent directories: the config directory and the invocation directory are
always the same, so relative paths have exactly one meaning.

Precedence: `--config PATH`, then `$COUCOU_CONFIG`, then `./.coucou.yaml`
(or `.coucou.yml`). If none is found, Coucou exits non-zero and says where it
looked.

```yaml
tasks:
  - name: API Refresh
    description: Fetch latest API changelog
    command: curl -s https://api.github.com/repos/joelhelbling/coucou/commits.json
    schedule: "@weekly on mon at 18:30"
    log: log/api.log
    timeout: 30s
    missed_runs: catch_up

  - name: xkcd
    description: Fetch latest xkcd comic
    command: curl -s https://xkcd.com/info.0.json >> log/xkcd.log
    schedule: "@daily at 17:00"

  - name: fetch deps
    description: Warm the dependency cache
    command: go mod download
    schedule: "@every 30m"
    on_overlap: skip
```

Editing the file while Coucou is running reloads it. A config that doesn't
parse leaves the previous one scheduling and reports the error in the Status
line.

### Task keys

| Key | Required | Default | Meaning |
|---|---|---|---|
| `name` | yes | — | Unique; identifies the task's history |
| `command` | yes | — | Shell command to run |
| `schedule` | yes | — | See below |
| `description` | no | — | Shown in the detail pane |
| `log` | no | none | Append stdout and stderr here; without it, both go to `/dev/null` |
| `shell` | no | `$SHELL` | Falls back to `/bin/sh` if unset |
| `timeout` | no | none | SIGTERM the process group, then SIGKILL after 5s |
| `on_overlap` | no | `skip` | `skip`, `queue` (depth 1), or `replace` |
| `missed_runs` | no | `report` | `catch_up`, `report`, or `ignore` |

Commands run with Coucou's own environment inherited, in the config's
directory. Because `$SHELL -c` is non-interactive, functions and aliases from
your shell config may not be loaded — a command that works at your prompt can
still fail here.

### Schedules

One key, three families:

```yaml
schedule: "30 18 * * 1"              # five-field cron
schedule: "@daily"                   # POSIX macro — midnight
schedule: "@daily at 17:00"          # macro with a time
schedule: "@weekly on mon at 18:30"
schedule: "@monthly on 1 at 09:00"
schedule: "@hourly at :15"
schedule: "@every 90s"               # interval
schedule: "@every 2h30m"
```

Cron expressions behave exactly as they do in a crontab, including the POSIX
quirk that specifying both day-of-month and day-of-week *ORs* them rather than
ANDing them. `@every` measures from when the previous run **finished**, so a
slow task cannot pile up on itself.

Times are local wall clock, year round. Across DST transitions: a time that
doesn't exist on spring-forward runs once at the next valid minute rather than
being skipped, and a time that occurs twice on fall-back runs once.

### Missed runs

If a due time passes while Coucou isn't running — or while the machine is
asleep — each task decides for itself:

- `catch_up` — run once, shortly after Coucou is back, then resume normally.
  Away for a week with a daily task means one run, not seven.
- `report` (default) — don't run; mark the row as overdue.
- `ignore` — do nothing.

### State

Per-project state lives in `.coucou/` beside the config (override with
`state_dir:`). It holds `state.json` (last-run times, the last twelve outcomes
per task, the disabled flags set with `d`), the instance `lock`, and
`lock.mutex`, a zero-length file used to serialize lock acquisition. **Coucou
never writes to your config file.** Add `.coucou/` to `.gitignore`.

Only one Coucou may schedule a given config at a time — a second one would
mean every task fires twice — so launching another exits non-zero and names
the instance that holds the lock. A lock left by a crashed process is detected
and broken automatically; `--force` breaks a live one.

## Command line

```
coucou                    # open the TUI
coucou validate           # parse and check the config; exit 0 or 1
coucou run NAME           # run one task in the foreground, streaming
coucou list               # print tasks and next run times
coucou next               # what fires soonest

coucou --config ../other/.coucou.yaml
coucou --ascii            # no box-drawing or braille glyphs
coucou --version
```

`coucou run` is you testing a command: it streams to your terminal, takes no
lock, and records nothing in history.
