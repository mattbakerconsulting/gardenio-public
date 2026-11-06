package bridge

import (
	"context"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// These tests close the ka-0000003838 gap: a freeze-eligible (runsc) thread
// agent that called `gb stop` is SUSPENDED, not closed — `gb stop` leaves the
// bead open in agent_state=stopping as the thaw target, the controller
// `runsc checkpoint`s it, and a NAME-keyed snapshot sits on the gVisor node
// (HasSnapshot(agentName)). The controller thaws that snapshot only when an
// AgentSpawn arrives for a bead with the SAME agent name. The warm-pool fast
// path in respawnThreadAgent grabs a DIFFERENTLY named pool pod, so the
// name-keyed thaw never fires and the user gets a fresh session with zero
// continuity — the "resume the previous agent promised never happened" bug.
//
// The fix: when the prior agent bead is in agent_state=stopping
// (frozen-awaiting-thaw — a state written ONLY by the freeze suspend path),
// respawnThreadAgent must SKIP the pool and fall through to the same-name
// cold-resume mint, which is what actually triggers the controller's thaw.

// seedFrozenAgentBead pre-populates the mock with an OPEN prior agent bead in
// agent_state=stopping under the agent name, so FindAgentBead resolves it and
// the respawn path's double-check sees the frozen-awaiting-thaw signal.
func seedFrozenAgentBead(daemon *mockDaemon, agentName, project string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName + "-frozen",
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       agentName,
			"project":     project,
			"agent_state": "stopping",
		},
	}
}

// TestRespawnThreadAgent_FrozenAwaitingThaw_SkipsPoolForSameNameResume verifies
// that a re-mention of a SUSPENDED (frozen) thread agent does NOT consult the
// warm pool — it falls through to a same-name cold-resume mint so the
// controller's name-keyed thaw fires. A pool hit here would abandon the
// snapshot and start a fresh session (the bug).
func TestRespawnThreadAgent_FrozenAwaitingThaw_SkipsPoolForSameNameResume(t *testing.T) {
	daemon := newMockDaemon()
	// A runsc/freeze-eligible channel→role mapping (the gvisor-poc shape).
	daemon.seedProjectWithChannelRole("gardenio", "C-GV", "gvisor-poc")
	const agentName = "thread-8888-2222"
	seedFrozenAgentBead(daemon, agentName, "gardenio")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	// fakeControllerForAssign with hit=true would make the pool a HIT if it were
	// ever called. We assert it is NOT called for a frozen agent.
	var gotRole string
	var mu sync.Mutex
	var poolCalled bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GV", "8888.2222",
		agentName, "are you back?", "U-HUMAN")

	mu.Lock()
	wasPoolCalled := poolCalled
	mu.Unlock()
	if wasPoolCalled {
		t.Fatal("expected respawn of a frozen-awaiting-thaw agent to SKIP the pool " +
			"(a pool pod has a different name → the name-keyed thaw never fires)")
	}

	// Exactly one same-name cold-resume bead must be minted — the AgentSpawn that
	// triggers the controller's thaw of the frozen snapshot.
	var resumeBeads []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, b)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected 1 same-name cold-resume bead for a frozen agent, got %d", len(resumeBeads))
	}
	if resumeBeads[0].Fields["agent"] != agentName {
		t.Errorf("resume bead agent = %q, want %q (same name → name-keyed thaw)",
			resumeBeads[0].Fields["agent"], agentName)
	}
	if resumeBeads[0].Fields["mode"] != "crew" {
		t.Errorf("expected mode=crew (PVC + session resume), got %q", resumeBeads[0].Fields["mode"])
	}
}

// seedFrozenAgentBeadRC is seedFrozenAgentBead with an explicit runtime_class on
// the suspended prior bead, so a revive can carry it forward when the channel
// mapping resolves none.
func seedFrozenAgentBeadRC(daemon *mockDaemon, agentName, project, runtimeClass string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName + "-frozen",
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":         agentName,
			"project":       project,
			"agent_state":   "stopping",
			"runtime_class": runtimeClass,
		},
	}
}

// resumeBeadForAgent returns the single slack-thread-resume bead minted for the
// agent, failing the test if there is not exactly one.
func resumeBeadForAgent(t *testing.T, daemon *mockDaemon) *beadsapi.BeadDetail {
	t.Helper()
	var resumeBeads []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, b)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected exactly 1 slack-thread-resume bead, got %d", len(resumeBeads))
	}
	return resumeBeads[0]
}

// TestRespawnThreadAgent_FrozenThaw_PinsRunscWhenChannelResolvesNone is the
// residual half of the !4532 fix (ka-0000005508). !4532 made a frozen re-mention
// SKIP the pool and mint a same-name resume bead, but that bead's runtime_class
// was derived SOLELY from the channel mapping. When the channel resolves no
// runtime_class — mapping drift, a DM wake, or a transient ListProjectBeads error
// — the re-mint dropped its runsc pin, and the controller's shouldThaw gate
// rejects a non-runsc spec BEFORE it ever checks HasSnapshot. The snapshot then
// never thaws and the user cold-starts: "gvisor thaw still not firing on
// re-mention". The fix carries the prior (frozen) bead's runtime_class forward,
// falling back to runsc since a frozen bead is definitionally runsc.
func TestRespawnThreadAgent_FrozenThaw_PinsRunscWhenChannelResolvesNone(t *testing.T) {
	daemon := newMockDaemon()
	// A channel with a role mapping but NO channel_runtime_class, so
	// runtimeClassFromChannel returns "" — the drift/DM case.
	daemon.seedProjectWithChannelRole("gardenio", "C-GV", "gvisor-poc")
	const agentName = "thread-9191-3131"
	seedFrozenAgentBeadRC(daemon, agentName, "gardenio", "runsc")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var poolCalled bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GV", "9191.3131",
		agentName, "back?", "U-HUMAN")

	resume := resumeBeadForAgent(t, daemon)
	if got := resume.Fields["runtime_class"]; got != "runsc" {
		t.Fatalf("frozen revive with no channel runtime_class must stay pinned to runsc "+
			"(else shouldThaw rejects the spec before HasSnapshot and the snapshot never thaws); got %q", got)
	}
}

// TestRespawnThreadAgent_FrozenThaw_DefaultsRunscWhenPriorBeadUnset covers the
// belt-and-suspenders case: the frozen bead itself carries no runtime_class field
// AND the channel resolves none. Because a frozen-awaiting-thaw bead is
// definitionally runsc (only runsc pods freeze), the revive must still pin runsc.
func TestRespawnThreadAgent_FrozenThaw_DefaultsRunscWhenPriorBeadUnset(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("gardenio", "C-GV", "gvisor-poc")
	const agentName = "thread-4242-7878"
	// seedFrozenAgentBead sets agent_state=stopping but NO runtime_class field.
	seedFrozenAgentBead(daemon, agentName, "gardenio")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var poolCalled bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GV", "4242.7878",
		agentName, "back?", "U-HUMAN")

	resume := resumeBeadForAgent(t, daemon)
	if got := resume.Fields["runtime_class"]; got != "runsc" {
		t.Fatalf("frozen revive must default runtime_class to runsc even when the prior "+
			"bead carried none; got %q", got)
	}
}

// TestRespawnThreadAgent_FrozenThaw_ChannelRuntimeClassStillWins verifies the fix
// does not override a channel-resolved runtime_class: when the channel DOES pin a
// runtime_class, that value is used verbatim (the normal, pre-fix behaviour).
func TestRespawnThreadAgent_FrozenThaw_ChannelRuntimeClassStillWins(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRuntimeClass("gardenio", "C-TURBO", "runsc")
	const agentName = "thread-5050-6060"
	seedFrozenAgentBeadRC(daemon, agentName, "gardenio", "runsc")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var poolCalled bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-TURBO", "5050.6060",
		agentName, "back?", "U-HUMAN")

	resume := resumeBeadForAgent(t, daemon)
	if got := resume.Fields["runtime_class"]; got != "runsc" {
		t.Fatalf("channel-pinned runtime_class must be used verbatim; got %q", got)
	}
}

// TestRespawnThreadAgent_NonFrozen_NoRunscOverPin guards against over-pinning: a
// terminal DONE (non-frozen) resume that reaches the cold-mint path (e.g. pool
// disabled) must NOT be force-pinned to runsc — only a frozen-awaiting-thaw revive
// gets the runsc fallback. With no channel runtime_class and a done prior bead the
// resume bead must carry no runtime_class at all.
func TestRespawnThreadAgent_NonFrozen_NoRunscOverPin(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-DN", "thread")
	const agentName = "thread-1212-3434"
	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName + "-done",
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       agentName,
			"project":     "monorepo",
			"agent_state": "done",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var poolCalled bool
	// hit=false → pool miss forces the cold-mint path so a resume bead is minted.
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-DN", "1212.3434",
		agentName, "follow-up", "U-HUMAN")

	resume := resumeBeadForAgent(t, daemon)
	if got := resume.Fields["runtime_class"]; got != "" {
		t.Fatalf("a non-frozen resume must NOT be force-pinned to runsc; got runtime_class=%q", got)
	}
}

// TestRespawnThreadAgent_DoneState_StillUsesPool guards against over-reaching:
// a terminal DONE agent (the common runc thread case) is NOT frozen and must
// keep using the warm pool fast path — only agent_state=stopping skips it.
func TestRespawnThreadAgent_DoneState_StillUsesPool(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-DN", "thread")
	const agentName = "thread-7777-4444"
	// Open bead in a terminal DONE state (not stopping) — the non-frozen path.
	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName + "-done",
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       agentName,
			"project":     "monorepo",
			"agent_state": "done",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var poolCalled bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &poolCalled)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-DN", "7777.4444",
		agentName, "follow-up", "U-HUMAN")

	mu.Lock()
	wasPoolCalled := poolCalled
	mu.Unlock()
	if !wasPoolCalled {
		t.Fatal("expected respawn of a DONE (non-frozen) agent to still use the warm pool fast path")
	}
}
