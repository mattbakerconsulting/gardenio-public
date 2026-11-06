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
)

func TestKeywordsFromTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  []string
	}{
		{
			// Distinctive tokens survive; command/stopword noise ("gb", "news",
			// "jwt"<4ch) is dropped so the cross-project probe stays precise.
			// Order is length-desc then alpha, capped at relevanceKeywordN=4:
			// credential(10), laptop(6), then the len-4 set matt<mint<ziti alpha
			// with ziti falling off the cap.
			name:  "distinctive tokens ranked, noise dropped",
			title: "gb news: mint matt-laptop ziti JWT credential",
			want:  []string{"credential", "laptop", "matt", "mint"},
		},
		{
			name:  "all stopwords yields nothing",
			title: "gb news task fix bug update",
			want:  nil,
		},
		{
			// Every token is < 4 chars (or a stopword), so nothing survives.
			name:  "short tokens dropped",
			title: "fix a b cd eff",
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keywordsFromTitle(tt.title)
			if len(got) != len(tt.want) {
				t.Fatalf("keywordsFromTitle(%q) = %v, want %v", tt.title, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("keywordsFromTitle(%q)[%d] = %q, want %q", tt.title, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTopicalLabels(t *testing.T) {
	in := []string{"project:gardenio", "role:crew", "source:multi", "area:news", "gardener:x", "plan", "billing"}
	got := topicalLabels(in)
	want := map[string]bool{"area:news": true, "billing": true}
	if len(got) != len(want) {
		t.Fatalf("topicalLabels(%v) = %v, want keys %v", in, got, want)
	}
	for _, l := range got {
		if !want[l] {
			t.Errorf("topicalLabels leaked noisy label %q", l)
		}
	}
}

// TestTopicalLabelsDropsTriggerNamespace pins the CRON-EXHAUST leak in bug
// ka-0000006165. The trigger engine stamps `trigger:<id>` on every per-run bead
// it spawns, so a gardener's own run-instance bead shares that label with all of
// its prior firings. With `trigger:` treated as topical, the "Shares a label
// with your bead" section filled with the caller's own cron exhaust — measured
// 12/12 rows on 2026-08-21 01:53. It is an operational namespace, exactly like
// the `schedule:` sibling next to it, and must never count as "same subject".
func TestTopicalLabelsDropsTriggerNamespace(t *testing.T) {
	// The real label set off the news+ready relevance gardener's run bead.
	got := topicalLabels([]string{"project:gardenio", "source:schedule", "trigger:ka-0000005840"})
	if len(got) != 0 {
		t.Errorf("topicalLabels(per-run cron labels) = %v, want none — a run-instance bead has no topical label to match on", got)
	}
	// A genuinely topical label alongside the trigger stamp still survives.
	got = topicalLabels([]string{"trigger:ka-0000005840", "area:news"})
	if len(got) != 1 || got[0] != "area:news" {
		t.Errorf("topicalLabels = %v, want [area:news] — dropping trigger: must not drop real topics", got)
	}
}

func TestSplitRelevanceDeps(t *testing.T) {
	deps := []beadsapi.Dependency{
		{DependsOnID: "gd-blk1", Type: "blocks"},
		{DependsOnID: "gd-epic", Type: "parent-child"},
		{DependsOnID: "gd-legacy", Type: "parent"},
		{DependsOnID: "gd-rel", Type: "related"}, // ignored
		{DependsOnID: "", Type: "blocks"},        // empty target ignored
	}
	blockers, parents := splitRelevanceDeps(deps)
	if len(blockers) != 1 || blockers[0] != "gd-blk1" {
		t.Errorf("blockers = %v, want [gd-blk1]", blockers)
	}
	if len(parents) != 2 || parents[0] != "gd-epic" || parents[1] != "gd-legacy" {
		t.Errorf("parents = %v, want [gd-epic gd-legacy]", parents)
	}
}

func TestHasOtherProject(t *testing.T) {
	other := &beadsapi.BeadDetail{Labels: []string{"project:gardenio", "project:monorepo"}}
	if !hasOtherProject(other, "gardenio") {
		t.Error("expected dual-project bead to be flagged as other-project")
	}
	own := &beadsapi.BeadDetail{Labels: []string{"project:gardenio"}}
	if hasOtherProject(own, "gardenio") {
		t.Error("own-project bead must not be flagged as other-project")
	}
}

func TestRelevantComments_FiltersSelfAndOld(t *testing.T) {
	now := time.Now()
	anchor := &beadsapi.BeadDetail{ID: "gd-anchor"}
	comments := []beadsapi.Comment{
		{Author: "gardener", Text: "UNBLOCKED — go", CreatedAt: now.Add(-1 * time.Hour)},
		{Author: "me", Text: "my own note", CreatedAt: now.Add(-30 * time.Minute)},
		{Author: "gardener", Text: "ancient", CreatedAt: now.Add(-48 * time.Hour)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"comments": comments})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	got := relevantComments(context.Background(), anchor, "me", newsRelevanceWindow)
	if len(got) != 1 {
		t.Fatalf("relevantComments returned %d, want 1 (gardener's recent, not self or ancient): %+v", len(got), got)
	}
	if got[0].Author != "gardener" || !strings.Contains(got[0].Text, "UNBLOCKED") {
		t.Errorf("wrong comment surfaced: %+v", got[0])
	}
}

func TestRelevantBlockers_KeepsOpenAndRecentlyCleared(t *testing.T) {
	now := time.Now()
	beads := map[string]*beadsapi.BeadDetail{
		"gd-open":    {ID: "gd-open", Title: "still blocking", Status: "open"},
		"gd-cleared": {ID: "gd-cleared", Title: "just closed", Status: "closed", UpdatedAt: now.Add(-3 * time.Hour)},
		"gd-ancient": {ID: "gd-ancient", Title: "closed ages ago", Status: "closed", UpdatedAt: now.Add(-72 * time.Hour)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /v1/batch/beads response shape: {"results":[{"id","bead"}]}
		ids := r.URL.Query().Get("ids")
		var results []map[string]any
		for _, id := range strings.Split(ids, ",") {
			if b, ok := beads[id]; ok {
				results = append(results, map[string]any{"id": id, "bead": b})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	got := relevantBlockers(context.Background(), []string{"gd-open", "gd-cleared", "gd-ancient"}, newsRelevanceWindow)
	if len(got) != 2 {
		t.Fatalf("relevantBlockers = %d rows, want 2 (open + recently-cleared, ancient dropped): %+v", len(got), got)
	}
	// Cleared must sort first (the "you're unblocked" signal is more actionable).
	if !got[0].Cleared || got[0].Bead.ID != "gd-cleared" {
		t.Errorf("expected recently-cleared blocker first, got %+v", got[0])
	}
	if got[1].Cleared || got[1].Bead.ID != "gd-open" {
		t.Errorf("expected open blocker second, got %+v", got[1])
	}
}

// relevanceMux is a routed fake daemon covering every endpoint gatherRelevance
// touches. It lets the end-to-end test assert the full digest shape.
type relevanceMux struct {
	assigned     []*beadsapi.BeadDetail           // ListAssignedTask result
	comments     []beadsapi.Comment               // anchor comments
	deps         map[string][]beadsapi.Dependency // per-bead dependency edges
	batch        map[string]*beadsapi.BeadDetail  // BatchGetBeads store
	subtree      []*beadsapi.BeadDetail           // Parent=epic listing
	sharedLabel  []*beadsapi.BeadDetail           // LabelsAny listing
	crossProject []*beadsapi.BeadDetail           // Search listing
}

func (m *relevanceMux) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/batch/beads", func(w http.ResponseWriter, r *http.Request) {
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		var results []map[string]any
		for _, id := range ids {
			if b, ok := m.batch[id]; ok {
				results = append(results, map[string]any{"id": id, "bead": b})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	mux.HandleFunc("/v1/beads/", func(w http.ResponseWriter, r *http.Request) {
		// .../comments or .../dependencies
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": m.comments})
		case strings.HasSuffix(r.URL.Path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/dependencies")
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": m.deps[id]})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var rows []*beadsapi.BeadDetail
		switch {
		case q.Get("assignee") != "":
			rows = m.assigned
		case q.Get("parent") != "":
			rows = m.subtree
		case q.Get("labels_any") != "":
			rows = m.sharedLabel
		case q.Get("search") != "":
			rows = m.crossProject
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": len(rows)})
	})
	return mux
}

func TestGatherRelevance_EndToEnd(t *testing.T) {
	now := time.Now()
	anchor := &beadsapi.BeadDetail{
		ID: "gd-anchor", Title: "wire ziti tunnel for laptop", Status: "in_progress",
		Assignee: "me", Labels: []string{"project:gardenio", "area:networking"},
	}
	epic := &beadsapi.BeadDetail{ID: "gd-epic", Title: "networking epic", Type: "epic"}

	m := &relevanceMux{
		assigned: []*beadsapi.BeadDetail{anchor},
		comments: []beadsapi.Comment{
			{Author: "gardener", Text: "UNBLOCKED — proceed with apply", CreatedAt: now.Add(-1 * time.Hour)},
		},
		deps: map[string][]beadsapi.Dependency{
			"gd-anchor": {
				{DependsOnID: "gd-blk", Type: "blocks"},
				{DependsOnID: "gd-epic", Type: "parent-child"},
			},
		},
		batch: map[string]*beadsapi.BeadDetail{
			"gd-blk":  {ID: "gd-blk", Title: "escalation cleared", Type: "escalation", Status: "closed", UpdatedAt: now.Add(-2 * time.Hour)},
			"gd-epic": epic,
		},
		subtree: []*beadsapi.BeadDetail{
			{ID: "gd-sib", Title: "sibling task", Type: "task", Status: "in_progress", Assignee: "other", Labels: []string{"project:gardenio"}},
		},
		sharedLabel: []*beadsapi.BeadDetail{
			{ID: "gd-lbl", Title: "same-area work", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio", "area:networking"}},
		},
		crossProject: []*beadsapi.BeadDetail{
			{ID: "gd-dup", Title: "ziti tunnel for laptop already done", Type: "task", Status: "closed", UpdatedAt: now.Add(-5 * time.Hour), Labels: []string{"project:monorepo"}},
		},
	}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	swapDaemon(t, srv.URL)

	d := gatherRelevance(context.Background(), "me", "gardenio", newsRelevanceWindow)
	if d == nil {
		t.Fatal("gatherRelevance returned nil for a caller with an assigned bead")
	}
	if d.empty() {
		t.Fatal("digest is empty; expected all five axes populated")
	}
	if len(d.NewComments) != 1 || !strings.Contains(d.NewComments[0].Text, "UNBLOCKED") {
		t.Errorf("NewComments = %+v, want gardener's UNBLOCKED note", d.NewComments)
	}
	if len(d.Blockers) != 1 || !d.Blockers[0].Cleared || d.Blockers[0].Bead.ID != "gd-blk" {
		t.Errorf("Blockers = %+v, want one recently-cleared gd-blk", d.Blockers)
	}
	if d.Container == nil || d.Container.ID != "gd-epic" {
		t.Errorf("Container = %+v, want gd-epic", d.Container)
	}
	if len(d.Subtree) != 1 || d.Subtree[0].Bead.ID != "gd-sib" {
		t.Errorf("Subtree = %+v, want [gd-sib]", d.Subtree)
	}
	if len(d.SharedLabel) != 1 || d.SharedLabel[0].Bead.ID != "gd-lbl" {
		t.Errorf("SharedLabel = %+v, want [gd-lbl]", d.SharedLabel)
	}
	if len(d.CrossProject) != 1 || d.CrossProject[0].Bead.ID != "gd-dup" {
		t.Errorf("CrossProject = %+v, want [gd-dup]", d.CrossProject)
	}

	// JSON projection carries every axis under the `relevant` key.
	js := relevanceToJSON(d)
	if js == nil || js.Anchor != "gd-anchor" || js.Container != "gd-epic" {
		t.Fatalf("relevanceToJSON = %+v, want anchor+container populated", js)
	}
	if len(js.Blockers) != 1 || !js.Blockers[0].Cleared {
		t.Errorf("JSON blockers = %+v, want one cleared", js.Blockers)
	}
	if len(js.CrossProject) != 1 {
		t.Errorf("JSON cross_project = %+v, want 1", js.CrossProject)
	}
}

// TestGatherRelevance_NoAnchor pins that a caller with no claimed bead (a
// gardener, release owner, or thread agent) gets a nil digest — the audit found
// that cohort already reads rows, so relevance-scoping must not fire for them.
func TestGatherRelevance_NoAnchor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if d := gatherRelevance(context.Background(), "gardener", "gardenio", newsRelevanceWindow); !d.empty() {
		t.Errorf("expected empty digest for a caller with no assigned bead, got %+v", d)
	}
}

func TestGatherRelevance_UnknownActor(t *testing.T) {
	if d := gatherRelevance(context.Background(), "unknown", "gardenio", newsRelevanceWindow); d != nil {
		t.Errorf("expected nil digest for unknown actor, got %+v", d)
	}
	if d := gatherRelevance(context.Background(), "", "gardenio", newsRelevanceWindow); d != nil {
		t.Errorf("expected nil digest for empty actor, got %+v", d)
	}
}

// TestPrintRelevance_Renders exercises the human-readable renderer end to end so
// a formatting regression is caught. It asserts the section header and one line
// from each populated axis appear.
func TestPrintRelevance_Renders(t *testing.T) {
	now := time.Now()
	d := &relevanceDigest{
		Anchor:    &beadsapi.BeadDetail{ID: "gd-anchor", Title: "my task"},
		Container: &beadsapi.BeadDetail{ID: "gd-epic", Title: "the epic"},
		NewComments: []beadsapi.Comment{
			{Author: "gardener", Text: "UNBLOCKED", CreatedAt: now.Add(-1 * time.Hour)},
		},
		Blockers: []relevanceBlocker{
			{Bead: &beadsapi.BeadDetail{ID: "gd-blk", Title: "gate", Type: "decision", Status: "closed", UpdatedAt: now.Add(-2 * time.Hour)}, Cleared: true},
		},
		Subtree: []relevanceRow{
			{Bead: &beadsapi.BeadDetail{ID: "gd-sib", Title: "sibling", Type: "task"}, Reason: "in progress @other"},
		},
		CrossProject: []relevanceRow{
			{Bead: &beadsapi.BeadDetail{ID: "gd-dup", Title: "dup", Type: "task"}, Reason: "project:monorepo, closed 5h ago · possible duplicate"},
		},
	}
	var sb strings.Builder
	printRelevance(d, &sb)
	out := sb.String()
	for _, want := range []string{
		"Relevant to your work (gd-anchor",
		"New comments on your bead",
		"UNBLOCKED",
		"Blockers (1, 1 recently cleared)",
		"gd-blk",
		"you may be unblocked",
		"In your epic (gd-epic",
		"gd-sib",
		"Similar work in other projects",
		"gd-dup",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printRelevance output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRelevanceDigest_EmptyNilSafe(t *testing.T) {
	var d *relevanceDigest
	if !d.empty() {
		t.Error("nil digest must report empty")
	}
	if relevanceToJSON(d) != nil {
		t.Error("relevanceToJSON(nil) must be nil")
	}
}

// runNewsRelevanceFixture returns a mux that serves a single-axis relevance
// digest (one cleared blocker on the caller's anchor) plus empty fleet-wide
// sections, so the end-to-end runNews tests below can assert the caller-scoped
// block appears/disappears without unrelated rows in the way.
func runNewsRelevanceFixture(t *testing.T) {
	t.Helper()
	now := time.Now()
	anchor := &beadsapi.BeadDetail{ID: "gd-anchor", Title: "my claimed work", Status: "in_progress", Assignee: "me", Labels: []string{"project:gardenio"}}
	m := &relevanceMux{
		assigned: []*beadsapi.BeadDetail{anchor},
		deps:     map[string][]beadsapi.Dependency{"gd-anchor": {{DependsOnID: "gd-blk", Type: "blocks"}}},
		batch:    map[string]*beadsapi.BeadDetail{"gd-blk": {ID: "gd-blk", Title: "the gate", Type: "escalation", Status: "closed", UpdatedAt: now.Add(-3 * time.Hour)}},
	}
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
}

// setNewsFlags applies overrides to the shared newsCmd and restores the
// defaults on cleanup so package-level flag state does not leak between tests.
func setNewsFlags(t *testing.T, overrides map[string]string) {
	t.Helper()
	newsCmd.SetContext(context.Background())
	defaults := map[string]string{
		"all": "false", "window": "2h", "limit": "50", "project": "",
		"all-projects": "false", "no-relevance": "false", "ids-only": "false",
		"stale-only": "false", "active-only": "false",
	}
	for flag, val := range overrides {
		if err := newsCmd.Flags().Set(flag, val); err != nil {
			t.Fatalf("set --%s: %v", flag, err)
		}
	}
	t.Cleanup(func() {
		for flag, val := range defaults {
			_ = newsCmd.Flags().Set(flag, val)
		}
	})
}

// TestRunNews_RelevanceSection_Pretty drives the real newsCmd end to end and
// asserts the caller-scoped block leads the pretty digest for a claim-holding
// actor.
func TestRunNews_RelevanceSection_Pretty(t *testing.T) {
	runNewsRelevanceFixture(t)
	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })
	setNewsFlags(t, map[string]string{"project": "gardenio", "window": "2h"})

	out := string(captureStdout(t, func() error { return runNews(newsCmd, nil) }))
	if !strings.Contains(out, "Relevant to your work (gd-anchor") {
		t.Errorf("expected caller-scoped section, got:\n%s", out)
	}
	if !strings.Contains(out, "gd-blk") || !strings.Contains(out, "you may be unblocked") {
		t.Errorf("expected recently-cleared blocker line, got:\n%s", out)
	}
}

// TestRunNews_NoRelevanceFlag pins that --no-relevance suppresses the section.
func TestRunNews_NoRelevanceFlag(t *testing.T) {
	runNewsRelevanceFixture(t)
	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })
	setNewsFlags(t, map[string]string{"project": "gardenio", "no-relevance": "true"})

	out := string(captureStdout(t, func() error { return runNews(newsCmd, nil) }))
	if strings.Contains(out, "Relevant to your work") {
		t.Errorf("--no-relevance must suppress the section, got:\n%s", out)
	}
}

// TestRunNews_RelevanceSection_JSON pins the --json projection carries the
// `relevant` key with the anchor and blocker.
func TestRunNews_RelevanceSection_JSON(t *testing.T) {
	runNewsRelevanceFixture(t)
	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })
	setNewsFlags(t, map[string]string{"project": "gardenio"})
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	out := captureStdout(t, func() error { return runNews(newsCmd, nil) })
	var got newsJSONOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal news json: %v\n%s", err, out)
	}
	if got.Relevant == nil {
		t.Fatalf("expected `relevant` key in JSON, got:\n%s", out)
	}
	if got.Relevant.Anchor != "gd-anchor" {
		t.Errorf("relevant.anchor = %q, want gd-anchor", got.Relevant.Anchor)
	}
	if len(got.Relevant.Blockers) != 1 || !got.Relevant.Blockers[0].Cleared {
		t.Errorf("relevant.blockers = %+v, want one cleared", got.Relevant.Blockers)
	}
}
