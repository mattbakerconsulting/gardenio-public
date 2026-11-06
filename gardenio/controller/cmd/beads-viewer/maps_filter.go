package main

import (
	"net/url"
	"sort"
	"strings"
)

// maps_filter.go — the /maps filter rail (maps2 v2, ka-0000008126, epic
// ka-0000006790; design.md §6 "filter rail (reuse maps_fleet_filter cross-filter
// wire format)").
//
// The rail is a strip of facet chips ABOVE the board. Selecting a chip narrows
// the card grid to gardeners matching every selected facet (cross-key AND),
// where a facet with multiple selected values is satisfied by any one of them
// (within-key OR) — the same chip-strip semantics crossFilter.matchesAny
// already encodes. Four facets are offered, all projected off the gardenerRowView
// the board already loads (zero extra daemon fan-out):
//
//	trigger:  cron | bead_query | event | schedule   (mapsTriggerKind)
//	verdict:  progress | stalled | stuck | neutral    (row.VerdictKind)
//	owner:    <owner slug>                            (row.OwnerSlug)
//	enabled:  true | false                            (row.Enabled)
//
// The selected facets ride in the URL as `?filter=<wire>` using the SAME
// comma-separated `key:value` cross-filter wire format as crossfilter.go (the
// maps_fleet_filter format the design points at), so a filtered board is a
// shareable link. The handler reads it via r.URL.Query().Get — already
// percent-decoded once by Go — so it parses with parseCrossFilterDecoded.
//
// This file is pure (no http, no daemon, no clock): the handler owns the fetch
// and scope; this owns matching + the rail view-model.

// maps facet keys — the `key` half of each `key:value` cross-filter pair.
const (
	mapsFacetTrigger = "trigger"
	mapsFacetVerdict = "verdict"
	mapsFacetOwner   = "owner"
	mapsFacetEnabled = "enabled"
)

// mapsVerdictFacetValue is the verdict facet value for a row: the row's
// VerdictKind, or "neutral" when the gardener has no computed verdict (so the
// "neutral" chip catches the un-verdicted tail rather than leaving those rows
// unfacetable).
func mapsVerdictFacetValue(r gardenerRowView) string {
	if r.VerdictKind == "" {
		return "neutral"
	}
	return r.VerdictKind
}

// mapsEnabledFacetValue maps a row's Enabled bool to the enabled facet value.
func mapsEnabledFacetValue(r gardenerRowView) string {
	if r.Enabled {
		return "true"
	}
	return "false"
}

// mapsRowMatchesFilter reports whether a gardener row passes the cross-filter.
// An empty filter matches everything; otherwise every present facet key must be
// satisfied by the row's value for that facet (cross-key AND, within-key OR via
// matchesAny). A facet key absent from the filter is a no-op for that row.
func mapsRowMatchesFilter(r gardenerRowView, f crossFilter) bool {
	if len(f) == 0 {
		return true
	}
	return f.matchesAny(mapsFacetTrigger, mapsTriggerKind(r)) &&
		f.matchesAny(mapsFacetVerdict, mapsVerdictFacetValue(r)) &&
		f.matchesAny(mapsFacetOwner, r.OwnerSlug) &&
		f.matchesAny(mapsFacetEnabled, mapsEnabledFacetValue(r))
}

// mapsFacetOption is one selectable chip within a facet group.
type mapsFacetOption struct {
	Value  string // the facet value (the `value` half of the wire pair)
	Label  string // human chip text
	Count  int    // scoped rows carrying this value (each chip shows its own total)
	Active bool   // currently selected in the filter
	Href   string // toggles this value on/off, preserving the rest of the filter + scope
}

// mapsFacetGroup is one facet (trigger / verdict / owner / enabled) and its
// chips, rendered as a labeled row in the rail.
type mapsFacetGroup struct {
	Key     string
	Label   string
	Options []mapsFacetOption
}

// mapsFilterRail is the whole filter-rail view-model: the facet groups plus the
// overall active flag and a clear-all link. Active is true when any facet is
// selected, which the template uses to surface the "clear filters" affordance
// and a "showing N of M" note.
type mapsFilterRail struct {
	Groups    []mapsFacetGroup
	Active    bool
	ClearHref string
}

// mapsHref composes a /maps URL preserving the all-scope and the cross-filter.
// Query keys are encoded (and sorted) by url.Values.Encode, so the filter's
// `:`/`,` are percent-encoded on the wire and decoded once by Go on read —
// round-tripping cleanly through parseCrossFilterDecoded.
func mapsHref(basePath string, all bool, f crossFilter) string {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	if s := f.String(); s != "" {
		q.Set("filter", s)
	}
	if len(q) == 0 {
		return basePath + "/maps"
	}
	return basePath + "/maps?" + q.Encode()
}

// clone returns a deep copy of the cross-filter so a chip's toggle never
// mutates the request's parsed filter (used by every chip's href).
func (f crossFilter) clone() crossFilter {
	out := make(crossFilter, len(f))
	for k, v := range f {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// mapsToggleFacet returns a NEW cross-filter with (key,val) toggled: removed if
// already present (case-insensitively), added otherwise. Removing the last
// value for a key drops the key so the wire string stays minimal.
func mapsToggleFacet(f crossFilter, key, val string) crossFilter {
	out := f.clone()
	vals := out[key]
	idx := -1
	for i, v := range vals {
		if strings.EqualFold(v, val) {
			idx = i
			break
		}
	}
	if idx >= 0 {
		vals = append(vals[:idx], vals[idx+1:]...)
		if len(vals) == 0 {
			delete(out, key)
		} else {
			out[key] = vals
		}
	} else {
		out[key] = append(vals, val)
	}
	return out
}

// filterHasValue reports whether the filter selects (key,val), case-insensitively.
func (f crossFilter) filterHasValue(key, val string) bool {
	for _, v := range f[key] {
		if strings.EqualFold(v, val) {
			return true
		}
	}
	return false
}

// buildMapsFilterRail builds the rail view-model over the SCOPED rows (after the
// live/all scope, BEFORE the cross-filter is applied) so each chip's count is
// its own scoped total — the /gardeners chip idiom. Only facet values present in
// the scoped set get a chip, and a facet group with fewer than two distinct
// values is dropped (a one-value facet can't narrow anything). The trigger,
// verdict and enabled groups use a fixed canonical order; owners sort by label.
func buildMapsFilterRail(scoped []gardenerRowView, f crossFilter, basePath string, all bool) mapsFilterRail {
	// Tally counts per facet value, and capture owner display labels.
	type counter struct {
		order  []string
		count  map[string]int
		labels map[string]string // value -> display label (owner only)
	}
	newCounter := func() *counter {
		return &counter{count: map[string]int{}, labels: map[string]string{}}
	}
	trig, verd, own, enab := newCounter(), newCounter(), newCounter(), newCounter()
	bump := func(c *counter, val, label string) {
		if val == "" {
			return
		}
		if _, seen := c.count[val]; !seen {
			c.order = append(c.order, val)
		}
		c.count[val]++
		if label != "" {
			c.labels[val] = label
		}
	}
	for _, r := range scoped {
		bump(trig, mapsTriggerKind(r), "")
		bump(verd, mapsVerdictFacetValue(r), "")
		ownerLabel := r.OwnerDisplay
		if ownerLabel == "" {
			ownerLabel = r.OwnerSlug
		}
		bump(own, r.OwnerSlug, ownerLabel)
		bump(enab, mapsEnabledFacetValue(r), "")
	}

	// option builds one chip, computing its toggled href + active state.
	option := func(key, val, label string, count int) mapsFacetOption {
		active := f.filterHasValue(key, val)
		return mapsFacetOption{
			Value:  val,
			Label:  label,
			Count:  count,
			Active: active,
			Href:   mapsHref(basePath, all, mapsToggleFacet(f, key, val)),
		}
	}

	// orderedGroup emits options in a fixed canonical order, keeping only values
	// that actually occur in the scoped set.
	orderedGroup := func(key, groupLabel string, c *counter, order []string, label func(string) string) mapsFacetGroup {
		g := mapsFacetGroup{Key: key, Label: groupLabel}
		for _, val := range order {
			if n := c.count[val]; n > 0 {
				g.Options = append(g.Options, option(key, val, label(val), n))
			}
		}
		return g
	}
	// sortedGroup emits options sorted by display label (owners).
	sortedGroup := func(key, groupLabel string, c *counter) mapsFacetGroup {
		g := mapsFacetGroup{Key: key, Label: groupLabel}
		vals := make([]string, len(c.order))
		copy(vals, c.order)
		sort.Slice(vals, func(i, j int) bool {
			li, lj := c.labels[vals[i]], c.labels[vals[j]]
			if li == "" {
				li = vals[i]
			}
			if lj == "" {
				lj = vals[j]
			}
			if !strings.EqualFold(li, lj) {
				return strings.ToLower(li) < strings.ToLower(lj)
			}
			return vals[i] < vals[j]
		})
		for _, val := range vals {
			label := c.labels[val]
			if label == "" {
				label = val
			}
			g.Options = append(g.Options, option(key, val, label, c.count[val]))
		}
		return g
	}

	groups := []mapsFacetGroup{
		orderedGroup(mapsFacetTrigger, "Trigger",
			trig, []string{"cron", "bead_query", "event", "schedule"}, mapsTriggerKindLabel),
		orderedGroup(mapsFacetVerdict, "Health",
			verd, []string{"progress", "stalled", "stuck", "neutral"}, mapsVerdictFacetLabel),
		sortedGroup(mapsFacetOwner, "Owner", own),
		orderedGroup(mapsFacetEnabled, "State",
			enab, []string{"true", "false"}, mapsEnabledFacetLabel),
	}

	rail := mapsFilterRail{Active: len(f) > 0, ClearHref: mapsHref(basePath, all, nil)}
	for _, g := range groups {
		// A facet that resolves to a single value (or none) can't narrow the
		// board — drop it to keep the rail honest, UNLESS that value is the one
		// currently selected (then the user needs the chip to toggle it off).
		if len(g.Options) < 2 {
			if len(g.Options) == 1 && g.Options[0].Active {
				rail.Groups = append(rail.Groups, g)
			}
			continue
		}
		rail.Groups = append(rail.Groups, g)
	}
	return rail
}

// mapsVerdictFacetLabel humanizes a verdict facet value for its chip.
func mapsVerdictFacetLabel(kind string) string {
	switch kind {
	case "progress":
		return "progressing"
	case "stalled":
		return "stalled"
	case "stuck":
		return "stuck"
	case "neutral":
		return "neutral"
	}
	return kind
}

// mapsEnabledFacetLabel humanizes the enabled facet value for its chip.
func mapsEnabledFacetLabel(val string) string {
	if val == "true" {
		return "enabled"
	}
	return "disabled"
}
