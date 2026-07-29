package schedule

import (
	"fmt"
	"strconv"
	"strings"
)

// weekdayNames gives the full English weekday name for a cron dow value
// (0-6, Sunday = 0), matching the dayNames map in cron.go.
var weekdayNames = [...]string{
	"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday",
}

// expandMacro converts a macro expression such as "@weekly on mon at 18:30"
// into the equivalent five-field cron expression, along with a human-readable
// description of that same intent. It returns ok=false when expr is not a
// macro at all, so the caller can try other forms.
func expandMacro(expr string) (cron string, human string, ok bool, err error) {
	fields := strings.Fields(expr)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "@") {
		return "", "", false, nil
	}

	macro := strings.ToLower(fields[0])
	switch macro {
	case "@hourly", "@daily", "@weekly", "@monthly":
	default:
		return "", "", true, fmt.Errorf("unknown macro %q "+
			"(expected @hourly, @daily, @weekly, @monthly or @every)", macro)
	}

	var onArg, atArg string
	rest := fields[1:]
	for len(rest) > 0 {
		switch strings.ToLower(rest[0]) {
		case "on":
			if len(rest) < 2 {
				return "", "", true, fmt.Errorf("%q: 'on' needs a value", expr)
			}
			onArg, rest = rest[1], rest[2:]
		case "at":
			if len(rest) < 2 {
				return "", "", true, fmt.Errorf("%q: 'at' needs a value", expr)
			}
			atArg, rest = rest[1], rest[2:]
		default:
			return "", "", true, fmt.Errorf("%q: unexpected %q "+
				"(expected 'on' or 'at')", expr, rest[0])
		}
	}

	// Defaults matching the POSIX macros.
	minute, hour := 0, 0

	switch macro {
	case "@hourly":
		if onArg != "" {
			return "", "", true, fmt.Errorf("@hourly does not take 'on'")
		}
		if atArg != "" {
			m, err := parseMinuteOfHour(atArg)
			if err != nil {
				return "", "", true, err
			}
			minute = m
		}
		cron := fmt.Sprintf("%d * * * *", minute)
		human := fmt.Sprintf("at :%02d every hour", minute)
		return cron, human, true, nil

	case "@daily":
		if onArg != "" {
			return "", "", true, fmt.Errorf("@daily does not take 'on'")
		}
		if atArg != "" {
			var err error
			if hour, minute, err = parseTimeOfDay(atArg); err != nil {
				return "", "", true, err
			}
		}
		cron := fmt.Sprintf("%d %d * * *", minute, hour)
		human := fmt.Sprintf("at %02d:%02d every day", hour, minute)
		return cron, human, true, nil

	case "@weekly":
		dow := 0 // Sunday
		if onArg != "" {
			d, ok := dayNames[strings.ToLower(onArg)]
			if !ok {
				return "", "", true, fmt.Errorf("%q is not a day name "+
					"(expected sun, mon, tue, wed, thu, fri or sat)", onArg)
			}
			dow = d
		}
		if atArg != "" {
			var err error
			if hour, minute, err = parseTimeOfDay(atArg); err != nil {
				return "", "", true, err
			}
		}
		cron := fmt.Sprintf("%d %d * * %d", minute, hour, dow)
		human := fmt.Sprintf("at %02d:%02d every %s", hour, minute, weekdayNames[dow])
		return cron, human, true, nil

	case "@monthly":
		dom := 1
		if onArg != "" {
			d, err := strconv.Atoi(onArg)
			if err != nil || d < 1 || d > 31 {
				return "", "", true, fmt.Errorf("%q is not a day of month 1-31", onArg)
			}
			dom = d
		}
		if atArg != "" {
			var err error
			if hour, minute, err = parseTimeOfDay(atArg); err != nil {
				return "", "", true, err
			}
		}
		cron := fmt.Sprintf("%d %d %d * *", minute, hour, dom)
		human := fmt.Sprintf("at %02d:%02d on day %d of every month", hour, minute, dom)
		return cron, human, true, nil
	}
	return "", "", true, fmt.Errorf("unhandled macro %q", macro)
}

// parseTimeOfDay parses "17:00".
func parseTimeOfDay(s string) (hour, minute int, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%q is not a time like 17:00", s)
	}
	if hour, err = strconv.Atoi(parts[0]); err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("%q does not have an hour in 0-23", s)
	}
	if minute, err = strconv.Atoi(parts[1]); err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q does not have a minute in 0-59", s)
	}
	return hour, minute, nil
}

// parseMinuteOfHour parses ":15" or "15".
func parseMinuteOfHour(s string) (int, error) {
	m, err := strconv.Atoi(strings.TrimPrefix(s, ":"))
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is not a minute like :15", s)
	}
	return m, nil
}
