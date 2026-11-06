package plans

import (
	"encoding/json"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Label marks an epic as a plan: the epics carrying it are what /plans lists.
const Label = "plan"

// Field keys on the plan epic (and its phase sub-epics). Each is declared on
// type:epic in the kbeads builtin schema; epics don't allowAdditionalFields, so
// an undeclared key would 400 the write.
const (
	// PhaseOrderField holds the plan's ordered phase sub-epic ids (a JSON array).
	PhaseOrderField = "phase_order"
	// StepOrderField holds an epic's explicit step display order (a JSON array).
	StepOrderField = "step_order"
	// StateField holds the authoring lifecycle state (see State).
	StateField = "plan_state"
)

// Input caps the write verbs enforce server-side.
const (
	// MaxStepTitleLen caps a step title: wide enough for a descriptive step,
	// short enough that a paste mistake round-trips via kd instead. A phase
	// title (a short section header) shares it.
	MaxStepTitleLen = 500
	// MaxNarrativeLen caps the narrative summary (the plan epic's description, a
	// short framing blurb; the long-form prose lives in the bundle docs).
	MaxNarrativeLen = 32 * 1024
	// MaxDocLen caps a bundle-doc body. kbeads caps inline doc content at 256KB
	// (validateDocFields), so this stays under that and well above any
	// hand-authored narrative section.
	MaxDocLen = 128 * 1024
)

// State is the authoring lifecycle a plan epic moves through, stamped on
// fields.plan_state. The zero value (StateUnset) means the epic never entered
// the lifecycle (legacy plans, hand-marked epics) and is treated as ungated.
// The daemon owns transition legality (kbeads plan_state_validation.go); the
// checks in this package are UX pre-checks that pick the error message.
type State string

const (
	// StateUnset is a plan that never opted into the lifecycle. Ungated.
	StateUnset State = ""
	// StateDraft: the assistant is still researching; the gardener build is blocked.
	StateDraft State = "draft"
	// StateReady: finalized, awaiting "Approve & build"; the build is still blocked.
	StateReady State = "ready"
	// StateBuilding: approved; the bound gardener loop is enabled.
	StateBuilding State = "building"
	// StateComplete is terminal: the plan is done (epic closed, gardener disabled).
	StateComplete State = "complete"
	// StateAbandoned is the distinct terminal state for a plan dropped without
	// finishing, so /plans can render it apart from a completed one.
	StateAbandoned State = "abandoned"
)

// StateOf reads plan_state off an epic, normalizing an unknown or missing value
// to StateUnset. A nil bead is unset.
func StateOf(bead *beadsapi.BeadDetail) State {
	if bead == nil || bead.Fields == nil {
		return StateUnset
	}
	switch st := State(strings.TrimSpace(bead.Fields[StateField])); st {
	case StateDraft, StateReady, StateBuilding, StateComplete, StateAbandoned:
		return st
	default:
		return StateUnset
	}
}

// PhaseOrder decodes the plan epic's ordered phase sub-epic ids from
// fields.phase_order. The daemon stores a JSON array; the string-map field
// decoder re-marshals non-string values, so the map value is the JSON text
// `["ka-a","ka-b"]`. Returns nil (no phases) when the field is absent, empty or
// unparseable: every such case degrades to the flat timeline, never an error.
func PhaseOrder(epic *beadsapi.BeadDetail) []string {
	return idListField(epic, PhaseOrderField)
}

// StepOrder decodes an epic's explicit step display order from
// fields.step_order, the same way PhaseOrder does. Returns nil (creation order)
// when absent. Unlike phase_order this list is advisory: an unlisted step still
// renders, after every listed one (SortStepBeads).
func StepOrder(epic *beadsapi.BeadDetail) []string {
	return idListField(epic, StepOrderField)
}

func idListField(epic *beadsapi.BeadDetail, key string) []string {
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
	// Drop blanks defensively; duplicates are left to the caller's seen-set.
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// SortStepBeads orders one group's step beads (a plan epic's flat children, or
// one phase's children) by the explicit step_order overlay first, then by
// creation order for the remainder. It is the one ordering primitive the detail
// timeline, the index chip and the reorder seed share, so a step's #N is the
// same on every surface (ka-0000007589).
//
// A listed step sorts before any unlisted one, in the order's position; two
// unlisted steps fall back to CreatedAt asc with an ID tie-break, so an empty or
// absent order reproduces the pre-reorder sort exactly.
func SortStepBeads(steps []*beadsapi.BeadDetail, order []string) {
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
			return oki
		}
		if oki && okj && ri != rj {
			return ri < rj
		}
		if !steps[i].CreatedAt.Equal(steps[j].CreatedAt) {
			return steps[i].CreatedAt.Before(steps[j].CreatedAt)
		}
		return steps[i].ID < steps[j].ID
	})
}

// ResolvedPhaseIDs filters a raw phase_order down to the ids that actually
// render as phases: deduped (first occurrence wins), keeping only ids that
// resolve to a current direct-child phase container. It is the single source of
// phase membership and order, shared by the timeline (which numbers the phases
// and draws the ▲▼ arrows over this subset) and ReorderPhase (which must swap
// over the same subset, ka-0000002075).
func ResolvedPhaseIDs(order []string, directChildren map[string]*beadsapi.BeadDetail) []string {
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

// IsIssueType reports whether a bead type is a work item, the only kind of
// child the timeline renders as a step. Nested epics are excluded: a sub-plan
// renders as its own plan, not as a step.
func IsIssueType(t string) bool {
	switch t {
	case "task", "bug", "feature", "chore":
		return true
	default:
		return false
	}
}

// BundleID returns the plan's narrative bundle, the first direct type=bundle
// child, or "" when it has none. One bundle per plan is the accepted invariant
// (operator decision, thread ka-0000003180).
func BundleID(childIDs []string, children map[string]*beadsapi.BeadDetail) string {
	for _, cid := range childIDs {
		if c := children[cid]; c != nil && c.Type == "bundle" {
			return cid
		}
	}
	return ""
}

// ReorderStep returns a new order with stepID moved one slot in dir ("up"
// toward index 0, "down" toward the end). It returns (nil, false) when stepID is
// absent or the move would fall off a boundary, which callers treat as a
// harmless no-op. The input is never mutated.
func ReorderStep(order []string, stepID, dir string) ([]string, bool) {
	idx := -1
	for i, id := range order {
		if id == stepID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, false
	}
	swap := idx - 1
	if dir == "down" {
		swap = idx + 1
	}
	if swap < 0 || swap >= len(order) {
		return nil, false
	}
	next := make([]string, len(order))
	copy(next, order)
	next[idx], next[swap] = next[swap], next[idx]
	return next, true
}

// ReorderPhaseResolved moves phaseID one position within the resolved
// (rendered) phase subset, then splices the result back into the raw
// phase_order, leaving every stale or non-rendering id pinned at its original
// index. The ▲▼ arrows are computed over resolved, so the swap must be too:
// swapping raw-array neighbors lets a stale id between two rendered phases
// absorb a click (ka-0000002075).
//
// Returns (nil, false) when phaseID is not a rendered phase or the move would
// fall off the resolved boundary. order is never mutated; resolved must be an
// order-preserving sub-sequence of order (ResolvedPhaseIDs guarantees it). When
// resolved == order this is the plain raw-neighbor swap.
func ReorderPhaseResolved(order, resolved []string, phaseID, dir string) ([]string, bool) {
	ridx := -1
	for i, id := range resolved {
		if id == phaseID {
			ridx = i
			break
		}
	}
	if ridx < 0 {
		return nil, false
	}
	rswap := ridx - 1
	if dir == "down" {
		rswap = ridx + 1
	}
	if rswap < 0 || rswap >= len(resolved) {
		return nil, false
	}
	swapID := resolved[rswap]

	// Map the two resolved ids back to their positions in the raw array and swap
	// them there, preserving every other slot (including interleaved stale ids).
	// First occurrence wins, matching ResolvedPhaseIDs's dedup, so a duplicate id
	// in phase_order swaps the same slot the render numbered.
	iA, iB := -1, -1
	for i, id := range order {
		if id == phaseID && iA < 0 {
			iA = i
		}
		if id == swapID && iB < 0 {
			iB = i
		}
	}
	if iA < 0 || iB < 0 {
		// resolved is a sub-sequence of order, so this shouldn't happen; degrade
		// to a no-op rather than corrupt the array.
		return nil, false
	}
	next := make([]string, len(order))
	copy(next, order)
	next[iA], next[iB] = next[iB], next[iA]
	return next, true
}

// parentChildIDs walks reverse deps in first-seen order, keeping only
// parent-child edges, deduped by bead id.
func parentChildIDs(reverseDeps []beadsapi.Dependency) []string {
	seen := map[string]bool{}
	ids := make([]string, 0, len(reverseDeps))
	for _, dep := range reverseDeps {
		if dep.Type != "parent-child" || seen[dep.BeadID] {
			continue
		}
		seen[dep.BeadID] = true
		ids = append(ids, dep.BeadID)
	}
	return ids
}

// projectFromLabels returns the value of the first "project:<name>" label.
func projectFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			return v
		}
	}
	return ""
}

// isOpen reports whether a bead status is a live (open or in-progress) one.
func isOpen(status string) bool {
	switch status {
	case "open", "in_progress", "":
		return true
	default:
		return false
	}
}
