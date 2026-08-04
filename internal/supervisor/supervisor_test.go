package supervisor

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/schedule"
)

// fakeRunner records what it was asked to run and returns a canned result.
// The engine package has its own unexported copy; duplicating it here keeps
// the two packages' tests uncoupled.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	block chan struct{} // when non-nil, Run blocks until closed or ctx done
	ctxs  []context.Context
}

func (f *fakeRunner) Run(ctx context.Context, cfg *config.Config, t *config.Task) runner.Result {
	f.mu.Lock()
	f.calls = append(f.calls, t.Name)
	f.ctxs = append(f.ctxs, ctx)
	block := f.block
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return runner.Result{Outcome: runner.OutcomeReplaced, ExitCode: -1}
		}
	}
	return runner.Result{
		StartedAt: time.Now(), FinishedAt: time.Now(),
		Outcome: runner.OutcomeOK,
	}
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newCfg builds an in-memory config with a real temp directory. StateDir is
// set explicitly: config.Load would default it to ".coucou", but a
// hand-built Config leaves it empty, which would put the lock and state file
// directly in Dir.
func newCfg(t *testing.T, tasks ...config.Task) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Dir:      dir,
		Path:     filepath.Join(dir, ".coucou.yaml"),
		StateDir: ".coucou",
		Tasks:    tasks,
	}
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

func TestTickDrivesRuns(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 31, 16, 59, 0, 0, time.UTC))
	fr := &fakeRunner{}
	cfg := newCfg(t, config.Task{
		Name: "xkcd", Command: "true", Schedule: "0 17 * * *",
	})

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg, Clock: clk, Runner: fr, Ticks: ticks,
		}, nil)
	}()

	// Not due yet.
	ticks <- time.Time{}
	if fr.callCount() != 0 {
		t.Fatalf("ran before it was due")
	}

	clk.Advance(time.Minute) // now 17:00
	ticks <- time.Time{}
	// The next send is only accepted once the previous Tick returned, so
	// the dispatch has happened by now; the run itself is a goroutine.
	ticks <- time.Time{}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fr.callCount() != 1 {
		t.Errorf("ran %d times, want 1", fr.callCount())
	}
}
