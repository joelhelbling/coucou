package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".coucou.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidConfig(t *testing.T) {
	path := write(t, `
tasks:
  - name: API Refresh
    description: Fetch latest API changelog
    command: curl -s https://example.com
    schedule: "@weekly on mon at 18:30"
    log: log/api.log
    timeout: 30s
    missed_runs: catch_up
  - name: xkcd
    command: curl -s https://xkcd.com/info.0.json
    schedule: "@daily at 17:00"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(cfg.Tasks))
	}
	if cfg.Dir != filepath.Dir(path) {
		t.Errorf("Dir = %q, want %q", cfg.Dir, filepath.Dir(path))
	}

	api := cfg.Tasks[0]
	if api.Timeout.Std() != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", api.Timeout.Std())
	}
	if api.MissedRuns != MissedCatchUp {
		t.Errorf("MissedRuns = %q, want catch_up", api.MissedRuns)
	}
	if api.Parsed == nil {
		t.Error("Parsed schedule is nil; Load must parse it")
	}

	// Defaults applied to the second task.
	x := cfg.Tasks[1]
	if x.OnOverlap != OverlapSkip {
		t.Errorf("OnOverlap = %q, want skip", x.OnOverlap)
	}
	if x.MissedRuns != MissedReport {
		t.Errorf("MissedRuns = %q, want report", x.MissedRuns)
	}
	if x.Shell == "" {
		t.Error("Shell should default to $SHELL or /bin/sh")
	}
}

func TestLoadEmptyTasksIsValid(t *testing.T) {
	path := write(t, "tasks: []\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tasks) != 0 {
		t.Errorf("got %d tasks, want 0", len(cfg.Tasks))
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := write(t, `
tasks:
  - name: x
    command: "true"
    schedule: "@daily"
    on_overlaps: queue
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for the unknown key on_overlaps")
	}
	if !strings.Contains(err.Error(), "on_overlaps") {
		t.Errorf("error %q should name the unknown key", err)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	path := write(t, `
tasks:
  - name: ""
    command: "true"
    schedule: "@daily"
  - name: dup
    schedule: "@daily"
  - name: dup
    command: "true"
    schedule: "not a schedule"
  - name: bad enum
    command: "true"
    schedule: "@daily"
    on_overlap: sometimes
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation errors")
	}
	msg := err.Error()
	for _, want := range []string{
		"name",           // empty name
		"command",        // missing command
		"duplicate",      // duplicate name
		"not a schedule", // unparseable schedule, quoted back
		"sometimes",      // invalid enum value quoted back
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error output missing %q:\n%s", want, msg)
		}
	}
}

func TestLoadReportsLineNumbers(t *testing.T) {
	path := write(t, `tasks:
  - name: ok
    command: "true"
    schedule: "@daily"
  - name: broken
    command: "true"
    schedule: "nope"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	verrs, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("error is %T, want ValidationErrors", err)
	}
	if len(verrs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(verrs), verrs)
	}
	// The second task begins on line 5.
	if verrs[0].Line != 5 {
		t.Errorf("Line = %d, want 5", verrs[0].Line)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestLoadRejectsIntervalBelowFloor(t *testing.T) {
	path := write(t, `
tasks:
  - name: fast
    command: "true"
    schedule: "@every 500ms"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for an interval below the floor")
	}
}
