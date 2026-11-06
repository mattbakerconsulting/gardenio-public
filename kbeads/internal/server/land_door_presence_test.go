package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"gardenio/kbeads/internal/presence"
)

// rosterActors reads GET /v1/agents/roster through door as token.
func (f *scopedFixture) rosterActors(t *testing.T, token, door string) []string {
	t.Helper()
	code, body := f.landDo(t, token, door, http.MethodGet, "/v1/agents/roster", nil)
	if code != http.StatusOK {
		t.Fatalf("roster through %s's door: %d %s", door, code, body)
	}
	var out struct {
		Actors []struct {
			Actor     string `json:"actor"`
			LastEvent string `json:"last_event"`
		} `json:"actors"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("roster body: %v (%s)", err, body)
	}
	var names []string
	for _, a := range out.Actors {
		names = append(names, a.Actor+"/"+a.LastEvent)
	}
	slices.Sort(names)
	return names
}

// A land agent's own requests through its door are its presence, under the
// name its token was minted for (gd-2iqSr9ANOH); the roster through a door
// lists only that land's agents, never the fleet's or another land's.
func TestLandDoorPresence_AgentTrafficIsPresence(t *testing.T) {
	f := newScopedFixture(t)
	f.seedDoorAgents()
	const tokOther = "agent-other-presence-tok"
	f.seedTenantLand("other", nil)
	f.seedOwnMachineAgent("other-agent-1", "other-agent-1", "other", tokOther)
	f.srv.Presence.RecordHookEvent(presence.HookEvent{Actor: "fleet-crew", HookType: "PreToolUse", CWD: "/home/agent/fleet"})

	// The heartbeat gb sends on a land door, and an ordinary kd read.
	for _, s := range []struct{ tok, door, path string }{
		{tokLandAgent, "claimlayer", "/v1/me"},
		{tokLandAgent, "claimlayer", "/v1/beads?limit=1"},
		{tokOther, "other", "/v1/me"},
		// The land principal is not an agent: no presence.
		{tokLandClaimlayer, "claimlayer", "/v1/me"},
	} {
		if code, body := f.landDo(t, s.tok, s.door, http.MethodGet, s.path, nil); code != http.StatusOK {
			t.Fatalf("GET %s through %s's door: %d %s", s.path, s.door, code, body)
		}
	}
	// Straight at the daemon the agent token records nothing for the land.
	if code, body := f.landDo(t, tokLandAgent, "", http.MethodGet, "/v1/me", nil); code != http.StatusOK {
		t.Fatalf("GET /v1/me straight at the daemon: %d %s", code, body)
	}

	if got, want := f.rosterActors(t, tokLandAgent, "claimlayer"), []string{"cl-agent/door"}; !slices.Equal(got, want) {
		t.Errorf("claimlayer's roster through its door = %v, want %v", got, want)
	}
	if got, want := f.rosterActors(t, tokOther, "other"), []string{"other-agent-1/door"}; !slices.Equal(got, want) {
		t.Errorf("other's roster through its door = %v, want %v", got, want)
	}
	// The land principal reads the same land roster.
	if got, want := f.rosterActors(t, tokLandClaimlayer, "claimlayer"), []string{"cl-agent/door"}; !slices.Equal(got, want) {
		t.Errorf("claimlayer's roster as the land = %v, want %v", got, want)
	}

	var cl presence.Entry
	for _, e := range f.srv.Presence.Roster(0) {
		if e.Actor == "cl-agent" {
			cl = e
		}
	}
	if cl.Land != "claimlayer" || cl.EventCount != 3 {
		t.Errorf("cl-agent presence = %+v, want land claimlayer and three door requests (the roster read is one)", cl)
	}
}
