package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ── gb ci rerun / dispatch (P9.1, gd-FsqKPpFgaf) ────────────────────────────
//
// Crews hold a GitHub App installation token with Actions: write (cutover
// gd-gCzUTAg2a1). These verbs are the only sanctioned way to spend it, so a
// re-run or dispatch no longer needs an operator escalation and is still
// bounded by the owner CI rule of 2026-10-31 (root CLAUDE.md "CI on pull
// requests"):
//
//   - only ci.yml, checks.yml, images.yml and lands-perf.yml, only on an
//     agent/ branch;
//   - a re-run re-runs the failed jobs of a finished run at the branch head,
//     once per run (docs/merge-desk.md CANCELLED OR LOST JOBS);
//   - at most ciActionsPerHour re-runs plus dispatches per branch (one PR)
//     per rolling hour, counted from GitHub's own run history, so the budget
//     holds across agents and pod restarts with no state of ours.

// ciAllowedWorkflows are the workflow files gb ci rerun / dispatch may touch.
// lands-perf.yml is how a PR to the Commons proves the Commons v2 budgets
// (gd-4s2Rw9hgBd): it never runs on a pull request.
var ciAllowedWorkflows = []string{"ci.yml", "checks.yml", "images.yml", "lands-perf.yml"}

// ciAgentBranchPrefix is the prefix of the branches gb pr open opens PRs from
// (agent/<slug>/<bead>). Main, release tags and human branches are refused.
const ciAgentBranchPrefix = "agent/"

// ciActionsPerHour is the per-branch budget of re-runs plus dispatches.
const ciActionsPerHour = 2

// ciRetryBotLogin is the actor retry-lost-runners.yml re-runs as (it uses
// GITHUB_TOKEN). Its re-runs are the automatic lost-runner path and do not
// spend a crew's budget.
const ciRetryBotLogin = "github-actions[bot]"

// ciNow is the clock the budget window is measured on. A var for tests.
var ciNow = time.Now

var ciRerunCmd = &cobra.Command{
	Use:   "rerun <run-id-or-url>",
	Short: "Re-run the failed jobs of an allowlisted GitHub Actions run on an agent branch (once per run)",
	Long: `Re-runs only the failed and cancelled jobs of a finished GitHub Actions run
(POST .../actions/runs/<id>/rerun-failed-jobs), for a job that never reached
a verdict: a lost runner retry-lost-runners.yml did not re-run, a cancelled
job, an infra flake. Never use it to re-prove a red your change caused, or a
red that is only main's (fix main).

Refused unless all hold:
  - the run's workflow is ci.yml, checks.yml, images.yml or lands-perf.yml;
  - its branch starts with agent/ (not main, not a release tag);
  - the run has finished and did not succeed;
  - it was never re-run (run_attempt 1): a second failure goes back to the
    crew, as docs/merge-desk.md CANCELLED OR LOST JOBS says;
  - its head is still the branch head (re-running a superseded sha is waste);
  - the branch has had fewer than 2 re-runs plus dispatches in the last hour
    (retry-lost-runners.yml's own re-runs are not counted).

<run> is a run id or a run/job URL (https://github.com/O/R/actions/runs/<id>).
The repo is the URL's, else --repo, else the current checkout's. The re-run
is noted on your task bead (--task, else the agent bead's task_id).`,
	Args: cobra.ExactArgs(1),
	RunE: runCIRerun,
}

var ciDispatchCmd = &cobra.Command{
	Use:   "dispatch <workflow> --ref <agent-branch> [-f key=value]...",
	Short: "Dispatch ci.yml, checks.yml, images.yml or lands-perf.yml on an agent branch (2 per branch per hour)",
	Long: `Starts a workflow_dispatch run of <workflow> (ci.yml, checks.yml,
images.yml or lands-perf.yml; ".github/workflows/" may be included) on --ref,
which must be an agent/ branch. A dispatched run is a full run (every path
gate open), so this is for evidence the light draft tier does not give, not a
way round it.

-f key=value passes a workflow input, e.g. -f images=controller for
images.yml. A PR that touches the Commons proves its perf budgets with
lands-perf.yml -f surfaces=commonsthread,commonsplan. Refused once the branch has had 2 re-runs plus dispatches in the
last hour (retry-lost-runners.yml's re-runs are not counted). The dispatch is
noted on your task bead (--task, else the agent bead's task_id).

GitHub returns no run id for a dispatch; to act on the run, gb wait ci --repo
<owner/name> --sha <branch head>, then gb stop.`,
	Args: cobra.ExactArgs(1),
	RunE: runCIDispatch,
}

func init() {
	for _, c := range []*cobra.Command{ciRerunCmd, ciDispatchCmd} {
		c.Flags().String("repo", "", "owner/name (default: the run URL's repo, else the current checkout's)")
		c.Flags().String("task", "", "task bead to note the action on (default: the agent bead's task_id)")
	}
	ciDispatchCmd.Flags().String("ref", "", "agent/ branch to run the workflow on (required)")
	ciDispatchCmd.Flags().StringArrayP("field", "f", nil, "workflow input as key=value (repeatable)")
	ciCmd.AddCommand(ciRerunCmd, ciDispatchCmd)
}

// ghRun is the subset of a GitHub Actions workflow run (or one attempt of
// it) these verbs read.
type ghRun struct {
	ID           int64     `json:"id"`
	Path         string    `json:"path"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	RunAttempt   int       `json:"run_attempt"`
	HTMLURL      string    `json:"html_url"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	RunStartedAt time.Time `json:"run_started_at"`
	Actor        struct {
		Login string `json:"login"`
	} `json:"actor"`
	TriggeringActor struct {
		Login string `json:"login"`
	} `json:"triggering_actor"`
}

// ciGHAPI runs `gh api` and decodes the JSON reply into v (nil: ignore it).
func ciGHAPI(ctx context.Context, v any, args ...string) error {
	out, err := prGHRun(ctx, append([]string{"api"}, args...)...)
	if err != nil {
		return err
	}
	if v == nil || strings.TrimSpace(out) == "" {
		return nil
	}
	return json.Unmarshal([]byte(out), v)
}

var ciRunURLRe = regexp.MustCompile(`github\.com/([^/]+/[^/]+)/actions/runs/(\d+)`)

// parseRunTarget splits a run id or run/job URL into (repo, id). repo is ""
// for a bare id.
func parseRunTarget(s string) (string, int64, error) {
	if m := ciRunURLRe.FindStringSubmatch(s); m != nil {
		id, _ := strconv.ParseInt(m[2], 10, 64)
		return m[1], id, nil
	}
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return "", 0, fmt.Errorf("%q is not a run id or https://github.com/<owner>/<repo>/actions/runs/<id> URL", s)
	}
	return "", id, nil
}

// ciRepo resolves owner/name: urlRepo, else --repo, else the checkout's.
func ciRepo(cmd *cobra.Command, urlRepo string) (string, error) {
	flag, _ := cmd.Flags().GetString("repo")
	if urlRepo != "" {
		if flag != "" && !strings.EqualFold(flag, urlRepo) {
			return "", fmt.Errorf("--repo %s does not match the run URL's repo %s", flag, urlRepo)
		}
		return urlRepo, nil
	}
	if flag != "" {
		return flag, nil
	}
	out, err := prGHRun(cmd.Context(), "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return "", fmt.Errorf("resolving the repo (pass --repo owner/name): %w", err)
	}
	return strings.TrimSpace(out), nil
}

// ciWorkflowFile normalises a workflow argument or run path to its file name
// ("ci.yml") and reports whether it is on the allowlist.
func ciWorkflowFile(w string) (string, bool) {
	// A run's path can carry a ref suffix ("....yml@refs/heads/x") for
	// reusable workflows; the file is what precedes it.
	w, _, _ = strings.Cut(w, "@")
	w = strings.TrimPrefix(w, ".github/workflows/")
	return w, slices.Contains(ciAllowedWorkflows, w)
}

func checkAgentBranch(branch string) error {
	if !strings.HasPrefix(branch, ciAgentBranchPrefix) {
		return fmt.Errorf("branch %q is not an agent branch (%s...): gb ci acts only on PR branches gb pr open made; main and release tags are the release captain's", branch, ciAgentBranchPrefix)
	}
	return nil
}

// ciBranchSpend counts the re-runs and dispatches started on branch since
// cutoff, leaving out retry-lost-runners.yml's re-runs.
func ciBranchSpend(ctx context.Context, repo, branch string, cutoff time.Time) (int, error) {
	var list struct {
		WorkflowRuns []ghRun `json:"workflow_runs"`
	}
	if err := ciGHAPI(ctx, &list, fmt.Sprintf("repos/%s/actions/runs?branch=%s&per_page=100", repo, url.QueryEscape(branch))); err != nil {
		return 0, fmt.Errorf("listing runs on %s: %w", branch, err)
	}
	n := 0
	for _, r := range list.WorkflowRuns {
		// Attempt 1 of a dispatched run is the dispatch; actor is attempt
		// 1's (triggering_actor is the latest attempt's).
		if r.Event == "workflow_dispatch" && !r.CreatedAt.Before(cutoff) && r.Actor.Login != ciRetryBotLogin {
			n++
		}
		if r.RunAttempt < 2 || r.UpdatedAt.Before(cutoff) {
			continue
		}
		for a := 2; a <= r.RunAttempt; a++ {
			var at ghRun
			if err := ciGHAPI(ctx, &at, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d", repo, r.ID, a)); err != nil {
				return 0, fmt.Errorf("reading attempt %d of run %d: %w", a, r.ID, err)
			}
			if !at.RunStartedAt.Before(cutoff) && at.TriggeringActor.Login != ciRetryBotLogin {
				n++
			}
		}
	}
	return n, nil
}

// checkCIBudget refuses when branch has spent its hourly budget, and returns
// the spend so far.
func checkCIBudget(ctx context.Context, repo, branch string) (int, error) {
	spent, err := ciBranchSpend(ctx, repo, branch, ciNow().Add(-time.Hour))
	if err != nil {
		return 0, err
	}
	if spent >= ciActionsPerHour {
		return spent, fmt.Errorf("branch %s has had %d re-runs/dispatches in the last hour (limit %d per PR per hour); wait for CI with gb wait ci and gb stop, or file a github_actions_rerun escalation if this one cannot wait", branch, spent, ciActionsPerHour)
	}
	return spent, nil
}

// ciNote comments note on the task bead, warning (not failing) when there is
// no task or the daemon refuses: the GitHub action has already happened.
func ciNote(cmd *cobra.Command, note string) {
	task, _ := cmd.Flags().GetString("task")
	if task == "" && daemon != nil {
		task = deriveRequestingTask(cmd.Context(), "")
	}
	if task == "" {
		fmt.Fprintf(os.Stderr, "WARNING: no task bead resolved (pass --task); not commenting: %s\n", note)
		return
	}
	if err := daemon.AddComment(cmd.Context(), task, actor, note); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not comment on %s: %v\n", task, err)
	}
}

func runCIRerun(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	urlRepo, id, err := parseRunTarget(args[0])
	if err != nil {
		return err
	}
	repo, err := ciRepo(cmd, urlRepo)
	if err != nil {
		return err
	}
	var run ghRun
	if err := ciGHAPI(ctx, &run, fmt.Sprintf("repos/%s/actions/runs/%d", repo, id)); err != nil {
		return fmt.Errorf("reading run %d: %w", id, err)
	}
	wf, ok := ciWorkflowFile(run.Path)
	if !ok {
		return fmt.Errorf("run %d is %s: gb ci rerun allows only %s; file a github_actions_rerun escalation for anything else", id, wf, strings.Join(ciAllowedWorkflows, ", "))
	}
	if err := checkAgentBranch(run.HeadBranch); err != nil {
		return err
	}
	if run.Status != "completed" {
		return fmt.Errorf("run %d is %s, not finished: wait for it (gb wait ci) before re-running anything", id, run.Status)
	}
	if run.Conclusion == "success" {
		return fmt.Errorf("run %d succeeded: nothing to re-run", id)
	}
	if run.RunAttempt > 1 {
		return fmt.Errorf("run %d was already re-run (attempt %d): a run is re-run once; look at why CI does not complete on this head", id, run.RunAttempt)
	}
	var br struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := ciGHAPI(ctx, &br, fmt.Sprintf("repos/%s/branches/%s", repo, url.PathEscape(run.HeadBranch))); err != nil {
		return fmt.Errorf("reading the head of %s: %w", run.HeadBranch, err)
	}
	if br.Commit.SHA != run.HeadSHA {
		return fmt.Errorf("run %d is for %s, but %s is now at %s: its CI is the run on the new head, not a re-run of this one", id, shortSHA(run.HeadSHA), run.HeadBranch, shortSHA(br.Commit.SHA))
	}
	spent, err := checkCIBudget(ctx, repo, run.HeadBranch)
	if err != nil {
		return err
	}
	if err := ciGHAPI(ctx, nil, "-X", "POST", fmt.Sprintf("repos/%s/actions/runs/%d/rerun-failed-jobs", repo, id)); err != nil {
		return fmt.Errorf("re-running run %d: %w", id, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "re-running the failed jobs of %s run %d on %s (%d of %d this hour): %s\n", wf, id, run.HeadBranch, spent+1, ciActionsPerHour, run.HTMLURL)
	fmt.Fprintf(cmd.OutOrStdout(), "To act on it, do not poll: gb wait ci --repo %s --sha %s, then gb stop.\n", repo, run.HeadSHA)
	ciNote(cmd, fmt.Sprintf("gb ci rerun: re-ran the failed jobs of %s run %d on %s at %s (%d of %d this hour): %s", wf, id, run.HeadBranch, shortSHA(run.HeadSHA), spent+1, ciActionsPerHour, run.HTMLURL))
	return nil
}

func runCIDispatch(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	wf, ok := ciWorkflowFile(args[0])
	if !ok {
		return fmt.Errorf("%s is not dispatchable by gb ci: only %s; file a github_workflow_dispatch escalation for anything else", wf, strings.Join(ciAllowedWorkflows, ", "))
	}
	ref, _ := cmd.Flags().GetString("ref")
	if ref == "" {
		return fmt.Errorf("--ref <agent-branch> is required")
	}
	ref = strings.TrimPrefix(ref, "refs/heads/")
	if err := checkAgentBranch(ref); err != nil {
		return err
	}
	fields, _ := cmd.Flags().GetStringArray("field")
	apiArgs := []string{"-X", "POST", "", "-f", "ref=" + ref}
	for _, f := range fields {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" {
			return fmt.Errorf("-f %q: want key=value", f)
		}
		apiArgs = append(apiArgs, "-f", fmt.Sprintf("inputs[%s]=%s", k, v))
	}
	repo, err := ciRepo(cmd, "")
	if err != nil {
		return err
	}
	apiArgs[2] = fmt.Sprintf("repos/%s/actions/workflows/%s/dispatches", repo, wf)
	spent, err := checkCIBudget(ctx, repo, ref)
	if err != nil {
		return err
	}
	if err := ciGHAPI(ctx, nil, apiArgs...); err != nil {
		return fmt.Errorf("dispatching %s on %s: %w", wf, ref, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "dispatched %s on %s (%d of %d this hour); its run: gh run list --repo %s --workflow %s --branch %s --event workflow_dispatch --limit 1\n", wf, ref, spent+1, ciActionsPerHour, repo, wf, ref)
	ciNote(cmd, fmt.Sprintf("gb ci dispatch: dispatched %s on %s in %s (%d of %d this hour)", wf, ref, repo, spent+1, ciActionsPerHour))
	return nil
}
