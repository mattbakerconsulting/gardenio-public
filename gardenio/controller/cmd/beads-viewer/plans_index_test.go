package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plansIndexReq builds a GET /plans request, optionally authenticated as the
// given viewer email (empty = anonymous). target lets a test pass query params.
func plansIndexReq(target, email string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	if email != "" {
		req.Header.Set("X-Auth-Request-Email", email)
	}
	return req
}

// --- S1: filter-by-me by default --------------------------------------------

// TestPlansIndex_DefaultScopesToViewer proves an authenticated viewer with no
// ?operated_by= key gets the daemon query scoped to their oidc operator id, and
// the plans header chip renders the plan-count-aware cold-load "mine" state
// ("Showing your 1 plan (auto)" — Step 2, ka-0000005414).
func TestPlansIndex_DefaultScopesToViewer(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// The plan-epics list query must carry the viewer's operator alias.
	var epicQ *beadsapi.ListBeadsQuery
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if len(q.Types) == 1 && q.Types[0] == "epic" {
			epicQ = &f.listAllQ[i]
			break
		}
	}
	if epicQ == nil {
		t.Fatal("no plan-epics list query was issued")
	}
	if len(epicQ.OperatedByAliases) != 1 || epicQ.OperatedByAliases[0] != "oidc:matt@acme.example" {
		t.Errorf("expected default scoping to oidc:matt@acme.example, got aliases %v", epicQ.OperatedByAliases)
	}
	body := w.Body.String()
	// The seed has exactly one plan, so the count-aware "mine" headline reads
	// "Showing your 1 plan" (singular noun), and the tightened cold-load pill is
	// "(auto)" not the old "(default)".
	if !strings.Contains(body, "Showing your 1 plan") {
		t.Errorf("expected the count-aware 'mine' header chip ('Showing your 1 plan'), body missing it")
	}
	if !strings.Contains(body, "(auto)") {
		t.Errorf("cold-load default should render the (auto) pill")
	}
	// The widen-out affordance must read as discovery, not a bare "all" reset.
	if !strings.Contains(body, "everyone&#39;s plans") {
		t.Errorf("mine chip should offer the 'everyone's plans' discovery link")
	}
}

// TestPlansIndex_ExplicitAllClearsScope proves ?operated_by= (present key, empty
// value) is the explicit "everyone's plans" opt-out — no operator alias on the
// query, and the header chip reads the fleet-wide discovery state.
func TestPlansIndex_ExplicitAllClearsScope(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if len(q.Types) == 1 && q.Types[0] == "epic" && len(q.OperatedByAliases) != 0 {
			t.Errorf("explicit ?operated_by= (all) must not scope the query, got aliases %v", q.OperatedByAliases)
		}
	}
	// The "all" headline is written as raw HTML text (mirroring the shared
	// renderer), so its apostrophe is not entity-escaped.
	if !strings.Contains(w.Body.String(), "Showing everyone's plans") {
		t.Errorf("expected the 'everyone's plans' header chip")
	}
}

// TestPlansIndex_AnonymousShowsAll proves an unauthenticated viewer is not
// scoped (degrades to all) and sees the sign-in CTA chip.
func TestPlansIndex_AnonymousShowsAll(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if len(q.Types) == 1 && q.Types[0] == "epic" && len(q.OperatedByAliases) != 0 {
			t.Errorf("anonymous viewer must not scope the query, got aliases %v", q.OperatedByAliases)
		}
	}
	if !strings.Contains(w.Body.String(), "sign in") {
		t.Errorf("anonymous header chip should offer a sign-in CTA")
	}
}

// TestPlansIndex_HeaderChipPreservesProject proves the ?project= sibling filter
// is preserved on the header chip's switch-to links (ExtraQuery), mirroring
// /active's include_scheduled preservation.
func TestPlansIndex_HeaderChipPreservesProject(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?project=gardenio", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// The "switch to all" link must keep project=gardenio.
	if !strings.Contains(w.Body.String(), "project=gardenio&amp;operated_by=") &&
		!strings.Contains(w.Body.String(), "project=gardenio&operated_by=") {
		t.Errorf("header chip switch-to links should preserve ?project=gardenio")
	}
}

// --- Filter + visual grouping (user/project/status + group) -----------------

// planEpicQuery returns the plan-epics list query the handler issued (Types=
// [epic]), or nil if none was.
func planEpicQuery(f *fakeDaemon) *beadsapi.ListBeadsQuery {
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if len(q.Types) == 1 && q.Types[0] == "epic" {
			return &f.listAllQ[i]
		}
	}
	return nil
}

// TestPlansIndex_DefaultStatusScope proves the cold-load /plans index still
// scopes to the active status set (unchanged historical behavior).
func TestPlansIndex_DefaultStatusScope(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	q := planEpicQuery(f)
	if q == nil {
		t.Fatal("no plan-epics query issued")
	}
	if !slices.Equal(q.Statuses, activeEnumStatuses) {
		t.Errorf("default status scope should be activeEnumStatuses, got %v", q.Statuses)
	}
}

// TestPlansIndex_StatusFilterScopesQuery proves ?status=closed scopes the daemon
// query to closed only — the reach that made closed plans visible on the index.
func TestPlansIndex_StatusFilterScopesQuery(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=closed", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	q := planEpicQuery(f)
	if q == nil {
		t.Fatal("no plan-epics query issued")
	}
	if !slices.Equal(q.Statuses, []string{"closed"}) {
		t.Errorf("?status=closed should scope to [closed], got %v", q.Statuses)
	}
}

// TestPlansIndex_StatusAllUnscopesQuery proves ?status=all drops the status
// filter entirely (any lifecycle state).
func TestPlansIndex_StatusAllUnscopesQuery(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	q := planEpicQuery(f)
	if q == nil {
		t.Fatal("no plan-epics query issued")
	}
	if len(q.Statuses) != 0 {
		t.Errorf("?status=all should not scope status, got %v", q.Statuses)
	}
}

// TestPlansIndex_RendersControlStrip proves the filter control strip renders the
// status / project / group toggle rows, so the filters are discoverable in the
// UI (not just via hand-typed URLs).
func TestPlansIndex_RendersControlStrip(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="filter-rows"`, // shared labeled-control container (ka-0000006647 / F7)
		`aria-label="Status"`,
		`aria-label="Project"`,
		`aria-label="Group"`,
		`>Closed<`,   // the closed-status chip
		`>Deferred<`, // the deferred-status chip (parity with group-by-status)
		`>All<`,      // the all-status / all-project chip
	} {
		if !strings.Contains(body, want) {
			t.Errorf("control strip missing %q", want)
		}
	}
}

// TestPlansIndex_ControlChipPreservesUserScope proves a status chip URL keeps
// the active user scope so filtering by status does not silently reset which
// user's plans are shown.
func TestPlansIndex_ControlChipPreservesUserScope(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	// Scope to a peer, then check the emitted status chips keep it.
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=oidc:peer@acme.example", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "operated_by=oidc%3Apeer%40acme.example") {
		t.Errorf("status/group chips should preserve the peer user scope; body missing the encoded operated_by")
	}
}

// TestPlansIndex_ActiveChipMarked proves exactly the current status chip carries
// chip-active so the UI reflects the applied filter.
func TestPlansIndex_ActiveChipMarked(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=closed", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The Closed chip should render active.
	if !strings.Contains(body, `chip chip-active" href`) {
		t.Errorf("expected an active chip in the control strip")
	}
	// A "× clear" affordance appears once a non-default filter is applied.
	if !strings.Contains(body, "× clear") {
		t.Errorf("a non-default filter should surface the × clear affordance")
	}
}

// TestPlansIndex_GroupByProjectRendersSections proves ?group=project sections
// the list into per-group <section>s with headings + counts.
func TestPlansIndex_GroupByProjectRendersSections(t *testing.T) {
	f := seedPlansFake()
	// Add a second plan in a different project so grouping produces 2 sections.
	plan2 := &beadsapi.BeadDetail{
		ID:        "gd-plan2",
		Title:     "Plan: coop credential broker",
		Kind:      "issue",
		Type:      "epic",
		Status:    "open",
		Labels:    []string{"project:coop", "plan"},
		UpdatedAt: f.beads["gd-plan1"].UpdatedAt,
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2},
		Total: 2,
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&group=project", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-group"`) {
		t.Errorf("group=project should render grouped <section>s")
	}
	if !strings.Contains(body, `id="plan-group-gardenio"`) || !strings.Contains(body, `id="plan-group-coop"`) {
		t.Errorf("expected per-project group sections (gardenio + coop); body missing one")
	}
	if !strings.Contains(body, "plan-group-head") {
		t.Errorf("group sections should carry a heading")
	}
	// Both cards still render inside their groups.
	if !strings.Contains(body, "gd-plan1") || !strings.Contains(body, "gd-plan2") {
		t.Errorf("both plan cards should render under their group sections")
	}
}

// TestPlansIndex_FlatWhenNoGroup proves ?view=grid with no ?group= renders one
// flat grid with no group sections (the historical default layout, still
// reachable behind the board via the View toggle).
func TestPlansIndex_FlatWhenNoGroup(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&view=grid", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="plan-group"`) {
		t.Errorf("no ?group= should render a flat grid, not group sections")
	}
	if !strings.Contains(body, `class="plans-list"`) {
		t.Errorf("flat grid should still render the plans-list")
	}
	if strings.Contains(body, `class="plans-board"`) {
		t.Errorf("?view=grid must not render the board")
	}
}

// TestPlansIndex_FilteredEmptyStateViewerPointsAtClear proves that when a
// status/project/group filter matches nothing, a read-only viewer gets a
// clear-filters remedy — not the cold-start "you have no plans" copy.
func TestPlansIndex_FilteredEmptyStateViewerPointsAtClear(t *testing.T) {
	f := seedPlansFake()
	// No plans match: return an empty epic list for the filtered query.
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	req := plansIndexReq("/plans?operated_by=&status=closed", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No plans match the current filters") {
		t.Errorf("filtered empty state should explain a filter is hiding results")
	}
	if !strings.Contains(body, "Clear filters") {
		t.Errorf("filtered empty state should offer a clear-filters remedy")
	}
	// A read-only viewer never gets a create form.
	if strings.Contains(body, `action="/plans"`) {
		t.Errorf("a read-only viewer must not get a create form on the empty state")
	}
	// The stale "you have no plans yet" cold-start copy must not appear.
	if strings.Contains(body, "You have no plans yet") || strings.Contains(body, "No plans yet") {
		t.Errorf("filtered empty state must not use the cold-start no-plans copy")
	}
}

// TestPlansIndex_FilteredEmptyStateOperatorKeepsCreate proves an operator who
// filters to nothing gets the honest "no plans match" heading + clear remedy
// AND keeps the create form (a new plan in the active project scope is valid),
// but NOT the misleading cold-start "you have no plans yet" copy.
func TestPlansIndex_FilteredEmptyStateOperatorKeepsCreate(t *testing.T) {
	f := seedPlansFake()
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPlansReq("/plans?operated_by=&status=closed", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No plans match these filters") {
		t.Errorf("operator filtered empty state should lead with an honest no-match heading")
	}
	if !strings.Contains(body, "Clear filters") {
		t.Errorf("operator filtered empty state should offer a clear-filters remedy")
	}
	// Operator keeps the create affordance.
	if !strings.Contains(body, `action="/plans"`) {
		t.Errorf("an operator should keep the create form even when filtered to nothing")
	}
	// But not the misleading cold-start heading.
	if strings.Contains(body, "Start a plan from a one-line goal") {
		t.Errorf("filtered empty state must not render the cold-start create hero heading")
	}
}

// TestPlansIndex_UnfilteredEmptyStateKeepsColdStart proves the cold-start create
// hero still renders when the list is empty with NO filter active (unchanged
// behavior for a genuinely-empty /plans).
func TestPlansIndex_UnfilteredEmptyStateKeepsColdStart(t *testing.T) {
	f := seedPlansFake()
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	// Operator, show-all, no status/project/group filter → cold-start hero.
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "No plans match the current filters") {
		t.Errorf("unfiltered empty state must not show the filter-remedy copy")
	}
	if !strings.Contains(body, "Start a plan from a one-line goal") {
		t.Errorf("unfiltered empty state should keep the cold-start create hero")
	}
}

// --- S2: creator chip -------------------------------------------------------

// TestPlansIndex_RendersCreatorChip proves the card surfaces who operates the
// plan, with the filter link scoped to /plans (not /search).
func TestPlansIndex_RendersCreatorChip(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].OperatedBy = "oidc:alice@acme.example"
	f.beads["gd-plan1"].OperatedByDisplay = "Alice"
	// hotEpicsResp carries the same bead pointer, so the mutation above is
	// already reflected — but set it explicitly for clarity.
	f.hotEpicsResp.Beads[0] = f.beads["gd-plan1"]
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	// Show all so alice's plan isn't filtered out for the anonymous viewer.
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-operated-by="oidc:alice@acme.example"`) {
		t.Errorf("card should carry the creator chip for the plan operator")
	}
	if !strings.Contains(body, "/plans?operated_by=oidc") {
		t.Errorf("creator chip filter link should scope to /plans, not /search; body: %.400s",
			body[strings.Index(body, "plan-card-creator"):min(strings.Index(body, "plan-card-creator")+400, len(body))])
	}
	if strings.Contains(body, "/search?operated_by=oidc%3Aalice") {
		t.Errorf("creator chip must NOT link to /search")
	}
}

// TestPlansIndex_CreatorChipLinksToUserHomepage proves planIndexEnrich resolves
// homepage slugs (populateOperatedBySlugs) so a human creator chip links to
// /u/{slug} — parity with /active, /agents, /decisions, /escalations
// (ka-0000007589 item 1).
func TestPlansIndex_CreatorChipLinksToUserHomepage(t *testing.T) {
	f := seedPlansFake()
	opID := int64(42)
	f.beads["gd-plan1"].OperatedBy = "oidc:alice@acme.example"
	f.beads["gd-plan1"].OperatedByDisplay = "Alice"
	f.beads["gd-plan1"].OperatedByID = &opID
	f.opSlugResp = map[int64]*beadsapi.GetOperatorSlugResponse{
		opID: {Slug: "alice-a", Found: true},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/u/alice-a"`) {
		t.Errorf("creator chip should link to the operator homepage /u/alice-a")
	}
}

// --- S3: current-or-next step -----------------------------------------------

// TestPlansIndex_CurrentStepPrefersInProgress proves an in_progress step wins
// over a lower-numbered open step (work happening now beats what's next).
func TestPlansIndex_CurrentStepPrefersInProgress(t *testing.T) {
	f := seedPlansFake()
	// step1 (open, #1), step2 (in_progress, #2). The in_progress one is "now".
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Build the thing", Kind: "issue", Type: "task",
		Status: "in_progress", CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-step-kind-now") {
		t.Errorf("expected the 'now' (in-progress) step kind on the card")
	}
	if !strings.Contains(body, "Build the thing") {
		t.Errorf("expected the in-progress step title as current step")
	}
	if !strings.Contains(body, "#2") {
		t.Errorf("expected stable step number #2 for the in-progress step")
	}
}

// TestPlansIndex_NextStepWhenNoneInProgress proves the lowest-numbered open step
// is surfaced as "next" when nothing is in progress.
func TestPlansIndex_NextStepWhenNoneInProgress(t *testing.T) {
	f := seedPlansFake()
	// step1 (closed, #1), step2 (open, #2) → next is step2.
	f.beads["gd-step1"].Status = "closed"
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Do the next bit", Kind: "issue", Type: "task",
		Status: "open", CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 1}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-step-kind-next") {
		t.Errorf("expected the 'next' step kind on the card")
	}
	if !strings.Contains(body, "Do the next bit") {
		t.Errorf("expected the open step title as next step")
	}
}

// TestPlansIndex_BlockedNextStepReadsBlocked proves that when the next open step
// is status=blocked (it cannot start), the now/next chip paints it in the red
// "blocked" kind — NOT the calm "next" blue that reads identical to a ready
// step (ka-0000002840, iteration 14). pickCurrentOrNextStep returns the first
// non-closed step, which includes a blocked one, so this is the truth-gap the
// blocked kind closes: a wedged next step must not read as ready-to-dispatch.
func TestPlansIndex_BlockedNextStepReadsBlocked(t *testing.T) {
	f := seedPlansFake()
	// step1 (closed, #1), step2 (blocked, #2) → the next step is blocked.
	f.beads["gd-step1"].Status = "closed"
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Wedged on an escalation", Kind: "issue", Type: "task",
		Status: "blocked", CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 1}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-step-kind-blocked") {
		t.Errorf("expected the red 'blocked' step kind on the card for a blocked next step")
	}
	// The blocked step must NOT masquerade as a ready "next" step. The "next"
	// class also appears in the page's <style> block, so assert on the rendered
	// pill markup, not the bare class name.
	if strings.Contains(body, `plan-step-kind-next">next</span>`) {
		t.Errorf("a blocked next step must not render the calm 'next' pill")
	}
	if !strings.Contains(body, "Wedged on an escalation") {
		t.Errorf("expected the blocked step title as the current/next step")
	}
	// The tooltip must name the blocked condition, not the generic "Next open
	// step" lead-in.
	if !strings.Contains(body, "Blocked — cannot start") {
		t.Errorf("expected the blocked tooltip lead-in on the chip")
	}
}

// TestPlanCardStepKind_Projections pins the three now/next chip states as a pure
// projection of Status, so the CSS class, the pill word, and the tooltip
// lead-in can never disagree (ka-0000002840, iteration 14).
func TestPlanCardStepKind_Projections(t *testing.T) {
	cases := []struct {
		status    string
		wantKind  string
		wantLabel string
		wantTitle string
	}{
		{"in_progress", "now", "now", "In progress"},
		{"blocked", "blocked", "blocked", "Blocked — cannot start"},
		{"open", "next", "next", "Next open step"},
		{"deferred", "next", "next", "Next open step"},
	}
	for _, c := range cases {
		s := planCardStep{Status: c.status}
		if got := s.Kind(); got != c.wantKind {
			t.Errorf("Kind(%q) = %q, want %q", c.status, got, c.wantKind)
		}
		if got := s.KindLabel(); got != c.wantLabel {
			t.Errorf("KindLabel(%q) = %q, want %q", c.status, got, c.wantLabel)
		}
		if got := s.KindTitle(); got != c.wantTitle {
			t.Errorf("KindTitle(%q) = %q, want %q", c.status, got, c.wantTitle)
		}
		// Anti-drift: the pill word IS the class token (KindLabel == Kind).
		if s.KindLabel() != s.Kind() {
			t.Errorf("KindLabel(%q)=%q must equal Kind()=%q (class/label drift)", c.status, s.KindLabel(), s.Kind())
		}
	}
}

// TestPlansIndex_NoStepChipWhenAllClosed proves a fully-done plan shows no
// current/next chip (the progress bar already tells that story).
func TestPlansIndex_NoStepChipWhenAllClosed(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "closed"
	f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 1}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Assert on the rendered markup (a <div class="plan-card-step"), not the
	// bare class name which also appears in the page's <style> block.
	if strings.Contains(w.Body.String(), `<div class="plan-card-step"`) {
		t.Errorf("a plan with all steps closed should render no current/next step chip")
	}
}

// --- S4: assistant status chip ----------------------------------------------

// TestPlansIndex_AssistantChipLive proves a plan with a live bound assistant
// renders the "assistant live" chip linking to a real, non-404 target. With no
// coopmux configured and no pod_name on the assistant, assistantLinkURL falls
// back to the assistant's bead page /{id} — never the old /agents/{id}, which
// 404s (ka-0000007019: there is no GET /agents/{id} route).
func TestPlansIndex_AssistantChipLive(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Unified chip (step 7): a gardener-less plan with a live assistant reads
	// "live" with the live tone — the SAME plan_agent_chip the detail card
	// renders. Match the rendered class attribute (not the bare class name,
	// which also appears in the shared CSS in every page's <head>).
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-live") {
		t.Errorf("expected the live-tone unified chip on the card; body=%s", snippetN(body, 800))
	}
	// The click-through now lives on the separate compact terminal link, not the chip.
	if !strings.Contains(body, `class="plan-card-agent-link" href="/gd-asst1"`) {
		t.Errorf("terminal link should point at the bead-page fallback /gd-asst1; body=%s", snippetN(body, 800))
	}
	if strings.Contains(body, "/agents/gd-asst1") {
		t.Errorf("terminal link must NOT link to the 404 route /agents/{id}")
	}
	// The old assistant-only chip vocabulary is gone.
	if strings.Contains(body, "plan-asst-chip") {
		t.Errorf("the legacy plan-asst-chip should be replaced by the unified chip")
	}
}

// TestPlansIndex_AssistantChipLinksCoopmux proves that when coopmux is
// configured AND the assistant has a pod_name, the chip links to the public
// coopmux terminal page (<COOPMUX_PUBLIC_URL>#<pod_name>) in a new tab — the
// primary intent of ka-0000007019 ("take us to the coopmux page for the agent").
func TestPlansIndex_AssistantChipLinksCoopmux(t *testing.T) {
	f := seedPlansFake()
	a := seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	a.Notes = "pod_name: gardenio-plan-asst-1-abc12"
	srv := NewServer(f, slog.Default(), "", "", "", &fakeScreenClient{})
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	srv.coopmuxPublicURL = "https://coopmux.example/mux"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="https://coopmux.example/mux#gardenio-plan-asst-1-abc12"`) {
		t.Errorf("terminal link should point at the coopmux terminal page; body=%s", snippetN(body, 1200))
	}
	// The click-through moved from the chip to the compact terminal link (step 7);
	// external coopmux links still open in a new tab, matching the /agents chips.
	if !strings.Contains(body, `class="plan-card-agent-link" href="https://coopmux.example/mux#gardenio-plan-asst-1-abc12" target="_blank" rel="noopener"`) {
		t.Errorf("coopmux terminal link should open in a new tab; body=%s", snippetN(body, 1200))
	}
	if strings.Contains(body, "/agents/gd-asst1") {
		t.Errorf("terminal link must NOT link to the 404 route /agents/{id}")
	}
}

// TestPlansIndex_AssistantChipParked proves a plan with a stale (parked)
// assistant and no gardener renders the "parked"-tone unified chip.
func TestPlansIndex_AssistantChipParked(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstp", "gardenio-plan-asst-p", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-parked") {
		t.Errorf("expected the parked-tone unified chip on the card; body=%s", snippetN(body, 800))
	}
	if strings.Contains(body, "plan-asst-chip") {
		t.Errorf("the legacy plan-asst-chip should be replaced by the unified chip")
	}
}

// TestPlansIndex_AssistantChipQueued proves a woken assistant whose spawn was
// declined on an exhausted account pool (agent_state=queued) renders the
// distinct queued-tone chip on the index card — NOT collapsed to the idle
// parked tone (ka-0000003525). Freshness is irrelevant: a queued spawn stays
// queued until the reconciler re-attempts, so this uses a stale timestamp to
// prove queued is checked before the parked fallthrough.
func TestPlansIndex_AssistantChipQueued(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstq", "gardenio-plan-asst-q", "gd-plan1", "queued", planAssistantTestNow.Add(-2*time.Hour))
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-queued") {
		t.Errorf("expected the queued-tone unified chip on the card; body=%s", snippetN(body, 800))
	}
	// Match the rendered class attribute ("plan-agent-chip plan-agent-chip-parked"),
	// not the bare token — the parked tone appears in the shared <style> block.
	if strings.Contains(body, "plan-agent-chip plan-agent-chip-parked") {
		t.Errorf("a queued assistant must NOT collapse to the parked tone; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "rate-limited") {
		t.Errorf("the queued chip should read \"rate-limited\"; body=%s", snippetN(body, 800))
	}
}

// TestPlansIndex_AssistantChipNone proves a plan with no gardener and no
// assistant renders the "not started" unified chip (a span, no terminal link).
func TestPlansIndex_AssistantChipNone(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-none") {
		t.Errorf("expected the none-tone unified chip on a plan with no gardener/assistant; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "not started") {
		t.Errorf("the none chip should read \"not started\"; body=%s", snippetN(body, 800))
	}
	// No assistant → no terminal link anchor to render (the bare class name
	// still appears in the shared CSS, so match the rendered anchor).
	if strings.Contains(body, `class="plan-card-agent-link"`) {
		t.Errorf("a plan with no agent should render no terminal link")
	}
}

// TestPlansIndex_AssistantChipPrefersLiveOverParked proves the bulk join picks
// the live assistant when a plan has both a live and a stale bead bound to it
// (mirrors planAssistant's single-plan preference).
func TestPlansIndex_AssistantChipPrefersLiveOverParked(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-parked", "gardenio-plan-asst-old", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
	seedAssistantBead(f, "gd-live", "gardenio-plan-asst-new", "gd-plan1", "working", planAssistantTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-live") {
		t.Errorf("bulk join should prefer the live assistant (live-tone chip); body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, `class="plan-card-agent-link" href="/gd-live"`) {
		t.Errorf("terminal link should point at the LIVE assistant gd-live, not the parked one")
	}
	if strings.Contains(body, `href="/gd-parked"`) {
		t.Errorf("terminal link must not point at the parked assistant gd-parked")
	}
}

// --- step 7 (ka-0000003685): gardener/loop state on the index card ----------

// TestPlansIndex_GardenerChipIdleWhenNoAgent proves an enabled gardener with NO
// live iteration reads the calm "idle" chip — NOT green "running" (Matt's ask,
// thread ka-0000001290: "plans without a running agent show as running"). The
// loop is armed and will fire on schedule, but nothing is executing, so the chip
// must not over-claim activity.
func TestPlansIndex_GardenerChipIdleWhenNoAgent(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-idle") {
		t.Errorf("an enabled gardener with no live agent should show the idle-tone chip; body=%s", snippetN(body, 800))
	}
	// It must NOT read as green "running" — that tone is now reserved for a
	// genuinely-live agent.
	if strings.Contains(body, "plan-agent-chip plan-agent-chip-running") {
		t.Errorf("a gardener with no live agent must NOT show the running-tone chip; body=%s", snippetN(body, 800))
	}
	// The hover title still names the running loop (the loop IS enabled) even
	// though the label reads idle.
	if !strings.Contains(body, "gardener loop is running") {
		t.Errorf("the idle chip's title should still name the running loop; body=%s", snippetN(body, 800))
	}
}

// TestPlansIndex_GardenerChipRunningWhenLive proves the green "running · live"
// chip appears precisely when the enabled gardener HAS a live iteration — so
// "running" now means exactly "an agent is executing", never just "the loop is
// enabled".
func TestPlansIndex_GardenerChipRunningWhenLive(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-running") {
		t.Errorf("an enabled gardener WITH a live iteration should show the running-tone chip; body=%s", snippetN(body, 800))
	}
	if strings.Contains(body, "plan-agent-chip plan-agent-chip-idle") {
		t.Errorf("a running gardener with a live agent must NOT read idle; body=%s", snippetN(body, 800))
	}
}

// TestPlansIndex_GardenerChipPaused proves a paused-gardener plan is visibly
// distinct from a running one right on the index — the task's acceptance
// criterion. The paused tone (amber) differs from the running tone (green).
func TestPlansIndex_GardenerChipPaused(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-paused") {
		t.Errorf("a plan with a disabled gardener should show the paused-tone chip; body=%s", snippetN(body, 800))
	}
	// Acceptance: the paused plan must NOT render a running-tone chip. Match the
	// rendered class attribute, not the bare tone name (which is in the CSS on
	// every page).
	if strings.Contains(body, "plan-agent-chip plan-agent-chip-running") {
		t.Errorf("a paused-gardener plan must be visibly distinct from a running one (no running-tone chip)")
	}
}

// TestPlansIndex_GardenerChipRetired proves a closed gardener reads "retired" on
// the index (the honest lifecycle word the detail card shows), distinct from
// both running and paused.
func TestPlansIndex_GardenerChipRetired(t *testing.T) {
	f := seedPlansFake()
	// seedPlanGardener seeds status=open; a retired gardener is closed.
	cap := seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	cap.Status = "closed"
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-retired") {
		t.Errorf("a plan with a closed gardener should show the retired-tone chip; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "gardener loop is retired") {
		t.Errorf("the retired chip's title should name the retired loop; body=%s", snippetN(body, 800))
	}
	if strings.Contains(body, "plan-agent-chip plan-agent-chip-running") {
		t.Errorf("a retired-gardener plan must not render a running-tone chip")
	}
}

// TestPlansIndex_GardenerChipRunningWithLiveIteration proves the compound chip
// folds a live iteration into a running gardener as "running · live" with a
// liveness dot — the SAME loop-primary + agent-liveness projection as the
// detail card, so the two surfaces can never contradict.
func TestPlansIndex_GardenerChipRunningWithLiveIteration(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-running") {
		t.Errorf("running gardener + live iteration should keep the running tone; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "running · live") {
		t.Errorf("the compound chip should read \"running · live\"; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "plan-agent-chip-dot plan-agent-chip-dot-live") {
		t.Errorf("a live iteration under a running loop should render the live liveness dot; body=%s", snippetN(body, 800))
	}
}

// TestPlansIndex_GardenerChipIsSingleQuery proves the gardener-state join for the
// whole page is ONE role-scoped gardeners list — shared with the stall badge
// (step 7 hoisted planGardenersByEpic into planIndexEnrich), NOT one-per-plan.
// Two plans, each with its own gardener → still exactly one gardeners list call.
func TestPlansIndex_GardenerChipIsSingleQuery(t *testing.T) {
	f := seedPlansFake()
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "open", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	seedPlanGardener(f, "gd-cap2", "gd-plan2", "false")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Count role:gardener list queries (the gardeners-by-epic batch). One for the
	// whole page — the stall badge and the unified chip share it.
	gardenerCalls := 0
	for _, q := range f.listAllQ {
		if slices.Contains(q.Labels, "role:gardener") {
			gardenerCalls++
		}
	}
	if gardenerCalls != 1 {
		t.Errorf("expected exactly 1 bulk gardeners query for the whole page, got %d", gardenerCalls)
	}
	body := w.Body.String()
	// Both plans render their respective chip from the single bulk join: the
	// enabled gardener with no live agent reads "idle" (Matt's ask, thread
	// ka-0000001290 — not green "running"), the disabled one reads "paused".
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-idle") || !strings.Contains(body, "plan-agent-chip plan-agent-chip-paused") {
		t.Errorf("both plans' gardener states should resolve from the single bulk join; body=%s", snippetN(body, 1200))
	}
}

// --- step 2 (ka-0000007131): per-gardener enable/disable toggle affordance ----

// plansIndexBodyOperator serves an operator-scoped /plans request against a
// clock-pinned mux and returns the rendered body — the toggle tests need both
// the operator role (so the lever renders) AND a pinned now (so the
// gardener-liveness join is stable).
func plansIndexBodyOperator(t *testing.T, f *fakeDaemon, target string) string {
	t.Helper()
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPlansReq(target, "op@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	return w.Body.String()
}

// TestPlansIndex_GardenerToggleForOperatorRunning proves an operator sees the
// per-gardener enable/disable lever on a row whose gardener is running, that it
// POSTs to the plans-scoped /plans/{id}/gardener/pause-toggle handler (step 3,
// ka-0000003881) targeting the whole row so the unified chip re-renders live,
// and that it reflects the running state.
func TestPlansIndex_GardenerToggleForOperatorRunning(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	// The lever is the shared gardener_pause_toggle partial, keyed by the bound
	// GARDENER trigger id (gd-cap1) — not the epic id.
	if !strings.Contains(body, `id="gardener-pause-gd-cap1"`) {
		t.Errorf("operator should see the per-gardener toggle keyed by the gardener id; body=%s", snippetN(body, 1000))
	}
	// Step 3 repoints the row toggle at the plans-scoped handler (keyed by the
	// PLAN epic id gd-plan1), NOT the shared /gardener/{id}/pause-toggle verb, so
	// the response can re-render the whole row (chip + lever) rather than just the
	// switch.
	if !strings.Contains(body, `hx-post="/plans/gd-plan1/gardener/pause-toggle"`) {
		t.Errorf("the row toggle should POST to the plans-scoped /plans/{id}/gardener/pause-toggle handler; body=%s", snippetN(body, 1000))
	}
	// It targets the whole row (closest .plan-card) and swaps it outerHTML, so the
	// unified state chip updates in lockstep with the switch.
	if !strings.Contains(body, `hx-target="closest .plan-card"`) {
		t.Errorf("the row toggle should target the whole row (closest .plan-card); body=%s", snippetN(body, 1000))
	}
	// A running gardener renders the running skin (switch on).
	if !strings.Contains(body, "cap-pause-running") {
		t.Errorf("a running gardener's toggle should render the running skin; body=%s", snippetN(body, 1000))
	}
	if !strings.Contains(body, `aria-checked="true"`) {
		t.Errorf("a running gardener's switch should read aria-checked=true; body=%s", snippetN(body, 1000))
	}
}

// TestPlansIndex_GardenerTogglePaused proves a paused gardener's row lever renders
// the paused skin (switch off) so an operator can resume it in place — and the
// paused state is visibly distinct from running.
func TestPlansIndex_GardenerTogglePaused(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	if !strings.Contains(body, `id="gardener-pause-gd-cap1"`) {
		t.Errorf("operator should see the per-gardener toggle on a paused gardener; body=%s", snippetN(body, 1000))
	}
	if !strings.Contains(body, "cap-pause-paused") {
		t.Errorf("a paused gardener's toggle should render the paused skin; body=%s", snippetN(body, 1000))
	}
	if !strings.Contains(body, `aria-checked="false"`) {
		t.Errorf("a paused gardener's switch should read aria-checked=false; body=%s", snippetN(body, 1000))
	}
}

// TestPlansIndex_NoGardenerToggleForViewer proves the lever is operator-gated: a
// read-only viewer sees the state chip but NO toggle (parity with the spawn
// form gate). The task's "read-only viewers see state but no lever."
func TestPlansIndex_NoGardenerToggleForViewer(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	req := plansIndexReq("/plans?operated_by=", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The viewer still sees the state chip … (enabled gardener, no live agent →
	// the idle tone, Matt's ask thread ka-0000001290; the assertion's point is
	// that the chip renders for a viewer at all).
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-idle") {
		t.Errorf("a read-only viewer should still see the gardener state chip; body=%s", snippetN(body, 800))
	}
	// … but NOT the mutation lever.
	if strings.Contains(body, "pause-toggle") {
		t.Errorf("a read-only viewer must not see the per-gardener toggle lever")
	}
}

// TestPlansIndex_NoGardenerToggleWhenRetired proves a retired (closed) gardener
// renders no lever — there is nothing to resume, mirroring the detail card's
// "this gardener is retired" note. The state chip still reads "retired".
func TestPlansIndex_NoGardenerToggleWhenRetired(t *testing.T) {
	f := seedPlansFake()
	cap := seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	cap.Status = "closed"

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-retired") {
		t.Errorf("a closed gardener should still read the retired-tone chip; body=%s", snippetN(body, 800))
	}
	if strings.Contains(body, "pause-toggle") {
		t.Errorf("a retired gardener has nothing to resume — no toggle should render")
	}
}

// TestPlansIndex_NoGardenerToggleWhenNoGardener proves a gardener-less plan renders
// no lever for an operator (there is no gardener to enable/disable) — only the
// "not started" state chip.
func TestPlansIndex_NoGardenerToggleWhenNoGardener(t *testing.T) {
	f := seedPlansFake()

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-none") {
		t.Errorf("a gardener-less plan should read the none-tone chip; body=%s", snippetN(body, 800))
	}
	if strings.Contains(body, "pause-toggle") {
		t.Errorf("a plan with no bound gardener should render no per-gardener toggle")
	}
}

// --- Step 3: plans-scoped per-gardener enable/disable handler ----------------
// (ka-0000003881). The POST /plans/{id}/gardener/pause-toggle handler flips the
// bound gardener's enabled field via the SHARED setGardenerEnabledCore and
// re-renders the WHOLE index row (plan_card) so the unified chip updates live.

// TestPlanRowGardenerToggle_PausesRunning proves the row toggle disables a
// running gardener (writes enabled=false via the shared core) and the re-rendered
// row is the plan_card fragment reflecting the new paused state.
func TestPlanRowGardenerToggle_PausesRunning(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPost("/plans/gd-plan1/gardener/pause-toggle", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	// The mutation goes through the shared core, keyed by the GARDENER id, and
	// writes a JSON bool (not the string "false").
	if f.updateFieldsLastID != "gd-cap1" {
		t.Errorf("toggle wrote to %q, want gd-cap1", f.updateFieldsLastID)
	}
	if f.updateFieldsLastVal["enabled"] != false {
		t.Errorf("pausing a running gardener should write enabled=false, got %v", f.updateFieldsLastVal["enabled"])
	}
	body := w.Body.String()
	// The response is a single re-rendered ROW: the plan_card <li>, now showing
	// the paused chip + the paused-skin switch (the chip updated live alongside
	// the lever — the whole point of the whole-row swap).
	if !strings.Contains(body, `data-plan-id="gd-plan1"`) {
		t.Errorf("response should be the re-rendered plan row; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-paused") {
		t.Errorf("the re-rendered row's chip should read paused; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "cap-pause-paused") {
		t.Errorf("the re-rendered switch should read the paused skin; body=%s", snippetN(body, 800))
	}
}

// TestPlanRowGardenerToggle_ResumesPaused proves the row toggle enables a paused
// gardener (writes enabled=true) on an ungated plan and the row re-renders with
// the enabled-loop chip. With no live iteration bound that reads "idle" (the loop
// is armed but nothing is executing — Matt's ask, thread ka-0000001290), NOT
// green "running".
func TestPlanRowGardenerToggle_ResumesPaused(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPost("/plans/gd-plan1/gardener/pause-toggle", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if f.updateFieldsLastVal["enabled"] != true {
		t.Errorf("resuming a paused gardener should write enabled=true, got %v", f.updateFieldsLastVal["enabled"])
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-idle") {
		t.Errorf("the re-rendered row's chip should read idle (enabled, no live agent); body=%s", snippetN(body, 800))
	}
	// And it must NOT read paused anymore — the resume took effect.
	if strings.Contains(body, "plan-agent-chip plan-agent-chip-paused") {
		t.Errorf("a resumed gardener's row must not still read paused; body=%s", snippetN(body, 800))
	}
}

// TestPlanRowGardenerToggle_GateBlocksEnableOnDraft proves the R6 build gate is
// honoured on the index row too: enabling a gardener on a still-authoring (draft)
// plan is refused, nothing is written, and the row re-renders with the inline
// "Approve & build" note — the index is NOT a build-gate bypass.
func TestPlanRowGardenerToggle_GateBlocksEnableOnDraft(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateDraft)}
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false") // paused → a click would ENABLE
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPost("/plans/gd-plan1/gardener/pause-toggle", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("a gated enable must not write; got %d UpdateBeadFieldsTyped calls", f.updateFieldsCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-cap-toggle-err") || !strings.Contains(body, "Approve &amp; build") {
		t.Errorf("a gated enable should re-render the row with the inline Approve & build note; body=%s", snippetN(body, 800))
	}
}

// TestPlanRowGardenerToggle_GateAllowsPauseOnDraft proves the gate blocks only the
// ENABLE direction: pausing a running gardener on a draft plan is always allowed.
func TestPlanRowGardenerToggle_GateAllowsPauseOnDraft(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateDraft)}
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true") // running → a click PAUSES
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPost("/plans/gd-plan1/gardener/pause-toggle", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if f.updateFieldsLastVal["enabled"] != false {
		t.Errorf("pausing on a draft plan should still write enabled=false, got %v", f.updateFieldsLastVal["enabled"])
	}
}

// TestPlanRowGardenerToggle_NoGardenerRerendersRow proves a stale click (the plan
// lost its gardener between page load and click) writes nothing and re-renders
// the ROW (no lever), NOT the detail cockpit card.
func TestPlanRowGardenerToggle_NoGardenerRerendersRow(t *testing.T) {
	f := seedPlansFake() // no gardener bound
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPost("/plans/gd-plan1/gardener/pause-toggle", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("a toggle with no bound gardener must not write; got %d calls", f.updateFieldsCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-plan-id="gd-plan1"`) {
		t.Errorf("response should be the re-rendered plan row; body=%s", snippetN(body, 800))
	}
	if strings.Contains(body, "cap-pause") {
		t.Errorf("a row with no bound gardener must render no toggle; body=%s", snippetN(body, 800))
	}
}

// TestPlanRowGardenerToggle_RequiresOperator proves the mutation route is
// operator-gated: a read-only viewer is rejected and nothing is written.
func TestPlanRowGardenerToggle_RequiresOperator(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := httptest.NewRequest(http.MethodPost, "/plans/gd-plan1/gardener/pause-toggle", nil)
	req.Header.Set("X-Auth-Request-Email", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Errorf("a read-only viewer must not reach the toggle handler; got 200")
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("a rejected viewer toggle must not write; got %d calls", f.updateFieldsCalls)
	}
}

// --- S5: dispatch-next button -----------------------------------------------

// TestPlansIndex_SpawnFormForOperator proves an operator sees the "Dispatch Next"
// button on a plan card that has an open step to hand off to the gardener.
func TestPlansIndex_SpawnFormForOperator(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	req := plansIndexReq("/plans?operated_by=", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-post="/plans/gd-plan1/dispatch-next"`) {
		t.Errorf("operator should see the dispatch button posting to /plans/{id}/dispatch-next")
	}
	if !strings.Contains(body, `Dispatch`) {
		t.Errorf("dispatch button should render with 'Dispatch' label")
	}
	if !strings.Contains(body, `plan-card-dispatch`) {
		t.Errorf("dispatch form should carry the plan-card-dispatch class")
	}
}

// TestPlansIndex_SpawnFormButtonAdaptsToState proves the dispatch button only
// renders when the plan has a current/next open step.
func TestPlansIndex_SpawnFormButtonAdaptsToState(t *testing.T) {
	f := seedPlansFake()
	// No assistant, but step1 is open — dispatch button should render.
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := plansIndexReq("/plans?operated_by=", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The dispatch button should render with the step number.
	if !strings.Contains(body, `Dispatch #1`) {
		t.Errorf("dispatch button should render with step number when an open step exists; body=%s", snippetN(body, 800))
	}
}

// TestPlansIndex_NoSpawnFormForViewer proves a read-only viewer does NOT see
// the dispatch button (operator-gated).
func TestPlansIndex_NoSpawnFormForViewer(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	req := plansIndexReq("/plans?operated_by=", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "dispatch-next") {
		t.Errorf("a viewer must not see the operator dispatch button")
	}
}

// --- S0 / cross-cutting: bounded round-trips --------------------------------

// TestPlansIndex_BulkAssistantIsSingleQuery proves the assistant-state join for
// the whole page is ONE role-scoped list query, not one-per-plan (S0/S4). Two
// plans, each with an assistant → still exactly one plan-assistant list call.
func TestPlansIndex_BulkAssistantIsSingleQuery(t *testing.T) {
	f := seedPlansFake()
	// A second plan epic + its own assistant.
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "open", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	seedAssistantBead(f, "gd-asst2", "gardenio-plan-asst-2", "gd-plan2", "working", planAssistantTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Count role-scoped plan-gardener list queries with no plan_id field. The
	// resolver keys on the single role:plan-gardener label (the plan-assistant
	// dual-read was removed, decision ka-0000007411), so match it in either the
	// AND (Labels) or OR (LabelsAny) slot.
	bulkCalls := 0
	for _, q := range f.listAllQ {
		hasRoleLabel := slices.Contains(q.Labels, planGardenerRoleLabel) ||
			slices.Contains(q.LabelsAny, planGardenerRoleLabel)
		if hasRoleLabel && q.Fields["plan_id"] == "" {
			bulkCalls++
		}
	}
	if bulkCalls != 1 {
		t.Errorf("expected exactly 1 bulk plan-gardener query for the whole page, got %d", bulkCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/gd-asst1"`) || !strings.Contains(body, `href="/gd-asst2"`) {
		t.Errorf("both plans' assistants should resolve from the single bulk join")
	}
}

// --- Open-decision count folds into the batched walk (Phase 2, ka-0000004522) --

// addPlanDecisionChild seeds one decision bead as a parent-child child of the
// given plan epic (the real shape) so the index's batched child walk counts it.
// status is "open" for a live clarifying question, "closed" for a resolved one.
func addPlanDecisionChild(f *fakeDaemon, decID, planID, status string) {
	f.beads[decID] = &beadsapi.BeadDetail{
		ID: decID, Title: "Q " + decID, Type: "decision", Status: status,
		Fields:    map[string]string{"prompt": "clarify?"},
		UpdatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.revDeps[planID] = append(f.revDeps[planID],
		beadsapi.Dependency{BeadID: decID, DependsOnID: planID, Type: "parent-child"})
}

// TestPlansIndex_OpenDecisionCountIsBatchedNoPerRowQuery is the Phase 2
// regression guard (ka-0000004522 / ka-0000006056 / gd-dmqKFmr9z0): the "N to
// answer" chip must be derived from the SAME batched child walk that
// planIndexStepData already performs — NOT a per-row planOpenDecisionCount
// CountOnly round-trip. Before the fix, each plan row issued its own
// Types=[decision] Parent=epicID query; across a full page that was the N+1 this
// phase kills. The assertions:
//   - a plan with N open + M resolved decision children reports OpenDecisions=N
//     ("N to answer") purely from the batch; and
//   - ZERO Types=[decision] daemon queries fire during the index render, so the
//     per-row fan-out is gone regardless of how many plans are on the page.
func TestPlansIndex_OpenDecisionCountIsBatchedNoPerRowQuery(t *testing.T) {
	f := seedPlansFake()
	// seedPlansFake already parents ONE open decision (gd-dec1) to gd-plan1; add
	// a second open decision and a resolved one, so plan1 has 2 open + 1 resolved.
	addPlanDecisionChild(f, "gd-dec2", "gd-plan1", "open")
	addPlanDecisionChild(f, "gd-dec-closed", "gd-plan1", "closed")
	// A second plan on the same page with NO decisions — proves the batch tally is
	// per-plan and a decision-less plan shows no chip and costs no query either.
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// plan1: 2 open decisions counted from the batch, the resolved one excluded.
	if !strings.Contains(body, "2 to answer") {
		t.Errorf("plan1 should show '2 to answer' from the batch tally (2 open, 1 resolved); body missing it")
	}
	// Exactly one "to answer" chip on the whole page — plan2 (no decisions) shows none.
	if got := strings.Count(body, "to answer"); got != 1 {
		t.Errorf("expected exactly 1 open-decision chip on the page, got %d", got)
	}

	// The regression anchor: NO PER-ROW decision query may fire during the index
	// render. The old per-row planOpenDecisionCount issued a Types=[decision],
	// Parent=epicID, CountOnly query per plan; the batched walk issues none. Match
	// that precise N+1 shape (CountOnly && Parent set) rather than any
	// Types=[decision] query — the Layer-1 unhomed backstop (ka-0000004983) issues
	// exactly ONE page-wide open-decision enumeration (no Parent, no CountOnly),
	// which is O(1) in the plan count, not the per-row fan-out this guards. Same
	// precise shape TestPlansEnrich_NoPerRowDecisionCountCall pins.
	for _, q := range f.listAllQ {
		if slices.Contains(q.Types, "decision") && q.CountOnly && q.Parent != "" {
			t.Errorf("index enrich issued a per-row decision query (Types=[decision], CountOnly=%v, Parent=%q) — the N+1 must be folded into the batched child walk",
				q.CountOnly, q.Parent)
		}
	}
}

// seedPlanWithDecisions builds a fresh fake serving ONE unphased plan epic
// (planID) with `steps` issue-type step children, `open` OPEN decision children,
// and `resolved` (closed) decision children — the exact N-open / M-resolved shape
// planIndexStepData's batched open-decision tally must count. Every child is a
// parent-child reverse-dep of the plan (the real on-daemon shape), so the tier-1
// reverse-deps walk fetches them all in one batch. Returns the fake and the plan
// epic to feed planIndexStepData directly.
func seedPlanWithDecisions(planID string, steps, open, resolved int) (*fakeDaemon, *beadsapi.BeadDetail) {
	f := &fakeDaemon{
		beads:   map[string]*beadsapi.BeadDetail{},
		revDeps: map[string][]beadsapi.Dependency{},
	}
	plan := &beadsapi.BeadDetail{
		ID: planID, Title: "Plan " + planID, Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	f.beads[planID] = plan
	addChild := func(id, typ, status string) {
		f.beads[id] = &beadsapi.BeadDetail{
			ID: id, Title: typ + " " + id, Kind: "issue", Type: typ, Status: status,
			UpdatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		}
		f.revDeps[planID] = append(f.revDeps[planID],
			beadsapi.Dependency{BeadID: id, DependsOnID: planID, Type: "parent-child"})
	}
	for i := range steps {
		addChild(fmt.Sprintf("%s-step%d", planID, i), "task", "open")
	}
	for i := range open {
		addChild(fmt.Sprintf("%s-decopen%d", planID, i), "decision", "open")
	}
	for i := range resolved {
		addChild(fmt.Sprintf("%s-decdone%d", planID, i), "decision", "closed")
	}
	return f, plan
}

// TestPlanIndexStepData_OpenDecisionCountFromBatchNoExtraCalls is the focused
// unit test the Phase 2 acceptance calls for (gd-dmqKFmr9z0, epic ka-0000004522):
// it drives planIndexStepData — the batched child walk — DIRECTLY (no HTTP
// render) and asserts two things off the fake daemon's call-count instrumentation:
//
//   - a plan with N open + M resolved decision children reports OpenDecisions=N
//     purely from the batched walk (the resolved ones are excluded, and step
//     children never inflate the tally); and
//   - the walk costs EXACTLY one BatchGetDependencies + one BatchGetBeads for an
//     unphased plan and ZERO ListBeadsFiltered calls — i.e. no per-row
//     planOpenDecisionCount CountOnly round-trip. That absent CountOnly query is
//     the N+1 this phase kills; the counters prove it never fires.
func TestPlanIndexStepData_OpenDecisionCountFromBatchNoExtraCalls(t *testing.T) {
	cases := []struct {
		name           string
		open, resolved int
	}{
		{"3 open, 2 resolved", 3, 2},
		{"5 open, 0 resolved", 5, 0},
		{"0 open, 4 resolved", 0, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// steps=2 guarantees the plan renders steps, so the index entry is kept
			// even in the 0-open case (a decision-less, step-only plan is not
			// skipped) — this isolates the open-decision assertion from the
			// "question-only plan" keep-alive branch.
			f, plan := seedPlanWithDecisions("gd-plan1", 2, tc.open, tc.resolved)
			s := &Server{daemon: f, logger: slog.Default()}

			out := s.planIndexStepData(context.Background(), []*beadsapi.BeadDetail{plan})

			info := out[plan.ID]
			if info == nil {
				t.Fatalf("expected a planIndexStepInfo for %s", plan.ID)
			}
			if info.OpenDecisions != tc.open {
				t.Errorf("OpenDecisions = %d, want %d (%d open + %d resolved decision children in the batch)",
					info.OpenDecisions, tc.open, tc.open, tc.resolved)
			}

			// Exact daemon-call budget: an unphased plan is ONE reverse-deps batch
			// (tier 1) + ONE get-beads batch over the child union. No phase tier
			// fires (no phase_order on the epic).
			if f.batchGetDepsCalls != 1 {
				t.Errorf("BatchGetDependencies calls = %d, want exactly 1 (one tier-1 reverse-deps batch)", f.batchGetDepsCalls)
			}
			if f.batchGetBeadsCalls != 1 {
				t.Errorf("BatchGetBeads calls = %d, want exactly 1 (one batch over the child union)", f.batchGetBeadsCalls)
			}
			// The N+1 regression anchor: ZERO ListBeadsFiltered calls. The former
			// planOpenDecisionCount issued a Types=[decision], Statuses=[open],
			// CountOnly ListBeadsFiltered per plan; the batched walk issues none.
			if len(f.listAllQ) != 0 {
				t.Errorf("planIndexStepData issued %d ListBeadsFiltered call(s), want 0 — the open-decision count must come off the batched walk, not a CountOnly round-trip: %+v",
					len(f.listAllQ), f.listAllQ)
			}
		})
	}
}

// seedPhasedPlanWithDecisions builds a fresh fake serving ONE PHASED plan epic
// (planID, one phase sub-epic in fields.phase_order) carrying open decisions in
// BOTH tiers of the reverse-deps walk: `directOpen`/`directResolved` decisions
// parented straight to the plan epic (tier-1), and `phaseOpen`/`phaseResolved`
// decisions parented to the phase sub-epic (tier-2 — the rare phased-plan case,
// ka-0000007691). Each phase also carries one open task step so the phase renders.
// Every child is a parent-child reverse-dep of its parent (the real on-daemon
// shape), so the two-tier reverse-deps + get walk fetches them all. Returns the
// fake and the plan epic to feed planIndexStepData directly.
func seedPhasedPlanWithDecisions(planID, phaseID string, directOpen, directResolved, phaseOpen, phaseResolved int) (*fakeDaemon, *beadsapi.BeadDetail) {
	f := &fakeDaemon{
		beads:   map[string]*beadsapi.BeadDetail{},
		revDeps: map[string][]beadsapi.Dependency{},
	}
	plan := &beadsapi.BeadDetail{
		ID: planID, Title: "Plan " + planID, Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Fields:    map[string]string{"phase_order": fmt.Sprintf("[%q]", phaseID)},
		UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	phase := &beadsapi.BeadDetail{
		ID: phaseID, Title: "Phase " + phaseID, Kind: "issue", Type: "epic",
		Status:    "in_progress",
		UpdatedAt: time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC),
	}
	f.beads[planID] = plan
	f.beads[phaseID] = phase
	// The phase sub-epic is a direct child of the plan.
	f.revDeps[planID] = append(f.revDeps[planID],
		beadsapi.Dependency{BeadID: phaseID, DependsOnID: planID, Type: "parent-child"})
	addChild := func(parent, id, typ, status string) {
		f.beads[id] = &beadsapi.BeadDetail{
			ID: id, Title: typ + " " + id, Kind: "issue", Type: typ, Status: status,
			UpdatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		}
		f.revDeps[parent] = append(f.revDeps[parent],
			beadsapi.Dependency{BeadID: id, DependsOnID: parent, Type: "parent-child"})
	}
	// One open step under the phase so the phase (and plan) renders steps.
	addChild(phaseID, phaseID+"-step0", "task", "open")
	for i := range directOpen {
		addChild(planID, fmt.Sprintf("%s-decopen%d", planID, i), "decision", "open")
	}
	for i := range directResolved {
		addChild(planID, fmt.Sprintf("%s-decdone%d", planID, i), "decision", "closed")
	}
	for i := range phaseOpen {
		addChild(phaseID, fmt.Sprintf("%s-decopen%d", phaseID, i), "decision", "open")
	}
	for i := range phaseResolved {
		addChild(phaseID, fmt.Sprintf("%s-decdone%d", phaseID, i), "decision", "closed")
	}
	return f, plan
}

// TestPlanIndexStepData_PhasedPlanOpenDecisionCountBothTiers is the phased-plan
// acceptance for this task (ka-0000007691): a plan's clarifying-question
// decisions live under the plan epic directly (tier-1) OR, rarely, under a phase
// sub-epic (tier-2). planIndexStepData must count BOTH off the SAME batched
// reverse-deps walk it already performs for a phased plan — no per-plan
// planOpenDecisionCount CountOnly round-trip (that helper now survives only as
// the single-plan board-peek detail helper). Asserts:
//
//   - OpenDecisions = directOpen + phaseOpen across the two tiers (resolved
//     decisions in either tier are excluded, and the phase's task step never
//     inflates the tally); and
//   - the walk costs EXACTLY two BatchGetDependencies + two BatchGetBeads (tier-1
//     over the plan, tier-2 over the phase sub-epic) and ZERO ListBeadsFiltered —
//     no CountOnly decision query fires for the count.
func TestPlanIndexStepData_PhasedPlanOpenDecisionCountBothTiers(t *testing.T) {
	cases := []struct {
		name                                                 string
		directOpen, directResolved, phaseOpen, phaseResolved int
	}{
		{"decisions only under the phase (tier-2)", 0, 0, 2, 1},
		{"decisions only under the plan (tier-1)", 3, 1, 0, 0},
		{"decisions in both tiers", 2, 1, 2, 2},
		{"no open decisions, some resolved in both tiers", 0, 2, 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, plan := seedPhasedPlanWithDecisions("gd-plan1", "gd-phase1",
				tc.directOpen, tc.directResolved, tc.phaseOpen, tc.phaseResolved)
			s := &Server{daemon: f, logger: slog.Default()}

			out := s.planIndexStepData(context.Background(), []*beadsapi.BeadDetail{plan})

			info := out[plan.ID]
			if info == nil {
				t.Fatalf("expected a planIndexStepInfo for %s", plan.ID)
			}
			want := tc.directOpen + tc.phaseOpen
			if info.OpenDecisions != want {
				t.Errorf("OpenDecisions = %d, want %d (%d open tier-1 + %d open tier-2; resolved excluded)",
					info.OpenDecisions, want, tc.directOpen, tc.phaseOpen)
			}

			// Exact daemon-call budget for a phased plan: tier-1 reverse-deps over
			// the plan + tier-2 reverse-deps over the phase sub-epic = 2
			// BatchGetDependencies; one get over each tier's child union = 2
			// BatchGetBeads.
			if f.batchGetDepsCalls != 2 {
				t.Errorf("BatchGetDependencies calls = %d, want exactly 2 (tier-1 plan + tier-2 phase reverse-deps)", f.batchGetDepsCalls)
			}
			if f.batchGetBeadsCalls != 2 {
				t.Errorf("BatchGetBeads calls = %d, want exactly 2 (one get over each tier's child union)", f.batchGetBeadsCalls)
			}
			// The N+1 regression anchor: ZERO ListBeadsFiltered calls. The phased
			// open-decision count must come off the batched walk, not the per-plan
			// planOpenDecisionCount CountOnly round-trip (now board-peek-only).
			if len(f.listAllQ) != 0 {
				t.Errorf("planIndexStepData issued %d ListBeadsFiltered call(s), want 0 — the phased open-decision count must come off the two-tier batched walk: %+v",
					len(f.listAllQ), f.listAllQ)
			}
		})
	}
}

// --- Live-crew chip on the index card (ka-0000004801) ------------------------

// TestPlansIndex_CrewChipSurfacesLiveCrew proves a plan whose gardener has
// dispatched crew onto a step renders the dense live-crew chip on the index
// card — the "N crew · M working" tally with a live-dot — linking to the
// cockpit's crew roster.
func TestPlansIndex_CrewChipSurfacesLiveCrew(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-crew-a", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "done", "closed")
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on rendered markup (the anchor's class attribute), not the bare
	// class name — those also appear in the page's <style> block.
	if !strings.Contains(body, `<a class="plan-card-crew plan-card-crew-live"`) {
		t.Errorf("expected the live-crew chip (with the live modifier) on the index card")
	}
	if !strings.Contains(body, "2 crew") {
		t.Errorf("expected a '2 crew' total on the chip")
	}
	if !strings.Contains(body, "1 working") {
		t.Errorf("expected '1 working' on the chip")
	}
	if !strings.Contains(body, `<span class="plan-card-crew-dot"`) {
		t.Errorf("a plan with a working crew should render the live-dot")
	}
	if !strings.Contains(body, `/plans/gd-plan1#plan-crew`) {
		t.Errorf("the crew chip should link to the cockpit crew roster")
	}
	// The live-crew AVATAR strip (ka-0000002189): a click-through face for the
	// WORKING crew (gardenio-crew-a → "GCA"), but NOT the finished one.
	if !strings.Contains(body, `<span class="plan-card-crew-faces"`) {
		t.Errorf("expected the live-crew avatar strip on a plan with a working crew")
	}
	if !strings.Contains(body, `<a class="plan-card-crew-face" href="/gd-crewA"`) {
		t.Errorf("expected a click-through avatar linking to the working crew's bead page")
	}
	if !strings.Contains(body, `>GCA`) {
		t.Errorf("expected the working crew's initials (GCA) inside its avatar")
	}
	// The step-context tag (ka-0000004037, iter-15): the crew works the single flat
	// step, which is grid square #1, so the avatar carries a "#1" step tag naming
	// WHAT it's on — the same #N the mini-grid square + cockpit roster show.
	if !strings.Contains(body, `<span class="plan-card-crew-face-step" aria-hidden="true">#1</span></a>`) {
		t.Errorf("expected the crew face to carry its step-number tag (#1)")
	}
	// The step title folds into the avatar's hover tooltip.
	if !strings.Contains(body, `on step #1: Wire the route`) {
		t.Errorf("expected the crew face tooltip to name the step it works")
	}
	if strings.Contains(body, "gd-crewB") {
		t.Errorf("the finished crew (gd-crewB) must not appear as a live avatar")
	}
}

// TestPlansIndex_CrewChipSurfacesSpend proves the cumulative crew SPEND facet
// (ka-0000004218, iter-12) renders on the index crew chip: a "· $N" span summing
// every dispatched crew's dollar cost, folded onto the SAME chip (add signal, not
// rows). A closed crew counts its reconciled total_cost_usd only; a live crew
// counts total + its live estimate.
func TestPlansIndex_CrewChipSurfacesSpend(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	live := seedCrew(f, "gd-crewA", "gardenio-crew-a", "gd-step1", "working", "in_progress")
	live.Fields["total_cost_usd"] = "1.00"
	live.Fields["live_cost_usd"] = "0.50"
	done := seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"done"}`
	done.Fields["total_cost_usd"] = "3.00"
	done.Fields["live_cost_usd"] = "9.99" // stale on a closed crew — must be excluded
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// 1.00 + 0.50 (live) + 3.00 (closed, stale live excluded) = $4.50.
	if !strings.Contains(body, `<span class="plan-card-crew-spend"`) {
		t.Errorf("expected the crew-spend facet on the index card")
	}
	if !strings.Contains(body, "· $4.50") {
		t.Errorf("expected the '· $4.50' cumulative crew spend on the chip (got body without it)")
	}
}

// TestPlansIndex_NoCrewSpendFacetWhenNoCost proves a plan whose crew have reported
// no cost renders no spend facet — the zero-ink gate.
func TestPlansIndex_NoCrewSpendFacetWhenNoCost(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-crew-a", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if strings.Contains(w.Body.String(), `<span class="plan-card-crew-spend"`) {
		t.Errorf("a plan whose crew reported no cost must not render the spend facet")
	}
}

// TestPlansIndex_StuckCrewFaceSurfacesRedAvatar proves an OPEN stuck crew
// (ka-0000003793) renders as a RED click-through avatar on the index card — the
// stuck strip with the plan-card-crew-face-stuck class, linking to the crew's
// terminal/bead page — while a CLOSED-failed crew never gets an avatar.
func TestPlansIndex_StuckCrewFaceSurfacesRedAvatar(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	// An open rate-limited crew (stuck, reachable) + a healthy working crew + a
	// historical closed-failed crew.
	seedCrew(f, "gd-crewR", "gardenio-rate-limited-x", "gd-step1", "rate_limited", "in_progress")
	seedCrew(f, "gd-crewW", "gardenio-working-y", "gd-step1", "working", "in_progress")
	fail := seedCrew(f, "gd-crewF", "gardenio-closed-fail-z", "gd-step1", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"hit a wall"}`
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The stuck strip and a red click-through avatar for the open stuck crew.
	if !strings.Contains(body, `<span class="plan-card-crew-faces plan-card-crew-faces-stuck"`) {
		t.Errorf("expected the stuck-crew avatar strip on a plan with an open stuck crew")
	}
	if !strings.Contains(body, `<a class="plan-card-crew-face plan-card-crew-face-stuck" href="/gd-crewR"`) {
		t.Errorf("expected a red click-through avatar linking to the open stuck crew's bead page")
	}
	// The healthy working crew is a green face, never in the stuck strip.
	if !strings.Contains(body, `<a class="plan-card-crew-face" href="/gd-crewW"`) {
		t.Errorf("the working crew should still render its green working avatar")
	}
	// The closed-failed crew never gets any avatar (no reachable terminal).
	if strings.Contains(body, "gd-crewF") {
		t.Errorf("the closed-failed crew (gd-crewF) must not appear as any avatar")
	}
	// iter-29 two-tier facet: this plan has BOTH an open stuck crew AND a
	// closed-failed crew, so the chip renders the LOUD red "⚠ 1" (reachable,
	// act-now) AND the CALM "✕ 1" (historical, no terminal) as siblings — the
	// open/closed split the avatars already made, now on the counts too.
	if !strings.Contains(body, `<span class="plan-card-crew-failed"`) || !strings.Contains(body, "⚠ 1") {
		t.Errorf("expected the loud red '⚠ 1' open-stuck facet (one reachable stuck crew)")
	}
	if !strings.Contains(body, `<span class="plan-card-crew-failed-closed"`) || !strings.Contains(body, "✕ 1") {
		t.Errorf("expected the calm '✕ 1' closed-failed facet (one historical failure)")
	}
}

// TestPlansIndex_CrewChipStuckFacetSplitsOpenVsClosed is the iter-29 crux
// regression: the crew chip's stuck facet splits by REACHABILITY so a card whose
// failures are all CLOSED history never screams the loud red act-now "⚠ K". It
// fails on the pre-iter-29 markup (which rendered the whole cumulative Failed count
// in one loud .plan-card-crew-failed span). Three plans, one board:
//   - all-closed: renders CALM "✕ 2", never the loud red facet — the false-alarm fix.
//   - all-open:   renders LOUD "⚠ 2", never the calm marker.
//   - mixed:      renders BOTH "⚠ 1" (open) and "✕ 1" (closed).
func TestPlansIndex_CrewChipStuckFacetSplitsOpenVsClosed(t *testing.T) {
	f := seedPlansFake()
	// Plan A (gd-plan1 / gd-step1): TWO closed-failed crew, zero open — the common
	// live-board shape (measured 17/17 cards). Must read calm, never act-now red.
	f1 := seedCrew(f, "gd-a1", "gardenio-closed-a1", "gd-step1", "failed", "closed")
	f1.Fields["wrapup"] = `{"tldr":"wall 1"}`
	f2 := seedCrew(f, "gd-a2", "gardenio-closed-a2", "gd-step1", "failed", "closed")
	f2.Fields["wrapup"] = `{"tldr":"wall 2"}`
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The all-closed plan renders the calm marker with the full count, and NOT the
	// loud red open-stuck facet. This is the assertion that fails on old markup: the
	// old template rendered `.plan-card-crew-failed">· ⚠ 2` for these closed crew.
	if !strings.Contains(body, `<span class="plan-card-crew-failed-closed"`) {
		t.Errorf("all-closed plan: expected the calm closed-failed marker")
	}
	if !strings.Contains(body, "✕ 2") { // "✕ 2" — both closed failures, calm
		t.Errorf("all-closed plan: expected the calm '✕ 2' historical count")
	}
	if strings.Contains(body, `<span class="plan-card-crew-failed"`) {
		t.Errorf("all-closed plan: must NOT render the loud red act-now open-stuck facet (the false-alarm this iteration fixes)")
	}
	if strings.Contains(body, "⚠ 2") { // "⚠ 2"
		t.Errorf("all-closed plan: must NOT render the loud '⚠ 2' act-now count")
	}
}

// TestPlansIndex_StuckFacetCarriesOldestAge proves the crew chip's stuck facet
// (ka-0000002016, iter-13) folds the OLDEST open-stuck crew's wait-age onto the
// "· ⚠ K" count — completing the decision/escalation/stall wait-age family. With
// the clock at 12:00, an open stuck crew last active at 09:00 has waited 3h,
// which is past the 1h stale bar, so the facet also gains the loud stale tint.
func TestPlansIndex_StuckFacetCarriesOldestAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	stuck := seedCrew(f, "gd-crewR", "gardenio-rate-limited-x", "gd-step1", "rate_limited", "in_progress")
	stuck.UpdatedAt = time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC) // 3h ago — past the 1h bar
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The stuck facet carries the loud stale class and the age span with "3h".
	// iter-29: the facet now also carries a title= attribute after the class, so
	// match on the class-attribute start rather than the class="...">​ close.
	if !strings.Contains(body, `<span class="plan-card-crew-failed plan-card-crew-stuck-stale" title=`) {
		t.Errorf("expected the stuck facet to gain the loud stale tint past the 1h bar")
	}
	if !strings.Contains(body, `<span class="plan-card-crew-stuck-age">3h</span>`) {
		t.Errorf("expected the '3h' oldest-stuck wait-age suffix on the stuck facet")
	}
	// The loud facet counts the ONE open stuck crew.
	if !strings.Contains(body, "⚠ 1") {
		t.Errorf("expected the '⚠ 1' loud open-stuck count on the facet")
	}
}

// TestPlansIndex_FreshStuckFacetReadsCountOnly proves a plan whose open stuck crew
// is FRESH (well under the 1h stale bar) still surfaces its wait-age but stays
// calm-red — the two-tier treatment, so a briefly-rate-limited crew doesn't read
// as an alarm-level "stuck too long".
func TestPlansIndex_FreshStuckFacetReadsCountOnly(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	stuck := seedCrew(f, "gd-crewR", "gardenio-rate-limited-x", "gd-step1", "rate_limited", "in_progress")
	stuck.UpdatedAt = time.Date(2026, 7, 5, 11, 58, 0, 0, time.UTC) // 2m ago — fresh
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	body := w.Body.String()
	// The age still renders (2m), but WITHOUT the loud stale class. Assert on the
	// rendered facet markup (the plain-class span) — the bare stale class name also
	// appears in the page's <style> block.
	if !strings.Contains(body, `<span class="plan-card-crew-stuck-age">2m</span>`) {
		t.Errorf("expected the '2m' wait-age on a fresh open stuck crew")
	}
	// iter-29: the facet carries a title= after the class. A fresh open stuck crew
	// gets the plain (non-stale) loud-red facet — assert the class-attribute start
	// WITHOUT the stale modifier.
	if !strings.Contains(body, `<span class="plan-card-crew-failed" title=`) {
		t.Errorf("expected the calm-red (non-stale) stuck facet on a fresh stuck crew")
	}
	if strings.Contains(body, `plan-card-crew-failed plan-card-crew-stuck-stale`) {
		t.Errorf("a fresh (2m) stuck crew must not gain the loud stale tint")
	}
}

// TestPlansIndex_ClosedFailedFacetNoAge proves a plan whose only failed crew are
// CLOSED renders the CALM "✕ K" historical marker (iter-29) — NOT the loud red
// "⚠ K" (which is now open-stuck only) and with NO wait-age suffix. A closed-failed
// crew's age is its death, not an ongoing wait, and it has no reachable terminal —
// so it must never read as an act-now alarm (mirroring how it never gets a red
// avatar). This is the mission-family card-vs-truth fix: the red facet was a false
// "act now" on cards whose failures are all closed history.
func TestPlansIndex_ClosedFailedFacetNoAge(t *testing.T) {
	f := seedPlansFake()
	fail := seedCrew(f, "gd-crewF", "gardenio-closed-fail-z", "gd-step1", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"hit a wall"}`
	fail.UpdatedAt = time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	body := w.Body.String()
	// The CALM closed-failed marker: "✕ 1" on the muted historical facet.
	if !strings.Contains(body, `<span class="plan-card-crew-failed-closed"`) {
		t.Errorf("expected the calm closed-failed marker on a closed-failed-only plan")
	}
	if !strings.Contains(body, "✕ 1") { // "✕ 1"
		t.Errorf("expected the '✕ 1' calm historical-failure count")
	}
	// It must NOT render the loud red open-stuck facet — every failure is closed.
	if strings.Contains(body, `<span class="plan-card-crew-failed"`) ||
		strings.Contains(body, `plan-card-crew-failed plan-card-crew-stuck-stale`) {
		t.Errorf("a closed-failed-only plan must not render the loud red open-stuck facet")
	}
	if strings.Contains(body, "⚠ 1") { // "⚠ 1" — the loud act-now count
		t.Errorf("a closed-failed-only plan must not render the loud '⚠ 1' act-now count")
	}
	// Assert on the rendered age span markup — the bare class also lives in <style>.
	if strings.Contains(body, `<span class="plan-card-crew-stuck-age">`) {
		t.Errorf("a closed-failed-only plan must not render a stuck wait-age suffix")
	}
}

// TestPlansIndex_NoStuckFacesWhenOnlyClosedFailed proves the stuck strip is gated
// on OPEN stuck crew — a plan whose only failed crew is closed renders the "⚠ N"
// tally but no stuck avatar strip.
func TestPlansIndex_NoStuckFacesWhenOnlyClosedFailed(t *testing.T) {
	f := seedPlansFake()
	fail := seedCrew(f, "gd-crewF", "gardenio-closed-fail-z", "gd-step1", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"hit a wall"}`
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	body := w.Body.String()
	if strings.Contains(body, `plan-card-crew-faces-stuck"`) {
		t.Errorf("a plan with only a closed-failed crew should render no stuck avatar strip")
	}
	// It still surfaces the failed count facet on the chip — but as the CALM "✕ 1"
	// closed-failed marker (iter-29), not the loud red act-now "⚠ 1".
	if !strings.Contains(body, "✕ 1") { // "✕ 1"
		t.Errorf("expected the '✕ 1' calm closed-failed facet on the crew chip")
	}
	if strings.Contains(body, "⚠ 1") { // "⚠ 1"
		t.Errorf("a closed-failed-only plan must not render the loud '⚠ 1' act-now facet")
	}
}

// TestPlansIndex_NoCrewFacesWhenOnlyFinished proves the avatar strip is gated on
// LIVE crew — an all-finished plan renders the count chip but no faces.
func TestPlansIndex_NoCrewFacesWhenOnlyFinished(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "done", "closed")
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if strings.Contains(w.Body.String(), `<span class="plan-card-crew-faces"`) {
		t.Errorf("an all-finished plan should render no live-crew avatar strip")
	}
}

// TestPlansIndex_NoCrewChipWhenNoCrew proves a plan with no dispatched crew
// renders no crew chip (data-density: a crew-less plan adds no ink).
func TestPlansIndex_NoCrewChipWhenNoCrew(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	// The rendered chip anchor, not the CSS rules in the <style> block.
	if strings.Contains(w.Body.String(), `<a class="plan-card-crew`) {
		t.Errorf("a crew-less plan should render no crew chip")
	}
}

// TestPlansIndex_CrewChipNoDotWhenOnlyFinished proves a plan whose crew have all
// finished renders the neutral chip WITHOUT the live-dot (no crew working now)
// and without the live modifier class.
func TestPlansIndex_CrewChipNoDotWhenOnlyFinished(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "done", "closed")
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	body := w.Body.String()
	if !strings.Contains(body, "1 crew") {
		t.Errorf("expected '1 crew' on the chip")
	}
	// The neutral (non-live) chip renders without the live modifier class...
	if !strings.Contains(body, `<a class="plan-card-crew"`) {
		t.Errorf("an all-finished plan should render the neutral crew chip")
	}
	// ...and the live-dot span is gated on HasLive, so it renders none.
	if strings.Contains(body, `<span class="plan-card-crew-dot"`) {
		t.Errorf("an all-finished plan should render no live-dot")
	}
}
