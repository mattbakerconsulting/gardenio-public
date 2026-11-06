package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// These tests exercise the warm-pool coop-resume graft end-to-end on the
// follow-up (respawn) path (ka-0000000983): respawnThreadAgent must transmit the
// prior thread agent's S3 session key as resume_s3_key to /api/v1/pool/assign
// when WARM_POOL_RESUME_ENABLED is on, and transmit "" (a fresh warm session,
// today's behavior) when it is off — so the feature is a true no-op by default.

// fakeControllerCaptureResumeKey emulates the controller's pool-assign endpoint
// and records the resume_s3_key from the request body. Always returns a 200 hit.
func fakeControllerCaptureResumeKey(t *testing.T, gotKey *string, mu *sync.Mutex, called *bool) *httptest.Server {
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
		*gotKey = req.ResumeS3Key
		*called = true
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"bead_id":    "gd-pooled-warm",
			"agent_name": "thread-warm-1",
		})
	}))
}

// seedPriorAgentBeadWithID pre-populates a CLOSED prior agent bead under an
// explicit id (the id IS the S3 transcript prefix coop uploaded under), so a
// test can assert the transmitted resume key equals that id.
func seedPriorAgentBeadWithID(daemon *mockDaemon, agentName, project, beadID string, fields map[string]string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	f := map[string]string{"agent": agentName, "project": project}
	for k, v := range fields {
		f[k] = v
	}
	daemon.beads[beadID] = &beadsapi.BeadDetail{
		ID:     beadID,
		Title:  agentName,
		Type:   "agent",
		Status: "closed",
		Fields: f,
	}
}

// TestRespawnThreadAgent_WarmResume_On_TransmitsPriorKey verifies that with the
// feature ON, the respawn's warm assign carries the prior agent bead's id as the
// resume key, so the assigned warm pod resumes that transcript.
func TestRespawnThreadAgent_WarmResume_On_TransmitsPriorKey(t *testing.T) {
	t.Setenv(warmPoolResumeEnabledEnv, "true")

	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-WR", "thread")

	const agentName = "thread-8100-2200"
	const priorBeadID = "gd-PRIOR-warm-transcript"
	seedPriorAgentBeadWithID(daemon, agentName, "monorepo", priorBeadID, nil)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotKey string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerCaptureResumeKey(t, &gotKey, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-WR", "8100.2200",
		agentName, "follow-up", "U-HUMAN")

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("expected warm assign to be attempted")
	}
	if gotKey != priorBeadID {
		t.Errorf("resume_s3_key = %q, want %q (prior bead id / transcript prefix)", gotKey, priorBeadID)
	}
}

// TestRespawnThreadAgent_WarmResume_On_PrefersCanonicalField verifies the
// resume key prefers a persisted s3_session_key over the bead id, so a respawn
// chain follows the ORIGINAL transcript prefix rather than a moving target.
func TestRespawnThreadAgent_WarmResume_On_PrefersCanonicalField(t *testing.T) {
	t.Setenv(warmPoolResumeEnabledEnv, "1")

	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-WR2", "thread")

	const agentName = "thread-8300-4400"
	seedPriorAgentBeadWithID(daemon, agentName, "monorepo", "gd-PRIOR-second-life",
		map[string]string{"s3_session_key": "gd-ORIGINAL-prefix"})

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotKey string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerCaptureResumeKey(t, &gotKey, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-WR2", "8300.4400",
		agentName, "follow-up", "U-HUMAN")

	mu.Lock()
	defer mu.Unlock()
	if gotKey != "gd-ORIGINAL-prefix" {
		t.Errorf("resume_s3_key = %q, want gd-ORIGINAL-prefix (canonical field preferred)", gotKey)
	}
}

// TestRespawnThreadAgent_WarmResume_Off_TransmitsEmpty verifies the default:
// with the feature OFF the warm assign carries an empty resume key, so the pod
// starts a fresh warm session exactly as before this feature existed.
func TestRespawnThreadAgent_WarmResume_Off_TransmitsEmpty(t *testing.T) {
	t.Setenv(warmPoolResumeEnabledEnv, "")

	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-WR3", "thread")

	const agentName = "thread-8500-6600"
	seedPriorAgentBeadWithID(daemon, agentName, "monorepo", "gd-PRIOR-ignored", nil)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotKey string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerCaptureResumeKey(t, &gotKey, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-WR3", "8500.6600",
		agentName, "follow-up", "U-HUMAN")

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("expected warm assign to be attempted")
	}
	if gotKey != "" {
		t.Errorf("resume_s3_key = %q, want \"\" (feature off → fresh warm session)", gotKey)
	}
}
