package gardenhub

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

type fakeSource struct {
	mu       sync.Mutex
	active   []*beadsapi.BeadDetail
	decision []*beadsapi.BeadDetail
	escalate []*beadsapi.BeadDetail // status=open escalation beads
	agents   []*beadsapi.BeadDetail // status=open agent beads (not in the in_progress list)
	events   chan beadsapi.SSEEvent
	lists    int
}

func newFake() *fakeSource { return &fakeSource{events: make(chan beadsapi.SSEEvent, 16)} }

func (f *fakeSource) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if len(q.Types) == 1 && q.Types[0] == "decision" {
		return &beadsapi.ListBeadsResult{Beads: f.decision, Total: len(f.decision)}, nil
	}
	if len(q.Types) == 1 && q.Types[0] == "escalation" {
		return &beadsapi.ListBeadsResult{Beads: f.escalate, Total: len(f.escalate)}, nil
	}
	if len(q.Types) == 1 && q.Types[0] == "agent" {
		var out []*beadsapi.BeadDetail
		for _, b := range append(append([]*beadsapi.BeadDetail{}, f.active...), f.agents...) {
			if b.Type == "agent" {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	return &beadsapi.ListBeadsResult{Beads: f.active, Total: len(f.active)}, nil
}

func (f *fakeSource) EventStream(context.Context, string) (<-chan beadsapi.SSEEvent, error) {
	return f.events, nil
}

func (f *fakeSource) set(active, decision []*beadsapi.BeadDetail) {
	f.mu.Lock()
	f.active, f.decision = active, decision
	f.mu.Unlock()
}

func bead(id, typ string, updated int64) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Title: "t " + id, Type: typ, Status: "in_progress",
		OperatedBy: "OIDC:Me@Example.com", UpdatedAt: time.Unix(updated, 0), Labels: []string{"project:gardenio"}}
}

func TestRefreshDiffAndVersioning(t *testing.T) {
	f := newFake()
	f.set([]*beadsapi.BeadDetail{bead("a-1", "task", 100), bead("a-2", "agent", 100)}, nil)
	h := New(f, "", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	v1 := h.Stats().Version
	if got := h.Stats().Items; got != 2 {
		t.Fatalf("items=%d want 2", got)
	}

	// Identical re-read: no version bump, no patch.
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.Stats().Version != v1 {
		t.Fatal("version bumped without a change")
	}

	// One removal + one addition => exactly one patch.
	f.set([]*beadsapi.BeadDetail{bead("a-1", "task", 100), bead("a-3", "task", 200)}, nil)
	catch, live, cancel := h.Subscribe(v1)
	defer cancel()
	if len(catch) != 0 {
		t.Fatalf("up-to-date client got %d catch-up msgs", len(catch))
	}
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := <-live
	var p patch
	if err := json.Unmarshal(m.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Prev != v1 || p.V != v1+1 || len(p.Upserts) != 1 || p.Upserts[0].ID != "a-3" || len(p.Removes) != 1 || p.Removes[0] != "a-2" {
		t.Fatalf("unexpected patch %+v", p)
	}
}

func TestSubscribeReplayAndReset(t *testing.T) {
	f := newFake()
	h := New(f, "", nil)
	f.set([]*beadsapi.BeadDetail{bead("a-1", "task", 1)}, nil)
	_ = h.refresh(context.Background())
	v0 := h.Stats().Version
	for i, id := range []string{"a-2", "a-3", "a-4"} {
		cur, _ := f.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{})
		f.set(append(append([]*beadsapi.BeadDetail{}, cur.Beads...), bead(id, "task", int64(10+i))), nil)
		_ = h.refresh(context.Background())
	}

	// Behind by three versions: replayed as three ordered patches.
	catch, _, cancel := h.Subscribe(v0)
	cancel()
	if len(catch) != 3 || catch[0].Event != "patch" || catch[2].V != h.Stats().Version {
		t.Fatalf("replay = %d msgs %+v", len(catch), catch)
	}
	// Unknown / future / zero versions: one reset carrying the full state.
	for _, since := range []uint64{0, 1, h.Stats().Version + 5} {
		catch, _, cancel := h.Subscribe(since)
		cancel()
		if len(catch) != 1 || catch[0].Event != "reset" {
			t.Fatalf("since=%d: want one reset, got %+v", since, catch)
		}
		if !strings.Contains(string(catch[0].Data), "i-a-4") {
			t.Fatalf("reset missing current item: %s", catch[0].Data)
		}
	}
}

func TestSlowClientCollapsesToReset(t *testing.T) {
	f := newFake()
	h := New(f, "", nil)
	_ = h.refresh(context.Background())
	_, live, cancel := h.Subscribe(h.Stats().Version)
	defer cancel()
	for i := 0; i < clientBuffer+10; i++ {
		cur, _ := f.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{})
		f.set(append(append([]*beadsapi.BeadDetail{}, cur.Beads...), bead("n-"+strings.Repeat("x", i+1), "task", int64(i))), nil)
		_ = h.refresh(context.Background())
	}
	m := <-live
	if m.Event != "reset" || m.Data != nil {
		t.Fatalf("laggard should see a reset marker, got %+v", m)
	}
}

func TestPageIsServedFromSnapshotAndETag(t *testing.T) {
	f := newFake()
	f.set([]*beadsapi.BeadDetail{bead("a-1", "agent", 5)}, []*beadsapi.BeadDetail{bead("d-1", "decision", 6)})
	h := New(f, "/v", nil)
	_ = h.refresh(context.Background())
	listsBefore := f.lists

	hd := &Handler{Hub: h, BasePath: "/v", Viewer: func(*http.Request) string { return "oidc:me@example.com" }}
	mux := http.NewServeMux()
	hd.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `href="/v/garden?bead=d-1"`) || !strings.Contains(body, `data-me="oidc:me@example.com"`) || !strings.Contains(body, `data-op="oidc:me@example.com"`) {
		t.Fatalf("page missing expected content (code %d):\n%s", rec.Code, body)
	}
	// The split the Lands budget reads (gd-7jlAbyk8Fy): the snapshot read, the template, then the whole.
	if st := rec.Header().Get("Server-Timing"); !regexp.MustCompile(`^snapshot;dur=[\d.]+, template;dur=[\d.]+, (snapshot-age;dur=[\d.]+, )?total;dur=[\d.]+$`).MatchString(st) {
		t.Fatalf("Server-Timing = %q", st)
	}
	if f.lists != listsBefore {
		t.Fatal("page render hit the daemon")
	}

	req := httptest.NewRequest("GET", "/garden", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("revisit = %d, want 304", rec2.Code)
	}
}

func TestSSEEndToEnd(t *testing.T) {
	f := newFake()
	f.set([]*beadsapi.BeadDetail{bead("a-1", "task", 5)}, nil)
	h := New(f, "", nil)
	h.Start()
	defer h.Close()
	v := h.Stats().Version

	mux := http.NewServeMux()
	(&Handler{Hub: h}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/garden/events?since=" + itoa(v))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}

	// A daemon change + an upstream event must reach the open stream well inside 2s.
	f.set([]*beadsapi.BeadDetail{bead("a-1", "task", 5), bead("a-9", "task", 9)}, nil)
	start := time.Now()
	f.events <- beadsapi.SSEEvent{Event: "bead.updated"}

	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), "a-9") {
				got <- sc.Text()
				return
			}
		}
	}()
	select {
	case <-got:
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("event->browser took %s", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("patch never arrived")
	}
}

func itoa(v uint64) string { b, _ := json.Marshal(v); return string(b) }

// Agent beads sit at status=open for their whole life; the hub must list them
// even though the in_progress query never returns them.
func TestRefreshListsOpenAgents(t *testing.T) {
	f := newFake()
	ag := bead("g-1", "agent", 100)
	ag.Status = "open"
	ag.Fields = map[string]string{"agent_state": "working"}
	f.set([]*beadsapi.BeadDetail{bead("a-1", "task", 100)}, nil)
	f.agents = []*beadsapi.BeadDetail{ag}
	h := New(f, "", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := h.Snapshot()
	if snap.Counts[SecAgent] != 1 || snap.Counts[SecWork] != 1 {
		t.Fatalf("counts=%v want 1 agent + 1 work", snap.Counts)
	}
	if !strings.Contains(snap.Frags[SecAgent][0], `data-st="working"`) {
		t.Fatalf("agent fragment lacks its state: %s", snap.Frags[SecAgent][0])
	}
}

// An escalation that is open but already approved (or rejected) is nobody's to answer: the Mailbox leaves it out
// rather than offer Approve on it again (gd-yuOG6cX6VI).
func TestMailboxSkipsAnsweredEscalation(t *testing.T) {
	f := newFake()
	pending := bead("e-1", "escalation", 8)
	pending.Status = "open"
	pending.Fields = map[string]string{"operation_type": "gitlab_mr_merge", "context": "Merge PR 1"}
	approved := bead("e-2", "escalation", 8)
	approved.Status = "open"
	approved.Fields = map[string]string{"operation_type": "gitlab_mr_merge", "context": "Merge PR 2", "approved_by": "mo", "approved_at": "2026-11-02T18:00:00Z"}
	rejected := bead("e-3", "escalation", 8)
	rejected.Status = "open"
	rejected.Fields = map[string]string{"operation_type": "gitlab_mr_merge", "rejected_by": "mo", "rejection_reason": "not now"}
	f.set(nil, nil)
	f.escalate = []*beadsapi.BeadDetail{pending, approved, rejected}
	h := New(f, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := h.Snapshot()
	if snap.Counts[SecDecision] != 1 {
		t.Fatalf("mailbox count=%d want 1 (the pending escalation only)", snap.Counts[SecDecision])
	}
	html := strings.Join(snap.Frags[SecDecision], "")
	if !strings.Contains(html, `data-act="approve" data-id="e-1"`) || strings.Contains(html, `data-id="e-2"`) || strings.Contains(html, `data-id="e-3"`) {
		t.Errorf("mailbox offers Approve on e-1 only\n%s", html)
	}
	if !strings.Contains(html, `data-act="comment"`) || !strings.Contains(html, `aria-label="Comment (optional)"`) {
		t.Errorf("the escalation's box offers a comment with the answer\n%s", html)
	}
}

// An escalation an executor has claimed (status in_progress) is in the daemon's in_progress list with the work: it
// is a letter in hand, not a plant, so the work section (the Growing field) leaves it out (gd-oChRvR8mg2).
func TestWorkSkipsInProgressEscalation(t *testing.T) {
	f := newFake()
	esc := bead("e-1", "escalation", 8)
	esc.Fields = map[string]string{"operation_type": "gitlab_mr_merge", "context": "Merge PR 1", "executor": "mo"}
	f.set([]*beadsapi.BeadDetail{bead("a-1", "task", 9), esc}, nil)
	h := New(f, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := h.Snapshot()
	if snap.Counts[SecWork] != 1 {
		t.Fatalf("work count=%d want 1 (the task only)", snap.Counts[SecWork])
	}
	if html := strings.Join(snap.Frags[SecWork], ""); strings.Contains(html, `id="w-e-1"`) || strings.Contains(html, `data-id="e-1"`) {
		t.Errorf("the in-hand escalation is drawn in the field\n%s", html)
	}
}

// The Mailbox holds open decisions AND open escalations, each with what the dock needs to answer it in place.
func TestMailboxCarriesDecisionsAndEscalations(t *testing.T) {
	f := newFake()
	d := bead("d-1", "decision", 7)
	d.Status = "open"
	d.Description = "Background from the <description>" // an older writer: no context field (gd-hR3ImbMVhP)
	d.Fields = map[string]string{
		"prompt":  "Ship  it\nnow?",
		"options": `[{"id":"a","short":"Ship","label":"Ship it today"},{"id":"b","label":"Wait"},{"label":"no id, free text only"}]`,
	}
	e := bead("e-1", "escalation", 8)
	e.Status = "open"
	e.Fields = map[string]string{"operation_type": "gitlab_mr_merge", "context": "Merge <b>PR 1</b>"}
	f.set(nil, []*beadsapi.BeadDetail{d})
	f.escalate = []*beadsapi.BeadDetail{e}
	h := New(f, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := h.Snapshot()
	if snap.Counts[SecDecision] != 2 {
		t.Fatalf("mailbox count=%d want 2 (one decision, one escalation)", snap.Counts[SecDecision])
	}
	html := strings.Join(snap.Frags[SecDecision], "")
	for _, want := range []string{
		`data-kind="decision"`, `<p class="ask">Ship it now?</p>`,
		`<p class="ask">Ship it now?</p><div class="ctx md"><p>Background from the &lt;description&gt;</p>`, // under the ask, open, as Markdown (gd-OSI1uK35Wa)
		`data-act="resolve" data-id="d-1" data-opt="a" title="Ship it today">Ship</button>`,
		`data-opt="b" title="Wait">Wait</button>`, `data-act="other"`, `<textarea`,
		`data-kind="escalation"`, `<b>gitlab_mr_merge</b> · Merge &lt;b&gt;PR 1&lt;/b&gt;`,
		`data-act="approve" data-id="e-1"`, `data-act="reject" data-id="e-1"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("mailbox fragments missing %q\n%s", want, html)
		}
	}
	if strings.Contains(html, "ctx-none") {
		t.Errorf("a decision with context does not say it has none\n%s", html)
	}
	if strings.Count(html, `data-act="resolve" data-id="d-1"`) != 3 {
		t.Errorf("want two option buttons plus the free-text send, none for the option without an id\n%s", html)
	}
	if strings.Contains(html, "<b>PR 1</b>") {
		t.Error("escalation context was not escaped")
	}

	// Answering removes the bead from the open list: it leaves the mailbox in one patch.
	f.escalate = nil
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.Snapshot().Counts[SecDecision]; got != 1 {
		t.Fatalf("mailbox count=%d after the escalation was resolved, want 1", got)
	}
}

func TestClipAndOptionList(t *testing.T) {
	if got := clip("  a   b\nc ", 10); got != "a b c" {
		t.Errorf("clip whitespace: %q", got)
	}
	if got := clip("héllo wörld", 5); got != "héll…" {
		t.Errorf("clip must cut on runes: %q", got)
	}
	if (Item{Options: "not json"}).OptionList() != nil || (Item{}).OptionList() != nil {
		t.Error("bad or empty options must yield no buttons")
	}
	if got := (Item{Options: `["x", {"id":"k"}]`}).OptionList(); len(got) != 0 {
		t.Errorf("a bare string option has no id and must be skipped: %+v", got)
	}
}

func TestHubIgnoresIrrelevantEvents(t *testing.T) {
	f := newFake()
	h := New(f, "", nil)
	var got []Relevance
	var mu sync.Mutex
	h.OnEvent(func(r Relevance) { mu.Lock(); got = append(got, r); mu.Unlock() })
	h.Start()
	defer h.Close()
	time.Sleep(2 * refreshCoalesce) // let the connect refresh settle
	f.mu.Lock()
	before := f.lists
	f.mu.Unlock()
	f.events <- beadsapi.SSEEvent{Event: "beads.bead.updated", Data: []byte(`{"bead":{"type":"agent","status":"open"},"changes":{"fields":{}}}`)}
	time.Sleep(3 * refreshCoalesce)
	f.mu.Lock()
	after := f.lists
	f.mu.Unlock()
	if after != before {
		t.Fatalf("agent usage event re-read the hub: lists %d -> %d", before, after)
	}
	f.events <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: []byte(`{"bead":{"type":"decision","status":"open"}}`)}
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := f.lists
		f.mu.Unlock()
		if n > after {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("decision event did not refresh the hub (positive control)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 3 || got[0] != relevantAll || got[1] != (Relevance{Roster: true}) || got[len(got)-1] != (Relevance{Hub: true, Farm: true}) {
		t.Fatalf("onEvent relevances = %+v, want relevantAll, {Roster}, then {Hub Farm}", got)
	}
}

// The Mailbox row's context (gd-OSI1uK35Wa): Markdown under the ask and above the answers, a long one folded behind Show
// more after gardenmd.FoldAt runes, and an ask with none says so rather than offer answers over nothing.
func TestMailboxContextMarkdownAndFold(t *testing.T) {
	long := bead("d-2", "decision", 5)
	long.Status = "open"
	long.Fields = map[string]string{"prompt": "Merge?", "options": `[{"id":"y","short":"Yes"}]`,
		"context": "**Why**: blocked on gd-AbCdEfGhIj.\n\n" + strings.Repeat("More background. ", 50)}
	none := bead("d-3", "decision", 6)
	none.Status = "open"
	none.Fields = map[string]string{"prompt": "Bare?", "options": `[{"id":"y","short":"Yes"}]`}
	f := newFake()
	f.set(nil, []*beadsapi.BeadDetail{long, none})
	h := New(f, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	html := strings.Join(h.Snapshot().Frags[SecDecision], "")
	for _, want := range []string{
		`<p class="ask">Merge?</p><div class="ctx md md-fold"><div class="md-body"><p><strong>Why</strong>: blocked on <a class="bl" href="/v/garden?bead=gd-AbCdEfGhIj" data-bead="gd-AbCdEfGhIj"`,
		`<details class="md-more"><summary><span class="md-more-l">Show more</span>`,
		`<p class="ask">Bare?</p><p class="ctx-none">No context was given with this question.</p><div class="acts">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("mailbox fragments missing %q\n%s", want, html)
		}
	}
	if i, j := strings.Index(html, `class="ctx md md-fold"`), strings.Index(html, `data-id="d-2" data-opt="y"`); i < 0 || j < i {
		t.Errorf("the context comes before the answers\n%s", html)
	}
}

// Every ledger row says its project on the wire (data-pj), in every section, so a home strip fills its Mailbox, Growing
// field and Barn from the ledger by project (homestead-world-design.md 28.2, gd-GYo4PWlGUb). An agent bead whose project
// is only a field gets it from the field, as the farm's gardener does, and a project-scoped viewer's cut keeps it.
func TestLedgerRowsCarryProject(t *testing.T) {
	zeta := func(b *beadsapi.BeadDetail) *beadsapi.BeadDetail { b.Labels = []string{"project:zeta"}; return b }
	esc := zeta(bead("z-esc", "escalation", 4))
	esc.Status = "open"
	agent := bead("z-agent", "agent", 5)
	agent.Status, agent.Labels, agent.Fields = "open", nil, map[string]string{"project": " zeta ", "agent_state": "working", "role": " crew "}
	bare := bead("n-work", "task", 6)
	bare.Labels = nil
	f := newFake()
	f.set([]*beadsapi.BeadDetail{zeta(bead("z-work", "task", 1)), bead("g-work", "task", 2), bare},
		[]*beadsapi.BeadDetail{zeta(bead("z-ask", "decision", 3))})
	f.escalate, f.agents = []*beadsapi.BeadDetail{esc}, []*beadsapi.BeadDetail{agent}
	h := New(f, "", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := map[string]struct{ sec, pj string }{
		"z-work": {SecWork, "zeta"}, "g-work": {SecWork, "gardenio"}, "n-work": {SecWork, ""},
		"z-ask": {SecDecision, "zeta"}, "z-esc": {SecDecision, "zeta"}, "z-agent": {SecAgent, "zeta"},
	}
	for id, w := range want {
		it, ok := h.items[id]
		if !ok || it.Section != w.sec || it.Project != w.pj {
			t.Errorf("%s = %+v, want section %s project %q", id, it, w.sec, w.pj)
			continue
		}
		if frag := h.frags[id]; !strings.Contains(frag, `data-pj="`+w.pj+`"`) {
			t.Errorf("%s's row does not say data-pj=%q: %s", id, w.pj, frag)
		}
		// only an agent row says its role (data-role, every agent row even with none), so the Yard dresses the hub's villagers
		// as the farm's gardeners from the first frame (gd-wJVEevAYPP)
		if frag, isAgent := h.frags[id], w.sec == SecAgent; strings.Contains(frag, `data-role="`) != isAgent || (isAgent && !strings.Contains(frag, `data-role="crew"`)) {
			t.Errorf("%s's row role: %s", id, frag)
		}
	}

	snap := h.CutSnapshot(FarmCut{Scoped: true, Projects: []string{"zeta"}})
	if snap.Counts[SecWork] != 1 || snap.Counts[SecDecision] != 2 || snap.Counts[SecAgent] != 1 {
		t.Errorf("zeta's cut counts = %v, want work 1, decision 2, agent 1", snap.Counts)
	}
	for _, frags := range snap.Frags {
		for _, frag := range frags {
			if !strings.Contains(frag, `data-pj="zeta"`) {
				t.Errorf("zeta's cut carries another project's row: %s", frag)
			}
		}
	}
}
