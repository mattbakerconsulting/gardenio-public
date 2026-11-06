package poolmanager

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestParseTime(t *testing.T) {
	tests := []struct {
		input string
		want  bool // true if parsed successfully
	}{
		{"2026-03-06T04:15:00Z", true},
		{"2026-03-06T04:15:00", true},
		{"2026-03-06 04:15:00", true},
		{"", false},
		{"invalid", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := parseTime(tt.input)
			if tt.want && result.IsZero() {
				t.Errorf("parseTime(%q) returned zero time, expected valid time", tt.input)
			}
			if !tt.want && !result.IsZero() {
				t.Errorf("parseTime(%q) returned %v, expected zero time", tt.input, result)
			}
		})
	}
}

func TestPrewarmedPoolConfig_Defaults(t *testing.T) {
	cfg := beadsapi.PrewarmedPoolConfig{
		Enabled: true,
		MinSize: 2,
		MaxSize: 5,
		Role:    "thread",
		Mode:    "crew",
	}

	if cfg.MinSize != 2 {
		t.Errorf("expected MinSize 2, got %d", cfg.MinSize)
	}
	if cfg.MaxSize != 5 {
		t.Errorf("expected MaxSize 5, got %d", cfg.MaxSize)
	}
	if !cfg.Enabled {
		t.Error("expected Enabled true")
	}
}
