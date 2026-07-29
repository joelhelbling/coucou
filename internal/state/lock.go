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
//
// The lock file is put in place with os.Link from a fully-written temp file,
// so two processes racing to acquire the same lock at the same instant
// cannot both win, and the file is never visible half-written: only one of
// them can win the atomic link. The read-then-write shape this replaced was
// time-of-check-to-time-of-use, allowing exactly that double-firing this
// lock exists to prevent.
func AcquireLock(dir, configPath string, force bool) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create state directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, lockName)

	// One retry: if the exclusive create loses the race to a file that
	// turns out to be stale (dead pid, unparseable, or force), remove it
	// and try exactly once more. If that second attempt also collides,
	// someone else won the race in the meantime and the correct outcome is
	// to report "already running" rather than loop.
	for attempt := 0; attempt < 2; attempt++ {
		l, err := tryAcquire(path, configPath)
		if err == nil {
			return l, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}

		existing, ok := readLock(path)
		if ok && !force && processAlive(existing.PID) {
			return nil, fmt.Errorf(
				"coucou is already running for this config (pid %d, since %s)\n"+
					"Use --force to take over.",
				existing.PID, existing.StartedAt.Format("15:04"))
		}

		// Dead pid, unparseable file, or force: the lock is stale (or being
		// forcibly taken over). Remove it and retry the exclusive create.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot remove stale lock %s: %w", path, err)
		}
	}

	return nil, fmt.Errorf(
		"coucou is already running for this config; another process won the race to acquire the lock %s",
		path)
}

// tryAcquire claims the lock file atomically. The full JSON payload is built
// in memory and written to a uniquely-named temp file in the same directory
// first, then linked into place with os.Link. Link is atomic and fails with
// EEXIST if the target already exists, so the lock file is never observable
// in a half-written (empty or partial) state: the instant it exists, it is
// complete. This closes the window that O_CREATE|O_EXCL left open, where a
// second acquirer could see an empty file mid-write and misclassify it as an
// unparseable, and therefore stale, lock.
//
// On any failure it returns the raw os error, so callers can test it with
// os.IsExist. The temp file is always removed, whether the link succeeds or
// not.
func tryAcquire(path, configPath string) (*Lock, error) {
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

	// The token is unique per acquirer, so including it in the temp file
	// name means two concurrent acquirers can never collide on it.
	tmpPath := path + ".tmp." + token
	if err := os.WriteFile(tmpPath, append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("cannot write lock temp file %s: %w", tmpPath, err)
	}
	defer os.Remove(tmpPath)

	if err := os.Link(tmpPath, path); err != nil {
		return nil, err
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
