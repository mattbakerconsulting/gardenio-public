package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// dashboardBlockText extracts a section block's rendered mrkdwn text.
func dashboardBlockText(t *testing.T, b interface{}) string {
	t.Helper()
	j, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	var m struct {
		Text struct{ Text string } `json:"text"`
	}
	if err := json.Unmarshal(j, &m); err != nil {
		t.Fatalf("unmarshal block: %v", err)
	}
	return m.Text.Text
}

// dashboardRollupMeta is a fully-populated usage field set: a settled cost, a
// stale in-pod estimate that must never be added on a terminal row, an
// authoritative turn count, and a context reading.
func dashboardRollupMeta() map[string]string {
	return map[string]string{
		"total_cost_usd": "4.25", "live_cost_usd": "9.99",
		"total_input_tokens": "50000", "total_output_tokens": "12000",
		"total_request_count": "37", "context_tokens": "120000", "session_count": "2",
	}
}

// TestDashboardRows_CarryTurnsAndContext pins the cost/turns/context rollup onto
// EVERY dashboard row type (ka-0000003171). Each row previously rendered
// formatCostSummary alone — $/in/out/sessions — so no row showed a turn count or
// a context figure, and the Stopped section showed nothing but the dollar total
// for a finished agent.
//
// The liveness column is the subtle part: it is derived from the agent's own
// agent_state via agentStateIsLive, NOT from the section the row renders in. The
// dashboard files agent_state=rate_limited under "Stopped", but a rate-limited
// agent is still RUNNING — so its context must read as a live figure, while a
// done agent in the same section reads "at exit".
func TestDashboardRows_CarryTurnsAndContext(t *testing.T) {
	agent := func(state string) beadsapi.AgentBead {
		return beadsapi.AgentBead{
			AgentName: "bot", Project: "gardenio", Role: "crew", Mode: "crew",
			AgentState: state, Metadata: dashboardRollupMeta(),
		}
	}

	cases := []struct {
		name       string
		block      interface{}
		wantAtExit bool // true => context is a frozen final reading
	}{
		{"stopped/done", dashboardAgentDeadBlock(Garden, agent("done"), false), true},
		{"stopped/failed", dashboardAgentDeadBlock(Garden, agent("failed"), false), true},
		// Filed under "Stopped" by the dashboard, but still a LIVE pod.
		{"stopped/rate_limited", dashboardAgentDeadBlock(Garden, agent("rate_limited"), false), false},
		{"working", dashboardAgentWorkingBlock(Garden, agent("working"), false), false},
		{"starting", dashboardAgentStartingBlock(Garden, agent("spawning"), false), false},
		{"idle", dashboardAgentIdleBlock(Garden, agent("idle"), 0, false), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dashboardBlockText(t, tc.block)
			// Cost (pre-existing) plus the two chips this fix adds.
			if !strings.Contains(got, "$4.25") {
				t.Errorf("row should carry the settled cost, got %q", got)
			}
			if !strings.Contains(got, Garden.Semantic(SemTurns)+" 37") {
				t.Errorf("row should carry the turns chip, got %q", got)
			}
			if !strings.Contains(got, "120.0k ctx") {
				t.Errorf("row should carry the context chip, got %q", got)
			}
			// The stale live estimate must never be folded in anywhere:
			// 4.25 + 9.99 = 14.24 would be a double-count.
			if strings.Contains(got, "$14.24") {
				t.Errorf("row must not add the stale live_cost_usd estimate, got %q", got)
			}
			if atExit := strings.Contains(got, "ctx at exit"); atExit != tc.wantAtExit {
				t.Errorf("ctx-at-exit = %v, want %v (liveness must come from agent_state, not the section) — got %q",
					atExit, tc.wantAtExit, got)
			}
		})
	}
}

// TestDashboardRows_SuppressionSilencesWholeRollup confirms SuppressCost hides
// the turns and context chips too, not just the dollar figure — the chips sit
// inside the same suppression guard.
func TestDashboardRows_SuppressionSilencesWholeRollup(t *testing.T) {
	a := beadsapi.AgentBead{AgentName: "bot", Project: "gardenio", AgentState: "done", Metadata: dashboardRollupMeta()}
	for name, block := range map[string]interface{}{
		"dead":     dashboardAgentDeadBlock(Garden, a, true),
		"working":  dashboardAgentWorkingBlock(Garden, a, true),
		"starting": dashboardAgentStartingBlock(Garden, a, true),
		"idle":     dashboardAgentIdleBlock(Garden, a, 0, true),
	} {
		got := dashboardBlockText(t, block)
		for _, leak := range []string{"$4.25", Garden.Semantic(SemTurns), "ctx"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: suppressed row leaked %q: %q", name, leak, got)
			}
		}
	}
}
