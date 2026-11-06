package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// maps_run_glyphs.go — board-only recent-run glyph fallback (bug ka-0000001069).
//
// The /maps board's per-gardener recent-run strip + verdict sparkline ride on
// gardenerRowView.Runs, which buildGardenerTrajectory builds ONLY from
// GardenerSpec.ProgressState.Runs (gardener_trajectory.go:126-155). For ~every
// real gardener that blob is empty: ProgressState.Runs is written by
// progress.FinalizeRun, gated on the gardener's LastAgentID being populated
// (triggers.go), and subtree-mode gardeners (those with assign_bead set — nearly
// all real gardeners) never carry a populated LastAgentID, so FinalizeRun never
// runs. The strip + sparkline therefore rendered EMPTY even for gardeners with
// dozens-to-hundreds of real iterations (ka-0000005432=149, ka-0000002190=63).
//
// This adds a bounded fallback that, for the rendered rows whose Runs is empty,
// derives the glyph strip from the gardener's REAL iteration agent beads — the
// same source the gardener DETAIL page already reads (gardenerIterations,
// gardener.go:420). It is confined to the /maps handler (NOT the shared
// loadGardenerRows spine), so /gardeners pays nothing and the cost is bounded to
// exactly the cards the board is about to draw.
//
// Boundedness (acceptance: "no N+1 per-gardener daemon fan-out that would slow
// the board"):
//   - only the EMPTY-Runs subset of the rendered rows is queried;
//   - each query is capped to the tail gardenerRunGlyphFetchLimit iterations
//     (newest-first), so a 149-iteration gardener pulls ~24 small beads, not 149;
//   - the per-row fetches are fanned out at gardenerRunGlyphFanout, mirroring the
//     existing enrichGardenerRunCounts pattern (gardeners_enrich.go);
//   - a query error / no-iterations row simply keeps its empty strip — the card
//     degrades to counters + last-run chip, exactly as before this fix.

// gardenerRunGlyphFanout bounds the concurrent iteration-fetch queries the glyph
// fallback issues. Mirrors gardenerRunCountFanout: the empty-Runs subset is small
// (the operator-bounded live fleet, minus the handful with a real ProgressState),
// so a modest fan-out drains it in one wave without flooding the daemon.
const gardenerRunGlyphFanout = 8

// gardenerRunGlyphFetchLimit caps how many recent iteration beads the fallback
// pulls per gardener. The board only draws the tail gardenerTrajectorySparkRuns
// (sparkline) / mapsRecentRunStrip (pills), so fetching the newest
// gardenerTrajectorySparkRuns is enough to fill both — pulling the full
// life-thread would be wasteful for a deep gardener.
const gardenerRunGlyphFetchLimit = gardenerTrajectorySparkRuns

// Per-outcome glyph heights for the iteration-derived strip. The iteration beads
// carry no progress SCORE (that lives only in ProgressState), so we cannot scale
// bars by score the way buildGardenerTrajectory does. Instead each glyph gets a
// fixed height by outcome so the strip + sparkline stay legible: a clean run
// reads tall, a live/unknown run mid, a failure a short stub. The OUTCOME COLOUR
// still carries the real signal; the height is only there so the bars/line are
// visible and non-uniform.
const (
	runGlyphHeightOK   = 70
	runGlyphHeightFlat = 35
	runGlyphHeightFail = 12
)

// enrichGardenerRunGlyphs backfills the recent-run glyph strip for rows whose
// Runs is empty (bug ka-0000001069), deriving it from the gardener's real
// iteration beads. Bounded fan-out over only the empty-Runs subset; a row with a
// real ProgressState-derived strip is left untouched, and a row with no findable
// iterations (or a query error) keeps its empty strip.
func (s *Server) enrichGardenerRunGlyphs(ctx context.Context, rows []gardenerRowView) {
	idxs := make([]int, 0)
	for i := range rows {
		if len(rows[i].Runs) == 0 && strings.TrimSpace(rows[i].ID) != "" {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) == 0 {
		return
	}

	sem := make(chan struct{}, gardenerRunGlyphFanout)
	var wg sync.WaitGroup
	for _, i := range idxs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if glyphs := s.deriveGardenerRunGlyphs(ctx, rows[i].ID); len(glyphs) > 0 {
				rows[i].Runs = glyphs
			}
		}(i)
	}
	wg.Wait()
}

// deriveGardenerRunGlyphs fetches a gardener's most-recent iteration agent beads
// and projects them into the oldest→newest run-glyph slice the board strip +
// sparkline consume. It unions the dual ownership keys (trigger_id / schedule_id)
// the way gardenerIterations does, takes the tail gardenerRunGlyphFetchLimit by
// created_at, and skips never-ran spawn shells (iterationDidNotRun) so an expired
// queue entry is not drawn as a run.
//
// Returns nil when the gardener has no findable iterations or every underlying
// query errored — the caller then leaves the row's empty strip in place.
func (s *Server) deriveGardenerRunGlyphs(ctx context.Context, gardenerID string) []gardenerRunGlyph {
	seen := make(map[string]bool)
	beads := make([]*beadsapi.BeadDetail, 0, gardenerRunGlyphFetchLimit)
	for _, key := range []string{fieldTriggerID, fieldScheduleID} {
		res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:  []string{"agent"},
			Fields: map[string]string{key: gardenerID},
			// Newest-first so the per-key Limit keeps the RECENT tail (the strip
			// shows the latest runs), not the oldest. Re-sorted oldest→newest below.
			Sort:  "-created_at",
			Limit: gardenerRunGlyphFetchLimit,
		})
		if err != nil {
			s.logger.Error("maps: derive run glyphs", "gardener", gardenerID, "key", key, "error", err)
			continue
		}
		for _, b := range res.Beads {
			if b == nil || b.ID == "" || seen[b.ID] {
				continue
			}
			// Defensive owner filter (mirrors gardenerIterations): even with the
			// daemon field filter, a fixture/daemon that serves adjacent agents
			// must not pollute the strip.
			if b.Fields[fieldTriggerID] != gardenerID && b.Fields[fieldScheduleID] != gardenerID {
				continue
			}
			// Never-ran spawn shells (agent_state=expired) are real type=agent
			// beads but NOT runs (bug ka-0000006805) — drop them so the strip
			// matches the detail page's run-only thread.
			if iterationDidNotRun(b) {
				continue
			}
			seen[b.ID] = true
			beads = append(beads, b)
		}
	}
	if len(beads) == 0 {
		return nil
	}

	// Re-order oldest→newest (the union arrived newest-first per key) and keep
	// only the tail gardenerRunGlyphFetchLimit so the strip reads left-to-right as
	// the gardener's life advances, matching buildGardenerTrajectory's ordering.
	// created_at order is the heuristic the detail page also falls back to when
	// the causal chain is unavailable; for a glyph strip it is sufficient.
	sort.SliceStable(beads, func(a, b int) bool {
		return beads[a].CreatedAt.Before(beads[b].CreatedAt)
	})
	if len(beads) > gardenerRunGlyphFetchLimit {
		beads = beads[len(beads)-gardenerRunGlyphFetchLimit:]
	}

	glyphs := make([]gardenerRunGlyph, 0, len(beads))
	for i, b := range beads {
		glyphs = append(glyphs, runGlyphFromIterationBead(i+1, b))
	}
	return glyphs
}

// runGlyphFromIterationBead projects one iteration agent bead into a run glyph.
// The outcome is the real wrap-up result (agentRunOutcome) for a closed run, or
// "flat" for a still-running iteration; the height is the fixed per-outcome value
// (the bead carries no progress score). Score stays 0 — there is no score signal
// on an iteration bead — so the tooltip omits it.
func runGlyphFromIterationBead(runNumber int, b *beadsapi.BeadDetail) gardenerRunGlyph {
	closed := statusOpenSortKey(b.Status) != 0

	outcome := "flat"
	state := "in progress"
	height := runGlyphHeightFlat
	if closed {
		switch agentRunOutcome(b.Fields["wrapup"]) {
		case "fail":
			outcome = "fail"
			state = "failed"
			height = runGlyphHeightFail
		default:
			// "ok" / "unknown" — a clean close with no blockers is an outcome
			// (matches applyLastRun + the drawer's RunOK glyph).
			outcome = "ok"
			state = "done"
			height = runGlyphHeightOK
		}
	}

	return gardenerRunGlyph{
		RunNumber: runNumber,
		Outcome:   outcome,
		Score:     0,
		Height:    height,
		Title:     fmt.Sprintf("Run #%d · %s", runNumber, state),
	}
}
