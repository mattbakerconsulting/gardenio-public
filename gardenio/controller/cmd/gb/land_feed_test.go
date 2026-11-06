package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// landFeedStub serves a land door at /land/acme: the daemon routes of a
// yieldDaemonStub under it, the land feed, and the gate route, which it
// counts. Each feed connection takes the next entry of streams, writes its
// frames and ends (as the server does at the 5-minute lifetime); past the
// last entry the stream stays open until the client goes.
type landFeedStub struct {
	daemon *yieldDaemonStub

	mu          sync.Mutex
	streams     [][]string // raw SSE frames per connection
	lastEventID []string   // Last-Event-ID of each connection
	auth        []string   // Authorization of each connection
	gateCalls   int
}

func (s *landFeedStub) handler() http.Handler {
	inner := http.StripPrefix("/land/acme", s.daemon.handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/land/acme/v1/lands/v1/lands/acme/events":
			s.mu.Lock()
			n := len(s.lastEventID)
			s.lastEventID = append(s.lastEventID, r.Header.Get("Last-Event-ID"))
			s.auth = append(s.auth, r.Header.Get("Authorization"))
			var frames []string
			if n < len(s.streams) {
				frames = s.streams[n]
			}
			last := n >= len(s.streams)-1
			s.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(":keepalive\n\n"))
			for _, f := range frames {
				_, _ = w.Write([]byte(f))
			}
			w.(http.Flusher).Flush()
			if last {
				<-r.Context().Done()
			}
		case strings.Contains(r.URL.Path, "/gates/"):
			s.mu.Lock()
			s.gateCalls++
			s.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
		default:
			inner.ServeHTTP(w, r)
		}
	})
}

func landFrame(id, typ string, data map[string]any) string {
	b, _ := json.Marshal(data)
	return "id: " + id + "\nevent: " + typ + "\ndata: " + string(b) + "\n\n"
}

func setupLandFeed(t *testing.T, s *landFeedStub) {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL + "/land/acme"})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon, oldToken, oldBackoff := daemon, tokenFlag, landFeedBackoff
	daemon, tokenFlag, landFeedBackoff = c, "land-tok", time.Millisecond
	t.Cleanup(func() { daemon, tokenFlag, landFeedBackoff = oldDaemon, oldToken, oldBackoff })
}

func TestLandDoor(t *testing.T) {
	old := daemon
	t.Cleanup(func() { daemon = old })
	for addr, want := range map[string]string{
		"https://api.example.test/land/acme": "acme",
		"https://api.example.test":           "",
		"http://localhost:8080":              "",
	} {
		c, err := beadsapi.New(beadsapi.Config{HTTPAddr: addr})
		if err != nil {
			t.Fatal(err)
		}
		daemon = c
		if got := landDoor(); got != want {
			t.Errorf("landDoor(%s) = %q, want %q", addr, got, want)
		}
	}
}

func TestReadLandFeed_FramesAndCursor(t *testing.T) {
	body := ":keepalive\n\n" +
		landFrame("c1", "ticket.updated", map[string]any{"id": "gd-1"}) +
		"event: plan.updated\ndata: {\"id\":\"gd-2\"}\n\n" // no id: cursor stays c1
	ch := make(chan landEvent, 4)
	cursor := readLandFeed(context.Background(), strings.NewReader(body), "c0", ch)
	close(ch)
	if cursor != "c1" {
		t.Errorf("cursor = %q, want c1", cursor)
	}
	var got []string
	for e := range ch {
		var rec landRecord
		_ = json.Unmarshal(e.Data, &rec)
		got = append(got, e.Type+" "+rec.ID)
	}
	if strings.Join(got, ",") != "ticket.updated gd-1,plan.updated gd-2" {
		t.Errorf("events = %v", got)
	}
}

// A decision resolved after the first stream ends reaches yield on the
// reconnect, which resumes from the cursor with a fresh bearer.
func TestYieldLandFeed_ResolvesAcrossReconnect(t *testing.T) {
	s := &landFeedStub{
		daemon: &yieldDaemonStub{beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "closed", map[string]any{"chosen": "a"}),
		}},
		streams: [][]string{
			{landFrame("c1", "ticket.updated", map[string]any{"id": "gd-other", "kind": "task", "status": "open", "assignee": "someone-else"})},
			{landFrame("c2", "decision.resolved", map[string]any{"id": "gd-dec", "status": "resolved"})},
		},
	}
	setupLandFeed(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := captureStdout(t, func() error {
		outcome := yieldSSE(ctx, []*beadsapi.BeadDetail{{ID: "gd-dec", Type: "decision"}}, "agent-me", "agent-me", time.Time{})
		if !outcome.resolved {
			t.Errorf("want resolved, got %+v", outcome)
		}
		return nil
	})
	if !strings.Contains(string(out), "gd-dec resolved") {
		t.Errorf("output = %q", out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lastEventID) != 2 || s.lastEventID[0] != "" || s.lastEventID[1] != "c1" {
		t.Errorf("Last-Event-ID per connection = %q, want [\"\" c1]", s.lastEventID)
	}
	for i, a := range s.auth {
		if a != "Bearer land-tok" {
			t.Errorf("connection %d Authorization = %q", i, a)
		}
	}
}

func TestYieldLandFeed_AssignedTicketInterrupts(t *testing.T) {
	s := &landFeedStub{
		daemon: &yieldDaemonStub{beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "open", nil),
		}},
		streams: [][]string{{
			landFrame("c1", "ticket.updated", map[string]any{"id": "gd-mine", "kind": "task", "status": "in_progress", "assignee": "agent-me"}),
			landFrame("c2", "ticket.updated", map[string]any{"id": "gd-new", "kind": "bug", "status": "open", "assignee": "agent-me"}),
		}},
	}
	setupLandFeed(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := captureStdout(t, func() error {
		outcome := yieldSSE(ctx, []*beadsapi.BeadDetail{{ID: "gd-dec", Type: "decision"}}, "oidc:agent-me@example.test", "", time.Time{})
		if outcome.resolved || ctx.Err() != nil {
			t.Errorf("want an interrupt before the deadline, got %+v (ctx %v)", outcome, ctx.Err())
		}
		return nil
	})
	if !strings.Contains(string(out), "Work assigned: gd-new (type=bug)") {
		t.Errorf("output = %q", out)
	}
}

func TestWaitDecisionLandFeed(t *testing.T) {
	s := &landFeedStub{
		daemon: &yieldDaemonStub{beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "closed", map[string]any{"chosen": "b"}),
		}},
		streams: [][]string{{
			landFrame("c1", "decision.resolved", map[string]any{"id": "gd-other"}),
			landFrame("c2", "decision.resolved", map[string]any{"id": "gd-dec"}),
		}},
	}
	setupLandFeed(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := captureStdout(t, func() error { return waitDecisionLandFeed(ctx, "acme", "gd-dec") })
	if ctx.Err() != nil || !strings.Contains(string(out), "gd-dec") {
		t.Errorf("wait did not end on gd-dec's decision.resolved: ctx %v, output %q", ctx.Err(), out)
	}
}

func TestSatisfyStopGate_SkippedOnLandDoor(t *testing.T) {
	s := &landFeedStub{daemon: &yieldDaemonStub{}}
	setupLandFeed(t, s)
	satisfyStopGate(context.Background(), "gd-agent")
	satisfyGateOnReady(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gateCalls != 0 {
		t.Errorf("gate route called %d times through a land door", s.gateCalls)
	}
}

func TestLandDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"agent-me":                   "agent-me",
		"oidc:alice@example.test":    "alice",
		"bob@example.test":           "bob",
		" gardenio-d18b-gb-yield ":   "gardenio-d18b-gb-yield",
		"weird name:with colon@x.io": "weird name:with colon",
	} {
		if got := landDisplayName(in); got != want {
			t.Errorf("landDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
