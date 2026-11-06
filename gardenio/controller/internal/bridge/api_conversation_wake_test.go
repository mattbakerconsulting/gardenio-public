package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
)

// newWakeAPI is newSayAPI with the bridge's thread-agent state, which the
// wake reads to find the thread's agent.
func newWakeAPI(t *testing.T, twin *surfacetest.SlackTwin) (*ConversationAgentAPI, *mockDaemon) {
	t.Helper()
	a, daemon := newSayAPI(t, twin)
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a.bot.state = state
	seedLandProject(daemon, "gardenio")
	return a, daemon
}

// postWake posts a say with wake=true as crew-say.
func postWake(t *testing.T, h http.Handler, extra map[string]string) (*httptest.ResponseRecorder, sayResponse) {
	t.Helper()
	body := map[string]any{"wake": true}
	for k, v := range sayAgentBody(extra) {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/conversation/say", bytes.NewReader(raw)))
	var resp sayResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr, resp
}

// gb say --thread <head> --wake posts the words in the thread and gives them
// to the thread's running agent (gd-y8LrXMOa46): the herder's wake of a wish
// it judged unfulfilled.
func TestAgentSayWakeNudgesThreadAgent(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newWakeAPI(t, twin)
	coop, coopCalls := newDeliveringCoop(t)
	seedLiveThreadAgent(a.bot, daemon, "thread-ci", sayHeadID, "gardenio", coop)
	_ = a.bot.state.SetThreadAgent(sayChannel, sayRootTS, "thread-ci")

	rr, resp := postWake(t, a.mux(), map[string]string{"thread": sayHeadID, "text": "CI is still red on main; the fix has not landed"})
	if rr.Code != http.StatusOK || resp.ID == "" || !strings.Contains(resp.Woke, "Told thread-ci") {
		t.Fatalf("wake: %d %s", rr.Code, rr.Body)
	}
	calls := strings.Join(coopCalls(), "\n")
	if !strings.Contains(calls, "CI is still red on main") || !strings.Contains(calls, "Wake from crew-say") {
		t.Fatalf("coop calls = %s", calls)
	}
	posted := twin.Calls()
	if len(posted) == 0 || posted[len(posted)-1].Params["thread_ts"] != sayRootTS {
		t.Fatalf("the words were not posted in the thread: %+v", posted)
	}
}

// A say without wake reaches no agent, whatever it says: an agent's
// "@gardenio" is not a mention.
func TestAgentSayWithoutWakeReachesNoAgent(t *testing.T) {
	a, daemon := newWakeAPI(t, surfacetest.NewSlackTwin())
	coop, coopCalls := newDeliveringCoop(t)
	seedLiveThreadAgent(a.bot, daemon, "thread-ci", sayHeadID, "gardenio", coop)
	_ = a.bot.state.SetThreadAgent(sayChannel, sayRootTS, "thread-ci")

	rr, resp := postSay(t, a.mux(), sayAgentBody(map[string]string{"thread": sayHeadID, "text": "@gardenio CI is still red"}))
	if rr.Code != http.StatusOK || resp.Woke != "" {
		t.Fatalf("say: %d %s", rr.Code, rr.Body)
	}
	if got := coopCalls(); len(got) != 0 {
		t.Fatalf("a plain say reached the thread's agent: %v", got)
	}
}

// The thread's own agent does not wake itself.
func TestAgentSayWakeNotItself(t *testing.T) {
	a, daemon := newWakeAPI(t, surfacetest.NewSlackTwin())
	coop, coopCalls := newDeliveringCoop(t)
	seedLiveThreadAgent(a.bot, daemon, "crew-say", sayHeadID, "gardenio", coop)
	_ = a.bot.state.SetThreadAgent(sayChannel, sayRootTS, "crew-say")

	rr, resp := postWake(t, a.mux(), map[string]string{"thread": sayHeadID, "text": "still working on it"})
	if rr.Code != http.StatusOK || !strings.Contains(resp.Woke, "does not wake itself") {
		t.Fatalf("self wake: %d %s", rr.Code, rr.Body)
	}
	if got := coopCalls(); len(got) != 0 {
		t.Fatalf("the speaker nudged itself: %v", got)
	}
}

// A Commons thread with no agent gets a fresh one, which runs as the person
// who asked (the head's operated_by), not as the agent that woke it.
func TestAgentSayWakeSpawnsAsTheAsker(t *testing.T) {
	a, daemon := newWakeAPI(t, surfacetest.NewSlackTwin())
	daemon.beads["gd-cwake"] = &beadsapi.BeadDetail{
		ID: "gd-cwake", Type: "task", Status: "open", Title: "a wish",
		Labels: []string{threadKindLabel, "project:gardenio"},
		Fields: map[string]string{"channel_id": sayChanBead, "operated_by": "oidc:ada@example.com"},
	}

	rr, resp := postWake(t, a.mux(), map[string]string{"thread": "gd-cwake", "text": "the docs page is still missing"})
	if rr.Code != http.StatusOK || !strings.Contains(resp.Woke, "Starting an agent") {
		t.Fatalf("wake: %d %s", rr.Code, rr.Body)
	}
	var agent *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, d := range daemon.beads {
		if d.Type == "agent" && d.Fields["thread_id"] == "gd-cwake" {
			agent = d
		}
	}
	daemon.mu.Unlock()
	if agent == nil {
		t.Fatal("no agent was spawned for the thread")
	}
	if got := agent.Fields["operated_by"]; got != "oidc:ada@example.com" {
		t.Fatalf("operated_by = %q, want the asker", got)
	}
}

// A wake is refused, and nothing is posted, unless it is said in a thread of
// a land.
func TestAgentSayWakeRefusals(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newWakeAPI(t, twin)
	daemon.beads["gd-noland"] = &beadsapi.BeadDetail{ID: "gd-noland", Type: "task", Status: "open",
		Labels: []string{threadKindLabel}, Fields: map[string]string{}}
	for name, c := range map[string]struct {
		extra map[string]string
		want  int
	}{
		"channel": {map[string]string{"channel": sayChanBead, "text": "x"}, http.StatusBadRequest},
		"to":      {map[string]string{"to": "agent:thread-ci", "text": "x"}, http.StatusBadRequest},
		"no land": {map[string]string{"thread": "gd-noland", "text": "x"}, http.StatusUnprocessableEntity},
	} {
		if rr, _ := postWake(t, a.mux(), c.extra); rr.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rr.Code, rr.Body, c.want)
		}
	}
	if n := len(sayMessageBeads(daemon)); n != 0 {
		t.Fatalf("a refused wake wrote %d message beads", n)
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("a refused wake called Slack %d times", n)
	}
}
