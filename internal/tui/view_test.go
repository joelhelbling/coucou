package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/engine"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/state"
)

var now = time.Date(2026, 8, 11, 14, 32, 7, 0, time.UTC)

func snap() engine.Snapshot {
	return engine.Snapshot{
		At: now,
		Tasks: []engine.TaskView{
			{
				Name: "API Refresh", Description: "Fetch latest API changelog",
				Command: "curl -s https://example.com", Schedule: "@weekly on mon at 18:30",
				Shell: "/bin/sh", Log: "log/api.log", Timeout: 30 * time.Second,
				OnOverlap: config.OverlapSkip, MissedRuns: config.MissedCatchUp,
				NextAt: now.Add(96 * time.Hour),
				Last: &state.LastRun{
					StartedAt:  now.Add(-72 * time.Hour),
					FinishedAt: now.Add(-72*time.Hour + 1400*time.Millisecond),
					ExitCode:   0, Outcome: string(runner.OutcomeOK),
				},
				History: []state.RunRecord{
					{Outcome: string(runner.OutcomeOK)},
					{Outcome: string(runner.OutcomeFail)},
					{Outcome: string(runner.OutcomeOK)},
				},
			},
			{
				Name: "xkcd", Command: "curl -s https://xkcd.com/info.0.json",
				Schedule: "@daily at 17:00", Shell: "/bin/sh",
				OnOverlap: config.OverlapSkip, MissedRuns: config.MissedReport,
				NextAt: now.Add(3 * time.Hour),
				Last: &state.LastRun{
					StartedAt:  now.Add(-21 * time.Hour),
					FinishedAt: now.Add(-21*time.Hour + 900*time.Millisecond),
					ExitCode:   7, Outcome: string(runner.OutcomeFail),
				},
			},
		},
	}
}

// Columns must be shed in a fixed order so a pane that gets resized
// repeatedly degrades predictably rather than jittering.
func TestColumnShedding(t *testing.T) {
	th := UnicodeTheme(false)
	for _, tc := range []struct {
		width int
		want  []string
		gone  []string
	}{
		{100, []string{"NAME", "SCHEDULE", "NEXT RUN", "LAST RUN", "STATUS"}, nil},
		{68, []string{"NAME", "SCHEDULE", "NEXT RUN", "STATUS"}, []string{"LAST RUN"}},
		{52, []string{"NAME", "NEXT RUN", "STATUS"}, []string{"LAST RUN", "SCHEDULE"}},
		{34, []string{"NAME", "STATUS"}, []string{"LAST RUN", "SCHEDULE", "NEXT RUN"}},
	} {
		out := View(snap(), 0, tc.width, 40, 0, th)
		head := strings.SplitN(out, "\n", 5)[3]
		for _, w := range tc.want {
			if !strings.Contains(head, w) {
				t.Errorf("width %d: header %q missing %q", tc.width, head, w)
			}
		}
		for _, g := range tc.gone {
			if strings.Contains(head, g) {
				t.Errorf("width %d: header %q should have shed %q", tc.width, head, g)
			}
		}
	}
}

// NAME and STATUS survive every width; that is the invariant the whole
// shedding order exists to protect.
func TestNameAndStatusAlwaysSurvive(t *testing.T) {
	th := UnicodeTheme(false)
	for w := MinWidth; w <= 100; w++ {
		head := strings.SplitN(View(snap(), 0, w, 40, 0, th), "\n", 5)[3]
		if !strings.Contains(head, "NAME") || !strings.Contains(head, "STATUS") {
			t.Fatalf("width %d lost a mandatory column: %q", w, head)
		}
	}
}

func TestNoLineExceedsWidth(t *testing.T) {
	th := UnicodeTheme(false)
	for _, w := range []int{20, 34, 52, 68, 100} {
		for _, line := range strings.Split(View(snap(), 0, w, 40, 0, th), "\n") {
			if n := len([]rune(line)); n > w {
				t.Errorf("width %d: line of %d runes overflows: %q", w, n, line)
			}
		}
	}
}

// A render taller than its pane would scroll the alt screen and destroy the
// layout, so height is a hard bound, not a hint.
func TestNeverExceedsHeight(t *testing.T) {
	th := UnicodeTheme(false)
	for _, w := range []int{20, 34, 52, 68, 100} {
		for h := 3; h <= 40; h++ {
			got := len(strings.Split(View(snap(), 0, w, h, 0, th), "\n"))
			if got > h {
				t.Errorf("%dx%d rendered %d lines", w, h, got)
			}
		}
	}
}

func TestTooNarrow(t *testing.T) {
	if got := View(snap(), 0, 10, 40, 0, UnicodeTheme(false)); got != "pane too narrow" {
		t.Errorf("got %q", got)
	}
}

// A running task drives the status line, the row, and the spinner together.
func TestRunningTask(t *testing.T) {
	s := snap()
	s.Tasks[1].Running = true
	s.Tasks[1].StartedAt = now.Add(-3 * time.Second)

	out := View(s, 1, 68, 40, 2, UnicodeTheme(false))
	for _, want := range []string{"Running", "xkcd", "running now", "⠹"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestIdleStatus(t *testing.T) {
	if !strings.Contains(View(snap(), 0, 68, 40, 0, UnicodeTheme(false)), "Status  Idle") {
		t.Error("idle snapshot should show Idle")
	}
}

// The detail pane is sacrificed before the list: the list is why the window
// exists.
func TestShortPaneDropsDetail(t *testing.T) {
	th := UnicodeTheme(false)
	full := View(snap(), 0, 68, 40, 0, th)
	short := View(snap(), 0, 68, 7, 0, th)

	if !strings.Contains(full, "Fetch latest API changelog") {
		t.Fatal("tall pane should show the detail pane")
	}
	if strings.Contains(short, "Fetch latest API changelog") {
		t.Error("short pane should have dropped the detail pane")
	}
	if !strings.Contains(short, "xkcd") {
		t.Error("short pane must still show the task list")
	}
}

// "no log" appears exactly where a failure is read, so the fix is
// discoverable at the moment you need it.
func TestFailureWithoutLogSaysSo(t *testing.T) {
	out := View(snap(), 1, 68, 40, 0, UnicodeTheme(false))
	if !strings.Contains(out, "no log") {
		t.Errorf("a failed task with no log: should say so:\n%s", out)
	}
}

func TestFailureWithLogDoesNotSayNoLog(t *testing.T) {
	out := View(snap(), 0, 68, 40, 0, UnicodeTheme(false))
	if strings.Contains(out, "no log") {
		t.Error("a task with a log configured must not say 'no log'")
	}
}

// The ASCII theme exists for terminals that cannot render anything outside
// ASCII, so checking a list of known glyphs is not enough -- any non-ASCII
// byte at all is a bug, including in the key hints and field separators.
func TestASCIIThemeIsPureASCII(t *testing.T) {
	s := snap()
	s.Tasks[1].Running = true
	s.Tasks[1].StartedAt = now.Add(-3 * time.Second)

	for _, w := range []int{20, 34, 52, 68, 100} {
		out := View(s, 0, w, 40, 1, ASCIITheme(false))
		for _, r := range out {
			if r > 127 {
				t.Fatalf("width %d: ascii theme emitted %q in:\n%s", w, r, out)
			}
		}
	}
}

func TestSelectionMarker(t *testing.T) {
	out := View(snap(), 1, 68, 40, 0, UnicodeTheme(false))
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "xkcd") && strings.HasPrefix(line, "▸") {
			return
		}
	}
	t.Errorf("selected row not marked:\n%s", out)
}

func TestNeverRunTask(t *testing.T) {
	s := snap()
	s.Tasks[0].Last = nil
	s.Tasks[0].History = nil
	out := View(s, 0, 100, 40, 0, UnicodeTheme(false))
	if !strings.Contains(out, "never") {
		t.Errorf("a task that has never run should say so:\n%s", out)
	}
}

func TestElapsed(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{3 * time.Second, "0:03"},
		{90 * time.Second, "1:30"},
		{3 * time.Hour, "3:00:00"},
		{50 * time.Hour, "2d 2h 0m"},
		{-time.Second, "0:00"},
	} {
		if got := elapsed(tc.d); got != tc.want {
			t.Errorf("elapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFitEllipsizes(t *testing.T) {
	if got := fit("abcdefgh", 5, "…"); got != "abcd…" {
		t.Errorf("got %q", got)
	}
	if got := fit("ab", 5, "…"); got != "ab   " {
		t.Errorf("got %q", got)
	}
	// A multi-rune ASCII ellipsis must still land exactly on n columns.
	if got := fit("abcdefgh", 5, "..."); got != "ab..." {
		t.Errorf("ascii ellipsis: got %q", got)
	}
}
