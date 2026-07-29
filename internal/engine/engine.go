package engine

import (
	"context"
	"sync"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/runner"
	"github.com/joelhelbling/coucou/internal/schedule"
	"github.com/joelhelbling/coucou/internal/state"
)

// OutcomeSkipped is recorded when an occurrence was dropped because the
// previous run was still going. It is not a runner outcome because no
// process ever started.
const OutcomeSkipped = "skipped"

type EventKind int

const (
	EventStarted EventKind = iota
	EventFinished
	EventSkipped
	EventOverdue
)

// Event is what the engine tells its subscriber. The TUI consumes these; the
// engine itself knows nothing about terminals.
type Event struct {
	Kind   EventKind
	Task   string
	Result *runner.Result
}

// taskRuntime is the engine's in-memory bookkeeping for one task.
type taskRuntime struct {
	nextAt  time.Time
	overdue bool
	running bool
	cancel  context.CancelFunc
	queued  bool // on_overlap: queue, depth 1

	// staggering is true while a catch_up task has been pushed into the
	// near future by applyMissedPolicies's stampede stagger, and has not
	// yet actually dispatched. It stops isMissedLocked's signal 2 (which is
	// based on a stale last-finish time that stagger deliberately does not
	// update) from re-flagging the task as newly missed on every
	// subsequent applyMissedPolicies pass -- Start and Tick both call it,
	// and each call's local stagger counter starts over at zero, so
	// without this guard a still-pending staggered task would be
	// rediscovered as "first in this pass" and dispatched immediately,
	// defeating the stagger. The ordinary due-time check in Tick's main
	// loop is what actually dispatches it once nextAt arrives.
	staggering bool
}

// Engine owns scheduling. It has no terminal dependency and communicates
// only through its event channel, which is the seam a future daemon uses.
type Engine struct {
	cfg    *config.Config
	state  *state.State
	runner runner.Runner
	clock  clock.Clock

	mu       sync.Mutex
	rt       map[string]*taskRuntime
	lastTick time.Time
	stopped  bool

	events chan Event
	wg     sync.WaitGroup
}

func New(cfg *config.Config, st *state.State, r runner.Runner, clk clock.Clock) *Engine {
	return &Engine{
		cfg:    cfg,
		state:  st,
		runner: r,
		clock:  clk,
		rt:     make(map[string]*taskRuntime, len(cfg.Tasks)),
		events: make(chan Event, 64),
	}
}

func (e *Engine) Events() <-chan Event { return e.events }

// Start computes the first next_at for every task and applies each task's
// missed-run policy for anything already overdue.
func (e *Engine) Start() {
	now := e.clock.Now()
	e.mu.Lock()
	e.lastTick = now
	for i := range e.cfg.Tasks {
		t := &e.cfg.Tasks[i]
		e.rt[t.Name] = &taskRuntime{}
		e.scheduleNextLocked(t, now)
	}
	e.mu.Unlock()

	e.applyMissedPolicies(now)
}

// Tick evaluates one scheduling moment. Production drives it from a 1Hz
// ticker; tests call it directly.
func (e *Engine) Tick() {
	now := e.clock.Now()

	e.mu.Lock()
	// A large backward step invalidates every cached next_at.
	if now.Before(e.lastTick) {
		for i := range e.cfg.Tasks {
			e.scheduleNextLocked(&e.cfg.Tasks[i], now)
		}
	}
	e.lastTick = now
	e.mu.Unlock()

	e.applyMissedPolicies(now)

	for i := range e.cfg.Tasks {
		t := &e.cfg.Tasks[i]

		e.mu.Lock()
		rt := e.rt[t.Name]
		disabled := e.state.Get(t.Name).Disabled
		due := rt != nil && !rt.nextAt.IsZero() && !now.Before(rt.nextAt)
		running := rt != nil && rt.running
		e.mu.Unlock()

		if rt == nil || disabled || !due {
			continue
		}

		if running {
			e.handleOverlap(t, now)
			continue
		}
		e.dispatch(t, now)
	}
}

// applyMissedPolicies handles due times that passed without a run -- at
// startup, after a suspend, or after a clock step. It is deliberately not
// startup-only.
func (e *Engine) applyMissedPolicies(now time.Time) {
	// catchUpStagger spaces overdue catch_up dispatches apart so several
	// overdue tasks don't stampede at once (spec section 2.5). The first
	// overdue catch_up task in a pass still runs immediately.
	const catchUpStagger = 2 * time.Second
	staggered := 0

	for i := range e.cfg.Tasks {
		t := &e.cfg.Tasks[i]

		e.mu.Lock()
		rt := e.rt[t.Name]
		if rt == nil || rt.running || rt.staggering || e.state.Get(t.Name).Disabled {
			e.mu.Unlock()
			continue
		}
		var lastFinish time.Time
		if ls := e.state.Get(t.Name).LastRun; ls != nil {
			lastFinish = ls.FinishedAt
		}
		missed := e.isMissedLocked(t, rt, now, lastFinish)
		e.mu.Unlock()

		if !missed {
			continue
		}

		switch t.MissedRuns {
		case config.MissedCatchUp:
			if staggered == 0 {
				e.dispatch(t, now)
			} else {
				e.mu.Lock()
				rt.nextAt = now.Add(time.Duration(staggered) * catchUpStagger)
				rt.staggering = true
				e.mu.Unlock()
			}
			staggered++
		case config.MissedReport:
			e.mu.Lock()
			rt.overdue = true
			e.scheduleNextLocked(t, now)
			e.mu.Unlock()
			e.emit(Event{Kind: EventOverdue, Task: t.Name})
		case config.MissedIgnore:
			e.mu.Lock()
			e.scheduleNextLocked(t, now)
			e.mu.Unlock()
		}
	}
}

// isMissedLocked reports whether a due time passed without a run. Callers
// must hold e.mu.
//
// Two independent signals are needed, and neither alone is sufficient:
//
//  1. The cached next_at is well in the past. This catches a suspend or a
//     clock jump while Coucou was already running, including for a task
//     that has never run before.
//
//  2. The first occurrence after the previous run's finish time is already
//     behind us. This catches a restart, where next_at was computed forward
//     from "now" at startup and is therefore in the future even though an
//     occurrence was missed while we were closed.
//
// Signal 2 uses Next(lastFinish, lastFinish), which works for both kinds:
// cron returns the first occurrence after that instant, and an interval
// returns lastFinish plus its duration.
func (e *Engine) isMissedLocked(t *config.Task, rt *taskRuntime, now, lastFinish time.Time) bool {
	if t.Parsed == nil {
		return false
	}

	// Signal 1: the cached next_at is more than a tick behind.
	if !rt.nextAt.IsZero() && now.Sub(rt.nextAt) > time.Minute {
		return true
	}

	// Signal 2: an occurrence elapsed while we were not running. A task that
	// has never run is not overdue -- a fresh config must not fire
	// everything the moment it is first loaded.
	if lastFinish.IsZero() {
		return false
	}
	due := t.Parsed.Next(lastFinish, lastFinish)
	return !due.IsZero() && due.Before(now)
}

func (e *Engine) handleOverlap(t *config.Task, now time.Time) {
	switch t.OnOverlap {
	case config.OverlapSkip:
		e.mu.Lock()
		e.scheduleNextLocked(t, now)
		// state.State is not internally synchronized, so its mutation must
		// stay under e.mu alongside every other reader/writer of it.
		e.state.Record(t.Name,
			state.RunRecord{At: now, Outcome: OutcomeSkipped},
			state.LastRun{Outcome: OutcomeSkipped, ExitCode: -1})
		e.saveState()
		e.mu.Unlock()
		e.emit(Event{Kind: EventSkipped, Task: t.Name})

	case config.OverlapQueue:
		e.mu.Lock()
		e.rt[t.Name].queued = true
		e.scheduleNextLocked(t, now)
		e.mu.Unlock()

	case config.OverlapReplace:
		e.mu.Lock()
		if cancel := e.rt[t.Name].cancel; cancel != nil {
			cancel()
		}
		e.rt[t.Name].queued = true
		e.scheduleNextLocked(t, now)
		e.mu.Unlock()
	}
}

// dispatch starts a run in its own goroutine.
func (e *Engine) dispatch(t *config.Task, now time.Time) {
	ctx, cancel := context.WithCancel(context.Background())

	e.mu.Lock()
	rt := e.rt[t.Name]
	if rt == nil || rt.running {
		e.mu.Unlock()
		cancel()
		return
	}
	rt.running = true
	rt.overdue = false
	rt.staggering = false
	rt.cancel = cancel
	e.scheduleNextLocked(t, now)
	e.mu.Unlock()

	e.emit(Event{Kind: EventStarted, Task: t.Name})

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancel()

		res := e.runner.Run(ctx, e.cfg, t)

		// state.State is not internally synchronized: its mutation, and the
		// rt bookkeeping it interacts with, must all happen under e.mu, the
		// same lock every other reader of state uses (scheduleNextLocked,
		// applyMissedPolicies, Tick).
		e.mu.Lock()
		e.state.Record(t.Name,
			state.RunRecord{
				At:         res.StartedAt,
				Outcome:    string(res.Outcome),
				DurationMS: res.FinishedAt.Sub(res.StartedAt).Milliseconds(),
			},
			state.LastRun{
				StartedAt:  res.StartedAt,
				FinishedAt: res.FinishedAt,
				ExitCode:   res.ExitCode,
				Outcome:    string(res.Outcome),
			})
		e.saveState()

		rt := e.rt[t.Name]
		rt.running = false
		rt.cancel = nil
		// An interval anchors on the finish time, so recompute now.
		if t.Parsed != nil && t.Parsed.Kind() == schedule.KindInterval {
			rt.nextAt = t.Parsed.Next(res.FinishedAt, e.clock.Now())
		}
		queued := rt.queued
		rt.queued = false
		e.mu.Unlock()

		r := res
		e.emit(Event{Kind: EventFinished, Task: t.Name, Result: &r})

		if queued {
			e.dispatch(t, e.clock.Now())
		}
	}()
}

// scheduleNextLocked recomputes next_at. Callers must hold e.mu.
func (e *Engine) scheduleNextLocked(t *config.Task, now time.Time) {
	rt := e.rt[t.Name]
	if rt == nil || t.Parsed == nil {
		return
	}
	var prev time.Time
	if ls := e.state.Get(t.Name).LastRun; ls != nil {
		prev = ls.FinishedAt
	}
	next := t.Parsed.Next(prev, now)
	// Next must always land strictly after now. An interval anchored on a
	// stale prev (e.g. a last run long ago) would otherwise stay in the
	// past forever, making report/ignore behave like catch_up: the very
	// next tick would see it as due again. Re-anchor on now itself when
	// that happens; cron is unaffected since it already ignores prev.
	if !next.After(now) {
		next = t.Parsed.Next(now, now)
	}
	rt.nextAt = next
}

func (e *Engine) saveState() {
	// A state file we cannot write is a degraded scheduler, not a dead one.
	_ = e.state.Save(e.cfg.StateDirPath())
}

func (e *Engine) emit(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return
	}
	select {
	case e.events <- ev:
	default: // never block scheduling on a slow subscriber
	}
}

// NextAt reports when a task is next due.
func (e *Engine) NextAt(name string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rt := e.rt[name]; rt != nil {
		return rt.nextAt
	}
	return time.Time{}
}

// Running reports whether a task has a run in flight.
func (e *Engine) Running(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.rt[name]
	return rt != nil && rt.running
}

// Overdue reports whether a task missed a due time under the report policy.
func (e *Engine) Overdue(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.rt[name]
	return rt != nil && rt.overdue
}

// Wait blocks until no run is in flight.
func (e *Engine) Wait() { e.wg.Wait() }

// Stop cancels every running task and closes the event channel. It is safe
// to call concurrently with Tick: emit holds e.mu across every send, and
// Stop closes the channel under the same lock after every run has finished,
// so a close and a send can never race.
func (e *Engine) Stop() {
	e.mu.Lock()
	for _, rt := range e.rt {
		if rt.cancel != nil {
			rt.cancel()
		}
	}
	e.mu.Unlock()
	e.wg.Wait()
	e.mu.Lock()
	e.stopped = true
	close(e.events)
	e.mu.Unlock()
}
