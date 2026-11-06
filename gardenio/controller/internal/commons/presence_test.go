package commons

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// presenceHub is a started hub over a source that never sends, so the only
// invalidations it carries are the ones Presence publishes.
func presenceHub(t *testing.T, d *commonstest.Daemon) *Hub {
	t.Helper()
	hub := NewHub(make(liveSource).open, d, nil)
	hub.Start()
	t.Cleanup(hub.Close)
	return hub
}

// next returns the next invalidation on ch, or fails after a second.
func next(t *testing.T, ch <-chan Invalidation) Invalidation {
	t.Helper()
	select {
	case inv := <-ch:
		return inv
	case <-time.After(time.Second):
		t.Fatal("no invalidation")
		return Invalidation{}
	}
}

// quiet fails when ch carries anything within a short wait.
func quiet(t *testing.T, ch <-chan Invalidation) {
	t.Helper()
	select {
	case inv := <-ch:
		t.Fatalf("unexpected invalidation %+v", inv)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestPresenceJoinLeave(t *testing.T) {
	hub := presenceHub(t, commonstest.TwoLands(t0))
	p := NewPresence(hub, nil, nil)
	p.Linger = 0
	cl, stopCL := hub.Subscribe(context.Background(), claimlayerScope)
	defer stopCL()
	gd, stopGD := hub.Subscribe(context.Background(), Scope{Lands: []string{"gardenio"}})
	defer stopGD()

	general := Spot{Land: "claimlayer", Channel: "cl-chgen"}
	leave1 := p.Join("oidc:ann@example.com", general)
	inv := next(t, cl)
	if inv.Kind != "presence" || inv.ID != "cl-chgen" || inv.Channel != "cl-chgen" || inv.Land != "claimlayer" {
		t.Errorf("join published %+v", inv)
	}
	quiet(t, gd) // another land's subscriber never hears of it

	// A second tab of the same person is not news, and neither is closing it.
	leave2 := p.Join("oidc:ann@example.com", general)
	quiet(t, cl)
	leave2()
	leave2() // idempotent
	quiet(t, cl)
	if got := p.Here(general); len(got) != 1 || got[0] != "oidc:ann@example.com" {
		t.Errorf("Here(general) = %v", got)
	}
	if !p.Present("oidc:ann@example.com", Spot{Land: "claimlayer"}) {
		t.Error("a person in a channel is not present in its land")
	}
	if p.Present("oidc:ann@example.com", Spot{Land: "claimlayer", Channel: "cl-chcrew"}) {
		t.Error("a person in #general is present in #crew")
	}

	leave1()
	if inv := next(t, cl); inv.Channel != "cl-chgen" {
		t.Errorf("leave published %+v", inv)
	}
	if got := p.Here(Spot{Land: "claimlayer"}); len(got) != 0 {
		t.Errorf("Here(claimlayer) after leaving = %v", got)
	}

	// Nothing is recorded without a principal or a land.
	p.Join("", general)()
	p.Join("oidc:ann@example.com", Spot{})()
	quiet(t, cl)
}

func TestPresenceLinger(t *testing.T) {
	hub := presenceHub(t, commonstest.TwoLands(t0))
	p := NewPresence(hub, nil, nil)
	p.Linger = 100 * time.Millisecond
	cl, stop := hub.Subscribe(context.Background(), claimlayerScope)
	defer stop()

	at := Spot{Land: "claimlayer", Channel: "cl-chgen"}
	leave := p.Join("oidc:ann@example.com", at)
	next(t, cl)
	// The stream ends at bearer expiry and the browser reconnects at once:
	// nobody sees the person leave and come back.
	leave()
	leave = p.Join("oidc:ann@example.com", at)
	quiet(t, cl)
	if !p.Present("oidc:ann@example.com", at) {
		t.Error("the reconnecting person dropped out")
	}
	time.Sleep(150 * time.Millisecond)
	quiet(t, cl)
	leave()
	if inv := next(t, cl); inv.Channel != "cl-chgen" {
		t.Errorf("leave published %+v", inv)
	}
	if p.Present("oidc:ann@example.com", at) {
		t.Error("still present after the linger")
	}
}

func TestNilPresence(t *testing.T) {
	var p *Presence
	p.Start()
	p.Join("oidc:ann@example.com", Spot{Land: "claimlayer"})()
	p.Publish(Spot{Land: "claimlayer"})
	p.PollAgents(context.Background())
	if p.Here(Spot{Land: "claimlayer"}) != nil || p.Present("x", Spot{Land: "claimlayer"}) {
		t.Error("a nil Presence has someone in it")
	}
	p.Close()
	// A presence without a hub records but publishes nowhere.
	q := NewPresence(nil, nil, nil)
	q.Linger = 0
	leave := q.Join("oidc:ann@example.com", Spot{Land: "claimlayer"})
	if !q.Present("oidc:ann@example.com", Spot{Land: "claimlayer"}) {
		t.Error("hubless presence lost the join")
	}
	leave()
}

func TestEventsStreamJoinsPresence(t *testing.T) {
	d := commonstest.TwoLands(t0)
	hub := presenceHub(t, d)
	p := NewPresence(hub, nil, nil)
	p.Linger = 0
	h := &Handler{
		Beads: d, Hub: hub, Presence: p,
		Scope:  func(*http.Request) Scope { return claimlayerScope },
		Viewer: func(*http.Request) string { return "oidc:ann@example.com" },
	}

	// Naming the channel by name puts the viewer in it, and the stream hears
	// its own arrival, carrying the header's presence slot (stream.go).
	body := stream(t, h, "/garden/c/events?land=claimlayer&channel=general", commonstest.ClaimlayerBearer, "", "event: presence")
	mustContain(t, body, "event: presence\ndata: {\"id\":\"cl-chgen\",\"channel\":\"cl-chgen\",\"land\":\"claimlayer\",\"html\":\"<div class=\\\"cm-presence\\\"",
		`aria-label=\"1 person in #general\"`)
	if got := p.Here(Spot{Land: "claimlayer"}); len(got) != 0 {
		t.Errorf("the ended stream left %v behind", got)
	}

	// A land alone: present in the land, in no channel.
	body = stream(t, h, "/garden/c/events?land=claimlayer", commonstest.ClaimlayerBearer, "", "event: presence")
	mustContain(t, body, "event: presence\ndata: {\"id\":\"\",\"land\":\"claimlayer\"}")

	// Another land, a channel that is not there, another land's channel, or no
	// land at all: the stream runs, nobody is recorded.
	for _, q := range []string{"?land=gardenio", "?land=gardenio&channel=general", "?land=claimlayer&channel=nope",
		"?land=claimlayer&channel=gd-chgen", ""} {
		body = stream(t, h, "/garden/c/events"+q, commonstest.ClaimlayerBearer, "", "event: presence")
		mustContain(t, body, ": commons")
		mustNotContain(t, body, "event: presence")
	}

	// An anonymous viewer is never recorded.
	h.Viewer = func(*http.Request) string { return "" }
	body = stream(t, h, "/garden/c/events?land=claimlayer&channel=general", commonstest.ClaimlayerBearer, "", "event: presence")
	mustNotContain(t, body, "event: presence")
}

// presenceServer is commonsServer with Presence wired.
func presenceServer(t *testing.T, d *commonstest.Daemon, p *Presence) http.Handler {
	t.Helper()
	h := &Handler{
		Beads: d, BasePath: "/base", Presence: p,
		Scope: func(r *http.Request) Scope {
			if r.Header.Get("X-Test-As") == "claimlayer" {
				return claimlayerScope
			}
			return adminScope
		},
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := commonstest.AdminBearer
		if r.Header.Get("X-Test-As") == "claimlayer" {
			bearer = commonstest.ClaimlayerBearer
		}
		mux.ServeHTTP(w, r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer)))
	})
}

// fixedAgents is an AgentSource that answers spots, or err when set.
type fixedAgents struct {
	spots map[string][]Spot
	err   error
}

func (f *fixedAgents) source(context.Context) (map[string][]Spot, error) { return f.spots, f.err }

func TestPresenceFragment(t *testing.T) {
	d := commonstest.TwoLands(t0)
	agents := &fixedAgents{spots: map[string][]Spot{
		"agent:claimlayer-crew-k1": {{Land: "claimlayer"}, {Land: "claimlayer", Channel: "cl-chgen"}},
		"agent:claimlayer-ops-x2":  {{Land: "claimlayer"}},
		"agent:gardenio-crew-z9":   {{Land: "gardenio"}, {Land: "gardenio", Channel: "gd-chgen"}},
	}}
	p := NewPresence(nil, agents.source, nil)
	p.Linger = 0
	p.PollAgents(context.Background())
	p.Join("oidc:zed@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"})
	p.Join("oidc:ann@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"})
	p.Join("oidc:bo@example.com", Spot{Land: "claimlayer", Channel: "cl-chcrew"})
	p.Join("oidc:gd-secret@example.com", Spot{Land: "gardenio", Channel: "gd-chgen"})
	h := presenceServer(t, d, p)

	code, body := get(t, h, "/garden/c/claimlayer/general/presence", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("GET presence = %d: %s", code, body)
	}
	// People first, then agents, each by name; the rest of the land is a count.
	mustContain(t, body, `aria-label="3 people in #general"`, `>2 more in claimlayer</p>`)
	order := []string{`data-who="oidc:ann@example.com"`, `data-who="oidc:zed@example.com"`, `data-who="agent:claimlayer-crew-k1"`}
	last := -1
	for _, o := range order {
		i := strings.Index(body, o)
		if i < last {
			t.Errorf("%s out of order:\n%s", o, body)
		}
		last = i
	}
	mustNotContain(t, body, "bo@example.com", "claimlayer-ops-x2", "gd-secret", "gardenio-crew-z9")

	// The same channel by id, and the golden the pane builds on.
	code, byID := get(t, h, "/garden/c/claimlayer/cl-chgen/presence", "claimlayer")
	if code != http.StatusOK || byID != body {
		t.Errorf("GET presence by id = %d, differs from by name:\n%s", code, byID)
	}
	golden(t, "presence", body)

	// Another land is not found for a scoped viewer, and a nil Presence shows
	// an empty header.
	if code, _ := get(t, h, "/garden/c/gardenio/general/presence", "claimlayer"); code != http.StatusNotFound {
		t.Errorf("scoped GET another land's presence = %d, want 404", code)
	}
	code, body = get(t, presenceServer(t, d, nil), "/garden/c/claimlayer/general/presence", "claimlayer")
	if code != http.StatusOK || !strings.Contains(body, `aria-label="0 people in #general"`) || strings.Contains(body, "cm-presence-land") {
		t.Errorf("nil Presence = %d:\n%s", code, body)
	}
}

func TestPresenceFragmentCapsAvatars(t *testing.T) {
	p := NewPresence(nil, nil, nil)
	for _, n := range "abcdefghijklmnop" {
		p.Join("oidc:"+string(n)+"@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"})
	}
	_, body := get(t, presenceServer(t, commonstest.TwoLands(t0), p), "/garden/c/claimlayer/general/presence", "claimlayer")
	if n := strings.Count(body, `data-who=`); n != presenceShown {
		t.Errorf("drew %d avatars, want %d", n, presenceShown)
	}
	mustContain(t, body, `aria-label="16 people in #general"`, `>+4</span>`)
	// The names list under the avatars names all 16, past the avatar cap.
	if n := strings.Count(body, `class="cm-who-text"`); n != 16 {
		t.Errorf("listed %d names, want 16", n)
	}
	mustNotContain(t, body, "cm-who-name--more")

	for i := 0; i < presenceListed; i++ {
		p.Join("oidc:x"+strconv.Itoa(i)+"@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"})
	}
	_, body = get(t, presenceServer(t, commonstest.TwoLands(t0), p), "/garden/c/claimlayer/general/presence", "claimlayer")
	if n := strings.Count(body, `class="cm-who-text"`); n != presenceListed {
		t.Errorf("listed %d names, want %d", n, presenceListed)
	}
	mustContain(t, body, `>and 16 more</li>`)
}

func golden(t *testing.T, name, body string) {
	t.Helper()
	file := filepath.Join("testdata", name+".golden.html")
	if *update {
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if body != string(want) {
		t.Errorf("%s drifted from %s (run with -update if meant):\n%s", name, file, body)
	}
}

func TestPollAgentsPublishesChanges(t *testing.T) {
	hub := presenceHub(t, commonstest.TwoLands(t0))
	agents := &fixedAgents{spots: map[string][]Spot{
		"agent:k1": {{Land: "claimlayer"}, {Land: "claimlayer", Channel: "cl-chcrew"}},
	}}
	p := NewPresence(hub, agents.source, nil)
	cl, stop := hub.Subscribe(context.Background(), claimlayerScope)
	defer stop()

	p.PollAgents(context.Background())
	got := map[string]bool{}
	got[next(t, cl).Channel] = true
	got[next(t, cl).Channel] = true
	if !got[""] || !got["cl-chcrew"] {
		t.Errorf("first poll published %v, want the land and #crew", got)
	}

	// The same answer is not news; a failed read keeps the last answer.
	p.PollAgents(context.Background())
	agents.err = errors.New("daemon down")
	p.PollAgents(context.Background())
	quiet(t, cl)
	if !p.Present("agent:k1", Spot{Land: "claimlayer", Channel: "cl-chcrew"}) {
		t.Error("a failed read dropped the agent")
	}

	// The agent moves to #general: both channels change, the land does not.
	agents.err = nil
	agents.spots = map[string][]Spot{"agent:k1": {{Land: "claimlayer"}, {Land: "claimlayer", Channel: "cl-chgen"}}}
	p.PollAgents(context.Background())
	got = map[string]bool{}
	got[next(t, cl).Channel] = true
	got[next(t, cl).Channel] = true
	quiet(t, cl)
	if !got["cl-chcrew"] || !got["cl-chgen"] {
		t.Errorf("move published %v, want #crew and #general", got)
	}
}

func TestAgentsSpots(t *testing.T) {
	d := commonstest.TwoLands(t0)
	agent := func(id, name, status string, labels ...string) {
		d.Put(&beadsapi.BeadDetail{ID: id, Type: "agent", Title: name, Status: status, Labels: labels,
			Fields: map[string]string{"agent": name, "role": "crew"}})
	}
	agent("cl-ag1", "claimlayer-crew-k1", "open", "project:claimlayer", "role:crew")
	agent("cl-ag2", "claimlayer-reaped", "open", "project:claimlayer")
	agent("cl-ag3", "claimlayer-idle", "open", "project:claimlayer")
	agent("cl-ag4", "claimlayer-gone", "closed", "project:claimlayer")
	agent("xx-ag5", "two-lands", "open", "project:claimlayer", "project:gardenio")
	agent("gd-ag6", "gardenio-crew-z9", "open", "project:gardenio")
	d.Roster = []beadsapi.RosterActor{
		{Actor: "claimlayer-crew-k1", IdleSecs: 3},
		{Actor: "claimlayer-reaped", Reaped: true},
		{Actor: "claimlayer-idle", IdleSecs: agentIdle.Seconds() + 1},
		{Actor: "claimlayer-gone"},
		{Actor: "two-lands"},
		{Actor: "gardenio-crew-z9", IdleSecs: 60},
		{Actor: "no-bead-at-all"},
	}
	places := 0
	a := &Agents{Daemon: d, Place: func(ctx context.Context, b *beadsapi.BeadDetail) (string, error) {
		if !beadsapi.IsSystemRead(ctx) {
			t.Error("placement not read as the system")
		}
		places++
		if b.ID == "gd-ag6" {
			return "", errors.New("no #crew in gardenio")
		}
		return "cl-chcrew", nil
	}}

	spots, err := a.Spots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]Spot{
		"agent:claimlayer-crew-k1": {{Land: "claimlayer"}, {Land: "claimlayer", Channel: "cl-chcrew"}},
		"agent:gardenio-crew-z9":   {{Land: "gardenio"}},
	}
	if len(spots) != len(want) {
		t.Fatalf("Spots = %v, want %v", spots, want)
	}
	for who, ss := range want {
		if len(spots[who]) != len(ss) {
			t.Errorf("%s at %v, want %v", who, spots[who], ss)
			continue
		}
		for i := range ss {
			if spots[who][i] != ss[i] {
				t.Errorf("%s at %v, want %v", who, spots[who], ss)
			}
		}
	}

	// Placements, failed ones too, are reused within placeTTL.
	if _, err := a.Spots(context.Background()); err != nil {
		t.Fatal(err)
	}
	if places != 2 {
		t.Errorf("placed %d times over two reads, want 2", places)
	}

	// Without Place, agents are in their land only.
	spots, _ = (&Agents{Daemon: d}).Spots(context.Background())
	if ss := spots["agent:claimlayer-crew-k1"]; len(ss) != 1 || ss[0] != (Spot{Land: "claimlayer"}) {
		t.Errorf("unplaced agent at %v", ss)
	}
}

func TestInitial(t *testing.T) {
	for in, want := range map[string]string{
		"oidc:ann@example.com": "A", "agent:9lives": "9", "agent:_x": "X", "oidc:@x": "?", "agent:—": "—",
	} {
		if got := avatarInitial(in); got != want {
			t.Errorf("avatarInitial(%q) = %q, want %q", in, got, want)
		}
	}
}
