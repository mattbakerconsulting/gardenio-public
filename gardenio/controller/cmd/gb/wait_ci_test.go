package main

import (
	"bytes"
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

const testCISHA = "0123456789abcdef0123456789abcdef01234567"

// ciWaitAgentFixture serves the calling agent's own bead (GET) and applies
// field PATCHes to it in memory, so a test can drive `gb wait ci` and then
// read back what it stamped.
type ciWaitAgentFixture struct {
	mu      sync.Mutex
	id      string
	fields  map[string]string
	patches int
	// others are read-only beads served besides the agent's, by id → status
	// (the wait's task, for the task-closed check).
	others map[string]string
}

func newCIWaitAgentFixture(t *testing.T, id string, fields map[string]string) *ciWaitAgentFixture {
	t.Helper()
	f := &ciWaitAgentFixture{id: id, fields: fields, others: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		other, isOther := f.others[strings.TrimPrefix(r.URL.Path, "/v1/beads/")]
		f.mu.Unlock()
		if isOther && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "type": "task", "status": other,
			})
			return
		}
		if r.URL.Path != "/v1/beads/"+id {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodPatch {
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body.Fields {
				s, _ := v.(string)
				f.fields[k] = s
			}
			f.patches++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "type": "agent", "status": "in_progress", "fields": f.fields,
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	t.Setenv("KD_AGENT_ID", id)
	return f
}

// runWaitCIArgs runs `gb wait ci` with args against the fixture and returns
// stdout. Flags are reset afterwards so cases do not leak into each other.
func runWaitCIArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		for _, name := range []string{"repo", "sha", "requesting-task", "status", "result", "cancel"} {
			_ = waitCICmd.Flags().Set(name, waitCICmd.Flags().Lookup(name).DefValue)
		}
		_ = waitCICmd.Flags().Set("timeout", defaultCIWaitTimeout.String())
		if sv, ok := waitCICmd.Flags().Lookup("workflow").Value.(interface{ Replace([]string) error }); ok {
			_ = sv.Replace(nil)
		}
	})
	if err := waitCICmd.ParseFlags(args); err != nil {
		t.Fatalf("parse flags %v: %v", args, err)
	}
	var out bytes.Buffer
	waitCICmd.SetOut(&out)
	waitCICmd.SetErr(&bytes.Buffer{})
	waitCICmd.SetContext(t.Context())
	err := runWaitCI(waitCICmd, nil)
	return out.String(), err
}

func TestBuildCIWaitFields(t *testing.T) {
	now := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)

	t.Run("defaults the task to the agent's task_id and stamps every key", func(t *testing.T) {
		got, err := buildCIWaitFields("joyousaversion/gardenio", strings.ToUpper(testCISHA),
			[]string{"ci", " checks ", ""}, "", "gd-TASK01", time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			beadsapi.FieldCIWaitState:     beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:       "joyousaversion/gardenio@" + testCISHA,
			beadsapi.FieldCIWaitWorkflows: "ci,checks",
			beadsapi.FieldCIWaitTask:      "gd-TASK01",
			beadsapi.FieldCIWaitDeadline:  "2026-10-28T07:00:00Z",
			beadsapi.FieldCIWaitResult:    "",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
		// Every ci_wait_* key is written so a replaced wait inherits nothing.
		for _, k := range beadsapi.CIWaitFields {
			if _, ok := got[k]; !ok {
				t.Errorf("PATCH omits %s; a replaced wait would keep the old value", k)
			}
		}
	})

	t.Run("--requesting-task wins over task_id", func(t *testing.T) {
		got, err := buildCIWaitFields("o/r", testCISHA, nil, "gd-OTHER", "gd-TASK01", time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		if got[beadsapi.FieldCIWaitTask] != "gd-OTHER" {
			t.Errorf("task = %q, want gd-OTHER", got[beadsapi.FieldCIWaitTask])
		}
	})

	for _, tc := range []struct {
		name, repo, sha string
		workflows       []string
		timeout         time.Duration
		wantErr         string
	}{
		{"missing sha", "o/r", "", nil, time.Hour, "--repo and --sha are required"},
		{"abbreviated sha", "o/r", testCISHA[:12], nil, time.Hour, "full 40-character"},
		{"non-hex sha", "o/r", strings.Repeat("z", 40), nil, time.Hour, "full 40-character"},
		{"repo without owner", "gardenio", testCISHA, nil, time.Hour, "OWNER/REPO"},
		{"comma in a workflow name", "o/r", testCISHA, []string{"a,b"}, time.Hour, "cannot contain a comma"},
		{"non-positive timeout", "o/r", testCISHA, nil, 0, "--timeout must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildCIWaitFields(tc.repo, tc.sha, tc.workflows, "", "gd-TASK01", tc.timeout, now)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestWaitCI_RecordsOnOwnBead(t *testing.T) {
	f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
		"task_id": "gd-TASK01",
		// A previous, concluded wait: its result must not survive the new one.
		beadsapi.FieldCIWaitState:  beadsapi.CIWaitStateConsumed,
		beadsapi.FieldCIWaitResult: `[{"name":"old"}]`,
	})

	out, err := runWaitCIArgs(t, "--repo", "joyousaversion/gardenio", "--sha", testCISHA, "--workflow", "ci")
	if err != nil {
		t.Fatalf("gb wait ci: %v", err)
	}
	if f.fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStatePending {
		t.Errorf("state = %q, want pending", f.fields[beadsapi.FieldCIWaitState])
	}
	if f.fields[beadsapi.FieldCIWaitKey] != "joyousaversion/gardenio@"+testCISHA {
		t.Errorf("key = %q", f.fields[beadsapi.FieldCIWaitKey])
	}
	if f.fields[beadsapi.FieldCIWaitTask] != "gd-TASK01" {
		t.Errorf("task = %q, want the agent's task_id", f.fields[beadsapi.FieldCIWaitTask])
	}
	if f.fields[beadsapi.FieldCIWaitResult] != "" {
		t.Errorf("result = %q, want cleared", f.fields[beadsapi.FieldCIWaitResult])
	}
	if !strings.Contains(out, "gb stop") {
		t.Errorf("output should tell the agent to stop next, got:\n%s", out)
	}
}

func TestWaitCI_StatusIsReadOnly(t *testing.T) {
	f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
		beadsapi.FieldCIWaitState: beadsapi.CIWaitStateResolved,
		beadsapi.FieldCIWaitKey:   "o/r@" + testCISHA,
	})
	out, err := runWaitCIArgs(t, "--status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "resolved") || !strings.Contains(out, "o/r@"+testCISHA) {
		t.Errorf("status output missing state/key:\n%s", out)
	}
	if f.patches != 0 {
		t.Errorf("--status wrote %d PATCH(es); it must be read-only", f.patches)
	}
}

func TestWaitCI_Result(t *testing.T) {
	result := `[{"name":"ci","run_id":11,"run_attempt":1,"conclusion":"failure"},{"name":"checks","run_id":41402257330,"run_attempt":2,"conclusion":"success"}]`

	for _, state := range []string{beadsapi.CIWaitStateResolved, beadsapi.CIWaitStateTimedOut} {
		t.Run(state+" -> prints conclusions and marks consumed", func(t *testing.T) {
			f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
				beadsapi.FieldCIWaitState:  state,
				beadsapi.FieldCIWaitKey:    "o/r@" + testCISHA,
				beadsapi.FieldCIWaitResult: result,
			})
			out, err := runWaitCIArgs(t, "--result")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"ci: failure", "checks: success", "run 41402257330, attempt 2", state} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			if f.fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateConsumed {
				t.Errorf("state = %q, want consumed", f.fields[beadsapi.FieldCIWaitState])
			}
		})
	}

	t.Run("pending, CI still running on GitHub -> refused, nothing written", func(t *testing.T) {
		gh := fakeCIRunsGH(t, `{"workflow_runs":[
			{"id":11,"name":"ci","run_attempt":1,"status":"completed","conclusion":"success"},
			{"id":12,"name":"checks","run_attempt":1,"status":"in_progress","conclusion":null}]}`)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
			beadsapi.FieldCIWaitState:    beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:      "o/r@" + testCISHA,
			beadsapi.FieldCIWaitDeadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
		_, err := runWaitCIArgs(t, "--result")
		if err == nil || !strings.Contains(err.Error(), "still pending") || !strings.Contains(err.Error(), "1/2") {
			t.Fatalf("err = %v, want still pending with 1/2 concluded", err)
		}
		if f.patches != 0 {
			t.Errorf("a pending --result wrote %d PATCH(es)", f.patches)
		}
		if want := "api repos/o/r/actions/runs?head_sha=" + testCISHA + "&per_page=100"; *gh != want {
			t.Errorf("gh called with %q, want %q", *gh, want)
		}
	})

	t.Run("pending, CI concluded on GitHub -> resolves, prints and consumes", func(t *testing.T) {
		fakeCIRunsGH(t, `{"workflow_runs":[
			{"id":11,"name":"ci","run_attempt":1,"status":"completed","conclusion":"failure"},
			{"id":12,"name":"checks","run_attempt":2,"status":"completed","conclusion":"success"}]}`)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
			beadsapi.FieldCIWaitState:    beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:      "o/r@" + testCISHA,
			beadsapi.FieldCIWaitDeadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
		out, err := runWaitCIArgs(t, "--result")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"ci: failure", "checks: success", "resolved"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if f.fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateConsumed {
			t.Errorf("state = %q, want consumed", f.fields[beadsapi.FieldCIWaitState])
		}
		if !strings.Contains(f.fields[beadsapi.FieldCIWaitResult], `"conclusion":"failure"`) {
			t.Errorf("result not recorded: %q", f.fields[beadsapi.FieldCIWaitResult])
		}
	})

	t.Run("no wait -> error", func(t *testing.T) {
		newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{})
		if _, err := runWaitCIArgs(t, "--result"); err == nil || !strings.Contains(err.Error(), "no CI wait") {
			t.Fatalf("err = %v, want no CI wait", err)
		}
	})

	t.Run("consumed -> prints again without writing", func(t *testing.T) {
		f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
			beadsapi.FieldCIWaitState:  beadsapi.CIWaitStateConsumed,
			beadsapi.FieldCIWaitKey:    "o/r@" + testCISHA,
			beadsapi.FieldCIWaitResult: result,
		})
		out, err := runWaitCIArgs(t, "--result")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "ci: failure") {
			t.Errorf("output missing conclusions:\n%s", out)
		}
		if f.patches != 0 {
			t.Errorf("re-reading a consumed wait wrote %d PATCH(es)", f.patches)
		}
	})
}

// fakeCIRunsGH answers `gh api .../actions/runs` with body and records the
// args of the last call.
func fakeCIRunsGH(t *testing.T, body string) *string {
	t.Helper()
	var got string
	old := prGHRun
	prGHRun = func(_ context.Context, args ...string) (string, error) {
		got = strings.Join(args, " ")
		return body, nil
	}
	t.Cleanup(func() { prGHRun = old })
	return &got
}

func TestCIWaitOutcome(t *testing.T) {
	now := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	done := func(id int64, name, conclusion string) ciRun {
		return ciRun{ID: id, Name: name, RunAttempt: 1, Status: "completed", Conclusion: conclusion}
	}
	running := func(id int64, name string) ciRun {
		return ciRun{ID: id, Name: name, RunAttempt: 1, Status: "in_progress"}
	}

	for _, tc := range []struct {
		name       string
		runs       []ciRun
		workflows  string
		deadline   string
		wantState  string
		wantResult string
	}{
		{"no runs yet", nil, "", future, beadsapi.CIWaitStatePending, `[]`},
		{"one still running", []ciRun{done(1, "ci", "success"), running(2, "checks")}, "", future,
			beadsapi.CIWaitStatePending, ""},
		{"all completed, red and green", []ciRun{done(1, "ci", "failure"), done(2, "checks", "success")}, "", future,
			beadsapi.CIWaitStateResolved,
			`[{"name":"checks","run_id":2,"run_attempt":1,"conclusion":"success"},{"name":"ci","run_id":1,"run_attempt":1,"conclusion":"failure"}]`},
		{"newest run of a workflow wins", []ciRun{done(1, "ci", "failure"), running(3, "ci")}, "", future,
			beadsapi.CIWaitStatePending, ""},
		{"filter ignores other workflows", []ciRun{done(1, "ci", "success"), running(2, "e2e")}, "ci", future,
			beadsapi.CIWaitStateResolved, `[{"name":"ci","run_id":1,"run_attempt":1,"conclusion":"success"}]`},
		{"filtered workflow not started", []ciRun{done(1, "ci", "success")}, "ci,checks", future,
			beadsapi.CIWaitStatePending, ""},
		{"deadline passed while running", []ciRun{running(2, "checks")}, "", past,
			beadsapi.CIWaitStateTimedOut, `[{"name":"checks","run_id":2,"run_attempt":1,"conclusion":"in_progress"}]`},
		{"completed beats a passed deadline", []ciRun{done(1, "ci", "success")}, "", past,
			beadsapi.CIWaitStateResolved, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, result, _ := ciWaitOutcome(tc.runs, tc.workflows, tc.deadline, now)
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if tc.wantResult != "" && result != tc.wantResult {
				t.Errorf("result = %s\nwant     %s", result, tc.wantResult)
			}
		})
	}
}

// fakeCIGH answers `gh api` by endpoint: the Actions runs listing with runs,
// the commit's pull-request listing with pulls. It records every call.
func fakeCIGH(t *testing.T, runs, pulls string) *[]string {
	t.Helper()
	var calls []string
	old := prGHRun
	prGHRun = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if strings.Contains(call, "/pulls") {
			return pulls, nil
		}
		return runs, nil
	}
	t.Cleanup(func() { prGHRun = old })
	return &calls
}

// mergedElsewhere is GitHub's pull-request listing for testCISHA after the
// merge desk merged main into the branch and merged the PR at that new head:
// the gd-lHWcyCRj1Q case, where the awaited sha never got a run.
const mergedElsewhere = `[{"number":1941,"state":"closed","merged_at":"2026-11-02T06:01:00Z",
	"merge_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	"head":{"sha":"494f2a6f27ae96ddb755b963851307b6dacd98b9"}}]`

func TestCIWaitSuperseded(t *testing.T) {
	key := "o/r@" + testCISHA
	for _, tc := range []struct {
		name, pulls string
		want        string // substring; "" means not superseded
	}{
		{"PR merged at a later head", mergedElsewhere, "PR #1941 is merged at head 494f2a6f27ae"},
		{"open PR whose head moved on", `[{"number":7,"state":"open","head":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}]`,
			"PR #7 is open at head bbbbbbbbbbbb"},
		{"PR closed at this head", `[{"number":8,"state":"closed","head":{"sha":"` + testCISHA + `"}}]`, "PR #8 is closed"},
		{"open PR still at this head", `[{"number":9,"state":"open","head":{"sha":"` + testCISHA + `"}}]`, ""},
		{"sha is a PR's merge commit", `[{"number":10,"state":"closed","merged_at":"2026-11-02T06:01:00Z",
			"merge_commit_sha":"` + testCISHA + `","head":{"sha":"cccccccccccccccccccccccccccccccccccccccc"}}]`, ""},
		{"no pull request (a direct push)", `[]`, ""},
		{"one PR moved on, another still at this head", `[
			{"number":7,"state":"open","head":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
			{"number":9,"state":"open","head":{"sha":"` + testCISHA + `"}}]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeCIGH(t, "", tc.pulls)
			got, err := ciWaitSuperseded(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("reason = %q, want containing %q", got, tc.want)
			}
			if want := "api repos/o/r/commits/" + testCISHA + "/pulls"; len(*calls) != 1 || (*calls)[0] != want {
				t.Errorf("gh calls = %q, want [%q]", *calls, want)
			}
		})
	}
}

// TestWaitCI_ResultCancels: a pending --result drops the wait instead of
// reporting it pending until the deadline when no CI will ever conclude it
// (gd-lHWcyCRj1Q).
func TestWaitCI_ResultCancels(t *testing.T) {
	pendingWait := func() map[string]string {
		return map[string]string{
			beadsapi.FieldCIWaitState:    beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:      "o/r@" + testCISHA,
			beadsapi.FieldCIWaitTask:     "gd-WORK01",
			beadsapi.FieldCIWaitDeadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}
	}

	t.Run("no runs, PR merged at another head -> cancelled", func(t *testing.T) {
		fakeCIGH(t, `{"workflow_runs":[]}`, mergedElsewhere)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait())
		out, err := runWaitCIArgs(t, "--result")
		if err != nil {
			t.Fatal(err)
		}
		if f.fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateCancelled {
			t.Errorf("state = %q, want cancelled", f.fields[beadsapi.FieldCIWaitState])
		}
		if !strings.Contains(f.fields[beadsapi.FieldCIWaitResult], "PR #1941 is merged") {
			t.Errorf("result should carry the reason, got %q", f.fields[beadsapi.FieldCIWaitResult])
		}
		if !strings.Contains(out, "was cancelled") || !strings.Contains(out, "PR #1941") {
			t.Errorf("output should say why it was cancelled:\n%s", out)
		}
	})

	t.Run("no runs yet, PR still at this head -> still pending", func(t *testing.T) {
		fakeCIGH(t, `{"workflow_runs":[]}`, `[{"number":9,"state":"open","head":{"sha":"`+testCISHA+`"}}]`)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait())
		if _, err := runWaitCIArgs(t, "--result"); err == nil || !strings.Contains(err.Error(), "still pending") {
			t.Fatalf("err = %v, want still pending", err)
		}
		if f.patches != 0 {
			t.Errorf("wrote %d PATCH(es) on a wait CI may still conclude", f.patches)
		}
	})

	t.Run("task closed -> cancelled without asking GitHub", func(t *testing.T) {
		calls := fakeCIGH(t, `{"workflow_runs":[]}`, `[]`)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait())
		f.others["gd-WORK01"] = "closed"
		out, err := runWaitCIArgs(t, "--result")
		if err != nil {
			t.Fatal(err)
		}
		if f.fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateCancelled {
			t.Errorf("state = %q, want cancelled", f.fields[beadsapi.FieldCIWaitState])
		}
		if !strings.Contains(out, "gd-WORK01 is closed") {
			t.Errorf("output should name the closed task:\n%s", out)
		}
		if len(*calls) != 0 {
			t.Errorf("gh called %q; a closed task needs no GitHub read", *calls)
		}
	})
}

func TestWaitCI_Cancel(t *testing.T) {
	t.Run("pending -> cancelled", func(t *testing.T) {
		f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
			beadsapi.FieldCIWaitState: beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:   "o/r@" + testCISHA,
			beadsapi.FieldCIWaitTask:  "gd-WORK01",
		})
		out, err := runWaitCIArgs(t, "--cancel")
		if err != nil {
			t.Fatal(err)
		}
		w := beadsapi.CIWaitFromFields(f.fields)
		if w.State != beadsapi.CIWaitStateCancelled || w.Pending() || w.Concluded() {
			t.Errorf("state = %q, want cancelled (neither pending nor concluded)", w.State)
		}
		if !strings.Contains(out, "gd-WORK01") {
			t.Errorf("output should say the task is no longer parked:\n%s", out)
		}
		if applyCIWaitPark(map[string]string{}, w, false, time.Now()) {
			t.Error("a cancelled wait must not park the close")
		}
		if checkCIWaitConcluded(w) != nil {
			t.Error("a cancelled wait must not refuse the stop")
		}
		if ciWaitWakeHintFor(w) != "" {
			t.Error("a cancelled wait has nothing to read on wake")
		}
	})

	t.Run("concluded -> refused, read it with --result", func(t *testing.T) {
		f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
			beadsapi.FieldCIWaitState: beadsapi.CIWaitStateResolved,
			beadsapi.FieldCIWaitKey:   "o/r@" + testCISHA,
		})
		if _, err := runWaitCIArgs(t, "--cancel"); err == nil || !strings.Contains(err.Error(), "--result") {
			t.Fatalf("err = %v, want pointer to --result", err)
		}
		if f.patches != 0 {
			t.Errorf("wrote %d PATCH(es)", f.patches)
		}
	})

	t.Run("consumed -> nothing to cancel", func(t *testing.T) {
		f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
			beadsapi.FieldCIWaitState: beadsapi.CIWaitStateConsumed,
			beadsapi.FieldCIWaitKey:   "o/r@" + testCISHA,
		})
		out, err := runWaitCIArgs(t, "--cancel")
		if err != nil || !strings.Contains(out, "nothing to cancel") || f.patches != 0 {
			t.Errorf("err=%v patches=%d out=%q; want a no-op", err, f.patches, out)
		}
	})

	t.Run("no wait -> error", func(t *testing.T) {
		newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{})
		if _, err := runWaitCIArgs(t, "--cancel"); err == nil || !strings.Contains(err.Error(), "no CI wait") {
			t.Fatalf("err = %v, want no CI wait", err)
		}
	})

	t.Run("with --result -> mutually exclusive", func(t *testing.T) {
		newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{})
		if _, err := runWaitCIArgs(t, "--cancel", "--result"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("err = %v, want mutually exclusive", err)
		}
	})
}
