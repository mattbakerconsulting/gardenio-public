package main

import (
	"context"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Gardener-grouped /agents view (task ka-0000006067; persistent-agents epic
// ka-0000006566). The default /agents view buckets the live fleet by PROJECT; a
// `?group=gardener` toggle re-buckets it by GARDENER OWNERSHIP so an operator can
// read the fleet as "who is driving what" — the mental model the persistent-agent
// identity work (gardener_id stamp + lineage back-pointers) was built to support.
//
// Two structural moves distinguish this view from the flat project grouping:
//
//  1. BUCKET BY CANONICAL GARDENER. Every agent bead carries a resolved
//     gardener_id (beadsapi.DeriveGardenerID — gardener_id ∨ schedule_id/trigger_id
//     ∨ synthetic). Real gardener ids (a live trigger bead) get their own bucket
//     headed by the same gardenerRowView the /gardeners index renders, so the
//     header reads identically on both surfaces. Agents whose owner is SYNTHETIC
//     (IsSyntheticGardenerID — no real gardener) collapse into one "fleet-direct"
//     sibling lane so a one-off / ad-hoc agent is never mis-attributed to a
//     gardener that does not exist.
//
//  2. COLLAPSE LINEAGE TO THE LIVE HEAD. Within a bucket, successor incarnations
//     of the same agent (thread respawn, gardener iteration, wrapup successor)
//     share a lineage_root_id. Rather than list every incarnation as a peer row,
//     each chain collapses to its LIVE HEAD (the newest run — the one no other
//     live row points at) with the older incarnations folded into an expandable
//     successor-history disclosure. This keeps a 40-iteration gardener from
//     blowing out its bucket while preserving one-click access to the history.

// agentGardenerBucket is one gardener's section in the ?group=gardener view. A
// bucket headed by a real gardener carries a non-nil Gardener header (the same
// gardenerRowView /gardeners renders); the synthetic fleet-direct lane carries a
// nil Gardener and renders a plain header off Label instead.
type agentGardenerBucket struct {
	// GardenerID is the canonical owner id this bucket is keyed on. A real
	// gardener bead id, or a synthetic-gardener:<source> marker for the
	// fleet-direct lane.
	GardenerID string
	// Gardener is the gardener header row (name, goal, verdict, spend), populated
	// only when GardenerID resolves to a real, fetchable gardener (trigger) bead.
	// Nil for the fleet-direct lane and for a gardener bead that could not be
	// fetched — the template falls back to Label in that case.
	Gardener *gardenerRowView
	// Label is the header text used when Gardener is nil (the fleet-direct lane,
	// or an unresolvable gardener id). Human-readable, never a raw synthetic
	// marker.
	Label string
	// FleetDirect marks the synthetic bucket that holds every gardener-less agent
	// (a one-off / ad-hoc spawn). Rendered as a sibling lane, sorted last.
	FleetDirect bool
	// Lineages are the succession chains in this bucket, each collapsed to its
	// live head. Sorted newest-head-first.
	Lineages []agentLineageGroup
	// AgentCount is the total number of live agent rows in the bucket across all
	// lineages (heads + folded successors) — the header count.
	AgentCount int
}

// agentLineageGroup is one succession chain within a gardener bucket, collapsed to
// its live head. Head is the newest incarnation; Successors are the older runs,
// newest-first, revealed by an expandable disclosure.
type agentLineageGroup struct {
	// Head is the live head of the chain — the newest incarnation, the row an
	// operator acts on. Always present.
	Head agentRowView
	// Successors are the older incarnations of the same agent, newest-first.
	// Empty for a first-of-line agent (a one-run chain). Folded into an
	// expandable "N earlier" disclosure so the bucket stays scannable.
	Successors []agentRowView
	// rootID is the chain's lineage_root_id (or the head's own id) — the map key
	// used while assembling the group. Not rendered.
	rootID string
	// newest is the head's created-at, retained as the bucket sort key so the
	// most-recently-spawned chain leads.
	newest int64
}

// HasSuccessors reports whether the expandable successor-history disclosure
// should render.
func (g agentLineageGroup) HasSuccessors() bool { return len(g.Successors) > 0 }

// SuccessorCount is the number of folded older incarnations, for the disclosure
// summary ("+N earlier").
func (g agentLineageGroup) SuccessorCount() int { return len(g.Successors) }

// buildAgentGardenerGroups re-buckets the already-built active agent rows by
// canonical gardener ownership, collapsing each succession chain to its live
// head. It is pure over `rows` plus one batched gardener fetch — no per-row
// fan-out. A gardener bead that cannot be fetched degrades to a Label-only
// bucket rather than failing the view.
func (s *Server) buildAgentGardenerGroups(ctx context.Context, rows []agentRowView) []agentGardenerBucket {
	// Partition rows by canonical gardener id. Synthetic owners all fold into the
	// single fleet-direct lane so a gardener-less agent is never mis-attributed.
	byGardener := map[string][]agentRowView{}
	var fleetDirect []agentRowView
	for _, row := range rows {
		if beadsapi.IsSyntheticGardenerID(row.GardenerID) {
			fleetDirect = append(fleetDirect, row)
			continue
		}
		byGardener[row.GardenerID] = append(byGardener[row.GardenerID], row)
	}

	// Batch-fetch the real gardener beads for the headers in one round-trip —
	// gardenerRowFromBead then projects each into the same header /gardeners shows.
	gardenerIDs := make([]string, 0, len(byGardener))
	for id := range byGardener {
		gardenerIDs = append(gardenerIDs, id)
	}
	gardenerBeads := map[string]*beadsapi.BeadDetail{}
	if len(gardenerIDs) > 0 {
		fetched, err := s.daemon.BatchGetBeads(ctx, gardenerIDs)
		if err != nil {
			// Degrade to Label-only buckets — the view still groups by gardener id,
			// it just cannot show the rich header. Not a page failure.
			s.logger.Warn("agents group=gardener: batch gardener fetch", "error", err)
		} else {
			gardenerBeads = fetched
		}
	}

	buckets := make([]agentGardenerBucket, 0, len(byGardener)+1)
	for id, bucketRows := range byGardener {
		bucket := agentGardenerBucket{
			GardenerID: id,
			Lineages:   collapseLineages(bucketRows),
			AgentCount: len(bucketRows),
		}
		if cb := gardenerBeads[id]; cb != nil && cb.Type != "" {
			row := s.gardenerRowFromBead(ctx, cb, s.now())
			bucket.Gardener = &row
			bucket.Label = row.Name
		} else {
			// Unresolvable gardener id (bead reaped, or a non-gardener type): show
			// the id itself so the bucket is still identifiable.
			bucket.Label = id
		}
		buckets = append(buckets, bucket)
	}

	// Sort real-gardener buckets: most-recently-active head first (by the newest
	// lineage head in each bucket), so a live gardener floats to the top.
	sort.SliceStable(buckets, func(i, j int) bool {
		return bucketNewest(buckets[i]) > bucketNewest(buckets[j])
	})

	// The fleet-direct lane is a sibling, always sorted LAST — it is the
	// catch-all, not a gardener.
	if len(fleetDirect) > 0 {
		buckets = append(buckets, agentGardenerBucket{
			GardenerID:  syntheticFleetDirectID,
			Label:       "Fleet-direct",
			FleetDirect: true,
			Lineages:    collapseLineages(fleetDirect),
			AgentCount:  len(fleetDirect),
		})
	}

	return buckets
}

// syntheticFleetDirectID is the stable bucket key for the gardener-less lane.
const syntheticFleetDirectID = "fleet-direct"

// collapseLineages groups a bucket's rows into succession chains keyed on
// lineage_root_id and collapses each chain to its live head. The head is the
// newest incarnation present (the row nothing else in the chain points at, with
// created-at as the tie-break); older incarnations fold into Successors,
// newest-first. Chains are returned newest-head-first.
func collapseLineages(rows []agentRowView) []agentLineageGroup {
	byRoot := map[string][]agentRowView{}
	for _, row := range rows {
		byRoot[row.LineageRootID] = append(byRoot[row.LineageRootID], row)
	}

	groups := make([]agentLineageGroup, 0, len(byRoot))
	for root, chain := range byRoot {
		// The live head is the incarnation no other row in the chain points at
		// via prior_agent_id — i.e. the newest. Fall back to created-at order so a
		// chain with a dangling head pointer still picks a deterministic head.
		pointedAt := map[string]bool{}
		for _, r := range chain {
			if r.PriorAgentID != "" {
				pointedAt[r.PriorAgentID] = true
			}
		}
		// Sort newest-first so the head and the folded successors are ordered.
		sort.SliceStable(chain, func(i, j int) bool {
			return chain[i].CreatedAt.After(chain[j].CreatedAt)
		})
		headIdx := -1
		for i, r := range chain {
			if !pointedAt[r.ID] {
				headIdx = i
				break
			}
		}
		if headIdx < 0 {
			// Every row is pointed at (a cycle, or the true head is not in this
			// live set) — take the newest as head.
			headIdx = 0
		}
		head := chain[headIdx]
		successors := make([]agentRowView, 0, len(chain)-1)
		for i, r := range chain {
			if i == headIdx {
				continue
			}
			successors = append(successors, r)
		}
		groups = append(groups, agentLineageGroup{
			Head:       head,
			Successors: successors,
			rootID:     root,
			newest:     head.CreatedAt.Unix(),
		})
	}

	// Newest head first so the freshest chain leads the bucket.
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].newest > groups[j].newest
	})
	return groups
}

// bucketNewest is the sort key for a gardener bucket — the created-at (unix) of
// its newest lineage head, so the bucket with the most-recently-spawned agent
// floats up.
func bucketNewest(b agentGardenerBucket) int64 {
	if len(b.Lineages) == 0 {
		return 0
	}
	return b.Lineages[0].newest
}

// agentLineageRootID returns the agent bead's succession-chain root:
// lineage_root_id when set, else the bead's own id (a first-of-line agent is the
// root of its own one-run chain). Total — never empty for a bead with an id.
func agentLineageRootID(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if b.Fields != nil {
		if root := strings.TrimSpace(b.Fields["lineage_root_id"]); root != "" {
			return root
		}
	}
	return b.ID
}
