package main

import "net/http"

// maps.go — the /maps board handler (epic ka-0000001644, build ka-0000005805,
// promote ka-0000002415).
//
// /maps shows every gardener as a board of connected cards (trigger → identity +
// verdict → recent-run strip). It reuses the /gardeners projection spine
// verbatim — loadGardenerRows → sortGardenerRows — so the two surfaces report the
// same gardeners in the same order. The recent-run glyphs ride on the row's
// embedded trajectory (built from the gardener bead's ProgressState during the
// row decode); when that ProgressState is empty (true for ~every subtree-mode
// gardener — bug ka-0000001069) the board falls back to a BOUNDED iteration-bead
// fetch (enrichGardenerRunGlyphs) over only the empty-Runs subset of the rendered
// rows. buildMapsBoard turns the sorted rows into the pure, template-ready board
// model.
//
// This replaces the old /maps (a gardener picker + per-gardener radial
// constellation + undifferentiated fleet grid), torn down in ka-0000000247. The
// board is the canonical /maps now; the "Maps" nav link points here.
//
// Default scope is the LIVE fleet (open + enabled + modern type=trigger
// gardeners — fleetGardenerIsLive), so the board leads with the gardeners that can
// actually fire. ?all=1 widens to every gardener (disabled, closed, legacy
// schedule) for archival/triage browsing.
func (s *Server) handleMaps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, excluded, truncated, err := s.loadGardenerRows(ctx)
	if err != nil {
		// Degrade-render like handleGardeners: a load failure still returns 200
		// with an empty board (and the error surfaced), so the nav link never
		// 500s the page.
		s.logger.Error("maps: load gardener rows", "error", err)
		s.render(w, r, "maps.html", map[string]any{
			"Board": mapsBoardView{BasePath: s.basePath},
			"Error": "Failed to load gardeners.",
			"All":   false,
			"Rail":  mapsFilterRail{},
		})
		return
	}

	all := parseBool(r.URL.Query().Get("all"))
	if !all {
		live := rows[:0]
		for _, row := range rows {
			if fleetGardenerIsLive(row) {
				live = append(live, row)
			}
		}
		rows = live
	}

	sortGardenerRows(rows)

	// Filter rail (maps2 v2, ka-0000008126). The selected facets ride in
	// ?filter= using the cross-filter wire format (crossfilter.go); the value is
	// read via Query().Get (Go-decoded once), so parse it with the
	// already-decoded parser. The rail's chips/counts are built over the SCOPED
	// rows (post live/all, pre cross-filter) so each chip shows its own scoped
	// total; the board then renders only the rows that pass the filter.
	filter := parseCrossFilterDecoded(r.URL.Query().Get("filter"))
	rail := buildMapsFilterRail(rows, filter, s.basePath, all)
	scopedTotal := len(rows)
	if len(filter) > 0 {
		kept := rows[:0]
		for _, row := range rows {
			if mapsRowMatchesFilter(row, filter) {
				kept = append(kept, row)
			}
		}
		rows = kept
	}

	// Recent-run glyph fallback (bug ka-0000001069). The strip + sparkline ride on
	// row.Runs, built by buildGardenerTrajectory from GardenerSpec.ProgressState —
	// which is empty for ~every subtree-mode gardener (assign_bead set ⇒ no
	// populated LastAgentID ⇒ progress.FinalizeRun never runs). For the rendered
	// rows whose Runs is empty, derive the glyphs from the gardener's real
	// iteration beads (the same source the detail page reads), bounded to this
	// post-filter slice and the tail of each gardener's thread. Board-only: the
	// shared loadGardenerRows spine is untouched, so /gardeners pays nothing.
	s.enrichGardenerRunGlyphs(ctx, rows)

	board := buildMapsBoard(rows, s.basePath)

	// Layer gardener-chaining workflows onto the board (task ka-0000004778 / "C2").
	// The chain edges ride on the typed-mention read path (GetMentions); a
	// fetch hiccup or an over-cap fleet degrades to "no workflows" — the plain
	// card grid still renders. Done after the board is built so the standalone
	// remainder keeps the sortGardenerRows order.
	//
	// Planned successors (task ka-0000005273 / "C3"): a gardener that DECLARES a
	// `chain.to` but whose heir has not materialized yet contributes a PLANNED
	// edge (predecessor → synthetic pending node), synthesized with no extra
	// fetch from the row's parsed chain. Merging them with the materialized
	// chain edges makes a just-drawn successor render immediately, before the
	// engine spawns it.
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	edges := s.loadGardenerChainEdges(ctx, ids)
	// Per-predecessor materialized-heir counts, so a fully-spawned chain does not
	// ALSO render its (still-declared) chain.to as a duplicate pending node.
	realHeirCount := make(map[string]int, len(edges))
	for _, e := range edges {
		realHeirCount[e.Pred]++
	}
	planned := suppressSpawnedPlannedEdges(plannedChainEdges(rows), realHeirCount)
	edges = append(edges, planned...)
	if len(edges) > 0 {
		board = applyMapsChainEdges(board, edges)
	}

	// Owner grouping (maps2 v2, ka-0000002262). With ?group=owner the standalone
	// card grid is bucketed into per-owner collapsible sections; the workflow
	// rails above it are left flat (a chains_to chain can span owners). Applied
	// after applyMapsChainEdges so only the un-chained remainder is grouped.
	grouped := r.URL.Query().Get("group") == mapsGroupOwner
	if grouped {
		board.Grouped = true
		board.OwnerGroups = groupMapsCardsByOwner(board.Cards)
	}

	s.render(w, r, "maps.html", map[string]any{
		"Board":          board,
		"Truncated":      truncated,
		"Hidden":         buildGardenersHidden(len(rows), excluded),
		"All":            all,
		"Rail":           rail,
		"ScopedTotal":    scopedTotal,
		"FilterCount":    len(rows),
		"Grouped":        grouped,
		"GroupToggleOn":  mapsGroupHref(s.basePath, all, filter, true),
		"GroupToggleOff": mapsGroupHref(s.basePath, all, filter, false),
	})
}
