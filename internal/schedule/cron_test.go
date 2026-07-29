package schedule

import (
	"testing"
	"time"
)

func TestParseCronRejectsBadInput(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 32 * *", "* * * 13 *", "* * * * 8", "x * * * *",
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) expected error, got nil", expr)
		}
	}
}

func TestMatchesDay(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}

	tests := []struct {
		name string
		expr string
		when time.Time
		want bool
	}{
		// 2026-08-03 is a Monday; 2026-08-01 is a Saturday.
		{"every day", "0 0 * * *", day(2026, 8, 3), true},
		{"dow only matches", "0 0 * * mon", day(2026, 8, 3), true},
		{"dow only rejects", "0 0 * * mon", day(2026, 8, 4), false},
		{"dom only matches", "0 0 1 * *", day(2026, 8, 1), true},
		{"dom only rejects", "0 0 1 * *", day(2026, 8, 2), false},
		{"month rejects", "0 0 * 9 *", day(2026, 8, 3), false},
		{"month matches", "0 0 * 9 *", day(2026, 9, 3), true},

		// POSIX quirk: when BOTH dom and dow are restricted they are ORed.
		{"dom or dow: matches dom", "0 0 1 * mon", day(2026, 8, 1), true},
		{"dom or dow: matches dow", "0 0 1 * mon", day(2026, 8, 3), true},
		{"dom or dow: matches neither", "0 0 1 * mon", day(2026, 8, 4), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCron(tc.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tc.expr, err)
			}
			if got := c.matchesDay(tc.when); got != tc.want {
				t.Errorf("matchesDay(%s) = %v, want %v",
					tc.when.Format("2006-01-02 Mon"), got, tc.want)
			}
		})
	}
}
