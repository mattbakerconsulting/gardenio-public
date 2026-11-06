// Command `gb plan` drives the beads-viewer /plans surface from a machine
// operator (a steering gardener) rather than a human clicking in the browser.
//
// ORIGINATION IS DAEMON-DIRECT (epic ka-0000005932, MR !5910 + task
// ka-0000004615). Every `gb plan` WRITE verb — create / add-step / add-phase /
// reorder-phase / complete / reopen — composes straight against the beads
// daemon through the helpers in plan_write.go, using only the *beadsapi.Client
// `gb` already carries (main.go's `daemon`) and BEADS_AUTH_TOKEN. There is no
// viewer URL, no X-Auth-Request-Email header, and no 303-is-error edge dance:
// the earlier viewer-POST path 401'd from an agent pod against an OIDC-gated
// viewer (ka-0000006389), and moving the writes onto the daemon closed that
// blocker for good. A plan is a plain bead graph (an epic + parent-child
// children + a phase_order/step_order overlay in fields), so each verb reduces
// to daemon primitives the client already exposes.
//
// REACTIVE COLLABORATION — INTERIM SEAM. The SHIPPED reactive-collaboration
// contract (epic ka-0000004813, MR !5223) — nudge a live plan agent sub-second,
// wake a parked one, or spawn a fresh one on an add-step, behind a 5s per-plan
// debounce — is NOT yet driven from this daemon path. Only the WRITE moves in
// P2; re-homing the nudge is the sibling task ka-0000007841 (an interim
// daemon→viewer signal, or the viewer observing the new step edge, keeps the
// nudge firing until P3). So `gb plan add-step` here creates + links the step
// but does NOT itself fire the nudge — the message deliberately makes no such
// claim.
//
// Steering-loop epic ka-0000000048, child C2 ka-0000004843.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var (
	planAddStepTitle string
	planAddStepPhase string
)

// planCmdWriter resolves the planDaemonWriter the `gb plan` WRITE verbs execute
// against. In production it returns the daemon client gb built in
// PersistentPreRunE (the same `daemon` global runPlanShow reads through for the
// READ path; *beadsapi.Client satisfies planDaemonWriter — asserted in
// plan_write.go). It is a package var so a test can substitute an in-memory fake
// (fakePlanWriter) and exercise a RunE body end-to-end with no live daemon.
var planCmdWriter = func() planDaemonWriter { return daemon }

var planCmd = &cobra.Command{
	Use:     "plan",
	Short:   "Steer a /plans plan (originate + edit steps as a machine operator, daemon-direct)",
	GroupID: "orchestration",
	Long: `Drive the beads-viewer /plans surface the way an operator does in the browser,
but straight against the beads daemon — no viewer URL, no browser session.

Every write verb (create / add-step / add-phase / reorder-phase / complete /
reopen) composes against the daemon with only BEADS_AUTH_TOKEN, so it works from
an agent pod against the default OIDC-gated deployment (where the earlier
viewer-POST path 401'd — ka-0000006389). Each write is stamped on behalf of the
--operated-by HUMAN operator — oidc:<email> (a human at a browser) OR slack:U…
(the human who spawned a steering gardener, so an AGENT GARDENER can drive these
verbs with real human attribution; a svc:/machine id is rejected — ka-0000005347).

'gardener' binds the plan's single watchdog/steering plan-gardener (whose
iterations spawn in the plan-gardener role — one agent both watches the
plan and answers operator messages) — see 'gb plan gardener --help'.

'show' prints the plan's resolved timeline (phases + steps), also daemon-direct.

Note: the reactive plan-agent collaboration that a browser "Add step" fires is
NOT yet driven from this daemon path (interim seam ka-0000007841) — 'add-step'
creates and links the step but does not itself nudge/wake/spawn the plan agent.`,
}

var planAddStepCmd = &cobra.Command{
	Use:   "add-step <plan-epic-id>",
	Short: "Append a step (an issue child) to a plan",
	Long: `Append a step (an issue child) to the plan epic <plan-epic-id>: mint a
task, link it parent-child under the plan, and inherit the plan's project label.

By default the step is a flat direct child of the plan. Pass --phase <phase-epic-id>
to parent it to one of the plan's PHASES instead (a phase is a direct-child epic
listed in the plan's phase_order — see 'gb plan add-phase'); the step then renders
inline under that phase. The phase MUST belong to this plan or the add is rejected.

Note: this does NOT fire the browser's reactive plan-agent collaboration
(nudge/wake/spawn) — that is re-homed by the sibling task ka-0000007841.`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanAddStep,
}

var planCompleteCmd = &cobra.Command{
	Use:   "complete <plan-epic-id>",
	Short: "Mark a plan complete (close the plan + disable its gardener), daemon-direct",
	Long: `Mark the plan epic <plan-epic-id> complete: DISABLE the bound gardener
(so its loop stops firing), stamp plan_state=complete, and CLOSE the plan epic
(status=closed). The gardener is disabled FIRST so even a later failure leaves the
loop quiet.

The plan's bound gardener runs this itself once the plan's three close criteria
hold, and posts the evidence (owner ruling 1 on gd-7qUpu15BBK, which reverses
ka-0000004595's propose-and-confirm); an operator may run it too.
Use 'gb plan reopen <id>' to undo it (status=open, plan_state=building).`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanComplete,
}

var planAbandonCmd = &cobra.Command{
	Use:   "abandon <plan-epic-id>",
	Short: "Abandon a plan (close the plan + disable its gardener, plan_state=abandoned), daemon-direct",
	Long: `Abandon the plan epic <plan-epic-id> — the DISTINCT terminal "cheap exit"
for a plan dropped WITHOUT finishing its work: DISABLE the bound gardener (so its
loop stops firing), stamp plan_state=abandoned, and CLOSE the plan epic
(status=closed). The gardener is disabled FIRST so even a later failure leaves the
loop quiet.

Structurally identical to 'gb plan complete' but records the plan as GIVEN UP ON
rather than DONE, so /plans can render the two terminal states distinctly. A plan
can be abandoned from any non-terminal working state (draft/ready/building) — no
need to walk it forward to building first. Leftover open child steps are expected
(abandoning is exactly "give up on the leftover work") and never block the exit.
Use 'gb plan reopen <id>' to undo it (status=open, plan_state=building).`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanAbandon,
}

var planReopenCmd = &cobra.Command{
	Use:   "reopen <plan-epic-id>",
	Short: "Reopen a completed or abandoned plan (status=open, plan_state=building), daemon-direct",
	Long: `Reopen the terminal plan epic <plan-epic-id> — the inverse of 'gb plan
complete' / 'gb plan abandon': reopen the epic (status=open) and restore
plan_state=building so work can resume. The bound gardener is left PAUSED; resume it
deliberately (the cockpit's Resume lever or by re-enabling the trigger). Reopening
never silently restarts the loop. Both terminal states (complete, abandoned) reopen
to building — the single back-edge.`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanReopen,
}

func init() {
	planCmd.AddCommand(planAddStepCmd)
	planAddStepCmd.Flags().StringVar(&planAddStepTitle, "title", "", "step title (required)")
	planAddStepCmd.Flags().StringVar(&planAddStepPhase, "phase", "",
		"phase epic id to parent the step under (a phase of this plan; default: flat direct child of the plan)")
	planAddStepCmd.SilenceUsage = true

	planCmd.AddCommand(planCompleteCmd)
	planCompleteCmd.SilenceUsage = true

	planCmd.AddCommand(planAbandonCmd)
	planAbandonCmd.SilenceUsage = true

	planCmd.AddCommand(planReopenCmd)
	planReopenCmd.SilenceUsage = true
}

// runPlanComplete marks a plan complete daemon-direct on behalf of the
// --operated-by operator.
func runPlanComplete(cmd *cobra.Command, args []string) error {
	return runPlanLifecycleVerb(cmd, args, "complete")
}

// runPlanAbandon marks a plan abandoned daemon-direct on behalf of the
// --operated-by operator.
func runPlanAbandon(cmd *cobra.Command, args []string) error {
	return runPlanLifecycleVerb(cmd, args, "abandon")
}

// runPlanReopen reopens a completed or abandoned plan daemon-direct.
func runPlanReopen(cmd *cobra.Command, args []string) error {
	return runPlanLifecycleVerb(cmd, args, "reopen")
}

// runPlanLifecycleVerb is the shared body behind 'gb plan complete' and 'gb plan
// reopen': validate the operator, call the matching daemon-direct helper
// (completePlan / reopenPlan), and print the resulting {plan_state,status}.
func runPlanLifecycleVerb(cmd *cobra.Command, args []string, verb string) error {
	planID := strings.TrimSpace(args[0])
	if planID == "" {
		return errors.New("plan epic id is required")
	}
	email, err := operatorEmailForViewer(operatedBy)
	if err != nil {
		return err
	}

	var res planWriteResult
	switch verb {
	case "complete":
		res, err = completePlan(cmd.Context(), planCmdWriter(), planID, email)
	case "abandon":
		res, err = abandonPlan(cmd.Context(), planCmdWriter(), planID, email)
	case "reopen":
		res, err = reopenPlan(cmd.Context(), planCmdWriter(), planID, email)
	default:
		return fmt.Errorf("unknown lifecycle verb %q", verb)
	}
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":         planID,
			"plan_state": res.PlanState,
			"status":     res.Status,
			"operator":   canonicalPlanOperator(email),
		})
		return nil
	}
	action := "completed"
	switch verb {
	case "abandon":
		action = "abandoned"
	case "reopen":
		action = "reopened"
	}
	fmt.Printf("plan %s %s (plan_state=%s, status=%s)\n", planID, action, res.PlanState, res.Status)
	return nil
}

func runPlanAddStep(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	if planID == "" {
		return errors.New("plan epic id is required")
	}
	title := strings.TrimSpace(planAddStepTitle)
	if title == "" {
		return errors.New("--title is required")
	}
	email, err := operatorEmailForViewer(operatedBy)
	if err != nil {
		return err
	}

	phase := strings.TrimSpace(planAddStepPhase)
	res, err := addPlanStep(cmd.Context(), planCmdWriter(), planID, title, phase, email)
	if err != nil {
		return err
	}

	if jsonOutput {
		out := map[string]any{
			"id":       res.ID,
			"plan":     planID,
			"title":    title,
			"operator": canonicalPlanOperator(email),
			"status":   "ok",
		}
		if res.Phase != "" {
			out["phase"] = res.Phase
		}
		printJSON(out)
		return nil
	}
	if res.Phase != "" {
		fmt.Printf("added step %s to plan %s under phase %s\n", res.ID, planID, res.Phase)
	} else {
		fmt.Printf("added step %s to plan %s\n", res.ID, planID)
	}
	return nil
}

// operatorEmailForViewer validates the HUMAN-operator id every `gb plan` write
// runs on behalf of and returns the stamp-ready operator token:
//
//   - oidc:<local>@<domain>  → the bare email (back-compat: this token is also
//     used verbatim as the created_by field + idempotency-key actor, which have
//     always carried a bare email for the oidc path).
//   - slack:U<alnum>         → the full "slack:U…" id.
//
// Both shapes are first-class HUMAN operators in the kbeads audit resolver
// (isHumanOperator, audit.go) — slack:U… resolves to the human who spawned the
// calling agent. Accepting slack:U… here is what lets an AGENT GARDENER (whose
// $BEADS_OPERATED_BY is slack:U…, never an oidc email) drive the ergonomic,
// atomic, membership-validated `gb plan` write verbs, attributed to the real
// human behind it — WITHOUT a machine/service (svc:) operator identity. A svc:
// sink would reverse the deliberate sentinel collapse (epic ka-0000004835) and
// break the "operator is always a human" invariant (ka-0000006809); slack:U…
// upholds that invariant, so it is the sanctioned agent route (design ka-0000005347).
//
// Rejects everything else — bare agent names, sentinels (system:*, svc:*), and
// malformed ids — locally, with an actionable message rather than a downstream
// 400. The daemon then stamps the canonical form via withPlanOperator /
// canonicalPlanOperator.
//
// (The name predates the viewer-POST path this replaced; the function now feeds
// the daemon-direct writer, not an X-Auth-Request-Email header — which is why
// the oidc:-only "requires an email" gate is no longer load-bearing.)
func operatorEmailForViewer(op string) (string, error) {
	v := strings.TrimSpace(op)
	if v == "" {
		return "", errors.New("no operator identity: set --operated-by oidc:<email> / slack:U… or $BEADS_OPERATED_BY (a steering gardener drives plan writes on behalf of a human operator)")
	}
	// The daemon caps the whole operator at 128 bytes (kbeads
	// sanitizeOperatedByHeader, ka-0000006809). Reject locally so an over-long id
	// surfaces here rather than as an opaque downstream 502.
	if len(v) > 128 {
		return "", fmt.Errorf("operated-by %q is too long (>128 bytes) for the operator header", v)
	}
	// slack:U<alnum> — the human who spawned the calling agent. Mirror kbeads
	// extractSlackUserID (auto_inherit.go) EXACTLY: 'U'-led, ≤64 chars after the
	// prefix, all [0-9A-Z]. Any divergence lets an id pass that the daemon 400s.
	const slackPrefix = "slack:"
	if strings.HasPrefix(v, slackPrefix) {
		if !validSlackUserID(v[len(slackPrefix):]) {
			return "", fmt.Errorf("operated-by %q is not a well-formed slack:U<id> (U-led, [0-9A-Z], ≤64 chars)", v)
		}
		return v, nil
	}
	const oidcPrefix = "oidc:"
	if !strings.HasPrefix(v, oidcPrefix) {
		return "", fmt.Errorf("operated-by %q is not a human operator id: gb plan writes are stamped on behalf of a human (oidc:<email> or slack:U…); svc:/system:/agent ids are rejected so a machine write can never masquerade as the operator", v)
	}
	email := strings.TrimSpace(v[len(oidcPrefix):])
	// Mirror kbeads isHumanOperator (audit.go) EXACTLY: an @ with a non-empty
	// local part and a dotted domain that neither starts nor ends with a dot.
	// Any divergence lets an email pass here that the daemon then rejects.
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return "", fmt.Errorf("operated-by %q does not carry a well-formed email after the oidc: prefix", v)
	}
	domain := email[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", fmt.Errorf("operated-by %q does not carry a well-formed email after the oidc: prefix", v)
	}
	return email, nil
}

// validSlackUserID mirrors kbeads extractSlackUserID (auto_inherit.go) EXACTLY
// for the id part after the "slack:" prefix: non-empty, 'U'-led, ≤64 chars, and
// every character a digit or uppercase letter. Kept in lockstep so an id this
// gate accepts is one the daemon audit resolver also accepts.
func validSlackUserID(rest string) bool {
	if rest == "" || rest[0] != 'U' || len(rest) > 64 {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}
