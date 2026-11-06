package main

// gardener_audit.go — `gb gardener-audit` reports cycle-level laziness signals
// for scheduled gardeners (gardener-intake/-delivery/-release).
//
// Why this exists: close_reason=context_cancelled is a broken signal — every
// cleanly-closed gardener bead ends up labeled that way because the dying pod's
// init_agent_loop calls closeAgentBeadBestEffort('context_cancelled') *after*
// the agent has already written its own close_reason (see ka-0000007597, and
// the fix in ka-0000000961). The real signal of a failed cycle is "the agent
// did not write a structured wrap-up", which `gb stop --wrapup` enforces.
//
// This command walks agent beads by schedule_id or role over a lookback
// window and reports, per cycle:
//   - wrapup present + accomplishments length (the laziness KPI)
//   - decisions filed by the agent during the cycle window
//   - decisions resolved during the cycle window
//   - pull_requests URL count from the wrap-up (optional advisory field)

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var gardenerAuditCmd = &cobra.Command{
	Use:     "gardener-audit",
	Aliases: []string{"captain-audit"}, // pre-rename name (gd-5O9QUlA8zr)
	Short:   "Audit gardener-cycle wrap-up + KPI deltas",
	Long: `Report per-cycle laziness signals for scheduled gardener agents.

For each agent bead in the window (filtered by --role or --schedule-id),
report whether the cycle wrote a structured wrap-up, the length of
accomplishments, decisions filed/resolved during the cycle, and pull-request
URLs captured in the wrap-up. A cycle with no wrap-up is flagged FAILED.

This replaces any audit keyed on close_reason=context_cancelled — that
signal is overwritten by init_agent_loop on every pod teardown and is
therefore useless for laziness detection (see ka-0000007597).

A bare --since (no --role and no --schedule-id) audits every gardener cycle in
the window fleet-wide. The window and the --role/--schedule-id selectors are
pushed into the daemon query, and the walk pages to exhaustion by default, so
the reported cycle_count is the TRUE in-window count. --limit 0 (the default)
means no cap; an explicit --limit caps the scan and warns on stderr when the
cap actually truncates the window. The JSON also carries the
wrap-up text (tldr/accomplishments/blockers/handoff_notes) and the cycle's
agent_state/close_reason, so a downstream no-op classifier does not need a
second bulk bead fetch to recover them (see ka-0000008261 item 3).

Examples:
  gb gardener-audit --role gardener-delivery --since 24h
  gb gardener-audit --schedule-id ka-0000001610 --since 12h --json
  gb gardener-audit --since 7d --json | jq '.cycles[] | select(.failed)'`,
	GroupID: "orchestration",
	RunE:    runGardenerAudit,
}

func init() {
	gardenerAuditCmd.Flags().String("role", "", "filter by agent role (e.g., gardener-intake, gardener-delivery, gardener-release)")
	gardenerAuditCmd.Flags().String("schedule-id", "", "filter by schedule_id (the scheduler bead ID that spawned the cycles)")
	gardenerAuditCmd.Flags().String("since", "24h", "lookback window (Go duration; accepts trailing d for days)")
	// Default 0 = no cap: the window (--since) and the --role/--schedule-id
	// selectors are all pushed server-side, so the matched set is already small
	// and the walk pages it to exhaustion. The old default of 500 capped the
	// PRE-FILTER fleet-wide agent-bead scan, silently collapsing a --since 7d
	// window to whatever the newest 500 agent beads spanned (~1 day at current
	// volume) — bug ka-0000001500.
	gardenerAuditCmd.Flags().Int("limit", 0, "max agent beads to scan (0 = no cap; the --since window is applied server-side)")
}

// gardenerCycle is one scheduled-gardener cycle audit.
type gardenerCycle struct {
	AgentID              string    `json:"agent_id"`
	AgentName            string    `json:"agent_name"`
	Role                 string    `json:"role"`
	ScheduleID           string    `json:"schedule_id,omitempty"`
	Status               string    `json:"status"`
	AgentState           string    `json:"agent_state,omitempty"`
	CloseReason          string    `json:"close_reason,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	ClosedAt             time.Time `json:"closed_at,omitzero"`
	Duration             string    `json:"duration,omitempty"`
	WrapupPresent        bool      `json:"wrapup_present"`
	AccomplishmentsChars int       `json:"accomplishments_chars"`
	// Wrap-up text carried through verbatim so a downstream no-op classifier
	// does not need a second bulk bead fetch to recover it (ka-0000008261
	// item 3). JSON-only; the text table stays a compact one-line-per-cycle view.
	TLDR              string `json:"tldr,omitempty"`
	Accomplishments   string `json:"accomplishments,omitempty"`
	Blockers          string `json:"blockers,omitempty"`
	HandoffNotes      string `json:"handoff_notes,omitempty"`
	DecisionsFiled    int    `json:"decisions_filed"`
	DecisionsResolved int    `json:"decisions_resolved"`
	PullRequests      int    `json:"pull_requests"`
	Failed            bool   `json:"failed"`
	FailedReason      string `json:"failed_reason,omitempty"`
}

type gardenerAuditReport struct {
	Role         string          `json:"role,omitempty"`
	ScheduleID   string          `json:"schedule_id,omitempty"`
	Since        string          `json:"since"`
	CycleCount   int             `json:"cycle_count"`
	FailedCount  int             `json:"failed_count"`
	WithWrapup   int             `json:"with_wrapup"`
	DecisionsSum int             `json:"decisions_filed_sum"`
	ResolvedSum  int             `json:"decisions_resolved_sum"`
	Cycles       []gardenerCycle `json:"cycles"`
}

func runGardenerAudit(cmd *cobra.Command, args []string) error {
	role, _ := cmd.Flags().GetString("role")
	scheduleID, _ := cmd.Flags().GetString("schedule-id")
	sinceStr, _ := cmd.Flags().GetString("since")
	limit, _ := cmd.Flags().GetInt("limit")

	// --role and --schedule-id are both optional. With neither, the audit runs
	// fleet-wide over every gardener cycle in the window (uncapped by default) —
	// the shape the weekly gardener-audit roll-up needs. (ka-0000008261 item 3.)

	dur, err := parseDuration(sinceStr)
	if err != nil {
		return fmt.Errorf("invalid --since: %w", err)
	}
	cutoff := time.Now().Add(-dur)

	ctx := cmd.Context()

	// List agent beads across all statuses — scheduler cycles close quickly,
	// so the closed-status set carries most of the audit surface. We ask for
	// both open and closed explicitly (activeStatuses + closed) to dodge any
	// daemon default that silently scopes to open.
	//
	// Every selector is pushed SERVER-SIDE (bug ka-0000001500): CreatedAfter
	// carries the --since window and Fields carries --role/--schedule-id, so a
	// row cap — when the caller sets one — bounds the ALREADY-WINDOWED matched
	// set rather than a fleet-wide pre-filter scan. Previously the daemon
	// returned the newest `limit` agent beads fleet-wide and the window + role
	// filters ran client-side afterwards, so `--since 7d` silently collapsed to
	// whatever ~500 newest beads spanned (measured: 79 cycles reported for a
	// true 505, and 4 for a true 294). Same family as ka-0000001936.
	agentQuery := beadsapi.ListBeadsQuery{
		Types:        []string{"agent"},
		Statuses:     []string{"open", "in_progress", "closed"},
		CreatedAfter: cutoff,
		Fields:       gardenerAuditFieldFilters(role, scheduleID),
	}
	agentResult, err := listGardenerAuditBeads(ctx, agentQuery, limit, "agent")
	if err != nil {
		return fmt.Errorf("listing agent beads: %w", err)
	}

	// The daemon has already applied the window and the role/schedule
	// selectors; nothing is left to filter client-side. Sort newest-first for
	// display (the exhaustive walk pins an ascending created_at page order).
	agents := agentResult.Beads
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].CreatedAt.After(agents[j].CreatedAt)
	})

	// Fetch decisions filed in the window (single query, filter per cycle).
	// Windowed server-side for the same reason as the agent query: a cycle's
	// decisions are always created at or after the cycle's own created_at, so
	// nothing inside any in-window cycle predates the cutoff.
	// No status filter, matching the pre-fix query: a decision can sit in any
	// status and every one of them counts toward decisions_filed. (Verified
	// against the live corpus — an unfiltered 7d decision query returns 88,
	// exactly open(35)+closed(53), so the daemon is not defaulting to
	// open-only here and an explicit status list could only ever drop rows.)
	decResult, err := listGardenerAuditBeads(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"decision"},
		CreatedAfter: cutoff,
	}, decisionScanLimit(limit), "decision")
	if err != nil {
		return fmt.Errorf("listing decision beads: %w", err)
	}
	decisions := decResult.Beads

	report := gardenerAuditReport{
		Role:       role,
		ScheduleID: scheduleID,
		Since:      sinceStr,
	}

	for _, ab := range agents {
		cycle := summarizeCycle(ab, decisions)
		report.Cycles = append(report.Cycles, cycle)
		report.CycleCount++
		if cycle.Failed {
			report.FailedCount++
		}
		if cycle.WrapupPresent {
			report.WithWrapup++
		}
		report.DecisionsSum += cycle.DecisionsFiled
		report.ResolvedSum += cycle.DecisionsResolved
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	printGardenerAuditTable(&report)
	return nil
}

// gardenerAuditFieldFilters turns the --role / --schedule-id selectors into
// daemon field predicates (fields->>'key' = 'value'). Returns nil when neither
// is set so the query stays fleet-wide. Running these server-side is what makes
// a row cap bound the MATCHED set rather than a pre-filter scan: on the real
// corpus, `--role plan-gardener --since 7d` matches 294 agent beads out of 3962
// in-window ones, so a client-side role filter wastes >92% of any cap.
func gardenerAuditFieldFilters(role, scheduleID string) map[string]string {
	var f map[string]string
	if role != "" {
		f = map[string]string{"role": role}
	}
	if scheduleID != "" {
		if f == nil {
			f = map[string]string{}
		}
		f["schedule_id"] = scheduleID
	}
	return f
}

// decisionScanLimit derives the decision-query cap from the agent-query one.
// An uncapped agent scan (--limit 0) means an uncapped decision scan too;
// otherwise keep the historical 2× headroom, since one cycle can file several
// decisions.
func decisionScanLimit(limit int) int {
	if limit <= 0 {
		return 0
	}
	return limit * 2
}

// listGardenerAuditBeads runs one audit query. With limit <= 0 (the default) it
// pages the already-windowed match set to exhaustion — the growth-proof shape,
// since --since + --role are applied server-side so the match set is small. An
// explicit --limit is honoured as a hard cap, but a cap that actually truncates
// is reported on stderr rather than silently shrinking the window: silent
// truncation is precisely the defect this fixes (ka-0000001500), so a caller
// who opts back into a cap still learns when the answer is partial.
func listGardenerAuditBeads(ctx context.Context, q beadsapi.ListBeadsQuery, limit int, what string) (*beadsapi.ListBeadsResult, error) {
	if limit <= 0 {
		return daemon.ListAllBeadsFiltered(ctx, q)
	}
	q.Sort = "-created_at"
	q.Limit = limit
	res, err := daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		return nil, err
	}
	if res.Total > len(res.Beads) {
		fmt.Fprintf(os.Stderr,
			"warning: --limit %d truncated the %s scan to %d of %d in-window matches; raise --limit or drop it (0 = no cap) for the true count\n",
			limit, what, len(res.Beads), res.Total)
	}
	return res, nil
}

// summarizeCycle builds a gardenerCycle row for one agent bead.
func summarizeCycle(ab *beadsapi.BeadDetail, decisions []*beadsapi.BeadDetail) gardenerCycle {
	agentName := ab.Fields["agent"]
	if agentName == "" {
		agentName = ab.Assignee
	}

	cycle := gardenerCycle{
		AgentID:     ab.ID,
		AgentName:   agentName,
		Role:        ab.Fields["role"],
		ScheduleID:  ab.Fields["schedule_id"],
		Status:      ab.Status,
		AgentState:  ab.Fields["agent_state"],
		CloseReason: ab.Fields["close_reason"],
		CreatedAt:   ab.CreatedAt,
	}

	// Determine cycle end. Prefer the bead's ClosedAt (authoritative close
	// time); fall back to UpdatedAt which gets touched on every comment.
	cycleEnd := time.Now()
	if ab.ClosedAt != "" {
		if t, err := time.Parse(time.RFC3339, ab.ClosedAt); err == nil {
			cycle.ClosedAt = t
			cycleEnd = t
		}
	}
	if cycle.ClosedAt.IsZero() && ab.Status == "closed" && !ab.UpdatedAt.IsZero() {
		cycle.ClosedAt = ab.UpdatedAt
		cycleEnd = ab.UpdatedAt
	}
	if !cycle.CreatedAt.IsZero() && !cycleEnd.IsZero() && cycleEnd.After(cycle.CreatedAt) {
		cycle.Duration = cycleEnd.Sub(cycle.CreatedAt).Truncate(time.Second).String()
	}

	// Wrap-up parsing — the *whole* point of this audit.
	wu := parseWrapupField(ab.Fields["wrapup"])
	if wu != nil {
		cycle.WrapupPresent = true
		cycle.AccomplishmentsChars = len(strings.TrimSpace(wu.Accomplishments))
		cycle.PullRequests = len(wu.PullRequests)
		// Carry the wrap-up text through so a downstream no-op classifier can
		// score it without a second bulk bead fetch (ka-0000008261 item 3).
		cycle.TLDR = wu.TLDR
		cycle.Accomplishments = wu.Accomplishments
		cycle.Blockers = wu.Blockers
		cycle.HandoffNotes = wu.HandoffNotes
	}

	// Decisions filed/resolved during the cycle window, scoped to this agent.
	for _, d := range decisions {
		if !isCreatedByAgent(d, agentName, ab.ID) {
			continue
		}
		if d.CreatedAt.Before(cycle.CreatedAt) {
			continue
		}
		if !cycleEnd.IsZero() && d.CreatedAt.After(cycleEnd) {
			continue
		}
		cycle.DecisionsFiled++
		if d.Fields["chosen"] != "" {
			cycle.DecisionsResolved++
		}
	}

	// Cycle is FAILED if no wrap-up OR accomplishments is empty. Anything
	// else (ran out of time, yielded, failed technically) is *not* the
	// laziness signal we are measuring.
	if !cycle.WrapupPresent {
		cycle.Failed = true
		cycle.FailedReason = "no wrapup"
	} else if cycle.AccomplishmentsChars == 0 {
		cycle.Failed = true
		cycle.FailedReason = "empty accomplishments"
	}

	return cycle
}

// isCreatedByAgent matches a decision bead to the owning agent. Agent beads
// themselves rarely file decisions under their own bead ID; decisions are
// typically attributed to the agent name (created_by). We check both.
func isCreatedByAgent(d *beadsapi.BeadDetail, agentName, agentBeadID string) bool {
	if agentName == "" && agentBeadID == "" {
		return false
	}
	cb := d.CreatedBy
	if cb == "" {
		return false
	}
	if agentName != "" && strings.EqualFold(cb, agentName) {
		return true
	}
	if agentBeadID != "" && cb == agentBeadID {
		return true
	}
	return false
}

// parseWrapupField decodes the bead's wrapup field into a WrapUp struct.
// Returns nil for missing or unparseable values — both are treated as "no
// wrap-up" by the caller.
func parseWrapupField(raw string) *WrapUp {
	if raw == "" || raw == "null" {
		return nil
	}
	var wu WrapUp
	if err := json.Unmarshal([]byte(raw), &wu); err != nil {
		return nil
	}
	return &wu
}

// printGardenerAuditTable renders the audit report as a text table.
func printGardenerAuditTable(r *gardenerAuditReport) {
	scope := []string{}
	if r.Role != "" {
		scope = append(scope, "role="+r.Role)
	}
	if r.ScheduleID != "" {
		scope = append(scope, "schedule="+r.ScheduleID)
	}
	scopeStr := strings.Join(scope, " ")
	if scopeStr == "" {
		scopeStr = "fleet-wide (all gardeners)"
	}

	fmt.Fprintf(os.Stdout, "\nGardener audit: %s  since=%s\n", scopeStr, r.Since)
	if r.CycleCount == 0 {
		fmt.Fprintln(os.Stdout, "\nNo cycles in window.")
		return
	}
	fmt.Fprintf(os.Stdout, "cycles=%d  with_wrapup=%d  FAILED=%d  decisions_filed=%d  decisions_resolved=%d\n\n",
		r.CycleCount, r.WithWrapup, r.FailedCount, r.DecisionsSum, r.ResolvedSum)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CYCLE\tSTARTED\tDUR\tSTATUS\tWRAPUP\tACCOMP\tDECISIONS\tRESOLVED\tPRs\tFLAG")
	for _, c := range r.Cycles {
		wrapup := "no"
		if c.WrapupPresent {
			wrapup = "yes"
		}
		flag := ""
		if c.Failed {
			flag = "FAILED: " + c.FailedReason
		}
		started := c.CreatedAt.Format("01-02 15:04")
		dur := c.Duration
		if dur == "" {
			dur = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
			c.AgentID, started, dur, c.Status, wrapup,
			c.AccomplishmentsChars, c.DecisionsFiled, c.DecisionsResolved, c.PullRequests, flag)
	}
	w.Flush()
	fmt.Fprintln(os.Stdout)
}
