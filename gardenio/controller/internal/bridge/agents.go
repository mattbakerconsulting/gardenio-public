// Package bridge provides the agent lifecycle watcher.
//
// Agents subscribes to kbeads SSE event stream for agent bead lifecycle events
// and posts Slack crash notifications when an agent fails. It deduplicates
// notifications per agent bead ID to avoid spam on SSE reconnect or repeated
// update events.
package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	gardeniometrics "gardenio/controller/internal/metrics"
)

// isReaperCleanup reports whether a close_reason marks an expected reaper
// cleanup rather than a real crash. The stale reaper closes orphaned agents
// (source task already done before the agent finished) with
// agent_state=failed, but these are not crashes — they are expected cleanup
// of agents that lost their reason to exist (e.g. decision-resume agents
// respawned after their parent task closed, scheduled-gardener agents whose
// schedule was disabled mid-flight). Posting "Agent crashed" to Slack for
// these spams operators with non-incidents.
//
// Genuine liveness failures from the reaper (timeouts, stuck spawning,
// unschedulable) use the "stale:" prefix and remain crash-worthy.
func isReaperCleanup(closeReason string) bool {
	return strings.HasPrefix(closeReason, "orphan:")
}

// AgentNotifier posts agent lifecycle notifications to Slack.
type AgentNotifier interface {
	NotifyAgentCrash(ctx context.Context, bead BeadEvent) error
	// NotifyAgentDeath is called when an agent bead closes with an INVOLUNTARY
	// mid-work death (close_reason carries the `_involuntary` marker or the
	// context_cancelled_unclaimed / context_cancelled_claim_lookup_failed
	// prefix). Unlike a crash, an involuntary death
	// closes with agent_state=done, so it is invisible to NotifyAgentCrash —
	// this is the auto-forensics signal (epic ka-0000005447). Implementations
	// post a warning naming the agent + a one-line cause and attach a forensic
	// breadcrumb to the dead bead, gating volume against death storms.
	NotifyAgentDeath(ctx context.Context, bead BeadEvent) error
	// NotifyAgentSpawn is called when an agent bead is first created.
	// Implementations should post an initial status card / message.
	NotifyAgentSpawn(ctx context.Context, bead BeadEvent)
	// NotifyAgentState is called whenever an agent bead's agent_state changes.
	// Implementations should update any live status display (e.g. agent card).
	NotifyAgentState(ctx context.Context, bead BeadEvent)
	// NotifyAgentTaskUpdate is called when a non-agent bead assigned to an agent
	// changes to in_progress (i.e., the agent claimed a task). Implementations
	// should refresh the agent's live status display to show the new task.
	NotifyAgentTaskUpdate(ctx context.Context, agentName string)
	// NotifyAgentPaused is called once when the stale reaper pauses a laptop
	// agent because its runner went away (local-agents D5). Implementations
	// post the paused message in the agent's thread.
	NotifyAgentPaused(ctx context.Context, bead BeadEvent)
}

// AgentsConfig holds configuration for the Agents watcher.
type AgentsConfig struct {
	Notifier AgentNotifier // nil = no notifications
	Logger   *slog.Logger
	// ThreadTaskAutoCloser, when set, propagates a successful agent close to
	// the parent slack-thread task after a grace window. See
	// ThreadTaskAutoCloser for details. Nil disables propagation.
	ThreadTaskAutoCloser *ThreadTaskAutoCloser
}

// Agents watches the kbeads SSE event stream for agent bead lifecycle events.
type Agents struct {
	notifier         AgentNotifier
	logger           *slog.Logger
	threadAutoCloser *ThreadTaskAutoCloser

	// seen dedups crash/death/rate-limit notifications per agent bead across
	// SSE reconnects. TTL/size-bounded so it cannot leak for the process
	// lifetime (ka-0000008059); carries its own lock.
	seen *seenSet

	mu            sync.Mutex
	taskAssignees map[string]string // task bead ID → last known assignee
}

// NewAgents creates a new agent lifecycle watcher.
func NewAgents(cfg AgentsConfig) *Agents {
	return &Agents{
		notifier:         cfg.Notifier,
		logger:           cfg.Logger,
		threadAutoCloser: cfg.ThreadTaskAutoCloser,
		seen:             newSeenSet(seenTTL, seenMaxEntries),
		taskAssignees:    make(map[string]string),
	}
}

// Run starts the background dedup-map GC loop. It blocks until ctx is
// cancelled; callers run it in a goroutine. Without it the seen set would only
// self-evict lazily on re-observation, so idle keys would linger. Mirrors
// AgentStreamManager.Run's gc ticker.
func (a *Agents) Run(ctx context.Context) {
	ticker := time.NewTicker(seenGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.seen.gc()
		}
	}
}

// RegisterHandlers registers SSE event handlers on the given stream for
// agent bead created, closed, and updated events.
func (a *Agents) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", a.handleCreated)
	stream.On("beads.bead.closed", a.handleClosed)
	stream.On("beads.bead.updated", a.handleUpdated)
	a.logger.Info("agents watcher registered SSE handlers",
		"topics", []string{"beads.bead.created", "beads.bead.closed", "beads.bead.updated"})
}

func (a *Agents) handleCreated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}
	if bead.Type != "agent" {
		return
	}

	a.logger.Info("agent bead created",
		"id", bead.ID, "assignee", bead.Assignee, "title", bead.Title)

	if a.notifier != nil {
		a.notifier.NotifyAgentSpawn(ctx, *bead)
	}
}

func (a *Agents) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}
	if bead.Type != "agent" {
		// For task beads, refresh the agent's card so the completed task is cleared.
		// When the assignee is empty (e.g. unassigned before close), use the
		// last-known assignee so their card still gets refreshed.
		assignee := bead.Assignee
		if assignee == "" {
			a.mu.Lock()
			assignee = a.taskAssignees[bead.ID]
			a.mu.Unlock()
		}
		// Clean up tracking entry — the bead is closed.
		a.mu.Lock()
		delete(a.taskAssignees, bead.ID)
		a.mu.Unlock()

		if assignee != "" && a.notifier != nil {
			a.notifier.NotifyAgentTaskUpdate(ctx, assignee)
		}
		return
	}

	// An agent bead closing with agent_state=failed or pod_phase=failed is a crash.
	// Guard: if agent_state=done, trust it as authoritative and suppress the
	// crash. Pod teardown can race — a pod SIGKILLed after its graceful
	// period syncs pod_phase=failed after the agent already closed cleanly,
	// and notifying a crash for that race posts spam to Slack.
	// Also suppress the crash for orphan reaper cleanups (close_reason has
	// the "orphan:" prefix) — these are expected cleanups, not crashes.
	agentState := bead.Fields["agent_state"]
	podPhase := bead.Fields["pod_phase"]
	closeReason := bead.Fields["close_reason"]
	if agentState != "done" && (agentState == "failed" || podPhase == "failed") && !isReaperCleanup(closeReason) {
		a.notifyCrash(ctx, *bead)
		// Ensure agent_state is set so the card update below shows "failed".
		if agentState == "" {
			bead.Fields["agent_state"] = "failed"
			agentState = "failed"
		}
	}

	// An INVOLUNTARY mid-work death (close_reason carries the `_involuntary`
	// marker or the context_cancelled_unclaimed /
	// context_cancelled_claim_lookup_failed prefix) closes with
	// agent_state=done, so it is INVISIBLE to notifyCrash above. It is the
	// auto-forensics signal (epic ka-0000005447) — post a "why did this agent
	// die" report and attach a forensic breadcrumb to the bead.
	a.notifyDeath(ctx, *bead)

	// Update the card so it shows current state (done/failed) with the Clear button.
	if a.notifier != nil {
		if agentState != "done" && agentState != "failed" {
			bead.Fields["agent_state"] = "done"
		}
		a.notifier.NotifyAgentState(ctx, *bead)
	}

	// Propagate a clean close to the parent slack-thread task (ka-0000003222).
	// MaybeSchedule no-ops for non-thread agents and for crashes.
	if a.threadAutoCloser != nil {
		a.threadAutoCloser.MaybeSchedule(*bead)
	}
}

func (a *Agents) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	if bead.Type != "agent" {
		// For task beads claimed by an agent, refresh the agent's card so it
		// reflects the newly claimed task without waiting for a pod phase change.
		// Also refresh on close in case kbeads emits an update before the closed event.
		if bead.Assignee != "" && a.notifier != nil &&
			(bead.Status == "in_progress" || bead.Status == "closed") {
			a.notifier.NotifyAgentTaskUpdate(ctx, bead.Assignee)
		}

		// When assignee is cleared (unassigned), refresh the previous assignee's
		// card so it no longer shows the stale task title.
		if bead.Assignee == "" && a.notifier != nil {
			a.mu.Lock()
			prev := a.taskAssignees[bead.ID]
			delete(a.taskAssignees, bead.ID)
			a.mu.Unlock()
			if prev != "" {
				a.notifier.NotifyAgentTaskUpdate(ctx, prev)
			}
		}

		// Track the current assignee for future unassignment detection.
		// When reassigned directly (A→B without clearing), also notify the
		// previous assignee so their card drops the stale task title.
		if bead.Assignee != "" {
			a.mu.Lock()
			prev := a.taskAssignees[bead.ID]
			a.taskAssignees[bead.ID] = bead.Assignee
			a.mu.Unlock()
			if prev != "" && prev != bead.Assignee && a.notifier != nil {
				a.notifier.NotifyAgentTaskUpdate(ctx, prev)
			}
		}
		return
	}

	agentState := bead.Fields["agent_state"]
	podPhase := bead.Fields["pod_phase"]

	// Notify crash on agent_state=failed or pod_phase=failed.
	// Guard: if agent_state=done, trust it and suppress the crash — pod
	// teardown can emit pod_phase=failed after a clean close.
	// Also suppress for orphan reaper cleanups (see handleClosed).
	closeReason := bead.Fields["close_reason"]
	if agentState != "done" && (agentState == "failed" || podPhase == "failed") && !isReaperCleanup(closeReason) {
		a.notifyCrash(ctx, *bead)
		// Ensure agent_state is set so the card update below shows "failed".
		if agentState == "" {
			bead.Fields["agent_state"] = "failed"
			agentState = "failed"
		}
	}

	// Notify rate limit so operators know.
	if agentState == "rate_limited" {
		a.notifyRateLimited(ctx, *bead)
	}

	// Tell the thread once when a laptop agent is paused. Changes carries the
	// values this update wrote, so a paused agent_state in it is the
	// transition (the reaper writes it only to an agent not yet paused); any
	// other update of a paused agent re-renders the card without a repost.
	if agentState == "paused" && a.notifier != nil {
		if v, _ := bead.Changes["agent_state"].(string); v == "paused" {
			a.notifier.NotifyAgentPaused(ctx, *bead)
		}
	}

	// For any state change, notify so the agent card can be refreshed.
	if agentState != "" && a.notifier != nil {
		a.notifier.NotifyAgentState(ctx, *bead)
	}
}

func (a *Agents) notifyRateLimited(ctx context.Context, bead BeadEvent) {
	// Deduplicate: only notify once per agent bead.
	if a.seen.markSeen(bead.ID + ":rate_limited") {
		return
	}

	agent := bead.Assignee
	if agent == "" {
		if n := bead.Fields["agent"]; n != "" {
			agent = n
		}
	}

	a.logger.Warn("agent rate-limited",
		"id", bead.ID,
		"agent", agent,
		"agent_state", bead.Fields["agent_state"])

	if a.notifier != nil {
		a.notifier.NotifyAgentState(ctx, bead)
	}
}

// notifyDeath forwards an involuntary mid-work death to the notifier, once per
// agent bead. The death signal is the close_reason carrying the `_involuntary`
// marker (or the context_cancelled_unclaimed / context_cancelled_claim_lookup_failed
// prefix) — the SHARED predicate the
// metric emitter uses (gardeniometrics.InvoluntaryDeathFamily), so the forensic
// report and gardenio_agent_involuntary_deaths_total can never disagree about
// what counts as a death. A close that is not a classified involuntary death
// (clean idle-out, graceful stop, plain crash) is a no-op here.
func (a *Agents) notifyDeath(ctx context.Context, bead BeadEvent) {
	if a.notifier == nil {
		return
	}
	if _, ok := gardeniometrics.InvoluntaryDeathFamily(bead.Fields["close_reason"]); !ok {
		return
	}

	// Deduplicate: only report a given agent bead's death once, even if the
	// closed event and a trailing updated event both carry the marker (or SSE
	// redelivers). Keyed distinctly from the crash dedup so a bead that both
	// crashed and (later) carries a death marker is not cross-suppressed.
	if a.seen.markSeen(bead.ID + ":death") {
		return
	}

	a.logger.Info("involuntary agent death detected",
		"id", bead.ID,
		"assignee", bead.Assignee,
		"close_reason", bead.Fields["close_reason"])

	if err := a.notifier.NotifyAgentDeath(ctx, bead); err != nil {
		a.logger.Error("failed to notify agent death",
			"id", bead.ID, "error", err)
	}
}

func (a *Agents) notifyCrash(ctx context.Context, bead BeadEvent) {
	// Deduplicate: only notify once per agent bead.
	if a.seen.markSeen(bead.ID) {
		return
	}

	a.logger.Info("agent crash detected",
		"id", bead.ID,
		"title", bead.Title,
		"assignee", bead.Assignee,
		"agent_state", bead.Fields["agent_state"],
		"pod_phase", bead.Fields["pod_phase"])

	if a.notifier != nil {
		if err := a.notifier.NotifyAgentCrash(ctx, bead); err != nil {
			a.logger.Error("failed to notify agent crash",
				"id", bead.ID, "error", err)
		}
	}
}
