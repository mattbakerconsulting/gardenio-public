package server

import (
	"net/http"
	"strings"
	"testing"
)

// The Commons message cap holds at the land door, not only in beads-viewer: a
// land caller posting type=message through its door is held to the per-member
// cap, which the viewer alone enforced before (gd-i74esUHCTg item 2). Each
// caller has its own bucket; other types and the same caller off the door are
// not counted.
func TestLandDoorMessageCap(t *testing.T) {
	f := newScopedFixture(t)
	f.seedDoorAgents()
	f.srv.landDoorMessages = newLandDoorMessageRate(3)
	msg := map[string]any{"title": "status", "type": "message", "labels": []string{"project:claimlayer"}}

	for i := 0; i < 3; i++ {
		if code, body := f.landDo(t, tokLandClaimlayer, "claimlayer", "POST", "/v1/beads", msg); code != http.StatusCreated {
			t.Fatalf("message %d through the door: %d %s", i+1, code, body)
		}
	}
	code, body := f.landDo(t, tokLandClaimlayer, "claimlayer", "POST", "/v1/beads", msg)
	if code != http.StatusTooManyRequests || !strings.Contains(body, "posting too fast") {
		t.Fatalf("message past the cap through the door: %d %s, want 429", code, body)
	}

	task := map[string]any{"title": "a task", "type": "task", "labels": []string{"project:claimlayer"}}
	if code, body := f.landDo(t, tokLandClaimlayer, "claimlayer", "POST", "/v1/beads", task); code != http.StatusCreated {
		t.Fatalf("a task past the message cap: %d %s, want it created", code, body)
	}
	if code, body := f.landDo(t, tokLandAgent, "claimlayer", "POST", "/v1/beads", msg); code != http.StatusCreated {
		t.Fatalf("another caller in the land: %d %s, want its own bucket", code, body)
	}
	if code, body := f.landDo(t, tokLandClaimlayer, "", "POST", "/v1/beads", msg); code != http.StatusCreated {
		t.Fatalf("the same caller off the door: %d %s, want it uncounted", code, body)
	}
}

func TestLandDoorMessagesPerMinute(t *testing.T) {
	for v, want := range map[string]int{"": 20, "0": 0, "5": 5, " 7 ": 7, "-1": 20, "lots": 20} {
		if got := landDoorMessagesPerMinute(func(string) string { return v }); got != want {
			t.Errorf("%s=%q: %d, want %d", landDoorMessagesPerMinuteEnv, v, got, want)
		}
	}
	if !(*landDoorMessageRate)(nil).allow("x") || !newLandDoorMessageRate(0).allow("x") {
		t.Fatal("a nil or zero cap must admit")
	}
}
