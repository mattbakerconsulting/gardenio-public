// Package bridge — dark per-agent/day token-budget enforcement (R6-FU6c,
// ka-0000001323).
//
// The global bounded-stop guard (gardener_bounds_guard.go, ka-0000006744) caps a
// gardener's RUN count via run_budget. This file is the complementary cap on
// per-agent TOKEN spend: a dark agent that burns more than its per-agent/day
// ceiling — measured in USD, keyed to the authoritative LiteLLM cost-tracking
// the session-close reconciler writes onto each agent bead — is transitioned to
// agent_state=budget_exceeded and the gardener's further spawns are suppressed
// for that cycle.
//
// Source of truth: the agent bead's total_cost_usd (authoritative, written at
// session close from real LiteLLM tag spend by sessionSpendReconciler) plus,
// while the agent is still live, its live_cost_usd in-pod estimate. This mirrors
// formatAgentUsageLine's cost rendering so the guard and the Slack card agree on
// what an agent "has spent so far".
//
// "Per-agent/day": dark agents are ephemeral — a gardener spawns a fresh agent
// per run — so an agent bead's lifetime spend is, in practice, its spend within
// the day it was created. The guard scopes to agents created within the current
// UTC day so a long-lived gardener's historical agents from prior days never
// re-trip today's gate. The ceiling is per individual agent (not a gardener-wide
// sum): when any one spawned agent crosses it, that agent is flagged and the
// next spawn is held off.
//
// Only dark gardeners (IsDarkGardener) are subject to this guard — it is wired
// inside the dark branch of gardenerPreFireCheck. Non-dark gardeners are
// untouched.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// darkDefaultAgentDailyBudgetUSD is the per-agent/day spend ceiling applied when
// a dark gardener declares no explicit AgentDailyBudgetUSD (value 0). It is a
// generous-but-real cap: a single dark agent that spends more than this in one
// day is almost certainly looping or runaway, which is exactly the cost footgun
// R6-FU6 exists to bound (advice ka-0000000974). Operators tune it per gardener
// via the gardener block's `agent_daily_budget_usd`.
const darkDefaultAgentDailyBudgetUSD = 25.0

// darkAgentBudgetListLimit bounds the per-cycle list of a gardener's live agents.
// The live (non-terminal) dark-agent population per gardener is small (bounded by
// run_budget + one_spawn_at_a_time), so this is a generous backstop, not a
// working limit.
const darkAgentBudgetListLimit = 200

// effectiveAgentDailyBudgetUSD resolves a gardener's per-agent/day budget:
//
//   - negative ⇒ guard disabled (enabled=false), an explicit opt-out for a
//     gardener that genuinely should not cap per-agent spend;
//   - zero ⇒ use darkDefaultAgentDailyBudgetUSD;
//   - positive ⇒ the configured ceiling.
func effectiveAgentDailyBudgetUSD(cap *GardenerSpec) (budget float64, enabled bool) {
	if cap == nil {
		return darkDefaultAgentDailyBudgetUSD, true
	}
	switch {
	case cap.AgentDailyBudgetUSD < 0:
		return 0, false
	case cap.AgentDailyBudgetUSD == 0:
		return darkDefaultAgentDailyBudgetUSD, true
	default:
		return cap.AgentDailyBudgetUSD, true
	}
}

// agentSpendSoFarUSD computes an agent bead's spend "so far" the same way the
// Slack agent card does (formatAgentUsageLine): the authoritative total_cost_usd
// plus, while the agent is still live (non-terminal agent_state), its in-pod
// live_cost_usd estimate. At close total_cost_usd is authoritative and the live
// estimate is stale, so it is dropped to avoid double-counting.
func agentSpendSoFarUSD(detail *beadsapi.BeadDetail) float64 {
	if detail == nil {
		return 0
	}
	cost := parseUSD(detail.Fields["total_cost_usd"])
	if !isAgentTerminal(detail.Fields["agent_state"]) {
		cost += parseUSD(detail.Fields["live_cost_usd"])
	}
	return cost
}

// agentCreatedToday reports whether the agent bead was created within the
// current UTC day. Agents from a prior day must not re-trip today's per-agent/day
// gate. A zero CreatedAt (unknown) is treated as today (fail-closed: an agent
// with unknown age is still subject to the cap).
func agentCreatedToday(detail *beadsapi.BeadDetail, now time.Time) bool {
	if detail == nil {
		return false
	}
	if detail.CreatedAt.IsZero() {
		return true
	}
	y, m, d := now.UTC().Date()
	dayStart := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return !detail.CreatedAt.UTC().Before(dayStart)
}

// enforceDarkAgentDailyBudget is the firing-time per-agent/day token-budget gate
// for dark gardeners (R6-FU6c). It lists the live agents this gardener spawned
// (spawned_by: trigger:<id>), and for each one created today whose spend "so
// far" has crossed the gardener's per-agent/day ceiling it:
//
//   - stamps agent_state=budget_exceeded on that agent bead (idempotent: an
//     agent already in budget_exceeded is not re-written), and
//   - records the agent ID so the caller can suppress the gardener's next spawn.
//
// It returns exceeded=true when at least one live agent is over budget, which
// the caller uses to skip the fire (no new spawns while an existing agent is
// blowing its budget). Under-budget agents are left entirely untouched.
//
// Fail-open on infrastructure errors: a daemon list error returns
// (false, nil) so a transient hiccup cannot permanently wedge a gardener — the
// same posture as the run_budget / one_spawn_at_a_time gates. The bounded-stop
// guard still independently caps the gardener's run count.
func enforceDarkAgentDailyBudget(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, now time.Time) (exceeded bool, exceededAgents []string) {
	if logger == nil {
		logger = slog.Default()
	}
	if trig.Gardener == nil {
		return false, nil
	}
	budget, enabled := effectiveAgentDailyBudgetUSD(trig.Gardener)
	if !enabled {
		return false, nil
	}

	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Fields:   map[string]string{"spawned_by": "trigger:" + trig.ID},
		Limit:    darkAgentBudgetListLimit,
	})
	if err != nil {
		logger.Warn("dark agent budget: ListBeadsFiltered failed, treating as under-budget",
			"trigger", trig.ID, "error", err)
		return false, nil
	}
	if res == nil {
		return false, nil
	}

	for _, agent := range res.Beads {
		if agent == nil || agent.ID == "" {
			continue
		}
		if !agentCreatedToday(agent, now) {
			continue
		}
		spend := agentSpendSoFarUSD(agent)
		if spend < budget {
			continue
		}
		exceeded = true
		exceededAgents = append(exceededAgents, agent.ID)
		markAgentBudgetExceeded(ctx, daemon, logger, agent, spend, budget)
	}
	return exceeded, exceededAgents
}

// markAgentBudgetExceeded transitions an over-budget agent bead to
// agent_state=budget_exceeded and comments the overrun. Idempotent — an agent
// already flagged is skipped so the gate does not re-comment every cron tick.
// Best-effort: a write failure is logged, not fatal (the spawn was already
// suppressed by the caller).
func markAgentBudgetExceeded(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, agent *beadsapi.BeadDetail, spend, budget float64) {
	if agent.Fields["agent_state"] == "budget_exceeded" {
		return // already flagged — no churn
	}
	if err := daemon.UpdateBeadFields(ctx, agent.ID, map[string]string{
		"agent_state": "budget_exceeded",
	}); err != nil {
		logger.Warn("dark agent budget: UpdateBeadFields(agent_state=budget_exceeded) failed",
			"agent", agent.ID, "error", err)
		return
	}
	comment := fmt.Sprintf(
		"🛑 dark per-agent/day token budget exceeded (R6-FU6c, ka-0000001323): "+
			"this agent has spent $%.2f today, over its $%.2f/day ceiling — "+
			"agent_state set to budget_exceeded and the gardener's further spawns are suppressed.",
		spend, budget)
	if err := daemon.AddComment(ctx, agent.ID, "trigger-engine", comment); err != nil {
		logger.Warn("dark agent budget: AddComment failed", "agent", agent.ID, "error", err)
	}
}
