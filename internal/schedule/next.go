package schedule

import "time"

// maxSearchDays bounds the forward scan. Four years covers every leap-year
// cycle, so any expression that can ever match will match inside it.
const maxSearchDays = 4 * 366

// Next returns the first instant strictly after `after` that matches the
// spec, in after's location. It returns the zero time if the expression can
// never match (for example "0 0 30 2 *").
func (c *CronSpec) Next(after time.Time) time.Time {
	loc := after.Location()

	// Begin at the start of the minute following `after`.
	start := after.Truncate(time.Minute).Add(time.Minute)
	startY, startM, startD := start.Date()

	for offset := 0; offset < maxSearchDays; offset++ {
		day := time.Date(startY, startM, startD+offset, 0, 0, 0, 0, loc)
		if !c.matchesDay(day) {
			continue
		}

		// On the first day, skip any hour/minute already behind us.
		minHour, minMinute := 0, 0
		if offset == 0 {
			minHour, minMinute = start.Hour(), start.Minute()
		}

		for h := minHour; h < 24; h++ {
			if !c.Hour.has(h) {
				continue
			}
			m0 := 0
			if h == minHour {
				m0 = minMinute
			}
			for m := m0; m < 60; m++ {
				if !c.Minute.has(m) {
					continue
				}
				t := wallTime(day.Year(), day.Month(), day.Day(), h, m, loc)
				// A snapped spring-forward time can land at or before
				// `after`; keep searching if so.
				if t.After(after) {
					return t
				}
			}
		}
	}
	return time.Time{}
}

// wallTime builds the instant for a local wall-clock time.
//
// When the requested wall time does not exist because the clock sprang
// forward over it, time.Date normalizes to some other wall time. The spec
// requires running once at the next VALID minute instead, so we detect the
// mismatch and scan the day for the first instant whose wall clock is at or
// after what was asked for.
func wallTime(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, h, mi, 0, 0, loc)
	if t.Hour() == h && t.Minute() == mi && t.Day() == d {
		return t
	}
	return firstValidAtOrAfter(y, mo, d, h, mi, loc)
}

func firstValidAtOrAfter(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	cursor := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	// A day is at most 25 hours long when the clock falls back.
	for i := 0; i < 25*60; i++ {
		c := cursor.Add(time.Duration(i) * time.Minute)
		if c.Day() != d {
			break
		}
		if c.Hour() > h || (c.Hour() == h && c.Minute() >= mi) {
			return c
		}
	}
	return time.Date(y, mo, d, h, mi, 0, 0, loc)
}
