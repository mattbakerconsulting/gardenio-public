package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var yieldAgentID string

var yieldCmd = &cobra.Command{
	Use:   "yield",
	Short: "Block until a pending decision is resolved or mail arrives",
	Long: `Blocks the agent until one of the following events occurs:
  - A pending decision bead (type=decision, status=open) is closed/resolved
  - A mail/message bead targeting this agent is created
  - The timeout expires (default 24h)

Uses HTTP SSE for real-time notification, with 2-second polling as fallback.
Through a land door (--http-url https://<host>/land/<name>) it waits on the
land's event feed instead, resuming from its cursor when a stream ends.

Requires an open decision bead created with 'gb decision create' before
calling yield. After the decision resolves, gb yield calls
POST /v1/agents/{id}/gates/decision/satisfy to release the Stop gate.

Use 'gb yield' ONLY when you need the operator's answer to finish the CURRENT
turn — it holds a live pod parked. If you filed a decision but have no further
work this turn and the answer only triggers a FUTURE action (a merge awaiting
approval, an indefinite/multi-day wait), call 'gb stop' instead: a decision
filed with --requesting-task persists as the durable re-entry handle and
AUTO-RESPAWNS you the moment the operator resolves it, so 'gb stop' loses
nothing, while re-yielding across a long wait just gets torn down by cooldown
and re-spawned in a loop (see bug ka-0000003244).`,
	GroupID: "session",
	RunE: func(cmd *cobra.Command, args []string) error {
		timeout, _ := cmd.Flags().GetDuration("timeout")

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()

		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		// Resolve agent ID — required for gate satisfaction.
		agentID, err := resolveAgentIDWithFallback(ctx, yieldAgentID)
		if err != nil {
			return fmt.Errorf("agent identity required for yield: %w", err)
		}

		// Find open decision beads for this agent.
		// Use a high limit to avoid missing this agent's decisions when many
		// other agents have open decisions (the API doesn't support field filtering,
		// so we filter client-side by requesting_agent_bead_id).
		result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Statuses: []string{"open"},
			Types:    []string{"decision"},
			Sort:     "-created_at",
			Limit:    200,
		})
		if err != nil {
			return fmt.Errorf("listing decisions: %w", err)
		}

		// Find a decision that belongs to this agent.
		var pending *beadsapi.BeadDetail
		for _, b := range result.Beads {
			if b.Fields["requesting_agent_bead_id"] == agentID {
				pending = b
				break
			}
		}

		if pending == nil {
			return noPendingDecisionErr(agentID)
		}

		prompt := pending.Fields["prompt"]
		if prompt == "" {
			prompt = pending.Title
		}
		fmt.Fprintf(os.Stderr, "Yielding on decision %s: %s\n", pending.ID, prompt)

		// Write yield marker so the Stop hook can fast-path allow a quiet
		// turn-end silently while the agent is genuinely parked on a decision,
		// without injecting text or closing the bead (see hook.go's yield-parked
		// fast path). The marker is REMOVED only when the wait genuinely ends and
		// the agent is about to proceed — NOT on a context-cancellation exit.
		//
		// ka-0000002844: Claude Code's Bash tool sends SIGINT when a foreground
		// tool call blows its 120s timeout; signal.NotifyContext(ctx, os.Interrupt)
		// catches it, yieldSSE returns via ctx.Done, and the old
		// `defer os.Remove(yieldMarker)` fired — deleting the parked flag mid-wait
		// even though the decision was still open and the agent was still parked.
		// That un-marked, un-satisfied turn-end then tripped the cooldown close.
		// Keep the marker across a timeout/interrupt chop so the parked posture
		// survives; the resolve-nudge (or the next gb yield) proceeds normally,
		// and resetStopGateSessionState clears it at the next session launch.
		yieldMarker := stopGateYieldMarkerFile
		if f, err := os.Create(yieldMarker); err == nil {
			f.Close()
		}
		defer func() {
			// Only drop the parked flag when the wait ended for a genuine reason
			// (decision resolved, or interrupted by newly-assigned work) and the
			// agent is going to act now. On a ctx-cancellation exit (timeout /
			// interrupt) the agent is STILL parked — leave the marker in place so
			// the Stop hook keeps recognizing the parked state.
			if ctx.Err() == nil {
				os.Remove(yieldMarker)
			}
		}()

		// Resolve agent name for matching assigned beads (mentions, tasks).
		// This allows yield to unblock when work is assigned to this agent,
		// not just when mail/message beads arrive.
		agentName := resolveAgentName()
		if agentName == "" {
			// Fall back: read agent field from the agent bead itself.
			if ab, err := daemon.GetBead(ctx, agentID); err == nil {
				agentName = ab.Fields["agent"]
			}
		}

		// Freeze-on-yield (Δ4/A2, ka-0000000241): a freeze-eligible agent that has
		// opted into freeze-on-yield stops HOLDING the pod once the decision stays
		// open past a config'd threshold. yieldFreezeDeadline is the zero Time
		// (never freeze — today's behavior) unless both gates pass. The wait loop
		// returns outcome.freeze=true at the deadline; the suspend happens below.
		freezeAt := yieldFreezeDeadline(resolveYieldFreeze(ctx))

		// mailRecipient is this agent's mail identity — the value gb mail send
		// stamps as the recipient (Assignee) and gb mail inbox filters on. yield
		// must only wake on mail addressed to THIS identity (see mailAddressedToMe).
		outcome := yieldSSE(ctx, []*beadsapi.BeadDetail{pending}, agentName, resolveMailActor(), freezeAt)

		// If timed out or interrupted, return without further action.
		if ctx.Err() != nil {
			return nil
		}

		// Freeze branch: the live-hold threshold elapsed with the decision still
		// open. Suspend the pod (checkpoint + free the slot) and end the turn. The
		// open decision bead filed with --requesting-task is the durable re-entry
		// handle: on resolution the bridge's decision-resume respawn re-mints this
		// same-name agent, which THAWS the frozen process (A1 left the bead open at
		// agent_state=stopping with a persisted snapshot) instead of cold-starting.
		if outcome.freeze {
			return freezeOnYield(agentID, pending.ID)
		}

		// Track the yield on the agent bead for stop-gate backoff logic.
		// This MUST run before returning any artifact-required error so
		// that gate_satisfied_by=yield is set even when an artifact is needed.
		// NOTE: We intentionally do NOT satisfy the decision gate here.
		// The gate stays pending so the stop-gate continues to block natural
		// stops. Only `gb done` (which bypasses the gate via coop shutdown)
		// should terminate the agent. This prevents yield from accidentally
		// shutting down thread-bound agents before they act on the decision.
		markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer markCancel()

		// Set gate_satisfied_by first — this is critical for the stop gate.
		// Update it separately from yield_count so it succeeds even if
		// yield_count is not supported by the server version.
		if err := daemon.UpdateBeadFields(markCtx, agentID, map[string]string{
			"gate_satisfied_by": "yield",
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to set gate_satisfied_by: %v\n", err)
		}

		// Increment yield count for backoff tracking (best-effort).
		// yield_count is an integer-typed field on the agent bead; it must be
		// written as a JSON number, not a string. When the field is absent
		// (the common first-yield case), the string-map UpdateBeadFields path
		// would store "1" as a JSON string and the server rejects it with
		// HTTP 400 "yield_count: must be an integer". Use the typed update so
		// the value is always serialized as an int and initialize to 0/1.
		yieldCount := 1
		if bead, err := daemon.GetBead(markCtx, agentID); err == nil {
			if prev := bead.Fields["yield_count"]; prev != "" {
				if n, perr := strconv.Atoi(strings.TrimSpace(prev)); perr == nil {
					yieldCount = n + 1
				}
			}
		}
		if err := daemon.UpdateBeadFieldsTyped(markCtx, agentID, map[string]any{
			"yield_count": yieldCount,
		}); err != nil {
			// Non-critical: yield_count may not be supported on older servers
			fmt.Fprintf(os.Stderr, "Warning: failed to update yield count: %v\n", err)
		}

		// Re-fetch the resolved decision to check if an artifact is required.
		resolvedBead, fetchErr := daemon.GetBead(context.Background(), pending.ID)
		if fetchErr == nil && resolvedBead.Fields["required_artifact"] != "" {
			fmt.Fprintf(os.Stderr, "Artifact required (%s) — run: gb decision report %s --content '<artifact>'\n",
				resolvedBead.Fields["required_artifact"], pending.ID)
		}

		// If yieldSSE returned an error (e.g. artifact required), propagate it
		// now that gate tracking has been updated.
		if outcome.err != nil {
			return outcome.err
		}

		// Only announce "Decision resolved" when the target decision bead
		// actually resolved. yield can also unblock because work was assigned
		// to this agent (e.g. a Slack mention) — in that case the decision is
		// still open and claiming resolution would be a lie. Print the
		// appropriate continuation message for whichever event fired.
		fmt.Println()
		if outcome.resolved {
			fmt.Println("Decision resolved. Continue working on the outcome above.")
		} else {
			fmt.Println("Interrupted by new work above (decision still pending). Handle it, then resume.")
		}
		fmt.Println("When ALL work is complete: call `gb done` to despawn.")
		fmt.Println("Do NOT just exit — the stop gate will block until you call `gb done`.")

		return nil
	},
}

// noPendingDecisionErr is returned when an agent calls `gb yield` with no open
// self-owned decision bead. The old message told the agent to `gb decision
// create` — but yield's whole job is to BLOCK on a decision, and a gardener in
// the normal "I dispatched crew, now I wait" state has nothing to decide.
// Following the old hint manufactured an option-less decision, which the
// decision-options validator then rejected and bounced back to `gb stop`
// (ka-0000006309 / umbrella ka-0000004255 abatement #3). So point the agent at
// the command that actually matches its state: `gb stop` for the
// done/waiting-on-crews case (it is respawned next tick by its trigger or
// schedule), reserving `gb decision create` for a genuine operator-resolved
// fork. We still return a non-zero error so yield never silently "succeeds"
// and clears the stop gate. SilenceUsage on the root command (MR !4876) keeps
// this clean — one line on stderr, no cobra usage dump.
func noPendingDecisionErr(agentID string) error {
	return fmt.Errorf("no open decision to yield on for agent %s — yield blocks until an operator resolves a decision you own. "+
		"If you've dispatched crew / finished this turn, call 'gb stop' (you are respawned when there's more to do). "+
		"Only run 'gb decision create' when there is a real fork an operator must resolve before you can continue", agentID)
}

// yieldOutcome captures why yield stopped blocking.
//   - resolved=true: one of the target decision beads actually resolved
//     (closed, or a choice/response_text was recorded). Only this case may
//     print "Decision resolved".
//   - freeze=true: the live-hold threshold elapsed with the decision still open
//     and this freeze-eligible agent opted into freeze-on-yield (Δ4/A2). The
//     caller SUSPENDS the pod (checkpoint + free the slot) and ends the turn;
//     the open decision bead is the re-entry handle that thaws the agent on
//     resolution.
//   - resolved=false, freeze=false, err=nil: yield was interrupted by
//     newly-assigned work (e.g. a Slack mention) while the decision is still
//     pending, or it timed out / was cancelled.
//   - err!=nil: the resolved decision requires an artifact; propagated to the
//     caller so the agent is told to file it.
type yieldOutcome struct {
	resolved bool
	freeze   bool
	err      error
}

// mailAddressedToMe reports whether a type=mail bead created event is addressed
// to this agent. yield must NOT wake on mail bound for some OTHER agent — the
// kd server is shared fleet-wide, so a naive "any type=mail" wake fires the
// instant ANY agent anywhere receives mail, spuriously ending this agent's
// park (ka-0000008032: this raced the decisions-yield E2E, where a concurrent
// shard's mail woke an unrelated yield). The recipient is the mail bead's
// assignee — the value gb mail send stamps and gb mail inbox filters on.
//
// A conservative fallback: if we could not resolve our own mail identity
// (mailRecipient == ""), fall back to the legacy "any mail wakes" behavior so
// we never DEADLOCK a yield that a mail was legitimately meant to interrupt.
func mailAddressedToMe(beadAssignee, mailRecipient string) bool {
	if mailRecipient == "" {
		return true
	}
	return beadAssignee == mailRecipient
}

func yieldSSE(ctx context.Context, pending []*beadsapi.BeadDetail, agentName, mailRecipient string, freezeAt time.Time) yieldOutcome {
	pendingIDs := make(map[string]bool, len(pending))
	for _, b := range pending {
		pendingIDs[b.ID] = true
	}

	// A land door has no daemon event stream; the land feed wakes the wait.
	if land := landDoor(); land != "" {
		return yieldLandFeed(ctx, land, pendingIDs, agentName, freezeAt)
	}

	ch, err := daemon.EventStream(ctx, "beads.>")
	if err != nil {
		return yieldPoll(ctx, pendingIDs, agentName, freezeAt)
	}

	// Hybrid polling: check bead status every 5 seconds alongside SSE.
	// This guards against dropped SSE events (e.g. full client buffer,
	// multi-instance kbeads, transient reconnects) that would otherwise
	// cause yield to block indefinitely.
	pollTicker := time.NewTicker(5 * time.Second)
	defer pollTicker.Stop()

	// freezeCh fires at the freeze-on-yield deadline, or never (nil) when
	// freeze-on-yield is disabled for this agent.
	freezeCh := freezeChan(freezeAt)

	for {
		select {
		case <-freezeCh:
			// Live-hold threshold reached. Confirm the decision hasn't just
			// resolved (a race with the deadline) before freezing — a resolved
			// decision must take the warm path, never a needless checkpoint.
			if out, done := yieldPollOnce(ctx, pendingIDs, agentName); done {
				return out
			}
			return yieldOutcome{freeze: true}
		case evt, ok := <-ch:
			if !ok {
				return yieldPoll(ctx, pendingIDs, agentName, freezeAt)
			}
			var data map[string]any
			if json.Unmarshal(evt.Data, &data) == nil {
				// BeadClosed/BeadCreated/BeadUpdated: id and type are nested under "bead".
				// BeadDeleted: id is at top-level "bead_id".
				var beadID, beadType, beadKind, beadAssignee string
				if bead, ok := data["bead"].(map[string]any); ok {
					beadID, _ = bead["id"].(string)
					beadType, _ = bead["type"].(string)
					beadKind, _ = bead["kind"].(string)
					beadAssignee, _ = bead["assignee"].(string)
				} else {
					beadID, _ = data["bead_id"].(string)
				}
				// The ONLY event that counts as the decision resolving is the
				// target decision bead closing. A close event for any other
				// bead must be ignored. Re-check status to avoid relying solely
				// on the event shape.
				if pendingIDs[beadID] && evt.Event == "beads.bead.closed" {
					if decisionResolved(ctx, beadID) {
						return yieldOutcome{resolved: true, err: printYieldResult(beadID)}
					}
				}
				// Unblock when mail arrives FOR THIS AGENT. Mail is the canonical
				// inter-agent interrupt (gb mail send creates a type=mail bead
				// assigned to the recipient). yield must wake on it so the agent
				// can read its inbox, but the decision is still open — so this
				// returns resolved=false and the caller prints "Interrupted by
				// new work", never "Decision resolved" (ka-0000005877). This
				// deliberately does NOT wake on broadcast type=message squawks:
				// those flooded the inbox and busy-returned yield in the original
				// bug. Only directed type=mail wakes — and only mail addressed to
				// THIS agent, never a peer's mail on the shared server
				// (ka-0000008032; mirrors the assignee check on assigned work).
				if evt.Event == "beads.bead.created" && beadType == "mail" &&
					mailAddressedToMe(beadAssignee, mailRecipient) {
					fmt.Printf("Mail received: %s\n", beadID)
					return yieldOutcome{}
				}
				// Unblock when issue-kind work is created and assigned to THIS
				// agent (e.g. Slack mentions create task beads assigned to the
				// agent). Without this, mentions can't interrupt yield because
				// the coop nudge is blocked by the running gb yield command.
				// The decision stays open here too, so resolved=false.
				if evt.Event == "beads.bead.created" && agentName != "" && beadAssignee == agentName &&
					beadKind == "issue" && beadType != "decision" {
					fmt.Printf("Work assigned: %s (type=%s)\n", beadID, beadType)
					return yieldOutcome{}
				}
			}
		case <-pollTicker.C:
			if out, done := yieldPollOnce(ctx, pendingIDs, agentName); done {
				return out
			}
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				fmt.Println("Yield timed out")
			}
			return yieldOutcome{}
		}
	}
}

// yieldLandFeed is yieldSSE for an agent on a land door: it waits on the
// land feed (see land_feed.go), with a slow guard poll beside it, and falls
// back to yieldPoll when the feed cannot be reached.
func yieldLandFeed(ctx context.Context, land string, pendingIDs map[string]bool, agentName string, freezeAt time.Time) yieldOutcome {
	ch, err := streamLandFeed(ctx, land)
	if err != nil {
		return yieldPoll(ctx, pendingIDs, agentName, freezeAt)
	}
	pollTicker := time.NewTicker(landFeedGuardPoll)
	defer pollTicker.Stop()
	freezeCh := freezeChan(freezeAt)
	me := landDisplayName(agentName)

	for {
		select {
		case <-freezeCh:
			// Live-hold threshold reached (see yieldSSE).
			if out, done := yieldPollOnce(ctx, pendingIDs, agentName); done {
				return out
			}
			return yieldOutcome{freeze: true}
		case evt, ok := <-ch:
			if !ok {
				return yieldPoll(ctx, pendingIDs, agentName, freezeAt)
			}
			if out, done := yieldLandEvent(ctx, evt, pendingIDs, me); done {
				return out
			}
		case <-pollTicker.C:
			if out, done := yieldPollOnce(ctx, pendingIDs, agentName); done {
				return out
			}
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				fmt.Println("Yield timed out")
			}
			return yieldOutcome{}
		}
	}
}

// yieldLandEvent reports whether a land feed frame ends the wait. A target
// decision's decision.resolved resolves it, once decisionResolved confirms
// it. An open ticket assigned to this agent (me, as the feed names it)
// interrupts it, as newly-assigned work does in yieldPollOnce. The feed
// streams no mail (and no message.posted yet), so mail wakes only the poll.
func yieldLandEvent(ctx context.Context, evt landEvent, pendingIDs map[string]bool, me string) (yieldOutcome, bool) {
	var rec landRecord
	if json.Unmarshal(evt.Data, &rec) != nil {
		return yieldOutcome{}, false
	}
	switch evt.Type {
	case landEventDecisionResolved:
		if pendingIDs[rec.ID] && decisionResolved(ctx, rec.ID) {
			return yieldOutcome{resolved: true, err: printYieldResult(rec.ID)}, true
		}
	case landEventTicketUpdated:
		if me != "" && rec.Assignee == me && rec.Status == "open" {
			fmt.Printf("Work assigned: %s (type=%s)\n", rec.ID, rec.Kind)
			return yieldOutcome{}, true
		}
	}
	return yieldOutcome{}, false
}

func yieldPoll(ctx context.Context, pendingIDs map[string]bool, agentName string, freezeAt time.Time) yieldOutcome {
	freezeCh := freezeChan(freezeAt)
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				fmt.Println("Yield timed out")
			}
			return yieldOutcome{}
		case <-freezeCh:
			// Live-hold threshold reached (see yieldSSE). Confirm no last-instant
			// resolution before freezing.
			if out, done := yieldPollOnce(ctx, pendingIDs, agentName); done {
				return out
			}
			return yieldOutcome{freeze: true}
		case <-time.After(2 * time.Second):
		}

		if out, done := yieldPollOnce(ctx, pendingIDs, agentName); done {
			return out
		}
	}
}

// decisionResolved re-fetches a decision bead and reports whether it is
// genuinely resolved — closed, or carrying a recorded choice/response. This is
// the authoritative check the wait loop uses before declaring "Decision
// resolved", so a stray close/SSE event for the wrong bead can never trip it.
func decisionResolved(ctx context.Context, id string) bool {
	bead, err := daemon.GetBead(ctx, id)
	if err != nil {
		return false
	}
	return bead.Status == "closed" || bead.Fields["chosen"] != "" || bead.Fields["response_text"] != ""
}

// yieldPollOnce performs a single poll check against the target decision beads
// and newly-assigned work. The returned bool reports whether yield should stop
// blocking; the yieldOutcome distinguishes a real decision resolution from an
// interruption by assigned work.
func yieldPollOnce(ctx context.Context, pendingIDs map[string]bool, agentName string) (yieldOutcome, bool) {
	for id := range pendingIDs {
		if decisionResolved(ctx, id) {
			return yieldOutcome{resolved: true, err: printYieldResult(id)}, true
		}
	}

	// Check for newly assigned work (e.g. Slack mentions creating task beads).
	if agentName != "" {
		result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Assignee: agentName,
			Statuses: []string{"open"},
			Sort:     "-created_at",
			Limit:    1,
		})
		if err == nil && len(result.Beads) > 0 {
			b := result.Beads[0]
			// Only interrupt for issue-kind beads (task, bug, etc.) —
			// not for decisions or config beads which are infrastructure.
			if b.Kind == "issue" {
				fmt.Printf("Work assigned: %s (type=%s)\n", b.ID, b.Type)
				return yieldOutcome{}, true
			}
		}
	}

	return yieldOutcome{}, false
}

func printYieldResult(id string) error {
	bead, err := daemon.GetBead(context.Background(), id)
	if err != nil {
		return err
	}
	chosen := bead.Fields["chosen"]
	responseText := bead.Fields["response_text"]
	rationale := bead.Fields["rationale"]

	result := chosen
	if result == "" {
		result = responseText
	}
	if result != "" {
		if rationale != "" {
			fmt.Printf("Decision %s resolved: %s — %s\n", id, result, rationale)
		} else {
			fmt.Printf("Decision %s resolved: %s\n", id, result)
		}
	} else {
		// A closed decision IS resolved, even when the chosen/response_text
		// field hasn't propagated to the read replica yet. yieldSSE only
		// reaches here after decisionResolved() returned true (status=closed
		// or a recorded choice), so the close itself is the resolution. Emit
		// "resolved" unconditionally — the fields just aren't visible yet on
		// the fast SSE-close path. Saying "closed" here made the output
		// nondeterministic and flaked the decisions-yield E2E (ka-0000006882).
		fmt.Printf("Decision %s resolved (closed)\n", id)
	}

	// Check if the chosen option requires an artifact (set by gb decision respond).
	if ra := bead.Fields["required_artifact"]; ra != "" {
		fmt.Printf("ARTIFACT_REQUIRED type=%s decision_id=%s\n", ra, id)
		return fmt.Errorf("decision %s requires a %s artifact — run: gb decision report %s --content '<your report>'", id, ra, id)
	}

	return nil
}

func init() {
	yieldCmd.Flags().Duration("timeout", 24*time.Hour, "maximum time to wait")
	yieldCmd.Flags().StringVar(&yieldAgentID, "agent-id", "", "agent bead ID (default: KD_AGENT_ID env)")
}
