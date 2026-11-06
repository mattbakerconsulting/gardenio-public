package advice

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func auditBead(id string, descLen int, labels []string, fields map[string]string) *beadsapi.BeadDetail {
	desc := make([]byte, descLen)
	for i := range desc {
		desc[i] = 'x'
	}
	return &beadsapi.BeadDetail{
		ID:          id,
		Title:       "advice " + id,
		Description: string(desc),
		Type:        "advice",
		Status:      "open",
		Labels:      labels,
		Fields:      fields,
	}
}

func TestArchetypeSubscriptions(t *testing.T) {
	cases := []struct {
		project, role string
		want          []string
	}{
		// Singular role gets its plural variant appended (a real agent
		// subscribes to both via agent-ID path + Singularize). Bare gardener
		// already carries the anchor, so the family rule adds nothing.
		{"gardenio", "gardener", []string{"global", "project:gardenio", "role:gardener", "role:gardeners"}},
		// Plural role gets its singular variant via Singularize.
		{"gardenio", "crews", []string{"global", "project:gardenio", "role:crews", "role:crew"}},
		{"", "", []string{"global"}},
		{"monorepo", "", []string{"global", "project:monorepo"}},
		// Differentiated gardener (hyphen-prefix token): the role-family rule
		// appends the role:gardener/role:gardeners anchor so it inherits the
		// gardener doctrine pool (ka-0000000880).
		{"gardenio", "gardener-delivery", []string{"global", "project:gardenio", "role:gardener-delivery", "role:gardener-deliverys", "role:gardener", "role:gardeners"}},
		// Suffix-token gardener (plan-gardener / cost-gardener / infra-gardener): a
		// gardener-* PREFIX glob would MISS these — the token match does not.
		{"gardenio", "plan-gardener", []string{"global", "project:gardenio", "role:plan-gardener", "role:plan-gardeners", "role:gardener", "role:gardeners"}},
		// Non-gardener differentiated role: NOT in the family, no anchor added —
		// crew-fable must not inherit the (large) role:gardener pool.
		{"gardenio", "crew-fable", []string{"global", "project:gardenio", "role:crew-fable", "role:crew-fables"}},
	}
	for _, c := range cases {
		got := ArchetypeSubscriptions(c.project, c.role)
		if len(got) != len(c.want) {
			t.Errorf("ArchetypeSubscriptions(%q,%q) = %v, want %v", c.project, c.role, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ArchetypeSubscriptions(%q,%q)[%d] = %q, want %q", c.project, c.role, i, got[i], c.want[i])
			}
		}
	}
}

// TestAuditCatalog_MatchesDeliverySemantics is the core contract: the audit
// must count exactly what the real delivery pipeline delivers — bare-project
// beads broadcast to every role, role-gated beads only reach that role, and
// fixture/hippo/closed/expired beads never count.
func TestAuditCatalog_MatchesDeliverySemantics(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		auditBead("gd-glob", 400, []string{"global"}, nil),
		// Opted-in project broadcast — matches every gardenio archetype
		// regardless of role. Since Phase 3 the scope:project-broadcast
		// opt-in is required; without it the bead is denied entirely
		// (TestAuditCatalog_DropsDeniedBroadcast).
		auditBead("gd-bare", 1000, []string{"project:gardenio", BroadcastOptInLabel}, nil),
		// AND-grouped project+role: gardener only.
		auditBead("gd-capt", 2000, []string{"g0:project:gardenio", "g0:role:gardener"}, nil),
		// Different project: never matches gardenio archetypes.
		auditBead("gd-mono", 3000, []string{"project:monorepo", BroadcastOptInLabel}, nil),
		// Delivery-path drops, none of which may count:
		auditBead("gd-fixt", 5000, []string{"project:gardenio", "fixture:e2e"}, nil),
		auditBead("gd-hippo", 5000, []string{"project:gardenio", "source:hippo-memory"}, nil),
	}
	closed := auditBead("gd-closed", 5000, []string{"project:gardenio", BroadcastOptInLabel}, nil)
	closed.Status = "closed"
	beads = append(beads, closed)

	archetypes := []Archetype{
		{Project: "gardenio", Role: "gardener"},
		{Project: "gardenio", Role: "crew"},
		{Project: "monorepo", Role: "crew"},
	}
	results := AuditCatalog(beads, archetypes)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	byKey := map[string]ArchetypeAudit{}
	for _, r := range results {
		byKey[r.Project+"/"+r.Role] = r
	}

	capt := byKey["gardenio/gardener"]
	if capt.Matched != 3 || capt.Bytes != 400+1000+2000 {
		t.Errorf("gardenio/gardener matched=%d bytes=%d, want 3 beads / 3400 bytes", capt.Matched, capt.Bytes)
	}
	if capt.Tokens != capt.Bytes/TokenBytesRatio {
		t.Errorf("gardenio/gardener tokens=%d, want bytes/%d=%d", capt.Tokens, TokenBytesRatio, capt.Bytes/TokenBytesRatio)
	}
	// The bare-project bead broadcasts to crew too — the leak the audit exists
	// to make visible.
	crew := byKey["gardenio/crew"]
	if crew.Matched != 2 || crew.Bytes != 400+1000 {
		t.Errorf("gardenio/crew matched=%d bytes=%d, want 2 beads / 1400 bytes (global + bare-project broadcast)", crew.Matched, crew.Bytes)
	}
	mono := byKey["monorepo/crew"]
	if mono.Matched != 2 || mono.Bytes != 400+3000 {
		t.Errorf("monorepo/crew matched=%d bytes=%d, want 2 beads / 3400 bytes", mono.Matched, mono.Bytes)
	}

	// Scope attribution: gardener's role-scoped bead lands under "role".
	if st := capt.ByScope["role"]; st.Beads != 1 || st.Bytes != 2000 {
		t.Errorf("gardenio/gardener by_scope[role] = %+v, want 1 bead / 2000 bytes", st)
	}
	if st := capt.ByScope["project"]; st.Beads != 1 || st.Bytes != 1000 {
		t.Errorf("gardenio/gardener by_scope[project] = %+v, want 1 bead / 1000 bytes", st)
	}
	if st := capt.ByScope["global"]; st.Beads != 1 || st.Bytes != 400 {
		t.Errorf("gardenio/gardener by_scope[global] = %+v, want 1 bead / 400 bytes", st)
	}

	// Items sorted by bytes descending.
	if len(capt.Items) != 3 || capt.Items[0].ID != "gd-capt" || capt.Items[2].ID != "gd-glob" {
		t.Errorf("gardenio/gardener items not sorted by bytes desc: %+v", capt.Items)
	}
}

// TestAuditCatalog_AppliesSupersession asserts a matched superseder knocks its
// predecessor out of the audit, mirroring gb prime's ApplySupersessions step.
func TestAuditCatalog_AppliesSupersession(t *testing.T) {
	old := auditBead("gd-old", 1000, []string{"project:gardenio", BroadcastOptInLabel}, nil)
	newer := auditBead("gd-new", 500, []string{"project:gardenio", BroadcastOptInLabel}, nil)
	newer.Description = "This supersedes the earlier advice (gd-old)."

	results := AuditCatalog(
		[]*beadsapi.BeadDetail{old, newer},
		[]Archetype{{Project: "gardenio", Role: "crew"}},
	)
	r := results[0]
	if r.Matched != 1 || r.Items[0].ID != "gd-new" {
		t.Errorf("superseded bead still counted: %+v", r.Items)
	}
}

// TestAggregateByBead is the core contract for the fleet-amplified per-bead
// view (ka-0000003599): a global bead reaching every archetype must outrank a
// role bead of EQUAL per-copy size that reaches one, because its fleet cost is
// size × reach. The per-archetype audit alone cannot express this — a global
// bead appears once per archetype at the same Bytes, so its aggregate weight is
// invisible until reach is folded in.
func TestAggregateByBead(t *testing.T) {
	// gd-glob (global, 400 B) matches BOTH archetypes -> reach 2.
	// gd-role (role:crew, 400 B) matches only the crew archetype -> reach 1.
	// gd-capt (project+role gardener, 2000 B) matches only gardener -> reach 1.
	beads := []*beadsapi.BeadDetail{
		auditBead("gd-glob", 400, []string{"global"}, nil),
		auditBead("gd-role", 400, []string{"role:crew"}, nil),
		auditBead("gd-capt", 2000, []string{"g0:project:gardenio", "g0:role:gardener"}, nil),
	}
	archetypes := []Archetype{
		{Project: "gardenio", Role: "crew"},
		{Project: "gardenio", Role: "gardener"},
	}
	results := AuditCatalog(beads, archetypes)
	costs := AggregateByBead(results)

	byID := map[string]BeadCost{}
	for _, c := range costs {
		byID[c.ID] = c
	}

	// Reach: glob hits both, role/capt hit one each.
	if got := byID["gd-glob"].Reach; got != 2 {
		t.Errorf("gd-glob reach = %d, want 2", got)
	}
	if got := byID["gd-role"].Reach; got != 1 {
		t.Errorf("gd-role reach = %d, want 1", got)
	}

	// FleetBytes = Bytes × Reach. glob: 400×2=800; role: 400×1=400; capt: 2000×1=2000.
	if got := byID["gd-glob"].FleetBytes; got != 800 {
		t.Errorf("gd-glob fleet bytes = %d, want 800", got)
	}
	if got := byID["gd-glob"].FleetTokens; got != 800/TokenBytesRatio {
		t.Errorf("gd-glob fleet tokens = %d, want %d", got, 800/TokenBytesRatio)
	}

	// The amplification story: a 400 B global bead (reach 2) must outrank a
	// 400 B role bead (reach 1) purely on reach — the whole point of the view.
	if byID["gd-glob"].FleetBytes <= byID["gd-role"].FleetBytes {
		t.Errorf("global bead (fleet %d) should outrank equal-size role bead (fleet %d)",
			byID["gd-glob"].FleetBytes, byID["gd-role"].FleetBytes)
	}

	// Scope is read through per-bead: global stays global, the gardener bead is role-scoped.
	if byID["gd-glob"].Scope != "global" {
		t.Errorf("gd-glob scope = %q, want global", byID["gd-glob"].Scope)
	}
	if byID["gd-capt"].Scope != "role" {
		t.Errorf("gd-capt scope = %q, want role", byID["gd-capt"].Scope)
	}

	// Sort order: FleetBytes descending. capt(2000) > glob(800) > role(400).
	wantOrder := []string{"gd-capt", "gd-glob", "gd-role"}
	if len(costs) != len(wantOrder) {
		t.Fatalf("got %d bead costs, want %d", len(costs), len(wantOrder))
	}
	for i, want := range wantOrder {
		if costs[i].ID != want {
			t.Errorf("costs[%d].ID = %q, want %q (order = %+v)", i, costs[i].ID, want, costs)
		}
	}
}

// TestAggregateByBead_TieBreakByID pins the deterministic ID tie-break so equal
// fleet-cost rows render in a stable order (CI/table diffs must not flap).
func TestAggregateByBead_TieBreakByID(t *testing.T) {
	// Two globals of equal size -> equal FleetBytes; ID ascending breaks the tie.
	beads := []*beadsapi.BeadDetail{
		auditBead("gd-bbb", 500, []string{"global"}, nil),
		auditBead("gd-aaa", 500, []string{"global"}, nil),
	}
	costs := AggregateByBead(AuditCatalog(beads, []Archetype{{Project: "gardenio", Role: "crew"}}))
	if len(costs) != 2 {
		t.Fatalf("got %d costs, want 2", len(costs))
	}
	if costs[0].ID != "gd-aaa" || costs[1].ID != "gd-bbb" {
		t.Errorf("tie-break order = [%s, %s], want [gd-aaa, gd-bbb]", costs[0].ID, costs[1].ID)
	}
}

// TestAggregateByBead_Empty guards the trivial-corpus case so the CLI never
// panics on an empty ranking.
func TestAggregateByBead_Empty(t *testing.T) {
	if got := AggregateByBead(nil); len(got) != 0 {
		t.Errorf("AggregateByBead(nil) = %v, want empty", got)
	}
	empty := AuditCatalog(nil, []Archetype{{Project: "gardenio", Role: "crew"}})
	if got := AggregateByBead(empty); len(got) != 0 {
		t.Errorf("AggregateByBead(no-match) = %v, want empty", got)
	}
}

// agentBead builds an agent-type bead fixture with project/role custom fields
// (the source ArchetypesFromAgentBeads prefers), for the archetype-derivation
// tests below. labels are optional extra labels (e.g. to test the label
// fallback when a field is absent).
func agentBead(id, project, role string, labels ...string) *beadsapi.BeadDetail {
	fields := map[string]string{}
	if project != "" {
		fields["project"] = project
	}
	if role != "" {
		fields["role"] = role
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  "agent " + id,
		Type:   "agent",
		Status: "closed",
		Labels: labels,
		Fields: fields,
	}
}

// TestArchetypesFromAgentBeads is the regression test for ka-0000002153: the
// derived archetype set must equal the DISTINCT (project, role) pairs the agent
// beads actually carry — one pair per bead — never the cross-product of every
// project with every role. Roles are singularized; the gd- junk-project guard
// and empty-project agents are dropped; the role/project labels are read as a
// fallback when the field is absent.
func TestArchetypesFromAgentBeads(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		// Two projects, disjoint roles. A cross-product would invent
		// gardenio/plan-gardener and monorepo/gardener-bugfix; the fix must not.
		agentBead("gd-a1", "gardenio", "gardener-bugfix"),
		agentBead("gd-a2", "gardenio", "gardener-bugfix"), // duplicate pair collapses
		agentBead("gd-a3", "gardenio", "crew"),
		agentBead("gd-a4", "monorepo", "plan-gardener"),
		// Plural role folds to its singular form (crews -> crew already present).
		agentBead("gd-a5", "monorepo", "crews"),
		// Field absent: project/role read from the labels instead.
		agentBead("gd-a6", "", "", "g0:project:devsecops", "g0:role:thread"),
		// Roleless agent defaults to crew (mirrors ListAgentBeads).
		agentBead("gd-a7", "glm-lab", ""),
		// Junk project shaped like a bead ID is skipped.
		agentBead("gd-a8", "ka-0000005132", "gardener"),
		// Un-projected agent (bare role:gardener) names no grid cell.
		agentBead("gd-a9", "", "gardener"),
		agentBead("gd-nil", "", ""), // no-op
		nil,                         // must not panic
	}
	got := ArchetypesFromAgentBeads(beads)
	want := []Archetype{
		{Project: "devsecops", Role: "thread"},
		{Project: "gardenio", Role: "crew"},
		{Project: "gardenio", Role: "gardener-bugfix"},
		{Project: "glm-lab", Role: "crew"},
		{Project: "monorepo", Role: "crew"},
		{Project: "monorepo", Role: "plan-gardener"},
	}
	if len(got) != len(want) {
		t.Fatalf("ArchetypesFromAgentBeads = %v (%d), want %v (%d)", got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("ArchetypesFromAgentBeads[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestArchetypesFromAgentBeads_NoCrossProduct pins the exact defect the bug
// reported: given N projects and M roles spread across agent beads, the old
// corpus-cross-product produced N×M archetypes (most of them phantom). The fix
// produces exactly the pairs that co-occur on a real agent — here 3, not 3×3=9.
func TestArchetypesFromAgentBeads_NoCrossProduct(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		agentBead("gd-p1", "projA", "roleX"),
		agentBead("gd-p2", "projB", "roleY"),
		agentBead("gd-p3", "projC", "roleZ"),
	}
	got := ArchetypesFromAgentBeads(beads)
	if len(got) != 3 {
		t.Fatalf("got %d archetypes, want 3 (one per bead) — a cross-product would give 9: %v", len(got), got)
	}
	// None of the phantom cross terms (e.g. projA/roleY) may appear.
	real := map[Archetype]bool{
		{Project: "projA", Role: "roleX"}: true,
		{Project: "projB", Role: "roleY"}: true,
		{Project: "projC", Role: "roleZ"}: true,
	}
	for _, a := range got {
		if !real[a] {
			t.Errorf("phantom archetype %v — not a real (project,role) pair from any agent bead", a)
		}
	}
}

// TestIsHippoMirror_SharedPredicate pins the shared predicate the audit,
// gb prime, gb advice list, and the catalog metrics all rely on.
func TestIsHippoMirror_SharedPredicate(t *testing.T) {
	cases := []struct {
		bead *beadsapi.BeadDetail
		want bool
	}{
		{auditBead("gd-a", 10, []string{"source:hippo-memory"}, nil), true},
		{auditBead("gd-b", 10, []string{"source:hippo-global"}, nil), true},
		{auditBead("gd-c", 10, []string{"global"}, map[string]string{"source": "hippo-memory"}), true},
		{auditBead("gd-d", 10, []string{"global"}, map[string]string{"source": "hippo-global"}), true},
		{auditBead("gd-e", 10, []string{"global"}, nil), false},
		{auditBead("gd-f", 10, []string{"global"}, map[string]string{"source": "gitlab"}), false},
	}
	for _, c := range cases {
		if got := IsHippoMirror(c.bead); got != c.want {
			t.Errorf("IsHippoMirror(%s) = %v, want %v", c.bead.ID, got, c.want)
		}
	}
}
