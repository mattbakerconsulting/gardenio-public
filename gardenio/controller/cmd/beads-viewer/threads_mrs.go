package main

import (
	"context"

	"gardenio/controller/internal/beadsapi"
)

// gb-ux follow-up (ka-0000003947): make /threads a richer mirror of Slack by
// surfacing the concrete WORK a wish spawned. This file is the DATA aggregator
// for the "Produced MRs" section of the /threads/{id} detail rail: the merge
// requests a wish's member beads produced.
//
// Why MRs are invisible today. A thread's membership is UNION(field thread_id,
// parent-child closure) — see threadMembers in threads.go. A merge-request bead
// is NOT a member: it is a separate type=merge-request bead reached from a
// member (a task/agent bead) via a FORWARD `produces` dep edge
// (task --produces--> merge-request; see beadsapi.MRBeadForTask / epic
// ka-0000001613). So the fused timeline and the work-member roster never show
// the code that shipped. threadProducedMRs closes that gap: it walks the
// produces edges out of the resolved member set and projects each MR bead onto
// the SAME wrapUpMRView shape the /agents "Produced MRs" section uses
// (agents.go), so the /threads rail renders identical mr_pill chrome.
//
// Read-only by construction: it issues only the batched BatchGetDependencies /
// BatchGetBeads read queries, mutates nothing, and carries no operator gate —
// the same posture as every other /threads aggregator (threads.go).

// threadMRStore is the narrow store surface threadProducedMRs needs: the
// batched forward-dependency walk (member --produces--> X) plus the batched
// bead fetch that resolves each candidate to a merge-request bead. Both methods
// are already on threadTimelineStore and satisfied structurally by *Server.daemon
// (beadAPI), so the aggregator is drivable by the tiny threadFake in tests.
type threadMRStore interface {
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// threadProducedMRs returns the merge-request beads produced by any member of a
// thread, projected onto the shared wrapUpMRView pill shape and de-duplicated.
//
// memberIDs is the thread's resolved member set (fuseThreadTimeline's
// tl.Members — field ∪ parent-child closure, in dep-first order). For each
// member the function collects the FORWARD `produces` dep targets, batch-fetches
// them, and keeps only the ones that are actually type=merge-request beads
// carrying a canonical mr_url (a pill with no URL would dead-link). The result
// is ordered by member position then discovery order (deterministic, dep-first),
// so the rail lists MRs in the same stable order the work roster uses.
//
// Cost: exactly two batched round-trips regardless of member count — one
// BatchGetDependencies(forward) over every member, one BatchGetBeads over the
// unique produced candidates — mirroring the constellation's batched fan-out
// rather than the per-bead MRBeadForTask/MRBeadsForAgent walk.
//
// Returns nil (not an error) when there are no members or nothing was produced.
// Transport failures propagate so the handler can degrade to omitting the
// section rather than 500ing.
func threadProducedMRs(ctx context.Context, store threadMRStore, memberIDs []string) ([]wrapUpMRView, error) {
	if store == nil || len(memberIDs) == 0 {
		return nil, nil
	}

	fwd, err := store.BatchGetDependencies(ctx, memberIDs, "forward")
	if err != nil {
		return nil, err
	}

	// Collect unique produced-MR candidate ids in member order (dep-first), then
	// discovery order within a member. Dedup so an MR produced by several members
	// (a single MR linked to multiple task beads — the multi-producer pattern
	// beadsapi.UpsertMergeRequestBead preserves) appears once.
	var candidateIDs []string
	seen := make(map[string]bool)
	for _, mid := range memberIDs {
		for _, d := range fwd[mid] {
			if d.Type != "produces" || d.DependsOnID == "" {
				continue
			}
			if seen[d.DependsOnID] {
				continue
			}
			seen[d.DependsOnID] = true
			candidateIDs = append(candidateIDs, d.DependsOnID)
		}
	}
	if len(candidateIDs) == 0 {
		return nil, nil
	}

	beads, err := store.BatchGetBeads(ctx, candidateIDs)
	if err != nil {
		return nil, err
	}

	out := make([]wrapUpMRView, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		b := beads[id]
		// A candidate the daemon did not return (deleted / cross-fleet) or one
		// that is not actually a merge-request bead is skipped — the produces
		// edge is authoritative for membership but the target must still resolve
		// to a real MR bead before it earns a pill.
		if b == nil || b.Type != "merge-request" {
			continue
		}
		url := b.Fields["mr_url"]
		if url == "" {
			// No canonical URL → the pill anchor would dead-link. Skip, matching
			// buildAgentProducedMRs.
			continue
		}
		out = append(out, wrapUpMRViewFromBead(url, b))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
