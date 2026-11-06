package main

// GET /releases       — the releases of each project with a release source,
//                       newest first (plan gd-UZKZ9H25UD step 8,
//                       gardenio/docs/releases-design.md §3.5).
// GET /release/{id}   — one release: header, contents by plan and step,
//                       deployments, previous and next. {id} is the release's
//                       bead id, or its tag with ?project=.
//
// The pages carry words, not data: the handler reads only what the page's
// frame needs (which projects have releases, the release's tag for the
// title, the project's repository for the sha link), and
// templates/static/releases.js fills the rest from the JSON routes
// GET /api/releases?project= and GET /api/releases/{id} (step 6). A later
// /garden release view reads the same JSON and reuses the template's words.
//
// Beside the pages: the bead view of a merged-MR bead, a step or a plan says
// "Shipped in <tag>" (releaseShippedIn), the front page has a "Latest
// release" tile per project (latestReleaseTiles), and the /health Build &
// release tile links the running build to its release (runningReleaseHref).
// Every read is on the request's context, so the daemon narrows it to what
// the signed-in user may read; none of it is cached across principals.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// releaseOwnProject is the project whose releases this viewer's own build
// comes from: the /health Build & release tile links `version` to its
// release in it.
const releaseOwnProject = "gardenio"

// releaseShippedLookback is how many of a project's newest releases the
// "Shipped in" line searches. Thirty is the backfill's depth (§3.4).
const releaseShippedLookback = 30

// releaseContentRow is the part of a release contents row (§3.1,
// internal/releaserecord.Row) the viewer's Go side reads.
type releaseContentRow struct {
	PR       int    `json:"pr"`
	Train    int    `json:"train"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	MergedAt string `json:"merged_at"`
	MRBead   string `json:"mr_bead"`
	Task     string `json:"task"`
	Step     string `json:"step"`
	Plan     string `json:"plan"`
}

func releaseContents(r beadsapi.Release) []releaseContentRow {
	if len(r.Contents) == 0 {
		return nil
	}
	var rows []releaseContentRow
	if err := json.Unmarshal(r.Contents, &rows); err != nil {
		return nil
	}
	return rows
}

// releaseHref is the Release page of r.
func releaseHref(basePath string, r beadsapi.Release) string {
	return basePath + "/release/" + url.PathEscape(r.ID)
}

// releaseProjectView is one project section of /releases.
type releaseProjectView struct {
	Name    string
	Targets []string
}

// handleReleases renders /releases: one section per project with a release
// source (?project= narrows it to one), each filled by releases.js.
func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	only := strings.TrimSpace(r.URL.Query().Get("project"))
	data := map[string]any{"Projects": []releaseProjectView{}, "Only": only}
	projects, err := s.daemon.ListProjectBeads(r.Context())
	if err != nil {
		s.logger.Error("listing projects for releases", "error", err)
		data["Error"] = "Failed to load the project list."
		s.render(w, r, "releases.html", data)
		return
	}
	var views []releaseProjectView
	for name, p := range projects {
		if p.ReleaseSource == nil || (only != "" && name != only) {
			continue
		}
		v := releaseProjectView{Name: name}
		for _, t := range p.ReleaseSource.Targets {
			v.Targets = append(v.Targets, t.Name)
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	if views != nil {
		data["Projects"] = views
	}
	s.render(w, r, "releases.html", data)
}

// handleRelease renders /release/{id}. It reads the release once, for the
// title and a real 404, and leaves the body to releases.js.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	rel, err := s.daemon.GetRelease(r.Context(), id, project)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusForbidden) {
			http.Error(w, "Release not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting release", "id", id, "error", err)
		http.Error(w, "Failed to get release", http.StatusInternalServerError)
		return
	}
	repo := ""
	if projects, err := s.daemon.ListProjectBeads(r.Context()); err == nil {
		repo = githubRepoURL(projects[rel.Project].GitURL)
	}
	s.render(w, r, "release.html", map[string]any{
		"Release": rel,
		"RepoURL": repo,
	})
}

// githubRepoURL turns a project's git_url into the https URL of its GitHub
// repository, for the sha link; empty for a repository that is not on
// github.com.
func githubRepoURL(gitURL string) string {
	u := strings.TrimSpace(gitURL)
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	switch {
	case strings.HasPrefix(u, "git@github.com:"):
		return "https://github.com/" + strings.TrimPrefix(u, "git@github.com:")
	case strings.HasPrefix(u, "https://github.com/"):
		return u
	}
	return ""
}

// releaseShippedView is one release on a bead's "Shipped in" line. Count is
// how many of the release's PRs belong to the bead (a plan's line counts
// its steps' PRs). Open is the project's open release, which a merged PR
// boards before the cut: the line says "on the next release (open)".
type releaseShippedView struct {
	Tag   string
	Href  string
	Count int
	Open  bool
}

// releaseShippedIn returns the releases that carry bead, oldest first and the
// open release last: a
// merged-MR bead by its id or PR url, a task or step by the rows it
// produced, a plan by every row of its steps. Nil when the bead is not
// shipped, has no project, or the read fails (the line is a nicety, never an
// error on the page). Open beads other than plans are skipped without a
// read: only a plan ships while still open.
func (s *Server) releaseShippedIn(ctx context.Context, bead *beadsapi.BeadDetail) []releaseShippedView {
	if bead == nil {
		return nil
	}
	project := projectFromLabels(bead.Labels)
	isPlan := bead.Type == "epic" && hasLabel(bead, planLabel)
	if project == "" || (bead.Status != "closed" && !isPlan) {
		return nil
	}
	releases, err := s.daemon.ListReleases(ctx, project, "", releaseShippedLookback)
	if err != nil {
		return nil
	}
	mrURL := strings.TrimSpace(bead.Fields["mr_url"])
	var out []releaseShippedView
	for _, rel := range releases {
		n := 0
		for _, row := range releaseContents(rel) {
			if row.MRBead == bead.ID || row.Task == bead.ID || row.Step == bead.ID || row.Plan == bead.ID ||
				(mrURL != "" && row.URL == mrURL) {
				n++
			}
		}
		if n > 0 {
			out = append(out, releaseShippedView{Tag: rel.Tag, Href: releaseHref(s.basePath, rel), Count: n, Open: rel.Tag == ""})
		}
	}
	// ListReleases is the open release, then newest cut first; the line reads
	// in the order they shipped.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// latestReleaseTile is one project's "Latest release" tile on the front page.
type latestReleaseTile struct {
	Project string
	Tag     string
	Status  string
	Href    string
	CutAgo  string
	RunsOn  []string
	PRs     int
}

// latestReleaseTiles reads the newest release of every project with a release
// source, one read per project in parallel. A project with no release yet, or
// whose read fails, has no tile.
func (s *Server) latestReleaseTiles(ctx context.Context, projects map[string]beadsapi.ProjectInfo, now time.Time) []latestReleaseTile {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		tiles []latestReleaseTile
	)
	for name, p := range projects {
		if p.ReleaseSource == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The open release is listed first: read one more and skip it.
			rels, err := s.daemon.ListReleases(ctx, name, "", 2)
			rels = beadsapi.CutReleases(rels)
			if err != nil || len(rels) == 0 {
				return
			}
			rel := rels[0]
			t := latestReleaseTile{
				Project: name,
				Tag:     rel.Tag,
				Status:  releaseStatusWord(rel.Status),
				Href:    releaseHref(s.basePath, rel),
				RunsOn:  rel.RunsOn(),
				PRs:     len(releaseContents(rel)),
			}
			if cut, err := time.Parse(time.RFC3339, rel.CutAt); err == nil {
				t.CutAgo = fmtTimeAgo(cut, now)
			}
			mu.Lock()
			tiles = append(tiles, t)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(tiles, func(i, j int) bool { return tiles[i].Project < tiles[j].Project })
	return tiles
}

// releaseStatusWord maps a release status to the word the surfaces show; an
// unknown word reads as cut (step 6's ruling).
func releaseStatusWord(status string) string {
	switch status {
	case beadsapi.ReleaseStatusBuilt, beadsapi.ReleaseStatusLive, beadsapi.ReleaseStatusSuperseded:
		return status
	}
	return beadsapi.ReleaseStatusCut
}

// runningReleaseHref links the running build's version to its release in
// releaseOwnProject; empty for a dev build or a version with no release.
func (s *Server) runningReleaseHref(ctx context.Context) string {
	if _, ok := parseCalverDate(version); !ok {
		return ""
	}
	rel, err := s.daemon.GetRelease(ctx, version, releaseOwnProject)
	if err != nil || rel == nil || rel.ID == "" {
		return ""
	}
	return releaseHref(s.basePath, *rel)
}
