package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// escalation.go gives kd parity with `gb escalation`'s resolve surface
// (ka-0000007156). Before this, a ka-only agent that hit the escalation
// resolve-surface divert (ka-0000005516: a generic `kd close` on an OPEN
// escalation is refused with 409/FailedPrecondition) had NO kd verb to fall
// back to and had to switch binaries to gb. These verbs close that gap.
//
// The resolve verbs (approve/reject/resolve) POST to
// /v1/escalations/{id}/{approve,reject,result}, which are RoleOperator-gated on
// the daemon. An agent token therefore gets 403 BY DESIGN — escalations are an
// operator surface. A crew agent that filed neither the escalation nor holds
// operator creds cannot resolve one, and should file a `gb decision` for a
// work-blocking question instead. handleEscalationAuthErr surfaces that
// distinction instead of a bare "HTTP 403".

var escalationCmd = &cobra.Command{
	Use:     "escalation",
	Aliases: []string{"esc", "escalations"},
	Short:   "Inspect and resolve privilege escalations",
	Long: `Inspect the operator escalation queue and (for operators) resolve escalations.

Escalations are an OPERATOR surface: 'approve', 'reject', and 'resolve' hit the
RoleOperator-gated endpoints, so an ordinary agent token gets a 403 by design.
A crew agent that needs a work-blocking answer should file a 'gb decision'
instead — escalations are for concrete privileged actions a human must run.

  kd escalation list                       # open escalations (JSON rows carry labels, operation_type, operation_spec, requesting_task)
  kd escalation list --operation-type github_pr_merge,gitlab_mr_merge --exclude-label merge-desk:seen
  kd escalation show <id>                   # full detail for one escalation
  kd escalation approve <id> --as <who>     # operator: approve + auto-replay the denied op
  kd escalation reject  <id> --reason <why> # operator: close as failure, no side effect
  kd escalation resolve <id> --status <s>   # operator: record an externally-run result`,
	GroupID: "workflow",
}

// ── escalation list ─────────────────────────────────────────────────────

var escalationListCmd = &cobra.Command{
	Use:   "list",
	Short: "List open escalations (gate-resolved by operators in Slack)",
	Long: `List escalations in the operator queue.

Default status filter is open + in_progress (the actionable queue). Pass
--status to widen (e.g. --status closed, or --status open,in_progress,closed).

--operation-type keeps any of the given types, --label keeps escalations
carrying ALL the given labels, and --exclude-label drops those carrying ANY.
All three are comma-separated or repeatable and narrow within your read scope.

JSON output (--json) is a bare array of objects with the fields:
  id, title, priority, status, created_by, created_at, age_minutes,
  labels, operation_type, operation_spec, requesting_task, requesting_agent,
  held_by
so a caller that filters on type or label needs no kd show per escalation.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := client.ListEscalationsOptions{}
		opts.Status, _ = cmd.Flags().GetString("status")
		opts.Limit, _ = cmd.Flags().GetInt("limit")
		opts.OperationTypes, _ = cmd.Flags().GetStringSlice("operation-type")
		opts.Labels, _ = cmd.Flags().GetStringSlice("label")
		opts.ExcludeLabels, _ = cmd.Flags().GetStringSlice("exclude-label")

		escs, err := beadsClient.ListEscalations(cmd.Context(), opts)
		if err != nil {
			return handleEscalationAuthErr(err, "listing escalations")
		}

		rows := make([]escalationRow, 0, len(escs))
		for i := range escs {
			// The daemon filters too; this pass keeps the result right against
			// a daemon that predates the filters and ignores their params.
			if r := escalationRowFrom(&escs[i]); r.matches(opts) {
				rows = append(rows, r)
			}
		}

		if jsonOutput {
			emitJSON(rows)
			return nil
		}

		if len(rows) == 0 {
			fmt.Println("No escalations found.")
			return nil
		}
		printEscalationRows(rows)
		return nil
	},
}

// escalationRow is the flat projection emitted by `kd escalation list --json`:
// the bead identity fields (ka-0000007156) plus the labels and escalation
// fields a queue reader filters on (gd-CndLtkZ5h0), so the merge desk needs one
// list call rather than a kd show per escalation. Everything here is already in
// the list response, which the daemon scopes exactly as it scopes kd show.
type escalationRow struct {
	ID              string          `json:"id"`
	Title           string          `json:"title"`
	Priority        int             `json:"priority"`
	Status          string          `json:"status"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       string          `json:"created_at"`
	AgeMinutes      int64           `json:"age_minutes"`
	Labels          []string        `json:"labels"`
	OperationType   string          `json:"operation_type"`
	OperationSpec   json.RawMessage `json:"operation_spec,omitempty"`
	RequestingTask  string          `json:"requesting_task,omitempty"`
	RequestingAgent string          `json:"requesting_agent,omitempty"`
	HeldBy          string          `json:"held_by,omitempty"`
}

// matches reports whether the row passes opts' operation-type and label filters.
func (r escalationRow) matches(opts client.ListEscalationsOptions) bool {
	if len(opts.OperationTypes) > 0 && !slices.Contains(opts.OperationTypes, r.OperationType) {
		return false
	}
	for _, l := range opts.Labels {
		if !slices.Contains(r.Labels, l) {
			return false
		}
	}
	for _, l := range opts.ExcludeLabels {
		if slices.Contains(r.Labels, l) {
			return false
		}
	}
	return true
}

// escalationRowFrom flattens an EscalationDetail into the list projection. The
// authoritative identity/status/timestamp fields live on the `issue` bead (the
// daemon puts only escalation-specific fields under `escalation`), so we read
// from Issue.
func escalationRowFrom(d *client.EscalationDetail) escalationRow {
	var r escalationRow
	if d == nil || d.Issue == nil {
		return r
	}
	b := d.Issue
	r.ID = b.ID
	r.Title = b.Title
	r.Priority = b.Priority
	r.Status = string(b.Status)
	r.CreatedBy = b.CreatedBy
	if !b.CreatedAt.IsZero() {
		r.CreatedAt = b.CreatedAt.UTC().Format(time.RFC3339)
		r.AgeMinutes = int64(time.Since(b.CreatedAt).Minutes())
	}
	r.Labels = b.Labels
	if r.Labels == nil {
		r.Labels = []string{}
	}
	// The escalation-specific fields come from the bead's Fields, the same
	// source the daemon's `escalation` projection is extracted from.
	var f struct {
		OperationType   string          `json:"operation_type"`
		OperationSpec   json.RawMessage `json:"operation_spec"`
		RequestingTask  string          `json:"requesting_task"`
		RequestingAgent string          `json:"requesting_agent"`
		HeldBy          string          `json:"held_by"`
	}
	if len(b.Fields) > 0 && json.Unmarshal(b.Fields, &f) == nil {
		r.OperationType = f.OperationType
		r.OperationSpec = f.OperationSpec
		r.RequestingTask = f.RequestingTask
		r.RequestingAgent = f.RequestingAgent
		r.HeldBy = f.HeldBy
	}
	if r.OperationType == "" {
		r.OperationType, _ = d.Escalation["operation_type"].(string)
	}
	return r
}

func printEscalationRows(rows []escalationRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPRI\tSTATUS\tAGE\tOP\tCREATED_BY\tTITLE")
	for _, r := range rows {
		title := r.Title
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		createdBy := r.CreatedBy
		if createdBy == "" {
			createdBy = "-"
		}
		op := r.OperationType
		if op == "" {
			op = "-"
		}
		fmt.Fprintf(w, "%s\tP%d\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Priority, r.Status, formatAgeMinutes(r.AgeMinutes), op, createdBy, title)
	}
	w.Flush()
}

// formatAgeMinutes renders an age (in minutes) compactly: m under an hour, h
// under a day, else d. Mirrors gb escalation list's age column.
func formatAgeMinutes(mins int64) string {
	switch {
	case mins < 0:
		return "-"
	case mins < 60:
		return fmt.Sprintf("%dm", mins)
	case mins < 24*60:
		return fmt.Sprintf("%dh", mins/60)
	default:
		return fmt.Sprintf("%dd", mins/(24*60))
	}
}

// ── escalation show ─────────────────────────────────────────────────────

var escalationShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show full detail for one escalation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		d, err := beadsClient.GetEscalation(cmd.Context(), id)
		if err != nil {
			return handleEscalationAuthErr(err, fmt.Sprintf("getting escalation %s", id))
		}
		if d.Issue == nil {
			return fmt.Errorf("escalation %s not found", id)
		}

		if jsonOutput {
			// Emit the full detail (both the escalation-fields projection and
			// the underlying issue bead) so `--json | jq` sees everything.
			emitJSON(d)
			return nil
		}

		printBeadTable(d.Issue)
		return nil
	},
}

// ── escalation approve ──────────────────────────────────────────────────

var escalationApproveCmd = &cobra.Command{
	Use:   "approve <id>",
	Short: "Operator: approve an escalation (auto-replays the denied operation)",
	Long: `Approve an escalation, triggering the daemon's auto-replay of the denied
operation (POST /v1/escalations/{id}/approve).

A person never approves from kd: the daemon accepts a person's approval only
from the escalation's page (browser or phone), so on a refusal kd prints that
page's link and approves nothing. What kd can approve with is a coordinator
lane's own principal (a machine client or a role agent) on an escalation its
lane holds. If you filed a work-blocking escalation and need it acted on, that
is the operator's or its lane's job; do not try to self-approve.

The approver identity comes from --as, else $KD_ACTOR, else --actor.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		approvedBy, _ := cmd.Flags().GetString("as")
		if approvedBy == "" {
			approvedBy = os.Getenv("KD_ACTOR")
		}
		if approvedBy == "" {
			approvedBy = actor
		}
		if approvedBy == "" {
			return fmt.Errorf("--as is required (or set KD_ACTOR)")
		}

		if err := requireOpenEscalation(cmd.Context(), id); err != nil {
			return err
		}

		req := client.ApproveEscalationRequest{
			ApprovedBy: approvedBy,
			// The daemon derives approval_method (P7 Task 15); older daemons
			// still read it from the body.
			ApprovalMethod: "cli",
		}
		if until, _ := cmd.Flags().GetString("until"); until != "" {
			t, err := time.Parse(time.RFC3339, until)
			if err != nil {
				return fmt.Errorf("parsing --until: %w", err)
			}
			req.GrantUntil = &t
		}
		req.Comment, _ = cmd.Flags().GetString("comment")

		if err := beadsClient.ApproveEscalation(cmd.Context(), id, req); err != nil {
			return handOnToThePage(err, fmt.Sprintf("approving escalation %s", id), id)
		}

		if jsonOutput {
			emitJSON(map[string]any{"id": id, "approved_by": approvedBy, "method": "cli"})
		} else {
			fmt.Printf("Approved %s (by %s)\n", id, approvedBy)
		}
		return nil
	},
}

// ── escalation reject ───────────────────────────────────────────────────

var escalationRejectCmd = &cobra.Command{
	Use:   "reject <id>",
	Short: "Operator: reject an escalation (closes as failure, no side effect)",
	Long: `Reject an escalation, closing it as result_status=failure with an audit
trail and no side-effect replay (POST /v1/escalations/{id}/reject).

OPERATOR-ONLY: the endpoint is RoleOperator-gated, so an ordinary agent token
gets a 403. An escalation on the owner floor is rejected only on its page
(browser or phone); kd prints the link. The rejector identity comes from --as,
else $KD_ACTOR, else --actor.`,
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
		if reason == "" {
			reason = "Rejected via kd"
		}

		if err := requireOpenEscalation(cmd.Context(), id); err != nil {
			return err
		}

		if err := beadsClient.RejectEscalation(cmd.Context(), id, client.RejectEscalationRequest{
			RejectedBy: rejectedBy,
			Reason:     reason,
		}); err != nil {
			return handOnToThePage(err, fmt.Sprintf("rejecting escalation %s", id), id)
		}

		if jsonOutput {
			emitJSON(map[string]any{"id": id, "rejected_by": rejectedBy, "reason": reason})
		} else {
			fmt.Printf("Rejected %s (by %s): %s\n", id, rejectedBy, reason)
		}
		return nil
	},
}

// ── escalation resolve ──────────────────────────────────────────────────

var escalationResolveCmd = &cobra.Command{
	Use:     "resolve <id>",
	Aliases: []string{"result", "close"},
	Short:   "Operator: record an externally-run result and close the escalation",
	Long: `Record the result of an escalation you executed out of band and close it
(POST /v1/escalations/{id}/result). Use this when the privileged action ran
somewhere else and you are just capturing the outcome — 'approve' is what runs
the daemon's auto-replay.

'resolve' is spelled distinctly from the generic 'kd close' on purpose: a
generic close of an OPEN escalation is refused by the daemon (see task
ka-0000005516) because it would skip operation_spec replay and wrongly release
close-on-escalation wrappers. Use 'resolve' (or 'reject') instead.

OPERATOR-ONLY: the endpoint is RoleOperator-gated, so an ordinary agent token
gets a 403. --status is required (success, failure, or partial).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		resultStatus, _ := cmd.Flags().GetString("status")
		resultJSON, _ := cmd.Flags().GetString("result")
		executor, _ := cmd.Flags().GetString("executor")

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

		req := client.CaptureEscalationResultRequest{
			ResultStatus: resultStatus,
			Executor:     executor,
		}
		if resultJSON != "" {
			var v any
			if err := json.Unmarshal([]byte(resultJSON), &v); err != nil {
				return fmt.Errorf("--result must be valid JSON: %w", err)
			}
			req.Result = v
		}

		if err := beadsClient.CaptureEscalationResult(cmd.Context(), id, req); err != nil {
			return handleEscalationAuthErr(err, fmt.Sprintf("resolving escalation %s", id))
		}

		if jsonOutput {
			emitJSON(map[string]any{"id": id, "result_status": resultStatus, "executor": executor})
		} else {
			fmt.Printf("Resolved %s (%s)\n", id, resultStatus)
		}
		return nil
	},
}

// ── escalation claim / hand-up ──────────────────────────────────────────

var escalationClaimCmd = &cobra.Command{
	Use:   "claim <id>",
	Short: "Claim an escalation your held_by lane holds (status unchanged)",
	Long: `Claim an escalation (POST /v1/escalations/{id}/claim). Only a member of the
escalation's held_by lane may claim; the daemon refuses anyone else with 403.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		if err := requireOpenEscalation(cmd.Context(), id); err != nil {
			return err
		}
		b, err := beadsClient.ClaimEscalation(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("claiming escalation %s: %w", id, err)
		}
		if jsonOutput {
			emitJSON(b)
		} else {
			fmt.Printf("Claimed %s\n", id)
		}
		return nil
	},
}

var escalationHandUpCmd = &cobra.Command{
	Use:   "hand-up <id>",
	Short: "Hand an escalation up to the operator (or the land owner)",
	Long: `Hand an escalation your held_by lane holds up one level
(POST /v1/escalations/{id}/hand-up): to the operator, or for a tenant land to
its owner. --reason is required and re-notifies the new holder.

--operator-class 1|2|3 is the merge desk's OPERATOR-ONLY class on a PR merge
(docs/merge-desk.md, "Sensitive paths"): the merge goes to the operator on the
owner floor (policy_rule floor:operator_class_merge), so only the owner approves
it, and the desk merges once they do.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		reason, _ := cmd.Flags().GetString("reason")
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--reason is required")
		}
		if err := requireOpenEscalation(cmd.Context(), id); err != nil {
			return err
		}
		class, _ := cmd.Flags().GetString("operator-class")
		b, err := beadsClient.HandUpEscalation(cmd.Context(), id, client.HandUpEscalationRequest{Reason: reason, OperatorClass: class})
		if err != nil {
			return fmt.Errorf("handing up escalation %s: %w", id, err)
		}
		if jsonOutput {
			emitJSON(b)
		} else {
			fmt.Printf("Handed up %s: %s\n", id, reason)
		}
		return nil
	},
}

// requireOpenEscalation verifies id is an escalation still open/in_progress
// BEFORE calling a resolve verb, so approve/reject fail fast with a clear
// message on an already-resolved or wrong-type bead instead of racing the
// daemon. Best-effort: a read failure (e.g. 403 on the read surface) is NOT
// fatal here — the subsequent write call surfaces the authoritative error.
func requireOpenEscalation(ctx context.Context, id string) error {
	d, err := beadsClient.GetEscalation(ctx, id)
	if err != nil {
		return nil // let the write path report the authoritative error
	}
	if d.Issue == nil {
		return fmt.Errorf("escalation %s not found", id)
	}
	if d.Issue.Type != model.TypeEscalation {
		return fmt.Errorf("%s is not an escalation (type=%s)", id, d.Issue.Type)
	}
	switch d.Issue.Status {
	case model.StatusOpen, model.StatusInProgress:
		return nil
	default:
		return fmt.Errorf("escalation %s is already %s", id, d.Issue.Status)
	}
}

// handleEscalationAuthErr wraps a client error, turning a 403 on the operator-
// gated resolve endpoints into an actionable message (escalations are an
// operator surface; agents file a gb decision for work-blocking questions)
// rather than a bare "HTTP 403". Non-403 errors are wrapped with the action
// context unchanged.
func handleEscalationAuthErr(err error, action string) error {
	if err == nil {
		return nil
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 403 {
		return fmt.Errorf("%s: %w\n  escalations are resolved by OPERATORS — an agent token cannot approve/reject/resolve them.\n  If work is blocked on a decision, file 'gb decision create' instead", action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

// handOnToThePage is approve and reject's 403 path (P10.B gd-4mEAhIE01U): a
// person approves or rejects only on the escalation's page, so kd prints that
// link (approve_url under --json) and fails, having approved nothing. Other
// errors go to handleEscalationAuthErr.
func handOnToThePage(err error, action, id string) error {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 403 {
		return handleEscalationAuthErr(err, action)
	}
	page := escalationPageURL(id)
	if jsonOutput {
		emitJSON(map[string]any{"id": id, "approve_url": page, "refused": apiErr.Message})
	} else if page != "" {
		fmt.Printf("Approve or reject %s on its page, in the browser or on a phone:\n  %s\n", id, page)
	} else {
		fmt.Printf("Approve or reject %s on its page in the garden (set BEADS_VIEWER_URL to get the link).\n", id)
	}
	return fmt.Errorf("%s: %w\n  kd never approves or rejects for a person; that happens on the page above.\n  Escalations are resolved by OPERATORS or their coordinator lanes; an agent blocked on an answer files 'gb decision create' instead", action, err)
}

// escalationPageURL is the escalation's page: $BEADS_VIEWER_URL/<id>, the link
// Slack's escalation cards carry (bridge.EscalationReviewURL), else the same
// path on the daemon's https host (https://<host>/kd serves the garden at
// https://<host>/). "" when neither is known.
func escalationPageURL(id string) string {
	base := strings.TrimRight(os.Getenv("BEADS_VIEWER_URL"), "/")
	if base == "" {
		if u, err := url.Parse(httpURL); err == nil && u.Scheme == "https" && u.Host != "" {
			base = "https://" + u.Host
		}
	}
	if base == "" {
		return ""
	}
	return base + "/" + url.PathEscape(id)
}

func init() {
	escalationListCmd.Flags().String("status", "", "filter by status (comma-separated; default: open,in_progress)")
	escalationListCmd.Flags().Int("limit", 0, "max escalations to return (0 = daemon default, 50)")
	escalationListCmd.Flags().StringSlice("operation-type", nil, "keep escalations whose operation_type is ANY of these (repeatable/comma; e.g. github_pr_merge,gitlab_mr_merge)")
	escalationListCmd.Flags().StringSliceP("label", "l", nil, "keep escalations carrying ALL of these labels (repeatable/comma)")
	escalationListCmd.Flags().StringSlice("exclude-label", nil, "drop escalations carrying ANY of these labels (repeatable/comma; e.g. merge-desk:seen)")

	escalationApproveCmd.Flags().String("as", "", "approver identity (default: $KD_ACTOR or --actor)")
	escalationApproveCmd.Flags().String("comment", "", "optional note recorded with the approval, as a comment on the escalation by you")
	escalationApproveCmd.Flags().String("until", "",
		"grant expiry for a taint grant escalation (RFC3339, e.g. 2025-03-01T00:00:00Z); defaults to 7 days (docs/adr/taints.md §10.4); ignored for every other operation_type")

	escalationRejectCmd.Flags().String("as", "", "rejector identity (default: $KD_ACTOR or --actor)")
	escalationRejectCmd.Flags().String("reason", "", "rejection reason (recorded on the bead)")

	escalationResolveCmd.Flags().String("status", "", "result status: success, failure, partial (required)")
	escalationResolveCmd.Flags().String("result", "", "result data as JSON")
	escalationResolveCmd.Flags().String("executor", "", "executor identity (default: $KD_ACTOR or --actor)")

	escalationCmd.AddCommand(escalationListCmd)
	escalationCmd.AddCommand(escalationShowCmd)
	escalationCmd.AddCommand(escalationApproveCmd)
	escalationCmd.AddCommand(escalationRejectCmd)
	escalationCmd.AddCommand(escalationResolveCmd)
	escalationHandUpCmd.Flags().String("reason", "", "why this needs the next level up (required)")
	escalationHandUpCmd.Flags().String("operator-class", "", "the merge desk's OPERATOR-ONLY class (1, 2 or 3): puts a PR merge on the owner floor")
	escalationCmd.AddCommand(escalationClaimCmd)
	escalationCmd.AddCommand(escalationHandUpCmd)
}
