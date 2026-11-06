package scheduler

import (
	"testing"
	"time"
)

func TestParseDurationField(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		defVal time.Duration
		want   time.Duration
	}{
		{"empty falls back to default", "", 30 * time.Second, 30 * time.Second},
		{"valid duration string", "5m", 1 * time.Second, 5 * time.Minute},
		{"integer interpreted as minutes", "7", 1 * time.Second, 7 * time.Minute},
		{"garbage falls back to default", "not-a-duration", 42 * time.Second, 42 * time.Second},
		{"zero integer falls back to default", "0", 11 * time.Second, 11 * time.Second},
		{"negative duration falls back to default", "-3s", 9 * time.Second, 9 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDurationField(tc.in, tc.defVal)
			if got != tc.want {
				t.Fatalf("parseDurationField(%q, %v) = %v; want %v", tc.in, tc.defVal, got, tc.want)
			}
		})
	}
}
