package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// stripOrderBy must remove the trailing ORDER BY clause buildJQL always appends
// (the /search/approximate-count endpoint 400s on ORDER BY), while leaving a
// JQL that has none untouched and not clobbering an inner substring.
func TestStripOrderBy(t *testing.T) {
	cases := map[string]string{
		`project IN ("PE") AND status IN ("To Do") ORDER BY created DESC`: `project IN ("PE") AND status IN ("To Do")`,
		`project IN ("PE") order by created desc`:                         `project IN ("PE")`, // case-insensitive
		`project IN ("PE") AND status IN ("To Do")`:                       `project IN ("PE") AND status IN ("To Do")`, // no clause
		``: ``,
	}
	for in, want := range cases {
		if got := stripOrderBy(in); got != want {
			t.Errorf("stripOrderBy(%q) = %q, want %q", in, got, want)
		}
	}
}

// stallTestPoller builds a poller whose only live dependency is a JiraClient
// pointed at the given approximate-count handler. recordPollOutcome uses only
// p.jira and p.cfg.Logger, so the daemon can be nil.
func stallTestPoller(t *testing.T, approxCount int64, approxStatus int, gotJQL *atomic.Value) *JiraPoller {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/search/approximate-count" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		var body struct {
			JQL string `json:"jql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if gotJQL != nil {
			gotJQL.Store(body.JQL)
		}
		if approxStatus != http.StatusOK {
			http.Error(w, "boom", approxStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int64{"count": approxCount})
	}))
	t.Cleanup(srv.Close)

	return &JiraPoller{
		jira: NewJiraClient(JiraClientConfig{BaseURL: srv.URL, Email: "e@x", APIToken: "tok"}),
		cfg:  JiraPollerConfig{Logger: slog.New(slog.NewTextHandler(nopWriter{}, nil))},
	}
}

// A poll cycle that returned rows is never a stall — no approximate-count call
// is made, and the verdict is cleared to 0.
func TestRecordPollOutcome_RowsReturned_NotStalled(t *testing.T) {
	jiraPollerStalled.Set(1) // ensure a prior tripped state is cleared
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]int64{"count": 999})
	}))
	defer srv.Close()
	p := &JiraPoller{
		jira: NewJiraClient(JiraClientConfig{BaseURL: srv.URL, Email: "e", APIToken: "t"}),
		cfg:  JiraPollerConfig{Logger: slog.New(slog.NewTextHandler(nopWriter{}, nil))},
	}

	p.recordPollOutcome(context.Background(), `project IN ("PE") ORDER BY created DESC`, 3)

	if called.Load() {
		t.Error("approximate-count must NOT be called when the search returned rows")
	}
	if got := testutil.ToFloat64(jiraPollerStalled); got != 0 {
		t.Errorf("stalled gauge = %v, want 0 (rows returned)", got)
	}
}

// Zero rows AND approximate-count > 0 is the silent-stall signature: matter
// exists upstream but the ingestion search is blind to it. The gauge trips to 1
// and the probe JQL has its ORDER BY stripped.
func TestRecordPollOutcome_ZeroRowsWithUpstreamMatches_Stalled(t *testing.T) {
	jiraPollerStalled.Set(0)
	var gotJQL atomic.Value
	p := stallTestPoller(t, 7, http.StatusOK, &gotJQL)

	p.recordPollOutcome(context.Background(), `project IN ("PE") AND status IN ("To Do") ORDER BY created DESC`, 0)

	if got := testutil.ToFloat64(jiraPollerStalled); got != 1 {
		t.Errorf("stalled gauge = %v, want 1 (zero rows, upstream matches)", got)
	}
	if got := testutil.ToFloat64(jiraPollerUpstreamMatches); got != 7 {
		t.Errorf("upstream_matches gauge = %v, want 7", got)
	}
	if jql, _ := gotJQL.Load().(string); jql != `project IN ("PE") AND status IN ("To Do")` {
		t.Errorf("approximate-count JQL = %q, want ORDER BY stripped", jql)
	}
}

// Zero rows AND a genuinely empty upstream (approximate-count == 0) is healthy —
// nothing is being missed. The gauge stays 0.
func TestRecordPollOutcome_ZeroRowsEmptyUpstream_NotStalled(t *testing.T) {
	jiraPollerStalled.Set(1) // must be cleared even from a prior tripped state
	p := stallTestPoller(t, 0, http.StatusOK, nil)

	p.recordPollOutcome(context.Background(), `project IN ("PE") ORDER BY created DESC`, 0)

	if got := testutil.ToFloat64(jiraPollerStalled); got != 0 {
		t.Errorf("stalled gauge = %v, want 0 (empty upstream)", got)
	}
}

// A count-endpoint error on a zero-row cycle must NOT page: the stall cannot be
// confirmed, and a real stall persists to the next cycle. The gauge clears to 0.
func TestRecordPollOutcome_ZeroRowsCountError_NotStalled(t *testing.T) {
	jiraPollerStalled.Set(1)
	p := stallTestPoller(t, 0, http.StatusInternalServerError, nil)

	p.recordPollOutcome(context.Background(), `project IN ("PE") ORDER BY created DESC`, 0)

	if got := testutil.ToFloat64(jiraPollerStalled); got != 0 {
		t.Errorf("stalled gauge = %v, want 0 (unconfirmed count error must not page)", got)
	}
}

// Every ok poll increments the poll counter (the deadman baseline).
func TestRecordPollOutcome_CountsOkPoll(t *testing.T) {
	jiraPollerPolls.Reset()
	p := stallTestPoller(t, 0, http.StatusOK, nil)

	p.recordPollOutcome(context.Background(), `project IN ("PE") ORDER BY created DESC`, 0)
	p.recordPollOutcome(context.Background(), `project IN ("PE") ORDER BY created DESC`, 5)

	if got := testutil.ToFloat64(jiraPollerPolls.WithLabelValues(pollResultOK)); got != 2 {
		t.Errorf("ok poll counter = %v, want 2", got)
	}
}

// initPollerMetrics touches every counter label so increase()/absent() have a
// baseline the instant the pod is first scraped.
func TestInitPollerMetrics_BaselinesCounterLabels(t *testing.T) {
	jiraPollerPolls.Reset()
	initPollerMetrics()
	for _, result := range []string{pollResultOK, pollResultSearchError, pollResultNotCaughtUp} {
		if got := testutil.ToFloat64(jiraPollerPolls.WithLabelValues(result)); got != 0 {
			t.Errorf("polls_total{result=%q} baseline = %v, want 0 (series must exist)", result, got)
		}
	}
}
