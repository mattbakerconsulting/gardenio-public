package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
)

// UnreleasedResponse is the JSON response for the /api/unreleased endpoint.
type UnreleasedResponse struct {
	Repos   []UnreleasedRepo       `json:"repos"`
	Images  []UnreleasedImage      `json:"images,omitempty"`
	Cluster *controllerVersionInfo `json:"cluster,omitempty"`
	Bridge  string                 `json:"bridge"`
}

// UnreleasedImage holds unreleased build context changes for a container image.
type UnreleasedImage struct {
	Name         string `json:"name"`
	DeployedTag  string `json:"deployedTag"`
	AheadBy      int    `json:"aheadBy"`      // total commits in range
	ImageAheadBy int    `json:"imageAheadBy"` // files touching image paths
	// FilesTruncated is set when GitLab capped the compare diffs array (3000
	// files) so ImageAheadBy/Files under-report: the true count is a lower
	// bound, not exact. Consumers render it as "N+" (ka-0000004642).
	FilesTruncated bool               `json:"filesTruncated,omitempty"`
	Files          []string           `json:"files,omitempty"`
	Commits        []UnreleasedCommit `json:"commits,omitempty"`
	Error          string             `json:"error,omitempty"`
}

// UnreleasedRepo holds unreleased commit info for a single repository.
type UnreleasedRepo struct {
	Repo      string             `json:"repo"`
	LatestTag string             `json:"latestTag,omitempty"`
	AheadBy   int                `json:"aheadBy"`
	Commits   []UnreleasedCommit `json:"commits,omitempty"`
	Error     string             `json:"error,omitempty"`
	External  bool               `json:"external,omitempty"` // external dep — deployed via rolling tag
}

// UnreleasedCommit is a single commit in the unreleased response.
type UnreleasedCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
}

// UnreleasedConfig holds the dependencies needed to serve unreleased data.
type UnreleasedConfig struct {
	// GitLab client for fetching tags and comparing refs.
	GitLab *GitLabClient

	// Projects is the list of GitLab project paths to check for unreleased commits.
	Projects []ProjectRef

	ControllerURL string
	Version       string
	Images        []ImageTrackConfig

	// DEPRECATED: GitHub client — kept for backward compatibility during migration.
	// Will be removed once all callers switch to GitLab.
	GitHub *GitHubClient
	Repos  []RepoRef
}

// ProjectRef identifies a GitLab project to track for unreleased commits.
type ProjectRef struct {
	Path     string // GitLab project path, e.g. "example-org/gardenio"
	External bool   // external dep — deployed via rolling tag, not its own release
}

func (p ProjectRef) String() string { return p.Path }

// GetUnreleasedData fetches unreleased commits across all tracked repos and
// the controller version info. It returns a JSON-serializable response.
func GetUnreleasedData(ctx context.Context, cfg UnreleasedConfig) *UnreleasedResponse {
	// Determine projects to query: prefer GitLab, fall back to GitHub repos.
	projects := cfg.Projects
	if len(projects) == 0 && len(cfg.Repos) > 0 {
		for _, r := range cfg.Repos {
			projects = append(projects, ProjectRef{Path: r.String(), External: r.External})
		}
	}

	resp := &UnreleasedResponse{
		Repos:  make([]UnreleasedRepo, len(projects)),
		Bridge: cfg.Version,
	}

	var wg sync.WaitGroup

	// Fetch all projects concurrently via GitLab API.
	if cfg.GitLab != nil {
		for i, proj := range projects {
			wg.Add(1)
			go func(idx int, p ProjectRef) {
				defer wg.Done()
				resp.Repos[idx] = getGitLabUnreleased(ctx, cfg.GitLab, p)
			}(i, proj)
		}
	} else if cfg.GitHub != nil {
		// Legacy GitHub path — used when GitLab client is not configured.
		for i, repo := range cfg.Repos {
			wg.Add(1)
			go func(idx int, r RepoRef) {
				defer wg.Done()
				result := cfg.GitHub.GetUnreleased(ctx, r, "main")
				entry := UnreleasedRepo{
					Repo:      r.String(),
					LatestTag: result.LatestTag,
					AheadBy:   result.AheadBy,
					External:  r.External,
				}
				if result.Error != nil {
					entry.Error = result.Error.Error()
				}
				for _, c := range result.Commits {
					entry.Commits = append(entry.Commits, UnreleasedCommit{
						SHA:     c.SHA,
						Message: firstLine(c.Message),
						Author:  c.Author,
					})
				}
				resp.Repos[idx] = entry
			}(i, repo)
		}
	}

	// Fetch image unreleased data concurrently.
	if len(cfg.Images) > 0 {
		resp.Images = make([]UnreleasedImage, len(cfg.Images))
		for i, img := range cfg.Images {
			wg.Add(1)
			go func(idx int, ic ImageTrackConfig) {
				defer wg.Done()
				if cfg.GitLab != nil {
					resp.Images[idx] = getGitLabImageUnreleased(ctx, cfg.GitLab, ic, "main")
				} else if cfg.GitHub != nil {
					result := cfg.GitHub.GetImageUnreleased(ctx, ic, "main")
					entry := UnreleasedImage{
						Name:         result.Name,
						DeployedTag:  result.DeployedTag,
						AheadBy:      result.AheadBy,
						ImageAheadBy: result.ImageAheadBy,
						Files:        result.Files,
					}
					if result.Error != nil {
						entry.Error = result.Error.Error()
					}
					for _, c := range result.Commits {
						entry.Commits = append(entry.Commits, UnreleasedCommit{
							SHA:     c.SHA,
							Message: firstLine(c.Message),
							Author:  c.Author,
						})
					}
					resp.Images[idx] = entry
				}
			}(i, img)
		}
	}

	// Fetch controller version concurrently.
	if cfg.ControllerURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp.Cluster = fetchControllerVersion(ctx, cfg.ControllerURL)
		}()
	}

	wg.Wait()
	return resp
}

// getGitLabUnreleased fetches unreleased commits for a GitLab project.
func getGitLabUnreleased(ctx context.Context, gl *GitLabClient, proj ProjectRef) UnreleasedRepo {
	entry := UnreleasedRepo{
		Repo:     proj.Path,
		External: proj.External,
	}

	tag, err := gl.GetLatestTag(ctx, proj.Path)
	if err != nil {
		entry.Error = fmt.Errorf("get latest tag: %w", err).Error()
		return entry
	}
	entry.LatestTag = tag

	cmp, err := gl.CompareRefs(ctx, proj.Path, tag, "main")
	if err != nil {
		entry.Error = fmt.Errorf("compare %s...main: %w", tag, err).Error()
		return entry
	}
	entry.AheadBy = cmp.CommitCount()

	// Cap at 10 commits for Slack block limits.
	commits := cmp.Commits
	start := 0
	if len(commits) > 10 {
		start = len(commits) - 10
	}
	for _, c := range commits[start:] {
		entry.Commits = append(entry.Commits, UnreleasedCommit{
			SHA:     c.ID,
			Message: firstLine(c.Message),
			Author:  c.Author,
		})
	}
	return entry
}

// getGitLabImageUnreleased compares a deployed image tag to a branch using
// GitLab's compare API, returning only changes that touch the image's paths.
func getGitLabImageUnreleased(ctx context.Context, gl *GitLabClient, cfg ImageTrackConfig, branch string) UnreleasedImage {
	entry := UnreleasedImage{
		Name:        cfg.Name,
		DeployedTag: cfg.Tag,
	}

	if cfg.Tag == "" {
		entry.Error = fmt.Sprintf("no deployed tag for image %s", cfg.Name)
		return entry
	}

	projectPath := cfg.ProjectPath
	if projectPath == "" {
		// Fall back to RepoRef for backward compatibility.
		projectPath = cfg.Repo.String()
	}

	cmp, err := gl.CompareRefs(ctx, projectPath, cfg.Tag, branch)
	if err != nil {
		entry.Error = fmt.Errorf("compare %s...%s: %w", cfg.Tag, branch, err).Error()
		return entry
	}
	entry.AheadBy = cmp.CommitCount()

	// Filter diffs by path prefixes.
	for _, d := range cmp.Diffs {
		path := d.NewPath
		if d.DeletedFile {
			path = d.OldPath
		}
		if matchesPathPrefixes(path, cfg.Paths) {
			entry.Files = append(entry.Files, path)
		}
	}
	entry.ImageAheadBy = len(entry.Files)
	// GitLab caps the compare diffs array at 3000 files (ka-0000004642). When
	// it does, the path-filtered file list is derived from a truncated set, so
	// ImageAheadBy is a lower bound, not exact — flag it for honest rendering.
	entry.FilesTruncated = cmp.DiffsTruncated()

	// Include commits (capped at 10).
	commits := cmp.Commits
	start := 0
	if len(commits) > 10 {
		start = len(commits) - 10
	}
	for _, c := range commits[start:] {
		entry.Commits = append(entry.Commits, UnreleasedCommit{
			SHA:     c.ID,
			Message: firstLine(c.Message),
			Author:  c.Author,
		})
	}
	return entry
}

// NewGitHubClientIfConfigured creates a GitHubClient when a token or repos are
// provided. Returns nil otherwise (matching Bot constructor behaviour).
//
// Deprecated: Use GitLabClient instead. Kept for backward compatibility during
// the GitHub→GitLab migration. Will be removed once GITHUB_TOKEN is no longer
// set in any deployment.
func NewGitHubClientIfConfigured(token string, repos []RepoRef, logger *slog.Logger) *GitHubClient {
	if token != "" || len(repos) > 0 {
		return NewGitHubClient(token, logger)
	}
	return nil
}

// MonorepoImageConfigs returns ImageTrackConfigs for the gardenio monorepo.
// All images are built from a single repo, so paths are prefixed with their
// subtree directory (e.g., "gardenio/images/agent/").
//
// Deprecated: Use GitLabMonorepoImageConfigs instead. This function uses GitHub
// RepoRef which is being phased out.
func MonorepoImageConfigs(repo RepoRef, tag string) []ImageTrackConfig {
	if tag == "" {
		return nil
	}
	return []ImageTrackConfig{
		{Name: "agent", Repo: repo, Tag: tag, Paths: []string{"gardenio/images/agent/", ".github/workflows/images.yml"}},
		{Name: "controller", Repo: repo, Tag: tag, Paths: []string{"gardenio/controller/"}},
		{Name: "slack-bridge", Repo: repo, Tag: tag, Paths: []string{"gardenio/images/slack-bridge/", "gardenio/controller/"}},
		{Name: "jira-bridge", Repo: repo, Tag: tag, Paths: []string{"gardenio/images/jira-bridge/", "gardenio/controller/"}},
		{Name: "kbeads", Repo: repo, Tag: tag, Paths: []string{"kbeads/"}},
		{Name: "coopmux", Repo: repo, Tag: tag, Paths: []string{"coop/"}},
	}
}

// GitLabMonorepoImageConfigs returns ImageTrackConfigs for the gardenio monorepo
// using a GitLab project path instead of a GitHub RepoRef.
func GitLabMonorepoImageConfigs(projectPath, tag string) []ImageTrackConfig {
	if tag == "" {
		return nil
	}
	return []ImageTrackConfig{
		{Name: "agent", ProjectPath: projectPath, Tag: tag, Paths: []string{"gardenio/images/agent/", ".github/workflows/images.yml"}},
		{Name: "controller", ProjectPath: projectPath, Tag: tag, Paths: []string{"gardenio/controller/"}},
		{Name: "slack-bridge", ProjectPath: projectPath, Tag: tag, Paths: []string{"gardenio/images/slack-bridge/", "gardenio/controller/"}},
		{Name: "jira-bridge", ProjectPath: projectPath, Tag: tag, Paths: []string{"gardenio/images/jira-bridge/", "gardenio/controller/"}},
		{Name: "kbeads", ProjectPath: projectPath, Tag: tag, Paths: []string{"kbeads/"}},
		{Name: "coopmux", ProjectPath: projectPath, Tag: tag, Paths: []string{"coop/"}},
	}
}

// HandleUnreleased returns an http.HandlerFunc that serves unreleased data as JSON.
func HandleUnreleased(cfg UnreleasedConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := GetUnreleasedData(r.Context(), cfg)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(data)
	}
}
