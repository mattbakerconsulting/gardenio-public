package main

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// The regression suite for ka-0000001482: a controller-initiated account-rotation
// restart must not kill an agent mid-turn.
//
// The incident (2026-07-27 20:27:36Z): ReconcileStrandedRecovered deleted 5 pods
// that were actively working — one had reported `state=working ready=true` 52
// seconds earlier — because none of the restarter's delete paths consulted agent
// state. The Slack death report then blamed "node drain, rollout, or Spot
// reclaim", all three false on an all-on-demand cluster with no rollout.

// fakeDrainGate stands in for reconciler.UpgradeTracker. agentIdle drives what
// DrainReady reports; timedOut forces the timeout branch.
type fakeDrainGate struct {
	agentIdle bool
	timedOut  bool

	draining map[string]bool
	nudges   []string // pods nudged, in order
	agents   []string // agents the drain mail was addressed to
	msgs     []string
}

func newFakeDrainGate() *fakeDrainGate {
	return &fakeDrainGate{draining: map[string]bool{}}
}

func (f *fakeDrainGate) IsDraining(podName string) bool { return f.draining[podName] }

func (f *fakeDrainGate) StartDrainWithMsg(_ context.Context, podName, agentName, msg string) bool {
	f.draining[podName] = true
	f.nudges = append(f.nudges, podName)
	f.agents = append(f.agents, agentName)
	f.msgs = append(f.msgs, msg)
	return true
}

func (f *fakeDrainGate) DrainReady(_ context.Context, podName string, _ time.Duration) bool {
	if f.timedOut {
		delete(f.draining, podName)
		return true
	}
	if f.agentIdle {
		delete(f.draining, podName)
		return true
	}
	return false
}

// fakeBeadStamper records controller_restart_reason writes.
type fakeBeadStamper struct {
	fields map[string]map[string]string
	err    error
}

func newFakeBeadStamper() *fakeBeadStamper {
	return &fakeBeadStamper{fields: map[string]map[string]string{}}
}

func (f *fakeBeadStamper) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	if f.err != nil {
		return f.err
	}
	if f.fields[beadID] == nil {
		f.fields[beadID] = map[string]string{}
	}
	maps.Copy(f.fields[beadID], fields)
	return nil
}

// workingStrandedPod is a stranded pod that is REACHABLE (running, with an agent
// name to mail) and carries an agent bead id, i.e. a live mid-turn agent.
// agent "" leaves BOAT_AGENT unset.
func workingStrandedPod(name, project, litellmKey, agent, beadID string) corev1.Pod {
	pod := strandedPodForProject(name, project, litellmKey)
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BOAT_AGENT_BEAD_ID", Value: beadID},
	)
	if agent != "" {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "BOAT_AGENT", Value: agent})
	}
	pod.Status.Phase = corev1.PodRunning
	return pod
}

// strandedRecoveryFixture builds the standard "pod stranded on a dead account,
// healthy sibling available" setup the sweep acts on.
func strandedRecoveryFixture(pod corev1.Pod) (*config.Config, *mockPodManager, *mockSessionAccounts) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio3": strandedAccount("sk-litellm-exhausted", "project:gardenio"),
			"shannon":   acct("sk-litellm-healthy", "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{pod}}
	// coopmux confirms the session is still ON the stranded account, so the
	// in-process-switch guard does not short-circuit the sweep.
	sessions := &mockSessionAccounts{accounts: map[string]string{pod.Name: "gardenio3"}}
	return cfg, pm, sessions
}

// TestStrandedRecovery_DoesNotDeleteWorkingAgent is THE regression test for the
// incident: a pod whose agent is mid-turn is nudged to land its WIP and left
// alive, instead of being deleted out from under the running turn.
func TestStrandedRecovery_DoesNotDeleteWorkingAgent(t *testing.T) {
	cfg, pm, sessions := strandedRecoveryFixture(
		workingStrandedPod("agent-working", "gardenio", "sk-litellm-exhausted", "working-agent", "gd-AAA"))
	gate := newFakeDrainGate() // agentIdle=false → still working
	r := newRestarterWithSessions(pm, sessions).withDrainGate(gate, time.Minute)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)

	if len(deleted) != 0 {
		t.Fatalf("a mid-turn agent must not be deleted, got %v", deleted)
	}
	if len(pm.deleted) != 0 {
		t.Fatalf("no pod delete should have been issued, got %v", pm.deleted)
	}
	// It must have been ASKED to wrap up — a silent defer would stall the pod on a
	// dead account forever.
	if len(gate.nudges) != 1 || gate.nudges[0] != "agent-working" {
		t.Fatalf("expected exactly one drain nudge for agent-working, got %v", gate.nudges)
	}
	if want := "working-agent"; gate.agents[0] != want {
		t.Errorf("drain mail addressed to %q, want %q", gate.agents[0], want)
	}
	if gate.msgs[0] != accountRotationDrainNudgeMsg {
		t.Errorf("nudge message = %q, want the account-rotation drain message", gate.msgs[0])
	}
}

// TestStrandedRecovery_DeletesOnceAgentIdle verifies the gate defers rather than
// cancels: once the agent finishes its turn the restart proceeds, so a stranded
// pod still gets moved onto a healthy account.
func TestStrandedRecovery_DeletesOnceAgentIdle(t *testing.T) {
	cfg, pm, sessions := strandedRecoveryFixture(
		workingStrandedPod("agent-working", "gardenio", "sk-litellm-exhausted", "working-agent", "gd-AAA"))
	gate := newFakeDrainGate()
	r := newRestarterWithSessions(pm, sessions).withDrainGate(gate, time.Minute)

	if deleted := r.ReconcileStrandedRecovered(context.Background(), cfg); len(deleted) != 0 {
		t.Fatalf("first sweep (agent working) must defer, got %v", deleted)
	}

	gate.agentIdle = true
	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-working" {
		t.Fatalf("idle agent must be restarted, got %v", deleted)
	}
	// Still only ONE nudge across both sweeps — IsDraining dedups the re-entry, so
	// a long turn is not spammed every tick (ka-0000000550).
	if len(gate.nudges) != 1 {
		t.Errorf("expected 1 nudge across both sweeps, got %d (%v)", len(gate.nudges), gate.nudges)
	}
}

// TestStrandedRecovery_DeletesOnDrainTimeout verifies the gate can never
// permanently strand a pod: a wedged agent that never reports idle is recycled
// once the drain timeout expires.
func TestStrandedRecovery_DeletesOnDrainTimeout(t *testing.T) {
	cfg, pm, sessions := strandedRecoveryFixture(
		workingStrandedPod("agent-wedged", "gardenio", "sk-litellm-exhausted", "wedged-agent", "gd-BBB"))
	gate := newFakeDrainGate()
	gate.timedOut = true // never reaches idle, but the window has closed
	r := newRestarterWithSessions(pm, sessions).withDrainGate(gate, time.Millisecond)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-wedged" {
		t.Fatalf("drain timeout must force the restart, got %v", deleted)
	}
}

// TestStrandedRecovery_DeletesUnreachablePodImmediately verifies a pod with no
// live session to mail (not running) is deleted without waiting. There is no
// way to nudge it, so blocking on it would strand it on a dead account for the
// full timeout.
func TestStrandedRecovery_DeletesUnreachablePodImmediately(t *testing.T) {
	pod := workingStrandedPod("agent-pending", "gardenio", "sk-litellm-exhausted", "pending-agent", "gd-CCC")
	pod.Status.Phase = corev1.PodPending
	cfg, pm, sessions := strandedRecoveryFixture(pod)
	gate := newFakeDrainGate() // would report "still working" if consulted
	r := newRestarterWithSessions(pm, sessions).withDrainGate(gate, time.Minute)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-pending" {
		t.Fatalf("unreachable pod must be restarted immediately, got %v", deleted)
	}
	if len(gate.nudges) != 0 {
		t.Errorf("must not attempt to nudge a pod that is not running, got %v", gate.nudges)
	}
}

// TestPodDrainAgent pins where the drain mail's address comes from: the
// BOAT_AGENT env baked at spawn, else the gardenio.io/agent label, and nobody
// for a pod that is not running.
func TestPodDrainAgent(t *testing.T) {
	pod := workingStrandedPod("p", "gardenio", "sk", "env-agent", "gd-X")
	pod.Labels = map[string]string{podmanager.LabelAgent: "label-agent"}
	if got := podDrainAgent(&pod); got != "env-agent" {
		t.Errorf("podDrainAgent = %q, want the BOAT_AGENT env value", got)
	}

	noEnv := workingStrandedPod("p", "gardenio", "sk", "", "gd-X")
	noEnv.Labels = map[string]string{podmanager.LabelAgent: "label-agent"}
	if got := podDrainAgent(&noEnv); got != "label-agent" {
		t.Errorf("podDrainAgent = %q, want the label fallback", got)
	}

	pod.Status.Phase = corev1.PodPending
	if got := podDrainAgent(&pod); got != "" {
		t.Errorf("podDrainAgent on a pending pod = %q, want empty", got)
	}
}

// TestStrandedRecovery_DeletesWhenGateCannotDrain: a gate that cannot open a
// drain (StartDrainWithMsg false, e.g. no messenger) must not block the restart.
func TestStrandedRecovery_DeletesWhenGateCannotDrain(t *testing.T) {
	cfg, pm, sessions := strandedRecoveryFixture(
		workingStrandedPod("agent-working", "gardenio", "sk-litellm-exhausted", "working-agent", "gd-AAA"))
	gate := &refusingDrainGate{newFakeDrainGate()}
	r := newRestarterWithSessions(pm, sessions).withDrainGate(gate, time.Minute)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-working" {
		t.Fatalf("a gate that cannot drain must not block the restart, got %v", deleted)
	}
}

// refusingDrainGate is a fakeDrainGate that never opens a drain.
type refusingDrainGate struct{ *fakeDrainGate }

func (refusingDrainGate) StartDrainWithMsg(context.Context, string, string, string) bool {
	return false
}

// TestStrandedRecovery_NilGateDeletesImmediately pins the fail-open: a controller
// wired without a drain gate keeps the pre-fix behavior rather than refusing to
// recycle pods at all.
func TestStrandedRecovery_NilGateDeletesImmediately(t *testing.T) {
	cfg, pm, sessions := strandedRecoveryFixture(
		workingStrandedPod("agent-working", "gardenio", "sk-litellm-exhausted", "working-agent", "gd-AAA"))
	r := newRestarterWithSessions(pm, sessions) // no withDrainGate

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-working" {
		t.Fatalf("nil drain gate must delete immediately, got %v", deleted)
	}
}

// TestStrandedRecovery_StampsControllerRestartReason verifies the death report
// gets ground truth. Without this the report blames "node drain, rollout, or Spot
// reclaim" for a delete the controller itself performed (ka-0000001482 D3).
func TestStrandedRecovery_StampsControllerRestartReason(t *testing.T) {
	cfg, pm, sessions := strandedRecoveryFixture(
		workingStrandedPod("agent-working", "gardenio", "sk-litellm-exhausted", "working-agent", "gd-AAA"))
	gate := newFakeDrainGate()
	gate.agentIdle = true
	beads := newFakeBeadStamper()
	r := newRestarterWithSessions(pm, sessions).
		withDrainGate(gate, time.Minute).
		withBeadStamper(beads)

	if deleted := r.ReconcileStrandedRecovered(context.Background(), cfg); len(deleted) != 1 {
		t.Fatalf("expected the idle pod to be restarted, got %v", deleted)
	}

	got := beads.fields["gd-AAA"][controllerRestartReasonField]
	if got == "" {
		t.Fatal("controller_restart_reason was not stamped on the agent bead")
	}
	// The reason must name the controller as the actor and identify the accounts,
	// since that is exactly what the misleading report omitted.
	for _, want := range []string{"controller restarted", "gardenio3", "shannon"} {
		if !strings.Contains(got, want) {
			t.Errorf("stamped reason %q does not mention %q", got, want)
		}
	}
}

// TestStrandedRecovery_StampFailureDoesNotBlockRestart verifies the stamp is
// best-effort: a beads outage must not prevent recycling a stranded pod.
func TestStrandedRecovery_StampFailureDoesNotBlockRestart(t *testing.T) {
	cfg, pm, sessions := strandedRecoveryFixture(
		workingStrandedPod("agent-working", "gardenio", "sk-litellm-exhausted", "working-agent", "gd-AAA"))
	gate := newFakeDrainGate()
	gate.agentIdle = true
	beads := newFakeBeadStamper()
	beads.err = fmt.Errorf("beads down")
	r := newRestarterWithSessions(pm, sessions).
		withDrainGate(gate, time.Minute).
		withBeadStamper(beads)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-working" {
		t.Fatalf("a failed stamp must not block the restart, got %v", deleted)
	}
}

// TestTerminalExhaustion_DrainsBeforeDelete covers the second live-session delete
// site: the account is terminally dead, but the session is alive and can still
// land its WIP, so the kill waits for the turn to end.
func TestTerminalExhaustion_DrainsBeforeDelete(t *testing.T) {
	key := "sk-litellm-terminal"
	pod := workingStrandedPod("agent-pinned", "gardenio", key, "pinned-agent", "gd-DDD")
	pm := &mockPodManager{pods: []corev1.Pod{pod}}
	gate := newFakeDrainGate()
	r := newRestarterForTest(pm).withDrainGate(gate, time.Minute)

	// Seed the status snapshot, then drive the active→exhausted edge. The
	// exhaustion must be TERMINAL (a hard probe auth rejection) — Reconcile
	// deliberately leaves pods alone on a transient rate-limit window, since the
	// window reset recovers them in place (ka-0000002085).
	terminal := acct(key, "exhausted", "", "project:gardenio")
	terminal.ProbeAuthStatus = "rejected"
	accounts := map[string]config.AccountCacheEntry{
		"gardenio": acct(key, "active", "", "project:gardenio"),
	}
	r.Reconcile(context.Background(), accounts)
	accounts["gardenio"] = terminal

	if deleted := r.Reconcile(context.Background(), accounts); len(deleted) != 0 {
		t.Fatalf("mid-turn agent must not be deleted on terminal exhaustion, got %v", deleted)
	}
	if len(gate.nudges) != 1 {
		t.Fatalf("expected the agent to be nudged to wrap up, got %v", gate.nudges)
	}

	gate.agentIdle = true
	if deleted := r.Reconcile(context.Background(), accounts); len(deleted) != 1 {
		t.Fatalf("idle agent must be restarted off the dead account, got %v", deleted)
	}
}

// TestTerminalExhaustion_DeferredRestartIsRetriedAcrossCycles guards the trap the
// drain gate introduces on this path: Reconcile is EDGE-triggered, so once the
// active→exhausted edge is consumed it never revisits the account. A deferred
// delete with no re-arm would leave the pod on a dead account forever — the gate
// would have traded a context loss for a permanent strand, which is worse.
//
// Here the account stays exhausted (no new edge) and the agent goes idle only on
// a LATER cycle; the restart must still happen.
func TestTerminalExhaustion_DeferredRestartIsRetriedAcrossCycles(t *testing.T) {
	key := "sk-litellm-terminal"
	pod := workingStrandedPod("agent-pinned", "gardenio", key, "pinned-agent", "gd-DDD")
	pm := &mockPodManager{pods: []corev1.Pod{pod}}
	gate := newFakeDrainGate()
	r := newRestarterForTest(pm).withDrainGate(gate, time.Minute)

	terminal := acct(key, "exhausted", "", "project:gardenio")
	terminal.ProbeAuthStatus = "rejected"
	accounts := map[string]config.AccountCacheEntry{
		"gardenio": acct(key, "active", "", "project:gardenio"),
	}
	r.Reconcile(context.Background(), accounts) // seed
	accounts["gardenio"] = terminal

	// Cycle 1: the edge fires, agent is mid-turn → deferred.
	if deleted := r.Reconcile(context.Background(), accounts); len(deleted) != 0 {
		t.Fatalf("cycle 1 must defer the mid-turn agent, got %v", deleted)
	}

	// Cycle 2: still exhausted (NO new edge) and still working → still deferred.
	if deleted := r.Reconcile(context.Background(), accounts); len(deleted) != 0 {
		t.Fatalf("cycle 2 must keep deferring, got %v", deleted)
	}

	// Cycle 3: the turn ends. The restart must fire even though the transition
	// edge was consumed two cycles ago.
	gate.agentIdle = true
	deleted := r.Reconcile(context.Background(), accounts)
	if len(deleted) != 1 || deleted[0] != "agent-pinned" {
		t.Fatalf("deferred restart must be retried once the agent idles, got %v", deleted)
	}
	// And the retry entry is cleared, so it does not re-arm forever.
	if _, still := r.pendingTerminal["gardenio"]; still {
		t.Error("pendingTerminal must be cleared once no pod is left draining")
	}
}

// TestTerminalExhaustion_PendingClearedWhenAccountRecovers verifies a deferred
// retry is abandoned if the account comes back healthy — there is no longer a
// dead account to move the pod off, so restarting it would be pure churn.
func TestTerminalExhaustion_PendingClearedWhenAccountRecovers(t *testing.T) {
	key := "sk-litellm-terminal"
	pod := workingStrandedPod("agent-pinned", "gardenio", key, "pinned-agent", "gd-DDD")
	pm := &mockPodManager{pods: []corev1.Pod{pod}}
	gate := newFakeDrainGate()
	r := newRestarterForTest(pm).withDrainGate(gate, time.Minute)

	terminal := acct(key, "exhausted", "", "project:gardenio")
	terminal.ProbeAuthStatus = "rejected"
	accounts := map[string]config.AccountCacheEntry{
		"gardenio": acct(key, "active", "", "project:gardenio"),
	}
	r.Reconcile(context.Background(), accounts) // seed
	accounts["gardenio"] = terminal
	if deleted := r.Reconcile(context.Background(), accounts); len(deleted) != 0 {
		t.Fatalf("expected the mid-turn agent to be deferred, got %v", deleted)
	}

	// The account recovers before the agent idles.
	accounts["gardenio"] = acct(key, "active", "", "project:gardenio")
	gate.agentIdle = true
	if deleted := r.Reconcile(context.Background(), accounts); len(deleted) != 0 {
		t.Fatalf("a recovered account must not restart the pod, got %v", deleted)
	}
	if _, still := r.pendingTerminal["gardenio"]; still {
		t.Error("pendingTerminal must be cleared when the account recovers")
	}
}

// TestStaleVKey_DeletesWithoutDraining pins the DELIBERATE exception. A pod whose
// baked key was rotated out 401s on every request, so Claude is in an error state
// and the drain nudge is undeliverable by construction. Draining would add the
// full timeout to a pod that is guaranteed dead and cannot land WIP — so this
// path must keep deleting promptly even with a gate wired.
func TestStaleVKey_DeletesWithoutDraining(t *testing.T) {
	pod := workingStrandedPod("agent-401", "gardenio", "sk-litellm-rotated-out", "agent-401", "gd-EEE")
	pm := &mockPodManager{pods: []corev1.Pod{pod}}
	gate := newFakeDrainGate() // would report "still working" if consulted
	r := newRestarterForTest(pm).withDrainGate(gate, time.Minute)

	// The pod's baked key belongs to no current account → stale.
	deleted := r.ReconcileStaleVKeys(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": acct("sk-litellm-current", "active", "", "project:gardenio"),
	})
	if len(deleted) != 1 || deleted[0] != "agent-401" {
		t.Fatalf("a 401-crashlooping pod must be restarted immediately, got %v", deleted)
	}
	if len(gate.nudges) != 0 {
		t.Errorf("stale-vkey path must not drain (nudge is undeliverable), got %v", gate.nudges)
	}
}
