package planagent

import (
	"context"
	"errors"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
)

// PromptPath enumerates which delivery path Prompt took. The caller uses it to
// pick the right HTTP translation (a nudge error is an upstream-write error; a
// spawn error is a bad-gateway) and the right lane chip state (live vs waking).
type PromptPath string

const (
	// PromptPathNudge — the message was (or was attempted to be) delivered to a
	// live agent via coopmux Nudge.
	PromptPathNudge PromptPath = "nudge"
	// PromptPathSpawn — the message went through the wake/spawn path.
	PromptPathSpawn PromptPath = "spawn"
)

// DeliveryOutcome is the HONEST fate of a just-submitted message, deliberately
// distinct from the agent-liveness chip. Callers render it as a transient
// acknowledgment so the operator learns what actually happened to their words —
// mitigating R1 ("message into the void"): a bare chip flip to "waking" used to
// hide whether the message was *delivered* to a live pod, merely *queued* on a
// parked one (delivered later on wake), or kicking off a cold *spawn*.
type DeliveryOutcome string

const (
	// DeliveryNone — not a just-sent render, or a graceful no-op (closed plan).
	DeliveryNone DeliveryOutcome = ""
	// DeliverySent — nudged a LIVE session; delivered now.
	DeliverySent DeliveryOutcome = "sent"
	// DeliveryQueuedWaking — queued on a parked/unreachable agent; delivered on wake.
	DeliveryQueuedWaking DeliveryOutcome = "queued-waking"
	// DeliverySpawning — first message; cold-starting a fresh agent.
	DeliverySpawning DeliveryOutcome = "spawning"
)

// PromptResult is what Prompt reports back: which path it took, the honest
// delivery fate for an ack line, and the agent name (the nudged agent's name on
// the nudge path, the (re)spawned name on the spawn path; "" only when the spawn
// itself failed).
type PromptResult struct {
	Path      PromptPath
	Outcome   DeliveryOutcome
	AgentName string
}

// Prompt is the single source of truth for the live/parked/none decision that
// delivers a message to a plan's agent. It is shared by every caller that
// steers a plan (the viewer's explicit prompt verb + reactive add-step path, and
// the controller reactor):
//
//   - live agent + a usable coopmux session → Nudge it (sub-second). Returns
//     path=nudge, outcome=sent.
//   - live but coopmux is nil/Disabled, or the agent has no coopmux session
//     id yet (pod-up race), or the session is gone upstream → degrade to the spawn path
//     so the operator's message is never lost. Returns path=spawn.
//   - a hard (non-session-gone) nudge error → returns path=nudge + err so the
//     caller can surface it; NO spawn (mirrors the prior handler's behavior).
//   - parked or no agent → Spawn, which reuses the prior name + flags
//     session_resume for a parked agent (wake continuity) or mints a fresh name
//     on first contact. Returns path=spawn.
//
// A CLOSED plan short-circuits inside Spawn (ErrPlanClosedNoWake): Prompt maps
// it to a graceful no-op — path=spawn, outcome=none, no error — so callers
// render "no gardener kicked" rather than a 5xx or a false "spawning" chip.
func (e *Engine) Prompt(ctx context.Context, epic *beadsapi.BeadDetail, actor, seed string) (PromptResult, error) {
	epicID := epic.ID
	agent := e.Resolve(ctx, epicID)

	if agent != nil && e.IsLive(agent) {
		switch {
		case e.nudge == nil || e.nudge.Disabled():
			// No coopmux in this deployment — a spawn (thaw/respawn) is the only
			// way to deliver the message. Degrade rather than error.
			e.logger.Info("plan_agent_prompt", "epic", epicID, "agent", agent.ID, "path", "wake_fallback_no_coopmux")
		default:
			// coopmux knows a pod-run agent by its pod name, not by
			// last_session_id; nudging the latter always 404s, and the
			// fall-through Wake then re-drives a pod that is still running
			// (gd-XCwA8MXX8m, same cause as gd-oJOJvsOdw7).
			sessionID := agent.MuxSessionID()
			if sessionID == "" {
				// Live-but-sessionless is a transient race between the pod coming
				// up and its pod name / last_session_id being stamped. Fall through
				// to the wake path so the message isn't lost.
				e.logger.Info("plan_agent_prompt", "epic", epicID, "agent", agent.ID, "path", "wake_fallback_no_session")
			} else if err := e.nudge.Nudge(ctx, sessionID, seed); err != nil {
				if errors.Is(err, coopmuxapi.ErrSessionNotFound) {
					// Session gone upstream — spawn/thaw as usual. Fall through to
					// the wake path below.
					e.logger.Info("plan_agent_prompt", "epic", epicID, "agent", agent.ID, "path", "wake_fallback_session_gone", "session", sessionID)
				} else {
					e.logger.Error("plan_agent_prompt: nudge",
						"epic", epicID, "agent", agent.ID, "session", sessionID, "operator", actor, "error", err)
					return PromptResult{Path: PromptPathNudge, Outcome: DeliveryNone, AgentName: agent.Title}, err
				}
			} else {
				e.logger.Info("plan_agent_prompt",
					"event", "plan_agent_prompt",
					"path", "nudge",
					"epic", epicID,
					"agent", agent.ID,
					"session", sessionID,
					"operator", actor,
					"len", len(seed),
					"snippet", snippet(seed, 80),
					"result", "ok")
				return PromptResult{Path: PromptPathNudge, Outcome: DeliverySent, AgentName: agent.Title}, nil
			}
		}
	}

	// Wake / spawn path — Spawn handles the "reuse prior name + flag
	// session_resume" continuity contract when a parked agent exists; a
	// first-time spawn mints a fresh name. An agent that already exists here
	// (parked, or live-but-unreachable and fallen through) means the message is
	// QUEUED and delivered on wake; only a first-contact spawn is a cold spawn.
	outcome := DeliverySpawning
	if agent != nil {
		outcome = DeliveryQueuedWaking
	}
	_, agentName, err := e.Spawn(ctx, actor, epic, seed)
	if errors.Is(err, ErrPlanClosedNoWake) {
		// The plan is CLOSED — Spawn refused to spawn/wake its gardener
		// (ka-0000000753 backstop). This is a graceful no-op, not a failure: a
		// closed plan is done, so there is nothing to deliver the message to and
		// no spawn to report. Return no error and no delivery outcome so callers
		// render "no gardener kicked" rather than a 5xx or a false "spawning" chip.
		e.logger.Info("plan_agent_prompt", "event", "plan_agent_prompt",
			"path", "spawn", "outcome", "skipped_plan_closed", "epic", epicID, "operator", actor)
		return PromptResult{Path: PromptPathSpawn, Outcome: DeliveryNone, AgentName: ""}, nil
	}
	if err != nil {
		e.logger.Error("plan_agent_prompt: spawn", "epic", epicID, "operator", actor, "error", err)
		return PromptResult{Path: PromptPathSpawn, Outcome: DeliveryNone, AgentName: ""}, err
	}
	e.logger.Info("plan_agent_prompt",
		"event", "plan_agent_prompt",
		"path", "spawn",
		"outcome", string(outcome),
		"epic", epicID,
		"agent_name", agentName,
		"operator", actor,
		"len", len(seed),
		"snippet", snippet(seed, 80),
		"result", "ok")
	return PromptResult{Path: PromptPathSpawn, Outcome: outcome, AgentName: agentName}, nil
}

// snippet returns a single-line, length-capped preview of text for structured
// logging (mirrors cmd/beads-viewer's agentInputSnippet). Kept local so the
// engine has no dependency on package main.
func snippet(text string, n int) string {
	t := strings.ReplaceAll(text, "\n", " ")
	t = strings.ReplaceAll(t, "\r", " ")
	if len(t) <= n {
		return t
	}
	return t[:n] + "…"
}
