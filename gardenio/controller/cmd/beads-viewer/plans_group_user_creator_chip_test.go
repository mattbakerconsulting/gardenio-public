package main

// plans_group_user_creator_chip_test.go — iter-63 (ka-0000005352): suppress the
// redundant per-card `creator` chip on a ?group=user /plans board — the group
// twin of iter-61's ?operated_by= SCOPE suppression, and the 4th corner of the
// redundant-ink family's 2×2 (project + creator) × (filter/scope + group):
//
//	iter-60  project chip · ?project=X FILTER
//	iter-61  creator chip · ?operated_by=X SCOPE
//	iter-62  project chip · ?group=project
//	iter-63  creator chip · ?group=user   ← here
//
// THE MEASURE: a ?group=user board sections its cards into one <h2> heading per
// operator (groupPlanRows / planUserGroupKey), and each section heading already
// names the operator. So the per-card "by X" creator chip is the SAME redundant
// ink AND dead-end self-link (its href=/plans?operated_by=X points back at a
// scope of the operator the section already names) the ?operated_by= scope case
// is, just reached via grouping instead of scoping.
//
// THE FIX: extend the "render only where the value discriminates" guard to also
// drop the chip when $root.GroupKey == "user". The group key is URL-derivable in
// EVERY render path (page dot $root.GroupKey; single-row + bulk-OOB re-renders
// read it from the Referer via refererPlansGroup — plumbing iter-62 already
// landed), so a swapped-in card stays byte-consistent with its neighbours.
//
// The A/B deliberately uses a PEER operator (not the viewer): on a bare
// ?group=user board the effective scope is the viewer's own cold-load default,
// so a peer card is NOT scope-suppressed by iter-61 (operator ≠ scope) — which
// isolates the group suppression as the sole reason the chip drops.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- page render: chip suppressed grouped-by-user, kept otherwise --------------

// TestPlansIndex_CreatorChipSuppressedWhenGroupedByUser is the headline: on a
// ?group=user board every section's <h2> already names the operator, so the
// redundant per-card creator chip is gone — even for a PEER card iter-61's scope
// suppression would keep.
func TestPlansIndex_CreatorChipSuppressedWhenGroupedByUser(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:peer@acme.example")
	card := planCardFor(t, f, "/plans?group=user")
	if creatorChipRe.MatchString(card) {
		t.Fatalf("a ?group=user board should suppress the redundant creator chip:\n%s", card)
	}
	// The card itself must still render — suppression removes ONLY the chip.
	if !strings.Contains(card, `data-plan-id="gd-plan1"`) {
		t.Fatalf("the plan card must still render, only the chip drops:\n%s", card)
	}
	// The grouping heading must still name the operator (that is what makes the
	// per-card chip redundant in the first place).
	mux := mkServerWithNow(t, seedPlansFakeOperatedBy("oidc:peer@acme.example"), func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?group=user", "matt@acme.example"))
	if !strings.Contains(w.Body.String(), "plan-group-head") {
		t.Fatalf("a ?group=user board should render group headings:\n%s", snippetN(w.Body.String(), 800))
	}
}

// TestPlansIndex_CreatorChipPresentWhenGroupedByProject is the discriminating
// floor: a ?group=project board sections by project, NOT operator — so within a
// project section a peer card's creator label still discriminates and the chip
// must render (proves the suppression keys on group=="user", not on "any group
// present").
func TestPlansIndex_CreatorChipPresentWhenGroupedByProject(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:peer@acme.example")
	card := planCardFor(t, f, "/plans?group=project")
	if !creatorChipRe.MatchString(card) {
		t.Fatalf("a ?group=project board keeps the discriminating creator chip:\n%s", card)
	}
}

// TestPlansIndex_CreatorChipPresentWhenGroupedByStatus is the second
// discriminating floor: a ?group=status board sections by status, so the
// operator label still discriminates within a status section and the chip
// renders.
func TestPlansIndex_CreatorChipPresentWhenGroupedByStatus(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:peer@acme.example")
	card := planCardFor(t, f, "/plans?group=status")
	if !creatorChipRe.MatchString(card) {
		t.Fatalf("a ?group=status board keeps the discriminating creator chip:\n%s", card)
	}
}

// TestPlansIndex_CreatorChipPresentUngroupedPeer is the ungrouped floor: on a
// plain /plans board (no ?group=) a peer card whose operator differs from the
// viewer's default scope renders the creator chip exactly as before — the
// suppression is group-scoped, so it never bleeds onto the ungrouped board.
func TestPlansIndex_CreatorChipPresentUngroupedPeer(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:peer@acme.example")
	card := planCardFor(t, f, "/plans?operated_by=")
	if !creatorChipRe.MatchString(card) {
		t.Fatalf("an ungrouped board keeps a peer card's discriminating creator chip:\n%s", card)
	}
}

// --- re-render consistency: a toggled row matches its unswapped neighbours -----

// TestPlanRowToggle_SuppressesCreatorChipFromGroupUserReferer proves the
// single-row re-render (a gardener pause-toggle) reads the active ?group= key off
// the Referer and suppresses the creator chip on a ?group=user board too — so a
// swapped-in row stays byte-consistent with the neighbours the page render
// already stripped, instead of re-growing a chip they dropped.
func TestPlanRowToggle_SuppressesCreatorChipFromGroupUserReferer(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:peer@acme.example")
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := operatorPost("/plans/gd-plan1/gardener/pause-toggle", "")
	req.Header.Set("Referer", "http://viewer/plans?group=user")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-plan-id="gd-plan1"`) {
		t.Fatalf("response should be the re-rendered plan row; body=%s", snippetN(body, 800))
	}
	if creatorChipRe.MatchString(body) {
		t.Fatalf("a row re-rendered onto a ?group=user board should drop the creator chip; body=%s", snippetN(body, 800))
	}
}

// TestPlanRowToggle_KeepsCreatorChipGroupProjectReferer is the symmetric floor: a
// toggle issued from a ?group=project page re-renders the peer row WITH the chip
// (the group is "project", not "user"), matching the grouped-by-project
// neighbours — proving the row re-render keys the suppression on the Referer's
// group VALUE, not on "any group present".
func TestPlanRowToggle_KeepsCreatorChipGroupProjectReferer(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:peer@acme.example")
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := operatorPost("/plans/gd-plan1/gardener/pause-toggle", "")
	req.Header.Set("Referer", "http://viewer/plans?group=project")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if !creatorChipRe.MatchString(w.Body.String()) {
		t.Fatalf("a row re-rendered onto a ?group=project board should keep the creator chip; body=%s", snippetN(w.Body.String(), 800))
	}
}
