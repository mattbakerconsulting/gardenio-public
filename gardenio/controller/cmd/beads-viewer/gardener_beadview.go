package main

// Gardener bead-view wiring (task ka-0000006542).
//
// The gardener bead view (/beads/<id>) used to fall through to the bare
// type_generic field-dump for a gardener trigger bead — a wasted opportunity
// next to the rich /gardener/<id> page. This builds the SAME gardenerView header
// the gardener page uses (goal, cadence, counters, config, trajectory) plus a
// short preview of the most-recent runs, so the bead view and the gardener page
// converge on one design (rendered via the shared gardener_summary partial).
//
// The full, ordered, lazily-paginated life-thread and the operator control bar
// stay on /gardener/<id>; the bead view links into it. This keeps the bead page
// cheap (a capped preview, not the whole thread) while still answering "what is
// this gardener, what is its goal, and what has it been doing lately".

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// isGardenerBeadView reports whether a bead should render the rich gardener
// summary on the bead page. This is DELIBERATELY narrower than isGardenerBead
// (gardener_ops.go), which treats any type=schedule bead as a gardener target for
// the gardener page's legacy compatibility. On the bead view a bare
// type=schedule with no gardener spec is an ordinary scheduled job (e.g. a
// nightly-sync crew schedule) and must keep its plain schedule field-list; only
// a role:gardener trigger or a bead carrying an actual gardener spec gets the
// rich render.
func isGardenerBeadView(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if b.Type == "trigger" && hasLabel(b, "role:gardener") {
		return true
	}
	return strings.TrimSpace(b.Fields["gardener"]) != ""
}

// gardenerBeadViewRuns caps how many iteration cards the gardener bead view
// previews inline. The full thread lives on /gardener/<id>; this is just enough
// to answer "what has this gardener been doing lately" without leaving the bead
// page. A "view all" affordance links to the gardener page when more exist.
const gardenerBeadViewRuns = 5

// injectGardenerBeadView populates the gardener-specific template keys on the
// bead-show data map when the bead is a gardener (type=trigger + role:gardener,
// or a legacy type=schedule bead carrying a gardener spec). It mirrors
// handleGardenerDetail's header + iteration pipeline so the two surfaces never
// drift: build the header, re-derive the live goal-state badge, fetch the
// ordered life-thread, reconcile the authoritative run counters, and hand the
// template a capped run preview.
//
// Best-effort throughout: a header-build failure leaves the page to fall back
// to the generic field dump (IsGardener stays false); an iterations failure
// degrades the preview to "no runs yet" while still rendering the rich header.
func (s *Server) injectGardenerBeadView(ctx context.Context, data map[string]any, bead *beadsapi.BeadDetail) {
	if !isGardenerBeadView(bead) {
		return
	}
	// Foreign-namespace schedule-mirror artifacts and E2E fixtures are excluded
	// from the gardener detail surface (bug ka-0000002811 / ka-0000008054); keep
	// the bead view consistent so a mirror bead shows its plain field list, not
	// a synthetic gardener header.
	if isScheduleMirrorGardenerBead(bead) || excludeFixtureGardener(bead) {
		return
	}

	header, err := s.gardenerViewFromBead(ctx, bead)
	if err != nil {
		s.logger.Debug("gardener bead view: build header", "id", bead.ID, "error", err)
		return
	}
	s.populateLiveGoalState(ctx, &header, bead)

	now := s.now()
	iters, truncated, ierr := s.gardenerIterations(ctx, bead.ID, now)
	if ierr != nil {
		s.logger.Debug("gardener bead view: iterations", "id", bead.ID, "error", ierr)
	}

	header.IterationCount = runIterationCount(iters)
	header.DidNotRunCount = countDidNotRun(iters)
	header.LiveCount = countLiveIterations(iters)
	header.HistoryTruncated = truncated
	// A transient iterations-fetch failure collapses IterationCount to 0; flag
	// it so reconcileCounters does not mis-frame the gap against a real
	// RunsSpawned as "older runs pruned" (bug ka-0000006095 / A2).
	header.IterationsUnavailable = ierr != nil
	header.reconcileCounters()

	// Overdue / silent-gardener liveness (bug ka-0000006095 / A1). Same as the
	// detail page: derive from the freshest last_progress_at across the fetched
	// thread; skip on a transient fetch error.
	if ierr == nil {
		header.computeLiveness(freshestProgress(iters), now)
	}

	preview, hasMore := iters, false
	if len(iters) > gardenerBeadViewRuns {
		preview = iters[:gardenerBeadViewRuns]
		hasMore = true
	}

	data["IsGardener"] = true
	data["Gardener"] = header
	data["GardenerRuns"] = preview
	data["GardenerHasMoreRuns"] = hasMore
}
