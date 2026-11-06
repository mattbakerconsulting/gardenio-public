package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// AcceptanceMetrics mirrors store.AcceptanceMetrics (kbeads); see
// kbeads/internal/store/store.go for the canonical doc. Returned by
// GET /v1/metrics/acceptance and rendered by the `gb metrics`
// subcommands.
type AcceptanceMetrics struct {
	OwnerShareMatt        AcceptanceOwnerShare    `json:"owner_share_matt"`
	AgentOperatorLeaks    AcceptanceOperatorLeaks `json:"agent_operator_leaks"`
	FleetFallbackCount    AcceptanceFleetFallback `json:"fleet_fallback_count"`
	OperatorAliasCoverage AcceptanceAliasCoverage `json:"operator_alias_coverage"`
}

// AcceptanceOwnerShare is the fraction of last-24h beads owned by
// Matt, resolved across his slack:/oidc: identities via
// operator_aliases (ka-0000005200).
type AcceptanceOwnerShare struct {
	Total    int     `json:"total"`
	Matt     int     `json:"matt"`
	Fraction float64 `json:"fraction"`
}

// AcceptanceOperatorLeaks counts last-1h mutation_history rows whose
// operator points at an agent-shape operators row. Steady-state value
// MUST be 0 (ka-0000006809).
type AcceptanceOperatorLeaks struct {
	Count int `json:"count"`
}

// AcceptanceFleetFallback counts last-1h mutation_history rows whose
// operator resolves to a fleet sentinel (kind='fleet'). These are
// resolver regressions that lost the human; steady-state value MUST be
// 0 (ka-0000005200, epic ka-0000000095).
type AcceptanceFleetFallback struct {
	Count int `json:"count"`
}

// AcceptanceAliasCoverage measures the share of type=user beads with
// a fully-linked slack ↔ oidc alias pair.
type AcceptanceAliasCoverage struct {
	Total    int     `json:"total"`
	Matched  int     `json:"matched"`
	Fraction float64 `json:"fraction"`
}

// GetAcceptanceMetrics pulls the three C5 acceptance signals
// (ka-0000004365) from the daemon in one round trip.
func (c *Client) GetAcceptanceMetrics(ctx context.Context) (*AcceptanceMetrics, error) {
	var m AcceptanceMetrics
	if err := c.doJSON(ctx, http.MethodGet, "/v1/metrics/acceptance", nil, &m); err != nil {
		return nil, fmt.Errorf("acceptance metrics: %w", err)
	}
	return &m, nil
}
