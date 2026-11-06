package main

import (
	"os"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestDumpThreadsHTML renders the /threads index and a populated /threads/{id}
// detail to standalone HTML files for visual-proof screenshots (rubric gate
// B10). It is NOT a CI test: it is gated behind THREADS_DUMP_DIR so it only runs
// when a human (or the screenshot harness) sets that env var; in CI the env var
// is unset and the test is skipped, so it never writes files or slows the suite.
//
//	THREADS_DUMP_DIR=/tmp/threads-shots go test ./cmd/beads-viewer/ \
//	    -run TestDumpThreadsHTML -count=1
func TestDumpThreadsHTML(t *testing.T) {
	dir := os.Getenv("THREADS_DUMP_DIR")
	if dir == "" {
		t.Skip("THREADS_DUMP_DIR unset; skipping visual-proof dump (not a CI test)")
	}

	now := time.Now()

	// Index fixture: a spread of wish states so the index card variants all show
	// (active wish with work, fulfilled wish, needs-triage wish, head-only wish).
	fIdx := newFake()
	fIdx.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish-onboard": {
			ID: "gd-wish-onboard", Title: "Make onboarding smoother", Status: "in_progress",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			Fields:    map[string]string{threadRootTextField: "onboarding takes too many steps, please simplify"},
			UpdatedAt: now.Add(-8 * time.Minute), CreatedAt: now.Add(-3 * time.Hour),
		},
		"gd-wish-csv": {
			ID: "gd-wish-csv", Title: "Add CSV export to reports", Status: "closed",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			Fields:    map[string]string{threadRootTextField: "I want to download my reports as CSV"},
			UpdatedAt: now.Add(-2 * time.Hour), CreatedAt: now.Add(-2 * 24 * time.Hour),
		},
		"gd-wish-deploy": {
			ID: "gd-wish-deploy", Title: "Fix the flaky deploy", Status: "needs_triage",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			Fields:    map[string]string{threadRootTextField: "the deploy keeps failing halfway and nobody knows why"},
			UpdatedAt: now.Add(-40 * time.Minute), CreatedAt: now.Add(-6 * time.Hour),
		},
		"gd-wish-darkmode": {
			ID: "gd-wish-darkmode", Title: "Add a dark mode", Status: "open",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			UpdatedAt: now.Add(-90 * time.Minute), CreatedAt: now.Add(-90 * time.Minute),
		},
		// Members lifting the onboarding wish's work tally (bound by thread_id).
		"gd-onb-1": {ID: "gd-onb-1", Title: "Cut signup to 2 fields", Status: "closed",
			Fields: map[string]string{threadIDField: "gd-wish-onboard"}, UpdatedAt: now.Add(-20 * time.Minute)},
		"gd-onb-2": {ID: "gd-onb-2", Title: "Autodetect timezone", Status: "open",
			Fields: map[string]string{threadIDField: "gd-wish-onboard"}, UpdatedAt: now.Add(-8 * time.Minute)},
		"gd-onb-3": {ID: "gd-onb-3", Title: "Pre-fill org from email domain", Status: "in_progress",
			Fields: map[string]string{threadIDField: "gd-wish-onboard"}, UpdatedAt: now.Add(-15 * time.Minute)},
		// One member under the triage wish.
		"gd-dep-1": {ID: "gd-dep-1", Title: "Investigate deploy timeout", Status: "failed",
			Fields: map[string]string{threadIDField: "gd-wish-deploy"}, UpdatedAt: now.Add(-40 * time.Minute)},
	}
	dumpPage(t, fIdx, "/threads", dir+"/threads-index.html")

	// Detail fixture: a wish with a real conversation AND a fleet of agent work,
	// so the screenshot shows the curation default — conversation leading, work
	// collapsed behind the toggle.
	fDet := newFake()
	fDet.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish-onboard": {
			ID: "gd-wish-onboard", Title: "Make onboarding smoother", Status: "in_progress",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			Fields:    map[string]string{threadRootTextField: "Onboarding takes too many steps — the signup form is huge and the timezone guess is always wrong. Can we make the first run feel effortless?"},
			CreatedBy: "alice", CreatedAt: now.Add(-3 * time.Hour),
		},
		"gd-onb-1": {ID: "gd-onb-1", Title: "Cut the signup form to 2 fields", Status: "closed",
			Fields: map[string]string{threadIDField: "gd-wish-onboard"}, CreatedBy: "agent-alpha",
			CreatedAt: now.Add(-2 * time.Hour), ClosedBy: "agent-alpha", ClosedAt: now.Add(-70 * time.Minute).Format(time.RFC3339)},
		"gd-onb-2": {ID: "gd-onb-2", Title: "Autodetect timezone from browser", Status: "open",
			Fields: map[string]string{threadIDField: "gd-wish-onboard"}, CreatedBy: "agent-beta", CreatedAt: now.Add(-90 * time.Minute)},
		"gd-onb-3": {ID: "gd-onb-3", Title: "Pre-fill org from email domain", Status: "in_progress",
			Fields: map[string]string{threadIDField: "gd-wish-onboard"}, CreatedBy: "agent-gamma", CreatedAt: now.Add(-50 * time.Minute)},
	}
	fDet.revDeps = map[string][]beadsapi.Dependency{
		"gd-wish-onboard": {
			{BeadID: "gd-onb-1", DependsOnID: "gd-wish-onboard", Type: "parent-child"},
			{BeadID: "gd-onb-2", DependsOnID: "gd-wish-onboard", Type: "parent-child"},
			{BeadID: "gd-onb-3", DependsOnID: "gd-wish-onboard", Type: "parent-child"},
		},
	}
	fDet.comments = map[string][]beadsapi.Comment{
		"gd-wish-onboard": {
			{ID: 1, Author: "alice", Text: "The timezone guess is the single worst part — it's wrong every single time.", CreatedAt: now.Add(-2*time.Hour - 30*time.Minute)},
			{ID: 2, Author: "bob", Text: "Agreed, that one bit me too. And the signup form asking for a fax number in 2026 is wild.", CreatedAt: now.Add(-2 * time.Hour)},
			{ID: 3, Author: "alice", Text: "Cutting it to email + password would already be a huge win. Thanks for picking this up!", CreatedAt: now.Add(-80 * time.Minute)},
			// A markdown-rich reply so the visual dump exercises markdown parity
			// (code spans, links, lists) — the same goldmark treatment a bead
			// comment gets (rubric (b), ka-0000001087).
			{ID: 4, Author: "gardenio-worker", Text: "Patch up. Run `make test` to verify, then see the [signup PR](https://example.com/mr/123).\n\n- email + password only\n- timezone autodetected from the browser\n- org pre-filled from the email domain", CreatedAt: now.Add(-40 * time.Minute)},
		},
	}
	dumpPage(t, fDet, "/threads/gd-wish-onboard", dir+"/threads-detail.html")
}

// dumpPage renders a viewer page through the real mux and writes the HTML to
// path, so a headless browser can screenshot the live template output.
func dumpPage(t *testing.T, f *fakeDaemon, urlPath, outPath string) {
	t.Helper()
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, urlPath)
	if code != 200 {
		t.Fatalf("GET %s = %d, want 200", urlPath, code)
	}
	if err := os.WriteFile(outPath, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", outPath, err)
	}
	t.Logf("wrote %d bytes to %s", len(body), outPath)
}
