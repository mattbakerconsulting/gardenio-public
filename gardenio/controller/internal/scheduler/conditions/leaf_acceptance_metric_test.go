package conditions

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stubAcceptanceClient embeds stubBeadClient and adds GetAcceptanceMetrics so
// it satisfies AcceptanceMetricsClient through a type assertion.
type stubAcceptanceClient struct {
	stubBeadClient
	metrics *beadsapi.AcceptanceMetrics
	err     error
}

func (s *stubAcceptanceClient) GetAcceptanceMetrics(_ context.Context) (*beadsapi.AcceptanceMetrics, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.metrics, nil
}

func TestEvaluate_AcceptanceMetricLeaf(t *testing.T) {
	good := &beadsapi.AcceptanceMetrics{
		OwnerShareMatt:        beadsapi.AcceptanceOwnerShare{Total: 100, Matt: 8, Fraction: 0.08},
		AgentOperatorLeaks:    beadsapi.AcceptanceOperatorLeaks{Count: 0},
		FleetFallbackCount:    beadsapi.AcceptanceFleetFallback{Count: 0},
		OperatorAliasCoverage: beadsapi.AcceptanceAliasCoverage{Total: 5, Matched: 5, Fraction: 1.0},
	}
	bad := &beadsapi.AcceptanceMetrics{
		OwnerShareMatt:        beadsapi.AcceptanceOwnerShare{Total: 100, Matt: 55, Fraction: 0.55},
		AgentOperatorLeaks:    beadsapi.AcceptanceOperatorLeaks{Count: 3},
		FleetFallbackCount:    beadsapi.AcceptanceFleetFallback{Count: 17},
		OperatorAliasCoverage: beadsapi.AcceptanceAliasCoverage{Total: 5, Matched: 2, Fraction: 0.40},
	}

	cases := []struct {
		name string
		m    *beadsapi.AcceptanceMetrics
		leaf Leaf
		want bool
	}{
		{"owner share <10 met when 8%", good, Leaf{Type: "acceptance_metric", Field: "owner_share_matt.fraction_pct", Op: "<", Value: "10"}, true},
		{"owner share <10 NOT met when 55%", bad, Leaf{Type: "acceptance_metric", Field: "owner_share_matt.fraction_pct", Op: "<", Value: "10"}, false},
		{"leaks == 0 met when zero", good, Leaf{Type: "acceptance_metric", Field: "agent_operator_leaks.count", Op: "==", Value: "0"}, true},
		{"leaks == 0 NOT met when 3", bad, Leaf{Type: "acceptance_metric", Field: "agent_operator_leaks.count", Op: "==", Value: "0"}, false},
		{"fleet fallback == 0 met when zero", good, Leaf{Type: "acceptance_metric", Field: "fleet_fallback_count.count", Op: "==", Value: "0"}, true},
		{"fleet fallback == 0 NOT met when 17", bad, Leaf{Type: "acceptance_metric", Field: "fleet_fallback_count.count", Op: "==", Value: "0"}, false},
		{"fleet_fallback_count shorthand alias", good, Leaf{Type: "acceptance_metric", Field: "fleet_fallback_count", Op: "==", Value: "0"}, true},
		{"alias coverage >=100 met at 100", good, Leaf{Type: "acceptance_metric", Field: "operator_alias_coverage.fraction_pct", Op: ">=", Value: "100"}, true},
		{"alias coverage >=100 NOT met at 40", bad, Leaf{Type: "acceptance_metric", Field: "operator_alias_coverage.fraction_pct", Op: ">=", Value: "100"}, false},
		{"agent_operator_leaks shorthand alias", good, Leaf{Type: "acceptance_metric", Field: "agent_operator_leaks", Op: "==", Value: "0"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := &stubAcceptanceClient{metrics: tc.m}
			got, err := Evaluate(context.Background(), Expression{Leaf: tc.leaf}, Env{Bead: bc})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestEvaluate_AcceptanceMetricLeaf_UnknownField(t *testing.T) {
	bc := &stubAcceptanceClient{metrics: &beadsapi.AcceptanceMetrics{}}
	leaf := Leaf{Type: "acceptance_metric", Field: "bogus", Op: "==", Value: "0"}
	_, err := Evaluate(context.Background(), Expression{Leaf: leaf}, Env{Bead: bc})
	if err == nil {
		t.Fatalf("expected error for unknown field")
	}
}

func TestEvaluate_AcceptanceMetricLeaf_ClientDoesNotSupport(t *testing.T) {
	// A plain BeadClient without GetAcceptanceMetrics must produce an error
	// rather than a silent false.
	bc := &stubBeadClient{fn: count(0)}
	leaf := Leaf{Type: "acceptance_metric", Field: "agent_operator_leaks.count", Op: "==", Value: "0"}
	_, err := Evaluate(context.Background(), Expression{Leaf: leaf}, Env{Bead: bc})
	if err == nil {
		t.Fatalf("expected error when client lacks AcceptanceMetricsClient")
	}
}

func TestEvaluate_AcceptanceMetricLeaf_TransportError(t *testing.T) {
	bc := &stubAcceptanceClient{err: errors.New("boom")}
	leaf := Leaf{Type: "acceptance_metric", Field: "agent_operator_leaks.count", Op: "==", Value: "0"}
	_, err := Evaluate(context.Background(), Expression{Leaf: leaf}, Env{Bead: bc})
	if err == nil {
		t.Fatalf("expected transport error to surface")
	}
}
