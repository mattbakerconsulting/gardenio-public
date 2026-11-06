package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// These tests lock in the respawn half of the --fresh flag (task ka-0000008188).
// respawnThreadAgent normally REVIVES the prior thread agent bead in place so
// coop --resume restores the prior transcript (ka-0000006807). --fresh must
// instead mint a NEW bead (fresh S3 prefix → no transcript), stamp
// session_resume=false (BOAT_SESSION_RESUME=0 → no PVC/thaw resume), and NOT
// graft the prior transcript onto a pooled pod.

// seedClosedPriorAgentBeadFresh pre-populates the mock with a CLOSED prior agent
// bead keyed by NAME (so both FindAgentBead and FindAgentBeadAnyStatus resolve
// it), giving the revive-in-place path a concrete id it WOULD reopen absent
// --fresh. It carries an s3_session_key so the warm-resume resolver would have a
// real key to forward if the fresh guard did not force "".
func seedClosedPriorAgentBeadFresh(daemon *mockDaemon, agentName, project string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:        agentName + "-prior",
		Title:     agentName,
		Type:      "agent",
		Status:    "closed",
		CreatedAt: time.Now(),
		Fields: map[string]string{
			"agent":          agentName,
			"project":        project,
			"agent_state":    "done",
			"s3_session_key": agentName + "-prior",
		},
	}
}

// TestRespawnThreadAgent_Fresh_MintsNewBeadNotRevive is the key regression: a
// --fresh wake-up must mint a NEW cold-resume bead (its own id) rather than
// revive the prior bead in place, so the new pod gets a fresh COOP_S3_SESSION_KEY
// prefix and finds no transcript. It also asserts session_resume=false is stamped
// and the description is the clean-start variant, not the resume preamble.
func TestRespawnThreadAgent_Fresh_MintsNewBeadNotRevive(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedClosedPriorAgentBeadFresh(daemon, "thread-7777-1111", "gardenio")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	// Pool MISS so the respawn falls through to the deterministic cold-mint path.
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "7777.1111",
		"thread-7777-1111", "--fresh brand new question", "U-HUMAN")

	bead := singleResumeBead(t, daemon)

	// A revive-in-place would reuse the prior bead's id ("thread-7777-1111-prior").
	// --fresh must mint a fresh id instead.
	if bead.ID == "thread-7777-1111-prior" {
		t.Errorf("expected a NEW cold-mint bead under --fresh, got the revived prior id %q", bead.ID)
	}
	if got := bead.Fields["session_resume"]; got != "false" {
		t.Errorf("expected session_resume=false under --fresh, got %q", got)
	}
	if got := bead.Fields["spawn_source"]; got != "slack-thread-resume" {
		t.Errorf("expected spawn_source=slack-thread-resume, got %q", got)
	}
	// The clean-start description must NOT claim a resumed session, and the
	// --fresh token must be stripped from the trigger text.
	if strings.Contains(bead.Description, "Session-resumed thread agent") {
		t.Errorf("expected clean-start description under --fresh, got resume preamble:\n%s", bead.Description)
	}
	if !strings.Contains(bead.Description, "FRESH CONVERSATION") {
		t.Errorf("expected FRESH CONVERSATION marker in description, got:\n%s", bead.Description)
	}
	if strings.Contains(bead.Description, "--fresh") {
		t.Errorf("expected --fresh token stripped from description, got:\n%s", bead.Description)
	}
	if !strings.Contains(bead.Description, "brand new question") {
		t.Errorf("expected remaining prompt preserved in description, got:\n%s", bead.Description)
	}
}

// TestRespawnThreadAgent_NoFresh_RevivesInPlace is the control: WITHOUT --fresh
// the same seeded prior bead is revived in place (same id) and session_resume is
// NOT stamped false — proving the fresh behavior above is the flag's doing, not a
// side effect of the seeding.
func TestRespawnThreadAgent_NoFresh_RevivesInPlace(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedClosedPriorAgentBeadFresh(daemon, "thread-7777-2222", "gardenio")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "7777.2222",
		"thread-7777-2222", "keep going", "U-HUMAN")

	bead := singleResumeBead(t, daemon)
	if bead.ID != "thread-7777-2222-prior" {
		t.Errorf("expected revive-in-place to reuse the prior bead id, got %q", bead.ID)
	}
	if got := bead.Fields["session_resume"]; got != "" {
		t.Errorf("expected no session_resume stamp without --fresh, got %q", got)
	}
	if !strings.Contains(bead.Description, "Session-resumed thread agent") {
		t.Errorf("expected resume preamble without --fresh, got:\n%s", bead.Description)
	}
}

// fakeControllerForAssignCapturingResumeKey is like fakeControllerForAssign but
// captures the resume_s3_key the bridge sends to /api/v1/pool/assign, so a test
// can assert --fresh forces it empty even when WARM_POOL_RESUME_ENABLED is on.
func fakeControllerForAssignCapturingResumeKey(t *testing.T, assignOK bool, gotResumeKey *string, mu *sync.Mutex, called *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pool/assign" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ResumeS3Key string `json:"resume_s3_key"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		*gotResumeKey = req.ResumeS3Key
		*called = true
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if !assignOK {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no prewarmed agents available"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"bead_id":    "gd-pooled-fresh",
			"agent_name": "thread-7777-3333",
		})
	}))
}

// TestRespawnThreadAgent_Fresh_SkipsWarmResumeGraft verifies that even with the
// warm-pool coop-resume graft ENABLED and a prior transcript resolvable, --fresh
// forwards an EMPTY resume_s3_key to the pool so the assigned warm pod never
// grafts the old transcript back on (ka-0000000983).
func TestRespawnThreadAgent_Fresh_SkipsWarmResumeGraft(t *testing.T) {
	t.Setenv(warmPoolResumeEnabledEnv, "true")

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedClosedPriorAgentBeadFresh(daemon, "thread-7777-3333", "gardenio")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotResumeKey string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssignCapturingResumeKey(t, true, &gotResumeKey, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "7777.3333",
		"thread-7777-3333", "--fresh new topic", "U-HUMAN")

	mu.Lock()
	wasCalled := called
	key := gotResumeKey
	mu.Unlock()
	if !wasCalled {
		t.Fatal("expected pool assign to be attempted under --fresh")
	}
	if key != "" {
		t.Errorf("expected EMPTY resume_s3_key under --fresh (no transcript graft), got %q", key)
	}
}
