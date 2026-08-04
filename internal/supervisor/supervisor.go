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
	"path/filepath"
	"strings"
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
// Events() without missing any. observe must neither block nor panic: it runs
// on Run's own goroutine, so a blocking observe means Run never reaches its
// select and ignores context cancellation entirely, and a panic unwinds the
// whole session.
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

	sweepOrphanTemps(dir, clk.Now())

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
	// Registered immediately after Start and before observe: Start applies
	// the missed-run policies, so subprocesses may already be running by the
	// time observe is called. If observe panicked with this defer registered
	// any later, unwinding would release the lock while this process still
	// had children running and was still writing state.json -- handing a
	// second scheduler the same config, the exact thing the lock prevents.
	// It is still registered *after* the lock-release defer above, so LIFO
	// order stops the engine before the lock is released.
	//
	// Stop cancels every in-flight run, waits for each to record its
	// outcome, and closes the event channel. No separate Wait is needed,
	// and no final state.Save: the engine saves after every run.
	defer eng.Stop()

	if observe != nil {
		observe(eng)
	}

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

// tmpOrphanAge is how old a lock temp file must be before the sweep will
// remove it. AcquireLock writes lock.tmp.<token> and links it into place,
// removing it via defer, so a temp file normally lives for microseconds and
// only a SIGKILL in that window orphans one. Deleting a temp file belonging
// to a live acquirer would make its os.Link fail with ENOENT, converting a
// rare orphan into a reproducible acquisition failure. A genuine orphan
// persists forever, so waiting a minute costs nothing.
const tmpOrphanAge = time.Minute

// sweepOrphanTemps removes lock temp files left behind by a killed process.
// Failures are ignored: a stray temp file is cosmetic, and refusing to
// schedule because of one would be worse than leaving it.
//
// The directory is scanned rather than globbed. filepath.Glob would treat the
// *directory* portion of its argument as a pattern too, so a config living
// under a path containing [, * or ? would either fail with ErrBadPattern --
// silently disabling the sweep forever -- or match siblings of the intended
// directory. Names are matched against state.LockTempPrefix so that a rename
// in internal/state breaks the build here instead of quietly reaping nothing.
func sweepOrphanTemps(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), state.LockTempPrefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) < tmpOrphanAge {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}
