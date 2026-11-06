package main

// plans_lane_crew.go — the aggregate live-crew tally folded onto the /plans
// board LANE and ?group= SECTION headers (ka-0000005352).
//
// MEASURE-FIRST GAP. The mission's 86 card-facet iterations folded crew and
// attention signals onto every plan-CARD surface (the index card face, the
// cockpit roster header, the /deck card, the peek header chip), but the board's
// LANE section headers (plans.html, .plans-lane-head) and the ?group= SECTION
// headers (.plan-group-head) still rendered only "Label (N)". So an operator
// scanning the whole board could not tell WHICH column has live crew working —
// or, more urgently, which column holds STUCK crew needing a human — without
// reading every card in the lane. The per-card facets answer "this plan has
// crew"; the lane header answers the aggregate "this COLUMN has crew", the
// scan-level signal the section head is the natural home for.
//
// This folds a compact aggregate — total working (a green live-dot + "N
// working") and open-stuck ("⚠ N") — summed over the section's own rows, onto
// the header. It reads the SAME .Crew (planCrewRollup) already hydrated on
// every row by the batched enrich pass, so it costs ZERO extra daemon call and
// the header can never disagree with the cards beneath it (advice
// ka-0000005924: the header tally and the sum of the card facets are two
// derivations of the same crew truth). Calm working, loud stuck, and the whole
// facet self-suppresses when a section has no live/stuck crew — the density
// discipline every card facet uses ("add signal, not rows"; render only where
// the value discriminates).
//
// One helper serves BOTH the lane header and the group header (they pass the
// same []planRowView), so the two surfaces agree by construction — the
// redundant-ink / parity idiom the card iterations established.

// planLaneCrewAggregate is the crew tally summed across one board lane / group
// section's rows. Working is the count of crew working RIGHT NOW across the
// section (the green-dot "N working" facet); OpenStuck is the count of OPEN,
// reachable stuck crew across the section (the loud "⚠ N" facet — a human can
// jump to a terminal to unstick each). Both are plain sums of the per-row
// planCrewRollup fields, so they agree with the card facets by construction.
type planLaneCrewAggregate struct {
	Working   int
	OpenStuck int
}

// Any reports whether the section has any crew signal worth surfacing on its
// header — a working crew or an open-stuck crew. When false the template
// renders no aggregate facet at all (a section whose crew are all queued/done,
// or which has no crew, adds no ink to its header). Queued and done crew are
// deliberately NOT summed onto the header: the lane header is a SCAN signal
// (who's live, who's stuck — the two dispositions an operator acts on at the
// board level), and folding every disposition would rebuild the whole card chip
// on the header. The per-card facets carry queued/done/spend where an operator
// is already reading one plan; the header carries only the two column-level
// act-now signals.
func (a planLaneCrewAggregate) Any() bool { return a.Working > 0 || a.OpenStuck > 0 }

// HasWorking gates the calm green "● N working" aggregate facet.
func (a planLaneCrewAggregate) HasWorking() bool { return a.Working > 0 }

// HasOpenStuck gates the loud red "⚠ N" aggregate facet — the reachable
// act-now count, mirroring the card chip's HasOpenStuck facet gate.
func (a planLaneCrewAggregate) HasOpenStuck() bool { return a.OpenStuck > 0 }

// planLaneCrew sums the crew tally across a board lane / group section's rows.
// Registered as a template func so the lane header (.plans-lane-head) and the
// group header (.plan-group-head) both call it over their own .Rows — one code
// path, so the two surfaces can never disagree. Reads the SAME per-row .Crew
// rollup the card facets render; zero extra daemon call.
//
// It sums Working and OpenStuck() straight off each row's rollup. The rollup is
// already terminal-reconciled per row before it reaches here (reconcileTerminal
// folds a terminal plan's Working+Idle into Done and leaves Failed untouched),
// so a Done? / Closed lane's finished plans contribute 0 working and only a
// genuinely-still-open stuck crew to the aggregate — the header never
// manufactures a false "live" on a lane of finished work.
func planLaneCrew(rows []planRowView) planLaneCrewAggregate {
	var agg planLaneCrewAggregate
	for _, r := range rows {
		agg.Working += r.Crew.Working
		agg.OpenStuck += r.Crew.OpenStuck()
	}
	return agg
}
