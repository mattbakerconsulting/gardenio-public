package bridge

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// leaseStaleHoursField parses the optional per-trigger `lease_stale_hours`
// override into a Duration. A positive, finite float of hours wins; anything
// else (missing, non-numeric, <= 0, non-finite) returns 0 so callers fall back
// to defaultLeaseStaleAfter. Bug ka-0000001095.
func leaseStaleHoursField(fields map[string]string) time.Duration {
	raw := strings.TrimSpace(fields["lease_stale_hours"])
	if raw == "" {
		return 0
	}
	hours, err := strconv.ParseFloat(raw, 64)
	if err != nil || hours <= 0 || math.IsInf(hours, 0) {
		return 0
	}
	return time.Duration(hours * float64(time.Hour))
}

// resolveLeaseStale returns a positive lease-staleness threshold: the caller's
// override when > 0, else the 3h default. Bug ka-0000001095.
func resolveLeaseStale(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return defaultLeaseStaleAfter
}

// leaseStaleThreshold returns the age past which an agent stops holding this
// trigger's max_concurrent lease. It is the trigger's per-fire override
// (LeaseStaleAfter, from the `lease_stale_hours` field) when positive, else the
// 3h default. See defaultLeaseStaleAfter for the justification. Bug ka-0000001095.
func leaseStaleThreshold(trig TriggerDef) time.Duration {
	return resolveLeaseStale(trig.LeaseStaleAfter)
}

// leaseHolderStale reports whether an agent bead is too stale to keep holding a
// trigger's max_concurrent lease: updated_at older than the threshold. A zero
// updated_at is NOT stale — we can't prove inactivity, so we fail toward keeping
// the lease held (never release a lease we can't justify releasing). Bug
// ka-0000001095.
func leaseHolderStale(b *beadsapi.BeadDetail, threshold time.Duration, now time.Time) bool {
	if b == nil || b.UpdatedAt.IsZero() {
		return false
	}
	return now.Sub(b.UpdatedAt) > threshold
}

// leaseHolderOverAgeCeiling reports whether an agent bead has held a trigger's
// max_concurrent lease past the ABSOLUTE age ceiling: created_at older than
// leaseAgeCeilingFactor × the (release) threshold. Unlike leaseHolderStale this
// keys on created_at, so a LIVELOCKED agent that keeps updated_at fresh by
// churning cannot dodge it (bug ka-0000002023). A zero/absent created_at is NOT
// over the ceiling — we can't prove age, so we fail toward keeping the lease
// held (symmetric with leaseHolderStale's zero-updated_at handling).
func leaseHolderOverAgeCeiling(b *beadsapi.BeadDetail, threshold time.Duration, now time.Time) bool {
	if b == nil || b.CreatedAt.IsZero() {
		return false
	}
	return now.Sub(b.CreatedAt) > leaseAgeCeilingFactor*threshold
}

// triggerLeaseField names the trigger an agent bead holds a live-run lease on
// when the engine did not fire it: the slack-bridge's decision- and
// escalation-resume of a trigger-fired agent (stampResumeLineage). That respawn
// must keep spawned_by=<prior agent bead id>, because spawn authz admits a
// privileged gardener's resume only as a same-role continuation of the bead
// spawned_by names, so spawned_by:trigger:<id> no longer finds it. Without this
// field the lease gates saw no live run and the trigger fired a second session
// beside the respawn (gd-H17IlFclcl). Authz never reads it, and it is not
// trigger_id/schedule_id, which drive run accounting.
const triggerLeaseField = "trigger_lease_id"

// listTriggerLeaseAgents returns the non-terminal agent beads holding a lease on
// trigger trigID: those it fired (spawned_by:trigger:<id>) and those resumed on
// its behalf (triggerLeaseField), deduplicated by id. Callers apply the lease
// staleness bounds and choose their own fail-open posture on error.
func listTriggerLeaseAgents(ctx context.Context, daemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}, trigID string) ([]*beadsapi.BeadDetail, error) {
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for _, fields := range []map[string]string{
		{"spawned_by": "trigger:" + trigID},
		{triggerLeaseField: trigID},
	} {
		res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"open", "in_progress", "blocked", "deferred"},
			Fields:   fields,
			// Holders are read for lease timestamps and runtime fields only
			// (gd-zXdZq4Traf).
			Omit:       leanAgentOmit,
			OmitFields: leanAgentOmitFields,
		})
		if err != nil {
			return nil, err
		}
		if res == nil {
			continue
		}
		for _, b := range res.Beads {
			if b == nil || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			out = append(out, b)
		}
	}
	return out, nil
}

// liveSpawnCount returns the number of agent beads still alive (status
// open/in_progress/blocked/deferred) that were spawned by this trigger AND are
// not stale past the lease threshold. It is the basis for treating
// max_concurrent as a LIVE lease rather than a per-batch cap: the firing loop
// seeds its `fired` counter with this so prior fires AND agents spawned by
// sibling sources of the same multi-entry trigger all count against the cap.
//
// Spawned agents carry `spawned_by: trigger:<id>` (see actionSpawnAgent's
// extraFields). We query agent beads filtered on that field, and on
// triggerLeaseField for a bridge resume of one (listTriggerLeaseAgents).
//
// Age bound (bug ka-0000001095): an agent whose updated_at is older than
// leaseStaleThreshold no longer counts, so a single hung agent cannot hold a
// max_concurrent=1 lane's only lease forever. The wedged agent bead is left
// completely alone — the lane simply resumes firing (option A: lease-side only,
// non-destructive). The skip is observable: a WARN log per stale holder, plus a
// one-shot comment on the trigger naming the stale agent (leaseStaleComment), so
// it surfaces rather than going silent. A zero/absent updated_at counts as live
// (fail toward keeping the lease held).
//
// Fail-open: any daemon error returns 0 (logged) so a transient hiccup cannot
// permanently wedge a trigger — matching the one_spawn_at_a_time gate's posture.
// We fetch rows (not count-only) so staleness can be evaluated per holder and
// the stale one can be named; the holder set is bounded by max_concurrent-scale
// concurrency in practice.
func (e *TriggerEngine) liveSpawnCount(ctx context.Context, trig TriggerDef) int {
	return len(e.liveSpawnHolders(ctx, trig))
}

// liveSpawnHolders is the shared implementation behind liveSpawnCount: it returns
// the agent beads currently HOLDING this trigger's max_concurrent lease (alive,
// spawned_by:trigger:<id>, and inside both lease bounds). liveSpawnCount is just
// len() of this. The dark-lane sweep (trigger_dark_lane.go) also calls it so it
// can INSPECT the holders — e.g. classify whether a pinning holder is parked in a
// credential/spend-exhaustion error state — without issuing a second identical
// agent-bead query (bug ka-0000001246). Callers must not mutate the returned
// beads. Same fail-open posture as before: a daemon error yields a nil slice.
func (e *TriggerEngine) liveSpawnHolders(ctx context.Context, trig TriggerDef) []*beadsapi.BeadDetail {
	holders, err := listTriggerLeaseAgents(ctx, e.daemon, trig.ID)
	if err != nil {
		e.logger.Warn("triggers: live spawn count failed, treating as 0",
			"trigger", trig.ID, "error", err)
		return nil
	}

	threshold := leaseStaleThreshold(trig)
	now := e.now()
	live := make([]*beadsapi.BeadDetail, 0, len(holders))
	var stale []*beadsapi.BeadDetail
	for _, b := range holders {
		// Two independent lease bounds, EITHER of which releases the holder:
		//   - updated_at older than threshold — the holder went QUIET
		//     (ka-0000001095);
		//   - created_at older than the age ceiling — the holder has LIVELOCKED,
		//     churning updated_at fresh so the quiet bound never fires
		//     (ka-0000002023). created_at is immune to that churn.
		if leaseHolderStale(b, threshold, now) || leaseHolderOverAgeCeiling(b, threshold, now) {
			stale = append(stale, b)
			continue
		}
		live = append(live, b)
	}
	for _, b := range stale {
		e.logger.Warn("triggers: lease holder stale — not counting toward max_concurrent",
			"trigger", trig.ID, "agent", b.ID, "agent_name", b.Fields["agent"],
			"updated_at", b.UpdatedAt.UTC().Format(time.RFC3339),
			"created_at", b.CreatedAt.UTC().Format(time.RFC3339),
			"updated_stale_for", now.Sub(b.UpdatedAt).Truncate(time.Minute).String(),
			"age", now.Sub(b.CreatedAt).Truncate(time.Minute).String(),
			"threshold", threshold.String(),
			"age_ceiling", (leaseAgeCeilingFactor * threshold).String())
	}
	// Only surface the wedge (once per stale holder) when it actually matters:
	// the trigger is at its cap on stale holders alone, so it would otherwise
	// defer forever. A trigger with spare capacity keeps firing regardless.
	if len(stale) > 0 && trig.MaxConcurrent > 0 && len(live) < trig.MaxConcurrent {
		e.commentLeaseStaleOnce(ctx, trig, stale, threshold, now)
	}
	return live
}

// findExistingSpawnForMatch looks up an agent bead this trigger already
// spawned for this exact match, so actionSpawnAgent's idempotency guard can
// return the existing agent instead of creating a duplicate (gd-pLf5LSHDEh).
// It queries on the SAME two fields actionSpawnAgent stamps at spawn time —
// spawned_by: trigger:<id> and the source's dedicated match-key field
// (triggerSourceFieldKey) — across every status (not just alive), since a
// match that already spawned stays "already spawned" even after the agent
// closes. Returns "" with no error when there is no match key to dedup on
// (m.Key == "") or no existing spawn is found. Fail-open on a daemon error:
// the caller logs and proceeds with a fresh spawn, matching liveSpawnHolders'
// posture — a transient lookup failure must never permanently block a fire.
func (e *TriggerEngine) findExistingSpawnForMatch(ctx context.Context, trig TriggerDef, m TriggerMatch) (string, error) {
	if m.Key == "" {
		return "", nil
	}
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"},
		Fields: map[string]string{
			"spawned_by":                       "trigger:" + trig.ID,
			triggerSourceFieldKey(trig.Source): m.Key,
		},
		Limit: 1,
	})
	if err != nil {
		return "", err
	}
	if res == nil || len(res.Beads) == 0 {
		return "", nil
	}
	return res.Beads[0].ID, nil
}

// commentLeaseStaleOnce writes a one-shot comment on the trigger bead naming the
// stale lease holder(s) the engine is releasing, so a wedged lane recovering is
// observable rather than silent (bug ka-0000001095 acceptance #3). Deduped per
// (trigger, stale agent set) via warnedGate so a persistently-stale holder does
// not re-comment every cycle; a NEW stale agent re-arms the comment. Best-effort:
// a comment failure is logged, never propagated (the lease release already
// happened and must not be undone by a comment hiccup).
func (e *TriggerEngine) commentLeaseStaleOnce(ctx context.Context, trig TriggerDef, stale []*beadsapi.BeadDetail, threshold time.Duration, now time.Time) {
	ids := make([]string, 0, len(stale))
	for _, b := range stale {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	key := "lease-stale:" + trig.ID + ":" + strings.Join(ids, ",")

	e.mu.Lock()
	already := e.warnedGate[key]
	if !already {
		e.warnedGate[key] = true
	}
	e.mu.Unlock()
	if already {
		return
	}

	details := make([]string, 0, len(stale))
	for _, b := range stale {
		name := b.Fields["agent"]
		if name == "" {
			name = b.ID
		}
		// Name WHICH bound released it: a quiet holder (updated_at old) vs a
		// livelocked holder (updated_at fresh but created_at past the age
		// ceiling — ka-0000002023). Reporting only "idle Xm" would read as "1m"
		// for a livelock and hide the real 15h wedge, so surface both the idle
		// gap and the absolute age.
		bound := "quiet (updated_at stale)"
		if !leaseHolderStale(b, threshold, now) && leaseHolderOverAgeCeiling(b, threshold, now) {
			bound = "livelocked (updated_at fresh, created_at past age ceiling)"
		}
		details = append(details, fmt.Sprintf("%s (%s, %s; idle %s, age %s since created_at %s)",
			b.ID, name, bound,
			now.Sub(b.UpdatedAt).Truncate(time.Minute).String(),
			now.Sub(b.CreatedAt).Truncate(time.Minute).String(),
			b.CreatedAt.UTC().Format(time.RFC3339)))
	}
	msg := fmt.Sprintf(
		"lease auto-released: %d stale agent(s) past the %s max_concurrent lease bound (or the %s age ceiling) are no longer counted, so this trigger can fire again — %s. "+
			"The stale agent bead(s) are left untouched (non-destructive lease-side release, bugs ka-0000001095 / ka-0000002023); "+
			"if a holder is genuinely wedged it should be closed/killed separately.",
		len(stale), threshold.String(), (leaseAgeCeilingFactor * threshold).String(), strings.Join(details, "; "))
	if err := e.daemon.AddComment(ctx, trig.ID, "trigger-engine", msg); err != nil {
		e.logger.Warn("triggers: lease-stale comment failed",
			"trigger", trig.ID, "error", err)
	}
}
