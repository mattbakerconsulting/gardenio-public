package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// C5 acceptance signals (ka-0000006219, epic ka-0000008007). The three numbers
// the gardener reads each cycle to decide whether the operated_by multi-user
// readiness epic is done — surfaced on /health as a tile so a human can read
// the same signal at a glance.
//
// Window semantics match the kbeads queries (queries_acceptance_metrics.go):
// owner-share is 24h, leaks is 1h, alias coverage is point-in-time over live
// type=user beads. Thresholds are advisory pill colours, not gates.
const (
	// operatorSignalsOwnerShareWarn classifies the Matt-as-owner share. The
	// epic's done-state is "near-baseline" (well under 50%); above the warn
	// line still renders the value but as a warn pill so a regression in the
	// resolver shows up red.
	operatorSignalsOwnerShareWarn = 0.10
	// operatorSignalsAliasCoverageWarn classifies coverage: 100% is the
	// target, below the warn line means users with both slack+oidc identities
	// are not being collapsed onto a single operator row.
	operatorSignalsAliasCoverageWarn = 0.90
)

// healthOperatorSignalsTileView is the template view model for the C5
// acceptance-signals tile. Stale and Error are mutually exclusive — Error
// short-circuits the render.
type healthOperatorSignalsTileView struct {
	OwnerShareMatt        operatorSignalRow
	AgentOperatorLeaks    operatorSignalRow
	FleetFallbackCount    operatorSignalRow
	OperatorAliasCoverage operatorSignalRow
	Error                 string
	AsOf                  string
}

// operatorSignalRow renders one signal line. Display is the formatted value
// ("8/100 (8%)", "0", "5/5"); State is one of {ok,warn,stale} so the
// template can choose a pill class without recomputing the threshold.
type operatorSignalRow struct {
	Label   string
	Display string
	State   string
	BeadID  string
}

const (
	operatorSignalStateOK   = "ok"
	operatorSignalStateWarn = "warn"
)

// acceptanceMetricsClient is the subset of beadsapi.Client the tile uses.
// Kept narrow so tests pass a fake without dragging in the full interface.
type acceptanceMetricsClient interface {
	GetAcceptanceMetrics(ctx context.Context) (*beadsapi.AcceptanceMetrics, error)
}

// handleHealthTileOperatorSignals renders the C5 acceptance-signals tile for
// hx-get refresh. The metrics live at /v1/metrics/acceptance; a daemon fault
// surfaces inline rather than failing the whole /health page.
func (s *Server) handleHealthTileOperatorSignals(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthSnapshotTimeout)
	defer cancel()
	view := buildOperatorSignalsTileView(ctx, s.acceptanceMetricsAPI(), time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_operator_signals.html", "health_tile_operator_signals", map[string]any{
		"OperatorSignals": view,
	})
}

// acceptanceMetricsAPI returns the daemon-backed client used for the tile.
// Pulled out so tests can swap in a fake without re-wiring the full beadAPI
// surface; nil-safe via the buildOperatorSignalsTileView guard.
func (s *Server) acceptanceMetricsAPI() acceptanceMetricsClient {
	if s == nil || s.daemon == nil {
		return nil
	}
	if c, ok := s.daemon.(acceptanceMetricsClient); ok {
		return c
	}
	return nil
}

// buildOperatorSignalsTileView shapes the three metrics into the tile view.
// Each row carries its own ok/warn state so the template renders a stable
// pill colour even when one signal is degraded and another is healthy.
func buildOperatorSignalsTileView(ctx context.Context, api acceptanceMetricsClient, now time.Time) healthOperatorSignalsTileView {
	v := healthOperatorSignalsTileView{AsOf: now.UTC().Format(time.RFC3339)}
	if api == nil {
		v.Error = "acceptance metrics unavailable"
		return v
	}
	m, err := api.GetAcceptanceMetrics(ctx)
	if err != nil || m == nil {
		v.Error = "acceptance metrics unavailable"
		return v
	}

	// Row 1: owner-share-matt (24h). Display fraction as percent; warn when
	// the Matt sentinel still owns more than the threshold of recent writes.
	osState := operatorSignalStateOK
	if m.OwnerShareMatt.Fraction > operatorSignalsOwnerShareWarn {
		osState = operatorSignalStateWarn
	}
	v.OwnerShareMatt = operatorSignalRow{
		Label:   "Matt-as-owner (24h)",
		Display: fmt.Sprintf("%d/%d (%s)", m.OwnerShareMatt.Matt, m.OwnerShareMatt.Total, formatPercent(m.OwnerShareMatt.Fraction)),
		State:   osState,
	}

	// Row 2: agent-operator-leaks (1h). Steady-state MUST be zero per
	// ka-0000006809 Invariant §1; anything non-zero is a resolver regression.
	leaksState := operatorSignalStateOK
	if m.AgentOperatorLeaks.Count > 0 {
		leaksState = operatorSignalStateWarn
	}
	v.AgentOperatorLeaks = operatorSignalRow{
		Label:   "Agent-operator leaks (1h)",
		Display: fmt.Sprintf("%d", m.AgentOperatorLeaks.Count),
		State:   leaksState,
	}

	// Row 3: fleet-fallback-count (1h). Fleet-sentinel writes are the
	// resolver regression epic ka-0000000095 exists to kill; the leak
	// query deliberately excludes fleet, so without this row a fleet
	// fallback reads as zero leaks (ka-0000005200). Any non-zero count is
	// warn — a human was lost on the way to the daemon.
	fleetState := operatorSignalStateOK
	if m.FleetFallbackCount.Count > 0 {
		fleetState = operatorSignalStateWarn
	}
	v.FleetFallbackCount = operatorSignalRow{
		Label:   "Fleet fallbacks (1h)",
		Display: fmt.Sprintf("%d", m.FleetFallbackCount.Count),
		State:   fleetState,
	}

	// Row 4: operator-alias-coverage (point-in-time). 100% means every
	// type=user bead with both slack_id and email has a matched alias pair;
	// less than that means agent-spawned writes for the same human risk
	// resolving to two distinct operator rows. A zero population is fine —
	// the metric carries no warning when no users qualify.
	covState := operatorSignalStateOK
	if m.OperatorAliasCoverage.Total > 0 && m.OperatorAliasCoverage.Fraction < operatorSignalsAliasCoverageWarn {
		covState = operatorSignalStateWarn
	}
	v.OperatorAliasCoverage = operatorSignalRow{
		Label:   "Alias coverage",
		Display: fmt.Sprintf("%d/%d (%s)", m.OperatorAliasCoverage.Matched, m.OperatorAliasCoverage.Total, formatPercent(m.OperatorAliasCoverage.Fraction)),
		State:   covState,
	}
	return v
}

// formatPercent renders a 0..1 fraction as a short percent label. The tile
// keeps no decimals because a single integer digit per row is plenty —
// distinguishing 8% from 8.4% does not help the operator at a glance, and
// the underlying integers are right next to it for anyone who needs precision.
func formatPercent(f float64) string {
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return fmt.Sprintf("%d%%", int(f*100+0.5))
}
