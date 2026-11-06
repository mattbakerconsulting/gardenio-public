package main

import (
	"net/url"
	"sort"

	"gardenio/controller/internal/beadsapi"
)

// decisionProjectFacet is one chip in the /decisions project-filter row. It
// satisfies the shared `filter_row` partial's element shape (Label / URL /
// Active / Count), so the row renders byte-identical to the /plans and /extern
// control rows rather than hand-rolling a third variant (ka-0000006647 F7).
type decisionProjectFacet struct {
	Label  string
	URL    string
	Active bool
	Count  int
}

// buildDecisionProjectFacets turns the fetched open-decision set into the
// project chip row: an "All" chip carrying the total, then one chip per project
// that actually HAS an open decision, sorted by count descending then name.
//
// The facet is derived from the DECISIONS, not from the project-bead registry,
// on purpose. The registry holds 67 projects — most of them ephemeral
// `pool-rmw-e2e-*` test fixtures — while only a handful ever carry an open
// decision, so a registry-sourced row would be almost entirely dead chips. A
// decision-sourced row is short, always clickable, and discloses the queue
// depth per project for free.
//
// showAll is the ?all=1 scope-toggle state, carried through every chip URL so
// switching project never silently resets the mine/all scope; active is the
// currently-selected project and base is the page path (basePath + "/decisions").
func buildDecisionProjectFacets(beads []*beadsapi.BeadDetail, active string, showAll bool, base string) []decisionProjectFacet {
	counts := map[string]int{}
	total := 0
	for _, b := range beads {
		if b == nil {
			continue
		}
		total++
		if p := projectFromLabels(b.Labels); p != "" {
			counts[p]++
		}
	}
	// Fold the active scope in even at zero, so a ?project=<x> that matches
	// nothing still has a lit chip to click off of rather than vanishing.
	if active != "" {
		if _, ok := counts[active]; !ok {
			counts[active] = 0
		}
	}

	href := func(project string) string {
		q := url.Values{}
		if showAll {
			q.Set("all", "1")
		}
		if project != "" {
			q.Set("project", project)
		}
		if len(q) == 0 {
			return base
		}
		return base + "?" + q.Encode()
	}

	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})

	facets := make([]decisionProjectFacet, 0, len(names)+1)
	facets = append(facets, decisionProjectFacet{
		Label:  "All",
		URL:    href(""),
		Active: active == "",
		Count:  total,
	})
	for _, name := range names {
		facets = append(facets, decisionProjectFacet{
			Label:  name,
			URL:    href(name),
			Active: active == name,
			Count:  counts[name],
		})
	}
	return facets
}
