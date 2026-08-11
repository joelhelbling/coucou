package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/joelhelbling/coucou/internal/engine"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/state"
)

// MinWidth is the narrowest pane worth drawing into. Below it there is no
// honest layout, so we say so rather than emit garbage.
const MinWidth = 20

// column identifies an optional column in shedding order. NAME and STATUS
// always survive; the rest are dropped widest-first as the pane narrows.
type column int

const (
	colNextRun column = iota
	colSchedule
	colLastRun
)

// shedOrder is the order columns are given up: LAST RUN goes first because
// its information also appears in STATUS, then SCHEDULE, then NEXT RUN.
var shedOrder = []column{colLastRun, colSchedule, colNextRun}

const (
	wSchedule = 14
	wNextRun  = 14
	wLastRun  = 14
	wStatus   = 10
	wGutter   = 2
)

// View renders the whole display. It is a pure function of its arguments:
// no clock, no terminal, no engine. That is what makes the degradation
// table testable.
func View(s engine.Snapshot, sel, width, height, tick int, th Theme) string {
	if width < MinWidth {
		return "pane too narrow"
	}

	rule := strings.Repeat(th.Rule, width)

	// Vertical space is budgeted rather than just written out: a display
	// taller than its pane would scroll the alt screen and destroy the
	// layout. The list is why the window exists, so it is paid first; the
	// detail pane takes whatever is left and is truncated if that is not
	// enough.
	lines := []string{
		header(s.At, width, th),
		statusLine(s, tick, th),
		rule,
	}
	lines = append(lines, splitLines(taskList(s, sel, width, tick, th))...)

	hintBlock := []string{rule, hints(width, th)}
	remaining := height - len(lines) - len(hintBlock)

	if remaining > 1 && sel >= 0 && sel < len(s.Tasks) {
		body := splitLines(detail(s.Tasks[sel], s.At, width, th))
		if len(body) > remaining-1 {
			body = body[:remaining-1]
		}
		lines = append(lines, rule)
		lines = append(lines, body...)
	}
	if height >= len(lines)+len(hintBlock) {
		lines = append(lines, hintBlock...)
	}

	// Final guard: never hand back more rows than the pane has, whatever
	// the arithmetic above did.
	if len(lines) > height && height > 0 {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// splitLines splits a rendered block, dropping the trailing empty element
// that a block ending in a newline produces.
func splitLines(s string) []string {
	out := strings.Split(s, "\n")
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func header(now time.Time, width int, th Theme) string {
	stamp := now.Format("2006-01-02 15:04:05")
	if width < len("Coucou")+len(stamp)+2 {
		stamp = now.Format("15:04:05")
	}
	return pad("Coucou", stamp, width, th.Ellipsis)
}

func statusLine(s engine.Snapshot, tick int, th Theme) string {
	for i := range s.Tasks {
		t := &s.Tasks[i]
		if !t.Running {
			continue
		}
		el := s.At.Sub(t.StartedAt).Truncate(time.Second)
		return fmt.Sprintf("Status  %s Running  %s   %s",
			th.Frame(tick), t.Name, elapsed(el))
	}
	return "Status  Idle"
}

// visibleColumns decides which optional columns fit, shedding in a fixed
// order so a pane that is resized repeatedly degrades predictably.
func visibleColumns(width, nameWidth int) map[column]bool {
	vis := map[column]bool{colNextRun: true, colSchedule: true, colLastRun: true}

	need := func() int {
		w := 2 + nameWidth + wGutter + wStatus
		if vis[colSchedule] {
			w += wSchedule + wGutter
		}
		if vis[colNextRun] {
			w += wNextRun + wGutter
		}
		if vis[colLastRun] {
			w += wLastRun + wGutter
		}
		return w
	}

	for _, c := range shedOrder {
		if need() <= width {
			break
		}
		vis[c] = false
	}
	return vis
}

func taskList(s engine.Snapshot, sel, width, tick int, th Theme) string {
	nameWidth := len("NAME")
	for i := range s.Tasks {
		if n := len(s.Tasks[i].Name); n > nameWidth {
			nameWidth = n
		}
	}
	vis := visibleColumns(width, nameWidth)

	// Names ellipsize only once every optional column is already gone.
	if !vis[colNextRun] && !vis[colSchedule] && !vis[colLastRun] {
		avail := width - 2 - wGutter - wStatus
		if avail > 3 && nameWidth > avail {
			nameWidth = avail
		}
	}

	var b strings.Builder
	row := func(marker, name, sched, next, last, status string) {
		b.WriteString(marker)
		b.WriteByte(' ')
		b.WriteString(fit(name, nameWidth, th.Ellipsis))
		if vis[colSchedule] {
			b.WriteString("  " + fit(sched, wSchedule, th.Ellipsis))
		}
		if vis[colNextRun] {
			b.WriteString("  " + fit(next, wNextRun, th.Ellipsis))
		}
		if vis[colLastRun] {
			b.WriteString("  " + fit(last, wLastRun, th.Ellipsis))
		}
		b.WriteString("  " + status)
		b.WriteByte('\n')
	}

	row(" ", "NAME", "SCHEDULE", "NEXT RUN", "LAST RUN", "STATUS")

	for i := range s.Tasks {
		t := &s.Tasks[i]
		marker := " "
		if i == sel {
			marker = th.Selected
		}
		row(marker, t.Name, t.Schedule,
			nextRunCell(t, s.At), lastRunCell(t), statusCell(t, s.At, tick, th))
	}
	return b.String()
}

func nextRunCell(t *engine.TaskView, now time.Time) string {
	switch {
	case t.Running:
		return "running now"
	case t.Disabled:
		return "disabled"
	case t.NextAt.IsZero():
		return "never"
	}
	return stamp(t.NextAt, now)
}

func lastRunCell(t *engine.TaskView) string {
	if t.Last == nil {
		return "never"
	}
	return t.Last.StartedAt.Format("Jan _2 15:04")
}

func statusCell(t *engine.TaskView, now time.Time, tick int, th Theme) string {
	if t.Running {
		el := now.Sub(t.StartedAt).Truncate(time.Second)
		return fmt.Sprintf("%s %s", th.Frame(tick), elapsed(el))
	}
	if t.Last == nil {
		return "-"
	}
	// The ASCII glyphs are already words ("ok", "FAIL"), so repeating the
	// outcome after them would just read "ok ok".
	if !th.Unicode {
		return outcomeGlyph(t.Last.Outcome, th)
	}
	return outcomeGlyph(t.Last.Outcome, th) + " " + t.Last.Outcome
}

func outcomeGlyph(outcome string, th Theme) string {
	switch runner.Outcome(outcome) {
	case runner.OutcomeOK:
		return th.OK
	case runner.OutcomeTimeout:
		return th.Timeout
	default:
		return th.Fail
	}
}

func detail(t engine.TaskView, now time.Time, width int, th Theme) string {
	var b strings.Builder

	state := "enabled"
	if t.Disabled {
		state = "disabled"
	}
	b.WriteString(pad(t.Name, fmt.Sprintf("%s%s%d runs", state, th.Sep, len(t.History)), width, th.Ellipsis))
	b.WriteByte('\n')
	if t.Description != "" {
		for _, line := range wrap(t.Description, width) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')

	// Values wrap under a hanging indent so a long command stays readable
	// in a narrow pane instead of overflowing it.
	const labelWidth = 10
	field := func(label, value string) {
		if value == "" {
			return
		}
		if width <= labelWidth+4 {
			// No room for the two-column form; stack instead.
			b.WriteString(label + "\n")
			for _, line := range wrap(value, width) {
				b.WriteString(line + "\n")
			}
			return
		}
		lines := wrap(value, width-labelWidth)
		b.WriteString(fmt.Sprintf("%-*s%s\n", labelWidth, label, lines[0]))
		for _, line := range lines[1:] {
			b.WriteString(strings.Repeat(" ", labelWidth) + line + "\n")
		}
	}

	field("Command", t.Command)

	// Composed values are joined with · rather than runs of spaces: wrap()
	// splits on whitespace, so multi-space separators would collapse to one
	// and the parts would run together.
	runAs := []string{t.Shell}
	if t.Timeout > 0 {
		runAs = append(runAs, fmt.Sprintf("timeout %s", t.Timeout))
	}
	if t.OnOverlap != "" {
		runAs = append(runAs, fmt.Sprintf("overlap %s", t.OnOverlap))
	}
	field("Run as", strings.Join(runAs, th.Sep))
	field("Schedule", t.Schedule)

	switch {
	case t.Running:
		field("Next run", "running now, started "+t.StartedAt.Format("15:04:05"))
	case !t.NextAt.IsZero():
		field("Next run", fmt.Sprintf("%s%sin %s",
			stamp(t.NextAt, now), th.Sep, elapsed(t.NextAt.Sub(now).Truncate(time.Second))))
	}

	if t.Last != nil {
		d := t.Last.FinishedAt.Sub(t.Last.StartedAt).Truncate(time.Millisecond)
		parts := []string{
			t.Last.StartedAt.Format("Jan _2 15:04"),
			fmt.Sprintf("exit %d", t.Last.ExitCode),
			d.String(),
		}
		// Surfacing "no log" exactly where a failure is read is the point:
		// the fix is discoverable at the moment you need it.
		if t.Last.Outcome != string(runner.OutcomeOK) && t.Log == "" {
			parts = append(parts, "no log")
		}
		field("Last run", strings.Join(parts, th.Sep))
	}

	field("Missed", string(t.MissedRuns))
	if len(t.History) > 0 {
		field("History", sparkline(t.History, th)+"   (oldest to newest)")
	}
	field("Log", t.Log)

	return b.String()
}

func sparkline(h []state.RunRecord, th Theme) string {
	marks := make([]string, 0, len(h))
	for i := range h {
		marks = append(marks, outcomeGlyph(h[i].Outcome, th))
	}
	return strings.Join(marks, " ")
}

// hints advertises only keys that actually exist. Showing "⏎ run now"
// before run-now is implemented would be a promise the display cannot keep.
func hints(width int, th Theme) string {
	nav := "↑/↓"
	if !th.Unicode {
		nav = "up/dn"
	}
	full := nav + " select   L log   q quit"
	short := nav + "  L  q"
	if len([]rune(full)) <= width {
		return full
	}
	return short
}

// stamp formats an absolute time, dropping the date when it is today --
// a pane you glance at does not need to be told what day it is.
func stamp(t, now time.Time) string {
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Jan _2 15:04")
}

func elapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, h, m)
	case h > 0:
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	default:
		return fmt.Sprintf("%d:%02d", m, s)
	}
}

// wrap breaks s into lines of at most n runes, preferring word boundaries
// but hard-splitting anything longer than a line -- URLs and shell commands
// routinely have no spaces to break on. It always returns at least one line.
func wrap(s string, n int) []string {
	if n < 1 {
		return []string{""}
	}
	var lines []string
	cur := ""

	flush := func() {
		lines = append(lines, cur)
		cur = ""
	}

	for _, word := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = word
		case len([]rune(cur))+1+len([]rune(word)) <= n:
			cur += " " + word
			continue
		default:
			flush()
			cur = word
		}
		for len([]rune(cur)) > n {
			r := []rune(cur)
			lines = append(lines, string(r[:n]))
			cur = string(r[n:])
		}
	}
	if cur != "" || len(lines) == 0 {
		flush()
	}
	return lines
}

// fit pads or ellipsizes to exactly n columns. The ellipsis comes from the
// theme because it must degrade to ASCII along with everything else.
func fit(s string, n int, ell string) string {
	r := []rune(s)
	if len(r) > n {
		e := []rune(ell)
		if n <= len(e) {
			return strings.Repeat(".", n)
		}
		return string(r[:n-len(e)]) + ell
	}
	return s + strings.Repeat(" ", n-len(r))
}

// pad puts left and right on one line, flush to the edges.
func pad(left, right string, width int, ell string) string {
	gap := width - len([]rune(left)) - len([]rune(right))
	if gap < 1 {
		return fit(left, width, ell)
	}
	return left + strings.Repeat(" ", gap) + right
}
