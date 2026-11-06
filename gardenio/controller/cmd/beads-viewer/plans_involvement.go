package main

// plans_involvement.go — the reusable "plans this operator is INVOLVED in" query
// (Step 1, task ka-0000008064; decision ka-0000000098 resolved 'involved').
//
// "Involved in" is the UNION of four facets, so a plan an operator actively
// drives but did not originate is no longer invisible in their default /plans
// view:
//
//	A. creator          — the plan epic's operated_by resolves to X
//	B. step-owner       — X owns a rendered step (an issue-kind child) of the plan
//	C. decision-resolver — X resolved one of the plan's clarifying-question decisions
//	D. gardener-owner    — X owns the plan's bound gardener (a role:gardener trigger)
//
// The load-bearing subtlety is operator IDENTITY. A human carries several aliases
// (a plan epic is stamped `slack:U…`, the decision they resolved records
// `oidc:<email>`), and the beads' operated_by_id FK is frequently NULL on the
// list projection — so client-side FK/string matching cannot reliably recognise
// "these two beads belong to the same human." The daemon's operator_aliases table
// is the only authoritative resolver: passing ONE alias in OperatedByAliases
// resolves it to the operators(id) FK and matches every bead operated by that
// human under ANY alias. So facets A/B/D lean on the daemon alias filter rather
// than matching in Go; only facet C (decisions) matches the canonical
// `responded_by` string, which is always stored as `oidc:<email>`.
//
// Shape mirrors the blessed multi-facet precedent (inbox.go listInboxBeadsForViewer,
// gardener.go gardenerIterations): the daemon has no single OR across operated_by /
// parent / field predicates, so we issue parallel scoped queries and dedup by id.
// The creator facet is authoritative (its error fails the surface); the other
// three are best-effort (a facet's daemon error is logged and contributes
// nothing, never failing the page — degrade toward the narrower creator view,
// never toward an error). Reused by the /plans cold-load default here and, per the
// plan (ka-0000004110), by the /u/{slug} "their plans" section (Step 3) and the
// /plans discover facet (Step 5).

import (
	"context"
	"slices"
	"sort"

	"gardenio/controller/internal/beadsapi"
)

// involvementFacetScanLimit caps each best-effort facet scan (steps X owns,
// decisions X resolved, gardeners X owns). An operator can own many issues
// fleet-wide, so the step-owner scan in particular is bounded here — the vast
// majority of an operator's involvement is the creator facet, and a step owned
// in someone else's plan is the long tail, so a generous-but-finite cap keeps the
// scan a bounded number of round-trips. Truncation is logged, not silent.
const involvementFacetScanLimit = 500

// plansInvolvingOperator returns the plan epics operator `alias` is involved in
// (creator ∪ step-owner ∪ decision-resolver ∪ gardener-owner). baseQuery carries
// the shared plan universe constraints already assembled by the caller —
// Types=[epic], Labels=[plan] (plus any project: scope), Statuses, Sort, Limit,
// ExcludeScheduled — and is composed across the four facets; the returned result
// is deduped by id, sorted newest-created-first, and capped to baseQuery.Limit,
// so it is a drop-in for the single ListBeadsFiltered the caller would otherwise
// issue.
//
// alias is the resolved operator id (e.g. "oidc:matt@x" or "slack:U123"). It is
// forwarded verbatim to the daemon alias filter for facets A/B/D and matched
// against the decisions' `responded_by` for facet C.
//
// Only the creator facet's error is returned (it is the authoritative "mine"
// set and the historical behaviour); the other facets degrade silently to
// nothing so a transient daemon hiccup narrows the view rather than erroring it.
func (s *Server) plansInvolvingOperator(ctx context.Context, baseQuery beadsapi.ListBeadsQuery, alias string) (*beadsapi.ListBeadsResult, error) {
	// Facet A — creator. The base query scoped by the operator alias returns the
	// plan epics directly (this IS the historical "mine == created-by-me" query),
	// already plan/status/project/limit constrained. Its error fails the surface.
	creatorQ := baseQuery
	creatorQ.OperatedByAliases = []string{alias}
	creatorRes, err := s.daemon.ListBeadsFiltered(ctx, creatorQ)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	out := make([]*beadsapi.BeadDetail, 0)
	creatorTotal := 0
	if creatorRes != nil {
		creatorTotal = creatorRes.Total
		for _, b := range creatorRes.Beads {
			if b == nil {
				continue
			}
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			out = append(out, b)
		}
	}

	// Facets B/C/D — gather the ADDITIONAL plan-epic ids the operator drives but
	// did not create. Each facet resolves to a set of candidate epic ids; the
	// creator ids are already covered, so drop them here to avoid a redundant
	// materialize.
	extra := make(map[string]struct{})
	addExtra := func(ids map[string]struct{}) {
		for id := range ids {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue // already a creator hit
			}
			extra[id] = struct{}{}
		}
	}
	addExtra(s.plansWithStepOwnedBy(ctx, alias))        // B
	addExtra(s.plansWithDecisionResolvedBy(ctx, alias)) // C
	addExtra(s.plansGardeneredBy(ctx, alias))           // D

	// Materialize the extra candidates in one batch and keep only those that are
	// genuine plans matching the SAME universe constraints the creator query
	// applied (type=epic, plan label + any project: label, status set). A step /
	// decision / gardener can point at a non-plan epic, a closed plan while the
	// view is scoped active, or a different project — all correctly dropped here.
	if len(extra) > 0 {
		ids := make([]string, 0, len(extra))
		for id := range extra {
			ids = append(ids, id)
		}
		beads, berr := s.daemon.BatchGetBeads(ctx, ids)
		if berr != nil {
			s.logger.Debug("plans involvement: batch get extra epics", "error", berr)
		} else {
			for _, id := range ids {
				b := beads[id]
				if b == nil || !planMatchesBaseConstraints(b, baseQuery) {
					continue
				}
				if _, dup := seen[b.ID]; dup {
					continue
				}
				seen[b.ID] = struct{}{}
				out = append(out, b)
			}
		}
	}

	// Newest-created-first, matching the base query's Sort=-created_at so the
	// merged set reads in the same order as the pure creator list would.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})

	// Total is the count of DISTINCT involved plans, floored by the daemon's
	// creator-facet total (which may exceed the rows it returned when the daemon
	// itself capped the creator query) — so the caller's `Total > len(Beads)`
	// truncation banner fires when either the creator facet or the merged set was
	// clipped. Then cap the returned rows to the base query's page limit
	// (Limit <= 0 means unbounded).
	total := max(len(out), creatorTotal)
	if lim := baseQuery.Limit; lim > 0 && len(out) > lim {
		out = out[:lim]
	}
	total = max(total, len(out))
	return &beadsapi.ListBeadsResult{Beads: out, Total: total}, nil
}

// plansWithStepOwnedBy resolves facet B: the plan epics that have a rendered
// step (issue-kind child) whose operated_by resolves to `alias`. Scans the
// operator's issue-kind beads (bounded, alias-filtered so the daemon does the
// cross-alias FK match), then batch-maps each to its parent-child parent — the
// candidate plan epic. Best-effort: a daemon error yields the empty set.
//
// The scan is deliberately not restricted to "plan steps" (there is no marker
// label distinguishing a plan step from any other issue) — non-plan parents are
// dropped downstream when plansInvolvingOperator materializes and constraint-
// checks each candidate epic.
func (s *Server) plansWithStepOwnedBy(ctx context.Context, alias string) map[string]struct{} {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:             []string{"issue"},
		OperatedByAliases: []string{alias},
		Sort:              "-created_at",
		Limit:             involvementFacetScanLimit,
	})
	if err != nil {
		s.logger.Debug("plans involvement: step-owner scan", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	if res.Total > len(res.Beads) {
		s.logger.Debug("plans involvement: step-owner scan truncated",
			"alias", alias, "scanned", len(res.Beads), "total", res.Total)
	}
	stepIDs := make([]string, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b != nil && b.ID != "" {
			stepIDs = append(stepIDs, b.ID)
		}
	}
	return s.parentPlanEpicIDs(ctx, stepIDs)
}

// plansWithDecisionResolvedBy resolves facet C: the plan epics with a
// clarifying-question decision `alias` resolved. The resolver identity is
// recorded in the decision bead's `responded_by` field, always stored as the
// canonical `oidc:<email>` (kbeads derives it from the authenticated operator,
// never the client body). We match that field directly rather than via the alias
// FK filter, because `responded_by` is a plain string column, not an operator
// FK. Each matched decision is then batch-mapped to its parent plan epic.
func (s *Server) plansWithDecisionResolvedBy(ctx context.Context, alias string) map[string]struct{} {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"decision"},
		Fields: map[string]string{"responded_by": alias},
		Sort:   "-created_at",
		Limit:  involvementFacetScanLimit,
	})
	if err != nil {
		s.logger.Debug("plans involvement: decision-resolver scan", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	decisionIDs := make([]string, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		// Defense-in-depth: a lax fake (or a future fuzzy field match) could
		// surface a decision this operator did NOT resolve. Re-check the field
		// locally so only genuine resolver hits map to a plan.
		if b.Fields["responded_by"] != alias {
			continue
		}
		decisionIDs = append(decisionIDs, b.ID)
	}
	return s.parentPlanEpicIDs(ctx, decisionIDs)
}

// plansGardeneredBy resolves facet D: the plan epics whose bound gardener `alias`
// owns. A plan gardener is a role:gardener trigger/schedule bead pinned to its epic
// by a `gardener-epic:<id>` label (or the legacy assign_bead field), so the epic
// id is read straight off the gardener bead (gardenerOwnedEpicID) — no parent walk.
// The alias filter scopes to gardeners this operator owns. Best-effort.
func (s *Server) plansGardeneredBy(ctx context.Context, alias string) map[string]struct{} {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:             []string{"trigger", "schedule"},
		Labels:            []string{"role:gardener"},
		OperatedByAliases: []string{alias},
		Sort:              "-updated_at",
		Limit:             involvementFacetScanLimit,
	})
	if err != nil {
		s.logger.Debug("plans involvement: gardener-owner scan", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	out := make(map[string]struct{})
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if epicID := gardenerOwnedEpicID(b); epicID != "" {
			out[epicID] = struct{}{}
		}
	}
	return out
}

// parentPlanEpicIDs batch-maps a set of child bead ids (steps or decisions) to
// their parent-child PARENT ids — the candidate plan epics. Parent-child edges
// are stored child→parent, so a forward-dependency query on the child returns the
// parent as DependsOnID. Returns the deduped parent-id set; the caller
// materializes and constraint-checks them (so a parent that is not actually a
// plan is dropped there). Best-effort: a daemon error yields nothing.
func (s *Server) parentPlanEpicIDs(ctx context.Context, childIDs []string) map[string]struct{} {
	out := make(map[string]struct{})
	if len(childIDs) == 0 {
		return out
	}
	depsByChild, err := s.daemon.BatchGetDependencies(ctx, childIDs, "forward")
	if err != nil {
		s.logger.Debug("plans involvement: child→parent walk", "error", err)
		return out
	}
	for _, cid := range childIDs {
		for _, d := range depsByChild[cid] {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				out[d.DependsOnID] = struct{}{}
			}
		}
	}
	return out
}

// planMatchesBaseConstraints reports whether a materialized epic satisfies the
// SAME universe constraints the creator ListBeadsFiltered applied — so a facet
// B/C/D candidate is admitted only if it is a real plan the pure creator query
// would also have been allowed to return. Checks: type=epic, ALL of baseQuery's
// labels present (the `plan` marker plus any `project:<name>` scope), and status
// in baseQuery's status set (empty set = any status, mirroring the daemon).
func planMatchesBaseConstraints(b *beadsapi.BeadDetail, baseQuery beadsapi.ListBeadsQuery) bool {
	if b == nil || b.Type != "epic" {
		return false
	}
	for _, want := range baseQuery.Labels {
		if !hasLabel(b, want) {
			return false
		}
	}
	if len(baseQuery.Statuses) > 0 && !slices.Contains(baseQuery.Statuses, b.Status) {
		return false
	}
	return true
}
