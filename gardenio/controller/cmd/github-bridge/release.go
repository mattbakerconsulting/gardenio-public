package main

// Release recording (plan gd-UZKZ9H25UD step 3b, releases-design.md §4.2): a
// tag pushed to a project's repo becomes that project's release record, for
// any project with a release_tag_pattern and a GitHub git_url. Two ways in,
// both running releaserecord.Recorder, whose write is idempotent on (project,
// tag):
//
//   - the webhook: `create` with ref_type=tag, or `push` of refs/tags/*;
//   - a tag poll every 5 minutes (the owner's choice for v1, §6 Q1): a tenant
//     repo sends no webhook, so the poll lists each project's tags and records
//     the ones above its newest recorded release.
//
// A new tag is cut onto the project's open release when it has one, and the
// next release is opened after the write (plan gd-lgN4U7X5Tb); a row the open
// release carried that git does not show is dropped and logged. A PR merged on
// the release branch boards the open release as it lands (board).
//
// The same recorder turns a successful `deployment_status` into a deployment
// row on the release (deployment.go, step 4b).
//
// The daemon refuses a release write on the shared static token
// (releaseWriterDenial), so the release writes go through a second client on
// the bridge's machine principal (BEADS_OIDC_*, realm role
// gardenio-machine-release-recorder). Without it the recorder is off.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/releaserecord"
)

const (
	// defaultReleasePollInterval is how often the poll lists each project's
	// tags (RELEASE_POLL_INTERVAL).
	defaultReleasePollInterval = 5 * time.Minute

	// releaseRecorderRole is the realm role the machine principal must hold.
	releaseRecorderRole = "gardenio-machine-release-recorder"

	// recordedReleasesRead is how many of a project's newest releases the
	// poll reads to find the newest recorded tag.
	recordedReleasesRead = 20
)

// releaseProjects is what the recorder reads beside the Recorder's own calls.
type releaseProjects interface {
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
	ListReleases(ctx context.Context, project, status string, limit int) ([]beadsapi.Release, error)
}

var _ releaseProjects = (*beadsapi.Client)(nil)

// releaseDaemon is the Recorder's daemon: reads and plan comments on the
// bridge's own client, the release create, cut and fill on the machine
// principal.
type releaseDaemon struct {
	releaserecord.Daemon
	writer interface {
		CreateRelease(ctx context.Context, req beadsapi.CreateReleaseRequest) (*beadsapi.Release, error)
		UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
		releaserecord.Appender
	}
}

func (d releaseDaemon) CreateRelease(ctx context.Context, req beadsapi.CreateReleaseRequest) (*beadsapi.Release, error) {
	return d.writer.CreateRelease(ctx, req)
}

// UpdateBeadFieldsTyped is only ever a release cut or fill in the Recorder.
func (d releaseDaemon) UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error {
	return d.writer.UpdateBeadFieldsTyped(ctx, beadID, fields)
}

func (d releaseDaemon) AppendReleaseContents(ctx context.Context, ref string, rows any) (*beadsapi.Release, error) {
	return d.writer.AppendReleaseContents(ctx, ref, rows)
}

// releaseRecorder is the webhook sink and the poll.
type releaseRecorder struct {
	daemon   releaseDaemon
	projects releaseProjects
	gh       releaserecord.GitHubGetter
	interval time.Duration
	logger   *slog.Logger

	// deploys writes the deployment rows of deployment_status (deployment.go)
	// on the same machine principal; nil leaves that event to the log sink.
	deploys releaseDeployer

	// mu runs one recording at a time: a tag push delivers both `create` and
	// `push`, and the poll may find the same tag, and the plan comment is
	// checked-then-written.
	mu sync.Mutex
}

func newReleaseRecorder(daemon releaseDaemon, projects releaseProjects, gh releaserecord.GitHubGetter, interval time.Duration, logger *slog.Logger) *releaseRecorder {
	if interval <= 0 {
		interval = defaultReleasePollInterval
	}
	return &releaseRecorder{daemon: daemon, projects: projects, gh: gh, interval: interval, logger: logger}
}

// tagOfEvent is the tag a `create` (ref_type=tag) or `push` (refs/tags/*,
// not a delete) names; ok is false for every other event.
func tagOfEvent(ev Event) (string, bool) {
	var p struct {
		Ref     string `json:"ref"`
		RefType string `json:"ref_type"`
		Deleted bool   `json:"deleted"`
	}
	switch ev.Type {
	case "create", "push":
	default:
		return "", false
	}
	if json.Unmarshal(ev.Payload, &p) != nil {
		return "", false
	}
	if ev.Type == "create" {
		return p.Ref, p.RefType == "tag" && p.Ref != ""
	}
	tag, ok := strings.CutPrefix(p.Ref, "refs/tags/")
	return tag, ok && tag != "" && !p.Deleted
}

// githubRepoOf is a project's GitHub owner/repo from its git_url, "" when the
// git_url is not on github.com. A project's release repo is its git_url: the
// Recorder builds the contents' PR links from it.
func githubRepoOf(info beadsapi.ProjectInfo) string {
	web := releaserecord.RepoWebURL(info.GitURL)
	u, err := url.Parse(web)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return ""
	}
	return releaserecord.RepoFullName(web)
}

// releaseProject is one project whose releases the recorder writes.
type releaseProject struct {
	name    string
	repo    string
	pattern *regexp.Regexp
	branch  string // the release branch merged PRs board from
}

// releaseProjectsList lists the projects with a release_tag_pattern and a
// GitHub git_url, by name. A pattern that does not compile is skipped (the
// daemon refuses one on write, so it is a hand edit).
func (h *releaseRecorder) releaseProjectsList(ctx context.Context) ([]releaseProject, error) {
	all, err := h.projects.ListProjectBeads(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	var out []releaseProject
	for name, info := range all {
		if info.ReleaseSource == nil || info.ReleaseSource.TagPattern == "" {
			continue
		}
		repo := githubRepoOf(info)
		if repo == "" {
			continue
		}
		re, err := regexp.Compile(info.ReleaseSource.TagPattern)
		if err != nil {
			h.logger.Warn("release: bad release_tag_pattern", "project", name, "error", err)
			continue
		}
		out = append(out, releaseProject{name: name, repo: repo, pattern: re, branch: info.ReleaseSource.Branch})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// Handle records the release a tag webhook names, for each project whose repo
// it is. A tag that does not match the project's pattern is ignored. A
// deployment_status is a deployment row (deployment.go); a merged
// pull_request boards the open release (board).
func (h *releaseRecorder) Handle(ctx context.Context, ev Event) error {
	switch ev.Type {
	case "deployment_status":
		return h.handleDeploymentStatus(ctx, ev)
	case "pull_request":
		return h.board(ctx, ev)
	}
	tag, ok := tagOfEvent(ev)
	if !ok || ev.Repo == "" {
		return nil
	}
	projects, err := h.releaseProjectsList(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if !strings.EqualFold(p.repo, ev.Repo) {
			continue
		}
		if !p.pattern.MatchString(tag) {
			h.logger.Debug("release: tag does not match the release_tag_pattern",
				"project", p.name, "tag", tag, "pattern", p.pattern.String())
			continue
		}
		if err := h.record(ctx, p, tag); err != nil {
			return fmt.Errorf("recording %s %s: %w", p.name, tag, err)
		}
	}
	return nil
}

// board puts a PR merged on a project's release branch on the project's open
// release (releaserecord.Board), joined as the cut joins it. The release sink
// runs after the PR sink, so the merge's MR bead, merge sha and task edge are
// written before the join reads them. A project with no open release boards
// nothing: its next cut carries the PR.
func (h *releaseRecorder) board(ctx context.Context, ev Event) error {
	var pe pullRequestEvent
	if json.Unmarshal(ev.Payload, &pe) != nil || pe.Action != "closed" || !pe.PullRequest.Merged || ev.Repo == "" {
		return nil
	}
	pr, sha := pe.PullRequest.Number, strings.TrimSpace(pe.PullRequest.MergeCommitSHA)
	if sha == "" {
		return nil
	}
	projects, err := h.releaseProjectsList(ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range projects {
		if !strings.EqualFold(p.repo, ev.Repo) || p.branch == "" || p.branch != pe.PullRequest.Base.Ref {
			continue
		}
		b, err := h.recorderFor(p).Board(ctx, h.daemon, p.name, sha)
		if err != nil {
			return fmt.Errorf("boarding #%d on %s's open release: %w", pr, p.name, err)
		}
		if b.Release == nil {
			h.logger.Info("release: no open release to board; the next cut carries the PR", "project", p.name, "pr", pr, "sha", sha)
			continue
		}
		h.logger.Info("release: merged PR boarded the open release", "project", p.name, "pr", pr, "sha", sha,
			"release", b.Release.ID, "rows", len(b.Rows), "plan", b.Rows[0].Plan, "task", b.Rows[0].Task)
	}
	return nil
}

// recorderFor is a fresh Recorder on p's repo; it caches beads per run, so
// one is built per recording.
func (h *releaseRecorder) recorderFor(p releaseProject) *releaserecord.Recorder {
	gh := releaserecord.GitHub{API: h.gh, Repo: p.repo}
	return &releaserecord.Recorder{Daemon: h.daemon, Source: gh, PRs: gh, CommentPlans: true, Author: bridgeActor, CutOpen: true}
}

// record writes the release of tag. One already recorded with its contents
// is left alone without reading GitHub, so the second delivery of a tag (or
// the poll after the webhook) writes nothing.
func (h *releaseRecorder) record(ctx context.Context, p releaseProject, tag string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rel, err := h.daemon.GetRelease(ctx, tag, p.name); err == nil && hasContents(rel) {
		h.logger.Debug("release: already recorded", "project", p.name, "tag", tag, "release", rel.ID)
		return nil
	}
	return h.write(ctx, h.recorderFor(p), p, tag)
}

func (h *releaseRecorder) write(ctx context.Context, r *releaserecord.Recorder, p releaseProject, tag string) error {
	rec, err := r.Plan(ctx, p.name, tag, releaserecord.SourceWebhook)
	if err != nil {
		return err
	}
	if len(rec.Dropped) > 0 {
		h.logger.Warn("release: the cut drops rows git does not show",
			"project", p.name, "tag", tag, "release", rec.Open.ID, "previous", rec.Previous,
			"dropped", releaserecord.DroppedSummary(rec.Dropped))
	}
	res, err := r.Write(ctx, rec)
	if err != nil {
		return err
	}
	next := ""
	if res.Next != nil {
		next = res.Next.ID
	}
	h.logger.Info("release recorded",
		"project", p.name, "tag", tag, "release", res.Release.ID, "created", res.Created,
		"cut_open", res.CutOpen, "previous", rec.Previous, "rows", len(rec.Rows), "complete", rec.Complete,
		"filled", res.Filled, "plans_commented", res.Commented, "next", next, "next_opened", res.NextOpened)
	return nil
}

func hasContents(rel *beadsapi.Release) bool {
	c := strings.TrimSpace(string(rel.Contents))
	return rel.SHA != "" && c != "" && c != "null" && c != "[]"
}

// Run polls until ctx is done, starting with one pass now.
func (h *releaseRecorder) Run(ctx context.Context) {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		h.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// poll records, for each release-enabled project, the tags above its newest
// recorded release. One project's failure is logged and the next one runs.
func (h *releaseRecorder) poll(ctx context.Context) {
	projects, err := h.releaseProjectsList(ctx)
	if err != nil {
		h.logger.Warn("release poll: listing projects failed", "error", err)
		return
	}
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		if err := h.pollProject(ctx, p); err != nil {
			h.logger.Warn("release poll: project failed", "project", p.name, "repo", p.repo, "error", err)
		}
	}
}

func (h *releaseRecorder) pollProject(ctx context.Context, p releaseProject) error {
	recorded, err := h.projects.ListReleases(ctx, p.name, "", recordedReleasesRead)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(recorded))
	for _, rel := range recorded {
		names = append(names, rel.Tag)
	}
	tags, err := h.recorderFor(p).NewTags(ctx, p.name, names)
	if err != nil {
		return err
	}
	for _, t := range tags {
		if err := h.record(ctx, p, t.Name); err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}
	}
	return nil
}

// Get makes githubCommenter a releaserecord.GitHubGetter: path is
// "repos/<owner>/<repo>/...", and the token is that repo's installation token.
func (c *githubCommenter) Get(ctx context.Context, path string, out any) error {
	parts := strings.SplitN(path, "/", 4)
	if len(parts) < 3 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("github get: %q is not a repos/<owner>/<repo> path", path)
	}
	repo := parts[1] + "/" + strings.SplitN(parts[2], "?", 2)[0]
	return c.do(ctx, repo, http.MethodGet, "/"+path, nil, http.StatusOK, out)
}

var _ releaserecord.GitHubGetter = (*githubCommenter)(nil)
