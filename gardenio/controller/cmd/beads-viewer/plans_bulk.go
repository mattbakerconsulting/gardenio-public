package main

import (
	"fmt"
	"net/http"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plans_bulk.go — bulk multi-select verbs ON the EXISTING /plans index. Filter
// the /plans list (operated_by + project + status + group), "Select all" the
// rows project_bulk.js sees in the DOM (which — because handlePlans renders ONLY
// the filtered rows — is exactly the currently-filtered plans), and apply one
// verb to every one in a single POST.
//
// Three verbs, one selection model:
//
//   - enable / disable the bound plan GARDENER (task ka-0000004771, epic
//     ka-0000004792) — the original control-room "disable all of Matt's monorepo
//     gardeners" ask, a near-drop-in adaptation of the shipped /triggers bulk
//     pattern (triggers_bulk.go). Plan gardeners ARE trigger/schedule beads, so
//     the per-bead mutation delegates to setGardenerEnabledCore (gardener_ops.go),
//     the SAME shared core the /gardener/{id} control bar and the /plans row
//     toggle use — the enable semantics are never forked, and enabling a
//     still-authoring plan's gardener stays behind the R6 build gate (the index is
//     not a bypass).
//   - Mark complete the PLAN (task ka-0000005361; Matt's ask, thread
//     ka-0000001785: "a better way to close out plans so we dont have to look at
//     every plan we've every made" — decision ka-0000001273 chose build-now).
//     Retire 20 drained plans in one POST instead of 20 clicks. The per-plan
//     mutation delegates to completePlanCore (plans_lifecycle.go), the SAME
//     disable-gardener → stamp complete → close-epic composition the single-plan
//     handler and `gb plan complete` perform — so the terminal ordering and the
//     safe-failure direction are never forked.
//
// ── THE RE-KEY (ka-0000005361) ────────────────────────────────────────────────
// The checkbox value is now the PLAN-EPIC id. It was originally the bound
// GARDENER trigger-bead id, which meant the checkbox rendered only on a plan with
// a non-retired gardener bound — and 48 of the 56 live open plans have NO bound
// gardener (measured 2026-08-12), so the selection model was invisible on 86% of
// the very pile the close-out ask is about. Since "Mark complete" is a verb on
// the PLAN and every plan has an epic id, keying on the epic makes one selection
// serve all three verbs. The enable/disable handlers therefore resolve epic →
// gardener server-side (planGardener) instead of receiving the gardener id directly;
// a selected plan with no gardener is reported as such rather than silently
// skipped, so "select all + disable" cannot quietly no-op on most of the board.
// ─────────────────────────────────────────────────────────────────────────────

// maxBulkPlans caps how many plans one bulk request will touch. Same
// defense-in-depth motivation as the trigger/project bulk caps — keeps a runaway
// form post from fanning out into hundreds of daemon RPCs.
const maxBulkPlans = 200

// handlePlansBulkEnable flips `enabled: true` across every gardener bead listed
// in the `bead_id` form values, behind the R6 build gate.
func (s *Server) handlePlansBulkEnable(w http.ResponseWriter, r *http.Request) {
	s.handleBulkTogglePlanGardeners(w, r, true)
}

// handlePlansBulkDisable is the symmetric counterpart for `enabled: false` — the
// headline "disable all of Matt's monorepo gardeners" verb. Disable is NOT close:
// closed gardeners are filtered out at the per-bead guard and the action never
// touches bead status.
func (s *Server) handlePlansBulkDisable(w http.ResponseWriter, r *http.Request) {
	s.handleBulkTogglePlanGardeners(w, r, false)
}

// handleBulkTogglePlanGardeners is the shared implementation for the two gardener
// bulk endpoints. It mirrors handleBulkToggleTriggers: ParseForm, dedupe
// bead_id[], cap at maxBulkPlans, then per PLAN resolve the bound gardener and
// REUSE setGardenerEnabledCore.
//
// Partial-failure policy: no rollback (identical to the trigger bulk). Successful
// flips are kept committed; the first failure is surfaced as an inline error on
// the re-rendered toolbar and the affected plan rows OOB-swap so the unified
// loop+agent chip reflects the new state without a refresh.
func (s *Server) handleBulkTogglePlanGardeners(w http.ResponseWriter, r *http.Request, enable bool) {
	verb := "disable"
	if enable {
		verb = "enable"
	}
	s.runPlansBulk(w, r, verb, func(r *http.Request, epic *beadsapi.BeadDetail, email string) string {
		// Resolve epic → gardener (the checkbox now carries the EPIC id; see the
		// re-key note in the file header). A plan with no gardener is REPORTED, not
		// silently skipped: since most open plans have none, a silent skip would
		// make "select all + disable" look like it worked while touching nothing.
		gardener := s.planGardener(r.Context(), epic.ID)
		if gardener == nil {
			return fmt.Sprintf("%s has no gardener bound.", epic.ID)
		}
		// A closed/retired gardener has nothing to flip. setGardenerEnabledCore also
		// guards this, but the explicit check yields a per-id message (matching the
		// trigger bulk) instead of a generic failure.
		if statusOpenSortKey(gardener.Status) != 0 {
			s.logger.Warn("bulk plan-gardener toggle: closed gardener skipped", "epic", epic.ID, "gardener", gardener.ID, "actor", email)
			return fmt.Sprintf("%s's gardener is closed.", epic.ID)
		}
		// R6 build gate (task ka-0000005350): a still-authoring plan (draft/ready)
		// must not have a running gardener, so a half-baked plan never auto-builds.
		// Gate the ENABLE direction only — pausing is always allowed, and an
		// ungated (unset) plan_state toggles exactly as before. This keeps the bulk
		// path from becoming a build-gate bypass the single-row toggle refuses.
		// The epic is already in hand here (the loop loaded it), so unlike the
		// pre-re-key version this costs no second GetBead.
		if enable && gardenerEnableGatedByState(planStateOf(epic)) {
			s.logger.Warn("bulk plan-gardener toggle: enable gated by plan_state", "epic", epic.ID, "actor", email)
			return fmt.Sprintf("%s: approve the plan for build before starting its gardener.", epic.ID)
		}
		if opErr := s.setGardenerEnabledCore(r, gardener, enable, email); opErr != nil {
			s.logger.Error("bulk plan-gardener toggle: update", "epic", epic.ID, "gardener", gardener.ID, "actor", email, "verb", verb, "error", opErr.Message)
			return fmt.Sprintf("Failed to update %s.", epic.ID)
		}
		return ""
	})
}

// handlePlansBulkComplete is the close-out verb (task ka-0000005361; Matt's ask,
// thread ka-0000001785): Mark complete across every selected plan in ONE POST,
// so draining the Done? lane is one gesture instead of one click per plan.
//
// Each plan goes through completePlanCore — the SAME disable-gardener → stamp
// complete → close-epic composition the single-plan handler and `gb plan
// complete` perform — so the bulk path is not a second, forkable implementation
// of "what it means to finish a plan".
//
// The LEGALITY GATE is enforced per plan, not just in the UI: the daemon admits
// only building→complete (plus any edge from unset), so a selected draft/ready
// plan is refused HERE with a message naming its lifecycle rather than posting a
// write the daemon is guaranteed to 400. The card renders no Mark-complete lever
// on such a plan (planCompleteReachable), but "Select all" can absolutely put one
// in the form — the gate is what keeps a mixed selection honest instead of
// aborting the whole batch on one bad row. See plans_closeout.go's legality
// table for why draft/ready cannot reach complete and why `abandoned` (step
// ka-0000001377, deferred) is the real fix.
func (s *Server) handlePlansBulkComplete(w http.ResponseWriter, r *http.Request) {
	s.runPlansBulk(w, r, "complete", func(r *http.Request, epic *beadsapi.BeadDetail, email string) string {
		st := planStateOf(epic)
		// Already terminal — treat as success (idempotent), matching
		// handlePlanComplete's own early return. A double-submit or a plan another
		// tab just completed must not read as a failure.
		if st == planStateComplete && statusOpenSortKey(epic.Status) != 0 {
			return ""
		}
		if !planCompleteReachable(st) {
			s.logger.Warn("bulk plan complete: unreachable lifecycle", "epic", epic.ID, "plan_state", st, "actor", email)
			return fmt.Sprintf("%s is %s — approve it for build first (a drained pre-build plan has no legal move to complete).", epic.ID, st)
		}
		if err := s.completePlanCore(r, epic, email); err != nil {
			s.logger.Error("bulk plan complete: complete", "epic", epic.ID, "actor", email, "error", err)
			return fmt.Sprintf("Failed to complete %s.", epic.ID)
		}
		return ""
	})
}

// handlePlansBulkAbandon is the stale-sweep's retire verb (task ka-0000004351,
// epic ka-0000007470): Abandon every selected plan in ONE POST. It is the twin of
// handlePlansBulkComplete, and it is what makes the ?status=stale sweep genuinely
// useful — a stale idle plan is far more likely to be draft/ready (never approved
// for build) than a drained one, and draft/ready CANNOT legally reach complete
// (plans_closeout.go's legality table). Complete would refuse most of the sweep;
// abandon is the honest terminal for a plan given up on WITHOUT finishing, and the
// daemon accepts draft/ready/building→abandoned.
//
// Each plan goes through abandonPlanCore — the SAME disable-gardener → stamp
// abandoned → close-epic composition the single-plan lever and `gb plan abandon`
// perform — so the bulk path is not a second, forkable implementation.
//
// Legality is enforced per plan: an already-terminal plan (complete/abandoned) is
// treated as idempotent success rather than failing the batch, matching
// handlePlanAbandon's own early return. Every non-terminal working state is a legal
// abandon source, so — unlike bulk complete — no state is refused here.
func (s *Server) handlePlansBulkAbandon(w http.ResponseWriter, r *http.Request) {
	s.runPlansBulk(w, r, "abandon", func(r *http.Request, epic *beadsapi.BeadDetail, email string) string {
		st := planStateOf(epic)
		// Already terminal — treat as success (idempotent), matching
		// handlePlanAbandon's early return. A double-submit or a plan another tab
		// just retired must not read as a failure.
		if (st == planStateAbandoned || st == planStateComplete) && statusOpenSortKey(epic.Status) != 0 {
			return ""
		}
		if err := s.abandonPlanCore(r, epic, email); err != nil {
			s.logger.Error("bulk plan abandon: abandon", "epic", epic.ID, "actor", email, "error", err)
			return fmt.Sprintf("Failed to abandon %s.", epic.ID)
		}
		return ""
	})
}

// runPlansBulk is the shared spine for every /plans bulk verb: authenticate,
// ParseForm, dedupe + cap the selected PLAN-EPIC ids, load each epic once, apply
// the per-plan mutation, then re-render the cleared toolbar plus an OOB row for
// every plan that changed.
//
// apply returns "" on success or a human-facing message on refusal/failure; the
// FIRST such message becomes the toolbar's inline error (later ones are logged by
// the callee), matching the trigger/project bulk's first-error-wins convention.
// No rollback: successful mutations stay committed, which is the right failure
// direction for retire/pause verbs — a partial drain is progress, and re-running
// the same selection is idempotent for every verb here.
func (s *Server) runPlansBulk(w http.ResponseWriter, r *http.Request, verb string,
	apply func(r *http.Request, epic *beadsapi.BeadDetail, email string) string) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	// bead_id values are PLAN-EPIC ids (see the re-key note in the file header).
	ids := dedupeNonEmpty(r.PostForm["bead_id"])
	if len(ids) == 0 {
		s.renderPlansBulkBar(w, r, "Select at least one plan.", nil, http.StatusBadRequest)
		return
	}
	if len(ids) > maxBulkPlans {
		s.renderPlansBulkBar(w, r, fmt.Sprintf("Too many plans (max %d).", maxBulkPlans), nil, http.StatusRequestEntityTooLarge)
		return
	}

	updatedEpicIDs := make([]string, 0, len(ids))
	var firstErr string
	for _, id := range ids {
		epic, err := s.daemon.GetBead(r.Context(), id)
		if err != nil || epic == nil {
			s.logger.Error("bulk plan verb: load epic", "id", id, "actor", email, "verb", verb, "error", err)
			if firstErr == "" {
				firstErr = fmt.Sprintf("Failed to load %s.", id)
			}
			continue
		}
		// Guard: only a plan EPIC is a valid target. The checkbox renders only on
		// plan cards, but a stray id in the form must not silently mutate some
		// unrelated bead — and a non-epic can't be re-rendered as a plan row anyway.
		if epic.Type != "epic" {
			s.logger.Warn("bulk plan verb: non-epic bead skipped", "id", id, "type", epic.Type, "actor", email, "verb", verb)
			if firstErr == "" {
				firstErr = fmt.Sprintf("%s is not a plan.", id)
			}
			continue
		}
		if msg := apply(r, epic, email); msg != "" {
			if firstErr == "" {
				firstErr = msg
			}
			continue
		}
		updatedEpicIDs = append(updatedEpicIDs, epic.ID)
	}

	httpStatus := http.StatusOK
	if firstErr != "" && len(updatedEpicIDs) == 0 {
		httpStatus = http.StatusBadGateway
	}
	s.renderPlansBulkBar(w, r, firstErr, updatedEpicIDs, httpStatus)
}

// renderPlansBulkBar writes the cleared bulk toolbar plus OOB plan_card rows for
// every plan whose gardener was successfully flipped. The toolbar is the htmx
// target (#plans-bulk-bar, outerHTML swap); each row OOB-swaps into its own
// #plan-card-{epicID} so the unified loop+agent chip (and the paused/running
// switch skin) update live. Mirrors renderTriggersBulkBar.
//
// The affected plan rows are re-rendered through the SAME planIndexEnrich pass
// the page render uses (one batch call over all affected epics), so a row after
// a bulk flip can never disagree with a fresh page load.
func (s *Server) renderPlansBulkBar(w http.ResponseWriter, r *http.Request, errMsg string, updatedEpicIDs []string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}

	// Cleared toolbar (count=0; the inline error renders only when set). Lives at
	// id="plans-bulk-bar" so the htmx outerHTML swap drops it back into place.
	toolbarTmpl, ok := s.partials["templates/partials/plans_bulk_toolbar.html"]
	if !ok {
		s.logger.Error("plans_bulk_toolbar partial not registered")
		return
	}
	// Clone (via bindAgentLink) before executing: the parsed partial is shared
	// across Server instances (loadTemplateAssets), and html/template forbids
	// Clone after a direct execute.
	boundToolbar, err := s.bindAgentLink(toolbarTmpl, r)
	if err != nil {
		s.logger.Error("plans_bulk_toolbar bind", "error", err)
		return
	}
	if err := boundToolbar.ExecuteTemplate(w, "plans_bulk_toolbar", map[string]any{
		"BasePath": s.basePath,
		"ErrMsg":   errMsg,
	}); err != nil {
		s.logger.Error("plans_bulk_toolbar render", "error", err)
		return
	}
	if len(updatedEpicIDs) == 0 {
		return
	}

	// Load every affected plan epic (dedupe — normally one gardener per plan) and
	// enrich them all in ONE batch so the OOB rows are byte-identical to a page
	// render. A per-epic load failure is logged and its row silently dropped (the
	// operator can refresh to recover), matching the trigger bulk.
	epics := make([]*beadsapi.BeadDetail, 0, len(updatedEpicIDs))
	for _, epicID := range dedupeNonEmpty(updatedEpicIDs) {
		epic, err := s.daemon.GetBead(r.Context(), epicID)
		if err != nil || epic == nil {
			s.logger.Warn("re-fetching plan epic for OOB row", "epic", epicID, "error", err)
			continue
		}
		epics = append(epics, epic)
	}
	if len(epics) == 0 {
		return
	}

	cardTmpl, ok := s.partials["templates/partials/plan_card.html"]
	if !ok {
		s.logger.Error("plan_card partial not registered")
		return
	}
	bound, err := s.bindAgentLink(cardTmpl, r)
	if err != nil {
		s.logger.Error("plan_card bind for OOB bulk row", "error", err)
		return
	}
	root := map[string]any{
		"BasePath":    s.basePath,
		"ViewerEmail": oidcauth.EmailFromContext(r.Context()),
		"IsOperator":  viewerCanOperate(r),
		// The active ?project= filter of the /plans page these OOB rows splat back
		// into, read from the Referer, so each swapped card's project-chip
		// suppression (iter-60) matches its unswapped neighbours. Empty on an
		// unfiltered board (or a non-/plans Referer) → the chip renders as before.
		"ProjectFilter": s.refererPlansProject(r),
		// The active ?group= key of the /plans page these OOB rows splat back into
		// (iter-62), read from the Referer, so each swapped card's project-chip
		// suppression on a ?group=project board matches its unswapped neighbours.
		// Empty (no group) on an ungrouped board → the chip renders as before.
		"GroupKey": s.refererPlansGroup(r),
		// The resolved operator scope of the /plans page these OOB rows splat back
		// into (iter-61), recovered from the Referer with the SAME precedence
		// handlePlans uses, so each swapped card's creator-chip suppression matches
		// its unswapped neighbours. Empty on an "all"/anonymous board → renders as
		// before.
		"EffectiveOperatedBy": s.refererPlansOperatedBy(r),
	}
	for _, row := range s.planIndexEnrich(r.Context(), epics) {
		if err := bound.ExecuteTemplate(w, "planCard", map[string]any{
			"Root": root,
			"Row":  row,
			"OOB":  true,
		}); err != nil {
			s.logger.Error("plan_card OOB render", "id", row.ID, "error", err)
		}
	}
}
