package main

// plans_people_strip.go — Step 5 (task ka-0000004302, plan epic ka-0000004110 /
// bundle ka-0000007352): the "browse plans by person" discovery strip on the
// /plans index.
//
// Gap B (the bundle's cross-user-discoverability gap): before this, the only
// ways to reach a teammate's plans were to flip the header chip to the
// everyone's-plans firehose or to stumble on a card they created and click its
// creator chip. There was no directory-style "pivot to Alice's plans" entry
// point ON the index itself.
//
// The strip is a row of one chip per distinct HUMAN operator who owns a plan in
// the current (status/project-filtered) universe, each linking to
// ?operated_by=<their-alias>. A click lands on that person's INVOLVED plans —
// the Step 1 plansInvolvingOperator union (creator ∪ step-owner ∪
// decision-resolver ∪ gardener-owner) the header-chip default already runs — so
// the strip is a one-hop pivot that REUSES the involvement primitive rather than
// inventing a new query. A trailing link crosses over to the full /people
// directory for the long tail (and for operators with no plans yet).
//
// The strip is SCOPE-INDEPENDENT: it is derived from the UNSCOPED plan universe,
// not the currently-scoped list, so browsing to a peer is possible even while
// the main list is narrowed to "your plans". The current scope only decides
// which chip renders as active.

import (
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// planPeopleStripCap bounds the number of person chips the browse strip renders.
// The strip is a lightweight pivot affordance, not the full directory — past a
// couple dozen chips it stops being scannable, and the trailing "all people →"
// link to /people covers the long tail. Chips are the most prolific plan owners
// first, so the cap keeps the highest-signal browse targets.
const planPeopleStripCap = 24

// planPersonChip is one operator in the browse-by-person strip.
type planPersonChip struct {
	// RawID is the operator's operated_by id (oidc:<email> / slack:U…) — the
	// value the chip's ?operated_by= link carries, resolved daemon-side to the
	// operator FK by the involvement query. An oidc: alias is preferred when a
	// person owns plans under more than one identity, for a stable pivot link.
	RawID string
	// Display is the resolved human label (bot-name re-attribution + email
	// humanization via the shared chip resolver; falls back to the raw id).
	Display string
	// Slug, when non-empty, links the chip label to the operator's /u/{slug}
	// homepage (parity with the card creator chip). Empty → no homepage link.
	Slug string
	// Count is the number of plans this operator CREATED in the current
	// (status/project-filtered) universe — the teaser badge. The scoped list a
	// click lands on may show MORE (their involved set ⊇ created set), so this
	// is a floor.
	Count int
	// Href is the ?operated_by=<RawID> pivot link, preserving the sibling
	// status/project/group filters.
	Href string
	// IsSelf marks the viewer's own chip (rendered with a "(you)" tag).
	IsSelf bool
	// IsActive marks the chip matching the current operated_by scope, so a
	// person you have already pivoted to reads as selected.
	IsActive bool
}

// planPeopleStrip is the whole browse-by-person facet view-model.
type planPeopleStrip struct {
	People []planPersonChip
	// Truncated is true when more distinct people own plans than the cap
	// rendered — the template then leans harder on the "all people →" link.
	Truncated bool
	// PeopleHref is the cross-link to the full /people directory.
	PeopleHref string
	// Show is true when the strip is worth rendering: it holds at least one
	// person the viewer can browse TO (a non-self chip). A strip that would show
	// only the viewer's own chip adds no "pivot to a teammate" value, so it is
	// suppressed (mirroring the zero-section suppression on /u/{slug} Step 3).
	Show bool
}

// buildPlanPeopleStrip derives the browse-by-person strip from the UNSCOPED plan
// universe (universe = the plan epics matching the current status/project
// filters with NO operator scope). It groups the epics by their human creator,
// counts plans per person, and emits one chip per person linking to
// ?operated_by=<id>. Non-human operators (machine sentinels, agent-created,
// unowned) are skipped — the strip browses PEOPLE.
//
// Grouping folds a person's aliases together by their resolved email when the
// list projection carries one (a plan spawned from a Slack thread is stamped
// slack:U…, a browser/dogfood plan oidc:<email> — the same human), so they
// render as ONE chip; when no email is present it keys on the raw id (a person
// with only email-less plans may then appear once per raw id, an accepted
// cosmetic edge, never a wrong link). The pivot link is a bounded no-op for the
// daemon either way: the involvement query resolves ANY one alias to the
// operator FK.
//
// basePath/extraQuery build the links (extraQuery preserves the status/project/
// group siblings, minus operated_by — the same encoded fragment the header chip
// uses). effectiveOperatedBy/viewerEmail mark the active + self chips.
func buildPlanPeopleStrip(universe []*beadsapi.BeadDetail, basePath, extraQuery, effectiveOperatedBy, viewerEmail string) planPeopleStrip {
	strip := planPeopleStrip{PeopleHref: basePath + "/people"}

	type agg struct {
		chip    planPersonChip
		count   int
		aliases []string // every operated_by id folded into this person
	}
	byKey := map[string]*agg{}
	var order []string
	for _, b := range universe {
		if b == nil {
			continue
		}
		raw := strings.TrimSpace(b.OperatedBy)
		// The strip browses people, so skip machine sentinels (fleet:/system:),
		// agent-created rows (empty operated_by), and anything else the daemon
		// would not accept as a human operator. bridge.IsHumanOperator is the
		// exact predicate the write path gates on.
		if !bridge.IsHumanOperator(raw) {
			continue
		}
		key := planPersonGroupKey(b)
		a, ok := byKey[key]
		if !ok {
			// Resolve display + slug + self through the shared chip resolver so
			// the strip label matches the card creator chip exactly. basePath=""
			// suppresses its filter link — the strip builds its own pivot Href.
			chip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, "", viewerEmail)
			a = &agg{chip: planPersonChip{
				RawID:   raw,
				Display: chip.Display,
				Slug:    chip.Slug,
				IsSelf:  chip.IsViewerSelf,
			}}
			byKey[key] = a
			order = append(order, key)
		} else {
			// Fold a second alias into the existing person: keep the richest
			// display/slug/self any alias offers, and prefer an oidc: id as the
			// representative pivot link (stable + self-matchable).
			chip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, "", viewerEmail)
			if planPersonDisplayIsFallback(a.chip.Display, a.chip.RawID) && !planPersonDisplayIsFallback(chip.Display, raw) {
				a.chip.Display = chip.Display
			}
			if a.chip.Slug == "" && chip.Slug != "" {
				a.chip.Slug = chip.Slug
			}
			if chip.IsViewerSelf {
				a.chip.IsSelf = true
			}
			if planIsOIDCAlias(raw) && !planIsOIDCAlias(a.chip.RawID) {
				a.chip.RawID = raw
			}
		}
		a.count++
		a.aliases = appendUniqueFold(a.aliases, raw)
	}
	if len(order) == 0 {
		return strip // no human plan owners — the template omits the strip
	}

	chips := make([]planPersonChip, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		a.chip.Count = a.count
		a.chip.Href = buildOperatorHeaderHref(basePath+"/plans", extraQuery, a.chip.RawID)
		// Active when the current scope matches ANY of the person's aliases (a
		// pivot may have arrived via the oidc or the slack id).
		if effectiveOperatedBy != "" {
			for _, al := range a.aliases {
				if strings.EqualFold(al, effectiveOperatedBy) {
					a.chip.IsActive = true
					break
				}
			}
		}
		chips = append(chips, a.chip)
	}

	// Self first (a one-click return to your own scope), then most prolific plan
	// owners (the highest-signal browse targets), then alphabetical by display
	// for a stable tail.
	sort.SliceStable(chips, func(i, j int) bool {
		if chips[i].IsSelf != chips[j].IsSelf {
			return chips[i].IsSelf
		}
		if chips[i].Count != chips[j].Count {
			return chips[i].Count > chips[j].Count
		}
		return strings.ToLower(chips[i].Display) < strings.ToLower(chips[j].Display)
	})
	if len(chips) > planPeopleStripCap {
		chips = chips[:planPeopleStripCap]
		strip.Truncated = true
	}
	strip.People = chips
	// Only render the strip when there is at least one teammate to browse TO. A
	// strip holding only the viewer's own chip is a no-op pivot.
	for _, c := range chips {
		if !c.IsSelf {
			strip.Show = true
			break
		}
	}
	return strip
}

// planPersonGroupKey derives the fold key for one plan epic's human creator: the
// resolved email when the list projection carries one (folds a person's slack:
// + oidc: aliases into one chip), else the raw operated_by id. Mirrors
// people.go's peopleIdentityKey precedent (email-first identity fold).
func planPersonGroupKey(b *beadsapi.BeadDetail) string {
	if e := strings.ToLower(strings.TrimSpace(b.OperatedByEmail)); e != "" {
		return "email:" + e
	}
	return "id:" + strings.ToLower(strings.TrimSpace(b.OperatedBy))
}

// planPersonDisplayIsFallback reports whether a resolved chip display is just
// the raw-id fallback (empty or equal to the raw id), so a later alias with a
// real display can upgrade it.
func planPersonDisplayIsFallback(display, rawID string) bool {
	d := strings.TrimSpace(display)
	return d == "" || strings.EqualFold(d, strings.TrimSpace(rawID))
}

// planIsOIDCAlias reports whether an operated_by id is an oidc:<email> alias —
// the preferred representative for a pivot link (canonical + self-matchable).
func planIsOIDCAlias(rawID string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawID)), "oidc:")
}

// appendUniqueFold appends v to xs unless a case-insensitive equal is already
// present. Keeps the per-person alias set small and duplicate-free.
func appendUniqueFold(xs []string, v string) []string {
	for _, x := range xs {
		if strings.EqualFold(x, v) {
			return xs
		}
	}
	return append(xs, v)
}
