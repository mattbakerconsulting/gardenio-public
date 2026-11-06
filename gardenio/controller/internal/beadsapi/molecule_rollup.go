package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// maxRollupDepth bounds the descendant BFS in RollupMoleculeUsage. Molecule
// (formula instance) trees are typically 2-4 levels deep — a molecule, its
// step beads, and any spawned per-step agents. 64 leaves generous headroom
// and still terminates in a bounded number of round-trips on a degenerate or
// cyclic graph (the visited set guards cycles; this caps pathological depth).
const maxRollupDepth = 64

// WorkflowSpend mirrors the `workflow_spend` step-bead field reported by a
// dynamic-workflow host pod (design ka-0000007305 §3.2). A dynamic-workflow
// step runs one host pod whose in-session agent()/parallel() subagents never
// get their own beads, so their token pool is invisible to the per-session
// usage that RollupMoleculeUsage walks. The host pod therefore reports that
// pool explicitly via `kd update --field` (see ReportWorkflowSpend) so the
// rollup can SUM it onto the host session — the two pools are non-overlapping
// (driver vs subagents), so the subtree total is host-session + workflow_spend,
// never a prefer-one.
//
// On resume the field is overwritten — idempotent on RunID. Final is false at
// per-checkpoint writes so a mid-run pod death does not lose the partial; the
// rollup surfaces MoleculeUsage.Partial whenever any descendant's workflow_spend
// is non-final (§3.4).
type WorkflowSpend struct {
	AgentsSpawned    int64   `json:"agents_spawned"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ThinkingTokens   int64   `json:"thinking_tokens,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	RunID            string  `json:"run_id"`
	ReportedAt       string  `json:"reported_at"`
	Final            bool    `json:"final"`
}

// MoleculeStepUsage is the per-bead contribution surfaced in a molecule rollup.
// SessionUsage is the bead's own per-session usage (the driver pool);
// WorkflowSpend is non-nil only for dynamic-workflow host steps. TotalCostUSD
// is the SUM of the two — that sum is the whole point of §3.2.
type MoleculeStepUsage struct {
	BeadID        string         `json:"bead_id"`
	Title         string         `json:"title"`
	SessionUsage  SessionUsage   `json:"session_usage"`
	WorkflowSpend *WorkflowSpend `json:"workflow_spend,omitempty"`
	TotalCostUSD  float64        `json:"total_cost_usd"`
}

// MoleculeUsage is the rolled-up usage for a molecule's parent-child subtree.
//
//   - SessionUsage is the sum of every visited bead's per-session usage (the
//     host/driver token pool).
//   - WorkflowSpend is the sum of every visited bead's reported workflow_spend
//     (the dynamic-workflow subagent pool). It is NON-overlapping with
//     SessionUsage, so molecule cost = SessionUsage + WorkflowSpend.
//   - TotalCostUSD is SessionUsage.TotalCostUSD + WorkflowSpend.TotalCostUSD,
//     surfaced explicitly because that SUM is the corrected semantics of §3.2.
//   - Partial is true when any visited workflow_spend is non-final (an in-flight
//     dynamic-workflow run that may still be growing), so a mid-run rollup is
//     not silently treated as complete (§3.4).
type MoleculeUsage struct {
	MoleculeID       string              `json:"molecule_id"`
	SessionUsage     SessionUsage        `json:"session_usage"`
	WorkflowSpend    WorkflowSpend       `json:"workflow_spend"`
	TotalCostUSD     float64             `json:"total_cost_usd"`
	BeadCount        int                 `json:"bead_count"`
	SessionCount     int                 `json:"session_count"`
	WorkflowRunCount int                 `json:"workflow_run_count"`
	Partial          bool                `json:"partial"`
	Steps            []MoleculeStepUsage `json:"steps"`
}

// RollupMoleculeUsage walks the parent-child subtree rooted at molID and sums
// the per-session usage (already attached to agent/task/session beads by
// client_session.go) plus any reported `workflow_spend` (the dynamic-workflow
// subagent pool). Read-side only, no cache (design ka-0000007305 §3.1-3.2,
// no-cache-in-v1).
//
// The walk is a breadth-first descent over parent-child REVERSE dependencies:
// a child bead depends-on its parent (child → parent edge), so the children of
// a node are the reverse-dep rows whose Type is "parent-child" and whose
// DependsOnID is that node. The root molecule bead is included in the subtree
// (it contributes zero usage in practice but keeps the walk uniform). A
// visited set guards cycles; a depth cap guards pathological trees.
//
// Crucially (§3.2): host-session usage and workflow_spend are *non-overlapping*
// (the host session measures the orchestrator/driver tokens; workflow_spend
// measures the in-session subagent pool that has no beads). The molecule total
// is therefore their SUM — RollupMoleculeUsage adds them, it does not prefer one.
func (c *Client) RollupMoleculeUsage(ctx context.Context, molID string) (*MoleculeUsage, error) {
	if molID == "" {
		return nil, fmt.Errorf("rollup: empty molecule id")
	}

	// Phase 1: BFS over parent-child reverse-deps to collect every bead ID in
	// the subtree, in visitation order (root first).
	visited := map[string]bool{molID: true}
	var order []string
	level := []string{molID}
	for depth := 0; len(level) > 0; depth++ {
		if depth > maxRollupDepth {
			return nil, fmt.Errorf("rollup: subtree walk exceeded depth %d for molecule %s", maxRollupDepth, molID)
		}
		revs, err := c.BatchGetDependencies(ctx, level, "reverse")
		if err != nil {
			return nil, fmt.Errorf("rollup: fetching children of molecule %s: %w", molID, err)
		}
		var next []string
		for _, id := range level {
			order = append(order, id)
			for _, d := range revs[id] {
				if d.Type != "parent-child" || d.BeadID == "" {
					continue
				}
				child := d.BeadID
				if visited[child] {
					continue
				}
				visited[child] = true
				next = append(next, child)
			}
		}
		level = next
	}

	// Phase 2: fetch every bead's fields and sum the usage. Beads the daemon
	// did not return (deleted, cross-fleet) are absent from the map and simply
	// contribute nothing.
	beads, err := c.BatchGetBeads(ctx, order)
	if err != nil {
		return nil, fmt.Errorf("rollup: fetching subtree beads for molecule %s: %w", molID, err)
	}

	out := &MoleculeUsage{MoleculeID: molID, BeadCount: len(order)}
	for _, id := range order {
		bead := beads[id]
		if bead == nil {
			continue
		}
		su := parseSessionUsage(bead.Fields)
		ws, hasWS := parseWorkflowSpend(bead.Fields)

		stepCost := su.TotalCostUSD
		addSessionUsage(&out.SessionUsage, su)
		if su.hasAny() {
			out.SessionCount++
		}

		step := MoleculeStepUsage{BeadID: id, Title: bead.Title, SessionUsage: su}
		if hasWS {
			out.WorkflowRunCount++
			addWorkflowSpend(&out.WorkflowSpend, ws)
			if !ws.Final {
				out.Partial = true
			}
			stepCost += ws.TotalCostUSD
			wsCopy := ws
			step.WorkflowSpend = &wsCopy
		}
		step.TotalCostUSD = stepCost
		out.Steps = append(out.Steps, step)
	}

	// The molecule total is the SUM of the two non-overlapping pools.
	out.TotalCostUSD = out.SessionUsage.TotalCostUSD + out.WorkflowSpend.TotalCostUSD
	return out, nil
}

// ReportWorkflowSpend writes (overwrites) the `workflow_spend` field on a
// dynamic-workflow host step bead. This is the typed Go equivalent of the
// `kd update --field workflow_spend=...` write path the orchestration script
// uses from its completion / checkpoint handler. Idempotent on ws.RunID:
// re-reporting the same run overwrites the field rather than accumulating.
func (c *Client) ReportWorkflowSpend(ctx context.Context, stepBeadID string, ws WorkflowSpend) error {
	if stepBeadID == "" {
		return fmt.Errorf("report workflow_spend: empty step bead id")
	}
	return c.UpdateBeadFieldsTyped(ctx, stepBeadID, map[string]any{"workflow_spend": ws})
}

// hasAny reports whether a SessionUsage carries any non-zero usage, used to
// count contributing sessions without double-counting empty beads.
func (u SessionUsage) hasAny() bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.ThinkingTokens != 0 ||
		u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.TotalCostUSD != 0 ||
		u.RequestCount != 0 || u.ToolCallCount != 0 || u.ErrorCount != 0 ||
		u.CompactionCount != 0
}

// addSessionUsage accumulates b into a.
func addSessionUsage(a *SessionUsage, b SessionUsage) {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.ThinkingTokens += b.ThinkingTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheWriteTokens += b.CacheWriteTokens
	a.TotalCostUSD += b.TotalCostUSD
	a.RequestCount += b.RequestCount
	a.ToolCallCount += b.ToolCallCount
	a.ErrorCount += b.ErrorCount
	a.CompactionCount += b.CompactionCount
}

// addWorkflowSpend accumulates b into a. RunID/ReportedAt are run-scoped and
// not meaningful once summed across multiple runs, so they are left zero on
// the aggregate; Final is intentionally not aggregated here — MoleculeUsage.Partial
// captures non-finality at the rollup level.
func addWorkflowSpend(a *WorkflowSpend, b WorkflowSpend) {
	a.AgentsSpawned += b.AgentsSpawned
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.ThinkingTokens += b.ThinkingTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheWriteTokens += b.CacheWriteTokens
	a.TotalCostUSD += b.TotalCostUSD
}

// parseSessionUsage reads the per-session usage fields off a bead's stringified
// Fields map. Field names match the SessionUsage JSON tags written by
// CloseSession. Missing / unparseable fields default to zero.
func parseSessionUsage(fields map[string]string) SessionUsage {
	return SessionUsage{
		InputTokens:      fieldInt(fields, "input_tokens"),
		OutputTokens:     fieldInt(fields, "output_tokens"),
		ThinkingTokens:   fieldInt(fields, "thinking_tokens"),
		CacheReadTokens:  fieldInt(fields, "cache_read_tokens"),
		CacheWriteTokens: fieldInt(fields, "cache_write_tokens"),
		TotalCostUSD:     fieldFloat(fields, "total_cost_usd"),
		RequestCount:     fieldInt(fields, "request_count"),
		ToolCallCount:    fieldInt(fields, "tool_call_count"),
		ErrorCount:       fieldInt(fields, "error_count"),
		CompactionCount:  fieldInt(fields, "compaction_count"),
	}
}

// parseWorkflowSpend decodes the `workflow_spend` field (stored as a JSON
// object string in the stringified Fields map) into a WorkflowSpend. The
// boolean ok is false when the field is absent or empty so callers can
// distinguish "no dynamic-workflow run on this bead" from a zero-cost run.
func parseWorkflowSpend(fields map[string]string) (WorkflowSpend, bool) {
	raw, ok := fields["workflow_spend"]
	if !ok || raw == "" {
		return WorkflowSpend{}, false
	}
	var ws WorkflowSpend
	if err := json.Unmarshal([]byte(raw), &ws); err != nil {
		// A malformed workflow_spend should not break the whole rollup;
		// treat it as absent rather than erroring.
		return WorkflowSpend{}, false
	}
	return ws, true
}

// fieldInt parses an integer-valued usage field. Values are stored as
// stringified JSON numbers; ParseFloat tolerates both "123" and "1.2e3"
// before truncation to int64.
func fieldInt(fields map[string]string, key string) int64 {
	s := fields[key]
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}

// fieldFloat parses a float-valued usage field (e.g. total_cost_usd).
func fieldFloat(fields map[string]string, key string) float64 {
	s := fields[key]
	if s == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return 0
}
