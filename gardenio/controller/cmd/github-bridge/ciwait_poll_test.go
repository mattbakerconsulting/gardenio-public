package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/ciwait"
)

// fakeRuns is the Actions listing the poll fallback sees.
type fakeRuns struct {
	runs  []ciwait.Run
	err   error
	calls int
	asked string
}

func (f *fakeRuns) WorkflowRuns(_ context.Context, repo, sha string) ([]ciwait.Run, error) {
	f.calls++
	f.asked = repo + "@" + sha
	return f.runs, f.err
}

func newPollHarness(t *testing.T) (*ciHarness, *fakeRuns) {
	c := newCIHarness(t, newMemDaemon())
	f := &fakeRuns{}
	c.h.runs = f
	return c, f
}

func (c *ciHarness) sweepAt(d time.Duration) map[string]string {
	c.at(d)
	c.h.sweepOnce(context.Background())
	return c.fields("gd-agentA")
}

// TestCIWaitPoll_ResolvesWithNoDeliveries is the missed-webhook case: not one
// workflow_run delivery for the sha, and the wait still resolves from the
// Actions listing alone.
func TestCIWaitPoll_ResolvesWithNoDeliveries(t *testing.T) {
	c, f := newPollHarness(t)
	c.waiter("gd-agentA", ciKey, "")

	c.sweepAt(0)
	if f.calls != 0 {
		t.Fatalf("polled inside the grace period: %d calls", f.calls)
	}
	f.runs = []ciwait.Run{{ID: 101, Name: "ci", Attempt: 1, Status: "in_progress"}}
	if st := c.sweepAt(defaultCIWaitPollGrace)[beadsapi.FieldCIWaitState]; st != beadsapi.CIWaitStatePending || f.calls != 1 {
		t.Fatalf("after the grace: state = %q, calls = %d; want pending after one poll", st, f.calls)
	}
	if f.asked != ciKey {
		t.Errorf("polled %q, want %q", f.asked, ciKey)
	}
	c.sweepAt(4 * time.Minute)
	if f.calls != 1 {
		t.Fatalf("polled again inside the interval: %d calls", f.calls)
	}

	f.runs = []ciwait.Run{{ID: 101, Name: "ci", Attempt: 1, Status: "completed", Conclusion: "failure"}}
	at := defaultCIWaitPollGrace + defaultCIWaitPollInterval
	if st := c.sweepAt(at)[beadsapi.FieldCIWaitState]; st != beadsapi.CIWaitStatePending || f.calls != 2 {
		t.Fatalf("completion seen by a poll settles first: state = %q, calls = %d", st, f.calls)
	}
	fl := c.sweepAt(at + ciwait.DefaultQuiesce)
	if fl[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateResolved {
		t.Fatalf("after the quiesce: state = %q, want resolved", fl[beadsapi.FieldCIWaitState])
	}
	if f.calls != 3 {
		t.Errorf("calls = %d, want 3 (the last one confirms nothing new was requested)", f.calls)
	}
	if res := ciResults(t, fl[beadsapi.FieldCIWaitResult]); res["ci"] != (ciwait.Result{Name: "ci", RunID: 101, RunAttempt: 1, Conclusion: "failure"}) {
		t.Errorf("ci_wait_result = %+v", res)
	}
}

// TestCIWaitPoll_ConfirmPollCatchesAMissedRun: the webhook reported one run
// and lost another. Without the confirm poll the wait would resolve green on
// the first run alone.
func TestCIWaitPoll_ConfirmPollCatchesAMissedRun(t *testing.T) {
	c, f := newPollHarness(t)
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.waiter("gd-agentA", ciKey, "")
	f.runs = []ciwait.Run{
		{ID: 101, Name: "ci", Attempt: 1, Status: "completed", Conclusion: "success"},
		{ID: 102, Name: "checks", Attempt: 1, Status: "in_progress"},
	}
	if st := c.sweepAt(time.Minute)[beadsapi.FieldCIWaitState]; st != beadsapi.CIWaitStatePending || f.calls != 1 {
		t.Fatalf("checks is still running: state = %q, calls = %d; want pending after a confirm poll", st, f.calls)
	}

	f.runs[1] = ciwait.Run{ID: 102, Name: "checks", Attempt: 1, Status: "completed", Conclusion: "failure"}
	at := time.Minute + defaultCIWaitPollInterval
	c.sweepAt(at)
	fl := c.sweepAt(at + ciwait.DefaultQuiesce)
	if fl[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateResolved {
		t.Fatalf("state = %q, want resolved", fl[beadsapi.FieldCIWaitState])
	}
	if res := ciResults(t, fl[beadsapi.FieldCIWaitResult]); res["checks"].Conclusion != "failure" || res["ci"].Conclusion != "success" {
		t.Errorf("ci_wait_result = %+v", res)
	}
}

// TestCIWaitPoll_ConfirmedLedgerResolvesWithoutAnotherPoll: once a poll has
// looked at the sha since the ledger last changed, resolving costs no request.
func TestCIWaitPoll_ConfirmedLedgerResolvesWithoutAnotherPoll(t *testing.T) {
	c, f := newPollHarness(t)
	f.runs = []ciwait.Run{{ID: 101, Name: "ci", Attempt: 1, Status: "in_progress"}}
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "in_progress", "")
	c.waiter("gd-agentA", ciKey, "")
	c.sweepAt(0)
	c.at(90 * time.Second)
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	f.runs[0] = ciwait.Run{ID: 101, Name: "ci", Attempt: 1, Status: "completed", Conclusion: "success"}
	// The periodic poll lands after the delivery and finds nothing new.
	if st := c.sweepAt(defaultCIWaitPollGrace)[beadsapi.FieldCIWaitState]; st != beadsapi.CIWaitStatePending || f.calls != 1 {
		t.Fatalf("inside the quiesce: state = %q, calls = %d; want pending after one poll", st, f.calls)
	}
	if st := c.sweepAt(90*time.Second + ciwait.DefaultQuiesce)[beadsapi.FieldCIWaitState]; st != beadsapi.CIWaitStateResolved {
		t.Fatalf("state = %q, want resolved", st)
	}
	if f.calls != 1 {
		t.Errorf("calls = %d, want no confirm poll on a ledger the last poll already saw", f.calls)
	}
}

func TestCIWaitPoll_FailuresNeitherHammerNorHoldBack(t *testing.T) {
	c, f := newPollHarness(t)
	f.err = errors.New("HTTP 502")
	c.waiter("gd-agentA", ciKey, "")
	c.sweepAt(0)
	for _, d := range []time.Duration{defaultCIWaitPollGrace, defaultCIWaitPollGrace + 15*time.Second, 4 * time.Minute} {
		c.sweepAt(d)
	}
	if f.calls != 1 {
		t.Fatalf("a failing API was asked %d times inside one interval, want 1", f.calls)
	}

	// The webhook still works: a failed confirm poll does not hold the
	// resolution back.
	c.at(5 * time.Minute)
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	if st := c.sweepAt(6*time.Minute + 30*time.Second)[beadsapi.FieldCIWaitState]; st != beadsapi.CIWaitStateResolved {
		t.Errorf("state = %q, want resolved", st)
	}
	if f.calls != 2 {
		t.Errorf("calls = %d, want 2 (one periodic, then the confirm)", f.calls)
	}
}

func TestGitHubWorkflowRuns(t *testing.T) {
	const otherSHA = "0123456789abcdef0123456789abcdef01234567"
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+ciRepo+"/actions/runs" || r.URL.Query().Get("head_sha") != ciSHA {
			http.Error(w, "unexpected "+r.URL.String(), http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		var runs []map[string]any
		if page == "1" {
			for i := 0; i < ciWaitRunsPerPage; i++ {
				runs = append(runs, map[string]any{"id": 1000 + i, "name": fmt.Sprintf("wf-%d", i), "head_sha": ciSHA,
					"run_attempt": 1, "status": "completed", "conclusion": "success"})
			}
			runs[0]["head_sha"] = otherSHA // GitHub's filter is trusted, but not blindly
		} else {
			runs = append(runs, map[string]any{"id": 2000, "name": "ci", "head_sha": ciSHA, "run_attempt": 2, "status": "in_progress", "conclusion": nil})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": ciWaitRunsPerPage + 1, "workflow_runs": runs})
	}))
	defer srv.Close()

	c := &githubCommenter{tokens: stubTokens{}, baseURL: srv.URL, hc: srv.Client()}
	runs, err := c.WorkflowRuns(context.Background(), ciRepo, ciSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Errorf("pages fetched = %v, want 2", pages)
	}
	if len(runs) != ciWaitRunsPerPage {
		t.Fatalf("runs = %d, want %d (the other sha's run dropped)", len(runs), ciWaitRunsPerPage)
	}
	if last := runs[len(runs)-1]; last != (ciwait.Run{ID: 2000, Name: "ci", Attempt: 2, Status: "in_progress"}) {
		t.Errorf("last run = %+v", last)
	}
}
