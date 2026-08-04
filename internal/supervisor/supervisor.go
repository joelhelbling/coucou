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
