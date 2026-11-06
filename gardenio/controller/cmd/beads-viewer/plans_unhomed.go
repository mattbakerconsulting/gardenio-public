package main

// plans_unhomed.go — Layer 1 of the orphaned-fork fix (design ka-0000006833 §3
// Layer 1, task ka-0000004983; closes bug ka-0000003246). It renders a global
// "Unhomed open decisions & escalations" section on the /plans INDEX — the
// surface operators actually watch.
//
// THE HOLE THIS BACKSTOPS: an open decision/escalation reaches a /plans surface
// through EITHER a parent-child path to a plan epic OR an open blocks-edge from a
// rendered step (the --requesting-task edge). A fork whose only surfacing edge was
// a blocks-edge from an ephemeral (cycle/session) bead silently orphans from every
// /plans surface the moment that bead closes — it still exists in the flat
// /decisions + /escalations inboxes, but operators watch /plans, not those
// inboxes (ka-0000004749 sat P0-open ~3.5 days this way). This section enumerates
// every open decision + escalation in scope and SUBTRACTS the ones already
// surfaced via an existing /plans path, so a fork that belongs to no plan and has
// no live operator surface pointing at it cannot hide.
//
// THE SUBTRACT REUSES THE EXISTING COLLECTORS (no re-implemented membership, so it
// can never drift from what the index renders):
//   - parent-child subtree decisions: planRowView.openDecisionIDs, collected off
//     the SAME batched child walk the "N to answer" chip counts (planIndexStepData
//     → countOpenDecisionChildren + countPhaseDecisionGrandchildren).
//   - escalation blocks-edge targets ∪ requesting_plan pin: planRowView.Escalations
//     (planEscalationsBulk already unions PATH 1 blocks-edge + PATH 2 pin per plan).
//   - the L2 requesting_plan pin for decisions: a decision whose
//     fields.requesting_plan names a known plan epic — the exact durable-homing rule
//     planDecisionBeads PATH 2 (Layer 2, ka-0000007801) reads, applied client-side
//     over the enumerated set against the known plan-id set.
//
// The known plan-id set is the rendered plans UNIONED with the unscoped plan
// universe the handler already fetches for the browse-by-person strip, so a fork
// durably pinned to a plan that is simply off the current (e.g. operator-scoped)
// view is NOT falsely flagged unhomed.
//
// COST: two bounded, project-scoped enumeration queries per index render (open
// decisions + open escalations, mirroring the flat-inbox query shapes), plus an
// in-memory set-subtract over already-fetched rows. No per-plan fan-out. Gate the
// section on non-empty so a healthy cockpit is unchanged (zero new noise).

import (
	"context"
	"sort"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Fork kinds rendered in the unhomed section.
const (
	unhomedKindDecision   = "decision"
	unhomedKindEscalation = "escalation"
)

// unhomedForkView is one open decision or escalation that belongs to no rendered
// plan and has no live operator surface pointing at it — the "would have hidden
// forever" row this backstop rescues onto /plans.
type unhomedForkView struct {
	ID    string
	Title string
	// Kind is unhomedKindDecision | unhomedKindEscalation — drives the row glyph
	// and the resolve deep-link target.
	Kind    string
	Project string
	// Ago is the humanized wait-age ("3 days ago") — age IS the urgency signal,
	// same convention as the /escalations queue and the plan decision chip.
	Ago string
	// Detail is a one-line secondary marker: the escalation's operation_type, or a
	// decision's requested_by. Empty renders nothing.
	Detail string
	// URL is where the row's title links: the flat decision card (/{id}) an
	// operator can resolve on, or the escalation review deep-link (falling back to
	// /{id} when ESCALATION_REVIEW_URL is unset).
	URL string
	// createdAt orders the section oldest-first; not rendered.
	createdAt time.Time
}

// unhomedForksView is the /plans index section view model. Count is the header
// badge total across both lists; the template renders the section only when
// Count > 0 (gate-on-non-empty is enforced at the handler by setting the data key
// only then, so a healthy cockpit adds no ink).
type unhomedForksView struct {
	Count       int
	Decisions   []unhomedForkView
	Escalations []unhomedForkView
	// Truncated is true when either enumeration hit its list cap, so the count is
	// a floor — surfaced honestly rather than silently clipping orphans.
	Truncated bool
}

// buildUnhomedForks enumerates the open decisions + escalations in scope and
// returns the remainder after subtracting every fork already surfaced on a
// rendered plan. rows are the enriched plan rows (they carry the reused surfaced
// sets: openDecisionIDs + Escalations). planIDs is the set of KNOWN plan epic ids
// (rendered ∪ unscoped universe) used for the L2 requesting_plan pin subtract.
// projectFilter honours the index ?project scope (server-side for decisions;
// client-side for escalations, mirroring handleEscalations). Best-effort: a
// daemon error on either enumeration degrades that half to empty — a backstop
// must never break the page it guards.
func (s *Server) buildUnhomedForks(ctx context.Context, rows []planRowView, planIDs map[string]struct{}, projectFilter string) unhomedForksView {
	// Surfaced sets, reused straight off the rendered rows (no re-query).
	surfacedDec := make(map[string]struct{})
	surfacedEsc := make(map[string]struct{})
	for i := range rows {
		for _, id := range rows[i].openDecisionIDs {
			surfacedDec[id] = struct{}{}
		}
		for _, e := range rows[i].Escalations.Escalations {
			surfacedEsc[e.ID] = struct{}{}
		}
	}
	// pinnedToKnownPlan reports whether a fork's requesting_plan names a plan we
	// know about — the L2 durable-homing subtract. Empty pin never matches.
	pinnedToKnownPlan := func(fields map[string]string) bool {
		rp := fields["requesting_plan"]
		if rp == "" {
			return false
		}
		_, ok := planIDs[rp]
		return ok
	}

	view := unhomedForksView{}

	// --- Open decisions ----------------------------------------------------
	// Mirror the flat /decisions inbox query shape (handleDecisions): open
	// decisions, newest-first, project-scoped server-side via the label filter.
	decQ := beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Sort:     "-created_at",
		Limit:    decisionsListLimit,
	}
	if projectFilter != "" {
		decQ.Labels = []string{"project:" + projectFilter}
	}
	if decRes, err := s.daemon.ListBeadsFiltered(ctx, decQ); err != nil {
		s.logger.Debug("unhomed forks: open-decision enumeration", "error", err)
	} else if decRes != nil {
		if len(decRes.Beads) >= decisionsListLimit {
			view.Truncated = true
		}
		for _, b := range decRes.Beads {
			if b == nil || b.ID == "" {
				continue
			}
			if _, dup := surfacedDec[b.ID]; dup {
				continue // parent-child subtree of a rendered plan
			}
			if pinnedToKnownPlan(b.Fields) {
				continue // L2 requesting_plan pin homes it to a known plan
			}
			requestedBy := b.Fields["requested_by"]
			if requestedBy == "" {
				requestedBy = b.CreatedBy
			}
			view.Decisions = append(view.Decisions, unhomedForkView{
				ID:        b.ID,
				Title:     b.Title,
				Kind:      unhomedKindDecision,
				Project:   projectFromLabels(b.Labels),
				Ago:       humanizeTime(b.CreatedAt),
				Detail:    requestedBy,
				URL:       s.basePath + "/" + b.ID,
				createdAt: b.CreatedAt,
			})
		}
	}

	// --- Open escalations --------------------------------------------------
	// Mirror the flat /escalations inbox (handleEscalations → ListEscalations):
	// open escalations, project filtered client-side (the daemon list endpoint has
	// no project param). row.Escalations already covers the surfaced set for both
	// the blocks-edge path AND the requesting_plan pin to a rendered plan; the
	// pinnedToKnownPlan check additionally covers a pin to an off-view universe
	// plan (planEscalationsBulk only buckets pins into on-page plans).
	if escs, err := s.daemon.ListEscalations(ctx, "open", escalationsListLimit); err != nil {
		s.logger.Debug("unhomed forks: open-escalation enumeration", "error", err)
	} else {
		if len(escs) >= escalationsListLimit {
			view.Truncated = true
		}
		for _, ed := range escs {
			b := ed.Issue
			if b == nil || b.ID == "" {
				continue
			}
			project := projectFromLabels(b.Labels)
			if projectFilter != "" && project != projectFilter {
				continue
			}
			if _, dup := surfacedEsc[b.ID]; dup {
				continue // blocks a rendered step, or pinned to a rendered plan
			}
			if pinnedToKnownPlan(b.Fields) {
				continue // L2 requesting_plan pin homes it to a known plan
			}
			// buildEscalationReviewURL falls back to the in-viewer bead page
			// ({basePath}/{id}) when ESCALATION_REVIEW_URL is unset.
			url := s.buildEscalationReviewURL(b.ID)
			view.Escalations = append(view.Escalations, unhomedForkView{
				ID:        b.ID,
				Title:     b.Title,
				Kind:      unhomedKindEscalation,
				Project:   project,
				Ago:       humanizeTime(b.CreatedAt),
				Detail:    b.Fields["operation_type"],
				URL:       url,
				createdAt: b.CreatedAt,
			})
		}
	}

	// Oldest-first within each list (age = urgency), so the >30-day orphan tail
	// leads — the exact class ka-0000004749 rotted in.
	sortUnhomedForks(view.Decisions)
	sortUnhomedForks(view.Escalations)
	view.Count = len(view.Decisions) + len(view.Escalations)
	return view
}

// sortUnhomedForks orders forks oldest-first; a zero createdAt sorts last so an
// unstamped fork never jumps the aged tail.
func sortUnhomedForks(forks []unhomedForkView) {
	sort.SliceStable(forks, func(i, j int) bool {
		if forks[i].createdAt.IsZero() {
			return false
		}
		if forks[j].createdAt.IsZero() {
			return true
		}
		return forks[i].createdAt.Before(forks[j].createdAt)
	})
}
