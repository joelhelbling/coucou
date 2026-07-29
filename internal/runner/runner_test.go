package runner

import (
	"bytes"
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
	res := New(300*time.Millisecond).Run(context.Background(), cfg, task)
	elapsed := time.Since(start)

	if res.Outcome != OutcomeTimeout {
		t.Errorf("Outcome = %q, want timeout", res.Outcome)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v; SIGKILL escalation did not happen", elapsed)
	}
}

func TestRunKillsGrandchildren(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	// The shell backgrounds a grandchild that sleeps briefly and then writes
	// the marker. Killing only the direct child (the process-group leader)
	// would leave this grandchild orphaned but still running, so it would
	// still write the marker after its sleep elapses.
	//
	// Timing must make a surviving grandchild's marker write observable
	// within the window this test actually checks: the grandchild's sleep
	// (300ms) MUST stay shorter than the post-run wait below (1.5s), or an
	// orphaned grandchild would still be sleeping when we check for the
	// marker and the test would pass regardless of whether the whole group
	// was killed — silently testing nothing.
	cfg := &config.Config{Dir: dir, Tasks: []config.Task{{
		Name:    "family",
		Shell:   "/bin/sh",
		Command: "sh -c 'sleep 0.3; echo alive > " + marker + "' & wait",
		Timeout: config.Duration(200 * time.Millisecond),
	}}}
	task := &cfg.Tasks[0]

	res := New(300*time.Millisecond).Run(context.Background(), cfg, task)
	if res.Outcome != OutcomeTimeout {
		t.Fatalf("Outcome = %q, want timeout", res.Outcome)
	}

	// Give any survivor time to finish its 300ms sleep and write its marker.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("grandchild survived; the whole process group must be killed")
	}
}

func TestRunContextCancelReplacesAndKillsProcess(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	cfg := &config.Config{Dir: dir, Tasks: []config.Task{{
		Name:    "long",
		Shell:   "/bin/sh",
		Command: "sleep 30; echo alive > " + marker,
	}}}
	task := &cfg.Tasks[0]

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res := New(300*time.Millisecond).Run(ctx, cfg, task)
	elapsed := time.Since(start)

	if res.Outcome != OutcomeReplaced {
		t.Errorf("Outcome = %q, want replaced", res.Outcome)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v; cancellation did not terminate the process promptly", elapsed)
	}

	// Give any survivor time to write its marker; a correctly killed process
	// never reaches the "sleep 30" command's completion.
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("process survived context cancellation; it must be killed")
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

// TestNewStreamingWritesToArbitraryWriter is a regression test for a bug
// where NewStreaming took a *os.File, so callers had nothing to pass but a
// real file (e.g. os.Stdout) and could never capture output in a test or
// redirect it to any other io.Writer. NewStreaming must accept any
// io.Writer, such as a bytes.Buffer.
func TestNewStreamingWritesToArbitraryWriter(t *testing.T) {
	cfg, task := fixture(t, config.Task{Name: "stream", Command: "echo hello-stream"})

	var buf bytes.Buffer
	res := NewStreaming(DefaultGrace, &buf).Run(context.Background(), cfg, task)

	if res.Outcome != OutcomeOK {
		t.Fatalf("Outcome = %q, err %v", res.Outcome, res.Err)
	}
	if !strings.Contains(buf.String(), "hello-stream") {
		t.Errorf("buffer = %q, want it to contain command output", buf.String())
	}
}
