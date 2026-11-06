// Package planspend is the per-plan crew spend rollup (gd-kcy4IFNzv5, decision
// gd-PZH1utyrBt = controller). The controller sums what a plan's crew cost onto
// fields of the plan epic every SyncInterval; gardenhub reads those fields into
// the farm document, so /garden shows per-plan spend without walking agent beads
// itself.
//
// "A plan's crew" is exactly the set the /plans crew card counts
// (beads-viewer planCrewBulk / planCrew): agent beads with an `assigned` edge
// into one of the plan's rendered steps, or into the plan epic itself, minus the
// plan-gardener. Rendered steps are the issue-type children of the epic plus the
// issue-type children of each phase container listed in its phase_order. One
// agent's dollars are AgentSpendUSD, the helper the viewer's agent card and crew
// rollup also call, so /plans and /garden agree to the cent; the viewer pins
// that with a shared fixture (plans_crew_rollup_agree_test.go).
package planspend

import (
	"context"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planagent"
	"gardenio/controller/internal/plans"
)

// Fields the rollup writes on the plan epic (declared on type:epic in the
// kbeads schema, which does not allow additional fields).
const (
	// FieldUSD is the summed crew spend, a fixed-precision decimal string.
	FieldUSD = "plan_spend_usd"
	// FieldSyncedAt is the RFC3339 time the figure was last written.
	FieldSyncedAt = "plan_spend_synced_at"
	// FieldCrew is how many crew agent beads the figure sums.
	FieldCrew = "plan_spend_crew"
	// FieldLive is how many of them were live at FieldSyncedAt. The figure is
	// only rewritten when it, the crew or the live count changes; with live
	// crew it is moving, and a reader judges it by the controller's newest
	// stamp on any plan (ReadWithPass).
	FieldLive = "plan_spend_live"
)

// Fields the controller's per-gardener rollup writes on a gardener's
// trigger or schedule bead (cmd/controller/gardener_spend_sync.go).
const (
	// GardenerFieldUSD is the gardener's runs plus the crew they spawned.
	GardenerFieldUSD = "gardener_spend_usd"
	// GardenerFieldSyncedAt is the RFC3339 time the figure was last written.
	GardenerFieldSyncedAt = "gardener_spend_synced_at"
)

// SyncInterval is the controller's cadence, the same as gardener_spend_usd's:
// a crew's authoritative cost lands at session close, and the live estimate
// moves slowly enough that ten minutes is fresh.
const SyncInterval = 10 * time.Minute

// StaleAfter is when a reader stops trusting a moving figure: three passes
// with no plan written (ReadWithPass).
const StaleAfter = 3 * SyncInterval

// CrewStaleAfter is how long an alive-state crew may go without a heartbeat
// before it stops counting as live. It is the viewer's agentStaleThreshold,
// pinned there by a test.
const CrewStaleAfter = time.Hour

// aliveStates are the agent_state values that read as live (the viewer's
// agentStateAlive).
var aliveStates = map[string]bool{"working": true, "spawning": true, "yielding": true}

// ParseUSD parses a USD cost field, returning 0 on empty, invalid or negative.
func ParseUSD(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// AgentSpendUSD is one agent bead's dollar spend: the reconciled
// total_cost_usd, plus the in-pod live_cost_usd estimate of the current
// session only while the agent is live (once it closes, the reconciler folds
// that session into total_cost_usd and adding it again would double-count).
func AgentSpendUSD(fields map[string]string, live bool) float64 {
	cost := ParseUSD(fields["total_cost_usd"])
	if live {
		cost += ParseUSD(fields["live_cost_usd"])
	}
	return cost
}

// CrewLive reports whether a crew agent bead is live: open, in an alive
// agent_state, and with a heartbeat (updated_at) under CrewStaleAfter old. A
// zero updated_at reads fresh, as in the viewer, so a timestampless fixture is
// never demoted. This is the live flag the viewer's classifyCrewBead returns.
func CrewLive(b *beadsapi.BeadDetail, now time.Time) bool {
	if b == nil || b.Status == "closed" || !aliveStates[b.Fields["agent_state"]] {
		return false
	}
	return b.UpdatedAt.IsZero() || now.Sub(b.UpdatedAt) < CrewStaleAfter
}

// IsPlanGardener reports whether an agent bead is the plan's gardener, which
// is assigned to the epic but is not crew. Keyed on the role label and the
// role field, so a bead carrying only one of them is still recognized.
func IsPlanGardener(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	for _, l := range b.Labels {
		if l == planagent.RoleLabel {
			return true
		}
	}
	return strings.TrimSpace(b.Fields["role"]) == planagent.SpawnRole
}

// Source is the slice of the daemon client the walk needs.
type Source interface {
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// Rollup is one plan's crew spend at one instant.
type Rollup struct {
	USD  float64
	Crew int // crew agent beads summed
	Live int // of which live
}

// Plans computes the Rollup of every plan epic in epics (each must carry its
// fields.phase_order). Daemon calls are batched over the whole set: reverse
// deps of the epics, of their phase containers and of their steps, plus
// hydration of children whose type the edge does not report and of the crew.
// Any daemon error fails the whole call, so a caller never writes a partial
// figure. A plan with no crew gets a zero Rollup.
func Plans(ctx context.Context, src Source, epics []*beadsapi.BeadDetail, now time.Time) (map[string]Rollup, error) {
	out := make(map[string]Rollup, len(epics))
	epicIDs := make([]string, 0, len(epics))
	for _, e := range epics {
		if e != nil && e.ID != "" {
			epicIDs = append(epicIDs, e.ID)
		}
	}
	if len(epicIDs) == 0 {
		return out, nil
	}

	// Tier 1: the epics' children (and the epic-anchored crew edges).
	revEpic, err := src.BatchGetDependencies(ctx, epicIDs, "reverse")
	if err != nil {
		return nil, err
	}
	childType := map[string]string{}
	directOf := map[string][]string{}
	for _, id := range epicIDs {
		directOf[id] = childIDs(revEpic[id], childType)
	}
	if err := hydrateTypes(ctx, src, childType); err != nil {
		return nil, err
	}

	// Tier 2: the children of each phase container named in phase_order. As in
	// the viewer, a phase must be a known child (of any listed plan) of
	// container type; an unlisted child epic is a sub-plan, not a phase.
	phasesOf := map[string][]string{}
	var phaseIDs []string
	phaseSeen := map[string]bool{}
	for _, e := range epics {
		if e == nil || e.ID == "" {
			continue
		}
		for _, pid := range plans.PhaseOrder(e) {
			if !isPhaseType(childType[pid]) {
				continue
			}
			phasesOf[e.ID] = append(phasesOf[e.ID], pid)
			if !phaseSeen[pid] {
				phaseSeen[pid] = true
				phaseIDs = append(phaseIDs, pid)
			}
		}
	}
	grandOf := map[string][]string{}
	if len(phaseIDs) > 0 {
		revPhase, err := src.BatchGetDependencies(ctx, phaseIDs, "reverse")
		if err != nil {
			return nil, err
		}
		grandType := map[string]string{}
		for _, pid := range phaseIDs {
			grandOf[pid] = childIDs(revPhase[pid], grandType)
		}
		if err := hydrateTypes(ctx, src, grandType); err != nil {
			return nil, err
		}
		for id, t := range grandType {
			if _, ok := childType[id]; !ok {
				childType[id] = t
			}
		}
	}

	// Steps per plan: issue-type phase children, then issue-type direct
	// children. The walk ids are the steps plus the epic itself.
	walkOf := map[string][]string{}
	var stepIDs []string
	stepSeen := map[string]bool{}
	for _, id := range epicIDs {
		var walk []string
		add := func(sid string) {
			if !plans.IsIssueType(childType[sid]) {
				return
			}
			walk = append(walk, sid)
			if !stepSeen[sid] {
				stepSeen[sid] = true
				stepIDs = append(stepIDs, sid)
			}
		}
		inPhase := map[string]bool{}
		for _, pid := range phasesOf[id] {
			inPhase[pid] = true
			for _, sid := range grandOf[pid] {
				add(sid)
			}
		}
		for _, cid := range directOf[id] {
			if !inPhase[cid] {
				add(cid)
			}
		}
		walkOf[id] = append(walk, id)
	}

	// Tier 3: crew edges into the steps; the epic's own are already in revEpic.
	assigned := map[string][]beadsapi.Dependency{}
	if len(stepIDs) > 0 {
		revStep, err := src.BatchGetDependencies(ctx, stepIDs, "reverse")
		if err != nil {
			return nil, err
		}
		assigned = revStep
	}
	for _, id := range epicIDs {
		assigned[id] = revEpic[id]
	}
	var agentIDs []string
	agentSeen := map[string]bool{}
	for _, id := range epicIDs {
		for _, wid := range walkOf[id] {
			for _, d := range assigned[wid] {
				if d.Type == "assigned" && d.BeadID != "" && !agentSeen[d.BeadID] {
					agentSeen[d.BeadID] = true
					agentIDs = append(agentIDs, d.BeadID)
				}
			}
		}
	}
	var agents map[string]*beadsapi.BeadDetail
	if len(agentIDs) > 0 {
		if agents, err = src.BatchGetBeads(ctx, agentIDs); err != nil {
			return nil, err
		}
	}

	// Sum each plan's crew once per agent bead.
	for _, id := range epicIDs {
		var r Rollup
		seen := map[string]bool{}
		for _, wid := range walkOf[id] {
			for _, d := range assigned[wid] {
				if d.Type != "assigned" || d.BeadID == "" || seen[d.BeadID] {
					continue
				}
				b := agents[d.BeadID]
				if b == nil || b.Type != "agent" || IsPlanGardener(b) {
					continue
				}
				seen[d.BeadID] = true
				live := CrewLive(b, now)
				r.Crew++
				if live {
					r.Live++
				}
				r.USD += AgentSpendUSD(b.Fields, live)
			}
		}
		out[id] = r
	}
	return out, nil
}

// childIDs returns the deduped parent-child children on a reverse-deps list,
// recording each child's type (from the edge, "" when the daemon did not say).
func childIDs(deps []beadsapi.Dependency, types map[string]string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, d := range deps {
		if d.Type != "parent-child" || d.BeadID == "" || seen[d.BeadID] {
			continue
		}
		seen[d.BeadID] = true
		ids = append(ids, d.BeadID)
		if types[d.BeadID] == "" {
			types[d.BeadID] = d.BeadType
		}
	}
	return ids
}

// hydrateTypes fills in the type of every child whose edge did not report one.
// A child the daemon cannot return keeps "" and is neither a step nor a phase.
func hydrateTypes(ctx context.Context, src Source, types map[string]string) error {
	var unknown []string
	for id, t := range types {
		if t == "" {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	got, err := src.BatchGetBeads(ctx, unknown)
	if err != nil {
		return err
	}
	for _, id := range unknown {
		if b := got[id]; b != nil {
			types[id] = b.Type
		}
	}
	return nil
}

func isPhaseType(t string) bool { return t == "epic" || t == "molecule" }

// FormatUSD is how FieldUSD is written: a 4-decimal string, the shape
// gardener_spend_usd uses.
func FormatUSD(usd float64) string { return strconv.FormatFloat(usd, 'f', 4, 64) }

// Reading is a plan epic's rollup fields as a reader sees them.
type Reading struct {
	Has      bool // the controller has written a figure
	USD      float64
	SyncedAt time.Time // zero when Has is false or the stamp does not parse
	Crew     int
	Live     int
	// Stale is true when the figure was moving (live crew at the last write)
	// and the controller has written no plan for StaleAfter: its pass has
	// stopped keeping the figure. A settled figure (no live crew) is never
	// stale.
	Stale bool
}

// Read decodes the rollup fields of a plan epic, judging Stale by the epic's
// own stamp alone (ReadWithPass with no passAt).
func Read(fields map[string]string, now time.Time) Reading {
	return ReadWithPass(fields, now, time.Time{})
}

// ReadWithPass is Read with passAt, the newest FieldSyncedAt the reader holds
// across plans: proof the controller's pass is alive. The controller does not
// re-stamp an unchanged figure (gd-vwMhcJgts2), so a moving figure is stale
// only when neither its own stamp nor passAt is within StaleAfter.
func ReadWithPass(fields map[string]string, now, passAt time.Time) Reading {
	raw := strings.TrimSpace(fields[FieldUSD])
	if raw == "" {
		return Reading{}
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 {
		return Reading{}
	}
	r := Reading{Has: true, USD: f, Crew: atoi(fields[FieldCrew]), Live: atoi(fields[FieldLive])}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[FieldSyncedAt])); err == nil {
		r.SyncedAt = t
	}
	if r.SyncedAt.After(passAt) {
		passAt = r.SyncedAt
	}
	r.Stale = r.Live > 0 && (passAt.IsZero() || now.Sub(passAt) > StaleAfter)
	return r
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
