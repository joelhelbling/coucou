package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func project(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".coucou.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func run(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut, dir)
	return code, out.String(), errOut.String()
}

const goodConfig = `
tasks:
  - name: xkcd
    description: Fetch latest xkcd comic
    command: "true"
    schedule: "@daily at 17:00"
  - name: poll
    command: "true"
    schedule: "@every 30m"
`

func TestValidateSucceeds(t *testing.T) {
	dir := project(t, goodConfig)
	code, _, errOut := run(t, dir, "validate")
	if code != 0 {
		t.Errorf("exit %d, want 0 (stderr: %s)", code, errOut)
	}
}

func TestValidateFailsWithAllErrors(t *testing.T) {
	dir := project(t, `
tasks:
  - name: broken
    schedule: "nope"
`)
	code, _, errOut := run(t, dir, "validate")
	if code == 0 {
		t.Fatal("exit 0, want non-zero")
	}
	for _, want := range []string{"command", "nope"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

func TestMissingConfigIsAnError(t *testing.T) {
	code, _, errOut := run(t, t.TempDir(), "validate")
	if code == 0 {
		t.Fatal("exit 0, want non-zero")
	}
	if !strings.Contains(errOut, ".coucou.yaml") {
		t.Errorf("stderr should name the file looked for:\n%s", errOut)
	}
}

func TestListPrintsTasks(t *testing.T) {
	dir := project(t, goodConfig)
	code, out, errOut := run(t, dir, "list")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, errOut)
	}
	for _, want := range []string{"xkcd", "poll", "@daily at 17:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestNextPrintsSoonest(t *testing.T) {
	// A single task makes the answer deterministic regardless of the wall
	// clock: with two tasks, which one is soonest depends on the time of day.
	dir := project(t, `
tasks:
  - name: only one
    command: "true"
    schedule: "@every 30m"
`)
	code, out, errOut := run(t, dir, "next")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "only one") {
		t.Errorf("output should name the task:\n%s", out)
	}
	if !strings.Contains(out, "in ") {
		t.Errorf("output should include a countdown:\n%s", out)
	}
}

func TestNextWithNoTasks(t *testing.T) {
	dir := project(t, "tasks: []\n")
	code, out, _ := run(t, dir, "next")
	if code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if !strings.Contains(out, "no tasks") {
		t.Errorf("output should say there is nothing scheduled:\n%s", out)
	}
}

func TestRunExecutesOneTask(t *testing.T) {
	dir := project(t, `
tasks:
  - name: writer
    command: "echo hello > proof.txt"
    schedule: "@daily"
`)
	code, _, errOut := run(t, dir, "run", "writer")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "proof.txt")); err != nil {
		t.Errorf("task did not run: %v", err)
	}
}

func TestRunRecordsNothingAndTakesNoLock(t *testing.T) {
	dir := project(t, `
tasks:
  - name: writer
    command: "true"
    schedule: "@daily"
`)
	if code, _, errOut := run(t, dir, "run", "writer"); code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, ".coucou", "state.json")); err == nil {
		t.Error("coucou run must not write state")
	}
	if _, err := os.Stat(filepath.Join(dir, ".coucou", "lock")); err == nil {
		t.Error("coucou run must not take the lock")
	}
}

func TestRunPropagatesExitCode(t *testing.T) {
	dir := project(t, `
tasks:
  - name: failer
    command: "exit 7"
    schedule: "@daily"
`)
	code, _, _ := run(t, dir, "run", "failer")
	if code != 7 {
		t.Errorf("exit %d, want 7", code)
	}
}

func TestRunUnknownTask(t *testing.T) {
	dir := project(t, goodConfig)
	code, _, errOut := run(t, dir, "run", "nope")
	if code == 0 {
		t.Fatal("exit 0, want non-zero")
	}
	if !strings.Contains(errOut, "nope") {
		t.Errorf("stderr should name the unknown task:\n%s", errOut)
	}
}

func TestConfigFlagOverrides(t *testing.T) {
	dir := project(t, goodConfig)
	other := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(other, []byte("tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, dir, "--config", other, "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, "xkcd") {
		t.Error("--config was ignored; the local config was loaded")
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, t.TempDir(), "--version")
	if code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("--version printed nothing")
	}
}

func TestUnknownSubcommand(t *testing.T) {
	code, _, errOut := run(t, t.TempDir(), "frobnicate")
	if code == 0 {
		t.Fatal("exit 0, want non-zero")
	}
	if !strings.Contains(errOut, "frobnicate") {
		t.Errorf("stderr should name the unknown subcommand:\n%s", errOut)
	}
}
