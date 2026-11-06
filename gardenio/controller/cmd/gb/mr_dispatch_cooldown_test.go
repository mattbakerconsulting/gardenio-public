package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// cooldownStub stands up ONE httptest server that serves BOTH the beads daemon
// endpoints evaluateDispatchCooldown touches (GetBead → the MR bead carrying its
// own mr_url, GetComments, AddComment) AND the GitLab MR-view endpoint (for the
// head SHA). The gate arg is the MR bead id ("gd-mrbead"): GetBead returns a
// merge-request bead whose mr_url points back at this same server, so both the
// URL/IID resolution and the head-SHA fetch route here.
type cooldownStub struct {
	mu         sync.Mutex
	headSHA    string             // served as the MR's current head SHA
	comments   []beadsapi.Comment // served by GetComments
	addedText  string             // captured by AddComment
	addedCount int
	mrBeadID   string
	mrURL      string // the MR bead's mr_url field (set to <srv.URL>/owner/repo/-/merge_requests/42)
}

func (s *cooldownStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/api/v4/projects/") && strings.Contains(path, "/merge_requests/"):
			// GitLab MR view → current head SHA.
			_ = json.NewEncoder(w).Encode(bridge.GitLabMR{
				IID: 42, State: "opened", SHA: s.headSHA,
			})
		case strings.HasSuffix(path, "/comments") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": s.comments})
		case strings.HasSuffix(path, "/comments") && r.Method == http.MethodPost:
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.addedText = body.Text
			s.addedCount++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		case strings.HasPrefix(path, "/v1/beads/"):
			// GetBead(mrBeadID) → the MR bead itself (a merge-request bead, so
			// resolveMRBeadID returns it directly), carrying its mr_url so
			// resolveMRURL/ParseMRURL yield the project path + IID.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": s.mrBeadID, "type": "merge-request", "status": "open",
				"title": "cooldown MR", "fields": map[string]string{"mr_url": s.mrURL},
			})
		default:
			http.Error(w, "unexpected path "+path, http.StatusNotFound)
		}
	}
}

// TestEvaluateDispatchCooldown_CLI_SuppressSameSHA drives evaluateDispatchCooldown
// end-to-end: the MR bead carries a recent cooldown stamp whose head SHA equals the
// MR's current head SHA, so the cooldown SUPPRESSES and does NOT write a new stamp.
func TestEvaluateDispatchCooldown_CLI_SuppressSameSHA(t *testing.T) {
	const sha = "8f3fb5cedeadbeefcafefeedfacebeefdeadfeed"
	stub := &cooldownStub{
		headSHA:  sha,
		mrBeadID: "gd-mrbead",
		comments: []beadsapi.Comment{
			// Stamped 20m ago (one cycle) at the same head SHA → inside the window.
			{ID: 1, Text: bridge.FormatCooldownStamp(sha, time.Now().Add(-20*time.Minute))},
		},
	}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	stub.mrURL = srv.URL + "/owner/repo/-/merge_requests/42"

	origURL, origToken, origDaemon := mrGitLabURL, mrGitLabToken, daemon
	t.Cleanup(func() { mrGitLabURL, mrGitLabToken, daemon = origURL, origToken, origDaemon })
	mrGitLabURL, mrGitLabToken = "", "tok"
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	daemon = c

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	got := evaluateDispatchCooldown(cmd, stub.mrBeadID, stub.mrURL, "owner/repo", 42)
	if got.decision.MayDispatch {
		t.Fatalf("expected SUPPRESS (same head SHA within window); reason=%q", got.decision.Reason)
	}
	if got.currentHeadSHA != sha {
		t.Errorf("currentHeadSHA = %q, want %q", got.currentHeadSHA, sha)
	}
	if got.stamped {
		t.Error("stamped = true on a suppressed dispatch, want false (no new stamp)")
	}
	if stub.addedCount != 0 {
		t.Errorf("AddComment called %d times on suppress, want 0", stub.addedCount)
	}
}

// TestEvaluateDispatchCooldown_CLI_DispatchNewSHAStamps drives the may-dispatch
// path: the MR's current head SHA differs from the stamped one, so the cooldown
// permits dispatch AND writes a fresh stamp carrying the new head SHA.
func TestEvaluateDispatchCooldown_CLI_DispatchNewSHAStamps(t *testing.T) {
	const oldSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const newSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	stub := &cooldownStub{
		headSHA:  newSHA,
		mrBeadID: "gd-mrbead",
		comments: []beadsapi.Comment{
			{ID: 1, Text: bridge.FormatCooldownStamp(oldSHA, time.Now().Add(-5*time.Minute))},
		},
	}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	stub.mrURL = srv.URL + "/owner/repo/-/merge_requests/42"

	origURL, origToken, origDaemon, origActor := mrGitLabURL, mrGitLabToken, daemon, actor
	t.Cleanup(func() {
		mrGitLabURL, mrGitLabToken, daemon, actor = origURL, origToken, origDaemon, origActor
	})
	mrGitLabURL, mrGitLabToken, actor = "", "tok", "gardenio-test"
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	daemon = c

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	got := evaluateDispatchCooldown(cmd, stub.mrBeadID, stub.mrURL, "owner/repo", 42)
	if !got.decision.MayDispatch {
		t.Fatalf("expected MAY DISPATCH (head SHA moved); reason=%q", got.decision.Reason)
	}
	if !got.stamped {
		t.Error("stamped = false, want true (fresh stamp on dispatch)")
	}
	if stub.addedCount != 1 {
		t.Fatalf("AddComment called %d times, want 1", stub.addedCount)
	}
	// The fresh stamp must be parseable and carry the NEW head SHA.
	gotSHA, _, ok := bridge.ParseCooldownStamp(stub.addedText)
	if !ok {
		t.Fatalf("written stamp %q did not parse", stub.addedText)
	}
	if gotSHA != newSHA {
		t.Errorf("stamped head SHA = %q, want the current %q", gotSHA, newSHA)
	}
}

// TestEvaluateDispatchCooldown_CLI_FirstDispatchStamps confirms the first-ever
// dispatch (no prior stamp) permits dispatch and lays down the initial stamp.
func TestEvaluateDispatchCooldown_CLI_FirstDispatchStamps(t *testing.T) {
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	stub := &cooldownStub{headSHA: sha, mrBeadID: "gd-mrbead"} // no comments
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	stub.mrURL = srv.URL + "/owner/repo/-/merge_requests/42"

	origURL, origToken, origDaemon, origActor := mrGitLabURL, mrGitLabToken, daemon, actor
	t.Cleanup(func() {
		mrGitLabURL, mrGitLabToken, daemon, actor = origURL, origToken, origDaemon, origActor
	})
	mrGitLabURL, mrGitLabToken, actor = "", "tok", "gardenio-test"
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	daemon = c

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	got := evaluateDispatchCooldown(cmd, stub.mrBeadID, stub.mrURL, "owner/repo", 42)
	if !got.decision.MayDispatch {
		t.Fatalf("expected MAY DISPATCH (no prior stamp); reason=%q", got.decision.Reason)
	}
	if !got.stamped || stub.addedCount != 1 {
		t.Errorf("expected one fresh stamp on first dispatch; stamped=%v count=%d", got.stamped, stub.addedCount)
	}
}
