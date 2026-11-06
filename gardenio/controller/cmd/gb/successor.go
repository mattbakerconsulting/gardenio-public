package main

// Successor: the fallback owner for an operator decision/escalation.
//
// A decision or escalation is created by an agent and ADDRESSED to an operator
// (a human, via operated_by). The requesting agent is ephemeral: it files the
// item with --requesting-task, then `gb stop`s, and is auto-respawned when the
// operator resolves it. That works as long as SOMEONE eventually acts on the
// item. When the addressed operator goes quiet — offline, on leave, or simply
// swamped — the item can sit open indefinitely with nobody minding it and no
// live agent parked on a yield.
//
// The `successor` field names a fallback HUMAN (a type=user bead) who should
// pick the item up when its primary owner has been idle past a threshold. This
// is a ROUTING mechanism, not an auto-decider:
//
//   - `gb decision successor set <id> <user-id>` stamps the fallback owner.
//   - `gb decision successor resolve` finds open items whose owner has been
//     idle > threshold (default 1h) and re-surfaces each to its successor via
//     the same pingback path reflag uses. It NEVER closes, decides, approves,
//     or rejects on anyone's behalf.
//
// The one thing this deliberately does not do is resolve an item on the
// operator's behalf. The daemon's cross-operator guard (which refuses a close
// by a principal other than the addressed operator, and is CORRECT — see
// ka-0000000798) would reject that anyway, and quietly deciding an operator's
// escalation from an agent is exactly the failure the whole gate model exists
// to prevent. "Escalate on their behalf" here means re-route and re-notify the
// designated backup human — surfacing the item to a principal who CAN act,
// never acting for them.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// successorIdleThreshold is the default idle window after which an open
// decision/escalation with a designated successor is considered eligible for
// re-routing. Matches the ">1h without yielding" contract in the task brief
// (ka-0000006921) and the pingback rate-limit granularity in escalation.go.
const successorIdleThreshold = 1 * time.Hour

// SuccessorCandidate is one open item that should be routed to its fallback
// owner because its primary owner has been idle past the threshold.
type SuccessorCandidate struct {
	Bead      *beadsapi.BeadDetail
	Successor string        // the fallback owner (a user bead id)
	IdleFor   time.Duration // how long since the last owner-facing touch
}

// successorLastTouch returns the freshest owner-facing timestamp on an item —
// the moment the queue last did something the operator would have seen. It is
// deliberately NOT bead.UpdatedAt: any field write (including a successor stamp)
// bumps UpdatedAt, which would reset the idle clock and hide a genuinely stale
// item. We take the max of create time and the explicit re-surface stamps
// (reflag / pingback) instead, so routing an item does not itself make the item
// look freshly-tended on the next pass.
func successorLastTouch(b *beadsapi.BeadDetail) time.Time {
	last := b.CreatedAt
	for _, key := range []string{"reflagged_at", "pingback_requested_at"} {
		if v := b.Fields[key]; v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(last) {
				last = t
			}
		}
	}
	return last
}

// successorItemAnswered reports whether a decision has already been answered by
// the operator (a choice or free-text response landed) even though the bead is
// still open — the resolution comment/close may lag. An answered item must not
// be re-routed. Escalations carry no chosen/response_text, so this is a no-op
// for them.
func successorItemAnswered(b *beadsapi.BeadDetail) bool {
	return b.Fields["chosen"] != "" || b.Fields["response_text"] != ""
}

// resolveSuccessors is the pure fallback-resolution core: given a set of
// decision/escalation beads, it returns those that should be re-routed to their
// designated successor because the primary owner has been idle at least
// `threshold`. `now` is injected for deterministic testing.
//
// An item is eligible when ALL hold:
//   - it is open (a closed item needs no owner);
//   - it names a non-empty successor;
//   - it has not already been answered (no chosen/response_text);
//   - its idle window (now - successorLastTouch) is at least threshold.
//
// Results are sorted most-idle-first so the caller drains the stalest items
// before the merely-stale ones. Nil/closed beads are skipped, never panicked.
func resolveSuccessors(beads []*beadsapi.BeadDetail, threshold time.Duration, now time.Time) []SuccessorCandidate {
	var out []SuccessorCandidate
	for _, b := range beads {
		if b == nil || b.Status != "open" {
			continue
		}
		successor := b.Fields["successor"]
		if successor == "" {
			continue
		}
		if successorItemAnswered(b) {
			continue
		}
		idle := now.Sub(successorLastTouch(b))
		if idle < threshold {
			continue
		}
		out = append(out, SuccessorCandidate{Bead: b, Successor: successor, IdleFor: idle})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].IdleFor > out[j].IdleFor
	})
	return out
}

// ── command wiring ───────────────────────────────────────────────────────

func newSuccessorCmd(beadType string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "successor",
		Short: fmt.Sprintf("Manage the fallback owner for an open %s", beadType),
		Long: fmt.Sprintf(`Manage the SUCCESSOR (fallback owner) for %ss.

The successor is a backup human — a type=user bead — who should pick up an
open %s when its primary owner has been idle past a threshold (default 1h).
It is a routing lever, not an auto-decider: `+"`resolve`"+` only re-surfaces
stale items to their successor; it never closes, decides, approves, or rejects
on anyone's behalf (the cross-operator guard would refuse that anyway).

  gb %s successor set <id> <user-bead-id>   # stamp the fallback owner
  gb %s successor set <id> --clear          # remove it
  gb %s successor resolve                    # re-route idle items to successors`,
			beadType, beadType, beadType, beadType, beadType),
	}
	cmd.AddCommand(newSuccessorSetCmd(beadType))
	cmd.AddCommand(newSuccessorResolveCmd(beadType))
	return cmd
}

func newSuccessorSetCmd(beadType string) *cobra.Command {
	c := &cobra.Command{
		Use:   "set <id> [user-bead-id]",
		Short: fmt.Sprintf("Set (or --clear) the fallback owner on a %s", beadType),
		Long: fmt.Sprintf(`Stamp fields.successor on an open %s to a user bead id (the fallback owner),
or clear it with --clear.

The target must be a type=user bead (the type_constraint): the successor is a
human who can act on the operator queue, so an agent/task/other id is refused.
The field is nullable — --clear removes it.`, beadType),
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSuccessorSet(cmd, beadType, args)
		},
	}
	c.Flags().Bool("clear", false, "remove the successor field instead of setting it")
	return c
}

func newSuccessorResolveCmd(beadType string) *cobra.Command {
	c := &cobra.Command{
		Use:   "resolve",
		Short: fmt.Sprintf("Re-route idle %ss to their designated successor", beadType),
		Long: fmt.Sprintf(`Scan open %ss and re-surface each one whose primary owner has been idle past
the threshold (default 1h) to its designated successor.

For every eligible item this records successor_routed_at / successor_route_count,
adds an audit comment naming the successor, and requests a Slack re-post via the
same pingback path reflag uses. It NEVER closes, decides, approves, or rejects —
it only routes the item to a human who can.

  gb %s successor resolve                 # route items idle > 1h
  gb %s successor resolve --idle 30m      # tighter window
  gb %s successor resolve --dry-run       # show what would route, change nothing`, beadType, beadType, beadType, beadType),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSuccessorResolve(cmd, beadType)
		},
	}
	c.Flags().Duration("idle", successorIdleThreshold, "idle window before an item routes to its successor")
	c.Flags().Bool("dry-run", false, "list eligible items without stamping or notifying")
	return c
}

func runSuccessorSet(cmd *cobra.Command, beadType string, args []string) error {
	id := args[0]
	clear, _ := cmd.Flags().GetBool("clear")

	bead, err := daemon.GetBead(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("getting %s %s: %w", beadType, id, err)
	}
	if bead.Type != beadType {
		return fmt.Errorf("%s is not a %s bead (type=%s)", id, beadType, bead.Type)
	}
	if bead.Status == "closed" {
		return fmt.Errorf("%s %s is already closed; setting a successor has no effect", beadType, id)
	}

	if clear {
		if len(args) > 1 {
			return fmt.Errorf("--clear takes no user-bead-id argument")
		}
		if err := daemon.UpdateBeadFields(cmd.Context(), id, map[string]string{"successor": ""}); err != nil {
			return fmt.Errorf("clearing successor on %s: %w", id, err)
		}
		_ = daemon.AddComment(cmd.Context(), id, actor, fmt.Sprintf("successor cleared by %s", actor))
		if jsonOutput {
			printJSON(map[string]any{"id": id, "successor": nil})
			return nil
		}
		fmt.Printf("Cleared successor on %s\n", id)
		return nil
	}

	if len(args) != 2 {
		return fmt.Errorf("set requires a user-bead-id argument (or --clear)")
	}
	userID := args[1]

	// type_constraint: the successor must be a type=user bead. Validate against
	// the live daemon so a typo or an agent/task id is refused at set time
	// rather than silently routing to a non-human on the next resolve pass.
	if err := validateSuccessorTarget(cmd.Context(), userID); err != nil {
		return err
	}

	if err := daemon.UpdateBeadFields(cmd.Context(), id, map[string]string{
		"successor":        userID,
		"successor_set_at": time.Now().UTC().Format(time.RFC3339),
		"successor_set_by": actor,
	}); err != nil {
		return fmt.Errorf("setting successor on %s: %w", id, err)
	}
	_ = daemon.AddComment(cmd.Context(), id, actor, fmt.Sprintf("successor set to %s by %s", userID, actor))

	if jsonOutput {
		printJSON(map[string]any{"id": id, "successor": userID})
		return nil
	}
	fmt.Printf("Set successor on %s → %s\n", id, userID)
	return nil
}

// validateSuccessorTarget enforces the user-bead-id type_constraint.
func validateSuccessorTarget(ctx context.Context, userID string) error {
	target, err := daemon.GetBead(ctx, userID)
	if err != nil {
		return fmt.Errorf("successor target %s: %w (must be an existing type=user bead)", userID, err)
	}
	if target.Type != "user" {
		return fmt.Errorf("successor target %s is type=%s, not a user bead; the successor must be a human (type=user)", userID, target.Type)
	}
	return nil
}

func runSuccessorResolve(cmd *cobra.Command, beadType string) error {
	idle, _ := cmd.Flags().GetDuration("idle")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Enumerate the FULL open queue for this type. `gb escalation list` caps at
	// 50 rows silently (task brief ka-0000006921); we page with a high limit so
	// the oldest, stalest items — exactly the ones a successor exists for — are
	// never hidden.
	result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Statuses: []string{"open"},
		Types:    []string{beadType},
		Sort:     "-created_at",
		Limit:    500,
	})
	if err != nil {
		return fmt.Errorf("listing open %ss: %w", beadType, err)
	}

	candidates := resolveSuccessors(result.Beads, idle, time.Now().UTC())

	if len(candidates) == 0 {
		if jsonOutput {
			printJSON(map[string]any{"routed": []any{}, "count": 0, "dry_run": dryRun})
			return nil
		}
		fmt.Printf("No open %ss idle > %s with a successor set.\n", beadType, idle)
		return nil
	}

	routed := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		entry := map[string]any{
			"id":        c.Bead.ID,
			"successor": c.Successor,
			"idle_for":  c.IdleFor.Round(time.Minute).String(),
			"priority":  c.Bead.Priority,
		}
		if !dryRun {
			if err := routeToSuccessor(cmd.Context(), c); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to route %s to successor: %v\n", c.Bead.ID, err)
				entry["error"] = err.Error()
			}
		}
		routed = append(routed, entry)
	}

	if jsonOutput {
		printJSON(map[string]any{"routed": routed, "count": len(routed), "dry_run": dryRun})
		return nil
	}

	verb := "Routed"
	if dryRun {
		verb = "Would route"
	}
	fmt.Printf("%s %d %s(s) to their successor:\n", verb, len(routed), beadType)
	for _, e := range routed {
		fmt.Printf("  %-14s idle %-8s → %s\n", e["id"], e["idle_for"], e["successor"])
	}
	return nil
}

// routeToSuccessor applies the side effects for one candidate: it records the
// routing on the bead and requests a Slack re-post via the pingback handshake.
// It never mutates status, chosen, or response_text — routing is not resolving.
func routeToSuccessor(ctx context.Context, c SuccessorCandidate) error {
	b := c.Bead
	now := time.Now().UTC().Format(time.RFC3339)

	routeCount := 0
	if v := b.Fields["successor_route_count"]; v != "" {
		fmt.Sscanf(v, "%d", &routeCount)
	}
	routeCount++

	pingbacks := 0
	if v := b.Fields["pingback_count"]; v != "" {
		fmt.Sscanf(v, "%d", &pingbacks)
	}

	fields := map[string]string{
		"successor_routed_at":   now,
		"successor_route_count": fmt.Sprintf("%d", routeCount),
		"successor_routed_by":   actor,
		// Re-surface handshake — same fields the Escalations/decision watchers
		// key off (see runReflag in decision.go). Routes the card back to Slack.
		"pingback_requested_at": now,
		"pingback_count":        fmt.Sprintf("%d", pingbacks+1),
		"pingback_by":           actor,
		"pingback_source":       "successor",
		"pingback_reason":       fmt.Sprintf("owner idle %s; routed to successor %s", c.IdleFor.Round(time.Minute), c.Successor),
	}
	if err := daemon.UpdateBeadFields(ctx, b.ID, fields); err != nil {
		return err
	}
	// Audit comment — non-fatal; the routing fields already landed.
	msg := fmt.Sprintf("routed to successor %s (owner idle %s) by %s", c.Successor, c.IdleFor.Round(time.Minute), actor)
	if err := daemon.AddComment(ctx, b.ID, actor, msg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to record successor audit comment on %s: %v\n", b.ID, err)
	}
	return nil
}
