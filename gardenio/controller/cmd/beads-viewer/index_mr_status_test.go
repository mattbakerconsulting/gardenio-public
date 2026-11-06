package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// fakeMRLister is a deterministic mrLister for tests. fixedOpened /
// fixedMerged are returned in lockstep with the State filter; openedErr /
// mergedErr drive the failure-path branches.
type fakeMRLister struct {
	mu           sync.Mutex
	fixedOpened  []bridge.GitLabMR
	openedErr    error
	fixedMerged  []bridge.GitLabMR
	mergedErr    error
	callsOpened  int
	callsMerged  int
	lastOpenedTo time.Time
	lastMergedTo time.Time
}

func (f *fakeMRLister) ListMergeRequests(_ context.Context, _ string, opts bridge.MRListOptions) ([]bridge.GitLabMR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch opts.State {
	case "opened":
		f.callsOpened++
		f.lastOpenedTo = opts.UpdatedAfter
		return f.fixedOpened, f.openedErr
	case "merged":
		f.callsMerged++
		f.lastMergedTo = opts.UpdatedAfter
		return f.fixedMerged, f.mergedErr
	}
	return nil, nil
}

func (f *fakeMRLister) openedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callsOpened
}

func (f *fakeMRLister) mergedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callsMerged
}

// mrPtr is a one-liner pointer-to-string helper used for MergedAt fields.
func mrPtr(s string) *string { return &s }

// newMRRailServer builds a Server wired with the given MR lister plus an
// empty fake daemon, returns the mux and the underlying server so tests can
// assert cache behavior.
func newMRRailServer(t *testing.T, lister mrLister, now func() time.Time) (*http.ServeMux, *Server) {
	t.Helper()
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	srv := buildServerForTest(t, f)
	srv.SetNow(now)
	srv.SetMRLister(lister, "Acme/Platform/gardenio", "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux, srv
}

// buildServerForTest mirrors mkServer but returns the *Server (mkServer
// returns the mux only). The standard helper is unchanged; this one is
// dedicated to the MR-status panel tests so we can poke private fields.
func buildServerForTest(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	return NewServer(daemon, slog.Default(), "", "", "", nil)
}

// TestIndexMRStatus_Counts asserts the four count derivations from a fixed
// GitLab response: open (all opened), mergeable (opened+non-draft+
// can_be_merged), drafts (opened+draft), merged today (state=merged).
func TestIndexMRStatus_Counts(t *testing.T) {
	now := time.Date(2026, 5, 15, 14, 0, 0, 0, time.UTC)
	midnight := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	lister := &fakeMRLister{
		fixedOpened: []bridge.GitLabMR{
			{IID: 1, Title: "ready", Draft: false, MergeStatus: "can_be_merged"},
			{IID: 2, Title: "also ready", Draft: false, MergeStatus: "can_be_merged"},
			{IID: 3, Title: "conflict", Draft: false, MergeStatus: "cannot_be_merged", HasConflicts: true},
			{IID: 4, Title: "wip", Draft: true},
			{IID: 5, Title: "wip 2", Draft: true},
		},
		fixedMerged: []bridge.GitLabMR{
			{IID: 100, Title: "merged today A", MergedAt: mrPtr(midnight.Add(2 * time.Hour).Format(time.RFC3339))},
			{IID: 101, Title: "merged today B", MergedAt: mrPtr(midnight.Add(8 * time.Hour).Format(time.RFC3339))},
			{IID: 102, Title: "merged yesterday (stale updated_at)", MergedAt: mrPtr(midnight.Add(-3 * time.Hour).Format(time.RFC3339))},
		},
	}
	mux, _ := newMRRailServer(t, lister, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Merge requests",
		`Acme/Platform/gardenio`,
		`<span class="index-rail-mrstatus-num">5</span>`, // open total
		`<span class="index-rail-mrstatus-num">2</span>`, // mergeable
		`<span class="index-rail-mrstatus-num">2</span>`, // drafts (same count as mergeable here; both should appear)
		`<span class="index-rail-mrstatus-num">2</span>`, // merged today (the yesterday merged_at is filtered out)
		">open<",
		">approved<",
		">needs review<",
		">merged today<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("MR-status panel missing %q\nbody=%s", want, body)
		}
	}
}

// TestIndexMRStatus_HardError verifies an opened-fetch error renders the
// Err message rather than blanking the panel.
func TestIndexMRStatus_HardError(t *testing.T) {
	now := time.Date(2026, 5, 15, 14, 0, 0, 0, time.UTC)
	lister := &fakeMRLister{openedErr: errors.New("boom")}
	mux, _ := newMRRailServer(t, lister, func() time.Time { return now })
	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Couldn&#39;t reach GitLab.") && !strings.Contains(body, "Couldn't reach GitLab.") {
		t.Errorf("expected error banner, body=%s", body)
	}
}

// TestIndexMRStatus_NotConfigured asserts the panel is HIDDEN entirely when no
// lister or project path is wired (ka-0000005241): a permanent "GitLab not
// configured." empty on flagship Home real estate was the UX defect, so an
// unconfigured deploy simply drops the panel rather than rendering a dead card.
func TestIndexMRStatus_NotConfigured(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	// Scope to markup after the last </style> so the CSS rules that legitimately
	// name .index-rail-mrstatus don't false-positive (same body-only convention
	// as the measure-home audit tool).
	rendered := body
	if i := strings.LastIndex(body, "</style>"); i >= 0 {
		rendered = body[i:]
	}
	for _, dontWant := range []string{
		"GitLab not configured.",
		`class="index-rail-panel index-rail-mrstatus"`,
	} {
		if strings.Contains(rendered, dontWant) {
			t.Errorf("unconfigured MR-status panel should be hidden but rendered body still contains %q\nbody=%s", dontWant, body)
		}
	}
}

// TestIndexMRStatus_CacheCollapsesBurst asserts the 60s TTL: two requests
// within the window hit GitLab once; a third request past the TTL refetches.
func TestIndexMRStatus_CacheCollapsesBurst(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 14, 0, 0, 0, time.UTC)
	clock := t0
	lister := &fakeMRLister{
		fixedOpened: []bridge.GitLabMR{{IID: 1, Draft: false, MergeStatus: "can_be_merged"}},
		fixedMerged: nil,
	}
	mux, _ := newMRRailServer(t, lister, func() time.Time { return clock })

	for i := range 2 {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/index", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}
	if got := lister.openedCalls(); got != 1 {
		t.Errorf("expected 1 opened call (cached), got %d", got)
	}
	if got := lister.mergedCalls(); got != 1 {
		t.Errorf("expected 1 merged call (cached), got %d", got)
	}

	clock = t0.Add(mrStatusCacheTTL + time.Second)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/index", nil))
	if got := lister.openedCalls(); got != 2 {
		t.Errorf("expected 2 opened calls after TTL, got %d", got)
	}
}
