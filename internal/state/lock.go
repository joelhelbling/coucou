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

// mutexName is a small, permanent (never removed) control file used only to
// serialize the stale-breaking path of AcquireLock across every racer, in
// this process or any other, on this machine. It is not the lock file
// itself: its content is unused, and it is safe for it to accumulate no
// state at all.
const mutexName = "lock.mutex"

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

	l, err := tryAcquire(path, configPath)
	if err == nil {
		return l, nil
	}
	if !os.IsExist(err) {
		return nil, err
	}

	// Contended: a file already sits at path (live, stale, or corrupt).
	// Deciding "is it stale" and then acting on that decision is a
	// time-of-check-to-time-of-use race if two racers can do it at once:
	// if we just os.Remove'd it, the first remover's retry-link could win,
	// and the second remover would then delete that winner's brand new,
	// live lock out from under it, only for its own retry-link to succeed
	// too - a second scheduler for the same config, so every task fires
	// twice.
	//
	// A rename-based claim on the stale file alone does not fully close
	// this: os.Rename cares only about whatever currently sits at path, not
	// what any particular racer read a moment earlier, and closing that gap
	// with a temp-name-then-verify dance still leaves a window where path
	// is briefly absent - a window a completely different, uncontended
	// acquirer's plain os.Link can slip into.
	//
	// So instead we serialize every racer's whole "inspect, decide, and act
	// on a stale lock" sequence behind a flock on a dedicated, permanent
	// mutex file. Only one of them can hold it at a time, so whichever one
	// does is guaranteed nobody else is concurrently deciding the same
	// thing, and it can safely act on what it reads. Because os.Link
	// (fast path, above) and this flock-guarded path (both below) are the
	// only two ways path is ever mutated, and both either atomically create
	// a file that did not exist or atomically replace one that did, path is
	// never observably absent to a third party - closing the gap the
	// rename-only approach left open.
	return acquireStaleLock(dir, path, configPath, force)
}

// acquireStaleLock handles the contended path of AcquireLock: a file
// already exists at path, and it is either live, stale, or being forced.
// The entire decision is made and acted on while holding an exclusive flock
// on a dedicated mutex file, so it cannot race with any other stale-breaking
// attempt, in this process or another.
func acquireStaleLock(dir, path, configPath string, force bool) (*Lock, error) {
	mutexPath := filepath.Join(dir, mutexName)
	mf, err := os.OpenFile(mutexPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock mutex %s: %w", mutexPath, err)
	}
	defer mf.Close()
	if err := syscall.Flock(int(mf.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("cannot lock %s: %w", mutexPath, err)
	}
	defer syscall.Flock(int(mf.Fd()), syscall.LOCK_UN)

	// Re-read now that we hold the mutex: another racer may have already
	// broken the lock and installed a fresh, live one while we waited.
	existing, ok := readLock(path)
	if ok && !force && processAlive(existing.PID) {
		return nil, fmt.Errorf(
			"coucou is already running for this config (pid %d, since %s)\n"+
				"Use --force to take over.",
			existing.PID, existing.StartedAt.Format("15:04"))
	}

	// Dead pid, unparseable file, or force: the lock is stale (or being
	// forcibly taken over), and we are the only racer that can possibly be
	// acting on that right now. Build the replacement payload, then swap it
	// into place with a single os.Rename over the existing path: rename's
	// replace is atomic, so path is never briefly absent the way a
	// remove-then-create sequence would leave it, and no unrelated,
	// uncontended acquirer's os.Link can ever observe it missing.
	l, data, err := newLockPayload(path, configPath)
	if err != nil {
		return nil, err
	}
	tmpPath := path + ".tmp." + l.token
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("cannot write lock temp file %s: %w", tmpPath, err)
	}
	defer os.Remove(tmpPath)

	if err := os.Rename(tmpPath, path); err != nil {
		return nil, fmt.Errorf("cannot install lock over stale file %s: %w", path, err)
	}
	return l, nil
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
	l, data, err := newLockPayload(path, configPath)
	if err != nil {
		return nil, err
	}

	// The token is unique per acquirer, so including it in the temp file
	// name means two concurrent acquirers can never collide on it.
	tmpPath := path + ".tmp." + l.token
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("cannot write lock temp file %s: %w", tmpPath, err)
	}
	defer os.Remove(tmpPath)

	if err := os.Link(tmpPath, path); err != nil {
		return nil, err
	}
	return l, nil
}

// newLockPayload builds a fresh Lock for path and its serialized on-disk
// form, ready to be written to a temp file and put in place with either
// os.Link (uncontended create) or os.Rename (contended replace-of-stale).
func newLockPayload(path, configPath string) (*Lock, []byte, error) {
	token, err := newToken()
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	return l, append(data, '\n'), nil
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
	return parseLock(data)
}

// parseLock decodes an already-read lock payload. ok is false when the
// payload is unparseable; an unparseable lock is treated as stale.
func parseLock(data []byte) (*Lock, bool) {
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
