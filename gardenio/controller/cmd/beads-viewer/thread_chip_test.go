package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// chipServer builds a Server whose daemon is a fakeDaemon seeded with the
// given beads, for direct buildThreadChip unit tests. basePath is threaded
// through so the Href assertion exercises the same join the handler uses.
func chipServer(t *testing.T, basePath string, beads map[string]*beadsapi.BeadDetail) *Server {
	t.Helper()
	f := &fakeDaemon{beads: beads}
	// NewServer signature: (daemon, logger, basePath, beadsHTTPAddr, token, coopmux).
	return NewServer(f, slog.Default(), basePath, "", "", nil)
}

func TestBuildThreadChip_NilBead(t *testing.T) {
	s := chipServer(t, "", nil)
	if chip := s.buildThreadChip(context.Background(), nil); chip != nil {
		t.Fatalf("expected nil chip for nil bead, got %+v", chip)
	}
}

func TestBuildThreadChip_NoThreadID(t *testing.T) {
	// A bead with no thread_id field is either a thread head (it IS the wish)
	// or a bead never bound to a thread. Either way: no chip.
	member := &beadsapi.BeadDetail{ID: "gd-member", Title: "Some task", Fields: map[string]string{}}
	s := chipServer(t, "", map[string]*beadsapi.BeadDetail{"gd-member": member})
	if chip := s.buildThreadChip(context.Background(), member); chip != nil {
		t.Fatalf("expected nil chip when thread_id absent, got %+v", chip)
	}
}

func TestBuildThreadChip_EmptyThreadID(t *testing.T) {
	// A blank/whitespace thread_id must be treated as absent, not as a link
	// to "/threads/".
	member := &beadsapi.BeadDetail{ID: "gd-member", Fields: map[string]string{threadIDField: "   "}}
	s := chipServer(t, "", map[string]*beadsapi.BeadDetail{"gd-member": member})
	if chip := s.buildThreadChip(context.Background(), member); chip != nil {
		t.Fatalf("expected nil chip for blank thread_id, got %+v", chip)
	}
}

func TestBuildThreadChip_SelfPointer(t *testing.T) {
	// Defensive: a bead whose thread_id points at itself must not render a
	// self-referential chip.
	head := &beadsapi.BeadDetail{ID: "gd-wish", Fields: map[string]string{threadIDField: "gd-wish"}}
	s := chipServer(t, "", map[string]*beadsapi.BeadDetail{"gd-wish": head})
	if chip := s.buildThreadChip(context.Background(), head); chip != nil {
		t.Fatalf("expected nil chip for self-referential thread_id, got %+v", chip)
	}
}

func TestBuildThreadChip_ResolvedHeadTitle(t *testing.T) {
	head := &beadsapi.BeadDetail{
		ID:    "gd-wish",
		Title: "Slack thread: ship the dashboard",
	}
	member := &beadsapi.BeadDetail{
		ID:     "gd-member",
		Title:  "Build the chart panel",
		Fields: map[string]string{threadIDField: "gd-wish"},
	}
	s := chipServer(t, "/beads", map[string]*beadsapi.BeadDetail{
		"gd-wish":   head,
		"gd-member": member,
	})
	chip := s.buildThreadChip(context.Background(), member)
	if chip == nil {
		t.Fatal("expected a chip for a bead carrying thread_id")
	}
	if chip.ThreadID != "gd-wish" {
		t.Errorf("ThreadID = %q, want gd-wish", chip.ThreadID)
	}
	if !chip.Resolved {
		t.Errorf("expected Resolved=true when head title fetched")
	}
	if chip.Title != "Slack thread: ship the dashboard" {
		t.Errorf("Title = %q, want head title", chip.Title)
	}
	if want := "/beads/threads/gd-wish"; chip.Href != want {
		t.Errorf("Href = %q, want %q", chip.Href, want)
	}
}

func TestBuildThreadChip_PrefersSlackRootText(t *testing.T) {
	// threadWishText prefers slack_root_text (the verbatim wish) over the
	// head title — the chip must show the clearest "what was asked for".
	head := &beadsapi.BeadDetail{
		ID:     "gd-wish",
		Title:  "Slack thread C123/456",
		Fields: map[string]string{threadRootTextField: "please make the costs page load faster"},
	}
	member := &beadsapi.BeadDetail{
		ID:     "gd-member",
		Fields: map[string]string{threadIDField: "gd-wish"},
	}
	s := chipServer(t, "", map[string]*beadsapi.BeadDetail{
		"gd-wish":   head,
		"gd-member": member,
	})
	chip := s.buildThreadChip(context.Background(), member)
	if chip == nil || !chip.Resolved {
		t.Fatalf("expected a resolved chip, got %+v", chip)
	}
	if chip.Title != "please make the costs page load faster" {
		t.Errorf("Title = %q, want the slack_root_text wish", chip.Title)
	}
}

func TestBuildThreadChip_LongTitleClipped(t *testing.T) {
	// 300 chars with a trailing space; threadWishText trims it to 299.
	long := strings.Repeat("wish ", 60)
	wantFull := strings.TrimSpace(long)
	head := &beadsapi.BeadDetail{ID: "gd-wish", Title: long}
	member := &beadsapi.BeadDetail{ID: "gd-member", Fields: map[string]string{threadIDField: "gd-wish"}}
	s := chipServer(t, "", map[string]*beadsapi.BeadDetail{"gd-wish": head, "gd-member": member})
	chip := s.buildThreadChip(context.Background(), member)
	if chip == nil {
		t.Fatal("expected a chip")
	}
	if !strings.HasSuffix(chip.Title, "…") {
		t.Errorf("expected clipped Title to end with ellipsis, got %q", chip.Title)
	}
	if n := len([]rune(chip.Title)); n > threadChipTitleLimit+1 {
		t.Errorf("clipped Title is %d runes, want <= %d", n, threadChipTitleLimit+1)
	}
	// FullTitle preserves the (trimmed-but-unclipped) wish for the hover attribute.
	if chip.FullTitle != wantFull {
		t.Errorf("FullTitle should be unclipped; got len %d want len %d", len(chip.FullTitle), len(wantFull))
	}
}

func TestBuildThreadChip_UnresolvedHeadFallsBackToID(t *testing.T) {
	// The head bead is missing (deleted / not in the daemon). The chip still
	// links correctly but falls back to the bare id as its label and reports
	// Resolved=false so the template can soften the wording.
	member := &beadsapi.BeadDetail{ID: "gd-member", Fields: map[string]string{threadIDField: "gd-gone"}}
	s := chipServer(t, "", map[string]*beadsapi.BeadDetail{"gd-member": member})
	chip := s.buildThreadChip(context.Background(), member)
	if chip == nil {
		t.Fatal("expected a chip even when the head is unresolved")
	}
	if chip.Resolved {
		t.Errorf("expected Resolved=false for a missing head")
	}
	if chip.Title != "gd-gone" || chip.ThreadID != "gd-gone" {
		t.Errorf("expected fallback to bare id, got Title=%q ThreadID=%q", chip.Title, chip.ThreadID)
	}
	if want := "/threads/gd-gone"; chip.Href != want {
		t.Errorf("Href = %q, want %q", chip.Href, want)
	}
}

func TestBuildThreadChip_DaemonErrorFallsBack(t *testing.T) {
	// A hard daemon error resolving the head must degrade to an unresolved
	// chip, not fail the page.
	member := &beadsapi.BeadDetail{ID: "gd-member", Fields: map[string]string{threadIDField: "gd-wish"}}
	f := &fakeDaemon{
		beads:      map[string]*beadsapi.BeadDetail{"gd-member": member},
		getBeadErr: errors.New("daemon down"),
	}
	s := NewServer(f, slog.Default(), "", "", "", nil)
	chip := s.buildThreadChip(context.Background(), member)
	if chip == nil {
		t.Fatal("expected a chip despite daemon error")
	}
	if chip.Resolved {
		t.Errorf("expected Resolved=false on daemon error")
	}
	if chip.ThreadID != "gd-wish" || chip.Href != "/threads/gd-wish" {
		t.Errorf("link should still be correct, got ThreadID=%q Href=%q", chip.ThreadID, chip.Href)
	}
}

func TestClipThreadChipTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short unchanged", "hello world", "hello world"},
		{"exact limit unchanged", strings.Repeat("a", threadChipTitleLimit), strings.Repeat("a", threadChipTitleLimit)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clipThreadChipTitle(tc.in); got != tc.want {
				t.Errorf("clipThreadChipTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// Over-limit input is clipped at a word boundary with an ellipsis and
	// never exceeds the rune cap + the ellipsis.
	over := strings.Repeat("ab ", 50) // 150 chars
	got := clipThreadChipTitle(over)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
	if n := len([]rune(got)); n > threadChipTitleLimit+1 {
		t.Errorf("clipped to %d runes, want <= %d", n, threadChipTitleLimit+1)
	}
}

// --- bead-show render integration -----------------------------------------

// TestBeadShow_RendersThreadChip pins the end-to-end render: a member bead
// (carrying thread_id) shows a clickable "part of wish" chip linking to the
// wish's /threads detail.
func TestBeadShow_RendersThreadChip(t *testing.T) {
	head := &beadsapi.BeadDetail{
		ID:    "gd-wish",
		Title: "Make the costs page faster",
		Kind:  "issue",
		Type:  "task",
	}
	member := &beadsapi.BeadDetail{
		ID:     "gd-member",
		Title:  "Optimize the rollup query",
		Kind:   "issue",
		Type:   "task",
		Status: "open",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{threadIDField: "gd-wish"},
	}
	f := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-wish":   head,
		"gd-member": member,
	}}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/gd-member", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="thread-wish-chip"`) {
		t.Errorf("expected the thread-wish-chip on a member bead-show page")
	}
	if !strings.Contains(body, `href="/threads/gd-wish"`) {
		t.Errorf("expected the chip to link to /threads/gd-wish, body had no such href")
	}
	if !strings.Contains(body, "part of wish:") {
		t.Errorf("expected the 'part of wish:' label")
	}
	if !strings.Contains(body, "Make the costs page faster") {
		t.Errorf("expected the resolved wish title in the chip")
	}
}

// TestBeadShow_NoThreadChipWhenNoThreadID pins that a bead with no thread_id
// (e.g. a thread head, or any non-member bead) does NOT render the chip — the
// rubric gate: "the chip only renders when thread_id is present".
func TestBeadShow_NoThreadChipWhenNoThreadID(t *testing.T) {
	// newFake's gd-aaa carries no thread_id.
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Assert on the rendered element (class="…"), not the bare class token,
	// which also appears in layout.html's <style> block.
	if strings.Contains(w.Body.String(), `class="thread-wish-chip"`) {
		t.Errorf("expected NO thread-wish-chip element on a bead without thread_id")
	}
}

// TestBeadShow_ThreadHeadShowsNoChip pins that the wish head itself — which
// carries NO thread_id (it IS the wish) — never renders a self-link chip even
// when other beads point at it.
func TestBeadShow_ThreadHeadShowsNoChip(t *testing.T) {
	head := &beadsapi.BeadDetail{
		ID:     "gd-wish",
		Title:  "The wish head",
		Kind:   "issue",
		Type:   "task",
		Labels: []string{threadHeadLabel},
	}
	f := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-wish": head}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-wish", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `class="thread-wish-chip"`) {
		t.Errorf("expected NO chip element on the thread head itself")
	}
}

// TestNav_ThreadsLinkPresent pins the rubric gate 14: /threads is reachable
// from the primary nav (rendered on every page via layout.html).
func TestNav_ThreadsLinkPresent(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The nav link points at /threads and carries the "Threads" label.
	if !strings.Contains(body, `href="/threads"`) {
		t.Errorf("expected a nav link to /threads")
	}
	navIdx := strings.Index(body, `href="/threads"`)
	if navIdx < 0 || !strings.Contains(body[navIdx:min(navIdx+80, len(body))], "Threads") {
		t.Errorf("expected the /threads nav link to be labelled Threads")
	}
}

// TestThreadChipSnapshot writes the fully-rendered, CSS-inlined bead-show page
// for a member bead (carrying thread_id) to disk when THREAD_CHIP_SNAPSHOT
// names an output path. It is a render harness for the rubric's required
// VISUAL screenshot (gate B10 / B13 / B14): the same handler + templates the
// deployed viewer uses, so the screenshot is faithful. Skipped in normal CI
// runs (no env var set) — it asserts nothing, it only emits HTML.
func TestThreadChipSnapshot(t *testing.T) {
	out := os.Getenv("THREAD_CHIP_SNAPSHOT")
	if out == "" {
		t.Skip("set THREAD_CHIP_SNAPSHOT=<path> to emit the rendered bead-show HTML")
	}
	head := &beadsapi.BeadDetail{
		ID:    "gd-wish42",
		Title: "Make the costs page load under 1s",
		Kind:  "issue",
		Type:  "task",
		Fields: map[string]string{
			threadRootTextField: "the costs page takes 6s to load, can we make it fast",
		},
	}
	member := &beadsapi.BeadDetail{
		ID:          "gd-member7",
		Title:       "Optimize the costs rollup query",
		Kind:        "issue",
		Type:        "task",
		Status:      "in_progress",
		Assignee:    "gardenio-perf-crew",
		Priority:    2,
		Labels:      []string{"project:gardenio"},
		Description: "Add a covering index and batch the per-row sub-queries.",
		Fields:      map[string]string{threadIDField: "gd-wish42"},
	}
	f := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-wish42":  head,
		"gd-member7": member,
	}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-member7", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if err := os.WriteFile(out, w.Body.Bytes(), 0o644); err != nil {
		t.Fatalf("writing snapshot: %v", err)
	}
	t.Logf("wrote %d bytes of rendered bead-show HTML to %s", w.Body.Len(), out)
}
