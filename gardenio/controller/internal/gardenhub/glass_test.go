package gardenhub

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
)

// glassMux is a fake coopmux behind the relay: every dial gets an upstream that answers replay:get with the session's
// bytes so far and can be fed live pty frames.
type glassMux struct {
	dials   atomic.Int32
	mu      sync.Mutex
	ups     []*glassUp
	signers []string
}

type glassUp struct {
	ctx  context.Context
	in   chan coopmuxapi.Frame
	once sync.Once
	done chan struct{}
}

func (m *glassMux) dial(ctx context.Context, session string, _ []string) (glass.Upstream, error) {
	m.dials.Add(1)
	if session == "gone-session" {
		return nil, coopmuxapi.ErrSessionNotFound
	}
	id, _ := coopmuxapi.IdentityFromContext(ctx)
	u := &glassUp{ctx: ctx, in: make(chan coopmuxapi.Frame, 64), done: make(chan struct{})}
	m.mu.Lock()
	m.ups = append(m.ups, u)
	m.signers = append(m.signers, id.Operator)
	m.mu.Unlock()
	return u, nil
}

func (u *glassUp) Recv() (coopmuxapi.Frame, error) {
	select {
	case f := <-u.in:
		return f, nil
	case <-u.ctx.Done():
		return coopmuxapi.Frame{}, u.ctx.Err()
	case <-u.done:
		return coopmuxapi.Frame{}, io.EOF
	}
}

func (u *glassUp) Send(frame any) error {
	b, _ := json.Marshal(frame)
	var m struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(b, &m)
	if m.Event == "replay:get" {
		u.emit(`{"event":"replay","data":"` + base64.StdEncoding.EncodeToString([]byte("$ ")) + `","offset":0,"next_offset":2,"total_written":2}`)
	}
	return nil
}

func (u *glassUp) Close() error { u.once.Do(func() { close(u.done) }); return nil }

func (u *glassUp) emit(raw string) {
	var tag struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal([]byte(raw), &tag)
	u.in <- coopmuxapi.Frame{Event: tag.Event, Data: json.RawMessage(raw)}
}

type glassScreens struct{ missing bool }

func (s glassScreens) GetScreen(_ context.Context, id string) (*coopmuxapi.Screen, error) {
	if s.missing {
		return nil, coopmuxapi.ErrSessionNotFound
	}
	return &coopmuxapi.Screen{Lines: []string{"$ make test", "ok"}, Cols: 80, Rows: 24, Seq: 9}, nil
}

// who reads the test's identity off two headers, as the viewer reads it off the OIDC context.
func glassWho(r *http.Request) coopmuxapi.Identity {
	return coopmuxapi.Identity{Operator: r.Header.Get("X-Test-Email"), AuthMode: "cookie", Roles: strings.Split(r.Header.Get("X-Test-Roles"), ",")}
}

func glassHandler(mux *glassMux, screens GlassScreens) *Handler {
	beads := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{
		"gd-agent1": {ID: "gd-agent1", Type: "agent", Title: "crew-one", Labels: []string{"project:gardenio", "role:crew"},
			Notes: "pod_name: gardenio-crew-one-abc\n",
			Fields: map[string]string{"agent": "crew-one", "project": "gardenio", "role": "crew", "task_id": "gd-task1",
				"agent_state": "working", "last_session_id": "sess-1", "account": "acct-a", "live_cost_usd": "1.234", "context_tokens": "48000"}},
		"gd-other": {ID: "gd-other", Type: "agent", Title: "tenant-crew", Labels: []string{"project:claimlayer"},
			Fields: map[string]string{"agent": "tenant-crew", "project": "claimlayer", "last_session_id": "sess-2"}},
		"gd-idle":  {ID: "gd-idle", Type: "agent", Title: "no-session", Fields: map[string]string{"project": "gardenio"}},
		"gd-gone":  {ID: "gd-gone", Type: "agent", Fields: map[string]string{"project": "gardenio", "last_session_id": "gone-session"}},
		"gd-task1": {ID: "gd-task1", Type: "task", Title: "6.3 Glass relay"},
		"gd-plain": {ID: "gd-plain", Type: "task", Title: "not an agent", Fields: map[string]string{"project": "gardenio"}},
	}}
	relay := glass.NewRelay(mux.dial, nil)
	return &Handler{BasePath: "/v", Beads: beads, Glass: &Glass{
		Relay: relay, Screens: screens, Who: glassWho,
		// the test's scoped viewer holds only gardenio; X-Test-Admin is an unrestricted one
		InScope: func(r *http.Request, project string) bool {
			return r.Header.Get("X-Test-Admin") != "" || project == "gardenio"
		},
	}}
}

func glassGet(t *testing.T, h *Handler, path, roles string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	m := http.NewServeMux()
	h.Register(m)
	// a served stream runs until its request ends
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	req.Header.Set("X-Test-Email", "op@example.com")
	req.Header.Set("X-Test-Roles", roles)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec
}

func TestGlassViewHeader(t *testing.T) {
	h := glassHandler(&glassMux{}, glassScreens{})
	h.Glass.Nudge = func(context.Context, string, string) NudgeOutcome { return NudgeDelivered }
	h.Glass.Prompts = &glassCoop{}
	rec := glassGet(t, h, "/garden/glass/gd-agent1", "gardenio-operator")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var d GlassDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	want := GlassDoc{ID: "gd-agent1", Agent: "crew-one", Project: "gardenio", Role: "crew", Task: "gd-task1", TaskTitle: "6.3 Glass relay",
		Pod: "gardenio-crew-one-abc", Account: "acct-a", Cost: "$1.23", Context: 48000, State: "working", Live: true,
		Stream: "/v/garden/glass/gd-agent1/stream", Watch: "operators", CanNudge: true, CanRespond: true, You: "op@example.com"}
	got := d
	got.Screen = nil
	if got != want {
		t.Fatalf("doc =\n %+v\nwant\n %+v", got, want)
	}
	if d.Screen == nil || d.Screen.Cols != 80 || len(d.Screen.Lines) != 2 || d.Screen.Lines[0] != "$ make test" {
		t.Fatalf("screen = %+v, want the cached first paint", d.Screen)
	}
	if strings.Contains(rec.Body.String(), "sess-1") {
		t.Fatal("the session id reached the browser; the stream is addressed by agent")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("glass doc is cacheable")
	}
}

// glassStateCoop is a coop whose state read fails, or stalls until the caller gives up.
type glassStateCoop struct {
	glassCoop
	err   error
	stall bool
	asked atomic.Int32
}

func (c *glassStateCoop) GetAgentState(ctx context.Context, session string) (*coopmuxapi.AgentState, error) {
	c.asked.Add(1)
	if c.stall {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.glassCoop.GetAgentState(ctx, session)
}

// The doc's state is coop's for a live agent, so a viewer who may watch but not respond (no prompt read, no stream: no
// relay key, 6.3b) still sees Prompt; coop failing or slow leaves the bead's agent_state (gd-vWfjr8cTVq).
func TestGlassViewStateFromCoop(t *testing.T) {
	for _, tc := range []struct {
		name string
		coop *glassStateCoop
		want string
	}{
		{"parked at a prompt", &glassStateCoop{glassCoop: glassCoop{state: bashPrompt()}}, "prompt"},
		{"idle", &glassStateCoop{glassCoop: glassCoop{state: &coopmuxapi.AgentState{State: "idle"}}}, "idle"},
		{"no state", &glassStateCoop{glassCoop: glassCoop{state: &coopmuxapi.AgentState{}}}, "working"},
		{"coop errs", &glassStateCoop{err: coopmuxapi.ErrDisabled}, "working"},
		{"coop stalls", &glassStateCoop{stall: true}, "working"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := glassHandler(&glassMux{}, glassScreens{})
			h.Glass.Prompts = tc.coop
			h.ReadOnly = func(*http.Request) bool { return true } // watch-only: the Glass offers no answers
			m := http.NewServeMux()
			h.Register(m)
			// a request that outlives glassStateTimeout, so a stalled coop is cut off by it and not by the request
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/garden/glass/gd-agent1", nil).WithContext(ctx)
			req.Header.Set("X-Test-Email", "op@example.com")
			req.Header.Set("X-Test-Roles", "gardenio-operator")
			rec := httptest.NewRecorder()
			start := time.Now()
			m.ServeHTTP(rec, req)
			var d GlassDoc
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if d.State != tc.want || !d.Live || d.CanRespond || d.Screen == nil {
				t.Fatalf("state %q live %v can_respond %v screen %v, want state %q, live, no answers, the screen", d.State, d.Live, d.CanRespond, d.Screen != nil, tc.want)
			}
			if tc.coop.asked.Load() != 1 {
				t.Fatalf("coop asked %d times, want once", tc.coop.asked.Load())
			}
			if el := time.Since(start); el > 2*glassStateTimeout {
				t.Fatalf("the doc took %v; a slow coop must not hold the first paint past %v", el, glassStateTimeout)
			}
		})
	}
	// coopmux lost the session: the doc is not live and coop's state is not asked for
	h := glassHandler(&glassMux{}, glassScreens{missing: true})
	c := &glassStateCoop{glassCoop: glassCoop{state: bashPrompt()}}
	h.Glass.Prompts = c
	var d GlassDoc
	if err := json.Unmarshal(glassGet(t, h, "/garden/glass/gd-agent1", "gardenio-operator").Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Live || d.State != "working" || c.asked.Load() != 0 {
		t.Fatalf("live %v state %q asked %d, want not live, the bead's state, coop not asked", d.Live, d.State, c.asked.Load())
	}
}

func TestGlassViewNotLive(t *testing.T) {
	h := glassHandler(&glassMux{}, glassScreens{missing: true})
	for _, id := range []string{"gd-agent1", "gd-idle"} { // coopmux lost the session; the agent never had one
		var d GlassDoc
		rec := glassGet(t, h, "/garden/glass/"+id, "gardenio-operator")
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", id, rec.Code, rec.Body)
		}
		if d.Live || d.Stream != "" || d.Screen != nil {
			t.Fatalf("%s: %+v, want not live, no stream, no screen", id, d)
		}
	}
}

// The scope and tier checks, on every read route: an agent outside the viewer's land answers 404 exactly as a missing
// one does (ids do not leak); one in scope answers 403 below the operator tier (owner Q8d: watch is operator-only).
func TestGlassScopeAndTier(t *testing.T) {
	mux := &glassMux{}
	h := glassHandler(mux, glassScreens{})
	for _, route := range []string{"", "/stream"} {
		cases := []struct {
			name, agent, roles string
			hdr                []string
			want               int
		}{
			{"out-of-scope agent", "gd-other", "gardenio-admin", nil, http.StatusNotFound},
			{"missing agent", "gd-nope", "gardenio-admin", nil, http.StatusNotFound},
			{"not an agent", "gd-plain", "gardenio-admin", nil, http.StatusNotFound},
			{"viewer tier", "gd-agent1", "gardenio-viewer", nil, http.StatusForbidden},
			{"project member, no rank", "gd-agent1", "gardenio-project-member", nil, http.StatusForbidden},
			{"no roles", "gd-agent1", "", nil, http.StatusForbidden},
			{"out of scope and below tier", "gd-other", "gardenio-viewer", nil, http.StatusNotFound},
			{"unrestricted operator, other land", "gd-other", "gardenio-operator", []string{"X-Test-Admin", "1"}, 0},
		}
		for _, c := range cases {
			rec := glassGet(t, h, "/garden/glass/"+c.agent+route, c.roles, c.hdr...)
			if c.want == 0 {
				if rec.Code == http.StatusNotFound || rec.Code == http.StatusForbidden {
					t.Errorf("%s%s: %d, want it served", c.name, route, rec.Code)
				}
				continue
			}
			if rec.Code != c.want {
				t.Errorf("%s%s: %d, want %d", c.name, route, rec.Code, c.want)
			}
			if c.want == http.StatusNotFound && !strings.Contains(rec.Body.String(), "no such agent") {
				t.Errorf("%s%s: body %q tells a hidden agent from a missing one", c.name, route, rec.Body)
			}
		}
	}
	mux.mu.Lock()
	defer mux.mu.Unlock()
	for _, s := range mux.signers {
		if s != "op@example.com" {
			t.Fatalf("an upstream was signed for %q", s)
		}
	}
	if h.Glass.Relay.Upstreams() > 1 {
		t.Fatalf("refused requests opened upstreams: %d", h.Glass.Relay.Upstreams())
	}
}

func TestGlassStreamNoSession(t *testing.T) {
	h := glassHandler(&glassMux{}, glassScreens{})
	if rec := glassGet(t, h, "/garden/glass/gd-idle/stream", "gardenio-operator"); rec.Code != http.StatusNotFound {
		t.Fatalf("no session: %d, want 404", rec.Code)
	}
}

func TestGlassDisabled(t *testing.T) {
	h := &Handler{Beads: &fakeBeads{}}
	if rec := glassGet(t, h, "/garden/glass/gd-agent1", "gardenio-admin"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no glass: %d, want 503", rec.Code)
	}
}

// sseEvent is one parsed SSE event.
type sseEvent struct {
	name, id, data string
}

// readSSE parses events off a live stream onto ch until it closes.
func readSSE(body io.Reader, ch chan<- sseEvent) {
	defer close(ch)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var ev sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.name != "" {
				ch <- ev
			}
			ev = sseEvent{}
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			ev.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func openGlassStream(t *testing.T, srv *httptest.Server, agent, lastID string) (<-chan sseEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/garden/glass/"+agent+"/stream", nil)
	req.Header.Set("X-Test-Email", "op@example.com")
	req.Header.Set("X-Test-Roles", "gardenio-operator")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ch := make(chan sseEvent, 64)
	go readSSE(resp.Body, ch)
	return ch, func() { cancel(); resp.Body.Close() }
}

func nextEvent(t *testing.T, ch <-chan sseEvent, name string) sseEvent {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before a %q event", name)
			}
			if ev.name == name {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %q event", name)
		}
	}
}

func ptyText(t *testing.T, ev sseEvent) string {
	t.Helper()
	var m struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(ev.data), &m); err != nil {
		t.Fatal(err)
	}
	b, _ := base64.StdEncoding.DecodeString(m.Data)
	return string(b)
}

// Two browsers watching one agent ride one upstream; a reconnect with Last-Event-ID gets only what it missed.
func TestGlassStreamSharedAndResumed(t *testing.T) {
	mux := &glassMux{}
	h := glassHandler(mux, glassScreens{})
	m := http.NewServeMux()
	h.Register(m)
	srv := httptest.NewServer(m)
	defer srv.Close()

	a, closeA := openGlassStream(t, srv, "gd-agent1", "")
	defer closeA()
	if ev := nextEvent(t, a, "replay"); ptyText(t, ev) != "$ " || ev.id != "2" {
		t.Fatalf("first watcher opened with %+v, want the replay to offset 2", ev)
	}
	b, closeB := openGlassStream(t, srv, "gd-agent1", "")
	defer closeB()
	if ev := nextEvent(t, b, "replay"); ptyText(t, ev) != "$ " {
		t.Fatalf("second watcher opened with %+v", ev)
	}
	if n := mux.dials.Load(); n != 1 {
		t.Fatalf("dials = %d, want one shared upstream for two watchers", n)
	}

	mux.mu.Lock()
	up := mux.ups[0]
	mux.mu.Unlock()
	up.emit(`{"event":"pty","data":"` + base64.StdEncoding.EncodeToString([]byte("ls\r\n")) + `","offset":2}`)
	up.emit(`{"event":"transition","prev":"working","next":"prompt","seq":5,"prompt":{"type":"permission","options":["Yes","No"]}}`)
	for _, ch := range []<-chan sseEvent{a, b} {
		if ev := nextEvent(t, ch, "pty"); ptyText(t, ev) != "ls\r\n" || ev.id != "6" {
			t.Fatalf("pty = %+v, want ls at id 6", ev)
		}
		var st glass.State
		if err := json.Unmarshal([]byte(nextEvent(t, ch, "state").data), &st); err != nil || st.State != "prompt" || !st.Attention {
			t.Fatalf("state = %+v (%v), want an attention-pulsing prompt", st, err)
		}
	}

	closeB()
	resumed, closeR := openGlassStream(t, srv, "gd-agent1", "2")
	defer closeR()
	if ev := nextEvent(t, resumed, "pty"); ptyText(t, ev) != "ls\r\n" || ev.id != "6" {
		t.Fatalf("resume from 2 = %+v, want only the bytes after it", ev)
	}
	if n := mux.dials.Load(); n != 1 {
		t.Fatalf("dials = %d after a reconnect, want the upstream reused", n)
	}
}

func TestGlassStreamEndsWhenSessionGone(t *testing.T) {
	mux := &glassMux{}
	h := glassHandler(mux, glassScreens{})
	m := http.NewServeMux()
	h.Register(m)
	srv := httptest.NewServer(m)
	defer srv.Close()
	ch, closeS := openGlassStream(t, srv, "gd-gone", "")
	defer closeS()
	if ev := nextEvent(t, ch, "ended"); ev.data != `{"reason":"gone"}` {
		t.Fatalf("ended = %+v", ev)
	}
	timeout := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // the handler closed the stream after ended
			}
		case <-timeout:
			t.Fatal("stream stayed open after ended")
		}
	}
}

// The Glass carries its gardener's transcript under the terminal (gd-GPu096omMw): on the card world, the terminal, then the
// rows of the fixture activity document (glassActivity), then the header's rows; folded at first on a phone; and a viewer
// below the operator tier asking for a Glass lands on the transcript kind. What is asserted lives in
// testdata/glass_transcript_browser.js.
func TestGlassTranscriptUnderChromium(t *testing.T) {
	underChromium(t, "glass_transcript_browser.js", func() *httptest.Server { return cardWorld(t, func() time.Time { return goldenNow }) })
}
