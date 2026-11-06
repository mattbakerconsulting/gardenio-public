package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

// These tests lock in the respawn half of the --backend flag. respawnThreadAgent
// mints a NEW pod (or, for a suspended VM, resumes in place), so it must carry
// forward the prior agent's execution_target (so a MicroVM thread stays on the
// MicroVM backend across restart/resume) and honor a freshly-typed --backend on
// the wake-up (which WINS over the prior).

func seedPriorAgentBeadWithBackend(daemon *mockDaemon, agentName, project, backend string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName+"-prior"] = &beadsapi.BeadDetail{
		ID:        agentName + "-prior",
		Title:     agentName,
		Type:      "agent",
		Status:    "closed",
		CreatedAt: time.Now(),
		Fields: map[string]string{
			"agent":            agentName,
			"project":          project,
			"execution_target": backend,
		},
	}
}

// TestRespawnThreadAgent_CarriesForwardPriorMicrovmBackend is the key
// regression: a thread spawned on the MicroVM backend must STAY on it when its
// pod respawns with no fresh flag. Without the carry-forward, the cold remint
// would drop execution_target and revert the agent to the role/project default
// backend. The MicroVM target also SKIPS the warm pool (a K8s pool pod can't
// become a VM), so we assert the pool is never hit. --backend=microvm also
// forces ultracode on across the resume.
func TestRespawnThreadAgent_CarriesForwardPriorMicrovmBackend(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedPriorAgentBeadWithBackend(daemon, "thread-9999-1111", "gardenio", podmanager.ExecutionTargetLambdaMicroVM)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "9999.1111",
		"thread-9999-1111", "keep going", "U-HUMAN")

	mu.Lock()
	wasCalled := called
	mu.Unlock()
	if wasCalled {
		t.Error("expected pool assign to be SKIPPED when the prior backend is MicroVM (pooled pod can't become a VM)")
	}

	bead := singleResumeBead(t, daemon)
	if got := bead.Fields["execution_target"]; got != podmanager.ExecutionTargetLambdaMicroVM {
		t.Errorf("expected carried-forward execution_target=lambda_microvm, got %q", got)
	}
	if got := bead.Fields["ultracode"]; got != "on" {
		t.Errorf("expected MicroVM resume to force ultracode=on, got %q", got)
	}
	// A MicroVM respawn forces a FRESH cold-mint (reviveBeadID == "" — a VM is
	// never a bead-reopen), so this is a genuine recreate into a NEW bead. It
	// must carry the neutral succession back-pointers to the predecessor bead
	// (task ka-0000006931). seedPriorAgentBeadWithBackend keys the prior bead as
	// "<agentName>-prior" and it has no lineage_root_id, so it anchors the chain.
	const priorID = "thread-9999-1111-prior"
	if got := bead.Fields["prior_agent_id"]; got != priorID {
		t.Errorf("cold-mint respawn must stamp prior_agent_id=%s, got %q", priorID, got)
	}
	if got := bead.Fields["lineage_root_id"]; got != priorID {
		t.Errorf("cold-mint respawn with a root-less predecessor must anchor lineage_root_id=%s, got %q", priorID, got)
	}
}

// TestRespawnThreadAgent_FreshBackendWinsAndIsStripped verifies a --backend
// typed on the wake-up (a) overrides the prior value and (b) is stripped from
// the resume Description so the flag token does not leak into the agent's
// prompt. Here the operator moves a prior-MicroVM thread onto Kubernetes.
func TestRespawnThreadAgent_FreshBackendWinsAndIsStripped(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedPriorAgentBeadWithBackend(daemon, "thread-9999-2222", "gardenio", podmanager.ExecutionTargetLambdaMicroVM)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	// --backend=kubernetes does NOT skip the pool, so force a pool MISS
	// (assignOK=false) to fall through to the cold-resume mint whose fields we
	// assert.
	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "9999.2222",
		"thread-9999-2222", "--backend=kubernetes keep going", "U-HUMAN")

	bead := singleResumeBead(t, daemon)
	if got := bead.Fields["execution_target"]; got != podmanager.ExecutionTargetKubernetes {
		t.Errorf("expected fresh --backend=kubernetes to win over prior microvm, got %q", got)
	}
	if strings.Contains(bead.Description, "--backend") {
		t.Errorf("expected --backend token stripped from resume description, got:\n%s", bead.Description)
	}
	if !strings.Contains(bead.Description, "keep going") {
		t.Errorf("expected remaining prompt text preserved in resume description, got:\n%s", bead.Description)
	}
}
