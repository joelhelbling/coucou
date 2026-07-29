package state

import (
	"crypto/rand"
	"encoding/hex"
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

	path  string
	token string
}

// lockJSON is the on-disk representation of a Lock. token is unexported on
// Lock itself (callers have no business inspecting it), but it still needs
// to round-trip through the lock file so Release can prove ownership.
type lockJSON struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	ConfigPath string    `json:"config_path"`
	Token      string    `json:"token"`
}

// MarshalJSON serializes the lock, including its ownership token.
func (l *Lock) MarshalJSON() ([]byte, error) {
	return json.Marshal(lockJSON{
		PID:        l.PID,
		StartedAt:  l.StartedAt,
		ConfigPath: l.ConfigPath,
		Token:      l.token,
	})
}

// UnmarshalJSON populates the lock, including its ownership token.
func (l *Lock) UnmarshalJSON(data []byte) error {
	var j lockJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	l.PID = j.PID
	l.StartedAt = j.StartedAt
	l.ConfigPath = j.ConfigPath
	l.token = j.Token
	return nil
}

// newToken generates a random ownership token so that Release can tell
// whether the on-disk lock is still the one this process created, even if
// pids get recycled.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("cannot generate lock token: %w", err)
	}
	return hex.EncodeToString(b), nil
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

	token, err := newToken()
	if err != nil {
		return nil, err
	}

	l := &Lock{
		PID:        os.Getpid(),
		StartedAt:  time.Now(),
		ConfigPath: configPath,
		path:       path,
		token:      token,
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

// Release removes the lock file, but only if it still belongs to this Lock.
// If another process has since force-acquired and overwritten the file, or
// the file is missing, unreadable, or unparseable, Release leaves it alone
// and returns nil: releasing a lock you no longer hold is a no-op, not a
// failure, and we can never delete a file we cannot prove we own.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	existing, ok := readLock(l.path)
	if !ok {
		return nil
	}
	if existing.token != l.token {
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
