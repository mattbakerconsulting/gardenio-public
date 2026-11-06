package decision

import (
	"testing"
	"time"
)

// TestSortDecisionsByPriority tests that decisions are sorted by priority then time.
func TestSortDecisionsByPriority(t *testing.T) {
	m := New(nil, "test")
	m.filter = "all"

	now := time.Now()
	decisions := []DecisionItem{
		{ID: "1", Urgency: "low", Priority: 3, RequestedAt: now.Add(-1 * time.Hour)},
		{ID: "2", Urgency: "high", Priority: 1, RequestedAt: now.Add(-2 * time.Hour)},
		{ID: "3", Urgency: "medium", Priority: 2, RequestedAt: now},
		{ID: "4", Urgency: "high", Priority: 1, RequestedAt: now}, // newer high
		{ID: "5", Urgency: "low", Priority: 3, RequestedAt: now},  // newer low
	}

	sorted := m.filterDecisions(decisions)

	// Expected order: priority 1 (newer first), priority 2, priority 3 (newer first)
	expectedIDs := []string{"4", "2", "3", "5", "1"}

	if len(sorted) != len(expectedIDs) {
		t.Fatalf("Expected %d decisions, got %d", len(expectedIDs), len(sorted))
	}

	for i, expected := range expectedIDs {
		if sorted[i].ID != expected {
			t.Errorf("Position %d: expected ID '%s', got '%s'", i, expected, sorted[i].ID)
		}
	}
}

// TestNewModel tests the model constructor.
func TestNewModel(t *testing.T) {
	m := New(nil, "test-actor")

	if m == nil {
		t.Fatal("New() returned nil")
	}

	if m.filter != "all" {
		t.Errorf("default filter = %q, want %q", m.filter, "all")
	}

	if m.inputMode != ModeNormal {
		t.Errorf("default inputMode = %v, want ModeNormal", m.inputMode)
	}

	if m.selected != 0 {
		t.Errorf("default selected = %d, want 0", m.selected)
	}

	if m.selectedOption != 0 {
		t.Errorf("default selectedOption = %d, want 0", m.selectedOption)
	}

	if m.showHelp {
		t.Error("default showHelp should be false")
	}

	if m.actor != "test-actor" {
		t.Errorf("actor = %q, want %q", m.actor, "test-actor")
	}
}

// TestSetFilter tests the SetFilter method.
func TestSetFilter(t *testing.T) {
	m := New(nil, "test")

	tests := []string{"all", "high", "medium", "low"}
	for _, filter := range tests {
		m.SetFilter(filter)
		if m.filter != filter {
			t.Errorf("SetFilter(%q): filter = %q, want %q", filter, m.filter, filter)
		}
	}
}

// TestFilterDecisions tests the filter functionality.
func TestFilterDecisions(t *testing.T) {
	m := New(nil, "test")
	now := time.Now()

	decisions := []DecisionItem{
		{ID: "1", Urgency: "high", Priority: 1, RequestedAt: now.Add(-1 * time.Hour)},
		{ID: "2", Urgency: "medium", Priority: 2, RequestedAt: now},
		{ID: "3", Urgency: "low", Priority: 3, RequestedAt: now.Add(-2 * time.Hour)},
	}

	t.Run("filter all", func(t *testing.T) {
		m.SetFilter("all")
		result := m.filterDecisions(decisions)
		if len(result) != 3 {
			t.Errorf("filter 'all': got %d decisions, want 3", len(result))
		}
	})

	t.Run("filter high", func(t *testing.T) {
		m.SetFilter("high")
		result := m.filterDecisions(decisions)
		if len(result) != 1 {
			t.Errorf("filter 'high': got %d decisions, want 1", len(result))
		}
		if result[0].ID != "1" {
			t.Errorf("filter 'high': got ID %s, want '1'", result[0].ID)
		}
	})

	t.Run("filter medium", func(t *testing.T) {
		m.SetFilter("medium")
		result := m.filterDecisions(decisions)
		if len(result) != 1 {
			t.Errorf("filter 'medium': got %d decisions, want 1", len(result))
		}
		if result[0].ID != "2" {
			t.Errorf("filter 'medium': got ID %s, want '2'", result[0].ID)
		}
	})

	t.Run("filter low", func(t *testing.T) {
		m.SetFilter("low")
		result := m.filterDecisions(decisions)
		if len(result) != 1 {
			t.Errorf("filter 'low': got %d decisions, want 1", len(result))
		}
		if result[0].ID != "3" {
			t.Errorf("filter 'low': got ID %s, want '3'", result[0].ID)
		}
	})
}

// TestInputModeConstants tests that input mode constants are distinct.
func TestInputModeConstants(t *testing.T) {
	if ModeNormal == ModeRationale {
		t.Error("ModeNormal should not equal ModeRationale")
	}
}

// TestDecisionItemWithOptions tests that DecisionItem works with options.
func TestDecisionItemWithOptions(t *testing.T) {
	now := time.Now()

	d := DecisionItem{
		ID:     "bd-test-123",
		Prompt: "Which approach?",
		Options: []DecisionOption{
			{ID: "a", Label: "Fast approach", Description: "Quick but risky"},
			{ID: "b", Label: "Safe approach", Description: "Slower but reliable"},
		},
		Urgency:     "high",
		Priority:    1,
		RequestedBy: "agent-1",
		RequestedAt: now,
		Context:     `{"key": "value"}`,
	}

	if d.ID != "bd-test-123" {
		t.Errorf("ID = %q, want %q", d.ID, "bd-test-123")
	}
	if len(d.Options) != 2 {
		t.Fatalf("len(Options) = %d, want 2", len(d.Options))
	}
	if d.Options[0].ID != "a" {
		t.Errorf("Options[0].ID = %q, want %q", d.Options[0].ID, "a")
	}
	if d.Options[1].Label != "Safe approach" {
		t.Errorf("Options[1].Label = %q, want %q", d.Options[1].Label, "Safe approach")
	}
}

// TestPriorityToUrgency tests the priority-to-urgency conversion.
func TestPriorityToUrgency(t *testing.T) {
	tests := []struct {
		priority int
		want     string
	}{
		{0, "high"},
		{1, "high"},
		{2, "medium"},
		{3, "low"},
		{4, "low"},
	}

	for _, tt := range tests {
		got := priorityToUrgency(tt.priority)
		if got != tt.want {
			t.Errorf("priorityToUrgency(%d) = %q, want %q", tt.priority, got, tt.want)
		}
	}
}
