package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// --- E-PROV FW-2 (ka-0000007488) GATE -------------------------------------
//
// GATE (task description): "a simulated upstream merge_request=merged payload
// closes the mirror bead + stamps mr_outcome=merged."
//
// The webhook merge path is payload-driven (reads URL/IID/state from the body,
// never calls a host-pinned client), so a real-monorepo webhook closes the
// upstream-mirror bead with NO host-aware client. This test drives a simulated
// upstream payload through GitLabWebhookHandlerWithConfig and asserts the
// linked MR mirror bead reaches closed + mr_outcome=merged.

func TestGitLabWebhookHandler_UpstreamPeelMRMerged_GATE(t *testing.T) {
	daemon := newMockGitLabDaemon()
	// The peel-request bead carries the upstream MR URL (set by
	// `gb mr link <peel-request> <upstream-mr-url>` — FW-5). On the upstream
	// host (gitlab.com), distinct from the dark host.
	upstreamMRURL := "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/777"
	daemon.beads["peel-req-1"] = &beadsapi.BeadDetail{
		ID:     "peel-req-1",
		Title:  "Peel: extract auth refactor slice",
		Type:   "task",
		Fields: map[string]string{"mr_url": upstreamMRURL},
	}

	handler := GitLabWebhookHandler(nil, daemon, "test-secret", slog.Default())

	event := map[string]any{
		"object_kind": "merge_request",
		"object_attributes": map[string]any{
			"iid":               777,
			"state":             "merged",
			"action":            "merge",
			"url":               upstreamMRURL,
			"target_project_id": 4242,
			"title":             "Peel: extract auth refactor slice",
		},
		"project": map[string]any{
			"path_with_namespace": "Acme/Platform/monorepo",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// GATE: the upstream-MR mirror bead is closed and stamped mr_outcome=merged.
	mrBead, err := daemon.MRBeadForTask(context.Background(), "peel-req-1")
	if err != nil || mrBead == nil {
		t.Fatalf("expected MR mirror bead linked to peel-request: bead=%v err=%v", mrBead, err)
	}
	if mrBead.Status != "closed" {
		t.Errorf("mirror bead status=%q, want closed", mrBead.Status)
	}
	if mrBead.Fields["mr_outcome"] != "merged" {
		t.Errorf("mirror bead mr_outcome=%q, want merged", mrBead.Fields["mr_outcome"])
	}
	if mrBead.Fields["mr_state"] != "merged" {
		t.Errorf("mirror bead mr_state=%q, want merged", mrBead.Fields["mr_state"])
	}
	if mrBead.Fields["gitlab_mr_iid"] != "777" {
		t.Errorf("mirror bead gitlab_mr_iid=%q, want 777", mrBead.Fields["gitlab_mr_iid"])
	}
}

// --- Scoped upstream backstop poller --------------------------------------

// mockUpstreamBeadClient is a tiny mock for the scoped poller's narrow bead
// surface. It records the upserts so tests can assert the close transition.
type mockUpstreamBeadClient struct {
	mu       sync.Mutex
	open     []*beadsapi.BeadDetail
	upserts  []mrUpsertRecord
	listErr  error
	scopeArg string
}

func (m *mockUpstreamBeadClient) ListOpenMergeRequestBeads(_ context.Context, label string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scopeArg = label
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.open, nil
}

func (m *mockUpstreamBeadClient) UpsertMergeRequestBead(_ context.Context, f beadsapi.MergeRequestBeadFields, closed bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upserts = append(m.upserts, mrUpsertRecord{Fields: f, Closed: closed})
	return "mr-bead-x", nil
}

func (m *mockUpstreamBeadClient) getUpserts() []mrUpsertRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mrUpsertRecord, len(m.upserts))
	copy(out, m.upserts)
	return out
}

// newUpstreamGitLabStub spins up an httptest server that answers the single
// per-MR GET the poller makes, returning the given state for any MR.
func newUpstreamGitLabStub(t *testing.T, mr GitLabMR) (*GitLabClient, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mr)
	}))
	client := NewGitLabClient(GitLabClientConfig{
		BaseURL: srv.URL,
		Token:   "stub",
		Logger:  slog.Default(),
	})
	return client, srv.Close
}

// GATE-equivalent for the backstop: a poll over an open upstream-mirror bead
// whose upstream MR is merged closes the bead via upsert(closed=true,
// mr_outcome=merged) — the same end state the webhook reaches.
func TestUpstreamBackstopPoller_MergedMR_ClosesMirrorBead(t *testing.T) {
	upstreamMRURL := "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/512"
	client, closeFn := newUpstreamGitLabStub(t, GitLabMR{
		IID:    512,
		State:  "merged",
		WebURL: upstreamMRURL,
		Title:  "Peel: cache layer",
	})
	defer closeFn()

	daemon := &mockUpstreamBeadClient{
		open: []*beadsapi.BeadDetail{
			{
				ID:     "mirror-1",
				Type:   "merge-request",
				Status: "open",
				Labels: []string{"upstream:true"},
				Fields: map[string]string{"mr_url": upstreamMRURL},
			},
		},
	}

	poller := NewGitLabUpstreamPoller(GitLabUpstreamPollerConfig{
		GitLab:     client,
		Daemon:     daemon,
		Logger:     slog.Default(),
		ScopeLabel: "upstream:true",
		Host:       "gitlab.com",
	})
	poller.poll(context.Background())

	if daemon.scopeArg != "upstream:true" {
		t.Errorf("poller listed with scope label %q, want upstream:true", daemon.scopeArg)
	}
	ups := daemon.getUpserts()
	if len(ups) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(ups))
	}
	if !ups[0].Closed {
		t.Errorf("upsert closed=false, want true (backstop must close the mirror bead)")
	}
	if ups[0].Fields.MROutcome != "merged" {
		t.Errorf("upsert mr_outcome=%q, want merged", ups[0].Fields.MROutcome)
	}
	if ups[0].Fields.MRState != "merged" {
		t.Errorf("upsert mr_state=%q, want merged", ups[0].Fields.MRState)
	}
}

// An open (non-terminal) upstream MR must NOT trigger any bead write — the
// backstop only acts on a terminal state the webhook should have delivered.
func TestUpstreamBackstopPoller_OpenMR_NoOp(t *testing.T) {
	upstreamMRURL := "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/99"
	client, closeFn := newUpstreamGitLabStub(t, GitLabMR{
		IID:    99,
		State:  "opened",
		WebURL: upstreamMRURL,
	})
	defer closeFn()

	daemon := &mockUpstreamBeadClient{
		open: []*beadsapi.BeadDetail{
			{
				ID:     "mirror-2",
				Type:   "merge-request",
				Status: "open",
				Fields: map[string]string{"mr_url": upstreamMRURL},
			},
		},
	}

	poller := NewGitLabUpstreamPoller(GitLabUpstreamPollerConfig{
		GitLab: client,
		Daemon: daemon,
		Logger: slog.Default(),
	})
	poller.poll(context.Background())

	if ups := daemon.getUpserts(); len(ups) != 0 {
		t.Errorf("expected no upsert for an open MR, got %d", len(ups))
	}
}

// Dual-GitLab safety: a mirror bead whose mr_url lives on a foreign host (the
// dark host) must be skipped — the upstream poller must never GET a dark MR
// URL against the upstream API. With Host scoping, no GitLab fetch happens, so
// even a stub that would return "merged" produces no write.
func TestUpstreamBackstopPoller_ForeignHost_Skipped(t *testing.T) {
	darkMRURL := "https://gitlab-dark.example.com/org/monorepo/-/merge_requests/4821"
	// Stub would say "merged" if ever called — but host scoping should prevent
	// the call entirely.
	client, closeFn := newUpstreamGitLabStub(t, GitLabMR{
		IID:    4821,
		State:  "merged",
		WebURL: darkMRURL,
	})
	defer closeFn()

	daemon := &mockUpstreamBeadClient{
		open: []*beadsapi.BeadDetail{
			{
				ID:     "mirror-3",
				Type:   "merge-request",
				Status: "open",
				Fields: map[string]string{"mr_url": darkMRURL},
			},
		},
	}

	poller := NewGitLabUpstreamPoller(GitLabUpstreamPollerConfig{
		GitLab: client,
		Daemon: daemon,
		Logger: slog.Default(),
		Host:   "gitlab.com", // upstream host; dark URL is foreign
	})
	poller.poll(context.Background())

	if ups := daemon.getUpserts(); len(ups) != 0 {
		t.Errorf("expected no upsert for a foreign-host (dark) MR, got %d", len(ups))
	}
}

// A closed-upstream MR collapses to mr_outcome=closed (not merged).
func TestUpstreamBackstopPoller_ClosedMR_OutcomeClosed(t *testing.T) {
	upstreamMRURL := "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/600"
	client, closeFn := newUpstreamGitLabStub(t, GitLabMR{
		IID:    600,
		State:  "closed",
		WebURL: upstreamMRURL,
	})
	defer closeFn()

	daemon := &mockUpstreamBeadClient{
		open: []*beadsapi.BeadDetail{
			{
				ID:     "mirror-4",
				Type:   "merge-request",
				Status: "open",
				Fields: map[string]string{"mr_url": upstreamMRURL},
			},
		},
	}

	poller := NewGitLabUpstreamPoller(GitLabUpstreamPollerConfig{
		GitLab: client,
		Daemon: daemon,
		Logger: slog.Default(),
		Host:   "gitlab.com",
	})
	poller.poll(context.Background())

	ups := daemon.getUpserts()
	if len(ups) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(ups))
	}
	if ups[0].Fields.MROutcome != "closed" {
		t.Errorf("upsert mr_outcome=%q, want closed", ups[0].Fields.MROutcome)
	}
}
