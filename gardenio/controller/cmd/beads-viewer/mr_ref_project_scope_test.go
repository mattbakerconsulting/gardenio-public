package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The bare `!N` MR refs used across this file. 897 is the number Tim's report
// turned up on: it exists in BOTH gardenio and the project's real repo, so a
// wrong link is live and looks plausible rather than 404ing.
const mrRefScopeBare = "!897"

// TestMRRefProjectURL covers the single-repo resolution table directly, so the
// precedence rules are pinned independent of any handler wiring.
func TestMRRefProjectURL(t *testing.T) {
	cases := []struct {
		name string
		info beadsapi.ProjectInfo
		want string
	}{
		{
			name: "sole repo wins, .git stripped",
			info: beadsapi.ProjectInfo{Repos: []beadsapi.RepoEntry{{URL: "https://gitlab.com/org/monorepo.git"}}},
			want: "https://gitlab.com/org/monorepo",
		},
		{
			// devsecops' real shape — role=reference, no git_url. The role
			// must not gate resolution: one repo is one repo.
			name: "sole repo with role=reference still resolves",
			info: beadsapi.ProjectInfo{Repos: []beadsapi.RepoEntry{{URL: "https://gitlab.com/org/monorepo.git", Role: "reference"}}},
			want: "https://gitlab.com/org/monorepo",
		},
		{
			name: "sole repo beats git_url",
			info: beadsapi.ProjectInfo{
				GitURL: "https://gitlab.com/org/stale.git",
				Repos:  []beadsapi.RepoEntry{{URL: "https://gitlab.com/org/current.git"}},
			},
			want: "https://gitlab.com/org/current",
		},
		{
			name: "git_url used when repos[] is empty",
			info: beadsapi.ProjectInfo{GitURL: "https://gitlab.com/org/legacy.git"},
			want: "https://gitlab.com/org/legacy",
		},
		{
			name: "trailing slash trimmed",
			info: beadsapi.ProjectInfo{GitURL: "https://gitlab.com/org/legacy/"},
			want: "https://gitlab.com/org/legacy",
		},
		{
			name: "no repo declared resolves to empty (caller keeps the default)",
			info: beadsapi.ProjectInfo{Name: "bare"},
			want: "",
		},
		{
			// Multi-repo never reaches this helper in production
			// (linkifyOptsForBead short-circuits to SkipMRRefs), but a
			// caller that did must not get an arbitrary pick.
			name: "multi-repo resolves to empty, not an arbitrary entry",
			info: beadsapi.ProjectInfo{Repos: []beadsapi.RepoEntry{
				{URL: "https://gitlab.com/org/a.git", Role: "primary"},
				{URL: "https://gitlab.com/org/b.git", Role: "reference"},
			}},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mrRefProjectURL(tc.info); got != tc.want {
				t.Errorf("mrRefProjectURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// mrRefScopeFake builds a bead in the given project carrying a bare `!897`
// in whichever surface the caller asks for, plus the project bead that names
// the repo the ref must resolve against.
func mrRefScopeFake(t *testing.T, project string, info beadsapi.ProjectInfo) (*fakeDaemon, string) {
	t.Helper()
	f := newFake()
	id := "gd-mrscope"
	f.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  "MR ref scope",
		Kind:   "issue",
		Type:   "task",
		Status: "open",
		Labels: []string{"project:" + project},
	}
	f.projectsResp = map[string]beadsapi.ProjectInfo{project: info}
	return f, id
}

// TestBeadShow_MRRef_ChatPanel_UsesProjectScope is the ka-0000005612 defect-B
// regression. The bead-show chat panel rendered message-bead bodies through a
// bare LinkifyText, so it could never see SkipMRRefs or a resolved project
// URL: every `!N` in a chat line linked at the hardcoded gardenio default,
// including on `project:monorepo` beads — the exact "monorepo bead links to
// gardenio" symptom that was reported.
func TestBeadShow_MRRef_ChatPanel_UsesProjectScope(t *testing.T) {
	cases := []struct {
		name string
		info beadsapi.ProjectInfo
		// wantHref is the href the chat line's !897 must carry, or "" when
		// the ref must render as plain text.
		wantHref string
	}{
		{
			name: "multi-repo project leaves chat MR refs as plain text",
			info: beadsapi.ProjectInfo{Repos: []beadsapi.RepoEntry{
				{URL: "https://gitlab.com/org/monorepo.git", Role: "reference"},
				{URL: "https://gitlab.com/org/gardenio.git", Role: "reference"},
			}},
			wantHref: "",
		},
		{
			name:     "single-repo project scopes chat MR refs to its own repo",
			info:     beadsapi.ProjectInfo{Repos: []beadsapi.RepoEntry{{URL: "https://gitlab.com/org/monorepo.git", Role: "reference"}}},
			wantHref: "https://gitlab.com/org/monorepo/-/merge_requests/897",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, id := mrRefScopeFake(t, "proj", tc.info)

			// A message bead mentioned by the host bead becomes a chat line.
			msgID := "gd-mrscope-msg"
			f.beads[msgID] = &beadsapi.BeadDetail{
				ID:     msgID,
				Title:  "squawk",
				Kind:   "config",
				Type:   "message",
				Status: "open",
				Fields: map[string]string{"text": "landed in " + mrRefScopeBare + " just now"},
			}
			f.mentions = map[string][]beadsapi.BeadReferenceSummary{
				id: {{ID: msgID, Type: "message"}},
			}

			mux := mkServer(t, f)
			req := httptest.NewRequest("GET", "/"+id, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()

			if !strings.Contains(body, mrRefScopeBare) {
				t.Fatalf("chat line body missing — the panel did not render, so this test proves nothing\nbody: %s", body)
			}
			assertMRRefHref(t, body, tc.wantHref)
		})
	}
}

// TestBeadComments_MRRef_PollMatchesSSR is the ka-0000005612 defect-C
// regression. handleBeadComments (GET /b/{id}/comments) rendered comments
// WITHOUT the per-bead linkifier opts the full-page render uses, so a bead's
// comments were correct on first paint and then silently degraded to gardenio
// links when the 30s htmx poll swapped in. The two must agree.
func TestBeadComments_MRRef_PollMatchesSSR(t *testing.T) {
	cases := []struct {
		name     string
		info     beadsapi.ProjectInfo
		wantHref string
	}{
		{
			name: "multi-repo project: both surfaces leave the ref as plain text",
			info: beadsapi.ProjectInfo{Repos: []beadsapi.RepoEntry{
				{URL: "https://gitlab.com/org/monorepo.git", Role: "reference"},
				{URL: "https://gitlab.com/org/gardenio.git", Role: "reference"},
			}},
			wantHref: "",
		},
		{
			name:     "single-repo project: both surfaces scope to its own repo",
			info:     beadsapi.ProjectInfo{Repos: []beadsapi.RepoEntry{{URL: "https://gitlab.com/org/monorepo.git", Role: "reference"}}},
			wantHref: "https://gitlab.com/org/monorepo/-/merge_requests/897",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, id := mrRefScopeFake(t, "proj", tc.info)
			f.comments[id] = []beadsapi.Comment{{
				ID:     1,
				Author: "someone",
				Text:   "fixed by " + mrRefScopeBare,
			}}

			mux := mkServer(t, f)

			get := func(path string) string {
				t.Helper()
				req := httptest.NewRequest("GET", path, nil)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s: expected 200, got %d", path, w.Code)
				}
				return w.Body.String()
			}

			ssr := get("/" + id)
			poll := get("/b/" + id + "/comments")

			for _, s := range []struct {
				label string
				body  string
			}{{"SSR page", ssr}, {"comments poll", poll}} {
				if !strings.Contains(s.body, mrRefScopeBare) {
					t.Fatalf("%s: comment text missing — nothing was asserted\nbody: %s", s.label, s.body)
				}
				t.Run(s.label, func(t *testing.T) { assertMRRefHref(t, s.body, tc.wantHref) })
			}
		})
	}
}

// assertMRRefHref checks that body linkifies the bare !897 to wantHref, or
// leaves it unlinked when wantHref is empty. It always fails a leak to the
// hardcoded gardenio default, which is the reported symptom.
func assertMRRefHref(t *testing.T, body, wantHref string) {
	t.Helper()
	if wantHref == "" {
		if strings.Contains(body, "/-/merge_requests/897") {
			t.Errorf("bare !897 should not be linkified at all\nbody: %s", body)
		}
		return
	}
	if !strings.Contains(body, `href="`+wantHref+`"`) {
		t.Errorf("bare !897 should link to %q\nbody: %s", wantHref, body)
	}
	if !strings.Contains(wantHref, "gardenio") && strings.Contains(body, "gardenio/-/merge_requests/897") {
		t.Errorf("bare !897 leaked to the hardcoded gardenio default\nbody: %s", body)
	}
}
