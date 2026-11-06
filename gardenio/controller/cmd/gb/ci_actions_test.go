package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var ciTestNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// fakeActionsGH answers the gh api calls gb ci rerun / dispatch make, from a
// fixed run, branch head and branch run history.
type fakeActionsGH struct {
	run      map[string]any            // GET actions/runs/<id>
	head     string                    // GET branches/<b>
	history  []map[string]any          // GET actions/runs?branch=
	attempts map[string]map[string]any // GET runs/<id>/attempts/<n>, key "<id>/<n>"
	calls    [][]string
}

func (g *fakeActionsGH) call(_ context.Context, args ...string) (string, error) {
	g.calls = append(g.calls, args)
	if len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
		return "mattbakerconsulting/gardenio\n", nil
	}
	if len(args) < 2 || args[0] != "api" {
		return "", fmt.Errorf("fakeActionsGH: unexpected gh %v", args)
	}
	if slices.Contains(args, "POST") {
		return "", nil
	}
	path := args[1]
	var v any
	switch {
	case strings.Contains(path, "/actions/runs?branch="):
		v = map[string]any{"workflow_runs": g.history}
	case strings.Contains(path, "/attempts/"):
		_, rest, _ := strings.Cut(path, "/actions/runs/")
		v = g.attempts[strings.Replace(rest, "/attempts/", "/", 1)]
	case strings.Contains(path, "/actions/runs/"):
		v = g.run
	case strings.Contains(path, "/branches/"):
		v = map[string]any{"commit": map[string]any{"sha": g.head}}
	default:
		return "", fmt.Errorf("fakeActionsGH: unexpected path %s", path)
	}
	b, _ := json.Marshal(v)
	return string(b), nil
}

func (g *fakeActionsGH) posts() [][]string {
	var out [][]string
	for _, c := range g.calls {
		if slices.Contains(c, "POST") {
			out = append(out, c)
		}
	}
	return out
}

func ago(d time.Duration) string { return ciTestNow.Add(-d).Format(time.RFC3339) }

func failedCIRun() map[string]any {
	return map[string]any{
		"id": 41366086276, "path": ".github/workflows/ci.yml", "head_branch": "agent/x/gd-1",
		"head_sha": "abc1234def", "event": "pull_request", "status": "completed",
		"conclusion": "failure", "run_attempt": 1,
		"html_url": "https://github.com/mattbakerconsulting/gardenio/actions/runs/41366086276",
	}
}

func setupCIActionsTest(t *testing.T) (*fakePRDaemon, *fakeActionsGH) {
	t.Helper()
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	fd := &fakePRDaemon{taskFields: map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(fd.handler))
	t.Cleanup(srv.Close)
	oldDaemon, oldGH, oldNow := daemon, prGHRun, ciNow
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	gh := &fakeActionsGH{run: failedCIRun(), head: "abc1234def", attempts: map[string]map[string]any{}}
	prGHRun = gh.call
	ciNow = func() time.Time { return ciTestNow }
	t.Cleanup(func() {
		daemon, prGHRun, ciNow = oldDaemon, oldGH, oldNow
		for _, c := range []*cobra.Command{ciRerunCmd, ciDispatchCmd} {
			c.Flags().Set("repo", "")
			c.Flags().Set("task", "")
		}
		ciDispatchCmd.Flags().Set("ref", "")
		ciDispatchCmd.Flags().Lookup("field").Value.(interface{ Replace([]string) error }).Replace(nil)
	})
	return fd, gh
}

// spentHistory is a branch history worth exactly 2 actions in the last hour:
// a dispatch 10m ago and a crew re-run (attempt 3) 5m ago. The old dispatch,
// the bot's lost-runner re-run (attempt 2) and the stale re-run are free.
func spentHistory(gh *fakeActionsGH) {
	bot := map[string]any{"login": "gardenio-io[bot]"}
	gh.history = []map[string]any{
		{"id": 1, "event": "workflow_dispatch", "created_at": ago(10 * time.Minute), "updated_at": ago(9 * time.Minute), "run_attempt": 1, "actor": bot},
		{"id": 2, "event": "workflow_dispatch", "created_at": ago(2 * time.Hour), "updated_at": ago(2 * time.Hour), "run_attempt": 1, "actor": bot},
		{"id": 3, "event": "pull_request", "created_at": ago(50 * time.Minute), "updated_at": ago(5 * time.Minute), "run_attempt": 3},
		{"id": 4, "event": "pull_request", "created_at": ago(4 * time.Hour), "updated_at": ago(3 * time.Hour), "run_attempt": 2},
	}
	gh.attempts["3/2"] = map[string]any{"run_started_at": ago(30 * time.Minute), "triggering_actor": map[string]any{"login": "github-actions[bot]"}}
	gh.attempts["3/3"] = map[string]any{"run_started_at": ago(5 * time.Minute), "triggering_actor": bot}
}

func TestCIBranchSpendCountsCrewActionsInTheHour(t *testing.T) {
	_, gh := setupCIActionsTest(t)
	spentHistory(gh)
	n, err := ciBranchSpend(context.Background(), "o/r", "agent/x/gd-1", ciTestNow.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("spend = %d, want 2 (one dispatch, one crew re-run)", n)
	}
	for _, c := range gh.calls {
		if strings.Contains(c[1], "runs/4/attempts") {
			t.Errorf("read attempts of run 4, which was last updated before the window: %v", c)
		}
	}
}

func TestCIRerunReRunsFailedJobsAndNotes(t *testing.T) {
	fd, gh := setupCIActionsTest(t)
	ciRerunCmd.Flags().Set("task", "gd-task")
	out, err := runPRCmd(ciRerunCmd, "https://github.com/mattbakerconsulting/gardenio/actions/runs/41366086276/job/9")
	if err != nil {
		t.Fatal(err)
	}
	posts := gh.posts()
	if len(posts) != 1 || !strings.HasSuffix(posts[0][3], "/actions/runs/41366086276/rerun-failed-jobs") {
		t.Fatalf("POSTs = %v, want one rerun-failed-jobs", posts)
	}
	if !strings.Contains(out, "(1 of 2 this hour)") || !strings.Contains(out, "gb wait ci --repo mattbakerconsulting/gardenio --sha abc1234def") {
		t.Errorf("output = %q", out)
	}
	if len(fd.comments) != 1 || !strings.Contains(fd.comments[0], "gb ci rerun: re-ran the failed jobs of ci.yml run 41366086276") {
		t.Errorf("task comments = %v", fd.comments)
	}
}

func TestCIRerunRefusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(run map[string]any, gh *fakeActionsGH)
		want string
	}{
		{"workflow not allowlisted", func(r map[string]any, _ *fakeActionsGH) { r["path"] = ".github/workflows/e2e.yml" }, "allows only ci.yml, checks.yml, images.yml, lands-perf.yml"},
		{"main", func(r map[string]any, _ *fakeActionsGH) { r["head_branch"] = "main" }, "not an agent branch"},
		{"release tag", func(r map[string]any, _ *fakeActionsGH) { r["head_branch"] = "2026.307.1" }, "not an agent branch"},
		{"still running", func(r map[string]any, _ *fakeActionsGH) { r["status"] = "in_progress" }, "not finished"},
		{"succeeded", func(r map[string]any, _ *fakeActionsGH) { r["conclusion"] = "success" }, "nothing to re-run"},
		{"already re-run", func(r map[string]any, _ *fakeActionsGH) { r["run_attempt"] = 2 }, "already re-run"},
		{"superseded head", func(_ map[string]any, gh *fakeActionsGH) { gh.head = "fff0000aaa" }, "is now at fff0000"},
		{"budget spent", func(_ map[string]any, gh *fakeActionsGH) { spentHistory(gh) }, "2 re-runs/dispatches in the last hour"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, gh := setupCIActionsTest(t)
			tc.edit(gh.run, gh)
			_, err := runPRCmd(ciRerunCmd, "41366086276")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if p := gh.posts(); len(p) != 0 {
				t.Errorf("refused re-run still POSTed: %v", p)
			}
		})
	}
}

func TestCIDispatchPassesRefAndInputs(t *testing.T) {
	fd, gh := setupCIActionsTest(t)
	ciDispatchCmd.Flags().Set("task", "gd-task")
	ciDispatchCmd.Flags().Set("ref", "refs/heads/agent/x/gd-1")
	ciDispatchCmd.Flags().Set("field", "images=controller,gb")
	out, err := runPRCmd(ciDispatchCmd, ".github/workflows/images.yml")
	if err != nil {
		t.Fatal(err)
	}
	posts := gh.posts()
	if len(posts) != 1 {
		t.Fatalf("POSTs = %v, want one dispatch", posts)
	}
	want := []string{"api", "-X", "POST", "repos/mattbakerconsulting/gardenio/actions/workflows/images.yml/dispatches", "-f", "ref=agent/x/gd-1", "-f", "inputs[images]=controller,gb"}
	if !slices.Equal(posts[0], want) {
		t.Errorf("dispatch args = %v\nwant %v", posts[0], want)
	}
	if !strings.Contains(out, "dispatched images.yml on agent/x/gd-1 (1 of 2 this hour)") {
		t.Errorf("output = %q", out)
	}
	if len(fd.comments) != 1 || !strings.Contains(fd.comments[0], "gb ci dispatch: dispatched images.yml on agent/x/gd-1") {
		t.Errorf("task comments = %v", fd.comments)
	}
}

func TestCIDispatchRefusals(t *testing.T) {
	cases := []struct {
		name, workflow, ref string
		spent               bool
		want                string
	}{
		{"workflow not allowlisted", "e2e.yml", "agent/x/gd-1", false, "not dispatchable"},
		{"main", "ci.yml", "main", false, "not an agent branch"},
		{"no ref", "ci.yml", "", false, "--ref"},
		{"budget spent", "checks.yml", "agent/x/gd-1", true, "limit 2 per PR per hour"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, gh := setupCIActionsTest(t)
			if tc.spent {
				spentHistory(gh)
			}
			ciDispatchCmd.Flags().Set("ref", tc.ref)
			_, err := runPRCmd(ciDispatchCmd, tc.workflow)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if p := gh.posts(); len(p) != 0 {
				t.Errorf("refused dispatch still POSTed: %v", p)
			}
		})
	}
}

func TestParseRunTarget(t *testing.T) {
	for in, want := range map[string]struct {
		repo string
		id   int64
	}{
		"41366086276": {"", 41366086276},
		"https://github.com/mattbakerconsulting/gardenio/actions/runs/41366086276":               {"mattbakerconsulting/gardenio", 41366086276},
		"https://github.com/mattbakerconsulting/gardenio/actions/runs/41366086276/job/120034567": {"mattbakerconsulting/gardenio", 41366086276},
	} {
		repo, id, err := parseRunTarget(in)
		if err != nil || repo != want.repo || id != want.id {
			t.Errorf("parseRunTarget(%q) = %q, %d, %v; want %q, %d", in, repo, id, err, want.repo, want.id)
		}
	}
	if _, _, err := parseRunTarget("https://github.com/o/r/pull/3"); err == nil {
		t.Error("a PR URL parsed as a run")
	}
}
