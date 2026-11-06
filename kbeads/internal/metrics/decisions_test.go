package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNormalizeUrgency(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"low", UrgencyLow},
		{"LOW", UrgencyLow},
		{"  Medium ", UrgencyMedium},
		{"high", UrgencyHigh},
		{"critical", UrgencyCritical},
		{"", UrgencyUnknown},
		{"banana", UrgencyUnknown},
		{"p0", UrgencyUnknown},
	}
	for _, tt := range tests {
		if got := NormalizeUrgency(tt.in); got != tt.want {
			t.Errorf("NormalizeUrgency(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeRole(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"crew", RoleCrew},
		{"CREW", RoleCrew},
		{"role:gardener", RoleGardener},
		{"role:crew", RoleCrew},
		{"thread", RoleThread},
		{"operator", RoleOperator},
		{"concierge", RoleConcierge},
		{"jira-mention", RoleJiraMention},
		{"triage", RoleTriage},
		{"", RoleOther},
		{"superuser", RoleOther},
	}
	for _, tt := range tests {
		if got := NormalizeRole(tt.in); got != tt.want {
			t.Errorf("NormalizeRole(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeOutcome(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", OutcomeRequested}, // empty == escalation raised, not yet resolved
		{"requested", OutcomeRequested},
		{"APPROVED", OutcomeApproved},
		{"rejected", OutcomeRejected},
		{"expired", OutcomeExpired},
		{"weird", OutcomeOther},
	}
	for _, tt := range tests {
		if got := NormalizeOutcome(tt.in); got != tt.want {
			t.Errorf("NormalizeOutcome(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestObserveDecisionLifecycle(t *testing.T) {
	DecisionsLifecycle.Reset()

	ObserveDecisionLifecycle(DecisionEventCreated, "high")
	ObserveDecisionLifecycle(DecisionEventCreated, "high")
	ObserveDecisionLifecycle(DecisionEventResponded, "")          // → unknown
	ObserveDecisionLifecycle(DecisionEventExpired, "not-a-level") // → unknown
	ObserveDecisionLifecycle("", "high")                          // no-op (empty event label)

	if n := testutil.ToFloat64(DecisionsLifecycle.WithLabelValues(DecisionEventCreated, UrgencyHigh)); n != 2 {
		t.Errorf("created/high = %v, want 2", n)
	}
	if n := testutil.ToFloat64(DecisionsLifecycle.WithLabelValues(DecisionEventResponded, UrgencyUnknown)); n != 1 {
		t.Errorf("responded/unknown = %v, want 1", n)
	}
	if n := testutil.ToFloat64(DecisionsLifecycle.WithLabelValues(DecisionEventExpired, UrgencyUnknown)); n != 1 {
		t.Errorf("expired/unknown = %v, want 1", n)
	}
	if total := testutil.CollectAndCount(DecisionsLifecycle); total != 3 {
		t.Errorf("expected 3 distinct label series (empty-event no-op), got %d", total)
	}
}

func TestObserveEscalation(t *testing.T) {
	Escalations.Reset()

	ObserveEscalation("crew", "") // → crew/requested
	ObserveEscalation("role:gardener", "approved")
	ObserveEscalation("ghost", "rejected") // → other/rejected

	if n := testutil.ToFloat64(Escalations.WithLabelValues(RoleCrew, OutcomeRequested)); n != 1 {
		t.Errorf("crew/requested = %v, want 1", n)
	}
	if n := testutil.ToFloat64(Escalations.WithLabelValues(RoleGardener, OutcomeApproved)); n != 1 {
		t.Errorf("gardener/approved = %v, want 1", n)
	}
	if n := testutil.ToFloat64(Escalations.WithLabelValues(RoleOther, OutcomeRejected)); n != 1 {
		t.Errorf("other/rejected = %v, want 1", n)
	}
}

func TestRegisterDecisions(t *testing.T) {
	reg := prometheus.NewRegistry()
	// MustRegister panics on failure; a clean registry must accept both.
	RegisterDecisions(reg)
}
