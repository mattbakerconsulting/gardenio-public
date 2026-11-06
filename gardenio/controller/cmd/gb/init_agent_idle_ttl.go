package main

// init_agent_idle_ttl.go — warm-keepalive idle-TTL watcher for thread/DM agents.
//
// Epic ka-0000006987, task ka-0000005793. The spike (ka-0000001429,
// gardenio/docs/research/warm-idle-baseline-measurements.md) established that a
// thread follow-up today *always* cold-respawns (~126s median) because thread
// agents are told to `gb done` after one turn — the warm-nudge path
// (~10ms) that already exists in the bridge is never taken.
//
// This watcher keeps a thread/DM pod alive while it is parked-idle, so a
// follow-up message lands on the live coop session via nudgeAgentForMention
// instead of a cold respawn. After idle_ttl of *continuous* idle with no new
// turn, it despawns the pod cleanly — the deliberate-despawn the fleet lacked.
//
// Placement: this is an in-pod gb watcher rather than a controller-reconciler
// loop. The idle/working signal (coop.GetAgentState), the never-despawn-mid-turn
// guarantee, and the clean-despawn primitives (syncAgentSessions, coop.Shutdown,
// CloseBead with agent_state=done) all already live in-pod beside monitorIdle /
// monitorExit. A reconciler would have to poll every pod's coop API across
// namespaces and re-implement that machinery. See the MR description and
// gardenio/docs/research/warm-idle-thread-agents.md for the full trade-off.

import (
	"context"
	"os"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
)

// idleTTLParkRespawnDefault is the wake delay stamped as next_respawn_at when a
// PERSISTENT agent parks on idle-TTL expiry. It is the "wake metadata" the
// future controller wake-scanner (ka-0000004902 — scan CLOSED agent beads with
// next_respawn_at ≤ now, recreate the pod by name) keys off. A modest default
// keeps a parked identity revivable without hammering: overridable per scope via
// the idle-ttl config bead's "respawn_after_minutes". Inert until the wake
// scanner lands — this task only ARMS the intent (design memo §2.5, the
// successor=same "records intent only today" contract on WrapUp).
const idleTTLParkRespawnDefault = 30 * time.Minute

// idleTTLDefault is the warm-keepalive window applied when no idle-ttl config
// bead overrides it. ~90m balances follow-up latency (warm nudge) against the
// cost of an idle pod; the 2h stale reaper + upgrade drain remain a backstop.
const idleTTLDefault = 90 * time.Minute

// idleTTLPollDefault is how often the watcher samples coop state. Coarse on
// purpose — the TTL is measured in tens of minutes, so a 30s poll is plenty
// and keeps the extra coop load negligible.
const idleTTLPollDefault = 30 * time.Second

// Test hooks (mirrors the monitorSleepOverride / zombieWatchdogTimeout pattern
// in init_agent.go). idleTTLPollOverride replaces the poll interval; when set,
// monitorIdleTTL also skips its initial settle sleep.
var idleTTLPollOverride time.Duration

// idleTTLConfig is the resolved per-agent warm-keepalive policy.
type idleTTLConfig struct {
	enabled bool
	ttl     time.Duration

	// persistent marks this agent as one whose idle-TTL expiry should PARK the
	// identity (successor=same + wake metadata) rather than terminally close it.
	// A parked agent's bead is still closed (so the reconciler stops tracking the
	// pod), but it carries successor=same + next_respawn_at so the future wake
	// scanner (ka-0000004902) can recreate the pod under the same name and
	// `claude --resume` the conversation. Default-off: only opts in via an
	// idle-ttl config bead with "persistent": true, so every existing thread/DM
	// agent keeps today's terminal idle despawn until an operator opts a
	// project/role in.
	persistent bool

	// respawnAfter is the wake delay stamped as next_respawn_at when a persistent
	// agent parks. Zero (the default when not persistent, or when config omits
	// respawn_after_minutes) falls back to idleTTLParkRespawnDefault at park time.
	respawnAfter time.Duration
}

// resolveIdleTTL determines whether this agent participates in warm-keepalive
// and, if so, with what TTL. Resolution order:
//
//   - default: enabled for thread/DM-spawned agents (SLACK_THREAD_* set),
//     disabled for everyone else, TTL = idleTTLDefault.
//   - an "idle-ttl" config bead (category resolved global < project < role <
//     agent) may override "enabled" and/or "ttl_minutes" for any scope.
//
// Keeping the feature default-off for non-thread agents means crew/gardener
// lifecycle is unchanged unless an operator opts a project/role in via config.
func resolveIdleTTL(ctx context.Context) idleTTLConfig {
	cfg := idleTTLConfig{
		enabled: isThreadSpawnedAgent(),
		ttl:     idleTTLDefault,
	}

	if daemon == nil {
		return cfg
	}

	subs := buildSubscriptions(os.Getenv("BOAT_ROLE"))
	merged, count, err := ResolveConfigBeads(ctx, daemon, "idle-ttl", subs)
	if err != nil || count == 0 {
		return cfg
	}

	if v, ok := merged["enabled"].(bool); ok {
		cfg.enabled = v
	}
	if m, ok := idleTTLMinutes(merged["ttl_minutes"]); ok && m > 0 {
		cfg.ttl = time.Duration(m * float64(time.Minute))
	}
	// Persistence opt-in: a "persistent": true idle-ttl config bead makes idle-TTL
	// expiry PARK the identity (successor=same + wake metadata) instead of a plain
	// terminal close. "respawn_after_minutes" tunes the stamped wake delay.
	if v, ok := merged["persistent"].(bool); ok {
		cfg.persistent = v
	}
	if m, ok := idleTTLMinutes(merged["respawn_after_minutes"]); ok && m > 0 {
		cfg.respawnAfter = time.Duration(m * float64(time.Minute))
	}
	return cfg
}

// idleTTLNextRespawnAt computes the RFC3339 next_respawn_at wake timestamp for a
// parked persistent agent: now + the configured respawn delay, falling back to
// idleTTLParkRespawnDefault when the config omits respawn_after_minutes. This is
// the wake metadata the future controller wake-scanner (ka-0000004902) keys off.
func idleTTLNextRespawnAt(cfg idleTTLConfig) string {
	delay := cfg.respawnAfter
	if delay <= 0 {
		delay = idleTTLParkRespawnDefault
	}
	return time.Now().Add(delay).UTC().Format(time.RFC3339)
}

// idleTTLMinutes coerces a config "ttl_minutes" value (a JSON number decodes
// to float64) into minutes.
func idleTTLMinutes(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// monitorIdleTTL keeps a warm thread/DM pod alive while it is parked-idle and
// despawns it cleanly once it has been *continuously* idle for ttl.
//
// Requirements (ka-0000005793):
//
//	(a) never despawn mid-turn — only ever acts on state == "idle"; any
//	    working/starting/rate_limited state resets the clock.
//	(b) reset the timer on every inbound nudge — a follow-up message drives a
//	    "working" turn, so observing "working" resets lastActive. This is the
//	    in-pod proxy for "now - last_inbound_ts".
//	(c) leave session JSONL intact for post-TTL cold resume — despawn fsyncs
//	    transcripts (syncAgentSessions) and never retires sessions (no --clean).
//	(d) coexist with upgrade drain + reaper — despawn is idempotent: it skips
//	    if the bead is already closed or stop_requested, and closes with
//	    agent_state=done so the reconciler sees a clean terminal bead (the same
//	    shape gb stop produces), avoiding a double-despawn.
//	(e) park-instead-of-close for persistent agents — when cfg.persistent, the
//	    despawn stamps successor=same + next_respawn_at wake metadata on the bead
//	    before closing it, so the future wake scanner (ka-0000004902) can recreate
//	    the pod under the same name and `claude --resume` the conversation. The
//	    bead still closes (so the reconciler stops tracking the pod), matching
//	    the design-memo model where a parked identity is a CLOSED bead carrying
//	    wake metadata rather than a still-open one.
func monitorIdleTTL(ctx context.Context, coop *coopapi.Client, cfg idleTTLConfig) {
	ttl := cfg.ttl
	if ttl <= 0 {
		return
	}

	poll := idleTTLPollDefault
	if idleTTLPollOverride != 0 {
		poll = idleTTLPollOverride
		// In tests the settle sleep would dwarf the TTL; skip it.
	} else {
		// Let the session reach a steady state before arming the clock.
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}

	logInit("idle-TTL: warm-keepalive armed (window=%s, poll=%s)", ttl, poll)

	// Accumulate continuous-idle time in units of the poll interval rather than
	// wall-clock. Each idle observation adds one poll's worth of idle; any
	// working/starting state zeroes it. This decouples the despawn decision from
	// scheduler jitter: under CI load a poll tick may fire late, but the count of
	// consecutive idle polls is unaffected, so the TTL trips after exactly
	// ⌈ttl/poll⌉ idle observations regardless of how the runtime schedules us. A
	// delayed poll only makes the real despawn marginally later than ttl, which
	// is harmless for a tens-of-minutes warm window (the 2h reaper backstops it).
	var idleFor time.Duration

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}

		state, err := coop.GetAgentState(ctx)
		if err != nil {
			return // coop gone — session already ending
		}

		switch state.State {
		case "exited":
			return
		case "idle":
			idleFor += poll
			if idleFor >= ttl {
				idleTTLDespawn(coop, cfg)
				return
			}
		default:
			// "working", "starting", "rate_limited", or transient "" — the pod
			// is doing (or about to do) work, so it is not warm-idle. Reset.
			idleFor = 0
		}
	}
}

// idleTTLDespawn performs the deliberate clean despawn after the warm window
// expires. It mirrors the bead lifecycle of `gb stop` so the reconciler treats
// it identically to an explicit stop and never respawns the pod.
//
// For a PERSISTENT agent (cfg.persistent) it instead PARKS the identity: the
// bead is still closed (the reconciler stops tracking the pod), but it carries
// successor=same + a next_respawn_at wake timestamp so the future controller
// wake-scanner (ka-0000004902) recreates the pod under the same name and
// `claude --resume`s the conversation. Arming intent only — inert until that
// scanner ships (design memo: successor=same "records intent only today").
func idleTTLDespawn(coop *coopapi.Client, cfg idleTTLConfig) {
	agentID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if cfg.persistent {
		logInit("idle-TTL: parked-idle ≥ %s with no new turn — parking persistent agent (successor=same, warm window expired)", cfg.ttl)
	} else {
		logInit("idle-TTL: parked-idle ≥ %s with no new turn — despawning cleanly (warm window expired)", cfg.ttl)
	}

	// (c) Flush transcripts to the PVC/S3 before teardown so a later cold
	// respawn can `claude --resume` the conversation. Never retire sessions.
	syncAgentSessions()

	// (f) SUSPEND-vs-PARK on idle-TTL (Δ4/A1, ka-0000000860). A freeze-eligible
	// agent (runsc + freeze/thaw enabled; BOAT_FREEZE_ELIGIBLE=true, the same
	// controller-injected gate `gb stop`'s shouldSuspend reads) should FREEZE on
	// idle-TTL expiry instead of cold-parking — the idle Claude process is
	// checkpointed and a later same-name spawn THAWS it hot, rather than paying a
	// cold `claude --resume`. This mirrors gb stop's suspend path exactly:
	// transition agent_state=stopping (the sole AgentStop freeze trigger) WITHOUT
	// closing the bead, WITHOUT stop_requested, and WITHOUT shutting coop down —
	// the controller's shouldFreeze gate then runs `runsc checkpoint
	// --leave-running` on the LIVE sandbox and owns Pod A teardown. Shutting coop
	// down first (as the park path below does) would checkpoint a dead session, so
	// the freeze branch returns before the coop.Shutdown at the end of this func.
	// Non-eligible agents fall through to the unchanged done/park path.
	if agentID != "" && daemon != nil && freezeEligible() {
		if suspendIdleAgent(agentID, cfg) {
			return
		}
		// A suspend attempt that could not transition (already closing, or the
		// bead write failed) falls through to the normal park/despawn below so the
		// pod still winds down cleanly rather than lingering idle.
	}

	if agentID != "" && daemon != nil {
		// Use a fresh timeout context for the bead writes: the session ctx may
		// already be cancelling as the pod winds down (and syncAgentSessions
		// just slept), which would cancel the close mid-flight. This mirrors
		// closeAgentBeadBestEffort / gb stop, which both detach from the
		// session context for teardown writes.
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// (d) Idempotency: if gb stop / the reaper / an upgrade drain already
		// closed or flagged the bead, do not race them. agent_state=stopping is
		// the frozen-awaiting-thaw signal (suspendAgent, ka-0000002104) — the
		// controller owns this pod's teardown and the bead must stay open as the
		// thaw target, so closing it to done here would defeat the freeze.
		if b, err := daemon.GetBead(closeCtx, agentID); err == nil && b != nil {
			if b.Status == "closed" || b.Fields["stop_requested"] == "true" ||
				b.Fields["agent_state"] == "stopping" {
				logInit("idle-TTL: agent bead %s already closing (status=%s, agent_state=%s) — skipping despawn",
					agentID, b.Status, b.Fields["agent_state"])
				return
			}
		}

		// stop_requested=true makes the restart loop's isStopRequested check
		// exit without a respawn after coop shuts down.
		if err := daemon.UpdateBeadFields(closeCtx, agentID, map[string]string{"stop_requested": "true"}); err != nil {
			logInit("idle-TTL: warning: set stop_requested: %v", err)
		}

		closeFields := map[string]string{"agent_state": "done", "close_reason": "idle_ttl_expired"}
		if cfg.persistent {
			// (e) Park: stamp successor=same intent + wake metadata so the wake
			// scanner can recreate this identity. next_respawn_at is the same
			// controller-authoritative field the goal-respawn / restart-condition
			// line already reads (AgentBeadFieldNextRespawnAt), so the Slack card
			// renders a "Resumes ~<when>" progressing line for free once the
			// scanner is live. close_reason distinguishes a park from a terminal
			// idle despawn in the audit trail.
			//
			// agent_state=dormant (not "done") is the ENGINE-SET signal the viewer
			// buckets a parked identity on (task ka-0000004675). Stamping it here,
			// in the same close write that sets successor=same + next_respawn_at,
			// means the viewer never has to DERIVE dormancy from that field combo —
			// and because both the wake-scanner and `gb wake` CLEAR agent_state in
			// the atomic reopen, the row moves dormant → (cleared) → working
			// without flapping across the respawn window.
			closeFields["agent_state"] = beadsapi.AgentStateDormant
			closeFields["successor"] = SuccessorSame
			closeFields["close_reason"] = "idle_ttl_parked"
			closeFields[AgentBeadFieldNextRespawnAt] = idleTTLNextRespawnAt(cfg)
		}

		// Route through the shared fallback path so an idle despawn without a
		// wrap-up still gets a synthesized tl;dr headline (Fix A,
		// ka-0000007639). A real wrap-up already on the bead always wins.
		if err := closeAgentBeadWithFallback(closeCtx, agentID, closeFields["close_reason"], closeFields["close_reason"],
			closeFields); err != nil {
			logInit("idle-TTL: warning: close agent bead %s: %v", agentID, err)
		} else if cfg.persistent {
			logInit("idle-TTL: parked agent bead %s (idle_ttl_parked, successor=same, next_respawn_at=%s)",
				agentID, closeFields[AgentBeadFieldNextRespawnAt])
		} else {
			logInit("idle-TTL: closed agent bead %s (idle_ttl_expired)", agentID)
		}
	}

	// Terminate the coop session so the pod winds down. The restart loop then
	// sees stop_requested and exits cleanly (no respawn). Use a fresh context
	// for the same reason as the bead writes above.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = coop.Shutdown(shutdownCtx)
}

// suspendIdleAgent freezes a freeze-eligible agent on idle-TTL expiry instead of
// cold-parking it (Δ4/A1, ka-0000000860). It is the idle-path analogue of
// suspendAgent (cmd/gb/stop.go): it transitions agent_state=stopping — the sole
// AgentStop freeze trigger the controller observes — WITHOUT closing the bead,
// WITHOUT stop_requested, and WITHOUT shutting coop down, so the controller's
// shouldFreeze gate can `runsc checkpoint --leave-running` the LIVE sandbox and
// own Pod A teardown. The bead is left OPEN at agent_state=stopping: that is the
// name-keyed frozen-awaiting-thaw signal every wake path (respawnThreadAgent, the
// reconciler thaw exemptions) keys on, so the next same-name spawn THAWS rather
// than cold-starts.
//
// Returns true when the agent has been handed to the freeze path (the caller
// must NOT also park/close the bead or shut coop down). Returns false when no
// suspend was performed and the caller should fall through to the normal
// park/despawn — either because the bead is already closing (someone else owns
// teardown) is handled as true, or because the stopping transition failed.
func suspendIdleAgent(agentID string, cfg idleTTLConfig) bool {
	// Fresh timeout context: the session ctx may already be cancelling as the pod
	// winds down (and syncAgentSessions just slept), which would cancel the write
	// mid-flight. Mirrors idleTTLDespawn's teardown writes.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Idempotency: if gb stop / the reaper / an upgrade drain already closed or
	// flagged the bead, do not race them. agent_state=stopping means a freeze is
	// already in flight (this pod, or a duplicate idle fire) — the controller owns
	// teardown, so report handled (true) and do NOT fall through to park, which
	// would overwrite the stopping state and defeat the freeze.
	if b, err := daemon.GetBead(ctx, agentID); err == nil && b != nil {
		if b.Status == "closed" || b.Fields["stop_requested"] == "true" ||
			b.Fields["agent_state"] == "stopping" {
			logInit("idle-TTL: agent bead %s already closing (status=%s, agent_state=%s) — skipping idle-freeze",
				agentID, b.Status, b.Fields["agent_state"])
			return true
		}
	}

	// Transition to stopping — the sole AgentStop trigger. NOT a CloseBead and NOT
	// stop_requested: the bead must stay OPEN in a thaw-able state for a later
	// AgentSpawn, and stop_requested would drive the entrypoint restart loop to
	// CloseBead(agent_state=done), overwriting the stopping state and defeating the
	// freeze (see suspendAgent's doc in stop.go).
	if err := daemon.UpdateAgentState(ctx, agentID, "stopping"); err != nil {
		logInit("idle-TTL: warning: transition agent %s to stopping (idle-freeze) failed: %v — falling back to normal park/despawn",
			agentID, err)
		return false
	}

	logInit("idle-TTL: idle ≥ %s — freezing freeze-eligible agent %s (agent_state=stopping); controller will checkpoint the live sandbox and a later spawn thaws it",
		cfg.ttl, agentID)

	// Deliberately do NOT shut coop down: the checkpoint must capture the LIVE
	// Claude process for a hot thaw. coop is terminated by the controller's Pod A
	// teardown only after `runsc checkpoint --leave-running` has persisted the
	// snapshot (mirrors suspendAgent, which also skips requestCoopShutdown).
	return true
}
