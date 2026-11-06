package commons

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// squawkBead is a squawk as gb squawk writes it for an agent with no Slack
// thread (cmd/gb/channel_stamp.go agentCardPlace): no author field, created by
// the agent's bare name, a reply in its agent card's thread.
func squawkBead(id, card, text string, at time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "message", Title: text, Status: "closed", CreatedAt: at, CreatedBy: "claimlayer-crew-1",
		Labels: []string{"squawk", "from:claimlayer-crew-1", "project:claimlayer"},
		Fields: map[string]string{"source_agent": "claimlayer-crew-1", "text": text, "thread_id": card, "channel_id": "cl-chgen"},
	}
}

// An agent's squawks show in its card's thread, oldest first, each under the
// agent's name, as Slack shows them as replies to the card (gd-fIM0zNikOd). The
// card counts them in the timeline.
func TestSquawksInCardThread(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(squawkBead("cl-sq1", "cl-card1", "claimed the task, reading the squawk path", t0.Add(20*time.Minute)))
	d.Put(squawkBead("cl-sq2", "cl-card1", "PR is up: https://github.com/acme/app/pull/43", t0.Add(25*time.Minute)))
	h := commonsServer(t, d)

	code, body := get(t, h, "/garden/c/claimlayer/general/t/cl-card1", "claimlayer")
	if code != 200 {
		t.Fatalf("GET card thread = %d: %s", code, body)
	}
	mustContain(t, body, `id="m-cl-card1"`, `id="m-cl-sq1"`, `id="m-cl-sq2"`,
		`data-voice="agent" data-via="squawk">`+"\n"+`<p class="cm-meta"><span class="cm-author cm-author--agent" title="agent:claimlayer-crew-1">claimlayer-crew-1</span> <span class="cm-via cm-via--squawk"`,
		"claimed the task, reading the squawk path", `href="https://github.com/acme/app/pull/43"`)
	mustNotContain(t, body, commonstest.Leaks...)
	if a, b := strings.Index(body, "m-cl-sq1"), strings.Index(body, "m-cl-sq2"); a < 0 || b < a {
		t.Errorf("squawks not oldest first")
	}
	file := filepath.Join("testdata", "squawks.golden.html")
	if *update {
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	} else if want, err := os.ReadFile(file); err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	} else if body != string(want) {
		t.Errorf("card thread drifted from %s (run with -update if meant):\n%s", file, body)
	}

	_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `href="/base/garden/c/claimlayer/cl-chgen/t/cl-card1" class="cm-replies">2 replies</a>`)
	mustNotContain(t, body, `id="m-cl-sq1"`)

	// The event stream names a squawk by id; its refetch is the reply row.
	_, body = get(t, h, "/test/m/cl-sq2", "claimlayer")
	mustContain(t, body, `id="m-cl-sq2"`, `data-thread="cl-card1"`, "cm-msg--reply")
	if inv, ok := classify(commonstest.Payload(d.Beads["cl-sq2"])); !ok || inv.Kind != "message" || inv.Thread != "cl-card1" || inv.Channel != "cl-chgen" {
		t.Errorf("classify squawk = %+v, %v; want a message in cl-card1's thread", inv, ok)
	}
}

// The fixture wish's thread (commonstest.WishFamily) speaks in each writer's
// voice (kin.Speaker, gd-d3irroS7Q1): the head and the two Slack replies the
// bridge wrote are the person's, the three squawks the crew's, each with the
// squawk mark, and the card is the agent's face, never its "Agent: ..." words.
// A card with no face is its subject's chip alone; the steward's transcript
// draws the card as a state line, its agent linked and its state word.
func TestWishThreadVoices(t *testing.T) {
	d := commonstest.TwoLands(t0)
	const head = "cl-wishV"
	f := commonstest.WishFamily(head, "claimlayer", "cl-chgen", t0)
	f.PutIn(d)
	h := commonsServer(t, d)
	code, body := get(t, h, "/garden/c/claimlayer/general/t/"+head, "claimlayer")
	if code != 200 {
		t.Fatalf("GET wish thread = %d: %s", code, body)
	}
	agent := "claimlayer-one-bead-view-k2p"
	person := `<span class="cm-author cm-author--human" title="oidc:matt@example.com">matt</span>`
	squawk := `data-voice="agent" data-via="squawk">` + "\n" + `<p class="cm-meta"><span class="cm-author cm-author--agent" title="agent:` + agent + `">` + agent +
		`</span> <span class="cm-via cm-via--squawk" title="A squawk: the agent's own report">squawk</span>`
	for _, id := range []string{head, head + commonstest.WishReply1, head + commonstest.WishReply2} {
		i := strings.Index(body, `id="m-`+id+`"`)
		if i < 0 || !strings.HasPrefix(body[strings.Index(body[i:], "<p class=\"cm-meta\">")+i:], `<p class="cm-meta">`+person) {
			t.Errorf("%s is not the person's:\n%s", id, body)
		}
	}
	if n := strings.Count(body, squawk); n != 3 {
		t.Errorf("%d squawks under %s with the squawk mark, want 3:\n%s", n, agent, body)
	}
	card := body[strings.Index(body, `id="m-`+head+commonstest.WishCard+`"`):]
	card = card[:strings.Index(card, "</li>")]
	mustContain(t, card, `data-via="card"`, `title="agent:`+agent+`">`+agent+`</span>`, `<article class="cm-acard`)
	mustNotContain(t, card, "Agent: ", `class="cm-text"`, "cm-via--squawk")
	mustNotContain(t, body, "slack-bridge")

	// Its agent gone, the card has no face: the chip alone.
	delete(d.Beads, head+commonstest.WishAgent)
	_, body = get(t, h, "/test/m/"+head+commonstest.WishCard, "claimlayer")
	mustContain(t, body, `<p class="cm-card"><a class="cm-card-subject cm-bead`)
	mustNotContain(t, body, "Agent: ", `class="cm-text"`, "cm-acard")
}
