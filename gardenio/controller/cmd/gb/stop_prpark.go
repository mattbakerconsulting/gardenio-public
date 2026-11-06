package main

// stop_prpark.go — gb stop parks an open PR behind a merge escalation
// (gd-x7HUu7s3gD, plan gd-3F4W22h2nT).
//
// On 2026-10-28, 64 of gardenio's 92 open PRs were orphans: open, with no
// merge escalation, so the merge desk never saw them, and the crews that
// opened them had stopped. A PR that did not merge inside its crew's lifetime
// was never picked up again. gb stop is the last point where the agent that
// opened the PR still exists, so it files the escalation itself: for every
// open PR the agent leaves with no open merge escalation, it runs the same
// `gb escalation create --requesting-task <task> --close-on-execute` a crew is
// told to run. A draft goes through `gb pr ready` instead, which flips it to
// ready and files; a merge escalation for a draft is refused
// (gd-9mAPy3HB8j). The escalation's blocks-edge then parks the task, so the
// claimed-work gate lets the stop through, and the merge closes the task and
// respawns the agent.
//
// A task can hold its PR back (gd-L9Bfqv2Ndn) with the pr:hold label. The
// park then leaves that open PR alone, draft or ready, and files nothing; the
// claimed-work gate lets the stop through on the hold. A merged or closed PR
// is not held. Plan gd-WFMvg9sDKE's #1575 had a merge floor and three stops in
// a row readied it and filed a merge escalation anyway.
//
// A PR the merge desk sent back at its current head is not re-filed
// (gd-bV2iypX2mD): the send-back closed its escalation, and the same head would
// only be sent back again. The park reports it with SentBack set, and the
// claimed-work gate still fails on its task: the agent pushes a fix and runs
// gb pr ready, or unclaims the task.
//
// Best-effort: a gh or daemon failure is reported and never blocks the stop.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// prParkFileTimeout bounds one `gb pr ready` / `gb escalation create`.
const prParkFileTimeout = 2 * time.Minute

// prHoldLabel is how a task holds its PR back from the park, until the label
// is removed.
const prHoldLabel = "pr:hold"

// taskPRHold says why task holds its PR back from the park, "" when it does
// not.
func taskPRHold(task *beadsapi.BeadDetail) string {
	if task == nil {
		return ""
	}
	for _, l := range task.Labels {
		if l == prHoldLabel {
			return fmt.Sprintf("task %s has the %s label", task.ID, prHoldLabel)
		}
	}
	return ""
}

// prParkDaemon is the slice of the beads client the park needs.
type prParkDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	ListAssignedTasks(ctx context.Context, agentName string) ([]*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// stopPRView is the slice of `gh pr view --json` the park reads.
type stopPRView struct {
	prView
	State string `json:"state"`
}

// viewStopPR reads a PR's state; target "" is the current branch's PR.
func viewStopPR(ctx context.Context, target string) (*stopPRView, error) {
	ctx, cancel := context.WithTimeout(ctx, draftLookupTimeout)
	defer cancel()
	args := []string{"pr", "view"}
	if target != "" {
		args = append(args, target)
	}
	args = append(args, "--json", "number,url,title,state,isDraft,headRefName,headRefOid")
	out, err := prGHRun(ctx, args...)
	if err != nil {
		return nil, err
	}
	var v stopPRView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("parsing gh pr view: %w", err)
	}
	if v.Number == 0 || v.URL == "" {
		return nil, fmt.Errorf("gh pr view returned no PR: %s", strings.TrimSpace(out))
	}
	return &v, nil
}

// prParkResult is what the park did for one PR.
type prParkResult struct {
	URL          string
	Task         string
	EscalationID string
	Filed        bool   // gb stop filed (or readied and filed) the escalation
	Readied      bool   // the PR was a draft and gb stop marked it ready
	Held         string // why Task holds the PR back; nothing was filed
	SentBack     string // the desk's send-back comment at the PR's current head; nothing was filed
	HeadSHA      string // the PR head SentBack names
	Err          error  // the PR may be open with no escalation: say so
}

// heldPRTasks is the set of tasks whose PR the park held back, which the
// claimed-work gate lets through. A sent-back PR is not held: its task must
// not strand in_progress behind a stopped agent with no handle (ka-0000000908).
func heldPRTasks(results []prParkResult) map[string]string {
	held := map[string]string{}
	for _, r := range results {
		if r.Held != "" && r.Task != "" {
			held[r.Task] = r.Held
		}
	}
	return held
}

// prParkEnabled reports whether gb stop should park open PRs. --force skips
// it like every other stop check; an unpushed head would put the wrong commit
// in front of the desk; a goal-unachievable stop is abandoning the work; and
// a reviewer's wrap-up lists PRs it reviewed, not PRs it owns, so re-filing
// one it sent back would undo the send-back. GB_STOP_PR_PARK=off is the
// kill switch.
func prParkEnabled(force bool, deliveryErr error, wrapup *WrapUp) bool {
	if force || deliveryErr != nil {
		return false
	}
	if wrapup != nil && wrapup.GoalUnachievable {
		return false
	}
	if os.Getenv("BOAT_ROLE") == "reviewer" {
		return false
	}
	return os.Getenv("GB_STOP_PR_PARK") != "off"
}

// parkOpenPRs files a merge escalation for each open PR the agent leaves
// without one. The PRs are the mr_url of every task the agent has claimed and
// of its assigned task_id, the wrap-up's pull_requests, and the current
// branch's PR. Merged and closed PRs, PRs already in the desk queue, and open
// PRs whose task holds them (taskPRHold) are left alone; a held PR is reported
// with Held set. A wrap-up or current-branch PR is attributed to the default
// task, so a hold on that task holds those PRs too. Only GitHub PRs are read (gh is the lookup).
func parkOpenPRs(ctx context.Context, d prParkDaemon, agentID, agentName string, wrapup *WrapUp) []prParkResult {
	if d == nil {
		return nil
	}

	// The task a PR with no task of its own parks: the agent's assigned
	// task_id, else its first claimed task.
	var claimed []*beadsapi.BeadDetail
	if agentName != "" {
		claimed, _ = d.ListAssignedTasks(ctx, agentName)
	}
	defaultTask := ""
	if b, err := d.GetBead(ctx, agentID); err == nil && b != nil {
		defaultTask = b.Fields["task_id"]
	}
	if defaultTask == "" && len(claimed) > 0 {
		defaultTask = claimed[0].ID
	}

	type candidate struct {
		url, task string
		view      *stopPRView
	}
	var cands []candidate
	seen := map[string]bool{}
	add := func(raw, task string, view *stopPRView) {
		u := firstPRURL(raw)
		if u == "" || !strings.Contains(u, "/pull/") {
			return
		}
		key := prKey(u)
		if seen[key] {
			return
		}
		seen[key] = true
		if task == "" {
			task = defaultTask
		}
		cands = append(cands, candidate{url: u, task: task, view: view})
	}
	// Tasks first, so a PR also named in the wrap-up parks the task that
	// records it rather than the default.
	tasks := map[string]*beadsapi.BeadDetail{}
	for _, t := range claimed {
		tasks[t.ID] = t
		add(t.Fields["mr_url"], t.ID, nil)
	}
	if defaultTask != "" && tasks[defaultTask] == nil {
		if t, err := d.GetBead(ctx, defaultTask); err == nil && t != nil {
			tasks[t.ID] = t
			add(t.Fields["mr_url"], t.ID, nil)
		}
	}
	if wrapup != nil {
		for _, u := range wrapup.PullRequests {
			add(u, "", nil)
		}
	}
	if v, err := viewStopPR(ctx, ""); err == nil {
		add(v.URL, "", v)
	}

	var results []prParkResult
	for _, c := range cands {
		pr := c.view
		if pr == nil {
			v, err := viewStopPR(ctx, c.url)
			if err != nil {
				results = append(results, prParkResult{URL: c.url, Task: c.task, Err: fmt.Errorf("could not read its state: %w", err)})
				continue
			}
			pr = v
		}
		if pr.State != "OPEN" {
			continue
		}
		// After the state check: a hold on a merged or closed PR must not
		// exempt the task from the claimed-work gate.
		if hold := taskPRHold(tasks[c.task]); hold != "" {
			results = append(results, prParkResult{URL: c.url, Task: c.task, Held: hold})
			continue
		}
		if esc := findOpenMergeEscalation(ctx, d, prKey(pr.URL)); esc != nil {
			continue
		}
		if ref := sentBackAtHead(ctx, pr); ref != "" {
			results = append(results, prParkResult{URL: pr.URL, Task: c.task, SentBack: ref, HeadSHA: pr.HeadRefOid})
			continue
		}
		results = append(results, filePRPark(ctx, pr, c.task))
	}
	return results
}

// prVerdictComment is one PR comment as `gh pr view --json comments` lists it.
type prVerdictComment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
}

// sentBackAtHead reports where the merge desk sent pr back at its current
// head (the comment URL, else its time), "" when it did not (gd-bV2iypX2mD).
// It reads the newest comment whose first line starts "Verdict:"; that verdict
// counts only if it is "Verdict: sent back" and its first line names
// pr.HeadRefOid, in full or as a hex prefix of at least 7 chars. A gh failure
// reads as "" so the park files as before.
func sentBackAtHead(ctx context.Context, pr *stopPRView) string {
	if pr == nil || pr.HeadRefOid == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, draftLookupTimeout)
	defer cancel()
	out, err := prGHRun(ctx, "pr", "view", pr.URL, "--json", "comments")
	if err != nil {
		return ""
	}
	var v struct {
		Comments []prVerdictComment `json:"comments"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return ""
	}
	// gh lists oldest first; the stable sort keeps that order for equal times.
	sort.SliceStable(v.Comments, func(i, j int) bool { return v.Comments[i].CreatedAt < v.Comments[j].CreatedAt })
	for i := len(v.Comments) - 1; i >= 0; i-- {
		c := v.Comments[i]
		first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(c.Body), "\n", 2)[0])
		if !strings.HasPrefix(first, "Verdict:") {
			continue
		}
		if !strings.HasPrefix(first, "Verdict: sent back") || !namesHead(first, pr.HeadRefOid) {
			return ""
		}
		if c.URL != "" {
			return c.URL
		}
		if c.CreatedAt != "" {
			return c.CreatedAt
		}
		return "a PR comment"
	}
	return ""
}

// namesHead reports whether line names the commit head: a hex token of at
// least 7 chars that head starts with.
func namesHead(line, head string) bool {
	head = strings.ToLower(head)
	tokens := strings.FieldsFunc(strings.ToLower(line), func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	})
	for _, t := range tokens {
		if len(t) >= 7 && strings.HasPrefix(head, t) {
			return true
		}
	}
	return false
}

// short7 is the first 7 chars of a commit sha.
func short7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// filePRPark files the merge escalation for one open PR through the gb
// subprocess, so it takes exactly the `gb pr ready` / `gb escalation create`
// path (draft refusal, merge dedupe, capability guard, blocks-edge,
// close-on-execute).
func filePRPark(ctx context.Context, pr *stopPRView, task string) prParkResult {
	ctx, cancel := context.WithTimeout(ctx, prParkFileTimeout)
	defer cancel()
	res := prParkResult{URL: pr.URL, Task: task}
	var args []string
	if pr.IsDraft {
		args = []string{"pr", "ready", pr.URL}
		if task != "" {
			args = append(args, "--task", task)
		}
	} else {
		args = mergeEscalationArgs(&pr.prView, task, fmt.Sprintf(
			"Merge %s once CI is green. Filed by gb stop: the agent that opened it stopped with the PR open and no merge escalation.", pr.URL))
	}
	out, err := prGBRun(ctx, args...)
	if err != nil {
		res.Err = err
		return res
	}
	res.EscalationID = escalationIDFromOutput(out)
	if res.EscalationID == "" {
		res.Err = fmt.Errorf("gb %s printed no escalation_id: %s", args[0], strings.TrimSpace(out))
		return res
	}
	res.Filed = true
	res.Readied = pr.IsDraft
	return res
}

// prParkNote renders the park results as wrap-up prose, "" when nothing was
// filed, held, sent back or failed.
func prParkNote(results []prParkResult) string {
	var lines []string
	for _, r := range results {
		switch {
		case r.Held != "":
			lines = append(lines, fmt.Sprintf("gb stop left %s as it is and filed no merge escalation: %s.", r.URL, r.Held))
		case r.SentBack != "":
			task := r.Task
			if task == "" {
				task = "<task>"
			}
			lines = append(lines, fmt.Sprintf("gb stop filed no merge escalation: the merge desk sent back %s at its current head %s (%s); push a commit that addresses its Required changes and run gb pr ready, or kd unclaim %s to hand it back.",
				r.URL, short7(r.HeadSHA), r.SentBack, task))
		case r.Readied && r.Task != "":
			lines = append(lines, fmt.Sprintf("gb stop marked draft %s ready and filed merge escalation %s; it parks %s and closes it on merge.", r.URL, r.EscalationID, r.Task))
		case r.Readied:
			lines = append(lines, fmt.Sprintf("gb stop marked draft %s ready and filed merge escalation %s.", r.URL, r.EscalationID))
		case r.Filed && r.Task != "":
			lines = append(lines, fmt.Sprintf("gb stop filed merge escalation %s for %s; it parks %s and closes it on merge.", r.EscalationID, r.URL, r.Task))
		case r.Filed:
			lines = append(lines, fmt.Sprintf("gb stop filed merge escalation %s for %s.", r.EscalationID, r.URL))
		case r.Err != nil:
			lines = append(lines, fmt.Sprintf("gb stop could not file a merge escalation for %s (%v); if it is still open, it has none.", r.URL, r.Err))
		}
	}
	return strings.Join(lines, "\n")
}

// applyPRParkToWrapUp records the park in the wrap-up: the note goes on the
// end of handoff_notes and each filed PR joins pull_requests.
func applyPRParkToWrapUp(w *WrapUp, results []prParkResult) {
	note := prParkNote(results)
	if w == nil || note == "" {
		return
	}
	if strings.TrimSpace(w.HandoffNotes) == "" {
		w.HandoffNotes = note
	} else {
		w.HandoffNotes = strings.TrimRight(w.HandoffNotes, "\n") + "\n" + note
	}
	listed := map[string]bool{}
	for _, u := range w.PullRequests {
		listed[prKey(u)] = true
	}
	for _, r := range results {
		if r.Filed && !listed[prKey(r.URL)] {
			w.PullRequests = append(w.PullRequests, r.URL)
			listed[prKey(r.URL)] = true
		}
	}
}
