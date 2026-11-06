package main

// gb wake <agent> — manually wake a PARKED persistent identity NOW, instead of
// waiting for the controller wake-scanner to notice its next_respawn_at is due
// (persistent-agents Phase 2, task ka-0000004675).
//
// A persistent agent that hits its idle-TTL parks: the idle-TTL path CLOSES its
// agent bead but stamps wake metadata — successor=same + next_respawn_at + a
// parked close_reason + agent_state=dormant — so the identity is revivable
// rather than terminal (cmd/gb init_agent_idle_ttl.go idleTTLDespawn). The
// controller wake-scanner (internal/statusreporter/wake_scanner.go) revives it
// automatically when next_respawn_at ≤ now. `gb wake` is the MANUAL trigger: it
// performs the SAME atomic revive immediately, regardless of how far in the
// future next_respawn_at sits, so an operator can bring a parked agent back on
// demand.
//
// The revive is deliberately identical to the scanner's wakeOne revive path so
// the two never diverge: reopen the bead (status=open) in ONE atomic PATCH that
// also DELETES next_respawn_at (idempotent — a subsequent scanner pass no longer
// matches), DELETES the terminal agent_state (dormant → cleared, so the
// recreated pod boots clean and the viewer's dormant bucket empties without
// flapping), and BUMPS respawn_count. The bead keeps its NAME, so the
// reconciler's existing cold-start create-missing loop recreates the pod under
// the same name and the pod `claude --resume`s the conversation — no new spawn
// primitive, no new pod path.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var wakeCmd = &cobra.Command{
	Use:     "wake <agent-name-or-bead-id>",
	Short:   "Manually wake a parked persistent agent now (bypass its next_respawn_at wait)",
	GroupID: "agent",
	Long: `Wake a PARKED persistent identity immediately instead of waiting for the
controller wake-scanner to notice its next_respawn_at is due.

A persistent agent that hits its idle-TTL parks: its bead is CLOSED but carries
wake metadata (successor=same + next_respawn_at + agent_state=dormant). The
wake-scanner revives it when next_respawn_at arrives; ` + "`gb wake`" + ` performs the
same revive NOW — reopening the bead, clearing the wake metadata, and bumping
respawn_count in one atomic write so the reconciler recreates the pod under the
same name and ` + "`claude --resume`" + `s the conversation.

The target may be an agent NAME (fields.agent) or a bead id (gd-…).

  gb wake gardenio-slack-thread-assess-ivg   # by agent name
  gb wake ka-abc123                          # by bead id
  gb wake <agent> --force                    # revive past the respawn budget cap`,
	Args: cobra.ExactArgs(1),
	RunE: runWake,
}

func init() {
	wakeCmd.Flags().Bool("force", false, "revive even if the per-identity respawn budget (respawn_count ≥ max_respawns) is exhausted")
}

func runWake(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	target := strings.TrimSpace(args[0])
	force, _ := cmd.Flags().GetBool("force")

	bead, err := resolveWakeTarget(ctx, target)
	if err != nil {
		return err
	}

	// Validate the bead is actually a parked, wakeable identity. A wake only
	// makes sense for a CLOSED type=agent bead carrying successor=same — the
	// exact selection the wake-scanner uses. Anything else (a live agent, a
	// non-agent bead, a terminally-closed agent with no wake intent) is refused
	// so `gb wake` never resurrects an identity that was never meant to persist.
	if bead.Type != "agent" {
		return fmt.Errorf("%s is not an agent bead (type=%s)", bead.ID, bead.Type)
	}
	if bead.Status != "closed" {
		return fmt.Errorf("agent %s is not parked — its bead is %s, not closed (only a parked/dormant identity can be woken)", wakeName(bead), bead.Status)
	}
	if bead.Fields[beadsapi.FieldSuccessor] != beadsapi.SuccessorSame {
		return fmt.Errorf("agent %s is closed but carries no wake intent (successor=%q, not %q) — it was not parked as a persistent identity and cannot be woken",
			wakeName(bead), bead.Fields[beadsapi.FieldSuccessor], beadsapi.SuccessorSame)
	}

	// Respawn budget gate — mirrors the wake-scanner's wakeOne budget check so a
	// manual wake cannot bypass the per-identity self-wake backstop silently. An
	// operator can override with --force when they genuinely want to revive past
	// the cap.
	count := parseWakeIntField(bead.Fields[beadsapi.FieldRespawnCount], 0)
	max := parseWakeIntField(bead.Fields[beadsapi.FieldMaxRespawns], beadsapi.DefaultMaxRespawns)
	if count >= max && !force {
		return fmt.Errorf("agent %s has exhausted its respawn budget (respawn_count=%d ≥ max_respawns=%d); pass --force to wake it anyway",
			wakeName(bead), count, max)
	}

	// Atomic revive: reopen + clear wake field + clear terminal agent_state +
	// bump the respawn budget, all in ONE PATCH (RFC-7386 merge on `fields`; nil
	// deletes). Identical shape to WakeScanner.wakeOne and the plan-gardener
	// operator-reopen wake (ka-0000001583), so the manual and automatic paths can
	// never drift.
	open := "open"
	if err := daemon.UpdateBead(ctx, bead.ID, beadsapi.UpdateBeadRequest{
		Status: &open,
		// Clears next_respawn_at + agent_state (dormant → cleared), bumps
		// respawn_count, stamps wake_reason=manual (BOAT_WAKE_REASON, ka-0000000027).
		Fields: beadsapi.ReviveAgentFields(count+1, beadsapi.WakeReasonManual),
	}); err != nil {
		return fmt.Errorf("waking %s: %w", wakeName(bead), err)
	}

	// Audit comment mirrors the wake-scanner's revive trail so manual and
	// automatic wakes read consistently. Best-effort — a comment failure does not
	// undo the wake.
	comment := fmt.Sprintf(
		"Manually woken via `gb wake` by %s: parked persistent identity `%s` reopened "+
			"(respawn %d/%d) so the reconciler recreates the pod under the same name and "+
			"`claude --resume`s the conversation. (persistent-agents Phase 2, ka-0000004675)",
		actor, wakeName(bead), count+1, max)
	if err := daemon.AddComment(ctx, bead.ID, actor, comment); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: woke %s but audit comment failed: %v\n", bead.ID, err)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":            bead.ID,
			"agent":         wakeName(bead),
			"woken":         true,
			"respawn_count": count + 1,
			"max_respawns":  max,
			"forced":        force && count >= max,
		})
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Woke %s (bead %s) — respawn %d/%d. The reconciler will recreate its pod under the same name.\n",
		wakeName(bead), bead.ID, count+1, max)
	return nil
}

// resolveWakeTarget resolves a `gb wake` argument to an agent bead. A "gd-"
// prefix is treated as a bead id (direct GetBead); anything else is resolved as
// an agent NAME (fields.agent) across ANY status, so the parked CLOSED bead is
// found. A name that fails to resolve falls back to a bead-id lookup so an id
// that happens not to start with "gd-" still works.
func resolveWakeTarget(ctx context.Context, arg string) (*beadsapi.BeadDetail, error) {
	if strings.HasPrefix(arg, "gd-") || strings.HasPrefix(arg, "ka-") {
		bead, err := daemon.GetBead(ctx, arg)
		if err != nil {
			return nil, fmt.Errorf("getting bead %s: %w", arg, err)
		}
		return bead, nil
	}
	bead, err := daemon.FindAgentBeadAnyStatus(ctx, arg)
	if err == nil {
		return bead, nil
	}
	// Fall back to a direct bead-id lookup — the arg may be an id without the
	// conventional prefix. Surface the original name-resolution error if that
	// also fails, since a name is the common case.
	if bead, idErr := daemon.GetBead(ctx, arg); idErr == nil {
		return bead, nil
	}
	return nil, fmt.Errorf("resolving agent %q: %w", arg, err)
}

// wakeName returns the human agent name (fields.agent) for messages, falling
// back to the bead id when the field is absent.
func wakeName(b *beadsapi.BeadDetail) string {
	if n := strings.TrimSpace(b.Fields["agent"]); n != "" {
		return n
	}
	return b.ID
}

// parseWakeIntField parses a string-coded int field, returning def on
// empty/malformed. Mirrors the wake-scanner's parseIntField so the budget gate
// reads identically on both paths.
func parseWakeIntField(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
