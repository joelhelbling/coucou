package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/engine"
	"github.com/joelhelbling/coucou/internal/supervisor"
)

// TestEndToEndRender drives the real chain -- supervisor, engine, snapshot,
// view -- with a real subprocess. Every other test in this package renders a
// hand-built fixture, so without this nothing proves that a Snapshot taken
// from a live engine actually populates the display.
func TestEndToEndRender(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real subprocess")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".coucou.yaml")
	body := "tasks:\n" +
		"  - name: slow\n" +
		"    description: A task that takes a moment\n" +
		"    command: sleep 2\n" +
		"    schedule: \"@every 1s\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticks := make(chan time.Time)
	engCh := make(chan *engine.Engine, 1)
	done := make(chan error, 1)

	go func() {
		done <- supervisor.Run(ctx, supervisor.Options{
			Config: cfg,
			Ticks:  ticks,
		}, func(e *engine.Engine) { engCh <- e })
	}()

	eng := <-engCh

	// Drive ticks until the task is actually executing.
	deadline := time.Now().Add(10 * time.Second)
	var snap engine.Snapshot
	for {
		ticks <- time.Time{}
		snap = eng.Snapshot()
		if snap.AnyRunning() {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("task never started")
		}
		time.Sleep(100 * time.Millisecond)
	}

	out := View(snap, 0, 80, 40, 3, UnicodeTheme(false))
	t.Logf("live render:\n%s", out)

	for _, want := range []string{"slow", "Running", "running now", "A task that takes a moment"} {
		if !strings.Contains(out, want) {
			t.Errorf("live render missing %q:\n%s", want, out)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("supervisor: %v", err)
	}

	// After shutdown the run is recorded, so a fresh snapshot must show a
	// last-run outcome rather than "never".
	final := eng.Snapshot()
	if final.Tasks[0].Last == nil {
		t.Error("no last run recorded after the task ran")
	}
	if final.AnyRunning() {
		t.Error("a task is still marked running after shutdown")
	}
}
