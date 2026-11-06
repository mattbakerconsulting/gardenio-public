package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/httpjson"
)

// GitHubClient is a lightweight REST client for the GitHub API.
//
// Deprecated: Use GitLabClient instead. The gardenio repo moved to GitLab.
// Kept for backward compatibility during migration.
type GitHubClient struct {
	httpClient *http.Client
	baseURL    string // defaults to "https://api.github.com"
	token      string
	logger     *slog.Logger
}

// RepoRef identifies a GitHub repository.
//
// Deprecated: Use ProjectRef instead for GitLab project paths.
type RepoRef struct {
	Owner    string
	Repo     string
	External bool // external dep — deployed via rolling tag, not its own release
}

func (r RepoRef) String() string { return r.Owner + "/" + r.Repo }

// RepoUnreleased holds the result of checking unreleased commits for a repo.
type RepoUnreleased struct {
	Repo      RepoRef
	LatestTag string
	AheadBy   int
	Commits   []GitCommit
	Error     error
}

// GitCommit is a single commit from the GitHub compare API.
type GitCommit struct {
	SHA     string
	Message string
	Author  string
}

// ghTag is a tag from the GitHub tags API.
type ghTag struct {
	Name string `json:"name"`
}

// ghCompare is the response from the GitHub compare API.
type ghCompare struct {
	AheadBy int `json:"ahead_by"`
	Commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commit"`
	} `json:"commits"`
	Files []ghCompareFile `json:"files"`
}

// ghCompareFile is a file entry from the GitHub compare API response.
type ghCompareFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"` // "added", "removed", "modified", "renamed"
}

// NewGitHubClient creates a new GitHub REST API client.
// Token is optional; when set it is sent as a Bearer token for higher rate limits.
func NewGitHubClient(token string, logger *slog.Logger) *GitHubClient {
	return &GitHubClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    "https://api.github.com",
		token:      token,
		logger:     logger,
	}
}

// calverRe matches calver tags like "2026.60.2" or "v2026.60.2".
var calverRe = regexp.MustCompile(`^v?(\d{4})\.(\d+)\.(\d+)$`)

// calverKey extracts (year, dayOfYear, build) from a calver tag.
// Returns ok=false if the tag doesn't match.
func calverKey(tag string) (year, doy, build int, ok bool) {
	m := calverRe.FindStringSubmatch(tag)
	if m == nil {
		return 0, 0, 0, false
	}
	year, _ = strconv.Atoi(m[1])
	doy, _ = strconv.Atoi(m[2])
	build, _ = strconv.Atoi(m[3])
	return year, doy, build, true
}

// GetLatestTag returns the most recent calver tag for the repo.
// It fetches up to 100 tags and picks the highest calver tag by
// (year, dayOfYear, build). Returns an error if no calver tags exist.
func (c *GitHubClient) GetLatestTag(ctx context.Context, repo RepoRef) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/tags?per_page=100", c.baseURL, repo.Owner, repo.Repo)
	var tags []ghTag
	if err := c.doJSON(ctx, url, &tags); err != nil {
		return "", err
	}
	if len(tags) == 0 {
		return "", fmt.Errorf("no tags found for %s", repo)
	}

	// Collect tags that match calver format.
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
		return "", fmt.Errorf("no calver tags found for %s", repo)
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

// CompareTagToHead compares a tag to the head of a branch.
func (c *GitHubClient) CompareTagToHead(ctx context.Context, repo RepoRef, tag, branch string) (*ghCompare, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", c.baseURL, repo.Owner, repo.Repo, tag, branch)
	var result ghCompare
	if err := c.doJSON(ctx, url, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetUnreleased fetches unreleased commits for a repo (tag → branch head).
func (c *GitHubClient) GetUnreleased(ctx context.Context, repo RepoRef, branch string) *RepoUnreleased {
	result := &RepoUnreleased{Repo: repo}

	tag, err := c.GetLatestTag(ctx, repo)
	if err != nil {
		result.Error = fmt.Errorf("get latest tag: %w", err)
		return result
	}
	result.LatestTag = tag

	cmp, err := c.CompareTagToHead(ctx, repo, tag, branch)
	if err != nil {
		result.Error = fmt.Errorf("compare %s...%s: %w", tag, branch, err)
		return result
	}
	result.AheadBy = cmp.AheadBy

	// Cap at 10 commits for Slack block limits.
	limit := len(cmp.Commits)
	if limit > 10 {
		limit = 10
	}
	for _, c := range cmp.Commits[len(cmp.Commits)-limit:] {
		result.Commits = append(result.Commits, GitCommit{
			SHA:     c.SHA,
			Message: c.Commit.Message,
			Author:  c.Commit.Author.Name,
		})
	}
	return result
}

// ImageTrackConfig describes a container image to track for unreleased changes.
type ImageTrackConfig struct {
	Name        string   // human-readable name, e.g., "agent"
	Repo        RepoRef  // DEPRECATED: GitHub repo that builds this image
	ProjectPath string   // GitLab project path (e.g., "example-org/gardenio")
	Tag         string   // deployed tag (calver or commit SHA)
	Paths       []string // path prefixes to filter (e.g., "images/agent/")
}

// ImageUnreleased holds the result of checking unreleased image changes.
type ImageUnreleased struct {
	Name         string
	DeployedTag  string
	AheadBy      int // total commits in range
	ImageAheadBy int // commits touching image paths
	Files        []string
	Commits      []GitCommit
	Error        error
}

// GetImageUnreleased compares a deployed image tag to a branch and returns
// only the changes that touch the image's build context paths.
func (c *GitHubClient) GetImageUnreleased(ctx context.Context, cfg ImageTrackConfig, branch string) *ImageUnreleased {
	result := &ImageUnreleased{
		Name:        cfg.Name,
		DeployedTag: cfg.Tag,
	}

	if cfg.Tag == "" {
		result.Error = fmt.Errorf("no deployed tag for image %s", cfg.Name)
		return result
	}

	cmp, err := c.CompareTagToHead(ctx, cfg.Repo, cfg.Tag, branch)
	if err != nil {
		result.Error = fmt.Errorf("compare %s...%s: %w", cfg.Tag, branch, err)
		return result
	}
	result.AheadBy = cmp.AheadBy

	// Filter files by path prefixes.
	for _, f := range cmp.Files {
		if matchesPathPrefixes(f.Filename, cfg.Paths) {
			result.Files = append(result.Files, f.Filename)
		}
	}
	result.ImageAheadBy = len(result.Files)

	// Include commits (capped at 10) — these are all commits in the range,
	// not filtered by path. The files list shows what actually changed.
	limit := len(cmp.Commits)
	if limit > 10 {
		limit = 10
	}
	for _, c := range cmp.Commits[len(cmp.Commits)-limit:] {
		result.Commits = append(result.Commits, GitCommit{
			SHA:     c.SHA,
			Message: c.Commit.Message,
			Author:  c.Commit.Author.Name,
		})
	}

	return result
}

// matchesPathPrefixes returns true if the filename starts with any of the given prefixes.
func matchesPathPrefixes(filename string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(filename, p) {
			return true
		}
	}
	return false
}

// doJSON performs a GET request and decodes the JSON response.
func (c *GitHubClient) doJSON(ctx context.Context, url string, result any) error {
	header := http.Header{"Accept": {"application/vnd.github+json"}}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	err := httpjson.Do(ctx, c.httpClient, httpjson.Request{
		Method: http.MethodGet,
		URL:    url,
		Header: header,
	}, result)
	var se *httpjson.StatusError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return fmt.Errorf("GitHub API %s returned %d: %s", url, se.StatusCode, truncate(se.Body, 256))
	default:
		return fmt.Errorf("GitHub %w", err)
	}
}
