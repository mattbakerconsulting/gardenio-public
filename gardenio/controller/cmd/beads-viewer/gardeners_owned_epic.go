package main

// Per-gardener owned-epic rollup for the /maps board (task ka-0000001266, epic
// ka-0000006790 — maps2 v2 §6 deferred roadmap).
//
// Every gardener that drives an epic carries that epic id on the gardener bead:
// the modern trigger:schedule shape pins it via a `gardener-epic:<id>` label, the
// legacy type=schedule shape via the `assign_bead` field (gardenerOwnedEpicID,
// mirroring cmd/gb/gardener.go gardenerEpicOf). This surfaces, inline on the card,
// HOW FAR that epic has drained — a "12/40 done · 30%" rollup over the epic's
// issue-kind subtree — so an operator can see a gardener's progress against its
// goal without drilling into the epic.
//
// It is batch-hydrated to avoid N+1 (the same shape as enrichGardenerRunCounts):
// the distinct owned-epic ids across all rows are resolved with NO fetch in
// gardenerRowFromBead, then enrichGardenerOwnedEpics fans out two CountOnly subtree
// queries per distinct epic (total issue descendants + closed issue descendants)
// at a bounded concurrency, deduping so two gardeners pinned to the same epic
// share one pair of queries.

import (
	"context"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// gardenerOwnedEpicFanout bounds the concurrent subtree-count queries the
// owned-epic rollup issues. The gardener set is operator-bounded (capped at
// gardenersListLimit) and the distinct-epic subset is smaller still, so a modest
// fan-out drains it in one wave without flooding the daemon — mirroring
// gardenerRunCountFanout.
const gardenerOwnedEpicFanout = 8

// gardenerOwnedEpicID extracts the epic a gardener bead drives — the same
// resolution cmd/gb/gardener.go gardenerEpicOf uses: the legacy `assign_bead`
// field for type=schedule gardeners, else the `gardener-epic:<id>` label for the
// modern trigger:schedule shape. Returns "" when neither is present (an
// exit-condition / project-wide gardener with no single owned epic), in which
// case the card renders no rollup chip.
//
// This is deliberately the gardener's OWNED epic, not its goal-TARGET
// (gardenerGoalTargetID): a subtree-drain gardener's stop target is usually the
// same epic, but the owned-epic label is the authoritative "this gardener is
// responsible for this epic" pointer and is present even on gardeners whose stop
// spec is a non-subtree condition.
func gardenerOwnedEpicID(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if b.Fields != nil {
		if a := strings.TrimSpace(b.Fields["assign_bead"]); a != "" {
			return a
		}
	}
	for _, l := range b.Labels {
		if rest, ok := strings.CutPrefix(l, "gardener-epic:"); ok {
			if id := strings.TrimSpace(rest); id != "" {
				return id
			}
		}
	}
	return ""
}

// ownedEpicRollup is the per-epic count result, computed once per distinct epic
// and copied onto every row that owns it.
type ownedEpicRollup struct {
	Total  int // issue-kind descendants in the epic subtree (excludes the root)
	Closed int // those that are closed
}

// PctDone returns the integer percentage of the owned epic's issue subtree that
// is closed (0 when the subtree is empty, so a childless epic reads 0% rather
// than dividing by zero).
func (r ownedEpicRollup) PctDone() int {
	if r.Total <= 0 {
		return 0
	}
	return int(float64(r.Closed) / float64(r.Total) * 100)
}

// enrichGardenerOwnedEpics fills each row's owned-epic rollup fields
// (OwnedEpicTotal / OwnedEpicClosed / OwnedEpicOpen / OwnedEpicPct /
// HasOwnedEpic) from a bounded batch of CountOnly subtree queries.
//
// One pair of queries per DISTINCT owned epic (deduped across rows): a gardener
// fleet often has several gardeners pinned to the same epic, and two gardeners on
// one epic must not issue the rollup twice. Each pair counts the epic's
// issue-kind subtree (Kinds=[issue] restricts to work items — task/bug/feature/
// epic — so agent/config/data descendants never inflate the rollup) total and
// closed via the daemon's transitive parent-child subtree CTE (Parent), which
// already excludes the root epic itself.
//
// Bounded and degrade-safe: only rows carrying an owned-epic id are queried,
// fanned out at gardenerOwnedEpicFanout, two CountOnly queries per distinct epic.
// A query error leaves that epic's rows without a rollup (HasOwnedEpic stays
// false — the card renders no chip) rather than failing the page. An epic with
// zero issue descendants also renders no chip (HasOwnedEpic gated on Total > 0)
// so a gardener whose epic has no children carries no misleading "0/0 · 0%".
func (s *Server) enrichGardenerOwnedEpics(ctx context.Context, rows []gardenerRowView) {
	// Collect distinct owned-epic ids → the row indices that own each.
	epicRows := make(map[string][]int)
	order := make([]string, 0)
	for i := range rows {
		id := strings.TrimSpace(rows[i].ownedEpicID)
		if id == "" {
			continue
		}
		if _, seen := epicRows[id]; !seen {
			order = append(order, id)
		}
		epicRows[id] = append(epicRows[id], i)
	}
	if len(order) == 0 {
		return
	}

	rollups := make(map[string]ownedEpicRollup, len(order))
	var mu sync.Mutex
	sem := make(chan struct{}, gardenerOwnedEpicFanout)
	var wg sync.WaitGroup
	for _, epicID := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func(epicID string) {
			defer wg.Done()
			defer func() { <-sem }()
			roll, ok := s.deriveOwnedEpicRollup(ctx, epicID)
			if !ok {
				return // query failed — leave the epic's rows without a rollup
			}
			mu.Lock()
			rollups[epicID] = roll
			mu.Unlock()
		}(epicID)
	}
	wg.Wait()

	for epicID, idxs := range epicRows {
		roll, ok := rollups[epicID]
		if !ok || roll.Total <= 0 {
			continue // failed query, or a childless epic — render no chip
		}
		for _, i := range idxs {
			rows[i].OwnedEpicID = epicID
			rows[i].OwnedEpicTotal = roll.Total
			rows[i].OwnedEpicClosed = roll.Closed
			rows[i].OwnedEpicOpen = roll.Total - roll.Closed
			rows[i].OwnedEpicPct = roll.PctDone()
			rows[i].HasOwnedEpic = true
		}
	}
}

// deriveOwnedEpicRollup counts an epic's issue-kind subtree (total + closed) via
// two daemon CountOnly queries over the transitive parent-child subtree. Returns
// (rollup, ok); ok is false when either count query errored, so a transient
// failure leaves the owning rows without a rollup rather than a wrong count.
func (s *Server) deriveOwnedEpicRollup(ctx context.Context, epicID string) (ownedEpicRollup, bool) {
	total, terr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:     []string{"issue"},
		Parent:    epicID,
		CountOnly: true,
	})
	if terr != nil {
		s.logger.Error("gardeners: owned-epic rollup (total)", "epic", epicID, "error", terr)
		return ownedEpicRollup{}, false
	}
	closed, cerr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:     []string{"issue"},
		Statuses:  []string{"closed"},
		Parent:    epicID,
		CountOnly: true,
	})
	if cerr != nil {
		s.logger.Error("gardeners: owned-epic rollup (closed)", "epic", epicID, "error", cerr)
		return ownedEpicRollup{}, false
	}
	return ownedEpicRollup{Total: total.Total, Closed: closed.Total}, true
}
