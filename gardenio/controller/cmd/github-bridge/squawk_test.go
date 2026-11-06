package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/githubapp"
)

type fakeGH struct {
	mu        sync.Mutex
	existing  []string // bodies already on the PR
	posts     []string
	posted    []prRef
	hasErr    error
	postErr   error
	hasMarker int
}

func (g *fakeGH) HasMarker(_ context.Context, _ prRef, marker string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hasMarker++
	if g.hasErr != nil {
		return false, g.hasErr
	}
	for _, b := range append(append([]string{}, g.existing...), g.posts...) {
		if strings.Contains(b, marker) {
			return true, nil
		}
	}
	return false, nil
}

func (g *fakeGH) Post(_ context.Context, pr prRef, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.postErr != nil {
		return g.postErr
	}
	g.posts = append(g.posts, body)
	g.posted = append(g.posted, pr)
	return nil
}

type squawkDaemon struct {
	*fakeDaemon
	agentErr error
}

func (s squawkDaemon) FindAgentBead(_ context.Context, name string) (*beadsapi.BeadDetail, error) {
	if s.agentErr != nil {
		return nil, s.agentErr
	}
	if name != "crew-pr-events" {
		return nil, beadsapi.ErrAgentNotFound
	}
	return s.agents[0], nil
}

func squawkEvent(t *testing.T, id string, labels []string, text string) []byte {
	t.Helper()
	fields, _ := json.Marshal(map[string]string{"source_agent": "crew-pr-events", "text": text})
	raw, err := json.Marshal(map[string]any{"bead": map[string]any{
		"id": id, "type": "message", "title": text, "created_by": "crew-pr-events",
		"labels": labels, "fields": json.RawMessage(fields),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var toMR = []string{"squawk", "from:crew-pr-events", "to-mr"}

func newPoster(t *testing.T, taskFields map[string]string) (*squawkPoster, *fakeGH, squawkDaemon) {
	t.Helper()
	d := squawkDaemon{fakeDaemon: daemonWithTask(taskFields)}
	gh := &fakeGH{}
	return newSquawkPoster(d, gh, quiet()), gh, d
}

func TestSquawk_PostsOptInSquawkWithMarkerAndAgentPrefix(t *testing.T) {
	p, gh, _ := newPoster(t, map[string]string{"mr_url": ghPRURL})
	p.handleClosed(context.Background(), squawkEvent(t, "sq-1", toMR, "Rebased and pushed; CI is running."))
	if len(gh.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(gh.posts))
	}
	body := gh.posts[0]
	if !strings.HasPrefix(body, "**gardenio agent** `crew-pr-events`:\n\nRebased and pushed; CI is running.") ||
		!strings.Contains(body, squawkMarkerPrefix) || !strings.HasSuffix(body, " -->") {
		t.Errorf("body = %q", body)
	}
	if gh.posted[0] != (prRef{Repo: "joyousaversion/gardenio", Number: 42}) {
		t.Errorf("posted to %+v", gh.posted[0])
	}
}

func TestSquawk_PlainProgressSquawkStaysSlackOnly(t *testing.T) {
	p, gh, _ := newPoster(t, map[string]string{"mr_url": ghPRURL})
	p.handleClosed(context.Background(), squawkEvent(t, "sq-2", []string{"squawk", "from:crew-pr-events"}, "working on it"))
	if len(gh.posts) != 0 || gh.hasMarker != 0 {
		t.Errorf("posts=%d markerChecks=%d, want the PR untouched", len(gh.posts), gh.hasMarker)
	}
}

func TestSquawk_DedupeSameBeadReplayedAfterSSEReconnect(t *testing.T) {
	p, gh, _ := newPoster(t, map[string]string{"mr_url": ghPRURL})
	ev := squawkEvent(t, "sq-3", toMR, "done")
	p.handleClosed(context.Background(), ev)
	p.handleClosed(context.Background(), ev)
	if len(gh.posts) != 1 {
		t.Errorf("posts = %d, want 1", len(gh.posts))
	}
}

// After a restart the in-memory set is empty; the marker already on the PR is
// what prevents the second post.
func TestSquawk_DedupeAcrossRestartViaMarkerOnPR(t *testing.T) {
	p1, gh, d := newPoster(t, map[string]string{"mr_url": ghPRURL})
	p1.handleClosed(context.Background(), squawkEvent(t, "sq-4", toMR, "same words"))
	if len(gh.posts) != 1 {
		t.Fatal("first post missing")
	}
	p2 := newSquawkPoster(d, gh, quiet()) // fresh process, same PR
	p2.handleClosed(context.Background(), squawkEvent(t, "sq-4", toMR, "same words"))
	if len(gh.posts) != 1 {
		t.Errorf("posts = %d after restart, want 1", len(gh.posts))
	}
}

func TestSquawk_DedupeIdenticalTextFromDistinctBeads(t *testing.T) {
	p, gh, _ := newPoster(t, map[string]string{"mr_url": ghPRURL})
	p.handleClosed(context.Background(), squawkEvent(t, "sq-5", toMR, "CI is red, investigating"))
	p.handleClosed(context.Background(), squawkEvent(t, "sq-6", toMR, "CI is red, investigating"))
	p.handleClosed(context.Background(), squawkEvent(t, "sq-7", toMR, "CI is green now"))
	if len(gh.posts) != 2 {
		t.Errorf("posts = %d, want 2 (a looping agent's repeat is dropped, new text is not)", len(gh.posts))
	}
}

func TestSquawk_SkipsAgentsWithoutAGitHubPR(t *testing.T) {
	cases := map[string]map[string]string{
		"no mr_url":     {},
		"a GitLab MR":   {"mr_url": "https://gitlab.example.com/g/p/-/merge_requests/9"},
		"not a PR link": {"mr_url": "https://github.com/joyousaversion/gardenio/issues/42"},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			p, gh, _ := newPoster(t, f)
			p.handleClosed(context.Background(), squawkEvent(t, "sq-8", toMR, "hello"))
			if len(gh.posts) != 0 {
				t.Errorf("posted %v", gh.posts)
			}
		})
	}
}

func TestSquawk_AgentBeadMrURLWinsOverTask(t *testing.T) {
	p, gh, d := newPoster(t, map[string]string{"mr_url": "https://github.com/joyousaversion/gardenio/pull/1"})
	d.agents[0].Fields["mr_url"] = ghPRURL
	p.handleClosed(context.Background(), squawkEvent(t, "sq-9", toMR, "hi"))
	if len(gh.posted) != 1 || gh.posted[0].Number != 42 {
		t.Errorf("posted = %+v, want PR 42", gh.posted)
	}
}

func TestSquawk_UnknownAgentIsSkippedSilently(t *testing.T) {
	d := squawkDaemon{fakeDaemon: daemonWithTask(map[string]string{"mr_url": ghPRURL})}
	gh := &fakeGH{}
	ev, _ := json.Marshal(map[string]any{"bead": map[string]any{
		"id": "sq-10", "type": "message", "labels": toMR, "created_by": "someone-else",
		"fields": map[string]string{"text": "hi", "source_agent": "someone-else"},
	}})
	newSquawkPoster(d, gh, quiet()).handleClosed(context.Background(), ev)
	if len(gh.posts) != 0 {
		t.Errorf("posted for an unknown agent: %v", gh.posts)
	}
}

func TestSquawk_FailedPostIsRetriedOnReplay(t *testing.T) {
	p, gh, _ := newPoster(t, map[string]string{"mr_url": ghPRURL})
	ev := squawkEvent(t, "sq-11", toMR, "try again")
	gh.postErr = errors.New("502")
	p.handleClosed(context.Background(), ev)
	gh.postErr = nil
	p.handleClosed(context.Background(), ev)
	if len(gh.posts) != 1 {
		t.Errorf("posts = %d, want the replay to succeed once", len(gh.posts))
	}
}

func TestSquawk_LongTextIsCapped(t *testing.T) {
	p, gh, _ := newPoster(t, map[string]string{"mr_url": ghPRURL})
	p.handleClosed(context.Background(), squawkEvent(t, "sq-12", toMR, strings.Repeat("y", 10000)))
	if len(gh.posts) != 1 || strings.Count(gh.posts[0], "y") > squawkMaxLen {
		t.Fatalf("posts=%d", len(gh.posts))
	}
}

func TestParsePRURL(t *testing.T) {
	ok := map[string]prRef{
		"https://github.com/o/r/pull/7":         {"o/r", 7},
		"https://github.com/o/r/pull/7/files":   {"o/r", 7},
		"https://ghe.example.com/o/r/pull/12#x": {"o/r", 12},
	}
	for u, want := range ok {
		if got, found := parsePRURL(u); !found || got != want {
			t.Errorf("parsePRURL(%q) = %+v %v, want %+v", u, got, found, want)
		}
	}
	for _, u := range []string{"", "https://gitlab.com/g/p/-/merge_requests/3", "https://github.com/o/r/issues/3", "https://github.com/o/r/pull/x"} {
		if _, found := parsePRURL(u); found {
			t.Errorf("parsePRURL(%q) matched", u)
		}
	}
}

// githubCommenter against a stub API: marker scan paginates, posts carry the
// per-repo installation token.
type stubTokens struct{}

func (stubTokens) Token(_ context.Context, repo string) (githubapp.Token, error) {
	return githubapp.NewToken("tok-"+repo, time.Now().Add(time.Hour)), nil
}

func TestGitHubCommenter_MarkerScanPaginatesAndPostUsesToken(t *testing.T) {
	var pages []string
	var posted map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-o/r" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/5/comments":
			pages = append(pages, r.URL.Query().Get("page"))
			var out []map[string]string
			if r.URL.Query().Get("page") == "1" {
				for range commentsPerPage {
					out = append(out, map[string]string{"body": "filler"})
				}
			} else {
				out = []map[string]string{{"body": "x <!-- gardenio-squawk:abc -->"}}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/5/comments":
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &githubCommenter{tokens: stubTokens{}, baseURL: srv.URL, hc: srv.Client()}
	pr := prRef{Repo: "o/r", Number: 5}

	found, err := c.HasMarker(context.Background(), pr, "<!-- gardenio-squawk:abc -->")
	if err != nil || !found || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("found=%v err=%v pages=%v", found, err, pages)
	}
	if found, _ := c.HasMarker(context.Background(), pr, "<!-- gardenio-squawk:zzz -->"); found {
		t.Error("matched a marker that is not there")
	}
	if err := c.Post(context.Background(), pr, "hello"); err != nil || posted["body"] != "hello" {
		t.Errorf("post err=%v body=%v", err, posted)
	}
}

func TestGitHubCommenter_ErrorStatusSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()
	c := &githubCommenter{tokens: stubTokens{}, baseURL: srv.URL, hc: srv.Client()}
	err := c.Post(context.Background(), prRef{Repo: "o/r", Number: 1}, "x")
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("err = %v", err)
	}
}

// Run reconnects after the stream drops and feeds closed-bead events through.
type fakeStream struct {
	mu     sync.Mutex
	opens  int
	events []beadsapi.SSEEvent
}

func (f *fakeStream) EventStream(ctx context.Context, _ string) (<-chan beadsapi.SSEEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	ch := make(chan beadsapi.SSEEvent, len(f.events))
	if f.opens == 1 {
		for _, e := range f.events {
			ch <- e
		}
	}
	close(ch)
	return ch, nil
}

func TestSquawk_RunConsumesStream(t *testing.T) {
	p, gh, _ := newPoster(t, map[string]string{"mr_url": ghPRURL})
	st := &fakeStream{events: []beadsapi.SSEEvent{
		{Event: "beads.bead.created", Data: squawkEvent(t, "sq-20", toMR, "ignored: wrong topic")},
		{Event: squawkTopic, Data: squawkEvent(t, "sq-21", toMR, "from the stream")},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, st); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		gh.mu.Lock()
		n := len(gh.posts)
		gh.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("squawk from the stream was never posted")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if len(gh.posts) != 1 || !strings.Contains(gh.posts[0], "from the stream") {
		t.Errorf("posts = %v", gh.posts)
	}
}
