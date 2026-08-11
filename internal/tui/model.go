package tui

import (
	"os"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joelhelbling/coucou/internal/engine"
)

// Refresh cadences. Idle cost matters: this process runs all day in a pane,
// so we only animate while something is actually executing.
const (
	idleInterval = time.Second
	busyInterval = 100 * time.Millisecond
)

type tickMsg time.Time

// supervisorDoneMsg arrives when supervisor.Run returns. If the supervisor
// dies -- a lost lock, an unwritable state dir -- the TUI must not sit there
// rendering a frozen snapshot forever.
type supervisorDoneMsg struct{ err error }

type model struct {
	eng   *engine.Engine
	theme Theme

	snap engine.Snapshot
	sel  int
	tick int

	width, height int

	quitting bool
	err      error
}

func newModel(eng *engine.Engine, th Theme) model {
	return model{eng: eng, theme: th, snap: eng.Snapshot()}
}

func (m model) Init() tea.Cmd {
	return tea.Tick(idleInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case supervisorDoneMsg:
		m.err = msg.err
		return m, tea.Quit

	case tickMsg:
		m.snap = m.eng.Snapshot()
		m.tick++
		m.clampSelection()

		interval := idleInterval
		if m.snap.AnyRunning() {
			interval = busyInterval
		}
		return m, tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.sel > 0 {
				m.sel--
			}
			return m, nil
		case "down", "j":
			if m.sel < len(m.snap.Tasks)-1 {
				m.sel++
			}
			return m, nil
		case "g", "home":
			m.sel = 0
			return m, nil
		case "G", "end":
			m.sel = len(m.snap.Tasks) - 1
			m.clampSelection()
			return m, nil
		case "L":
			return m, m.openLog()
		}
	}

	return m, nil
}

func (m model) View() string {
	if len(m.snap.Tasks) == 0 {
		return "No tasks configured.\n"
	}
	// Bubble Tea sends WindowSizeMsg before the first render in practice,
	// but a zero width would render a "pane too narrow" flash if it ever
	// did not.
	w, h := m.width, m.height
	if w == 0 {
		w, h = 80, 24
	}
	return View(m.snap, m.sel, w, h, m.tick, m.theme)
}

func (m *model) clampSelection() {
	if m.sel >= len(m.snap.Tasks) {
		m.sel = len(m.snap.Tasks) - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
}

// openLog hands the selected task's log to $PAGER and restores the display
// afterwards. Coucou never reads, parses, or renders command output itself:
// your pager already does that better.
//
// Scheduling continues while the pager is up -- only the TUI is suspended.
func (m model) openLog() tea.Cmd {
	if m.sel < 0 || m.sel >= len(m.snap.Tasks) {
		return nil
	}
	path := m.snap.Tasks[m.sel].Log
	if path == "" {
		return nil // inert on a task with no log:, per the design
	}
	if _, err := os.Stat(path); err != nil {
		return nil // nothing written yet; silently inert rather than noisy
	}

	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less"
	}
	return tea.ExecProcess(exec.Command(pager, path), func(error) tea.Msg {
		// A pager that fails to launch is not worth tearing the TUI down
		// for; the next tick redraws over whatever it left behind.
		return tickMsg(time.Now())
	})
}
