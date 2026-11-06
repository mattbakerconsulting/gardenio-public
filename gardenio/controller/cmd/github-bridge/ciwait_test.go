package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/ciwait"
)

const (
	ciRepo = "mattbakerconsulting/gardenio"
	ciSHA  = "82a7367954e1805b80695fca3e5f2cd9a797b689"
	ciKey  = ciRepo + "@" + ciSHA
)

var ciT0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

type ciHarness struct {
	t     *testing.T
	d     *memDaemon
	h     *ciWaitHandler
	clock time.Time
}

func newCIHarness(t *testing.T, d *memDaemon) *ciHarness {
	c := &ciHarness{t: t, d: d, clock: ciT0}
	c.h = newCIWaitHandler(d, 0, 0, quiet())
	c.h.now = func() time.Time { return c.clock }
	return c
}

func (c *ciHarness) at(d time.Duration) { c.clock = ciT0.Add(d) }

// deliver hands the handler a workflow_run delivery for sha on repo.
func (c *ciHarness) deliver(repo, sha string, id int64, name string, attempt int, status, conclusion string) {
	c.t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"action": status,
		"workflow_run": map[string]any{
			"id": id, "name": name, "head_sha": sha, "run_attempt": attempt,
			"status": status, "conclusion": conclusion,
		},
		"repository": map[string]any{"full_name": repo},
	})
	ev := Event{DeliveryID: "d", Type: "workflow_run", Action: status, Repo: repo, Payload: payload}
	if err := c.h.Handle(context.Background(), ev); err != nil {
		c.t.Fatalf("Handle: %v", err)
	}
}

// waiter adds a closed agent bead parked on key, the shape `gb wait ci` +
// `gb stop` leave behind.
func (c *ciHarness) waiter(id, key, workflows string) {
	c.d.add(&beadsapi.BeadDetail{
		ID: id, Type: "agent", Status: "closed", Title: "agent-" + id,
		Fields: map[string]string{
			"agent":                       "agent-" + id,
			beadsapi.FieldCIWaitState:     beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:       key,
			beadsapi.FieldCIWaitWorkflows: workflows,
			beadsapi.FieldCIWaitDeadline:  ciT0.Add(2 * time.Hour).Format(time.RFC3339),
		},
	})
}

func (c *ciHarness) fields(id string) map[string]string {
	c.t.Helper()
	b, err := c.d.GetBead(context.Background(), id)
	if err != nil {
		c.t.Fatal(err)
	}
	return b.Fields
}

func ciResults(t *testing.T, raw string) map[string]ciwait.Result {
	t.Helper()
	var rs []ciwait.Result
	if err := json.Unmarshal([]byte(raw), &rs); err != nil {
		t.Fatalf("ci_wait_result %q: %v", raw, err)
	}
	out := map[string]ciwait.Result{}
	for _, r := range rs {
		out[r.Name] = r
	}
	return out
}

// TestCIWait_ResolvesAfterEveryRunCompletesAndQuiesces walks one push: runs
// requested before the agent waits, one finishing red, one green, then the
// quiesce window.
func TestCIWait_ResolvesAfterEveryRunCompletesAndQuiesces(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	ctx := context.Background()

	// Pushed: both runs are requested before the agent has run `gb wait ci`.
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "requested", "")
	c.deliver(ciRepo, ciSHA, 102, "checks", 1, "requested", "")
	c.at(20 * time.Second)
	c.waiter("gd-agentA", ciKey, "")

	c.at(5 * time.Minute)
	c.deliver(ciRepo, ciSHA, 102, "checks", 1, "completed", "success")
	c.at(7 * time.Minute)
	c.h.sweepOnce(ctx)
	f := c.fields("gd-agentA")
	if f[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStatePending {
		t.Fatalf("ci is still running: state = %q, want pending", f[beadsapi.FieldCIWaitState])
	}
	persisted, err := ciwait.Decode(f[beadsapi.FieldCIWaitLedger])
	if err != nil || len(persisted.Runs) != 2 {
		t.Fatalf("ledger not mirrored onto the waiter (%v): %q", err, f[beadsapi.FieldCIWaitLedger])
	}

	c.at(20 * time.Minute)
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "failure")
	c.at(20*time.Minute + 30*time.Second)
	c.h.sweepOnce(ctx)
	if got := c.fields("gd-agentA")[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStatePending {
		t.Fatalf("inside the quiesce window: state = %q, want pending", got)
	}

	c.at(21 * time.Minute)
	b, _ := c.d.GetBead(ctx, "gd-agentA")
	wrote := c.h.apply(ctx, b)
	if wrote[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateResolved || wrote[beadsapi.FieldCIWaitResult] == "" {
		t.Fatalf("resolution PATCH = %v, want state and result in one write", wrote)
	}
	res := ciResults(t, c.fields("gd-agentA")[beadsapi.FieldCIWaitResult])
	if res["ci"] != (ciwait.Result{Name: "ci", RunID: 101, RunAttempt: 1, Conclusion: "failure"}) ||
		res["checks"] != (ciwait.Result{Name: "checks", RunID: 102, RunAttempt: 1, Conclusion: "success"}) {
		t.Errorf("ci_wait_result = %+v", res)
	}

	// A redelivery after resolution writes nothing: the wait is no longer
	// pending, so the sweep does not even see it.
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "failure")
	c.at(30 * time.Minute)
	before := c.fields("gd-agentA")[beadsapi.FieldCIWaitResult]
	c.h.sweepOnce(ctx)
	if c.fields("gd-agentA")[beadsapi.FieldCIWaitResult] != before {
		t.Error("a resolved wait was rewritten")
	}
}

func TestCIWait_RestartRecoversThePersistedLedger(t *testing.T) {
	d := newMemDaemon()
	ctx := context.Background()
	c := newCIHarness(t, d)
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "in_progress", "")
	c.deliver(ciRepo, ciSHA, 102, "checks", 1, "in_progress", "")
	c.waiter("gd-agentA", ciKey, "")
	c.at(time.Minute)
	c.h.sweepOnce(ctx) // mirrors the ledger onto the bead

	// The bridge restarts: a fresh handler with an empty memory.
	c2 := newCIHarness(t, d)
	c2.at(10 * time.Minute)
	c2.deliver(ciRepo, ciSHA, 102, "checks", 1, "completed", "success")
	c2.at(12 * time.Minute)
	c2.h.sweepOnce(ctx)
	if got := c2.fields("gd-agentA")[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStatePending {
		t.Fatalf("ci was in progress before the restart: state = %q, want pending", got)
	}
	c2.at(15 * time.Minute)
	c2.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c2.at(17 * time.Minute)
	c2.h.sweepOnce(ctx)
	f := c2.fields("gd-agentA")
	if f[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateResolved {
		t.Fatalf("state = %q, want resolved", f[beadsapi.FieldCIWaitState])
	}
	if res := ciResults(t, f[beadsapi.FieldCIWaitResult]); len(res) != 2 {
		t.Errorf("result lost a run across the restart: %+v", res)
	}
}

func TestCIWait_LateWaiterOnAFinishedSHAResolves(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.at(10 * time.Minute)
	c.waiter("gd-agentA", ciKey, "")
	c.h.sweepOnce(context.Background())
	if got := c.fields("gd-agentA")[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStateResolved {
		t.Errorf("CI finished before the wait was recorded: state = %q, want resolved", got)
	}
}

func TestCIWait_MatchesOnlyItsOwnKey(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	const otherSHA = "0123456789abcdef0123456789abcdef01234567"
	// GitHub reports the canonical repo case; the agent typed it lower-cased.
	c.deliver("MattBakerConsulting/Gardenio", ciSHA, 101, "ci", 1, "completed", "success")
	c.waiter("gd-agentA", "mattbakerconsulting/gardenio@"+ciSHA, "")
	c.waiter("gd-agentB", ciRepo+"@"+otherSHA, "")
	c.at(5 * time.Minute)
	c.h.sweepOnce(context.Background())
	if got := c.fields("gd-agentA")[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStateResolved {
		t.Errorf("repo case differs from the delivery: state = %q, want resolved", got)
	}
	fb := c.fields("gd-agentB")
	if fb[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStatePending || fb[beadsapi.FieldCIWaitLedger] != "" {
		t.Errorf("a waiter on another sha was touched: %v", fb)
	}
}

func TestCIWait_WorkflowFilter(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.deliver(ciRepo, ciSHA, 102, "lands-perf", 1, "in_progress", "")
	c.waiter("gd-agentA", ciKey, "ci")
	c.waiter("gd-agentB", ciKey, "")
	c.at(5 * time.Minute)
	c.h.sweepOnce(context.Background())
	if got := c.fields("gd-agentA")[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStateResolved {
		t.Errorf("--workflow ci: state = %q, want resolved", got)
	}
	if got := c.fields("gd-agentB")[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStatePending {
		t.Errorf("no filter, lands-perf running: state = %q, want pending", got)
	}
}

func TestCIWait_DeadlineTimesOut(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "queued", "")
	c.waiter("gd-agentA", ciKey, "")
	c.at(2 * time.Hour)
	c.h.sweepOnce(context.Background())
	f := c.fields("gd-agentA")
	if f[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateTimedOut {
		t.Fatalf("state = %q, want timed_out", f[beadsapi.FieldCIWaitState])
	}
	if res := ciResults(t, f[beadsapi.FieldCIWaitResult]); res["ci"].Conclusion != "queued" {
		t.Errorf("timed-out result should report the run's status: %+v", res)
	}
}

func TestCIWait_NoLedgerWaitsThenTimesOut(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.waiter("gd-agentA", ciKey, "")
	c.at(time.Hour)
	c.h.sweepOnce(context.Background())
	if f := c.fields("gd-agentA"); f[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStatePending || f[beadsapi.FieldCIWaitLedger] != "" {
		t.Fatalf("nothing seen for the sha, before the deadline: want no write: %v", f)
	}
	// Past the deadline with nothing ever seen: a lost wait must not strand
	// its task, so it times out like any other (plan §2.5).
	c.at(3 * time.Hour)
	c.h.sweepOnce(context.Background())
	f := c.fields("gd-agentA")
	if f[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateTimedOut || f[beadsapi.FieldCIWaitResult] != "[]" {
		t.Errorf("state = %q result = %q, want timed_out with an empty result", f[beadsapi.FieldCIWaitState], f[beadsapi.FieldCIWaitResult])
	}
	if f[beadsapi.FieldCIWaitLedger] != "" {
		t.Errorf("an empty ledger was mirrored: %q", f[beadsapi.FieldCIWaitLedger])
	}
}

func TestCIWait_IgnoresOtherEventsAndBadPayloads(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	ctx := context.Background()
	for _, ev := range []Event{
		{Type: "check_suite", Action: "completed", Repo: ciRepo, Payload: []byte(`{"check_suite":{"id":1,"head_sha":"` + ciSHA + `"}}`)},
		{Type: "workflow_run", Action: "completed", Repo: ciRepo, Payload: []byte(`{not json`)},
		{Type: "workflow_run", Action: "completed", Repo: ciRepo, Payload: []byte(`{"workflow_run":{"id":1,"name":"ci","head_sha":"82a7367","status":"completed"}}`)},
		{Type: "workflow_run", Action: "completed", Repo: "", Payload: []byte(`{"workflow_run":{"id":1,"name":"ci","head_sha":"` + ciSHA + `","status":"completed"}}`)},
	} {
		if err := c.h.Handle(ctx, ev); err != nil {
			t.Errorf("Handle(%s) = %v, want nil (never fail a delivery)", ev.Type, err)
		}
	}
	if len(c.h.ledgers) != 0 {
		t.Errorf("ledgers = %v, want none", c.h.ledgers)
	}
}

// failingUpdates fails UpdateBeadFields until ok is set.
type failingUpdates struct {
	*memDaemon
	ok bool
}

func (f *failingUpdates) UpdateBeadFields(ctx context.Context, id string, fields map[string]string) error {
	if !f.ok {
		return errors.New("daemon unavailable")
	}
	return f.memDaemon.UpdateBeadFields(ctx, id, fields)
}

func TestCIWait_FailedWriteIsRetriedNextSweep(t *testing.T) {
	d := newMemDaemon()
	fu := &failingUpdates{memDaemon: d}
	c := newCIHarness(t, d)
	c.h.beads = fu
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.waiter("gd-agentA", ciKey, "")
	c.at(5 * time.Minute)
	c.h.sweepOnce(context.Background())
	if got := c.fields("gd-agentA")[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStatePending {
		t.Fatalf("write failed: state = %q, want still pending", got)
	}
	fu.ok = true
	c.h.sweepOnce(context.Background())
	if got := c.fields("gd-agentA")[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStateResolved {
		t.Errorf("retry: state = %q, want resolved", got)
	}
}

func TestCIWait_ExpiresStaleLedgers(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.at(ciWaitLedgerTTL + time.Minute)
	c.h.sweepOnce(context.Background())
	if len(c.h.ledgers) != 0 {
		t.Errorf("ledger kept past its TTL: %v", c.h.ledgers)
	}
}
