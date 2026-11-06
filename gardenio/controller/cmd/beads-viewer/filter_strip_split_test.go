package main

import (
	"regexp"
	"strings"
	"testing"
)

// filter_strip_split_test.go — ka-0000005049 (audit F5).
//
// WHY THIS EXISTS. `.chip-strip` was an OVERLOADED class: it dressed two
// unrelated roles that happened to want the same flex row —
//
//  1. the PAGE-LEVEL filter menu (the row of filter toggles under the main
//     nav on /p, /agents, /triggers, /inbox — the "second menu bar" the
//     operator sees), and
//  2. per-CARD attribute badges inside list rows (escalations, decisions,
//     costs, fleet_index, fleet_bridge, peel_catalog, people rows, the
//     agents summary/tile breakdowns, …).
//
// Because both shared `.chip-strip{margin:0.5rem 0 1rem}`, card badges
// inherited menu-bar spacing and any menu-bar restyle leaked into every card.
// The F5 fix gives the page-level menu a DISTINCT hook class, `.filter-strip`,
// carried alongside `.chip-strip` on the page-level wrappers ONLY. F1-F4 (chip
// style / aria-current / wrapper element) and any future menu-bar restyle now
// target `.filter-strip` and can never touch card badges.
//
// Most page-level strips now flow through the canonical `filter_strip` partial
// (F6, ka-0000005357), so the hook lives in ONE place — the partial's default
// StripClass. A handful of page-level strips keep bespoke inline markup
// (agents active/role/state, inbox kind); those carry the hook inline. This
// guard pins BOTH: the partial default, the inline strips, and the reverse
// direction — the known card-badge strips must NOT grab the hook, so a future
// strip can't silently re-overload the class.

// TestFilterStripSplit_PartialDefaultsToHook asserts the canonical filter_strip
// partial — through which project/project_hub/triggers/agents-project/inbox-scope
// page-level menus render — defaults its wrapper class to the `.filter-strip`
// hook, so every partial caller is a page-level menu that carries it.
func TestFilterStripSplit_PartialDefaultsToHook(t *testing.T) {
	src, err := templateFS.ReadFile("templates/partials/filter_strip.html")
	if err != nil {
		t.Fatalf("read filter_strip partial: %v", err)
	}
	// The default StripClass must include both chip-strip (shared flex layout)
	// and filter-strip (the F5 page-level hook).
	if !regexp.MustCompile(`\$stripClass\s*:=\s*or\s+\.StripClass\s+"chip-strip filter-strip"`).Match(src) {
		t.Errorf("filter_strip partial must default StripClass to \"chip-strip filter-strip\" " +
			"so every page-level caller carries the F5 hook; found a different default.")
	}
}

// inlinePageLevelStrips enumerates the page-level filter menu wrappers that keep
// bespoke inline markup (not routed through the partial) and so must carry the
// `.filter-strip` hook inline.
var inlinePageLevelStrips = []struct {
	file    string
	marker  string // a substring that uniquely identifies the wrapper open tag
	comment string
}{
	{"agents.html", `<nav class="chip-strip filter-strip" aria-label="Active filter">`, "/agents active filter"},
	{"agents.html", `<nav class="chip-strip filter-strip" aria-label="Role filter">`, "/agents role filter"},
	{"agents.html", `<nav class="chip-strip filter-strip" aria-label="State filter">`, "/agents state filter"},
	{"inbox.html", `<div class="chip-strip filter-strip inbox-kind-strip" role="tablist" aria-label="Inbox kind filter">`, "/inbox kind filter"},
}

// TestFilterStripSplit_InlinePageLevelStripsCarryHook asserts every page-level
// filter menu wrapper that keeps inline markup carries the `.filter-strip` hook.
func TestFilterStripSplit_InlinePageLevelStripsCarryHook(t *testing.T) {
	for _, s := range inlinePageLevelStrips {
		src, err := templateFS.ReadFile("templates/" + s.file)
		if err != nil {
			t.Fatalf("read template %s: %v", s.file, err)
		}
		if !strings.Contains(string(src), s.marker) {
			t.Errorf("%s (%s): inline page-level filter strip missing .filter-strip hook.\n"+
				"expected wrapper substring: %q\n"+
				"page-level filter menus MUST carry `chip-strip filter-strip` so menu-bar "+
				"styling (audit F1-F4) targets `.filter-strip` and never leaks into card badges.",
				s.file, s.comment, s.marker)
		}
	}
}

// TestFilterStripSplit_HookIsDefined asserts the `.filter-strip` rule exists in
// the served stylesheet — a hook nothing styles is a silent no-op.
func TestFilterStripSplit_HookIsDefined(t *testing.T) {
	layout, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	if !regexp.MustCompile(`\.filter-strip\s*\{`).Match(layout) {
		t.Fatalf(".filter-strip must be defined in layout.html's stylesheet; " +
			"the F5 hook is inert without a rule.")
	}
}

// TestFilterStripSplit_CardBadgesDoNotGrabHook guards the OTHER direction: the
// per-card attribute-badge `.chip-strip` usages must NOT carry `.filter-strip`,
// or the overload the F5 split removed silently returns. We assert the known
// card-badge strips render plain `chip-strip` (optionally with a card-scoped
// modifier), never the `filter-strip` hook.
func TestFilterStripSplit_CardBadgesDoNotGrabHook(t *testing.T) {
	// Card-badge / summary strips that must stay OFF the page-level hook.
	cardBadgeStrips := []struct {
		file   string
		marker string
	}{
		{"escalations.html", `<div class="chip-strip" aria-label="Escalation attributes">`},
		{"decisions.html", `<div class="chip-strip" aria-label="Decision options">`},
		{"extern.html", `<div class="chip-strip" aria-label="Status">`},
		{"fleet_index.html", `<div class="chip-strip" aria-label="Sentinel identity">`},
		{"fleet_bridge.html", `<div class="chip-strip" aria-label="Bead metadata">`},
		{"peel_catalog.html", `<div class="chip-strip" aria-label="Slice attributes">`},
		{"agents.html", `<div class="chip-strip agents-summary-roles" aria-label="Active by role">`},
		{"agents.html", `<div class="chip-strip agents-summary-projects" aria-label="Active by project">`},
	}
	for _, s := range cardBadgeStrips {
		src, err := templateFS.ReadFile("templates/" + s.file)
		if err != nil {
			t.Fatalf("read template %s: %v", s.file, err)
		}
		if !strings.Contains(string(src), s.marker) {
			t.Errorf("%s: expected card-badge strip to keep plain `.chip-strip` (no filter-strip hook).\n"+
				"expected wrapper substring: %q\n"+
				"if this strip legitimately became a page-level filter menu, route it through the "+
				"filter_strip partial (or add it to inlinePageLevelStrips) instead of grafting the hook silently.",
				s.file, s.marker)
		}
	}
}
