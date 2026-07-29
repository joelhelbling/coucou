package schedule

import (
	"fmt"
	"strconv"
	"strings"
)

// field is a bitmask of permitted values for one cron field.
// Bit i is set when value i is permitted. Max value is 59, so uint64 suffices.
type field struct{ bits uint64 }

func (f field) has(v int) bool {
	if v < 0 || v > 63 {
		return false
	}
	return f.bits&(1<<uint(v)) != 0
}

func (f *field) set(v int) { f.bits |= 1 << uint(v) }

// parseField parses one cron field. restricted reports whether the expression
// constrains the field at all; it is false only for "*".
func parseField(s string, min, max int, names map[string]int) (field, bool, error) {
	var f field
	if s == "" {
		return f, false, fmt.Errorf("empty field")
	}
	if s == "*" {
		for v := min; v <= max; v++ {
			f.set(v)
		}
		return f, false, nil
	}

	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return field{}, false, fmt.Errorf("empty element in %q", s)
		}

		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return field{}, false, fmt.Errorf("bad step in %q", part)
			}
			step = n
			part = part[:i]
		}

		var lo, hi int
		switch {
		case part == "*":
			lo, hi = min, max
		case strings.Contains(part, "-"):
			bits := strings.SplitN(part, "-", 2)
			var err error
			if lo, err = parseValue(bits[0], min, max, names); err != nil {
				return field{}, false, err
			}
			if hi, err = parseValue(bits[1], min, max, names); err != nil {
				return field{}, false, err
			}
			if lo > hi {
				return field{}, false, fmt.Errorf("range %q is inverted", part)
			}
		default:
			v, err := parseValue(part, min, max, names)
			if err != nil {
				return field{}, false, err
			}
			lo, hi = v, v
		}

		for v := lo; v <= hi; v += step {
			f.set(v)
		}
	}
	return f, true, nil
}

func parseValue(s string, min, max int, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number or known name", s)
	}
	// POSIX allows 7 for Sunday in the day-of-week field.
	if max == 6 && v == 7 {
		v = 0
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%d is out of range %d-%d", v, min, max)
	}
	return v, nil
}
