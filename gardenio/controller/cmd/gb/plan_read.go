// plan_read.go — daemon-direct READ helpers for `gb plan` (epic ka-0000003451,
// task ka-0000004997). Every `gb plan` WRITE verb (create/add-step/add-phase/
// complete/reopen) POSTs to the beads-viewer's operator-gated endpoints, which
// 401 from an agent pod against an OIDC-gated viewer (ka-0000006389). The READ
// path must NOT share that fate: a plan is a plain bead graph (an epic + its
// parent-child children + a phase_order/step_order overlay in fields), so it
// can be resolved straight from the daemon using the *beadsapi.Client `gb`
// already carries (main.go's `daemon`) — the same client `gb plan gardener`
// reads through today (plangardener.Bind → daemon.GetBead).
//
// This file ports the viewer's phase/step resolution (cmd/beads-viewer/
// plan_phases.go: planPhaseOrder, planStepOrder, sortStepBeads, resolvedPhaseIDs,
// planPhases) into package main here so `gb plan show` — and any future
// decoupled read verb — resolves a plan + phases + steps with only
// BEADS_AUTH_TOKEN, never a viewer URL. The pure helpers are split from the
// daemon round-trips so the ordering/membership logic is unit-testable without
// a server, and the composed reader takes a narrow interface (planDaemonReader)
// so a fake can drive the contract test (ka-0000004277).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"gardenio/controller/internal/beadsapi"
)

// planPhaseFieldKey / planStepFieldKey mirror the beads-viewer field names
// (cmd/beads-viewer/plan_phases.go) — the SAME plan-epic / phase-epic fields the
// browser cockpit reads, so the CLI resolves an identical timeline.
//
//   - phase_order: a JSON array of the plan's phase sub-epic ids, in display
//     order. It is BOTH membership and order — a direct-child epic renders as a
//     phase IFF its id is listed here (decision ka-0000001034).
//   - step_order: a JSON array giving an epic's explicit step display order. It
//     is ADVISORY (an unlisted step still renders, after every listed one) and
//     lives on whichever epic is the step's DIRECT parent — the plan epic for
//     flat steps, the phase sub-epic for phased steps (ka-0000007445).
const (
	planPhaseFieldKey = "phase_order"
	planStepFieldKey  = "step_order"
	// planStateFieldKey holds the plan's authoring lifecycle state
	// (draft/ready/building/complete). Mirrors the viewer's and plangardener's
	// same-named field; surfaced read-only so `gb plan show` reports it.
	planStateFieldKey = "plan_state"
)

// planStepRead is one resolved step (an issue-type child rendered on the plan),
// numbered 1..N within its group (the flat list or one phase). Heraldry, here
// and on the phase and plan rows, is the bead's arms in the canonical form, ""
// for none (heraldry spec §3.3); the text view prints its emoji run before the
// title.
type planStepRead struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Heraldry   string `json:"heraldry,omitempty"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Assignee   string `json:"assignee,omitempty"`
	Closed     bool   `json:"closed"`
	Number     int    `json:"number"`
}

// planPhaseRead is one resolved phase group: a direct-child sub-epic listed in
// the plan's phase_order, numbered 1..N in that order, plus its own steps.
type planPhaseRead struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	ShortTitle string         `json:"short_title,omitempty"`
	Heraldry   string         `json:"heraldry,omitempty"`
	Status     string         `json:"status"`
	Closed     bool           `json:"closed"`
	Number     int            `json:"number"`
	Steps      []planStepRead `json:"steps"`
	// Unreadable lists the phase's parent-child children the caller's token
	// cannot read (see planReadView.Unreadable).
	Unreadable []string `json:"unreadable,omitempty"`
}

// planReadView is the daemon-direct read model of a /plans plan: the plan epic,
// its ordered phases (each carrying its own steps), and the flat/unphased steps
// (direct issue children not claimed by any phase). It is the CLI counterpart of
// the viewer's planTimeline (steps + phases), resolved with no viewer round-trip.
type planReadView struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	ShortTitle string          `json:"short_title,omitempty"`
	Heraldry   string          `json:"heraldry,omitempty"`
	Status     string          `json:"status"`
	PlanState  string          `json:"plan_state,omitempty"`
	Project    string          `json:"project,omitempty"`
	Phases     []planPhaseRead `json:"phases,omitempty"`
	Steps      []planStepRead  `json:"steps,omitempty"`
	// Unreadable lists the plan's direct parent-child children the batch read
	// did not return: beads outside the caller's project scope, which the
	// daemon answers as missing. They are shown, not dropped, so a step that
	// no agent of the plan can read is seen (gd-BYCG4IGJB2).
	Unreadable []string `json:"unreadable,omitempty"`
}

// planDaemonReader is the narrow slice of *beadsapi.Client the plan read path
// needs. Pulling it out as an interface lets the resolver be unit-tested with a
// fake (no daemon), and documents the EXACT read surface a decoupled plan verb
// requires (task ka-0000004997): one GetBead, one reverse-deps walk per bead
// whose children we list, and batched fetches to collapse the fan-out.
type planDaemonReader interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
}

// *beadsapi.Client satisfies planDaemonReader — asserted at compile time so a
// signature drift on the client surfaces here, not at the (untyped) call site.
var _ planDaemonReader = (*beadsapi.Client)(nil)

// resolvePlanRead resolves a plan epic and its full step timeline straight from
// the daemon: the plan bead, its ordered phases (each with its steps), and the
// flat/unphased steps. It is the daemon-direct twin of the viewer's planTimeline
// (cmd/beads-viewer/plans.go) — same membership + ordering rules, no viewer URL.
//
// Cost: 1 GetBead + 1 reverse-deps + 1 BatchGetBeads for the plan's direct
// children, then (for a phased plan) 1 BatchGetDependencies(reverse) over the
// phase sub-epics + 1 BatchGetBeads over the union of their children — a small
// constant number of round-trips regardless of step count, matching planPhases.
func resolvePlanRead(ctx context.Context, r planDaemonReader, planID string) (*planReadView, error) {
	epic, err := r.GetBead(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("getting plan %s: %w", planID, err)
	}
	if epic == nil {
		return nil, fmt.Errorf("plan %s not found", planID)
	}
	if epic.Type != "epic" {
		return nil, fmt.Errorf("%s is a %s, not an epic — gb plan reads a plan epic (mark one with 'kd label add <id> plan')", planID, epic.Type)
	}

	view := &planReadView{
		ID:         epic.ID,
		Title:      epic.Title,
		ShortTitle: epic.ShortTitle,
		Heraldry:   epic.Heraldry,
		Status:     epic.Status,
		PlanState:  epic.Fields[planStateFieldKey],
		Project:    inheritProjectFromBead(epic),
	}

	// Direct parent-child children of the plan epic (the phase sub-epics + flat
	// steps + narrative bundle). A reverse-dep row reads "BeadID depends on
	// planID"; keep only the parent-child edges, deduped in daemon order.
	revDeps, err := r.GetReverseDependencies(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("listing children of plan %s: %w", planID, err)
	}
	directIDs := planParentChildIDs(revDeps)
	if len(directIDs) == 0 {
		return view, nil
	}
	directChildren, err := r.BatchGetBeads(ctx, directIDs)
	if err != nil {
		return nil, fmt.Errorf("fetching children of plan %s: %w", planID, err)
	}

	// Phases: the phase_order ids that resolve to a current direct-child epic
	// (resolvePlanPhaseIDs drops stale/typo/non-epic ids, deduped). An unphased
	// plan yields none, and every direct issue child falls through to the flat
	// list below — the pre-phase behavior.
	phaseStepIDs := map[string]struct{}{}
	if phaseIDs := resolvePlanPhaseIDs(planPhaseOrder(epic), directChildren); len(phaseIDs) > 0 {
		phases, claimed, perr := resolvePlanPhases(ctx, r, phaseIDs, directChildren)
		if perr != nil {
			return nil, perr
		}
		view.Phases = phases
		phaseStepIDs = claimed
	}

	// Flat/unphased steps: direct ISSUE children not already claimed by a phase,
	// ordered by the plan epic's own step_order overlay (its flat steps' order).
	flat := make([]*beadsapi.BeadDetail, 0, len(directIDs))
	for _, id := range directIDs {
		if _, inPhase := phaseStepIDs[id]; inPhase {
			continue
		}
		c := directChildren[id]
		if c == nil {
			view.Unreadable = append(view.Unreadable, id)
			continue
		}
		if !isPlanStepType(c.Type) {
			continue
		}
		flat = append(flat, c)
	}
	sortPlanSteps(flat, planStepOrder(epic))
	view.Steps = planStepRows(flat)
	return view, nil
}

// resolvePlanPhases descends one level into each phase sub-epic to build its
// ordered step list, and returns the flat set of every step id claimed by a
// phase (so resolvePlanRead can exclude them from the flat list without
// re-deriving membership). phaseIDs must already be the RESOLVED, ordered subset
// (resolvePlanPhaseIDs output) — each id is a guaranteed direct-child epic in
// directChildren. Mirrors the viewer's planPhases batched fan-out.
func resolvePlanPhases(ctx context.Context, r planDaemonReader, phaseIDs []string, directChildren map[string]*beadsapi.BeadDetail) ([]planPhaseRead, map[string]struct{}, error) {
	// One batched reverse-deps over all phase sub-epics → their child ids.
	revBySub, err := r.BatchGetDependencies(ctx, phaseIDs, "reverse")
	if err != nil {
		return nil, nil, fmt.Errorf("fetching phase children: %w", err)
	}
	childIDsBySub := make(map[string][]string, len(phaseIDs))
	unionIDs := make([]string, 0, len(phaseIDs)*4)
	unionSeen := make(map[string]struct{})
	for _, pid := range phaseIDs {
		ids := planParentChildIDs(revBySub[pid])
		childIDsBySub[pid] = ids
		for _, id := range ids {
			if _, dup := unionSeen[id]; dup {
				continue
			}
			unionSeen[id] = struct{}{}
			unionIDs = append(unionIDs, id)
		}
	}

	// One batched get over the union of all phase children.
	var grandchildren map[string]*beadsapi.BeadDetail
	if len(unionIDs) > 0 {
		grandchildren, err = r.BatchGetBeads(ctx, unionIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("fetching phase step beads: %w", err)
		}
	}

	phaseStepIDs := make(map[string]struct{})
	phases := make([]planPhaseRead, 0, len(phaseIDs))
	for i, pid := range phaseIDs {
		sub := directChildren[pid] // guaranteed non-nil phase container (resolvePlanPhaseIDs)
		steps := make([]*beadsapi.BeadDetail, 0, len(childIDsBySub[pid]))
		var unreadable []string
		for _, cid := range childIDsBySub[pid] {
			c := grandchildren[cid]
			if c == nil {
				unreadable = append(unreadable, cid)
				continue
			}
			if !isPlanStepType(c.Type) {
				continue
			}
			steps = append(steps, c)
		}
		// Each phase sub-epic carries its OWN step_order overlay (ka-0000007445).
		sortPlanSteps(steps, planStepOrder(sub))
		rows := planStepRows(steps)
		for _, s := range rows {
			phaseStepIDs[s.ID] = struct{}{}
		}
		phases = append(phases, planPhaseRead{
			ID:         pid,
			Title:      sub.Title,
			ShortTitle: sub.ShortTitle,
			Heraldry:   sub.Heraldry,
			Status:     sub.Status,
			Closed:     sub.Status == "closed",
			Number:     i + 1,
			Steps:      rows,
			Unreadable: unreadable,
		})
	}
	return phases, phaseStepIDs, nil
}

// planStepRows materializes ordered step beads into planStepRead rows, numbered
// 1..N over the (already-sorted) slice. Extracted so the flat list and each
// phase number their steps identically.
func planStepRows(beads []*beadsapi.BeadDetail) []planStepRead {
	rows := make([]planStepRead, 0, len(beads))
	for i, b := range beads {
		rows = append(rows, planStepRead{
			ID:         b.ID,
			Title:      b.Title,
			ShortTitle: b.ShortTitle,
			Heraldry:   b.Heraldry,
			Type:       b.Type,
			Status:     b.Status,
			Assignee:   b.Assignee,
			Closed:     b.Status == "closed",
			Number:     i + 1,
		})
	}
	return rows
}

// ── pure helpers (no daemon; unit-testable) ─────────────────────────────────

// planPhaseOrder decodes the plan epic's ordered phase-sub-epic id list from
// fields.phase_order (a JSON array stored as a string-map value). Returns nil
// (no phases → flat timeline) when the field is absent, empty, or unparseable —
// never an error. Blank ids are dropped defensively; order is preserved. Ported
// from the viewer's planPhaseOrder so both surfaces detect phases identically.
func planPhaseOrder(epic *beadsapi.BeadDetail) []string {
	return decodePlanIDList(epic, planPhaseFieldKey)
}

// planStepOrder decodes an epic's explicit step display order from
// fields.step_order — the symmetric sibling of planPhaseOrder for steps
// (ka-0000007445). Same JSON-array decode; same degrade-to-nil (creation order)
// on any absence/parse failure. Unlike phase_order this list is ADVISORY: an
// unlisted step still renders (sortPlanSteps orders it after every listed one).
func planStepOrder(epic *beadsapi.BeadDetail) []string {
	return decodePlanIDList(epic, planStepFieldKey)
}

// decodePlanIDList is the shared JSON-array-of-ids decode behind planPhaseOrder
// and planStepOrder: read fields[key], unmarshal to []string, drop blanks,
// preserve order. Any absence/parse error degrades to nil — the caller treats
// that as "no overlay", never an error (matching the viewer).
func decodePlanIDList(epic *beadsapi.BeadDetail, key string) []string {
	if epic == nil || epic.Fields == nil {
		return nil
	}
	raw, ok := epic.Fields[key]
	if !ok || raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// resolvePlanPhaseIDs filters a raw phase_order list down to the ids that
// actually render as phases: deduped (first occurrence wins) and keeping only
// ids that resolve to a current direct-child phase CONTAINER — a type=epic
// sub-epic OR a type=molecule poured into the plan (ka-0000000759) — dropping
// stale/deleted/re-parented/typo ids and non-container children. Ported from the
// viewer's resolvedPhaseIDs so CLI phase membership + numbering match the
// cockpit, and — like it — routed through beadsapi.IsPhaseContainer so the two
// can never diverge on whether a poured molecule is a phase (ka-0000004781: this
// copy previously kept a raw `Type != "epic"` check that silently dropped poured
// molecule phases the cockpit rendered).
func resolvePlanPhaseIDs(order []string, directChildren map[string]*beadsapi.BeadDetail) []string {
	out := make([]string, 0, len(order))
	seen := make(map[string]struct{}, len(order))
	for _, id := range order {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if !beadsapi.IsPhaseContainer(directChildren[id]) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// sortPlanSteps orders a single group's step beads by the explicit step_order
// overlay first, then stable creation order (with an ID tie-break) for the
// remainder. Ported from the viewer's sortStepBeads: a listed step sorts before
// any unlisted one, two listed steps by their position, and both-unlisted by
// CreatedAt asc then ID — so an empty/absent order reproduces pure creation
// order (the pre-reorder behavior) exactly. The slice is sorted in place.
func sortPlanSteps(steps []*beadsapi.BeadDetail, order []string) {
	rank := make(map[string]int, len(order))
	for i, id := range order {
		if _, dup := rank[id]; dup {
			continue // first occurrence wins
		}
		rank[id] = i
	}
	sort.SliceStable(steps, func(i, j int) bool {
		ri, oki := rank[steps[i].ID]
		rj, okj := rank[steps[j].ID]
		if oki != okj {
			return oki // a listed step sorts before an unlisted one
		}
		if oki && okj && ri != rj {
			return ri < rj // both listed: explicit position wins
		}
		if !steps[i].CreatedAt.Equal(steps[j].CreatedAt) {
			return steps[i].CreatedAt.Before(steps[j].CreatedAt)
		}
		return steps[i].ID < steps[j].ID
	})
}

// planParentChildIDs walks reverse-deps preserving first-seen order, keeps only
// parent-child edges, and dedupes by BeadID — the child ids to batch-fetch.
// Ported from the viewer's dedupedParentChildIDs.
func planParentChildIDs(reverseDeps []beadsapi.Dependency) []string {
	seen := make(map[string]struct{}, len(reverseDeps))
	ids := make([]string, 0, len(reverseDeps))
	for _, dep := range reverseDeps {
		if dep.Type != "parent-child" {
			continue
		}
		if _, dup := seen[dep.BeadID]; dup {
			continue
		}
		seen[dep.BeadID] = struct{}{}
		ids = append(ids, dep.BeadID)
	}
	return ids
}

// isPlanStepType reports whether a bead type is a work item (a plan STEP
// candidate). Nested epics are excluded — a sub-epic is either a phase (grouped
// via phase_order) or a freestanding sub-plan, never a flat step. Ported from
// the viewer's isIssueType.
func isPlanStepType(t string) bool {
	switch t {
	case "task", "bug", "feature", "chore":
		return true
	default:
		return false
	}
}
