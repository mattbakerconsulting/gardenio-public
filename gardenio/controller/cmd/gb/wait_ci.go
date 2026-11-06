package main

// gb wait ci — park this agent on a GitHub CI run instead of polling it (plan
// gardenio/plans/wake-on-ci-completion.md §2.1, task gd-AwuRDezONo).
//
// The wait is NOT a bead of its own (operator steer): it is the ci_wait_* fields
// on the CALLING agent's own bead (beadsapi.CIWait*). One wait per agent; a
// second `gb wait ci` replaces the first. The sequence is
//
//	git push; gb wait ci --repo O/R --sha <40-char sha>; gb stop
//
// `gb stop` then treats the claimed task named by ci_wait_task as parked (third
// findParkHandle branch) and closes the bead successor=same so the identity is
// revivable in place. The bridge resolver flips ci_wait_state to resolved (or
// timed_out at the deadline), writes ci_wait_result and event-wakes the agent
// with wake_reason=ci; the woken agent reads the conclusions with
// `gb wait ci --result`, which marks the wait consumed.
//
// Until that resolver ships (plan P2-P4, gd-j4OrJOprh9) nothing on the bridge
// side ever resolves a wait, so the park also carries a timer backstop
// (gd-PwyvuF0fHp, plan §2.5's "belt-and-braces"): `gb stop` stamps
// next_respawn_at, the controller wake-scanner revives the identity in place
// when it comes due, and `gb wait ci --result` on a still-pending wait reads the
// runs from GitHub itself and resolves it. Still running: `gb stop` again.
// The woken session's work prompt leads with that instruction
// (ciWaitWakeHint), and `gb stop` asks GitHub too before it re-parks
// (settleCIWaitForStop), so a wake can never re-park on a concluded run
// (gd-3iOqZlkZLV).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// defaultCIWaitTimeout is how long a wait stays pending before it resolves as
// timed_out (plan §2.5, proposed 2 h): a lost webhook can never strand a task.
const defaultCIWaitTimeout = 2 * time.Hour

// ciWaitPollInterval is how long a stop parked on a pending CI wait sleeps
// before the wake-scanner revives it to look again. Each timer wake spends one
// of the identity's DefaultMaxRespawns (5), and five wakes at this interval
// (the last clamped to the deadline) span the 2 h default timeout.
const ciWaitPollInterval = 25 * time.Minute

var waitCmd = &cobra.Command{
	Use:     "wait",
	Short:   "Park this agent on an external event and be woken when it happens",
	GroupID: "agent",
}

var waitCICmd = &cobra.Command{
	Use:   "ci",
	Short: "Park this agent on a GitHub CI run; it is woken when the run concludes",
	Long: `Record a CI wait on THIS agent's own bead, then ` + "`gb stop`" + `. When every
workflow run for the commit concludes (red or green), or the deadline passes,
the agent is woken in place with the conclusions in hand. Do not poll CI
(no gh run watch / gh run list loops): a Bash call that waits on CI holds the
pod for up to an hour.

  git push
  gb wait ci --repo mattbakerconsulting/gardenio --sha "$(git rev-parse HEAD)"
  gb stop --wrapup '...'

--sha must be the FULL 40-character sha: an abbreviated sha matches no runs.
--repo must be the repository's current OWNER/NAME (git remote get-url origin):
webhook deliveries carry the current name, so a pre-transfer name never matches one.
--workflow (repeatable) narrows the wait to the named workflows. The wait parks
--requesting-task, which defaults to this agent's assigned task_id: ` + "`gb stop`" + `
lets that claimed task stay in_progress. A second ` + "`gb wait ci`" + ` replaces the first.

After waking:
  gb wait ci --status   # read-only: state, key, deadline, result
  gb wait ci --result   # print the per-workflow conclusions and mark the wait consumed
  gb wait ci --cancel   # drop a pending wait (e.g. the PR merged out from under it)

A parked stop is also woken every 25m to look for itself. If --result
finds the wait still pending it asks GitHub (gh api) for the sha's runs: when
every awaited run has completed (or the deadline has passed) it resolves the
wait and prints the conclusions; otherwise it says CI is still running and you
` + "`gb stop`" + ` again. A pending wait is cancelled instead (by --result and by
` + "`gb stop`" + `) once its task is closed, or once the sha has no runs and every
pull request carrying it has moved its head past it or closed: no CI will come.`,
	Args: cobra.NoArgs,
	RunE: runWaitCI,
}

func init() {
	waitCICmd.Flags().String("repo", "", "GitHub repository as OWNER/REPO")
	waitCICmd.Flags().String("sha", "", "full 40-character commit sha the CI runs are for")
	waitCICmd.Flags().StringSlice("workflow", nil, "workflow name to wait on (repeatable; default: every workflow run for the sha)")
	waitCICmd.Flags().String("requesting-task", "", "claimed task this wait parks (default: this agent's task_id)")
	waitCICmd.Flags().Duration("timeout", defaultCIWaitTimeout, "resolve as timed_out (and wake) if CI has not concluded by then")
	waitCICmd.Flags().Bool("status", false, "print the current wait (read-only)")
	waitCICmd.Flags().Bool("result", false, "print the concluded wait's result and mark it consumed (a pending wait asks GitHub first)")
	waitCICmd.Flags().Bool("cancel", false, "drop this agent's pending wait: nothing parks on it or wakes for it")
	waitCmd.AddCommand(waitCICmd)
}

func runWaitCI(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	status, _ := cmd.Flags().GetBool("status")
	result, _ := cmd.Flags().GetBool("result")
	cancelWait, _ := cmd.Flags().GetBool("cancel")
	if (status && result) || (status && cancelWait) || (result && cancelWait) {
		return fmt.Errorf("--status, --result and --cancel are mutually exclusive")
	}

	agentID, err := resolveAgentIDWithFallback(ctx, "")
	if err != nil {
		return fmt.Errorf("agent identity required: %w", err)
	}
	bead, err := daemon.GetBead(ctx, agentID)
	if err != nil {
		return fmt.Errorf("reading agent bead %s: %w", agentID, err)
	}
	wait := beadsapi.CIWaitFromFields(bead.Fields)

	switch {
	case status:
		printCIWait(cmd, agentID, wait)
		return nil
	case result:
		return consumeCIWait(cmd, agentID, wait)
	case cancelWait:
		return cancelCIWait(cmd, agentID, wait)
	}

	repo, _ := cmd.Flags().GetString("repo")
	sha, _ := cmd.Flags().GetString("sha")
	workflows, _ := cmd.Flags().GetStringSlice("workflow")
	task, _ := cmd.Flags().GetString("requesting-task")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	fields, err := buildCIWaitFields(repo, sha, workflows, task, bead.Fields["task_id"], timeout, time.Now())
	if err != nil {
		return err
	}
	if wait.Pending() && wait.Key != fields[beadsapi.FieldCIWaitKey] {
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: replacing the pending wait on %s.\n", wait.Key)
	}
	if err := daemon.UpdateBeadFields(ctx, agentID, fields); err != nil {
		return fmt.Errorf("recording CI wait on %s: %w", agentID, err)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"agent_bead": agentID,
			"state":      fields[beadsapi.FieldCIWaitState],
			"key":        fields[beadsapi.FieldCIWaitKey],
			"workflows":  fields[beadsapi.FieldCIWaitWorkflows],
			"task":       fields[beadsapi.FieldCIWaitTask],
			"deadline":   fields[beadsapi.FieldCIWaitDeadline],
		})
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "CI wait recorded on %s: %s", agentID, fields[beadsapi.FieldCIWaitKey])
	if w := fields[beadsapi.FieldCIWaitWorkflows]; w != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " (workflows: %s)", w)
	}
	fmt.Fprintf(cmd.OutOrStdout(), ", deadline %s.\n", fields[beadsapi.FieldCIWaitDeadline])
	if t := fields[beadsapi.FieldCIWaitTask]; t != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Now `gb stop`: %s stays in_progress, parked on this wait; you are woken when CI concludes.\n", t)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Now `gb stop`; you are woken when CI concludes. (No task is parked: pass --requesting-task to keep a claim.)\n")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "When you are woken, run `gb wait ci --result`; if CI is still running it says so, and you `gb stop` again.\n")
	return nil
}

// ciWaitNextWake is the next_respawn_at a stop parked on w stamps: one poll
// interval from now, clamped to the wait's deadline so a timed-out wait is
// noticed on time. An unreadable deadline falls back to the plain interval.
func ciWaitNextWake(w beadsapi.CIWait, now time.Time) string {
	next := now.Add(ciWaitPollInterval)
	if d, err := time.Parse(time.RFC3339, w.Deadline); err == nil && d.Before(next) {
		next = d
		if next.Before(now) {
			next = now
		}
	}
	return next.UTC().Format(time.RFC3339)
}

// ciRun is the slice of a GitHub Actions workflow run a CI wait reads.
type ciRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	RunAttempt int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// ciRunResult is one entry of ci_wait_result, in the shape plan §2.1 documents.
type ciRunResult struct {
	Name       string `json:"name"`
	RunID      int64  `json:"run_id"`
	RunAttempt int    `json:"run_attempt"`
	Conclusion string `json:"conclusion"`
}

// listCIRuns asks GitHub for every Actions run on the wait's sha (gh api, with
// the pod's own GitHub credentials).
func listCIRuns(ctx context.Context, key string) ([]ciRun, error) {
	repo, sha, ok := strings.Cut(key, "@")
	if !ok {
		return nil, fmt.Errorf("CI wait key %q is not OWNER/REPO@SHA", key)
	}
	out, err := prGHRun(ctx, "api", "repos/"+repo+"/actions/runs?head_sha="+sha+"&per_page=100")
	if err != nil {
		return nil, err
	}
	var resp struct {
		WorkflowRuns []ciRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("parsing the Actions runs for %s: %w", key, err)
	}
	return resp.WorkflowRuns, nil
}

// ciWaitOutcome decides a pending wait from the runs GitHub lists for its sha.
// Only each workflow's newest run counts (a re-run supersedes the old one);
// --workflow narrows the set, and a named workflow with no run yet is still
// awaited. The wait resolves once every awaited run has completed, times out
// once the deadline passes first, and otherwise stays pending. result is the
// ci_wait_result JSON (an unfinished run reports its status as the
// conclusion); summary is a one-line progress count.
func ciWaitOutcome(runs []ciRun, workflows, deadline string, now time.Time) (state, result, summary string) {
	latest := map[string]ciRun{}
	for _, r := range runs {
		if cur, ok := latest[r.Name]; !ok || r.ID > cur.ID {
			latest[r.Name] = r
		}
	}
	var names []string
	if workflows != "" {
		names = strings.Split(workflows, ",")
	} else {
		for name := range latest {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	done := 0
	results := make([]ciRunResult, 0, len(names))
	for _, name := range names {
		r, ok := latest[name]
		switch {
		case !ok:
			results = append(results, ciRunResult{Name: name, Conclusion: "not_started"})
		case r.Status == "completed":
			done++
			results = append(results, ciRunResult{Name: name, RunID: r.ID, RunAttempt: r.RunAttempt, Conclusion: r.Conclusion})
		default:
			results = append(results, ciRunResult{Name: name, RunID: r.ID, RunAttempt: r.RunAttempt, Conclusion: r.Status})
		}
	}
	raw, _ := json.Marshal(results)
	summary = fmt.Sprintf("%d/%d workflow runs concluded", done, len(names))

	state = beadsapi.CIWaitStatePending
	if len(names) > 0 && done == len(names) {
		state = beadsapi.CIWaitStateResolved
	} else if d, err := time.Parse(time.RFC3339, deadline); err == nil && !now.Before(d) {
		state = beadsapi.CIWaitStateTimedOut
	}
	return state, string(raw), summary
}

// ciPull is the slice of a GitHub pull request ciWaitSuperseded reads.
type ciPull struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// ciWaitSuperseded is asked when GitHub lists no run for a wait's sha. It
// returns why no CI will ever run there, or "" when runs may still start.
// The sha is superseded when every pull request carrying it has moved its head
// past it or is closed: the merge desk merged main into the branch, CI ran on
// that new head, and the awaited sha never got a run (gd-lHWcyCRj1Q). A sha
// with no pull request (a direct push), the open head of one, or a PR's merge
// commit may still get runs, so those stay pending.
func ciWaitSuperseded(ctx context.Context, key string) (string, error) {
	repo, sha, ok := strings.Cut(key, "@")
	if !ok {
		return "", fmt.Errorf("CI wait key %q is not OWNER/REPO@SHA", key)
	}
	out, err := prGHRun(ctx, "api", "repos/"+repo+"/commits/"+sha+"/pulls")
	if err != nil {
		return "", err
	}
	var pulls []ciPull
	if err := json.Unmarshal([]byte(out), &pulls); err != nil {
		return "", fmt.Errorf("parsing the pull requests for %s: %w", key, err)
	}
	if len(pulls) == 0 {
		return "", nil
	}
	var where []string
	for _, p := range pulls {
		if p.MergeCommitSHA == sha || (p.State == "open" && p.Head.SHA == sha) {
			return "", nil
		}
		state := p.State
		if p.MergedAt != "" {
			state = "merged"
		}
		where = append(where, fmt.Sprintf("PR #%d is %s at head %s", p.Number, state, shortSHA(p.Head.SHA)))
	}
	return fmt.Sprintf("no workflow run on %s and none will start: %s", shortSHA(sha), strings.Join(where, "; ")), nil
}

// ciWaitTaskClosed reports whether the task a wait parks is closed. Nothing
// then acts on the run, so a wake for it only spends a pod (gd-lHWcyCRj1Q). A
// read error counts as open: the wait is kept.
func ciWaitTaskClosed(ctx context.Context, task string) bool {
	if task == "" {
		return false
	}
	b, err := daemon.GetBead(ctx, task)
	return err == nil && b != nil && b.Status == "closed"
}

// refreshCIWait asks GitHub whether a pending wait has concluded and returns
// the wait as GitHub now sees it (State resolved or timed_out with its Result,
// cancelled with the reason as its Result, or still pending) plus a one-line
// summary. A wait whose task is closed, or whose sha ciWaitSuperseded says
// will never get a run, is cancelled. A wait that is not pending is returned
// unchanged without a call.
func refreshCIWait(ctx context.Context, w beadsapi.CIWait, now time.Time) (beadsapi.CIWait, string, error) {
	if !w.Pending() {
		return w, "", nil
	}
	if ciWaitTaskClosed(ctx, w.Task) {
		w.State = beadsapi.CIWaitStateCancelled
		w.Result = fmt.Sprintf("task %s is closed, so nothing waits on this CI any more", w.Task)
		return w, w.Result, nil
	}
	runs, err := listCIRuns(ctx, w.Key)
	if err != nil {
		return w, "", err
	}
	if len(runs) == 0 {
		// Best-effort: a failed lookup leaves the wait to its deadline.
		if reason, err := ciWaitSuperseded(ctx, w.Key); err == nil && reason != "" {
			w.State, w.Result = beadsapi.CIWaitStateCancelled, reason
			return w, reason, nil
		}
	}
	state, result, summary := ciWaitOutcome(runs, w.Workflows, w.Deadline, now)
	if state != beadsapi.CIWaitStatePending {
		w.State, w.Result = state, result
	}
	return w, summary, nil
}

// ciWaitStopCheckTimeout bounds the GitHub read settleCIWaitForStop makes, so
// a slow gh never holds a stop.
const ciWaitStopCheckTimeout = 20 * time.Second

// settleCIWaitForStop runs before the stop gates when this agent's own CI wait
// is still pending. Nothing on the bridge resolves a wait yet (plan P2-P4), so
// a woken agent that went straight to `gb stop` re-parked on a run that had
// concluded hours earlier, again on every wake until the respawn budget ran
// out (gd-3iOqZlkZLV). It asks GitHub and, when every awaited run has
// completed or the deadline has passed, records the concluded state and result
// on the bead; checkCIWaitConcluded then refuses the stop and points the agent
// at `gb wait ci --result`. A wait refreshCIWait cancels (task closed, sha
// superseded) is recorded as cancelled, so the stop does not park on it.
// Best-effort: a GitHub or daemon error leaves the wait pending and the stop
// parks as before.
func settleCIWaitForStop(ctx context.Context, agentID string) {
	w := loadCIWait(ctx, agentID)
	if !w.Pending() {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, ciWaitStopCheckTimeout)
	defer cancel()
	got, _, err := refreshCIWait(qctx, w, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: could not read CI runs for %s from GitHub (%v); parking on the wait as recorded.\n", w.Key, err)
		return
	}
	if got.Pending() {
		return
	}
	if err := daemon.UpdateBeadFields(ctx, agentID, map[string]string{
		beadsapi.FieldCIWaitState:  got.State,
		beadsapi.FieldCIWaitResult: got.Result,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: CI for %s has concluded (%s) but recording it on %s failed: %v\n", w.Key, got.State, agentID, err)
		return
	}
	if got.State == beadsapi.CIWaitStateCancelled {
		fmt.Fprintf(os.Stderr, "Note: dropped the CI wait on %s: %s.\n", w.Key, got.Result)
	}
}

// ciWaitWakeHint reads this pod's agent bead and returns ciWaitWakeHintFor its
// CI wait, or "" when there is no bead, no daemon or nothing unread.
func ciWaitWakeHint() string {
	agentBeadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if agentBeadID == "" || daemon == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bead, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil || bead == nil {
		return ""
	}
	return ciWaitWakeHintFor(beadsapi.CIWaitFromFields(bead.Fields))
}

// ciWaitWakeHintFor is the lead-in of the work prompt a session gets when it
// resumes with a pending or concluded (unread) CI wait. A timer or manual wake
// otherwise got only the plain task nudge, and the woken agent went straight
// back to `gb stop` without reading CI (gd-3iOqZlkZLV). A consumed wait, or
// none, has nothing to read.
func ciWaitWakeHintFor(w beadsapi.CIWait) string {
	if !w.Pending() && !w.Concluded() {
		return ""
	}
	return fmt.Sprintf("You were parked on `gb wait ci` for %s. Before anything else run `gb wait ci --result`: "+
		"if CI has concluded it prints each workflow's conclusion, so act on them (fix and push a red run, then `gb wait ci` again; "+
		"or finish the task); if it says CI is still running, `gb stop` again. ", w.Key)
}

// buildCIWaitFields validates the flags and returns the ci_wait_* PATCH for a
// new pending wait. Every key is written, so a replaced wait never inherits a
// stale workflows filter or result. defaultTask is the agent bead's task_id.
func buildCIWaitFields(repo, sha string, workflows []string, task, defaultTask string, timeout time.Duration, now time.Time) (map[string]string, error) {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(sha) == "" {
		return nil, fmt.Errorf("--repo and --sha are required (or pass --status / --result)")
	}
	key, err := beadsapi.CIWaitKey(repo, sha)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("--timeout must be positive, got %s", timeout)
	}
	var names []string
	for _, w := range workflows {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		// The filter is stored comma-joined, so a comma inside a name would
		// split it into two names the resolver can never match.
		if strings.Contains(w, ",") {
			return nil, fmt.Errorf("--workflow %q: a workflow name cannot contain a comma; repeat the flag instead", w)
		}
		names = append(names, w)
	}
	task = strings.TrimSpace(task)
	if task == "" {
		task = strings.TrimSpace(defaultTask)
	}
	return map[string]string{
		beadsapi.FieldCIWaitState:     beadsapi.CIWaitStatePending,
		beadsapi.FieldCIWaitKey:       key,
		beadsapi.FieldCIWaitWorkflows: strings.Join(names, ","),
		beadsapi.FieldCIWaitTask:      task,
		beadsapi.FieldCIWaitDeadline:  now.Add(timeout).UTC().Format(time.RFC3339),
		beadsapi.FieldCIWaitResult:    "",
	}, nil
}

func printCIWait(cmd *cobra.Command, agentID string, w beadsapi.CIWait) {
	if jsonOutput {
		printJSON(ciWaitJSON(agentID, w))
		return
	}
	out := cmd.OutOrStdout()
	if w.State == "" {
		fmt.Fprintf(out, "No CI wait on %s.\n", agentID)
		return
	}
	fmt.Fprintf(out, "CI wait on %s: %s\n", agentID, w.State)
	fmt.Fprintf(out, "  key:       %s\n", w.Key)
	if w.Workflows != "" {
		fmt.Fprintf(out, "  workflows: %s\n", w.Workflows)
	}
	if w.Task != "" {
		fmt.Fprintf(out, "  task:      %s\n", w.Task)
	}
	fmt.Fprintf(out, "  deadline:  %s\n", w.Deadline)
	if w.Result != "" {
		fmt.Fprintf(out, "  result:    %s\n", w.Result)
	}
}

// consumeCIWait prints a concluded wait's result and marks it consumed, which
// releases the stop gate's "CI already concluded" refusal. Reading an already
// consumed wait prints the result again without writing.
func consumeCIWait(cmd *cobra.Command, agentID string, w beadsapi.CIWait) error {
	switch {
	case w.State == "":
		return fmt.Errorf("no CI wait on %s — record one with `gb wait ci --repo ... --sha ...`", agentID)
	case w.Pending():
		// No resolver has concluded it: look at GitHub directly (the timer
		// backstop's half of the wake, gd-PwyvuF0fHp).
		got, summary, err := refreshCIWait(cmd.Context(), w, time.Now())
		if err != nil {
			return fmt.Errorf("CI wait on %s is still pending and its runs could not be read from GitHub: %w", w.Key, err)
		}
		if got.Pending() {
			return fmt.Errorf("CI wait on %s is still pending (%s, deadline %s) — `gb stop` and you are woken again in about %s", w.Key, summary, w.Deadline, ciWaitPollInterval)
		}
		w = got
		state := beadsapi.CIWaitStateConsumed
		if w.State == beadsapi.CIWaitStateCancelled {
			state = beadsapi.CIWaitStateCancelled
		}
		if err := daemon.UpdateBeadFields(cmd.Context(), agentID, map[string]string{
			beadsapi.FieldCIWaitState:  state,
			beadsapi.FieldCIWaitResult: w.Result,
		}); err != nil {
			return fmt.Errorf("recording the concluded CI wait on %s: %w", agentID, err)
		}
	case w.Concluded():
		if err := daemon.UpdateBeadFields(cmd.Context(), agentID, map[string]string{
			beadsapi.FieldCIWaitState: beadsapi.CIWaitStateConsumed,
		}); err != nil {
			return fmt.Errorf("marking CI wait on %s consumed: %w", agentID, err)
		}
	}

	if jsonOutput {
		out := ciWaitJSON(agentID, w)
		out["concluded_as"] = w.State
		printJSON(out)
		return nil
	}
	if w.State == beadsapi.CIWaitStateCancelled {
		fmt.Fprintf(cmd.OutOrStdout(), "CI wait %s was cancelled: %s.\n", w.Key, w.Result)
		fmt.Fprintf(cmd.OutOrStdout(), "Nothing will wake you for it. If the task still needs CI, `gb wait ci` on the branch's current head sha; otherwise finish the task and `gb stop`.\n")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "CI wait %s concluded as %s.\n", w.Key, w.State)
	if w.Result == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "No per-workflow result was recorded.\n")
		return nil
	}
	var runs []ciRunResult
	if err := json.Unmarshal([]byte(w.Result), &runs); err != nil {
		// Not the documented array shape: show it verbatim rather than hide it.
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", w.Result)
		return nil
	}
	for _, r := range runs {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s (run %d, attempt %d)\n",
			r.Name, r.Conclusion, r.RunID, r.RunAttempt)
	}
	return nil
}

// cancelCIWait drops this agent's pending wait, so `gb stop` no longer parks
// its task on it and no timer or resolver wakes the agent for it: for a wait
// whose PR merged out from under it, or one recorded on the wrong sha
// (gd-lHWcyCRj1Q). A concluded wait is read with --result instead; a consumed
// or cancelled one is left as is.
func cancelCIWait(cmd *cobra.Command, agentID string, w beadsapi.CIWait) error {
	out := cmd.OutOrStdout()
	switch {
	case w.State == "":
		return fmt.Errorf("no CI wait on %s", agentID)
	case w.Concluded():
		return fmt.Errorf("CI wait on %s already concluded (%s): read it with `gb wait ci --result`", w.Key, w.State)
	case !w.Pending():
		fmt.Fprintf(out, "CI wait %s is already %s; nothing to cancel.\n", w.Key, w.State)
		return nil
	}
	w.State = beadsapi.CIWaitStateCancelled
	w.Result = "cancelled by the agent (gb wait ci --cancel)"
	if err := daemon.UpdateBeadFields(cmd.Context(), agentID, map[string]string{
		beadsapi.FieldCIWaitState:  w.State,
		beadsapi.FieldCIWaitResult: w.Result,
	}); err != nil {
		return fmt.Errorf("cancelling CI wait on %s: %w", agentID, err)
	}
	if jsonOutput {
		printJSON(ciWaitJSON(agentID, w))
		return nil
	}
	fmt.Fprintf(out, "CI wait %s cancelled; nothing will wake you for it.\n", w.Key)
	if w.Task != "" {
		fmt.Fprintf(out, "`gb stop` no longer parks %s on it: close it, or record a new wait, before you stop.\n", w.Task)
	}
	return nil
}

func ciWaitJSON(agentID string, w beadsapi.CIWait) map[string]any {
	out := map[string]any{
		"agent_bead": agentID,
		"state":      w.State,
		"key":        w.Key,
		"workflows":  w.Workflows,
		"task":       w.Task,
		"deadline":   w.Deadline,
	}
	if w.Result != "" {
		var parsed any
		if json.Unmarshal([]byte(w.Result), &parsed) == nil {
			out["result"] = parsed
		} else {
			out["result"] = w.Result
		}
	}
	return out
}
