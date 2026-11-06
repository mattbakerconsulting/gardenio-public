package main

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

// --- normalizePlanStatus / normalizePlanGroup ------------------------------

func TestNormalizePlanStatus(t *testing.T) {
	cases := map[string]string{
		"":            planStatusActive, // default
		"active":      planStatusActive,
		"garbage":     planStatusActive, // unrecognized → default
		"open":        planStatusOpen,
		"in_progress": planStatusInProgress,
		"in-progress": planStatusInProgress, // alias
		"progress":    planStatusInProgress, // alias
		"blocked":     planStatusBlocked,
		"deferred":    planStatusDeferred,
		"defer":       planStatusDeferred, // alias
		"closed":      planStatusClosed,
		"done":        planStatusClosed, // alias
		"stale":       planStatusStale,
		"idle":        planStatusStale, // alias
		"sweep":       planStatusStale, // alias
		"all":         planStatusAll,
		"any":         planStatusAll,    // alias
		"CLOSED":      planStatusClosed, // case-insensitive
		"  Open  ":    planStatusOpen,   // trimmed
	}
	for in, want := range cases {
		if got := normalizePlanStatus(in); got != want {
			t.Errorf("normalizePlanStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizePlanGroup(t *testing.T) {
	cases := map[string]string{
		"":        planGroupNone,
		"garbage": planGroupNone,
		"project": planGroupProject,
		"status":  planGroupStatus,
		"state":   planGroupStatus, // alias
		"user":    planGroupUser,
		"owner":   planGroupUser, // alias
		"USER":    planGroupUser, // case-insensitive
	}
	for in, want := range cases {
		if got := normalizePlanGroup(in); got != want {
			t.Errorf("normalizePlanGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

// planStatusEnum maps the ?status= value onto the daemon status set. "active"
// is the shared activeEnumStatuses; "all" is nil (unscoped); each single value
// is its own enum.
func TestPlanStatusEnum(t *testing.T) {
	if got := planStatusEnum(planStatusAll); got != nil {
		t.Errorf("all → nil status set, got %v", got)
	}
	if got := planStatusEnum(planStatusActive); !slices.Equal(got, activeEnumStatuses) {
		t.Errorf("active → activeEnumStatuses, got %v", got)
	}
	if got := planStatusEnum(planStatusClosed); !slices.Equal(got, []string{"closed"}) {
		t.Errorf("closed → [closed], got %v", got)
	}
	if got := planStatusEnum(planStatusInProgress); !slices.Equal(got, []string{"in_progress"}) {
		t.Errorf("in_progress → [in_progress], got %v", got)
	}
	if got := planStatusEnum(planStatusDeferred); !slices.Equal(got, []string{"deferred"}) {
		t.Errorf("deferred → [deferred], got %v", got)
	}
	// Stale is a DERIVED sweep: the daemon has no column for the folded
	// lastActivityAt, so the query scopes to the SAME active set the default view
	// uses and handlePlans post-filters on FreshnessStale. If this ever narrowed to
	// a raw status the sweep would silently miss stale plans in the other statuses.
	if got := planStatusEnum(planStatusStale); !slices.Equal(got, activeEnumStatuses) {
		t.Errorf("stale → activeEnumStatuses (post-filtered), got %v", got)
	}
}

// --- planFilterState chip URLs ----------------------------------------------

// mustQuery parses a chip URL and returns its query values, failing the test on
// a malformed URL.
func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

// TestPlanFilterState_PreservesOtherDimensions proves that switching one filter
// dimension keeps the others (status/project/group) on the emitted chip URL —
// the whole point of routing every chip through the shared state.
func TestPlanFilterState_PreservesOtherDimensions(t *testing.T) {
	st := planFilterState{
		status:  planStatusClosed,
		project: "gardenio",
		group:   planGroupProject,
		base:    "/plans",
	}
	// Switching group to "status" must keep status=closed AND project=gardenio.
	got := mustQuery(t, st.chipURL(map[string]string{"group": planGroupStatus}))
	if got.Get("status") != planStatusClosed {
		t.Errorf("switching group dropped status: %v", got)
	}
	if got.Get("project") != "gardenio" {
		t.Errorf("switching group dropped project: %v", got)
	}
	if got.Get("group") != planGroupStatus {
		t.Errorf("switching group did not apply: %v", got)
	}
}

// TestPlanFilterState_DefaultsOmitted proves the default dimensions
// (status=active, project="", group="") never appear in a chip URL, so the
// cold-load "Active" chip points at the bare /plans path.
func TestPlanFilterState_DefaultsOmitted(t *testing.T) {
	st := planFilterState{status: planStatusActive, base: "/plans"}
	if got := st.chipURL(map[string]string{"status": ""}); got != "/plans" {
		t.Errorf("default state should yield bare /plans, got %q", got)
	}
}

// TestPlanFilterState_PreservesUserScope proves the user scope (operated_by) is
// carried on every filter/group chip so switching status/project/group never
// silently resets which user's plans are shown — including the explicit
// show-all (empty value, key present) form.
func TestPlanFilterState_PreservesUserScope(t *testing.T) {
	// A concrete peer scope.
	st := planFilterState{
		status:        planStatusActive,
		operatedByRaw: "oidc:peer@acme.example",
		operatedByKey: true,
		base:          "/plans",
	}
	got := mustQuery(t, st.chipURL(map[string]string{"status": planStatusClosed}))
	if got.Get("operated_by") != "oidc:peer@acme.example" {
		t.Errorf("filter switch dropped the user scope: %v", got)
	}

	// The explicit show-all opt-out: key present, value empty. Must survive.
	all := planFilterState{status: planStatusActive, operatedByKey: true, base: "/plans"}
	raw := all.chipURL(map[string]string{"group": planGroupProject})
	if !strings.Contains(raw, "operated_by=") {
		t.Errorf("show-all scope (empty operated_by) must be preserved, got %q", raw)
	}
}

// TestPlanFilterState_ClearURL proves the "× clear" target drops
// status/project/group but keeps the user scope (which the header chip owns).
func TestPlanFilterState_ClearURL(t *testing.T) {
	st := planFilterState{
		status:        planStatusClosed,
		project:       "gardenio",
		group:         planGroupUser,
		operatedByRaw: "oidc:peer@acme.example",
		operatedByKey: true,
		base:          "/plans",
	}
	if !st.isFiltered() {
		t.Fatal("state with non-default dims should read as filtered")
	}
	got := mustQuery(t, st.clearURL())
	if got.Get("status") != "" || got.Get("project") != "" || got.Get("group") != "" {
		t.Errorf("clear should drop status/project/group, got %v", got)
	}
	if got.Get("operated_by") != "oidc:peer@acme.example" {
		t.Errorf("clear should preserve the user scope, got %v", got)
	}
}

// TestPlanFilterState_IsFiltered_DefaultFalse proves an untouched state is not
// "filtered" (no × clear affordance on cold load).
func TestPlanFilterState_IsFiltered_DefaultFalse(t *testing.T) {
	st := planFilterState{status: planStatusActive, base: "/plans"}
	if st.isFiltered() {
		t.Error("cold-load state must not read as filtered")
	}
}

// TestPlanControls_ActiveReflectsState proves exactly one chip per row is marked
// active and it matches the current state.
func TestPlanControls_ActiveReflectsState(t *testing.T) {
	st := planFilterState{status: planStatusClosed, group: planGroupStatus, base: "/plans"}
	c := st.controls([]string{"gardenio", "coop"})

	assertOneActive := func(name string, items []planControl, wantLabel string) {
		t.Helper()
		var active []string
		for _, it := range items {
			if it.Active {
				active = append(active, it.Label)
			}
		}
		if len(active) != 1 {
			t.Errorf("%s row: expected exactly one active chip, got %v", name, active)
			return
		}
		if active[0] != wantLabel {
			t.Errorf("%s row: active chip = %q, want %q", name, active[0], wantLabel)
		}
	}
	assertOneActive("status", c.Statuses, "Closed")
	assertOneActive("group", c.Groups, "Status")
	assertOneActive("project", c.Projects, "All") // project unset → All active

	// The project chips include one per supplied project plus the leading "All".
	var haveGardenio, haveCoop bool
	for _, it := range c.Projects {
		haveGardenio = haveGardenio || it.Label == "gardenio"
		haveCoop = haveCoop || it.Label == "coop"
	}
	if !haveGardenio || !haveCoop {
		t.Errorf("project chips should include every option, got %+v", c.Projects)
	}
}

// --- groupPlanRows ----------------------------------------------------------

// TestGroupPlanRows_ByProject proves rows are sectioned by project, missing
// projects fall into a "(no project)" bucket, and groups are largest-first.
func TestGroupPlanRows_ByProject(t *testing.T) {
	rows := []planRowView{
		{ID: "a", Project: "gardenio"},
		{ID: "b", Project: "coop"},
		{ID: "c", Project: "gardenio"},
		{ID: "d", Project: ""}, // no project
	}
	groups := groupPlanRows(rows, planGroupProject)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups (gardenio, coop, no-project), got %d: %+v", len(groups), groups)
	}
	// Largest first: gardenio (2) leads.
	if groups[0].Label != "gardenio" || len(groups[0].Rows) != 2 {
		t.Errorf("first group should be gardenio×2, got %q×%d", groups[0].Label, len(groups[0].Rows))
	}
	// The empty-project bucket is labeled and keyed distinctly.
	var noneSeen bool
	for _, g := range groups {
		if g.Key == "~none" {
			noneSeen = true
			if g.Label != "(no project)" {
				t.Errorf("empty-project group label = %q", g.Label)
			}
		}
	}
	if !noneSeen {
		t.Error("expected a (no project) bucket")
	}
}

// TestGroupPlanRows_ByStatus proves status grouping humanizes the heading and
// keys on the lowercased status.
func TestGroupPlanRows_ByStatus(t *testing.T) {
	rows := []planRowView{
		{ID: "a", Status: "in_progress"},
		{ID: "b", Status: "open"},
		{ID: "c", Status: "in_progress"},
	}
	groups := groupPlanRows(rows, planGroupStatus)
	if len(groups) != 2 {
		t.Fatalf("expected 2 status groups, got %d", len(groups))
	}
	if groups[0].Label != "In progress" || groups[0].Key != "in_progress" {
		t.Errorf("largest status group should be 'In progress'/in_progress, got %q/%q",
			groups[0].Label, groups[0].Key)
	}
}

// TestGroupPlanRows_ByUser proves user grouping prefers the display name, keys
// stably on the operator id, and buckets attribution-less plans as
// "(unassigned)".
func TestGroupPlanRows_ByUser(t *testing.T) {
	rows := []planRowView{
		{ID: "a", OperatedBy: "oidc:matt@acme.example", OperatedByDisplay: "Matt Baker"},
		{ID: "b", OperatedBy: "oidc:matt@acme.example", OperatedByDisplay: "Matt Baker"},
		{ID: "c"}, // no attribution
	}
	groups := groupPlanRows(rows, planGroupUser)
	if len(groups) != 2 {
		t.Fatalf("expected 2 user groups, got %d: %+v", len(groups), groups)
	}
	if groups[0].Label != "Matt Baker" || groups[0].Key != "oidc:matt@acme.example" {
		t.Errorf("largest user group should be 'Matt Baker', got %q/%q", groups[0].Label, groups[0].Key)
	}
	var unassigned bool
	for _, g := range groups {
		if g.Key == "~none" && g.Label == "(unassigned)" {
			unassigned = true
		}
	}
	if !unassigned {
		t.Error("expected an (unassigned) bucket for attribution-less plans")
	}
}

// TestGroupPlanRows_ByUser_RichestLabelWins proves that when two plans share an
// operator id but only one carries a display name, the group heading uses the
// display name regardless of row order (not first-writer-wins on the raw id).
func TestGroupPlanRows_ByUser_RichestLabelWins(t *testing.T) {
	// Attribution-less row FIRST, enriched row second — the order that would
	// have pinned the heading to the raw id under first-writer-wins.
	rows := []planRowView{
		{ID: "a", OperatedBy: "oidc:matt@acme.example"}, // no display
		{ID: "b", OperatedBy: "oidc:matt@acme.example", OperatedByDisplay: "Matt Baker"},
	}
	groups := groupPlanRows(rows, planGroupUser)
	if len(groups) != 1 {
		t.Fatalf("both rows share an operator → 1 group, got %d", len(groups))
	}
	if groups[0].Label != "Matt Baker" {
		t.Errorf("group heading should upgrade to the display name, got %q", groups[0].Label)
	}
	if groups[0].Key != "oidc:matt@acme.example" {
		t.Errorf("group key must stay the stable lowercased operator id, got %q", groups[0].Key)
	}
}

// TestGroupPlanRows_ByUser_DisplayBeatsEmailLabel mirrors RichestLabelWins for
// the email-fallback case (ka-0000006264): the first-sorted row has an empty
// display name but a present email, so its projected heading is the email; a
// later sibling row carries the real display name. Before the tiered predicate,
// planLabelRicherThan only upgraded from the bare-id fallback, so the email
// heading was pinned — cosmetically wrong. The display name must win.
func TestGroupPlanRows_ByUser_DisplayBeatsEmailLabel(t *testing.T) {
	// Email-labeled row FIRST (empty display, present email), display-name row
	// second — the order that pinned the heading to the email pre-fix.
	rows := []planRowView{
		{ID: "a", OperatedBy: "oidc:matt@acme.example", OperatedByEmail: "matt@acme.example"},
		{ID: "b", OperatedBy: "oidc:matt@acme.example", OperatedByDisplay: "Matt Baker"},
	}
	groups := groupPlanRows(rows, planGroupUser)
	if len(groups) != 1 {
		t.Fatalf("both rows share an operator → 1 group, got %d", len(groups))
	}
	if groups[0].Label != "Matt Baker" {
		t.Errorf("group heading should upgrade the email to the display name, got %q", groups[0].Label)
	}
	if groups[0].Key != "oidc:matt@acme.example" {
		t.Errorf("group key must stay the stable lowercased operator id, got %q", groups[0].Key)
	}
}

// TestPlanLabelRicherThan covers the heading-upgrade predicate directly.
func TestPlanLabelRicherThan(t *testing.T) {
	key := "oidc:matt@acme.example"
	email := "matt@acme.example"
	// A real display name beats the id fallback.
	if !planLabelRicherThan("Matt Baker", key, key) {
		t.Error("display name should be richer than the raw-id heading")
	}
	if !planLabelRicherThan("Matt Baker", "", key) {
		t.Error("display name should be richer than an empty heading")
	}
	// A real display name beats an email fallback (the ka-0000006264 fix).
	if !planLabelRicherThan("Matt Baker", email, key) {
		t.Error("display name should be richer than an email heading")
	}
	// An email fallback beats the id fallback but never a display name.
	if !planLabelRicherThan(email, key, key) {
		t.Error("email should be richer than the raw-id heading")
	}
	if !planLabelRicherThan(email, "", key) {
		t.Error("email should be richer than an empty heading")
	}
	if planLabelRicherThan(email, "Matt Baker", key) {
		t.Error("email must not overwrite a real display name")
	}
	// The id fallback never upgrades anything.
	if planLabelRicherThan(key, "Matt Baker", key) {
		t.Error("raw id must not overwrite a real display name")
	}
	if planLabelRicherThan(key, email, key) {
		t.Error("raw id must not overwrite an email heading")
	}
	if planLabelRicherThan("", "Matt Baker", key) {
		t.Error("empty candidate must not overwrite a real display name")
	}
	// Two distinct real names: first keeps it (candidate not richer).
	if planLabelRicherThan("Second Name", "First Name", key) {
		t.Error("a second real name must not fight an existing real heading")
	}
	// Two emails: same tier, so the first stable one keeps the heading.
	if planLabelRicherThan("other@acme.example", email, key) {
		t.Error("a second email must not fight an existing email heading")
	}
	// A key that is itself an email (operated_by absent, email promoted to key)
	// still ranks as the id fallback, not the email tier.
	if planLabelRicherThan(email, "", email) {
		t.Error("a label equal to an email KEY is the id fallback, not an upgrade")
	}
}

// TestGroupPlanRows_NoneReturnsNil proves ?group= absent/unrecognized yields no
// groups (the handler then renders a flat grid).
func TestGroupPlanRows_NoneReturnsNil(t *testing.T) {
	rows := []planRowView{{ID: "a", Project: "gardenio"}}
	if groups := groupPlanRows(rows, planGroupNone); groups != nil {
		t.Errorf("group=none should return nil, got %+v", groups)
	}
	if groups := groupPlanRows(rows, "bogus"); groups != nil {
		t.Errorf("unrecognized group should return nil, got %+v", groups)
	}
}
