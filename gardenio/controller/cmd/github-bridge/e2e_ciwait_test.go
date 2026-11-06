package main

// Hermetic end-to-end test of wake-on-ci (plan gardenio/plans/wake-on-ci-completion.md
// §3/P6, task gd-q4KMd2egqF). Recorded workflow_run deliveries are HMAC-signed
// and POSTed over loopback HTTP to the production receiver, through the sink
// chain run() builds (the CI-wait resolver first, then newBeadSink), into the
// in-memory beads daemon (e2e_test.go) holding an agent bead parked the way
// `gb wait ci` + `gb stop` leave it. The resolver's sweep then settles the wait
// and wakes the agent. The poll fallback lists runs from a fake Actions API
// through the production githubCommenter, serving the same run objects the
// deliveries carry.
//
// Nothing here touches GitHub or a cluster. Live verification waits on
// github-bridge being enabled on k3s (helm_deploy escalation gd-AUdgkFSB1f).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/ciwait"
)

const (
	e2eCIRepo      = "mattbakerconsulting/gardenio"
	e2eCISHA       = "5c2e8f1a9b7d3e6f0a4c8b2d1e9f7a3c5b0d6e48" // head_sha of the testdata/workflow_run_* fixtures
	e2eCIKey       = e2eCIRepo + "@" + e2eCISHA
	e2eCIAgent     = "crew-ci-wait"
	e2eCIAgentBead = "gd-ciwaitagent"
	e2eCITask      = "gd-ciwaittask"
	e2eCIRunCI     = 18850011
	e2eCIRunChecks = 18850012
)

var e2eCIT0 = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)

// ciE2E is one github-bridge as run() wires it, on a fake clock.
type ciE2E struct {
	t     *testing.T
	d     *memDaemon
	ciw   *ciWaitHandler
	hook  *httptest.Server
	clock time.Time
	n     int
}

// newCIE2E starts the receiver with the resolver built from cfg. runs is the
// Actions listing; nil is a bridge with no GitHub App (webhook-only).
func newCIE2E(t *testing.T, cfg *config, runs ciRunLister) *ciE2E {
	t.Helper()
	c := &ciE2E{t: t, d: newMemDaemon(), clock: e2eCIT0}
	c.d.add(&beadsapi.BeadDetail{ID: e2eCITask, Type: "task", Status: "in_progress", Assignee: e2eCIAgent, Title: "wake-on-ci"})
	c.ciw = newCIWaitResolver(c.d, runs, cfg, quiet())
	c.ciw.now = func() time.Time { return c.clock }
	// The order run() uses: the resolver first, so a later sink's error never
	// hides a run from the ledger.
	sink := fanoutSink{c.ciw, newBeadSink(logSink{logger: quiet()}, c.d, nil, nil, quiet())}
	c.hook = httptest.NewServer(newWebhookHandler(webhookConfig{
		Secret: testSecret,
		Sink:   sink,
		Logger: quiet(),
		Replay: newReplayCache(time.Hour, 100, nil),
	}))
	t.Cleanup(c.hook.Close)
	return c
}

func (c *ciE2E) at(d time.Duration) { c.clock = e2eCIT0.Add(d) }

// post sends a workflow_run payload, signed with secret, as delivery id, and
// returns the receiver's status code.
func (c *ciE2E) post(body []byte, id string, secret []byte) int {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.hook.URL+"/webhook", strings.NewReader(string(body)))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerSignature, sign(secret, body))
	req.Header.Set(headerDelivery, id)
	req.Header.Set(headerEvent, "workflow_run")
	resp, err := c.hook.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// deliver is GitHub delivering fixtureName as a new delivery.
func (c *ciE2E) deliver(fixtureName string) {
	c.t.Helper()
	c.n++
	if code := c.post(fixture(c.t, fixtureName), "ciwait-e2e-"+strconv.Itoa(c.n), testSecret); code != http.StatusAccepted {
		c.t.Fatalf("deliver %s = %d, want 202", fixtureName, code)
	}
}

// park adds the agent bead `gb wait ci --repo R --sha S` stamps and `gb stop`
// then closes, with extra fields on top.
func (c *ciE2E) park(extra map[string]string) {
	fields := map[string]string{
		"agent":                      e2eCIAgent,
		"project":                    "gardenio",
		"role":                       "crew",
		"task_id":                    e2eCITask,
		beadsapi.FieldCIWaitState:    beadsapi.CIWaitStatePending,
		beadsapi.FieldCIWaitKey:      e2eCIKey,
		beadsapi.FieldCIWaitTask:     e2eCITask,
		beadsapi.FieldCIWaitDeadline: e2eCIT0.Add(2 * time.Hour).Format(time.RFC3339),
	}
	for k, v := range extra {
		fields[k] = v
	}
	c.d.add(&beadsapi.BeadDetail{ID: e2eCIAgentBead, Type: "agent", Status: "closed", Title: e2eCIAgent, Fields: fields})
}

// sweep runs one resolver tick at t0+d and returns the parked agent bead.
func (c *ciE2E) sweep(d time.Duration) *beadsapi.BeadDetail {
	c.t.Helper()
	c.at(d)
	c.ciw.sweepOnce(context.Background())
	b, err := c.d.GetBead(context.Background(), e2eCIAgentBead)
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

func (c *ciE2E) taskComments() []string {
	var out []string
	for _, cm := range c.d.comments {
		if cm.bead == e2eCITask {
			out = append(out, cm.text)
		}
	}
	return out
}

func requireState(t *testing.T, b *beadsapi.BeadDetail, want, when string) {
	t.Helper()
	if got := b.Fields[beadsapi.FieldCIWaitState]; got != want {
		t.Fatalf("%s: ci_wait_state = %q, want %q", when, got, want)
	}
}

func requireContains(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, s)
		}
	}
}

// wantRedResult is the conclusion of the push both fixtures' completions report.
var wantRedResult = map[string]ciwait.Result{
	"ci":     {Name: "ci", RunID: e2eCIRunCI, RunAttempt: 1, Conclusion: "success"},
	"checks": {Name: "checks", RunID: e2eCIRunChecks, RunAttempt: 1, Conclusion: "failure"},
}

func requireRedResult(t *testing.T, raw string) {
	t.Helper()
	got := ciResults(t, raw)
	if len(got) != len(wantRedResult) {
		t.Fatalf("ci_wait_result = %+v, want %+v", got, wantRedResult)
	}
	for name, want := range wantRedResult {
		if got[name] != want {
			t.Errorf("ci_wait_result[%s] = %+v, want %+v", name, got[name], want)
		}
	}
}

// TestE2E_CIWait_WebhookResolvesAndWakesInPlace: a push's runs are requested
// before the agent parks, complete one red and one green, and the dormant
// identity is reopened in place once the quiesce window passes. A forged
// "success" in between is refused at the door and moves nothing.
func TestE2E_CIWait_WebhookResolvesAndWakesInPlace(t *testing.T) {
	c := newCIE2E(t, &config{}, nil)

	// The agent pushes: both runs start before it has run `gb wait ci`.
	c.deliver("workflow_run_requested_ci")
	c.at(30 * time.Second)
	c.deliver("workflow_run_in_progress_checks")
	c.at(45 * time.Second)
	c.park(dormant)

	b := c.sweep(time.Minute)
	requireState(t, b, beadsapi.CIWaitStatePending, "both runs open")
	if l, err := ciwait.Decode(b.Fields[beadsapi.FieldCIWaitLedger]); err != nil || len(l.Runs) != 2 {
		t.Fatalf("pre-wait deliveries not mirrored onto the waiter (%v): %q", err, b.Fields[beadsapi.FieldCIWaitLedger])
	}

	c.at(10 * time.Minute)
	c.deliver("workflow_run_completed_ci_success")
	requireState(t, c.sweep(12*time.Minute), beadsapi.CIWaitStatePending, "checks still running")

	// Someone without the secret claims checks went green.
	forged := strings.Replace(string(fixture(t, "workflow_run_completed_checks_failure")),
		`"conclusion": "failure"`, `"conclusion": "success"`, 1)
	if code := c.post([]byte(forged), "ciwait-e2e-forged", []byte("not-the-webhook-secret")); code != http.StatusForbidden {
		t.Fatalf("forged delivery = %d, want 403", code)
	}
	requireState(t, c.sweep(15*time.Minute), beadsapi.CIWaitStatePending, "after a forged delivery")

	c.at(21 * time.Minute)
	c.deliver("workflow_run_completed_checks_failure")
	b = c.sweep(21*time.Minute + ciwait.DefaultQuiesce/2)
	requireState(t, b, beadsapi.CIWaitStatePending, "inside the quiesce window")
	if b.Status != "closed" {
		t.Fatalf("agent woken before the wait resolved: status %q", b.Status)
	}

	b = c.sweep(21*time.Minute + ciwait.DefaultQuiesce)
	requireState(t, b, beadsapi.CIWaitStateResolved, "after the quiesce window")
	requireRedResult(t, b.Fields[beadsapi.FieldCIWaitResult])
	if b.Status != "open" || b.Fields[beadsapi.FieldWakeReason] != beadsapi.WakeReasonCI {
		t.Errorf("dormant identity not woken in place: status %q wake_reason %q", b.Status, b.Fields[beadsapi.FieldWakeReason])
	}
	if len(c.d.spawns) != 0 {
		t.Errorf("woken in place but also spawned: %+v", c.d.spawns)
	}
	nudges := c.taskComments()
	if len(nudges) != 1 {
		t.Fatalf("task nudges = %q, want one", nudges)
	}
	requireContains(t, "task nudge", nudges[0],
		"CI concluded RED on "+e2eCIKey,
		"Red: checks (run 18850012): failure.",
		"Green: ci (run 18850011).",
		"gb wait ci --result")

	// GitHub redelivers the last completion (same GUID): acknowledged, and the
	// concluded wait is neither rewritten nor woken again.
	if code := c.post(fixture(t, "workflow_run_completed_checks_failure"), "ciwait-e2e-"+strconv.Itoa(c.n), testSecret); code != http.StatusOK {
		t.Errorf("redelivery = %d, want 200", code)
	}
	result := b.Fields[beadsapi.FieldCIWaitResult]
	b = c.sweep(40 * time.Minute)
	if b.Fields[beadsapi.FieldCIWaitResult] != result || len(c.taskComments()) != 1 || len(c.d.spawns) != 0 {
		t.Errorf("a concluded wait was settled again: result %q, nudges %d, spawns %d",
			b.Fields[beadsapi.FieldCIWaitResult], len(c.taskComments()), len(c.d.spawns))
	}
}

// fakeActions is GitHub's Actions API for one sha: GET
// /repos/{repo}/actions/runs?head_sha= answers with the workflow_run objects of
// the named fixtures, the same run objects the deliveries carry.
type fakeActions struct {
	t     *testing.T
	mu    sync.Mutex
	runs  []string // fixture names
	calls int
}

func (f *fakeActions) serve(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = names
}

func (f *fakeActions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeActions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/repos/"+e2eCIRepo+"/actions/runs" ||
		r.URL.Query().Get("head_sha") != e2eCISHA {
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "no installation token", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var runs []json.RawMessage
	for _, name := range f.runs {
		var p struct {
			WorkflowRun json.RawMessage `json:"workflow_run"`
		}
		if err := json.Unmarshal(fixture(f.t, name), &p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		runs = append(runs, p.WorkflowRun)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "workflow_runs": runs})
}

// TestE2E_CIWait_PollFallbackResolvesWithoutTheWebhook: the runs' requested
// deliveries arrive, then the webhook goes quiet (bridge down, or GitHub never
// retried) and neither completion is delivered. The Actions poll alone sees
// them, and the agent, stopped for good, is spawned fresh with the result in
// its prompt.
func TestE2E_CIWait_PollFallbackResolvesWithoutTheWebhook(t *testing.T) {
	gh := &fakeActions{t: t}
	api := httptest.NewServer(gh)
	t.Cleanup(api.Close)
	cfg := &config{ciWaitPollGrace: time.Minute, ciWaitPollInterval: 3 * time.Minute}
	c := newCIE2E(t, cfg, &githubCommenter{tokens: stubTokens{}, baseURL: api.URL, hc: api.Client()})

	c.deliver("workflow_run_requested_ci")
	c.deliver("workflow_run_in_progress_checks")
	gh.serve("workflow_run_requested_ci", "workflow_run_in_progress_checks")
	c.park(nil) // a terminal stop: no successor to reopen

	requireState(t, c.sweep(30*time.Second), beadsapi.CIWaitStatePending, "inside the poll grace")
	if gh.count() != 0 {
		t.Fatalf("polled inside the grace: %d calls", gh.count())
	}
	// The grace counts from the first sweep that saw the waiter (30s).
	requireState(t, c.sweep(90*time.Second), beadsapi.CIWaitStatePending, "first poll, runs open")
	if gh.count() != 1 {
		t.Fatalf("calls after the grace = %d, want 1", gh.count())
	}

	// GitHub completes both runs; no delivery reaches the bridge.
	gh.serve("workflow_run_completed_ci_success", "workflow_run_completed_checks_failure")
	requireState(t, c.sweep(2*time.Minute), beadsapi.CIWaitStatePending, "inside the poll interval")
	if gh.count() != 1 {
		t.Fatalf("polled again inside the interval: %d calls", gh.count())
	}
	requireState(t, c.sweep(4*time.Minute+30*time.Second), beadsapi.CIWaitStatePending, "completions just polled, quiescing")
	if gh.count() != 2 {
		t.Fatalf("calls after the interval = %d, want 2", gh.count())
	}

	b := c.sweep(4*time.Minute + 30*time.Second + ciwait.DefaultQuiesce)
	requireState(t, b, beadsapi.CIWaitStateResolved, "after the quiesce window")
	requireRedResult(t, b.Fields[beadsapi.FieldCIWaitResult])
	if gh.count() != 3 {
		t.Errorf("calls = %d, want 3 (the last confirms nothing new was requested)", gh.count())
	}

	if len(c.d.spawns) != 1 {
		t.Fatalf("spawns = %+v, want the stopped agent spawned once", c.d.spawns)
	}
	s := c.d.spawns[0]
	if s.name != e2eCIAgent || s.task != e2eCITask || s.project != "gardenio" || s.role != "crew" {
		t.Errorf("spawn = %+v, want %s on %s", s, e2eCIAgent, e2eCITask)
	}
	if s.fields[beadsapi.FieldWakeReason] != beadsapi.WakeReasonCI || s.fields["spawn_source"] != ciWaitSpawnSource {
		t.Errorf("spawn fields = %v, want wake_reason=ci spawn_source=%s", s.fields, ciWaitSpawnSource)
	}
	requireContains(t, "spawn prompt", s.prompt,
		"CI concluded RED on "+e2eCIKey, `"conclusion":"failure"`, "kd show "+e2eCITask)
	nudges := c.taskComments()
	if len(nudges) != 1 {
		t.Fatalf("task nudges = %q, want one", nudges)
	}
	if strings.Contains(nudges[0], "gb wait ci --result") {
		t.Errorf("a fresh spawn has no wait to read, but the nudge says to: %q", nudges[0])
	}
}

// TestE2E_CIWait_DeadlineTimesOutAndStillWakes: CI never concludes. At the
// deadline the wait times out with each run's last status, and the agent is
// still woken, so a lost run never strands its task.
func TestE2E_CIWait_DeadlineTimesOutAndStillWakes(t *testing.T) {
	c := newCIE2E(t, &config{}, nil)
	c.deliver("workflow_run_requested_ci")
	c.deliver("workflow_run_in_progress_checks")
	c.park(dormant)

	b := c.sweep(2*time.Hour - time.Second)
	requireState(t, b, beadsapi.CIWaitStatePending, "just before the deadline")
	if b.Status != "closed" || len(c.taskComments()) != 0 {
		t.Fatalf("woken before the deadline: status %q, nudges %q", b.Status, c.taskComments())
	}

	b = c.sweep(2 * time.Hour)
	requireState(t, b, beadsapi.CIWaitStateTimedOut, "at the deadline")
	res := ciResults(t, b.Fields[beadsapi.FieldCIWaitResult])
	if res["ci"].Conclusion != "queued" || res["checks"].Conclusion != "in_progress" {
		t.Errorf("timed-out result should carry each run's last status: %+v", res)
	}
	if b.Status != "open" || b.Fields[beadsapi.FieldWakeReason] != beadsapi.WakeReasonCI {
		t.Errorf("timed-out wait did not wake the dormant identity: status %q wake_reason %q", b.Status, b.Fields[beadsapi.FieldWakeReason])
	}
	nudges := c.taskComments()
	if len(nudges) != 1 {
		t.Fatalf("task nudges = %q, want one", nudges)
	}
	requireContains(t, "task nudge", nudges[0],
		"CI wait on "+e2eCIKey+" TIMED OUT",
		"ci (run 18850011): queued",
		"checks (run 18850012): in_progress")
}
