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

// TestReleaseDoesNotDeleteAnotherHoldersLock is the regression test for the
// critical finding: Release must not delete a lock it no longer owns.
//
// Sequence: process A acquires the lock, process B force-acquires it
// (overwriting A's lock file and becoming the legitimate holder), and A
// later calls Release on its now-stale in-memory *Lock. That must be a
// no-op: the lock file must survive, and it must still name B's token.
func TestReleaseDoesNotDeleteAnotherHoldersLock(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireLock(dir, "/proj/.coucou.yaml", true)
	if err != nil {
		t.Fatalf("force acquire: %v", err)
	}
	defer second.Release()

	if err := first.Release(); err != nil {
		t.Errorf("Release of a superseded lock must return nil, got: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "lock")); err != nil {
		t.Fatalf("lock file should still exist after the superseded holder releases: %v", err)
	}
	onDisk, ok := readLock(filepath.Join(dir, "lock"))
	if !ok {
		t.Fatal("lock file should still be readable")
	}
	if onDisk.token != second.token {
		t.Error("lock file should still name the second holder's token")
	}
}

func TestReleaseLeavesUnparseableLockFileInPlace(t *testing.T) {
	dir := t.TempDir()

	l, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "lock")
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := l.Release(); err != nil {
		t.Errorf("Release on an unparseable lock file must return nil, got: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lock file should still exist: %v", err)
	}
	if string(data) != "garbage" {
		t.Error("Release must not modify a lock file it cannot parse")
	}
}

func TestReleaseOrdinaryPathRemovesFile(t *testing.T) {
	dir := t.TempDir()

	l, err := AcquireLock(dir, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); !os.IsNotExist(err) {
		t.Error("lock file should be removed when the current holder releases it")
	}
}

func TestAcquireLockGeneratesDistinctTokens(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()

	l1, err := AcquireLock(dir1, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()

	l2, err := AcquireLock(dir2, "/proj/.coucou.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()

	if l1.token == "" || l2.token == "" {
		t.Fatal("tokens should not be empty")
	}
	if l1.token == l2.token {
		t.Error("two AcquireLock calls should produce different tokens")
	}
}
