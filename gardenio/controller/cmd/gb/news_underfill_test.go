package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestCapBeads pins the trim half of the news over-fetch: cap to `limit`, but
// treat limit <= 0 as "unlimited" so an unbounded caller is never truncated
// (matching the fetchLimit guard in runNews).
func TestCapBeads(t *testing.T) {
	mk := func(n int) []*beadsapi.BeadDetail {
		out := make([]*beadsapi.BeadDetail, n)
		for i := range out {
			out[i] = &beadsapi.BeadDetail{ID: "gd-" + strconv.Itoa(i)}
		}
		return out
	}
	tests := []struct {
		name  string
		in    int
		limit int
		want  int
	}{
		{"trims to limit", 10, 3, 3},
		{"exact fit unchanged", 3, 3, 3},
		{"under limit unchanged", 2, 5, 2},
		{"zero limit is unlimited", 10, 0, 10},
		{"negative limit is unlimited", 10, -1, 10},
		{"empty stays empty", 0, 5, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := capBeads(mk(tt.in), tt.limit)
			if len(got) != tt.want {
				t.Errorf("capBeads(len=%d, limit=%d) = %d, want %d", tt.in, tt.limit, len(got), tt.want)
			}
		})
	}
}

// newsUnderfillWireBead is the minimal JSON shape the fake daemon returns.
type newsUnderfillWireBead struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Type     string   `json:"type"`
	Status   string   `json:"status"`
	Assignee string   `json:"assignee"`
	Labels   []string `json:"labels"`
}

// TestNewsFiltersExclusiveProjectServerSide is the end-to-end regression for
// ka-0000003808 (the `gb news` analogue of ka-0000004559), migrated to the
// SERVER-SIDE exclusive_project filter (ka-0000003411). The client-side
// filterExclusiveProject pass and its 4x over-fetch are gone: the daemon's SQL
// LIMIT now accounts for project narrowing, so the query asks for the BARE
// limit and every returned row is already exclusive-to-gardenio.
//
// It proves two coupled properties against a fake daemon that applies the
// exclusive_project predicate BEFORE its LIMIT (as the real daemon's SQL does):
//
//  1. No over-fetch: the in-progress query the daemon receives asks for exactly
//     the caller `limit` and carries exclusive_project=gardenio.
//  2. Exact fill: the rendered in-progress section carries exactly `limit`
//     single-project rows, with the dual-project rows excluded server-side
//     rather than fetched-then-dropped, so no slot is wasted.
//
// The fixture places dual-project rows at the FRONT: a daemon that applied
// LIMIT before the exclusive_project predicate would underfill; the server-side
// filter (predicate first) must return a full window.
func TestNewsFiltersExclusiveProjectServerSide(t *testing.T) {
	const limit = 3
	// In-progress ordering as the store holds it (project-scoped so every row
	// carries project:gardenio; some ALSO carry project:monorepo and must be
	// excluded by exclusive_project). 2 dual-project rows sit at the front; the
	// server-side predicate drops them BEFORE LIMIT, so a full limit-sized
	// window of solo rows is returned.
	ipBeads := []newsUnderfillWireBead{
		{ID: "gd-dual1", Title: "dual 1", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio", "project:monorepo"}},
		{ID: "gd-solo1", Title: "solo 1", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio"}},
		{ID: "gd-dual2", Title: "dual 2", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio", "project:monorepo"}},
		{ID: "gd-solo2", Title: "solo 2", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio"}},
		{ID: "gd-solo3", Title: "solo 3", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio"}},
		{ID: "gd-solo4", Title: "solo 4", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio"}},
	}

	var ipLimitSeen int
	var ipExclusiveSeen string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limitParam, _ := strconv.Atoi(q.Get("limit"))
		var rows []newsUnderfillWireBead
		// Only the in-progress SECTION query drives this regression; the other
		// sections (closed/decisions/reports/open-decisions) return empty, which
		// is fine. The caller-scoped relevance path also issues a status=
		// in_progress query, but with an assignee filter (ListAssignedTask) — we
		// exclude it here (assignee=="") so it neither clobbers the captured
		// section limit nor injects the section fixture as the caller's anchor.
		if q.Get("status") == "in_progress" && q.Get("assignee") == "" {
			ipLimitSeen = limitParam
			ipExclusiveSeen = q.Get("exclusive_project")
			rows = ipBeads
		}
		// Faithfully mimic the daemon: apply the exclusive_project predicate
		// BEFORE the SQL LIMIT, so any dual-project row is dropped before the
		// window is cut. This is the whole point of the server-side move — the
		// LIMIT accounts for the narrowing.
		if ex := q.Get("exclusive_project"); ex != "" {
			want := "project:" + ex
			kept := rows[:0:0]
			for _, b := range rows {
				exclusive := true
				for _, lbl := range b.Labels {
					if strings.HasPrefix(lbl, "project:") && lbl != want {
						exclusive = false
						break
					}
				}
				if exclusive {
					kept = append(kept, b)
				}
			}
			rows = kept
		}
		total := len(rows)
		if limitParam > 0 && len(rows) > limitParam {
			rows = rows[:limitParam]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": total})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	// project-scoped (not --all-projects) so exclusive_project is applied;
	// --all so the current actor's rows are not filtered out; JSON for a stable
	// machine-checkable shape.
	newsCmd.SetContext(context.Background())
	for flag, val := range map[string]string{
		"project": "gardenio",
		"all":     "true",
		"limit":   strconv.Itoa(limit),
		"window":  "24h",
	} {
		if err := newsCmd.Flags().Set(flag, val); err != nil {
			t.Fatalf("set --%s: %v", flag, err)
		}
	}
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() {
		jsonOutput = oldJSON
		_ = newsCmd.Flags().Set("all", "false")
		_ = newsCmd.Flags().Set("limit", "50")
		_ = newsCmd.Flags().Set("window", "2h")
		_ = newsCmd.Flags().Set("project", "")
	})

	out := string(captureStdout(t, func() error {
		return runNews(newsCmd, nil)
	}))

	// Property 1: the daemon carried the server-side filter and was asked for
	// the BARE limit — no client-side over-fetch remains.
	if ipExclusiveSeen != "gardenio" {
		t.Errorf("in-progress query exclusive_project = %q, want %q (server-side narrowing)", ipExclusiveSeen, "gardenio")
	}
	if ipLimitSeen != limit {
		t.Errorf("in-progress query limit = %d, want %d (bare limit; the server-side filter removed the over-fetch)", ipLimitSeen, limit)
	}

	var got newsJSONOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal news json: %v\noutput: %s", err, out)
	}
	// Property 2: the section is filled to exactly `limit` with the dual-project
	// rows excluded server-side, so we see limit solo rows, not limit-2.
	// activeIP/staleIP/unassignedIP partition the in-progress set; sum them.
	gotIP := len(got.InProgress) + len(got.Stale) + len(got.Unassigned)
	if gotIP != limit {
		t.Fatalf("in-progress rendered %d rows, want %d (server-side exclusive_project fills the window)", gotIP, limit)
	}
	// None of the rendered rows may be a dropped dual-project row.
	for _, section := range [][]newsJSONBead{got.InProgress, got.Stale, got.Unassigned} {
		for _, b := range section {
			if b.ID == "gd-dual1" || b.ID == "gd-dual2" {
				t.Errorf("dual-project row %s leaked into rendered in-progress; exclusive_project should have excluded it server-side", b.ID)
			}
		}
	}
}
