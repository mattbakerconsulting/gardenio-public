package reconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/beadsapi"
)

// fakeMessenger stands in for the controller's drainMessenger: it records the
// drain mail, keeps each agent's open mail the way the daemon does (until it
// is closed), and serves the agent bead DrainReady reads.
type fakeMessenger struct {
	mu         sync.Mutex
	fails      int // Nudge fails this many times before it succeeds
	closeFails int // CloseMail fails this many times before it succeeds
	sent       []string
	to         []string
	open       map[string]string // mail id -> addressee, while open
	closed     []string
	bead       *beadsapi.BeadDetail
	beadErr    error
}

func (f *fakeMessenger) Nudge(_ context.Context, agentName, message string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return "", fmt.Errorf("daemon unavailable")
	}
	f.sent = append(f.sent, message)
	f.to = append(f.to, agentName)
	id := fmt.Sprintf("gd-mail%d", len(f.sent))
	if f.open == nil {
		f.open = map[string]string{}
	}
	f.open[id] = agentName
	return id, nil
}

func (f *fakeMessenger) CloseMail(_ context.Context, mailID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeFails > 0 {
		f.closeFails--
		return fmt.Errorf("daemon unavailable")
	}
	delete(f.open, mailID)
	f.closed = append(f.closed, mailID)
	return nil
}

// openMailFor is what a new mail subscriber for agentName would drain.
func (f *fakeMessenger) openMailFor(agentName string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, to := range f.open {
		if to == agentName {
			ids = append(ids, id)
		}
	}
	return ids
}

func (f *fakeMessenger) FindAgentBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beadErr != nil {
		return nil, f.beadErr
	}
	return f.bead, nil
}

func (f *fakeMessenger) setState(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bead = agentBeadInState(state)
}

func agentBeadInState(state string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: "gd-agent", Fields: map[string]string{"agent_state": state}}
}

// drainTracker returns a tracker wired to a messenger whose agent bead reads
// state. mailGrace is zeroed so a resting agent is drainable at once; the
// grace has its own test.
func drainTracker(state string) (*UpgradeTracker, *fakeMessenger) {
	m := &fakeMessenger{bead: agentBeadInState(state)}
	tr := NewUpgradeTracker(testLogger())
	tr.SetMessenger(m)
	tr.mailGrace = 0
	return tr, m
}

// TestUpgradeTracker_CleanStaleDrains_RemovesGonePod proves the drain leak fix
// (ka-0000000604): the node-drain branch opens a draining entry then continue's
// without ever calling DrainReady (the only path to clearDrain), so a
// force-reclaimed pod's entry leaks. CleanStaleDrains must prune any draining
// entry whose pod is absent from the live set, so a same-named recreated pod
// opens a FRESH drain on a later drain instead of inheriting the stale
// entry's (already-expired) started time. Fails pre-fix (method absent / entry
// never removed).
func TestUpgradeTracker_CleanStaleDrains_RemovesGonePod(t *testing.T) {
	tr, _ := drainTracker("working")
	// Simulate the node-drain branch: open a drain, never call DrainReady.
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-gone", "gone", nodeDrainNudgeMsg)
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-live", "live", nodeDrainNudgeMsg)
	if !tr.IsDraining("crew-p-r-gone") || !tr.IsDraining("crew-p-r-live") {
		t.Fatal("both pods should be marked draining")
	}

	// The node teardown killed crew-p-r-gone; only crew-p-r-live is still live.
	tr.CleanStaleDrains(map[string]struct{}{"crew-p-r-live": {}})

	if tr.IsDraining("crew-p-r-gone") {
		t.Error("stale drain entry for a gone pod should have been cleaned")
	}
	if !tr.IsDraining("crew-p-r-live") {
		t.Error("drain entry for a still-live pod must be preserved")
	}
}

// TestUpgradeTracker_CleanStaleDrains_LetsRecreatedPodReDrain proves the
// downstream data-loss fix: after a leaked entry is pruned, a same-named pod
// recreated on a healthy node opens a FRESH drain (new started time), so its
// later drain waits for the agent to commit+push instead of the
// stale entry's already-expired started tripping DrainReady's immediate
// timeout branch. Pre-fix (no prune) the recreated pod inherits the stale
// entry and is force-deleted with no graceful drain.
func TestUpgradeTracker_CleanStaleDrains_LetsRecreatedPodReDrain(t *testing.T) {
	tr, _ := drainTracker("working")
	// Node-drain opens the entry, then leaves it (no DrainReady), and backdates
	// started well past any drain timeout — exactly the leaked-and-stale state.
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	tr.mu.Lock()
	tr.draining["crew-p-r-a"].started = time.Now().Add(-1 * time.Hour)
	tr.mu.Unlock()

	// The pod is gone; the reconcile pass prunes the stale entry.
	tr.CleanStaleDrains(map[string]struct{}{})
	if tr.IsDraining("crew-p-r-a") {
		t.Fatal("stale entry must be pruned before the pod is recreated")
	}

	// A same-named pod is recreated; a later drain opens a fresh
	// drain and DrainReady must NOT immediately time out (agent is "working").
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	if tr.DrainReady(context.Background(), "crew-p-r-a", 5*time.Minute) {
		t.Error("recreated pod's fresh drain must wait for the working agent, " +
			"not trip the stale-started timeout")
	}
}

func TestUpgradeTracker_StartDrain_SuccessAndIsDraining(t *testing.T) {
	tr, m := drainTracker("idle")
	if !tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg) {
		t.Fatal("StartDrainWithMsg should return true")
	}
	if !tr.IsDraining("crew-p-r-a") {
		t.Fatal("pod should be marked draining")
	}

	// Second call on same pod returns true (idempotent) and does not re-nudge.
	if !tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg) {
		t.Fatal("StartDrainWithMsg on existing drain should return true")
	}
	if len(m.sent) != 1 {
		t.Errorf("expected one drain mail, got %d", len(m.sent))
	}
}

// TestUpgradeTracker_StartDrain_CannotReachAgent: with no messenger wired, or
// no agent name to mail, there is nobody to ask, so StartDrainWithMsg opens nothing
// and reports false; the caller then replaces the pod without waiting.
func TestUpgradeTracker_StartDrain_CannotReachAgent(t *testing.T) {
	bare := NewUpgradeTracker(testLogger())
	if bare.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg) {
		t.Error("StartDrainWithMsg without a messenger must report false")
	}
	if bare.IsDraining("crew-p-r-a") {
		t.Error("no drain entry may be opened without a messenger")
	}

	tr, m := drainTracker("working")
	if tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "", nodeDrainNudgeMsg) {
		t.Error("StartDrainWithMsg with no agent name must report false")
	}
	if tr.IsDraining("crew-p-r-a") || len(m.sent) != 0 {
		t.Error("no drain entry or mail for a pod with no agent name")
	}
}

func TestUpgradeTracker_StartDrain_NudgeFailureStillTracks(t *testing.T) {
	tr, m := drainTracker("working")
	m.fails = 1
	if !tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg) {
		t.Fatal("StartDrainWithMsg should still return true when nudge fails")
	}
	if !tr.IsDraining("crew-p-r-a") {
		t.Fatal("pod should be marked draining even if nudge failed")
	}
	// nudged flag should still be false so a retry can fire later.
	tr.mu.Lock()
	entry := tr.draining["crew-p-r-a"]
	tr.mu.Unlock()
	if entry == nil || entry.nudged {
		t.Fatal("nudged flag should be false when nudge failed")
	}
}

func TestUpgradeTracker_DrainReady_NotDraining(t *testing.T) {
	tr := NewUpgradeTracker(testLogger())
	// Not draining → treated as ready.
	if !tr.DrainReady(context.Background(), "nobody", time.Minute) {
		t.Error("non-draining pod should be reported ready")
	}
}

func TestUpgradeTracker_DrainReady_AgentIdleClearsDrain(t *testing.T) {
	tr, _ := drainTracker("idle")
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	if !tr.DrainReady(context.Background(), "crew-p-r-a", time.Minute) {
		t.Fatal("idle agent should be drain-ready")
	}
	if tr.IsDraining("crew-p-r-a") {
		t.Error("drain entry should be cleared after ready")
	}
}

// TestUpgradeTracker_DrainReady_IdleWaitsForMail: the mail reaches the session
// seconds after it is sent, so a bead that still reads idle right after the
// nudge is not proof the agent landed anything. Within drainMailGrace it must
// wait; a finished agent (done) needs no grace.
func TestUpgradeTracker_DrainReady_IdleWaitsForMail(t *testing.T) {
	tr, m := drainTracker("idle")
	tr.mailGrace = time.Hour
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	if tr.DrainReady(context.Background(), "crew-p-r-a", 2*time.Hour) {
		t.Fatal("idle agent must get the mail grace before it is drained")
	}

	m.setState("done")
	if !tr.DrainReady(context.Background(), "crew-p-r-a", 2*time.Hour) {
		t.Fatal("done agent is drain-ready without waiting out the grace")
	}
}

// TestUpgradeTracker_DrainReady_StopRequestedIsReady: `gb stop` sets
// stop_requested before agent_state leaves working, and stopping is exactly
// what the drain mail asks for.
func TestUpgradeTracker_DrainReady_StopRequestedIsReady(t *testing.T) {
	tr, m := drainTracker("working")
	m.bead.Fields["stop_requested"] = "true"
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	if !tr.DrainReady(context.Background(), "crew-p-r-a", time.Hour) {
		t.Fatal("agent with stop_requested=true should be drain-ready")
	}
}

// TestUpgradeTracker_DrainReady_AgentBeadGoneIsReady: no active agent bead
// means the agent closed out; there is nothing left to land.
func TestUpgradeTracker_DrainReady_AgentBeadGoneIsReady(t *testing.T) {
	tr, m := drainTracker("working")
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	m.beadErr = fmt.Errorf("lookup: %w", beadsapi.ErrAgentNotFound)
	if !tr.DrainReady(context.Background(), "crew-p-r-a", time.Hour) {
		t.Fatal("closed agent bead should be drain-ready")
	}
}

func TestUpgradeTracker_DrainReady_TimeoutForcesReady(t *testing.T) {
	tr, _ := drainTracker("working")
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	// Force started time into the past.
	tr.mu.Lock()
	tr.draining["crew-p-r-a"].started = time.Now().Add(-10 * time.Minute)
	tr.mu.Unlock()

	if !tr.DrainReady(context.Background(), "crew-p-r-a", time.Minute) {
		t.Fatal("expired drain should be forced ready")
	}
	if tr.IsDraining("crew-p-r-a") {
		t.Error("drain entry should be cleared after timeout")
	}
}

func TestUpgradeTracker_DrainReady_AgentBusyNotReady(t *testing.T) {
	for _, state := range []string{"working", "spawning", ""} {
		tr, _ := drainTracker(state)
		tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)

		if tr.DrainReady(context.Background(), "crew-p-r-a", time.Hour) {
			t.Fatalf("agent_state=%q should not be drain-ready", state)
		}
		if !tr.IsDraining("crew-p-r-a") {
			t.Errorf("agent_state=%q: drain entry should remain", state)
		}
	}
}

func TestUpgradeTracker_DrainReady_AgentBeadError(t *testing.T) {
	// Mail goes out, the bead read fails.
	tr, m := drainTracker("idle")
	m.beadErr = errors.New("daemon 503")
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	if tr.DrainReady(context.Background(), "crew-p-r-a", time.Hour) {
		t.Fatal("bead read error should leave pod not-ready")
	}
	if !tr.IsDraining("crew-p-r-a") {
		t.Error("drain entry should survive a transient bead read error")
	}
}

func TestUpgradeTracker_DrainReady_RetryNudgeAfterInitialFailure(t *testing.T) {
	// First mail fails (at StartDrainWithMsg), second succeeds (at DrainReady).
	tr, m := drainTracker("working")
	m.fails = 1
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	tr.DrainReady(context.Background(), "crew-p-r-a", time.Hour)

	tr.mu.Lock()
	entry := tr.draining["crew-p-r-a"]
	tr.mu.Unlock()
	if entry == nil || !entry.nudged {
		t.Error("retry nudge should have marked drain nudged=true")
	}
}

func TestDrainVerdict(t *testing.T) {
	cases := []struct {
		state string
		stop  bool
		want  drainState
	}{
		{"working", false, drainBusy},
		{"spawning", false, drainBusy},
		{"resuming", false, drainBusy},
		{"", false, drainBusy},
		{"idle", false, drainResting},
		{"yielding", false, drainResting},
		{"rate_limited", false, drainResting},
		{"done", false, drainDone},
		{"failed", false, drainDone},
		{"stopping", false, drainDone},
		{"stop_requested", false, drainDone},
		{"budget_exceeded", false, drainDone},
		{"working", true, drainDone},
	}
	for _, c := range cases {
		if got := drainVerdict(c.state, c.stop); got != c.want {
			t.Errorf("drainVerdict(%q, stop=%v) = %d, want %d", c.state, c.stop, got, c.want)
		}
	}
}

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "running with ready condition true",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "running but ready=false",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			want: false,
		},
		{
			name: "pending phase",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			},
			want: false,
		},
		{
			name: "running with no ready condition",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.pod.ObjectMeta = metav1.ObjectMeta{Name: "p"}
			if got := IsPodReady(tt.pod); got != tt.want {
				t.Errorf("got=%v want=%v", got, tt.want)
			}
		})
	}
}

// TestNodeDrainNudgeMsg_IsActionable guards the WS1 (ka-0000007969) node-drain
// nudge: it must give a concrete commit+push+gb-stop action and mention the S3
// salvage fallback, and signal a node reclaim (not an image upgrade) so the
// agent understands why it is being asked to land.
func TestNodeDrainNudgeMsg_IsActionable(t *testing.T) {
	wantSubstrings := []string{
		"git add -A && git commit && git push", // concrete action
		"gb stop",                              // clean despawn after pushing
		"node is being reclaimed",              // the reason (not an image upgrade)
		"salvaged to S3",                       // truthful fallback
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(nodeDrainNudgeMsg, want) {
			t.Errorf("nodeDrainNudgeMsg missing %q\ngot: %s", want, nodeDrainNudgeMsg)
		}
	}
	// It must NOT claim an image upgrade — that would be a misleading reason.
	if strings.Contains(nodeDrainNudgeMsg, "Image upgrade") {
		t.Errorf("nodeDrainNudgeMsg should not mention an image upgrade: %s", nodeDrainNudgeMsg)
	}
}

// TestStartDrainWithMsg_SendsCustomMessage verifies the node-drain path's own
// wording reaches the agent as mail, and that the DrainReady retry path
// re-sends that same wording after a first-try nudge failure.
func TestStartDrainWithMsg_SendsCustomMessage(t *testing.T) {
	// Report "working" so DrainReady does not short-circuit to ready.
	tr, m := drainTracker("working")
	m.fails = 1 // first nudge fails so DrainReady retries
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	// First nudge failed, so the entry is tracked but not yet nudged; DrainReady
	// retries and should re-send nodeDrainNudgeMsg.
	tr.DrainReady(context.Background(), "crew-p-r-a", time.Minute)

	if len(m.sent) != 1 {
		t.Fatalf("expected exactly one successful nudge, got %d: %v", len(m.sent), m.sent)
	}
	if m.sent[0] != nodeDrainNudgeMsg {
		t.Errorf("retry sent %q, want the node-drain message %q", m.sent[0], nodeDrainNudgeMsg)
	}
}

func TestNodeIsDoomed(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{name: "nil node", node: nil, want: false},
		{
			name: "healthy node, no taints",
			node: &corev1.Node{},
			want: false,
		},
		{
			name: "node with DeletionTimestamp",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}},
			want: true,
		},
		{
			name: "karpenter disrupted NoSchedule taint",
			node: &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
			}}},
			want: true,
		},
		{
			name: "karpenter disruption NoExecute taint",
			node: &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "karpenter.sh/disruption", Effect: corev1.TaintEffectNoExecute},
			}}},
			want: true,
		},
		{
			name: "unrelated taint is not doom",
			node: &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "gardenio.agent", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			}}},
			want: false,
		},
		{
			name: "disrupted taint with PreferNoSchedule effect does not count",
			node: &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectPreferNoSchedule},
			}}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeIsDoomed(tt.node); got != tt.want {
				t.Errorf("nodeIsDoomed = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUpgradeTracker_DrainEndClosesMail: however the drain ends, its mail is
// closed, so the session of a pod recreated for the same agent cannot drain
// it (mail stays open until closed; the subscriber drains all open mail).
func TestUpgradeTracker_DrainEndClosesMail(t *testing.T) {
	cases := []struct {
		name string
		end  func(tr *UpgradeTracker, m *fakeMessenger) bool // true when the drain should be over
	}{
		{"agent rests", func(tr *UpgradeTracker, m *fakeMessenger) bool {
			m.setState("idle")
			return tr.DrainReady(context.Background(), "crew-p-r-a", 5*time.Minute)
		}},
		{"agent stops", func(tr *UpgradeTracker, m *fakeMessenger) bool {
			m.setState("done")
			return tr.DrainReady(context.Background(), "crew-p-r-a", 5*time.Minute)
		}},
		{"agent bead gone", func(tr *UpgradeTracker, m *fakeMessenger) bool {
			m.mu.Lock()
			m.beadErr = beadsapi.ErrAgentNotFound
			m.mu.Unlock()
			return tr.DrainReady(context.Background(), "crew-p-r-a", 5*time.Minute)
		}},
		{"drain timeout", func(tr *UpgradeTracker, _ *fakeMessenger) bool {
			return tr.DrainReady(context.Background(), "crew-p-r-a", 0)
		}},
		{"pod gone", func(tr *UpgradeTracker, _ *fakeMessenger) bool {
			tr.CleanStaleDrains(map[string]struct{}{})
			return true
		}},
		{"pod recreated", func(tr *UpgradeTracker, _ *fakeMessenger) bool {
			tr.EndDrain(context.Background(), "crew-p-r-a")
			return true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, m := drainTracker("working")
			tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
			if len(m.openMailFor("a")) != 1 {
				t.Fatalf("expected one open drain mail, got %v", m.open)
			}
			if !tc.end(tr, m) {
				t.Fatal("drain should be over")
			}
			if tr.IsDraining("crew-p-r-a") {
				t.Error("drain entry must be gone")
			}
			if open := m.openMailFor("a"); len(open) != 0 {
				t.Errorf("drain mail left open: %v", open)
			}
		})
	}
}

// TestUpgradeTracker_DrainBusyKeepsMailOpen: while the agent is still working
// the mail must stay open; it is the agent's instruction.
func TestUpgradeTracker_DrainBusyKeepsMailOpen(t *testing.T) {
	tr, m := drainTracker("working")
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	if tr.DrainReady(context.Background(), "crew-p-r-a", 5*time.Minute) {
		t.Fatal("working agent must not be drainable")
	}
	tr.CleanStaleDrains(map[string]struct{}{"crew-p-r-a": {}})
	if len(m.openMailFor("a")) != 1 || len(m.closed) != 0 {
		t.Errorf("drain mail must stay open mid-drain: open=%v closed=%v", m.open, m.closed)
	}
}

// TestUpgradeTracker_FailedMailCloseIsRetried: a close that fails is retried
// on the next CleanStaleDrains pass, which the reconciler runs before it
// creates any pod.
func TestUpgradeTracker_FailedMailCloseIsRetried(t *testing.T) {
	tr, m := drainTracker("working")
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	m.closeFails = 1
	if !tr.DrainReady(context.Background(), "crew-p-r-a", 0) {
		t.Fatal("timed-out drain should be ready")
	}
	if len(m.openMailFor("a")) != 1 {
		t.Fatalf("first close fails, mail should still be open: %v", m.open)
	}
	tr.CleanStaleDrains(map[string]struct{}{})
	if open := m.openMailFor("a"); len(open) != 0 {
		t.Errorf("failed close must be retried, still open: %v", open)
	}
}

// blockingMessenger holds Nudge until release is closed.
type blockingMessenger struct {
	*fakeMessenger
	entered chan struct{}
	release chan struct{}
}

func (b *blockingMessenger) Nudge(ctx context.Context, agentName, message string) (string, error) {
	close(b.entered)
	<-b.release
	return b.fakeMessenger.Nudge(ctx, agentName, message)
}

// TestUpgradeTracker_StartDrainDoesNotHoldLockAcrossNudge: account rotation
// shares the tracker, so a slow daemon on one drain mail must not block other
// tracker calls.
func TestUpgradeTracker_StartDrainDoesNotHoldLockAcrossNudge(t *testing.T) {
	b := &blockingMessenger{
		fakeMessenger: &fakeMessenger{bead: agentBeadInState("working")},
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	tr := NewUpgradeTracker(testLogger())
	tr.SetMessenger(b)

	done := make(chan bool)
	go func() { done <- tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg) }()
	<-b.entered

	other := make(chan bool)
	go func() { other <- tr.IsDraining("crew-p-r-b") }()
	select {
	case <-other:
	case <-time.After(2 * time.Second):
		t.Fatal("IsDraining blocked behind an in-flight drain mail")
	}
	if !tr.IsDraining("crew-p-r-a") {
		t.Error("the drain entry must be open while its mail is in flight")
	}
	// A concurrent DrainReady must not send a second mail while the first is
	// in flight.
	tr.DrainReady(context.Background(), "crew-p-r-a", 5*time.Minute)

	close(b.release)
	if !<-done {
		t.Fatal("StartDrainWithMsg should report true")
	}
	if len(b.sent) != 1 {
		t.Errorf("expected exactly one drain mail, got %v", b.sent)
	}
}

// TestUpgradeTracker_DrainEndedDuringNudgeClosesMail: if the drain ends while
// its mail is being sent, the mail is closed as soon as its id is known.
func TestUpgradeTracker_DrainEndedDuringNudgeClosesMail(t *testing.T) {
	b := &blockingMessenger{
		fakeMessenger: &fakeMessenger{bead: agentBeadInState("working")},
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	tr := NewUpgradeTracker(testLogger())
	tr.SetMessenger(b)

	done := make(chan bool)
	go func() { done <- tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg) }()
	<-b.entered
	tr.CleanStaleDrains(map[string]struct{}{}) // pod went away mid-send
	close(b.release)
	<-done

	if open := b.openMailFor("a"); len(open) != 0 {
		t.Errorf("mail for an ended drain left open: %v", open)
	}
}

// TestUpgradeTracker_MailCloseGivesUp: a close that never succeeds is not
// retried forever.
func TestUpgradeTracker_MailCloseGivesUp(t *testing.T) {
	tr, m := drainTracker("working")
	tr.StartDrainWithMsg(context.Background(), "crew-p-r-a", "a", nodeDrainNudgeMsg)
	m.closeFails = 1000
	tr.DrainReady(context.Background(), "crew-p-r-a", 0)
	for range drainMailCloseAttempts + 2 {
		tr.CleanStaleDrains(map[string]struct{}{})
	}
	if left := 1000 - m.closeFails; left != drainMailCloseAttempts {
		t.Errorf("expected %d close attempts, got %d", drainMailCloseAttempts, left)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.unclosed) != 0 {
		t.Errorf("retry queue must be empty after giving up: %v", tr.unclosed)
	}
}
