package advice

import (
	"testing"
	"time"
)

func TestParseDuration_Stdlib(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"4h", 4 * time.Hour},
		{"24h", 24 * time.Hour},
		{"30m", 30 * time.Minute},
		{"4h30m", 4*time.Hour + 30*time.Minute},
		{"90s", 90 * time.Second},
		{"1.5h", 90 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseDuration(tc.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDuration_Extended(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"4 hours", 4 * time.Hour},
		{"7 days", 7 * 24 * time.Hour},
		{"30 minutes", 30 * time.Minute},
		{"1 week", 7 * 24 * time.Hour},
		{"2 weeks 3 days", 17 * 24 * time.Hour},
		{"1 day 12 hours", 36 * time.Hour},
		{"45 sec", 45 * time.Second},
		{"5 mins", 5 * time.Minute},
		{"1hr", time.Hour},
		{"  3 d  ", 3 * 24 * time.Hour},
		{"1.5d", 36 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseDuration(tc.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDuration_Errors(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"abc",
		"7",          // no unit
		"7 fortnights", // unknown unit
		"d7",         // unit before number
		"4h-30m",     // negative segment
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseDuration(in); err == nil {
				t.Fatalf("ParseDuration(%q) expected error, got nil", in)
			}
		})
	}
}
