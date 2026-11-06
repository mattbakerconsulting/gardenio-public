package advice

import (
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestIsProjectBroadcast(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"bare project", []string{"project:gardenio"}, true},
		{"bare project, grouped", []string{"g0:project:gardenio"}, true},
		{"multi-project bare", []string{"project:gardenio", "project:monorepo"}, true},
		{"project + role", []string{"g0:project:gardenio", "g0:role:crew"}, false},
		{"project + agent", []string{"project:gardenio", "agent:gardenio-x"}, false},
		{"project + global", []string{"project:gardenio", "global"}, false},
		{"global only", []string{"global"}, false},
		{"role only", []string{"role:crew"}, false},
		{"no labels", nil, false},
		// The junk project:gd-… artifact alone is not a real broadcast.
		{"junk project only", []string{"project:gd-abc123"}, false},
		{"junk + real project", []string{"project:gd-abc123", "project:gardenio"}, true},
		// Non-targeting labels don't change the verdict.
		{"bare project + misc label", []string{"project:gardenio", "needs-intake"}, true},
	}
	for _, c := range cases {
		if got := IsProjectBroadcast(c.labels); got != c.want {
			t.Errorf("%s: IsProjectBroadcast(%v) = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

func TestHasBroadcastOptIn(t *testing.T) {
	if !HasBroadcastOptIn([]string{"project:gardenio", BroadcastOptInLabel}) {
		t.Error("plain opt-in label not detected")
	}
	if !HasBroadcastOptIn([]string{"g0:" + BroadcastOptInLabel}) {
		t.Error("grouped opt-in label not detected")
	}
	if HasBroadcastOptIn([]string{"project:gardenio"}) {
		t.Error("opt-in reported without the label")
	}
}

func TestIsDeniedBroadcast(t *testing.T) {
	denied := &beadsapi.BeadDetail{Labels: []string{"project:gardenio"}}
	if !IsDeniedBroadcast(denied) {
		t.Error("bare-project bead without opt-in must be denied")
	}
	optedIn := &beadsapi.BeadDetail{Labels: []string{"project:gardenio", BroadcastOptInLabel}}
	if IsDeniedBroadcast(optedIn) {
		t.Error("opted-in broadcast must not be denied")
	}
	roleScoped := &beadsapi.BeadDetail{Labels: []string{"g0:project:gardenio", "g0:role:crew"}}
	if IsDeniedBroadcast(roleScoped) {
		t.Error("role-scoped bead must not be denied")
	}
}

func TestBroadcastProjects(t *testing.T) {
	got := BroadcastProjects([]string{"project:gardenio", "g0:project:monorepo", "project:gd-junk", "project:gardenio", "needs-intake"})
	want := []string{"gardenio", "monorepo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BroadcastProjects = %v, want %v", got, want)
	}
}

// TestAuditCatalog_DropsDeniedBroadcast pins the Phase-3 semantics change:
// a bare-project bead without the opt-in no longer counts toward any
// archetype's matched catalog, while an opted-in broadcast still does.
func TestAuditCatalog_DropsDeniedBroadcast(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		auditBead("gd-denied", 1000, []string{"project:gardenio"}, nil),
		auditBead("gd-optin", 2000, []string{"project:gardenio", BroadcastOptInLabel}, nil),
		auditBead("gd-role", 400, []string{"g0:project:gardenio", "g0:role:crew"}, nil),
	}
	results := AuditCatalog(beads, []Archetype{{Project: "gardenio", Role: "crew"}})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Matched != 2 || r.Bytes != 2000+400 {
		t.Errorf("matched=%d bytes=%d, want 2 beads / 2400 bytes (denied broadcast excluded)", r.Matched, r.Bytes)
	}
	for _, it := range r.Items {
		if it.ID == "gd-denied" {
			t.Error("denied broadcast bead leaked into the audit")
		}
	}
}
