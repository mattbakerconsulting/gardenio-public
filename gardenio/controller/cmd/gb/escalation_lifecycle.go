package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── escalation approve ──────────────────────────────────────────────────

var escalationApproveCmd = &cobra.Command{
	Use:   "approve <id>",
	Short: "Approve an escalation request",
	Long: `Approve an escalation. The daemon records the approval and claims the
one executor that runs it (execution_dispatch); --execution-target overrides
the escalation's execution_target as part of the same request.
Uses the kbeads API directly (no OIDC required).

The --as flag specifies the approver identity (defaults to KD_ACTOR env or actor flag).

Examples:
  gb escalation approve ka-abc123
  gb escalation approve ka-abc123 --as matthew.baker
  gb escalation approve ka-abc123 --execution-target local
  gb escalation approve ka-abc123 --comment "merge after the release cut"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		approvedBy, _ := cmd.Flags().GetString("as")
		execTarget, _ := cmd.Flags().GetString("execution-target")
		comment, _ := cmd.Flags().GetString("comment")
		if execTarget != "" && execTarget != "local" && execTarget != "service_account" {
			return fmt.Errorf("--execution-target must be local or service_account")
		}

		if approvedBy == "" {
			approvedBy = os.Getenv("KD_ACTOR")
		}
		if approvedBy == "" {
			approvedBy = actor
		}
		if approvedBy == "" {
			return fmt.Errorf("--as is required (or set KD_ACTOR)")
		}

		// Verify escalation exists and is approvable.
		bead, err := daemon.GetBead(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("getting escalation: %w", err)
		}
		if bead.Type != "escalation" {
			return fmt.Errorf("%s is not an escalation (type=%s)", id, bead.Type)
		}
		if bead.Status != "open" && bead.Status != "in_progress" {
			return fmt.Errorf("escalation is already %s", bead.Status)
		}

		// The executor override rides the approve request: the daemon records
		// it with the approval and claims the one executor (P7 Task 12), so
		// there is no separate PATCH to race the approval.
		if err := daemon.ApproveEscalation(cmd.Context(), id, beadsapi.ApproveEscalationRequest{
			ApprovedBy: approvedBy,
			// The daemon derives approval_method (P7 Task 15); older daemons
			// still read it from the body.
			ApprovalMethod:  "cli",
			ExecutionTarget: execTarget,
			Comment:         comment,
		}); err != nil {
			return escalationPageRefusal(err, "approving", id)
		}

		fmt.Printf("Approved: %s\n", id)
		fmt.Printf("  Approved by: %s\n", approvedBy)
		fmt.Printf("  Method: cli\n")
		if execTarget != "" {
			fmt.Printf("  Execution target: %s\n", execTarget)
		}

		return nil
	},
}

// escalationPageRefusal wraps an approve or reject error. A 403 names the
// escalation's page, the only place a person approves or rejects (P10.B
// gd-4mEAhIE01U): gb, like kd and gl, never does it for a person.
func escalationPageRefusal(err error, doing, id string) error {
	var apiErr *beadsapi.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		return fmt.Errorf("%s escalation: %w", doing, err)
	}
	base := strings.TrimRight(os.Getenv("BEADS_VIEWER_URL"), "/")
	if base == "" {
		base = strings.TrimRight(defaultGardenioViewerURL, "/")
	}
	page := "its page in the garden"
	if base != "" {
		page = base + "/" + url.PathEscape(id)
	}
	return fmt.Errorf("%s escalation: %w\n  gb never approves or rejects for a person; a person does it on the escalation's page, in the browser or on a phone:\n    %s", doing, err, page)
}

// ── escalation reject ───────────────────────────────────────────────────

var escalationRejectCmd = &cobra.Command{
	Use:   "reject <id>",
	Short: "Reject an escalation request",
	Long: `Reject an escalation, closing it with a failure status.

Examples:
  gb escalation reject ka-abc123 --reason "Not needed anymore"
  gb escalation reject ka-abc123 --as matthew.baker --reason "Duplicate"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		rejectedBy, _ := cmd.Flags().GetString("as")
		reason, _ := cmd.Flags().GetString("reason")

		if rejectedBy == "" {
			rejectedBy = os.Getenv("KD_ACTOR")
		}
		if rejectedBy == "" {
			rejectedBy = actor
		}
		if rejectedBy == "" {
			return fmt.Errorf("--as is required (or set KD_ACTOR)")
		}

		// Verify escalation exists and is rejectable.
		bead, err := daemon.GetBead(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("getting escalation: %w", err)
		}
		if bead.Type != "escalation" {
			return fmt.Errorf("%s is not an escalation (type=%s)", id, bead.Type)
		}
		if bead.Status != "open" && bead.Status != "in_progress" {
			return fmt.Errorf("escalation is already %s", bead.Status)
		}

		if reason == "" {
			reason = "Rejected via CLI"
		}

		if err := daemon.RejectEscalation(cmd.Context(), id, beadsapi.RejectEscalationRequest{
			RejectedBy: rejectedBy,
			Reason:     reason,
		}); err != nil {
			return escalationPageRefusal(err, "rejecting", id)
		}

		fmt.Printf("Rejected: %s\n", id)
		fmt.Printf("  Rejected by: %s\n", rejectedBy)
		fmt.Printf("  Reason: %s\n", reason)

		return nil
	},
}

// ── escalation close ────────────────────────────────────────────────────

var escalationCloseCmd = &cobra.Command{
	Use:   "close <id>",
	Short: "Close an escalation with a result",
	Long: `Close an escalation by recording the execution result.

Examples:
  gb escalation close ka-abc123 --status success --result '{"output":"credentials rotated"}'
  gb escalation close ka-abc123 --status failure --result '{"error":"permission denied"}'
  gb escalation close ka-abc123 --status partial --result '{"done":["step1"],"failed":["step2"]}'

--result also accepts a plain prose summary, which is stored as {"output":"<text>"}:
  gb escalation close ka-abc123 --status success --result 'MR merged, all good'

Withdraw an escalation satisfied some other way, with no approval needed: its
requesting agent, the current assignee of its requesting task, or (before an
approval) a later run on the requesting agent's seat may close it with
--status failure (never success):
  gb escalation close ka-abc123 --status failure --result 'withdrawn: release 2026.308.2 already rolled it'

An escalation filed with --close-on-execute closes its requesting task on
success. When this outcome does not finish that task (the merge did not carry
its fix), add --keep-requesting-task: the opt-in is withdrawn in the same write
as the close. Removing it afterwards races the auto-close and loses.
  gb escalation close ka-abc123 --status success --keep-requesting-task --result '...'

A helm_deploy escalation closed with the rolled tag records where that release
now runs, as release-deploy.sh does after a verified roll (the result carries
tag, revision and target; releases-design.md §4.3):
  gb escalation close ka-abc123 --status success --deployed-tag 2026.304.1 --revision 431`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		resultStatus, _ := cmd.Flags().GetString("status")
		resultJSON, _ := cmd.Flags().GetString("result")
		executor, _ := cmd.Flags().GetString("executor")
		keepTask, _ := cmd.Flags().GetBool("keep-requesting-task")

		if resultStatus == "" {
			return fmt.Errorf("--status is required (success, failure, or partial)")
		}
		switch resultStatus {
		case "success", "failure", "partial":
			// valid
		default:
			return fmt.Errorf("--status must be one of: success, failure, partial")
		}

		if executor == "" {
			executor = os.Getenv("KD_ACTOR")
		}
		if executor == "" {
			executor = actor
		}

		req := beadsapi.CaptureEscalationResultRequest{
			ResultStatus:       resultStatus,
			Executor:           executor,
			KeepRequestingTask: keepTask,
		}

		if resultJSON != "" {
			var v any
			if err := json.Unmarshal([]byte(resultJSON), &v); err != nil {
				// A human-style summary is the natural first instinct here, and
				// rejecting it drove 8 malformed retries in one session
				// (ka-0000007111). Accept prose by wrapping it as {"output": ...}.
				v = map[string]any{"output": resultJSON}
			}
			req.Result = v
		}

		deployedTag, _ := cmd.Flags().GetString("deployed-tag")
		if deployedTag != "" {
			if resultStatus != "success" {
				return fmt.Errorf("--deployed-tag records a deployment, so it needs --status success")
			}
			revision, _ := cmd.Flags().GetString("revision")
			target, _ := cmd.Flags().GetString("target")
			req.Result = withDeployedTag(req.Result, deployedTag, revision, target)
		}

		dep, err := daemon.CaptureEscalationResultDeployment(cmd.Context(), id, req)
		if err != nil {
			return fmt.Errorf("closing escalation: %w", err)
		}

		fmt.Printf("Closed: %s (%s)\n", id, resultStatus)
		releaseExecutorStopGate(cmd.Context(), id)
		if dep != nil {
			if dep.Error != "" {
				fmt.Printf("Deployment not recorded: %s %s on %s: %s\n", dep.Project, dep.Tag, dep.Target, dep.Error)
			} else {
				fmt.Printf("Deployment recorded: %s %s on %s at revision %s (%s)\n", dep.Project, dep.Tag, dep.Target, dashIfBlank(dep.Revision), dep.Release)
			}
		}
		return nil
	},
}

// releaseExecutorStopGate lets the turn that closed an escalation end
// without a decision when the closer is the local agent the executor
// delivered it to: closing the escalation is that agent's checkpoint, as
// `gb decision report` is a crew's. Like a yield, the release is consumed by
// the next Stop. Anyone else closing an escalation keeps the gate
// (gd-17BiVe2oC6). Best-effort: the escalation is already closed.
func releaseExecutorStopGate(ctx context.Context, escalationID string) {
	if os.Getenv(localAgentStartPIDEnv) == "" {
		return
	}
	agentID, _ := resolveAgentID("")
	if agentID == "" {
		return
	}
	esc, err := daemon.GetBead(ctx, escalationID)
	if err != nil || !deliveredToThisAgent(esc.Fields) {
		return
	}
	if err := daemon.SatisfyGate(ctx, agentID, "decision"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to satisfy decision gate: %v\n", err)
		return
	}
	if err := daemon.UpdateBeadFields(ctx, agentID, map[string]string{"gate_satisfied_by": "yield"}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to set gate_satisfied_by field: %v\n", err)
	}
}

// deliveredToThisAgent reports whether the escalation executor recorded
// delivering this escalation's prompt to the agent this session runs as.
func deliveredToThisAgent(fields map[string]string) bool {
	to := fields["prompt_delivered_to"]
	if to == "" {
		return false
	}
	for _, k := range []string{"BOAT_AGENT", "KD_ACTOR"} {
		if v := os.Getenv(k); v != "" {
			return v == to
		}
	}
	return false
}

// ── escalation backfill-links ───────────────────────────────────────────

var escalationBackfillLinksCmd = &cobra.Command{
	Use:   "backfill-links",
	Short: "Materialize blocks-edges for existing open escalations",
	Long: `One-shot backfill: for every OPEN/IN-PROGRESS escalation
whose requesting_task field is set, write the 'blocks' dependency edge from
that task to the escalation — the edge 'gb escalation create' now writes at
create time — so the /plans blocked-by-escalation signal lights up for the
escalations already in the queue, not just new ones.

Idempotent: an escalation whose task already carries the blocks-edge is
skipped. Bounded: the queue fetch is capped at 200 (matching the viewer's
queue cap). Use --dry-run to preview without writing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		ctx := cmd.Context()

		open, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"escalation"},
			Statuses: []string{"open", "in_progress"},
			Limit:    200,
		})
		if err != nil {
			return fmt.Errorf("listing open escalations: %w", err)
		}

		linked, skipped, failed := 0, 0, 0
		for _, b := range open.Beads {
			task := b.Fields["requesting_task"]
			if task == "" {
				skipped++
				continue
			}
			// Idempotence: skip when the task already blocks-depends on this
			// escalation (create-time edge or a previous backfill run).
			deps, derr := daemon.GetDependencies(ctx, task)
			if derr != nil {
				fmt.Fprintf(os.Stderr, "WARNING: %s: cannot read deps of %s: %v\n", b.ID, task, derr)
				failed++
				continue
			}
			exists := false
			for _, d := range deps {
				if d.Type == "blocks" && d.DependsOnID == b.ID {
					exists = true
					break
				}
			}
			if exists {
				skipped++
				continue
			}
			if dryRun {
				fmt.Printf("would link: %s blocks %s (%s)\n", task, b.ID, b.Title)
				linked++
				continue
			}
			if err := daemon.AddDependency(ctx, task, b.ID, "blocks", actor); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: %s: link failed: %v\n", b.ID, err)
				failed++
				continue
			}
			fmt.Printf("linked: %s blocks %s (%s)\n", task, b.ID, b.Title)
			linked++
		}
		fmt.Printf("backfill done: %d linked, %d skipped, %d failed (of %d open)\n",
			linked, skipped, failed, len(open.Beads))
		return nil
	},
}

// ── escalation reflag ───────────────────────────────────────────────────

var escalationReflagCmd = &cobra.Command{
	Use:   "reflag <id>",
	Short: "Reflag an escalation: bump priority and surface it to the top of the list",
	Long: `Reflag a stale escalation so it rises to the top of "gb escalation list" and
gains a higher priority. Each reflag decrements the bead's priority by one
(toward P0), records "reflagged_at" + "reflag_count" on the bead, and requests
a Slack re-post of the escalation card via the same pingback path as
"gb escalation pingback".

Reflag is idempotent at P0: once an escalation is at priority 0 (critical),
further reflag calls only bump the reflag_count and refresh reflagged_at —
priority does not wrap or go negative.

This mirrors "gb decision reflag" for type=escalation beads — use it when an
escalation gating your work has gone stale AND become more urgent. When it is
merely stalled, prefer "gb escalation pingback" (no priority change); the
engine also re-notifies automatically at ~12h/~24h/~48h with backoff.

Examples:
  # Bump priority by one toward P0
  gb escalation reflag ka-abc123

  # Force a specific priority (skips the +1 bump; useful to jump straight to P0)
  gb escalation reflag ka-abc123 --priority=0

  # Reflag with a reason (recorded on the bead and shown in Slack)
  gb escalation reflag ka-abc123 --reason="still blocking plan ka-xyz789"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReflag(cmd, args[0], "escalation")
	},
}

// withDeployedTag folds a helm_deploy roll's tag, revision and target into an
// escalation result: a JSON object result gains the keys, any other result is
// kept under output.
func withDeployedTag(result any, tag, revision, target string) map[string]any {
	m, ok := result.(map[string]any)
	if !ok {
		m = map[string]any{}
		if result != nil {
			m["output"] = result
		}
	}
	m["tag"] = tag
	if revision != "" {
		m["revision"] = revision
	}
	if target != "" {
		m["target"] = target
	}
	return m
}

// dashIfBlank is s, or "-" when it is empty.
func dashIfBlank(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
