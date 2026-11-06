package main

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// grantsGolden is kbeads' mirror of its machine capability table. The bridge
// authenticates as machine:gardenio-github-bridge, and kbeads admits its
// stamps on work beads it did not file only while the patch holds nothing but
// the fields gardenio-machine-github-bridge lists (FieldOwnership, all or
// nothing).
const grantsGolden = "../../../../kbeads/internal/server/testdata/machine_grants.golden.json"

// workBeadFields is every field key the bridge stamps on a PR's work beads:
// linkTask, the merge harvest, the review flag, the resolver stamps and the
// conflict scanner's patch.
var workBeadFields = []string{
	"mr_url", "forge",
	"ci_state", "tests_touched", "goldens_touched",
	reviewFlagField,
	fieldSpawnedAt, fieldSpawnCount, fieldThreadsHash, fieldGaveUp,
	fieldMergeState, fieldConflictFiles, fieldConflictKind, fieldConflictSince, fieldConflictBase, fieldConflictCause,
	fieldOverlaps,
}

// TestGitHubBridgeWorkFields_MatchGrant pins the bridge's work-bead stamps and
// the kbeads grant to each other, on every work type, exactly: a stamped key
// the grant lacks turns the whole patch into an ownership 403 (gd-tCMXlS9fXW),
// and a granted key the bridge never writes is a privilege it does not need.
func TestGitHubBridgeWorkFields_MatchGrant(t *testing.T) {
	b, err := os.ReadFile(grantsGolden)
	if err != nil {
		t.Fatalf("read kbeads grants golden: %v", err)
	}
	var golden map[string]struct {
		Writes         []string `json:"writes"`
		Ownership      []string `json:"ownership"`
		FieldOwnership []string `json:"field_ownership"`
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatalf("decode kbeads grants golden: %v", err)
	}
	g, ok := golden[bridgeMachineRole]
	if !ok {
		t.Fatalf("%s is not in the kbeads grants golden", bridgeMachineRole)
	}
	var want []string
	for _, typ := range []string{"bug", "chore", "epic", "feature", "task"} {
		if !workBeadTypes[typ] {
			t.Errorf("%s is granted but not a bridge work bead type", typ)
		}
		for _, f := range workBeadFields {
			want = append(want, typ+"."+f)
		}
	}
	got := slices.Clone(g.FieldOwnership)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s field_ownership = %v\nwant (the bridge's work-bead stamps on each work type) %v\nupdate machineGitHubBridgeWorkFields in kbeads/internal/server/machine_grants.go and regenerate the golden", bridgeMachineRole, got, want)
	}
	if !slices.Equal(g.Ownership, []string{"merge-request"}) {
		t.Errorf("%s ownership = %v, want [merge-request]", bridgeMachineRole, g.Ownership)
	}
	if len(g.Writes) != 0 {
		t.Errorf("%s writes = %v; the bridge writes no admin-gated type", bridgeMachineRole, g.Writes)
	}
}
