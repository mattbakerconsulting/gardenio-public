package main

import (
	"testing"
	"time"
)

func TestFmtPct(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0%"},
		{0.5, "50.0%"},
		{0.123, "12.3%"},
		{1.0, "100%"},
		{0.999, "99.9%"},
		{0.9999, "100%"},
		{0.0001, "0.0%"},
		{0.005, "0.5%"},
	}
	for _, tt := range tests {
		got := fmtPct(tt.in)
		if got != tt.want {
			t.Errorf("fmtPct(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{1 * time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "1d1h"},
		{48 * time.Hour, "2d"},
		{49 * time.Hour, "2d1h"},
	}
	for _, tt := range tests {
		got := fmtDuration(tt.in)
		if got != tt.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseFieldFloat(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"0", 0},
		{"0.5", 0.5},
		{"1.0", 1.0},
		{"0.1234", 0.1234},
		{"garbage", 0},
	}
	for _, tt := range tests {
		got := parseFieldFloat(tt.in)
		if got != tt.want {
			t.Errorf("parseFieldFloat(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
