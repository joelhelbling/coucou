package schedule

import (
	"fmt"
	"strings"
	"time"
)

// MinInterval is the floor for @every.
const MinInterval = time.Second

type Kind int

const (
	KindCron Kind = iota
	KindInterval
)

// Schedule answers when a task should next run.
//
// Next takes both anchors because the kinds differ: cron ignores prev and
// computes forward from now, while an interval computes from prev (the
// previous run's FINISH time), falling back to now when prev is zero.
type Schedule interface {
	Next(prev, now time.Time) time.Time
	Kind() Kind
	Human() string
	String() string
}

// Parse accepts raw cron, a parameterized macro, or an @every interval.
func Parse(expr string) (Schedule, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, fmt.Errorf("schedule is empty")
	}

	if strings.HasPrefix(strings.ToLower(trimmed), "@every") {
		return parseInterval(trimmed)
	}

	if cronExpr, isMacro, err := expandMacro(trimmed); isMacro {
		if err != nil {
			return nil, err
		}
		spec, err := ParseCron(cronExpr)
		if err != nil {
			return nil, fmt.Errorf("%q expanded to %q: %w", trimmed, cronExpr, err)
		}
		return &cronSchedule{spec: spec, source: trimmed}, nil
	}

	spec, err := ParseCron(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid schedule: %w", trimmed, err)
	}
	return &cronSchedule{spec: spec, source: trimmed}, nil
}

func parseInterval(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 2 {
		return nil, fmt.Errorf("%q: @every needs one duration, like @every 90s", expr)
	}
	d, err := time.ParseDuration(fields[1])
	if err != nil {
		return nil, fmt.Errorf("%q is not a duration like 90s or 2h30m", fields[1])
	}
	if d < MinInterval {
		return nil, fmt.Errorf("@every %s is below the %s floor", fields[1], MinInterval)
	}
	return &intervalSchedule{every: d, source: expr}, nil
}

type cronSchedule struct {
	spec   *CronSpec
	source string
}

func (c *cronSchedule) Next(_ time.Time, now time.Time) time.Time { return c.spec.Next(now) }
func (c *cronSchedule) Kind() Kind                                { return KindCron }
func (c *cronSchedule) String() string                            { return c.source }

func (c *cronSchedule) Human() string {
	// Describe the expanded spec by sampling it: this keeps the description
	// honest for both raw cron and macros without a second grammar.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := c.spec.Next(base)
	if first.IsZero() {
		return c.source
	}
	second := c.spec.Next(first)

	timeOfDay := first.Format("15:04")
	switch {
	case !second.IsZero() && second.Sub(first) == 24*time.Hour:
		return fmt.Sprintf("at %s every day", timeOfDay)
	case !second.IsZero() && second.Sub(first) == 7*24*time.Hour:
		return fmt.Sprintf("at %s every %s", timeOfDay, first.Weekday())
	case !second.IsZero() && second.Sub(first) == time.Hour:
		return fmt.Sprintf("at :%02d every hour", first.Minute())
	default:
		return c.source
	}
}

type intervalSchedule struct {
	every  time.Duration
	source string
}

func (i *intervalSchedule) Next(prev, now time.Time) time.Time {
	if prev.IsZero() {
		return now.Add(i.every)
	}
	return prev.Add(i.every)
}

func (i *intervalSchedule) Kind() Kind     { return KindInterval }
func (i *intervalSchedule) String() string { return i.source }
func (i *intervalSchedule) Human() string  { return "every " + i.every.String() }
