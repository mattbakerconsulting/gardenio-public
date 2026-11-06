package main

// plans_creator_chip_test.go — iter-61 (ka-0000005352): suppress the redundant
// per-card `creator` chip on a /plans board already SCOPED to that operator.
//
// THE MEASURE (measure-first): the /plans index is default-scoped to the
// viewer's OWN operator id (S1 "filter-by-me by default" — handlePlans applies
// resolveDefaultOperatedBy when the URL carries no operated_by key), and a
// ?operated_by=X link scopes it to one operator. On such a board EVERY rendered
// card shares that operator, the header chip already reads "Showing your N
// plans" / "Showing work operated by X", AND this per-card creator chip's own
// filter href (/plans?operated_by=X) points right back at the same scoped page —
// a dead-end self-link. So on a scoped board the creator chip is pure redundant
// ink.
//
// THE FIX: the project-chip (iter-60) "render only where the value
// discriminates" discipline, applied to the creator chip — planCreatorChipScoped
// returns "" when the card's own resolved operator RawID equals the board's
// active scope ($root.EffectiveOperatedBy). "More info in LESS space": every card
// reclaims the chip's width from the meta row, with zero info loss (the scope
// operator is named once in the header chip). The scope is available in EVERY
// render path so a swapped-in row stays byte-consistent with its neighbours: the
// page render reads it off the page dot, and the single-row + bulk-OOB re-renders
// recover it from the Referer (refererPlansOperatedBy, with the SAME me-expansion
// + cold-load-default precedence handlePlans uses).

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/oidcauth"
)

// creatorChipRe matches the per-card creator span an unsuppressed card renders.
var creatorChipRe = regexp.MustCompile(`<span class="plan-card-creator">`)

// seedPlansFakeOperatedBy is seedPlansFake with the plan epic stamped with an
// explicit operated_by operator id, so the creator chip resolves to a concrete
// RawID the board scope can (or cannot) match. The fakeDaemon does not itself
// filter hotEpicsResp by operated_by, so the seed plan renders on any scope —
// exactly what a suppression A/B needs.
func seedPlansFakeOperatedBy(operatedBy string) *fakeDaemon {
	f := seedPlansFake()
	p := f.beads["gd-plan1"]
	p.OperatedBy = operatedBy
	p.OperatedByEmail = strings.TrimPrefix(operatedBy, "oidc:")
	// hotEpicsResp holds the SAME pointer, so the stamp is visible on the index
	// render; assert it to guard against a future seed refactor that copies.
	return f
}

// --- unit: refererPlansOperatedBy over synthetic Referers ---------------------

func TestRefererPlansOperatedBy(t *testing.T) {
	s := &Server{basePath: ""}
	const viewer = "matt@acme.example"
	cases := []struct {
		name    string
		referer string
		want    string
	}{
		// No operated_by key on a /plans referer → the cold-load default scopes
		// to the viewer's canonical oidc id (the "filter-by-me" default).
		{"unscoped plans page defaults to viewer", "http://viewer/plans", "oidc:matt@acme.example"},
		{"plans page with sibling filters only", "http://viewer/plans?project=gardenio&status=open", "oidc:matt@acme.example"},
		// An explicit ?operated_by= (present key, empty value) is "show all" and
		// WINS over the default — no scope.
		{"explicit all clears scope", "http://viewer/plans?operated_by=", ""},
		// An explicit operator id scopes to it verbatim.
		{"explicit operator id", "http://viewer/plans?operated_by=oidc:peer@acme.example", "oidc:peer@acme.example"},
		// "me" expands to the viewer's canonical id.
		{"explicit me expands to viewer", "http://viewer/plans?operated_by=me", "oidc:matt@acme.example"},
		// null/(none) is the unowned filter, not an operator scope → "".
		{"unowned filter is not a scope", "http://viewer/plans?operated_by=null", ""},
		// A non-/plans referer, or no referer, is ignored (safe unscoped default).
		{"non-plans referer ignored", "http://viewer/deck?operated_by=oidc:peer@acme.example", ""},
		{"cockpit referer ignored", "http://viewer/plans/gd-plan1?operated_by=me", ""},
		{"no referer", "", ""},
		{"garbage referer", "://not a url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := plansTogglePostWithViewer("/plans/gd-plan1/gardener/pause-toggle", tc.referer, viewer)
			if got := s.refererPlansOperatedBy(r); got != tc.want {
				t.Errorf("refererPlansOperatedBy(referer=%q) = %q, want %q", tc.referer, got, tc.want)
			}
		})
	}
}

// TestRefererPlansOperatedBy_HonoursBasePath proves the path match is anchored on
// the server's basePath, mirroring refererPlansProject.
func TestRefererPlansOperatedBy_HonoursBasePath(t *testing.T) {
	s := &Server{basePath: "/beads"}
	r := plansTogglePostWithViewer("/beads/plans/gd-plan1/gardener/pause-toggle",
		"http://viewer/beads/plans?operated_by=oidc:peer@acme.example", "matt@acme.example")
	if got := s.refererPlansOperatedBy(r); got != "oidc:peer@acme.example" {
		t.Errorf("basePath /beads: got %q, want oidc:peer@acme.example", got)
	}
	r = plansTogglePostWithViewer("/beads/plans/gd-plan1/gardener/pause-toggle",
		"http://viewer/plans?operated_by=oidc:peer@acme.example", "matt@acme.example") // wrong mount
	if got := s.refererPlansOperatedBy(r); got != "" {
		t.Errorf("bare /plans under a /beads mount should not match; got %q", got)
	}
}

// TestRefererPlansOperatedBy_AnonymousNoDefault proves an anonymous re-render
// (no viewer email) does NOT synthesize a scope from an unscoped /plans referer —
// there is no "mine" to default to, so the chip renders as on an "all" board.
func TestRefererPlansOperatedBy_AnonymousNoDefault(t *testing.T) {
	s := &Server{basePath: ""}
	r := plansTogglePostWithViewer("/plans/gd-plan1/gardener/pause-toggle", "http://viewer/plans", "")
	if got := s.refererPlansOperatedBy(r); got != "" {
		t.Errorf("anonymous re-render from an unscoped board must not default a scope; got %q", got)
	}
}

// --- unit: planCreatorChipScoped suppression truth-table ----------------------

func TestPlanCreatorChipScoped(t *testing.T) {
	const viewer = "matt@acme.example"
	mine := planRowView{ID: "gd-p", OperatedBy: "oidc:matt@acme.example", OperatedByEmail: "matt@acme.example"}
	peer := planRowView{ID: "gd-p", OperatedBy: "oidc:peer@acme.example", OperatedByEmail: "peer@acme.example"}
	cases := []struct {
		name     string
		row      planRowView
		scope    string
		wantChip bool // true = chip rendered, false = suppressed ("")
	}{
		// The headline: a card whose operator matches the board scope drops the chip.
		{"match → suppressed", mine, "oidc:matt@acme.example", false},
		// Case-insensitive: an operator id differing only in header casing matches.
		{"case-insensitive match → suppressed", mine, "OIDC:Matt@acme.example", false},
		// A card whose operator differs from the scope keeps its discriminating chip.
		{"different operator → kept", peer, "oidc:matt@acme.example", true},
		// An unscoped ("all") board renders every chip — the creator discriminates.
		{"empty scope → kept", mine, "", true},
		{"peer on empty scope → kept", peer, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(planCreatorChipScoped(tc.row, "", viewer, tc.scope))
			if tc.wantChip && got == "" {
				t.Fatalf("expected a chip, got empty (scope=%q)", tc.scope)
			}
			if !tc.wantChip && got != "" {
				t.Fatalf("expected suppression (empty), got %q (scope=%q)", got, tc.scope)
			}
		})
	}
}

// TestPlanCreatorChipScoped_UnattributedByteIdentical proves an unattributed card
// (no operated_by, no created_by → Kind "empty", RawID "") is byte-identical
// across every scope — the RawID != "" guard means the new suppression branch can
// never fire for it, so it emits exactly the same output the unscoped
// planCreatorChip always did (the compact renderer returns "" for Kind "empty",
// which the template's {{if $creator}} guard already drops). The additive-guard
// floor: a card the chip never rendered for is untouched by iter-61.
func TestPlanCreatorChipScoped_UnattributedByteIdentical(t *testing.T) {
	row := planRowView{ID: "gd-p"} // no operator, no creator → Kind "empty", RawID ""
	base := string(planCreatorChip(row, "", "matt@acme.example"))
	for _, scope := range []string{"", "oidc:matt@acme.example", "oidc:peer@acme.example"} {
		got := string(planCreatorChipScoped(row, "", "matt@acme.example", scope))
		if got != base {
			t.Fatalf("unattributed card must be byte-identical across scopes; scope=%q got %q, base %q", scope, got, base)
		}
	}
}

// --- page render: chip present when unscoped/other, suppressed when scoped -----

// TestPlansIndex_CreatorChipSuppressedWhenScopedToViewer is the headline: on the
// default board (auto-scoped to the viewer), a card whose operator IS the viewer
// drops the redundant creator chip — the header already reads "Showing your N
// plans", and the chip link would only point back at the same scoped page.
func TestPlansIndex_CreatorChipSuppressedWhenScopedToViewer(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:matt@acme.example")
	card := planCardFor(t, f, "/plans") // no operated_by key → default-scoped to viewer
	if creatorChipRe.MatchString(card) {
		t.Fatalf("a board auto-scoped to the viewer should suppress the viewer's own creator chip:\n%s", card)
	}
	// The plan itself must still render — suppression removes ONLY the chip.
	if !strings.Contains(card, `data-plan-id="gd-plan1"`) {
		t.Fatalf("the plan card must still render, only the chip drops:\n%s", card)
	}
}

// TestPlansIndex_CreatorChipPresentOnAllBoard is the floor: an explicit
// ?operated_by= ("everyone's plans") board has NO active operator scope, so the
// creator chip renders — there it genuinely discriminates whose plan each card is.
func TestPlansIndex_CreatorChipPresentOnAllBoard(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:matt@acme.example")
	card := planCardFor(t, f, "/plans?operated_by=") // explicit all → no scope
	if !creatorChipRe.MatchString(card) {
		t.Fatalf("an unscoped 'all' board should render the discriminating creator chip:\n%s", card)
	}
}

// TestPlansIndex_CreatorChipPresentForPeerUnderScope is the defensive guard: if a
// board is scoped to one operator but a card names a DIFFERENT operator (a shape a
// scoped list query never actually produces), the card's still-discriminating chip
// renders. Proves the suppression keys on equality, not on "any scope present".
func TestPlansIndex_CreatorChipPresentForPeerUnderScope(t *testing.T) {
	// The card's operator is a peer; the board is explicitly scoped to that peer's
	// id so EffectiveOperatedBy is set, but the seed plan's operator is the viewer
	// (matt) — mismatch → chip kept.
	f := seedPlansFakeOperatedBy("oidc:matt@acme.example")
	card := planCardFor(t, f, "/plans?operated_by=oidc:peer@acme.example")
	if !creatorChipRe.MatchString(card) {
		t.Fatalf("a card whose operator differs from the active scope keeps its discriminating chip:\n%s", card)
	}
}

// --- re-render consistency: a toggled row matches its unswapped neighbours -----

// TestPlanRowToggle_SuppressesCreatorChipFromReferer proves the single-row
// re-render (a gardener pause-toggle) recovers the board's operator scope off the
// Referer (with the cold-load default) and suppresses the creator chip too — so a
// swapped-in row on a scoped board stays byte-consistent with the neighbours the
// page render already stripped.
func TestPlanRowToggle_SuppressesCreatorChipFromReferer(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:matt@acme.example")
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	// The toggle is issued from the DEFAULT /plans board (no operated_by key), so
	// the Referer recovers the viewer's own scope — matching the plan's operator.
	req := plansTogglePostWithViewer("/plans/gd-plan1/gardener/pause-toggle", "http://viewer/plans", "matt@acme.example")
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
		t.Fatalf("a row re-rendered onto a viewer-scoped board should drop the creator chip; body=%s", snippetN(body, 800))
	}
}

// TestPlanRowToggle_KeepsCreatorChipAllReferer is the symmetric floor: a toggle
// issued from an explicit ?operated_by= ("all") page re-renders the row WITH the
// chip, matching the unscoped neighbours — the Referer-derived scope is empty.
func TestPlanRowToggle_KeepsCreatorChipAllReferer(t *testing.T) {
	f := seedPlansFakeOperatedBy("oidc:matt@acme.example")
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := plansTogglePostWithViewer("/plans/gd-plan1/gardener/pause-toggle", "http://viewer/plans?operated_by=", "matt@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if !creatorChipRe.MatchString(w.Body.String()) {
		t.Fatalf("a row re-rendered onto an 'all' board should keep the creator chip; body=%s", snippetN(w.Body.String(), 800))
	}
}

// plansTogglePostWithViewer builds an operator POST to target with the given
// Referer and an authenticated viewer email (so resolveDefaultOperatedBy's
// cold-load default resolves), mirroring operatorPost's operator headers.
func plansTogglePostWithViewer(target, referer, viewerEmail string) *http.Request {
	r := operatorPost(target, "")
	if referer != "" {
		r.Header.Set("Referer", referer)
	}
	if viewerEmail != "" {
		r.Header.Set("X-Auth-Request-Email", viewerEmail)
		// refererPlansOperatedBy reads the viewer via oidcauth.EmailFromContext
		// (the SAME source handlePlans uses), so inject it into the context too —
		// the served mux's auth middleware does this from the header, but a direct
		// unit call bypasses it.
		r = r.WithContext(oidcauth.ContextWithEmail(r.Context(), viewerEmail))
	}
	return r
}
