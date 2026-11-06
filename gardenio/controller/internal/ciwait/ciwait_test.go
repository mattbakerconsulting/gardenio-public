package ciwait

import (
	"encoding/json"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

const testKey = "mattbakerconsulting/gardenio@82a7367954e1805b80695fca3e5f2cd9a797b689"

func run(id int64, name string, attempt int, status, conclusion string) Run {
	return Run{ID: id, Name: name, Attempt: attempt, Status: status, Conclusion: conclusion}
}

func TestRecord(t *testing.T) {
	type step struct {
		r           Run
		at          time.Duration // after t0
		wantChanged bool
	}
	for _, tc := range []struct {
		name       string
		steps      []step
		wantRuns   []Run
		wantChange time.Duration
	}{
		{
			name: "a redelivery of the same attempt changes nothing",
			steps: []step{
				{run(1, "ci", 1, "in_progress", ""), 0, true},
				{run(1, "ci", 1, "in_progress", ""), 10 * time.Second, false},
			},
			wantRuns:   []Run{run(1, "ci", 1, "in_progress", "")},
			wantChange: 0,
		},
		{
			name: "a run moves forward to completed",
			steps: []step{
				{run(1, "ci", 1, "requested", ""), 0, true},
				{run(1, "ci", 1, "completed", "failure"), 30 * time.Second, true},
			},
			wantRuns:   []Run{run(1, "ci", 1, "completed", "failure")},
			wantChange: 30 * time.Second,
		},
		{
			name: "an in_progress delivered after its completed does not move it back",
			steps: []step{
				{run(1, "ci", 1, "completed", "success"), 0, true},
				{run(1, "ci", 1, "in_progress", ""), 5 * time.Second, false},
			},
			wantRuns:   []Run{run(1, "ci", 1, "completed", "success")},
			wantChange: 0,
		},
		{
			name: "a re-run is a new attempt of the same run id, kept beside the old one",
			steps: []step{
				{run(1, "ci", 1, "completed", "failure"), 0, true},
				{run(1, "ci", 2, "requested", ""), time.Minute, true},
			},
			wantRuns:   []Run{run(1, "ci", 1, "completed", "failure"), run(1, "ci", 2, "requested", "")},
			wantChange: time.Minute,
		},
		{
			name: "a missing attempt counts as attempt 1, and an id of 0 is dropped",
			steps: []step{
				{run(1, "ci", 0, "queued", ""), 0, true},
				{run(1, "ci", 1, "queued", ""), time.Second, false},
				{run(0, "ci", 1, "queued", ""), 2 * time.Second, false},
			},
			wantRuns:   []Run{run(1, "ci", 1, "queued", "")},
			wantChange: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := Ledger{Key: testKey}
			for i, s := range tc.steps {
				if got := l.Record(s.r, t0.Add(s.at)); got != s.wantChanged {
					t.Errorf("step %d: Record changed = %v, want %v", i, got, s.wantChanged)
				}
			}
			if len(l.Runs) != len(tc.wantRuns) {
				t.Fatalf("runs = %+v, want %+v", l.Runs, tc.wantRuns)
			}
			for i := range l.Runs {
				if l.Runs[i] != tc.wantRuns[i] {
					t.Errorf("run %d = %+v, want %+v", i, l.Runs[i], tc.wantRuns[i])
				}
			}
			if want := t0.Add(tc.wantChange); !l.LastChange.Equal(want) {
				t.Errorf("LastChange = %s, want %s", l.LastChange, want)
			}
		})
	}
}

func TestRecordTrimsToMaxRuns(t *testing.T) {
	l := Ledger{Key: testKey}
	for i := int64(1); i <= MaxRuns+5; i++ {
		l.Record(run(i, "ci", 1, "completed", "success"), t0)
	}
	if len(l.Runs) != MaxRuns {
		t.Fatalf("len(runs) = %d, want %d", len(l.Runs), MaxRuns)
	}
	for _, r := range l.Runs {
		if r.ID <= 5 {
			t.Errorf("oldest run %d survived the trim", r.ID)
		}
	}
}

func TestMerge(t *testing.T) {
	mem := Ledger{Key: testKey}
	mem.Record(run(2, "checks", 1, "completed", "success"), t0.Add(time.Minute))

	persisted := Ledger{Key: testKey}
	persisted.Record(run(1, "ci", 1, "in_progress", ""), t0)
	persisted.Record(run(2, "checks", 1, "in_progress", ""), t0.Add(10*time.Second))

	if !mem.Merge(persisted) {
		t.Fatal("Merge of a ledger with an unseen run reported no change")
	}
	got := map[int64]string{}
	for _, r := range mem.Runs {
		got[r.ID] = r.Status
	}
	if got[1] != "in_progress" || got[2] != "completed" {
		t.Errorf("merged statuses = %v, want ci in_progress and checks completed", got)
	}
	if !mem.LastChange.Equal(t0.Add(time.Minute)) {
		t.Errorf("LastChange = %s, want the later of the two (%s)", mem.LastChange, t0.Add(time.Minute))
	}
	if mem.Merge(persisted) {
		t.Error("merging the same ledger twice reported a change")
	}
	other := Ledger{Key: "o/r@" + "0000000000000000000000000000000000000000"}
	other.Record(run(9, "x", 1, "queued", ""), t0)
	if mem.Merge(other) {
		t.Error("merged a ledger for a different key")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	l := Ledger{Key: testKey}
	l.Record(run(5, "ci", 1, "completed", "success"), t0)
	l.Record(run(3, "checks", 2, "in_progress", ""), t0.Add(time.Second))
	enc := l.Encode()

	// Insertion order does not change the encoding.
	m := Ledger{Key: testKey}
	m.Record(run(3, "checks", 2, "in_progress", ""), t0.Add(time.Second))
	m.Record(run(5, "ci", 1, "completed", "success"), t0)
	if m.Encode() != enc {
		t.Errorf("encodings differ by insertion order:\n%s\n%s", m.Encode(), enc)
	}

	back, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if back.Encode() != enc {
		t.Errorf("round trip changed the ledger:\n%s\n%s", back.Encode(), enc)
	}
	if empty, err := Decode(""); err != nil || len(empty.Runs) != 0 || empty.Key != "" {
		t.Errorf("Decode(\"\") = %+v, %v; want an empty ledger", empty, err)
	}
	if _, err := Decode("{not json"); err == nil {
		t.Error("Decode accepted malformed JSON")
	}
}

func TestEvaluate(t *testing.T) {
	deadline := t0.Add(2 * time.Hour).Format(time.RFC3339)
	ledger := func(lastChange time.Duration, runs ...Run) Ledger {
		return Ledger{Key: testKey, Runs: runs, LastChange: t0.Add(lastChange)}
	}
	for _, tc := range []struct {
		name        string
		l           Ledger
		workflows   string
		deadline    string
		now         time.Duration // after t0
		wantState   string
		wantResult  []Result
		wantSettles time.Duration // 0 = not settling
	}{
		{
			name:       "no runs yet stays pending",
			l:          ledger(0),
			deadline:   deadline,
			now:        time.Hour,
			wantState:  beadsapi.CIWaitStatePending,
			wantResult: []Result{},
		},
		{
			name: "a run still in progress keeps the wait pending",
			l: ledger(0,
				run(1, "ci", 1, "in_progress", ""),
				run(2, "checks", 1, "completed", "success")),
			deadline:  deadline,
			now:       10 * time.Minute,
			wantState: beadsapi.CIWaitStatePending,
			wantResult: []Result{
				{Name: "checks", RunID: 2, RunAttempt: 1, Conclusion: "success"},
				{Name: "ci", RunID: 1, RunAttempt: 1, Conclusion: "in_progress"},
			},
		},
		{
			name: "all completed but inside the quiesce window is settling",
			l: ledger(time.Minute,
				run(1, "ci", 1, "completed", "failure"),
				run(2, "checks", 1, "completed", "success")),
			deadline:    deadline,
			now:         time.Minute + 30*time.Second,
			wantState:   beadsapi.CIWaitStatePending,
			wantSettles: 2 * time.Minute,
			wantResult: []Result{
				{Name: "checks", RunID: 2, RunAttempt: 1, Conclusion: "success"},
				{Name: "ci", RunID: 1, RunAttempt: 1, Conclusion: "failure"},
			},
		},
		{
			name: "all completed and quiet for the window resolves, red and green alike",
			l: ledger(time.Minute,
				run(1, "ci", 1, "completed", "failure"),
				run(2, "checks", 1, "completed", "success")),
			deadline:  deadline,
			now:       2 * time.Minute,
			wantState: beadsapi.CIWaitStateResolved,
			wantResult: []Result{
				{Name: "checks", RunID: 2, RunAttempt: 1, Conclusion: "success"},
				{Name: "ci", RunID: 1, RunAttempt: 1, Conclusion: "failure"},
			},
		},
		{
			name: "cancelled, skipped and action_required are conclusions, reported verbatim",
			l: ledger(0,
				run(1, "e2e", 1, "completed", "skipped"),
				run(2, "images", 1, "completed", "cancelled"),
				run(3, "deploy", 1, "completed", "action_required")),
			deadline:  deadline,
			now:       time.Hour,
			wantState: beadsapi.CIWaitStateResolved,
			wantResult: []Result{
				{Name: "deploy", RunID: 3, RunAttempt: 1, Conclusion: "action_required"},
				{Name: "e2e", RunID: 1, RunAttempt: 1, Conclusion: "skipped"},
				{Name: "images", RunID: 2, RunAttempt: 1, Conclusion: "cancelled"},
			},
		},
		{
			name: "a re-run supersedes the failed attempt it replaces",
			l: ledger(5*time.Minute,
				run(1, "ci", 1, "completed", "failure"),
				run(1, "ci", 2, "in_progress", "")),
			deadline:  deadline,
			now:       time.Hour,
			wantState: beadsapi.CIWaitStatePending,
			wantResult: []Result{
				{Name: "ci", RunID: 1, RunAttempt: 2, Conclusion: "in_progress"},
			},
		},
		{
			name: "a later run of the same workflow supersedes an earlier one",
			l: ledger(0,
				run(10, "ci", 1, "completed", "failure"),
				run(11, "ci", 1, "completed", "success")),
			deadline:  deadline,
			now:       time.Hour,
			wantState: beadsapi.CIWaitStateResolved,
			wantResult: []Result{
				{Name: "ci", RunID: 11, RunAttempt: 1, Conclusion: "success"},
			},
		},
		{
			name: "a workflow filter resolves on exactly that set",
			l: ledger(0,
				run(1, "ci", 1, "completed", "success"),
				run(2, "lands-perf", 1, "in_progress", "")),
			workflows: " ci ,",
			deadline:  deadline,
			now:       time.Hour,
			wantState: beadsapi.CIWaitStateResolved,
			wantResult: []Result{
				{Name: "ci", RunID: 1, RunAttempt: 1, Conclusion: "success"},
			},
		},
		{
			name: "a filtered workflow with no run yet is still awaited",
			l: ledger(0,
				run(1, "ci", 1, "completed", "success")),
			workflows: "ci,checks",
			deadline:  deadline,
			now:       time.Hour,
			wantState: beadsapi.CIWaitStatePending,
			wantResult: []Result{
				{Name: "checks", Conclusion: "not_started"},
				{Name: "ci", RunID: 1, RunAttempt: 1, Conclusion: "success"},
			},
		},
		{
			name: "the deadline passing first times the wait out",
			l: ledger(0,
				run(1, "ci", 1, "queued", "")),
			workflows: "ci,checks",
			deadline:  deadline,
			now:       2 * time.Hour,
			wantState: beadsapi.CIWaitStateTimedOut,
			wantResult: []Result{
				{Name: "checks", Conclusion: "not_started"},
				{Name: "ci", RunID: 1, RunAttempt: 1, Conclusion: "queued"},
			},
		},
		{
			name: "at the deadline a fully completed sha resolves without waiting out the window",
			l: ledger(2*time.Hour-10*time.Second,
				run(1, "ci", 1, "completed", "success")),
			deadline:  deadline,
			now:       2 * time.Hour,
			wantState: beadsapi.CIWaitStateResolved,
			wantResult: []Result{
				{Name: "ci", RunID: 1, RunAttempt: 1, Conclusion: "success"},
			},
		},
		{
			name: "an unreadable deadline never times out",
			l: ledger(0,
				run(1, "ci", 1, "queued", "")),
			deadline:  "soon",
			now:       100 * time.Hour,
			wantState: beadsapi.CIWaitStatePending,
			wantResult: []Result{
				{Name: "ci", RunID: 1, RunAttempt: 1, Conclusion: "queued"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := Evaluate(tc.l, tc.workflows, tc.deadline, t0.Add(tc.now), DefaultQuiesce)
			if out.State != tc.wantState {
				t.Errorf("state = %q, want %q (%s)", out.State, tc.wantState, out.Summary)
			}
			var got []Result
			if err := json.Unmarshal([]byte(out.Result), &got); err != nil {
				t.Fatalf("result %q: %v", out.Result, err)
			}
			if len(got) != len(tc.wantResult) {
				t.Fatalf("result = %+v, want %+v", got, tc.wantResult)
			}
			for i := range got {
				if got[i] != tc.wantResult[i] {
					t.Errorf("result[%d] = %+v, want %+v", i, got[i], tc.wantResult[i])
				}
			}
			var wantSettles time.Time
			if tc.wantSettles != 0 {
				wantSettles = t0.Add(tc.wantSettles)
			}
			if !out.SettlesAt.Equal(wantSettles) {
				t.Errorf("SettlesAt = %s, want %s", out.SettlesAt, wantSettles)
			}
		})
	}
}

// TestEvaluateChainedWorkflowReopensTheWindow is the case the quiesce window
// exists for: a workflow_run-chained workflow is requested only after the run
// it follows completes, so a sha that looked done is not.
func TestEvaluateChainedWorkflowReopensTheWindow(t *testing.T) {
	deadline := t0.Add(2 * time.Hour).Format(time.RFC3339)
	l := Ledger{Key: testKey}
	l.Record(run(1, "ci", 1, "completed", "success"), t0)
	if out := Evaluate(l, "", deadline, t0.Add(20*time.Second), DefaultQuiesce); out.SettlesAt.IsZero() {
		t.Fatalf("want settling after the only run completed, got %+v", out)
	}
	l.Record(run(2, "deploy-preview", 1, "requested", ""), t0.Add(30*time.Second))
	if out := Evaluate(l, "", deadline, t0.Add(90*time.Second), DefaultQuiesce); out.State != beadsapi.CIWaitStatePending || !out.SettlesAt.IsZero() {
		t.Fatalf("a chained run turned up: want pending and not settling, got %+v", out)
	}
	l.Record(run(2, "deploy-preview", 1, "completed", "success"), t0.Add(3*time.Minute))
	if out := Evaluate(l, "", deadline, t0.Add(3*time.Minute+59*time.Second), DefaultQuiesce); out.State != beadsapi.CIWaitStatePending {
		t.Fatalf("inside the reopened window: want pending, got %+v", out)
	}
	if out := Evaluate(l, "", deadline, t0.Add(4*time.Minute), DefaultQuiesce); out.State != beadsapi.CIWaitStateResolved {
		t.Fatalf("after the window: want resolved, got %+v", out)
	}
}
