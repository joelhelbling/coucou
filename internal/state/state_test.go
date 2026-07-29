package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingReturnsFreshState(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Tasks == nil {
		t.Error("Tasks map should be initialized")
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)

	at := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	s.Record("xkcd",
		RunRecord{At: at, Outcome: "ok", DurationMS: 1400},
		LastRun{StartedAt: at, FinishedAt: at.Add(1400 * time.Millisecond),
			ExitCode: 0, Outcome: "ok"})
	s.Get("xkcd").Disabled = true

	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ts := reloaded.Get("xkcd")
	if !ts.Disabled {
		t.Error("Disabled did not survive the round trip")
	}
	if ts.LastRun == nil || ts.LastRun.ExitCode != 0 {
		t.Errorf("LastRun did not survive: %+v", ts.LastRun)
	}
	if len(ts.History) != 1 {
		t.Errorf("History has %d entries, want 1", len(ts.History))
	}
}

func TestHistoryRingEvicts(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	base := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	for i := 0; i < HistoryLimit+5; i++ {
		s.Record("t",
			RunRecord{At: base.Add(time.Duration(i) * time.Minute),
				Outcome: "ok", DurationMS: int64(i)},
			LastRun{Outcome: "ok"})
	}

	h := s.Get("t").History
	if len(h) != HistoryLimit {
		t.Fatalf("History has %d entries, want %d", len(h), HistoryLimit)
	}
	// The oldest entries must be the ones evicted.
	if h[0].DurationMS != 5 {
		t.Errorf("oldest kept entry has DurationMS %d, want 5", h[0].DurationMS)
	}
	if h[len(h)-1].DurationMS != int64(HistoryLimit+4) {
		t.Errorf("newest entry has DurationMS %d, want %d",
			h[len(h)-1].DurationMS, HistoryLimit+4)
	}
}

func TestLoadCorruptFileBacksUpAndStartsFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load must not fail on a corrupt file: %v", err)
	}
	if len(s.Tasks) != 0 {
		t.Errorf("expected a fresh state, got %d tasks", len(s.Tasks))
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.bak")); err != nil {
		t.Error("corrupt state should be preserved as state.json.bak")
	}
}

func TestLoadFutureVersionBacksUpAndStartsFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"version": 99, "tasks": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Version != Version {
		t.Errorf("Version = %d, want %d", s.Version, Version)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.bak")); err != nil {
		t.Error("future-version state should be preserved as state.json.bak")
	}
}

func TestSaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", ".coucou")
	s := &State{Version: Version, Tasks: map[string]*TaskState{}}
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save should create the state directory: %v", err)
	}
}
