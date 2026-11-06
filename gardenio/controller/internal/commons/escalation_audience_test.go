package commons

import (
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/commons/commonstest"
)

// TestEscalationCardAsker: the header names the agent that asked, never its
// agent bead's id, which read as the escalation's own (gd-uHO0OUzcl5).
func TestEscalationCardAsker(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)

	// requesting_agent is the agent bead's id: its name shows.
	_, body := get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, `<span class="cm-ecard-asker">claimlayer-crew-1</span>`)
	mustNotContain(t, body, `cm-ecard-asker">cl-agent1<`)

	// An agent bead id that names no agent of the land: no asker at all.
	_, body = get(t, h, "/test/m/cl-ecard5", "claimlayer")
	mustNotContain(t, body, "cm-ecard-asker", "gd-Pg1xaiun2c")

	// Already a name: shown as it is.
	_, body = get(t, h, "/test/m/cl-ecard2", "claimlayer")
	mustContain(t, body, `<span class="cm-ecard-asker">claimlayer-crew-1</span>`)
}

// TestInHandEscalationsAreMarked: owner-audience escalation cards show as
// they are; a desk or operator one is marked in hand, with its audience
// (gd-bmOiteQi36).
func TestInHandEscalationsAreMarked(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	inHand := func(want ...string) {
		t.Helper()
		for _, id := range escalationCards {
			_, body := get(t, h, "/test/m/"+id, "claimlayer")
			if got := strings.Contains(body, "cm-msg--inhand"); got != slices.Contains(want, id) {
				t.Errorf("%s in hand = %v, want %v:\n%s", id, got, !got, body)
			}
		}
	}
	// credentials, secret_rotate (the D3 floor), secret: the owner's
	inHand("cl-ecard4", "cl-ecard5")
	_, body := get(t, h, "/test/m/cl-ecard4", "claimlayer")
	mustContain(t, body, `cm-msg--inhand" id="m-cl-ecard4" data-audience="operator"`)

	// Re-routing to the owner takes it out of hand; the forced-owner rule
	// cannot be routed away.
	d.Beads["cl-esc5"].Fields["audience"] = "owner"
	d.Beads["cl-esc3"].Fields["audience"] = "operator"
	inHand("cl-ecard4")
}
