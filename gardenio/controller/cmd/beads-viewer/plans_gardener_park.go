package main

// plans_gardener_park.go — the "parked" line on the /plans/{id} plan-agent card
// (task gd-f2fc4jcRn3, the plan-page half of gd-FUC635HJ2E ask 2). The engine
// comments on the plan epic when a gardener parks; this surfaces the same fact
// where the operator looks: the plan's gardener is dormant, and on what.
//
// The verdict is computed LIVE by bridge.ReconcileDormancyProbe — the probe
// `gb trigger reconcile` uses, which calls the engine's own dormant-gate
// predicates (operator_wedge, mr_merged). The stamped dormant_on_escalation /
// dormant_blocker fields are never trusted for it: dormant_blocker is
// write-once and goes stale when the wedge moves to another bead. When the
// stamp disagrees with the live blocker the card says so.

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// planGardenerPark is the live park of a plan's gardener, or nil on the view
// when the gardener is not parked (or the probe could not prove it).
type planGardenerPark struct {
	// Kind is the predicate that proved the park: "operator_wedge" (every open
	// child waits behind an open escalation/decision) or "mr_merged" (a declared
	// block-until-MR-merged loop with an unmerged tracked MR).
	Kind string
	// BlockerID is the live blocker bead; BlockerTitle / BlockerType are read
	// from it best-effort (empty on a lookup miss — the id still renders).
	BlockerID    string
	BlockerTitle string
	BlockerType  string
	// StaleStamp is the stamped dormant_blocker when it names a different bead
	// than the live one, so the card can say the stamp is superseded.
	StaleStamp string
}

// planGardenerDormancyProbe is the live dormancy probe the cockpit calls.
// Server.gardenerDormancyProbe overrides it in tests; nil means this.
// A panic in the probe is logged and reads as "not parked": the line is
// best-effort and must never fail the cockpit render.
func (s *Server) planGardenerDormancyProbe(ctx context.Context, gardener *beadsapi.BeadDetail) (d *bridge.ReconcileDormancy) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Warn("plan gardener park: dormancy probe panicked", "gardener", gardener.ID, "panic", r)
			d = nil
		}
	}()
	if s.gardenerDormancyProbe != nil {
		return s.gardenerDormancyProbe(ctx, gardener)
	}
	return bridge.ReconcileDormancyProbeReadOnly(ctx, s.daemon, s.logger, gardener)
}

// planGardenerParkFor returns the live park of a plan's bound gardener, or nil.
// Only a running gardener is probed: a paused or retired one already says why
// it fires nothing, and a gardener-less plan has nothing to park. Fail-safe
// toward nil, as the probe is: only a positively proven park renders.
func (s *Server) planGardenerParkFor(ctx context.Context, gardener *beadsapi.BeadDetail, state planGardenerState) *planGardenerPark {
	if gardener == nil || state != planGardenerStateRunning {
		return nil
	}
	d := s.planGardenerDormancyProbe(ctx, gardener)
	if d == nil || !d.Parked || strings.TrimSpace(d.LiveBlocker) == "" {
		return nil
	}
	p := &planGardenerPark{Kind: d.Kind, BlockerID: d.LiveBlocker}
	if d.StampedBlockerStale {
		p.StaleStamp = d.StampedBlocker
	}
	if b, err := s.daemon.GetBead(ctx, d.LiveBlocker); err == nil && b != nil {
		p.BlockerTitle = b.Title
		p.BlockerType = b.Type
	}
	return p
}
