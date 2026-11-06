package commons

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
	"gardenio/controller/internal/commons/commonstest"
)

// nodeBudget is the most bytes one node fragment may weigh (gd-qlk31NBV6o's
// done line: each fixture root in one response under 20 KB).
const nodeBudget = 20 << 10

func workbench(t *testing.T) *commonstest.Daemon {
	t.Helper()
	d := commonstest.TwoLands(t0)
	commonstest.Workbench(d, t0)
	return d
}

// TestNodeRoots pins the node model (Commons v2 step 3, gd-qlk31NBV6o): the
// fixture plan, one of its phases, a plain epic, a thread and an agent each
// come back in one response under nodeBudget, read in one bead read and at
// most four rounds of batch reads, with what each kind of root lists.
func TestNodeRoots(t *testing.T) {
	d := workbench(t)
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes = d })
	for _, tc := range []struct {
		id      string
		want    []string
		order   []string // these appear in this order
		notWant []string
	}{
		{id: commonstest.WorkbenchPlan,
			want: []string{`data-kind="plan"`, `data-state="building"`,
				// its gardener and its thread
				`Gardener <a href="/base/garden/c/n/cl-gard" data-node="cl-gard">Plan gardener for cl-plan</a> <span class="cm-state" data-state="running">running</span>`,
				`href="/base/garden/c/bead/cl-wish1"`,
				// a step's state, PR and ask, and a step's blocker
				`data-id="cl-s3" data-type="task" data-state="in_progress"`, `<span class="cm-state" data-state="in_progress">in progress</span>`,
				`<a class="cm-tn-pr" data-state="opened" href="https://github.com/acme/app/pull/1792">PR opened</a>`,
				`<a class="cm-tn-ask" href="/base/garden/c/n/cl-wdec" data-node="cl-wdec" data-type="decision"`,
				`waits on cl-s3`, `data-expand="/base/garden/c/n/cl-s3"`},
			// phases in phase_order, then the step under no phase; open asks and
			// PRs before the done ones, ticked
			order: []string{`data-id="cl-ph1"`, `data-id="cl-s1"`, `data-id="cl-s4"`, `data-id="cl-ph2"`, `data-id="cl-s8"`, `data-id="cl-s9"`, `data-id="cl-mr-s3"`, `data-id="cl-wdec"`, `cm-open--done" data-id="cl-mr-s1"`, `cm-open--escalation cm-open--done" data-id="cl-wesc"`},
			// its face is the bead view's (gd-0GnBi7SRfK), its narrative no step
			notWant: []string{`cm-node-face`, `cm-tn--bundle`, `data-id="cl-bundle"`}},
		{id: commonstest.WorkbenchPhase,
			want: []string{`data-kind="phase"`, `<a class="cm-node-up" href="/base/garden/c/n/cl-plan" data-node="cl-plan">Commons v2</a>`,
				`<nav class="cm-node-siblings" aria-label="Other phases"><ol><li class="cm-tn cm-tn--epic" data-id="cl-ph2"`,
				// a step's crew under it, and the step's jobs (none yet)
				`data-id="cl-crew-s3" data-type="agent" data-state="working"`},
			order:   []string{`data-id="cl-s1"`, `data-id="cl-crew-s1"`, `data-id="cl-s2"`, `data-id="cl-s3"`, `data-id="cl-s4"`},
			notWant: []string{`data-id="cl-s5"`, `cm-tn-jobs`}},
		{id: commonstest.WorkbenchEpic,
			want: []string{`data-kind="epic"`, `cm-tn--closed" data-id="cl-et1"`,
				`<section class="cm-node-deps"><h3>Waiting on it</h3><ol><li class="cm-tn cm-tn--task" data-id="cl-edep"`},
			order: []string{`data-id="cl-et1"`, `data-id="cl-et2"`, `data-id="cl-et3"`, `data-id="cl-et3-1"`, `data-id="cl-et3-2"`, `data-id="cl-et4"`, `data-id="cl-et5"`}},
		{id: commonstest.WorkbenchThread,
			want: []string{`data-kind="thread"`, `data-thread="cl-wish1">2 replies</a>`,
				`data-id="cl-plan"`, `data-id="cl-tagent1"`, `data-id="cl-tagent2"`, `data-id="cl-task1"`},
			// a step shows under its phase, a crew under its step: not on the
			// thread's own level
			notWant: []string{`data-id="cl-s1"`, `data-id="cl-crew-s3"`, `data-id="cl-bundle"`, `cm-node-face`}},
		{id: commonstest.WorkbenchAgent,
			want: []string{`data-kind="agent"`, `<a class="cm-node-up" href="/base/garden/c/n/cl-s3" data-node="cl-s3">`, `data-thread="cl-wish1">Thread</a>`, `<section class="cm-jobs" data-slot="jobs" data-count="0">`, "No jobs seen yet.",
				// a running agent's card is its head (step 5): state, Watch live and Nudge
				`<p class="cm-node-live" data-glass="working">`, `<span class="cm-acard-state">Working</span>`, `href="/base/garden?glass=` + commonstest.WorkbenchAgent + `" aria-label="Watch`}},
		// a task's Up is its epic (the workbench's u)
		{id: "cl-et3-1", want: []string{`data-kind="bead"`, `<a class="cm-node-up" href="/base/garden/c/n/cl-et3" data-node="cl-et3">`}},
		{id: "cl-tagent1",
			want: []string{`data-kind="agent"`, `data-id="cl-tagent2" data-type="agent" data-state="idle"`}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			d.Batches, d.Reads = 0, nil
			code, body := get(t, srv, "/garden/c/n/"+tc.id, "claimlayer")
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			if len(body) > nodeBudget {
				t.Errorf("%d bytes, over the %d-byte budget", len(body), nodeBudget)
			}
			t.Logf("%s: %d bytes, %d batch reads", tc.id, len(body), d.Batches)
			if got := d.Reads[commonstest.ClaimlayerBearer]; got != 1 {
				t.Errorf("%d bead reads, want the root's one", got)
			}
			// four rounds, the widest (a thread's or a phase's) with three
			// batch reads in one round
			if d.Batches > 9 {
				t.Errorf("%d batch reads", d.Batches)
			}
			mustContain(t, body, append([]string{`<section class="cm-node cm-node--`, `data-id="` + tc.id + `"`}, tc.want...)...)
			mustNotContain(t, body, tc.notWant...)
			mustNotContain(t, body, commonstest.Leaks...)
			at := 0
			for _, o := range tc.order {
				i := strings.Index(body[at:], o)
				if i < 0 {
					t.Fatalf("%s not after the one before it:\n%s", o, body)
				}
				at += i + len(o)
			}
		})
	}
}

// TestNodeThreadKeepsItsOwnChildren: a wish head's node lists the task it gave
// rise to on its own level, though the task is bound to the thread and its
// parent is the head, with its crew under it and its PR in the Open rail
// (gd-efoAiLlzRC: the bead panel embeds this tree as its Work section).
func TestNodeThreadKeepsItsOwnChildren(t *testing.T) {
	d := workbench(t)
	const head = "cl-wishS"
	commonstest.WishFamily(head, "claimlayer", "cl-chgen", t0).PutIn(d)
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes = d })
	code, body := get(t, srv, "/garden/c/n/"+head, "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	task, agent := head+commonstest.WishTask, head+commonstest.WishAgent
	mustContain(t, body, `data-kind="thread"`, `<li class="cm-tn cm-tn--task" data-id="`+task+`"`, `data-id="`+head+commonstest.WishMR+`"`)
	if i, j := strings.Index(body, `data-id="`+task+`"`), strings.Index(body, `data-id="`+agent+`"`); i < 0 || j < i || !strings.Contains(body[i:j], "<ol>") {
		t.Errorf("the crew %s is not under its task %s:\n%s", agent, task, body)
	}
}

// TestNodeEmbedHasNoRefusal: the bead panel's read (?embed=1) of a bead with no
// node, missing or in a land the viewer may not see, or with the node model
// off, is a 204 with no body; without embed it stays a 404 or 503.
func TestNodeEmbedHasNoRefusal(t *testing.T) {
	d := workbench(t)
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes = d })
	off := commonsServer(t, d, func(h *Handler) { h.Nodes = nil })
	for _, tc := range []struct {
		srv  http.Handler
		path string
		want int
	}{
		{srv, "/garden/c/n/cl-nope?embed=1", http.StatusNoContent},
		{srv, "/garden/c/n/cl-nope", http.StatusNotFound},
		{off, "/garden/c/n/" + commonstest.WorkbenchEpic + "?embed=1", http.StatusNoContent},
		{off, "/garden/c/n/" + commonstest.WorkbenchEpic, http.StatusServiceUnavailable},
		{srv, "/garden/c/n/" + commonstest.WorkbenchEpic + "?embed=1", http.StatusOK},
	} {
		if code, body := get(t, tc.srv, tc.path, "claimlayer"); code != tc.want || (code == http.StatusNoContent && body != "") {
			t.Errorf("%s: %d %q, want %d", tc.path, code, body, tc.want)
		}
	}
}

// TestNodeCutsByLand: a root in a land the viewer may not see is a 404, and a
// bead there is left out wherever it would show (here, a task's blocker).
func TestNodeCutsByLand(t *testing.T) {
	d := workbench(t)
	d.Link("cl-et4", "gd-wish1", "blocks")
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes = d })
	if code, _ := get(t, srv, "/garden/c/n/gd-wish1", "claimlayer"); code != http.StatusNotFound {
		t.Errorf("another land's bead: %d", code)
	}
	if code, _ := get(t, srv, "/garden/c/n/cl-missing", "claimlayer"); code != http.StatusNotFound {
		t.Errorf("a missing bead: %d", code)
	}
	_, body := get(t, srv, "/garden/c/n/"+commonstest.WorkbenchEpic, "claimlayer")
	mustNotContain(t, body, "gd-wish1", "waits on")
	mustNotContain(t, body, commonstest.Leaks...)
	// the admin sees it
	_, body = get(t, srv, "/garden/c/n/"+commonstest.WorkbenchEpic, "")
	mustContain(t, body, `waits on gd-wish1`)
	if code, _ := get(t, commonsServer(t, d), "/garden/c/n/cl-epic", "claimlayer"); code != http.StatusServiceUnavailable {
		t.Errorf("no Nodes: %d", code)
	}
}

// TestNodeCarriesTheGardenersReason: an agent's head says why it is not
// working, as Handler.Reason (the farm) says it, with the agent and its task
// the workbench's Wake / Restart asks with (gd-7DyHdnx8fC); a working agent,
// a bead that is no agent, and a handler with no Reason carry no line.
func TestNodeCarriesTheGardenersReason(t *testing.T) {
	d := workbench(t)
	b, err := d.GetBead(context.Background(), commonstest.WorkbenchAgent)
	if err != nil {
		t.Fatal(err)
	}
	b.Fields["task_title"] = "Fix <the> fence"
	d.Put(b)
	var mu sync.Mutex
	var asked []string
	reason := func(id string) (string, string) {
		mu.Lock()
		asked = append(asked, id)
		mu.Unlock()
		if id == commonstest.WorkbenchAgent {
			return "down", "Failed: its pod was lost"
		}
		return "", ""
	}
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes, h.Reason = d, reason })
	_, body := get(t, srv, "/garden/c/n/"+commonstest.WorkbenchAgent, "claimlayer")
	mustContain(t, body, `<p class="cm-node-reason" data-reason="down" data-id="`+commonstest.WorkbenchAgent+`" data-agent="`+b.Fields["agent"]+`" data-task="Fix &lt;the&gt; fence">Failed: its pod was lost</p>`)
	if strings.Index(body, "cm-node-reason") > strings.Index(body, "</header>") {
		t.Error("the reason is not in the head")
	}
	_, body = get(t, srv, "/garden/c/n/cl-tagent1", "claimlayer")
	mustNotContain(t, body, "cm-node-reason")
	_, body = get(t, srv, "/garden/c/n/"+commonstest.WorkbenchEpic, "claimlayer")
	mustNotContain(t, body, "cm-node-reason")
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(strings.Join(asked, " "), commonstest.WorkbenchEpic) {
		t.Errorf("asked the farm about an epic: %v", asked)
	}
	_, body = get(t, commonsServer(t, d, func(h *Handler) { h.Nodes = d }), "/garden/c/n/"+commonstest.WorkbenchAgent, "claimlayer")
	mustNotContain(t, body, "cm-node-reason")
}

// TestNodeShowsJobs: an agent's node lists its jobs newest first, and a step's
// row counts its crew's.
func TestNodeShowsJobs(t *testing.T) {
	d := workbench(t)
	j := NewJobs(d, nil, nil)
	agent := commonstest.WorkbenchAgent
	observeAll(j,
		hookEvent("PostToolUse", agent, "tu1", map[string]any{"tool_name": "Bash", "duration_ms": 3400,
			"tool_input": map[string]any{"command": "go test -run TestNode ./internal/commons"}, "tool_response": map[string]any{"stdout": "ok  \tgardenio/controller/internal/commons\t0.15s"}}),
		hookEvent("PreToolUse", agent, "tu2", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "go vet ./internal/commons"}}),
	)
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes, h.Jobs = d, j })
	_, body := get(t, srv, "/garden/c/n/"+agent, "claimlayer")
	mustContain(t, body, `<section class="cm-jobs" data-slot="jobs" data-count="2">`,
		`<li class="cm-job cm-job--running" data-job="tu2" data-tool="Bash"><code class="cm-job-cmd">go vet ./internal/commons</code> <span class="cm-job-meta">running</span>`,
		`<li class="cm-job" data-job="tu1" data-tool="Bash"><code class="cm-job-cmd">go test -run TestNode ./internal/commons</code> <span class="cm-job-meta">exit 0 · 3.4 s · 47 B</span>`,
		`<pre class="cm-job-out">ok  	gardenio/controller/internal/commons	0.15s</pre>`)
	if strings.Index(body, `data-job="tu2"`) > strings.Index(body, `data-job="tu1"`) {
		t.Error("jobs not newest first")
	}
	_, body = get(t, srv, "/garden/c/n/"+commonstest.WorkbenchPhase, "claimlayer")
	// the row names its thread, so the workbench marks it unread from a stream message there (step 6, gd-Txos7COlj0)
	mustContain(t, body, `data-id="cl-crew-s3" data-type="agent" data-state="working" data-thread="cl-wish1"><a class="cm-tn-title" href="/base/garden/c/n/cl-crew-s3" data-node="cl-crew-s3">claimlayer-The node model</a><span class="cm-tn-meta"><span class="cm-state" data-state="working">working</span> <span class="cm-tn-jobs" data-slot="jobs">2 jobs</span>`)
	// the step row sums its crew's
	i := strings.Index(body, `data-id="cl-s3"`)
	mustContain(t, body[i:i+900], `<span class="cm-tn-jobs" data-slot="jobs">2 jobs</span>`)
}

// timedRecorder notes when each marker first shows in a stream's body.
type timedRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	markers []string
	seen    map[string]time.Time
	done    func()
}

func (s *timedRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ResponseRecorder.Flush()
	body := s.Body.String()
	for _, m := range s.markers {
		if _, ok := s.seen[m]; !ok && strings.Contains(body, m) {
			s.seen[m] = time.Now()
		}
	}
	if len(s.seen) == len(s.markers) {
		s.done()
	}
}

// TestNodeStreamLatency pins the stream half of step 3's done line: a job
// finishing and a step closing on the fixture reach a stream watching the
// phase within 100 ms of the hook event and the bead event. A viewer of
// another land, or a stream not watching the agent, gets no job.
func TestNodeStreamLatency(t *testing.T) {
	const budget = 100 * time.Millisecond
	d := workbench(t)
	src := make(liveSource, 16)
	hub := NewHub(src.open, d, nil)
	hub.Start()
	t.Cleanup(hub.Close)
	jobs := NewJobs(d, hub, nil)
	jobs.Start()
	t.Cleanup(jobs.Close)
	h := &Handler{Beads: d, Nodes: d, Hub: hub, Jobs: jobs, BasePath: "/base",
		Scope: func(r *http.Request) Scope {
			if beadsapi.UserBearerFromContext(r.Context()) == commonstest.ClaimlayerBearer {
				return claimlayerScope
			}
			return Scope{Lands: []string{"gardenio"}}
		},
		Now: func() time.Time { return t0.Add(30 * time.Minute) }}

	step := *d.Beads[commonstest.WorkbenchStep]
	step.Status = "closed"
	d.Put(&step)
	closed, _ := json.Marshal(map[string]any{"bead": map[string]any{"id": step.ID, "type": step.Type, "status": "closed", "labels": step.Labels,
		"dependencies": []map[string]string{{"bead_id": step.ID, "depends_on_id": commonstest.WorkbenchPhase, "type": "parent-child"}}}})
	run := func(path, bearer, job string, markers ...string) (*timedRecorder, map[string]time.Time) {
		finished := hookEvent("PostToolUse", commonstest.WorkbenchAgent, job, map[string]any{"tool_name": "Bash", "duration_ms": 120,
			"tool_input": map[string]any{"command": "go vet ./internal/commons"}})
		ctx, cancel := context.WithTimeout(beadsapi.WithUserBearer(context.Background(), bearer), time.Second)
		t.Cleanup(cancel)
		rec := &timedRecorder{ResponseRecorder: httptest.NewRecorder(), markers: markers, seen: map[string]time.Time{}, done: cancel}
		pushed := map[string]time.Time{}
		go func() {
			time.Sleep(50 * time.Millisecond)
			pushed["job"] = time.Now()
			jobs.Observe(finished)
			time.Sleep(20 * time.Millisecond)
			pushed["node"] = time.Now()
			src <- beadsapi.SSEEvent{Event: "beads.bead.updated", Data: closed}
		}()
		h.events(rec, httptest.NewRequest("GET", path, nil).WithContext(ctx))
		return rec, pushed
	}

	jobMark, nodeMark := `"job":{"id":"tu1","tool":"Bash","command":"go vet ./internal/commons"`, `"node":{"id":"cl-s3","type":"task","status":"closed"`
	rec, pushed := run("/garden/c/events?root="+commonstest.WorkbenchPhase+"&node="+commonstest.WorkbenchAgent, commonstest.ClaimlayerBearer, "tu1", jobMark, nodeMark)
	for kind, mark := range map[string]string{"job": jobMark, "node": nodeMark} {
		got, ok := rec.seen[mark]
		if !ok {
			t.Fatalf("no %s event:\n%s", kind, rec.Body.String())
		}
		lag := got.Sub(pushed[kind])
		t.Logf("%s event to the stream: %v", kind, lag)
		if lag > budget {
			t.Errorf("%s event took %v, over %v", kind, lag, budget)
		}
	}
	mustContain(t, rec.Body.String(), "event: job\n", `"exit":0`, `"duration_ms":120`)

	// Another land's viewer, and a stream that does not watch the agent: no job.
	for _, tc := range []struct{ path, bearer, job string }{
		{"/garden/c/events?root=" + commonstest.WorkbenchPhase + "&node=" + commonstest.WorkbenchAgent, commonstest.NoGroupBearer, "tu2"},
		{"/garden/c/events?root=" + commonstest.WorkbenchEpic, commonstest.ClaimlayerBearer, "tu3"},
	} {
		rec, _ := run(tc.path, tc.bearer, tc.job, "never")
		mustNotContain(t, rec.Body.String(), "event: job", `"id":"`+tc.job+`"`)
	}
	if _, n := jobs.Of(commonstest.WorkbenchAgent); n != 3 {
		t.Errorf("the record took %d jobs, want 3", n)
	}
}
