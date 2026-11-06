package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gardenio/clicreds/oidclogin"
	"gardenio/lands/contract/landsv1"
)

// hold keeps a stream open until gl hangs up, the way a land does between
// events.
func hold(w http.ResponseWriter, r *http.Request) {
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// frame renders a golden Event fixture as the SSE frame the land sends.
func frame(t *testing.T, name string) (id, text string) {
	t.Helper()
	var ev struct {
		Id     string          `json:"id"`
		Type   string          `json:"type"`
		Object json.RawMessage `json:"object"`
	}
	if err := json.Unmarshal(fixture(t, "Event/"+name), &ev); err != nil {
		t.Fatal(err)
	}
	var compact strings.Builder
	_ = json.NewEncoder(&compact).Encode(ev.Object)
	return ev.Id, fmt.Sprintf("id: %s\nevent: %s\ndata: %s\n", ev.Id, ev.Type, strings.TrimSpace(compact.String()))
}

func TestWatch_ReconnectsFromTheLastEventWithAFreshBearer(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	id1, f1 := frame(t, "ticket.updated.json")
	id2, f2 := frame(t, "decision.opened.json")
	_, f3 := frame(t, "budget.updated.json")
	var conns atomic.Int32
	var resumedFrom, query string
	f.on("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch conns.Add(1) {
		case 1:
			query = r.URL.RawQuery
			fmt.Fprint(w, ": keep-alive\n\n"+f1+"\n"+f2+"\n")
		default:
			resumedFrom = r.Header.Get("Last-Event-ID")
			fmt.Fprint(w, f3+"\n")
			hold(w, r)
		}
	})
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{stopAfter: 3, stream: f.srv.Client()}, "--edge", f.srv.URL, "--land", "acme", "watch", "--since", "ZXY00")
	if r.e != nil {
		t.Fatalf("gl watch: %v\n%s", r.e, r.err)
	}
	if query != "" {
		t.Errorf("first connection query = %q; --since goes in Last-Event-ID", query)
	}
	if first := f.requests()[0].header.Get("Last-Event-ID"); first != "ZXY00" {
		t.Errorf("first Last-Event-ID = %q, want ZXY00", first)
	}
	if resumedFrom != id2 {
		t.Errorf("resumed from %q, want %q", resumedFrom, id2)
	}
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ticket.updated ") || !strings.HasSuffix(lines[0], "["+id1+"]") || !strings.HasPrefix(lines[2], "budget.updated  spent ") {
		t.Errorf("output:\n%s", r.out)
	}
	for _, q := range f.requests() {
		if q.header.Get("Authorization") != "Bearer tok" {
			t.Errorf("a stream request went without the land's bearer: %v", q.header)
		}
	}
}

func TestWatch_JSONIsOneContractEventPerLine(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	_, f1 := frame(t, "message.posted.json")
	f.on("GET /events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, f1+"\n")
		hold(w, r)
	})
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{stopAfter: 1, stream: f.srv.Client()}, "--edge", f.srv.URL, "--land", "acme", "--json", "watch")
	if r.e != nil {
		t.Fatalf("gl watch: %v", r.e)
	}
	var got, want landsv1.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.out)), &got); err != nil {
		t.Fatalf("not an Event: %v\n%s", err, r.out)
	}
	_ = json.Unmarshal(fixture(t, "Event/message.posted.json"), &want)
	gm, _ := got.Object.AsMessage()
	wm, _ := want.Object.AsMessage()
	if got.Id != want.Id || got.Type != want.Type || gm != wm {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestWatch_BacksOffWhenRateLimitedAndStopsOnForbidden(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	_, f1 := frame(t, "plan.updated.json")
	var conns atomic.Int32
	f.on("GET /events", func(w http.ResponseWriter, r *http.Request) {
		switch conns.Add(1) {
		case 1, 2:
			problem(w, t, 429, "Problem/rate_limited.json")
		default:
			fmt.Fprint(w, f1+"\n")
			hold(w, r)
		}
	})
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{stopAfter: 1, stream: f.srv.Client()}, "--edge", f.srv.URL, "--land", "acme", "watch")
	if r.e != nil {
		t.Fatalf("gl watch: %v\n%s", r.e, r.err)
	}
	if len(r.sleeps) != 2 || r.sleeps[0] != watchBackoffMin || r.sleeps[1] != 2*watchBackoffMin {
		t.Errorf("backoff = %v", r.sleeps)
	}
	if !strings.HasPrefix(r.out, "plan.updated ") {
		t.Errorf("output:\n%s", r.out)
	}

	f2 := newFakeLand(t, "acme")
	f2.on("GET /events", func(w http.ResponseWriter, _ *http.Request) { problem(w, t, 403, "Problem/forbidden.json") })
	r = run(t, runOpts{stream: f2.srv.Client()}, "--edge", f2.srv.URL, "--land", "acme", "watch")
	if r.e == nil || len(f2.requests()) != 1 {
		t.Errorf("forbidden: %v after %d requests; want one try and an error", r.e, len(f2.requests()))
	}
}

func TestReadEvents(t *testing.T) {
	in := ": hello\n\nid: 1\nevent: a\ndata: {\"x\":\ndata: 1}\nretry: 10\n\nevent: b\ndata: {}\n\nid: 3\n\n"
	var got []rawEvent
	n, err := readEvents(strings.NewReader(in), func(ev rawEvent) error { got = append(got, ev); return nil })
	if err != nil || n != 2 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	if got[0] != (rawEvent{id: "1", typ: "a", data: "{\"x\":\n1}"}) || got[1] != (rawEvent{typ: "b", data: "{}"}) {
		t.Errorf("events = %+v", got)
	}
}

func TestDescribeEvent_AnUnknownTypeIsOther(t *testing.T) {
	if got := describeEvent(rawEvent{typ: "land.renamed", data: "{}"}); got != "(other)" {
		t.Errorf("describeEvent = %q", got)
	}
}
