// Package agentwake brings a stopped agent back when an external event it
// parked on lands: a decision it filed is resolved, an escalation it filed
// resolves, or the CI run it waits on concludes (plan
// gardenio/plans/wake-on-ci-completion.md §2.4, task gd-T7aFA3tFoW).
//
// Wake tries three things in order, so every caller revives an agent the same way:
//
//  1. Event-wake a DORMANT identity in place (EventWakeDormant): a closed agent
//     bead with successor=same is reopened with wake_reason stamped, and the
//     reconciler recreates the same-named pod, which `claude --resume`s.
//  2. Thaw a FROZEN one: an agent bead still OPEN at agent_state=stopping has a
//     checkpoint. It is closed so SpawnAgent's name-dedup misses it, and the
//     same-name spawn is pinned to runsc so its "created" event restores the
//     snapshot.
//  3. Otherwise SpawnAgent a fresh agent with the caller's prompt, carrying
//     plan_id, operated_by, spawned_by and (optionally) the Slack routing.
//
// The package does only bead writes, with no Slack or K8s deps, so both the
// slack-bridge (internal/bridge) and cmd/github-bridge can import it.
package agentwake

import (
	"context"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// RuntimeClassRunsc mirrors podmanager.RuntimeClassRunsc, which this package
// does not import (it would pull the K8s client into github-bridge). A bridge
// test keeps the two equal.
const RuntimeClassRunsc = "runsc"

// Client is the daemon subset Wake uses. *beadsapi.Client satisfies it.
type Client interface {
	EventWakeClient
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
}

var _ Client = (*beadsapi.Client)(nil)

// Request describes one wake.
type Request struct {
	// AgentName is the agent's name (no "project/" prefix). It names the fresh
	// spawn, which for a frozen agent must be the same name to thaw.
	AgentName string
	// AgentBead is the agent's own bead, in any status. Nil skips the in-place
	// wake and the thaw, and spawns without the bead's carried fields.
	AgentBead *beadsapi.BeadDetail
	// Reason is the wake_reason stamped on the woken or spawned bead
	// (beadsapi.WakeReason*), surfaced to the pod as BOAT_WAKE_REASON.
	Reason string
	// Operator is the human the agent works for ("slack:U…" or
	// "oidc:<email>"), already checked by the caller. It authors the
	// event-wake audit comment and is stamped as operated_by on a spawn (and
	// onto ctx), so the spawn is not attributed to the fleet sentinel.
	Operator string
	// Project, TaskID and Role are what a fresh spawn is minted with.
	Project, TaskID, Role string
	// ExtraFields are added to a fresh spawn's fields (spawn_source and the
	// bead that triggered the wake, at least).
	ExtraFields map[string]string
	// CarrySlackRouting copies the agent bead's Slack thread binding, spawn
	// channel, slack_user_id and wish pointer onto a fresh spawn, so it posts
	// where the old agent did (gd-jOHtHdkdIt, gd-AflFRvDw7Z).
	CarrySlackRouting bool
	// Prepare runs only when the agent was not woken in place, before anything
	// is closed or spawned. It returns the spawn prompt, or ok=false to spawn
	// nothing (e.g. the resolved project is a test fixture's).
	Prepare func(ctx context.Context) (prompt string, ok bool)
	// LogAttrs are added to every log line, e.g. "decision", id.
	LogAttrs []any
}

// Outcome is what Wake did.
type Outcome int

const (
	// Declined: the agent was not dormant and Prepare refused to spawn.
	Declined Outcome = iota
	// Woken: the dormant identity was reopened in place.
	Woken
	// Spawned: a fresh agent was spawned (thawing a frozen one if it was
	// frozen). SpawnAgent's dedup may have returned a live agent instead.
	Spawned
	// Failed: the spawn failed. A frozen bead closed for it was reopened.
	Failed
)

// Result reports a Wake.
type Result struct {
	Outcome Outcome
	// AgentBeadID is the woken bead or the spawned one.
	AgentBeadID string
	// Thawed is true when the spawn was a frozen agent's thaw.
	Thawed bool
	// Err is the spawn error when Outcome is Failed.
	Err error
}

// Wake revives the agent in req: in place if it is dormant, by thaw if it is
// frozen, else by a fresh spawn. Best-effort: it logs and reports rather than
// retrying, since its callers (SSE handlers, the CI-wait sweep) own any retry.
func Wake(ctx context.Context, c Client, logger *slog.Logger, req Request) Result {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := func(kv ...any) []any {
		out := append([]any{"agent", req.AgentName, "wake_reason", req.Reason}, req.LogAttrs...)
		return append(out, kv...)
	}
	b := req.AgentBead

	if woke, err := EventWakeDormant(ctx, c, logger, b, req.Reason, req.Operator); err != nil {
		logger.Warn("event-wake failed; falling back to spawn", attrs("error", err)...)
	} else if woke {
		logger.Info("event-woke dormant persistent identity", attrs("agent_bead", b.ID)...)
		return Result{Outcome: Woken, AgentBeadID: b.ID}
	}

	// Freeze/thaw (Δ4/A2, ka-0000000241): a FROZEN agent (gb yield or gb stop
	// checkpointed the pod) keeps an OPEN bead at agent_state=stopping with a
	// persisted snapshot. SpawnAgent's findActiveAgentByName dedup counts
	// agent_state=stopping as ACTIVE and returns SpawnExisting: no new bead, no
	// "created" AgentSpawn event, so the controller never runs shouldThaw.
	// Mirror respawnThreadAgent (bot_agent_kill.go): CLOSE the frozen bead
	// (just before SpawnAgent, not here) and pin runtime_class so the fresh
	// same-name runsc bead's "created" event thaws the snapshot (thaw is keyed
	// on agent NAME, not bead id). agent_state=stopping is written only by
	// suspendAgent, so it never matches a runc done/fail.
	frozen := IsFrozenAwaitingThaw(b)

	prompt := ""
	if req.Prepare != nil {
		p, ok := req.Prepare(ctx)
		if !ok {
			return Result{Outcome: Declined}
		}
		prompt = p
	}

	extra := map[string]string{
		// Stamp the wake reason on the fresh spawn too, so the controller
		// surfaces BOAT_WAKE_REASON whether the identity was woken in place
		// (above) or minted fresh here: the agent always learns why it runs.
		beadsapi.FieldWakeReason: req.Reason,
	}
	if b != nil && req.CarrySlackRouting {
		beadsapi.CopySlackRoutingFields(extra, b.Fields)
		if uid := b.Fields["slack_user_id"]; uid != "" {
			extra["slack_user_id"] = uid
		}
		// Without the wish pointer a respawned thread agent's card edit finds
		// no thread head and upserts a second, top-level card (gd-AflFRvDw7Z).
		beadsapi.SetWishPointer(extra, firstNonEmptyField(b.Fields,
			beadsapi.FieldThreadID, beadsapi.FieldSlackThreadBeadID))
	}
	for k, v := range req.ExtraFields {
		extra[k] = v
	}
	// A frozen bead is runsc by definition; without the pin shouldThaw rejects
	// the fresh bead (RuntimeClassName != runsc) and it cold-starts on a fresh
	// sandbox, throwing the checkpoint away. Only a frozen agent is pinned, so
	// a normal spawn keeps the role/project/cluster default.
	if frozen {
		rc := strings.TrimSpace(b.Fields["runtime_class"])
		if rc == "" {
			rc = RuntimeClassRunsc
		}
		extra["runtime_class"] = rc
	}
	if b != nil {
		// Keep a plan gardener bound to its plan (ka-0000000352): without
		// plan_id the respawn mints a second same-named gardener bound only by
		// task_id.
		if planID := b.Fields["plan_id"]; planID != "" {
			extra["plan_id"] = planID
		}
		// Keep the single-claim opt-in for a respawn onto the same task, so the
		// controller claims it for the fresh crew (gd-Yo28zKgkGx).
		if req.TaskID != "" && b.Fields["task_id"] == req.TaskID && b.Fields[beadsapi.FieldClaimTaskAtSpawn] != "" {
			extra[beadsapi.FieldClaimTaskAtSpawn] = b.Fields[beadsapi.FieldClaimTaskAtSpawn]
		}
		// Keep the human operator (ka-0000007919): a header-less fleet-token
		// create falls through to the fleet:slack-bridge sentinel.
		if req.Operator != "" {
			extra["operated_by"] = req.Operator
			ctx = beadsapi.WithOperator(ctx, req.Operator)
		}
		// spawned_by gives the daemon's operatedByFromAgentBead chain-walk
		// (ka-0000001404) a lineage edge back to the human.
		if b.ID != "" {
			extra["spawned_by"] = b.ID
		}
	}

	// Close the frozen bead only now that nothing before the create can bail
	// out, and put it back if the create fails. SpawnAgent must see it closed
	// or its name dedup returns it and the thaw never fires; but a failed
	// create must not strand the snapshot behind a closed bead nothing will
	// respawn (gd-VuYwGGYLLT).
	frozenClosed := false
	if frozen {
		logger.Info("closing frozen-awaiting-thaw agent bead so the respawn thaws instead of dedup-cold-starting",
			attrs("agent_bead", b.ID, "runtime_class", extra["runtime_class"])...)
		if err := c.CloseBead(ctx, b.ID, nil); err != nil {
			logger.Warn("failed to close frozen agent bead; proceeding anyway (spawn may dedup and skip thaw)",
				attrs("agent_bead", b.ID, "error", err)...)
		} else {
			frozenClosed = true
		}
	}

	id, err := c.SpawnAgent(ctx, req.AgentName, req.Project, req.TaskID, req.Role, prompt, extra)
	if err != nil {
		logger.Error("failed to spawn agent for wake", attrs("task", req.TaskID, "error", err)...)
		if frozenClosed {
			reopenFrozen(ctx, c, logger, b.ID, attrs)
		}
		return Result{Outcome: Failed, Err: err}
	}
	return Result{Outcome: Spawned, AgentBeadID: id, Thawed: frozen}
}

// IsFrozenAwaitingThaw reports whether b is a frozen agent waiting to be
// thawed: agent_state=stopping, which only suspendAgent writes.
func IsFrozenAwaitingThaw(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Fields[beadsapi.FieldAgentState] == "stopping"
}

// reopenFrozen puts back a frozen agent bead Wake closed before a create that
// then failed, so the agent is neither lost nor left with a snapshot no bead
// points at. The bead keeps agent_state=stopping, so a later wake takes the
// same thaw path. Best-effort: a failed reopen is logged for the operator.
func reopenFrozen(ctx context.Context, c Client, logger *slog.Logger, beadID string, attrs func(...any) []any) {
	open := "open"
	if err := c.UpdateBead(ctx, beadID, beadsapi.UpdateBeadRequest{Status: &open}); err != nil {
		logger.Error("spawn failed AND reopening the frozen agent bead failed; reopen it by hand to keep the snapshot reachable",
			attrs("agent_bead", beadID, "error", err)...)
		return
	}
	logger.Info("spawn failed; reopened the frozen agent bead so the snapshot stays reachable",
		attrs("agent_bead", beadID)...)
}

func firstNonEmptyField(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}
