// Package ciwait is the pure core of the CI-wait resolver (plan
// gardenio/plans/wake-on-ci-completion.md §2.2-2.3, task gd-WKrO492yYP): a
// per-sha run ledger fed by GitHub workflow_run deliveries (or, for the poll
// fallback, by an Actions API listing), and the rule that decides when a
// pending wait has concluded.
//
// It does no I/O. The caller (cmd/github-bridge's ciWaitHandler) owns where
// the ledger lives and writes the outcome onto the agent bead.
package ciwait

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// DefaultQuiesce is how long a sha's ledger must go without a change after
// every awaited run completed before the wait resolves. A push can create runs
// lazily (a workflow_run-chained workflow is requested only after the run it
// follows completes; a re-run bumps run_attempt), so "every run I know of has
// completed" is not yet "CI is done" (plan §2.3).
const DefaultQuiesce = 60 * time.Second

// MaxRuns caps one ledger. A sha has a handful of workflows and a few
// attempts each; the cap only stops a runaway sha from growing a bead field
// without bound. Past it the oldest entries are dropped.
const MaxRuns = 200

// Run is one workflow run attempt as the ledger records it.
type Run struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Attempt    int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

// Ledger is every run attempt seen for one ci_wait_key, deduped on
// (workflow_run.id, run_attempt).
type Ledger struct {
	Key  string `json:"key"`
	Runs []Run  `json:"runs"`
	// LastChange is when a run was last added or moved forward (queued →
	// in_progress → completed). The quiesce window counts from it.
	LastChange time.Time `json:"last_change"`
}

// statusRank orders workflow_run statuses so an out-of-order delivery (an
// in_progress arriving after its completed) never moves a run backwards.
// Unknown statuses rank with the queued family: still running.
func statusRank(status string) int {
	switch strings.ToLower(status) {
	case "completed":
		return 2
	case "in_progress":
		return 1
	}
	return 0
}

// Record adds r to the ledger, or moves an already-recorded attempt forward.
// It reports whether the ledger changed; a redelivery, or a status the ledger
// is already past, changes nothing.
func (l *Ledger) Record(r Run, at time.Time) bool {
	if r.ID == 0 {
		return false
	}
	if r.Attempt < 1 {
		r.Attempt = 1
	}
	for i := range l.Runs {
		cur := &l.Runs[i]
		if cur.ID != r.ID || cur.Attempt != r.Attempt {
			continue
		}
		changed := false
		if cur.Name == "" && r.Name != "" {
			cur.Name, changed = r.Name, true
		}
		if statusRank(r.Status) > statusRank(cur.Status) {
			cur.Status, cur.Conclusion = r.Status, r.Conclusion
			l.touch(at)
			return true
		}
		return changed
	}
	l.Runs = append(l.Runs, r)
	l.trim()
	l.touch(at)
	return true
}

// Merge folds other into l: each attempt keeps the furthest status either side
// has, and LastChange becomes the later of the two. It is how a ledger copy
// persisted on an agent bead is reconciled with the bridge's in-memory one
// after a restart. Merging a ledger for a different key is a no-op.
func (l *Ledger) Merge(other Ledger) bool {
	if other.Key != l.Key {
		return false
	}
	changed := false
	for _, r := range other.Runs {
		if l.Record(r, other.LastChange) {
			changed = true
		}
	}
	if other.LastChange.After(l.LastChange) {
		l.LastChange = other.LastChange
		changed = true
	}
	return changed
}

func (l *Ledger) touch(at time.Time) {
	if at.After(l.LastChange) {
		l.LastChange = at
	}
}

// trim drops the oldest attempts past MaxRuns.
func (l *Ledger) trim() {
	if len(l.Runs) <= MaxRuns {
		return
	}
	sort.Slice(l.Runs, func(i, j int) bool { return newer(l.Runs[j], l.Runs[i]) })
	l.Runs = append([]Run(nil), l.Runs[len(l.Runs)-MaxRuns:]...)
}

// newer reports whether a supersedes b: a later run id (a new run of the
// workflow on the same sha), or the same run re-run with a higher attempt.
func newer(a, b Run) bool {
	if a.ID != b.ID {
		return a.ID > b.ID
	}
	return a.Attempt > b.Attempt
}

// Encode renders the ledger for the ci_wait_ledger field, runs sorted so the
// same ledger always encodes to the same string (the caller skips a write
// when the persisted copy is unchanged).
func (l Ledger) Encode() string {
	runs := append([]Run(nil), l.Runs...)
	sort.Slice(runs, func(i, j int) bool { return newer(runs[j], runs[i]) })
	l.Runs = runs
	l.LastChange = l.LastChange.UTC()
	raw, _ := json.Marshal(l)
	return string(raw)
}

// Decode parses a ci_wait_ledger field value. An empty value is an empty
// ledger; a malformed one is an error the caller logs and then ignores.
func Decode(s string) (Ledger, error) {
	var l Ledger
	if strings.TrimSpace(s) == "" {
		return l, nil
	}
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return Ledger{}, fmt.Errorf("decoding ci_wait_ledger: %w", err)
	}
	return l, nil
}

// Result is one entry of ci_wait_result, the shape plan §2.1 documents and
// `gb wait ci --result` prints.
type Result struct {
	Name       string `json:"name"`
	RunID      int64  `json:"run_id"`
	RunAttempt int    `json:"run_attempt"`
	Conclusion string `json:"conclusion"`
}

// Outcome is what Evaluate decided for one wait.
type Outcome struct {
	// State is beadsapi.CIWaitStatePending, CIWaitStateResolved or
	// CIWaitStateTimedOut.
	State string
	// Result is the ci_wait_result JSON. A run that has not completed reports
	// its status as the conclusion, and a named workflow with no run yet
	// reports "not_started" (only visible on a timed_out wait).
	Result string
	// Summary is a one-line progress count for logs and thread lines.
	Summary string
	// SettlesAt is set while every awaited run has completed but the quiesce
	// window is still open: re-evaluate then, and it resolves unless a new run
	// turned up in between.
	SettlesAt time.Time
}

// Evaluate decides a wait from the ledger for its sha.
//
// Only each workflow's newest run counts (a re-run or a later run of the same
// workflow supersedes the old one). workflows is the comma-separated
// ci_wait_workflows filter: when set, exactly those workflows are awaited, and
// one with no run yet is still awaited; when empty, every workflow seen for
// the sha is. The wait resolves once every awaited run has completed and the
// ledger has been quiet for quiesce; cancelled, skipped and action_required
// are conclusions like any other and are reported verbatim. If the deadline
// passes first it times out, except that a wait whose runs have all completed
// resolves at the deadline without waiting out the quiesce window.
func Evaluate(l Ledger, workflows, deadline string, now time.Time, quiesce time.Duration) Outcome {
	latest := map[string]Run{}
	for _, r := range l.Runs {
		if r.Name == "" {
			continue
		}
		if cur, ok := latest[r.Name]; !ok || newer(r, cur) {
			latest[r.Name] = r
		}
	}
	var names []string
	for _, name := range strings.Split(workflows, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		for name := range latest {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	done := 0
	results := make([]Result, 0, len(names))
	for _, name := range names {
		r, ok := latest[name]
		switch {
		case !ok:
			results = append(results, Result{Name: name, Conclusion: "not_started"})
		case statusRank(r.Status) == 2:
			done++
			conclusion := r.Conclusion
			if conclusion == "" {
				conclusion = r.Status
			}
			results = append(results, Result{Name: name, RunID: r.ID, RunAttempt: r.Attempt, Conclusion: conclusion})
		default:
			results = append(results, Result{Name: name, RunID: r.ID, RunAttempt: r.Attempt, Conclusion: r.Status})
		}
	}
	raw, _ := json.Marshal(results)
	out := Outcome{
		State:   beadsapi.CIWaitStatePending,
		Result:  string(raw),
		Summary: fmt.Sprintf("%d/%d workflow runs concluded", done, len(names)),
	}

	pastDeadline := false
	if d, err := time.Parse(time.RFC3339, deadline); err == nil && !now.Before(d) {
		pastDeadline = true
	}
	allDone := len(names) > 0 && done == len(names)
	settles := l.LastChange.Add(quiesce)
	switch {
	case allDone && (pastDeadline || !now.Before(settles)):
		out.State = beadsapi.CIWaitStateResolved
	case pastDeadline:
		out.State = beadsapi.CIWaitStateTimedOut
	case allDone:
		out.SettlesAt = settles
		out.Summary += ", settling until " + settles.UTC().Format(time.RFC3339)
	}
	return out
}
