package releaserecord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// GitHubGetter issues one GET against the GitHub REST API (path is relative,
// "repos/owner/repo/..."), decoding the JSON response into out. GhCLI shells
// out to `gh api`; github-bridge can supply one on its installation token.
type GitHubGetter interface {
	Get(ctx context.Context, path string, out any) error
}

// GhCLI is a GitHubGetter on the gh CLI, which carries the pod's GitHub
// credentials (the agent image's credential helper).
type GhCLI struct{}

// Get runs `gh api <path>`.
func (GhCLI) Get(ctx context.Context, path string, out any) error {
	cmd := exec.CommandContext(ctx, "gh", "api", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("gh api %s: decoding response: %w", path, err)
	}
	return nil
}

// GitHub reads a repo's tags, history, merged PRs and image builds through the
// REST API. It is a Source (the compare API, walked on first parents) and a
// PRLookup.
type GitHub struct {
	API  GitHubGetter
	Repo string // owner/repo
}

var (
	_ Source   = GitHub{}
	_ PRLookup = GitHub{}
)

// maxTagPages caps Tags at 1,000 tags.
const maxTagPages = 10

// Tags lists the repo's tags (names and commits; no dates).
func (g GitHub) Tags(ctx context.Context) ([]Tag, error) {
	var out []Tag
	for page := 1; page <= maxTagPages; page++ {
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := g.API.Get(ctx, fmt.Sprintf("repos/%s/tags?per_page=100&page=%d", g.Repo, page), &resp); err != nil {
			return nil, err
		}
		for _, t := range resp {
			out = append(out, Tag{Name: t.Name, SHA: t.Commit.SHA})
		}
		if len(resp) < 100 {
			break
		}
	}
	return out, nil
}

// Tag reads one tag: its ref, then the tag object for an annotated tag (the
// tagger date and name), else the commit's date.
func (g GitHub) Tag(ctx context.Context, name string) (Tag, error) {
	var ref struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := g.API.Get(ctx, fmt.Sprintf("repos/%s/git/ref/tags/%s", g.Repo, url.PathEscape(name)), &ref); err != nil {
		return Tag{}, err
	}
	t := Tag{Name: name, SHA: ref.Object.SHA}
	if ref.Object.Type == "tag" {
		var obj struct {
			Tagger struct {
				Name string `json:"name"`
				Date string `json:"date"`
			} `json:"tagger"`
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := g.API.Get(ctx, fmt.Sprintf("repos/%s/git/tags/%s", g.Repo, ref.Object.SHA), &obj); err != nil {
			return Tag{}, err
		}
		t.SHA, t.Date, t.Tagger = obj.Object.SHA, obj.Tagger.Date, obj.Tagger.Name
		return t, nil
	}
	var c struct {
		Committer struct {
			Date string `json:"date"`
		} `json:"committer"`
	}
	if err := g.API.Get(ctx, fmt.Sprintf("repos/%s/git/commits/%s", g.Repo, t.SHA), &c); err == nil {
		t.Date = c.Committer.Date
	}
	return t, nil
}

// compareCommit is one commit of a compare response.
type compareCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date string `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// FirstParent compares base...head (GitHub returns at most 250 commits) and
// walks head's first parents through the result, so commits brought in by a
// merge are left out the way `git log --first-parent` leaves them out. With
// no base it lists head's first parents through the commits API.
func (g GitHub) FirstParent(ctx context.Context, base, head string) ([]Commit, bool, error) {
	var commits []compareCommit
	var complete bool
	if base == "" {
		var resp []compareCommit
		if err := g.API.Get(ctx, fmt.Sprintf("repos/%s/commits?sha=%s&per_page=100", g.Repo, url.QueryEscape(head)), &resp); err != nil {
			return nil, false, err
		}
		commits, complete = resp, len(resp) < 100
	} else {
		var resp struct {
			TotalCommits int             `json:"total_commits"`
			Commits      []compareCommit `json:"commits"`
		}
		path := fmt.Sprintf("repos/%s/compare/%s...%s?per_page=250", g.Repo, url.PathEscape(base), url.PathEscape(head))
		if err := g.API.Get(ctx, path, &resp); err != nil {
			return nil, false, err
		}
		commits, complete = resp.Commits, len(resp.Commits) >= resp.TotalCommits
	}
	bySHA := make(map[string]compareCommit, len(commits))
	for _, c := range commits {
		bySHA[c.SHA] = c
	}
	// Walk from head when it is a sha in the result (the Recorder passes
	// shas); else from the last commit of a compare (oldest first) or the
	// first of a commits listing (newest first).
	tip := head
	if _, ok := bySHA[tip]; !ok && len(commits) > 0 {
		tip = commits[len(commits)-1].SHA
		if base == "" {
			tip = commits[0].SHA
		}
	}
	var out []Commit
	for sha := tip; sha != ""; {
		c, ok := bySHA[sha]
		if !ok {
			break
		}
		out = append(out, c.commit())
		sha = ""
		if len(c.Parents) > 0 {
			sha = c.Parents[0].SHA
		}
	}
	return out, complete, nil
}

// Commit reads one commit.
func (g GitHub) Commit(ctx context.Context, sha string) (Commit, error) {
	var c compareCommit
	if err := g.API.Get(ctx, fmt.Sprintf("repos/%s/commits/%s", g.Repo, url.PathEscape(sha)), &c); err != nil {
		return Commit{}, err
	}
	return c.commit(), nil
}

func (c compareCommit) commit() Commit {
	subject, body, _ := strings.Cut(c.Commit.Message, "\n")
	return Commit{SHA: c.SHA, Subject: subject, Body: strings.TrimSpace(body), Date: c.Commit.Committer.Date}
}

// PullForCommit finds the merged PR that a commit belongs to.
func (g GitHub) PullForCommit(ctx context.Context, sha string) (PR, bool, error) {
	var resp []struct {
		Number   int    `json:"number"`
		HTMLURL  string `json:"html_url"`
		Title    string `json:"title"`
		MergedAt string `json:"merged_at"`
	}
	if err := g.API.Get(ctx, fmt.Sprintf("repos/%s/commits/%s/pulls", g.Repo, sha), &resp); err != nil {
		return PR{}, false, err
	}
	for _, p := range resp {
		if p.MergedAt != "" {
			return PR{Number: p.Number, URL: p.HTMLURL, Title: p.Title, MergedAt: p.MergedAt}, true, nil
		}
	}
	return PR{}, false, nil
}

// Pull reads one pull request; ok is false for one the repo does not have.
func (g GitHub) Pull(ctx context.Context, number int) (PR, bool, error) {
	var resp struct {
		Number   int    `json:"number"`
		HTMLURL  string `json:"html_url"`
		Title    string `json:"title"`
		MergedAt string `json:"merged_at"`
		Head     struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := g.API.Get(ctx, fmt.Sprintf("repos/%s/pulls/%d", g.Repo, number), &resp); err != nil {
		return PR{}, false, err
	}
	if resp.Number == 0 {
		return PR{}, false, nil
	}
	return PR{Number: resp.Number, URL: resp.HTMLURL, Title: resp.Title, MergedAt: resp.MergedAt, HeadRef: resp.Head.Ref}, true, nil
}

// BuiltAt returns when a successful push run of workflow (e.g. images.yml)
// for tag at sha finished, "" when there is none yet. It is the same test
// release-deploy.sh's "images ready" gate makes.
func (g GitHub) BuiltAt(ctx context.Context, workflow, tag, sha string) (string, error) {
	var resp struct {
		WorkflowRuns []struct {
			HeadSHA    string `json:"head_sha"`
			Conclusion string `json:"conclusion"`
			UpdatedAt  string `json:"updated_at"`
		} `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/%s/actions/workflows/%s/runs?branch=%s&event=push", g.Repo, workflow, url.QueryEscape(tag))
	if err := g.API.Get(ctx, path, &resp); err != nil {
		return "", err
	}
	latest := ""
	for _, r := range resp.WorkflowRuns {
		if r.Conclusion == "success" && r.HeadSHA == sha && r.UpdatedAt > latest {
			latest = r.UpdatedAt
		}
	}
	return latest, nil
}
