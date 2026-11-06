package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestFormatSpawner(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "(unknown)"},
		{"sched-1773977861", "(scheduler)"},
		{"sched-1773977860", "(scheduler)"},
		{"gardener-gardenio", "gardener-gardenio"},
		{"pe7385-investigate-bvf", "pe7385-investigate-bvf"},
	}
	for _, tt := range tests {
		got := formatSpawner(tt.input)
		if got != tt.want {
			t.Errorf("formatSpawner(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFilterAgentsByTask(t *testing.T) {
	agents := []beadsapi.AgentBead{
		{ID: "gd-a1", Metadata: map[string]string{"task_id": "gd-task1"}},
		{ID: "gd-a2", Metadata: map[string]string{"task_id": "gd-task2"}},
		{ID: "gd-a3", Metadata: map[string]string{"task_id": "gd-task1"}},
		{ID: "gd-a4", Metadata: map[string]string{}}, // no task_id
	}

	// Empty task is a no-op — returns everything.
	if got := filterAgentsByTask(agents, ""); len(got) != 4 {
		t.Errorf("filterAgentsByTask(_, \"\") returned %d agents, want 4", len(got))
	}

	// Task with live crew returns exactly the matching agents.
	got := filterAgentsByTask(agents, "gd-task1")
	if len(got) != 2 {
		t.Fatalf("filterAgentsByTask(_, gd-task1) returned %d agents, want 2", len(got))
	}
	if got[0].ID != "gd-a1" || got[1].ID != "gd-a3" {
		t.Errorf("filterAgentsByTask(_, gd-task1) = [%s %s], want [gd-a1 gd-a3]", got[0].ID, got[1].ID)
	}

	// Task with no live crew returns empty (not nil-panic, not error).
	if got := filterAgentsByTask(agents, "gd-nobody"); len(got) != 0 {
		t.Errorf("filterAgentsByTask(_, gd-nobody) returned %d agents, want 0", len(got))
	}
}

func TestFilterAgentsNoGardener(t *testing.T) {
	agents := []beadsapi.AgentBead{
		{ID: "gd-real1", GardenerID: "gd-gardenerA"},
		{ID: "gd-leak1", GardenerID: "synthetic-gardener:slack-thread"},
		{ID: "gd-real2", GardenerID: "gd-gardenerB"},
		{ID: "gd-leak2", GardenerID: "synthetic-gardener:unknown"},
		{ID: "gd-leak3", GardenerID: ""}, // empty => no real gardener
	}

	got := filterAgentsNoGardener(agents)
	if len(got) != 3 {
		t.Fatalf("filterAgentsNoGardener returned %d agents, want 3", len(got))
	}
	if got[0].ID != "gd-leak1" || got[1].ID != "gd-leak2" || got[2].ID != "gd-leak3" {
		t.Errorf("filterAgentsNoGardener = [%s %s %s], want [gd-leak1 gd-leak2 gd-leak3]",
			got[0].ID, got[1].ID, got[2].ID)
	}

	// Every agent owned by a real gardener => empty (not nil-panic).
	allOwned := []beadsapi.AgentBead{
		{ID: "gd-x", GardenerID: "gd-cap"},
	}
	if got := filterAgentsNoGardener(allOwned); len(got) != 0 {
		t.Errorf("filterAgentsNoGardener(all-owned) returned %d agents, want 0", len(got))
	}
}
