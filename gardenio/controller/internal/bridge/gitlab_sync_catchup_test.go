package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeListClient implements GitLabSyncCatchUpClient for the description
// catch-up tests. It records the query it received so tests can assert the
// server-side filter (type=task, active statuses).
type fakeListClient struct {
	beads   []*beadsapi.BeadDetail
	err     error
	lastQry beadsapi.ListBeadsQuery
}

func (f *fakeListClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.lastQry = q
	if f.err != nil {
		return nil, f.err
	}
	return &beadsapi.ListBeadsResult{Beads: f.beads, Total: len(f.beads)}, nil
}

// TestGitLabSync_CatchUpDescriptions_SyncsPreExistingMR is the regression guard
// for bug ka-0000002703 Finding 2: a task bead that carried mr_url before boot
// (no SSE event) still gets its managed description section PUT on startup.
func TestGitLabSync_CatchUpDescriptions_SyncsPreExistingMR(t *testing.T) {
	var (
		mu      sync.Mutex
		puts    int
		gotDesc string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(GitLabMR{IID: 7, Description: "human text"})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		puts++
		gotDesc = body["description"]
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	daemon := newMockGitLabDaemon()
	s := NewGitLabSync(GitLabSyncConfig{
		GitLab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"}),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	client := &fakeListClient{
		beads: []*beadsapi.BeadDetail{
			{
				ID:    "bead-mr",
				Type:  "task",
				Title: "Fix thing",
				Fields: map[string]string{
					"mr_url":   "https://gitlab.com/org/repo/-/merge_requests/7",
					"jira_key": "PE-42",
				},
			},
			{
				ID:     "bead-no-mr", // no mr_url — must be skipped
				Type:   "task",
				Fields: map[string]string{},
			},
		},
	}

	s.CatchUpDescriptions(context.Background(), client, 0)

	mu.Lock()
	defer mu.Unlock()
	if puts != 1 {
		t.Fatalf("expected exactly 1 description PUT (only the bead with mr_url), got %d", puts)
	}
	if !strings.Contains(gotDesc, "PE-42") {
		t.Errorf("rendered description should reference jira_key PE-42, got: %q", gotDesc)
	}
	// The catch-up query must scope to bead-linked types (task/bug/feature) in
	// active statuses — bug/feature beads carry MRs in sibling repos and must be
	// covered too (Gap A, task ka-0000004869).
	gotTypes := map[string]bool{}
	for _, ty := range client.lastQry.Types {
		gotTypes[ty] = true
	}
	for _, want := range []string{"task", "bug", "feature"} {
		if !gotTypes[want] {
			t.Errorf("expected query Types to include %q, got %v", want, client.lastQry.Types)
		}
	}
	if len(client.lastQry.Statuses) == 0 {
		t.Errorf("expected active-status filter on catch-up query, got none")
	}
}

// TestGitLabSync_CatchUpDescriptions_BackfillsBugAndFeature is the Gap A
// regression guard (task ka-0000004869): a bead-linked MR carried by a type=bug
// or type=feature bead (its MR living in a sibling group repo) must receive the
// managed "Agent Context" section on boot, not just type=task beads.
func TestGitLabSync_CatchUpDescriptions_BackfillsBugAndFeature(t *testing.T) {
	var (
		mu    sync.Mutex
		descs []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// No gardenio marker in the existing description — models the defect.
			_ = json.NewEncoder(w).Encode(GitLabMR{IID: 1234, Description: "human text, no marker"})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		descs = append(descs, body["description"])
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	s := NewGitLabSync(GitLabSyncConfig{
		GitLab:        NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"}),
		Daemon:        newMockGitLabDaemon(),
		Logger:        slog.Default(),
		ViewerBaseURL: "https://viewer.example/beads",
	})
	client := &fakeListClient{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "bug-bead",
				Type:   "bug",
				Fields: map[string]string{"mr_url": "https://gitlab.com/org/site-deployment/-/merge_requests/1234"},
			},
			{
				ID:     "feature-bead",
				Type:   "feature",
				Fields: map[string]string{"mr_url": "https://gitlab.com/org/other/-/merge_requests/9"},
			},
		},
	}

	s.CatchUpDescriptions(context.Background(), client, 0)

	mu.Lock()
	defer mu.Unlock()
	if len(descs) != 2 {
		t.Fatalf("expected 2 description PUTs (one per bug/feature bead), got %d", len(descs))
	}
	for _, d := range descs {
		if !strings.Contains(d, gardenioMarkerStart) {
			t.Errorf("back-filled description must contain the gardenio marker, got: %q", d)
		}
	}
	// The clickable bead deep-link must be present for the bug bead.
	joined := strings.Join(descs, "\n")
	if !strings.Contains(joined, "bug-bead") || !strings.Contains(joined, "feature-bead") {
		t.Errorf("expected both bead ids deep-linked in rendered sections, got: %q", joined)
	}
}

// TestGitLabSync_CatchUpDescriptions_Idempotent verifies the catch-up is
// idempotent across boots: the first boot PUTs the managed section, and a second
// boot (a fresh GitLabSync, so the in-memory dedup map is empty) sees the
// managed block already present in the MR description and issues NO further PUT.
// This is the strongest form of the Finding-2 safety guarantee — re-running on
// every boot never corrupts or churns the external MR description.
func TestGitLabSync_CatchUpDescriptions_Idempotent(t *testing.T) {
	var (
		mu      sync.Mutex
		descs   []string // PUT bodies
		curDesc = "human text"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Return the current stored description so the second boot sees the
			// managed block written by the first boot (models real GitLab state).
			mu.Lock()
			d := curDesc
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(GitLabMR{IID: 7, Description: d})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		descs = append(descs, body["description"])
		curDesc = body["description"] // persist so subsequent GETs reflect the write
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	beads := []*beadsapi.BeadDetail{
		{
			ID:    "bead-mr",
			Type:  "task",
			Title: "Fix thing",
			Fields: map[string]string{
				"mr_url":   "https://gitlab.com/org/repo/-/merge_requests/7",
				"jira_key": "PE-42",
			},
		},
	}

	run := func() {
		s := NewGitLabSync(GitLabSyncConfig{
			GitLab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"}),
			Daemon: newMockGitLabDaemon(),
			Logger: slog.Default(),
		})
		s.CatchUpDescriptions(context.Background(), &fakeListClient{beads: beads}, 0)
	}
	run()
	run()

	mu.Lock()
	defer mu.Unlock()
	// First boot writes once; second boot sees the unchanged managed block and
	// skips the PUT (syncMRDescription's existingContent==newContent guard).
	if len(descs) != 1 {
		t.Fatalf("expected exactly 1 PUT across two boots (second boot no-ops on unchanged section), got %d", len(descs))
	}
	if !strings.Contains(descs[0], "PE-42") {
		t.Errorf("rendered description should reference jira_key PE-42, got: %q", descs[0])
	}
}

// TestGitLabSync_CatchUpDescriptions_Dedup verifies the second call within the
// dedup window (same GitLabSync instance) issues no additional PUT.
func TestGitLabSync_CatchUpDescriptions_Dedup(t *testing.T) {
	var (
		mu   sync.Mutex
		puts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(GitLabMR{IID: 7, Description: "human text"})
			return
		}
		mu.Lock()
		puts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	s := NewGitLabSync(GitLabSyncConfig{
		GitLab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"}),
		Daemon: newMockGitLabDaemon(),
		Logger: slog.Default(),
	})
	client := &fakeListClient{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "bead-mr",
				Type:   "task",
				Fields: map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/7"},
			},
		},
	}

	s.CatchUpDescriptions(context.Background(), client, 0)
	s.CatchUpDescriptions(context.Background(), client, 0)

	mu.Lock()
	defer mu.Unlock()
	if puts != 1 {
		t.Errorf("expected 1 PUT (second deduped within TTL), got %d", puts)
	}
}

// TestGitLabSync_CatchUpDescriptions_NilAndError covers the defensive paths.
func TestGitLabSync_CatchUpDescriptions_NilAndError(t *testing.T) {
	s := NewGitLabSync(GitLabSyncConfig{
		GitLab: NewGitLabClient(GitLabClientConfig{BaseURL: "https://example.com", Token: "t"}),
		Daemon: newMockGitLabDaemon(),
		Logger: slog.Default(),
	})
	// nil client → no-op, no panic.
	s.CatchUpDescriptions(context.Background(), nil, 0)
	// list error → swallowed, no panic.
	s.CatchUpDescriptions(context.Background(), &fakeListClient{err: errors.New("boom")}, 0)

	// nil GitLab client → no-op even with beads present.
	s2 := NewGitLabSync(GitLabSyncConfig{Daemon: newMockGitLabDaemon(), Logger: slog.Default()})
	s2.CatchUpDescriptions(context.Background(), &fakeListClient{
		beads: []*beadsapi.BeadDetail{{ID: "b", Type: "task", Fields: map[string]string{"mr_url": "https://gitlab.com/o/r/-/merge_requests/1"}}},
	}, 0)
}

// TestGitLabSync_CatchUpDescriptions_HostScope verifies MRs on a foreign GitLab
// host are skipped when the bridge is host-scoped (FW-1).
func TestGitLabSync_CatchUpDescriptions_HostScope(t *testing.T) {
	var (
		mu   sync.Mutex
		puts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(GitLabMR{IID: 7, Description: "human"})
			return
		}
		mu.Lock()
		puts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	s := NewGitLabSync(GitLabSyncConfig{
		GitLab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"}),
		Daemon: newMockGitLabDaemon(),
		Logger: slog.Default(),
		Host:   "other-host.example.com",
	})
	client := &fakeListClient{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "bead-foreign",
				Type:   "task",
				Fields: map[string]string{"mr_url": "https://gitlab.com/o/r/-/merge_requests/7"},
			},
		},
	}
	s.CatchUpDescriptions(context.Background(), client, 0)

	mu.Lock()
	defer mu.Unlock()
	if puts != 0 {
		t.Errorf("expected 0 PUTs for foreign-host MR under host scope, got %d", puts)
	}
}
