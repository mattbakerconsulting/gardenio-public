package main

// plans_crew_avatar_iter35_test.go — iter-35 (ka-0000005352 / gd-NEml6AWxrW):
// the /plans index live-crew avatar (plan-card-crew-face) DROPS the redundant
// leading project token before computing its initials.
//
// THE GAP (measured on 299 live agent beads): every gardenio crew agent's name
// begins with its project ("gardenio-…"), and MANY double it
// ("gardenio-gardenio-crew-…"). actorInitials took the first rune of each of
// the first four segments, so the avatar's leading 1–2 glyphs — the eye's first
// landing point — were a CONSTANT carrying zero disambiguating signal (the
// project is already the card's own project chip). Two crew on one plan read
// "GGGD" vs "GGCB" (first two glyphs identical); after the strip they read
// "CDDR" / "CBD8", every glyph now disambiguating.
//
// The strip is per-crew and daemon-free (project = projectFromLabels(b.Labels),
// off the crew bead planCrewBulk already hydrated). A crew with no resolvable
// project passes through verbatim, so the non-gardenio / unlabeled case reads
// exactly as before.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStripLeadingProjectSegments is the crux: the pure string surgery.
func TestStripLeadingProjectSegments(t *testing.T) {
	cases := []struct {
		name    string
		project string
		want    string
	}{
		// The doubled-token case the live board is full of — BOTH leading
		// "gardenio" segments go, so the collision-prone "GG…" prefix vanishes.
		{"gardenio-gardenio-crew-bugfix-dispatch-8jyv", "gardenio", "crew-bugfix-dispatch-8jyv"},
		// The single-token case — one leading "gardenio" goes.
		{"gardenio-paint-grid-xyz", "gardenio", "paint-grid-xyz"},
		{"gardenio-continual-datadensity-improvement-gj2", "gardenio", "continual-datadensity-improvement-gj2"},
		// A different project token strips its own prefix.
		{"monorepo-monorepo-wedge-watch-merge-train-kjtp", "monorepo", "wedge-watch-merge-train-kjtp"},
		// No leading project segment — untouched (the token appears mid-name,
		// not as the leading segment).
		{"mr-2261-gardenio-fix", "gardenio", "mr-2261-gardenio-fix"},
		// A segment that merely CONTAINS the project (not equal) is not stripped.
		{"gardenioy-thing-abc", "gardenio", "gardenioy-thing-abc"},
		// Empty project → verbatim pass-through (the unlabeled / non-gardenio case).
		{"gardenio-paint-grid-xyz", "", "gardenio-paint-grid-xyz"},
		// Pathological: the whole name IS the project token(s) — keep the
		// original so the avatar never collapses to nothing.
		{"gardenio", "gardenio", "gardenio"},
		{"gardenio-gardenio", "gardenio", "gardenio-gardenio"},
		// Empty name → empty.
		{"", "gardenio", ""},
	}
	for _, tc := range cases {
		if got := stripLeadingProjectSegments(tc.name, tc.project); got != tc.want {
			t.Errorf("stripLeadingProjectSegments(%q, %q) = %q; want %q", tc.name, tc.project, got, tc.want)
		}
	}
}

// TestCrewAvatarInitials proves the composed helper: strip THEN initial.
func TestCrewAvatarInitials(t *testing.T) {
	cases := []struct {
		name    string
		project string
		want    string
	}{
		// The measured collision pair — distinct after the strip, identical GG… before.
		{"gardenio-gardenio-crew-delivery-drive-r-6av3", "gardenio", "CDDR"},
		{"gardenio-gardenio-crew-bugfix-dispatch-8jyv", "gardenio", "CBD8"},
		// Single-token name.
		{"gardenio-paint-grid-xyz", "gardenio", "PGX"},
		// No project label → unchanged from raw actorInitials (leading G kept).
		{"gardenio-paint-grid-xyz", "", "GPGX"},
		// Non-gardenio project.
		{"monorepo-monorepo-wedge-watch-merge-train-kjtp", "monorepo", "WWMT"},
		// The whole name is the project token — keep original, initial it as-is.
		{"gardenio", "gardenio", "GA"},
	}
	for _, tc := range cases {
		if got := crewAvatarInitials(tc.name, tc.project); got != tc.want {
			t.Errorf("crewAvatarInitials(%q, %q) = %q; want %q", tc.name, tc.project, got, tc.want)
		}
	}
}

// TestCrewAvatarInitials_DisambiguatesCollision is the headline assertion: two
// crew whose RAW initials collide on the shared project prefix become distinct
// after the strip. This is the whole point of the iteration.
func TestCrewAvatarInitials_DisambiguatesCollision(t *testing.T) {
	a := "gardenio-gardenio-crew-delivery-drive-r-6av3"
	b := "gardenio-gardenio-crew-bugfix-dispatch-8jyv"
	// Precondition: raw initials collide (both "GGGD"/"GGCB" share the GG prefix,
	// and the delivery/bugfix distinction only shows at glyph 4).
	if actorInitials(a)[:2] != actorInitials(b)[:2] {
		t.Fatalf("precondition failed: expected raw initials to share a 2-glyph prefix, got %q vs %q", actorInitials(a), actorInitials(b))
	}
	ia := crewAvatarInitials(a, "gardenio")
	ib := crewAvatarInitials(b, "gardenio")
	// After the strip both drop the constant "GG…" prefix and read their
	// distinctive tail — they still differ (CDDR vs CBD8), and crucially neither
	// wastes its leading glyph on the fleet-wide constant project letter G, so
	// the difference now surfaces two glyphs earlier than the raw "GGGD/GGCB".
	if ia == ib {
		t.Fatalf("avatars still collide after strip: %q == %q", ia, ib)
	}
	if ia[0] == 'G' || ib[0] == 'G' {
		t.Errorf("stripped avatars still lead with the constant project glyph G: %q / %q", ia, ib)
	}
}

// TestPlanCrewBulk_FaceInitialsStripProjectPrefix proves the strip fires through
// the real rollup path when the crew bead carries its project label (the
// production shape), and that a crew with NO project label passes through
// verbatim (defensive: the unlabeled case reads exactly as before).
func TestPlanCrewBulk_FaceInitialsStripProjectPrefix(t *testing.T) {
	f := seedPlansFake()
	labeled := seedCrew(f, "gd-crewL", "gardenio-gardenio-crew-delivery-drive-r-6av3", "gd-step1", "working", "in_progress")
	labeled.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC)
	labeled.Labels = []string{"project:gardenio", "role:crew"}
	unlabeled := seedCrew(f, "gd-crewU", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	unlabeled.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC)
	// No Labels on gd-crewU — passes through verbatim.
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	byID := map[string]planCrewFace{}
	for _, fc := range roll.Faces {
		byID[fc.ID] = fc
	}
	if got := byID["gd-crewL"].Initials; got != "CDDR" {
		t.Errorf("labeled crew avatar = %q, want CDDR (project run stripped)", got)
	}
	if got := byID["gd-crewU"].Initials; got != "GWRA" {
		t.Errorf("unlabeled crew avatar = %q, want GWRA (verbatim — no project label to strip)", got)
	}
}

// TestHandlePlansIndex_CrewAvatarStripsProjectPrefix is the end-to-end render
// crux: the rendered /plans card carries the STRIPPED avatar glyph, not the raw
// project-prefixed one. Fails pre-fix (the avatar would read "GGGD").
func TestHandlePlansIndex_CrewAvatarStripsProjectPrefix(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "open"
	crew := seedCrew(f, "gd-crewX", "gardenio-gardenio-crew-delivery-drive-r-6av3", "gd-step1", "working", "in_progress")
	crew.Labels = []string{"project:gardenio", "role:crew"}
	// Fixture clock so the working crew reads fresh (live) under the iter-42 gate —
	// a live avatar only renders for a crew classified live.
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	req := httptest.NewRequest("GET", "/plans?operated_by=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The live-crew avatar strip must render...
	if !strings.Contains(body, `class="plan-card-crew-face"`) {
		t.Fatal("expected a live-crew avatar face — the seed did not produce one")
	}
	// ...with the STRIPPED glyph as the avatar text.
	if !strings.Contains(body, `>CDDR<`) {
		t.Errorf("card missing the stripped avatar glyph 'CDDR' — the project prefix was not dropped")
	}
	// ...and NOT the raw project-prefixed glyph (the pre-fix behavior).
	if strings.Contains(body, `>GGGD<`) {
		t.Error("card still renders the raw project-prefixed avatar 'GGGD' — the strip did not fire")
	}
	// The full crew name is preserved in the tooltip/aria (only the avatar collapses).
	if !strings.Contains(body, "gardenio-gardenio-crew-delivery-drive-r-6av3") {
		t.Error("the full crew name must remain in the face tooltip/aria-label")
	}
}
