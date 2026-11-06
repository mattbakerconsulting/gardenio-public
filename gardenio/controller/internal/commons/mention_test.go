package commons

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// fakeComposer records the mentions the Commons sends the bridge.
type fakeComposer struct {
	mu     sync.Mutex
	got    []ComposeRequest
	answer CardAnswer
	err    error
}

func (f *fakeComposer) Compose(_ context.Context, req ComposeRequest) (CardAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, req)
	return f.answer, f.err
}

func (f *fakeComposer) calls() []ComposeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ComposeRequest(nil), f.got...)
}

// withPromotedThread adds a chat root a mention promoted (cl-chat1, with a
// reply from before) and its wish head (cl-phead, with a reply since).
func withPromotedThread(d *commonstest.Daemon) {
	cl := []string{"project:claimlayer"}
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	d.Put(&beadsapi.BeadDetail{ID: "cl-chat1", Type: "message", Title: "@gardenio why is CI red?", Status: "closed", Labels: cl, CreatedAt: at(14),
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ann@claimlayer.example", "origin": "commons",
			"text": "@gardenio why is CI red?", "promoted_to": "cl-phead"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-chatrep1", Type: "message", Title: "me too", Status: "closed", Labels: cl, CreatedAt: at(15),
		Fields: map[string]string{"channel_id": "cl-chgen", "thread_id": "cl-chat1", "author": "oidc:bo@claimlayer.example", "text": "me too"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-phead", Type: "task", Title: "Commons thread: @gardenio why is CI red?", Status: "open",
		Labels: []string{"thread", "project:claimlayer"}, CreatedAt: at(16), CreatedBy: "ann@claimlayer.example",
		Fields: map[string]string{"channel_id": "cl-chgen", "slack_root_text": "@gardenio why is CI", "promoted_from": "cl-chat1"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-prep", Type: "message", Title: "looking", Status: "closed", Labels: cl, CreatedAt: at(17),
		Fields: map[string]string{"channel_id": "cl-chgen", "thread_id": "cl-phead", "author": "agent:thread-cabc", "text": "looking"}})
}

// A promoted chat root reads as its wish head: the head takes the root's
// author and whole words, the thread is every reply under either, and a reply
// to the root goes under the head.
func TestPromotedThread(t *testing.T) {
	d := commonstest.TwoLands(t0)
	withPromotedThread(d)
	h := commonsServer(t, d)

	code, body := get(t, h, "/test/m/cl-phead", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("head: %d %s", code, body)
	}
	mustContain(t, body, `id="m-cl-phead"`, "why is CI red?")

	code, body = get(t, h, "/garden/c/claimlayer/general/t/cl-chat1", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("thread: %d %s", code, body)
	}
	mustContain(t, body, `id="m-cl-phead"`, `id="m-cl-chatrep1"`, `id="m-cl-prep"`, "why is CI red?")
	if strings.Index(body, `id="m-cl-chatrep1"`) > strings.Index(body, `id="m-cl-prep"`) {
		t.Errorf("replies are not oldest first")
	}

	rec := postForm(t, h, "/garden/c/t/cl-chat1/reply", "text="+url.QueryEscape("a reply to the old root"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body.String())
	}
	var posted *beadsapi.BeadDetail
	for _, b := range d.Beads {
		if b.Fields["text"] == "a reply to the old root" {
			posted = b
		}
	}
	if posted == nil || posted.Fields["thread_id"] != "cl-phead" {
		t.Fatalf("a reply to a promoted root went to %+v, want under cl-phead", posted)
	}
}

// A say or reply that mentions goes to the bridge once written, and the
// bridge's words come back in X-Commons-Note; a guest's mention goes too (the
// bridge delivers it in a thread as a reply, and refuses a top-level one,
// which is posted and told it starts nothing); only people named is no note.
func TestMentionNote(t *testing.T) {
	d := commonstest.TwoLands(t0)
	compose := &fakeComposer{answer: CardAnswer{Notes: []string{"Starting an agent for this thread."}}}
	h := commonsServer(t, d, func(h *Handler) { h.Compose = compose })

	rec := postForm(t, h, "/garden/c/claimlayer/general/say", "text="+url.QueryEscape("@gardenio --role crew why is CI red?"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("say: %d %s", rec.Code, rec.Body.String())
	}
	if got := compose.calls(); len(got) != 1 || got[0].Kind != "compose" || got[0].Message == "" {
		t.Fatalf("composer calls = %+v", got)
	}
	if note, _ := url.PathUnescape(rec.Header().Get("X-Commons-Note")); note != "Starting an agent for this thread." {
		t.Errorf("note %q", note)
	}

	compose.err = &CardRefusal{Status: http.StatusForbidden, Message: "Your role in claimlayer does not let you start or control agents."}
	rec = postForm(t, h, "/garden/c/claimlayer/general/say", "text="+url.QueryEscape("@gardenio spin one up"), "X-Test-Guest", "1")
	if rec.Code != http.StatusCreated || len(compose.calls()) != 2 {
		t.Fatalf("guest say: %d, composer calls %d", rec.Code, len(compose.calls()))
	}
	if note, _ := url.PathUnescape(rec.Header().Get("X-Commons-Note")); !strings.Contains(note, "does not let you start") {
		t.Errorf("guest note %q", note)
	}
	compose.err = nil

	rec = postForm(t, h, "/garden/c/claimlayer/general/say", "text="+url.QueryEscape("no mention here, mail ann@claimlayer.example"))
	if rec.Code != http.StatusCreated || len(compose.calls()) != 2 || rec.Header().Get("X-Commons-Note") != "" {
		t.Fatalf("plain say: %d, composer calls %d, note %q", rec.Code, len(compose.calls()), rec.Header().Get("X-Commons-Note"))
	}

	compose.err = &CardRefusal{Status: http.StatusUnprocessableEntity, Message: "That message names no agent."}
	rec = postForm(t, h, "/garden/c/t/cl-wish1/reply", "text="+url.QueryEscape("@ann what do you think?"))
	if rec.Code != http.StatusCreated || len(compose.calls()) != 3 || rec.Header().Get("X-Commons-Note") != "" {
		t.Fatalf("people-only reply: %d, composer calls %d, note %q", rec.Code, len(compose.calls()), rec.Header().Get("X-Commons-Note"))
	}

	compose.err = &CardRefusal{Status: http.StatusBadGateway, Message: "bridge down"}
	rec = postForm(t, h, "/garden/c/claimlayer/general/say", "text="+url.QueryEscape("@gardenio again"))
	if note, _ := url.PathUnescape(rec.Header().Get("X-Commons-Note")); rec.Code != http.StatusCreated || !strings.Contains(note, "did not reach the agents") {
		t.Fatalf("bridge down: %d note %q", rec.Code, note)
	}

	// A guest's mention in a thread goes to the bridge, which delivers it as
	// a reply.
	compose.err = nil
	compose.answer = CardAnswer{Notes: []string{"Your role in claimlayer does not let you start or call on agents, so your message went to the thread as a reply."}}
	n := len(compose.calls())
	rec = postForm(t, h, "/garden/c/t/cl-wish1/reply", "text="+url.QueryEscape("@gardenio also check the logs"), "X-Test-Guest", "1")
	if rec.Code != http.StatusCreated || len(compose.calls()) != n+1 {
		t.Fatalf("guest reply: %d, composer calls %d, want %d", rec.Code, len(compose.calls()), n+1)
	}
	if note, _ := url.PathUnescape(rec.Header().Get("X-Commons-Note")); !strings.Contains(note, "as a reply") {
		t.Errorf("guest reply note %q", note)
	}
}

// The @ list offers @gardenio and the land's agents, the running ones first,
// to a viewer who may start agents, and nothing to a guest.
func TestMentionsList(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, func(h *Handler) { h.Compose = &fakeComposer{} })

	code, body := get(t, h, "/garden/c/claimlayer/general/mentions", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("mentions: %d %s", code, body)
	}
	var got []mentionable
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if len(got) == 0 || got[0].Name != BotMention || got[0].Kind != "bot" {
		t.Fatalf("mentions = %+v, want @gardenio first", got)
	}
	names := map[string]bool{}
	for _, m := range got {
		names[m.Name] = true
	}
	if !names["claimlayer-crew-1"] {
		t.Errorf("a running agent of the land is missing: %+v", got)
	}
	for n := range names {
		if strings.HasPrefix(n, "gardenio-") {
			t.Errorf("another land's agent %q is offered", n)
		}
	}

	r, _ := http.NewRequest("GET", "/garden/c/claimlayer/general/mentions", nil)
	r.Header.Set("X-Test-As", "claimlayer")
	r.Header.Set("X-Test-Guest", "1")
	rec := httptestRecorder(h, r)
	if strings.TrimSpace(rec) != "[]" {
		t.Fatalf("guest mentions = %s, want []", rec)
	}
}

func httptestRecorder(h http.Handler, r *http.Request) string {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Body.String()
}

// The @ list offers the land's people to anyone who may post, guests too: the
// channel's land only, never the viewer, after @gardenio and before the
// agents.
func TestMentionsListPeople(t *testing.T) {
	d := commonstest.TwoLands(t0)
	list := func(h http.Handler, path string, hdr ...string) []mentionable {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var got []mentionable
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}
	names := func(ms []mentionable, kind string) []string {
		var out []string
		for _, m := range ms {
			if m.Kind == kind {
				out = append(out, m.Name)
			}
		}
		return out
	}

	h := commonsServer(t, d, func(h *Handler) { h.People = d })
	got := list(h, "/garden/c/claimlayer/general/mentions", "X-Test-As", "claimlayer")
	if p := names(got, "person"); strings.Join(p, ",") != "ann,cy" {
		t.Errorf("bo's people in claimlayer = %v, want ann,cy (not bo, not gardenio's dee)", p)
	}
	if len(names(got, "bot")) != 0 || len(names(got, "agent")) != 0 {
		t.Errorf("no composer, yet @gardenio or agents are offered: %+v", got)
	}
	// A guest mentions people too.
	if p := names(list(h, "/garden/c/claimlayer/general/mentions", "X-Test-As", "claimlayer", "X-Test-Guest", "1"), "person"); strings.Join(p, ",") != "ann,cy" {
		t.Errorf("guest's people = %v, want ann,cy", p)
	}
	// The operator in claimlayer gets claimlayer's people, not gardenio's.
	if p := names(list(h, "/garden/c/claimlayer/general/mentions"), "person"); strings.Join(p, ",") != "ann,bo,cy" {
		t.Errorf("operator's people in claimlayer = %v, want ann,bo,cy", p)
	}
	for _, m := range got {
		if m.Name == "cy" && m.Label != "person" {
			t.Errorf("cy's label %q, want person", m.Label)
		}
	}
	if got := list(h, "/garden/c/claimlayer/general/mentions"); got[1].Name != "bo" || got[1].Label != "person · Bo Lind" {
		t.Errorf("bo's entry = %+v, want the name in the label", got[1])
	}

	// With the composer: @gardenio, then people, then agents.
	h = commonsServer(t, d, func(h *Handler) { h.People, h.Compose = d, &fakeComposer{} })
	got = list(h, "/garden/c/claimlayer/general/mentions", "X-Test-As", "claimlayer")
	kinds := []string{}
	for _, m := range got {
		if len(kinds) == 0 || kinds[len(kinds)-1] != m.Kind {
			kinds = append(kinds, m.Kind)
		}
	}
	if strings.Join(kinds, ",") != "bot,person,agent" {
		t.Errorf("order of kinds = %v, want bot,person,agent", kinds)
	}

	// The composer names its @ list for a guest once people are offered.
	_, body := get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer")
	mustContain(t, body, `data-mentions="/base/garden/c/claimlayer/general/mentions"`)
}

// landPeople reads only lands the bearer holds (kbeads answers 404 for any
// other), and gives two people who share a local part a handle each.
func TestLandPeople(t *testing.T) {
	d := commonstest.TwoLands(t0)
	cl := beadsapi.WithUserBearer(context.Background(), commonstest.ClaimlayerBearer)
	if _, err := landPeople(cl, d, "gardenio"); err == nil {
		t.Fatalf("a claimlayer bearer read gardenio's people")
	}
	d.Members["claimlayer"] = append(d.Members["claimlayer"],
		beadsapi.LandMember{Principal: "oidc:Ann@Other.example", Name: "Ann O"},
		beadsapi.LandMember{Principal: "agent:x", Name: "x"},
		beadsapi.LandMember{Principal: "oidc:ann@claimlayer.example", Name: "dup"})
	people, err := landPeople(cl, d, "claimlayer")
	if err != nil {
		t.Fatal(err)
	}
	var handles []string
	for _, p := range people {
		handles = append(handles, p.Handle+"="+p.Principal)
	}
	want := "ann.claimlayer.example=oidc:ann@claimlayer.example,ann.other.example=oidc:ann@other.example,bo=oidc:bo@claimlayer.example,cy=oidc:cy@both.example"
	if strings.Join(handles, ",") != want {
		t.Errorf("handles = %v\nwant %s", handles, want)
	}
}

// A say or reply that @-mentions people of its land writes one notification
// per person, in the land, once each, never to the author and never to a
// person of another land.
func TestMentionNotifies(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Selves = map[string]string{commonstest.ClaimlayerBearer: "oidc:bo@claimlayer.example"}
	h := commonsServer(t, d, func(h *Handler) { h.People = d })
	notes := func() []*beadsapi.BeadDetail {
		var out []*beadsapi.BeadDetail
		for _, b := range d.Beads {
			if b.Type == typeNotification {
				out = append(out, b)
			}
		}
		return out
	}

	rec := postForm(t, h, "/garden/c/claimlayer/general/say", "text="+url.QueryEscape("@ann and @cy, look. @dee @bo @nobody @ANN"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("say: %d %s", rec.Code, rec.Body.String())
	}
	var msg string
	for _, b := range d.Beads {
		if b.Type == "message" && strings.HasPrefix(b.Fields["text"], "@ann and @cy") {
			msg = b.ID
		}
	}
	got := map[string]*beadsapi.BeadDetail{}
	for _, b := range notes() {
		got[b.Fields["to"]] = b
	}
	if len(got) != 2 || got["oidc:ann@claimlayer.example"] == nil || got["oidc:cy@both.example"] == nil || len(notes()) != 2 {
		t.Fatalf("notifications to %v, want ann and cy once each", got)
	}
	n := got["oidc:ann@claimlayer.example"]
	if n.Fields["reason"] != "mention" || n.Fields["message_id"] != msg || n.Fields["channel_id"] != "cl-chgen" || n.Fields["thread_id"] != "" {
		t.Errorf("notification fields = %v", n.Fields)
	}
	if strings.Join(n.Labels, ",") != "project:claimlayer" || n.Title != "bo mentioned you in #general" {
		t.Errorf("notification labels %v title %q", n.Labels, n.Title)
	}
	for _, c := range d.Created {
		if c.Type == typeNotification && c.IdempotencyKey != "mention:"+msg+":"+fieldsTo(c) {
			t.Errorf("idempotency key %q", c.IdempotencyKey)
		}
	}

	// A reply names its thread.
	rec = postForm(t, h, "/garden/c/t/cl-wish1/reply", "text="+url.QueryEscape("@ann see above"))
	if rec.Code != http.StatusCreated || len(notes()) != 3 {
		t.Fatalf("reply: %d, %d notifications", rec.Code, len(notes()))
	}
	for _, b := range notes() {
		if b.Fields["thread_id"] == "cl-wish1" && b.Fields["to"] != "oidc:ann@claimlayer.example" {
			t.Errorf("thread notification to %s", b.Fields["to"])
		}
	}

	// Cross-land: the operator in gardenio names claimlayer's ann and bo, and
	// gardenio's dee and cy. Only dee and cy are told, in gardenio.
	before := len(notes())
	rec = postForm(t, h, "/garden/c/gardenio/general/say", "text="+url.QueryEscape("@ann @bo @dee @cy"), "X-Test-As", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("gardenio say: %d %s", rec.Code, rec.Body.String())
	}
	var gd []string
	for _, b := range notes()[0:] {
		if strings.Join(b.Labels, ",") == "project:gardenio" {
			gd = append(gd, b.Fields["to"])
		}
	}
	if len(notes())-before != 2 || len(gd) != 2 || slices.Contains(gd, "oidc:ann@claimlayer.example") || slices.Contains(gd, "oidc:bo@claimlayer.example") {
		t.Errorf("gardenio notifications to %v, want dee and cy only", gd)
	}

	// The message stands when the people cannot be read or a write fails.
	d.Members = nil
	rec = postForm(t, h, "/garden/c/claimlayer/general/say", "text="+url.QueryEscape("@ann anyone?"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("say with no people: %d", rec.Code)
	}
}

func fieldsTo(c beadsapi.CreateBeadRequest) string {
	var f map[string]string
	_ = json.Unmarshal(c.Fields, &f)
	return f["to"]
}

// A mention by a handle in another script notifies the person and is never
// handed to the bridge, which reads only ASCII names.
func TestMentionUnicodeHandleStaysOffTheBridge(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Members["claimlayer"] = append(d.Members["claimlayer"], beadsapi.LandMember{Principal: "oidc:田中花子@claimlayer.example", Name: "田中花子"})
	compose := &fakeComposer{}
	h := commonsServer(t, d, func(h *Handler) { h.People, h.Compose = d, compose })
	rec := postForm(t, h, "/garden/c/claimlayer/general/say", "text="+url.QueryEscape("@田中花子 ありがとう"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("say: %d %s", rec.Code, rec.Body.String())
	}
	if got := compose.calls(); len(got) != 0 {
		t.Errorf("a Unicode-only mention went to the bridge: %+v", got)
	}
	n := 0
	for _, b := range d.Beads {
		if b.Type == typeNotification && b.Fields["to"] == "oidc:田中花子@claimlayer.example" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d notifications to 田中花子, want 1", n)
	}
}
