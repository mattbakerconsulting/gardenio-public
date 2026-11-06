package main

import (
	"testing"
	"time"
)

func TestParseDuration_StandardDurations(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"1h", time.Hour},
		{"30m", 30 * time.Minute},
		{"24h", 24 * time.Hour},
		{"90s", 90 * time.Second},
		{"2h30m", 2*time.Hour + 30*time.Minute},
	}

	for _, tt := range tests {
		got, err := parseDuration(tt.input)
		if err != nil {
			t.Errorf("parseDuration(%q) error = %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseDuration_DaysSuffix(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"1d", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"30d", 30 * 24 * time.Hour},
	}

	for _, tt := range tests {
		got, err := parseDuration(tt.input)
		if err != nil {
			t.Errorf("parseDuration(%q) error = %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseDuration_InvalidInput(t *testing.T) {
	invalids := []string{"", "abc", "d", "1x"}
	for _, s := range invalids {
		_, err := parseDuration(s)
		if err == nil {
			t.Errorf("parseDuration(%q) expected error, got nil", s)
		}
	}
}
