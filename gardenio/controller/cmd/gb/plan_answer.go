// Commands `gb plan answer` / `gb plan dismiss` let a machine operator (a
// steering plan-gardener) resolve or dismiss a plan's clarifying-question
// decision from the CLI — the headless twin of the browser cockpit's inline
// Answer / Dismiss buttons (beads-viewer plans.go handlePlanResolveDecision /
// handlePlanDismissDecision), authored under step ka-0000000161 / task
// ka-0000003853.
//
// Why these can be daemon-direct when every OTHER `gb plan` write verb POSTs to
// the operator-gated viewer (and 401s from an agent pod against an OIDC-gated
// viewer, ka-0000006389): the resolve/dismiss WRITE was ALREADY daemon-owned.
// The viewer handlers are thin OIDC-gate + plan-scope + SSE-re-render wrappers
// that delegate to daemon.ResolveDecision / daemon.CancelDecision
// (POST /v1/decisions/{id}/resolve|cancel — no OIDC gate; kbeads
// http_decisions.go). So a gardener can already answer/dismiss a plan question
// today via the generic `gb decision respond` / `gb decision cancel` under
// BEADS_AUTH_TOKEN. These verbs add nothing to that capability — they are
// ERGONOMIC SUGAR that (1) mirrors the plan-scoped `plan/{id}/decisions/…`
// browser URL for symmetry with the other `gb plan` write verbs, (2) adds the
// same client-side plan-membership guard the viewer's loadPlanDecision applies
// (the decision's parent-child edge must point at THIS plan epic), and (3)
// routes through daemon.ResolveDecision — i.e. POST /resolve — so a headless
// answer fires the SAME DecisionAnswer event + privileged-dibs rebind +
// report_required path a browser answer does. The generic `gb decision respond`
// closes the bead via daemon.CloseBead, which the bridge still picks up (it
// backfills `chosen` on the beads.closed event) but which skips the daemon-side
// DecisionAnswer/rebind — see the runbook's "Answering plan clarifying-questions
// from the CLI" section for the full nuance.
//
// Since gd-Du6wHclx4g the daemon refuses POST /v1/decisions/{id}/resolve|cancel
// from the shared static token and from per-agent fleet tokens (an agent could
// otherwise answer its own approval gate and name a human in X-Operated-By), so
// from an agent pod these verbs now 403; they work from a human session (OIDC /
// edge) or a bridge token.
//
// Unlike add-step/complete/reopen these verbs take NO --viewer-url: they talk
// only to the daemon via the *beadsapi.Client `gb` already carries, exactly like
// the `gb plan show` read path (plan_read.go).
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var (
	planAnswerSelect  string
	planAnswerText    string
	planDismissReason string
)

var planAnswerCmd = &cobra.Command{
	Use:   "answer <plan-epic-id> <decision-id>",
	Short: "Answer a plan's clarifying-question decision, daemon-direct (no viewer)",
	Long: `Resolve one clarifying-question decision of the plan epic <plan-epic-id> by
selecting an option or writing a free-text response — the headless twin of the
browser cockpit's inline "Answer" button.

Unlike the other 'gb plan' write verbs (create / add-step / add-phase /
complete / reopen), this talks ONLY to the beads daemon — no --viewer-url, no
operator header — so it works with just BEADS_AUTH_TOKEN, the exact environment
where the viewer-POSTing verbs 401 against an OIDC-gated viewer.
The resolve WRITE was always daemon-owned (POST /v1/decisions/{id}/resolve),
so a gardener can already do this with the generic 'gb decision respond'; this
verb is plan-scoped sugar that also enforces plan membership and fires the
daemon's DecisionAnswer + report_required path (which 'gb decision respond'
skips — see the plan-authoring runbook for the nuance).

The decision MUST be an open clarifying-question decision that belongs to THIS
plan (a direct parent-child child of the plan epic), mirroring the viewer's
loadPlanDecision scope guard. Provide --select <option-id> and/or --text.

Examples:
  gb plan answer ka-plan123 ka-dec456 --select jwt
  gb plan answer ka-plan123 ka-dec456 --text "Prefer server-side sessions"`,
	Args: cobra.ExactArgs(2),
	RunE: runPlanAnswer,
}

var planDismissCmd = &cobra.Command{
	Use:   "dismiss <plan-epic-id> <decision-id>",
	Short: "Dismiss a plan's clarifying-question decision, daemon-direct (no viewer)",
	Long: `Dismiss (cancel) one clarifying-question decision of the plan epic
<plan-epic-id> — the headless twin of the browser cockpit's "Dismiss" control.

Like 'gb plan answer', this talks ONLY to the daemon (POST
/v1/decisions/{id}/cancel), needs just BEADS_AUTH_TOKEN, and enforces that the
decision is an open clarifying-question belonging to THIS plan. Dismiss sets
chosen="dismissed" so the Slack bridge does NOT respawn the plan assistant
(unlike answer) — use it to clear a stale or no-longer-relevant question.

Example:
  gb plan dismiss ka-plan123 ka-dec456 --reason "superseded by the narrative rewrite"`,
	Args: cobra.ExactArgs(2),
	RunE: runPlanDismiss,
}

func init() {
	planCmd.AddCommand(planAnswerCmd)
	planAnswerCmd.Flags().StringVar(&planAnswerSelect, "select", "", "option id to select")
	planAnswerCmd.Flags().StringVar(&planAnswerText, "text", "", "free-text response")
	planAnswerCmd.SilenceUsage = true

	planCmd.AddCommand(planDismissCmd)
	planDismissCmd.Flags().StringVar(&planDismissReason, "reason", "", "why the question is being dismissed (recorded on the decision)")
	planDismissCmd.SilenceUsage = true
}

func runPlanAnswer(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	decisionID := strings.TrimSpace(args[1])
	if planID == "" || decisionID == "" {
		return errors.New("plan epic id and decision id are required")
	}
	if strings.TrimSpace(planAnswerSelect) == "" && strings.TrimSpace(planAnswerText) == "" {
		return errors.New("--select or --text is required")
	}

	if err := requirePlanDecisionMember(cmd.Context(), daemon, planID, decisionID); err != nil {
		return err
	}

	if err := daemon.ResolveDecision(cmd.Context(), decisionID, beadsapi.ResolveDecisionRequest{
		SelectedOption: strings.TrimSpace(planAnswerSelect),
		ResponseText:   strings.TrimSpace(planAnswerText),
		// RespondedBy is derived server-side from the authenticated token
		// (kbeads decisionAttribution — client-supplied values are ignored to
		// prevent audit spoofing), so we intentionally leave it empty.
	}); err != nil {
		return fmt.Errorf("answering plan decision %s: %w", decisionID, err)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"plan":     planID,
			"decision": decisionID,
			"status":   "resolved",
		})
		return nil
	}
	fmt.Printf("answered decision %s on plan %s\n", decisionID, planID)
	return nil
}

func runPlanDismiss(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	decisionID := strings.TrimSpace(args[1])
	if planID == "" || decisionID == "" {
		return errors.New("plan epic id and decision id are required")
	}

	if err := requirePlanDecisionMember(cmd.Context(), daemon, planID, decisionID); err != nil {
		return err
	}

	// canceledBy is derived server-side from the token; actor is passed only as
	// the no-auth dev/e2e fallback the daemon accepts (decisionAttribution).
	if err := daemon.CancelDecision(cmd.Context(), decisionID, strings.TrimSpace(planDismissReason), actor); err != nil {
		return fmt.Errorf("dismissing plan decision %s: %w", decisionID, err)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"plan":     planID,
			"decision": decisionID,
			"status":   "dismissed",
		})
		return nil
	}
	fmt.Printf("dismissed decision %s on plan %s\n", decisionID, planID)
	return nil
}

// planDecisionMemberReader is the narrow daemon surface the membership guard
// needs — one GetBead (to confirm the target is an open decision) and one
// forward-deps walk (to confirm it is parented onto the plan epic). Split out
// as an interface so requirePlanDecisionMember is unit-testable with a fake.
type planDecisionMemberReader interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
}

var _ planDecisionMemberReader = (*beadsapi.Client)(nil)

// requirePlanDecisionMember validates that decisionID is an open
// clarifying-question decision that belongs to plan epicID before a resolve /
// dismiss acts on it. It ports the viewer's loadPlanDecision scope guard
// (cmd/beads-viewer/plans.go): the bead must exist, be type=decision, still be
// open, and carry a forward parent-child edge INTO epicID — the same membership
// the render path lists (clarifying-question decisions auto-parent straight onto
// the plan epic). This keeps `gb plan answer/dismiss` from acting on a decision
// outside the addressed plan, matching the browser's plan-scoped routes; it is a
// scoping/attribution guard, not a privilege control (the daemon endpoints carry
// no OIDC gate either way).
func requirePlanDecisionMember(ctx context.Context, r planDecisionMemberReader, epicID, decisionID string) error {
	dec, err := r.GetBead(ctx, decisionID)
	if err != nil {
		return fmt.Errorf("loading decision %s: %w", decisionID, err)
	}
	if dec == nil {
		return fmt.Errorf("decision %s not found", decisionID)
	}
	if dec.Type != "decision" {
		return fmt.Errorf("%s is a %s, not a decision", decisionID, dec.Type)
	}
	if dec.Status != "open" {
		return fmt.Errorf("decision %s is already resolved (status=%s)", decisionID, dec.Status)
	}

	deps, err := r.GetDependencies(ctx, decisionID)
	if err != nil {
		return fmt.Errorf("verifying decision %s belongs to plan %s: %w", decisionID, epicID, err)
	}
	for _, d := range deps {
		if d.Type == "parent-child" && d.DependsOnID == epicID {
			return nil
		}
	}
	return fmt.Errorf("decision %s is not a clarifying-question of plan %s (no parent-child edge into the plan epic)", decisionID, epicID)
}
