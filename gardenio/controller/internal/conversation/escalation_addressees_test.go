package conversation

import (
	"slices"
	"testing"
)

func TestEscalationAddressees(t *testing.T) {
	cases := []struct {
		held, land string
		want       []EscalationAddressee
	}{
		// Unstamped: the caller already applied #756's audience gate, so an
		// unstamped owner-audience escalation is the operator's (Global
		// Constraint 7).
		{"", "", []EscalationAddressee{{Lane: "operator"}}},
		{"operator", "gardenio", []EscalationAddressee{{Lane: "operator"}}},
		{"land-owner:acme", "acme", []EscalationAddressee{{Lane: "land-owner:acme", Land: "acme"}}},
		// Legacy entries never hold an escalation (Task 2 validation).
		{"oidc:a@b.example", "", nil},
		// Agents and machines are nobody's personal ping.
		{"coordinator:laptop", "gardenio", nil},
		{"steward:acme", "acme", nil},
		{"requester", "", nil},
		{"server", "", nil},
		// A land-owner lane for another land than the escalation's own is a
		// stale or forged stamp: address nobody rather than the wrong owners.
		{"land-owner:acme", "beta", nil},
	}
	for _, c := range cases {
		if got := EscalationAddressees(c.held, c.land); !slices.Equal(got, c.want) {
			t.Errorf("EscalationAddressees(%q, %q) = %+v, want %+v", c.held, c.land, got, c.want)
		}
	}
}
