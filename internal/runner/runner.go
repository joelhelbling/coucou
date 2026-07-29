package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// "coucou run" uses it so a manual run streams to the terminal. It is an
	// io.Writer, not an *os.File, both so callers can pass any writer (a
	// bytes.Buffer in tests, a CLI-injected stdout) and structurally, since
	// io.Writer has no Close: a caller-owned stream can never be closed by
	// mistake.
	stream io.Writer
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
// task's log file. out may be any io.Writer (os.Stdout, a bytes.Buffer in
// tests, or a CLI-injected writer); it is never closed.
func NewStreaming(grace time.Duration, out io.Writer) Runner {
	if grace <= 0 {
		grace = DefaultGrace
	}
	return &runner{grace: grace, stream: out}
}

func (r *runner) Run(ctx context.Context, cfg *config.Config, t *config.Task) Result {
	res := Result{StartedAt: time.Now()}

	out, closer, err := r.openOutput(cfg, t)
	if err != nil {
		res.FinishedAt = time.Now()
		res.Outcome = OutcomeFail
		res.ExitCode = -1
		res.Err = err
		return res
	}
	// closer is only set for the log-file / devnull paths that this
	// function opened itself. A caller-owned stream (r.stream) has no
	// Close to call, so there is nothing to leak.
	if closer != nil {
		defer closer.Close()
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
	// exited is closed when the child has been reaped. terminateGroup
	// watches it so escalation stops as soon as the process is gone,
	// without consuming the value the caller reads from done.
	exited := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(exited)
	}()

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
		terminateGroup(pgid, r.grace, exited)
		err = <-done
	case <-ctx.Done():
		terminateGroup(pgid, r.grace, exited)
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

// terminateGroup signals the whole process group: SIGTERM, then SIGKILL if
// it has not exited within the grace period.
//
// It watches `exited` rather than `done`: only the caller receives from
// done, and a helper that consumed that value would deadlock the caller.
// Returning as soon as the child is reaped matters — sleeping out the full
// grace period would inflate every timed-out run's recorded duration by up
// to the grace period.
func terminateGroup(pgid int, grace time.Duration, exited <-chan struct{}) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-exited:
		return
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
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

// openOutput returns the destination for stdout and stderr, plus a closer
// when this function opened the underlying file itself (log file or
// /dev/null). With no log configured both go to /dev/null: they must never
// be inherited, because a child writing to the TUI's terminal would corrupt
// the display. When r.stream is set, the returned closer is nil: the
// caller-owned writer is never closed here.
func (r *runner) openOutput(cfg *config.Config, t *config.Task) (io.Writer, io.Closer, error) {
	if r.stream != nil {
		return r.stream, nil, nil
	}
	path := cfg.LogPath(t)
	if path == "" {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot open %s: %w", os.DevNull, err)
		}
		return f, f, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("cannot create log directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot open log %s: %w", path, err)
	}
	return f, f, nil
}
