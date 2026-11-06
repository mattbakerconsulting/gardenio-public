package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var closeCmd = &cobra.Command{
	Use:     "close <id>...",
	Short:   "Close one or more beads",
	GroupID: "workflow",
	Example: `  # Close a bead and record why (adds a comment before closing):
  kd close ka-abc123 --reason "fixed by MR !4321, CI green"

  # -r is the shorthand; --comment is an accepted alias:
  kd close ka-abc123 -r "duplicate of ka-def456"

  # The reason comment lands on EACH id when closing several at once:
  kd close ka-abc123 ka-def456 --reason "batch-closed: superseded by epic ka-ghi789"`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		escalate, _ := cmd.Flags().GetBool("escalate")
		if escalate {
			ctx = client.WithEscalate(ctx)
		}

		// Closure reason: --reason (shorthand -r); --comment is a normalized
		// alias (see aliasCloseReasonFlags) so both spellings resolve here. When
		// set, it is recorded as a comment on EACH id BEFORE that id is closed,
		// so the audit text survives even if the close later escalates — and a
		// failed comment aborts the close so the reason is never silently lost.
		reason, _ := cmd.Flags().GetString("reason")

		// Open-MR close guard (ka-0000003953): a bead carrying an mr_url whose
		// MR is still `opened` must not be closed — the ledger cannot claim a
		// fix shipped while its code is not on main. --allow-open-mr is the
		// deliberate override (e.g. closing a bug as won't-fix while its MR is
		// abandoned). Off by default so the common accidental close is caught.
		allowOpenMR, _ := cmd.Flags().GetBool("allow-open-mr")
		// --force (gd-lIKFejVISw): the daemon refuses the close itself when
		// the PR is open, and lets it through only for an operator who sends
		// X-Force-Close. --allow-open-mr sends it too, so the old spelling
		// keeps working for operators; for an agent token both are refused.
		force, _ := cmd.Flags().GetBool("force")
		if force || allowOpenMR {
			allowOpenMR = true
			ctx = client.WithForceClose(ctx)
		}
		// mrBeadCache memoizes mr_url → canonical merge-request bead across the
		// ids in a single invocation (findMRBead contract).
		mrBeadCache := map[string]*model.Bead{}

		for _, id := range args {
			if openMR, mrURL := beadHasOpenMR(ctx, id, mrBeadCache); openMR {
				if !allowOpenMR {
					return fmt.Errorf(
						"refusing to close %s: its merge request %s is still open (not merged) — "+
							"a closed bead would claim a fix shipped while its code is not on main.\n"+
							"  Merge the MR first, or pass --allow-open-mr to close anyway — e.g. the MR was\n"+
							"  abandoned, or it IS already merged and its merge-request bead has not caught up yet.",
						id, mrURL)
				}
				// Deliberate override: stamp mr_close_override=true so the
				// gitlab-bridge reconcile sweep (ka-0000000267) leaves this
				// close alone instead of re-opening it because its MR is still
				// open. Without this marker, direction 1 (the guard, which
				// --allow-open-mr overrode here) and direction 2 (the sweep)
				// would fight forever. Best-effort: a stamp failure must not
				// block the close the operator explicitly asked for — the
				// sweep re-verifies against GitLab and only re-opens beads
				// whose MR is genuinely still `opened`, so the worst case of a
				// missed stamp is one spurious re-open, not lost work.
				if err := stampMRCloseOverride(ctx, id); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not stamp mr_close_override on %s (sweep may re-open it): %v\n", id, err)
				}
			}

			if reason != "" {
				if _, err := beadsClient.AddComment(ctx, id, actor, reason); err != nil {
					// With --escalate on a type whose write is escalatable, the
					// comment write itself escalates (HTTP 202). Surface it the
					// same way the close path does and skip the close for this
					// id — the reason rides along in the escalation's payload, so
					// it is not lost, and we avoid filing a second escalation for
					// the close of the same bead.
					var esc *client.EscalationCreated
					if errors.As(err, &esc) {
						printEscalationNotice(esc)
						continue
					}
					return fmt.Errorf("recording reason on %s: %w", id, err)
				}
			}

			bead, err := beadsClient.CloseBead(ctx, id, actor)
			if err != nil {
				var esc *client.EscalationCreated
				if errors.As(err, &esc) {
					printEscalationNotice(esc)
					continue
				}
				return fmt.Errorf("closing %s: %w", id, err)
			}

			if jsonOutput {
				printBeadJSON(bead)
			} else {
				// Terse single-line success for every close (ka-0000005647).
				// The single-id path used to dump the full bead table, which
				// trained agents to pipe `kd close` through `2>/dev/null`/`tail`
				// — the error-masking habit the audit flagged. Multi-id was
				// already terse; single-id now matches. Use --json for structure.
				fmt.Printf("Closed %s\n", bead.ID)
			}
		}
		return nil
	},
}

// beadHasOpenMR reports whether the bead `id` links a merge request that is
// still open (not merged), returning that MR's url for the refusal message.
//
// It fires ONLY on POSITIVE evidence that the MR is open, so it can never turn
// a read failure into a spurious close-refusal:
//   - the bead has no mr_url                     → (false, "")
//   - the bead can't be fetched                  → (false, "") — never block on our own error
//   - the MR is merged / closed (or looks merged) → (false, "")
//   - the MR is demonstrably still `opened`       → (true, url)
//
// Projected MR state comes from the canonical type=merge-request bead
// (findMRBead), which the gitlab-bridge keeps current — NOT the issue bead's own
// mr_state, which is stale/unset by design (advice ka-0000000519, bug
// ka-0000003953). When no canonical MR bead exists yet (brand-new link the
// bridge hasn't projected), we fall back to the issue bead's own mr_state only
// if it positively says `opened`; absent that, we do not block.
//
// Both of those are bridge PROJECTIONS and can trail a just-completed merge, so
// an `opened` reading is not final: before refusing, mrStillOpenLive re-checks
// GitLab itself and lets the close through on a positive `merged`. That is what
// keeps the merge→close race (bug ka-0000004113) from false-refusing a bead
// whose MR shipped seconds ago.
func beadHasOpenMR(ctx context.Context, id string, cache map[string]*model.Bead) (bool, string) {
	bead, err := beadsClient.GetBead(ctx, id)
	if err != nil || bead == nil {
		return false, "" // never block a close on our own read failure
	}
	// Already closed → re-close is an idempotent no-op (see CloseBead); the bad
	// open→closed transition, if any, already happened, so don't add friction
	// to the repeat. Reconciling an already-drifted closed bead is a separate
	// sweep's job, not this transition guard's.
	if bead.Status == model.StatusClosed {
		return false, ""
	}
	fields := decodeFields(bead.Fields)
	mrURL, _ := fields["mr_url"].(string)
	if mrURL == "" {
		return false, "" // no linked MR — nothing to guard
	}

	// Projected state: the canonical merge-request bead.
	if mr, mrErr := findMRBead(ctx, mrURL, cache); mrErr == nil && mr != nil {
		mrFields := decodeFields(mr.Fields)
		if mrIsMerged(mrFields) {
			return false, ""
		}
		if state, _ := mrFields["mr_state"].(string); state == "opened" {
			return mrStillOpenLive(ctx, mrURL), mrURL
		}
		// Any other canonical state (closed, unknown, unset) → don't block.
		return false, ""
	}

	// No canonical MR bead (or lookup failed): fall back to the issue bead's
	// own mr_state, but ONLY block on a positive `opened` — never on absence.
	if state, _ := fields["mr_state"].(string); state == "opened" {
		return mrStillOpenLive(ctx, mrURL), mrURL
	}
	return false, ""
}

// mrStillOpenLive is the last word before a refusal: it re-checks `opened`
// projected evidence against GitLab itself and reports whether the MR really is
// still open.
//
// This closes the merge→close race (bug ka-0000004113). Both the canonical MR
// bead and the issue bead's mr_state are bridge-fed projections that trail a
// synchronous `glab mr merge` by tens of seconds, so a just-merged MR can read
// `opened` for a window in which the guard false-refuses a correctly-shipped
// bead — as it did for MR !6746, refusing 25s after that MR had merged and 3s
// before the projection caught up.
//
// Only a POSITIVE, authoritative `merged` acquits. Every failure mode
// (mrLiveUnknown: no token, network error, non-200, unparseable) preserves the
// projection's `opened` verdict, so the guard keeps its two invariants: it never
// blocks on our own read failure, and it never stops blocking because a check
// failed. A live `opened` is simply the projection confirmed.
//
// Cost: this runs ONLY when we are otherwise about to refuse, so a normal close
// (merged MR, or no MR at all) makes no network call.
func mrStillOpenLive(ctx context.Context, mrURL string) bool {
	if checkMRLiveState(ctx, mrURL) == mrLiveMerged {
		// The projection is merely behind GitLab; let the close proceed. No
		// mr_close_override is stamped on this path (the caller only stamps
		// when the guard actually fired), so a correctly-shipped bead is not
		// mislabeled as an override and the reconcile sweep still re-verifies
		// it normally.
		fmt.Fprintf(os.Stderr,
			"note: %s is merged on GitLab but its merge-request bead has not caught up yet; closing anyway\n", mrURL)
		return false
	}
	return true
}

// stampMRCloseOverride sets mr_close_override="true" on a bead being
// deliberately closed with --allow-open-mr despite its MR still being open. The
// gitlab-bridge reconcile sweep (ka-0000000267, direction 2 of ka-0000003953)
// reads this marker and skips re-opening the bead, so a legitimate won't-fix
// close stays closed. Sent as its own PATCH before the close call because the
// /close endpoint's field-merge is additive too but CloseBead here does not
// forward extra fields; a separate UpdateBead keeps the change small and
// leaves the close path untouched.
func stampMRCloseOverride(ctx context.Context, id string) error {
	_, err := beadsClient.UpdateBead(ctx, id, &client.UpdateBeadRequest{
		Fields: []byte(`{"mr_close_override":"true"}`),
	})
	return err
}

// mrIsMerged applies the same merged predicate the bridge and the gb mr
// resolver use (gitlab_sync.go / gardenio gb/mr.go filterMRBeads): merged when
// mr_merged=true OR mr_outcome=merged OR mr_state=merged.
func mrIsMerged(mrFields map[string]any) bool {
	merged, _ := mrFields["mr_merged"].(string)
	outcome, _ := mrFields["mr_outcome"].(string)
	state, _ := mrFields["mr_state"].(string)
	return merged == "true" || outcome == "merged" || state == "merged"
}

// printEscalationNotice prints the standard "escalation created, pending
// operator approval" block for a write that was denied but escalatable. Shared
// by the reason-comment and the close paths so both report escalation the same
// way (matching create.go / update.go).
func printEscalationNotice(esc *client.EscalationCreated) {
	fmt.Printf("Escalation created: %s\n", esc.EscalationID)
	fmt.Printf("  Action: %s on type %s\n", esc.DeniedAction, esc.BeadType)
	fmt.Println("  Status: pending operator approval")
}

// aliasCloseReasonFlags normalizes `--comment` → `--reason` on `kd close` so the
// spelling agents naturally reach for (ka-0000006283 guessed --comment;
// ka-0000007746 guessed --reason) resolves to the same flag instead of a bare
// cobra "unknown flag" usage dump that silently drops the closure text. Mirrors
// aliasUpdateLabelFlags (update.go); attached PER COMMAND, never global, so no
// other command's flags are shadowed. -r stays the shorthand for --reason only.
func aliasCloseReasonFlags(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "comment" {
			name = "reason"
		}
		return pflag.NormalizedName(name)
	})
}

func init() {
	closeCmd.Flags().Bool("escalate", false, "request elevated access if write is denied")
	closeCmd.Flags().StringP("reason", "r", "", "record a closure reason (adds a comment before closing); alias: --comment")
	closeCmd.Flags().Bool("force", false, "operator only: close a task or bug whose linked PR is still open (the daemon refuses it for an agent token; file the merge escalation with --close-on-execute, or gh pr close the PR first)")
	closeCmd.Flags().Bool("allow-open-mr", false, "close even if the bead's linked MR still reads open/unmerged — e.g. the MR was abandoned, or it is already merged and its merge-request bead has not caught up (default: refuse — a closed bead should not claim a fix shipped while its code is off main)")
	aliasCloseReasonFlags(closeCmd)
}
