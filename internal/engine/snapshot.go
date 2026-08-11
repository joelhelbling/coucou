package engine

import (
	"time"

	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/state"
)

// TaskView is everything a display needs about one task. It is a copy taken
// under e.mu, so a renderer can never observe a half-updated world, and it
// carries the static config fields as well as the live ones so that a view
// function is a pure function of a single value.
type TaskView struct {
	Name        string
	Description string
	Command     string
	Schedule    string
	Shell       string
	Log         string
	Timeout     time.Duration
	OnOverlap   config.OverlapPolicy
	MissedRuns  config.MissedPolicy

	Disabled  bool
	Running   bool
	Overdue   bool
	StartedAt time.Time // zero unless Running
	NextAt    time.Time
	Last      *state.LastRun    // nil when the task has never run
	History   []state.RunRecord // oldest first; copied, never aliased
}

// Snapshot is a consistent view of every task at one instant.
type Snapshot struct {
	At    time.Time
	Tasks []TaskView
}

// Snapshot returns a consistent copy of every task's state.
//
// This exists because state.State is not internally synchronized and the
// engine mutates it under e.mu; a consumer reading it directly would race.
// Everything mutable is copied rather than aliased -- History in particular,
// since the engine appends to that backing array on every completed run.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()

	snap := Snapshot{
		At:    e.clock.Now(),
		Tasks: make([]TaskView, 0, len(e.cfg.Tasks)),
	}

	for i := range e.cfg.Tasks {
		t := &e.cfg.Tasks[i]
		v := TaskView{
			Name:        t.Name,
			Description: t.Description,
			Command:     t.Command,
			Schedule:    t.Schedule,
			Shell:       t.Shell,
			Log:         e.cfg.LogPath(t),
			Timeout:     t.Timeout.Std(),
			OnOverlap:   t.OnOverlap,
			MissedRuns:  t.MissedRuns,
		}

		if rt := e.rt[t.Name]; rt != nil {
			v.Running = rt.running
			v.Overdue = rt.overdue
			v.StartedAt = rt.startedAt
			v.NextAt = rt.nextAt
		}

		if ts := e.state.Get(t.Name); ts != nil {
			v.Disabled = ts.Disabled
			if ts.LastRun != nil {
				last := *ts.LastRun
				v.Last = &last
			}
			if len(ts.History) > 0 {
				v.History = make([]state.RunRecord, len(ts.History))
				copy(v.History, ts.History)
			}
		}

		snap.Tasks = append(snap.Tasks, v)
	}

	return snap
}

// AnyRunning reports whether any task is executing. The TUI uses it to pick
// its refresh cadence: animating at 10fps all day would burn a core for
// nothing.
func (s Snapshot) AnyRunning() bool {
	for i := range s.Tasks {
		if s.Tasks[i].Running {
			return true
		}
	}
	return false
}
