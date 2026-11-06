package bridge

import (
	"context"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The cross-land negative matrix, push half (Lands Track B step 10.6b,
// gd-cIux5tj6WI; spec §5.6: push alerts carry nothing of B's to A). Land A is
// claimlayer, land B is bravo, and each has one person with a phone. Every way
// a decision or escalation of B's can be addressed (its own operated_by field,
// the daemon's row, the requesting agent by bead id or by name) alerts B's
// person's phone and never A's; one of B's beads that names no one alerts no
// one. The viewer half is beads-viewer TestViewerCrossLandMatrix.
func TestCrossLandMatrix_PushAlerts(t *testing.T) {
	const (
		aPhone, bPhone = "claimlayer-phone", "bravo-phone"
		bHuman         = "oidc:bv@bravo.example"
	)
	newDaemon := func() *fakePushDaemon {
		return &fakePushDaemon{
			devices: []*beadsapi.Device{
				{Token: aPhone, Platform: "ios", UserEmail: "a@claimlayer.example"},
				{Token: bPhone, Platform: "ios", UserEmail: "bv@bravo.example"},
			},
			beads: map[string]*beadsapi.BeadDetail{
				"bv-agent": {ID: "bv-agent", Type: "agent", Labels: []string{"project:bravo"}, OperatedBy: bHuman},
				"bv-row":   {ID: "bv-row", Type: "escalation", Labels: []string{"project:bravo"}, OperatedBy: bHuman},
				"bv-none":  {ID: "bv-none", Type: "decision", Labels: []string{"project:bravo"}},
			},
			agents: map[string]*beadsapi.BeadDetail{
				"bravo-crew-1": {ID: "bv-agent", Type: "agent", Labels: []string{"project:bravo"}, Fields: map[string]string{"operated_by": bHuman}},
			},
		}
	}
	label := []string{"project:bravo"}
	cases := []struct {
		name       string
		escalation bool
		bead       BeadEvent
		want       []string
	}{
		{"decision, its own operated_by", false, BeadEvent{ID: "bv-dec", Type: "decision", Labels: label,
			Fields: map[string]string{"operated_by": bHuman}}, []string{bPhone}},
		{"escalation, the daemon's row", true, BeadEvent{ID: "bv-row", Type: "escalation", Labels: label}, []string{bPhone}},
		{"decision, the requesting agent by bead id", false, BeadEvent{ID: "bv-dec2", Type: "decision", Labels: label,
			Fields: map[string]string{"requesting_agent_bead_id": "bv-agent"}}, []string{bPhone}},
		{"decision, the requesting agent by name", false, BeadEvent{ID: "bv-dec3", Type: "decision", Labels: label,
			Assignee: "bravo-crew-1"}, []string{bPhone}},
		{"escalation, the requesting agent", true, BeadEvent{ID: "bv-esc", Type: "escalation", Labels: label,
			Fields: map[string]string{"requesting_agent": "bv-agent"}}, []string{bPhone}},
		{"a bead that names no one", false, BeadEvent{ID: "bv-none", Type: "decision", Labels: label}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, a := newDaemon(), &fakeAPNs{}
			p := newTestPushNotifier(d, a)
			notify := p.NotifyDecision
			if c.escalation {
				notify = p.NotifyEscalation
			}
			if err := notify(context.Background(), c.bead); err != nil {
				t.Fatalf("notify: %v", err)
			}
			got := a.pushed()
			if slices.Contains(got, aPhone) {
				t.Errorf("B's %s alerted A's phone: pushed %v", c.bead.ID, got)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("pushed %v, want %v", got, c.want)
			}
			for _, listed := range d.listed {
				if slices.Contains(listed, "a@claimlayer.example") {
					t.Errorf("A's person was asked for as a recipient of B's %s: %v", c.bead.ID, d.listed)
				}
			}
		})
	}
}
