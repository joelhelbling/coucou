package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestParseMacroDesugaring(t *testing.T) {
	tests := []struct {
		expr string
		cron string // the equivalent cron expression
	}{
		{"@hourly", "0 * * * *"},
		{"@hourly at :15", "15 * * * *"},
		{"@daily", "0 0 * * *"},
		{"@daily at 17:00", "0 17 * * *"},
		{"@weekly", "0 0 * * 0"},
		{"@weekly on mon at 18:30", "30 18 * * 1"},
		{"@monthly", "0 0 1 * *"},
		{"@monthly on 1 at 09:00", "0 9 1 * *"},
	}

	after := time.Date(2026, 7, 29, 14, 32, 0, 0, time.UTC)

	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := Parse(tc.expr)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.expr, err)
			}
			want, err := Parse(tc.cron)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.cron, err)
			}
			gotNext := got.Next(time.Time{}, after)
			wantNext := want.Next(time.Time{}, after)
			if !gotNext.Equal(wantNext) {
				t.Errorf("%q -> next %v; %q -> next %v",
					tc.expr, gotNext, tc.cron, wantNext)
			}
		})
	}
}

func TestParseInterval(t *testing.T) {
	s, err := Parse("@every 90s")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Kind() != KindInterval {
		t.Errorf("Kind() = %v, want KindInterval", s.Kind())
	}

	now := time.Date(2026, 7, 29, 14, 32, 0, 0, time.UTC)
	prev := time.Date(2026, 7, 29, 14, 30, 0, 0, time.UTC)

	// Anchored on the previous FINISH time, not on now.
	if got, want := s.Next(prev, now), prev.Add(90*time.Second); !got.Equal(want) {
		t.Errorf("Next(prev, now) = %v, want %v", got, want)
	}
	// With no previous run, anchored on now.
	if got, want := s.Next(time.Time{}, now), now.Add(90*time.Second); !got.Equal(want) {
		t.Errorf("Next(zero, now) = %v, want %v", got, want)
	}
}

func TestParseIntervalCompound(t *testing.T) {
	s, err := Parse("@every 2h30m")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	now := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	want := now.Add(150 * time.Minute)
	if got := s.Next(time.Time{}, now); !got.Equal(want) {
		t.Errorf("Next() = %v, want %v", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{
		"",
		"@yearly",           // not in our grammar
		"@daily at 25:00",   // impossible hour
		"@daily at noon",    // not a time
		"@weekly on funday", // not a day
		"@monthly on 32",    // not a day of month
		"@hourly on mon",    // "on" is meaningless for hourly
		"@every",            // missing duration
		"@every 500ms",      // below the 1s floor
		"@every yesterday",  // not a duration
		"30 18 * *",         // four fields
		"nonsense",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", expr)
		}
	}
}

func TestHuman(t *testing.T) {
	tests := []struct{ expr, contains string }{
		{"@daily at 17:00", "17:00"},
		{"@weekly on mon at 18:30", "Monday"},
		{"@every 90s", "1m30s"},
	}
	for _, tc := range tests {
		s, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		if !strings.Contains(s.Human(), tc.contains) {
			t.Errorf("Human() for %q = %q, want it to contain %q",
				tc.expr, s.Human(), tc.contains)
		}
	}
}
