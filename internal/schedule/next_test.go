package schedule

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone %s unavailable: %v", name, err)
	}
	return loc
}

func TestNextBasic(t *testing.T) {
	utc := time.UTC
	tests := []struct {
		name  string
		expr  string
		after time.Time
		want  time.Time
	}{
		{"next minute", "* * * * *",
			time.Date(2026, 7, 29, 14, 32, 10, 0, utc),
			time.Date(2026, 7, 29, 14, 33, 0, 0, utc)},
		{"daily at 17:00 later today", "0 17 * * *",
			time.Date(2026, 7, 29, 14, 32, 0, 0, utc),
			time.Date(2026, 7, 29, 17, 0, 0, 0, utc)},
		{"daily at 17:00 rolls to tomorrow", "0 17 * * *",
			time.Date(2026, 7, 29, 17, 0, 0, 0, utc),
			time.Date(2026, 7, 30, 17, 0, 0, 0, utc)},
		{"weekly monday 18:30", "30 18 * * 1",
			time.Date(2026, 7, 29, 14, 32, 0, 0, utc),
			time.Date(2026, 8, 3, 18, 30, 0, 0, utc)},
		{"monthly on the 1st", "0 9 1 * *",
			time.Date(2026, 7, 29, 14, 32, 0, 0, utc),
			time.Date(2026, 8, 1, 9, 0, 0, 0, utc)},
		{"every 15 minutes", "*/15 * * * *",
			time.Date(2026, 7, 29, 14, 32, 0, 0, utc),
			time.Date(2026, 7, 29, 14, 45, 0, 0, utc)},
		{"leap day", "0 12 29 2 *",
			time.Date(2026, 7, 29, 0, 0, 0, 0, utc),
			time.Date(2028, 2, 29, 12, 0, 0, 0, utc)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCron(tc.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tc.expr, err)
			}
			got := c.Next(tc.after)
			if !got.Equal(tc.want) {
				t.Errorf("Next() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Spring forward: on 2026-03-08 in America/New_York, 02:00 jumps to 03:00,
// so 02:30 does not exist. Per spec it must run once at the next valid
// minute, which is 03:00 EDT.
func TestNextSpringForward(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	c, err := ParseCron("30 2 * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}

	after := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	got := c.Next(after) // 2026-03-07 02:30 already passed, so this is the 8th

	want := time.Date(2026, 3, 8, 3, 0, 0, 0, ny)
	if !got.Equal(want) {
		t.Errorf("Next() = %v, want %v (the skipped 02:30 must snap to 03:00)",
			got, want)
	}
}

// Fall back: on 2026-11-01 in America/New_York, 02:00 returns to 01:00, so
// 01:30 happens twice. Per spec it must run once.
func TestNextFallBackRunsOnce(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	c, err := ParseCron("30 1 * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}

	// Start before the first 01:30 of that morning.
	after := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)

	first := c.Next(after)
	if first.Hour() != 1 || first.Minute() != 30 || first.Day() != 1 {
		t.Fatalf("first occurrence = %v, want 2026-11-01 01:30", first)
	}

	// The next occurrence must be the following DAY, not the repeated hour.
	second := c.Next(first)
	if second.Day() != 2 {
		t.Errorf("second occurrence = %v, want 2026-11-02 01:30 "+
			"(the repeated 01:30 must not fire twice)", second)
	}
}

func TestNextNoMatchReturnsZero(t *testing.T) {
	// February 30th never happens.
	c, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if got := c.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Errorf("Next() = %v, want zero time", got)
	}
}
