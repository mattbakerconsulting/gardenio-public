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

// readyFooterFakeDaemon serves /v1/beads over a fixed dataset, honoring the
// exclusive_project predicate BEFORE limit/offset (as the real daemon's SQL
// does since ka-0000003411) so that `total` is the exact post-filter match
// count — the number the footer must print. This is the shape that used to make
// the footer overstate the claimable queue (ka-0000001533), now fixed at the
// source: the daemon narrows and counts in one query.
func readyFooterFakeDaemon(t *testing.T, beads []map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		// Apply exclusive_project FIRST so total reflects the narrowed set.
		page := beads
		if ex := r.URL.Query().Get("exclusive_project"); ex != "" {
			want := "project:" + ex
			kept := make([]map[string]any, 0, len(page))
			for _, b := range page {
				exclusive := true
				if lbls, ok := b["labels"].([]string); ok {
					for _, lbl := range lbls {
						if strings.HasPrefix(lbl, "project:") && lbl != want {
							exclusive = false
							break
						}
					}
				}
				if exclusive {
					kept = append(kept, b)
				}
			}
			page = kept
		}
		total := len(page)
		if offset > 0 {
			if offset >= len(page) {
				page = nil
			} else {
				page = page[offset:]
			}
		}
		if limit > 0 && len(page) > limit {
			page = page[:limit]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": page, "total": total})
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

	readyCmd.SetContext(context.Background())
	oldJSON := jsonOutput
	jsonOutput = false
	t.Cleanup(func() { jsonOutput = oldJSON })
}

// makeReadyFooterBeads builds n ready beads all carrying project:gardenio, of
// which every 5th ALSO carries project:monorepo (a dual-project row the
// server-side exclusive_project filter excludes). With n=25 that leaves 20
// exclusive-to-gardenio.
func makeReadyFooterBeads(n int) []map[string]any {
	beads := make([]map[string]any, n)
	for i := range beads {
		labels := []string{"project:gardenio"}
		if (i+1)%5 == 0 {
			labels = append(labels, "project:monorepo")
		}
		beads[i] = map[string]any{
			"id":     "gd-" + strconv.Itoa(1000+i),
			"title":  "ready " + strconv.Itoa(i),
			"type":   "task",
			"status": "open",
			"labels": labels,
		}
	}
	return beads
}

// TestReadyFooterHonestTotal is the end-to-end regression guard for ka-0000001533,
// migrated to the server-side exclusive_project filter (ka-0000003411): the
// footer reports the daemon's post-filter `total` directly, which is now exact
// because the daemon narrows-then-counts. It reproduces the bug's exact shape
// (25 matched, 20 claimable, 5 dual-project) across window regimes.
func TestReadyFooterHonestTotal(t *testing.T) {
	const matched, claimable = 25, 20

	t.Run("truncated window: footer prints the exact post-filter total", func(t *testing.T) {
		resetReadyFlags(t)
		readyFooterFakeDaemon(t, makeReadyFooterBeads(matched))
		if err := readyCmd.Flags().Set("project", "gardenio"); err != nil {
			t.Fatal(err)
		}
		// limit 3 → only 3 rows print, but the footer's total is the daemon's
		// exact post-filter count (20), never the pre-filter 25.
		if err := readyCmd.Flags().Set("limit", "3"); err != nil {
			t.Fatal(err)
		}
		out := string(captureStdout(t, func() error { return readyCmd.RunE(readyCmd, nil) }))
		if !strings.Contains(out, "3 beads (20 total)") {
			t.Errorf("footer = %q, want it to contain %q (the post-filter total, NOT the pre-filter %d)", out, "3 beads (20 total)", matched)
		}
		if strings.Contains(out, "("+strconv.Itoa(matched)+" total)") {
			t.Errorf("footer = %q, must not print the pre-filter total %d", out, matched)
		}
	})

	t.Run("complete window: exact post-filter total", func(t *testing.T) {
		resetReadyFlags(t)
		readyFooterFakeDaemon(t, makeReadyFooterBeads(matched))
		if err := readyCmd.Flags().Set("project", "gardenio"); err != nil {
			t.Fatal(err)
		}
		// limit 100 returns the whole 20-row narrowed set; all 20 claimable rows print.
		if err := readyCmd.Flags().Set("limit", "100"); err != nil {
			t.Fatal(err)
		}
		out := string(captureStdout(t, func() error { return readyCmd.RunE(readyCmd, nil) }))
		want := strconv.Itoa(claimable) + " beads (" + strconv.Itoa(claimable) + " total)"
		if !strings.Contains(out, want) {
			t.Errorf("footer = %q, want it to contain %q", out, want)
		}
	})

	t.Run("all-projects: exclusive filter inactive, pre-filter total is correct", func(t *testing.T) {
		resetReadyFlags(t)
		readyFooterFakeDaemon(t, makeReadyFooterBeads(matched))
		if err := readyCmd.Flags().Set("project", "gardenio"); err != nil {
			t.Fatal(err)
		}
		if err := readyCmd.Flags().Set("all-projects", "true"); err != nil {
			t.Fatal(err)
		}
		if err := readyCmd.Flags().Set("limit", "100"); err != nil {
			t.Fatal(err)
		}
		out := string(captureStdout(t, func() error { return readyCmd.RunE(readyCmd, nil) }))
		// No exclusive-project narrowing runs, so every matched row is claimable and
		// the daemon's match count is the honest total.
		want := strconv.Itoa(matched) + " beads (" + strconv.Itoa(matched) + " total)"
		if !strings.Contains(out, want) {
			t.Errorf("footer = %q, want it to contain %q (filter inactive → pre-filter total is correct)", out, want)
		}
	})
}
