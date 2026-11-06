package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// gitlab_dark_hostscope_test.go is the FW-1 GATE (E-PROV #16 / ka-0000006221):
// the gitlab-dark bridge deployment.
//
// FW-1 stands up a SECOND gitlab-bridge pointed at gitlab-dark alongside the
// existing prod bridge. Both connect to the SAME gardenio beads daemon and
// subscribe to the SAME SSE stream (R2 artifact §7 "Dual-GitLab structural
// note" + §9 decision #2 = two bridge deployments). The load-bearing
// invariant that makes that safe — and the thing #17-#22 build on — is host
// scoping: each bridge mints `produces` edges / type=merge-request beads ONLY
// for MR URLs on its own GitLab host, and ignores the other host's mr_urls on
// the shared stream.
//
// GATE (from the bead): "a dark MR linked via gb mr link produces a dark-MR
// bead + produces edge". This test proves exactly that against a dark-scoped
// GitLabSync, AND proves the dark bridge does NOT act on an upstream MR URL
// (which would query gitlab-dark for an upstream IID and corrupt state), AND
// the symmetric prod-bridge case. The default (host="") match-all behavior is
// covered too, so existing single-bridge deployments are provably unchanged.

const (
	darkHost     = "gitlab-dark.gardenio-e2e.app.e2e.dev.acme.example"
	upstreamHost = "gitlab.com"
)

// newHostScopedSync builds a GitLabSync whose GitLab client serves the given
// MR JSON and whose host scope is set to scopeHost. The httptest server stands
// in for the GitLab instance so the test is fully hermetic.
func newHostScopedSync(t *testing.T, scopeHost string, daemon GitLabBeadClient) *GitLabSync {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitLabMR{
			IID:          4821,
			State:        "opened",
			Title:        "peel: extract auth slice",
			SourceBranch: "peel/ka-0000006221",
			TargetBranch: "main",
			ProjectID:    7,
			Author:       &GitLabUser{Username: "matthew.baker"},
		})
	}))
	t.Cleanup(srv.Close)

	gitlab := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t", Logger: slog.Default()})
	return NewGitLabSync(GitLabSyncConfig{
		GitLab: gitlab,
		Daemon: daemon,
		Logger: slog.Default(),
		Host:   scopeHost, // the FW-1 host scope under test
	})
}

func darkMRURL(iid string) string {
	return "https://" + darkHost + "/monorepo-dark/monorepo/-/merge_requests/" + iid
}

func upstreamMRURL(iid string) string {
	return "https://" + upstreamHost + "/Acme/Platform/monorepo/-/merge_requests/" + iid
}

// TestGitLabDark_HostScopedProducesEdge is the FW-1 GATE. A dark-scoped bridge,
// fed a bead.updated event for a task bead whose mr_url points at gitlab-dark
// (the exact effect of `gb mr link <bead> <dark-mr-url>`), must upsert the
// type=merge-request bead and write the produces(task→MR) edge. This is the
// "dark `gb mr link` mints produces edges and type=merge-request dark-MR beads
// exist" outcome the bead requires.
func TestGitLabDark_HostScopedProducesEdge(t *testing.T) {
	mrURL := darkMRURL("4821")
	daemon := newMockGitLabDaemon()
	daemon.beads["gd-DarkTask"] = &beadsapi.BeadDetail{
		ID:     "gd-DarkTask",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	sync := newHostScopedSync(t, darkHost, daemon)
	sync.handleUpdated(context.Background(), beadUpdateEventWithMRURL(t, "gd-DarkTask", "task", mrURL))

	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("dark MR on dark bridge: got %d MR-bead upserts, want 1", len(upserts))
	}
	if got := upserts[0].Fields.MRURL; got != mrURL {
		t.Errorf("upserted MR url=%q, want %q", got, mrURL)
	}
	mrBeadID := upserts[0].BeadID

	deps := daemon.getDeps()
	if findDep(deps, "gd-DarkTask", mrBeadID, "produces") == nil {
		t.Errorf("missing produces dep gd-DarkTask → %s (dark-MR bead) in %+v", mrBeadID, deps)
	}
	// The MR bead itself must exist as type=merge-request — the dark-MR bead.
	if mrBead := daemon.getBead(mrBeadID); mrBead == nil || mrBead.Type != "merge-request" {
		t.Errorf("dark-MR bead %s missing or wrong type: %+v", mrBeadID, mrBead)
	}
}

// TestGitLabDark_IgnoresUpstreamMR proves the dark bridge does NOT act on an
// upstream MR URL on the shared SSE stream. Without host scoping the dark
// bridge would call its own (gitlab-dark) API for an upstream IID — producing
// a wrong-host MR bead and a bogus produces edge. The guard must make it a
// no-op so the prod bridge alone owns upstream MRs.
func TestGitLabDark_IgnoresUpstreamMR(t *testing.T) {
	mrURL := upstreamMRURL("4821")
	daemon := newMockGitLabDaemon()
	daemon.beads["gd-UpstreamTask"] = &beadsapi.BeadDetail{
		ID:     "gd-UpstreamTask",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	sync := newHostScopedSync(t, darkHost, daemon)
	sync.handleUpdated(context.Background(), beadUpdateEventWithMRURL(t, "gd-UpstreamTask", "task", mrURL))

	if ups := daemon.getMRUpserts(); len(ups) != 0 {
		t.Errorf("upstream MR on dark bridge: got %d upserts, want 0: %+v", len(ups), ups)
	}
	if deps := daemon.getDeps(); len(deps) != 0 {
		t.Errorf("upstream MR on dark bridge created %d deps, want 0: %+v", len(deps), deps)
	}
}

// TestGitLabProd_IgnoresDarkMR is the symmetric case: the prod (gitlab.com)
// bridge must ignore dark MR URLs so it never queries gitlab.com for a dark
// IID. Together with the two tests above this proves the two-deployment
// topology is non-overlapping on the shared stream.
func TestGitLabProd_IgnoresDarkMR(t *testing.T) {
	mrURL := darkMRURL("4821")
	daemon := newMockGitLabDaemon()
	daemon.beads["gd-DarkTask"] = &beadsapi.BeadDetail{
		ID:     "gd-DarkTask",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	sync := newHostScopedSync(t, upstreamHost, daemon)
	sync.handleUpdated(context.Background(), beadUpdateEventWithMRURL(t, "gd-DarkTask", "task", mrURL))

	if ups := daemon.getMRUpserts(); len(ups) != 0 {
		t.Errorf("dark MR on prod bridge: got %d upserts, want 0: %+v", len(ups), ups)
	}
	if deps := daemon.getDeps(); len(deps) != 0 {
		t.Errorf("dark MR on prod bridge created %d deps, want 0: %+v", len(deps), deps)
	}
}

// TestGitLab_UnscopedMatchesAllHosts pins the safety property for existing
// single-bridge prod: with host unset (the default), a bridge handles MR URLs
// on ANY host exactly as before. This is why FW-1 is zero-risk to the live
// deployment until GITLAB_BASE_URL-derived scoping is opted into per pod.
func TestGitLab_UnscopedMatchesAllHosts(t *testing.T) {
	for _, mrURL := range []string{darkMRURL("4821"), upstreamMRURL("4821")} {
		daemon := newMockGitLabDaemon()
		daemon.beads["gd-Task"] = &beadsapi.BeadDetail{
			ID:     "gd-Task",
			Type:   "task",
			Status: "in_progress",
			Fields: map[string]string{"mr_url": mrURL},
		}
		sync := newHostScopedSync(t, "", daemon) // host="" → match all
		sync.handleUpdated(context.Background(), beadUpdateEventWithMRURL(t, "gd-Task", "task", mrURL))

		if ups := daemon.getMRUpserts(); len(ups) != 1 {
			t.Errorf("unscoped bridge on %s: got %d upserts, want 1", mrURL, len(ups))
		}
	}
}

// TestParseMRURL_CapturesHost verifies the host capture that the scope guard
// relies on, including the deep-nested dark project path.
func TestParseMRURL_CapturesHost(t *testing.T) {
	tests := []struct {
		url      string
		wantHost string
		wantPath string
		wantIID  int
	}{
		{darkMRURL("4821"), darkHost, "monorepo-dark/monorepo", 4821},
		{upstreamMRURL("210"), upstreamHost, "Acme/Platform/monorepo", 210},
	}
	for _, tt := range tests {
		ref := ParseMRURL(tt.url)
		if ref == nil {
			t.Fatalf("ParseMRURL(%q) = nil", tt.url)
		}
		if ref.Host != tt.wantHost || ref.ProjectPath != tt.wantPath || ref.IID != tt.wantIID {
			t.Errorf("ParseMRURL(%q) = {host:%q path:%q iid:%d}, want {host:%q path:%q iid:%d}",
				tt.url, ref.Host, ref.ProjectPath, ref.IID, tt.wantHost, tt.wantPath, tt.wantIID)
		}
	}
}
