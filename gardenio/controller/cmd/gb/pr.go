package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// ── gb pr open / ready / draft (gd-6Wt39W9xPh, plan gd-WFMvg9sDKE) ─────────
//
// A line in a prompt drifts; a command is harder to forget. Crews opened PRs
// with raw `gh pr create` (claudemd_default.md) and filed the merge
// escalation by hand with `gb escalation create`. These verbs own both:
//
//	gb pr open   always opens a DRAFT (every PR push runs the light CI tier),
//	             appends the Operator:/Bead:/Agent: trailers and records
//	             mr_url on the task bead.
//	gb pr ready  flips the draft to ready (no new CI run: the head's light
//	             run is the verdict, gd-19ECZxvFQw), arms GitHub auto-merge
//	             when no changed file is on a merge-desk (d) path
//	             (gd-HNTG3YPBRs), files the merge escalation through `gb
//	             escalation create` (idempotent per PR, gd-f7GvZ7qahY) and
//	             says so on the task bead. On a PR already ready it only
//	             re-files the escalation (gd-0kbTgvCrFF).
//	gb pr draft  turns a ready PR back into a draft, for a big rework, and
//	             disarms auto-merge.

// prGHRun runs gh and returns its stdout. A var so tests can fake gh.
var prGHRun = func(ctx context.Context, args ...string) (string, error) {
	return prExec(ctx, "gh", args...)
}

// prGitRun runs git and returns its stdout. A var so tests can fake git.
var prGitRun = func(ctx context.Context, args ...string) (string, error) {
	return prExec(ctx, "git", args...)
}

// prGBRun runs this gb binary as a subprocess and returns its stdout. `gb pr
// ready` files its merge escalation this way so it takes exactly the path
// `gb escalation create` does (merge dedupe, capability guard, blocks-edge,
// close-on-execute) without a second copy of that RunE. A var so tests can
// run the command in-process against a fake daemon.
var prGBRun = func(ctx context.Context, args ...string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the gb binary: %w", err)
	}
	return prExec(ctx, self, args...)
}

// prExec runs name with args and returns its stdout. Stderr streams through
// (gh progress, gb notes and warnings) and is also kept for the error.
func prExec(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout = &stdout
	c.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := c.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

var prCmd = &cobra.Command{
	Use:     "pr",
	Short:   "Open, ready and re-draft GitHub pull requests",
	GroupID: "orchestration",
	Long: `Owns the PR lifecycle for a crew:

  gb pr open --title T    open the current branch as a DRAFT PR,
                          with the Operator:/Bead:/Agent: trailers, and
                          record its URL in mr_url on your task bead
  gb pr ready [<pr>]      mark it ready (no new CI run), arm auto-merge unless it
                          touches a merge-desk path, file the merge
                          escalation (one open per PR) and note it on the task
                          bead; on a PR already ready, only re-file the
                          escalation (no new CI run)
  gb pr draft [<pr>]      turn it back into a draft for a big rework and
                          disarm auto-merge

<pr> is a PR number or URL; without one, the current branch's PR.`,
}

var prOpenCmd = &cobra.Command{
	Use:   "open --title T [--body-file F] [--base main]",
	Short: "Open the current branch as a draft PR and record it on the task bead",
	Long: `Runs 'gh pr create --draft' for the current branch. The body is --body-file
(or a "## Summary" block naming the title), with the trailers
claudemd_default.md prescribes appended when missing:

  Operator: <name> ($BEADS_OPERATED_BY)
  Bead: <task>       (--task, else the agent bead's task_id)
  Agent: $KD_AGENT_ID

Bead: names the task, not the agent bead: the agent bead closes at gb stop,
and the PR sweeper closes a PR whose Bead: is closed. Only
when no task resolves does Bead: fall back to $KD_AGENT_ID.

The PR URL is recorded in mr_url (and forge=github) on the task bead — the
field the merge escalation's guard reads — and printed. Refuses to run on
main or on the --base branch.`,
	Args: cobra.NoArgs,
	RunE: runPROpen,
}

var prReadyCmd = &cobra.Command{
	Use:   "ready [<pr-url-or-number>]",
	Short: "Mark a draft PR ready, arm auto-merge, file its merge escalation, note it on the task bead",
	Long: `Marks a draft PR ready for review, then:

  - arms GitHub auto-merge ('gh pr merge --auto --merge --delete-branch',
    pinned to the PR head) when none of the PR's changed files is on a
    merge-desk (d) path: .github/workflows/**, gardenio/terraform/**,
    deploy-values/**, gardenio/helm/**, any path containing "authz",
    "auth/" or "secret", kbeads/internal/server/interceptors.go. The PR
    then merges itself once the required checks are green. A (d)-path PR
    is not armed: the merge desk reviews and merges it. Arming never uses
    --admin; if it fails, gb pr ready warns and carries on;
  - files the merge escalation via 'gb escalation create --operation-type
    gitlab_mr_merge --close-on-execute', the backstop for a PR that is not
    armed, fails CI or is blocked. It is idempotent per PR, so a second
    'gb pr ready' (after 'gb pr draft') files nothing new. The desk closes
    it as success once the PR has merged;
  - comments on the task bead: "PR #<n> marked ready; <armed or not armed:
    touches <path>>; merge escalation <id>".

Marking ready starts no CI run. A PR runs the light tier only, draft or
not, on every push (owner rule 2026-10-31, gd-19ECZxvFQw); the full suite
runs on main after the merge. The run already on the head is the verdict.

On a PR that is already ready (the merge desk sent it back and you pushed
a fix), it does not run 'gh pr ready' or arm auto-merge: it only files
the merge escalation the same way, which returns the open one if the desk
left it open, and comments "re-filed merge escalation <id> (PR already
ready)" on the task bead. Never toggle the PR
to draft and back to re-file.

Fails if the PR is this branch's and local HEAD is not the PR head (push
first). Auto-merge merges whatever the head is when checks pass, so before
pushing a (d)-path change to an armed PR, run gb pr draft first.

After it, gb stop: the merge wakes you.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPRReady,
}

var prDraftCmd = &cobra.Command{
	Use:   "draft [<pr-url-or-number>]",
	Short: "Turn a ready PR back into a draft (gh pr ready --undo) and disarm auto-merge",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runPRDraft,
}

func init() {
	for _, c := range []*cobra.Command{prOpenCmd, prReadyCmd} {
		c.Flags().String("task", "", "task bead to record the PR on (default: the agent bead's task_id)")
	}
	prOpenCmd.Flags().String("title", "", "PR title (required)")
	prOpenCmd.Flags().String("body-file", "", "file holding the PR body; missing trailers are appended")
	prOpenCmd.Flags().String("base", "main", "branch to merge into")
	prCmd.AddCommand(prOpenCmd, prReadyCmd, prDraftCmd)
}

// prTask resolves the task bead a PR belongs to: --task, else the agent
// bead's task_id. "" when neither resolves.
func prTask(cmd *cobra.Command) string {
	if t, _ := cmd.Flags().GetString("task"); t != "" {
		return t
	}
	if daemon == nil {
		return ""
	}
	return deriveRequestingTask(cmd.Context(), "")
}

func runPROpen(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	title, _ := cmd.Flags().GetString("title")
	bodyFile, _ := cmd.Flags().GetString("body-file")
	base, _ := cmd.Flags().GetString("base")
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("--title is required")
	}

	out, err := prGitRun(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("reading the current branch: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "main" || branch == base || branch == "HEAD" {
		return fmt.Errorf("refusing to open a PR from %q: create a feature branch first (git checkout -b <name>)", branch)
	}

	var body string
	if bodyFile != "" {
		b, err := os.ReadFile(bodyFile)
		if err != nil {
			return fmt.Errorf("reading --body-file: %w", err)
		}
		body = string(b)
	} else {
		body = "## Summary\n- " + title + "\n"
	}
	opID := os.Getenv("BEADS_OPERATED_BY")
	task := prTask(cmd)
	body = withPRTrailers(body, prOperatorLine(cmd, opID), task, prAgentBeadID())

	out, err = prGHRun(ctx, "pr", "create", "--draft", "--title", title, "--body", body, "--base", base)
	if err != nil {
		return err
	}
	prURL := firstPRURL(out)
	if prURL == "" {
		return fmt.Errorf("gh pr create printed no PR URL: %s", strings.TrimSpace(out))
	}

	if task == "" {
		fmt.Fprintf(os.Stderr, "WARNING: no task bead resolved (pass --task); run gb mr link <task> %s\n", prURL)
	} else if err := daemon.UpdateBeadFields(ctx, task, map[string]string{
		"mr_url": prURL,
		"forge":  "github",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not record mr_url on %s: %v\n", task, err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), prURL)
	return nil
}

// prAgentBeadID is the agent bead the Agent: trailer names.
func prAgentBeadID() string {
	if id := os.Getenv("KD_AGENT_ID"); id != "" {
		return id
	}
	return os.Getenv("BOAT_AGENT_BEAD_ID")
}

// prOperatorLine renders "Operator: <name> (<id>)", falling back to the id
// alone when the user bead does not resolve. "" when there is no operator.
func prOperatorLine(cmd *cobra.Command, opID string) string {
	if opID == "" {
		return ""
	}
	name := ""
	if daemon != nil {
		key := strings.TrimPrefix(strings.TrimPrefix(opID, "slack:"), "oidc:")
		if b, err := findUserBead(cmd, key); err == nil && b != nil {
			name = strings.TrimSpace(b.Title)
		}
	}
	if name == "" || name == opID {
		return "Operator: " + opID
	}
	return fmt.Sprintf("Operator: %s (%s)", name, opID)
}

// withPRTrailers appends the Operator:, Bead: and Agent: trailers a body
// lacks. Bead: names the task; Agent: names the agent bead. With no task,
// Bead: falls back to the agent bead and Agent: is left out. A body that
// already has a line starting with the trailer key keeps its own.
func withPRTrailers(body, operatorLine, taskID, agentID string) string {
	has := func(key string) bool {
		for _, l := range strings.Split(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), key) {
				return true
			}
		}
		return false
	}
	var add []string
	if operatorLine != "" && !has("Operator:") {
		add = append(add, operatorLine)
	}
	beadID := taskID
	if beadID == "" {
		beadID = agentID
	}
	if beadID != "" && !has("Bead:") {
		add = append(add, "Bead: "+beadID)
	}
	if agentID != "" && agentID != beadID && !has("Agent:") {
		add = append(add, "Agent: "+agentID)
	}
	if len(add) == 0 {
		return body
	}
	body = strings.TrimRight(body, "\n")
	if body != "" {
		body += "\n\n"
	}
	return body + strings.Join(add, "\n") + "\n"
}

// prView is the slice of `gh pr view --json` gb pr ready reads.
type prView struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	IsDraft     bool   `json:"isDraft"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
}

func viewPR(ctx context.Context, target string) (*prView, error) {
	args := []string{"pr", "view"}
	if target != "" {
		args = append(args, target)
	}
	args = append(args, "--json", "number,url,title,isDraft,headRefName,headRefOid")
	out, err := prGHRun(ctx, args...)
	if err != nil {
		return nil, err
	}
	var v prView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("parsing gh pr view: %w", err)
	}
	if v.Number == 0 || v.URL == "" {
		return nil, fmt.Errorf("gh pr view returned no PR: %s", strings.TrimSpace(out))
	}
	return &v, nil
}

// checkPRHeadPushed fails when pr is the current branch's PR and local HEAD
// is not its head commit. A PR for another branch is not checked: there is
// no local HEAD of it to compare.
func checkPRHeadPushed(ctx context.Context, pr *prView) error {
	out, err := prGitRun(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("reading the current branch: %w", err)
	}
	if strings.TrimSpace(out) != pr.HeadRefName {
		return nil
	}
	out, err = prGitRun(ctx, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("reading HEAD: %w", err)
	}
	if head := strings.TrimSpace(out); head != pr.HeadRefOid {
		return fmt.Errorf("local HEAD %s is not pushed (PR #%d head is %s): git push first, so CI judges what you mean to merge",
			shortSHA(head), pr.Number, shortSHA(pr.HeadRefOid))
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func runPRReady(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	pr, err := viewPR(ctx, target)
	if err != nil {
		return err
	}
	if err := checkPRHeadPushed(ctx, pr); err != nil {
		return err
	}

	// A PR that is already ready (the desk sent it back and the crew pushed a
	// fix) only gets its merge escalation re-filed: re-marking it or re-arming
	// auto-merge would take a draft/ready toggle, which the owner rule
	// of 2026-10-31 forbids (gd-0kbTgvCrFF).
	refile := !pr.IsDraft
	armed := ""
	escContext := fmt.Sprintf("Merge %s once its CI (the light tier on its head) is green. Marked ready by gb pr ready.", pr.URL)
	if refile {
		fmt.Fprintf(cmd.OutOrStdout(), "PR #%d is already ready: not re-marking it or arming auto-merge (no new CI run); re-filing its merge escalation\n", pr.Number)
		escContext = fmt.Sprintf("Merge %s once its CI (the light tier on its head) is green. Re-filed by gb pr ready on the already-ready PR at head %s.", pr.URL, shortSHA(pr.HeadRefOid))
	} else {
		if _, err := prGHRun(ctx, "pr", "ready", pr.URL); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "PR #%d marked ready: %s\n", pr.Number, pr.URL)

		// The escalation below stays the backstop whatever arming does, so a
		// failure here warns and carries on.
		armed, err = armAutoMerge(ctx, pr)
		if err != nil {
			armed = "auto-merge not armed: " + err.Error()
			fmt.Fprintf(os.Stderr, "WARNING: %s; the merge escalation is the backstop\n", armed)
		}
		fmt.Fprintln(cmd.OutOrStdout(), armed)
	}

	task := prTask(cmd)
	escArgs := mergeEscalationArgs(pr, task, escContext)
	out, err := prGBRun(ctx, escArgs...)
	if err != nil {
		return fmt.Errorf("PR #%d is ready, but filing its merge escalation failed (rerun gb escalation create): %w", pr.Number, err)
	}
	escID := escalationIDFromOutput(out)
	if escID == "" {
		return fmt.Errorf("PR #%d is ready, but gb escalation create printed no escalation_id: %s", pr.Number, strings.TrimSpace(out))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "escalation_id=%s\n", escID)
	if repo := prRepoFromURL(pr.URL); repo != "" && pr.HeadRefOid != "" {
		// Crews held Bash calls for up to an hour polling gh run watch/list
		// after this point (gd-PwyvuF0fHp): name the parked wait instead.
		fmt.Fprintf(cmd.OutOrStdout(), "Marking ready starts no CI run; the head's light-tier run is the verdict. To act on it, do not poll gh run watch/list: gb wait ci --repo %s --sha %s, then gb stop (read it on waking with gb wait ci --result).\n", repo, pr.HeadRefOid)
	}

	note := fmt.Sprintf("PR #%d marked ready; %s; merge escalation %s", pr.Number, armed, escID)
	if refile {
		note = fmt.Sprintf("re-filed merge escalation %s (PR already ready)", escID)
	}
	if task == "" {
		fmt.Fprintf(os.Stderr, "WARNING: no task bead resolved (pass --task); not commenting: %s\n", note)
	} else if err := daemon.AddComment(ctx, task, actor, note); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not comment on %s: %v\n", task, err)
	}
	return nil
}

// prDeskPath reports whether path is a merge-desk (d) path: a PR touching one
// is reviewed and merged by the desk (docs/merge-desk.md HIGH-RISK PATHS),
// never auto-merged.
func prDeskPath(path string) bool {
	for _, prefix := range []string{".github/workflows/", "gardenio/terraform/", "deploy-values/", "gardenio/helm/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	lower := strings.ToLower(path)
	for _, sub := range []string{"authz", "auth/", "secret"} {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return path == "kbeads/internal/server/interceptors.go"
}

// armAutoMerge arms GitHub auto-merge on pr when none of its changed files is
// a (d) path, and returns the line gb pr ready prints and comments: "auto-merge
// armed" or "auto-merge not armed: touches <path>". It never passes --admin:
// the fleet identity bypasses the ruleset, so an admin merge skips the checks.
// An error means arming could not be done or decided.
func armAutoMerge(ctx context.Context, pr *prView) (string, error) {
	out, err := prGHRun(ctx, "pr", "view", pr.URL, "--json", "files,changedFiles")
	if err != nil {
		return "", fmt.Errorf("reading the changed files: %w", err)
	}
	var v struct {
		ChangedFiles int `json:"changedFiles"`
		Files        []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("parsing the changed files: %w", err)
	}
	// gh lists at most 100 files; an unlisted file could be a (d) path.
	if len(v.Files) == 0 || len(v.Files) != v.ChangedFiles {
		return "", fmt.Errorf("gh listed %d of %d changed files", len(v.Files), v.ChangedFiles)
	}
	for _, f := range v.Files {
		if prDeskPath(f.Path) {
			return "auto-merge not armed: touches " + f.Path, nil
		}
	}
	if _, err := prGHRun(ctx, "pr", "merge", pr.URL, "--auto", "--merge", "--delete-branch", "--match-head-commit", pr.HeadRefOid); err != nil {
		return "", err
	}
	return "auto-merge armed", nil
}

// mergeEscalationArgs is the `gb escalation create` argv that asks the desk
// to merge pr. With a task it parks that task behind the escalation, closing
// it when the merge executes. gb pr ready and gb stop both file through it.
func mergeEscalationArgs(pr *prView, task, context string) []string {
	args := []string{"escalation", "create",
		"--title", fmt.Sprintf("Merge PR #%d: %s", pr.Number, pr.Title),
		"--operation-type", "gitlab_mr_merge",
		"--operation-spec", fmt.Sprintf(`{"mr_url":%q}`, pr.URL),
		"--context", context,
	}
	if task != "" {
		args = append(args, "--requesting-task", task, "--close-on-execute")
	}
	return args
}

// prRepoFromURL returns OWNER/REPO from a github.com PR URL, or "" when url is
// not one.
func prRepoFromURL(url string) string {
	rest, ok := strings.CutPrefix(url, "https://github.com/")
	if !ok {
		return ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// escalationIDFromOutput reads the escalation_id=<id> line `gb escalation
// create` prints, for a new escalation and for an existing one alike.
func escalationIDFromOutput(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(l), "escalation_id="); ok {
			return id
		}
	}
	return ""
}

func runPRDraft(cmd *cobra.Command, args []string) error {
	ghArgs := []string{"pr", "ready", "--undo"}
	if len(args) > 0 {
		ghArgs = append(ghArgs, args[0])
	}
	ctx := cmd.Context()
	if _, err := prGHRun(ctx, ghArgs...); err != nil {
		return err
	}
	// A draft must not merge itself when its checks go green: disarm the
	// auto-merge gb pr ready armed if GitHub kept it across the undo.
	viewArgs := []string{"pr", "view"}
	if len(args) > 0 {
		viewArgs = append(viewArgs, args[0])
	}
	out, err := prGHRun(ctx, append(viewArgs, "--json", "url,autoMergeRequest")...)
	var v struct {
		URL              string          `json:"url"`
		AutoMergeRequest json.RawMessage `json:"autoMergeRequest"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(out), &v)
	}
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "WARNING: could not check auto-merge (run gh pr merge --disable-auto): %v\n", err)
	case len(v.AutoMergeRequest) > 0 && string(v.AutoMergeRequest) != "null":
		if _, err := prGHRun(ctx, "pr", "merge", v.URL, "--disable-auto"); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: could not disarm auto-merge (run gh pr merge --disable-auto): %v\n", err)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "auto-merge disarmed")
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), "PR is a draft again; run gb pr ready when the rework is pushed")
	return nil
}
