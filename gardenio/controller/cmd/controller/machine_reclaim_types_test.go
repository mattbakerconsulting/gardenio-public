package main

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// The stale reaper on the machine principal releases claims only on
// controllerMachineReclaimTypes. Each must be an issue type the daemon lets
// gardenio-machine-controller-ownership update without owning the bead
// (Ownership or ReclaimOwnership in kbeads machine_grants.go), or every reap
// pass 403s on it (gd-ozX2LfXsrv). And each issue type the grant covers should
// be reclaimed, or a hard-killed crew's claim on it strands (gd-RZJk5gspFv).
func TestControllerMachineReclaimTypes_MatchGrant(t *testing.T) {
	b, err := os.ReadFile("../../../../kbeads/internal/server/testdata/machine_grants.golden.json")
	if err != nil {
		t.Fatalf("read kbeads grants golden: %v", err)
	}
	var golden map[string]struct {
		Ownership        []string `json:"ownership"`
		ReclaimOwnership []string `json:"reclaim_ownership"`
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatalf("decode kbeads grants golden: %v", err)
	}
	g, ok := golden["gardenio-machine-controller-ownership"]
	if !ok {
		t.Fatal("golden has no gardenio-machine-controller-ownership")
	}
	issueTypes := []string{"task", "bug", "feature", "chore", "epic"}
	var granted []string
	for _, typ := range issueTypes {
		if slices.Contains(g.Ownership, typ) || slices.Contains(g.ReclaimOwnership, typ) {
			granted = append(granted, typ)
		}
	}
	for _, typ := range controllerMachineReclaimTypes {
		if !slices.Contains(granted, typ) {
			t.Errorf("controllerMachineReclaimTypes has %q but gardenio-machine-controller-ownership does not grant it", typ)
		}
	}
	for _, typ := range granted {
		if !slices.Contains(controllerMachineReclaimTypes, typ) {
			t.Errorf("gardenio-machine-controller-ownership grants %q but controllerMachineReclaimTypes leaves it out", typ)
		}
	}
}
