package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildProjectAccountOverride(t *testing.T) {
	accounts := map[string]beadsapi.AccountInfo{
		"zeta": {
			Name:   "zeta",
			Status: "active",
			Labels: []string{"project:gardenio"},
		},
		"alpha": {
			Name:   "alpha",
			Status: "", // empty status is treated as active
			Labels: []string{"project:gardenio", "project:monorepo"},
		},
		"exhausted": {
			Name:   "exhausted",
			Status: "exhausted",
			Labels: []string{"project:gardenio"},
		},
		"cooldown": {
			Name:   "cooldown",
			Status: "cooldown",
			Labels: []string{"project:monorepo"},
		},
		"global": {
			Name:   "global",
			Status: "active",
			Labels: nil,
		},
		"other": {
			Name:   "other",
			Status: "active",
			Labels: []string{"project:other"},
		},
	}

	got := buildProjectAccountOverride(accounts)

	want := map[string]string{
		"gardenio": "alpha",
		"monorepo": "alpha",
		"other":    "other",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d entries, got %d: %+v", len(want), len(got), got)
	}
	for project, acct := range want {
		if got[project] != acct {
			t.Errorf("project %q: got account %q, want %q", project, got[project], acct)
		}
	}
}

func TestBuildProjectAccountOverride_Empty(t *testing.T) {
	if got := buildProjectAccountOverride(nil); len(got) != 0 {
		t.Errorf("nil input: expected empty map, got %+v", got)
	}
	if got := buildProjectAccountOverride(map[string]beadsapi.AccountInfo{}); len(got) != 0 {
		t.Errorf("empty input: expected empty map, got %+v", got)
	}
}

func TestBuildProjectAccountOverride_PrimaryMarker(t *testing.T) {
	// When an account has primary=true, it wins over the alphabetically-first
	// active sibling for the same project.
	accounts := map[string]beadsapi.AccountInfo{
		"alpha": {
			Name:   "alpha",
			Status: "active",
			Labels: []string{"project:gardenio"},
		},
		"matthewbaker1110": {
			Name:   "matthewbaker1110",
			Status: "active",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{"primary": "true"},
		},
	}
	got := buildProjectAccountOverride(accounts)
	if got["gardenio"] != "matthewbaker1110" {
		t.Errorf("primary marker: got %q, want matthewbaker1110", got["gardenio"])
	}

	// An exhausted-but-marked account does NOT win — status filter applies
	// before the marker check.
	accounts = map[string]beadsapi.AccountInfo{
		"alpha": {
			Name:   "alpha",
			Status: "active",
			Labels: []string{"project:gardenio"},
		},
		"marked-but-dead": {
			Name:   "marked-but-dead",
			Status: "exhausted",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{"primary": "true"},
		},
	}
	got = buildProjectAccountOverride(accounts)
	if got["gardenio"] != "alpha" {
		t.Errorf("exhausted-marked skipped: got %q, want alpha", got["gardenio"])
	}

	// Multiple marked accounts: alphabetical-first wins.
	accounts = map[string]beadsapi.AccountInfo{
		"zebra": {
			Name:   "zebra",
			Status: "active",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{"primary": "true"},
		},
		"apple": {
			Name:   "apple",
			Status: "active",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{"primary": "true"},
		},
	}
	got = buildProjectAccountOverride(accounts)
	if got["gardenio"] != "apple" {
		t.Errorf("multi-marked tiebreak: got %q, want apple", got["gardenio"])
	}
}
