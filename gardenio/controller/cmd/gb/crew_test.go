package main

import (
	"testing"
	"time"
)

func TestIsMySpawn(t *testing.T) {
	tests := []struct {
		spawner string
		myName  string
		want    bool
	}{
		{"gardener-gardenio", "gardener-gardenio", true},
		{"Gardener-Gardenio", "gardener-gardenio", true},
		{"other-gardener", "gardener-gardenio", false},
		{"", "gardener-gardenio", false},
		{"gardener-gardenio", "", false},
		{"sched-gardenio-123", "sched-gardenio-123", true},
		{"scheduler:gd-abc", "gardener-gardenio", false},
	}
	for _, tt := range tests {
		got := isMySpawn(tt.spawner, tt.myName)
		if got != tt.want {
			t.Errorf("isMySpawn(%q, %q) = %v, want %v", tt.spawner, tt.myName, got, tt.want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	tests := []struct {
		name string
		secs int
		want string
	}{
		{"seconds", 45, "45s"},
		{"minutes", 300, "5m"},
		{"hours", 3600, "1h"},
		{"hours_minutes", 5400, "1h30m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortDuration(time.Duration(tt.secs) * time.Second)
			if got != tt.want {
				t.Errorf("shortDuration(%ds) = %q, want %q", tt.secs, got, tt.want)
			}
		})
	}
}
