package poolmanager

import (
	"testing"
	"time"
)

func TestSortByAge(t *testing.T) {
	now := time.Now()
	agents := []prewarmedAgent{
		{ID: "c", CreatedAt: now.Add(-1 * time.Minute)},
		{ID: "a", CreatedAt: now.Add(-3 * time.Minute)},
		{ID: "b", CreatedAt: now.Add(-2 * time.Minute)},
	}

	sortByAge(agents)

	if agents[0].ID != "a" || agents[1].ID != "b" || agents[2].ID != "c" {
		t.Errorf("expected [a,b,c], got [%s,%s,%s]", agents[0].ID, agents[1].ID, agents[2].ID)
	}
}

func TestSortByAge_Empty(t *testing.T) {
	var agents []prewarmedAgent
	sortByAge(agents) // should not panic
}

func TestSortByAge_Single(t *testing.T) {
	agents := []prewarmedAgent{{ID: "a", CreatedAt: time.Now()}}
	sortByAge(agents)
	if agents[0].ID != "a" {
		t.Errorf("expected [a], got [%s]", agents[0].ID)
	}
}
