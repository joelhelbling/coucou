package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const lockName = "lock"

// Lock is an exclusive claim on scheduling one config. Two schedulers on one
// config would fire every task twice, so this is a correctness guard rather
// than a convenience.
type Lock struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	ConfigPath string    `json:"config_path"`

	path string
}

// AcquireLock claims the lock in dir. It fails if a live process holds it,
// unless force is set. A lock whose pid is dead is broken automatically.
func AcquireLock(dir, configPath string, force bool) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create state directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, lockName)

	if existing, ok := readLock(path); ok && !force && processAlive(existing.PID) {
		return nil, fmt.Errorf(
			"coucou is already running for this config (pid %d, since %s)\n"+
				"Use --force to take over.",
			existing.PID, existing.StartedAt.Format("15:04"))
	}

	l := &Lock{
		PID:        os.Getpid(),
		StartedAt:  time.Now(),
		ConfigPath: configPath,
		path:       path,
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("cannot write lock %s: %w", path, err)
	}
	return l, nil
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	err := os.Remove(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// readLock returns the lock on disk. ok is false when it is absent or
// unreadable; an unreadable lock is treated as stale.
func readLock(path string) (*Lock, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, false
	}
	return &l, true
}

// processAlive reports whether pid exists, using signal 0.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
