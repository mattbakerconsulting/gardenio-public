// Engine-side "credential-wedged agent" detector (bug ka-0000000592).
//
// This is the fourth sibling of the silent-failure sweeps — SweepGardenerLiveness
// (gardener_liveness.go), SweepPouringButNeverWorked (gardener_pouring_stall.go),
// and SweepDarkLaneByConcurrency (trigger_dark_lane.go) — for a NEW dimension of
// the same "invisible to every updated_at-based check" gap ka-0000002023 named:
// CREDENTIAL EXHAUSTION rather than churn.
//
// The failure it catches (gardener-delivery lane dark ~6h, ka-0000000592): an
// agent whose account exhausted its monthly-spend limit AFTER it spawned, with no
// LiteLLM fallback model group for its pinned model, so every injected turn dies
// on the same 429 rate_limit_error and `gb init` just re-injects the initial work
// prompt on a 1–4m backoff FOREVER. The agent completes ZERO turns and produces
// ZERO output tokens / ZERO cost, yet:
//
//   - The pod is Running (restartCount=0), so the reconciler reads it as alive.
//   - SyncAll keeps propagating agent_state=rate_limited, and each propagation
//     bumps the bead's updated_at, so every updated_at-based staleness check reads
//     the bead as FRESH — the same treadmill ka-0000002023 called out for churn.
//   - The lease-side age ceiling (leaseAgeCeilingFactor, triggers.go) only
//     releases the max_concurrent slot after ~6h, and a REPLACEMENT inherits the
//     same wedged config, so ceiling release converts a stuck lane into a respawn
//     loop rather than restoring progress.
//
// So the wedge is invisible on every dashboard for hours. This sweep makes it
// LOUD: it flags an agent bead parked in a credential-exhaustion error state
// (rate_limited / budget_exceeded) for longer than a grace window while having
// produced NO output tokens and NO cost — the signature of "never completed a
// turn". It WARN-logs and writes a one-shot comment on the agent bead (deduped per
// episode via warnedGate), and optionally files a bead, mirroring the dark-lane
// sweep's alert discipline.
//
// NEVER spawns and never kills — the reap-side runtime watchdog (ka-0000000716 /
// StaleReaper.runtimeCeilings) owns pod teardown. This sweep is the always-on VISIBILITY
// floor so a credential wedge is discoverable in one alert instead of by an
// operator eventually noticing hours-and-N-stranded-MRs later.

package bridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Credential-wedge detector tunables.
const (
	// credWedgeMinAge is how long an agent must have sat in a credential-error
	// state with zero progress before it is flagged. The wedge re-injects on a
	// 1–4m backoff, so a genuinely-transient rate limit (a single window bump
	// that clears on its own) resolves well inside this window; only a wedge
	// that has burned this long with zero output is real. 30m gives generous
	// slack over the longest observed backoff while still surfacing the wedge
	// hours before the ~6h lease age ceiling would even begin to churn it.
	credWedgeMinAge = 30 * time.Minute

	// credWedgeRosterLimit bounds the agent roster pulled per sweep. Mirrors
	// darkLaneRosterLimit — the live agent fleet is tens-to-low-hundreds of
	// beads; this cap only guards a runaway query.
	credWedgeRosterLimit = 1000
)

// credWedgeErrorStates are the agent_state values that mean "the current
// account's credentials cannot serve this agent's traffic" — the exact states
// SyncAll propagates from coop's error category (reporter.go queryCoopAgentState)
// and the SSE watcher routes to auth failover (sse.go). An agent parked here with
// zero progress is credential-wedged.
var credWedgeErrorStates = map[string]bool{
	"rate_limited":    true,
	"budget_exceeded": true,
}

// SweepCredentialWedge performs the credential-wedged-agent pass (bug
// ka-0000000592). For every active agent bead parked in a credential-error state
// (rate_limited / budget_exceeded) for longer than credWedgeMinAge while having
// produced ZERO output tokens AND ZERO cost — i.e. it has never completed a turn
// — it WARN-logs and writes a one-shot comment on the agent bead, deduped per
// wedge episode. Optionally files a bead (credWedgeCreateBead).
//
// No-op unless credWedgeEnabled. Safe to call every Evaluate cycle: it is a single
// ListBeadsFiltered over active agent beads plus, for the few that clear the cheap
// state+age+zero-progress gate, one AddComment each (deduped so a persistently
// wedged agent does not re-comment every cycle). Errors are logged and downgraded
// so a daemon hiccup cannot wedge the sweep.
func (e *TriggerEngine) SweepCredentialWedge(ctx context.Context) {
	if !e.credWedgeEnabled {
		return
	}

	// Attribute the notice-bead / bug-bead writes this sweep publishes to the
	// machine operator, or the daemon's fail-closed audit resolver 400s them
	// "operator could not be resolved" (ka-0000007257) and the alert never
	// reaches Slack. Mirrors closed-enabled-drain / gardener-epic-reaper. Set
	// once so both the notifier ctx and any createCredentialWedgeBead inherit it.
	ctx = beadsapi.WithSystemOperator(ctx, "credential-wedge")

	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Limit:    credWedgeRosterLimit,
		// The sweep reads only counters and identity keys from fields; an
		// agent bead's prompt / wrapup / description are ~85% of its bytes
		// and this roster is re-read every cycle (gd-zXdZq4Traf).
		Omit:       []string{"description", "notes"},
		OmitFields: beadsapi.LeanAgentOmitFields,
	})
	if err != nil {
		e.logger.Warn("credential-wedge sweep: failed to list agent beads", "error", err)
		return
	}
	if res == nil {
		return
	}

	now := e.now()
	live := make(map[string]bool, len(res.Beads))
	for _, bead := range res.Beads {
		live[bead.ID] = true

		state := bead.Fields["agent_state"]
		if !credWedgeErrorStates[state] {
			// Not in a credential-error state — recovered or never wedged. Clear
			// any prior episode flag so a future wedge re-alerts once.
			e.credWedgeState.markFresh(bead.ID)
			continue
		}

		// Zero-progress test: an agent that ever completed a turn produced output
		// tokens and accrued cost. total_output_tokens AND total_cost_usd both
		// zero is the signature of "every turn died before the model responded"
		// (the 429-on-every-inference wedge). A non-zero on EITHER means the agent
		// made real progress and is merely rate-limited between productive turns —
		// not wedged — so we do not flag it.
		outputTokens := credWedgeFieldFloat(bead.Fields["total_output_tokens"])
		costUSD := credWedgeFieldFloat(bead.Fields["total_cost_usd"])
		if outputTokens > 0 || costUSD > 0 {
			e.credWedgeState.markFresh(bead.ID)
			continue
		}

		// Age the wedge from created_at, NOT updated_at: the whole point is that
		// updated_at is churned fresh by SyncAll's state propagation, so it cannot
		// measure how long the wedge has persisted (the ka-0000002023 lesson).
		// created_at is immune to that churn.
		age := now.Sub(bead.CreatedAt)
		if bead.CreatedAt.IsZero() || age < credWedgeMinAge {
			// Too young (or unknown creation time) to distinguish a wedge from a
			// normal startup rate-limit blip. Do NOT mark fresh — a bead that is
			// mid-window must not clear a prior episode flag — just skip it.
			continue
		}

		if !e.credWedgeState.markWedged(bead.ID) {
			// Already alerted this episode.
			continue
		}

		e.logger.Warn("credential-wedge: agent parked in credential-error state with zero progress",
			"agent_bead", bead.ID, "agent", bead.Fields["agent"],
			"project", bead.Fields["project"], "role", bead.Fields["role"],
			"agent_state", state,
			"age", age.Round(time.Minute).String(),
			"min_age", credWedgeMinAge.String(),
			"total_output_tokens", bead.Fields["total_output_tokens"],
			"total_cost_usd", bead.Fields["total_cost_usd"],
			"session_count", bead.Fields["session_count"],
			"total_request_count", bead.Fields["total_request_count"],
			"account", bead.Fields["account"])

		e.fireCredentialWedgeAlert(ctx, bead, state, age)
	}

	e.credWedgeState.prune(live)
}

// fireCredentialWedgeAlert emits the alert for a credential-wedged agent. It
// mirrors fireDarkLaneAlert's notify/create_bead fan-out and NEVER spawns or
// kills. The one-shot agent-bead comment is always written (the primary,
// bead-navigable signal the task asked for); notify and create_bead are the
// optional external legs.
func (e *TriggerEngine) fireCredentialWedgeAlert(ctx context.Context, bead *beadsapi.BeadDetail, state string, age time.Duration) {
	agentName := bead.Fields["agent"]
	if agentName == "" {
		agentName = bead.Title
	}
	acct := bead.Fields["account"]
	if acct == "" {
		acct = "(unset)"
	}

	msg := fmt.Sprintf(
		"🚨 credential-wedge: agent %s (%s) has been parked in agent_state=%s for %s with ZERO completed turns "+
			"(total_output_tokens=%s, total_cost_usd=%s, session_count=%s, requests=%s) — every injected turn is dying on a "+
			"credential/rate-limit error and `gb init` is re-injecting the initial prompt forever (bug ka-0000000592). "+
			"updated_at reads fresh because SyncAll keeps propagating the error state, so this is invisible to every "+
			"updated_at-based check. Likely cause: the pinned account (%s) exhausted its spend limit with no healthy LiteLLM "+
			"fallback for its model group. Check the account bead's status/throttle and the model's fallbacks; the reap-side "+
			"runtime ceiling (ka-0000000716) or `gb agent kill` frees the pod, but the ACCOUNT/model config must change or a "+
			"replacement re-wedges identically.",
		agentName, bead.ID, state,
		age.Round(time.Minute),
		emptyDash(bead.Fields["total_output_tokens"]), emptyDash(bead.Fields["total_cost_usd"]),
		emptyDash(bead.Fields["session_count"]), emptyDash(bead.Fields["total_request_count"]),
		acct,
	)

	// One-shot comment on the AGENT bead — the primary signal the task asked for
	// ("WARN log + a one-shot comment on the trigger/agent bead"). Deduped by the
	// caller's markWedged, so a persistently-wedged agent comments once per
	// episode, not every cycle. Best-effort: a comment failure is logged, never
	// fatal (the WARN log already fired).
	if err := e.daemon.AddComment(ctx, bead.ID, "credential-wedge-sweep", msg); err != nil {
		e.logger.Warn("credential-wedge sweep: failed to comment on agent bead",
			"agent_bead", bead.ID, "error", err)
	}

	if e.credWedgeNotify && e.notifier != nil {
		e.notifier(WithNoticeProject(ctx, noticeBeadProject(bead)), "credential-wedge", msg, "")
	}

	if e.credWedgeCreateBead {
		desc := fmt.Sprintf("%s\n\nagent_bead: %s\ncreated_at: %s\nagent_state: %s",
			msg, bead.ID, bead.CreatedAt.UTC().Format(time.RFC3339), state)
		e.createCredentialWedgeBead(ctx, bead, desc)
	}
}

// createCredentialWedgeBead files a bug bead recording the wedged agent, parented
// to the agent bead so the alert is navigable from it in beads-viewer. A failure
// is logged but not fatal — the comment/notify legs may already have alerted.
// Mirrors createDarkLaneBead.
func (e *TriggerEngine) createCredentialWedgeBead(ctx context.Context, agentBead *beadsapi.BeadDetail, desc string) {
	agentName := agentBead.Fields["agent"]
	if agentName == "" {
		agentName = agentBead.Title
	}
	title := fmt.Sprintf("Agent credential-wedged (zero-progress): %s", agentName)

	labels := []string{
		"liveness",
		"credential-wedge",
		"area:gardener-loop",
		"source:credential-wedge-sweep",
	}
	if proj := agentBead.Fields["project"]; proj != "" {
		labels = append(labels, "project:"+proj)
	}

	req := beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "bug",
		Description: desc,
		Labels:      labels,
		Priority:    1,
		CreatedBy:   "credential-wedge-sweep",
	}

	beadID, err := e.daemon.CreateBead(ctx, req)
	if err != nil {
		e.logger.Warn("credential-wedge sweep: failed to create wedge bead",
			"agent_bead", agentBead.ID, "error", err)
		return
	}

	if err := e.daemon.AddDependency(ctx, beadID, agentBead.ID, "parent-child", "credential-wedge-sweep"); err != nil {
		e.logger.Warn("credential-wedge sweep: failed to parent wedge bead to agent",
			"agent_bead", agentBead.ID, "bead", beadID, "error", err)
	}

	e.logger.Info("credential-wedge sweep: filed wedge bead", "agent_bead", agentBead.ID, "bead", beadID)
}

// emptyDash renders an empty field value as "0" so the alert message reads
// cleanly (an unset total_output_tokens is semantically zero).
func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

// credWedgeSweepState tracks, per agent bead ID, whether the sweep has already
// alerted for the current wedge episode. Cleared when the agent recovers (leaves
// the credential-error state or makes progress), so an agent that wedges,
// recovers, then wedges again alerts once per episode. The dedup discipline
// mirrors darkLaneSweepState.
type credWedgeSweepState struct {
	mu     sync.Mutex
	wedged map[string]bool // agent bead ID → already alerted this wedge episode
}

func newCredWedgeSweepState() *credWedgeSweepState {
	return &credWedgeSweepState{wedged: make(map[string]bool)}
}

// markWedged records that agentBeadID is wedged and reports whether this is the
// FIRST observation of the current wedge episode (i.e. an alert should fire now).
func (s *credWedgeSweepState) markWedged(agentBeadID string) (firstThisEpisode bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wedged[agentBeadID] {
		return false
	}
	s.wedged[agentBeadID] = true
	return true
}

// markFresh clears any wedge-episode flag for agentBeadID (it recovered).
func (s *credWedgeSweepState) markFresh(agentBeadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.wedged, agentBeadID)
}

// prune drops wedge-episode flags for agents no longer in the roster so the map
// cannot grow unbounded as agents are closed/deleted.
func (s *credWedgeSweepState) prune(live map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.wedged {
		if !live[id] {
			delete(s.wedged, id)
		}
	}
}

// credWedgeFieldFloat parses a numeric agent-bead field as float64, returning 0
// for an empty or malformed value (fail-open). An unset/malformed
// total_output_tokens or total_cost_usd reads as zero progress, which is the
// correct default: the real gardener-delivery wedge (ka-0000000592) had
// total_output_tokens=0, and a bead that never wrote the field is likewise
// zero-progress. Mirrors podspec.parseAccountFloat.
func credWedgeFieldFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}
