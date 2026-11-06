package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminBackfillCloseOrphanSessionsCmd closes type=session beads whose main
// Status is still "open" but whose lifecycle has clearly ended. These are
// ghost session beads left behind when an agent pod crashed before
// CloseSession ran, or when a transient PATCH failure prevented the
// follow-up CloseBead from firing (ka-0000008049, parent epic ka-0000006881),
// or when the pod was terminated before gb's in-pod close ran at all
// (gd-c6muILNedN: 3,577 open on 2026-10-24, mostly for closed agents).
//
// Selection criteria (a bead is closed if ANY apply):
//
//  1. Fields.ended_at is set (CloseSession PATCH succeeded but CloseBead
//     never completed — legitimately finished session).
//  2. Fields.status in {"completed","crashed","retired"} (CloseSession
//     updated the field but CloseBead failed).
//  3. Its agent bead (Fields.agent_bead_id) is closed for longer than
//     --agent-grace, or does not exist.
//  4. Bead is older than --min-age (default 24h) AND either Fields.status is
//     not "active" or it names no agent bead (orphaned by pod crash; the agent
//     will never come back to reconcile it).
//
// A session whose agent bead is still live is never closed, however old.
// Before the close, ended_at (the agent's closed_at when known) and
// status=retired are stamped where unset, the same values the controller's
// session reaper writes.
//
// Idempotent: skips beads whose Status is already "closed". Bounded by
// --limit per run to honor the kbeads migration policy (no eager full-table
// walks; operator iterates in batches).
var adminBackfillCloseOrphanSessionsCmd = &cobra.Command{
	Use:   "close-orphan-sessions",
	Short: "Close type=session beads whose lifecycle ended but Status is still open",
	Long: `Walks open type=session beads in --page-size pages and closes those whose
agent lifecycle has clearly ended:

  * Fields.ended_at is set, OR
  * Fields.status in {completed, crashed, retired}, OR
  * its agent bead (Fields.agent_bead_id) is closed past --agent-grace, or missing, OR
  * Bead is older than --min-age AND Fields.status is not "active" or it has no agent bead.

A session whose agent bead is live is never closed. ended_at and
status=retired are stamped before the close where unset.

Idempotent: closed sessions are filtered out by the server-side status query.
Bounded by --limit per run. Use --dry-run to preview.`,
	RunE: runAdminBackfillCloseOrphanSessions,
}

func init() {
	adminBackfillCloseOrphanSessionsCmd.Flags().Bool("dry-run", false, "report sessions that would close without writing")
	adminBackfillCloseOrphanSessionsCmd.Flags().Int("limit", 1000, "max sessions to close in this run (0 = unbounded)")
	adminBackfillCloseOrphanSessionsCmd.Flags().Int("page-size", 200, "sessions per ListBeads page")
	adminBackfillCloseOrphanSessionsCmd.Flags().Duration("min-age", 24*time.Hour, "minimum age before an active-but-stale session is considered orphaned")
	adminBackfillCloseOrphanSessionsCmd.Flags().Duration("agent-grace", 10*time.Minute, "how long an agent bead must have been closed before its open session is closed (leaves gb's own in-pod close time to land)")
	adminBackfillCmd.AddCommand(adminBackfillCloseOrphanSessionsCmd)
}

// Reasons a session is selected, reported per run.
const (
	orphanReasonEndedAt      = "ended_at set"
	orphanReasonStatus       = "terminal status"
	orphanReasonAgentClosed  = "agent closed"
	orphanReasonAgentMissing = "agent missing"
	orphanReasonMinAge       = "older than min-age"
)

// maxLeadingCloseFailures is how many closes may fail before the first one
// succeeds; past it the run stops.
const maxLeadingCloseFailures = 5

// sessionAgents is what one page's agent lookup found: agent beads by ID, and
// the IDs the daemon reported as not found. An ID in neither (a per-ID error
// other than not-found) is unknown, and its sessions are left alone.
type sessionAgents struct {
	beads   map[string]*model.Bead
	missing map[string]bool
}

func runAdminBackfillCloseOrphanSessions(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	limit, _ := cmd.Flags().GetInt("limit")
	pageSize, _ := cmd.Flags().GetInt("page-size")
	minAge, _ := cmd.Flags().GetDuration("min-age")
	agentGrace, _ := cmd.Flags().GetDuration("agent-grace")
	if pageSize <= 0 {
		pageSize = 200
	}

	ctx := context.Background()
	start := time.Now()
	cutoff := start.Add(-minAge)

	var (
		scanned  int
		closed   int
		skipped  int
		failed   int
		offset   int
		byReason = map[string]int{}
	)

	for {
		req := &client.ListBeadsRequest{
			Type:   []string{"session"},
			Status: []string{string(model.StatusOpen), string(model.StatusInProgress)},
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created_at",
		}
		resp, err := beadsClient.ListBeads(ctx, req)
		if err != nil {
			return fmt.Errorf("listing sessions (offset=%d): %w", offset, err)
		}
		if len(resp.Beads) == 0 {
			break
		}

		agents, err := lookupSessionAgents(ctx, resp.Beads)
		if err != nil {
			return fmt.Errorf("looking up agent beads (offset=%d): %w", offset, err)
		}

		pageClosed := 0
		for _, b := range resp.Beads {
			scanned++
			reason, endedAt := sessionOrphanReason(b, cutoff, agents, start, agentGrace)
			if reason == "" {
				skipped++
				continue
			}

			if dryRun {
				fmt.Printf("would close %s  created=%s  status=%v  ended_at=%v  agent=%v  reason=%s\n",
					b.ID, b.CreatedAt.Format(time.RFC3339),
					fieldStr(b, "status"), fieldStr(b, "ended_at"), fieldStr(b, "agent_bead_id"), reason)
			} else {
				if stamp := sessionCloseStamp(b, endedAt); stamp != nil {
					// Best-effort, like CloseSession: a failed stamp must not
					// leave the bead open, so the close still runs.
					if _, err := beadsClient.UpdateBead(ctx, b.ID, &client.UpdateBeadRequest{Fields: stamp}); err != nil {
						fmt.Fprintf(os.Stderr, "  !! %s: stamping ended_at/status failed: %v\n", b.ID, err)
					}
				}
				if _, err := beadsClient.CloseBead(ctx, b.ID, ""); err != nil {
					fmt.Fprintf(os.Stderr, "  !! %s: close failed: %v\n", b.ID, err)
					failed++
					// A run whose writes are all refused (a token without
					// the admin role gets 403 on every session it did not
					// create) stops instead of walking the whole set.
					if closed == 0 && failed >= maxLeadingCloseFailures {
						return fmt.Errorf("first %d closes all failed (last: %w); not continuing", failed, err)
					}
					if limit > 0 && closed+failed >= limit {
						break
					}
					continue
				}
			}
			closed++
			pageClosed++
			byReason[reason]++

			// --limit bounds attempts, failed ones included.
			if limit > 0 && closed+failed >= limit {
				break
			}
		}

		// Advance offset only by entries we did NOT close — once a bead
		// flips to closed it leaves the open-status window, so the next
		// page naturally shifts. For dry-run, advance fully.
		if dryRun {
			offset += len(resp.Beads)
		} else {
			offset += len(resp.Beads) - pageClosed
		}

		if limit > 0 && closed+failed >= limit {
			break
		}
		if len(resp.Beads) < pageSize {
			break
		}
	}

	verb := "closed"
	if dryRun {
		verb = "would close"
	}
	fmt.Printf("\nsessions scanned: %d\n", scanned)
	fmt.Printf("%s:           %d\n", verb, closed)
	reasons := make([]string, 0, len(byReason))
	for r := range byReason {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Printf("  %-20s %d\n", r+":", byReason[r])
	}
	fmt.Printf("skipped (live):   %d\n", skipped)
	if failed > 0 {
		fmt.Printf("failed:           %d\n", failed)
	}
	fmt.Printf("elapsed:          %s\n", time.Since(start).Round(time.Millisecond))

	if failed > 0 {
		return fmt.Errorf("%d session(s) failed to close", failed)
	}
	return nil
}

// lookupSessionAgents batch-fetches the agent beads a page of sessions names,
// in one request per page.
func lookupSessionAgents(ctx context.Context, sessions []*model.Bead) (sessionAgents, error) {
	agents := sessionAgents{beads: map[string]*model.Bead{}, missing: map[string]bool{}}
	seen := map[string]bool{}
	var ids []string
	for _, b := range sessions {
		id, _ := decodeFields(b.Fields)["agent_bead_id"].(string)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return agents, nil
	}
	results, err := beadsClient.BatchGetBeads(ctx, ids)
	if err != nil {
		return agents, err
	}
	for _, r := range results {
		switch {
		case r.Bead != nil:
			agents.beads[r.ID] = r.Bead
		case strings.Contains(strings.ToLower(r.Error), "not found"):
			agents.missing[r.ID] = true
		}
	}
	return agents, nil
}

// sessionOrphanReason returns why a session bead's lifecycle has clearly
// ended even though Status is still open ("" = it has not), and the ended_at
// to stamp: the agent's closed_at when the agent is closed, else now. The
// triggers map to the criteria documented on the command.
func sessionOrphanReason(b *model.Bead, cutoff time.Time, agents sessionAgents, now time.Time, agentGrace time.Duration) (reason string, endedAt time.Time) {
	fields := decodeFields(b.Fields)

	if ended, _ := fields["ended_at"].(string); ended != "" {
		return orphanReasonEndedAt, now
	}
	status, _ := fields["status"].(string)
	switch status {
	case "completed", "crashed", "retired":
		return orphanReasonStatus, now
	}

	agentID, _ := fields["agent_bead_id"].(string)
	if agentID != "" {
		if agent, ok := agents.beads[agentID]; ok {
			if agent.Status != model.StatusClosed {
				return "", time.Time{} // live agent: its session is live
			}
			if agent.ClosedAt == nil {
				return orphanReasonAgentClosed, now
			}
			if now.Sub(*agent.ClosedAt) < agentGrace {
				return "", time.Time{} // gb may still be closing it
			}
			return orphanReasonAgentClosed, *agent.ClosedAt
		}
		if agents.missing[agentID] {
			return orphanReasonAgentMissing, now
		}
	}

	// Old enough to be considered orphaned. A non-active session, or an
	// active one that names no agent bead, on a >24h-old bead means the
	// agent crashed without reconciling.
	if b.CreatedAt.Before(cutoff) && (status != "active" || agentID == "") {
		return orphanReasonMinAge, now
	}
	return "", time.Time{}
}

// sessionCloseStamp is the fields patch written before closing: ended_at and
// status=retired where unset, so a session gb did record keeps its values.
// nil when there is nothing to stamp.
func sessionCloseStamp(b *model.Bead, endedAt time.Time) json.RawMessage {
	fields := decodeFields(b.Fields)
	stamp := map[string]string{}
	if ended, _ := fields["ended_at"].(string); ended == "" {
		stamp["ended_at"] = endedAt.UTC().Format(time.RFC3339)
	}
	if status, _ := fields["status"].(string); status == "" || status == "active" {
		stamp["status"] = "retired"
	}
	if len(stamp) == 0 {
		return nil
	}
	raw, _ := json.Marshal(stamp)
	return raw
}

func fieldStr(b *model.Bead, key string) string {
	fields := decodeFields(b.Fields)
	v, _ := fields[key].(string)
	if v == "" {
		return "—"
	}
	return v
}
