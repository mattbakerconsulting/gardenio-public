package commons

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// allowEsc1 is a can-approve that lets the viewer approve cl-esc1 only.
func allowEsc1(h *Handler) {
	h.Approvals = fakeApprovals{ans: map[string]beadsapi.CanApproveResult{
		"cl-esc1": {Allowed: true, Method: "web_ui", As: "operator"},
	}}
}

func TestEscalationCardFaces(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, allowEsc1)

	// Waiting: what is asked, Approve and Reject on the card's action route
	// (the daemon says this viewer may approve it), and Review on the
	// escalation's page.
	_, body := get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, `class="cm-ecard cm-ecard--waiting"`, `data-urgency="high"`, ">P0<", "Escalation needed",
		`<code class="cm-ecard-op">gitlab_mr_merge</code>`, "runs locally", "Needs <code>github_token</code>, <code>kube-prod</code>",
		`href="https://github.com/acme/app/pull/42"`, `href="/base/garden?bead=cl-AbCdEfGhIj"`, "Runs an attached script",
		"Pinged 2 times: still blocks the release",
		`action="/base/garden/c/action/cl-ecard1"`, `name="action" value="approve"`, `name="action" value="reject"`,
		`href="/base/cl-esc1">Review</a>`)
	mustNotContain(t, body, `<p class="cm-text">Escalation request</p>`) // the fallback text gives way to the face

	// Approved: who approved it, no buttons.
	_, body = get(t, h, "/test/m/cl-ecard2", "claimlayer")
	mustContain(t, body, "cm-ecard--approved", "Approved by ann · running")
	mustNotContain(t, body, `value="approve"`)

	// Done and secret: the redacted summary only, never the result.
	_, body = get(t, h, "/test/m/cl-ecard3", "claimlayer")
	mustContain(t, body, "cm-ecard--done", "Done · approved by ann · run by claimlayer-infra-1",
		"3 logins from new addresses", "The full result is secret")
	mustNotContain(t, body, "withheld", `value="approve"`)

	// Rejected, with the reason.
	_, body = get(t, h, "/test/m/cl-ecard4", "claimlayer")
	mustContain(t, body, "cm-ecard--rejected", "Rejected by ann", "Too broad: ask for the one namespace.")

	// Closed with no approval and no completed result is a rejection (the
	// bridge's rule), and a failed run after approval is a failure.
	d.Beads["cl-esc1"].Status = "closed"
	_, body = get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, "cm-ecard--rejected", ">Rejected</p>")
	d.Beads["cl-esc1"].Fields["approved_by"], d.Beads["cl-esc1"].Fields["result_status"] = "gd-user-1", "failure"
	_, body = get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, "cm-ecard--done", "Failed · approved by gd-user-1")
	mustNotContain(t, body, "Pinged") // a closed escalation is no longer waiting on anyone
}

func TestEscalationCardCountsCredentialsPastFour(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	d.Beads["cl-esc1"].Fields["credential_requirements"] = `[{"type":"a"},{"type":"b"},{"type":"c"},{"type":"d"},{"type":"e"},{"type":"f"}]`
	_, body := get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, "<code>d</code> and 2 more")
	mustNotContain(t, body, "<code>e</code>")
}

func TestEscalationCardSubjectStaysInItsLand(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	// The card names an escalation of another land: no face.
	d.Put(&beadsapi.BeadDetail{ID: "cl-esc1", Type: "escalation", Status: "open", Labels: []string{"project:gardenio"},
		Title: "Gardenio secret escalation", Fields: map[string]string{"operation_type": "secret_rotate"}})
	_, body := get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, `href="/base/garden?bead=cl-esc1" data-bead="cl-esc1" title="cl-esc1: not found" data-titles="off">Bead not found <span class="cm-bead-id">cl-esc1</span></a>`)
	mustNotContain(t, body, "cm-ecard", "Gardenio secret escalation")
}

func TestEscalationMR(t *testing.T) {
	for spec, want := range map[string]string{
		`{"mr_url":"https://gitlab.com/acme/app/-/merge_requests/7"}`: "https://gitlab.com/acme/app/-/merge_requests/7",
		`{"mr_url":"javascript:alert(1)"}`:                            "",
		`{"mr_url":"/relative/path"}`:                                 "",
		`{"project":"acme/app"}`:                                      "",
		`not json`:                                                    "",
	} {
		if got := escalationMR(spec); got != want {
			t.Errorf("escalationMR(%s) = %q, want %q", spec, got, want)
		}
	}
}

// escalationCards are the cards of the fixture's #escalations, in order.
var escalationCards = []string{"cl-ecard1", "cl-ecard2", "cl-ecard3", "cl-ecard4", "cl-ecard5"}

// cardIn is the message cardID's escalation card in page (cards): from the message's
// id to the card's end.
func cardIn(t *testing.T, page, cardID string) string {
	t.Helper()
	i := strings.Index(page, `id="m-`+cardID+`"`)
	if i < 0 {
		t.Fatalf("no %s on the page:\n%s", cardID, page)
	}
	j := strings.Index(page[i:], "</article>")
	if j < 0 {
		t.Fatalf("%s has no card face:\n%s", cardID, page[i:])
	}
	return page[i : i+j]
}

// TestEscalationCard_WaitingOnNamesTheLane: an open escalation of the owner's
// says whose move it is in plain words, and an in-hand one's chip names the
// holding lane rather than its audience (escalation routing D1).
func TestEscalationCard_WaitingOnNamesTheLane(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	_, body := cards(t, h, "claimlayer", escalationCards...)
	mustContain(t, cardIn(t, body, "cl-ecard1"), `<p class="cm-waiting">Waiting on the owner</p>`)
	esc5 := cardIn(t, body, "cl-ecard5")
	mustContain(t, esc5, `title="Who has it">coordinator merge-desk</span>`)
	mustNotContain(t, esc5, "Waiting on") // the chip already says who has it
	// Approved and running: no longer waiting on anyone, whatever held_by says.
	mustNotContain(t, cardIn(t, body, "cl-ecard2"), "Waiting on")
}

// TestEscalationCard_UnstampedKeepsAudience: an escalation filed before
// routing (no held_by) draws its chip from the audience, as #756 did, and
// never waits on an empty lane (Global Constraint 7): an open one of the
// owner's waits on the owner by its audience.
func TestEscalationCard_UnstampedKeepsAudience(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	delete(d.Beads["cl-esc5"].Fields, "held_by")
	delete(d.Beads["cl-esc1"].Fields, "held_by")
	_, body := cards(t, h, "claimlayer", escalationCards...)
	esc5 := cardIn(t, body, "cl-ecard5")
	mustContain(t, esc5, `title="Who has it">desk</span>`)
	mustNotContain(t, esc5, "Waiting on")
	mustContain(t, cardIn(t, body, "cl-ecard1"), `<p class="cm-waiting">Waiting on the owner</p>`)
}

// TestEscalationCard_WaitingOnYou: an open escalation the viewer may approve
// waits on them, in the card and in the "In hand" fold alike; one they may
// not names its lane or, unstamped, its audience (rubric C9).
func TestEscalationCard_WaitingOnYou(t *testing.T) {
	d := commonstest.TwoLands(t0)
	yes := beadsapi.CanApproveResult{Allowed: true}
	h := commonsServer(t, d, func(h *Handler) {
		h.Approvals = fakeApprovals{ans: map[string]beadsapi.CanApproveResult{"cl-esc1": yes, "cl-esc5": yes}}
	})
	_, body := cards(t, h, "claimlayer", escalationCards...)
	for _, card := range []string{"cl-ecard1", "cl-ecard5"} {
		mustContain(t, cardIn(t, body, card), `<p class="cm-waiting cm-waiting--you">Waiting on you</p>`)
	}
	for _, c := range []struct {
		card EscalationCard
		want string
	}{
		{EscalationCard{State: "waiting", Audience: beadsapi.AudienceOperator}, "an operator"},
		{EscalationCard{State: "waiting", Audience: beadsapi.AudienceDesk}, "the merge desk"},
		{EscalationCard{State: "waiting", Audience: beadsapi.AudienceOwner}, "the owner"},
		{EscalationCard{State: "waiting", Audience: beadsapi.AudienceDesk, WaitingOn: "coordinator merge-desk"}, "coordinator merge-desk"},
		{EscalationCard{State: "approved", CanApprove: true}, ""},
	} {
		if got := c.card.waiting(); got != c.want {
			t.Errorf("%+v waits on %q; want %q", c.card, got, c.want)
		}
	}
}

// TestEscalationCard_ButtonsOnlyWhenDaemonAllows: Approve and Reject render
// only where the daemon's can-approve says yes (D9); Review always does.
func TestEscalationCard_ButtonsOnlyWhenDaemonAllows(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, allowEsc1)
	_, body := cards(t, h, "claimlayer", escalationCards...)
	mustContain(t, cardIn(t, body, "cl-ecard1"), `name="action" value="approve"`, `name="action" value="reject"`,
		`href="/base/cl-esc1">Review</a>`)
	esc5 := cardIn(t, body, "cl-ecard5") // the fake says no
	mustNotContain(t, esc5, `value="approve"`, `value="reject"`, "Reject…")
	mustContain(t, esc5, `href="/base/cl-esc5">Review</a>`)
}

// TestEscalationCard_NoApprovalsMeansNoButtons: a handler with no can-approve
// shows no Approve or Reject anywhere (fail closed).
func TestEscalationCard_NoApprovalsMeansNoButtons(t *testing.T) {
	d := commonstest.TwoLands(t0)
	_, body := cards(t, commonsServer(t, d), "claimlayer", escalationCards...)
	mustNotContain(t, body, `value="approve"`, `value="reject"`)
	mustContain(t, cardIn(t, body, "cl-ecard1"), `href="/base/cl-esc1">Review</a>`)

	// Control: the same page with a can-approve that allows cl-esc1.
	_, body = cards(t, commonsServer(t, d, allowEsc1), "claimlayer", escalationCards...)
	mustContain(t, cardIn(t, body, "cl-ecard1"), `value="approve"`)
}

// TestEscalationCard_CanApproveErrorHidesButtons: a can-approve that fails
// hides the buttons and the page still answers (fail closed, logged).
func TestEscalationCard_CanApproveErrorHidesButtons(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, func(h *Handler) {
		h.Approvals = fakeApprovals{
			ans: map[string]beadsapi.CanApproveResult{"cl-esc1": {Allowed: true}},
			err: map[string]error{"cl-esc1": errors.New("daemon unreachable")},
		}
	})
	code, body := cards(t, h, "claimlayer", escalationCards...)
	if code != 200 {
		t.Fatalf("status %d, want 200", code)
	}
	mustNotContain(t, cardIn(t, body, "cl-ecard1"), `value="approve"`, `value="reject"`)
}

// TestEscalationActionsFor: closed cards and a nil can-approve never ask.
func TestEscalationActionsFor(t *testing.T) {
	ctx := context.Background()
	yes := fakeApprovals{ans: map[string]beadsapi.CanApproveResult{"e": {Allowed: true}}}
	if !EscalationActionsFor(ctx, yes, &EscalationCard{ID: "e", State: "waiting"}) {
		t.Error("an allowed waiting escalation should offer the buttons")
	}
	for name, c := range map[string]*EscalationCard{"approved": {ID: "e", State: "approved"}, "rejected": {ID: "e", State: "rejected"}, "nil card": nil} {
		if EscalationActionsFor(ctx, yes, c) {
			t.Errorf("%s: offered buttons", name)
		}
	}
	if EscalationActionsFor(ctx, nil, &EscalationCard{ID: "e", State: "waiting"}) {
		t.Error("a nil Approvals offered buttons")
	}
}
