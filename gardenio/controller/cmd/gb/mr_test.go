package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

func TestBaseURLFromGitLabURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "gitlab.com MR URL",
			in:   "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/5414",
			want: "https://gitlab.com",
		},
		{
			name: "self-hosted MR URL",
			in:   "https://gitlab-dark.gardenio-e2e.app.e2e.dev.acme.example/org/repo/-/merge_requests/12",
			want: "https://gitlab-dark.gardenio-e2e.app.e2e.dev.acme.example",
		},
		{
			name: "pipeline URL",
			in:   "https://gitlab.com/org/repo/-/pipelines/12345",
			want: "https://gitlab.com",
		},
		{
			name: "http scheme preserved",
			in:   "http://localhost:8080/org/repo/-/merge_requests/1",
			want: "http://localhost:8080",
		},
		{
			name: "bead id is not a URL",
			in:   "ka-0000008229",
			want: "",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "non-http scheme rejected",
			in:   "ssh://git@gitlab.com/org/repo.git",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := baseURLFromGitLabURL(tc.in); got != tc.want {
				t.Errorf("baseURLFromGitLabURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRunMRStatus_DerivesBaseURLFromMRURL is the end-to-end regression guard
// for bug ka-0000008229: with no --gitlab-url/GITLAB_BASE_URL set, `gb mr
// status <mr-url>` must derive the GitLab base URL from the MR URL host and
// make the real API call, instead of hard-erroring on missing config.
func TestRunMRStatus_DerivesBaseURLFromMRURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bridge.GitLabMR{
			IID: 42, Title: "derive base URL", State: "opened",
			WebURL: "https://example/mr/42",
		})
	}))
	defer srv.Close()

	// The MR URL host IS the httptest server, so a correctly-derived base URL
	// routes the API call to srv. Any residual config would point elsewhere.
	origURL, origToken, origJSON := mrGitLabURL, mrGitLabToken, jsonOutput
	t.Cleanup(func() { mrGitLabURL, mrGitLabToken, jsonOutput = origURL, origToken, origJSON })
	mrGitLabURL = ""
	mrGitLabToken = "tok"
	jsonOutput = false

	mrURL := srv.URL + "/owner/repo/-/merge_requests/42"

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)

	if err := runMRStatus(cmd, []string{mrURL}); err != nil {
		t.Fatalf("runMRStatus with derived base URL: %v", err)
	}
	// net/http decodes %2F in the request path, so the project path reads as
	// "owner/repo". The point is that the call reached srv at all (proving the
	// base URL was derived from the MR URL host) and hit the MR endpoint.
	if want := "/api/v4/projects/owner/repo/merge_requests/42"; gotPath != want {
		t.Errorf("API path = %q, want %q", gotPath, want)
	}
	if s := out.String(); !strings.Contains(s, "!42") || !strings.Contains(s, "derive base URL") {
		t.Errorf("output missing MR details: %q", s)
	}
}

func TestNewMRGitLabClientFallback(t *testing.T) {
	origURL, origToken := mrGitLabURL, mrGitLabToken
	t.Cleanup(func() { mrGitLabURL, mrGitLabToken = origURL, origToken })
	mrGitLabToken = "tok"

	// No explicit URL, no fallback → error.
	mrGitLabURL = ""
	if _, err := newMRGitLabClient(); err == nil {
		t.Errorf("expected error with no URL and no fallback")
	}

	// No explicit URL, empty fallbacks → still error.
	if _, err := newMRGitLabClient("", ""); err == nil {
		t.Errorf("expected error with no URL and only empty fallbacks")
	}

	// No explicit URL, but a derived fallback → uses fallback.
	c, err := newMRGitLabClient("https://gitlab.com")
	if err != nil {
		t.Fatalf("unexpected error with fallback: %v", err)
	}
	if got := c.Host(); got != "gitlab.com" {
		t.Errorf("fallback host = %q, want gitlab.com", got)
	}

	// First non-empty fallback wins.
	c, err = newMRGitLabClient("", "https://gitlab.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.Host(); got != "gitlab.example.com" {
		t.Errorf("host = %q, want gitlab.example.com", got)
	}

	// Explicit --gitlab-url/env wins over any fallback.
	mrGitLabURL = "https://explicit.gitlab.internal"
	c, err = newMRGitLabClient("https://gitlab.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.Host(); got != "explicit.gitlab.internal" {
		t.Errorf("explicit URL should win, host = %q, want explicit.gitlab.internal", got)
	}
}

// TestResolveMRGitLabToken is the regression guard for bug ka-0000003680: agent
// pods carry GITLAB_TOKEN (the token glab uses), not GITLAB_API_TOKEN, so the
// old os.Getenv("GITLAB_API_TOKEN")-only flag default resolved to "" and every
// `gb mr` call against Platform/monorepo 404'd. The resolver must fall through
// flag -> GITLAB_API_TOKEN -> GITLAB_TOKEN so GITLAB_TOKEN is honored.
func TestResolveMRGitLabToken(t *testing.T) {
	origToken := mrGitLabToken
	t.Cleanup(func() { mrGitLabToken = origToken })

	tests := []struct {
		name     string
		flag     string
		apiToken string
		token    string
		want     string
	}{
		{name: "flag wins over both env vars", flag: "flagtok", apiToken: "apitok", token: "gltok", want: "flagtok"},
		{name: "GITLAB_API_TOKEN when flag unset", flag: "", apiToken: "apitok", token: "gltok", want: "apitok"},
		{name: "GITLAB_TOKEN fallback when only it is set", flag: "", apiToken: "", token: "gltok", want: "gltok"},
		{name: "flag wins even when only flag set", flag: "flagtok", apiToken: "", token: "", want: "flagtok"},
		{name: "empty when nothing set", flag: "", apiToken: "", token: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mrGitLabToken = tc.flag
			t.Setenv("GITLAB_API_TOKEN", tc.apiToken)
			t.Setenv("GITLAB_TOKEN", tc.token)
			if got := resolveMRGitLabToken(); got != tc.want {
				t.Errorf("resolveMRGitLabToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunMRList_IncludesNonTaskTypes is the regression guard for bug
// ka-0000005054: `gb mr list` must surface mr_url-bearing beads of every issue
// type (bug/feature/epic), not just type=task. Before the fix, runMRList
// hard-coded Types:["task"] in the daemon query, so a type=bug bead with an
// mr_url was dropped before filterMRBeads (the real mr_url selector) ever saw
// it. This test stands up a mock daemon returning a task + a bug + a feature,
// each with mr_url, and asserts (a) the query widens Types to include
// bug/feature/epic and (b) all three beads appear in the JSON output.
func TestRunMRList_IncludesNonTaskTypes(t *testing.T) {
	var gotTypeParam, gotMRStatusParam string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		typeParam := r.URL.Query().Get("type")
		w.Header().Set("Content-Type", "application/json")
		// runMRList issues two queries: issue types, then the canonical
		// merge-request beads for the state join (bug ka-0000007109).
		if strings.Contains(typeParam, "merge-request") {
			gotMRStatusParam = r.URL.Query().Get("status")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id":     "gd-mr1",
						"title":  "!1",
						"type":   "merge-request",
						"status": "open",
						"fields": map[string]string{
							"mr_url":   "https://gitlab/x/-/merge_requests/1",
							"mr_state": "opened",
						},
					},
					{
						"id":     "gd-mr2",
						"title":  "!2",
						"type":   "merge-request",
						"status": "closed",
						"fields": map[string]string{
							"mr_url":     "https://gitlab/x/-/merge_requests/2",
							"mr_state":   "merged",
							"mr_outcome": "merged",
						},
					},
				},
				"total": 2,
			})
			return
		}
		gotTypeParam = typeParam
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id":     "gd-task1",
					"title":  "a task MR",
					"type":   "task",
					"status": "in_progress",
					"fields": map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/1"},
				},
				{
					"id":     "gd-bug1",
					"title":  "a bug MR",
					"type":   "bug",
					"status": "open",
					"fields": map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/2"},
				},
				{
					"id":     "gd-feat1",
					"title":  "a feature MR",
					"type":   "feature",
					"status": "open",
					"fields": map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/3"},
				},
			},
			"total": 3,
		})
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon, oldJSON, oldOwner, oldSpawnedBy := daemon, jsonOutput, mrListOwner, mrListSpawnedBy
	daemon = c
	jsonOutput = true
	mrListOwner, mrListSpawnedBy = "", ""
	t.Cleanup(func() {
		daemon, jsonOutput, mrListOwner, mrListSpawnedBy = oldDaemon, oldJSON, oldOwner, oldSpawnedBy
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	// runMRList prints JSON via printJSON, which writes to os.Stdout, so capture
	// there rather than cmd.SetOut.
	raw := captureStdout(t, func() error { return runMRList(cmd, nil) })

	// The daemon query must widen Types beyond task so bug/feature/epic beads
	// are not dropped before filterMRBeads sees them.
	for _, want := range []string{"task", "bug", "feature", "epic"} {
		if !strings.Contains(gotTypeParam, want) {
			t.Errorf("query type param %q missing %q", gotTypeParam, want)
		}
	}

	var got []mrBead
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", string(raw), err)
	}
	gotByID := map[string]mrBead{}
	for _, b := range got {
		gotByID[b.ID] = b
	}
	for _, want := range []string{"gd-task1", "gd-bug1", "gd-feat1"} {
		if _, ok := gotByID[want]; !ok {
			t.Errorf("expected %s in mr list output, got %+v", want, got)
		}
	}

	// The MR-bead query must include closed beads: merged MRs' beads are
	// status=closed and would otherwise never join.
	if !strings.Contains(gotMRStatusParam, "closed") {
		t.Errorf("merge-request query status param %q missing \"closed\"", gotMRStatusParam)
	}

	// State joined from the canonical MR beads (ka-0000007109): gd-task1's MR
	// is opened, gd-bug1's is merged, gd-feat1 has no MR bead (blank state).
	if b := gotByID["gd-task1"]; b.MRState != "opened" || b.MRMerged != "" {
		t.Errorf("gd-task1 state/merged = %q/%q, want opened/\"\"", b.MRState, b.MRMerged)
	}
	if b := gotByID["gd-bug1"]; b.MRState != "merged" || b.MRMerged != "true" {
		t.Errorf("gd-bug1 state/merged = %q/%q, want merged/true", b.MRState, b.MRMerged)
	}
	if b := gotByID["gd-feat1"]; b.MRState != "" || b.MRMerged != "" {
		t.Errorf("gd-feat1 state/merged = %q/%q, want blank/blank", b.MRState, b.MRMerged)
	}
}

// TestRunMRList_PushesMRURLSelectorServerSide is the regression guard for bug
// ka-0000001936: runMRList must push the mr_url selector to the DAEMON
// (field=mr_url=*) so the row cap applies to the mr_url-BEARING population, not
// the ~20× larger open/in_progress issue set. Before the fix it fetched a
// Limit:500 slice of that superset and applied `mr_url != ""` client-side in
// filterMRBeads afterward, so the daemon spent its cap on the ~93% of beads with
// no mr_url and silently dropped MR-bearing beads that sorted past the boundary
// (e.g. the real ka-0000003829). This mock asserts the issue query (a) carries
// the field=mr_url=* presence selector and (b) does NOT ride the old fixed
// Limit=500 — the two halves of the original hazard.
func TestRunMRList_PushesMRURLSelectorServerSide(t *testing.T) {
	var gotIssueQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Query().Get("type"), "merge-request") {
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{}, "total": 0})
			return
		}
		gotIssueQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-mrbearing", "title": "an MR-bearing task",
					"type": "task", "status": "open",
					"fields": map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/1"},
				},
			},
			"total": 1,
		})
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon, oldJSON, oldOwner, oldSpawnedBy := daemon, jsonOutput, mrListOwner, mrListSpawnedBy
	daemon = c
	jsonOutput = true
	mrListOwner, mrListSpawnedBy = "", ""
	t.Cleanup(func() {
		daemon, jsonOutput, mrListOwner, mrListSpawnedBy = oldDaemon, oldJSON, oldOwner, oldSpawnedBy
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	raw := captureStdout(t, func() error { return runMRList(cmd, nil) })

	// The mr_url selector must run SERVER-SIDE — field=mr_url=* url-encodes as
	// field=mr_url%3D%2A.
	if !strings.Contains(gotIssueQuery, "field=mr_url%3D%2A") {
		t.Errorf("issue query must push the mr_url presence selector server-side (field=mr_url=*), got %q", gotIssueQuery)
	}
	// It must NOT ride the old fixed 500 cap; the paged walk sends the daemon's
	// max page size instead so the cap applies to the relevant population.
	if strings.Contains(gotIssueQuery, "limit=500") {
		t.Errorf("issue query still carries the old fixed limit=500 cap, got %q", gotIssueQuery)
	}

	var got []mrBead
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", string(raw), err)
	}
	if len(got) != 1 || got[0].ID != "gd-mrbearing" {
		t.Errorf("expected the MR-bearing bead in output, got %+v", got)
	}
}

// TestFindMRBeadByURL_PushesMRURLSelectorServerSide is the regression guard for
// bug ka-0000005190 on the `gb mr status`/`gb mr skip` path: findMRBeadByURL
// must push field=mr_url=<url> to the daemon instead of scanning a Limit:500
// page of every merge-request bead. With 6834 merge-request beads fleet-wide
// the old scan capped at 500 (created_at DESC), so `gb mr status` against an MR
// whose bead sorted past that window failed with a bogus "no merge-request bead
// found". This mock asserts the request carries the server-side equality
// selector, drops the Limit:500 cap, keeps closed reachable, and surfaces the
// single matched bead id.
func TestFindMRBeadByURL_PushesMRURLSelectorServerSide(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/42"
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{{
				"id": "gd-mr-old", "type": "merge-request", "status": "closed",
				"fields": map[string]string{"mr_url": mrURL, "mr_outcome": "merged"},
			}},
			"total": 1,
		})
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	id, err := findMRBeadByURL(cmd, mrURL)
	if err != nil {
		t.Fatalf("findMRBeadByURL: %v", err)
	}
	if id != "gd-mr-old" {
		t.Fatalf("id=%q, want gd-mr-old", id)
	}

	// field=mr_url=<url> url-encodes as field=mr_url%3D<escaped-url>.
	if !strings.Contains(gotQuery, "field=mr_url%3D") {
		t.Errorf("query must push the mr_url equality selector server-side (field=mr_url=<url>), got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "limit=500") {
		t.Errorf("query still carries the old fixed limit=500 scan, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "closed") {
		t.Errorf("query must keep status=closed reachable (merged MR beads are closed), got %q", gotQuery)
	}
}

// TestFindMRBeadByURL_NoMatchErrors pins that an unlinked URL still surfaces the
// "is the MR linked via gb mr link?" error rather than silently succeeding —
// preserved even though the daemon now filters server-side.
func TestFindMRBeadByURL_NoMatchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{}, "total": 0})
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if _, err := findMRBeadByURL(cmd, "https://gitlab.com/org/repo/-/merge_requests/999"); err == nil {
		t.Error("expected an error for an unlinked MR URL, got nil")
	}
}

func TestFilterMRBeads(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{
			ID:       "gd-A",
			Title:    "alice's MR",
			Assignee: "alice",
			Fields:   map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/1", "spawned_by": "gardener1"},
		},
		{
			ID:       "gd-B",
			Title:    "bob's MR",
			Assignee: "bob",
			Fields:   map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/2", "spawned_by": "gardener2"},
		},
		{
			ID:       "gd-C",
			Title:    "no mr",
			Assignee: "alice",
			Fields:   map[string]string{"spawned_by": "gardener1"},
		},
		{
			ID:       "gd-D",
			Title:    "spawned-by-alice",
			Assignee: "carol",
			Fields:   map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/3", "spawned_by": "alice"},
		},
	}

	tests := []struct {
		name      string
		owner     string
		spawnedBy string
		wantIDs   []string
	}{
		{name: "no filter", wantIDs: []string{"gd-A", "gd-B", "gd-D"}},
		{name: "owner=alice matches assignee or spawned_by", owner: "alice", wantIDs: []string{"gd-A", "gd-D"}},
		{name: "owner=bob assignee only", owner: "bob", wantIDs: []string{"gd-B"}},
		{name: "owner=carol assignee only, no spawned_by hit", owner: "carol", wantIDs: []string{"gd-D"}},
		{name: "owner=nobody", owner: "nobody", wantIDs: nil},
		{name: "spawned-by=gardener1", spawnedBy: "gardener1", wantIDs: []string{"gd-A"}},
		{name: "spawned-by=alice", spawnedBy: "alice", wantIDs: []string{"gd-D"}},
		{name: "owner+spawnedBy AND-combined", owner: "alice", spawnedBy: "gardener1", wantIDs: []string{"gd-A"}},
		{name: "owner+spawnedBy AND no overlap", owner: "bob", spawnedBy: "gardener1", wantIDs: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterMRBeads(beads, nil, tc.owner, tc.spawnedBy)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %d beads, want %d: %+v", len(got), len(tc.wantIDs), got)
			}
			for i, id := range tc.wantIDs {
				if got[i].ID != id {
					t.Errorf("[%d] got id=%s, want %s", i, got[i].ID, id)
				}
			}
		})
	}
}

// TestFilterMRBeads_CanonicalMRJoin is the regression guard for bug
// ka-0000007109: STATE/MERGED must come from the canonical type=merge-request
// bead (keyed by mr_url), since the bridge no longer stamps mr_state/mr_merged
// on issue beads. Issue-bead fields remain a fallback for legacy rows.
func TestFilterMRBeads_CanonicalMRJoin(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{
			ID:     "gd-opened",
			Title:  "issue with opened MR",
			Fields: map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/1"},
		},
		{
			ID:     "gd-merged",
			Title:  "issue with merged MR",
			Fields: map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/2"},
		},
		{
			ID:     "gd-outcome",
			Title:  "issue whose MR bead has only mr_outcome",
			Fields: map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/3"},
		},
		{
			ID:    "gd-legacy",
			Title: "legacy issue-bead stamping, no MR bead",
			Fields: map[string]string{
				"mr_url":    "https://gitlab/x/-/merge_requests/4",
				"mr_state":  "opened",
				"mr_merged": "true",
			},
		},
		{
			ID:     "gd-none",
			Title:  "no MR bead, no legacy fields",
			Fields: map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/5"},
		},
	}
	mrBeadsByURL := map[string]*beadsapi.BeadDetail{
		"https://gitlab/x/-/merge_requests/1": {
			ID:     "gd-mr1",
			Fields: map[string]string{"mr_state": "opened"},
		},
		"https://gitlab/x/-/merge_requests/2": {
			ID:     "gd-mr2",
			Fields: map[string]string{"mr_state": "merged", "mr_merged": "true"},
		},
		"https://gitlab/x/-/merge_requests/3": {
			ID:     "gd-mr3",
			Fields: map[string]string{"mr_outcome": "merged"},
		},
	}

	got := filterMRBeads(beads, mrBeadsByURL, "", "")
	byID := map[string]mrBead{}
	for _, b := range got {
		byID[b.ID] = b
	}

	tests := []struct {
		id         string
		wantState  string
		wantMerged string
	}{
		{id: "gd-opened", wantState: "opened", wantMerged: ""},
		{id: "gd-merged", wantState: "merged", wantMerged: "true"},
		{id: "gd-outcome", wantState: "", wantMerged: "true"},
		{id: "gd-legacy", wantState: "opened", wantMerged: "true"},
		{id: "gd-none", wantState: "", wantMerged: ""},
	}
	for _, tc := range tests {
		b, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s missing from output", tc.id)
			continue
		}
		if b.MRState != tc.wantState || b.MRMerged != tc.wantMerged {
			t.Errorf("%s state/merged = %q/%q, want %q/%q",
				tc.id, b.MRState, b.MRMerged, tc.wantState, tc.wantMerged)
		}
	}
}

// TestPickCanonicalMRBead is the regression guard for bug ka-0000002394:
// `gb mr list` reported long-merged MRs as `opened`.
//
// The cause was not a missing GitLab backfill but the join itself. 396 live
// mr_urls carry 2-7 duplicate type=merge-request beads (concurrent
// UpsertMergeRequestBead inserts), 282 of which disagree on mr_state, and
// runMRList used to keep whichever the daemon returned LAST — an unordered
// choice that could land on a duplicate abandoned while the MR was still open,
// shadowing the sibling that recorded the merge. Measured against GitLab over
// all 282 conflicting groups: last-writer-wins matched ground truth 0 times,
// freshest-wins 278, this rule 282.
//
// The subtests below are the load-bearing cases in that measurement — most
// importantly "terminal beats fresher opened", which is the entire gap between
// this rule and a simpler freshest-wins one.
func TestPickCanonicalMRBead(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	bead := func(id, state string, updated time.Time) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:        id,
			Type:      "merge-request",
			UpdatedAt: updated,
			Fields:    map[string]string{"mr_state": state},
		}
	}

	tests := []struct {
		name   string
		cur    *beadsapi.BeadDetail
		next   *beadsapi.BeadDetail
		wantID string
	}{
		{
			name:   "nil cur takes next",
			next:   bead("gd-a", "opened", t0),
			wantID: "gd-a",
		},
		{
			name:   "nil next keeps cur",
			cur:    bead("gd-a", "opened", t0),
			wantID: "gd-a",
		},
		{
			// The exact live shape of MR !1180: a stale `opened` duplicate was
			// touched AFTER the sibling that recorded the close, so recency
			// alone picks the wrong one. Terminal state is absorbing, so the
			// merged/closed row is authoritative however old it is.
			name:   "terminal beats fresher opened (next terminal)",
			cur:    bead("gd-stale-open", "opened", t0.Add(48*time.Hour)),
			next:   bead("gd-closed", "closed", t0),
			wantID: "gd-closed",
		},
		{
			name:   "terminal beats fresher opened (cur terminal)",
			cur:    bead("gd-merged", "merged", t0),
			next:   bead("gd-stale-open", "opened", t0.Add(48*time.Hour)),
			wantID: "gd-merged",
		},
		{
			// Both terminal: no state-based tiebreak applies, so recency decides.
			name:   "two terminal rows fall back to freshest",
			cur:    bead("gd-old", "closed", t0),
			next:   bead("gd-new", "merged", t0.Add(time.Hour)),
			wantID: "gd-new",
		},
		{
			name:   "two non-terminal rows fall back to freshest",
			cur:    bead("gd-old", "opened", t0),
			next:   bead("gd-new", "opened", t0.Add(time.Hour)),
			wantID: "gd-new",
		},
		{
			name:   "older next does not displace fresher cur",
			cur:    bead("gd-new", "opened", t0.Add(time.Hour)),
			next:   bead("gd-old", "opened", t0),
			wantID: "gd-new",
		},
		{
			// Determinism guard: equal timestamps must not leave the winner up
			// to daemon row order, or the reported state flaps between runs.
			name:   "equal timestamps break to larger id",
			cur:    bead("gd-aaa", "opened", t0),
			next:   bead("gd-zzz", "opened", t0),
			wantID: "gd-zzz",
		},
		{
			name:   "equal timestamps break to larger id regardless of arg order",
			cur:    bead("gd-zzz", "opened", t0),
			next:   bead("gd-aaa", "opened", t0),
			wantID: "gd-zzz",
		},
		{
			// An absent mr_state is not terminal, so it must never outrank a
			// row that positively recorded the merge.
			name:   "missing state loses to terminal",
			cur:    &beadsapi.BeadDetail{ID: "gd-nostate", Type: "merge-request", UpdatedAt: t0.Add(time.Hour), Fields: map[string]string{}},
			next:   bead("gd-merged", "merged", t0),
			wantID: "gd-merged",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pickCanonicalMRBead(tc.cur, tc.next)
			if got == nil {
				t.Fatalf("pickCanonicalMRBead returned nil, want %s", tc.wantID)
			}
			if got.ID != tc.wantID {
				t.Errorf("pickCanonicalMRBead = %s, want %s", got.ID, tc.wantID)
			}
		})
	}
}

// TestPickCanonicalMRBead_NilNil covers the both-absent case separately, since
// it is the one input for which a nil return is correct.
func TestPickCanonicalMRBead_NilNil(t *testing.T) {
	if got := pickCanonicalMRBead(nil, nil); got != nil {
		t.Errorf("pickCanonicalMRBead(nil, nil) = %v, want nil", got)
	}
}

// TestRunMRList_DuplicateMRBeadsPickTerminal is the end-to-end guard for
// ka-0000002394: it drives the real runMRList against a daemon serving a
// duplicated mr_url, with the stale `opened` duplicate LAST in the result set
// — the row order that used to win. The listed state must still be `merged`.
func TestRunMRList_DuplicateMRBeadsPickTerminal(t *testing.T) {
	const mrURL = "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/1965"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		types := r.URL.Query().Get("type")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(types, "merge-request") {
			// Deliberately stale-last: the abandoned `opened` duplicate is
			// returned after the one that recorded the merge.
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{
				{
					"id": "gd-mr-merged", "type": "merge-request", "status": "closed",
					"updated_at": "2026-05-03T00:00:00Z",
					"fields": map[string]string{
						"mr_url": mrURL, "mr_state": "merged", "mr_outcome": "merged", "mr_iid": "1965",
					},
				},
				{
					"id": "gd-mr-stale", "type": "merge-request", "status": "open",
					"updated_at": "2026-06-18T00:00:00Z",
					"fields": map[string]string{
						"mr_url": mrURL, "mr_state": "opened", "mr_iid": "1965",
					},
				},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{
			{
				"id": "gd-issue", "type": "task", "status": "open", "title": "issue linked to a merged MR",
				"fields": map[string]string{"mr_url": mrURL},
			},
		}})
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer client.Close()

	prevDaemon, prevJSON := daemon, jsonOutput
	daemon, jsonOutput = client, false
	defer func() { daemon, jsonOutput = prevDaemon, prevJSON }()

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)

	if err := runMRList(cmd, nil); err != nil {
		t.Fatalf("runMRList: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "merged") {
		t.Errorf("listed state should be merged (the terminal duplicate wins), got:\n%s", got)
	}
	if strings.Contains(got, "opened") {
		t.Errorf("stale `opened` duplicate must not win the join, got:\n%s", got)
	}
}

// TestFilterMRBeads_ProjectsAssigneeAndStatus is the regression guard for bug
// ka-0000002009: each row must carry the ISSUE bead's assignee + status so the
// gardener-delivery orphan sweep ("skip if assignee ∈ ALIVE_AGENTS", "skip if
// not open") decides orphan-vs-owned from `gb mr list --json` alone, instead of
// an N+1 `kd show` per candidate every 20m cycle.
//
// The sharp edge: assignee/status must come from the issue bead, NOT the
// canonical type=merge-request bead joined by mr_url. MR beads carry their own
// (unrelated) assignee/status — sourcing from there would tell the sweep the
// wrong owner and report merged MRs' beads as closed.
func TestFilterMRBeads_ProjectsAssigneeAndStatus(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{
			ID:       "gd-owned",
			Title:    "open + assigned to a live crew",
			Status:   "in_progress",
			Assignee: "gardenio-live-crew",
			Fields:   map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/1"},
		},
		{
			ID:     "gd-orphan",
			Title:  "open + unassigned — the orphan the sweep must dispatch on",
			Status: "open",
			Fields: map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/2"},
		},
	}
	// Same mr_url keys, deliberately different assignee/status: if the
	// projection ever sources from the MR bead these values leak in.
	mrBeadsByURL := map[string]*beadsapi.BeadDetail{
		"https://gitlab/x/-/merge_requests/1": {
			ID:       "gd-mr1",
			Status:   "open",
			Assignee: "wrong-mr-bead-assignee",
			Fields:   map[string]string{"mr_state": "opened"},
		},
		"https://gitlab/x/-/merge_requests/2": {
			ID:       "gd-mr2",
			Status:   "closed",
			Assignee: "wrong-mr-bead-assignee",
			Fields:   map[string]string{"mr_state": "opened"},
		},
	}

	byID := map[string]mrBead{}
	for _, b := range filterMRBeads(beads, mrBeadsByURL, "", "") {
		byID[b.ID] = b
	}

	tests := []struct {
		id           string
		wantAssignee string
		wantStatus   string
	}{
		{id: "gd-owned", wantAssignee: "gardenio-live-crew", wantStatus: "in_progress"},
		{id: "gd-orphan", wantAssignee: "", wantStatus: "open"},
	}
	for _, tc := range tests {
		b, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s missing from output", tc.id)
			continue
		}
		if b.Assignee != tc.wantAssignee || b.Status != tc.wantStatus {
			t.Errorf("%s assignee/status = %q/%q, want %q/%q",
				tc.id, b.Assignee, b.Status, tc.wantAssignee, tc.wantStatus)
		}
	}

	// The keys must actually reach JSON consumers — the sweep reads them via
	// jq, so a missing/renamed tag is the same outage as a missing field.
	raw, err := json.Marshal(byID["gd-owned"])
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"assignee": "gardenio-live-crew", "status": "in_progress"} {
		if got, ok := row[k]; !ok {
			t.Errorf("JSON row missing %q key: %s", k, string(raw))
		} else if got != want {
			t.Errorf("JSON row[%q] = %v, want %v", k, got, want)
		}
	}

	// omitempty keeps the unassigned row from growing an empty key, so existing
	// output is unchanged where there is nothing to report.
	rawOrphan, err := json.Marshal(byID["gd-orphan"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawOrphan), `"assignee"`) {
		t.Errorf("unassigned row should omit the assignee key, got %s", string(rawOrphan))
	}
}

// TestFilterMRBeads_ProjectsAuthorIIDDraft is the regression guard for the
// second N+1 in bug ka-0000002009: each row must carry author + mr_iid + draft
// so the gardener-delivery orphan sweep skips draft + human-authored MRs from
// `gb mr list --json` alone, instead of an N+1 `glab mr view <iid>` per
// candidate every cycle.
//
// The sharp edge — this row is MIXED-SOURCE: author/mr_iid/draft must come from
// the canonical type=merge-request bead (mrBeadsByURL[u]), the OPPOSITE source
// from assignee/status (issue bead, guarded above). To pin the source, the
// issue bead below seeds CONFLICTING author/mr_iid/mr_title values: if the
// projection ever reads them from the issue bead, these leak in and the test
// fails. draft is derived from the MR bead's `Draft:` mr_title prefix.
func TestFilterMRBeads_ProjectsAuthorIIDDraft(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{
			ID:    "gd-draft-bot",
			Title: "draft MR, bot-authored — the sweep must SKIP this",
			// Conflicting decoys on the ISSUE bead: correct sourcing ignores them.
			Fields: map[string]string{
				"mr_url":   "https://gitlab/x/-/merge_requests/1",
				"author":   "wrong-issue-bead-author",
				"mr_iid":   "9999",
				"mr_title": "Draft: wrong-issue-bead-title",
			},
		},
		{
			ID:    "gd-ready-human",
			Title: "non-draft MR, human-authored",
			Fields: map[string]string{
				"mr_url":   "https://gitlab/x/-/merge_requests/2",
				"author":   "wrong-issue-bead-author",
				"mr_iid":   "8888",
				"mr_title": "Draft: wrong-issue-bead-title",
			},
		},
		{
			ID:     "gd-no-mrbead",
			Title:  "issue with no joined MR bead — author/iid/draft absent",
			Fields: map[string]string{"mr_url": "https://gitlab/x/-/merge_requests/3"},
		},
	}
	// The MR bead is the ONLY correct source for author/mr_iid/draft.
	mrBeadsByURL := map[string]*beadsapi.BeadDetail{
		"https://gitlab/x/-/merge_requests/1": {
			ID: "gd-mr1",
			Fields: map[string]string{
				"author":   "gardenio-some-crew",
				"mr_iid":   "1",
				"mr_title": "Draft: WIP feature",
			},
		},
		"https://gitlab/x/-/merge_requests/2": {
			ID: "gd-mr2",
			Fields: map[string]string{
				"author":   "matt-baker",
				"mr_iid":   "2",
				"mr_title": "feat: real feature",
			},
		},
	}

	byID := map[string]mrBead{}
	for _, b := range filterMRBeads(beads, mrBeadsByURL, "", "") {
		byID[b.ID] = b
	}

	tests := []struct {
		id         string
		wantAuthor string
		wantIID    string
		wantDraft  bool
	}{
		{id: "gd-draft-bot", wantAuthor: "gardenio-some-crew", wantIID: "1", wantDraft: true},
		{id: "gd-ready-human", wantAuthor: "matt-baker", wantIID: "2", wantDraft: false},
		{id: "gd-no-mrbead", wantAuthor: "", wantIID: "", wantDraft: false},
	}
	for _, tc := range tests {
		b, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s missing from output", tc.id)
			continue
		}
		if b.Author != tc.wantAuthor || b.MRIID != tc.wantIID || b.Draft != tc.wantDraft {
			t.Errorf("%s author/iid/draft = %q/%q/%v, want %q/%q/%v",
				tc.id, b.Author, b.MRIID, b.Draft, tc.wantAuthor, tc.wantIID, tc.wantDraft)
		}
	}

	// The keys must actually reach JSON consumers — the sweep reads them via jq,
	// so a missing/renamed tag is the same outage as a missing field.
	raw, err := json.Marshal(byID["gd-draft-bot"])
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"author": "gardenio-some-crew", "mr_iid": "1", "draft": true} {
		if got, ok := row[k]; !ok {
			t.Errorf("JSON row missing %q key: %s", k, string(raw))
		} else if got != want {
			t.Errorf("JSON row[%q] = %v, want %v", k, got, want)
		}
	}

	// omitempty keeps rows with nothing to report from growing empty keys: the
	// no-MR-bead row must carry none of the three, and the non-draft row must
	// omit draft (false) while still carrying author/mr_iid.
	rawNoMR, err := json.Marshal(byID["gd-no-mrbead"])
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"author"`, `"mr_iid"`, `"draft"`} {
		if strings.Contains(string(rawNoMR), k) {
			t.Errorf("no-MR-bead row should omit %s, got %s", k, string(rawNoMR))
		}
	}
	rawReady, err := json.Marshal(byID["gd-ready-human"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawReady), `"draft"`) {
		t.Errorf("non-draft row should omit the draft key, got %s", string(rawReady))
	}
}

// TestResolveMRStatusTarget covers the --bead/--mr aliases for the `gb mr status`
// positional target (bug ka-0000006140): all three forms are equivalent, the
// positional form still works, and naming two sources is a clear error rather
// than a silent precedence rule.
func TestResolveMRStatusTarget(t *testing.T) {
	const (
		beadID = "ka-0000006140"
		mrURL  = "https://gitlab.com/org/repo/-/merge_requests/42"
	)

	tests := []struct {
		name     string
		args     []string
		beadFlag string
		mrFlag   string
		want     string
		wantErr  string
	}{
		{
			name: "positional bead id (back-compat)",
			args: []string{beadID},
			want: beadID,
		},
		{
			name: "positional MR url (back-compat)",
			args: []string{mrURL},
			want: mrURL,
		},
		{
			name:     "--bead alias resolves to the same target as positional",
			beadFlag: beadID,
			want:     beadID,
		},
		{
			name:   "--mr alias resolves to the same target as positional",
			mrFlag: mrURL,
			want:   mrURL,
		},
		{
			// The flags are pure aliases, not a stricter contract: resolveMRURL
			// accepts either shape, so --bead with a URL is still coherent.
			name:     "--bead accepts a URL (pure alias, not a stricter contract)",
			beadFlag: mrURL,
			want:     mrURL,
		},
		{
			name:    "no target at all is an actionable error",
			wantErr: "required",
		},
		{
			name:     "positional + --bead is ambiguous",
			args:     []string{beadID},
			beadFlag: "gd-other",
			wantErr:  "exactly once",
		},
		{
			name:    "positional + --mr is ambiguous",
			args:    []string{beadID},
			mrFlag:  mrURL,
			wantErr: "exactly once",
		},
		{
			name:     "--bead + --mr is ambiguous",
			beadFlag: beadID,
			mrFlag:   mrURL,
			wantErr:  "exactly once",
		},
		{
			name:    "empty positional arg is treated as absent",
			args:    []string{""},
			wantErr: "required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMRStatusTarget(tc.args, tc.beadFlag, tc.mrFlag)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (target %q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("target = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMRStatusFlagsRegistered guards the wiring itself: the aliases must exist on
// the real command (a resolver nobody can reach would not fix ka-0000006140) and
// the command must accept zero args so a flag-only invocation parses.
func TestMRStatusFlagsRegistered(t *testing.T) {
	for _, name := range []string{"bead", "mr"} {
		if mrStatusCmd.Flags().Lookup(name) == nil {
			t.Errorf("gb mr status is missing the --%s flag", name)
		}
	}
	// Flag-only form supplies no positional arg, so ExactArgs(1) would reject it.
	if err := mrStatusCmd.Args(mrStatusCmd, []string{}); err != nil {
		t.Errorf("gb mr status must accept zero positional args (flag-only form): %v", err)
	}
	if err := mrStatusCmd.Args(mrStatusCmd, []string{"gd-abc"}); err != nil {
		t.Errorf("gb mr status must still accept one positional arg: %v", err)
	}
	if err := mrStatusCmd.Args(mrStatusCmd, []string{"gd-abc", "gd-def"}); err == nil {
		t.Error("gb mr status must reject two positional args")
	}
}

// TestRunMRVerifyCleanup_BranchDeleted covers the exit-0 "cleaned up" path of
// `gb mr verify-cleanup` (bug ka-0000004927): a merged MR whose source branch is
// gone from origin (GitLab 404s the branch endpoint) verifies as cleaned up.
// The exit-3 "branch survived" path calls os.Exit(3), which cannot run
// in-process, so the survived-branch behavior is covered by the bridge-level
// TestGitLabClient_BranchExists (200 → exists=true).
func TestRunMRVerifyCleanup_BranchDeleted(t *testing.T) {
	var gotBranchPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/repository/branches/") {
			gotBranchPath = r.URL.Path
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Branch Not Found"}`))
			return
		}
		// MR fetch: return a merged MR whose source branch we then check.
		_ = json.NewEncoder(w).Encode(bridge.GitLabMR{
			IID: 77, State: "merged", SourceBranch: "feat/gd-xyz-slug",
			WebURL: "https://example/mr/77",
		})
	}))
	defer srv.Close()

	origURL, origToken, origJSON, origBranch := mrGitLabURL, mrGitLabToken, jsonOutput, mrVerifyCleanupBranch
	t.Cleanup(func() {
		mrGitLabURL, mrGitLabToken, jsonOutput, mrVerifyCleanupBranch = origURL, origToken, origJSON, origBranch
	})
	mrGitLabURL, mrGitLabToken, jsonOutput, mrVerifyCleanupBranch = "", "tok", false, ""

	mrURL := srv.URL + "/owner/repo/-/merge_requests/77"
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)

	if err := runMRVerifyCleanup(cmd, []string{mrURL}); err != nil {
		t.Fatalf("runMRVerifyCleanup (deleted branch): %v", err)
	}
	if want := "/api/v4/projects/owner/repo/repository/branches/feat/gd-xyz-slug"; gotBranchPath != want {
		t.Errorf("branch check path = %q, want %q", gotBranchPath, want)
	}
	if s := out.String(); !strings.Contains(s, "CLEANED UP") || !strings.Contains(s, "feat/gd-xyz-slug") {
		t.Errorf("output missing cleaned-up confirmation: %q", s)
	}
}

// TestRunMRVerifyCleanup_ExplicitBranch confirms --branch overrides the MR's
// live source_branch (used when GitLab no longer returns source_branch for an
// already-deleted MR) and that no MR fetch is required in that path.
func TestRunMRVerifyCleanup_ExplicitBranch(t *testing.T) {
	var hitMRFetch bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/repository/branches/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Branch Not Found"}`))
			return
		}
		hitMRFetch = true
		_ = json.NewEncoder(w).Encode(bridge.GitLabMR{IID: 88, State: "merged"})
	}))
	defer srv.Close()

	origURL, origToken, origJSON, origBranch := mrGitLabURL, mrGitLabToken, jsonOutput, mrVerifyCleanupBranch
	t.Cleanup(func() {
		mrGitLabURL, mrGitLabToken, jsonOutput, mrVerifyCleanupBranch = origURL, origToken, origJSON, origBranch
	})
	mrGitLabURL, mrGitLabToken, jsonOutput, mrVerifyCleanupBranch = "", "tok", false, "agent/some-branch"

	mrURL := srv.URL + "/owner/repo/-/merge_requests/88"
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)

	if err := runMRVerifyCleanup(cmd, []string{mrURL}); err != nil {
		t.Fatalf("runMRVerifyCleanup (explicit branch): %v", err)
	}
	if hitMRFetch {
		t.Error("--branch set but MR was still fetched; expected no MR fetch")
	}
	if s := out.String(); !strings.Contains(s, "CLEANED UP") || !strings.Contains(s, "agent/some-branch") {
		t.Errorf("output missing cleaned-up confirmation: %q", s)
	}
}
