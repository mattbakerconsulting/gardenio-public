package main

import (
	"fmt"
	"slices"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── escalation stats ────────────────────────────────────────────────────

// escalationOutcomeCounts is the outcome breakdown for a set of closed
// escalations. Unrecorded is the count closed WITHOUT a result_status —
// out-of-band closures (operator/gardener closed the bead directly) whose
// outcome is unknown. It is reported as its own bucket rather than inferred
// to be a failure, which is the bug this fixes (ka-0000007267).
type escalationOutcomeCounts struct {
	Total       int
	Success     int
	Failure     int
	Partial     int
	Unrecorded  int
	E2EExcluded int
}

// classifyEscalationOutcomes buckets closed escalations by their recorded
// result_status. E2E fixtures are excluded from the denominator (the harness
// never captures result_status, so they would swamp real signal). An empty
// result_status is counted as Unrecorded — never inferred to be success or
// failure — because that field is the only authoritative outcome record.
func classifyEscalationOutcomes(beads []*beadsapi.BeadDetail) escalationOutcomeCounts {
	var c escalationOutcomeCounts
	for _, b := range beads {
		if isE2EFixture(b.CreatedBy) {
			c.E2EExcluded++
			continue
		}
		c.Total++
		switch b.Fields["result_status"] {
		case "success":
			c.Success++
		case "failure":
			c.Failure++
		case "partial":
			c.Partial++
		default:
			c.Unrecorded++
		}
	}
	return c
}

// escalationClosureLatency returns the create→close duration for a closed
// escalation. It prefers the authoritative ClosedAt timestamp and falls back
// to UpdatedAt for closed rows that predate closed_at population (mirrors
// gardener_audit.go). ok is false when neither timestamp yields a positive
// latency (missing/unparseable timestamps, or close-at-or-before create).
func escalationClosureLatency(b *beadsapi.BeadDetail) (time.Duration, bool) {
	if b.CreatedAt.IsZero() {
		return 0, false
	}
	var closeT time.Time
	if b.ClosedAt != "" {
		if t, err := time.Parse(time.RFC3339, b.ClosedAt); err == nil {
			closeT = t
		}
	}
	if closeT.IsZero() && b.Status == "closed" {
		closeT = b.UpdatedAt
	}
	if closeT.IsZero() || !closeT.After(b.CreatedAt) {
		return 0, false
	}
	return closeT.Sub(b.CreatedAt), true
}

// percentileDuration returns the p-th percentile (0..100) of sorted using the
// nearest-rank method. sorted must be pre-sorted ascending. Returns 0 for an
// empty slice.
func percentileDuration(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * N), 1-indexed
	rank = max(rank, 1)
	rank = min(rank, len(sorted))
	return sorted[rank-1]
}

// formatLatency renders a closure-latency duration compactly: seconds under a
// minute, minutes under an hour, hours (1 decimal) under two days, else days.
func formatLatency(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	default:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	}
}

var escalationStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show escalation summary statistics",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Fetch open and in-progress.
		open, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"escalation"},
			Statuses: []string{"open", "in_progress"},
			Limit:    200,
		})
		if err != nil {
			return fmt.Errorf("listing escalations: %w", err)
		}

		// Fetch recently closed (last 50).
		closed, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"escalation"},
			Statuses: []string{"closed"},
			Sort:     "-updated_at",
			Limit:    50,
		})
		if err != nil {
			return fmt.Errorf("listing closed escalations: %w", err)
		}

		// Count by status.
		openCount := 0
		inProgressCount := 0
		for _, b := range open.Beads {
			switch b.Status {
			case "open":
				openCount++
			case "in_progress":
				inProgressCount++
			}
		}

		// Count by operation type (open only).
		byType := map[string]int{}
		for _, b := range open.Beads {
			opType := b.Fields["operation_type"]
			if opType == "" {
				opType = "(unset)"
			}
			byType[opType]++
		}

		// Count by priority (open only).
		byPriority := map[int]int{}
		for _, b := range open.Beads {
			byPriority[b.Priority]++
		}

		// Find oldest open escalation.
		var oldest *beadsapi.BeadDetail
		for _, b := range open.Beads {
			if b.Status == "open" {
				if oldest == nil || b.CreatedAt.Before(oldest.CreatedAt) {
					oldest = b
				}
			}
		}

		// Classify closed outcomes by their RECORDED result_status. E2E
		// fixtures are excluded from the denominator (the harness never
		// captures result_status). Closures with no result_status are counted
		// as Unrecorded — a bucket of their own, NOT inferred to be failures.
		// The old code folded unset into failure via an approved_by fallback,
		// which reported ~85% of the corpus as failures when it was really
		// just out-of-band bookkeeping (bug ka-0000007267).
		outcomes := classifyEscalationOutcomes(closed.Beads)

		// Closure-latency percentiles over the same real (non-fixture)
		// closures — the signal actually worth surfacing (strongly bimodal:
		// p50 ~1.4h, p90 ~200h corpus-wide).
		var latencies []time.Duration
		for _, b := range closed.Beads {
			if isE2EFixture(b.CreatedBy) {
				continue
			}
			if d, ok := escalationClosureLatency(b); ok {
				latencies = append(latencies, d)
			}
		}
		slices.Sort(latencies)
		var p50, p90 time.Duration
		if len(latencies) > 0 {
			p50 = percentileDuration(latencies, 50)
			p90 = percentileDuration(latencies, 90)
		}

		if jsonOutput {
			out := map[string]any{
				"open":              openCount,
				"in_progress":       inProgressCount,
				"total_active":      openCount + inProgressCount,
				"by_operation_type": byType,
				"by_priority":       byPriority,
				"recent_closures": map[string]any{
					"window":       len(closed.Beads),
					"total":        outcomes.Total,
					"success":      outcomes.Success,
					"failure":      outcomes.Failure,
					"partial":      outcomes.Partial,
					"unrecorded":   outcomes.Unrecorded,
					"e2e_excluded": outcomes.E2EExcluded,
				},
			}
			if oldest != nil {
				out["oldest_open"] = map[string]any{
					"id":         oldest.ID,
					"title":      oldest.Title,
					"created_at": oldest.CreatedAt,
					"age":        escalationAge(oldest.CreatedAt),
				}
			}
			if len(latencies) > 0 {
				out["closure_latency"] = map[string]any{
					"n":           len(latencies),
					"p50_seconds": int64(p50.Seconds()),
					"p90_seconds": int64(p90.Seconds()),
					"p50":         formatLatency(p50),
					"p90":         formatLatency(p90),
				}
			}
			printJSON(out)
			return nil
		}

		// Print summary.
		fmt.Println("── Escalation Summary ──")
		fmt.Printf("  Open:          %d\n", openCount)
		fmt.Printf("  In Progress:   %d\n", inProgressCount)
		fmt.Printf("  Total Active:  %d\n", openCount+inProgressCount)
		fmt.Println()

		if len(byType) > 0 {
			fmt.Println("── By Operation Type ──")
			for opType, count := range byType {
				fmt.Printf("  %-24s %d\n", opType, count)
			}
			fmt.Println()
		}

		if len(byPriority) > 0 {
			fmt.Println("── By Priority ──")
			for p := 0; p <= 4; p++ {
				if count, ok := byPriority[p]; ok {
					fmt.Printf("  P%d: %d\n", p, count)
				}
			}
			fmt.Println()
		}

		if oldest != nil {
			fmt.Println("── Staleness ──")
			fmt.Printf("  Oldest open:   %s (%s ago)\n", oldest.ID, escalationAge(oldest.CreatedAt))
			fmt.Printf("                 %s\n", oldest.Title)
			fmt.Println()
		}

		if outcomes.Total > 0 {
			fmt.Println("── Recent Closures (last 50) ──")
			fmt.Printf("  Success:     %d\n", outcomes.Success)
			fmt.Printf("  Failure:     %d\n", outcomes.Failure)
			if outcomes.Partial > 0 {
				fmt.Printf("  Partial:     %d\n", outcomes.Partial)
			}
			fmt.Printf("  Unrecorded:  %d  (closed without 'gb escalation close' — outcome not captured)\n", outcomes.Unrecorded)
			if outcomes.E2EExcluded > 0 {
				fmt.Printf("  (excluded %d e2e fixtures)\n", outcomes.E2EExcluded)
			}
			fmt.Println()
		}

		if len(latencies) > 0 {
			fmt.Println("── Closure Latency (real closures) ──")
			fmt.Printf("  p50: %s\n", formatLatency(p50))
			fmt.Printf("  p90: %s\n", formatLatency(p90))
			fmt.Printf("  (n=%d create→close)\n", len(latencies))
		}

		return nil
	},
}
