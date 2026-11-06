package bridge

// gitlab_mr_backlink_e2e_test.go — end-to-end coverage for the reverse MR→bead
// reconciler (bug ka-0000004261, backfill task gd-MfK9UlebWg).
//
// The unit suite in gitlab_mr_backlink_test.go drives the reconciler through an
// in-memory fakeMRLister — it never touches HTTP, so it proves the resolution
// logic but not that the scanner actually talks to GitLab correctly. The
// gitlab-bridge's other e2e coverage is likewise one-sided: the S2 sync tests
// exercise the FORWARD poller (beads that already carry mr_url), and the
// crossowner tests exercise privileged produces/triggered-by writes — neither
// drives the REVERSE scanner.
//
// These tests close that gap. They wire the REAL *GitLabClient (bridge.NewGitLabClient)
// to an httptest server that serves genuine GitLab `/api/v4/groups/:id/merge_requests`
// JSON, so one scan exercises the whole chain end-to-end: URL + query-window
// construction, the PRIVATE-TOKEN header, the opened+merged two-state sweep, the
// JSON decode into []GitLabMR, key recovery, bead resolution, and the mr_url
// write — exactly what runs in production when an agent forgets `gb mr link`.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gitlabMRServer is an httptest stand-in for the GitLab group merge-requests
// API. It serves canned MRs per requested state and records the query params it
// received so tests can assert the reverse reconciler drove a real, correctly-
// windowed HTTP request rather than a fake in-memory call.
type gitlabMRServer struct {
	*httptest.Server
	mu        sync.Mutex
	byState   map[string][]GitLabMR
	gotStates []string          // states requested, in order
	gotQuery  map[string]string // last query values seen, by state
	gotToken  string            // PRIVATE-TOKEN header seen on the last request
	groupPath string            // the /api/v4/groups/<id>/merge_requests path hit
}

// newGitLabMRServer starts an httptest server that answers group MR list calls
// out of byState (regardless of the group id in the path). States with no entry
// return [].
func newGitLabMRServer(t *testing.T, byState map[string][]GitLabMR) *gitlabMRServer {
	t.Helper()
	s := &gitlabMRServer{
		byState:  byState,
		gotQuery: map[string]string{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		s.mu.Lock()
		s.gotStates = append(s.gotStates, state)
		s.gotToken = r.Header.Get("PRIVATE-TOKEN")
		s.groupPath = r.URL.Path
		// Record the window/paging params for the primary auto-link assertion.
		s.gotQuery[state+":updated_after"] = r.URL.Query().Get("updated_after")
		s.gotQuery[state+":per_page"] = r.URL.Query().Get("per_page")
		s.gotQuery[state+":order_by"] = r.URL.Query().Get("order_by")
		mrs := s.byState[state]
		s.mu.Unlock()

		if mrs == nil {
			mrs = []GitLabMR{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mrs)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *gitlabMRServer) states() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.gotStates))
	copy(out, s.gotStates)
	return out
}

// newE2EReconciler builds a reconciler backed by the REAL GitLabClient pointed
// at srv, so the scan makes genuine HTTP calls. The daemon is the shared
// in-memory fakeBacklinkDaemon from the unit suite.
func newE2EReconciler(srv *gitlabMRServer, d mrBacklinkDaemon, groupID int) *MRBacklinkReconciler {
	gl := NewGitLabClient(GitLabClientConfig{
		BaseURL: srv.URL,
		Token:   "e2e-private-token",
		Logger:  backlinkTestLogger(),
	})
	return NewMRBacklinkReconciler(MRBacklinkConfig{
		GitLab:              gl,
		Daemon:              d,
		Logger:              backlinkTestLogger(),
		GroupID:             groupID,
		Window:              14 * 24 * time.Hour,
		FileUnlinkedBeads:   true,
		UnlinkedBeadProject: "gardenio",
	})
}

const e2eGroupID = 4176856

// TestE2E_ReverseReconciler_AutoLinksUnlinkedMR is acceptance criterion #1: an
// MR exists in GitLab with NO linked bead (no type=merge-request bead), and one
// reconciler pass — driven over real HTTP against the group MR API — self-heals
// the missing `gb mr link` by back-filling mr_url on the owning bead. It also
// asserts the two-state (opened+merged) sweep, the windowed query, and the
// PRIVATE-TOKEN auth actually went out on the wire, and that an epic sharing
// the JIRA key is NEVER chosen over its actionable child.
func TestE2E_ReverseReconciler_AutoLinksUnlinkedMR(t *testing.T) {
	d := newFakeBacklinkDaemon()

	// Owning bead recoverable by an explicit gd-ref on the branch. No MR bead
	// exists for its URL yet (mrByURL empty) — the "agent forgot gb mr link"
	// state the reconciler exists to repair.
	const kdMRURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/9001"
	d.beads["gd-G7qyDT40mM"] = actionableBead("gd-G7qyDT40mM", "task", time.Now())

	// A second MR recoverable by JIRA label, where the key ALSO matches an epic.
	// The reconciler must pick the actionable child, never the epic.
	const jiraMRURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/9002"
	epic := &beadsapi.BeadDetail{ID: "gd-EpicPE", Kind: "issue", Type: "epic", Labels: []string{"jira:PE-7777"}, UpdatedAt: time.Now()}
	child := actionableBead("gd-ChildPE", "bug", time.Now(), "jira:PE-7777")
	d.byLabel["jira:PE-7777"] = []*beadsapi.BeadDetail{epic, child}

	srv := newGitLabMRServer(t, map[string][]GitLabMR{
		"opened": {{
			WebURL:       kdMRURL,
			SourceBranch: "feat/gd-G7qyDT40mM-reverse-reconciler",
			Title:        "feat(bridge): reverse reconciler",
			State:        "opened",
			UpdatedAt:    time.Now().Format(time.RFC3339),
		}},
		"merged": {{
			WebURL:       jiraMRURL,
			SourceBranch: "fix/PE-7777-ecrf-save",
			Title:        "Fix eCRF save (PE-7777)",
			State:        "merged",
			UpdatedAt:    time.Now().Format(time.RFC3339),
		}},
	})

	r := newE2EReconciler(srv, d, e2eGroupID)
	s := r.scan(context.Background())

	// The gd-ref MR self-healed onto its owning bead.
	if got := d.updates["gd-G7qyDT40mM"]["mr_url"]; got != kdMRURL {
		t.Fatalf("expected gd-ref MR back-linked to gd-G7qyDT40mM, got %q (updates=%v)", got, d.updates)
	}
	// The JIRA MR self-healed onto the actionable CHILD, not the epic.
	if got := d.updates["gd-ChildPE"]["mr_url"]; got != jiraMRURL {
		t.Fatalf("expected JIRA MR back-linked to actionable child gd-ChildPE, got %q (updates=%v)", got, d.updates)
	}
	if _, linkedEpic := d.updates["gd-EpicPE"]; linkedEpic {
		t.Fatalf("must NEVER back-link an MR to an epic, got updates=%v", d.updates)
	}
	if s.linked != 2 {
		t.Fatalf("expected linked=2, got %+v", s)
	}

	// Fidelity: the scan really hit GitLab over HTTP with the right shape.
	if states := srv.states(); len(states) != 2 || states[0] != "opened" || states[1] != "merged" {
		t.Fatalf("expected opened then merged group MR list calls, got %v", states)
	}
	if srv.gotToken != "e2e-private-token" {
		t.Fatalf("expected PRIVATE-TOKEN auth header on the wire, got %q", srv.gotToken)
	}
	if srv.groupPath != "/api/v4/groups/4176856/merge_requests" {
		t.Fatalf("unexpected group MR path hit: %q", srv.groupPath)
	}
	if srv.gotQuery["opened:updated_after"] == "" {
		t.Fatalf("expected a windowed updated_after query param, got none (query=%v)", srv.gotQuery)
	}
	if srv.gotQuery["opened:per_page"] != "100" {
		t.Fatalf("expected per_page=100 window paging, got %q", srv.gotQuery["opened:per_page"])
	}
}

// TestE2E_ReverseReconciler_StaleLinkRepair is acceptance criterion #2: the
// stale-link reconcile path, driven end-to-end over real HTTP. It covers BOTH
// halves of what "reconcile stale links" means for a backfill-only reconciler:
//
//  1. Re-affirm: a bead already carries THIS exact mr_url, but the canonical
//     type=merge-request bead was never minted (the link is stale/un-propagated
//     — the forward poller hasn't caught up). The reconciler idempotently
//     re-sets mr_url so the next forward pass mints the MR bead. No clobber.
//  2. Refuse-to-repoint: a bead already points at a DIFFERENT, correct MR (its
//     own merged fix). A newer unrelated MR whose branch names that same bead
//     must NOT silently overwrite the good link — the clobber guard fires and
//     the stale/incorrect attach is refused rather than made.
func TestE2E_ReverseReconciler_StaleLinkRepair(t *testing.T) {
	d := newFakeBacklinkDaemon()

	// (1) Re-affirm target: mr_url already set to reaffirmURL, no MR bead yet.
	const reaffirmURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/7010"
	reaffirm := actionableBead("gd-J1kOVms0Ro", "bug", time.Now())
	reaffirm.Fields["mr_url"] = reaffirmURL // stale: set on bead, not yet propagated
	d.beads["gd-J1kOVms0Ro"] = reaffirm

	// (2) Refuse-to-repoint target: already points at its own correct merged MR.
	const existingGoodURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/6099"
	const intruderURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/7011"
	guarded := actionableBead("gd-Fc7UT3roTS", "bug", time.Now())
	guarded.Fields["mr_url"] = existingGoodURL
	d.beads["gd-Fc7UT3roTS"] = guarded

	srv := newGitLabMRServer(t, map[string][]GitLabMR{
		"opened": {{
			WebURL:       intruderURL, // a DIFFERENT MR naming the guarded bead
			SourceBranch: "feat/gd-Fc7UT3roTS-unrelated-followup",
			State:        "opened",
			UpdatedAt:    time.Now().Format(time.RFC3339),
		}},
		"merged": {{
			WebURL:       reaffirmURL,
			SourceBranch: "fix/gd-J1kOVms0Ro-original",
			State:        "merged",
			UpdatedAt:    time.Now().Format(time.RFC3339),
		}},
	})

	r := newE2EReconciler(srv, d, e2eGroupID)
	s := r.scan(context.Background())

	// (1) Re-affirm succeeded idempotently — mr_url unchanged, clobber not tripped.
	if got := d.updates["gd-J1kOVms0Ro"]["mr_url"]; got != reaffirmURL {
		t.Fatalf("expected stale link re-affirmed on gd-J1kOVms0Ro, got %q (updates=%v)", got, d.updates)
	}
	// (2) The good existing link was preserved — never repointed at the intruder.
	if guarded.Fields["mr_url"] != existingGoodURL {
		t.Fatalf("clobber guard failed: gd-Fc7UT3roTS mr_url changed to %q", guarded.Fields["mr_url"])
	}
	if _, wrote := d.updates["gd-Fc7UT3roTS"]; wrote {
		t.Fatalf("must not write mr_url onto a bead that already points at a different MR, got %v", d.updates)
	}
	if s.linked != 1 || s.clobberBlocked != 1 {
		t.Fatalf("expected linked=1 (re-affirm) clobberBlocked=1 (refuse-repoint), got %+v", s)
	}
}

// TestE2E_ReverseReconciler_UnresolvableFilesUnlinkedBead exercises the "no
// single actionable bead" escape hatch end-to-end: an MR carries a recognizable
// key but nothing in the store resolves it, so the reconciler refuses to guess
// and files exactly one low-priority "unlinked MR" bead for a gardener to
// resolve — over real HTTP, and idempotent across repeated scans of the same MR.
func TestE2E_ReverseReconciler_UnresolvableFilesUnlinkedBead(t *testing.T) {
	d := newFakeBacklinkDaemon()
	const orphanURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/7099"

	srv := newGitLabMRServer(t, map[string][]GitLabMR{
		"opened": {{
			WebURL:       orphanURL,
			SourceBranch: "fix/PE-9999-nothing-matches",
			Title:        "orphaned fix (PE-9999)",
			State:        "opened",
			UpdatedAt:    time.Now().Format(time.RFC3339),
		}},
	})

	r := newE2EReconciler(srv, d, e2eGroupID)
	r.scan(context.Background())
	r.scan(context.Background()) // second pass over the still-unresolvable MR

	if len(d.updates) != 0 {
		t.Fatalf("must not link an unresolvable MR to any bead, got updates=%v", d.updates)
	}
	if len(d.created) != 1 {
		t.Fatalf("expected exactly one unlinked-MR bead filed across two scans, got %d", len(d.created))
	}
	got := d.created[0]
	if got.Type != "task" || got.Priority != 3 {
		t.Errorf("unexpected unlinked-MR bead shape: type=%q priority=%d", got.Type, got.Priority)
	}
	var hasProject, hasUnlinked bool
	for _, l := range got.Labels {
		if l == "project:gardenio" {
			hasProject = true
		}
		if l == "unlinked-mr" {
			hasUnlinked = true
		}
	}
	if !hasProject || !hasUnlinked {
		t.Errorf("expected project:gardenio + unlinked-mr labels on filed bead, got %v", got.Labels)
	}

	// The filed bead must carry the MR URL in its typed fields so a gardener can
	// act on it directly.
	var fields map[string]string
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("unlinked-MR bead fields not decodable: %v", err)
	}
	if fields["mr_url"] != orphanURL {
		t.Errorf("expected filed bead to carry mr_url=%q, got %q", orphanURL, fields["mr_url"])
	}
}
