package server

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/model"
)

// propagateReadTaint implements docs/adr/taints.md §4.2 row 3 (inherit:read):
// any read of a tainted bead by an agent acquires that bead's taints onto
// the agent's own bead, so the agent's session (and, per p1-prop-write, its
// subsequent writes) carries the same mark as the data it read. A non-agent
// caller (OIDC human, primary static token, fleet token) never acquires —
// resolveTaintAgentBeadID reports ok=false and this is a no-op. So is any
// read while the taints registry is empty (§10.1).
//
// Cost control (the ADR's own framing: "the one with a cost"): a taint the
// agent already carries costs nothing. taintContextOf(ctx) IS the phase-1
// p1-taint-context cache — a taint present in the agent's cached set means
// no store write here, and a taint newly acquired below is folded back into
// that same cache entry, so a second read of the same taint within
// identityCacheTTL is a cache hit too (no bead_taints query, no insert).
//
// §15.3 trusted-subject amendment (operator decision 2026-09-20, "decided,
// not optional"): acquisition is skipped for a taint whose effects exclude
// `gate` when the agent already holds a grant for it — a granted reviewer's
// verdict must not itself be marked untrusted, or the review loop could
// never close (Biba trusted-subject rule). This is keyed ONLY on the
// taint's effect set: a taint whose effects include `gate` is never skipped
// this way — a granted agent still acquires it on read, per the §15.3
// table's `gate` row ("the grant clears the bead for reading; it does not
// exempt the reader from carrying the mark").
func (s *BeadsServer) propagateReadTaint(ctx context.Context, beadID string) error {
	agentBeadID, ok := resolveTaintAgentBeadID(ctx)
	if !ok {
		return nil
	}
	if nonEmpty, err := s.taintRegistryNonEmpty(ctx); err != nil {
		return fmt.Errorf("checking taint registry: %w", err)
	} else if !nonEmpty {
		return nil
	}

	agentTaints, err := s.taintContextOf(ctx)
	if err != nil {
		return fmt.Errorf("resolving agent taint context: %w", err)
	}

	// GetAllTaintsForBead, not the gate-filtered GetTaintsForBead the read
	// gate itself used to authorize this read: inheritance is effect-
	// agnostic (same reasoning as resolveWriteTaints / the triggered-by/
	// derived-from union in taint_propagate_write.go) — a `{}`-effect trace
	// dye or a reserved `restrict`/`evict`/`sink` taint on the bead must
	// still land on the reading agent even though it never gated the read.
	beadTaintIDs, err := s.store.GetAllTaintsForBead(ctx, beadID)
	if err != nil {
		return fmt.Errorf("resolving bead taints: %w", err)
	}
	return s.acquireReadTaints(ctx, agentBeadID, agentTaints, []string{beadID}, map[string][]int64{beadID: beadTaintIDs})
}

// propagateReadTaintBeads is propagateReadTaint for a read that returns many
// beads at once: a list, search, batch, similar or mentions page (ADR §4.2
// "any read of a tainted bead by an agent"; gd-WdnPW8TLPq). Every bead in
// beadIDs is text the caller has now taken in, so the agent acquires the
// union of their taints, at one bead_taints query per page. Callers pass only
// beads that are in the response, after every read gate has filtered.
//
// It is a no-op, with no store call, for a non-agent caller or an empty
// taints registry, so a zero-config install pays nothing on its hot list
// paths. A non-nil error must fail the read: answering without the taint
// would hand the agent the text without the mark.
func (s *BeadsServer) propagateReadTaintBeads(ctx context.Context, beadIDs []string) error {
	if len(beadIDs) == 0 {
		return nil
	}
	agentBeadID, ok := resolveTaintAgentBeadID(ctx)
	if !ok {
		return nil
	}
	if nonEmpty, err := s.taintRegistryNonEmpty(ctx); err != nil {
		return fmt.Errorf("checking taint registry: %w", err)
	} else if !nonEmpty {
		return nil
	}

	agentTaints, err := s.taintContextOf(ctx)
	if err != nil {
		return fmt.Errorf("resolving agent taint context: %w", err)
	}
	taintsByBead, err := s.store.GetAllTaintsForBeads(ctx, beadIDs)
	if err != nil {
		return fmt.Errorf("resolving bead taints: %w", err)
	}
	return s.acquireReadTaints(ctx, agentBeadID, agentTaints, beadIDs, taintsByBead)
}

// beadIDsOf returns the ids of beads, in order.
func beadIDsOf(beads []*model.Bead) []string {
	out := make([]string, 0, len(beads))
	for _, b := range beads {
		out = append(out, b.ID)
	}
	return out
}

// semanticResultIDs returns the bead ids of results, in order.
func semanticResultIDs(results []*model.SemanticResult) []string {
	out := make([]string, 0, len(results))
	for _, res := range results {
		out = append(out, res.Bead.ID)
	}
	return out
}

// referenceSummaryIDs returns the ids of the referenced beads in rows that
// resolved to a bead; a dangling reference has no title and no taints.
func referenceSummaryIDs(rows []*model.BeadReferenceSummary) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Title != "" || row.Type != "" {
			out = append(out, row.ID)
		}
	}
	return out
}

// graphNodeBeadIDs returns the bead ids of a graph response's nodes.
func graphNodeBeadIDs(nodes []model.GraphNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// eventBeadIDs returns the distinct owning bead ids of evts, in order.
func eventBeadIDs(evts []*model.Event) []string {
	seen := make(map[string]bool, len(evts))
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		if e.BeadID == "" || seen[e.BeadID] {
			continue
		}
		seen[e.BeadID] = true
		out = append(out, e.BeadID)
	}
	return out
}

// acquireReadTaints applies to agentBeadID every taint in taintsByBead the
// agent does not already carry (agentTaints), less the §15.3 trusted-subject
// skips, and folds what it applied into the taint-context cache. beadIDs
// fixes the order: an acquired taint's source_bead_id is the first bead in
// beadIDs that carries it.
func (s *BeadsServer) acquireReadTaints(ctx context.Context, agentBeadID string, agentTaints TaintSet, beadIDs []string, taintsByBead map[string][]int64) error {
	var missing []int64
	sourceOf := make(map[int64]string)
	for _, beadID := range beadIDs {
		for _, taintID := range taintsByBead[beadID] {
			if agentTaints[taintID] {
				continue
			}
			if _, seen := sourceOf[taintID]; seen {
				continue
			}
			sourceOf[taintID] = beadID
			missing = append(missing, taintID)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	toAcquire, err := s.filterTrustedSubjectSkips(ctx, agentBeadID, missing)
	if err != nil {
		return err
	}
	if len(toAcquire) == 0 {
		return nil
	}

	appliedBy := actorFromContext(ctx)
	acquired := make(TaintSet, len(toAcquire))
	for _, taintID := range toAcquire {
		if _, err := s.store.ApplyTaint(ctx, &model.BeadTaint{
			BeadID:       agentBeadID,
			TaintID:      taintID,
			Source:       model.TaintSourceInheritRead,
			SourceBeadID: sourceOf[taintID],
			AppliedBy:    appliedBy,
		}); err != nil {
			return fmt.Errorf("applying taint %d (inherit:read) to agent %s: %w", taintID, agentBeadID, err)
		}
		acquired[taintID] = true
	}

	// Fold the newly-acquired taints into the SAME per-agent cache entry
	// taintContextOf reads, so a repeat read within identityCacheTTL sees
	// them as already-held (missing above comes back empty) and performs no
	// further store call at all.
	s.initTaintContextCache()
	merged := make(TaintSet, len(agentTaints)+len(acquired))
	for id := range agentTaints {
		merged[id] = true
	}
	for id := range acquired {
		merged[id] = true
	}
	s.taintContextCache.set(agentBeadID, merged)
	return nil
}

// filterTrustedSubjectSkips applies the §15.3 trusted-subject rule to
// candidateTaintIDs (taints agentBeadID does not yet carry), dropping any
// whose effects exclude `gate` when the agent itself holds a grant for it
// (agentOwnGrants: its spawn-chain root's grants do not make it a trusted
// subject, §15.7). It resolves the agent's grants and, only when at least
// one grant is held, the effects of the candidate taints — both skipped
// entirely for the common case of an ungranted agent, which never touches
// taintEffectsByID.
func (s *BeadsServer) filterTrustedSubjectSkips(ctx context.Context, agentBeadID string, candidateTaintIDs []int64) ([]int64, error) {
	grants, err := s.agentOwnGrants(ctx, agentBeadID)
	if err != nil {
		return nil, fmt.Errorf("resolving agent grants: %w", err)
	}
	if len(grants) == 0 {
		return candidateTaintIDs, nil
	}
	granted := make(map[int64]bool, len(grants))
	for _, g := range grants {
		granted[g] = true
	}

	effectsByID, err := s.taintEffectsByID(ctx, candidateTaintIDs)
	if err != nil {
		return nil, fmt.Errorf("resolving taint effects: %w", err)
	}

	out := make([]int64, 0, len(candidateTaintIDs))
	for _, taintID := range candidateTaintIDs {
		effects, known := effectsByID[taintID]
		if known && granted[taintID] && !model.HasEffect(effects, model.EffectGate) {
			continue
		}
		out = append(out, taintID)
	}
	return out, nil
}

// taintEffectsByID resolves the effects of each taint in ids via the full
// registry (ListTaints). The registry is small — the same "tiny, changes
// rarely" property ADR docs/adr/taints.md invokes for the per-agent taint
// set applies to the taint registry itself — and this only runs on the
// trusted-subject branch above, itself already gated behind the agent
// holding at least one grant, so it never touches the zero-config hot path.
func (s *BeadsServer) taintEffectsByID(ctx context.Context, ids []int64) (map[int64][]string, error) {
	all, err := s.store.ListTaints(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make(map[int64][]string, len(ids))
	for _, t := range all {
		if !want[t.ID] {
			continue
		}
		effects := t.Effects
		if effects == nil {
			// A taint row with no effects set defaults to {gate} — the
			// taints.effects column default (model.DefaultEffects,
			// docs/adr/taints.md §15.2) — not to "gates nothing". An
			// explicit empty slice (the legal trace-only-dye case, §15.2)
			// is left as-is and correctly excludes gate.
			effects = model.DefaultEffects()
		}
		out[t.ID] = effects
	}
	return out, nil
}
