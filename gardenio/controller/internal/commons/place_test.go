package commons

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// GET /garden/c/bead/{id} finds a bead's thread: a reply's, a root's own, an
// agent's through thread_id or the legacy slack_thread_bead_id, and a bead in
// no thread through its card (a decision, a permission, an agent's card in
// #crew), which is where a push about it lands. A browser is
// sent there; a JSON client gets the place. Nothing outside the viewer's lands
// resolves.
func TestPlaceFindsTheThread(t *testing.T) {
	d := commonstest.TwoLands(t0)
	cl := []string{"project:claimlayer"}
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent1", Type: "agent", Title: "claimlayer-crew-1", Status: "open", Labels: cl, CreatedAt: t0,
		Fields: map[string]string{"thread_id": "cl-wish1"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent2", Type: "agent", Title: "claimlayer-crew-2", Status: "open", Labels: cl, CreatedAt: t0,
		Fields: map[string]string{"slack_thread_bead_id": "cl-wish1"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent3", Type: "agent", Title: "claimlayer-crew-3", Status: "open", Labels: cl, CreatedAt: t0})
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent9", Type: "agent", Title: "claimlayer-crew-9", Status: "open", Labels: cl, CreatedAt: t0})
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent4", Type: "agent", Title: "claimlayer-crew-4", Status: "open", Labels: cl, CreatedAt: t0,
		Fields: map[string]string{"thread_id": "gd-wish1"}})
	h := commonsServer(t, d)

	wish := Place{Land: "claimlayer", Channel: "general", Thread: "cl-wish1", Href: "/base/garden/c/claimlayer/general/t/cl-wish1"}
	for _, tc := range []struct {
		id   string
		want Place
	}{
		{"cl-wish1", wish},  // a wish head
		{"cl-rep1", wish},   // a reply
		{"cl-rep2", wish},   // a reply forwarded before channel_id was stamped
		{"cl-task1", wish},  // the thread's work
		{"cl-agent1", wish}, // an agent bound by thread_id
		{"cl-agent2", wish}, // and by the legacy alias
		{"cl-msg1", Place{Land: "claimlayer", Channel: "general", Thread: "cl-msg1", Href: "/base/garden/c/claimlayer/general/t/cl-msg1"}},
		{"cl-crew1", Place{Land: "claimlayer", Channel: "crew", Thread: "cl-crew1", Href: "/base/garden/c/claimlayer/crew/t/cl-crew1"}},
		{"cl-dec1", Place{Land: "claimlayer", Channel: "decisions", Thread: "cl-dcard1", Href: "/base/garden/c/claimlayer/decisions/t/cl-dcard1"}},
		{"cl-perm1", Place{Land: "claimlayer", Channel: "decisions", Thread: "cl-pcard1", Href: "/base/garden/c/claimlayer/decisions/t/cl-pcard1"}},
		{"cl-agent3", Place{Land: "claimlayer", Channel: "crew", Thread: "cl-card3", Href: "/base/garden/c/claimlayer/crew/t/cl-card3"}}, // no thread, a card
	} {
		r := httptest.NewRequest("GET", "/garden/c/bead/"+tc.id, nil)
		r.Header.Set("X-Test-As", "claimlayer")
		r.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var got Place
		tc.want.Reply = "/base/garden/c/t/" + tc.want.Thread + "/reply" // the viewer may reply in each
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got != tc.want {
			t.Errorf("%s: %d %s, want %+v", tc.id, rec.Code, rec.Body, tc.want)
		}

		r = httptest.NewRequest("GET", "/garden/c/bead/"+tc.id, nil)
		r.Header.Set("Accept", "text/html")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.want.Href {
			t.Errorf("%s navigation: %d to %q, want 303 to %q", tc.id, rec.Code, rec.Header().Get("Location"), tc.want.Href)
		}
	}

	// No thread and no card, no such bead, a bead of another land, an agent
	// bound to a thread in another land, a bead of no land: all are not found.
	for _, id := range []string{"cl-agent9", "cl-nope", "gd-rep1", "gd-wish1", "cl-agent4", "nl-msg1", "xx-msg1", "cl-chgen"} {
		if code, body := get(t, h, "/garden/c/bead/"+id, "claimlayer"); code != http.StatusNotFound {
			t.Errorf("%s: %d %s, want 404", id, code, body)
		}
	}
	// The operator sees every land.
	if code, _ := get(t, h, "/garden/c/bead/gd-rep1", ""); code != http.StatusSeeOther {
		t.Errorf("gd-rep1 as the operator: %d, want 303", code)
	}
}

// The JSON place carries the thread's reply composer as its fragment offers it (composeFor), so the bead panel's
// conversation (gardenhub web/workbench.js, gd-tx2wMH48MV) posts only where the thread pane would: its reply route when
// the viewer may post, and otherwise only the line the fragment says instead.
func TestPlaceSaysWhoMayReply(t *testing.T) {
	d := commonstest.TwoLands(t0)
	place := func(h http.Handler, hdr ...string) Place {
		r := httptest.NewRequest("GET", "/garden/c/bead/cl-rep1", nil)
		r.Header.Set("Accept", "application/json")
		r.Header.Set("X-Test-As", "claimlayer")
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var p Place
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return p
	}
	if p := place(commonsServer(t, d)); p.Reply != "/base/garden/c/t/cl-wish1/reply" || p.Note != "" {
		t.Errorf("a member: reply %q, note %q, want the thread's reply route and no note", p.Reply, p.Note)
	}
	if p := place(commonsServer(t, d), "X-Test-Anon", "1"); p.Reply != "" || p.Note != "Sign in to post here." {
		t.Errorf("signed out: reply %q, note %q, want no route and the sign-in line", p.Reply, p.Note)
	}
	readOnly := func(h *Handler) { h.Gate.May = func(*http.Request, Channel, string) bool { return false } }
	if p := place(commonsServer(t, d, readOnly)); p.Reply != "" || p.Note != "You can read #general but not post in it." {
		t.Errorf("a reader: reply %q, note %q, want no route and the read-only line", p.Reply, p.Note)
	}
	if p := place(commonsServer(t, d, func(h *Handler) { h.Writes = nil })); p.Reply != "" {
		t.Errorf("no writes: reply %q, want none", p.Reply)
	}
}
