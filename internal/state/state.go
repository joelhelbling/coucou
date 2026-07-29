package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Version is the state schema version. A file with a higher version is
// treated as unreadable rather than guessed at.
const Version = 1

// HistoryLimit is how many outcomes are kept per task for the sparkline.
const HistoryLimit = 12

const fileName = "state.json"

// RunRecord is one entry in a task's history ring.
type RunRecord struct {
	At         time.Time `json:"at"`
	Outcome    string    `json:"outcome"`
	DurationMS int64     `json:"duration_ms"`
}

// LastRun is the detail shown for the most recent run.
type LastRun struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	ExitCode   int       `json:"exit_code"`
	Outcome    string    `json:"outcome"`
}

// TaskState is everything Coucou remembers about one task.
type TaskState struct {
	Disabled bool        `json:"disabled"`
	LastRun  *LastRun    `json:"last_run,omitempty"`
	History  []RunRecord `json:"history,omitempty"`
}

// State is the whole state file.
type State struct {
	Version int                   `json:"version"`
	Tasks   map[string]*TaskState `json:"tasks"`
}

// Load reads state from dir. It never fails because of bad content: a file
// that will not parse, or one written by a newer version, is renamed to
// state.json.bak and a fresh state is returned. Losing history beats
// refusing to start.
func Load(dir string) (*State, error) {
	fresh := &State{Version: Version, Tasks: map[string]*TaskState{}}
	path := filepath.Join(dir, fileName)

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fresh, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil || s.Version > Version {
		if renameErr := os.Rename(path, path+".bak"); renameErr != nil {
			return nil, fmt.Errorf("cannot set aside unreadable %s: %w", path, renameErr)
		}
		return fresh, nil
	}

	if s.Tasks == nil {
		s.Tasks = map[string]*TaskState{}
	}
	s.Version = Version
	return &s, nil
}

// Save writes state atomically: a temp file in the same directory, then a
// rename, so a crash mid-write cannot leave a truncated file.
func (s *State) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create state directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("cannot create temp state file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, fileName))
}

// Get returns the state for a task, creating it if absent.
func (s *State) Get(name string) *TaskState {
	if s.Tasks == nil {
		s.Tasks = map[string]*TaskState{}
	}
	ts, ok := s.Tasks[name]
	if !ok {
		ts = &TaskState{}
		s.Tasks[name] = ts
	}
	return ts
}

// Record appends a run to the history ring and updates the last run.
func (s *State) Record(name string, r RunRecord, last LastRun) {
	ts := s.Get(name)
	ts.History = append(ts.History, r)
	if len(ts.History) > HistoryLimit {
		ts.History = ts.History[len(ts.History)-HistoryLimit:]
	}
	l := last
	ts.LastRun = &l
}
