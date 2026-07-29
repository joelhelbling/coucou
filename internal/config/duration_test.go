package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		yaml string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"2h30m", 150 * time.Minute},
		{"500ms", 500 * time.Millisecond},
	}
	for _, tc := range tests {
		var d Duration
		if err := yaml.Unmarshal([]byte(tc.yaml), &d); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.yaml, err)
		}
		if d.Std() != tc.want {
			t.Errorf("%q = %v, want %v", tc.yaml, d.Std(), tc.want)
		}
	}
}

func TestDurationUnmarshalErrors(t *testing.T) {
	for _, s := range []string{"soon", "30", "-5s"} {
		var d Duration
		if err := yaml.Unmarshal([]byte(s), &d); err == nil {
			t.Errorf("unmarshal %q expected error, got nil", s)
		}
	}
}
