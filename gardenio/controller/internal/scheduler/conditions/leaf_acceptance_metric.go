package conditions

import (
	"context"
	"fmt"

	"gardenio/controller/internal/beadsapi"
)

// AcceptanceMetricsClient is the optional capability needed to evaluate the
// "acceptance_metric" daemon leaf. *beadsapi.Client satisfies it; tests can
// stub it independently of BeadClient.
type AcceptanceMetricsClient interface {
	GetAcceptanceMetrics(ctx context.Context) (*beadsapi.AcceptanceMetrics, error)
}

// evalAcceptanceMetric reads the three C5 acceptance signals (ka-0000004365)
// from the daemon and compares the field selected by l.Field against
// l.Value using CompareCount. Fractions are exposed as integer percent
// (0–100) so they fit the existing count-comparison vocabulary.
//
// Supported l.Field values:
//
//	owner_share_matt.fraction_pct  -> 0..100, percent of last-24h beads owned by Matt
//	owner_share_matt.matt          -> count of Matt-owned beads in last 24h
//	owner_share_matt.total         -> total beads in last 24h
//	agent_operator_leaks.count     -> last-1h writes with agent-shape operator
//	fleet_fallback_count.count     -> last-1h writes that fell back to a fleet sentinel
//	operator_alias_coverage.fraction_pct -> 0..100, share of users with linked alias pair
//	operator_alias_coverage.matched -> count of users with matched alias pair
//	operator_alias_coverage.total   -> total user beads with both identities
func evalAcceptanceMetric(ctx context.Context, client BeadClient, l Leaf) (bool, error) {
	amc, ok := client.(AcceptanceMetricsClient)
	if !ok {
		return false, fmt.Errorf("acceptance_metric: client does not support GetAcceptanceMetrics")
	}
	m, err := amc.GetAcceptanceMetrics(ctx)
	if err != nil {
		return false, fmt.Errorf("acceptance_metric: %w", err)
	}
	val, err := acceptanceMetricField(m, l.Field)
	if err != nil {
		return false, err
	}
	return CompareCount(val, l.Op, l.Value), nil
}

func acceptanceMetricField(m *beadsapi.AcceptanceMetrics, field string) (int, error) {
	switch field {
	case "owner_share_matt.fraction_pct":
		return int(m.OwnerShareMatt.Fraction * 100), nil
	case "owner_share_matt.matt":
		return m.OwnerShareMatt.Matt, nil
	case "owner_share_matt.total":
		return m.OwnerShareMatt.Total, nil
	case "agent_operator_leaks.count", "agent_operator_leaks":
		return m.AgentOperatorLeaks.Count, nil
	case "fleet_fallback_count.count", "fleet_fallback_count":
		return m.FleetFallbackCount.Count, nil
	case "operator_alias_coverage.fraction_pct":
		return int(m.OperatorAliasCoverage.Fraction * 100), nil
	case "operator_alias_coverage.matched":
		return m.OperatorAliasCoverage.Matched, nil
	case "operator_alias_coverage.total":
		return m.OperatorAliasCoverage.Total, nil
	default:
		return 0, fmt.Errorf("acceptance_metric: unknown field %q", field)
	}
}
