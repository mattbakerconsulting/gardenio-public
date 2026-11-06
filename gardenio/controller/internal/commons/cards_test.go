package commons

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

func TestAgentCardFaces(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)

	// The live face leads with the task and carries the stream line.
	_, body := get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `class="cm-acard cm-acard--live"`, `data-state="working"`,
		`<p class="cm-acard-task" title="Claimlayer member task">Claimlayer member task</p>`, "Working · 3m ago", "$1.23", "123k context", "42 turns", "for ann")
	mustNotContain(t, body, "crew agent") // the card's fallback text gives way to the face

	// The wrap-up face once the agent has stopped.
	_, body = get(t, h, "/test/m/cl-card2", "claimlayer")
	// It reads as a document (step 5, gd-Z05Hye7yP4): "a; b" accomplishments are bullets, the handoff is its next.
	mustContain(t, body, `class="cm-acard cm-acard--wrapup"`, `data-state="done"`, `<span class="cm-acard-tag">Wrap-up</span>`,
		`<h3 class="cm-acard-task">Chart lints again</h3>`, `<div class="cm-acard-done md"><ul>
<li>Fixed the values schema</li>
<li>PR #42 is green</li>
</ul>`, `<div class="cm-acard-next md"><p>Release when convenient</p>`, `href="/base/garden/c/n/cl-agent2">Jobs</a>`)
	mustNotContain(t, body, "cm-acard-stream", "cm-acard-blocked", "<dl", "Kill")

	// A failed agent with no wrap-up: its close reason is the tldr.
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent2", Type: "agent", Title: "claimlayer-crew-2", Status: "closed",
		Labels: []string{"project:claimlayer"}, Fields: map[string]string{"agent": "claimlayer-crew-2", "agent_state": "failed", "close_reason": "pod evicted"}})
	_, body = get(t, h, "/test/m/cl-card2", "claimlayer")
	mustContain(t, body, `data-state="failed"`, string(stateMarks["failed"]), "pod evicted")
}

func TestAgentCardSubjectStaysInItsLand(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	// The card names an agent of another land: no face, only the link, and
	// nothing of the foreign agent. A card is never prose, so its words
	// ("crew agent") are not drawn either (gd-d3irroS7Q1).
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent1", Type: "agent", Title: "gardenio-secret-agent", Status: "open",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent": "gardenio-secret-agent", "agent_state": "working"}})
	_, body := get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `href="/base/garden?bead=cl-agent1" data-bead="cl-agent1" title="cl-agent1: not found" data-titles="off">Bead not found <span class="cm-bead-id">cl-agent1</span></a>`)
	mustNotContain(t, body, "cm-acard", "gardenio-secret-agent", `class="cm-text"`, "crew agent")

	// A subject that is not an agent, or that is missing, is a link too.
	delete(d.Beads, "cl-agent1")
	if code, body := get(t, h, "/test/m/cl-card1", "claimlayer"); code != http.StatusOK || strings.Contains(body, "cm-acard") ||
		!strings.Contains(body, `title="cl-agent1: not found" data-titles="off">Bead not found`) {
		t.Fatalf("missing subject: %d %s", code, body)
	}
}

func TestAgo(t *testing.T) {
	now := t0
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 3 * time.Minute: "3m ago", 5 * time.Hour: "5h ago", 72 * time.Hour: "3d ago",
	} {
		if got := ago(now.Add(-d), now); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
	if ago(time.Time{}, now) != "" {
		t.Error("ago of the zero time")
	}
	if tokens(812) != "812" || tokens(12_345) != "12k" || tokens(1_234_567) != "1.2M" || dollars("0.001") != "<$0.01" || dollars("") != "" {
		t.Error("a fact formatter drifted")
	}
}

func TestClassifySubject(t *testing.T) {
	d := commonstest.TwoLands(t0)
	inv, ok := classify(commonstest.Payload(d.Beads["cl-agent1"]))
	if !ok || inv.Kind != kindSubject || inv.ID != "cl-agent1" {
		t.Fatalf("classify(agent) = %+v %v", inv, ok)
	}
	if inv, _ := classify(commonstest.Payload(d.Beads["cl-card1"])); inv.subject != "cl-agent1" {
		t.Fatalf("a card's invalidation lost its subject: %+v", inv)
	}
}

// TestHubSubjectRefreshesItsCards: an agent's change re-renders its card, for
// the subscribers of the card's land only.
func TestHubSubjectRefreshesItsCards(t *testing.T) {
	d := commonstest.TwoLands(t0)
	src := make(liveSource, 16)
	hub := NewHub(src.open, d, nil)
	hub.Start()
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scoped, _ := hub.Subscribe(ctx, claimlayerScope)
	other, _ := hub.Subscribe(ctx, Scope{Lands: []string{"gardenio"}})

	src <- beadsapi.SSEEvent{Event: "beads.bead.updated", Data: commonstest.Payload(d.Beads["cl-agent1"])}
	select {
	case inv := <-scoped:
		if inv.Kind != "card" || inv.ID != "cl-card1" || inv.Channel != "cl-chgen" || inv.Land != "claimlayer" {
			t.Fatalf("got %+v, want card cl-card1 in cl-chgen", inv)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no card invalidation for the subject's change")
	}
	select {
	case inv := <-other:
		t.Fatalf("another land got %+v", inv)
	case <-time.After(50 * time.Millisecond):
	}

	// A new card of the subject is found on its next change, memo or not.
	d.Put(&beadsapi.BeadDetail{ID: "cl-card3", Type: "message", Status: "closed", Labels: []string{"project:claimlayer"},
		Fields: map[string]string{"channel_id": "cl-chcrew", "kind": "card", "subject_id": "cl-agent1"}})
	src <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: commonstest.Payload(d.Beads["cl-card3"])}
	src <- beadsapi.SSEEvent{Event: "beads.bead.updated", Data: commonstest.Payload(d.Beads["cl-agent1"])}
	var got []string
	for len(got) < 3 {
		select {
		case inv := <-scoped:
			got = append(got, inv.ID)
		case <-time.After(2 * time.Second):
			t.Fatalf("got %v, want cl-card3's own event, then both cards of the subject", got)
		}
	}
	if got[0] != "cl-card3" || !(got[1] == "cl-card1" && got[2] == "cl-card3" || got[1] == "cl-card3" && got[2] == "cl-card1") {
		t.Fatalf("got %v, want cl-card3, then cl-card1 and cl-card3", got)
	}
}
