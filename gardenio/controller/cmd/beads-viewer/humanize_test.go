package main

import (
	"testing"
	"time"
)

func TestHumanizeTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, ""},
		{"future clock skew", now.Add(2 * time.Second), "just now"},
		{"sub-minute", now.Add(-30 * time.Second), "just now"},
		{"one minute", now.Add(-time.Minute), "1 minute ago"},
		{"three minutes", now.Add(-3 * time.Minute), "3 minutes ago"},
		{"one hour", now.Add(-time.Hour), "1 hour ago"},
		{"two hours", now.Add(-2 * time.Hour), "2 hours ago"},
		{"one day", now.Add(-24 * time.Hour), "1 day ago"},
		{"two days", now.Add(-48 * time.Hour), "2 days ago"},
		{"one week", now.Add(-7 * 24 * time.Hour), "1 week ago"},
		{"three weeks", now.Add(-21 * 24 * time.Hour), "3 weeks ago"},
		{"one month", now.Add(-30 * 24 * time.Hour), "1 month ago"},
		{"six months", now.Add(-180 * 24 * time.Hour), "6 months ago"},
		{"one year", now.Add(-365 * 24 * time.Hour), "1 year ago"},
		{"two years", now.Add(-730 * 24 * time.Hour), "2 years ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := humanizeTime(tc.t)
			if got != tc.want {
				t.Errorf("humanizeTime(%v) = %q, want %q", tc.t, got, tc.want)
			}
		})
	}
}
