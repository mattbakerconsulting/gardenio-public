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

// newsOpenDecWireBead is the minimal JSON shape the fake daemon returns for the
// open-decisions section.
type newsOpenDecWireBead struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Labels    []string          `json:"labels"`
	Fields    map[string]string `json:"fields"`
	CreatedAt string            `json:"created_at"`
}

// TestNewsOpenDecisionsOldestFirstAndMarker is the end-to-end regression for
// ka-0000005518. It proves two coupled properties against a fake daemon that
// faithfully honors the requested sort + limit (as the real daemon's SQL does):
//
//  1. Oldest-first: the open-decisions query the daemon receives asks for
//     `sort=created_at` (ascending), NOT `-created_at`. So when the open set
//     exceeds `limit`, capBeads drops the NEWEST rows and the rendered section
//     RETAINS THE OLDEST — the starved decisions that most need attention.
//     Previously `-created_at` buried exactly those oldest rows.
//  2. Truncation marker: when the section is capped, a "… and N more" marker
//     fires so an un-surfaced decision reads as "below the fold", never as
//     "no such decision".
func TestNewsOpenDecisionsOldestFirstAndMarker(t *testing.T) {
	const limit = 3
	// Five open decisions, OLDEST FIRST as an ascending created_at sort returns
	// them. old1 is the most-starved row and MUST survive the cap.
	openDecs := []newsOpenDecWireBead{
		{ID: "gd-old1", Title: "old 1", Type: "decision", Status: "open", Labels: []string{"project:gardenio"}, Fields: map[string]string{"prompt": "old 1"}, CreatedAt: "2026-08-01T00:00:00Z"},
		{ID: "gd-old2", Title: "old 2", Type: "decision", Status: "open", Labels: []string{"project:gardenio"}, Fields: map[string]string{"prompt": "old 2"}, CreatedAt: "2026-08-02T00:00:00Z"},
		{ID: "gd-old3", Title: "old 3", Type: "decision", Status: "open", Labels: []string{"project:gardenio"}, Fields: map[string]string{"prompt": "old 3"}, CreatedAt: "2026-08-03T00:00:00Z"},
		{ID: "gd-new1", Title: "new 1", Type: "decision", Status: "open", Labels: []string{"project:gardenio"}, Fields: map[string]string{"prompt": "new 1"}, CreatedAt: "2026-08-18T00:00:00Z"},
		{ID: "gd-new2", Title: "new 2", Type: "decision", Status: "open", Labels: []string{"project:gardenio"}, Fields: map[string]string{"prompt": "new 2"}, CreatedAt: "2026-08-19T00:00:00Z"},
	}

	var sortSeen string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limitParam, _ := strconv.Atoi(q.Get("limit"))
		var rows []newsOpenDecWireBead
		// Only the open-decisions SECTION query drives this regression: type=
		// decision + status=open. Every other section returns empty.
		if q.Get("type") == "decision" && q.Get("status") == "open" {
			sortSeen = q.Get("sort")
			rows = openDecs
		}
		// Faithfully mimic the daemon: report the full match count as total, then
		// truncate to the requested window BEFORE returning.
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

	// --- Property 1: JSON open_decisions retains the OLDEST rows. ---
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

	// The daemon must have been asked to sort ascending (oldest first), not desc.
	if sortSeen != "created_at" {
		t.Errorf("open-decisions query sort = %q, want %q (oldest-first FIFO); a %q sort buries the starved oldest rows",
			sortSeen, "created_at", "-created_at")
	}

	var got newsJSONOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal news json: %v\noutput: %s", err, out)
	}
	if len(got.OpenDecisions) != limit {
		t.Fatalf("open_decisions rendered %d rows, want %d (capped to limit)", len(got.OpenDecisions), limit)
	}
	gotIDs := make([]string, len(got.OpenDecisions))
	for i, od := range got.OpenDecisions {
		gotIDs[i] = od.ID
	}
	// The three OLDEST rows must be exactly what survives — the newest are dropped.
	wantIDs := []string{"gd-old1", "gd-old2", "gd-old3"}
	for i, want := range wantIDs {
		if gotIDs[i] != want {
			t.Errorf("open_decisions[%d] = %q, want %q; oldest rows must be retained (ids=%v)", i, gotIDs[i], want, gotIDs)
		}
	}
	for _, od := range got.OpenDecisions {
		if od.ID == "gd-new1" || od.ID == "gd-new2" {
			t.Errorf("newest decision %s leaked into the capped section; capBeads should drop the newest past limit", od.ID)
		}
	}

	// --- Property 2: text render emits a truncation marker. ---
	jsonOutput = false
	textOut := string(captureStdout(t, func() error {
		return runNews(newsCmd, nil)
	}))
	if !strings.Contains(textOut, "and 2 more open decision") {
		t.Errorf("expected truncation marker for 2 hidden decisions, output:\n%s", textOut)
	}
}
