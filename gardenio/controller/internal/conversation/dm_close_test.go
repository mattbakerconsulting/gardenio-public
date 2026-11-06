package conversation_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// dmWorld is a land with one DM between agent crew-1 and Ann.
type dmWorld struct {
	store *fakeStore
	dm    string
	agent *beadsapi.BeadDetail
	at    time.Time
}

func newDMWorld(t *testing.T) *dmWorld {
	t.Helper()
	w := &dmWorld{store: newFakeStore(), at: time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)}
	ch, err := conversation.EnsureDM(context.Background(), w.store, "gardenio", "crew-1, Ann", "agent:crew-1", "oidc:ann@example.com")
	if err != nil {
		t.Fatalf("EnsureDM: %v", err)
	}
	w.dm = ch.ID
	w.agent = w.put(&beadsapi.BeadDetail{Type: "agent", Status: "closed", Fields: map[string]string{"agent": "crew-1"}})
	return w
}

// put adds b under the next id, created a minute after the last bead put.
func (w *dmWorld) put(b *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	w.store.seq++
	w.at = w.at.Add(time.Minute)
	b.ID, b.CreatedAt = fmt.Sprintf("gd-msg%d", w.store.seq), w.at
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	w.store.beads[b.ID] = b
	return b
}

// say adds a top-level entry to the DM; subject makes it a card.
func (w *dmWorld) say(author, subject string) string {
	f := map[string]string{"channel_id": w.dm, "author": author}
	if subject != "" {
		f["kind"], f["subject_id"] = "card", subject
	}
	return w.put(&beadsapi.BeadDetail{Type: "message", Status: "closed", Labels: []string{"project:gardenio"}, Fields: f}).ID
}

// readTo sets Ann's read position in the DM.
func (w *dmWorld) readTo(msg string) {
	w.put(&beadsapi.BeadDetail{Type: "user", Fields: map[string]string{
		"email": "ann@example.com", "read_cursors": `{"` + w.dm + `":"` + msg + `"}`,
	}})
}

func (w *dmWorld) sweep(t *testing.T) (closed int, status string) {
	t.Helper()
	n, err := (&conversation.DMCloser{Store: w.store}).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return n, w.store.beads[w.dm].Status
}

func TestDMCloserClosesADMItsAgentLeftReadAndAnswered(t *testing.T) {
	w := newDMWorld(t)
	decision := w.put(&beadsapi.BeadDetail{Type: "decision", Status: "closed"}).ID
	w.say("agent:crew-1", "")
	card := w.say("agent:crew-1", decision)
	w.readTo(card)
	if n, status := w.sweep(t); n != 1 || status != "closed" {
		t.Fatalf("sweep closed %d, DM %q; want the DM closed", n, status)
	}
	if n, _ := w.sweep(t); n != 0 {
		t.Fatalf("a second sweep closed %d; the closed DM is not listed again", n)
	}
}

// The person's own words after their read position are not unread.
func TestDMCloserClosesWhenOnlyThePersonSpokeSinceTheirPosition(t *testing.T) {
	w := newDMWorld(t)
	w.readTo(w.say("agent:crew-1", ""))
	w.say("oidc:ann@example.com", "")
	if n, status := w.sweep(t); n != 1 || status != "closed" {
		t.Fatalf("sweep closed %d, DM %q; want the DM closed", n, status)
	}
}

func TestDMCloserKeepsADMWithAnOpenCard(t *testing.T) {
	for _, kind := range []string{"decision", "permission", "escalation"} {
		t.Run(kind, func(t *testing.T) {
			w := newDMWorld(t)
			subject := w.put(&beadsapi.BeadDetail{Type: kind, Status: "open"}).ID
			w.readTo(w.say("agent:crew-1", subject))
			if n, status := w.sweep(t); n != 0 || status == "closed" {
				t.Fatalf("sweep closed %d, DM %q; an open %s card keeps it open", n, status, kind)
			}
		})
	}
}

func TestDMCloserKeepsADMWithSomethingUnread(t *testing.T) {
	t.Run("said after the read position", func(t *testing.T) {
		w := newDMWorld(t)
		w.readTo(w.say("agent:crew-1", ""))
		w.say("agent:crew-1", "")
		if n, status := w.sweep(t); n != 0 || status == "closed" {
			t.Fatalf("sweep closed %d, DM %q; an unread entry keeps it open", n, status)
		}
	})
	t.Run("never read", func(t *testing.T) {
		w := newDMWorld(t)
		w.say("agent:crew-1", "")
		if n, status := w.sweep(t); n != 0 || status == "closed" {
			t.Fatalf("sweep closed %d, DM %q; a DM never read keeps it open", n, status)
		}
	})
}

func TestDMCloserKeepsADMWhoseAgentIsLive(t *testing.T) {
	w := newDMWorld(t)
	w.agent.Status = "open"
	w.readTo(w.say("agent:crew-1", ""))
	if n, status := w.sweep(t); n != 0 || status == "closed" {
		t.Fatalf("sweep closed %d, DM %q; a live agent keeps its DM open", n, status)
	}
}

// The same agent writing to the person again (woken in place) reopens the DM,
// history and all; a successor has a name of its own and gets its own DM.
func TestEnsureDMReopensAClosedDM(t *testing.T) {
	w := newDMWorld(t)
	w.readTo(w.say("agent:crew-1", ""))
	if n, _ := w.sweep(t); n != 1 {
		t.Fatalf("sweep closed %d, want 1", n)
	}
	creates := len(w.store.creates)
	ch, err := conversation.EnsureDM(context.Background(), w.store, "gardenio", "", "agent:crew-1", "oidc:ann@example.com")
	if err != nil || ch.ID != w.dm || w.store.beads[w.dm].Status != "open" || len(w.store.creates) != creates {
		t.Fatalf("EnsureDM = %+v, %v (DM %q, %d creates); want %s reopened", ch, err, w.store.beads[w.dm].Status, len(w.store.creates)-creates, w.dm)
	}
	next, err := conversation.EnsureDM(context.Background(), w.store, "gardenio", "", "agent:crew-2", "oidc:ann@example.com")
	if err != nil || next.ID == w.dm {
		t.Fatalf("successor's EnsureDM = %+v, %v; want a DM of its own", next, err)
	}
}
