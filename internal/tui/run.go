package tui

import (
	"context"
	"errors"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/engine"
	"github.com/joelhelbling/coucou/internal/supervisor"
)

// ErrNotATTY is returned when stdout is not a terminal. Refusing is kinder
// than emitting escape sequences into a pipe.
var ErrNotATTY = errors.New("not a terminal; try 'coucou list'")

// Options configures one TUI session.
type Options struct {
	Config *config.Config
	ASCII  bool // --ascii
	Force  bool // --force: break an existing instance lock
}

// Run opens the TUI and blocks until the user quits or the supervisor stops.
//
// The supervisor owns the schedule and runs in a goroutine; Bubble Tea owns
// the terminal and the main goroutine. They meet at the observe callback,
// which hands over the started engine before the first tick.
func Run(ctx context.Context, opts Options, out io.Writer) error {
	if f, ok := out.(interface{ Fd() uintptr }); !ok || !isatty.IsTerminal(f.Fd()) {
		return ErrNotATTY
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	theme := DetectTheme(opts.ASCII)

	// Buffered: if the user quits before the supervisor is ready, nothing
	// is listening, and an unbuffered send would leak the goroutine.
	engCh := make(chan *engine.Engine, 1)
	doneCh := make(chan error, 1)

	go func() {
		doneCh <- supervisor.Run(ctx, supervisor.Options{
			Config: opts.Config,
			Force:  opts.Force,
		}, func(e *engine.Engine) { engCh <- e })
	}()

	var eng *engine.Engine
	select {
	case eng = <-engCh:
	case err := <-doneCh:
		// The supervisor failed before it ever started -- almost always a
		// held lock. Report that, not a blank screen.
		if err != nil {
			return err
		}
		return fmt.Errorf("scheduler stopped before it started")
	case <-ctx.Done():
		return ctx.Err()
	}

	p := tea.NewProgram(newModel(eng, theme), tea.WithAltScreen(), tea.WithContext(ctx))

	// Surface a supervisor that dies mid-session as a message into the
	// update loop, rather than letting the TUI render a frozen world.
	go func() {
		err := <-doneCh
		p.Send(supervisorDoneMsg{err: err})
	}()

	final, err := p.Run()
	cancel()

	if err != nil {
		return err
	}
	if m, ok := final.(model); ok && m.err != nil {
		return m.err
	}
	return nil
}
