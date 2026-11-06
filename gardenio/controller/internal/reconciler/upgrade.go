package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
)

// nodeDrainNudgeMsg is the graceful-drain nudge sent when an agent's NODE is
// being force-reclaimed (WS1, ka-0000007969). The window here is not fixed —
// a forced node drain (spot reclaim, AWS health/retirement, interruption-queue)
// can SIGKILL the pod on its own schedule — so the message stresses "as soon
// as possible". Uncommitted work is still auto-salvaged to S3 by the preStop
// `gb salvage` hook (ka-0000004335), but a pushed branch is cleaner.
const nodeDrainNudgeMsg = "Your node is being reclaimed — this pod will be " +
	"terminated shortly. Commit and push your WIP now " +
	"(git add -A && git commit && git push), then gb stop. Uncommitted work is " +
	"auto-salvaged to S3 but pushing is cleaner."

// karpenterDisruptedTaintPrefix is the taint key prefix Karpenter applies to a
// node it is tearing down (e.g. karpenter.sh/disrupted, karpenter.sh/disruption).
// Every agent pod carries karpenter.sh/do-not-disrupt and no NodePool sets a
// terminationGracePeriod, so Karpenter never *voluntarily* drains a node hosting
// a running agent. A node that nonetheless carries this taint — or a
// DeletionTimestamp — is being force-reclaimed by AWS in a way do-not-disrupt
// does not stop, which is exactly the mid-turn kill WS1 drains ahead of.
const karpenterDisruptedTaintPrefix = "karpenter.sh/disrupt"

// nodeIsDoomed reports whether a node is being torn down and any agent pod on
// it is about to receive SIGTERM. It is deliberately conservative: it fires only
// on unambiguous going-away signals (a Karpenter disruption taint or a set
// DeletionTimestamp), NOT on a bare cordon (spec.unschedulable), because a node
// can be cordoned transiently (e.g. during an EKS upgrade hop) without being
// deleted, and draining agents off a node that survives is pure churn.
func nodeIsDoomed(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	if node.DeletionTimestamp != nil {
		return true
	}
	for _, t := range node.Spec.Taints {
		if strings.HasPrefix(t.Key, karpenterDisruptedTaintPrefix) &&
			(t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute) {
			return true
		}
	}
	return false
}

// drainMailGrace is how long a resting agent (idle, yielding, ...) is given to
// pick up the drain mail before DrainReady counts it drainable. The nudge used
// to be a synchronous POST into coop; as mail it reaches the session through
// the agent's own subscriber a few seconds later, and the bead's agent_state
// lags behind that. Without the grace an agent that was idle when the drain
// opened would be deleted on the same pass, before it ever read "push your WIP".
const drainMailGrace = 30 * time.Second

// DrainMessenger reaches a draining agent through the beads daemon, never by
// dialing its pod: Nudge sends the commit+push message as interrupt mail (the
// controller wires bridge.NudgeAgentMail, from:controller) and returns the
// mail's id, and FindAgentBead returns the agent bead whose agent_state decides
// whether the drain is done.
//
// CloseMail closes the drain mail when its drain ends. Mail stays open until
// the agent runs `gb mail read`, and every new subscriber drains the open mail
// for its agent name, so a drain mail left open would reach the session of the
// pod recreated for the same agent, which would commit, push and gb stop at
// once. The mail subscriber closes kill mail for the same reason.
type DrainMessenger interface {
	Nudge(ctx context.Context, agentName, message string) (mailID string, err error)
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	CloseMail(ctx context.Context, mailID string) error
}

// drainMailCloseTimeout bounds one CloseMail call made outside a caller's
// context (CleanStaleDrains has none).
const drainMailCloseTimeout = 10 * time.Second

// drainMailCloseAttempts is how many times a drain mail's close is tried
// before the tracker gives up on it and logs an error.
const drainMailCloseAttempts = 5

// drainEntry tracks the state of a pod being gracefully drained before deletion.
type drainEntry struct {
	started  time.Time
	agent    string    // agent name the drain mail is addressed to
	nudged   bool      // whether the commit+push nudge has been sent
	sending  bool      // a Nudge is in flight; DrainReady must not send another
	nudgedAt time.Time // when it was sent, for drainMailGrace
	mailID   string    // the drain mail, closed when the drain ends
	msg      string    // the nudge message to send (node-drain vs rotation wording)
}

// UpgradeTracker tracks graceful drains: an agent is mailed to commit+push
// and its pod is deleted once its turn is over or the drain times out. One
// tracker is shared by every kill trigger (node drain, account rotation) so a
// pod is never nudged twice.
type UpgradeTracker struct {
	mu     sync.Mutex
	logger *slog.Logger

	// draining tracks pods that have been nudged to checkpoint before deletion.
	// Key: pod name, Value: drain state.
	draining map[string]*drainEntry

	// messenger delivers the drain mail and reads the agent bead. nil means
	// the tracker cannot drain: StartDrainWithMsg reports false and callers delete
	// without waiting, as they did for a pod with no IP before.
	messenger DrainMessenger
	mailGrace time.Duration

	// unclosed holds drain mail whose CloseMail failed, with the attempts
	// made so far; CleanStaleDrains retries it every pass, up to
	// drainMailCloseAttempts. The daemon's close is idempotent.
	unclosed map[string]int
}

// NewUpgradeTracker creates a new upgrade tracker.
func NewUpgradeTracker(logger *slog.Logger) *UpgradeTracker {
	return &UpgradeTracker{
		logger:    logger,
		draining:  make(map[string]*drainEntry),
		mailGrace: drainMailGrace,
	}
}

// SetMessenger wires how drains reach agents (see DrainMessenger).
func (t *UpgradeTracker) SetMessenger(m DrainMessenger) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messenger = m
}

// CleanStaleDrains removes draining entries whose pod no longer exists. The
// node-drain branch (reconciler.go) opens a draining entry then continue's
// without ever calling DrainReady — its comment states "the tracker keeps the
// entry until the pod goes away", but nothing implemented that cleanup, so the
// only remover (clearDrain, reached only from DrainReady) was never invoked on
// that path. A leaked entry then poisons a LATER drain of a same-named
// recreated pod: StartDrainWithMsg early-returns on the stale entry (no refresh
// of started) and DrainReady's timeout branch trusts the stale started time, so
// the pod is force-deleted with NO graceful commit+push drain. Prune any draining entry whose
// pod name is absent from the live set each pass, so a recreated pod always
// opens a fresh drain (ka-0000000604).
//
// A pruned entry's drain mail is closed here too: the node-drain branch never
// reaches clearDrain, and the pod recreated for the same agent must not read
// the old pod's "commit, push, gb stop". Mail an earlier close failed on is
// retried on the same pass.
func (t *UpgradeTracker) CleanStaleDrains(livePodNames map[string]struct{}) {
	t.mu.Lock()
	var mails []string
	for id := range t.unclosed {
		mails = append(mails, id)
	}
	for name, entry := range t.draining {
		if _, alive := livePodNames[name]; !alive {
			t.logger.Info("pod gone, clearing stale drain entry",
				"pod", name)
			delete(t.draining, name)
			if entry.mailID != "" {
				mails = append(mails, entry.mailID)
			}
		}
	}
	t.mu.Unlock()

	if len(mails) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainMailCloseTimeout)
	defer cancel()
	for _, id := range mails {
		t.closeDrainMail(ctx, id)
	}
}

// StartDrainWithMsg begins the graceful drain of a pod: it mails msg, the
// caller's commit+push nudge (node drain WS1 ka-0000007969, account rotation),
// to agentName and tracks the drain state (nudge → watch agent bead → delete
// on idle/done or timeout). It returns true if the drain was started (or is
// already in progress), and false when there is no way to reach the agent (no
// agent name, or no messenger wired), in which case the caller should not
// wait on DrainReady. The message is stored on the drain entry so the
// DrainReady retry path re-sends the same wording.
//
// The entry is opened before the mail is sent and t.mu is not held across
// Nudge (two daemon round-trips), so account rotation, which shares the
// tracker, never waits behind a slow daemon.
func (t *UpgradeTracker) StartDrainWithMsg(ctx context.Context, podName, agentName, msg string) bool {
	t.mu.Lock()
	if _, ok := t.draining[podName]; ok {
		t.mu.Unlock()
		return true // already draining
	}
	messenger := t.messenger
	if messenger == nil || agentName == "" {
		t.mu.Unlock()
		return false
	}
	entry := &drainEntry{
		started: time.Now(),
		agent:   agentName,
		msg:     msg,
		sending: true,
	}
	t.draining[podName] = entry
	t.mu.Unlock()

	// Mail the commit+push nudge.
	mailID, err := messenger.Nudge(ctx, agentName, msg)
	if err != nil {
		t.mu.Lock()
		entry.sending = false
		t.mu.Unlock()
		t.logger.Warn("failed to mail drain nudge to agent, will retry next pass",
			"pod", podName, "agent", agentName, "error", err)
		return true
	}
	t.logger.Info("mailed agent to commit+push for drain",
		"pod", podName, "agent", agentName, "mail", mailID)
	t.recordNudge(ctx, podName, entry, mailID)
	return true
}

// recordNudge marks entry nudged with mailID. If the drain ended while the
// mail was being sent, the mail is closed at once instead.
func (t *UpgradeTracker) recordNudge(ctx context.Context, podName string, entry *drainEntry, mailID string) {
	t.mu.Lock()
	if t.draining[podName] != entry {
		t.mu.Unlock()
		t.closeDrainMail(ctx, mailID)
		return
	}
	entry.nudged, entry.nudgedAt, entry.mailID = true, time.Now(), mailID
	entry.sending = false
	t.mu.Unlock()
}

// IsDraining returns true if the pod is currently in the drain phase.
func (t *UpgradeTracker) IsDraining(podName string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.draining[podName]
	return ok
}

// DrainReady checks if a draining pod is ready to be deleted. It returns true
// if the agent bead says the turn is over (see drainVerdict) or if the drain
// timeout has expired.
func (t *UpgradeTracker) DrainReady(ctx context.Context, podName string, timeout time.Duration) bool {
	t.mu.Lock()
	entry, ok := t.draining[podName]
	if !ok {
		t.mu.Unlock()
		return true // not draining, ready to delete
	}
	agent := entry.agent
	started := entry.started
	nudged := entry.nudged
	nudgedAt := entry.nudgedAt
	msg := entry.msg
	messenger := t.messenger
	retry := !nudged && !entry.sending
	if retry {
		entry.sending = true
	}
	t.mu.Unlock()

	// Check timeout. A retry claimed above is given up with the entry.
	if time.Since(started) > timeout {
		t.logger.Warn("drain timeout expired, proceeding with forced delete",
			"pod", podName, "elapsed", time.Since(started).Round(time.Second))
		t.clearDrain(ctx, podName)
		return true
	}
	if messenger == nil {
		return false
	}

	// Retry nudge if it failed on the first attempt.
	if retry {
		if mailID, err := messenger.Nudge(ctx, agent, msg); err == nil {
			t.recordNudge(ctx, podName, entry, mailID)
			nudged, nudgedAt = true, time.Now()
			t.logger.Info("retry drain mail succeeded", "pod", podName, "agent", agent, "mail", mailID)
		} else {
			t.mu.Lock()
			entry.sending = false
			t.mu.Unlock()
		}
	}

	bead, err := messenger.FindAgentBead(ctx, agent)
	if errors.Is(err, beadsapi.ErrAgentNotFound) {
		// No active agent bead: the agent closed out, nothing left to land.
		t.logger.Info("agent bead closed, proceeding with upgrade",
			"pod", podName, "agent", agent,
			"elapsed", time.Since(started).Round(time.Second))
		t.clearDrain(ctx, podName)
		return true
	}
	if err != nil {
		t.logger.Debug("could not read agent bead during drain",
			"pod", podName, "agent", agent, "error", err)
		return false
	}

	state := bead.Fields["agent_state"]
	switch drainVerdict(state, bead.Fields["stop_requested"] == "true") {
	case drainDone:
	case drainResting:
		// Resting before the mail landed proves nothing; give it drainMailGrace.
		if !nudged || time.Since(nudgedAt) < t.mailGrace {
			t.logger.Debug("agent resting, waiting for it to read the drain mail",
				"pod", podName, "agent", agent, "state", state)
			return false
		}
	default:
		t.logger.Debug("agent still working during drain, waiting",
			"pod", podName, "agent", agent, "state", state,
			"elapsed", time.Since(started).Round(time.Second))
		return false
	}

	t.logger.Info("agent reached drainable state, proceeding with upgrade",
		"pod", podName, "agent", agent, "state", state,
		"elapsed", time.Since(started).Round(time.Second))
	t.clearDrain(ctx, podName)
	return true
}

// EndDrain ends podName's drain, if any, and closes its drain mail. The
// reconciler calls it before deleting a pod it recreates in the same pass
// (a terminal pod): CleanStaleDrains saw that pod alive at
// the top of the pass, so without this the recreated pod would inherit the
// entry and its session would drain the open mail.
func (t *UpgradeTracker) EndDrain(ctx context.Context, podName string) {
	t.clearDrain(ctx, podName)
}

// clearDrain ends podName's drain and closes its drain mail. Every caller is
// about to let the pod be deleted, so the mail no longer applies.
func (t *UpgradeTracker) clearDrain(ctx context.Context, podName string) {
	t.mu.Lock()
	entry, ok := t.draining[podName]
	delete(t.draining, podName)
	t.mu.Unlock()
	if ok && entry.mailID != "" {
		t.closeDrainMail(ctx, entry.mailID)
	}
}

// closeDrainMail closes one drain mail, queueing it for CleanStaleDrains to
// retry when the close fails.
func (t *UpgradeTracker) closeDrainMail(ctx context.Context, mailID string) {
	t.mu.Lock()
	messenger := t.messenger
	t.mu.Unlock()
	if messenger == nil {
		return
	}
	err := messenger.CloseMail(ctx, mailID)

	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		delete(t.unclosed, mailID)
		t.logger.Debug("closed drain mail", "mail", mailID)
		return
	}
	attempts := t.unclosed[mailID] + 1
	if attempts >= drainMailCloseAttempts {
		delete(t.unclosed, mailID)
		t.logger.Error("giving up closing drain mail; the agent's next session will read it",
			"mail", mailID, "attempts", attempts, "error", err)
		return
	}
	if t.unclosed == nil {
		t.unclosed = make(map[string]int)
	}
	t.unclosed[mailID] = attempts
	t.logger.Warn("closing drain mail failed, will retry next pass",
		"mail", mailID, "attempts", attempts, "error", err)
}

type drainState int

const (
	drainBusy    drainState = iota // mid-turn or state unknown: keep waiting
	drainResting                   // between turns: drainable once the mail had time to land
	drainDone                      // stopping or finished: drainable now
)

// drainVerdict classifies an agent bead for the drain. The terminal set and
// the stop_requested lame-duck rule are the bridge's nudge liveness rule
// (deadAgentStates / stop_requested in internal/bridge/nudge.go), copied
// rather than imported: an agent that ran `gb stop` is exactly the outcome the
// drain mail asks for. Busy is classifyActivity's active set, plus an empty
// state, which says nothing about the session.
//
// Behaviour change from the coop read (gd-az3o7XZf5M): the old drain deleted
// the pod only when coop said "idle" or "exited". Every other non-busy bead
// state now counts as resting, so idle, yielding, rate_limited,
// model_rate_limited, queued, prewarmed and any state added later are deleted
// once drainMailGrace has passed since the mail went out, instead of waiting
// out the full drain timeout. None of them is mid-turn, so there is no
// response to cut off, and a rate-limited session has nothing to land that
// the grace does not already give it a chance to push.
func drainVerdict(state string, stopRequested bool) drainState {
	if stopRequested {
		return drainDone
	}
	switch state {
	case "done", "failed", "stopping", "stop_requested", "budget_exceeded", "killed":
		return drainDone
	case "":
		return drainBusy
	}
	if classifyActivity(state) == "active" {
		return drainBusy
	}
	return drainResting
}

// IsPodReady returns true if a pod is in Running phase and all containers
// have passing readiness probes.
func IsPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
