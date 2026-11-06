package main

// Ground-truth run accounting for `gb gardener status` (bug ka-0000005640).
//
// The engine's run counters (runs_spawned / runs_completed) live in TWO places
// that historically drifted apart: the flat top-level schedule fields the CLI
// reads, and the nested `gardener` blob gardenerPostFire writes. On a type=trigger
// gardener the blob counters were frozen at 0 for a long stretch (ka-0000007976:
// gardenerPostFire PATCHed runs_spawned as a JSON string against an INTEGER-typed
// schema, so kbeads 400'd and atomically dropped the whole write). The result:
// `gb gardener status <epic>` reported spawned=0 / completed=0 while the trigger
// engine was demonstrably firing runs — misleading first-fire verification by
// ~20 minutes and eroding trust in the gardener primitive.
//
// The engine-write fix (typed PATCH) stops NEW drift, but it cannot retroactively
// backfill a gardener whose counter was never persisted. The durable cure is to
// stop trusting a single mutable counter and instead DERIVE the count from the
// ground truth the counter is only ever a proxy for: the agent beads the gardener
// actually spawned. Each spawned iteration carries the gardener's id under
// trigger_id (bridge path) or schedule_id (legacy path) — the same dual-key
// ownership the beads-viewer's gardenerIterations() and reconciler's
// closeOrphanedMirrorTask use. Counting those beads is authoritative and
// self-healing: a frozen counter can never again hide a live gardener.

import (
	"context"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Field keys under which a spawned gardener iteration carries its gardener's id.
// Mirrors beads-viewer gardener.go (fieldTriggerID / fieldScheduleID): the bridge
// trigger path stamps trigger_id, the legacy scheduler path stamps schedule_id.
const (
	gardenerOwnerKeyTrigger  = "trigger_id"
	gardenerOwnerKeySchedule = "schedule_id"
)

// gardenerGroundTruthLimit bounds the per-key agent-bead scan. A gardener that has
// spawned more iterations than this in one status call is astronomically beyond
// any real run budget (default max_runs is 20; the highest observed is in the
// low hundreds), so this is a safety cap, not a functional limit.
const gardenerGroundTruthLimit = 2000

// gardenerGroundTruth holds the run counts derived from the gardener's actual
// spawned agent beads. Ok is false when the underlying query failed, so callers
// can fall back to the stored counter rather than reporting a spurious 0.
type gardenerGroundTruth struct {
	Spawned   int
	Completed int
	Ok        bool
}

// gardenerGroundTruthCounts counts the agent beads a gardener actually spawned,
// keyed by trigger_id/schedule_id == gardenerID. Spawned is the distinct-bead
// total across both owner keys; Completed is the subset in the closed status.
//
// The query mirrors beads-viewer gardenerIterations: one list per owner key
// (the daemon Fields filter is AND-equality, so the two keys cannot be OR'd in
// a single query), unioned and deduped by bead id. A defensive client-side field
// check guards against a daemon/stub that ignores the field filter and returns
// adjacent agents.
func gardenerGroundTruthCounts(ctx context.Context, c *beadsapi.Client, gardenerID string) gardenerGroundTruth {
	gardenerID = strings.TrimSpace(gardenerID)
	if gardenerID == "" || c == nil {
		return gardenerGroundTruth{}
	}
	seen := make(map[string]bool)
	var gt gardenerGroundTruth
	for _, key := range []string{gardenerOwnerKeyTrigger, gardenerOwnerKeySchedule} {
		res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"open", "in_progress", "closed"},
			Fields:   map[string]string{key: gardenerID},
			Sort:     "-created_at",
			Limit:    gardenerGroundTruthLimit,
		})
		if err != nil {
			// A partial failure would under-count and could mask a live gardener
			// the same way the frozen counter did. Fail the whole derivation so
			// the caller keeps the stored counter instead.
			return gardenerGroundTruth{}
		}
		for _, b := range res.Beads {
			if b == nil || b.ID == "" || seen[b.ID] {
				continue
			}
			// Defensive: only count beads that actually carry this gardener's id
			// under one of the owner keys (a stub/daemon may ignore the filter).
			if b.Fields[gardenerOwnerKeyTrigger] != gardenerID && b.Fields[gardenerOwnerKeySchedule] != gardenerID {
				continue
			}
			seen[b.ID] = true
			gt.Spawned++
			if b.Status == "closed" {
				gt.Completed++
			}
		}
	}
	gt.Ok = true
	return gt
}

// reconcileRunCount fuses a stored counter with the ground-truth agent-bead
// count, returning the value to display. It takes the MAX of the two so the
// readout is:
//
//   - self-healing: a frozen/never-persisted counter (spawned="") is replaced by
//     the real spawned-bead count, so a live gardener can never render spawned=0;
//   - pruning-safe: a long-lived gardener whose oldest iteration beads were pruned
//     keeps its higher lifetime counter rather than regressing to the live count.
//
// When both are zero the original counter string is preserved verbatim (""/"0")
// so the existing defaultIfEmpty rendering is unchanged.
func reconcileRunCount(counter string, groundTruth int, haveGroundTruth bool) string {
	best := atoiOrZeroTrim(counter)
	if haveGroundTruth && groundTruth > best {
		best = groundTruth
	}
	if best == 0 {
		return counter
	}
	return strconv.Itoa(best)
}

// atoiOrZeroTrim parses s as an int, returning 0 on any error or empty input.
func atoiOrZeroTrim(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
