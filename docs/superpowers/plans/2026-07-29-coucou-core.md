# Coucou Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Coucou's headless scheduler core and CLI — everything except the TUI — so that `coucou validate`, `list`, `next`, and `run` work and the scheduling engine is fully tested without a terminal.

**Architecture:** Six packages with dependencies pointing one direction: `schedule` (pure, no deps) ← `config` ← `engine`, with `runner` and `state` as leaves the engine drives. `engine` communicates only by emitting events on a channel and never touches a terminal — that is the seam the TUI (Plan 2) and any future daemon attach to. A `Clock` interface is injected everywhere time is read so every scheduling decision is deterministically testable.

**Tech Stack:** Go 1.23, `gopkg.in/yaml.v3` (parsing with `KnownFields(true)` for unknown-key rejection). No cron library — we implement the five-field parser and matcher ourselves to control the POSIX day-of-month/day-of-week rule and our documented DST behavior exactly.

## Global Constraints

- Module path: `github.com/joelhelbling/coucou`
- Go 1.23 or later
- Only external dependency in this plan: `gopkg.in/yaml.v3`
- All times are local wall clock; no per-task time zones
- Spring-forward: a wall time that does not exist runs once at the next valid minute
- Fall-back: a wall time that occurs twice runs once
- `@every` measures from when the previous run **finished**
- Interval floor is 1s; below that is a config error
- Unknown config keys are errors, not warnings
- SIGTERM → 5s grace → SIGKILL, applied to the process group
- Task `name` is the state key; duplicates are a config error
- History is a 12-entry ring per task
- Coucou never writes to the config file
- Every test uses an injected clock or `t.TempDir()`; no test sleeps on real time

**Spec:** `docs/superpowers/specs/2026-07-29-coucou-design.md`

**Note:** The spec's architecture diagram shows `schedule → config`. That arrow is backwards; `config` imports `schedule` to validate schedule strings at load. This plan uses the corrected direction.

---

## File Structure

| File | Responsibility |
|---|---|
| `go.mod` | Module definition |
| `internal/schedule/field.go` | One cron field: parse `*`, lists, ranges, steps, names into a bitmask |
| `internal/schedule/cron.go` | Five-field spec; day matching with the POSIX OR rule |
| `internal/schedule/next.go` | `Next` computation and the two DST rules |
| `internal/schedule/macro.go` | `@daily at 17:00` style desugaring to a cron spec |
| `internal/schedule/schedule.go` | `Schedule` interface, `Parse`, `@every` intervals |
| `internal/config/duration.go` | YAML-friendly `Duration` type (`30s`, `2h30m`) |
| `internal/config/config.go` | Types, `Load`, defaults |
| `internal/config/validate.go` | All validation rules; accumulates every error with a line number |
| `internal/config/discover.go` | `--config` / `$COUCOU_CONFIG` / `./.coucou.yaml` precedence |
| `internal/runner/runner.go` | Execute one command: process group, timeout, log file or `/dev/null` |
| `internal/state/state.go` | `state.json` load/save, atomic write, history ring, corrupt recovery |
| `internal/state/lock.go` | Instance lock: pid liveness, stale break, force |
| `internal/clock/clock.go` | `Clock` interface, real and fake implementations |
| `internal/engine/engine.go` | Tick loop, due detection, overlap and missed-run policies, event channel |
| `internal/cli/cli.go` | Subcommand dispatch and flags |
| `main.go` | Entry point |

---

### Task 1: Cron field parser

**Files:**
- Create: `go.mod`, `internal/schedule/field.go`
- Test: `internal/schedule/field_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `type field struct { bits uint64 }`; `func (f field) has(v int) bool`; `func parseField(s string, min, max int, names map[string]int) (f field, restricted bool, err error)`. `restricted` is false only when the expression is exactly `*`; Task 2 needs it for the POSIX OR rule.

- [ ] **Step 1: Initialize the module**

```bash
cd /Users/joelhelbling/code/ai/coucou
go mod init github.com/joelhelbling/coucou
mkdir -p internal/schedule
```

- [ ] **Step 2: Write the failing test**

Create `internal/schedule/field_test.go`:

```go
package schedule

import "testing"

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

func TestParseField(t *testing.T) {
	tests := []struct {
		name       string
		expr       string
		min, max   int
		names      map[string]int
		want       []int // values that must be set
		notWant    []int // values that must not be set
		restricted bool
	}{
		{name: "star", expr: "*", min: 0, max: 59,
			want: []int{0, 30, 59}, restricted: false},
		{name: "single", expr: "30", min: 0, max: 59,
			want: []int{30}, notWant: []int{29, 31}, restricted: true},
		{name: "list", expr: "0,15,30", min: 0, max: 59,
			want: []int{0, 15, 30}, notWant: []int{1}, restricted: true},
		{name: "range", expr: "1-5", min: 0, max: 59,
			want: []int{1, 3, 5}, notWant: []int{0, 6}, restricted: true},
		{name: "step on star", expr: "*/15", min: 0, max: 59,
			want: []int{0, 15, 30, 45}, notWant: []int{1, 14}, restricted: true},
		{name: "step on range", expr: "10-20/5", min: 0, max: 59,
			want: []int{10, 15, 20}, notWant: []int{11, 25}, restricted: true},
		{name: "named dow", expr: "mon", min: 0, max: 6, names: dowNames,
			want: []int{1}, notWant: []int{0, 2}, restricted: true},
		{name: "named range", expr: "mon-fri", min: 0, max: 6, names: dowNames,
			want: []int{1, 5}, notWant: []int{0, 6}, restricted: true},
		{name: "sunday as 7", expr: "7", min: 0, max: 6, names: dowNames,
			want: []int{0}, restricted: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, restricted, err := parseField(tc.expr, tc.min, tc.max, tc.names)
			if err != nil {
				t.Fatalf("parseField(%q) error: %v", tc.expr, err)
			}
			if restricted != tc.restricted {
				t.Errorf("restricted = %v, want %v", restricted, tc.restricted)
			}
			for _, v := range tc.want {
				if !f.has(v) {
					t.Errorf("expected %d to be set in %q", v, tc.expr)
				}
			}
			for _, v := range tc.notWant {
				if f.has(v) {
					t.Errorf("expected %d NOT to be set in %q", v, tc.expr)
				}
			}
		})
	}
}

func TestParseFieldErrors(t *testing.T) {
	for _, expr := range []string{"", "60", "-1", "5-1", "*/0", "a", "1-", "1,,2"} {
		if _, _, err := parseField(expr, 0, 59, nil); err == nil {
			t.Errorf("parseField(%q) expected error, got nil", expr)
		}
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/schedule/ -run TestParseField -v`
Expected: FAIL — `undefined: parseField`

- [ ] **Step 4: Write the implementation**

Create `internal/schedule/field.go`:

```go
package schedule

import (
	"fmt"
	"strconv"
	"strings"
)

// field is a bitmask of permitted values for one cron field.
// Bit i is set when value i is permitted. Max value is 59, so uint64 suffices.
type field struct{ bits uint64 }

func (f field) has(v int) bool {
	if v < 0 || v > 63 {
		return false
	}
	return f.bits&(1<<uint(v)) != 0
}

func (f *field) set(v int) { f.bits |= 1 << uint(v) }

// parseField parses one cron field. restricted reports whether the expression
// constrains the field at all; it is false only for "*".
func parseField(s string, min, max int, names map[string]int) (field, bool, error) {
	var f field
	if s == "" {
		return f, false, fmt.Errorf("empty field")
	}
	if s == "*" {
		for v := min; v <= max; v++ {
			f.set(v)
		}
		return f, false, nil
	}

	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return field{}, false, fmt.Errorf("empty element in %q", s)
		}

		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return field{}, false, fmt.Errorf("bad step in %q", part)
			}
			step = n
			part = part[:i]
		}

		var lo, hi int
		switch {
		case part == "*":
			lo, hi = min, max
		case strings.Contains(part, "-"):
			bits := strings.SplitN(part, "-", 2)
			var err error
			if lo, err = parseValue(bits[0], min, max, names); err != nil {
				return field{}, false, err
			}
			if hi, err = parseValue(bits[1], min, max, names); err != nil {
				return field{}, false, err
			}
			if lo > hi {
				return field{}, false, fmt.Errorf("range %q is inverted", part)
			}
		default:
			v, err := parseValue(part, min, max, names)
			if err != nil {
				return field{}, false, err
			}
			lo, hi = v, v
		}

		for v := lo; v <= hi; v += step {
			f.set(v)
		}
	}
	return f, true, nil
}

func parseValue(s string, min, max int, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number or known name", s)
	}
	// POSIX allows 7 for Sunday in the day-of-week field.
	if max == 6 && v == 7 {
		v = 0
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%d is out of range %d-%d", v, min, max)
	}
	return v, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/schedule/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add go.mod internal/schedule/field.go internal/schedule/field_test.go
git commit -m "feat(schedule): parse individual cron fields into bitmasks"
```

---

### Task 2: Cron spec and day matching

**Files:**
- Create: `internal/schedule/cron.go`
- Test: `internal/schedule/cron_test.go`

**Interfaces:**
- Consumes: `parseField`, `field.has` from Task 1
- Produces: `type CronSpec struct { Minute, Hour, Dom, Month, Dow field; domRestricted, dowRestricted bool }`; `func ParseCron(expr string) (*CronSpec, error)`; `func (c *CronSpec) matchesDay(t time.Time) bool`. Task 3 calls `matchesDay` and reads the `Minute`/`Hour` fields directly.

- [ ] **Step 1: Write the failing test**

Create `internal/schedule/cron_test.go`:

```go
package schedule

import (
	"testing"
	"time"
)

func TestParseCronRejectsBadInput(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 32 * *", "* * * 13 *", "* * * * 8", "x * * * *",
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) expected error, got nil", expr)
		}
	}
}

func TestMatchesDay(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}

	tests := []struct {
		name string
		expr string
		when time.Time
		want bool
	}{
		// 2026-08-03 is a Monday; 2026-08-01 is a Saturday.
		{"every day", "0 0 * * *", day(2026, 8, 3), true},
		{"dow only matches", "0 0 * * mon", day(2026, 8, 3), true},
		{"dow only rejects", "0 0 * * mon", day(2026, 8, 4), false},
		{"dom only matches", "0 0 1 * *", day(2026, 8, 1), true},
		{"dom only rejects", "0 0 1 * *", day(2026, 8, 2), false},
		{"month rejects", "0 0 * 9 *", day(2026, 8, 3), false},
		{"month matches", "0 0 * 9 *", day(2026, 9, 3), true},

		// POSIX quirk: when BOTH dom and dow are restricted they are ORed.
		{"dom or dow: matches dom", "0 0 1 * mon", day(2026, 8, 1), true},
		{"dom or dow: matches dow", "0 0 1 * mon", day(2026, 8, 3), true},
		{"dom or dow: matches neither", "0 0 1 * mon", day(2026, 8, 4), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCron(tc.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tc.expr, err)
			}
			if got := c.matchesDay(tc.when); got != tc.want {
				t.Errorf("matchesDay(%s) = %v, want %v",
					tc.when.Format("2006-01-02 Mon"), got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/schedule/ -run 'TestParseCron|TestMatchesDay' -v`
Expected: FAIL — `undefined: ParseCron`

- [ ] **Step 3: Write the implementation**

Create `internal/schedule/cron.go`:

```go
package schedule

import (
	"fmt"
	"strings"
	"time"
)

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// CronSpec is a parsed five-field cron expression.
type CronSpec struct {
	Minute, Hour, Dom, Month, Dow field
	domRestricted, dowRestricted  bool
	expr                          string
}

func (c *CronSpec) String() string { return c.expr }

// ParseCron parses a standard five-field cron expression:
// minute hour day-of-month month day-of-week
func ParseCron(expr string) (*CronSpec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("expected 5 fields, got %d", len(parts))
	}

	c := &CronSpec{expr: expr}
	var err error

	if c.Minute, _, err = parseField(parts[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if c.Hour, _, err = parseField(parts[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if c.Dom, c.domRestricted, err = parseField(parts[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if c.Month, _, err = parseField(parts[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if c.Dow, c.dowRestricted, err = parseField(parts[4], 0, 6, dayNames); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	return c, nil
}

// matchesDay reports whether the calendar day of t is permitted.
//
// It implements the POSIX rule: when BOTH day-of-month and day-of-week are
// restricted, a day matches if EITHER matches. When only one is restricted,
// only that one applies.
func (c *CronSpec) matchesDay(t time.Time) bool {
	if !c.Month.has(int(t.Month())) {
		return false
	}
	dom := c.Dom.has(t.Day())
	dow := c.Dow.has(int(t.Weekday()))

	switch {
	case c.domRestricted && c.dowRestricted:
		return dom || dow
	case c.domRestricted:
		return dom
	case c.dowRestricted:
		return dow
	default:
		return true
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/schedule/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/schedule/cron.go internal/schedule/cron_test.go
git commit -m "feat(schedule): parse five-field cron with POSIX dom/dow OR rule"
```

---

### Task 3: Next-occurrence computation and DST rules

This is the task where the spec's two DST rules become real. Read them again before starting: a wall time that does not exist runs once at the next valid minute; a wall time that occurs twice runs once.

**Files:**
- Create: `internal/schedule/next.go`
- Test: `internal/schedule/next_test.go`

**Interfaces:**
- Consumes: `CronSpec`, `matchesDay` from Task 2
- Produces: `func (c *CronSpec) Next(after time.Time) time.Time` — returns the first matching instant strictly after `after`, in `after`'s location. Returns the zero `time.Time` if no match exists within four years.

- [ ] **Step 1: Write the failing test**

Create `internal/schedule/next_test.go`:

```go
package schedule

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone %s unavailable: %v", name, err)
	}
	return loc
}

func TestNextBasic(t *testing.T) {
	utc := time.UTC
	tests := []struct {
		name  string
		expr  string
		after time.Time
		want  time.Time
	}{
		{"next minute", "* * * * *",
			time.Date(2026, 7, 29, 14, 32, 10, 0, utc),
			time.Date(2026, 7, 29, 14, 33, 0, 0, utc)},
		{"daily at 17:00 later today", "0 17 * * *",
			time.Date(2026, 7, 29, 14, 32, 0, 0, utc),
			time.Date(2026, 7, 29, 17, 0, 0, 0, utc)},
		{"daily at 17:00 rolls to tomorrow", "0 17 * * *",
			time.Date(2026, 7, 29, 17, 0, 0, 0, utc),
			time.Date(2026, 7, 30, 17, 0, 0, 0, utc)},
		{"weekly monday 18:30", "30 18 * * 1",
			time.Date(2026, 7, 29, 14, 32, 0, 0, utc),
			time.Date(2026, 8, 3, 18, 30, 0, 0, utc)},
		{"monthly on the 1st", "0 9 1 * *",
			time.Date(2026, 7, 29, 14, 32, 0, 0, utc),
			time.Date(2026, 8, 1, 9, 0, 0, 0, utc)},
		{"every 15 minutes", "*/15 * * * *",
			time.Date(2026, 7, 29, 14, 32, 0, 0, utc),
			time.Date(2026, 7, 29, 14, 45, 0, 0, utc)},
		{"leap day", "0 12 29 2 *",
			time.Date(2026, 7, 29, 0, 0, 0, 0, utc),
			time.Date(2028, 2, 29, 12, 0, 0, 0, utc)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCron(tc.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tc.expr, err)
			}
			got := c.Next(tc.after)
			if !got.Equal(tc.want) {
				t.Errorf("Next() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Spring forward: on 2026-03-08 in America/New_York, 02:00 jumps to 03:00,
// so 02:30 does not exist. Per spec it must run once at the next valid
// minute, which is 03:00 EDT.
func TestNextSpringForward(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	c, err := ParseCron("30 2 * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}

	after := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	got := c.Next(after) // 2026-03-07 02:30 already passed, so this is the 8th

	want := time.Date(2026, 3, 8, 3, 0, 0, 0, ny)
	if !got.Equal(want) {
		t.Errorf("Next() = %v, want %v (the skipped 02:30 must snap to 03:00)",
			got, want)
	}
}

// Fall back: on 2026-11-01 in America/New_York, 02:00 returns to 01:00, so
// 01:30 happens twice. Per spec it must run once.
func TestNextFallBackRunsOnce(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	c, err := ParseCron("30 1 * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}

	// Start before the first 01:30 of that morning.
	after := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)

	first := c.Next(after)
	if first.Hour() != 1 || first.Minute() != 30 || first.Day() != 1 {
		t.Fatalf("first occurrence = %v, want 2026-11-01 01:30", first)
	}

	// The next occurrence must be the following DAY, not the repeated hour.
	second := c.Next(first)
	if second.Day() != 2 {
		t.Errorf("second occurrence = %v, want 2026-11-02 01:30 "+
			"(the repeated 01:30 must not fire twice)", second)
	}
}

func TestNextNoMatchReturnsZero(t *testing.T) {
	// February 30th never happens.
	c, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if got := c.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Errorf("Next() = %v, want zero time", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/schedule/ -run TestNext -v`
Expected: FAIL — `c.Next undefined`

- [ ] **Step 3: Write the implementation**

Create `internal/schedule/next.go`:

```go
package schedule

import "time"

// maxSearchDays bounds the forward scan. Four years covers every leap-year
// cycle, so any expression that can ever match will match inside it.
const maxSearchDays = 4 * 366

// Next returns the first instant strictly after `after` that matches the
// spec, in after's location. It returns the zero time if the expression can
// never match (for example "0 0 30 2 *").
func (c *CronSpec) Next(after time.Time) time.Time {
	loc := after.Location()

	// Begin at the start of the minute following `after`.
	start := after.Truncate(time.Minute).Add(time.Minute)
	startY, startM, startD := start.Date()

	for offset := 0; offset < maxSearchDays; offset++ {
		day := time.Date(startY, startM, startD+offset, 0, 0, 0, 0, loc)
		if !c.matchesDay(day) {
			continue
		}

		// On the first day, skip any hour/minute already behind us.
		minHour, minMinute := 0, 0
		if offset == 0 {
			minHour, minMinute = start.Hour(), start.Minute()
		}

		for h := minHour; h < 24; h++ {
			if !c.Hour.has(h) {
				continue
			}
			m0 := 0
			if h == minHour {
				m0 = minMinute
			}
			for m := m0; m < 60; m++ {
				if !c.Minute.has(m) {
					continue
				}
				t := wallTime(day.Year(), day.Month(), day.Day(), h, m, loc)
				// A snapped spring-forward time can land at or before
				// `after`; keep searching if so.
				if t.After(after) {
					return t
				}
			}
		}
	}
	return time.Time{}
}

// wallTime builds the instant for a local wall-clock time.
//
// When the requested wall time does not exist because the clock sprang
// forward over it, time.Date normalizes to some other wall time. The spec
// requires running once at the next VALID minute instead, so we detect the
// mismatch and scan the day for the first instant whose wall clock is at or
// after what was asked for.
func wallTime(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, h, mi, 0, 0, loc)
	if t.Hour() == h && t.Minute() == mi && t.Day() == d {
		return t
	}
	return firstValidAtOrAfter(y, mo, d, h, mi, loc)
}

func firstValidAtOrAfter(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	cursor := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	// A day is at most 25 hours long when the clock falls back.
	for i := 0; i < 25*60; i++ {
		c := cursor.Add(time.Duration(i) * time.Minute)
		if c.Day() != d {
			break
		}
		if c.Hour() > h || (c.Hour() == h && c.Minute() >= mi) {
			return c
		}
	}
	return time.Date(y, mo, d, h, mi, 0, 0, loc)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/schedule/ -v`
Expected: PASS, including both DST tests

- [ ] **Step 5: Commit**

```bash
git add internal/schedule/next.go internal/schedule/next_test.go
git commit -m "feat(schedule): compute next occurrence with documented DST rules"
```

---

### Task 4: Schedule interface, macros, and intervals

**Files:**
- Create: `internal/schedule/macro.go`, `internal/schedule/schedule.go`
- Test: `internal/schedule/schedule_test.go`

**Interfaces:**
- Consumes: `ParseCron`, `(*CronSpec).Next` from Tasks 2 and 3
- Produces:
  - `type Kind int` with `KindCron` and `KindInterval`
  - `type Schedule interface { Next(prev, now time.Time) time.Time; Kind() Kind; Human() string; String() string }`
  - `func Parse(expr string) (Schedule, error)`
  - `const MinInterval = time.Second`

  `Next(prev, now)` takes both because the two kinds need different anchors: cron ignores `prev` and computes from `now`; an interval computes from `prev` (the previous run's finish time), falling back to `now` when `prev` is zero. Tasks 8 calls it with exactly these arguments.

- [ ] **Step 1: Write the failing test**

Create `internal/schedule/schedule_test.go`:

```go
package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestParseMacroDesugaring(t *testing.T) {
	tests := []struct {
		expr string
		cron string // the equivalent cron expression
	}{
		{"@hourly", "0 * * * *"},
		{"@hourly at :15", "15 * * * *"},
		{"@daily", "0 0 * * *"},
		{"@daily at 17:00", "0 17 * * *"},
		{"@weekly", "0 0 * * 0"},
		{"@weekly on mon at 18:30", "30 18 * * 1"},
		{"@monthly", "0 0 1 * *"},
		{"@monthly on 1 at 09:00", "0 9 1 * *"},
	}

	after := time.Date(2026, 7, 29, 14, 32, 0, 0, time.UTC)

	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := Parse(tc.expr)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.expr, err)
			}
			want, err := Parse(tc.cron)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.cron, err)
			}
			gotNext := got.Next(time.Time{}, after)
			wantNext := want.Next(time.Time{}, after)
			if !gotNext.Equal(wantNext) {
				t.Errorf("%q -> next %v; %q -> next %v",
					tc.expr, gotNext, tc.cron, wantNext)
			}
		})
	}
}

func TestParseInterval(t *testing.T) {
	s, err := Parse("@every 90s")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Kind() != KindInterval {
		t.Errorf("Kind() = %v, want KindInterval", s.Kind())
	}

	now := time.Date(2026, 7, 29, 14, 32, 0, 0, time.UTC)
	prev := time.Date(2026, 7, 29, 14, 30, 0, 0, time.UTC)

	// Anchored on the previous FINISH time, not on now.
	if got, want := s.Next(prev, now), prev.Add(90*time.Second); !got.Equal(want) {
		t.Errorf("Next(prev, now) = %v, want %v", got, want)
	}
	// With no previous run, anchored on now.
	if got, want := s.Next(time.Time{}, now), now.Add(90*time.Second); !got.Equal(want) {
		t.Errorf("Next(zero, now) = %v, want %v", got, want)
	}
}

func TestParseIntervalCompound(t *testing.T) {
	s, err := Parse("@every 2h30m")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	now := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	want := now.Add(150 * time.Minute)
	if got := s.Next(time.Time{}, now); !got.Equal(want) {
		t.Errorf("Next() = %v, want %v", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{
		"",
		"@yearly",              // not in our grammar
		"@daily at 25:00",      // impossible hour
		"@daily at noon",       // not a time
		"@weekly on funday",    // not a day
		"@monthly on 32",       // not a day of month
		"@hourly on mon",       // "on" is meaningless for hourly
		"@every",               // missing duration
		"@every 500ms",         // below the 1s floor
		"@every yesterday",     // not a duration
		"30 18 * *",            // four fields
		"nonsense",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", expr)
		}
	}
}

func TestHuman(t *testing.T) {
	tests := []struct{ expr, contains string }{
		{"@daily at 17:00", "17:00"},
		{"@weekly on mon at 18:30", "Monday"},
		{"@every 90s", "1m30s"},
	}
	for _, tc := range tests {
		s, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		if !strings.Contains(s.Human(), tc.contains) {
			t.Errorf("Human() for %q = %q, want it to contain %q",
				tc.expr, s.Human(), tc.contains)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/schedule/ -run 'TestParse|TestHuman' -v`
Expected: FAIL — `undefined: Parse`

- [ ] **Step 3: Write the macro desugarer**

Create `internal/schedule/macro.go`:

```go
package schedule

import (
	"fmt"
	"strconv"
	"strings"
)

// expandMacro converts a macro expression such as "@weekly on mon at 18:30"
// into the equivalent five-field cron expression. It returns ok=false when
// expr is not a macro at all, so the caller can try other forms.
func expandMacro(expr string) (cron string, ok bool, err error) {
	fields := strings.Fields(expr)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "@") {
		return "", false, nil
	}

	macro := strings.ToLower(fields[0])
	switch macro {
	case "@hourly", "@daily", "@weekly", "@monthly":
	default:
		return "", true, fmt.Errorf("unknown macro %q "+
			"(expected @hourly, @daily, @weekly, @monthly or @every)", macro)
	}

	var onArg, atArg string
	rest := fields[1:]
	for len(rest) > 0 {
		switch strings.ToLower(rest[0]) {
		case "on":
			if len(rest) < 2 {
				return "", true, fmt.Errorf("%q: 'on' needs a value", expr)
			}
			onArg, rest = rest[1], rest[2:]
		case "at":
			if len(rest) < 2 {
				return "", true, fmt.Errorf("%q: 'at' needs a value", expr)
			}
			atArg, rest = rest[1], rest[2:]
		default:
			return "", true, fmt.Errorf("%q: unexpected %q "+
				"(expected 'on' or 'at')", expr, rest[0])
		}
	}

	// Defaults matching the POSIX macros.
	minute, hour := 0, 0

	switch macro {
	case "@hourly":
		if onArg != "" {
			return "", true, fmt.Errorf("@hourly does not take 'on'")
		}
		if atArg != "" {
			m, err := parseMinuteOfHour(atArg)
			if err != nil {
				return "", true, err
			}
			minute = m
		}
		return fmt.Sprintf("%d * * * *", minute), true, nil

	case "@daily":
		if onArg != "" {
			return "", true, fmt.Errorf("@daily does not take 'on'")
		}
		if atArg != "" {
			var err error
			if hour, minute, err = parseTimeOfDay(atArg); err != nil {
				return "", true, err
			}
		}
		return fmt.Sprintf("%d %d * * *", minute, hour), true, nil

	case "@weekly":
		dow := 0 // Sunday
		if onArg != "" {
			d, ok := dayNames[strings.ToLower(onArg)]
			if !ok {
				return "", true, fmt.Errorf("%q is not a day name "+
					"(expected sun, mon, tue, wed, thu, fri or sat)", onArg)
			}
			dow = d
		}
		if atArg != "" {
			var err error
			if hour, minute, err = parseTimeOfDay(atArg); err != nil {
				return "", true, err
			}
		}
		return fmt.Sprintf("%d %d * * %d", minute, hour, dow), true, nil

	case "@monthly":
		dom := 1
		if onArg != "" {
			d, err := strconv.Atoi(onArg)
			if err != nil || d < 1 || d > 31 {
				return "", true, fmt.Errorf("%q is not a day of month 1-31", onArg)
			}
			dom = d
		}
		if atArg != "" {
			var err error
			if hour, minute, err = parseTimeOfDay(atArg); err != nil {
				return "", true, err
			}
		}
		return fmt.Sprintf("%d %d %d * *", minute, hour, dom), true, nil
	}
	return "", true, fmt.Errorf("unhandled macro %q", macro)
}

// parseTimeOfDay parses "17:00".
func parseTimeOfDay(s string) (hour, minute int, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%q is not a time like 17:00", s)
	}
	if hour, err = strconv.Atoi(parts[0]); err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("%q does not have an hour in 0-23", s)
	}
	if minute, err = strconv.Atoi(parts[1]); err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q does not have a minute in 0-59", s)
	}
	return hour, minute, nil
}

// parseMinuteOfHour parses ":15" or "15".
func parseMinuteOfHour(s string) (int, error) {
	m, err := strconv.Atoi(strings.TrimPrefix(s, ":"))
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is not a minute like :15", s)
	}
	return m, nil
}
```

- [ ] **Step 4: Write the Schedule interface and Parse**

Create `internal/schedule/schedule.go`:

```go
package schedule

import (
	"fmt"
	"strings"
	"time"
)

// MinInterval is the floor for @every.
const MinInterval = time.Second

type Kind int

const (
	KindCron Kind = iota
	KindInterval
)

// Schedule answers when a task should next run.
//
// Next takes both anchors because the kinds differ: cron ignores prev and
// computes forward from now, while an interval computes from prev (the
// previous run's FINISH time), falling back to now when prev is zero.
type Schedule interface {
	Next(prev, now time.Time) time.Time
	Kind() Kind
	Human() string
	String() string
}

// Parse accepts raw cron, a parameterized macro, or an @every interval.
func Parse(expr string) (Schedule, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, fmt.Errorf("schedule is empty")
	}

	if strings.HasPrefix(strings.ToLower(trimmed), "@every") {
		return parseInterval(trimmed)
	}

	if cronExpr, isMacro, err := expandMacro(trimmed); isMacro {
		if err != nil {
			return nil, err
		}
		spec, err := ParseCron(cronExpr)
		if err != nil {
			return nil, fmt.Errorf("%q expanded to %q: %w", trimmed, cronExpr, err)
		}
		return &cronSchedule{spec: spec, source: trimmed}, nil
	}

	spec, err := ParseCron(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid schedule: %w", trimmed, err)
	}
	return &cronSchedule{spec: spec, source: trimmed}, nil
}

func parseInterval(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 2 {
		return nil, fmt.Errorf("%q: @every needs one duration, like @every 90s", expr)
	}
	d, err := time.ParseDuration(fields[1])
	if err != nil {
		return nil, fmt.Errorf("%q is not a duration like 90s or 2h30m", fields[1])
	}
	if d < MinInterval {
		return nil, fmt.Errorf("@every %s is below the %s floor", fields[1], MinInterval)
	}
	return &intervalSchedule{every: d, source: expr}, nil
}

type cronSchedule struct {
	spec   *CronSpec
	source string
}

func (c *cronSchedule) Next(_ , now time.Time) time.Time { return c.spec.Next(now) }
func (c *cronSchedule) Kind() Kind                       { return KindCron }
func (c *cronSchedule) String() string                   { return c.source }

func (c *cronSchedule) Human() string {
	// Describe the expanded spec by sampling it: this keeps the description
	// honest for both raw cron and macros without a second grammar.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := c.spec.Next(base)
	if first.IsZero() {
		return c.source
	}
	second := c.spec.Next(first)

	timeOfDay := first.Format("15:04")
	switch {
	case !second.IsZero() && second.Sub(first) == 24*time.Hour:
		return fmt.Sprintf("at %s every day", timeOfDay)
	case !second.IsZero() && second.Sub(first) == 7*24*time.Hour:
		return fmt.Sprintf("at %s every %s", timeOfDay, first.Weekday())
	case !second.IsZero() && second.Sub(first) == time.Hour:
		return fmt.Sprintf("at :%02d every hour", first.Minute())
	default:
		return c.source
	}
}

type intervalSchedule struct {
	every  time.Duration
	source string
}

func (i *intervalSchedule) Next(prev, now time.Time) time.Time {
	if prev.IsZero() {
		return now.Add(i.every)
	}
	return prev.Add(i.every)
}

func (i *intervalSchedule) Kind() Kind     { return KindInterval }
func (i *intervalSchedule) String() string { return i.source }
func (i *intervalSchedule) Human() string  { return "every " + i.every.String() }
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/schedule/ -v`
Expected: PASS

- [ ] **Step 6: Run go vet and gofmt**

```bash
gofmt -l ./internal/ && go vet ./...
```
Expected: no output from `gofmt -l`, no findings from `go vet`

- [ ] **Step 7: Commit**

```bash
git add internal/schedule/macro.go internal/schedule/schedule.go internal/schedule/schedule_test.go
git commit -m "feat(schedule): add Schedule interface, macros, and @every intervals"
```

---

### Task 5: Config duration type

**Files:**
- Create: `internal/config/duration.go`
- Test: `internal/config/duration_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `type Duration time.Duration` with `UnmarshalYAML`, and `func (d Duration) Std() time.Duration`. Task 6 uses this for the `timeout:` field.

- [ ] **Step 1: Write the failing test**

Create `internal/config/duration_test.go`:

```go
package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		yaml string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"2h30m", 150 * time.Minute},
		{"500ms", 500 * time.Millisecond},
	}
	for _, tc := range tests {
		var d Duration
		if err := yaml.Unmarshal([]byte(tc.yaml), &d); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.yaml, err)
		}
		if d.Std() != tc.want {
			t.Errorf("%q = %v, want %v", tc.yaml, d.Std(), tc.want)
		}
	}
}

func TestDurationUnmarshalErrors(t *testing.T) {
	for _, s := range []string{"soon", "30", "-5s"} {
		var d Duration
		if err := yaml.Unmarshal([]byte(s), &d); err == nil {
			t.Errorf("unmarshal %q expected error, got nil", s)
		}
	}
}
```

- [ ] **Step 2: Add the dependency and run the test**

```bash
mkdir -p internal/config
go get gopkg.in/yaml.v3
go test ./internal/config/ -v
```
Expected: FAIL — `undefined: Duration`

- [ ] **Step 3: Write the implementation**

Create `internal/config/duration.go`:

```go
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from strings like "30s".
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("expected a duration string like 30s")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration like 30s or 2h30m", s)
	}
	if parsed < 0 {
		return fmt.Errorf("%q is negative", s)
	}
	*d = Duration(parsed)
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/config/duration.go internal/config/duration_test.go
git commit -m "feat(config): add YAML duration type"
```

---

### Task 6: Config types, loading, and validation

**Files:**
- Create: `internal/config/config.go`, `internal/config/validate.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `Duration` from Task 5; `schedule.Parse` from Task 4
- Produces:
  - `type Task struct { Name, Description, Command, Schedule, Log, Shell string; Timeout Duration; OnOverlap OverlapPolicy; MissedRuns MissedPolicy; Line int; Parsed schedule.Schedule }`
  - `type Config struct { StateDir string; Tasks []Task; Path, Dir string }`
  - `type ValidationError struct { Line int; Msg string }` and `type ValidationErrors []ValidationError` implementing `error`
  - `func Load(path string) (*Config, error)`
  - Constants `OverlapSkip`, `OverlapQueue`, `OverlapReplace`, `MissedCatchUp`, `MissedReport`, `MissedIgnore`

  `Task.Parsed` is populated by `Load`, so `engine` never re-parses. `Config.Dir` is the directory containing the config file and is the working directory for every command.

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".coucou.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidConfig(t *testing.T) {
	path := write(t, `
tasks:
  - name: API Refresh
    description: Fetch latest API changelog
    command: curl -s https://example.com
    schedule: "@weekly on mon at 18:30"
    log: log/api.log
    timeout: 30s
    missed_runs: catch_up
  - name: xkcd
    command: curl -s https://xkcd.com/info.0.json
    schedule: "@daily at 17:00"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(cfg.Tasks))
	}
	if cfg.Dir != filepath.Dir(path) {
		t.Errorf("Dir = %q, want %q", cfg.Dir, filepath.Dir(path))
	}

	api := cfg.Tasks[0]
	if api.Timeout.Std() != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", api.Timeout.Std())
	}
	if api.MissedRuns != MissedCatchUp {
		t.Errorf("MissedRuns = %q, want catch_up", api.MissedRuns)
	}
	if api.Parsed == nil {
		t.Error("Parsed schedule is nil; Load must parse it")
	}

	// Defaults applied to the second task.
	x := cfg.Tasks[1]
	if x.OnOverlap != OverlapSkip {
		t.Errorf("OnOverlap = %q, want skip", x.OnOverlap)
	}
	if x.MissedRuns != MissedReport {
		t.Errorf("MissedRuns = %q, want report", x.MissedRuns)
	}
	if x.Shell == "" {
		t.Error("Shell should default to $SHELL or /bin/sh")
	}
}

func TestLoadEmptyTasksIsValid(t *testing.T) {
	path := write(t, "tasks: []\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tasks) != 0 {
		t.Errorf("got %d tasks, want 0", len(cfg.Tasks))
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := write(t, `
tasks:
  - name: x
    command: "true"
    schedule: "@daily"
    on_overlaps: queue
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for the unknown key on_overlaps")
	}
	if !strings.Contains(err.Error(), "on_overlaps") {
		t.Errorf("error %q should name the unknown key", err)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	path := write(t, `
tasks:
  - name: ""
    command: "true"
    schedule: "@daily"
  - name: dup
    schedule: "@daily"
  - name: dup
    command: "true"
    schedule: "not a schedule"
  - name: bad enum
    command: "true"
    schedule: "@daily"
    on_overlap: sometimes
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation errors")
	}
	msg := err.Error()
	for _, want := range []string{
		"name",           // empty name
		"command",        // missing command
		"duplicate",      // duplicate name
		"not a schedule", // unparseable schedule, quoted back
		"sometimes",      // invalid enum value quoted back
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error output missing %q:\n%s", want, msg)
		}
	}
}

func TestLoadReportsLineNumbers(t *testing.T) {
	path := write(t, `tasks:
  - name: ok
    command: "true"
    schedule: "@daily"
  - name: broken
    command: "true"
    schedule: "nope"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	verrs, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("error is %T, want ValidationErrors", err)
	}
	if len(verrs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(verrs), verrs)
	}
	// The second task begins on line 5.
	if verrs[0].Line != 5 {
		t.Errorf("Line = %d, want 5", verrs[0].Line)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestLoadRejectsIntervalBelowFloor(t *testing.T) {
	path := write(t, `
tasks:
  - name: fast
    command: "true"
    schedule: "@every 500ms"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for an interval below the floor")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: FAIL — `undefined: Load`

- [ ] **Step 3: Write the types and loader**

Create `internal/config/config.go`:

```go
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joelhelbling/coucou/internal/schedule"
	"gopkg.in/yaml.v3"
)

type OverlapPolicy string

const (
	OverlapSkip    OverlapPolicy = "skip"
	OverlapQueue   OverlapPolicy = "queue"
	OverlapReplace OverlapPolicy = "replace"
)

type MissedPolicy string

const (
	MissedCatchUp MissedPolicy = "catch_up"
	MissedReport  MissedPolicy = "report"
	MissedIgnore  MissedPolicy = "ignore"
)

// Task is one scheduled command.
type Task struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Command     string        `yaml:"command"`
	Schedule    string        `yaml:"schedule"`
	Log         string        `yaml:"log"`
	Shell       string        `yaml:"shell"`
	Timeout     Duration      `yaml:"timeout"`
	OnOverlap   OverlapPolicy `yaml:"on_overlap"`
	MissedRuns  MissedPolicy  `yaml:"missed_runs"`

	// Line is the 1-based line where this task begins, for error messages.
	Line int `yaml:"-"`
	// Parsed is the compiled schedule, populated by Load.
	Parsed schedule.Schedule `yaml:"-"`
}

// Config is a whole .coucou.yaml.
type Config struct {
	StateDir string `yaml:"state_dir"`
	Tasks    []Task `yaml:"tasks"`

	// Path is the config file; Dir is its directory, which is the working
	// directory for every command.
	Path string `yaml:"-"`
	Dir  string `yaml:"-"`
}

// Load reads, parses, and validates a config file. It returns
// ValidationErrors when the file parses but fails our rules.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // unknown keys are errors, not warnings
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.Path = abs
	cfg.Dir = filepath.Dir(abs)

	assignLineNumbers(data, &cfg)
	applyDefaults(&cfg)

	if errs := validate(&cfg); len(errs) > 0 {
		return nil, errs
	}
	return &cfg, nil
}

// assignLineNumbers walks the raw YAML so validation errors can point at the
// task that caused them.
func assignLineNumbers(data []byte, cfg *Config) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return
	}
	if len(root.Content) == 0 {
		return
	}
	doc := root.Content[0]
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "tasks" {
			continue
		}
		seq := doc.Content[i+1]
		for j, item := range seq.Content {
			if j < len(cfg.Tasks) {
				cfg.Tasks[j].Line = item.Line
			}
		}
	}
}

func applyDefaults(cfg *Config) {
	if cfg.StateDir == "" {
		cfg.StateDir = ".coucou"
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		if t.Shell == "" {
			t.Shell = shell
		}
		if t.OnOverlap == "" {
			t.OnOverlap = OverlapSkip
		}
		if t.MissedRuns == "" {
			t.MissedRuns = MissedReport
		}
	}
}

// StateDirPath returns the absolute state directory.
func (c *Config) StateDirPath() string {
	if filepath.IsAbs(c.StateDir) {
		return c.StateDir
	}
	return filepath.Join(c.Dir, c.StateDir)
}

// LogPath resolves a task's log path against the config directory.
// It returns "" when the task has no log configured.
func (c *Config) LogPath(t *Task) string {
	if t.Log == "" {
		return ""
	}
	if filepath.IsAbs(t.Log) {
		return t.Log
	}
	return filepath.Join(c.Dir, t.Log)
}
```

- [ ] **Step 4: Write the validator**

Create `internal/config/validate.go`:

```go
package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/joelhelbling/coucou/internal/schedule"
)

// ValidationError is one problem with the config, located by line.
type ValidationError struct {
	Line int
	Msg  string
}

// ValidationErrors is every problem found, in line order.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	sorted := make(ValidationErrors, len(e))
	copy(sorted, e)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Line < sorted[j].Line })

	var b strings.Builder
	for i, ve := range sorted {
		if i > 0 {
			b.WriteByte('\n')
		}
		if ve.Line > 0 {
			fmt.Fprintf(&b, "line %d: %s", ve.Line, ve.Msg)
		} else {
			b.WriteString(ve.Msg)
		}
	}
	return b.String()
}

// validate checks every rule and returns ALL failures, not just the first.
// It also compiles each task's schedule into Task.Parsed.
func validate(cfg *Config) ValidationErrors {
	var errs ValidationErrors
	seen := make(map[string]int, len(cfg.Tasks))

	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		add := func(format string, args ...any) {
			errs = append(errs, ValidationError{
				Line: t.Line,
				Msg:  fmt.Sprintf(format, args...),
			})
		}

		name := strings.TrimSpace(t.Name)
		switch {
		case name == "":
			add("task is missing a name")
		default:
			if prev, dup := seen[name]; dup {
				add("duplicate task name %q (first used on line %d)", name, prev)
			} else {
				seen[name] = t.Line
			}
		}

		if strings.TrimSpace(t.Command) == "" {
			add("task %q is missing a command", name)
		}

		if strings.TrimSpace(t.Schedule) == "" {
			add("task %q is missing a schedule", name)
		} else {
			parsed, err := schedule.Parse(t.Schedule)
			if err != nil {
				add("task %q: %v", name, err)
			} else {
				t.Parsed = parsed
			}
		}

		switch t.OnOverlap {
		case OverlapSkip, OverlapQueue, OverlapReplace:
		default:
			add("task %q: on_overlap %q is not one of skip, queue, replace",
				name, t.OnOverlap)
		}

		switch t.MissedRuns {
		case MissedCatchUp, MissedReport, MissedIgnore:
		default:
			add("task %q: missed_runs %q is not one of catch_up, report, ignore",
				name, t.MissedRuns)
		}
	}
	return errs
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/validate.go internal/config/config_test.go
git commit -m "feat(config): load and validate .coucou.yaml with all errors reported"
```

---

### Task 7: Config discovery

**Files:**
- Create: `internal/config/discover.go`
- Test: `internal/config/discover_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks
- Produces: `func Discover(cwd, flagPath, envPath string) (string, error)` — returns the config path to load. Precedence is flag, then env, then `./.coucou.yaml`, then `./.coucou.yml`. **No ancestor search.** Task 9 calls this.

- [ ] **Step 1: Write the failing test**

Create `internal/config/discover_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoverPrecedence(t *testing.T) {
	dir := t.TempDir()
	local := touch(t, dir, ".coucou.yaml")
	flagFile := touch(t, dir, "from-flag.yaml")
	envFile := touch(t, dir, "from-env.yaml")

	if got, err := Discover(dir, flagFile, envFile); err != nil || got != flagFile {
		t.Errorf("flag should win: got %q, err %v", got, err)
	}
	if got, err := Discover(dir, "", envFile); err != nil || got != envFile {
		t.Errorf("env should win over local: got %q, err %v", got, err)
	}
	if got, err := Discover(dir, "", ""); err != nil || got != local {
		t.Errorf("local should be used: got %q, err %v", got, err)
	}
}

func TestDiscoverYmlAlias(t *testing.T) {
	dir := t.TempDir()
	alias := touch(t, dir, ".coucou.yml")
	got, err := Discover(dir, "", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != alias {
		t.Errorf("got %q, want %q", got, alias)
	}
}

func TestDiscoverPrefersYamlOverYml(t *testing.T) {
	dir := t.TempDir()
	primary := touch(t, dir, ".coucou.yaml")
	touch(t, dir, ".coucou.yml")
	got, err := Discover(dir, "", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != primary {
		t.Errorf("got %q, want %q", got, primary)
	}
}

// The design explicitly rejects ancestor search: a config in the parent
// directory must NOT be found.
func TestDiscoverDoesNotSearchAncestors(t *testing.T) {
	parent := t.TempDir()
	touch(t, parent, ".coucou.yaml")
	child := filepath.Join(parent, "frontend")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Discover(child, "", "")
	if err == nil {
		t.Fatal("expected an error; ancestor configs must not be discovered")
	}
	if !strings.Contains(err.Error(), child) {
		t.Errorf("error %q should name the directory searched (%s)", err, child)
	}
}

func TestDiscoverMissingFlagFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if _, err := Discover(dir, filepath.Join(dir, "nope.yaml"), ""); err == nil {
		t.Fatal("expected an error for a --config path that does not exist")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run TestDiscover -v`
Expected: FAIL — `undefined: Discover`

- [ ] **Step 3: Write the implementation**

Create `internal/config/discover.go`:

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultNames are the config filenames looked for in the current directory,
// in order of preference.
var DefaultNames = []string{".coucou.yaml", ".coucou.yml"}

// Discover resolves which config file to load.
//
// Precedence: flagPath, then envPath, then a default name in cwd. There is
// deliberately NO ancestor search: the config directory and the invocation
// directory are always the same, so relative paths have one meaning.
func Discover(cwd, flagPath, envPath string) (string, error) {
	for _, explicit := range []struct{ path, source string }{
		{flagPath, "--config"},
		{envPath, "$COUCOU_CONFIG"},
	} {
		if explicit.path == "" {
			continue
		}
		if _, err := os.Stat(explicit.path); err != nil {
			return "", fmt.Errorf("%s: cannot read %s: %w",
				explicit.source, explicit.path, err)
		}
		return explicit.path, nil
	}

	for _, name := range DefaultNames {
		candidate := filepath.Join(cwd, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no %s found in %s\n"+
		"Coucou reads its config from the current directory only.\n"+
		"Use --config PATH or $COUCOU_CONFIG to point elsewhere.",
		DefaultNames[0], cwd)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/discover.go internal/config/discover_test.go
git commit -m "feat(config): discover config in cwd only, with flag and env override"
```

---

### Task 8: Clock

**Files:**
- Create: `internal/clock/clock.go`
- Test: `internal/clock/clock_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `type Clock interface { Now() time.Time }`; `func Real() Clock`; `type Fake struct{...}` with `func NewFake(t time.Time) *Fake`, `func (f *Fake) Now() time.Time`, `func (f *Fake) Advance(d time.Duration)`, `func (f *Fake) Set(t time.Time)`. Task 11 injects this into the engine.

- [ ] **Step 1: Write the failing test**

Create `internal/clock/clock_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
mkdir -p internal/clock
go test ./internal/clock/ -v
```
Expected: FAIL — `undefined: NewFake`

- [ ] **Step 3: Write the implementation**

Create `internal/clock/clock.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/clock/ -race -v`
Expected: PASS with no race warnings

- [ ] **Step 5: Commit**

```bash
git add internal/clock/
git commit -m "feat(clock): add injectable Clock with a race-safe fake"
```

---

### Task 9: Runner

**Files:**
- Create: `internal/runner/runner.go`
- Test: `internal/runner/runner_test.go`

**Interfaces:**
- Consumes: `config.Task`, `config.Config` from Task 6
- Produces:
  - `type Outcome string` with `OutcomeOK`, `OutcomeFail`, `OutcomeTimeout`, `OutcomeReplaced`
  - `type Result struct { StartedAt, FinishedAt time.Time; ExitCode int; Outcome Outcome; Err error }`
  - `type Runner interface { Run(ctx context.Context, cfg *config.Config, t *config.Task) Result }`
  - `func New(grace time.Duration) Runner` — `grace` is the SIGTERM-to-SIGKILL window; production passes `runner.DefaultGrace` (5s)
  - `func NewStreaming(grace time.Duration, out *os.File) Runner` — sends output to `out` instead of the task's log; Task 12's `coucou run` uses it
  - `const DefaultGrace = 5 * time.Second`

  Task 11 depends on `Runner` being an interface so the engine can be tested with a fake.

- [ ] **Step 1: Write the failing test**

Create `internal/runner/runner_test.go`:

```go
package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joelhelbling/coucou/internal/config"
)

func fixture(t *testing.T, task config.Task) (*config.Config, *config.Task) {
	t.Helper()
	dir := t.TempDir()
	if task.Shell == "" {
		task.Shell = "/bin/sh"
	}
	cfg := &config.Config{Dir: dir, Tasks: []config.Task{task}}
	return cfg, &cfg.Tasks[0]
}

func TestRunSuccess(t *testing.T) {
	cfg, task := fixture(t, config.Task{Name: "ok", Command: "exit 0"})
	res := New(DefaultGrace).Run(context.Background(), cfg, task)

	if res.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want ok (err: %v)", res.Outcome, res.Err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.FinishedAt.Before(res.StartedAt) {
		t.Error("FinishedAt is before StartedAt")
	}
}

func TestRunFailureCapturesExitCode(t *testing.T) {
	cfg, task := fixture(t, config.Task{Name: "bad", Command: "exit 7"})
	res := New(DefaultGrace).Run(context.Background(), cfg, task)

	if res.Outcome != OutcomeFail {
		t.Errorf("Outcome = %q, want fail", res.Outcome)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

func TestRunWritesToLogFile(t *testing.T) {
	cfg, task := fixture(t, config.Task{
		Name:    "logs",
		Command: "echo out; echo err 1>&2",
		Log:     "nested/dir/task.log",
	})

	res := New(DefaultGrace).Run(context.Background(), cfg, task)
	if res.Outcome != OutcomeOK {
		t.Fatalf("Outcome = %q, err %v", res.Outcome, res.Err)
	}

	body, err := os.ReadFile(filepath.Join(cfg.Dir, "nested/dir/task.log"))
	if err != nil {
		t.Fatalf("log not created (parent dirs must be made on demand): %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Errorf("log = %q, want both stdout and stderr", got)
	}
}

func TestRunAppendsToLogFile(t *testing.T) {
	cfg, task := fixture(t, config.Task{
		Name: "appends", Command: "echo line", Log: "a.log",
	})
	r := New(DefaultGrace)
	r.Run(context.Background(), cfg, task)
	r.Run(context.Background(), cfg, task)

	body, err := os.ReadFile(filepath.Join(cfg.Dir, "a.log"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "line"); n != 2 {
		t.Errorf("log has %d lines, want 2 (runs must append, not truncate)", n)
	}
}

func TestRunFailsWhenLogCannotBeOpened(t *testing.T) {
	cfg, task := fixture(t, config.Task{
		Name: "unwritable", Command: "echo hi", Log: "blocked/x.log",
	})
	// Make "blocked" a FILE so creating it as a directory must fail.
	if err := os.WriteFile(filepath.Join(cfg.Dir, "blocked"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := New(DefaultGrace).Run(context.Background(), cfg, task)
	if res.Outcome != OutcomeFail {
		t.Errorf("Outcome = %q, want fail", res.Outcome)
	}
	if res.Err == nil {
		t.Error("Err should explain why the run never started")
	}
}

func TestRunTimesOutAndKillsStubbornChild(t *testing.T) {
	// This shell traps SIGTERM and keeps going, so only SIGKILL stops it.
	cfg, task := fixture(t, config.Task{
		Name:    "stubborn",
		Command: "trap '' TERM; sleep 30",
		Timeout: config.Duration(200 * time.Millisecond),
	})

	start := time.Now()
	res := New(300 * time.Millisecond).Run(context.Background(), cfg, task)
	elapsed := time.Since(start)

	if res.Outcome != OutcomeTimeout {
		t.Errorf("Outcome = %q, want timeout", res.Outcome)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v; SIGKILL escalation did not happen", elapsed)
	}
}

func TestRunKillsGrandchildren(t *testing.T) {
	cfg, task := fixture(t, config.Task{Name: "family", Command: "sleep 30 & wait"})
	dir := cfg.Dir
	markerCmd := "sh -c 'sleep 30; echo alive > " + filepath.Join(dir, "marker") + "' & wait"
	cfg.Tasks[0].Command = markerCmd
	cfg.Tasks[0].Timeout = config.Duration(200 * time.Millisecond)
	task = &cfg.Tasks[0]

	res := New(300 * time.Millisecond).Run(context.Background(), cfg, task)
	if res.Outcome != OutcomeTimeout {
		t.Fatalf("Outcome = %q, want timeout", res.Outcome)
	}

	// Give any survivor time to write its marker.
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "marker")); err == nil {
		t.Error("grandchild survived; the whole process group must be killed")
	}
}

func TestRunUsesConfigDirAsWorkingDirectory(t *testing.T) {
	cfg, task := fixture(t, config.Task{
		Name: "pwd", Command: "pwd > where.txt", Log: "",
	})
	if res := New(DefaultGrace).Run(context.Background(), cfg, task); res.Outcome != OutcomeOK {
		t.Fatalf("Outcome = %q, err %v", res.Outcome, res.Err)
	}
	body, err := os.ReadFile(filepath.Join(cfg.Dir, "where.txt"))
	if err != nil {
		t.Fatalf("command did not run in the config directory: %v", err)
	}
	// macOS reports /private/var/... for /var/..., so compare the base name.
	if !strings.Contains(string(body), filepath.Base(cfg.Dir)) {
		t.Errorf("pwd = %q, want it inside %q", strings.TrimSpace(string(body)), cfg.Dir)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
mkdir -p internal/runner
go test ./internal/runner/ -v
```
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Write the implementation**

Create `internal/runner/runner.go`:

```go
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joelhelbling/coucou/internal/config"
)

// DefaultGrace is how long a process group has to exit after SIGTERM before
// it is SIGKILLed.
const DefaultGrace = 5 * time.Second

type Outcome string

const (
	OutcomeOK       Outcome = "ok"
	OutcomeFail     Outcome = "fail"
	OutcomeTimeout  Outcome = "timeout"
	OutcomeReplaced Outcome = "replaced"
)

// Result is what one execution produced.
type Result struct {
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	Outcome    Outcome
	Err        error
}

// Runner executes a single task. It is an interface so the engine can be
// tested without spawning processes.
type Runner interface {
	Run(ctx context.Context, cfg *config.Config, t *config.Task) Result
}

type runner struct {
	grace time.Duration
	// stream, when set, receives output instead of the task's log file.
	// "coucou run" uses it so a manual run streams to the terminal.
	stream *os.File
}

// New returns a Runner that waits grace between SIGTERM and SIGKILL and
// writes output to each task's log (or /dev/null).
func New(grace time.Duration) Runner {
	if grace <= 0 {
		grace = DefaultGrace
	}
	return &runner{grace: grace}
}

// NewStreaming returns a Runner that sends output to out rather than to the
// task's log file.
func NewStreaming(grace time.Duration, out *os.File) Runner {
	if grace <= 0 {
		grace = DefaultGrace
	}
	return &runner{grace: grace, stream: out}
}

func (r *runner) Run(ctx context.Context, cfg *config.Config, t *config.Task) Result {
	res := Result{StartedAt: time.Now()}

	out, err := r.openOutput(cfg, t)
	if err != nil {
		res.FinishedAt = time.Now()
		res.Outcome = OutcomeFail
		res.ExitCode = -1
		res.Err = err
		return res
	}
	// Never close a caller-owned stream such as os.Stdout.
	if r.stream == nil {
		defer out.Close()
	}

	cmd := exec.Command(t.Shell, "-c", t.Command)
	cmd.Dir = cfg.Dir
	cmd.Env = os.Environ()
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	// Put the child in its own process group so timeouts can signal the
	// whole family, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		res.FinishedAt = time.Now()
		res.Outcome = OutcomeFail
		res.ExitCode = -1
		res.Err = fmt.Errorf("cannot start %s: %w", t.Shell, err)
		return res
	}

	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timeout <-chan time.Time
	if d := t.Timeout.Std(); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		timeout = timer.C
	}

	timedOut := false
	select {
	case err = <-done:
	case <-timeout:
		timedOut = true
		terminateGroup(pgid, r.grace)
		err = <-done
	case <-ctx.Done():
		terminateGroup(pgid, r.grace)
		err = <-done
		res.FinishedAt = time.Now()
		res.Outcome = OutcomeReplaced
		res.ExitCode = -1
		return res
	}

	res.FinishedAt = time.Now()
	res.ExitCode = exitCode(err)

	switch {
	case timedOut:
		res.Outcome = OutcomeTimeout
		res.Err = fmt.Errorf("timed out after %s", t.Timeout)
	case err == nil:
		res.Outcome = OutcomeOK
	default:
		res.Outcome = OutcomeFail
		res.Err = err
	}
	return res
}

// terminateGroup signals the whole process group: SIGTERM, then SIGKILL
// after the grace period.
//
// It deliberately does NOT watch the done channel. Only the caller receives
// from done, so a helper that consumed the value would deadlock the caller.
// Sending SIGKILL to a group that has already exited simply returns an error
// we ignore, so waiting out the full grace period is harmless.
func terminateGroup(pgid int, grace time.Duration) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(grace)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// openOutput returns the destination for stdout and stderr. With no log
// configured both go to /dev/null: they must never be inherited, because a
// child writing to the TUI's terminal would corrupt the display.
func (r *runner) openOutput(cfg *config.Config, t *config.Task) (*os.File, error) {
	if r.stream != nil {
		return r.stream, nil
	}
	path := cfg.LogPath(t)
	if path == "" {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("cannot open %s: %w", os.DevNull, err)
		}
		return f, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create log directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open log %s: %w", path, err)
	}
	return f, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/runner/ -v`
Expected: PASS, including the stubborn-child and grandchild tests

- [ ] **Step 5: Commit**

```bash
git add internal/runner/
git commit -m "feat(runner): execute tasks with process groups, timeouts, and log files"
```

---

### Task 10: State and instance lock

**Files:**
- Create: `internal/state/state.go`, `internal/state/lock.go`
- Test: `internal/state/state_test.go`, `internal/state/lock_test.go`

**Interfaces:**
- Consumes: `runner.Outcome` from Task 9
- Produces:
  - `const HistoryLimit = 12`
  - `type RunRecord struct { At time.Time; Outcome string; DurationMS int64 }`
  - `type LastRun struct { StartedAt, FinishedAt time.Time; ExitCode int; Outcome string }`
  - `type TaskState struct { Disabled bool; LastRun *LastRun; History []RunRecord }`
  - `type State struct { Version int; Tasks map[string]*TaskState }`
  - `func Load(dir string) (*State, error)` — never fails on a corrupt file; renames it to `state.json.bak` and returns a fresh state
  - `func (s *State) Save(dir string) error` — atomic
  - `func (s *State) Get(name string) *TaskState` — creates on demand
  - `func (s *State) Record(name string, r RunRecord, last LastRun)` — appends and trims the ring
  - `func AcquireLock(dir, configPath string, force bool) (*Lock, error)`, `func (l *Lock) Release() error`, `type Lock struct { PID int; StartedAt time.Time; Path string }`

- [ ] **Step 1: Write the failing state test**

Create `internal/state/state_test.go`:

```go
package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingReturnsFreshState(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Tasks == nil {
		t.Error("Tasks map should be initialized")
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)

	at := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	s.Record("xkcd",
		RunRecord{At: at, Outcome: "ok", DurationMS: 1400},
		LastRun{StartedAt: at, FinishedAt: at.Add(1400 * time.Millisecond),
			ExitCode: 0, Outcome: "ok"})
	s.Get("xkcd").Disabled = true

	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ts := reloaded.Get("xkcd")
	if !ts.Disabled {
		t.Error("Disabled did not survive the round trip")
	}
	if ts.LastRun == nil || ts.LastRun.ExitCode != 0 {
		t.Errorf("LastRun did not survive: %+v", ts.LastRun)
	}
	if len(ts.History) != 1 {
		t.Errorf("History has %d entries, want 1", len(ts.History))
	}
}

func TestHistoryRingEvicts(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	base := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	for i := 0; i < HistoryLimit+5; i++ {
		s.Record("t",
			RunRecord{At: base.Add(time.Duration(i) * time.Minute),
				Outcome: "ok", DurationMS: int64(i)},
			LastRun{Outcome: "ok"})
	}

	h := s.Get("t").History
	if len(h) != HistoryLimit {
		t.Fatalf("History has %d entries, want %d", len(h), HistoryLimit)
	}
	// The oldest entries must be the ones evicted.
	if h[0].DurationMS != 5 {
		t.Errorf("oldest kept entry has DurationMS %d, want 5", h[0].DurationMS)
	}
	if h[len(h)-1].DurationMS != int64(HistoryLimit+4) {
		t.Errorf("newest entry has DurationMS %d, want %d",
			h[len(h)-1].DurationMS, HistoryLimit+4)
	}
}

func TestLoadCorruptFileBacksUpAndStartsFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load must not fail on a corrupt file: %v", err)
	}
	if len(s.Tasks) != 0 {
		t.Errorf("expected a fresh state, got %d tasks", len(s.Tasks))
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.bak")); err != nil {
		t.Error("corrupt state should be preserved as state.json.bak")
	}
}

func TestLoadFutureVersionBacksUpAndStartsFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"version": 99, "tasks": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Version != Version {
		t.Errorf("Version = %d, want %d", s.Version, Version)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.bak")); err != nil {
		t.Error("future-version state should be preserved as state.json.bak")
	}
}

func TestSaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", ".coucou")
	s := &State{Version: Version, Tasks: map[string]*TaskState{}}
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save should create the state directory: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
mkdir -p internal/state
go test ./internal/state/ -v
```
Expected: FAIL — `undefined: Load`

- [ ] **Step 3: Write the state implementation**

Create `internal/state/state.go`:

```go
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Version is the state schema version. A file with a higher version is
// treated as unreadable rather than guessed at.
const Version = 1

// HistoryLimit is how many outcomes are kept per task for the sparkline.
const HistoryLimit = 12

const fileName = "state.json"

// RunRecord is one entry in a task's history ring.
type RunRecord struct {
	At         time.Time `json:"at"`
	Outcome    string    `json:"outcome"`
	DurationMS int64     `json:"duration_ms"`
}

// LastRun is the detail shown for the most recent run.
type LastRun struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	ExitCode   int       `json:"exit_code"`
	Outcome    string    `json:"outcome"`
}

// TaskState is everything Coucou remembers about one task.
type TaskState struct {
	Disabled bool        `json:"disabled"`
	LastRun  *LastRun    `json:"last_run,omitempty"`
	History  []RunRecord `json:"history,omitempty"`
}

// State is the whole state file.
type State struct {
	Version int                   `json:"version"`
	Tasks   map[string]*TaskState `json:"tasks"`
}

// Load reads state from dir. It never fails because of bad content: a file
// that will not parse, or one written by a newer version, is renamed to
// state.json.bak and a fresh state is returned. Losing history beats
// refusing to start.
func Load(dir string) (*State, error) {
	fresh := &State{Version: Version, Tasks: map[string]*TaskState{}}
	path := filepath.Join(dir, fileName)

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fresh, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil || s.Version > Version {
		if renameErr := os.Rename(path, path+".bak"); renameErr != nil {
			return nil, fmt.Errorf("cannot set aside unreadable %s: %w", path, renameErr)
		}
		return fresh, nil
	}

	if s.Tasks == nil {
		s.Tasks = map[string]*TaskState{}
	}
	s.Version = Version
	return &s, nil
}

// Save writes state atomically: a temp file in the same directory, then a
// rename, so a crash mid-write cannot leave a truncated file.
func (s *State) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create state directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("cannot create temp state file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, fileName))
}

// Get returns the state for a task, creating it if absent.
func (s *State) Get(name string) *TaskState {
	if s.Tasks == nil {
		s.Tasks = map[string]*TaskState{}
	}
	ts, ok := s.Tasks[name]
	if !ok {
		ts = &TaskState{}
		s.Tasks[name] = ts
	}
	return ts
}

// Record appends a run to the history ring and updates the last run.
func (s *State) Record(name string, r RunRecord, last LastRun) {
	ts := s.Get(name)
	ts.History = append(ts.History, r)
	if len(ts.History) > HistoryLimit {
		ts.History = ts.History[len(ts.History)-HistoryLimit:]
	}
	l := last
	ts.LastRun = &l
}
```

- [ ] **Step 4: Write the failing lock test**

Create `internal/state/lock_test.go`:

```go
package state

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	dir := t.TempDir()

	l, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if l.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", l.PID, os.Getpid())
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); err != nil {
		t.Errorf("lock file not created: %v", err)
	}

	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); !os.IsNotExist(err) {
		t.Error("lock file should be removed on release")
	}
}

func TestAcquireRefusesWhenHeldByLiveProcess(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	_, err = AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err == nil {
		t.Fatal("expected the second acquisition to fail")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("error %q should name the holding pid", err)
	}
}

func TestAcquireBreaksStaleLock(t *testing.T) {
	dir := t.TempDir()

	// A process that has certainly exited.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := cmd.Process.Pid

	body := `{"pid": ` + strconv.Itoa(deadPID) +
		`, "started_at": "2026-07-29T14:00:00Z", "config_path": "/proj/.coucou.yaml"}`
	if err := os.WriteFile(filepath.Join(dir, "lock"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatalf("a lock held by a dead pid must be broken automatically: %v", err)
	}
	defer l.Release()
}

func TestAcquireBreaksCorruptLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lock"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatalf("a corrupt lock should be treated as stale: %v", err)
	}
	defer l.Release()
}

func TestAcquireForceBreaksLiveLock(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	second, err := AcquireLock(dir, "/proj/.coucou.yaml", true)
	if err != nil {
		t.Fatalf("force should break a live lock: %v", err)
	}
	defer second.Release()
}
```

- [ ] **Step 5: Run the lock test to verify it fails**

Run: `go test ./internal/state/ -run TestAcquire -v`
Expected: FAIL — `undefined: AcquireLock`

- [ ] **Step 6: Write the lock implementation**

Create `internal/state/lock.go`:

```go
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const lockName = "lock"

// Lock is an exclusive claim on scheduling one config. Two schedulers on one
// config would fire every task twice, so this is a correctness guard rather
// than a convenience.
type Lock struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	ConfigPath string    `json:"config_path"`

	path string
}

// AcquireLock claims the lock in dir. It fails if a live process holds it,
// unless force is set. A lock whose pid is dead is broken automatically.
func AcquireLock(dir, configPath string, force bool) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create state directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, lockName)

	if existing, ok := readLock(path); ok && !force && processAlive(existing.PID) {
		return nil, fmt.Errorf(
			"coucou is already running for this config (pid %d, since %s)\n"+
				"Use --force to take over.",
			existing.PID, existing.StartedAt.Format("15:04"))
	}

	l := &Lock{
		PID:        os.Getpid(),
		StartedAt:  time.Now(),
		ConfigPath: configPath,
		path:       path,
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("cannot write lock %s: %w", path, err)
	}
	return l, nil
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	err := os.Remove(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// readLock returns the lock on disk. ok is false when it is absent or
// unreadable; an unreadable lock is treated as stale.
func readLock(path string) (*Lock, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, false
	}
	return &l, true
}

// processAlive reports whether pid exists, using signal 0.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
```

- [ ] **Step 7: Run all state tests**

Run: `go test ./internal/state/ -race -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/state/
git commit -m "feat(state): add atomic state file, history ring, and instance lock"
```

---

### Task 11: Engine

This is the heart of the plan. It owns the tick, the two policies, and the event stream that Plan 2's TUI will consume.

**Files:**
- Create: `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `config.Config`, `clock.Clock`, `runner.Runner`, `state.State` from Tasks 6, 8, 9, 10
- Produces:
  - `type Event struct { Kind EventKind; Task string; Result *runner.Result }` with `EventStarted`, `EventFinished`, `EventSkipped`, `EventOverdue`
  - `type Engine struct{ ... }`
  - `func New(cfg *config.Config, st *state.State, r runner.Runner, clk clock.Clock) *Engine`
  - `func (e *Engine) Events() <-chan Event`
  - `func (e *Engine) Start()` — applies missed-run policies and computes the first `next_at` for every task
  - `func (e *Engine) Tick()` — evaluates one tick; called by a `time.Ticker` in production and directly in tests
  - `func (e *Engine) NextAt(name string) time.Time`
  - `func (e *Engine) Running(name string) bool`
  - `func (e *Engine) Overdue(name string) bool`
  - `func (e *Engine) Wait()` — blocks until no run is in flight, for tests and shutdown

- [ ] **Step 1: Write the failing test**

Create `internal/engine/engine_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
mkdir -p internal/engine
go test ./internal/engine/ -v
```
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Write the implementation**

Create `internal/engine/engine.go`:

```go
package engine

import (
	"context"
	"sync"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/schedule"
	"github.com/joelhelbling/coucou/internal/state"
)

// OutcomeSkipped is recorded when an occurrence was dropped because the
// previous run was still going. It is not a runner outcome because no
// process ever started.
const OutcomeSkipped = "skipped"

type EventKind int

const (
	EventStarted EventKind = iota
	EventFinished
	EventSkipped
	EventOverdue
)

// Event is what the engine tells its subscriber. The TUI consumes these; the
// engine itself knows nothing about terminals.
type Event struct {
	Kind   EventKind
	Task   string
	Result *runner.Result
}

// taskRuntime is the engine's in-memory bookkeeping for one task.
type taskRuntime struct {
	nextAt  time.Time
	overdue bool
	running bool
	cancel  context.CancelFunc
	queued  bool // on_overlap: queue, depth 1
}

// Engine owns scheduling. It has no terminal dependency and communicates
// only through its event channel, which is the seam a future daemon uses.
type Engine struct {
	cfg    *config.Config
	state  *state.State
	runner runner.Runner
	clock  clock.Clock

	mu       sync.Mutex
	rt       map[string]*taskRuntime
	lastTick time.Time

	events chan Event
	wg     sync.WaitGroup
}

func New(cfg *config.Config, st *state.State, r runner.Runner, clk clock.Clock) *Engine {
	return &Engine{
		cfg:    cfg,
		state:  st,
		runner: r,
		clock:  clk,
		rt:     make(map[string]*taskRuntime, len(cfg.Tasks)),
		events: make(chan Event, 64),
	}
}

func (e *Engine) Events() <-chan Event { return e.events }

// Start computes the first next_at for every task and applies each task's
// missed-run policy for anything already overdue.
func (e *Engine) Start() {
	now := e.clock.Now()
	e.mu.Lock()
	e.lastTick = now
	for i := range e.cfg.Tasks {
		t := &e.cfg.Tasks[i]
		e.rt[t.Name] = &taskRuntime{}
		e.scheduleNextLocked(t, now)
	}
	e.mu.Unlock()

	e.applyMissedPolicies(now)
}

// Tick evaluates one scheduling moment. Production drives it from a 1Hz
// ticker; tests call it directly.
func (e *Engine) Tick() {
	now := e.clock.Now()

	e.mu.Lock()
	// A large backward step invalidates every cached next_at.
	if now.Before(e.lastTick) {
		for i := range e.cfg.Tasks {
			e.scheduleNextLocked(&e.cfg.Tasks[i], now)
		}
	}
	e.lastTick = now
	e.mu.Unlock()

	e.applyMissedPolicies(now)

	for i := range e.cfg.Tasks {
		t := &e.cfg.Tasks[i]

		e.mu.Lock()
		rt := e.rt[t.Name]
		disabled := e.state.Get(t.Name).Disabled
		due := rt != nil && !rt.nextAt.IsZero() && !now.Before(rt.nextAt)
		running := rt != nil && rt.running
		e.mu.Unlock()

		if rt == nil || disabled || !due {
			continue
		}

		if running {
			e.handleOverlap(t, now)
			continue
		}
		e.dispatch(t, now)
	}
}

// applyMissedPolicies handles due times that passed without a run — at
// startup, after a suspend, or after a clock step. It is deliberately not
// startup-only.
func (e *Engine) applyMissedPolicies(now time.Time) {
	for i := range e.cfg.Tasks {
		t := &e.cfg.Tasks[i]

		e.mu.Lock()
		rt := e.rt[t.Name]
		if rt == nil || rt.running || e.state.Get(t.Name).Disabled {
			e.mu.Unlock()
			continue
		}
		var lastFinish time.Time
		if ls := e.state.Get(t.Name).LastRun; ls != nil {
			lastFinish = ls.FinishedAt
		}
		missed := e.isMissedLocked(t, rt, now, lastFinish)
		e.mu.Unlock()

		if !missed {
			continue
		}

		switch t.MissedRuns {
		case config.MissedCatchUp:
			e.dispatch(t, now)
		case config.MissedReport:
			e.mu.Lock()
			rt.overdue = true
			e.scheduleNextLocked(t, now)
			e.mu.Unlock()
			e.emit(Event{Kind: EventOverdue, Task: t.Name})
		case config.MissedIgnore:
			e.mu.Lock()
			e.scheduleNextLocked(t, now)
			e.mu.Unlock()
		}
	}
}

// isMissedLocked reports whether a due time passed without a run. Callers
// must hold e.mu.
//
// Two independent signals are needed, and neither alone is sufficient:
//
//  1. The cached next_at is well in the past. This catches a suspend or a
//     clock jump while Coucou was already running, including for a task
//     that has never run before.
//
//  2. The first occurrence after the previous run's finish time is already
//     behind us. This catches a restart, where next_at was computed forward
//     from "now" at startup and is therefore in the future even though an
//     occurrence was missed while we were closed.
//
// Signal 2 uses Next(lastFinish, lastFinish), which works for both kinds:
// cron returns the first occurrence after that instant, and an interval
// returns lastFinish plus its duration.
func (e *Engine) isMissedLocked(t *config.Task, rt *taskRuntime, now, lastFinish time.Time) bool {
	if t.Parsed == nil {
		return false
	}

	// Signal 1: the cached next_at is more than a tick behind.
	if !rt.nextAt.IsZero() && now.Sub(rt.nextAt) > time.Minute {
		return true
	}

	// Signal 2: an occurrence elapsed while we were not running. A task that
	// has never run is not overdue -- a fresh config must not fire
	// everything the moment it is first loaded.
	if lastFinish.IsZero() {
		return false
	}
	due := t.Parsed.Next(lastFinish, lastFinish)
	return !due.IsZero() && due.Before(now)
}

func (e *Engine) handleOverlap(t *config.Task, now time.Time) {
	switch t.OnOverlap {
	case config.OverlapSkip:
		e.mu.Lock()
		e.scheduleNextLocked(t, now)
		e.mu.Unlock()
		e.state.Record(t.Name,
			state.RunRecord{At: now, Outcome: OutcomeSkipped},
			state.LastRun{Outcome: OutcomeSkipped, ExitCode: -1})
		e.saveState()
		e.emit(Event{Kind: EventSkipped, Task: t.Name})

	case config.OverlapQueue:
		e.mu.Lock()
		e.rt[t.Name].queued = true
		e.scheduleNextLocked(t, now)
		e.mu.Unlock()

	case config.OverlapReplace:
		e.mu.Lock()
		if cancel := e.rt[t.Name].cancel; cancel != nil {
			cancel()
		}
		e.rt[t.Name].queued = true
		e.scheduleNextLocked(t, now)
		e.mu.Unlock()
	}
}

// dispatch starts a run in its own goroutine.
func (e *Engine) dispatch(t *config.Task, now time.Time) {
	ctx, cancel := context.WithCancel(context.Background())

	e.mu.Lock()
	rt := e.rt[t.Name]
	if rt == nil || rt.running {
		e.mu.Unlock()
		cancel()
		return
	}
	rt.running = true
	rt.overdue = false
	rt.cancel = cancel
	e.scheduleNextLocked(t, now)
	e.mu.Unlock()

	e.emit(Event{Kind: EventStarted, Task: t.Name})

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancel()

		res := e.runner.Run(ctx, e.cfg, t)

		e.state.Record(t.Name,
			state.RunRecord{
				At:         res.StartedAt,
				Outcome:    string(res.Outcome),
				DurationMS: res.FinishedAt.Sub(res.StartedAt).Milliseconds(),
			},
			state.LastRun{
				StartedAt:  res.StartedAt,
				FinishedAt: res.FinishedAt,
				ExitCode:   res.ExitCode,
				Outcome:    string(res.Outcome),
			})
		e.saveState()

		e.mu.Lock()
		rt := e.rt[t.Name]
		rt.running = false
		rt.cancel = nil
		// An interval anchors on the finish time, so recompute now.
		if t.Parsed != nil && t.Parsed.Kind() == schedule.KindInterval {
			rt.nextAt = t.Parsed.Next(res.FinishedAt, e.clock.Now())
		}
		queued := rt.queued
		rt.queued = false
		e.mu.Unlock()

		r := res
		e.emit(Event{Kind: EventFinished, Task: t.Name, Result: &r})

		if queued {
			e.dispatch(t, e.clock.Now())
		}
	}()
}

// scheduleNextLocked recomputes next_at. Callers must hold e.mu.
func (e *Engine) scheduleNextLocked(t *config.Task, now time.Time) {
	rt := e.rt[t.Name]
	if rt == nil || t.Parsed == nil {
		return
	}
	var prev time.Time
	if ls := e.state.Get(t.Name).LastRun; ls != nil {
		prev = ls.FinishedAt
	}
	rt.nextAt = t.Parsed.Next(prev, now)
}

func (e *Engine) saveState() {
	// A state file we cannot write is a degraded scheduler, not a dead one.
	_ = e.state.Save(e.cfg.StateDirPath())
}

func (e *Engine) emit(ev Event) {
	select {
	case e.events <- ev:
	default: // never block scheduling on a slow subscriber
	}
}

// NextAt reports when a task is next due.
func (e *Engine) NextAt(name string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rt := e.rt[name]; rt != nil {
		return rt.nextAt
	}
	return time.Time{}
}

// Running reports whether a task has a run in flight.
func (e *Engine) Running(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.rt[name]
	return rt != nil && rt.running
}

// Overdue reports whether a task missed a due time under the report policy.
func (e *Engine) Overdue(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.rt[name]
	return rt != nil && rt.overdue
}

// Wait blocks until no run is in flight.
func (e *Engine) Wait() { e.wg.Wait() }

// Stop cancels every running task and closes the event channel.
func (e *Engine) Stop() {
	e.mu.Lock()
	for _, rt := range e.rt {
		if rt.cancel != nil {
			rt.cancel()
		}
	}
	e.mu.Unlock()
	e.wg.Wait()
	close(e.events)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/engine/ -race -v`
Expected: PASS with no race warnings

- [ ] **Step 5: Commit**

```bash
git add internal/engine/
git commit -m "feat(engine): add tick loop, overlap and missed-run policies, events"
```

---

### Task 12: CLI

**Files:**
- Create: `internal/cli/cli.go`, `main.go`
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: everything from Tasks 4, 6, 7, 8, 9, 10
- Produces: `func Run(args []string, stdout, stderr io.Writer, cwd string) int` — the whole CLI as a testable function returning an exit code. `main.go` is a four-line wrapper.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/cli_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func project(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".coucou.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func run(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut, dir)
	return code, out.String(), errOut.String()
}

const goodConfig = `
tasks:
  - name: xkcd
    description: Fetch latest xkcd comic
    command: "true"
    schedule: "@daily at 17:00"
  - name: poll
    command: "true"
    schedule: "@every 30m"
`

func TestValidateSucceeds(t *testing.T) {
	dir := project(t, goodConfig)
	code, _, errOut := run(t, dir, "validate")
	if code != 0 {
		t.Errorf("exit %d, want 0 (stderr: %s)", code, errOut)
	}
}

func TestValidateFailsWithAllErrors(t *testing.T) {
	dir := project(t, `
tasks:
  - name: broken
    schedule: "nope"
`)
	code, _, errOut := run(t, dir, "validate")
	if code == 0 {
		t.Fatal("exit 0, want non-zero")
	}
	for _, want := range []string{"command", "nope"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

func TestMissingConfigIsAnError(t *testing.T) {
	code, _, errOut := run(t, t.TempDir(), "validate")
	if code == 0 {
		t.Fatal("exit 0, want non-zero")
	}
	if !strings.Contains(errOut, ".coucou.yaml") {
		t.Errorf("stderr should name the file looked for:\n%s", errOut)
	}
}

func TestListPrintsTasks(t *testing.T) {
	dir := project(t, goodConfig)
	code, out, errOut := run(t, dir, "list")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, errOut)
	}
	for _, want := range []string{"xkcd", "poll", "@daily at 17:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestNextPrintsSoonest(t *testing.T) {
	dir := project(t, goodConfig)
	code, out, errOut := run(t, dir, "next")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, errOut)
	}
	// "poll" runs every 30m, so it is always sooner than a daily 17:00 task
	// unless the clock is within 30 minutes of 17:00. Assert on shape only.
	if !strings.Contains(out, "xkcd") && !strings.Contains(out, "poll") {
		t.Errorf("output should name a task:\n%s", out)
	}
}

func TestRunExecutesOneTask(t *testing.T) {
	dir := project(t, `
tasks:
  - name: writer
    command: "echo hello > proof.txt"
    schedule: "@daily"
`)
	code, _, errOut := run(t, dir, "run", "writer")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "proof.txt")); err != nil {
		t.Errorf("task did not run: %v", err)
	}
}

func TestRunRecordsNothingAndTakesNoLock(t *testing.T) {
	dir := project(t, `
tasks:
  - name: writer
    command: "true"
    schedule: "@daily"
`)
	if code, _, errOut := run(t, dir, "run", "writer"); code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, ".coucou", "state.json")); err == nil {
		t.Error("coucou run must not write state")
	}
	if _, err := os.Stat(filepath.Join(dir, ".coucou", "lock")); err == nil {
		t.Error("coucou run must not take the lock")
	}
}

func TestRunPropagatesExitCode(t *testing.T) {
	dir := project(t, `
tasks:
  - name: failer
    command: "exit 7"
    schedule: "@daily"
`)
	code, _, _ := run(t, dir, "run", "failer")
	if code != 7 {
		t.Errorf("exit %d, want 7", code)
	}
}

func TestRunUnknownTask(t *testing.T) {
	dir := project(t, goodConfig)
	code, _, errOut := run(t, dir, "run", "nope")
	if code == 0 {
		t.Fatal("exit 0, want non-zero")
	}
	if !strings.Contains(errOut, "nope") {
		t.Errorf("stderr should name the unknown task:\n%s", errOut)
	}
}

func TestConfigFlagOverrides(t *testing.T) {
	dir := project(t, goodConfig)
	other := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(other, []byte("tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, dir, "--config", other, "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, "xkcd") {
		t.Error("--config was ignored; the local config was loaded")
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, t.TempDir(), "--version")
	if code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("--version printed nothing")
	}
}

func TestUnknownSubcommand(t *testing.T) {
	code, _, errOut := run(t, t.TempDir(), "frobnicate")
	if code == 0 {
		t.Fatal("exit 0, want non-zero")
	}
	if !strings.Contains(errOut, "frobnicate") {
		t.Errorf("stderr should name the unknown subcommand:\n%s", errOut)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
mkdir -p internal/cli
go test ./internal/cli/ -v
```
Expected: FAIL — `undefined: Run`

- [ ] **Step 3: Write the implementation**

Create `internal/cli/cli.go`:

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/runner"
)

// Version is the build version.
const Version = "0.1.0-dev"

const usage = `coucou - a project-scoped TUI task scheduler

Usage:
  coucou [flags]              open the TUI
  coucou validate             parse and check the config
  coucou run NAME             run one task in the foreground
  coucou list                 print tasks and next run times
  coucou next                 print what fires soonest

Flags:
  --config PATH  config file (default ./.coucou.yaml)
  --ascii        avoid box-drawing and braille glyphs
  --force        break an existing instance lock
  --version      print the version
`

// Run executes the CLI and returns a process exit code. Taking writers and a
// working directory as arguments is what makes it testable.
func Run(args []string, stdout, stderr io.Writer, cwd string) int {
	fs := flag.NewFlagSet("coucou", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	configPath := fs.String("config", "", "path to the config file")
	showVersion := fs.Bool("version", false, "print the version")
	fs.Bool("ascii", false, "avoid box-drawing and braille glyphs")
	fs.Bool("force", false, "break an existing instance lock")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "coucou "+Version)
		return 0
	}

	rest := fs.Args()
	command := "tui"
	if len(rest) > 0 {
		command = rest[0]
		rest = rest[1:]
	}

	switch command {
	case "validate", "list", "next", "run":
	case "tui":
		fmt.Fprintln(stderr, "the TUI is not implemented yet; try 'coucou list'")
		return 1
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n%s", command, usage)
		return 2
	}

	path, err := config.Discover(cwd, *configPath, os.Getenv("COUCOU_CONFIG"))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	switch command {
	case "validate":
		fmt.Fprintf(stdout, "%s: %d task(s), no problems found\n", path, len(cfg.Tasks))
		return 0
	case "list":
		return listTasks(cfg, stdout)
	case "next":
		return nextTask(cfg, stdout)
	case "run":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: coucou run NAME")
			return 2
		}
		return runOne(cfg, rest[0], stdout, stderr)
	}
	return 0
}

func listTasks(cfg *config.Config, stdout io.Writer) int {
	now := clock.Real().Now()
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE\tNEXT RUN")
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		fmt.Fprintf(w, "%s\t%s\t%s\n",
			t.Name, t.Schedule, formatNext(t.Parsed.Next(time.Time{}, now)))
	}
	w.Flush()
	return 0
}

func nextTask(cfg *config.Config, stdout io.Writer) int {
	now := clock.Real().Now()

	var soonestName string
	var soonest time.Time
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		at := t.Parsed.Next(time.Time{}, now)
		if at.IsZero() {
			continue
		}
		if soonest.IsZero() || at.Before(soonest) {
			soonest, soonestName = at, t.Name
		}
	}

	if soonestName == "" {
		fmt.Fprintln(stdout, "no tasks are scheduled")
		return 0
	}
	fmt.Fprintf(stdout, "%s in %s (%s)\n",
		soonestName, soonest.Sub(now).Round(time.Second), formatNext(soonest))
	return 0
}

// runOne executes a single task in the foreground. It deliberately takes no
// lock and records no state: this is the user testing a command, not an
// occurrence of the schedule.
func runOne(cfg *config.Config, name string, stdout, stderr io.Writer) int {
	var task *config.Task
	for i := range cfg.Tasks {
		if cfg.Tasks[i].Name == name {
			task = &cfg.Tasks[i]
			break
		}
	}
	if task == nil {
		fmt.Fprintf(stderr, "no task named %q\n", name)
		return 1
	}

	// Stream to the terminal rather than to the task's log file: this is a
	// manual test run, not an occurrence of the schedule.
	res := runner.NewStreaming(runner.DefaultGrace, os.Stdout).
		Run(context.Background(), cfg, task)
	if res.Err != nil {
		fmt.Fprintln(stderr, res.Err)
	}
	if res.ExitCode < 0 {
		return 1
	}
	return res.ExitCode
}

func formatNext(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("Jan _2 15:04")
}
```

- [ ] **Step 4: Write main.go**

Create `main.go`:

```go
package main

import (
	"os"

	"github.com/joelhelbling/coucou/internal/cli"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, cwd))
}
```

- [ ] **Step 5: Run the whole suite**

```bash
gofmt -l . && go vet ./... && go test ./... -race
```
Expected: no `gofmt` output, no vet findings, all tests PASS

- [ ] **Step 6: Build and smoke-test by hand**

```bash
go build -o coucou .
mkdir -p /tmp/coucou-smoke && cd /tmp/coucou-smoke
cat > .coucou.yaml <<'YAML'
tasks:
  - name: greet
    description: Say hello
    command: echo "hello from coucou"
    schedule: "@every 30m"
    log: log/greet.log
YAML
/Users/joelhelbling/code/ai/coucou/coucou validate
/Users/joelhelbling/code/ai/coucou/coucou list
/Users/joelhelbling/code/ai/coucou/coucou next
/Users/joelhelbling/code/ai/coucou/coucou run greet
cat log/greet.log
```
Expected: `validate` exits 0; `list` shows `greet`; `run greet` prints `hello from coucou` to the terminal and does **not** create `.coucou/state.json`.

- [ ] **Step 7: Commit**

```bash
cd /Users/joelhelbling/code/ai/coucou
git add internal/cli/ internal/runner/runner.go main.go
git commit -m "feat(cli): add validate, list, next, and run subcommands"
```

---

## What this plan does not cover

Deferred to Plan 2 (the TUI):

- The Bubble Tea model, the five screen regions, and responsive column shedding
- The spinner and its 10fps-while-running / 1Hz-while-idle cadence
- Key handling, the `L` pager handoff, and `d` disable
- Config file watching and hot reload (`fsnotify`)
- Instance lock acquisition at TUI startup and release on shutdown
- SIGTERM/SIGHUP shutdown, `q` confirmation while a run is in flight
- ASCII fallback, `NO_COLOR`, and non-TTY refusal

The engine, lock, and state APIs built here are what Plan 2 consumes. `AcquireLock` exists but is only *called* in Plan 2, since `coucou run` deliberately does not take it and there is no long-running process until the TUI exists.

## Open questions carried from the spec

- Whether `on_overlap: queue` needs a UI affordance beyond the row marker (Plan 2)
- ASCII substitutions for `▸ ✓ ✗ ·` and the spinner frames (Plan 2)
