package state

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	dir := t.TempDir()

	l, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if l.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", l.PID, os.Getpid())
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); err != nil {
		t.Errorf("lock file not created: %v", err)
	}

	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); !os.IsNotExist(err) {
		t.Error("lock file should be removed on release")
	}
}

func TestAcquireRefusesWhenHeldByLiveProcess(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	_, err = AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err == nil {
		t.Fatal("expected the second acquisition to fail")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("error %q should name the holding pid", err)
	}
}

func TestAcquireBreaksStaleLock(t *testing.T) {
	dir := t.TempDir()

	// A process that has certainly exited.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := cmd.Process.Pid

	body := `{"pid": ` + strconv.Itoa(deadPID) +
		`, "started_at": "2026-07-29T14:00:00Z", "config_path": "/proj/.coucou.yaml"}`
	if err := os.WriteFile(filepath.Join(dir, "lock"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatalf("a lock held by a dead pid must be broken automatically: %v", err)
	}
	defer l.Release()
}

func TestAcquireBreaksCorruptLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lock"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatalf("a corrupt lock should be treated as stale: %v", err)
	}
	defer l.Release()
}

func TestAcquireForceBreaksLiveLock(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	second, err := AcquireLock(dir, "/proj/.coucou.yaml", true)
	if err != nil {
		t.Fatalf("force should break a live lock: %v", err)
	}
	defer second.Release()
}
