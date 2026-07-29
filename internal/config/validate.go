package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/joelhelbling/coucou/internal/schedule"
)

// ValidationError is one problem with the config, located by line.
type ValidationError struct {
	Line int
	Msg  string
}

// ValidationErrors is every problem found, in line order.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	sorted := make(ValidationErrors, len(e))
	copy(sorted, e)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Line < sorted[j].Line })

	var b strings.Builder
	for i, ve := range sorted {
		if i > 0 {
			b.WriteByte('\n')
		}
		if ve.Line > 0 {
			fmt.Fprintf(&b, "line %d: %s", ve.Line, ve.Msg)
		} else {
			b.WriteString(ve.Msg)
		}
	}
	return b.String()
}

// validate checks every rule and returns ALL failures, not just the first.
// It also compiles each task's schedule into Task.Parsed.
func validate(cfg *Config) ValidationErrors {
	var errs ValidationErrors
	seen := make(map[string]int, len(cfg.Tasks))

	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		add := func(format string, args ...any) {
			errs = append(errs, ValidationError{
				Line: t.Line,
				Msg:  fmt.Sprintf(format, args...),
			})
		}

		name := strings.TrimSpace(t.Name)
		switch {
		case name == "":
			add("task is missing a name")
		default:
			if prev, dup := seen[name]; dup {
				add("duplicate task name %q (first used on line %d)", name, prev)
			} else {
				seen[name] = t.Line
			}
		}

		if strings.TrimSpace(t.Command) == "" {
			add("task %q is missing a command", name)
		}

		if strings.TrimSpace(t.Schedule) == "" {
			add("task %q is missing a schedule", name)
		} else {
			parsed, err := schedule.Parse(t.Schedule)
			if err != nil {
				add("task %q: %v", name, err)
			} else {
				t.Parsed = parsed
			}
		}

		switch t.OnOverlap {
		case OverlapSkip, OverlapQueue, OverlapReplace:
		default:
			add("task %q: on_overlap %q is not one of skip, queue, replace",
				name, t.OnOverlap)
		}

		switch t.MissedRuns {
		case MissedCatchUp, MissedReport, MissedIgnore:
		default:
			add("task %q: missed_runs %q is not one of catch_up, report, ignore",
				name, t.MissedRuns)
		}
	}
	return errs
}
