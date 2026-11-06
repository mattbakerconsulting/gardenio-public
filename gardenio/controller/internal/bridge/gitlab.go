// Package bridge provides the GitLab REST API v4 HTTP client.
//
// GitLabClient wraps GitLab REST API methods needed by the gitlab-bridge:
// merge request retrieval, merged MR polling, and pipeline status queries.
// It uses PRIVATE-TOKEN auth (Group Access Token) and returns typed Go structs.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/httpjson"
)

// GitLabClient is an HTTP client for the GitLab REST API v4.
type GitLabClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
	logger     *slog.Logger
}

// gitLabAPIError is a doJSON error that carries the HTTP status code, so
// callers can distinguish a semantic 404 ("branch is gone", a positive signal)
// from a transport/auth/5xx failure ("could not verify"). Its Error() preserves
// the exact message doJSON previously returned, so existing string-matching
// callers and logs are unchanged.
type gitLabAPIError struct {
	status int
	msg    string
}

func (e *gitLabAPIError) Error() string { return e.msg }

// isGitLabNotFound reports whether err is a GitLab API error with a 404 status.
func isGitLabNotFound(err error) bool {
	var apiErr *gitLabAPIError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound
}

// GitLabClientConfig holds configuration for creating a GitLabClient.
type GitLabClientConfig struct {
	BaseURL string // e.g., "https://gitlab.com"
	Token   string // Group Access Token with read_api scope
	Logger  *slog.Logger
}

// NewGitLabClient creates a new GitLab REST API client.
func NewGitLabClient(cfg GitLabClientConfig) *GitLabClient {
	return &GitLabClient{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		token:      cfg.Token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     cfg.Logger,
	}
}

// Host returns the bare host (no scheme, no trailing slash) of the GitLab
// instance this client is pointed at, e.g. "gitlab.com". It is derived from
// the configured base URL and is used by GitLabSync to scope SSE-driven MR
// handling to MR URLs on this client's host — the load-bearing invariant for
// running one bridge per GitLab instance against a shared beads daemon (see
// the R2 artifact §7 "Dual-GitLab structural note"). Returns "" when the base
// URL is empty or unparseable, which callers treat as "match all hosts" to
// preserve single-bridge behavior.
func (c *GitLabClient) Host() string {
	if c == nil || c.baseURL == "" {
		return ""
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// GitLabMR represents a GitLab merge request from the REST API.
type GitLabMR struct {
	ID           int         `json:"id"`
	IID          int         `json:"iid"`
	Title        string      `json:"title"`
	State        string      `json:"state"` // opened, closed, merged, locked
	CreatedAt    string      `json:"created_at"`
	MergedAt     *string     `json:"merged_at"`
	WebURL       string      `json:"web_url"`
	SourceBranch string      `json:"source_branch"`
	TargetBranch string      `json:"target_branch"`
	ProjectID    int         `json:"project_id"`
	Author       *GitLabUser `json:"author"`
	MergedBy     *GitLabUser `json:"merged_by"`
	// Assignee / Assignees are who currently OWNS the MR, as distinct from who
	// opened it. GitLab returns both the legacy single `assignee` and the
	// multi-assignee `assignees` array; read both (see AssigneeUsernames).
	// Ownership decisions must not key on Author alone — an agent-opened MR a
	// human has adopted keeps author=gardeniobot forever (bug ka-0000004809).
	Assignee     *GitLabUser   `json:"assignee"`
	Assignees    []GitLabUser  `json:"assignees"`
	HeadPipeline *GitLabPipRef `json:"head_pipeline"`
	SHA          string        `json:"sha"`
	UpdatedAt    string        `json:"updated_at"`
	Description  string        `json:"description"`
	Labels       []string      `json:"labels"`
	Draft        bool          `json:"draft"`
	HasConflicts bool          `json:"has_conflicts"`
	MergeStatus  string        `json:"merge_status"` // can_be_merged, cannot_be_merged, etc.
	// DetailedMergeStatus is the only field that reports WHY an MR is not
	// mergeable. `merge_status` collapses every reason into can_be_merged /
	// cannot_be_merged and reads can_be_merged on an MR that is merely waiting
	// for an approval, so it cannot distinguish "ready" from "needs a human".
	// Values seen in this fleet: mergeable, not_approved, ci_still_running,
	// need_rebase, conflict, draft_status, discussions_not_resolved,
	// requested_changes, policies_denied, jira_association_missing, checking,
	// unchecked, preparing, not_open, blocked_status.
	DetailedMergeStatus string `json:"detailed_merge_status"`
	// User carries GitLab's per-caller permission block, evaluated for the
	// token that made THIS request — not for the MR author. `user.can_merge`
	// folds in project role AND protected-branch merge access, so it answers
	// "does my ROLE permit merging this branch?" (ka-0000004913).
	//
	// It does NOT answer "can this MR be merged right now" — approval rules,
	// requested changes and security policies are invisible to it. MEASURED
	// 2026-08-27 (bug ka-0000005160): monorepo !2646 and !2643 both report
	// can_merge=true AND merge_status=can_be_merged while
	// detailed_merge_status=not_approved with approvals_left=1, an approval the
	// bot cannot give itself. Pair can_merge with DetailedMergeStatus before
	// concluding an MR is self-mergeable.
	User *GitLabMRUserPerms `json:"user"`
}

// GitLabMRUserPerms is the `user` block GitLab returns on a single-MR fetch:
// the requesting token's own permissions on that MR. It is absent on list
// endpoints, so a nil User means "unknown", never "not allowed".
type GitLabMRUserPerms struct {
	CanMerge bool `json:"can_merge"`
}

// GitLabUser represents a GitLab user.
type GitLabUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// GitLabPipRef is a minimal reference to a pipeline embedded in an MR.
type GitLabPipRef struct {
	ID     int    `json:"id"`
	Status string `json:"status"` // created, pending, running, success, failed, canceled, skipped
	WebURL string `json:"web_url"`
}

// GitLabPipeline represents a full pipeline from the REST API.
type GitLabPipeline struct {
	ID        int    `json:"id"`
	Status    string `json:"status"`
	Ref       string `json:"ref"`
	SHA       string `json:"sha"`
	WebURL    string `json:"web_url"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	StartedAt string `json:"started_at"`
	// Duration and QueuedDuration are the pipeline-level run and queue seconds.
	// They are pointers because GitLab returns null for created/pending
	// pipelines; the CI-minutes collector (cicd_metrics.go) prefers summing
	// per-job durations but keeps these as a coarse fallback. Additive,
	// back-compatible field adds (Phase G, ka-0000002378).
	Duration       *float64 `json:"duration"`
	QueuedDuration *float64 `json:"queued_duration"`
	FinishedAt     *string  `json:"finished_at"`
}

// GetMergeRequestByPath fetches a merge request using an encoded project path and MR IID.
// GET /projects/:id/merge_requests/:merge_request_iid
func (c *GitLabClient) GetMergeRequestByPath(ctx context.Context, projectPath string, mrIID int) (*GitLabMR, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d", encoded, mrIID)
	var mr GitLabMR
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &mr); err != nil {
		return nil, fmt.Errorf("GitLab get MR %s!%d: %w", projectPath, mrIID, err)
	}
	return &mr, nil
}

// BranchExists reports whether a branch still exists on the origin. It is the
// authoritative post-merge cleanup check for bug ka-0000004927: `glab mr merge
// --remove-source-branch` reports "✓ Merged!" and a zero exit even when the
// branch deletion is silently refused (a protected-branch pattern denies a
// Developer-role bot the delete), so a gardener that trusts that output claims a
// cleanup it never verified. This turns the assumption into a fact by asking
// GitLab directly.
//
// The GET returns 200 with the branch object when the branch exists and 404
// once it is gone; the 404 is the positive "cleaned up" signal, so it is mapped
// to (false, nil) rather than an error. Any other failure (network, auth, 5xx)
// returns a non-nil error so callers never mistake an unreachable API for a
// deleted branch. GET /projects/:id/repository/branches/:branch
func (c *GitLabClient) BranchExists(ctx context.Context, projectPath, branch string) (bool, error) {
	encoded := url.PathEscape(projectPath)
	// A branch like feat/x contains a slash, and GitLab requires the branch
	// name to be a single URL-encoded path segment (feat%2Fx). url.PathEscape
	// encodes the slash — it escapes for one path segment — which is exactly
	// what the branches endpoint needs.
	branchSeg := url.PathEscape(branch)
	path := fmt.Sprintf("/api/v4/projects/%s/repository/branches/%s", encoded, branchSeg)
	err := c.doJSON(ctx, http.MethodGet, path, nil, nil)
	if err == nil {
		return true, nil
	}
	if isGitLabNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("GitLab check branch %s@%s: %w", projectPath, branch, err)
}

// ListMergeRequestChangedFiles returns the set of repo-relative paths an MR
// changes (both NewPath and, for renames/deletes, OldPath), used by the two-tier
// /security-review classifier (gb mr security-tier) to decide low vs high effort
// from the sensitive path globs. GET /projects/:id/merge_requests/:iid/changes
func (c *GitLabClient) ListMergeRequestChangedFiles(ctx context.Context, projectPath string, mrIID int) ([]string, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/changes", encoded, mrIID)
	var payload struct {
		Changes []GitLabDiff `json:"changes"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return nil, fmt.Errorf("GitLab get MR changes %s!%d: %w", projectPath, mrIID, err)
	}
	seen := map[string]bool{}
	var files []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, ch := range payload.Changes {
		add(ch.NewPath)
		if ch.RenamedFile || ch.DeletedFile {
			add(ch.OldPath)
		}
	}
	return files, nil
}

// ListMergeRequestChanges returns the full per-file diffs for an MR — the same
// /changes payload ListMergeRequestChangedFiles reads, but keeping each file's
// unified-diff text (GitLabDiff.Diff) so callers can inspect what an MR added or
// removed, not merely which paths it touched. Used by the schema-flag MR-comment
// bot (ka-0000007779) to see whether an init.go change adds a type:* config
// field. GET /projects/:id/merge_requests/:iid/changes
func (c *GitLabClient) ListMergeRequestChanges(ctx context.Context, projectPath string, mrIID int) ([]GitLabDiff, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/changes", encoded, mrIID)
	var payload struct {
		Changes []GitLabDiff `json:"changes"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return nil, fmt.Errorf("GitLab get MR changes %s!%d: %w", projectPath, mrIID, err)
	}
	return payload.Changes, nil
}

// AddMergeRequestLabels adds labels to an MR without removing any it already
// carries — GitLab's `add_labels` update param is additive, unlike `labels`
// which replaces the whole set. Used as an idempotency marker by MR-comment
// bots that must fire exactly once per MR. PUT /projects/:id/merge_requests/:iid
func (c *GitLabClient) AddMergeRequestLabels(ctx context.Context, projectPath string, mrIID int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d", encoded, mrIID)
	body := map[string]string{"add_labels": strings.Join(labels, ",")}
	if err := c.doJSON(ctx, http.MethodPut, path, body, nil); err != nil {
		return fmt.Errorf("GitLab add MR labels %s!%d: %w", projectPath, mrIID, err)
	}
	return nil
}

// ListMergedMRs lists merge requests with state=merged in a group, updated after the given time.
// GET /groups/:id/merge_requests?state=merged&updated_after=...
func (c *GitLabClient) ListMergedMRs(ctx context.Context, groupID int, updatedAfter time.Time) ([]GitLabMR, error) {
	q := url.Values{}
	q.Set("state", "merged")
	q.Set("updated_after", updatedAfter.UTC().Format(time.RFC3339))
	q.Set("per_page", "100")
	q.Set("order_by", "updated_at")
	q.Set("sort", "desc")

	path := fmt.Sprintf("/api/v4/groups/%d/merge_requests?%s", groupID, q.Encode())
	var mrs []GitLabMR
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &mrs); err != nil {
		return nil, fmt.Errorf("GitLab list merged MRs for group %d: %w", groupID, err)
	}
	return mrs, nil
}

// ListGroupMergeRequests lists merge requests in a group filtered by state and
// updated after the given time, newest-first. state is one of opened, closed,
// merged, locked, or all; an empty state defaults to "all". The window
// (updatedAfter) keeps the scan cheap for the reverse MR→bead reconciler
// (ka-0000004261), which only cares about recently-touched MRs.
// GET /groups/:id/merge_requests?state=...&updated_after=...
func (c *GitLabClient) ListGroupMergeRequests(ctx context.Context, groupID int, state string, updatedAfter time.Time) ([]GitLabMR, error) {
	if state == "" {
		state = "all"
	}
	q := url.Values{}
	q.Set("state", state)
	if !updatedAfter.IsZero() {
		q.Set("updated_after", updatedAfter.UTC().Format(time.RFC3339))
	}
	q.Set("per_page", "100")
	q.Set("order_by", "updated_at")
	q.Set("sort", "desc")

	path := fmt.Sprintf("/api/v4/groups/%d/merge_requests?%s", groupID, q.Encode())
	var mrs []GitLabMR
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &mrs); err != nil {
		return nil, fmt.Errorf("GitLab list %s MRs for group %d: %w", state, groupID, err)
	}
	return mrs, nil
}

// MRListOptions holds query parameters for listing merge requests.
type MRListOptions struct {
	State        string    // opened, closed, merged, locked, all (default: all)
	Labels       []string  // filter by labels (comma-joined)
	AuthorUser   string    // filter by author username
	TargetBranch string    // filter by target branch
	Search       string    // search MR title or description
	UpdatedAfter time.Time // only MRs updated after this time
	OrderBy      string    // created_at, updated_at (default: updated_at)
	Sort         string    // asc, desc (default: desc)
	PerPage      int       // results per page (default: 100)
}

// ListMergeRequests lists merge requests for a project with optional filters.
// GET /projects/:id/merge_requests
func (c *GitLabClient) ListMergeRequests(ctx context.Context, projectPath string, opts MRListOptions) ([]GitLabMR, error) {
	encoded := url.PathEscape(projectPath)
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if len(opts.Labels) > 0 {
		q.Set("labels", strings.Join(opts.Labels, ","))
	}
	if opts.AuthorUser != "" {
		q.Set("author_username", opts.AuthorUser)
	}
	if opts.TargetBranch != "" {
		q.Set("target_branch", opts.TargetBranch)
	}
	if opts.Search != "" {
		q.Set("search", opts.Search)
	}
	if !opts.UpdatedAfter.IsZero() {
		q.Set("updated_after", opts.UpdatedAfter.UTC().Format(time.RFC3339))
	}
	orderBy := opts.OrderBy
	if orderBy == "" {
		orderBy = "updated_at"
	}
	q.Set("order_by", orderBy)
	sort := opts.Sort
	if sort == "" {
		sort = "desc"
	}
	q.Set("sort", sort)
	perPage := opts.PerPage
	if perPage <= 0 {
		perPage = 100
	}
	q.Set("per_page", strconv.Itoa(perPage))

	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests?%s", encoded, q.Encode())
	var mrs []GitLabMR
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &mrs); err != nil {
		return nil, fmt.Errorf("GitLab list MRs for %s: %w", projectPath, err)
	}
	return mrs, nil
}

// ListPipelines lists pipelines for a project with optional filters.
// GET /projects/:id/pipelines
func (c *GitLabClient) ListPipelines(ctx context.Context, projectPath string, ref, status string, updatedAfter time.Time) ([]GitLabPipeline, error) {
	encoded := url.PathEscape(projectPath)
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	if status != "" {
		q.Set("status", status)
	}
	if !updatedAfter.IsZero() {
		q.Set("updated_after", updatedAfter.UTC().Format(time.RFC3339))
	}
	q.Set("per_page", "100")
	q.Set("order_by", "updated_at")
	q.Set("sort", "desc")

	path := fmt.Sprintf("/api/v4/projects/%s/pipelines?%s", encoded, q.Encode())
	var pips []GitLabPipeline
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &pips); err != nil {
		return nil, fmt.Errorf("GitLab list pipelines for %s: %w", projectPath, err)
	}
	return pips, nil
}

// GetPipelineByPath fetches a pipeline using an encoded project path.
// GET /projects/:id/pipelines/:pipeline_id
func (c *GitLabClient) GetPipelineByPath(ctx context.Context, projectPath string, pipelineID int) (*GitLabPipeline, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/pipelines/%d", encoded, pipelineID)
	var pip GitLabPipeline
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &pip); err != nil {
		return nil, fmt.Errorf("GitLab get pipeline %s/%d: %w", projectPath, pipelineID, err)
	}
	return &pip, nil
}

// GitLabCommitStatus represents a commit status entry, including external
// statuses (e.g. pushed by another CI runner) that are NOT listed under pipeline jobs but
// still contribute to the aggregated pipeline status.
type GitLabCommitStatus struct {
	ID           int    `json:"id"`
	SHA          string `json:"sha"`
	Ref          string `json:"ref"`
	Status       string `json:"status"` // pending, running, success, failed, canceled, skipped
	Name         string `json:"name"`
	TargetURL    string `json:"target_url"`
	Description  string `json:"description"`
	AllowFailure bool   `json:"allow_failure"`
	PipelineID   int    `json:"pipeline_id"`
	FinishedAt   string `json:"finished_at"`
}

// ListCommitStatusesByPath lists all commit statuses for a SHA, including
// external statuses (e.g. another CI runner's) that do not appear in pipeline jobs.
// GET /projects/:id/repository/commits/:sha/statuses
func (c *GitLabClient) ListCommitStatusesByPath(ctx context.Context, projectPath, sha string) ([]GitLabCommitStatus, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/repository/commits/%s/statuses?per_page=100", encoded, sha)
	var statuses []GitLabCommitStatus
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &statuses); err != nil {
		return nil, fmt.Errorf("GitLab list commit statuses for %s@%s: %w", projectPath, sha, err)
	}
	return statuses, nil
}

// GitLabJob represents a CI job within a pipeline.
type GitLabJob struct {
	ID        int              `json:"id"`
	Name      string           `json:"name"`
	Stage     string           `json:"stage"`
	Status    string           `json:"status"` // created, pending, running, success, failed, canceled, skipped
	WebURL    string           `json:"web_url"`
	Artifacts []GitLabArtifact `json:"artifacts"`
	Pipeline  GitLabPipRef     `json:"pipeline"`
	// Duration and QueuedDuration are the per-job billed run and queue seconds
	// (the authoritative CI-minutes signal — see docs/research/
	// cicd-minutes-metrics.md §1b). Pointers because GitLab returns null for
	// created/pending jobs. StartedAt/Runner are additive fields the CI-minutes
	// collector uses for runner-type facets (Phase G, ka-0000002378).
	Duration       *float64      `json:"duration"`
	QueuedDuration *float64      `json:"queued_duration"`
	StartedAt      *string       `json:"started_at"`
	FinishedAt     *string       `json:"finished_at"`
	Runner         *GitLabRunner `json:"runner"`
}

// GitLabRunner is the minimal runner reference embedded in a job. runner_type
// is bounded (instance_type / group_type / project_type) and is the only field
// the CI-minutes histogram facets on; description is never used as a label
// (unbounded).
type GitLabRunner struct {
	ID          int    `json:"id"`
	Description string `json:"description"`
	RunnerType  string `json:"runner_type"`
}

// GitLabArtifact represents an artifact archive in a CI job.
type GitLabArtifact struct {
	FileType   string `json:"file_type"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	FileFormat string `json:"file_format"`
}

// ListPipelineJobsByPath lists all jobs for a pipeline using an encoded project path.
func (c *GitLabClient) ListPipelineJobsByPath(ctx context.Context, projectPath string, pipelineID int) ([]GitLabJob, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/pipelines/%d/jobs?per_page=100", encoded, pipelineID)
	var jobs []GitLabJob
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &jobs); err != nil {
		return nil, fmt.Errorf("GitLab list jobs for pipeline %s/%d: %w", projectPath, pipelineID, err)
	}
	return jobs, nil
}

// GetJobLog fetches the log (trace) output for a job.
// GET /projects/:id/jobs/:job_id/trace
func (c *GitLabClient) GetJobLog(ctx context.Context, projectPath string, jobID int) (string, error) {
	encoded := url.PathEscape(projectPath)
	reqURL := fmt.Sprintf("%s/api/v4/projects/%s/jobs/%d/trace", c.baseURL, encoded, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("create job log request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch job log: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("job log returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read job log: %w", err)
	}
	return string(data), nil
}

// DownloadJobArtifacts downloads the artifact archive for a job.
// GET /projects/:id/jobs/:job_id/artifacts
func (c *GitLabClient) DownloadJobArtifacts(ctx context.Context, projectPath string, jobID int, w io.Writer) error {
	encoded := url.PathEscape(projectPath)
	reqURL := fmt.Sprintf("%s/api/v4/projects/%s/jobs/%d/artifacts", c.baseURL, encoded, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("create artifacts request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch artifacts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("artifacts download returned %d", resp.StatusCode)
	}

	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("download artifacts: %w", err)
	}
	return nil
}

// DownloadJobArtifactFile downloads a single file from a job's artifacts.
// GET /projects/:id/jobs/:job_id/artifacts/*artifact_path
func (c *GitLabClient) DownloadJobArtifactFile(ctx context.Context, projectPath string, jobID int, artifactPath string) ([]byte, error) {
	encoded := url.PathEscape(projectPath)
	reqURL := fmt.Sprintf("%s/api/v4/projects/%s/jobs/%d/artifacts/%s", c.baseURL, encoded, jobID, artifactPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create artifact file request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch artifact file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artifact file download returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read artifact file: %w", err)
	}
	return data, nil
}

// UpdateMergeRequestDescription updates only the description field of a merge request.
// PUT /projects/:id/merge_requests/:merge_request_iid
func (c *GitLabClient) UpdateMergeRequestDescription(ctx context.Context, projectPath string, mrIID int, description string) error {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d", encoded, mrIID)
	body := map[string]string{"description": description}
	if err := c.doJSON(ctx, http.MethodPut, path, body, nil); err != nil {
		return fmt.Errorf("GitLab update MR description %s!%d: %w", projectPath, mrIID, err)
	}
	return nil
}

// GitLabMRApprovalRule is a merge-request-level approval rule as returned by
// the GitLab Merge Request Approvals API. We read back only the fields needed
// to detect an existing rule by name (for idempotency) and report its state.
type GitLabMRApprovalRule struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	ApprovalsRequired int    `json:"approvals_required"`
}

// ListMergeRequestApprovalRules lists the MR-level approval rules on a merge
// request. Used to detect whether the AI-review rule has already been applied
// so re-opens / updates don't create a duplicate.
// GET /projects/:id/merge_requests/:merge_request_iid/approval_rules
//
// MR-level approval rules are a GitLab Premium feature; on a Community-Edition
// instance the endpoint returns 404. That is treated as "no rules" (empty
// slice, nil error) so callers degrade gracefully rather than failing the
// webhook.
func (c *GitLabClient) ListMergeRequestApprovalRules(ctx context.Context, projectPath string, mrIID int) ([]GitLabMRApprovalRule, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/approval_rules", encoded, mrIID)
	var rules []GitLabMRApprovalRule
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &rules); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GitLab list MR approval rules %s!%d: %w", projectPath, mrIID, err)
	}
	return rules, nil
}

// MRApprovalRuleSpec describes an MR-level approval rule to create. A rule
// requires ApprovalsRequired approvals satisfiable by the union of the named
// users and groups.
type MRApprovalRuleSpec struct {
	Name              string
	ApprovalsRequired int
	GroupIDs          []int
	UserIDs           []int
}

// CreateMergeRequestApprovalRule adds an MR-level approval rule to a merge
// request, requiring the given number of approvals from the named users/groups.
// POST /projects/:id/merge_requests/:merge_request_iid/approval_rules
func (c *GitLabClient) CreateMergeRequestApprovalRule(ctx context.Context, projectPath string, mrIID int, spec MRApprovalRuleSpec) (*GitLabMRApprovalRule, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/approval_rules", encoded, mrIID)
	reqBody := map[string]any{
		"name":               spec.Name,
		"approvals_required": spec.ApprovalsRequired,
	}
	if len(spec.GroupIDs) > 0 {
		reqBody["group_ids"] = spec.GroupIDs
	}
	if len(spec.UserIDs) > 0 {
		reqBody["user_ids"] = spec.UserIDs
	}
	var rule GitLabMRApprovalRule
	if err := c.doJSON(ctx, http.MethodPost, path, reqBody, &rule); err != nil {
		return nil, fmt.Errorf("GitLab create MR approval rule %s!%d %q: %w", projectPath, mrIID, spec.Name, err)
	}
	return &rule, nil
}

// CreateMergeRequestPipeline triggers a new pipeline for a merge request.
// It yields a source=merge_request_event pipeline — the only
// kind accepted by projects whose workflow:rules admit merge_request_event
// only, e.g. the monorepo (ka-0000003062). It also works on projects that
// allow branch pipelines, so it is the correct call for any MR-scoped
// retrigger.
// POST /projects/:id/merge_requests/:merge_request_iid/pipelines
func (c *GitLabClient) CreateMergeRequestPipeline(ctx context.Context, projectPath string, mrIID int) (*GitLabPipeline, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/pipelines", encoded, mrIID)
	var pip GitLabPipeline
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &pip); err != nil {
		return nil, fmt.Errorf("GitLab create MR pipeline %s!%d: %w", projectPath, mrIID, err)
	}
	return &pip, nil
}

// CancelPipelineByPath cancels a pipeline. Safe to call on any non-terminal
// pipeline; GitLab returns 200 with the updated pipeline body. Calling on a
// terminal pipeline (success/failed/canceled/skipped) returns 400.
// POST /projects/:id/pipelines/:pipeline_id/cancel
func (c *GitLabClient) CancelPipelineByPath(ctx context.Context, projectPath string, pipelineID int) (*GitLabPipeline, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/pipelines/%d/cancel", encoded, pipelineID)
	var pip GitLabPipeline
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &pip); err != nil {
		return nil, fmt.Errorf("GitLab cancel pipeline %s/%d: %w", projectPath, pipelineID, err)
	}
	return &pip, nil
}

// GetMRNote fetches a single note on a merge request by note ID.
// GET /projects/:id/merge_requests/:merge_request_iid/notes/:note_id
func (c *GitLabClient) GetMRNote(ctx context.Context, projectPath string, mrIID, noteID int) (*GitLabNote, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/notes/%d", encoded, mrIID, noteID)
	var note GitLabNote
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &note); err != nil {
		return nil, fmt.Errorf("GitLab get MR note %s!%d#%d: %w", projectPath, mrIID, noteID, err)
	}
	return &note, nil
}

// GitLabNote represents a note (comment) on a GitLab merge request.
type GitLabNote struct {
	ID        int        `json:"id"`
	Body      string     `json:"body"`
	Author    GitLabUser `json:"author"`
	CreatedAt string     `json:"created_at"`
	System    bool       `json:"system"`
}

// PostMRNote creates a new note (comment) on a merge request.
// POST /projects/:id/merge_requests/:merge_request_iid/notes
func (c *GitLabClient) PostMRNote(ctx context.Context, projectPath string, mrIID int, body string) (*GitLabNote, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/notes", encoded, mrIID)
	reqBody := map[string]string{"body": body}
	var note GitLabNote
	if err := c.doJSON(ctx, http.MethodPost, path, reqBody, &note); err != nil {
		return nil, fmt.Errorf("GitLab post MR note %s!%d: %w", projectPath, mrIID, err)
	}
	return &note, nil
}

// PostMRDiscussionReply adds a reply to an existing discussion thread on a merge request.
// POST /projects/:id/merge_requests/:merge_request_iid/discussions/:discussion_id/notes
func (c *GitLabClient) PostMRDiscussionReply(ctx context.Context, projectPath string, mrIID int, discussionID, body string) (*GitLabNote, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/discussions/%s/notes", encoded, mrIID, discussionID)
	reqBody := map[string]string{"body": body}
	var note GitLabNote
	if err := c.doJSON(ctx, http.MethodPost, path, reqBody, &note); err != nil {
		return nil, fmt.Errorf("GitLab reply to discussion %s on MR %s!%d: %w", discussionID, projectPath, mrIID, err)
	}
	return &note, nil
}

// ListMRNotes lists all notes on a merge request, ordered by creation date descending.
// GET /projects/:id/merge_requests/:merge_request_iid/notes
func (c *GitLabClient) ListMRNotes(ctx context.Context, projectPath string, mrIID int) ([]GitLabNote, error) {
	encoded := url.PathEscape(projectPath)
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("sort", "desc")
	q.Set("order_by", "created_at")
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/notes?%s", encoded, mrIID, q.Encode())
	var notes []GitLabNote
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &notes); err != nil {
		return nil, fmt.Errorf("GitLab list MR notes %s!%d: %w", projectPath, mrIID, err)
	}
	return notes, nil
}

// MRRef holds the parsed components of a GitLab MR URL.
type MRRef struct {
	Host        string // URL host, e.g. "gitlab.com" or "gitlab-dark.gardenio-e2e.e2e.example.com"
	ProjectPath string // e.g., "example-org/helm-chart"
	IID         int    // merge request IID (project-scoped)
}

// mrURLPattern matches GitLab MR URLs, capturing host, project path and IID:
//
//	https://gitlab.com/example-org/helm-chart/-/merge_requests/211
//	  → Host=gitlab.com  ProjectPath=example-org/helm-chart  IID=211
//
// The host capture (group 1) is what makes the dual-GitLab world safe: with
// one bridge per host sharing the same beads daemon + SSE stream, each bridge
// uses MRRef.Host to skip MR URLs that belong to the other host (see
// GitLabSync.hostMatches and the R2 artifact §7 "Dual-GitLab structural note").
var mrURLPattern = regexp.MustCompile(`^https?://([^/]+)/(.+?)/-/merge_requests/(\d+)`)

// ParseMRURL extracts the host, project path and MR IID from a GitLab MR URL.
// Returns nil if the URL does not match the expected pattern.
func ParseMRURL(rawURL string) *MRRef {
	m := mrURLPattern.FindStringSubmatch(rawURL)
	if m == nil {
		return nil
	}
	iid, err := strconv.Atoi(m[3])
	if err != nil {
		return nil
	}
	return &MRRef{
		Host:        m[1],
		ProjectPath: m[2],
		IID:         iid,
	}
}

// GitLabTag represents a tag from the GitLab repository tags API.
type GitLabTag struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
		// CreatedAt is the tagged commit's timestamp (RFC3339). Release tags
		// are cut on a fresh HEAD by scripts/release.sh, so this is within
		// seconds of the tag push — and for a tag on an older commit it only
		// errs earlier, which is the conservative direction for freshness
		// checks like the release-drought watchdog.
		CreatedAt string `json:"created_at"`
	} `json:"commit"`
}

// GitLabCompare is the response from the GitLab repository compare API.
//
// Note there is deliberately no commit_count field: GitLab's
// /repository/compare response does NOT include one (its keys are commit,
// commits, diffs, compare_timeout, compare_same_ref, web_url). A
// `commit_count` field used to be declared here and always decoded to 0,
// silently zeroing every "commits ahead" readout (ka-0000003891). Use
// CommitCount() instead.
//
// CompareTimeout mirrors the API's `compare_timeout` flag. GitLab caps the
// diffs array at 3000 entries (and also on internal time/size limits) and sets
// this flag true when it does; the commits array is unaffected (verified live
// to 16,130 commits). Any diffs-derived count is a LOWER BOUND when it is set,
// so callers must not present len(Diffs) as exact — see DiffsTruncated()
// (ka-0000004642).
type GitLabCompare struct {
	Commits        []GitLabCompareCommit `json:"commits"`
	Diffs          []GitLabDiff          `json:"diffs"`
	CompareTimeout bool                  `json:"compare_timeout"`
}

// GitLabCompareCommit is one commit in a /repository/compare response.
type GitLabCompareCommit struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Author  string `json:"author_name"`
	// CommittedDate is the commit's committer timestamp (RFC3339) — for a
	// merge into main it is ~= the merge time, i.e. when the change landed and
	// became undelivered work. Used by `gb release status` to age the oldest
	// undelivered commit (the delivery-freshness idle window, ka-0000003628).
	// CreatedAt mirrors it (GitLab sets both) and is the fallback when a future
	// API omits committed_date.
	CommittedDate string `json:"committed_date"`
	CreatedAt     string `json:"created_at"`
}

// CommitCount returns the number of commits in the compared range, derived
// from the commits array GitLab actually returns.
//
// This equals `git rev-list --count <from>..<to>` — verified live against
// example-org/gardenio at 10 and at 15,978 commits (the array is not
// truncated or paginated), and on genuinely divergent refs where 9,896
// commits matched exactly. The `straight` query param changes which diffs
// are reported but not the commit set, so the count holds for both
// straight=true (direct from..to) and the default merge-base comparison.
func (c *GitLabCompare) CommitCount() int {
	return len(c.Commits)
}

// gitlabCompareDiffCap is the hard ceiling GitLab enforces on the compare
// endpoint's diffs array. When a comparison touches more than this many files,
// GitLab returns exactly this many diff entries and sets compare_timeout=true.
// Verified live against example-org/gardenio (root...main: 16,130
// commits, exactly 3000 diffs, compare_timeout=true) — a round-number cap, not
// a real file count (ka-0000004642).
const gitlabCompareDiffCap = 3000

// DiffsTruncated reports whether GitLab capped the diffs array so that any
// count derived from Diffs (e.g. ImageAheadBy) is a lower bound rather than an
// exact figure. It trusts the API's own compare_timeout flag, which GitLab
// sets whenever it hits the 3000-file cap or an internal time/size limit, and
// falls back to the len==cap heuristic in case a future API omits the flag.
// The commits array is never truncated, so CommitCount() stays exact.
func (c *GitLabCompare) DiffsTruncated() bool {
	return c.CompareTimeout || len(c.Diffs) >= gitlabCompareDiffCap
}

// GitLabDiff is a file diff entry from the GitLab compare / MR changes API
// response. Diff carries the unified-diff hunk text (present on the MR changes
// endpoint; empty on the endpoints that only return the changed-path set).
type GitLabDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
}

// GetLatestTag returns the most recent calver tag for a GitLab project.
// It fetches up to 100 tags and picks the highest calver tag by
// (year, dayOfYear, build). Returns an error if no calver tags exist.
// GET /projects/:id/repository/tags?per_page=100
func (c *GitLabClient) GetLatestTag(ctx context.Context, projectPath string) (string, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/repository/tags?per_page=100", encoded)
	var tags []GitLabTag
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &tags); err != nil {
		return "", fmt.Errorf("GitLab list tags for %s: %w", projectPath, err)
	}
	if len(tags) == 0 {
		return "", fmt.Errorf("no tags found for %s", projectPath)
	}

	type calverTag struct {
		name             string
		year, doy, build int
	}
	var cv []calverTag
	for _, t := range tags {
		if y, d, b, ok := calverKey(t.Name); ok {
			cv = append(cv, calverTag{name: t.Name, year: y, doy: d, build: b})
		}
	}
	if len(cv) == 0 {
		return "", fmt.Errorf("no calver tags found for %s", projectPath)
	}

	sort.Slice(cv, func(i, j int) bool {
		if cv[i].year != cv[j].year {
			return cv[i].year > cv[j].year
		}
		if cv[i].doy != cv[j].doy {
			return cv[i].doy > cv[j].doy
		}
		return cv[i].build > cv[j].build
	})
	return cv[0].name, nil
}

// CompareRefs compares two refs (tags, branches, or SHAs) in a GitLab project.
// GET /projects/:id/repository/compare?from=X&to=Y
func (c *GitLabClient) CompareRefs(ctx context.Context, projectPath, from, to string) (*GitLabCompare, error) {
	encoded := url.PathEscape(projectPath)
	q := url.Values{}
	q.Set("from", from)
	q.Set("to", to)
	path := fmt.Sprintf("/api/v4/projects/%s/repository/compare?%s", encoded, q.Encode())
	var result GitLabCompare
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, fmt.Errorf("GitLab compare %s %s...%s: %w", projectPath, from, to, err)
	}
	return &result, nil
}

// GitLabRelease represents a GitLab Release from the releases API. A Release
// exists for a tag only after the `create-release` pipeline job runs — the
// LAST step of the release pipeline — so the presence of a Release is the
// load-bearing signal that a calver tag was promoted to "golden" (all E2E
// gates green, prod deploy done, images promoted). `-rc`-only tags never
// appear here. See gardener-release-owner doc ka-0000000327 Step 2.
type GitLabRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	ReleasedAt  string `json:"released_at"`
	CreatedAt   string `json:"created_at"`
	UpcomingRel bool   `json:"upcoming_release"`
}

// ListTags lists repository tags for a GitLab project, newest-first by the
// GitLab API's default ordering (updated/commit date). It is the typed
// equivalent of the gardener's `glab api .../repository/tags` read in Step 2.
// Read-only: GET /projects/:id/repository/tags?per_page=100
func (c *GitLabClient) ListTags(ctx context.Context, projectPath string) ([]GitLabTag, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/repository/tags?per_page=100", encoded)
	var tags []GitLabTag
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &tags); err != nil {
		return nil, fmt.Errorf("GitLab list tags for %s: %w", projectPath, err)
	}
	return tags, nil
}

// ListReleases lists GitLab Releases for a project, newest-first. It is the
// typed equivalent of the gardener's `glab release list` read in Step 2 —
// used to confirm golden-promotion (golden tags appear; `-rc`-only tags do
// not). Read-only: GET /projects/:id/releases?per_page=100
func (c *GitLabClient) ListReleases(ctx context.Context, projectPath string) ([]GitLabRelease, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/releases?per_page=100", encoded)
	var releases []GitLabRelease
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &releases); err != nil {
		return nil, fmt.Errorf("GitLab list releases for %s: %w", projectPath, err)
	}
	return releases, nil
}

// ── First-party CI/CD compute-minutes usage + quota (Phase G) ──────────────
//
// These read GitLab's first-party billed compute-minutes surfaces the
// job-duration reconstruction can't reach: the GraphQL `ciMinutesUsage`
// namespace usage series and the REST group quota ceiling. Ported from
// Shannon's acme-helm-chart !440 gitlab_usage_collector.py into gardenio Go
// idiom (amendment decision ka-0000001083). Both need a group-owner-scoped
// token (escalation ka-0000006982); the existing bridge GAT 404s them, so the
// CI-minutes collector's first-party series stays dark until that token lands.

// ciMinutesUsageQuery is the GraphQL query for monthly namespace compute-minute
// usage, with per-project breakdown. Mirrors GRAPHQL_USAGE_QUERY in the port
// source.
const ciMinutesUsageQuery = `query($namespaceId: NamespaceID!, $first: Int!, $after: String) {
  ciMinutesUsage(namespaceId: $namespaceId, first: $first, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      month
      monthIso8601
      minutes
      projects(first: $first) {
        pageInfo { hasNextPage }
        nodes {
          minutes
          project { name fullPath }
        }
      }
    }
  }
}`

// GitLabCIMinutesMonth is one month's namespace compute-minute usage plus its
// per-project breakdown. minutes is the namespace total for the month.
type GitLabCIMinutesMonth struct {
	Month        string
	MonthISO8601 string
	Minutes      float64
	Projects     []GitLabCIMinutesProject
}

// GitLabCIMinutesProject is one project's compute-minute usage within a month.
type GitLabCIMinutesProject struct {
	Name     string
	FullPath string
	Minutes  float64
}

// GitLabComputeQuota is the group's compute-minute quota ceiling from the REST
// group endpoint: the base monthly allotment and any additional balance. Both
// are pointers because GitLab omits/nulls them for namespaces without a finite
// quota (unlimited). Base==nil or 0 means "no finite base quota".
type GitLabComputeQuota struct {
	Base       *float64
	Additional *float64
}

// gqlUsageResponse is the decode shape for the ciMinutesUsage GraphQL query.
type gqlUsageResponse struct {
	Data struct {
		CIMinutesUsage struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []struct {
				Month        string   `json:"month"`
				MonthISO8601 string   `json:"monthIso8601"`
				Minutes      *float64 `json:"minutes"`
				Projects     struct {
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						Minutes *float64 `json:"minutes"`
						Project struct {
							Name     string `json:"name"`
							FullPath string `json:"fullPath"`
						} `json:"project"`
					} `json:"nodes"`
				} `json:"projects"`
			} `json:"nodes"`
		} `json:"ciMinutesUsage"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// GetCIMinutesUsage fetches monthly namespace compute-minute usage for a
// namespace (GraphQL global id, e.g. "gid://gitlab/Group/14331212"), following
// GraphQL cursors, newest-month-first, capped at lookbackMonths (min 1). Needs
// the group-owner token; the bridge GAT returns a GraphQL error / empty data.
func (c *GitLabClient) GetCIMinutesUsage(ctx context.Context, namespaceID string, lookbackMonths int) ([]GitLabCIMinutesMonth, error) {
	if lookbackMonths < 1 {
		lookbackMonths = 1
	}
	var months []GitLabCIMinutesMonth
	after := ""
	for {
		vars := map[string]any{"namespaceId": namespaceID, "first": 100}
		if after != "" {
			vars["after"] = after
		}
		var resp gqlUsageResponse
		if err := c.doGraphQL(ctx, ciMinutesUsageQuery, vars, &resp); err != nil {
			return nil, err
		}
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("GitLab GraphQL ciMinutesUsage failed: %s", gqlErrorMessages(resp.Errors))
		}
		conn := resp.Data.CIMinutesUsage
		for _, n := range conn.Nodes {
			month := GitLabCIMinutesMonth{
				Month:        n.Month,
				MonthISO8601: n.MonthISO8601,
				Minutes:      deref(n.Minutes),
			}
			for _, p := range n.Projects.Nodes {
				if p.Project.FullPath == "" {
					continue
				}
				month.Projects = append(month.Projects, GitLabCIMinutesProject{
					Name:     p.Project.Name,
					FullPath: p.Project.FullPath,
					Minutes:  deref(p.Minutes),
				})
			}
			months = append(months, month)
		}
		if !conn.PageInfo.HasNextPage || conn.PageInfo.EndCursor == "" {
			break
		}
		after = conn.PageInfo.EndCursor
	}
	sort.Slice(months, func(i, j int) bool {
		return months[i].MonthISO8601 > months[j].MonthISO8601
	})
	if len(months) > lookbackMonths {
		months = months[:lookbackMonths]
	}
	return months, nil
}

// GetGroupComputeQuota fetches the base + additional compute-minute quota
// ceiling for a group via the REST group endpoint. shared_runners_minutes_limit
// is the base allotment; extra_shared_runners_minutes_limit is the additional
// balance. Needs the group-owner token; the bridge GAT sees these as null.
// GET /groups/:id
func (c *GitLabClient) GetGroupComputeQuota(ctx context.Context, groupPath string) (GitLabComputeQuota, error) {
	encoded := url.PathEscape(groupPath)
	path := fmt.Sprintf("/api/v4/groups/%s?with_custom_attributes=false&statistics=false", encoded)
	var raw struct {
		SharedRunnersMinutesLimit      *float64 `json:"shared_runners_minutes_limit"`
		ExtraSharedRunnersMinutesLimit *float64 `json:"extra_shared_runners_minutes_limit"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return GitLabComputeQuota{}, fmt.Errorf("GitLab get group compute quota %s: %w", groupPath, err)
	}
	return GitLabComputeQuota{
		Base:       raw.SharedRunnersMinutesLimit,
		Additional: raw.ExtraSharedRunnersMinutesLimit,
	}, nil
}

// deref returns the pointed-to float64 or 0 for a nil pointer.
func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// gqlErrorMessages joins GraphQL error messages for a single error string.
func gqlErrorMessages(errs []struct {
	Message string `json:"message"`
}) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	return strings.Join(msgs, "; ")
}

// doGraphQL POSTs a GraphQL query to the GitLab /api/graphql endpoint using
// Bearer auth (GraphQL rejects PRIVATE-TOKEN for some fields; the port source
// uses Authorization: Bearer). The response is decoded into result.
func (c *GitLabClient) doGraphQL(ctx context.Context, query string, variables map[string]any, result any) error {
	payload := map[string]any{"query": query, "variables": variables}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal GraphQL request: %w", err)
	}
	reqURL := c.baseURL + "/api/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("create GraphQL request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitLab GraphQL request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read GitLab GraphQL response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GitLab GraphQL returned %d: %s", resp.StatusCode, truncate(string(respBody), 512))
	}
	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("decode GitLab GraphQL response: %w", err)
		}
	}
	return nil
}

// doJSON performs an HTTP request against the GitLab API with JSON response handling.
func (c *GitLabClient) doJSON(ctx context.Context, method, path string, body any, result any) error {
	err := httpjson.Do(ctx, c.httpClient, httpjson.Request{
		Method: method,
		URL:    c.baseURL + path,
		Header: http.Header{"PRIVATE-TOKEN": {c.token}},
		Body:   body,
	}, result)
	var se *httpjson.StatusError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return &gitLabAPIError{
			status: se.StatusCode,
			msg:    fmt.Sprintf("GitLab API %s %s returned %d: %s", method, path, se.StatusCode, truncate(se.Body, 512)),
		}
	default:
		return fmt.Errorf("GitLab %w", err)
	}
}
