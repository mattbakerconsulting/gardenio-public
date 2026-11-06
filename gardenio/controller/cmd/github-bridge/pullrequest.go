package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/forgebeads"
)

// pullRequestEvent is the pull_request webhook subset the bridge reads.
type pullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		NodeID   string `json:"node_id"`
		Number   int    `json:"number"`
		HTMLURL  string `json:"html_url"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		State    string `json:"state"` // open | closed
		Draft    bool   `json:"draft"`
		Merged   bool   `json:"merged"`
		// MergeCommitSHA is the commit the merge put on the base branch (the
		// merge commit, or the squash or rebase head); MergedAt is when.
		MergeCommitSHA string `json:"merge_commit_sha"`
		MergedAt       string `json:"merged_at"`
		MergedBy *struct {
			Login string `json:"login"`
		} `json:"merged_by"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// PR states as the mr_state field spells them. They are GitLab's words
// (opened, not open) because the field is an enum shared by both forges.
const (
	mrOpened = "opened"
	mrClosed = "closed"
	mrMerged = "merged"
)

// prActions are the pull_request actions that can change what the bead fields
// say. Everything else (labeled, assigned, review_requested, locked, ...) is
// acknowledged and ignored.
var prActions = map[string]bool{
	"opened": true, "reopened": true, "synchronize": true, "closed": true,
	"edited": true, "ready_for_review": true, "converted_to_draft": true,
}

// prState maps a payload to the mr_state vocabulary. merged wins over state:
// a merged PR reports state=closed with merged=true.
func (e *pullRequestEvent) prState() string {
	switch {
	case e.PullRequest.Merged:
		return mrMerged
	case e.PullRequest.State == "closed":
		return mrClosed
	default:
		return mrOpened
	}
}

// Bead locating, in order of authority: a task bead that already carries this
// PR's URL (set by `gb mr link` or an earlier event), the bead id in the
// agent's branch name (agent/<role>/<bead-id>, the launch convention), then bead
// ids the PR body mentions.
var (
	branchBeadRe = regexp.MustCompile(`^agent/[^/]+/([a-z]{2}-[A-Za-z0-9]{6,12})$`)
	// A bead id in prose: two-letter prefix, an 8-12 char suffix with at least
	// one digit (checked in code), so "re-evaluating" never gets probed.
	bodyBeadRe = regexp.MustCompile(`\b[a-z]{2}-[A-Za-z0-9]{8,12}\b`)
)

// maxBodyCandidates caps how many ids scraped from a PR body are probed.
const maxBodyCandidates = 5

// workBeadTypes are the bead types that can own a PR.
var workBeadTypes = map[string]bool{"task": true, "bug": true, "feature": true, "epic": true, "chore": true}

func beadIDsInBody(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range bodyBeadRe.FindAllString(body, -1) {
		if seen[m] || !strings.ContainsAny(m, "0123456789") {
			continue
		}
		seen[m] = true
		out = append(out, m)
		if len(out) == maxBodyCandidates {
			break
		}
	}
	return out
}

// prHandler turns pull_request deliveries into bead updates, a thread status
// line and a mail to the agent.
type prHandler struct {
	daemon   beadClient
	notifier *beadNotifier
	logger   *slog.Logger
	// files lists a merged PR's paths for the harvest counts. Nil (no GitHub
	// App configured) records ci_state only.
	files prFilesLister
	// drafter converts an agent PR opened as ready back to a draft. Nil
	// (GITHUB_BRIDGE_DRAFT_AGENT_PRS off, or no GitHub App) leaves PRs alone.
	drafter prDrafter
}

// prFilesLister lists the paths a pull request touches.
type prFilesLister interface {
	PRFiles(ctx context.Context, pr prRef) ([]string, error)
}

func newPRHandler(daemon beadClient, logger *slog.Logger) *prHandler {
	return &prHandler{daemon: daemon, notifier: &beadNotifier{daemon: daemon, logger: logger}, logger: logger}
}

// locate returns the work beads a PR belongs to, deduplicated, in the order
// found.
func (h *prHandler) locate(ctx context.Context, ev *pullRequestEvent) ([]*beadsapi.BeadDetail, error) {
	pr := &ev.PullRequest
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	add := func(b *beadsapi.BeadDetail) {
		if b != nil && !seen[b.ID] && workBeadTypes[b.Type] {
			seen[b.ID] = true
			out = append(out, b)
		}
	}

	linked, err := h.daemon.ListTaskBeadsForMR(ctx, pr.HTMLURL)
	if err != nil {
		return nil, err
	}
	for _, b := range linked {
		add(b)
	}

	var candidates []string
	if m := branchBeadRe.FindStringSubmatch(pr.Head.Ref); m != nil {
		candidates = append(candidates, m[1])
	}
	candidates = append(candidates, beadIDsInBody(pr.Body)...)
	for _, id := range candidates {
		if seen[id] {
			continue
		}
		b, err := forgebeads.GetBeadIfExists(ctx, h.daemon, id)
		if err != nil {
			return nil, err
		}
		add(b)
	}
	return out, nil
}

// Handle consumes pull_request events and ignores the rest (it sits in a
// fanoutSink). An error makes the
// receiver answer 5xx so GitHub redelivers; every write below is idempotent
// (upserts, set-fields, idempotency-keyed notices), so a retry is safe.
func (h *prHandler) Handle(ctx context.Context, ev Event) error {
	if ev.Type != "pull_request" {
		return nil
	}
	var pe pullRequestEvent
	if err := json.Unmarshal(ev.Payload, &pe); err != nil {
		// Signed by GitHub but undecodable: retrying cannot help.
		h.logger.Warn("pull_request: undecodable payload", "delivery", ev.DeliveryID, "error", err)
		return nil
	}
	pr := &pe.PullRequest
	if !prActions[pe.Action] || pr.HTMLURL == "" {
		return nil
	}
	// Before the bead lookup: an agent branch is the signal, linked or not.
	h.draftAgentPR(ctx, &pe)

	beads, err := h.locate(ctx, &pe)
	if err != nil {
		return fmt.Errorf("locate beads for %s: %w", pr.HTMLURL, err)
	}
	if len(beads) == 0 {
		// Not one of ours (or not linked yet). Writing an MR bead for every PR
		// in every repo the App can see would flood the graph; a later event
		// finds the bead once `gb mr link` has set mr_url on it.
		h.logger.Info("pull_request: no matching bead, ignoring",
			"delivery", ev.DeliveryID, "repo", pe.Repository.FullName, "pr", pr.Number, "action", pe.Action)
		return nil
	}

	state := pe.prState()
	closed := state != mrOpened
	fields := beadsapi.MergeRequestBeadFields{
		Forge:        forgeGitHub,
		MRURL:        pr.HTMLURL,
		MRIID:        strconv.Itoa(pr.Number),
		ProjectPath:  pe.Repository.FullName,
		SourceBranch: pr.Head.Ref,
		TargetBranch: pr.Base.Ref,
		MRState:      state,
		Title:        pr.Title,
		Author:       pr.User.Login,
	}
	if closed {
		fields.MROutcome = state
	}
	mrBeadID, err := h.daemon.UpsertMergeRequestBead(ctx, fields, closed)
	if err != nil {
		return fmt.Errorf("upsert MR bead for %s: %w", pr.HTMLURL, err)
	}

	var harvest map[string]string
	if state == mrMerged {
		h.stampMerge(ctx, &pe, mrBeadID)
		harvest = h.harvestFields(ctx, &pe, mrBeadID)
	}
	for _, b := range beads {
		if err := h.linkTask(ctx, b, mrBeadID, pr.HTMLURL); err != nil {
			return err
		}
		if len(harvest) > 0 {
			if err := h.daemon.UpdateBeadFields(ctx, b.ID, harvest); err != nil {
				return fmt.Errorf("set harvest fields on %s: %w", b.ID, err)
			}
		}
		h.announce(ctx, ev, &pe, b)
	}
	return nil
}

// draftCommentMarker makes the draft notice idempotent: a redelivery of the
// opened event finds it and posts nothing more.
const draftCommentMarker = "<!-- gardenio:draft-agent-pr -->"

const draftComment = "Opened as ready from an agent branch, so moved to draft: drafts run only build + unit tests. " +
	"Run `gb pr ready` when the work is done.\n\n" + draftCommentMarker

// prDrafter converts a PR to a draft and comments on it (a seam for tests).
type prDrafter interface {
	ConvertToDraft(ctx context.Context, repo, nodeID string) error
	prCommenter
}

// isAgentPROpenedReady reports whether an event is a crew PR opened as ready
// onto main: the case the draft CI tier (plan gd-WFMvg9sDKE) wants as a draft.
func isAgentPROpenedReady(pe *pullRequestEvent) bool {
	pr := &pe.PullRequest
	return pe.Action == "opened" && !pr.Draft && pr.Base.Ref == "main" && strings.HasPrefix(pr.Head.Ref, "agent/")
}

// draftAgentPR converts a crew PR opened as ready back to a draft and says why
// on the PR. It is a backstop that does not depend on any prompt. Every
// failure is logged and swallowed: it never blocks the bead mapping, and a
// redelivery (after a bead error) converts again harmlessly and finds the
// marker instead of commenting twice.
func (h *prHandler) draftAgentPR(ctx context.Context, pe *pullRequestEvent) {
	if h.drafter == nil || !isAgentPROpenedReady(pe) {
		return
	}
	pr := &pe.PullRequest
	ref := prRef{Repo: pe.Repository.FullName, Number: pr.Number}
	log := h.logger.With("repo", ref.Repo, "pr", ref.Number, "branch", pr.Head.Ref)
	if pr.NodeID == "" || ref.Repo == "" {
		log.Warn("pull_request: agent PR opened as ready, but the payload has no node id or repo; left ready")
		return
	}
	if err := h.drafter.ConvertToDraft(ctx, ref.Repo, pr.NodeID); err != nil {
		log.Warn("pull_request: converting agent PR to draft failed", "error", err)
		return
	}
	log.Info("pull_request: converted agent PR opened as ready to draft")
	seen, err := h.drafter.HasMarker(ctx, ref, draftCommentMarker)
	if err != nil {
		log.Warn("pull_request: draft notice lookup failed, not commenting", "error", err)
		return
	}
	if seen {
		return
	}
	if err := h.drafter.Post(ctx, ref, draftComment); err != nil {
		log.Warn("pull_request: draft notice comment failed", "error", err)
		return
	}
	log.Info("pull_request: posted draft notice comment")
}

// convertToDraftMutation is GitHub's ready -> draft switch. It needs the
// installation token's pull_requests: write.
const convertToDraftMutation = `mutation($id:ID!){
  convertPullRequestToDraft(input:{pullRequestId:$id}){pullRequest{isDraft}}
}`

// githubDrafter converts over GraphQL and comments over REST. The two bases
// differ on GitHub Enterprise Server (/api/graphql vs /api/v3), so each half
// keeps its own.
type githubDrafter struct {
	*githubCommenter
	gql *graphQLThreads
}

func newGitHubDrafter(tokens *tokenSource, apiURL string) *githubDrafter {
	return &githubDrafter{githubCommenter: newGitHubCommenter(tokens, apiURL), gql: newGraphQLThreads(tokens, apiURL)}
}

func (d *githubDrafter) ConvertToDraft(ctx context.Context, repo, nodeID string) error {
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			ConvertPullRequestToDraft *struct {
				PullRequest struct {
					IsDraft bool `json:"isDraft"`
				} `json:"pullRequest"`
			} `json:"convertPullRequestToDraft"`
		} `json:"data"`
	}
	body := map[string]any{"query": convertToDraftMutation, "variables": map[string]any{"id": nodeID}}
	if err := d.gql.do(ctx, repo, http.MethodPost, "/graphql", body, http.StatusOK, &resp); err != nil {
		return err
	}
	// GraphQL reports failures with a 200 and an errors array.
	if len(resp.Errors) > 0 {
		return fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}
	if c := resp.Data.ConvertPullRequestToDraft; c == nil || !c.PullRequest.IsDraft {
		return errors.New("graphql: pull request is still not a draft")
	}
	return nil
}

var _ prDrafter = (*githubDrafter)(nil)

// stampMerge records merge_sha and merged_at on the MR bead, so a release's
// contents join a first-parent commit to its PR on sha (releases-design.md
// §3.4). It is its own write, after the upsert, and a failure is logged and
// swallowed: a daemon that does not know the fields yet must not fail the
// delivery and lose the rest of the merge.
func (h *prHandler) stampMerge(ctx context.Context, pe *pullRequestEvent, mrBeadID string) {
	fields := map[string]string{}
	if sha := strings.TrimSpace(pe.PullRequest.MergeCommitSHA); sha != "" {
		fields["merge_sha"] = sha
	}
	if at := strings.TrimSpace(pe.PullRequest.MergedAt); at != "" {
		fields["merged_at"] = at
	}
	if len(fields) == 0 {
		return
	}
	if err := h.daemon.UpdateBeadFields(ctx, mrBeadID, fields); err != nil {
		h.logger.Warn("pull_request: merge sha not recorded on MR bead", "mr_bead", mrBeadID, "error", err)
	}
}

// harvestFields are the quality signals a merge records on each producing task
// for the Lands harvest cell (spec 2026-10-22-lands-harvest-icons-design.md
// §2.3): ci_state is the MR bead's mr_pipeline_status at merge, which the
// pipeline handler keeps current; tests_touched and goldens_touched are counted
// over the PR's files, "0" included, so "no tests" reads apart from "not
// recorded". A failed read leaves its fields out and never fails the delivery.
func (h *prHandler) harvestFields(ctx context.Context, pe *pullRequestEvent, mrBeadID string) map[string]string {
	out := map[string]string{}
	mr, err := h.daemon.GetBead(ctx, mrBeadID)
	switch {
	case err != nil:
		h.logger.Warn("pull_request: MR bead read failed, ci_state not recorded", "mr_bead", mrBeadID, "error", err)
	case mr != nil && mr.Fields["mr_pipeline_status"] != "":
		out["ci_state"] = mr.Fields["mr_pipeline_status"]
	}
	if h.files == nil {
		return out
	}
	pr := prRef{Repo: pe.Repository.FullName, Number: pe.PullRequest.Number}
	paths, err := h.files.PRFiles(ctx, pr)
	if err != nil {
		h.logger.Warn("pull_request: files read failed, test and golden counts not recorded",
			"repo", pr.Repo, "pr", pr.Number, "error", err)
		return out
	}
	var tests, goldens int
	for _, p := range paths {
		test, golden := classifyPRFile(p)
		if test {
			tests++
		}
		if golden {
			goldens++
		}
	}
	out["tests_touched"] = strconv.Itoa(tests)
	out["goldens_touched"] = strconv.Itoa(goldens)
	return out
}

// classifyPRFile says whether a changed path is a test and whether it is a
// golden. A test is *_test.go, *_test.rs, *.test.js, *.spec.ts, a
// testdata/*_browser.js harness, or anything under a tests/ directory except a
// golden*/ directory below it. A golden is anything under golden/ or goldens/.
func classifyPRFile(path string) (test, golden bool) {
	parts := strings.Split(path, "/")
	dirs, base := parts[:len(parts)-1], parts[len(parts)-1]
	golden = slices.Contains(dirs, "golden") || slices.Contains(dirs, "goldens")
	for _, suffix := range []string{"_test.go", "_test.rs", ".test.js", ".spec.ts"} {
		if strings.HasSuffix(base, suffix) {
			return true, golden
		}
	}
	if len(dirs) > 0 && dirs[len(dirs)-1] == "testdata" && strings.HasSuffix(base, "_browser.js") {
		return true, golden
	}
	i := slices.Index(dirs, "tests")
	if i < 0 {
		return false, golden
	}
	for _, d := range dirs[i+1:] {
		if strings.HasPrefix(d, "golden") {
			return false, golden
		}
	}
	return true, golden
}

const (
	prFilesPerPage = 100
	// maxPRFilePages covers the 3,000 files GitHub lists at most for a PR.
	maxPRFilePages = 30
)

// PRFiles lists the paths a pull request changes (GET pulls/{n}/files, paged).
func (c *githubCommenter) PRFiles(ctx context.Context, pr prRef) ([]string, error) {
	var out []string
	for page := 1; page <= maxPRFilePages; page++ {
		var files []struct {
			Filename string `json:"filename"`
		}
		path := fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=%d&page=%d", pr.Repo, pr.Number, prFilesPerPage, page)
		if err := c.do(ctx, pr.Repo, http.MethodGet, path, nil, http.StatusOK, &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			out = append(out, f.Filename)
		}
		if len(files) < prFilesPerPage {
			break
		}
	}
	return out, nil
}

// linkTask makes the task bead point at the PR: produces/triggered-by edges
// with the MR bead, and mr_url + forge on the task bead itself when it carries
// no mr_url yet (never overwriting a link `gb mr link` already made).
func (h *prHandler) linkTask(ctx context.Context, task *beadsapi.BeadDetail, mrBeadID, mrURL string) error {
	if task.Fields["mr_url"] == "" {
		if err := h.daemon.UpdateBeadFields(ctx, task.ID, map[string]string{"mr_url": mrURL, "forge": forgeGitHub}); err != nil {
			return fmt.Errorf("set mr_url on %s: %w", task.ID, err)
		}
	}
	// Dep edges are best-effort: the daemon rejects a duplicate edge, which is
	// the steady state on every event after the first. The edge pair is shared
	// with gitlab-bridge's sync paths (forgebeads).
	if err := forgebeads.LinkTaskToMR(ctx, h.daemon, task.ID, mrBeadID, "system:github-bridge"); err != nil {
		h.logger.Debug("task-MR dep", "task", task.ID, "mr_bead", mrBeadID, "error", err)
	}
	return nil
}

// announce posts the thread status line and mails the agent, for the actions
// worth interrupting a human's scroll: not every synchronize push.
func (h *prHandler) announce(ctx context.Context, ev Event, pe *pullRequestEvent, task *beadsapi.BeadDetail) {
	line := statusLine(pe)
	if line == "" {
		return
	}
	ag, err := findAgentForTask(ctx, h.daemon, task)
	if err != nil {
		h.logger.Warn("pull_request: agent lookup failed", "task", task.ID, "error", err)
		return
	}
	key := "github-pr:" + ev.DeliveryID + ":" + task.ID
	h.notifier.postThread(ctx, ag, key, line)

	if pe.Action == "closed" || pe.Action == "reopened" {
		h.notifier.mailAgent(ctx, ag, key+":mail",
			fmt.Sprintf("PR #%d %s: %s", pe.PullRequest.Number, verb(pe), pe.PullRequest.Title),
			fmt.Sprintf("%s\n%s", pe.PullRequest.HTMLURL, plainDetail(pe)))
	}
}

// verb is the past-tense word for what happened to the PR.
func verb(pe *pullRequestEvent) string {
	switch {
	case pe.Action == "closed" && pe.PullRequest.Merged:
		return "merged"
	case pe.Action == "closed":
		return "closed without merging"
	case pe.Action == "reopened":
		return "reopened"
	default:
		return pe.Action
	}
}

func plainDetail(pe *pullRequestEvent) string {
	pr := &pe.PullRequest
	if pr.Merged && pr.MergedBy != nil && pr.MergedBy.Login != "" {
		return fmt.Sprintf("Merged by %s into %s.", pr.MergedBy.Login, pr.Base.Ref)
	}
	return fmt.Sprintf("%s → %s, by %s.", pr.Head.Ref, pr.Base.Ref, pr.User.Login)
}

// statusLine is the one line the agent's Slack thread gets for an event, or ""
// for actions that only update bead fields (synchronize and edited fire on
// every push and title tweak; the pipeline lines come from check events).
func statusLine(pe *pullRequestEvent) string {
	pr := &pe.PullRequest
	link := fmt.Sprintf("<%s|#%d %s>", pr.HTMLURL, pr.Number, slackText(pr.Title))
	branches := fmt.Sprintf("`%s` → `%s`", slackText(pr.Head.Ref), slackText(pr.Base.Ref))
	switch pe.Action {
	case "opened":
		if pr.Draft {
			return fmt.Sprintf(":memo: Draft PR opened: %s · %s", link, branches)
		}
		return fmt.Sprintf(":rocket: PR opened: %s · %s", link, branches)
	case "reopened":
		return fmt.Sprintf(":repeat: PR reopened: %s", link)
	case "ready_for_review":
		return fmt.Sprintf(":eyes: PR ready for review: %s", link)
	case "converted_to_draft":
		return fmt.Sprintf(":memo: PR moved back to draft: %s", link)
	case "closed":
		if pr.Merged {
			by := ""
			if pr.MergedBy != nil && pr.MergedBy.Login != "" {
				by = " by " + slackText(pr.MergedBy.Login)
			}
			return fmt.Sprintf(":white_check_mark: PR merged%s: %s", by, link)
		}
		return fmt.Sprintf(":no_entry_sign: PR closed without merging: %s", link)
	}
	return ""
}
