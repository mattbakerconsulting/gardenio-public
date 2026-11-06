package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

// These tests cover step 3 of the MicroVM suspend/resume plan (gd-4dtYcA5tRG,
// plan ka-0000004518): a re-mention of a SUSPENDED MicroVM thread agent must
// resume the EXISTING VM in place (POST /api/v1/microvm/resume) instead of
// closing the bead + reminting / grabbing a pool pod.
//
// The MicroVM path CANNOT reuse the gVisor close+remint route: a MicroVM's VM
// identity lives in runtime_id ON THE BEAD, so closing the bead would fire
// AgentDone -> Terminate and destroy the suspended VM (and a fresh mint has no
// runtime_id, so it would Launch a brand-new RunMicrovm) — either way the
// suspended VM is abandoned and continuity is lost. So respawnThreadAgent must:
//   - NOT close the suspended bead,
//   - NOT consult the warm pool,
//   - NOT mint a same-name resume bead,
// and instead ask the controller to resume the VM in place.

// seedSuspendedMicrovmBead pre-populates the mock with an OPEN prior agent bead
// shaped like step 2's suspend path leaves it: lambda_microvm, agent_state=
// stopping, with a persisted runtime_id.
func seedSuspendedMicrovmBead(daemon *mockDaemon, agentName, project string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName + "-suspended",
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":            agentName,
			"project":          project,
			"agent_state":      "stopping",
			"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
			"runtime_id":       "mvm-" + agentName,
		},
	}
}

// fakeControllerForMicrovmResume emulates the controller endpoints the bridge
// respawn path touches: /api/v1/microvm/resume (records whether it was called
// and returns resumed per resumeOK) and /api/v1/pool/assign (records whether it
// was called — it must NOT be for a suspended MicroVM agent).
func fakeControllerForMicrovmResume(t *testing.T, resumeOK bool, mu *sync.Mutex, resumeCalled, poolCalled *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/microvm/resume":
			mu.Lock()
			*resumeCalled = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resumed":    resumeOK,
				"microvm_id": "mvm-x",
			})
		case "/api/v1/pool/assign":
			mu.Lock()
			*poolCalled = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"bead_id":    "gd-pooled-1",
				"agent_name": "pooled-agent",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestRespawnThreadAgent_SuspendedMicrovm_ResumesInPlace verifies the happy
// path: the suspended VM is resumed via the controller endpoint, the bead is
// NOT closed, the pool is NOT consulted, and no same-name resume bead is minted.
func TestRespawnThreadAgent_SuspendedMicrovm_ResumesInPlace(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("gardenio", "C-MV", "thread")
	const agentName = "thread-5555-6666"
	seedSuspendedMicrovmBead(daemon, agentName, "gardenio")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var mu sync.Mutex
	var resumeCalled, poolCalled bool
	ctrlSrv := fakeControllerForMicrovmResume(t, true, &mu, &resumeCalled, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-MV", "5555.6666",
		agentName, "you there?", "U-HUMAN")

	mu.Lock()
	gotResume, gotPool := resumeCalled, poolCalled
	mu.Unlock()

	if !gotResume {
		t.Fatal("expected respawn of a suspended MicroVM agent to call the controller resume endpoint")
	}
	if gotPool {
		t.Fatal("suspended MicroVM resume must NOT consult the warm pool (a pool pod abandons the suspended VM)")
	}

	// The suspended bead must NOT be closed — closing it fires Terminate and
	// destroys the VM we just resumed.
	daemon.mu.Lock()
	var closedSuspended bool
	for _, c := range daemon.closed {
		if c.BeadID == agentName+"-suspended" {
			closedSuspended = true
		}
	}
	daemon.mu.Unlock()
	if closedSuspended {
		t.Error("suspended MicroVM bead must NOT be closed (close would terminate the resumed VM)")
	}

	// No same-name cold-resume bead must be minted — the whole point is resume in
	// place on the EXISTING bead.
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			t.Errorf("suspended MicroVM resume must NOT mint a cold-resume bead, found %s", b.ID)
		}
	}
}

// seedDoneMicrovmBead pre-populates the mock with an OPEN prior agent bead shaped
// like a MicroVM thread agent that ran once and closed agent_state=done with its
// terminal runtime still recorded on the bead (execution_target=lambda_microvm,
// a persisted runtime_id, runtime_state=TERMINATED). This is the exact shape of
// the ka-0000006530 incident bead (gd-JAaQhMXQdn): a DONE — not suspended —
// MicroVM agent whose VM is already gone.
func seedDoneMicrovmBead(daemon *mockDaemon, agentName, project string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName + "-done",
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		// A real (non-zero) CreatedAt so priorAgentBead — which picks the
		// most-recently-created prior agent bead by CreatedAt — actually selects
		// this bead and carries its execution_target forward, matching production
		// (a done bead always has a genuine creation timestamp).
		CreatedAt: time.Date(2026, 7, 23, 16, 23, 49, 0, time.UTC),
		Fields: map[string]string{
			"agent":            agentName,
			"project":          project,
			"agent_state":      "done",
			"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
			"runtime_id":       "mvm-" + agentName,
			"runtime_state":    "TERMINATED",
		},
	}
}

// TestRespawnThreadAgent_DoneMicrovm_ColdMintsFreshVM covers the ka-0000006530
// done→resume path: a re-mention of a DONE (not suspended) MicroVM thread agent
// must NOT take the in-place resume path — its VM is already gone, and the
// in-place resume gate requires agent_state=="stopping". Instead the respawn
// cold-mints a fresh same-name lambda_microvm bead (which the controller launches
// as a brand-new VM) and must NOT carry forward the dead runtime_id — reusing a
// TERMINATED runtime id would be the bug. It also must not consult the warm pool
// (a K8s pool pod cannot become a Firecracker VM).
func TestRespawnThreadAgent_DoneMicrovm_ColdMintsFreshVM(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("gardenio", "C-MV", "thread")
	const agentName = "thread-4444-3333"
	seedDoneMicrovmBead(daemon, agentName, "gardenio")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var mu sync.Mutex
	var resumeCalled, poolCalled bool
	// resumeOK=true would be a red herring: the bridge must never reach the resume
	// endpoint for a DONE agent, so the response value is irrelevant here.
	ctrlSrv := fakeControllerForMicrovmResume(t, true, &mu, &resumeCalled, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-MV", "4444.3333",
		agentName, "how are we doing?", "U-HUMAN")

	mu.Lock()
	gotResume, gotPool := resumeCalled, poolCalled
	mu.Unlock()

	// A done agent has no live VM to resume in place — the in-place resume gate
	// (agent_state=="stopping" + runtime_id) must NOT fire.
	if gotResume {
		t.Fatal("a DONE MicroVM agent must NOT hit the in-place resume endpoint (its VM is gone; the gate requires agent_state=stopping)")
	}
	// A MicroVM backend skips the warm pool — a K8s pool pod cannot become a VM.
	if gotPool {
		t.Fatal("a MicroVM respawn must NOT consult the warm pool (a pool pod cannot become a Firecracker VM)")
	}

	// The stale done bead is closed before the fresh mint (terminal-state carve-out).
	daemon.mu.Lock()
	var closedDone bool
	for _, c := range daemon.closed {
		if c.BeadID == agentName+"-done" {
			closedDone = true
		}
	}
	// Exactly one fresh cold-resume bead, carrying the MicroVM backend forward but
	// NOT the dead runtime_id — that is the crux: a fresh Launch must mint a new VM
	// (ClientToken = new bead id), never reuse the TERMINATED runtime.
	var resumeBeads []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" && b.Fields["agent"] == agentName {
			resumeBeads = append(resumeBeads, b)
		}
	}
	daemon.mu.Unlock()

	if !closedDone {
		t.Error("the stale done MicroVM bead should be closed before the cold-resume mint")
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected exactly 1 cold-resume bead for a done-MicroVM re-mention, got %d", len(resumeBeads))
	}
	got := resumeBeads[0]
	if got.Fields["execution_target"] != podmanager.ExecutionTargetLambdaMicroVM {
		t.Errorf("cold-resume bead execution_target = %q, want %q (backend must carry forward)",
			got.Fields["execution_target"], podmanager.ExecutionTargetLambdaMicroVM)
	}
	if rid := got.Fields["runtime_id"]; rid != "" {
		t.Errorf("cold-resume bead must NOT carry forward the dead runtime_id, got %q (a fresh Launch mints a new VM)", rid)
	}
}

// TestRespawnThreadAgent_MicrovmResumeMiss_FallsThrough verifies that if the
// controller reports resumed:false (e.g. a race flipped the bead out of stopping,
// or the backend declined), the bridge falls through to its normal respawn path
// rather than getting stuck. Because the prior bead is still agent_state=stopping,
// frozenAwaitingThaw is set, so the pool is (correctly) skipped and the stale bead
// is closed + a same-name cold-resume bead is minted — a fresh runtime if the
// channel still resolves to a MicroVM. This keeps the endpoint a pure best-effort
// optimization: a miss never strands the mention.
func TestRespawnThreadAgent_MicrovmResumeMiss_FallsThrough(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("gardenio", "C-MV", "thread")
	const agentName = "thread-7777-8888"
	seedSuspendedMicrovmBead(daemon, agentName, "gardenio")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var mu sync.Mutex
	var resumeCalled, poolCalled bool
	// resumeOK=false → the bridge must fall through to its normal respawn.
	ctrlSrv := fakeControllerForMicrovmResume(t, false, &mu, &resumeCalled, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-MV", "7777.8888",
		agentName, "you there?", "U-HUMAN")

	mu.Lock()
	gotResume, gotPool := resumeCalled, poolCalled
	mu.Unlock()

	if !gotResume {
		t.Fatal("expected the bridge to attempt the resume endpoint before falling through")
	}
	// The prior bead is still agent_state=stopping, so frozenAwaitingThaw skips
	// the pool (a pool pod would abandon any snapshot/VM) — matching the gVisor
	// frozen fall-through.
	if gotPool {
		t.Fatal("a still-suspended agent must skip the pool on the fall-through too")
	}

	// The stale bead is closed and a same-name cold-resume bead is minted so the
	// mention is served rather than stranded.
	daemon.mu.Lock()
	var closedSuspended bool
	for _, c := range daemon.closed {
		if c.BeadID == agentName+"-suspended" {
			closedSuspended = true
		}
	}
	var mintedResume bool
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" && b.Fields["agent"] == agentName {
			mintedResume = true
		}
	}
	daemon.mu.Unlock()
	if !closedSuspended {
		t.Error("on a resume miss the stale suspended bead should be closed before the cold-resume mint")
	}
	if !mintedResume {
		t.Error("on a resume miss a same-name cold-resume bead should be minted so the mention is served")
	}
}
