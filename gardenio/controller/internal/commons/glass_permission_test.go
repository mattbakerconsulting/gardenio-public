package commons

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// TestPermissionAnswered: a permission prompt answered on the Glass has the
// bridge close the agent's open request for that tool, by its card, on the
// viewer's bearer (Lands workbench-glass spec 3.3); any other request, agent
// or land is left alone, and a viewer whose role does not answer permissions
// is refused.
func TestPermissionAnswered(t *testing.T) {
	d := commonstest.TwoLands(t0)
	// another agent's request, newer, for the same tool and agent name: its
	// recorded bead says it is not cl-agent1's
	d.Put(&beadsapi.BeadDetail{ID: "cl-perm9", Type: "permission", Title: "Permission: Bash", Status: "open", Labels: []string{"project:claimlayer"},
		CreatedAt: t0.Add(20 * 60e9), Fields: map[string]string{"agent": "claimlayer-crew-1", "agent_bead_id": "cl-agent9", "tool_name": "Bash", "request_id": "zzzzz"}})
	in := &fakeInbound{}
	h := &Handler{Beads: d, BasePath: "/base", Gate: testGate, Inbound: in,
		Scope: func(r *http.Request) Scope {
			if r.Header.Get("X-Test-Other") != "" {
				return Scope{Lands: []string{"gardenio"}}
			}
			return claimlayerScope
		}}
	req := func(hdr ...string) *http.Request {
		r := httptest.NewRequest("POST", "/garden/glass/cl-agent1/send", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r.WithContext(beadsapi.WithUserBearer(r.Context(), commonstest.ClaimlayerBearer))
	}

	id, err := h.PermissionAnswered(req(), "cl-agent1", "Bash", "allow", "Yes")
	if err != nil || id != "cl-perm1" {
		t.Fatalf("Bash answered: %q, %v; want cl-perm1", id, err)
	}
	want := CardRequest{Kind: "action", Card: "cl-pcard1", Action: "answered", Values: map[string]string{"behavior": "allow", "said": "Yes"}}
	if len(in.got) != 1 || !reflect.DeepEqual(in.got[0], want) || in.bearer[0] != commonstest.ClaimlayerBearer {
		t.Fatalf("sent the bridge %+v on %v; want %+v on the viewer's bearer", in.got, in.bearer, want)
	}

	for _, c := range []struct {
		name, agent, tool string
		hdr               []string
	}{
		{"a tool no open request names", "cl-agent1", "Write", nil},
		{"an agent with no open request", "cl-agent2", "Bash", nil},
		{"no such agent", "cl-nobody", "Bash", nil},
		{"a viewer of another land", "cl-agent1", "Bash", []string{"X-Test-Other", "1"}},
	} {
		in.got = nil
		if id, err := h.PermissionAnswered(req(c.hdr...), c.agent, c.tool, "deny", "No"); id != "" || len(in.got) != 0 {
			t.Errorf("%s: closed %q (%v), sent %+v; want nothing", c.name, id, err, in.got)
		}
	}

	// a prompt that names no tool closes the agent's one open request (cl-perm9 is another agent's)
	in.got = nil
	if id, err := h.PermissionAnswered(req(), "cl-agent1", "", "", "Continue"); id != "cl-perm1" || err != nil {
		t.Errorf("no tool, one open: %q, %v; want cl-perm1", id, err)
	}
	// two open for the agent: a prompt that names no tool closes neither
	d.Put(&beadsapi.BeadDetail{ID: "cl-perm8", Type: "permission", Title: "Permission: Edit", Status: "open", Labels: []string{"project:claimlayer"},
		CreatedAt: t0, Fields: map[string]string{"agent": "claimlayer-crew-1", "tool_name": "Edit", "request_id": "yyyyy"}})
	in.got = nil
	if id, _ := h.PermissionAnswered(req(), "cl-agent1", "", "", "Continue"); id != "" || len(in.got) != 0 {
		t.Errorf("no tool, two open: closed %q, sent %+v; want nothing", id, in.got)
	}

	in.got = nil
	if id, err := h.PermissionAnswered(req("X-Test-Guest", "1"), "cl-agent1", "Bash", "allow", "Yes"); !errors.Is(err, ErrMayNotAnswer) || id != "" || len(in.got) != 0 {
		t.Errorf("a guest: %q, %v, sent %+v; want ErrMayNotAnswer and nothing sent", id, err, in.got)
	}

	in.err = &CardRefusal{Status: http.StatusConflict, Message: "This permission request was already answered."}
	if _, err := h.PermissionAnswered(req(), "cl-agent1", "Bash", "allow", "Yes"); err == nil {
		t.Error("the bridge's refusal was not passed on")
	}
}

// TestAgentIn: an open card's See the screen resolves its agent to the
// agent's bead in the card's land: the recorded bead first, else the one open
// agent of that name.
func TestAgentIn(t *testing.T) {
	d := commonstest.TwoLands(t0)
	ctx := beadsapi.WithUserBearer(context.Background(), commonstest.AdminBearer)
	for _, c := range []struct{ name, id, agent, land, want string }{
		{"the recorded bead", "cl-agent1", "", "claimlayer", "cl-agent1"},
		{"by name", "", "claimlayer-crew-1", "claimlayer", "cl-agent1"},
		{"a recorded bead that is no agent, then the name", "cl-perm1", "claimlayer-crew-1", "claimlayer", "cl-agent1"},
		{"a recorded bead of another land", "cl-agent1", "", "gardenio", ""},
		{"a name of another land", "", "claimlayer-crew-1", "gardenio", ""},
		{"a closed agent's name", "", "claimlayer-crew-2", "claimlayer", ""},
		{"neither", "", "", "claimlayer", ""},
	} {
		got := ""
		if a := agentIn(ctx, d, c.id, c.agent, c.land); a != nil {
			got = a.ID
		}
		if got != c.want {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}
