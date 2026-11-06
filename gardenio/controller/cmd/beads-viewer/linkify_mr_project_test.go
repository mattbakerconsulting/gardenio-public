package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadShow_MRRef_SingleRepoProject_UsesProjectRepo proves the primary
// defect (ka-0000007929): a bare `!N` MR ref on a bead belonging to a
// SINGLE-repo project is linkified against the process-wide default GitLab
// project URL (Acme/Platform/gardenio) instead of that project's own
// repo. Every non-gardenio single-repo project — glm-lab (oss-inference),
// identity-fabric (identity-fabric-deployment), load-testing, devsecops
// (monorepo) — therefore gets an MR link into the WRONG repo.
//
// The multi-repo gate (ka-0000004231) only suppresses linkification when a
// project has >1 repo; a single-repo project sails straight through to the
// hardcoded default.
func TestBeadShow_MRRef_SingleRepoProject_UsesProjectRepo(t *testing.T) {
	cases := []struct {
		name    string
		project string
		info    beadsapi.ProjectInfo
		wantURL string
	}{
		{
			name:    "glm-lab resolves to oss-inference",
			project: "glm-lab",
			info: beadsapi.ProjectInfo{
				Name:   "glm-lab",
				GitURL: "https://gitlab.com/Acme/Platform/oss-inference.git",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://gitlab.com/Acme/Platform/oss-inference.git", Role: "primary"},
				},
			},
			wantURL: "https://gitlab.com/Acme/Platform/oss-inference/-/merge_requests/1132",
		},
		{
			name:    "devsecops resolves to monorepo",
			project: "devsecops",
			info: beadsapi.ProjectInfo{
				Name:   "devsecops",
				GitURL: "https://gitlab.com/Acme/Platform/monorepo.git",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://gitlab.com/Acme/Platform/monorepo.git", Role: "primary"},
				},
			},
			wantURL: "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/1132",
		},
		{
			name:    "gardenio still resolves to gardenio",
			project: "gardenio",
			info: beadsapi.ProjectInfo{
				Name:   "gardenio",
				GitURL: "https://gitlab.com/Acme/Platform/gardenio.git",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://gitlab.com/Acme/Platform/gardenio.git", Role: "primary"},
				},
			},
			wantURL: "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/1132",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			id := "gd-mrproj-" + tc.project
			f.beads[id] = &beadsapi.BeadDetail{
				ID:          id,
				Title:       "MR ref bead",
				Type:        "task",
				Status:      "open",
				Labels:      []string{"project:" + tc.project},
				Description: "See !1132 for the fix.",
			}
			f.projectsResp = map[string]beadsapi.ProjectInfo{tc.project: tc.info}

			mux := mkServer(t, f)
			req := httptest.NewRequest("GET", "/"+id, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, `href="`+tc.wantURL+`"`) {
				t.Errorf("project %q: bare !1132 should link to %s\nbody excerpt: %s",
					tc.project, tc.wantURL, mrExcerpt(body))
			}
		})
	}
}

// TestPartials_MRRef_HonorProjectRepo proves the second defect: the
// project-aware linkifier options are resolved ONLY in handleBeadShow. Every
// htmx partial that re-renders the same content — GET /b/{id}/description,
// GET /b/{id}/title, GET /b/{id}/comments — calls the zero-opts Linkify*
// entry points, so a fragment swap silently rewrites a bare `!N` against the
// hardcoded default even for a multi-repo project the full page correctly
// left as plain text. The initial render and the swapped fragment disagree.
func TestPartials_MRRef_HonorProjectRepo(t *testing.T) {
	const id = "gd-partial-mr"
	newFixture := func() *fakeDaemon {
		f := newFake()
		f.beads[id] = &beadsapi.BeadDetail{
			ID:          id,
			Title:       "Follow-up on !1132",
			Type:        "task",
			Status:      "open",
			Labels:      []string{"project:monorepo"},
			Description: "See !1132 for the fix.",
		}
		f.comments[id] = []beadsapi.Comment{{ID: 1, Author: "a@b.c", Text: "reviewed !1132"}}
		f.projectsResp = map[string]beadsapi.ProjectInfo{"monorepo": {
			Name:   "monorepo",
			GitURL: "https://gitlab.com/Acme/Platform/monorepo.git",
			Repos: []beadsapi.RepoEntry{
				{URL: "https://gitlab.com/Acme/Platform/monorepo.git", Role: "primary"},
				{URL: "https://gitlab.com/Acme/Platform/gardenio.git", Role: "reference"},
				{URL: "https://gitlab.com/Acme/Platform/site-deployment.git", Role: "reference"},
			},
		}}
		return f
	}

	// Baseline: the FULL page correctly declines to linkify (multi-repo gate).
	full := httptest.NewRecorder()
	mkServer(t, newFixture()).ServeHTTP(full, httptest.NewRequest("GET", "/"+id, nil))
	if full.Code != http.StatusOK {
		t.Fatalf("full page: expected 200, got %d", full.Code)
	}
	if strings.Contains(full.Body.String(), "merge_requests/1132") {
		t.Fatalf("precondition failed: full page linkified a bare !N on a multi-repo project")
	}

	for _, path := range []string{
		"/b/" + id + "/description",
		"/b/" + id + "/title",
		"/b/" + id + "/comments",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("HX-Request", "true")
			mkServer(t, newFixture()).ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			if strings.Contains(w.Body.String(), "merge_requests/1132") {
				t.Errorf("partial %s linkified bare !1132 against the default repo, "+
					"but the full page (same bead, multi-repo project) did not\nbody excerpt: %s",
					path, mrExcerpt(w.Body.String()))
			}
		})
	}
}

// TestPlanNarrative_MRRef_HonorsProjectRepo covers the same defect on the
// /plans narrative fragment: its prose went through the default linkifier, so
// a bare `!N` in a plan epic's description linked into gardenio regardless of
// which project owns the plan.
func TestPlanNarrative_MRRef_HonorsProjectRepo(t *testing.T) {
	const id = "gd-plan-mr"
	f := newFake()
	f.beads[id] = &beadsapi.BeadDetail{
		ID:          id,
		Title:       "Load-testing plan",
		Type:        "epic",
		Status:      "open",
		Labels:      []string{"project:load-testing", "plan"},
		Description: "Rollout tracked in !4210.",
	}
	f.projectsResp = map[string]beadsapi.ProjectInfo{"load-testing": {
		Name:   "load-testing",
		GitURL: "https://gitlab.com/Acme/Platform/load-testing.git",
		Repos: []beadsapi.RepoEntry{
			{URL: "https://gitlab.com/Acme/Platform/load-testing.git", Role: "primary"},
		},
	}}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/"+id+"/narrative", nil)
	req.Header.Set("HX-Request", "true")
	mkServer(t, f).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	const want = "https://gitlab.com/Acme/Platform/load-testing/-/merge_requests/4210"
	if !strings.Contains(w.Body.String(), `href="`+want+`"`) {
		t.Errorf("plan narrative should link !4210 to %s\nbody excerpt: %s", want, mrExcerpt(w.Body.String()))
	}
}

// TestLinkifyOptsForBead_CarriesProjectRepo asserts the resolver surfaces the
// project's own repo URL so replaceMRIIDs can target it. Single-repo projects
// get a concrete ProjectURL; multi-repo projects keep SkipMRRefs.
func TestLinkifyOptsForBead_CarriesProjectRepo(t *testing.T) {
	f := newFake()
	f.beads["gd-x"] = &beadsapi.BeadDetail{ID: "gd-x", Labels: []string{"project:glm-lab"}}
	f.projectsResp = map[string]beadsapi.ProjectInfo{"glm-lab": {
		Name:   "glm-lab",
		GitURL: "https://gitlab.com/Acme/Platform/oss-inference.git",
		Repos: []beadsapi.RepoEntry{
			{URL: "https://gitlab.com/Acme/Platform/oss-inference.git", Role: "primary"},
		},
	}}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	opts := srv.linkifyOptsForBead(context.Background(), f.beads["gd-x"])
	if opts.SkipMRRefs {
		t.Errorf("single-repo project should not skip MR refs")
	}
	if opts.ProjectURL != "https://gitlab.com/Acme/Platform/oss-inference" {
		t.Errorf("ProjectURL = %q, want the project's own repo URL", opts.ProjectURL)
	}
}

// mrExcerpt trims a rendered body down to the neighborhood of the first
// merge_requests href so failure output stays readable.
func mrExcerpt(body string) string {
	i := strings.Index(body, "merge_requests")
	if i < 0 {
		i = strings.Index(body, "!1132")
	}
	if i < 0 {
		return "(no MR ref found)"
	}
	start := max(i-200, 0)
	end := min(i+120, len(body))
	return body[start:end]
}
