package main

import (
	"net/url"
	"sort"
	"strings"
)

// maps_group.go — the /maps "group by owner" mode (maps2 v2, ka-0000002262,
// epic ka-0000006790; design.md §6 "grouping by owner").
//
// By default the board renders its standalone cards as one flat responsive
// grid. With ?group=owner the same cards are bucketed into per-owner
// collapsible sections (one <details> per OwnerSlug), each headed by the
// owner's display name + a card count, so an operator scanning a many-owner
// fleet can collapse the owners they don't care about. The grouping reuses the
// gardenerRowView owner fields the cards already carry (OwnerSlug / OwnerDisplay)
// — zero extra daemon fan-out.
//
// Grouping applies ONLY to the standalone card grid, not to the workflow rails:
// a chains_to workflow can span owners, so bucketing its cards by owner would
// tear the rail apart. The workflow strip renders unchanged above the grouped
// grid, exactly as it does above the flat grid.
//
// This file is pure (no http, no daemon, no clock): the handler reads ?group=
// and the cross-filter scope; this owns the bucketing + the section view-model.

// mapsGroupOwner is the ?group= value that selects owner grouping. Any other
// value (or absence) leaves the board flat.
const mapsGroupOwner = "owner"

// mapsUnownedSlug / mapsUnownedLabel bucket gardeners with no owner so they are
// not silently dropped from the grouped board. The slug is empty-string's
// stand-in for the URL/sort key; the label heads the section.
const (
	mapsUnownedSlug  = "~unowned"
	mapsUnownedLabel = "Unowned"
)

// mapsOwnerGroup is one collapsible per-owner section on the grouped board: the
// owner's display label, a stable slug (for the section anchor / sort), the
// cards they own, and the count (cached so the template needn't re-len).
type mapsOwnerGroup struct {
	Slug  string
	Label string
	Cards []mapsCardView
	Count int
}

// groupMapsCardsByOwner buckets cards into per-owner sections, preserving the
// caller's card order WITHIN each section (the caller already applied
// sortGardenerRows, so /maps and /gardeners still agree on intra-owner order).
// Sections are ordered by owner display label (case-insensitive), with the
// "Unowned" bucket sorted last so the named owners lead. An empty input yields
// no groups.
func groupMapsCardsByOwner(cards []mapsCardView) []mapsOwnerGroup {
	if len(cards) == 0 {
		return nil
	}
	bySlug := make(map[string]*mapsOwnerGroup)
	var order []string // slugs in first-seen order; final sort applied below
	for _, c := range cards {
		slug, label := mapsOwnerBucket(c)
		g, ok := bySlug[slug]
		if !ok {
			g = &mapsOwnerGroup{Slug: slug, Label: label}
			bySlug[slug] = g
			order = append(order, slug)
		}
		g.Cards = append(g.Cards, c)
	}
	groups := make([]mapsOwnerGroup, 0, len(order))
	for _, slug := range order {
		g := bySlug[slug]
		g.Count = len(g.Cards)
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		// Unowned always sorts last.
		ui, uj := groups[i].Slug == mapsUnownedSlug, groups[j].Slug == mapsUnownedSlug
		if ui != uj {
			return uj
		}
		li, lj := strings.ToLower(groups[i].Label), strings.ToLower(groups[j].Label)
		if li != lj {
			return li < lj
		}
		return groups[i].Slug < groups[j].Slug
	})
	return groups
}

// mapsOwnerBucket returns the (slug, label) a card buckets under. A card with no
// OwnerSlug falls into the synthetic "Unowned" bucket; one with a slug but no
// display label falls back to the slug as its label.
func mapsOwnerBucket(c mapsCardView) (slug, label string) {
	slug = strings.TrimSpace(c.OwnerSlug)
	if slug == "" {
		return mapsUnownedSlug, mapsUnownedLabel
	}
	label = strings.TrimSpace(c.OwnerDisplay)
	if label == "" {
		label = slug
	}
	return slug, label
}

// mapsGroupHref composes a /maps URL preserving the all-scope and the
// cross-filter, with grouping toggled on or off. It mirrors mapsHref's encoding
// (url.Values.Encode → sorted, percent-encoded), adding ?group=owner when
// grouped. A nil/empty filter and the live scope collapse to a bare path so the
// toggle link stays clean.
func mapsGroupHref(basePath string, all bool, f crossFilter, grouped bool) string {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	if s := f.String(); s != "" {
		q.Set("filter", s)
	}
	if grouped {
		q.Set("group", mapsGroupOwner)
	}
	if len(q) == 0 {
		return basePath + "/maps"
	}
	return basePath + "/maps?" + q.Encode()
}
