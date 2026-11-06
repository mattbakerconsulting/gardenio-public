package main

// maps_chains.go — gardener CHAINING on the /maps card-graph (epic ka-0000005510,
// task ka-0000004778 / "C2"). Builds on C1 (ka-0000002600, MR !4585): the
// `chain.to` schema landed the `chains_to` (forward, predecessor→heir) /
// `succeeds` (reverse, heir→predecessor) typed verbs and model.DepChainsTo, so
// the chain is now a real typed-mention edge between two gardener beads. This
// file turns those edges into the visible half the design (docs/
// gardener-chaining-design-2026-06-18.md §3.5, §4) calls the missing organ:
//
//   - A WORKFLOW is the connected component of `chains_to` edges among gardener
//     cards — laid out predecessor→heir as a left-to-right rail of cards joined
//     by directed arrows.
//   - The HANDOFF lights the arrow: dim while the predecessor is mid-loop, lit
//     the moment the predecessor reaches its terminal goal (GoalState ==
//     "achieved"), at which point the heir's card halo ignites.
//
// The chain edges ride on the existing typed-mention read path (GetMentions —
// the same one gardener_beads.go's constellation uses). C1 stamps the surface
// verb into bead_references.relationship_type, so a forward mention with verb
// `chains_to`/`chains-to` means "src is the predecessor of target", and a
// forward `succeeds` means "src is the heir of target". Fetching forward
// mentions on every gardener therefore captures every edge from at least one
// endpoint (chain.to is declared on the predecessor; the succeeds provenance is
// stamped on the heir — either side yields the same normalized edge), so a
// single GetMentions per gardener — deduped — recovers the whole graph without
// the reverse round-trip.
//
// Everything below the fetch is pure (no http, no daemon, no clock) so the
// workflow partitioning, topological ordering and handoff/halo state are
// unit-testable in isolation, matching maps_view.go's split.

import (
	"context"
	"sort"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// mapsChainEdge is one normalized predecessor→heir gardener-chaining edge. Both
// endpoints are kd-ids; direction is always predecessor (the gardener whose
// `chain.to` names the heir) → heir, regardless of whether it was recovered
// from a forward `chains_to` mention or a `succeeds` provenance mention.
type mapsChainEdge struct {
	Pred string
	Heir string

	// Planned marks a PENDING edge (task ka-0000005273 / "C3"): the predecessor
	// declares this heir via `chain.to` but the heir bead has not materialized
	// yet (the C4 engine spawns it at the predecessor's goal handoff). The Heir
	// is then a SYNTHETIC id (plannedHeirID), not a real gardener. The rail
	// renders a planned heir as a "pending" node — a drawn-but-not-yet-live
	// successor — distinct from a broken dangling chain. PlannedLabel is the
	// pending node's caption; PlannedFanOut marks a multi-heir declaration.
	Planned       bool
	PlannedLabel  string
	PlannedFanOut bool
}

// chainVerbs is the set of surface verbs (lowercased, as C1 stores them in
// bead_references.relationship_type) that denote a gardener-chaining edge.
// `chains_to`/`chains-to` are forward (src is predecessor); `succeeds` is the
// reverse provenance (src is heir). All three map to model.DepChainsTo in
// kbeads; here we only need to recover the direction.
func chainEdgeFromMention(srcID string, relType, targetID string) (mapsChainEdge, bool) {
	srcID = strings.TrimSpace(srcID)
	targetID = strings.TrimSpace(targetID)
	if srcID == "" || targetID == "" || srcID == targetID {
		return mapsChainEdge{}, false
	}
	switch strings.ToLower(strings.TrimSpace(relType)) {
	case "chains_to", "chains-to":
		// src chains_to target → src is the predecessor, target the heir.
		return mapsChainEdge{Pred: srcID, Heir: targetID}, true
	case "succeeds":
		// src succeeds target → src is the heir, target the predecessor.
		return mapsChainEdge{Pred: targetID, Heir: srcID}, true
	}
	return mapsChainEdge{}, false
}

// chainEdgesFromMentions extracts every gardener-chaining edge declared by one
// gardener's forward mentions, normalized to predecessor→heir. Non-chain verbs
// (parent, blocks, untyped mentions, …) are ignored.
func chainEdgesFromMentions(srcID string, fwd []beadsapi.BeadReferenceSummary) []mapsChainEdge {
	var out []mapsChainEdge
	for _, r := range fwd {
		if r.RelationshipType == nil {
			continue
		}
		if e, ok := chainEdgeFromMention(srcID, *r.RelationshipType, r.ID); ok {
			out = append(out, e)
		}
	}
	return out
}

// plannedChainEdges synthesizes PLANNED predecessor→heir edges (task
// ka-0000005273 / "C3") from each gardener row's declared-but-unmaterialized
// `chain.to` successors (row.PlannedHeirs). Each becomes an edge to a synthetic
// pending node so a drawn successor renders on the card-graph before the engine
// spawns it.
//
// Materialized-heir dedup: a gardener whose heir has ALREADY been spawned
// carries BOTH a real `succeeds`/`chains_to` edge (from loadGardenerChainEdges)
// AND its still-present `chain.to` declaration. Rendering the planned node too
// would double the heir. We can't match a synthetic id to a real heir id, so
// we suppress a gardener's planned edges once it has AS MANY real materialized
// heirs as it declares — i.e. the chain is fully spawned. A partially-spawned
// fan-out still shows its not-yet-spawned remainder as pending. materialized is
// the set of predecessor ids that already have >=1 real chained heir, with the
// per-predecessor real-heir count.
func plannedChainEdges(rows []gardenerRowView) []mapsChainEdge {
	var out []mapsChainEdge
	for _, row := range rows {
		if len(row.PlannedHeirs) == 0 {
			continue
		}
		for _, ph := range row.PlannedHeirs {
			out = append(out, mapsChainEdge{
				Pred:          row.ID,
				Heir:          ph.ID,
				Planned:       true,
				PlannedLabel:  ph.Label,
				PlannedFanOut: ph.FanOut,
			})
		}
	}
	return out
}

// suppressSpawnedPlannedEdges drops planned edges for a predecessor that has
// already materialized all its declared heirs, keeping the pending remainder of
// a partially-spawned fan-out. realHeirCount maps a predecessor id to how many
// real (materialized) chained heirs it has (from the succeeds/chains_to edges).
// A predecessor's planned edges are kept only for the heirs BEYOND its real
// count, so a fully-spawned chain shows no pending nodes while a
// half-spawned fan-out shows just the unspawned ones.
func suppressSpawnedPlannedEdges(edges []mapsChainEdge, realHeirCount map[string]int) []mapsChainEdge {
	// Count planned edges seen per predecessor as we go so we can skip the first
	// realHeirCount[pred] of them (those are assumed already materialized).
	seenPlanned := make(map[string]int)
	out := edges[:0]
	for _, e := range edges {
		if !e.Planned {
			out = append(out, e)
			continue
		}
		idx := seenPlanned[e.Pred]
		seenPlanned[e.Pred] = idx + 1
		if idx < realHeirCount[e.Pred] {
			// This declared heir is assumed already materialized (a real edge
			// covers it) — drop the duplicate pending node.
			continue
		}
		out = append(out, e)
	}
	return out
}

// mapsChainFetchCap bounds how many gardeners the /maps board will fan out
// mention lookups for. The live fleet is small (operator-authored), but
// ?all=1 can list every gardener ever; past this cap the per-gardener mention
// fan-out stops paying for itself, so chain rendering degrades to "no
// workflows" (the plain card grid still renders). The cap is generous enough
// that no realistic live fleet is truncated.
const mapsChainFetchCap = 300

// mapsChainFanout bounds the concurrent per-gardener mention lookups (mirrors
// gardenerMentionFanout in gardener_beads.go).
const mapsChainFanout = 16

// loadGardenerChainEdges fetches forward mentions for each gardener id and
// returns the deduped set of normalized predecessor→heir chaining edges. A nil
// daemon, an over-cap id set, or per-gardener fetch errors degrade to "fewer
// edges" — never an error — so a chain-fetch hiccup can never 500 the board.
func (s *Server) loadGardenerChainEdges(ctx context.Context, ids []string) []mapsChainEdge {
	if s.daemon == nil || len(ids) == 0 || len(ids) > mapsChainFetchCap {
		return nil
	}
	perGardener := make([][]mapsChainEdge, len(ids))
	sem := make(chan struct{}, mapsChainFanout)
	var wg sync.WaitGroup
	for i, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			fwd, err := s.daemon.GetMentions(ctx, id, gardenerMentionLimit)
			if err != nil {
				return
			}
			perGardener[i] = chainEdgesFromMentions(id, fwd)
		}(i, id)
	}
	wg.Wait()

	seen := make(map[mapsChainEdge]struct{})
	var out []mapsChainEdge
	for _, edges := range perGardener {
		for _, e := range edges {
			if _, dup := seen[e]; dup {
				continue
			}
			seen[e] = struct{}{}
			out = append(out, e)
		}
	}
	// Deterministic order (Pred, Heir) so downstream partitioning + tests are
	// stable.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pred != out[j].Pred {
			return out[i].Pred < out[j].Pred
		}
		return out[i].Heir < out[j].Heir
	})
	return out
}
