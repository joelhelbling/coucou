package schedule

import "testing"

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

func TestParseField(t *testing.T) {
	tests := []struct {
		name       string
		expr       string
		min, max   int
		names      map[string]int
		want       []int // values that must be set
		notWant    []int // values that must not be set
		restricted bool
	}{
		{name: "star", expr: "*", min: 0, max: 59,
			want: []int{0, 30, 59}, restricted: false},
		{name: "single", expr: "30", min: 0, max: 59,
			want: []int{30}, notWant: []int{29, 31}, restricted: true},
		{name: "list", expr: "0,15,30", min: 0, max: 59,
			want: []int{0, 15, 30}, notWant: []int{1}, restricted: true},
		{name: "range", expr: "1-5", min: 0, max: 59,
			want: []int{1, 3, 5}, notWant: []int{0, 6}, restricted: true},
		{name: "step on star", expr: "*/15", min: 0, max: 59,
			want: []int{0, 15, 30, 45}, notWant: []int{1, 14}, restricted: true},
		{name: "step on range", expr: "10-20/5", min: 0, max: 59,
			want: []int{10, 15, 20}, notWant: []int{11, 25}, restricted: true},
		{name: "named dow", expr: "mon", min: 0, max: 6, names: dowNames,
			want: []int{1}, notWant: []int{0, 2}, restricted: true},
		{name: "named range", expr: "mon-fri", min: 0, max: 6, names: dowNames,
			want: []int{1, 5}, notWant: []int{0, 6}, restricted: true},
		{name: "sunday as 7", expr: "7", min: 0, max: 6, names: dowNames,
			want: []int{0}, restricted: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, restricted, err := parseField(tc.expr, tc.min, tc.max, tc.names)
			if err != nil {
				t.Fatalf("parseField(%q) error: %v", tc.expr, err)
			}
			if restricted != tc.restricted {
				t.Errorf("restricted = %v, want %v", restricted, tc.restricted)
			}
			for _, v := range tc.want {
				if !f.has(v) {
					t.Errorf("expected %d to be set in %q", v, tc.expr)
				}
			}
			for _, v := range tc.notWant {
				if f.has(v) {
					t.Errorf("expected %d NOT to be set in %q", v, tc.expr)
				}
			}
		})
	}
}

func TestParseFieldErrors(t *testing.T) {
	for _, expr := range []string{"", "60", "-1", "5-1", "*/0", "a", "1-", "1,,2"} {
		if _, _, err := parseField(expr, 0, 59, nil); err == nil {
			t.Errorf("parseField(%q) expected error, got nil", expr)
		}
	}
}
