package commons

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// liveStream is a claimlayer member's stream on #general over a hub the test
// feeds, with presence; feed runs once the stream is live. The hub drops what
// it reads before the stream subscribes (gd-AkTp2mfjyz), so live is an event
// the stream wrote: on a land, the viewer's own presence, which comes after the
// stream has subscribed, joined and resolved its channel; else the opening
// comment, written once it has subscribed.
func liveStream(t *testing.T, d *commonstest.Daemon, path, until string, feed func(src liveSource, p *Presence)) string {
	t.Helper()
	src := make(liveSource, 16)
	hub := NewHub(src.open, d, nil)
	hub.Start()
	t.Cleanup(hub.Close)
	p := NewPresence(hub, nil, nil)
	p.Linger = 0
	h := &Handler{
		Beads: d, Hub: hub, Presence: p, BasePath: "/base",
		Scope:  func(*http.Request) Scope { return claimlayerScope },
		Viewer: func(*http.Request) string { return "oidc:bo@claimlayer.example" },
		Now:    func() time.Time { return t0.Add(30 * time.Minute) },
	}
	live := ": commons"
	if httptest.NewRequest("GET", path, nil).URL.Query().Get("land") != "" {
		live = "event: presence"
	}
	return streamOpened(t, h, path, commonstest.ClaimlayerBearer, "", until, live, func() { feed(src, p) })
}

// events splits a stream body into its events' data lines, by event name.
func eventsNamed(body, name string) []string {
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		if strings.Contains(block, "event: "+name+"\n") {
			out = append(out, block[strings.Index(block, "data: ")+len("data: "):])
		}
	}
	return out
}

// TestStreamCarriesMessages pins step 2 of Commons v2 (gd-RvJ6t2Nagr): a
// message in the channel on screen arrives as its row, a reply with its thread
// head, and nothing for the pane to refetch; a message elsewhere stays ids only.
func TestStreamCarriesMessages(t *testing.T) {
	d := commonstest.TwoLands(t0)
	body := liveStream(t, d, "/garden/c/events?land=claimlayer&channel=general", `"id":"cl-rep2"`, func(src liveSource, _ *Presence) {
		for _, id := range []string{"gd-msg1", "cl-crew1", "cl-msg1", "cl-rep1", "cl-rep2"} {
			src <- beadsapi.SSEEvent{Data: commonstest.Payload(d.Beads[id])}
			time.Sleep(10 * time.Millisecond) // in order
		}
	})
	msgs := eventsNamed(body, "message")
	if len(msgs) != 4 {
		t.Fatalf("want 4 message events (not gd-msg1), got %d:\n%s", len(msgs), body)
	}
	mustContain(t, msgs[0], `{"id":"cl-crew1","channel":"cl-chcrew","land":"claimlayer"}`)
	mustNotContain(t, msgs[0], `"html"`)
	mustContain(t, msgs[1], `"id":"cl-msg1"`, `"html":"<li class=\"cm-msg cm-msg--text\" id=\"m-cl-msg1\"`,
		`Claimlayer hello &lt;b&gt;there&lt;/b&gt;`, `"v":1`)
	mustNotContain(t, msgs[1], `"head"`, `"append"`, `"stale"`)
	mustContain(t, msgs[2], `"id":"cl-rep1"`, `"thread":"cl-wish1"`, `id=\"m-cl-rep1\"`, `"head":"<li class=\"cm-msg`, `id=\"m-cl-wish1\"`, `1 reply`)
	// A reply written with no channel_id is read into its root's channel, so
	// the pane takes it.
	mustContain(t, msgs[3], `"id":"cl-rep2","channel":"cl-chgen","thread":"cl-wish1"`, `id=\"m-cl-rep2\"`, `"head"`)
	mustNotContain(t, body, "gd-msg1")
	mustNotContain(t, body, commonstest.Leaks...)
}

// TestStreamAppendsParagraphs: an agent's turn written a paragraph at a time
// arrives as the paragraphs added, against the version the stream sent last;
// any other edit is the whole row.
func TestStreamAppendsParagraphs(t *testing.T) {
	d := commonstest.TwoLands(t0)
	put := func(text string) *beadsapi.BeadDetail {
		b := *d.Beads["cl-msg1"]
		b.Fields = map[string]string{"channel_id": "cl-chgen", "author": "agent:claimlayer-crew-1", "text": text}
		d.Put(&b)
		return &b
	}
	body := liveStream(t, d, "/garden/c/events?land=claimlayer&channel=general", "Rewritten", func(src liveSource, _ *Presence) {
		for _, text := range []string{"First paragraph.", "First paragraph.\n\nSecond, see https://example.com/a/b/c.", "Rewritten"} {
			src <- beadsapi.SSEEvent{Data: commonstest.Payload(put(text))}
			time.Sleep(30 * time.Millisecond)
		}
	})
	msgs := eventsNamed(body, "message")
	if len(msgs) != 3 {
		t.Fatalf("want 3 message events, got %d:\n%s", len(msgs), body)
	}
	mustContain(t, msgs[0], `"v":1`)
	mustNotContain(t, msgs[0], `"append"`)
	mustContain(t, msgs[1], `"v":2,"on":1,"append":"\n\nSecond, see <a class=\"cm-link\" href=\"https://example.com/a/b/c\"`, `"html":"<li`)
	mustNotContain(t, msgs[1], `"append":"First`)
	mustContain(t, msgs[2], `"v":3`, "Rewritten")
	mustNotContain(t, msgs[2], `"append"`, `"on"`)
}

// TestStreamCarriesTypingLine: a typing event in the channel on screen
// carries the line, and leaves the viewer out of it.
func TestStreamCarriesTypingLine(t *testing.T) {
	d := commonstest.TwoLands(t0)
	at := Spot{Land: "claimlayer", Channel: "cl-chgen"}
	body := liveStream(t, d, "/garden/c/events?land=claimlayer&channel=general", `"line":"ann is typing…"`, func(_ liveSource, p *Presence) {
		p.Join("oidc:ann@claimlayer.example", at)
		p.Join("oidc:bo@claimlayer.example", at)
		p.Type("oidc:bo@claimlayer.example", at, "")
		time.Sleep(20 * time.Millisecond)
		p.Type("oidc:ann@claimlayer.example", at, "")
	})
	lines := eventsNamed(body, "typing")
	if len(lines) != 2 {
		t.Fatalf("want 2 typing events, got %d:\n%s", len(lines), body)
	}
	mustContain(t, lines[0], `"line":""`)
	mustContain(t, lines[1], `"line":"ann is typing…"`)
}

// TestStreamCarriesNodes: a stream that names a root gets node events for it
// and for what hangs from it, read on the viewer's bearer; then for what hangs
// from those (a step's agent); never for a bead elsewhere or outside its scope.
func TestStreamCarriesNodes(t *testing.T) {
	d := commonstest.TwoLands(t0)
	cl := d.Beads["cl-msg1"].Labels
	plan := &beadsapi.BeadDetail{ID: "cl-plan1", Type: "epic", Status: "open", Title: "A plan", Labels: cl}
	step := &beadsapi.BeadDetail{ID: "cl-step1", Type: "task", Status: "in_progress", Title: "A step", Labels: cl}
	agent := &beadsapi.BeadDetail{ID: "cl-agentS", Type: "agent", Status: "open", Title: "crew", Labels: cl,
		Fields: map[string]string{"task_id": "cl-step1", "agent_state": "idle"}}
	other := &beadsapi.BeadDetail{ID: "cl-other", Type: "task", Status: "open", Title: "elsewhere", Labels: cl}
	foreign := &beadsapi.BeadDetail{ID: "gd-stepX", Type: "task", Status: "closed", Title: "gardenio step", Labels: d.Beads["gd-msg1"].Labels}
	// kbeads' payload is the model bead, whose dependencies name its parents.
	payload := func(b *beadsapi.BeadDetail) json.RawMessage {
		parent := map[string]string{"cl-step1": "cl-plan1", "gd-stepX": "cl-plan1"}[b.ID]
		if parent == "" {
			return commonstest.Payload(b)
		}
		data, _ := json.Marshal(map[string]any{"bead": map[string]any{"id": b.ID, "type": b.Type, "labels": b.Labels,
			"dependencies": []map[string]string{{"bead_id": b.ID, "depends_on_id": parent, "type": "parent-child"}}}})
		return data
	}
	for _, b := range []*beadsapi.BeadDetail{plan, step, agent, other, foreign} {
		d.Put(b)
	}
	body := liveStream(t, d, "/garden/c/events?root=cl-plan1", `"id":"cl-agentS"`, func(src liveSource, _ *Presence) {
		for _, b := range []*beadsapi.BeadDetail{agent, other, foreign, plan, step, agent} {
			src <- beadsapi.SSEEvent{Event: "beads.bead.updated", Data: payload(b)}
			time.Sleep(10 * time.Millisecond)
		}
	})
	nodes := eventsNamed(body, "node")
	if len(nodes) != 3 {
		t.Fatalf("want 3 node events (plan, step, then its agent), got %d:\n%s", len(nodes), body)
	}
	mustContain(t, nodes[0], `"node":{"id":"cl-plan1","type":"epic","status":"open","title":"A plan"}`)
	mustContain(t, nodes[1], `"node":{"id":"cl-step1","type":"task","status":"in_progress","title":"A step"}`)
	mustContain(t, nodes[2], `"node":{"id":"cl-agentS","type":"agent","status":"open","title":"crew","agent_state":"idle"}`)
	mustNotContain(t, body, "cl-other", "gd-stepX")
}

// flakyReader is a daemon whose channel lookups fail while fail is above
// zero, one failure per lookup: a slow or down kbeads.
type flakyReader struct {
	*commonstest.Daemon
	fail atomic.Int32
}

func (f *flakyReader) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.fail.Add(-1) >= 0 {
		return nil, context.DeadlineExceeded
	}
	return f.Daemon.ListBeadsFiltered(ctx, q)
}

// TestStreamEndsOnFailedChannelLookup: a stream whose channel could not be
// looked up when it opened ends, rather than carry ids only for a channel the
// pane paints from content (the pane would drop every message). The client's
// reconnect, from the ping's id, replays what was said meanwhile with its row.
func TestStreamEndsOnFailedChannelLookup(t *testing.T) {
	d := commonstest.TwoLands(t0)
	f := &flakyReader{Daemon: d}
	f.fail.Store(1)
	now := t0.Add(30 * time.Minute)
	h := &Handler{Beads: f, Scope: func(*http.Request) Scope { return claimlayerScope }, Now: func() time.Time { return now }}
	path := "/garden/c/events?land=claimlayer&channel=general"

	start := time.Now()
	body := stream(t, h, path, commonstest.ClaimlayerBearer, "", "")
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("stream kept going %v after its channel lookup failed:\n%s", took, body)
	}
	mustContain(t, body, "event: ping")
	mustNotContain(t, body, "event: message")
	id := body[strings.Index(body, "id: ")+len("id: "):]
	id = id[:strings.Index(id, "\n")]

	d.Log("beads.bead.updated", d.Beads["cl-msg1"], now) // said while the stream was down
	body = stream(t, h, path, commonstest.ClaimlayerBearer, id, "event: replayed")
	msgs := eventsNamed(body, "message")
	if len(msgs) != 1 {
		t.Fatalf("want cl-msg1 replayed on reconnect, got %d message events:\n%s", len(msgs), body)
	}
	mustContain(t, msgs[0], `"id":"cl-msg1"`, `"html":"<li class=\"cm-msg cm-msg--text\" id=\"m-cl-msg1\"`)
	mustNotContain(t, msgs[0], `"stale"`)
}

// TestCarrierRefusesUnreadableNode: a node event for a bead the daemon
// returns but the viewer's scope does not admit, or for one that cannot be
// read, is not sent at all. The hub cuts the first before a stream sees it
// (TestStreamCarriesNodes), so the carrier is asked directly.
func TestCarrierRefusesUnreadableNode(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "gd-stepX", Type: "task", Status: "closed", Title: "gardenio step", Labels: d.Beads["gd-msg1"].Labels})
	d.Put(&beadsapi.BeadDetail{ID: "cl-step1", Type: "task", Status: "open", Title: "A step", Labels: d.Beads["cl-msg1"].Labels})
	ctx := beadsapi.WithUserBearer(context.Background(), commonstest.AdminBearer) // the daemon reads both lands
	r := httptest.NewRequest("GET", "/garden/c/events?root=cl-step1", nil).WithContext(ctx)
	c := (&Handler{Beads: d}).carrier(r, claimlayerScope, nil)

	if s, ok := c.dress(ctx, Invalidation{ID: "cl-step1", Kind: kindNode}); !ok || s.Node == nil || s.Node.ID != "cl-step1" {
		t.Fatalf("a node in scope: got %+v, %v", s, ok)
	}
	for _, id := range []string{"gd-stepX", "cl-missing"} {
		if s, ok := c.dress(ctx, Invalidation{ID: id, Kind: kindNode}); ok || s.Node != nil {
			t.Errorf("%s: want no event, got %+v, %v", id, s, ok)
		}
	}
}
