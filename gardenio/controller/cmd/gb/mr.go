package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

var (
	mrGitLabURL   string
	mrGitLabToken string

	mrListOwner     string
	mrListSpawnedBy string

	mrStatusBead string
	mrStatusMR   string
)

// resolveMRStatusTarget picks the single bead-id / MR-url `gb mr status` acts on
// from the positional arg and the --bead/--mr aliases (bug ka-0000006140).
//
// All three forms are equivalent and feed the same resolver (resolveMRURL), which
// already accepts either a gd-* bead id or a raw MR URL — so the flags are pure
// aliases for the positional arg rather than a second, stricter contract. The
// positional form keeps working unchanged (back-compat).
//
// Exactly one form must be supplied; naming two is an error rather than a silent
// precedence rule, so `--bead X ka-Y` can never quietly report on the wrong MR.
func resolveMRStatusTarget(args []string, beadFlag, mrFlag string) (string, error) {
	var (
		targets []string
		sources []string
	)
	if len(args) > 0 && args[0] != "" {
		targets = append(targets, args[0])
		sources = append(sources, fmt.Sprintf("positional %q", args[0]))
	}
	if beadFlag != "" {
		targets = append(targets, beadFlag)
		sources = append(sources, fmt.Sprintf("--bead %q", beadFlag))
	}
	if mrFlag != "" {
		targets = append(targets, mrFlag)
		sources = append(sources, fmt.Sprintf("--mr %q", mrFlag))
	}

	switch len(targets) {
	case 0:
		return "", fmt.Errorf("a bead id or MR URL is required: pass it positionally " +
			"(gb mr status <bead-id-or-mr-url>) or via --bead <id> / --mr <url>")
	case 1:
		return targets[0], nil
	default:
		return "", fmt.Errorf("specify the target exactly once, got %d: %s",
			len(targets), strings.Join(sources, ", "))
	}
}

// mrSkipLabel is the opt-out label the gardener-delivery trigger filters on.
// An MR bead carrying this label is skipped by the gardener's bead_event wake
// and excluded from its cron preflight's open-MR count, so docs-only / human /
// not-its-lane MRs don't keep waking the gardener. See bead ka-0000001456.
const mrSkipLabel = "gardener-delivery-skip"

var mrCmd = &cobra.Command{
	Use:     "mr",
	Short:   "GitLab merge request commands",
	GroupID: "orchestration",
}

var mrStatusCmd = &cobra.Command{
	Use:   "status [<bead-id-or-mr-url>]",
	Short: "Show MR state, pipeline status, and merge details",
	// --bead/--mr alias the positional target because sibling subcommands take
	// named flags (gb mr list --owner/--spawned-by), so a --bead guess is a
	// reasonable affordance expectation that used to die on "unknown flag"
	// (bug ka-0000006140).
	Long: `Shows MR state, pipeline status, and merge details.

The target is a bead id or a raw GitLab MR URL, given either positionally or via
the equivalent --bead / --mr aliases. Use exactly one form per invocation:

  gb mr status ka-ABC123
  gb mr status --bead ka-ABC123
  gb mr status https://gitlab.com/org/repo/-/merge_requests/42
  gb mr status --mr https://gitlab.com/org/repo/-/merge_requests/42`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMRStatus,
}

var mrListCmd = &cobra.Command{
	Use:   "list",
	Short: "List beads that have an MR URL and their merge status",
	RunE:  runMRList,
}

var mrLinkCmd = &cobra.Command{
	Use:   "link <bead-id> <mr-url>",
	Short: "Set mr_url on a bead to enable GitLab/GitHub notification tracking",
	Long: `Links a task bead to a GitLab merge request (or a GitHub pull request) by
setting the mr_url field. A GitHub PR URL also sets forge=github so the
github-bridge owns it. Once linked, the gitlab-bridge will automatically:
  - Track MR state, pipeline status, and merge events
  - Forward review comments as nudges to the assigned agent
  - Update bead fields (mr_state, mr_merged, mr_pipeline_status, etc.)

Run this after creating a GitLab MR for any bead.

With --upstream, the <mr-url> is a human-owned MR on the REAL monorepo (a
peeled slice; E-PROV FW-5). Instead of setting mr_url on the peel-request
bead, this creates a dedicated read-only type=merge-request MIRROR bead keyed
by the upstream MR URL (host preserved, so a dark !N and an upstream !N never
collide) and links the peel-request bead to it via a 'produces' dep. The
mirror bead carries the canonical upstream-mirror labels (upstream:true,
gardener-delivery-skip, external-mirror:true) so no gardener ever drives the
human-owned MR and beads-viewer renders it as external — honoring the
read-only-mirror invariant: the bead only DISPLAYS state
pulled FROM upstream, nothing is ever pushed outbound.`,
	Args: cobra.ExactArgs(2),
	RunE: runMRLink,
}

// mrLinkUpstream selects the upstream-mirror creation path for `gb mr link`.
var mrLinkUpstream bool

var mrRelevanceTicket string

var mrRelevanceGateCmd = &cobra.Command{
	Use:   "relevance-gate <bead-id-or-mr-url> --ticket <KEY>",
	Short: "Evaluate the MR Relevance Gardener TV=0 (ticket-resolved) close gate",
	Long: `Decides whether the MR Relevance Gardener may fire its TV=0
(ticket-resolved) close gate on an MR whose linked ticket has resolved.

The guard (bug ka-0000002060): a ticket can resolve via a SIBLING MR on a
different branch (e.g. a hotfix merged to a release branch) while an MR
targeting main or a release branch (*.x) still needs to ship the same fix.
Auto-closing the protected-target MR in that case loses the unshipped fix.

This command fetches the MR's target branch and the sibling MRs linked to the
same ticket (via GitLab search on the ticket key), then runs the tested gate
logic in internal/bridge. Exit code 0 = may close; exit code 3 = blocked
(leave open for a human). Use --json for machine-readable output.`,
	Args: cobra.ExactArgs(1),
	RunE: runMRRelevanceGate,
}

var mrDispatchGateCmd = &cobra.Command{
	Use:   "dispatch-gate <bead-id-or-mr-url>",
	Short: "Decide whether gardener-delivery may spawn a review-merge crew (suppress if parked on an operator merge-gate)",
	Long: `Deterministic suppress-dispatch gate for the gardener-delivery MR walk
(bug ka-0000004390).

gardener-delivery walks open MRs every cycle and dispatches an Opus review-merge
crew per eligible orphan. When an MR is green but its linked TASK is parked on
an OPEN operator merge-gate — a 'gb decision' (merge/hold fork) and/or a
'gb escalation' (raw approve+merge ask) blocking the task — the sole remaining
action is a HUMAN merge/hold call no crew can take. Spawning a fresh Opus crew
each cycle just re-reaches the same "held for operator" verdict (6+ redundant
crews in ~18h on MR !6218).

This resolves the MR to its linked task, reads the task's forward blocking-dep
chain (the exact edges 'gb ready' / 'kd list --no-blockers' key on), and:

  exit 0 = MAY dispatch (task not blocked by an open decision/escalation), or
  exit 3 = DO NOT dispatch — comment-only "no new crew (dedup)"; the existing
           decision/escalation governs.

When an MR stays parked past ~6 cycles (~2h at the 20m cadence) with no operator
action, this posts ONE aging nudge (bug ka-0000007496) as a comment on the SAME
existing decision/escalation bead — naming the MR URL, the decision id, and how
long it has been gated — then stamps a one-shot marker so the nudge never
repeats. It does NOT file a new decision and does NOT change the exit-3 suppress
verdict: a persistently-gated MR is still not re-crewed, the operator just gets
one ping that their call is the only thing missing. --json reports the nudge
under a "nudge" key (handle_id, gate_age, posted).

A further suppress signal is a per-MR dispatch COOLDOWN (task ka-0000001101): even
with no operator gate, a green orphan whose head SHA has NOT moved since the last
review-merge crew and was dispatched within the cooldown window (~50m, ≈2-3 cycles
at the 20m cadence) is NOT re-dispatched — nothing changed to re-review. A new head
SHA or an expired window re-dispatches as normal. On a may-dispatch the gate stamps
the MR bead with the head SHA + time so the next cycle measures the window from now.
Pass --no-cooldown to force a re-review of an unchanged MR (operator-gate
suppression still applies).

Fails OPEN (exit 0) on every uncertainty (no linked bead, unresolved blocker,
unreadable head SHA) so a flaky read can never strand a genuinely-actionable MR.
Use --json for machine-readable output.`,
	Args: cobra.ExactArgs(1),
	RunE: runMRDispatchGate,
}

var mrSkipCmd = &cobra.Command{
	Use:   "skip <bead-id-or-mr-url>",
	Short: "Mark an MR so the gardener-delivery gardener ignores it",
	Long: `Adds the ` + mrSkipLabel + ` label to an MR's merge-request bead.

The gardener-delivery gardener (trigger ka-0000001510) over-fires when it wakes
on every MR pipeline event and re-runs its cron floor whenever any work is
open. Labelling an MR with ` + mrSkipLabel + ` removes it from both:

  - the gardener's bead_event wake (label:` + mrSkipLabel + ` op:absent), and
  - its cron preflight's open-MR count (exclude_labels).

Use it for MRs the gardener should never drive: docs-only changes, human-owned
MRs, or MRs another gardener's crew owns. Reverse with 'gb mr unskip'.

The arg resolves to the merge-request bead either directly (a gd-* MR bead id)
or by mr_url (a gd-* task bead with mr_url set, or a raw GitLab MR URL).`,
	Args: cobra.ExactArgs(1),
	RunE: runMRSkip,
}

var mrUnskipCmd = &cobra.Command{
	Use:   "unskip <bead-id-or-mr-url>",
	Short: "Remove the gardener-delivery-skip label from an MR",
	Long:  `Removes the ` + mrSkipLabel + ` label so the gardener-delivery gardener drives the MR again. Inverse of 'gb mr skip'.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runMRUnskip,
}

var mrVerifyCleanupCmd = &cobra.Command{
	Use:   "verify-cleanup <bead-id-or-mr-url>",
	Short: "Verify a merged MR's source branch was actually deleted on origin",
	Long: `Confirms an MR's source branch is gone from origin AFTER a merge.

` + "`glab mr merge --remove-source-branch`" + ` reports "✓ Merged!" with a zero
exit even when the branch deletion is silently refused — a protected-branch
pattern can deny a Developer-role bot the delete, and the flag swallows that
denial (bug ka-0000004927). A gardener that trusts that output claims a cleanup
it never checked. This command asks GitLab directly whether the branch still
exists, so cleanup becomes verified instead of assumed.

The target is a bead id or a raw GitLab MR URL. The source branch is read from
the live MR unless overridden with --branch (useful for an already-deleted MR
whose source_branch GitLab no longer returns).

Exit codes let a gardener's shell branch on the outcome:
  0 = branch is gone — cleanup verified.
  3 = branch SURVIVED the merge — report honestly; it was NOT cleaned up.
  1 = could not verify (bad target, API/auth/network error).`,
	Args: cobra.ExactArgs(1),
	RunE: runMRVerifyCleanup,
}

var mrEnvCmd = &cobra.Command{
	Use:   "env <bead-id-or-mr-url>",
	Short: "Resolve deploy-mr environment URLs from CI artifacts",
	Long: `Finds the deploy-mr job in the MR's latest pipeline and downloads its
.urls/environment.env artifact to extract the actual deployment URLs.

This is the authoritative source for MR deployment URLs — it reflects what
the deploy job actually created, avoiding issues with local slug computation.

Output variables: TENANT_ID, UNITY_URL, HARMONY_URL, BASE_URL, KEYCLOAK_URL, etc.`,
	Args: cobra.ExactArgs(1),
	RunE: runMREnv,
}

func init() {
	mrCmd.PersistentFlags().StringVar(&mrGitLabURL, "gitlab-url", os.Getenv("GITLAB_BASE_URL"), "GitLab base URL")
	// Default empty on purpose. The token is resolved at client-build time by
	// resolveMRGitLabToken (flag -> GITLAB_API_TOKEN -> GITLAB_TOKEN). Baking
	// os.Getenv("GITLAB_API_TOKEN") into the flag default would (a) miss
	// GITLAB_TOKEN — the token agent pods actually carry and the one glab
	// resolves (bug ka-0000003680: agent pods get GITLAB_TOKEN, not
	// GITLAB_API_TOKEN, so gb mr fell back to no token and 404'd the monorepo),
	// and (b) print the secret in `gb mr --help` whenever the env var is set.
	mrCmd.PersistentFlags().StringVar(&mrGitLabToken, "gitlab-token", "", "GitLab API token (default: $GITLAB_API_TOKEN, else $GITLAB_TOKEN — the token glab uses)")

	mrEnvCmd.Flags().Bool("export", false, "Output as export VAR=value (shell-eval friendly)") // OK: "eval" is in help text, not code execution

	// --bead/--mr are aliases for the positional target (bug ka-0000006140).
	mrStatusCmd.Flags().StringVar(&mrStatusBead, "bead", "", "Bead id to report on (alias for the positional arg)")
	mrStatusCmd.Flags().StringVar(&mrStatusMR, "mr", "", "GitLab MR URL to report on (alias for the positional arg)")
	mrStatusCmd.MarkFlagsMutuallyExclusive("bead", "mr")

	mrListCmd.Flags().StringVar(&mrListOwner, "owner", "", "Filter to MRs whose bead is assigned to or was spawned by this agent")
	mrListCmd.Flags().StringVar(&mrListSpawnedBy, "spawned-by", "", "Filter to MRs whose bead has spawned_by=<agent>")

	mrLinkCmd.Flags().BoolVar(&mrLinkUpstream, "upstream", false, "Create a read-only upstream-MR mirror bead for a human-owned MR on the real monorepo (E-PROV FW-5) instead of setting mr_url on the bead")

	mrRelevanceGateCmd.Flags().StringVar(&mrRelevanceTicket, "ticket", "", "Ticket key (e.g. PE-491) the MR is linked to; siblings are found by searching MRs for this key")

	mrVerifyCleanupCmd.Flags().StringVar(&mrVerifyCleanupBranch, "branch", "", "Source branch to check (default: the MR's live source_branch)")

	mrDispatchGateCmd.Flags().BoolVar(&mrDispatchGateNoCooldown, "no-cooldown", false, "Skip the per-MR dispatch cooldown (force re-review of an unchanged MR); operator-gate suppression still applies")

	mrCmd.AddCommand(mrStatusCmd)
	mrCmd.AddCommand(mrListCmd)
	mrCmd.AddCommand(mrLinkCmd)
	mrCmd.AddCommand(mrEnvCmd)
	mrCmd.AddCommand(mrRelevanceGateCmd)
	mrCmd.AddCommand(mrDispatchGateCmd)
	mrCmd.AddCommand(mrSkipCmd)
	mrCmd.AddCommand(mrUnskipCmd)
	mrCmd.AddCommand(mrVerifyCleanupCmd)
}

// mrVerifyCleanupBranch overrides the source branch `gb mr verify-cleanup`
// checks, for the case where the MR no longer reports its source_branch.
var mrVerifyCleanupBranch string

// resolveMRBeadID resolves a CLI arg to the merge-request bead id whose label
// set the gardener-delivery trigger matches against. Accepts:
//   - a gd-* bead id that is itself a merge-request bead → returned as-is;
//   - a gd-* task bead carrying mr_url → the MR bead with that mr_url;
//   - a raw GitLab MR URL → the MR bead with that mr_url.
//
// The skip label must live on the merge-request bead (not the producing task
// bead) because that is the bead the trigger's bead_event/preflight evaluate.
func resolveMRBeadID(cmd *cobra.Command, arg string) (string, error) {
	if strings.HasPrefix(arg, "gd-") || strings.HasPrefix(arg, "ka-") {
		bead, err := daemon.GetBead(cmd.Context(), arg)
		if err != nil {
			return "", fmt.Errorf("fetching bead %s: %w", arg, err)
		}
		if bead.Type == "merge-request" {
			return bead.ID, nil
		}
		// Task (or other) bead — fall through to mr_url lookup.
		if bead.Fields["mr_url"] == "" {
			return "", fmt.Errorf("bead %s is not a merge-request bead and has no mr_url field", arg)
		}
		return findMRBeadByURL(cmd, bead.Fields["mr_url"])
	}
	// Treat as a raw MR URL.
	if bridge.ParseMRURL(arg) == nil {
		return "", fmt.Errorf("not a gd- bead id or a valid GitLab MR URL: %s", arg)
	}
	return findMRBeadByURL(cmd, arg)
}

// findMRBeadByURL locates the merge-request bead whose mr_url field matches the
// given URL. Mirrors beadsapi's unexported findMergeRequestBead lookup.
//
// The mr_url selector is pushed SERVER-SIDE via the daemon's field-equality
// filter (`fields ? 'mr_url' AND fields->>'mr_url' = $1`, kbeads queries.go) so
// the daemon returns the single matching bead. Before this it fetched Limit:500
// type=merge-request beads (created_at DESC) and re-scanned client-side — but
// with 6834 merge-request beads fleet-wide (measured 2026-08-03) the daemon
// capped the fetch at 500, so `gb mr status`/`gb mr skip` against an MR whose
// bead sorted past that window failed with a bogus "no merge-request bead
// found" (bug ka-0000005190, class ka-0000003555). All statuses stay in the
// filter — a merged MR's canonical bead is status=closed. Limit:2 surfaces
// (rather than hides) an unexpected duplicate for one canonical mr_url.
func findMRBeadByURL(cmd *cobra.Command, mrURL string) (string, error) {
	result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"merge-request"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred", "closed"},
		Fields:   map[string]string{"mr_url": mrURL},
		Limit:    2,
	})
	if err != nil {
		return "", fmt.Errorf("listing merge-request beads: %w", err)
	}
	for _, b := range result.Beads {
		if b.Fields["mr_url"] == mrURL {
			return b.ID, nil
		}
	}
	return "", fmt.Errorf("no merge-request bead found for %s (is the MR linked via 'gb mr link'?)", mrURL)
}

func runMRSkip(cmd *cobra.Command, args []string) error {
	beadID, err := resolveMRBeadID(cmd, args[0])
	if err != nil {
		return err
	}
	if err := daemon.AddLabel(cmd.Context(), beadID, mrSkipLabel); err != nil {
		return fmt.Errorf("adding %s label to %s: %w", mrSkipLabel, beadID, err)
	}
	cmd.Printf("Marked %s with %s — gardener-delivery will skip this MR.\n", beadID, mrSkipLabel)
	return nil
}

func runMRUnskip(cmd *cobra.Command, args []string) error {
	beadID, err := resolveMRBeadID(cmd, args[0])
	if err != nil {
		return err
	}
	if err := daemon.RemoveLabel(cmd.Context(), beadID, mrSkipLabel); err != nil {
		return fmt.Errorf("removing %s label from %s: %w", mrSkipLabel, beadID, err)
	}
	cmd.Printf("Removed %s from %s — gardener-delivery will drive this MR again.\n", mrSkipLabel, beadID)
	return nil
}

// runMRRelevanceGate evaluates the TV=0 ticket-resolved close gate for an MR.
// It resolves the MR, fetches sibling MRs linked to the same ticket, and runs
// the pure decision in bridge.EvaluateTicketResolvedCloseGate. See bug
// ka-0000002060.
func runMRRelevanceGate(cmd *cobra.Command, args []string) error {
	mrURL, err := resolveMRURL(cmd, args[0])
	if err != nil {
		return err
	}
	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return fmt.Errorf("could not parse MR URL: %s", mrURL)
	}
	client, err := newMRGitLabClient(baseURLFromGitLabURL(mrURL))
	if err != nil {
		return err
	}

	mr, err := client.GetMergeRequestByPath(cmd.Context(), ref.ProjectPath, ref.IID)
	if err != nil {
		return fmt.Errorf("fetching MR !%d: %w", ref.IID, err)
	}

	in := bridge.TicketResolvedGateInput{
		MR: bridge.RelevanceMR{
			IID:          mr.IID,
			TargetBranch: mr.TargetBranch,
			State:        mr.State,
		},
	}

	// Find sibling MRs linked to the same ticket. We search the project's MRs
	// for the ticket key in title/description. When --ticket is empty we have
	// no siblings to compare, so the gate falls through to its default (a
	// protected-target MR with no sibling resolution is still closeable).
	if mrRelevanceTicket != "" {
		siblings, sErr := client.ListMergeRequests(cmd.Context(), ref.ProjectPath, bridge.MRListOptions{
			State:   "all",
			Search:  mrRelevanceTicket,
			PerPage: 100,
		})
		if sErr != nil {
			return fmt.Errorf("listing sibling MRs for ticket %s: %w", mrRelevanceTicket, sErr)
		}
		for _, s := range siblings {
			if s.IID == mr.IID {
				continue
			}
			in.SiblingMRs = append(in.SiblingMRs, bridge.RelevanceMR{
				IID:          s.IID,
				TargetBranch: s.TargetBranch,
				State:        s.State,
			})
		}
	}

	decision := bridge.EvaluateTicketResolvedCloseGate(in)

	if jsonOutput {
		printJSON(map[string]any{
			"mr_iid":               mr.IID,
			"target_branch":        mr.TargetBranch,
			"ticket":               mrRelevanceTicket,
			"may_close":            decision.MayClose,
			"reason":               decision.Reason,
			"blocking_sibling_iid": decision.BlockingSiblingIID,
		})
	} else if decision.MayClose {
		cmd.Printf("MAY CLOSE !%d — %s\n", mr.IID, decision.Reason)
	} else {
		cmd.Printf("BLOCKED — do NOT close !%d — %s\n", mr.IID, decision.Reason)
	}

	if !decision.MayClose {
		// Distinct non-zero exit (3) so the gardener's shell can branch on a
		// "blocked, leave open" outcome vs. a real error (1). The decision was
		// already printed above, so suppress cobra's usage/error noise.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		os.Exit(3)
	}
	return nil
}

// runMRDispatchGate evaluates the review-merge suppress-dispatch gate for an MR
// (bugs ka-0000004390 + ka-0000001050). It resolves the MR arg to its linked TASK
// bead and its URL/IID, reads the task's forward blocking-dep chain AND the open
// decision/escalation beads that might reference the MR, and runs the pure
// decision in internal/bridge. Exit 0 = may dispatch; exit 3 = suppress
// (comment-only dedup).
func runMRDispatchGate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	task, err := resolveDispatchGateTaskBead(cmd, args[0])
	if err != nil {
		return err
	}

	in := bridge.DispatchGateInput{Blockers: map[string]*beadsapi.BeadDetail{}}
	if task != nil {
		in.TaskID = task.ID
		deps, derr := daemon.GetDependencies(ctx, task.ID)
		if derr != nil {
			return fmt.Errorf("fetching dependencies for task %s: %w", task.ID, derr)
		}
		in.Deps = deps

		// Batch-fetch the blocker beads referenced by blocking-typed deps so the
		// pure evaluator can inspect their type/status. Non-blocking edges are
		// skipped — nothing to resolve.
		var blockerIDs []string
		seen := map[string]bool{}
		for _, d := range deps {
			if d.DependsOnID == "" || seen[d.DependsOnID] {
				continue
			}
			switch d.Type {
			case "blocks", "blocked-by", "depends-on":
				seen[d.DependsOnID] = true
				blockerIDs = append(blockerIDs, d.DependsOnID)
			}
		}
		if len(blockerIDs) > 0 {
			beads, berr := daemon.BatchGetBeads(ctx, blockerIDs)
			if berr != nil {
				return fmt.Errorf("fetching blocker beads for task %s: %w", task.ID, berr)
			}
			in.Blockers = beads
		}
	}

	// REFERENCE signal (R1, bug ka-0000001050): resolve the MR's URL/IID and load
	// the candidate open decision/escalation handles so the evaluator can suppress
	// a fresh crew when one already references this MR — even without a blocks edge
	// or a linked task. Both are best-effort: a resolution/query failure leaves the
	// reference signal empty and the gate rests on the blocking-edge signal alone
	// (fail-open), never erroring out the whole walk.
	in.MRURL, in.MRProjectPath, in.MRIID = resolveDispatchGateMRRef(cmd, args[0])
	in.RefCandidates = gatherDispatchGateRefCandidates(ctx)
	// R3 (bug ka-0000007496): the injected clock the aging-nudge age is measured
	// against. time.Now() here keeps the evaluator itself pure/table-testable.
	in.Now = time.Now()
	// Test-observability seam: GB_DISPATCH_GATE_NUDGE_AGE overrides the 6-cycle
	// (~2h) park age past which the one-shot aging nudge fires. Production leaves
	// it unset (compiled DispatchGateNudgeAge governs); the e2e sets a tiny value
	// so a freshly-created handle — whose created_at cannot be backdated on the
	// shared daemon — crosses the threshold and drives the real fire-once
	// lifecycle. Mirrors gb news' GB_NEWS_ALIVE_FRESHNESS_WINDOW.
	in.NudgeAge = dispatchGateNudgeAgeValue()

	decision := bridge.EvaluateDispatchGate(in)

	// R3: when the gate says the MR has been parked past the nudge threshold and
	// no nudge has yet gone out, post ONE aging nudge on the EXISTING handle and
	// stamp its one-shot marker. Best-effort — a failed comment/stamp must not
	// change the suppress verdict nor abort the walk, so errors are surfaced but
	// swallowed. nudged records what actually happened for the output below.
	nudged := maybePostDispatchGateNudge(ctx, cmd, in, decision.Nudge)

	// COOLDOWN signal (task ka-0000001101): if the operator-gate signals did NOT
	// already suppress, consult the per-MR dispatch cooldown — a green orphan whose
	// head SHA has not moved since the last review-merge crew and was dispatched
	// within the cooldown window should NOT get a fresh crew (nothing changed to
	// re-review). Resolved best-effort; every failure fails OPEN (dispatch), and on
	// a genuine may-dispatch the stamp is (re)written so the next cycle's window is
	// measured from now. Skippable via --no-cooldown (e.g. a forced re-review).
	// Runs AFTER the R3 aging nudge, which applies only on the already-suppressed
	// (operator-gated) path, so the two signals never overlap on one MR.
	var cool cooldownResult
	if decision.MayDispatch && !mrDispatchGateNoCooldown {
		cool = evaluateDispatchCooldown(cmd, args[0], in.MRURL, in.MRProjectPath, in.MRIID)
		if !cool.decision.MayDispatch {
			// The cooldown, not an operator gate, is suppressing.
			decision = bridge.DispatchGateDecision{MayDispatch: false, Reason: cool.decision.Reason}
		}
	}

	if jsonOutput {
		out := map[string]any{
			"task_id":        in.TaskID,
			"mr_url":         in.MRURL,
			"mr_project":     in.MRProjectPath,
			"mr_iid":         in.MRIID,
			"may_dispatch":   decision.MayDispatch,
			"reason":         decision.Reason,
			"blocker_ids":    decision.BlockerIDs,
			"suppressed_by":  suppressedBy(decision, cool),
			"head_sha":       cool.currentHeadSHA,
			"last_head_sha":  cool.lastHeadSHA,
			"cooldown_stamp": cool.stamped,
		}
		if decision.Nudge != nil {
			out["nudge"] = map[string]any{
				"handle_id":     decision.Nudge.HandleID,
				"handle_type":   decision.Nudge.HandleType,
				"gate_age":      decision.Nudge.GateAge.String(),
				"gate_age_secs": int64(decision.Nudge.GateAge.Seconds()),
				"posted":        nudged,
			}
		}
		printJSON(out)
	} else if decision.MayDispatch {
		cmd.Printf("MAY DISPATCH — %s\n", decision.Reason)
	} else {
		cmd.Printf("SUPPRESS — do NOT spawn a review-merge crew — %s\n", decision.Reason)
		if decision.Nudge != nil {
			verb := "posted aging nudge"
			if !nudged {
				verb = "aging nudge DUE but not posted (see stderr)"
			}
			cmd.Printf("NUDGE — %s on %s %s (gated %s)\n",
				verb, decision.Nudge.HandleType, decision.Nudge.HandleID,
				humanizeGateAge(decision.Nudge.GateAge))
		}
	}

	if !decision.MayDispatch {
		// Distinct non-zero exit (3) so the gardener's shell can branch on a
		// "parked on operator / cooling down, comment-only" outcome vs. a real
		// error (1). The decision was already printed above, so suppress cobra's
		// usage/error noise.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		os.Exit(3)
	}
	return nil
}

// maybePostDispatchGateNudge posts the ONE aging nudge R3 (bug ka-0000007496)
// asks for — a single comment on the EXISTING parking decision/escalation bead
// naming the MR, the handle, and how long it has been gated — then stamps the
// one-shot marker field so the nudge never repeats on a later cycle. It is a
// no-op (returns false) when the evaluator flagged no nudge.
//
// Best-effort by contract: the comment and the stamp are two writes, and a
// failure of either must not change the suppress verdict nor abort the gardener's
// walk. So errors are printed to stderr and swallowed, and the marker is stamped
// only AFTER the comment succeeds — if the comment fails, the marker stays unset
// so the next cycle retries the nudge rather than silently dropping it. If the
// comment lands but the stamp fails, the operator got their one ping (the visible
// deliverable); a rare duplicate on a later cycle is the tolerable failure here.
func maybePostDispatchGateNudge(ctx context.Context, cmd *cobra.Command, in bridge.DispatchGateInput, nudge *bridge.DispatchGateNudge) bool {
	if nudge == nil || nudge.HandleID == "" {
		return false
	}
	comment := formatDispatchGateNudge(in, nudge)
	if err := daemon.AddComment(ctx, nudge.HandleID, defaultActor(), comment); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"dispatch-gate: aging nudge comment on %s failed (will retry next cycle): %v\n",
			nudge.HandleID, err)
		return false
	}
	// One-shot marker — stamped only after the comment landed. Store the wall
	// clock as RFC3339 for legibility; the evaluator only checks emptiness.
	if err := daemon.UpdateBeadFields(ctx, nudge.HandleID, map[string]string{
		bridge.DispatchGateNudgedAtField: in.Now.UTC().Format(time.RFC3339),
	}); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"dispatch-gate: aging nudge posted on %s but marking it one-shot failed "+
				"(a later cycle may re-nudge): %v\n",
			nudge.HandleID, err)
	}
	return true
}

// formatDispatchGateNudge builds the operator-facing aging-nudge comment. It
// leads with the ask, then the three required facts (acceptance criterion 3):
// the MR URL, the decision/escalation id, and how long the MR has been gated.
func formatDispatchGateNudge(in bridge.DispatchGateInput, nudge *bridge.DispatchGateNudge) string {
	mrRef := in.MRURL
	if mrRef == "" && in.MRIID > 0 {
		mrRef = fmt.Sprintf("!%d", in.MRIID)
	}
	if mrRef == "" {
		mrRef = "this MR"
	}
	return fmt.Sprintf(
		"⏳ gardener-delivery aging nudge: MR %s has been green + parked on this %s (%s) "+
			"for %s with no operator action — past the %d-cycle (~%s) threshold. "+
			"No new review-merge crew is being dispatched (they only re-confirm green + hold). "+
			"Please MERGE, HOLD, or CLOSE so the MR stops sitting. "+
			"This is a one-time ping; the gardener will keep suppressing re-dispatch silently after this.",
		mrRef, nudge.HandleType, nudge.HandleID,
		humanizeGateAge(nudge.GateAge),
		bridge.DispatchGateNudgeCycles, humanizeGateAge(bridge.DispatchGateNudgeAge))
}

// humanizeGateAge renders a gate age as a compact "Xh Ym" (or "Ym" under an
// hour) string for the nudge message, avoiding Go's default "2h0m0s" shape.
func humanizeGateAge(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// mrDispatchGateNoCooldown disables the per-MR dispatch cooldown for a single
// `gb mr dispatch-gate` invocation — the escape hatch for a deliberate forced
// re-review of an unchanged MR. The operator-gate signals still apply.
var mrDispatchGateNoCooldown bool

// dispatchGateNudgeAgeEnv is the env var that overrides the one-shot aging-nudge
// park-age threshold (R3, bug ka-0000007496). It exists ONLY as a
// test-observability seam: a freshly-created decision/escalation handle bead
// cannot have its created_at backdated on the shared daemon (the same wall the
// stale-reaper-grace e2e hit), so the dispatch-gate e2e sets a tiny value here
// to age a just-filed handle past the threshold and exercise the real
// fire-once-then-never-again lifecycle. Named + shaped exactly like gb news'
// GB_NEWS_ALIVE_FRESHNESS_WINDOW.
const dispatchGateNudgeAgeEnv = "GB_DISPATCH_GATE_NUDGE_AGE"

// dispatchGateNudgeAgeValue resolves the effective aging-nudge threshold: the
// GB_DISPATCH_GATE_NUDGE_AGE override (a Go duration, e.g. "5s") when set and
// parseable, else 0 so the pure evaluator falls back to the compiled
// bridge.DispatchGateNudgeAge (~2h). An unparseable value returns 0 too, so a
// typo never silently disables or reshapes the production threshold.
func dispatchGateNudgeAgeValue() time.Duration {
	if v := os.Getenv(dispatchGateNudgeAgeEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 0
}

// cooldownResult bundles what the cooldown stage resolved so the JSON projection
// and the suppressed-by label can report it without re-deriving anything.
type cooldownResult struct {
	decision       bridge.DispatchCooldownDecision
	currentHeadSHA string
	lastHeadSHA    string
	stamped        bool // a fresh cooldown stamp was written on the may-dispatch path
}

// evaluateDispatchCooldown runs the per-MR dispatch cooldown for the gate: it
// resolves the MR's current head SHA and its most-recent cooldown stamp, evaluates
// the pure cooldown decision, and — when the decision is may-dispatch — writes a
// fresh stamp on the MR bead so the next cycle's window starts now. Every step is
// best-effort: any resolution/read/write failure degrades to may-dispatch (the
// pure evaluator already fails open on an empty head SHA), so a flaky GitLab or
// daemon call can never strand an actionable MR on a phantom cooldown.
// mrURL/projectPath/iid are the already-resolved reference (from
// resolveDispatchGateMRRef in the caller) so we do not re-resolve them; any may be
// zero-valued when the caller could not resolve that dimension.
func evaluateDispatchCooldown(cmd *cobra.Command, arg, mrURL, projectPath string, iid int) cooldownResult {
	ctx := cmd.Context()
	res := cooldownResult{decision: bridge.DispatchCooldownDecision{MayDispatch: true}}

	// Fetch the MR's current head SHA from GitLab off the resolved ref. A missing
	// URL/token/SHA leaves currentHeadSHA empty; the evaluator then fails open.
	if mrURL != "" && projectPath != "" && iid > 0 {
		if client, err := newMRGitLabClient(baseURLFromGitLabURL(mrURL)); err == nil {
			if mr, err := client.GetMergeRequestByPath(ctx, projectPath, iid); err == nil && mr != nil {
				res.currentHeadSHA = mr.SHA
			}
		}
	}

	// Read the MR bead's comments for the last cooldown stamp. Resolve the
	// canonical merge-request bead (comments live there, not on the producing task)
	// best-effort — an orphan/unlinked MR simply has no prior stamp.
	mrBeadID, _ := resolveMRBeadID(cmd, arg)
	if mrBeadID != "" {
		if comments, err := daemon.GetComments(ctx, mrBeadID); err == nil {
			if sha, at, ok := bridge.LatestCooldownStamp(comments); ok {
				res.lastHeadSHA = sha
				res.decision = bridge.EvaluateDispatchCooldown(bridge.DispatchCooldownInput{
					CurrentHeadSHA:   res.currentHeadSHA,
					LastHeadSHA:      sha,
					LastDispatchedAt: at,
					Now:              time.Now(),
				})
			}
		}
	}

	// On may-dispatch with a resolved head SHA, stamp the MR bead so the next
	// cycle's cooldown is measured from this dispatch. Skip when suppressing (the
	// existing stamp still governs) or when we could not resolve either the head
	// SHA or the MR bead.
	if res.decision.MayDispatch && res.currentHeadSHA != "" && mrBeadID != "" {
		stamp := bridge.FormatCooldownStamp(res.currentHeadSHA, time.Now())
		if err := daemon.AddComment(ctx, mrBeadID, actor, stamp); err == nil {
			res.stamped = true
		}
	}
	return res
}

// suppressedBy labels which gate signal is holding the MR back, for the --json
// consumer: "operator-gate" when an open decision/escalation parks it,
// "cooldown" when the per-MR dispatch cooldown does, else "" (may dispatch).
func suppressedBy(decision bridge.DispatchGateDecision, cool cooldownResult) string {
	if decision.MayDispatch {
		return ""
	}
	if len(decision.BlockerIDs) > 0 {
		return "operator-gate"
	}
	if !cool.decision.MayDispatch {
		return "cooldown"
	}
	return "operator-gate"
}

// resolveDispatchGateMRRef best-effort resolves the CLI arg to the MR's full URL,
// project path, and project-scoped IID for the reference signal. Returns
// ("", "", 0) on any failure (unlinked bead, unparseable URL) — the reference
// scan then simply matches nothing, keeping the gate fail-open. It never errors:
// a missing reference dimension must not abort the dispatch decision.
func resolveDispatchGateMRRef(cmd *cobra.Command, arg string) (mrURL, projectPath string, iid int) {
	u, err := resolveMRURL(cmd, arg)
	if err != nil || u == "" {
		return "", "", 0
	}
	ref := bridge.ParseMRURL(u)
	if ref == nil {
		return u, "", 0
	}
	return u, ref.ProjectPath, ref.IID
}

// dispatchGateRefStatuses is the live-handle status set the reference-candidate
// query pulls. It mirrors bridge.dispatchGateOpenBlockerStatuses (the evaluator
// re-checks status itself, so this only bounds the fetch).
var dispatchGateRefStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// dispatchGateRefLimit bounds the reference-candidate fetch. The whole fleet's
// live decision+escalation handles number in the low hundreds (~180 measured
// 2026-08-04); the explicit cap makes a runaway population fail LOUD (the client
// warns on silent truncation) rather than silently dropping candidates.
const dispatchGateRefLimit = 5000

// gatherDispatchGateRefCandidates loads the OPEN decision + escalation beads the
// evaluator scans for a reference to the MR, in ONE bounded, explicitly-limited
// listing (never the paginated `kd list ... | jq contains` scan that silently
// returns only the first page — advice ka-0000005023, ka-0000002706). Best-
// effort: a query error contributes no candidates rather than aborting the gate
// (fail-open).
//
// COST NOTE: the gate is a fresh process per MR, so the gardener re-runs this
// once per open MR each cycle (no in-process cache to amortize across MRs). At
// fleet scale that is one list of ~180 rows per walked MR — acceptable today,
// but if the open-handle corpus or the MR count grows materially, hoist this
// into a single per-cycle snapshot the walk shares (the fetch-once-project-many
// pattern) rather than paying it per MR.
func gatherDispatchGateRefCandidates(ctx context.Context) []*beadsapi.BeadDetail {
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision", "escalation"},
		Statuses: dispatchGateRefStatuses,
		Limit:    dispatchGateRefLimit,
	})
	if err != nil || res == nil {
		return nil
	}
	return res.Beads
}

// resolveDispatchGateTaskBead resolves a CLI arg to the TASK bead whose blocking
// deps the dispatch gate inspects. Accepts:
//   - a gd-* task (or other non-MR) bead id → returned as-is;
//   - a gd-* merge-request bead id → its producing task bead (first `produces`
//     reverse dep);
//   - a gd-* task bead carrying mr_url, or a raw GitLab MR URL → the MR bead,
//     then its producing task bead.
//
// Returns (nil, nil) when no linked task can be resolved (e.g. an orphan MR with
// no producing bead) — the caller then reports MayDispatch=true (fail-open:
// there is nothing to prove parked).
func resolveDispatchGateTaskBead(cmd *cobra.Command, arg string) (*beadsapi.BeadDetail, error) {
	ctx := cmd.Context()
	if strings.HasPrefix(arg, "gd-") || strings.HasPrefix(arg, "ka-") {
		bead, err := daemon.GetBead(ctx, arg)
		if err != nil {
			return nil, fmt.Errorf("fetching bead %s: %w", arg, err)
		}
		if bead.Type != "merge-request" && bead.Fields["mr_url"] == "" {
			// A plain task/bug/feature bead — inspect its deps directly.
			return bead, nil
		}
		// An MR bead, or a task bead pointing at an MR by mr_url: resolve to the
		// MR bead id, then walk back to the producing task.
		mrBeadID, err := resolveMRBeadID(cmd, arg)
		if err != nil {
			return nil, err
		}
		return producingTaskForMRBead(ctx, mrBeadID)
	}
	// Raw MR URL → MR bead → producing task.
	mrBeadID, err := findMRBeadByURL(cmd, arg)
	if err != nil {
		return nil, err
	}
	return producingTaskForMRBead(ctx, mrBeadID)
}

// producingTaskForMRBead returns the first task bead that produced mrBeadID via
// a `produces` dep, or (nil, nil) when the MR has no producing bead.
func producingTaskForMRBead(ctx context.Context, mrBeadID string) (*beadsapi.BeadDetail, error) {
	tasks, err := daemon.ProducingTaskBeadsForMR(ctx, mrBeadID)
	if err != nil {
		return nil, fmt.Errorf("resolving producing task for MR bead %s: %w", mrBeadID, err)
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	return tasks[0], nil
}

// resolveMRURL resolves a bead ID or MR URL argument to an MR URL string.
func resolveMRURL(cmd *cobra.Command, arg string) (string, error) {
	// If it looks like a URL, use it directly.
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		return arg, nil
	}
	// Otherwise treat as bead ID.
	bead, err := daemon.GetBead(cmd.Context(), arg)
	if err != nil {
		return "", fmt.Errorf("fetching bead %s: %w", arg, err)
	}
	mrURL := bead.Fields["mr_url"]
	if mrURL == "" {
		return "", fmt.Errorf("bead %s has no mr_url field", arg)
	}
	return mrURL, nil
}

// baseURLFromGitLabURL derives a GitLab base URL ("scheme://host") from a full
// GitLab web URL (an MR or pipeline URL). Returns "" when rawURL is not an
// absolute http(s) URL with a host. This lets `gb mr`/`gb pipeline` accept a
// full MR URL and infer which GitLab instance to talk to without requiring
// --gitlab-url/GITLAB_BASE_URL to be set (bug ka-0000008229).
func baseURLFromGitLabURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// newMRGitLabClient builds a GitLab client for the `gb mr`/`gb pipeline`/
// `gb release` commands. The explicit --gitlab-url / GITLAB_BASE_URL always
// wins; when it is empty, callers may pass one or more fallback base URLs
// (typically derived from the MR/pipeline URL argument via
// baseURLFromGitLabURL) and the first non-empty one is used. Only when no base
// URL can be determined at all does it error.
func newMRGitLabClient(fallbackBaseURLs ...string) (*bridge.GitLabClient, error) {
	baseURL := mrGitLabURL
	for _, fb := range fallbackBaseURLs {
		if baseURL != "" {
			break
		}
		baseURL = fb
	}
	if baseURL == "" {
		return nil, fmt.Errorf("--gitlab-url or GITLAB_BASE_URL is required (or pass a full MR/pipeline URL)")
	}
	return bridge.NewGitLabClient(bridge.GitLabClientConfig{
		BaseURL: baseURL,
		Token:   resolveMRGitLabToken(),
	}), nil
}

// resolveMRGitLabToken picks the GitLab API token for `gb mr`/`gb pipeline`/
// `gb release`, matching how glab resolves its own token so agents don't have to
// pass --gitlab-token explicitly (bug ka-0000003680). Precedence:
//
//  1. the explicit --gitlab-token flag (mrGitLabToken), when set;
//  2. $GITLAB_API_TOKEN — the token the gitlab-bridge deployment carries and the
//     historical default for these commands;
//  3. $GITLAB_TOKEN — the token agent pods actually carry and the one glab reads.
//
// The old flag default was os.Getenv("GITLAB_API_TOKEN") only, so in an agent pod
// (which gets GITLAB_TOKEN, not GITLAB_API_TOKEN) the token resolved to "" and
// every gb mr call against Platform/monorepo 404'd as "404 Project Not Found"
// (GitLab masks a 403 from the unauthenticated/bridge-bot identity as a 404).
func resolveMRGitLabToken() string {
	if mrGitLabToken != "" {
		return mrGitLabToken
	}
	if t := os.Getenv("GITLAB_API_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GITLAB_TOKEN")
}

func runMRStatus(cmd *cobra.Command, args []string) error {
	target, err := resolveMRStatusTarget(args, mrStatusBead, mrStatusMR)
	if err != nil {
		return err
	}

	mrURL, err := resolveMRURL(cmd, target)
	if err != nil {
		return err
	}

	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return fmt.Errorf("could not parse MR URL: %s", mrURL)
	}

	client, err := newMRGitLabClient(baseURLFromGitLabURL(mrURL))
	if err != nil {
		return err
	}

	mr, err := client.GetMergeRequestByPath(cmd.Context(), ref.ProjectPath, ref.IID)
	if err != nil {
		return fmt.Errorf("fetching MR !%d: %w", ref.IID, err)
	}

	if jsonOutput {
		printJSON(mr)
		return nil
	}

	cmd.Printf("MR:       !%d\n", mr.IID)
	cmd.Printf("Title:    %s\n", mr.Title)
	cmd.Printf("State:    %s\n", mr.State)
	if mr.Author != nil {
		cmd.Printf("Author:   %s\n", mr.Author.Username)
	}
	cmd.Printf("Project:  %s\n", ref.ProjectPath)
	cmd.Printf("URL:      %s\n", mr.WebURL)
	if mr.SHA != "" {
		cmd.Printf("SHA:      %s\n", mr.SHA)
	}
	if mr.HeadPipeline != nil && mr.HeadPipeline.ID != 0 {
		cmd.Printf("Pipeline: #%d (%s)\n", mr.HeadPipeline.ID, mr.HeadPipeline.Status)
		if mr.HeadPipeline.WebURL != "" {
			cmd.Printf("          %s\n", mr.HeadPipeline.WebURL)
		}
	}

	// Show bead-side approval info if the target was a bead ID.
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		if bead, err := daemon.GetBead(cmd.Context(), target); err == nil {
			if approved := bead.Fields["mr_approved"]; approved != "" {
				cmd.Printf("Approved: %s\n", approved)
			}
			if approvers := bead.Fields["mr_approvers"]; approvers != "" {
				cmd.Printf("Approvers: %s\n", approvers)
			}
		}
	}
	return nil
}

// runMRVerifyCleanup checks whether an MR's source branch is gone from origin
// after a merge (bug ka-0000004927). It exits 0 when the branch is deleted, 3
// when it survived, and returns an ordinary error (exit 1) when the state could
// not be established.
func runMRVerifyCleanup(cmd *cobra.Command, args []string) error {
	mrURL, err := resolveMRURL(cmd, args[0])
	if err != nil {
		return err
	}
	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return fmt.Errorf("could not parse MR URL: %s", mrURL)
	}
	client, err := newMRGitLabClient(baseURLFromGitLabURL(mrURL))
	if err != nil {
		return err
	}

	branch := mrVerifyCleanupBranch
	if branch == "" {
		mr, err := client.GetMergeRequestByPath(cmd.Context(), ref.ProjectPath, ref.IID)
		if err != nil {
			return fmt.Errorf("fetching MR !%d: %w", ref.IID, err)
		}
		branch = mr.SourceBranch
		if branch == "" {
			return fmt.Errorf("MR !%d reports no source_branch; pass --branch to check explicitly", ref.IID)
		}
	}

	exists, err := client.BranchExists(cmd.Context(), ref.ProjectPath, branch)
	if err != nil {
		return fmt.Errorf("verifying branch %q on %s: %w", branch, ref.ProjectPath, err)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"mr_iid":        ref.IID,
			"project":       ref.ProjectPath,
			"source_branch": branch,
			"branch_exists": exists,
			"cleaned_up":    !exists,
		})
	} else if exists {
		cmd.Printf("NOT CLEANED UP — branch %q still exists on origin after merge of !%d.\n", branch, ref.IID)
		cmd.Printf("  --remove-source-branch was a silent no-op (likely a protected-branch denial). Report this honestly; do not claim the branch was cleaned up.\n")
	} else {
		cmd.Printf("CLEANED UP — branch %q is gone from origin (!%d).\n", branch, ref.IID)
	}

	if exists {
		// Distinct non-zero exit (3) so a gardener's shell branches on
		// "survived, report it" vs. a real error (1). Message already printed.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		os.Exit(3)
	}
	return nil
}

func runMREnv(cmd *cobra.Command, args []string) error {
	projectPath, pipelineID, baseURL, err := resolvePipelineInfo(cmd, args[0])
	if err != nil {
		return err
	}

	client, err := newMRGitLabClient(baseURL)
	if err != nil {
		return err
	}

	// Find the deploy-mr job.
	jobs, err := client.ListPipelineJobsByPath(cmd.Context(), projectPath, pipelineID)
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}

	var deployJob *bridge.GitLabJob
	for i := range jobs {
		if jobs[i].Name == "deploy-mr" {
			deployJob = &jobs[i]
			break
		}
	}
	if deployJob == nil {
		return fmt.Errorf("no deploy-mr job found in pipeline #%d", pipelineID)
	}

	if deployJob.Status != "success" {
		return fmt.Errorf("deploy-mr job status is %q (expected success)", deployJob.Status)
	}

	// Download the .urls/environment.env artifact file.
	data, err := client.DownloadJobArtifactFile(cmd.Context(), projectPath, deployJob.ID, ".urls/environment.env")
	if err != nil {
		return fmt.Errorf("downloading environment.env from deploy-mr job %d: %w", deployJob.ID, err)
	}

	// Parse the env file into key=value pairs.
	type envVar struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	var vars []envVar
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			vars = append(vars, envVar{Key: k, Value: v})
		}
	}

	if jsonOutput {
		printJSON(vars)
		return nil
	}

	exportMode, _ := cmd.Flags().GetBool("export")
	for _, v := range vars {
		if exportMode {
			cmd.Printf("export %s=%s\n", v.Key, v.Value)
		} else {
			cmd.Printf("%s=%s\n", v.Key, v.Value)
		}
	}
	return nil
}

type mrBead struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	MRURL string `json:"mr_url"`
	// Assignee and Status come from the ISSUE bead (the one carrying mr_url),
	// not the canonical merge-request bead: the gardener-delivery orphan sweep
	// keys its skip decisions on the issue bead's owner + lifecycle ("skip if
	// assignee ∈ ALIVE_AGENTS", "skip if not open"). Projecting them here lets
	// that decision be made from `gb mr list --json` alone, instead of an N+1
	// `kd show` per candidate every cycle (bug ka-0000002009).
	Assignee string `json:"assignee,omitempty"`
	Status   string `json:"status,omitempty"`
	MRMerged string `json:"mr_merged,omitempty"`
	MRState  string `json:"mr_state,omitempty"`
	// SHARP EDGE — mixed-source row: Author, MRIID, and Draft come from the
	// canonical merge-request bead (mrBeadsByURL[u]), NOT the issue bead — the
	// opposite source from Assignee/Status above. These fields exist ONLY on the
	// MR bead: on issue beads they are effectively absent (census 2026-08-03:
	// over 207 mr_url-bearing issue beads, mr_iid appeared once and author/draft
	// never). Draft is derived from the MR bead's mr_title `Draft:` prefix — the
	// same predicate the gardener-delivery orphan sweep applies by eye today.
	// Projecting them here lets that sweep skip draft + human-authored MRs with
	// zero `glab mr view <iid>` calls (the second N+1 in bug ka-0000002009).
	Author string `json:"author,omitempty"`
	MRIID  string `json:"mr_iid,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
}

// terminalMRStates are the GitLab MR states from which an MR never returns.
// Once GitLab reports one of these, no later `opened` reading for the same MR
// can be correct — so a bead carrying one always outranks a bead carrying
// `opened` regardless of which row the daemon happened to return last.
var terminalMRStates = map[string]bool{"merged": true, "closed": true}

// pickCanonicalMRBead resolves WHICH type=merge-request bead speaks for an
// mr_url when several claim it, returning the more authoritative of cur and
// next (either may be nil).
//
// The duplicates are real and not rare. A full census of the live population
// (2026-08-18: all 8407 MR beads, 7807 distinct mr_urls) found 396 urls carrying
// 2-7 canonical MR beads each, 282 of which DISAGREE about mr_state. They
// accumulate because UpsertMergeRequestBead's find-then-create is only
// serialised in-process (beadsapi client_mr.go), so two bridge replicas — or one
// replica across a restart — can each miss findMergeRequestBead and insert.
//
// Before this, runMRList did `mrBeadsByURL[u] = b` — last row wins, over a
// daemon result set with no ORDER BY. That is what made `gb mr list` report
// long-merged MRs as `opened` (bug ka-0000002394): the join could land on a
// duplicate abandoned while the MR was still open, while a sibling bead
// correctly recorded the merge. The sharp edge is that row order is stable for a
// GIVEN query but differs BETWEEN query shapes, so the old join was not
// reproducibly wrong — it was arbitrary, and silently changed answer as the
// population grew and pagination shifted. Measured against GitLab over all 282
// conflicting groups: last-writer-wins agreed with ground truth on 0 (its winner
// is whichever row sorted last, which is uncorrelated with truth), a pure
// freshest-wins rule on 278, and the rule below on 282.
//
// Precedence, in order:
//  1. A terminal mr_state (merged/closed) beats a non-terminal one. Terminal is
//     absorbing — GitLab never walks an MR back to `opened` — so this is a fact
//     about the MR rather than a guess about recency. It is also the entire gap
//     over freshest-wins: those 4 losses are all a stale `opened` duplicate that
//     was TOUCHED more recently than the sibling recording the merge.
//  2. Otherwise the more recently updated bead wins.
//  3. Ties (equal timestamps, e.g. two rows stamped in the same batch) fall to
//     the larger bead ID, purely so the result is deterministic across runs
//     rather than dependent on daemon row order.
//
// Scope limit: this reports the best state the data plane ALREADY HOLDS. It
// cannot fix a url whose every duplicate predates the merge — no local row
// records it — which is the separate drift the gitlab-bridge MR-terminalize
// sweep covers via a GitLab re-poll (internal/bridge/gitlab_mr_terminalize_drift.go).
func pickCanonicalMRBead(cur, next *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	if cur == nil {
		return next
	}
	if next == nil {
		return cur
	}
	curTerm := terminalMRStates[cur.Fields["mr_state"]]
	nextTerm := terminalMRStates[next.Fields["mr_state"]]
	if curTerm != nextTerm {
		if nextTerm {
			return next
		}
		return cur
	}
	if !next.UpdatedAt.Equal(cur.UpdatedAt) {
		if next.UpdatedAt.After(cur.UpdatedAt) {
			return next
		}
		return cur
	}
	if next.ID > cur.ID {
		return next
	}
	return cur
}

// filterMRBeads selects beads that have an mr_url and match optional owner/spawnedBy.
//
// owner matches if bead.Assignee == owner OR bead.Fields["spawned_by"] == owner.
// spawnedBy matches if bead.Fields["spawned_by"] == spawnedBy.
// Both filters are AND-combined when set; an empty filter is a no-op.
//
// mrBeadsByURL maps mr_url → canonical type=merge-request bead. MR state lives
// there, not on the issue bead: the bridge stopped stamping mr_state/mr_merged
// on issue beads (taskBeadBanFields, gitlab_sync.go), so the issue-bead fields
// are only a fallback for legacy rows. See bug ka-0000007109.
func filterMRBeads(in []*beadsapi.BeadDetail, mrBeadsByURL map[string]*beadsapi.BeadDetail, owner, spawnedBy string) []mrBead {
	var out []mrBead
	for _, b := range in {
		u := b.Fields["mr_url"]
		if u == "" {
			continue
		}
		sb := b.Fields["spawned_by"]
		if owner != "" && b.Assignee != owner && sb != owner {
			continue
		}
		if spawnedBy != "" && sb != spawnedBy {
			continue
		}
		// Legacy fallback: issue-bead fields, for rows stamped before the
		// canonical-MR-bead migration.
		state := b.Fields["mr_state"]
		merged := b.Fields["mr_merged"]
		// author/mr_iid/draft come from the canonical MR bead ONLY — unlike
		// assignee/status (issue bead) below. They live only on mrBeadsByURL[u].
		var author, mrIID string
		var draft bool
		if mr := mrBeadsByURL[u]; mr != nil {
			if s := mr.Fields["mr_state"]; s != "" {
				state = s
			}
			// Same merged predicate as gitlab_sync.go / gitlab_agent_resolver.go.
			if mr.Fields["mr_merged"] == "true" || mr.Fields["mr_outcome"] == "merged" || mr.Fields["mr_state"] == "merged" {
				merged = "true"
			}
			author = mr.Fields["author"]
			mrIID = mr.Fields["mr_iid"]
			// Same `Draft:`-prefix predicate the orphan sweep applies by eye.
			draft = strings.HasPrefix(mr.Fields["mr_title"], "Draft:")
		}
		out = append(out, mrBead{
			ID:       b.ID,
			Title:    b.Title,
			MRURL:    u,
			Assignee: b.Assignee,
			Status:   b.Status,
			MRMerged: merged,
			MRState:  state,
			Author:   author,
			MRIID:    mrIID,
			Draft:    draft,
		})
	}
	return out
}

func runMRList(cmd *cobra.Command, _ []string) error {
	result, err := daemon.ListAllBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		// mr_url can be stamped on any issue-type bead, not just tasks. Enumerate
		// the issue types explicitly (rather than dropping the Types filter) to
		// keep message/doc/event beads — which never carry mr_url — out of the
		// query. See bug ka-0000005054: a type=bug bead with mr_url was dropped by
		// the daemon before filterMRBeads (the real mr_url selector) ever saw it.
		Types:    []string{"task", "bug", "feature", "epic"},
		Statuses: []string{"open", "in_progress"},
		// Push the REAL selector server-side: `field=mr_url=*` is the daemon's
		// presence sentinel (`fields ? 'mr_url' AND fields->>'mr_url' != ''`,
		// kbeads queries.go) so the row cap applies to the mr_url-BEARING
		// population, not the 20× larger open/in_progress issue set. Before this,
		// filterMRBeads applied `mr_url != ""` CLIENT-SIDE after a Limit:500
		// fetch, so the daemon spent its cap on the ~93% of beads with no mr_url
		// and silently dropped ~45% of the MR-bearing ones (bug ka-0000001936;
		// same family as ka-0000005054). ListAllBeadsFiltered then pages the
		// (now small) matched set to exhaustion so the fix is growth-proof.
		Fields: map[string]string{"mr_url": "*"},
	})
	if err != nil {
		return fmt.Errorf("listing beads: %w", err)
	}

	// MR state lives on canonical type=merge-request beads, not on the issue
	// beads (bug ka-0000007109). One extra query keyed by mr_url — closed MUST
	// be included or merged MRs (whose MR beads are status=closed) show "-".
	// Paged to exhaustion for the same reason as the issue query: the MR-bead
	// population also outgrows a fixed 500 cap (bug ka-0000001936).
	mrBeadsByURL := map[string]*beadsapi.BeadDetail{}
	mrResult, err := daemon.ListAllBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"merge-request"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred", "closed"},
	})
	if err != nil {
		return fmt.Errorf("listing merge-request beads: %w", err)
	}
	for _, b := range mrResult.Beads {
		if u := b.Fields["mr_url"]; u != "" {
			mrBeadsByURL[u] = pickCanonicalMRBead(mrBeadsByURL[u], b)
		}
	}

	beads := filterMRBeads(result.Beads, mrBeadsByURL, mrListOwner, mrListSpawnedBy)

	if len(beads) == 0 {
		if jsonOutput {
			printJSON([]any{})
		} else {
			cmd.Println("No beads with MR URLs found")
		}
		return nil
	}

	if jsonOutput {
		printJSON(beads)
		return nil
	}

	cmd.Printf("%-16s %-10s %-8s %s\n", "BEAD", "MR STATE", "MERGED", "TITLE")
	cmd.Printf("%-16s %-10s %-8s %s\n",
		strings.Repeat("-", 16), strings.Repeat("-", 10), strings.Repeat("-", 8), strings.Repeat("-", 30))
	for _, b := range beads {
		state := b.MRState
		if state == "" {
			state = "-"
		}
		merged := b.MRMerged
		if merged == "" {
			merged = "-"
		}
		cmd.Printf("%-16s %-10s %-8s %s\n",
			truncate(b.ID, 16), state, merged, truncate(b.Title, 50))
	}
	return nil
}

func runMRLink(cmd *cobra.Command, args []string) error {
	beadID := args[0]
	mrURL := args[1]

	// Validate the URL looks like a GitLab MR or a GitHub PR.
	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		if pr := bridge.ParsePRURL(mrURL); pr != nil {
			return runMRLinkGitHub(cmd, beadID, mrURL, pr)
		}
		return fmt.Errorf("not a valid GitLab MR or GitHub PR URL: %s", mrURL)
	}

	if mrLinkUpstream {
		return runMRLinkUpstream(cmd, beadID, mrURL, ref)
	}

	// Set mr_url on the bead.
	if err := daemon.UpdateBeadFields(cmd.Context(), beadID, map[string]string{
		"mr_url": mrURL,
	}); err != nil {
		return fmt.Errorf("setting mr_url on bead %s: %w", beadID, err)
	}

	cmd.Printf("Linked %s → !%d (%s)\n", beadID, ref.IID, ref.ProjectPath)
	cmd.Printf("GitLab bridge will now track MR state, pipeline, and review comments.\n")
	return nil
}

// runMRLinkGitHub links a bead to a GitHub pull request. It writes the same
// mr_url field the gitlab-bridge path does, plus forge=github — the marker
// github-bridge sets itself (cmd/github-bridge/pullrequest.go) and that
// gitlab-bridge's nudge path keys on to leave the bead alone.
func runMRLinkGitHub(cmd *cobra.Command, beadID, mrURL string, pr *bridge.MRRef) error {
	if mrLinkUpstream {
		return fmt.Errorf("--upstream mirrors human-owned GitLab MRs and does not apply to GitHub PR URLs: %s", mrURL)
	}
	if err := daemon.UpdateBeadFields(cmd.Context(), beadID, map[string]string{
		"mr_url": mrURL,
		"forge":  "github",
	}); err != nil {
		return fmt.Errorf("setting mr_url on bead %s: %w", beadID, err)
	}
	cmd.Printf("Linked %s → #%d (%s)\n", beadID, pr.IID, pr.ProjectPath)
	cmd.Printf("GitHub bridge will now track PR state, checks, and review comments.\n")
	return nil
}

// runMRLinkUpstream handles `gb mr link <peel-request> <upstream-mr-url> --upstream`.
//
// It creates (or reuses) the read-only upstream-MR mirror bead for a human-owned
// peel MR on the REAL monorepo (E-PROV FW-5, ka-0000003146) and links the
// peel-request bead to it via a `produces` dep — the same lineage edge the
// gitlab-bridge draws for dark MRs, so the constellation shows
// peel-request --produces--> upstream-mirror.
//
// The mirror bead is created via UpsertUpstreamMirrorBead, which is the single
// enforcement point for the read-only-mirror invariant: it stamps upstream:true
// + gardener-delivery-skip + external-mirror:true and the external_mirror field.
// The URL is host-preserving (ref.Host comes straight off the supplied URL), so
// a dark !N and an upstream !N never collide. NEVER fires the upstream Jira
// write path; nothing is pushed outbound.
func runMRLinkUpstream(cmd *cobra.Command, beadID, mrURL string, ref *bridge.MRRef) error {
	ctx := cmd.Context()

	mirrorID, err := daemon.UpsertUpstreamMirrorBead(ctx, beadsapi.MergeRequestBeadFields{
		MRURL:       mrURL,
		MRIID:       strconv.Itoa(ref.IID),
		ProjectPath: ref.ProjectPath,
		// Title defaults to the URL inside the upsert when empty; the upstream
		// webhook / backstop poller fills in the real MR title on first sync.
		OperatedBy: actor,
	})
	if err != nil {
		return fmt.Errorf("creating upstream-mirror bead for %s: %w", mrURL, err)
	}

	// Lineage: peel-request --produces--> upstream-mirror. Best-effort — the
	// mirror bead exists and is correctly marked even if the dep write fails.
	if err := daemon.AddDependency(ctx, beadID, mirrorID, "produces", actor); err != nil {
		cmd.Printf("Warning: created mirror bead %s but failed to add produces dep from %s: %v\n", mirrorID, beadID, err)
	}

	cmd.Printf("Created upstream-mirror bead %s → !%d (%s) on %s\n", mirrorID, ref.IID, ref.ProjectPath, ref.Host)
	cmd.Printf("Marked %s (read-only mirror): gardener-delivery will skip it; viewer renders it as external.\n", strings.Join(beadsapi.UpstreamMirrorLabels, ", "))
	cmd.Printf("Linked %s --produces--> %s.\n", beadID, mirrorID)
	return nil
}
