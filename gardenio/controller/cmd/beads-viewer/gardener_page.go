package main

// Gardener Page route + handlers (epic ka-0000001579, task ka-0000001207,
// design doc ka-0000003219 §4).
//
// GET /gardener/{id}        — eager page: gardener header + life-thread of the
//                            most-recent iteration cards.
// GET /gardener/{id}/runs   — lazy "older runs" section, revealed by HTMX once
//                            the eager thread scrolls into view.
//
// Both are reads → viewer-scoped (plain mux.HandleFunc, gated by the ambient
// auth middleware exactly like /u/{slug} and /agents). No operator-only control
// actions live on the gardener page in v1; enable/disable stays on /triggers.
//
// The header view-model (gardenerView) + the per-run model (gardenerIterationView)
// and the ordering authority come from gardener.go / gardener_timeline.go. This
// file is purely the HTTP + render glue: resolve the gardener bead, build the
// header, fetch the ordered iterations, split into an eager head and a lazy
// tail, and render.

import (
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// gardenerEagerRuns caps how many iteration cards the eager /gardener/{id} page
// renders inline. The remainder are deferred to the lazy /gardener/{id}/runs
// section (design doc §4 — mirror /u/{slug}'s eager-head + lazy-tail split).
// The header's IterationCount always reflects the full thread, so the count
// stays honest even when the inline thread is truncated.
const gardenerEagerRuns = 25

// resolveLiveGardenerBead fetches the gardener bead for id and enforces the
// live-surface visibility rules shared by the eager /gardener/{id} page and the
// lazy /gardener/{id}/runs section: a missing bead, an E2E fixture gardener (bug
// ka-0000008054 — its synthetic runs must not render as a real life-thread), or
// a foreign schedule-mirror artifact (bug ka-0000002811) all 404. On any
// non-404 daemon error it writes a 500. Callers stop on ok=false — the response
// has already been written.
//
// Routing BOTH gardener handlers through this closes a monitoring blind spot
// surfaced by the gardener-page CDQR audit (ka-0000007587): GET /gardener/{id}
// 404'd an absent/fixture/mirror gardener, but GET /gardener/{id}/runs skipped the
// existence check and returned 200 with an empty section — so a liveness probe
// hitting the /runs fragment alone read green for a gardener that does not exist,
// and the fragment was inconsistent with its own parent page.
func (s *Server) resolveLiveGardenerBead(w http.ResponseWriter, r *http.Request, id string) (*beadsapi.BeadDetail, bool) {
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Gardener not found", http.StatusNotFound)
			return nil, false
		}
		s.logger.Error("gardener: get bead", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return nil, false
	}
	if bead == nil || excludeFixtureGardener(bead) || isScheduleMirrorGardenerBead(bead) {
		http.Error(w, "Gardener not found", http.StatusNotFound)
		return nil, false
	}
	return bead, true
}

// handleGardenerDetail renders the eager /gardener/{id} page: the gardener header
// plus the most-recent gardenerEagerRuns iteration cards (newest-first), with a
// lazy shell that reveals the older runs on scroll.
//
// Legacy compatibility (design doc §11): older gardeners may be type=schedule
// rather than type=trigger + trigger:schedule. gardenerViewFromBead reads the
// `gardener` JSON when present and otherwise renders identity/status from the
// bead surface, so a type=schedule bead still produces a valid (sparser)
// header — no special-casing needed here.
func (s *Server) handleGardenerDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing gardener id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	bead, ok := s.resolveLiveGardenerBead(w, r, id)
	if !ok {
		return
	}

	now := s.now()
	header, err := s.gardenerViewFromBead(ctx, bead)
	if err != nil {
		s.logger.Error("gardener detail: build header", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Re-derive the goal-state badge LIVE from the target epic's current status
	// (bug ka-0000007772) so an epic that CLOSED (or re-OPENED) after the
	// gardener's close is reflected, instead of the stale close-time snapshot.
	s.populateLiveGoalState(ctx, &header, bead)

	// Fetch + order the full life-thread. A lookup failure is soft: render the
	// header with an empty thread and a surfaced error rather than a 500, so the
	// operator still sees the gardener's identity/goal/counters.
	iters, truncated, ierr := s.gardenerIterations(ctx, id, now)
	if ierr != nil {
		s.logger.Error("gardener detail: iterations", "id", id, "error", ierr)
	}

	// The iteration count is authoritative: it equals the number of REAL runs
	// in the life-thread below — every rendered card EXCEPT never-ran spawn
	// shells (agent_state="expired", queued spawns the reconciler killed before
	// they claimed a pod). Those shells are still rendered (muted "did not run")
	// but must not inflate the run count (bug ka-0000006805). Reconcile against
	// the engine snapshot counter (RunsSpawned) so a stale spec value surfaces
	// as a subtle drift annotation instead of a silently-wrong number
	// (ka-0000002549).
	header.IterationCount = runIterationCount(iters)
	header.DidNotRunCount = countDidNotRun(iters)
	header.LiveCount = countLiveIterations(iters)
	header.HistoryTruncated = truncated
	// A transient iterations-fetch failure collapses IterationCount to 0; flag
	// it so reconcileCounters does not mis-frame the resulting gap against a
	// real RunsSpawned as "older runs pruned" (bug ka-0000006095 / A2).
	header.IterationsUnavailable = ierr != nil
	header.reconcileCounters()

	// Overdue / silent-gardener liveness (bug ka-0000006095 / A1): flag an
	// enabled gardener that should have fired again and did not, or is still
	// spawning but making no progress. The last-progress heartbeat is the
	// freshest last_progress_at across the life-thread we just fetched — no
	// extra round-trip. Skipped on a transient iterations failure, where a
	// spuriously-empty thread would read as "no progress" (mirrors the A2 guard).
	if ierr == nil {
		header.computeLiveness(freshestProgress(iters), now)
	}

	// C3 (bug ka-0000001009): a live iteration's cost is not yet in the
	// gardener_spend_usd roll-up (total_cost_usd lands only at session close), so
	// the header $ chip under-reports while a run is in flight. Now that the
	// live-run count is known, flag the chip approximate + annotate the exclusion
	// — but only when there IS a rolled-up figure to qualify.
	if header.LiveCount > 0 && header.HasSpend {
		header.SpendExcludesLiveRun = true
	}

	// Split newest-first thread into an eager head + a lazy tail.
	eager, hasMore := iters, false
	if len(iters) > gardenerEagerRuns {
		eager = iters[:gardenerEagerRuns]
		hasMore = true
	}

	// Operator-gated write/config surface (task ka-0000003713). Only viewers
	// holding the operator role AND an authenticated identity get the lifecycle
	// control bar + Edit-config affordance; everyone else sees the gardener's
	// state with no live-looking levers. viewerCanOperateAuthed mirrors the
	// mutation POST contract EXACTLY (operatorOnly route gate + the handlers'
	// email!="" 401 guard), so the page never advertises a lever the matching
	// POST would refuse — the render-gate ≠ route-gate dead-end this fixes
	// (bug ka-0000005377). A non-operator instead gets a muted "operator-only"
	// note (IsOperator false → OperatorNote true) rather than dead buttons.
	isOperator := viewerCanOperateAuthed(r)

	data := map[string]any{
		"Gardener":     header,
		"GardenHref":   s.gardenBanner(r, retireGardener, gardenGardenerTarget),
		"Iterations":   eager,
		"HasMoreRuns":  hasMore,
		"RunsLazyURL":  s.basePath + "/gardener/" + id + "/runs",
		"IsOperator":   isOperator,
		"OperatorNote": !isOperator,
	}
	if isOperator {
		data["Controls"] = gardenerControlsViewFromBead(bead)
	}
	if ierr != nil {
		data["IterationsError"] = "Could not load this gardener's runs right now."
	}
	s.render(w, r, "gardener_detail.html", data)
}

// handleGardenerRunsSection renders ONE bounded page of the lazy "older runs"
// tail (the iteration cards beyond the eager head), keyed on a stateless
// (schedule_key, id) cursor passed as ?cursor= (design doc §7/§8, task
// ka-0000006972).
//
// It re-runs the (cheap, indexed) iteration lookup, re-derives the canonical
// newest-first ordering, locates the cursor anchor, and serves the next
// gardenerRunsPageSize cards. When more remain, it emits a continuation cursor so
// the partial can request the following page on reveal. No server-side page
// state: an absent cursor means "first tail page" (resume at the eager-head
// boundary), and a stale/junk cursor degrades to that same first page rather
// than erroring. Replaces the previous "slice the entire tail in one response"
// approach so the reveal is bounded per request even if bead retention grows.
func (s *Server) handleGardenerRunsSection(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	// Enforce the same existence/fixture/mirror 404 guard as the eager page so
	// the lazy fragment can never render (empty) for a gardener the parent page
	// 404s — see resolveLiveGardenerBead (ka-0000007587).
	if _, ok := s.resolveLiveGardenerBead(w, r, id); !ok {
		return
	}
	ctx := r.Context()

	iters, _, err := s.gardenerIterations(ctx, id, s.now())
	if err != nil {
		s.logger.Error("gardener runs section: iterations", "id", id, "error", err)
		// Render an empty section rather than a 500 — the eager page already
		// loaded; a failed reveal should degrade quietly.
		s.renderPartial(w, r, "templates/partials/gardener_runs_section.html", "gardener_runs_section",
			map[string]any{"Iterations": []gardenerIterationView{}})
		return
	}

	// Parse the continuation cursor. An empty/malformed token resolves to nil,
	// which runsPage treats as "start at the eager-head boundary" — so the first
	// reveal (no ?cursor=) and a stale cursor both land on the first tail page.
	var cur *runsCursor
	if c, ok := decodeRunsCursor(r.URL.Query().Get("cursor")); ok {
		cur = &c
	}

	page, next := runsPage(iters, cur)
	data := map[string]any{"Iterations": page}
	if next != nil {
		// Hand back the next page's cursor so the partial can reveal-load it.
		data["HasMoreRuns"] = true
		data["RunsLazyURL"] = s.basePath + "/gardener/" + id + "/runs?cursor=" + next.encode()
	}
	s.renderPartial(w, r, "templates/partials/gardener_runs_section.html", "gardener_runs_section", data)
}

// countLiveIterations counts the in-flight (non-closed) iterations in a thread,
// for the header's LiveCount chip.
func countLiveIterations(iters []gardenerIterationView) int {
	n := 0
	for _, it := range iters {
		if it.IsLive {
			n++
		}
	}
	return n
}

// runIterationCount counts the REAL runs in a thread — every iteration except
// never-ran spawn shells (DidNotRun: queued spawns the reconciler expired
// before they claimed a pod). This is the authoritative "Iterations N" the
// header advertises: a gardener whose only "extra" beads are expired spawns must
// not have those inflate its run count (bug ka-0000006805). The shells are
// still rendered in the thread (muted "did not run") so the operator can see
// they happened — they are simply not counted as runs.
func runIterationCount(iters []gardenerIterationView) int {
	n := 0
	for _, it := range iters {
		if !it.DidNotRun {
			n++
		}
	}
	return n
}

// countDidNotRun counts the never-ran spawn shells in a thread, for the
// header's optional "(+N never ran)" annotation beside the Iterations chip.
func countDidNotRun(iters []gardenerIterationView) int {
	n := 0
	for _, it := range iters {
		if it.DidNotRun {
			n++
		}
	}
	return n
}
