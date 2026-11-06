package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestStepPathsKeepsOnlyPaths(t *testing.T) {
	desc := "Touch `page.go`, `garden.css` and `gardenio/controller/cmd/gardenhub/web/`.\n" +
		"Run `kd show gd-x`, set `mr_overlaps`, read `opts.Plan`, see `refs/pull/<a>/head`, " +
		"`--hold-overlaps`, `0.3s`, `https://example.com/a.go` and `page.go` again."
	got := stepPaths(desc)
	want := []string{"page.go", "garden.css", "gardenio/controller/cmd/gardenhub/web/"}
	if !slices.Equal(got, want) {
		t.Errorf("stepPaths = %q, want %q", got, want)
	}
	if got := stepPaths("No files here, only `gb ready`."); len(got) != 0 {
		t.Errorf("stepPaths with no paths = %q, want none", got)
	}
}

func TestOverlapHolds(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	step := func(id, desc string, created time.Time) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: id, Description: desc, CreatedAt: created}
	}
	steps := []*beadsapi.BeadDetail{
		step("gd-held", "Edit `page.go` and `garden.html`.", now.Add(-5*time.Hour)),
		step("gd-nofiles", "Rework the copy; no paths named.", now.Add(-5*time.Hour)),
		step("gd-other", "Edit `kbeads/cmd/kd/main.go`.", now.Add(-5*time.Hour)),
		step("gd-own", "Edit `page.go`.", now.Add(-5*time.Hour)),
		step("gd-old", "Edit `gardenio/controller/cmd/gardenhub/web/garden.css`.", now.Add(-30*time.Hour)),
	}
	prs := []planOpenPR{
		{Number: 2251, Task: "gd-own", OpenedAt: now.Add(-2 * time.Hour), Files: []string{
			"gardenio/controller/cmd/gardenhub/page.go", "gardenio/controller/cmd/gardenhub/web/garden.html",
		}},
		{Number: 2240, Task: "gd-t2", OpenedAt: now.Add(-20 * time.Hour), Files: []string{
			"gardenio/controller/cmd/gardenhub/web/garden.css",
		}},
	}
	holds := overlapHolds(steps, prs, now)

	h := holds["gd-held"]
	if h == nil || len(h.PRs) != 1 || h.PRs[0].PR != 2251 || !slices.Equal(h.PRs[0].Files, []string{"page.go", "garden.html"}) {
		t.Fatalf("gd-held hold = %+v, want #2251 on page.go, garden.html", h)
	}
	if !h.Since.Equal(now.Add(-2*time.Hour)) || h.Expired {
		t.Errorf("gd-held since=%v expired=%v, want the PR's opening and not expired", h.Since, h.Expired)
	}
	for _, id := range []string{"gd-nofiles", "gd-other", "gd-own"} {
		if holds[id] != nil {
			t.Errorf("%s held by %+v, want no hold (no files / disjoint / its own PR)", id, holds[id])
		}
	}
	if o := holds["gd-old"]; o == nil || !o.Expired || !o.Since.Equal(now.Add(-20*time.Hour)) {
		t.Errorf("gd-old hold = %+v, want expired, since #2240 opened 20h ago", o)
	}
	if note := readyHoldNote(holds["gd-old"], now); !strings.Contains(note, "HOLD EXPIRED") || !strings.Contains(note, "comment why") {
		t.Errorf("expired note = %q, want it to say dispatch anyway and comment why", note)
	}
}

// TestReadyHoldOverlapsEndToEnd drives `gb ready --plan --hold-overlaps --json`
// against a fake daemon and a fake gh: a merged PR and an unreadable PR hold
// nothing, an open PR holds the step that names its file, and the JSON stays a
// bare array with overlap_hold only on the held row.
func TestReadyHoldOverlapsEndToEnd(t *testing.T) {
	type wire struct {
		ID          string            `json:"id"`
		Title       string            `json:"title"`
		Type        string            `json:"type"`
		Status      string            `json:"status"`
		Description string            `json:"description"`
		Fields      map[string]string `json:"fields,omitempty"`
		CreatedAt   time.Time         `json:"created_at"`
	}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ready := []wire{
		{ID: "gd-s2", Title: "step 2", Type: "task", Status: "open", Description: "Edit `garden.css`.", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "gd-s3", Title: "step 3", Type: "task", Status: "open", Description: "Edit `world.js`.", CreatedAt: now.Add(-3 * time.Hour)},
	}
	planBeads := []wire{
		{ID: "gd-s1", Type: "task", Status: "in_progress", Fields: map[string]string{"mr_url": "https://github.com/o/r/pull/11"}},
		{ID: "gd-s0", Type: "task", Status: "open", Fields: map[string]string{"mr_url": "https://github.com/o/r/pull/10"}},
		{ID: "gd-s9", Type: "task", Status: "in_progress", Fields: map[string]string{"mr_url": "https://github.com/o/r/pull/19"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rows := []wire{}
		switch {
		case q.Get("no_open_deps") == "true":
			rows = ready
		case q.Get("parent") == "gd-plan":
			rows = planBeads
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": len(rows)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon, oldGH, oldNow, oldJSON := daemon, prGHRun, readyHoldNow, jsonOutput
	daemon, jsonOutput = c, true
	readyHoldNow = func() time.Time { return now }
	prGHRun = func(_ context.Context, args ...string) (string, error) {
		switch args[2] {
		case "https://github.com/o/r/pull/11":
			return `{"number":11,"state":"OPEN","createdAt":"2026-11-04T10:00:00Z","files":[{"path":"gardenio/controller/cmd/gardenhub/web/garden.css"}]}`, nil
		case "https://github.com/o/r/pull/10":
			return `{"number":10,"state":"MERGED","createdAt":"2026-11-03T10:00:00Z","files":[{"path":"gardenio/controller/cmd/gardenhub/web/world.js"}]}`, nil
		}
		return "", errors.New("gh: not found")
	}
	t.Cleanup(func() { daemon, prGHRun, readyHoldNow, jsonOutput = oldDaemon, oldGH, oldNow, oldJSON })

	out := captureReadyStdout(t, func() {
		cmd := readyCmd
		cmd.SetContext(context.Background())
		for k, v := range map[string]string{"plan": "gd-plan", "hold-overlaps": "true"} {
			_ = cmd.Flags().Set(k, v)
		}
		t.Cleanup(func() { _ = cmd.Flags().Set("plan", ""); _ = cmd.Flags().Set("hold-overlaps", "false") })
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("ready RunE: %v", err)
		}
	})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not a bare JSON array: %v\n%s", err, out)
	}
	if len(rows) != 2 || rows[0]["id"] != "gd-s2" {
		t.Fatalf("rows = %v, want gd-s2 and gd-s3", rows)
	}
	h, ok := rows[0]["overlap_hold"].(map[string]any)
	if !ok || h["expired"] != false || !strings.Contains(out, `"pr": 11`) || !strings.Contains(out, `"task": "gd-s1"`) {
		t.Errorf("gd-s2 overlap_hold = %v, want held by #11 (gd-s1), not expired\n%s", rows[0]["overlap_hold"], out)
	}
	if _, held := rows[1]["overlap_hold"]; held {
		t.Errorf("gd-s3 carries overlap_hold %v; its only overlapping PR #10 is merged", rows[1]["overlap_hold"])
	}
}

func TestReadyHoldOverlapsNeedsPlan(t *testing.T) {
	cmd := readyCmd
	_ = cmd.Flags().Set("hold-overlaps", "true")
	t.Cleanup(func() { _ = cmd.Flags().Set("hold-overlaps", "false") })
	if _, err := readyOptionsFromFlags(cmd); err == nil || !strings.Contains(err.Error(), "--plan") {
		t.Errorf("readyOptionsFromFlags err = %v, want a --plan requirement", err)
	}
}
