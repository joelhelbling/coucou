package schedule

import (
	"fmt"
	"strings"
	"time"
)

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// CronSpec is a parsed five-field cron expression.
type CronSpec struct {
	Minute, Hour, Dom, Month, Dow field
	domRestricted, dowRestricted  bool
	expr                          string
}

func (c *CronSpec) String() string { return c.expr }

// ParseCron parses a standard five-field cron expression:
// minute hour day-of-month month day-of-week
func ParseCron(expr string) (*CronSpec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("expected 5 fields, got %d", len(parts))
	}

	c := &CronSpec{expr: expr}
	var err error

	if c.Minute, _, err = parseField(parts[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if c.Hour, _, err = parseField(parts[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if c.Dom, c.domRestricted, err = parseField(parts[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if c.Month, _, err = parseField(parts[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if c.Dow, c.dowRestricted, err = parseField(parts[4], 0, 6, dayNames); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	return c, nil
}

// matchesDay reports whether the calendar day of t is permitted.
//
// It implements the POSIX rule: when BOTH day-of-month and day-of-week are
// restricted, a day matches if EITHER matches. When only one is restricted,
// only that one applies.
func (c *CronSpec) matchesDay(t time.Time) bool {
	if !c.Month.has(int(t.Month())) {
		return false
	}
	dom := c.Dom.has(t.Day())
	dow := c.Dow.has(int(t.Weekday()))

	switch {
	case c.domRestricted && c.dowRestricted:
		return dom || dow
	case c.domRestricted:
		return dom
	case c.dowRestricted:
		return dow
	default:
		return true
	}
}
