package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mkPlanEpic is a compact helper for the browse-by-person strip tests: a plan
// epic owned by a given operated_by id + optional resolved email/display.
func mkPlanEpic(id, operatedBy, email, display string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:                id,
		Title:             "Plan " + id,
		Kind:              "issue",
		Type:              "epic",
		Status:            "in_progress",
		Labels:            []string{"project:gardenio", "plan"},
		OperatedBy:        operatedBy,
		OperatedByEmail:   email,
		OperatedByDisplay: display,
		UpdatedAt:         time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
}

// TestBuildPlanPeopleStrip_GroupsAndCounts proves the strip emits one chip per
// distinct human owner, counts their plans, links each to ?operated_by=<id>, and
// crosses over to /people.
func TestBuildPlanPeopleStrip_GroupsAndCounts(t *testing.T) {
	universe := []*beadsapi.BeadDetail{
		mkPlanEpic("gd-a1", "oidc:alice@acme.example", "alice@acme.example", "Alice"),
		mkPlanEpic("gd-a2", "oidc:alice@acme.example", "alice@acme.example", "Alice"),
		mkPlanEpic("gd-b1", "oidc:bob@acme.example", "bob@acme.example", "Bob"),
	}
	strip := buildPlanPeopleStrip(universe, "", "", "", "carol@acme.example")
	if !strip.Show {
		t.Fatal("strip should render — two browsable people")
	}
	if len(strip.People) != 2 {
		t.Fatalf("expected 2 person chips, got %d: %+v", len(strip.People), strip.People)
	}
	// Alice owns 2 plans → sorts first (most prolific).
	if strip.People[0].Display != "Alice" || strip.People[0].Count != 2 {
		t.Errorf("expected Alice with count 2 first, got %+v", strip.People[0])
	}
	if strip.People[1].Display != "Bob" || strip.People[1].Count != 1 {
		t.Errorf("expected Bob with count 1 second, got %+v", strip.People[1])
	}
	if !strings.Contains(strip.People[0].Href, "operated_by=oidc%3Aalice%40acme.example") {
		t.Errorf("Alice chip should link to her ?operated_by= scope, got %q", strip.People[0].Href)
	}
	if strip.PeopleHref != "/people" {
		t.Errorf("expected /people cross-link, got %q", strip.PeopleHref)
	}
}

// TestBuildPlanPeopleStrip_SkipsNonHuman proves machine sentinels, agent-created
// (empty operated_by), and other non-human owners never become person chips.
func TestBuildPlanPeopleStrip_SkipsNonHuman(t *testing.T) {
	universe := []*beadsapi.BeadDetail{
		mkPlanEpic("gd-h1", "oidc:alice@acme.example", "alice@acme.example", "Alice"),
		mkPlanEpic("gd-m1", "fleet:slack-bridge", "", ""),  // machine sentinel
		mkPlanEpic("gd-s1", "system:machine", "", ""),      // system sentinel
		mkPlanEpic("gd-g1", "", "", ""),                    // agent-created, no operator
		mkPlanEpic("gd-j1", "jira:557058:abc", "", "JIRA"), // non-human provider id
	}
	strip := buildPlanPeopleStrip(universe, "", "", "", "carol@acme.example")
	if len(strip.People) != 1 {
		t.Fatalf("expected only the human (Alice) to become a chip, got %d: %+v", len(strip.People), strip.People)
	}
	if strip.People[0].Display != "Alice" {
		t.Errorf("expected Alice, got %+v", strip.People[0])
	}
}

// TestBuildPlanPeopleStrip_SelfFirstAndTagged proves the viewer's own chip is
// tagged self, sorted first, and does NOT by itself make the strip render.
func TestBuildPlanPeopleStrip_SelfFirstAndTagged(t *testing.T) {
	universe := []*beadsapi.BeadDetail{
		// Bob owns 3 (most prolific), Matt (viewer) owns 1 — self still sorts first.
		mkPlanEpic("gd-b1", "oidc:bob@acme.example", "bob@acme.example", "Bob"),
		mkPlanEpic("gd-b2", "oidc:bob@acme.example", "bob@acme.example", "Bob"),
		mkPlanEpic("gd-b3", "oidc:bob@acme.example", "bob@acme.example", "Bob"),
		mkPlanEpic("gd-m1", "oidc:matt@acme.example", "matt@acme.example", "Matt"),
	}
	strip := buildPlanPeopleStrip(universe, "", "", "oidc:matt@acme.example", "matt@acme.example")
	if len(strip.People) != 2 {
		t.Fatalf("expected 2 chips, got %d", len(strip.People))
	}
	if !strip.People[0].IsSelf {
		t.Errorf("viewer's own chip should sort first, got %+v", strip.People[0])
	}
	if !strip.Show {
		t.Error("strip should render — Bob is a browsable non-self person")
	}
}

// TestBuildPlanPeopleStrip_SuppressedWhenOnlySelf proves a strip holding only
// the viewer's own chip is suppressed (no teammate to browse to).
func TestBuildPlanPeopleStrip_SuppressedWhenOnlySelf(t *testing.T) {
	universe := []*beadsapi.BeadDetail{
		mkPlanEpic("gd-m1", "oidc:matt@acme.example", "matt@acme.example", "Matt"),
		mkPlanEpic("gd-m2", "oidc:matt@acme.example", "matt@acme.example", "Matt"),
	}
	strip := buildPlanPeopleStrip(universe, "", "", "oidc:matt@acme.example", "matt@acme.example")
	if strip.Show {
		t.Errorf("strip with only the viewer's own chip must be suppressed, got %+v", strip.People)
	}
}

// TestBuildPlanPeopleStrip_ActiveMarksCurrentScope proves the chip matching the
// current operated_by scope is marked active (already-pivoted-to reads selected).
func TestBuildPlanPeopleStrip_ActiveMarksCurrentScope(t *testing.T) {
	universe := []*beadsapi.BeadDetail{
		mkPlanEpic("gd-a1", "oidc:alice@acme.example", "alice@acme.example", "Alice"),
		mkPlanEpic("gd-b1", "oidc:bob@acme.example", "bob@acme.example", "Bob"),
	}
	strip := buildPlanPeopleStrip(universe, "", "", "oidc:alice@acme.example", "matt@acme.example")
	var alice *planPersonChip
	for i := range strip.People {
		if strip.People[i].Display == "Alice" {
			alice = &strip.People[i]
		}
	}
	if alice == nil || !alice.IsActive {
		t.Errorf("Alice chip should be active when the scope matches her id, got %+v", alice)
	}
}

// TestBuildPlanPeopleStrip_FoldsAliasesByEmail proves a person owning plans under
// two aliases (slack + oidc) folds into ONE chip, keyed by email, with the oidc
// alias preferred as the pivot link and the count summed.
func TestBuildPlanPeopleStrip_FoldsAliasesByEmail(t *testing.T) {
	universe := []*beadsapi.BeadDetail{
		mkPlanEpic("gd-a1", "slack:U0ALICE", "alice@acme.example", "Alice"),
		mkPlanEpic("gd-a2", "oidc:alice@acme.example", "alice@acme.example", "Alice"),
	}
	strip := buildPlanPeopleStrip(universe, "", "", "", "carol@acme.example")
	if len(strip.People) != 1 {
		t.Fatalf("expected the two aliases to fold into one chip, got %d: %+v", len(strip.People), strip.People)
	}
	if strip.People[0].Count != 2 {
		t.Errorf("folded chip should count both plans, got %d", strip.People[0].Count)
	}
	if !strings.Contains(strip.People[0].Href, "operated_by=oidc%3Aalice") {
		t.Errorf("folded chip should prefer the oidc alias for the pivot link, got %q", strip.People[0].Href)
	}
}

// TestBuildPlanPeopleStrip_CapTruncates proves the strip caps the chip count and
// flags truncation past the cap.
func TestBuildPlanPeopleStrip_CapTruncates(t *testing.T) {
	var universe []*beadsapi.BeadDetail
	for i := range planPeopleStripCap + 5 {
		id := "gd-p" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		email := "person" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@acme.example"
		universe = append(universe, mkPlanEpic(id, "oidc:"+email, email, "Person "+id))
	}
	strip := buildPlanPeopleStrip(universe, "", "", "", "viewer@acme.example")
	if len(strip.People) != planPeopleStripCap {
		t.Fatalf("expected chips capped at %d, got %d", planPeopleStripCap, len(strip.People))
	}
	if !strip.Truncated {
		t.Error("strip should flag truncation past the cap")
	}
}

// --- integration: through handlePlans ---------------------------------------

// seedPlansPeopleFake builds a fake with two plan epics owned by two different
// humans (Alice + Bob) so the browse-by-person strip has a peer to render. The
// Types=[epic] route returns BOTH for every epic query (scoped default AND the
// unscoped universe query), so the strip sees the full people set.
func seedPlansPeopleFake() *fakeDaemon {
	f := newFake()
	alicePlan := mkPlanEpic("gd-alice1", "oidc:alice@acme.example", "alice@acme.example", "Alice")
	bobPlan := mkPlanEpic("gd-bob1", "oidc:bob@acme.example", "bob@acme.example", "Bob")
	f.beads["gd-alice1"] = alicePlan
	f.beads["gd-bob1"] = bobPlan
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{alicePlan, bobPlan}, Total: 2,
	}
	return f
}

// TestPlansIndex_PeopleStripRendersPeer proves the browse-by-person strip renders
// on the index with a chip linking to a peer's ?operated_by= scope and a
// cross-link to /people.
func TestPlansIndex_PeopleStripRendersPeer(t *testing.T) {
	f := seedPlansPeopleFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	// Anonymous show-all so both Alice's and Bob's plans are in scope.
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Browse plans by person") {
		t.Errorf("index should render the browse-by-person strip")
	}
	if !strings.Contains(body, "operated_by=oidc%3Aalice%40acme.example") {
		t.Errorf("strip should link to Alice's plans scope")
	}
	if !strings.Contains(body, "operated_by=oidc%3Abob%40acme.example") {
		t.Errorf("strip should link to Bob's plans scope")
	}
	if !strings.Contains(body, `href="/people"`) {
		t.Errorf("strip should cross-link to the /people directory")
	}
}

// TestPlansIndex_PeopleStripScopeIndependent proves the strip is derived from the
// UNSCOPED universe: a viewer scoped to their OWN plans still sees a teammate's
// browse chip (the whole point — pivot to a peer without widening first). The
// operator-scoped path issues the extra unscoped universe query.
func TestPlansIndex_PeopleStripScopeIndependent(t *testing.T) {
	f := seedPlansPeopleFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	// Authenticated as Alice with NO operated_by key → cold-load scoped to her.
	mux.ServeHTTP(w, plansIndexReq("/plans", "alice@acme.example"))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Browse plans by person") {
		t.Errorf("strip should render even while scoped to the viewer's own plans")
	}
	// Bob is the browsable peer — his pivot link must be present.
	if !strings.Contains(body, "operated_by=oidc%3Abob%40acme.example") {
		t.Errorf("scope-independent strip should still offer Bob's browse chip")
	}
	// Alice's own chip carries the (you) tag.
	if !strings.Contains(body, "(you)") {
		t.Errorf("the viewer's own chip should carry a (you) tag")
	}
}

// TestPlansIndex_PeopleStripPreservesFilters proves the strip's pivot links keep
// the sibling status/project filters (via the header chip's ExtraQuery), so
// browsing to a peer doesn't silently reset the other filters.
func TestPlansIndex_PeopleStripPreservesFilters(t *testing.T) {
	f := seedPlansPeopleFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&project=gardenio", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "project=gardenio&amp;operated_by=oidc%3Aalice") &&
		!strings.Contains(body, "project=gardenio&operated_by=oidc%3Aalice") {
		t.Errorf("person-chip pivot link should preserve ?project=gardenio")
	}
}
