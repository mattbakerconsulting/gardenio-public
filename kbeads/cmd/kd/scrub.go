package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// scrub.go gives kd both surfaces of the scrub-request lifecycle:
// - 'request' files a new scrub-request against a taint (docs/adr/taints.md
//   §6.2, §7), restricted to a grant holder for the named taint.
// - 'show'/'approve'/'reject' resolve an existing one (§6.3, gd-DyWyWFxnzC),
//   mirroring escalation.go's resolve verbs.
//
// Unlike escalation approve/reject, there is NO --as flag on approve/reject:
// the approver/rejector identity is ALWAYS server-derived from the caller's
// own OIDC identity (taints ADR §6.3 correction 1) — offering a
// client-suppliable identity flag would invite the belief that it's
// honored, when the server only logs it. reject additionally requires
// --reason.
//
// scrubCmd is a parent group, but it MUST be runnable (have a RunE) so that
// cobra reaches the Args validator below — a non-runnable parent
// short-circuits to flag.ErrHelp BEFORE ValidateArgs runs, printing help to
// STDOUT and exiting 0 (ka-0000005587 / ka-0000003820 shape). With a RunE
// present, cobra.NoArgs rejects any UNKNOWN subcommand with a non-zero exit;
// a bare `kd scrub` passes NoArgs and falls through to RunE → help.
var scrubCmd = &cobra.Command{
	Use:     "scrub",
	Aliases: []string{"scrub-request", "scrub-requests"},
	Short:   "File and resolve scrub-request beads (taint detaint/purge)",
	Long: `File or resolve a scrub-request bead (docs/adr/taints.md §6.2, §6.3, §7).

  kd scrub request --taint <name> --kind <detaint|purge> --scope <...>
                                         # grant holder: file a new request
  kd scrub show <id>                    # full detail for one scrub-request
  kd scrub approve <id>                 # operator: approve (may require four-eyes)
  kd scrub reject  <id> --reason <why>  # operator: deny, reason required

'approve' and 'reject' hit RoleOperator-gated endpoints that require a
DIRECTLY authenticated OIDC identity. An ordinary agent token, and a
static/fleet token even when it relays a human identity via X-Operated-By,
both get a 403 by design — the X-Operated-By relay that escalation approve
honors is deliberately NOT accepted here, since scrub is the destructive arm
of taints and honoring it would let any holder of a shared static token name
an arbitrary approver. The approver/rejector identity always comes from your
own authenticated OIDC session — there is no --as flag, unlike 'kd escalation'.`,
	GroupID: "workflow",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

// ── scrub request ───────────────────────────────────────────────────────

var scrubRequestCmd = &cobra.Command{
	Use:   "request",
	Short: "File a scrub request against a taint",
	Long: `File a scrub request asking the taint system to detaint or purge every bead
named by --scope (docs/adr/taints.md §6.2, §7). Restricted to a holder of a
grant for --taint (or the taint owner team/admin).

There is no --mode/--detaint-mode flag: for a detaint request, whether the
original bead forks a clean successor, is stripped in place, or has the
taint simply lifted is decided by the named taint's own
scrub_policy.detaint_mode (set by the taint owner via
"kd taint create/update --policy"), never by the requester. The create
response echoes that taint's CURRENT detaint_mode read-only so you can see
up front what a detaint request will trigger, before it ever reaches
approval.

detaint_mode "lift" (docs/adr/taints.md §15.4) removes the bead_taints
marker in place with no content cascade — legal only for a taint whose
effects exclude "gate", since an integrity taint has no pre-image to
protect. It is NOT the right tool for a high-rate integrity taint: §15.4
requires that case to be lifted by the approval that already exists for the
underlying action (a merge, a plan approve), not by filing a per-bead
"kd scrub request" here — a scrub-request is scoped to a fixed set of beads
resolved once at file time, the wrong shape for something that needs to
fire on every occurrence of an ongoing action.

A fresh request is inert: it does nothing until it clears the taint's
approval policy and the scrub execution replay runs it. The response's
resolved_bead_ids is the scope evaluated once and frozen at file time — the
fixed blast radius the request names, for review before approval.

--scope accepts either a raw JSON scope object, e.g.
  --scope '{"bead_ids":["kd-abc123","kd-def456"]}'
  --scope '{"older_than_days":30,"labels_any":["project:acme"]}'
or the equivalent built from --bead-ids / --older-than-days / --labels-any.
--scope and the flag form are mutually exclusive; the two scope shapes
themselves (bead_ids vs. older_than_days/labels_any) remain mutually
exclusive as documented in §6.2.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := context.Background()

		taint, _ := cmd.Flags().GetString("taint")
		kind, _ := cmd.Flags().GetString("kind")
		if taint == "" {
			return fmt.Errorf("--taint is required")
		}
		if kind != "detaint" && kind != "purge" {
			return fmt.Errorf(`--kind must be "detaint" or "purge"`)
		}

		scope, err := buildScrubRequestScope(cmd)
		if err != nil {
			return err
		}

		req := &client.CreateScrubRequestRequest{
			Taint: taint,
			Kind:  kind,
			Scope: scope,
		}
		view, err := beadsClient.CreateScrubRequest(ctx, req)
		if err != nil {
			return fmt.Errorf("filing scrub request against taint %q: %w", taint, err)
		}

		if jsonOutput {
			emitJSON(view)
			return nil
		}
		fmt.Printf("Scrub Request: %s\n", view.ID)
		fmt.Printf("Taint:         %s\n", taint)
		fmt.Printf("Kind:          %s\n", kind)
		if view.DetaintMode != "" {
			fmt.Printf("Detaint Mode:  %s\n", view.DetaintMode)
		}
		var fields struct {
			ResolvedBeadIDs []string `json:"resolved_bead_ids"`
		}
		_ = json.Unmarshal(view.Fields, &fields)
		fmt.Printf("Resolved Beads (%d):\n", len(fields.ResolvedBeadIDs))
		for _, id := range fields.ResolvedBeadIDs {
			fmt.Printf("  %s\n", id)
		}
		return nil
	},
}

// buildScrubRequestScope resolves --scope into the scrub-request scope JSON,
// either verbatim from a raw JSON --scope value or built from the
// --bead-ids/--older-than-days/--labels-any flags. The two input styles are
// mutually exclusive; the daemon separately enforces that the resulting
// scope names only one of the two shapes §6.2 defines.
func buildScrubRequestScope(cmd *cobra.Command) (json.RawMessage, error) {
	scopeFlag, _ := cmd.Flags().GetString("scope")
	beadIDs, _ := cmd.Flags().GetStringSlice("bead-ids")
	labelsAny, _ := cmd.Flags().GetStringSlice("labels-any")
	olderThanSet := cmd.Flags().Changed("older-than-days")
	olderThanDays, _ := cmd.Flags().GetInt("older-than-days")

	hasFlagsShape := len(beadIDs) > 0 || len(labelsAny) > 0 || olderThanSet

	if scopeFlag != "" && hasFlagsShape {
		return nil, fmt.Errorf("--scope cannot be combined with --bead-ids/--older-than-days/--labels-any")
	}
	if scopeFlag != "" {
		if !json.Valid([]byte(scopeFlag)) {
			return nil, fmt.Errorf("--scope must be valid JSON")
		}
		return json.RawMessage(scopeFlag), nil
	}
	if !hasFlagsShape {
		return nil, fmt.Errorf(`--scope is required (either a JSON object, or one of --bead-ids / --older-than-days)`)
	}

	m := make(map[string]any, 3)
	if len(beadIDs) > 0 {
		m["bead_ids"] = beadIDs
	}
	if olderThanSet {
		m["older_than_days"] = olderThanDays
	}
	if len(labelsAny) > 0 {
		m["labels_any"] = labelsAny
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding scope: %w", err)
	}
	return out, nil
}

// ── scrub show ──────────────────────────────────────────────────────────

var scrubShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show full detail for one scrub-request",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		bead, err := beadsClient.GetBead(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("getting scrub-request %s: %w", id, err)
		}
		if bead.Type != model.TypeScrubRequest {
			return fmt.Errorf("%s is not a scrub-request (type=%s)", id, bead.Type)
		}

		if jsonOutput {
			emitJSON(bead)
			return nil
		}
		printBeadTable(bead)
		return nil
	},
}

// ── scrub approve ───────────────────────────────────────────────────────

var scrubApproveCmd = &cobra.Command{
	Use:   "approve <id>",
	Short: "Operator: approve a scrub-request",
	Long: `Approve a scrub-request (POST /v1/scrub-requests/{id}/approve).

OPERATOR-ONLY, OIDC-ONLY: the endpoint is RoleOperator-gated and refuses any
caller with no OIDC-authenticated identity behind it, even one holding the
operator role via a static/fleet token. The approver is always your own
authenticated identity — never client-supplied.

When the taint's scrub_policy resolves to human-four-eyes for this request,
approval additionally requires that you differ from the original requester
(after operator-alias resolution) and that you are a member of the rule's
approvers.

--artifact <path> writes the docs/adr/taints.md §6.7 audit artifact (request
id, taint, kind, approver, mode, and per-bead {id, content_sha,
tables_touched}) to the named file on YOUR machine and nothing else — the
daemon never persists this artifact itself, only the taint_audit rows it
summarizes, so the file this flag writes is the only copy. Omit --artifact to
print the same JSON to stdout instead; no file is written in that case.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		reason, _ := cmd.Flags().GetString("reason")
		artifactPath, _ := cmd.Flags().GetString("artifact")

		if err := requireOpenScrubRequest(cmd.Context(), id); err != nil {
			return err
		}

		artifact, err := beadsClient.ApproveScrubRequest(cmd.Context(), id, client.ApproveScrubRequestRequest{
			ApprovalReason: reason,
		})
		if err != nil {
			return handleScrubAuthErr(err, fmt.Sprintf("approving scrub-request %s", id))
		}

		if artifactPath != "" {
			if artifact == nil {
				return fmt.Errorf("approving scrub-request %s: response carried no audit artifact (scrub-request was already executed)", id)
			}
			data, err := json.MarshalIndent(artifact, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling audit artifact: %w", err)
			}
			if err := os.WriteFile(artifactPath, data, 0o600); err != nil {
				return fmt.Errorf("writing audit artifact to %s: %w", artifactPath, err)
			}
			return nil
		}

		if artifact != nil {
			data, err := json.MarshalIndent(artifact, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling audit artifact: %w", err)
			}
			fmt.Println(string(data))
			return nil
		}

		if jsonOutput {
			emitJSON(map[string]any{"id": id, "status": "approved"})
		} else {
			fmt.Printf("Approved %s\n", id)
		}
		return nil
	},
}

// ── scrub reject ────────────────────────────────────────────────────────

var scrubRejectCmd = &cobra.Command{
	Use:   "reject <id>",
	Short: "Operator: reject a scrub-request (--reason required)",
	Long: `Reject a scrub-request, closing it as approval_status=denied (POST
/v1/scrub-requests/{id}/reject).

OPERATOR-ONLY, OIDC-ONLY — the same identity restrictions as 'scrub approve':
the endpoint refuses any caller with no OIDC-authenticated identity behind
it. --reason is required.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		reason, _ := cmd.Flags().GetString("reason")
		if reason == "" {
			return fmt.Errorf("--reason is required")
		}

		if err := requireOpenScrubRequest(cmd.Context(), id); err != nil {
			return err
		}

		if err := beadsClient.RejectScrubRequest(cmd.Context(), id, client.RejectScrubRequestRequest{
			Reason: reason,
		}); err != nil {
			return handleScrubAuthErr(err, fmt.Sprintf("rejecting scrub-request %s", id))
		}

		if jsonOutput {
			emitJSON(map[string]any{"id": id, "status": "denied", "reason": reason})
		} else {
			fmt.Printf("Rejected %s: %s\n", id, reason)
		}
		return nil
	},
}

// requireOpenScrubRequest verifies id is a still-open scrub-request before
// calling a resolve verb, so approve/reject fail fast with a clear message
// on an already-resolved or wrong-type bead instead of racing the daemon.
// Best-effort, mirroring requireOpenEscalation: a read failure (e.g. 403 on
// the read surface) is not fatal here — the write call surfaces the
// authoritative error.
func requireOpenScrubRequest(ctx context.Context, id string) error {
	bead, err := beadsClient.GetBead(ctx, id)
	if err != nil {
		return nil // let the write path report the authoritative error
	}
	if bead == nil {
		return fmt.Errorf("scrub-request %s not found", id)
	}
	if bead.Type != model.TypeScrubRequest {
		return fmt.Errorf("%s is not a scrub-request (type=%s)", id, bead.Type)
	}
	switch bead.Status {
	case model.StatusOpen, model.StatusInProgress:
		return nil
	default:
		return fmt.Errorf("scrub-request %s is already %s", id, bead.Status)
	}
}

func handleScrubAuthErr(err error, action string) error {
	if err == nil {
		return nil
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 403 {
		return fmt.Errorf("%s: %w\n  scrub-requests are resolved by OPERATORS over a directly-authenticated OIDC session — an agent token, or a static/fleet token even with a relayed OIDC identity, cannot approve/reject them", action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func init() {
	scrubApproveCmd.Flags().String("reason", "", "approval reason (recorded on the bead; optional)")
	scrubApproveCmd.Flags().String("artifact", "", "write the audit artifact (docs/adr/taints.md §6.7) to this path instead of stdout")
	scrubRejectCmd.Flags().String("reason", "", "rejection reason (recorded on the bead; required)")

	scrubRequestCmd.Flags().String("taint", "", "name of the taint this request concerns (required)")
	scrubRequestCmd.Flags().String("kind", "", `"detaint" or "purge" (required)`)
	scrubRequestCmd.Flags().String("scope", "", `scope as a raw JSON object, e.g. '{"bead_ids":["kd-abc123"]}'`)
	scrubRequestCmd.Flags().StringSlice("bead-ids", nil, "scope: explicit comma-separated bead ids (alternative to --scope)")
	scrubRequestCmd.Flags().Int("older-than-days", 0, "scope: beads older than N days (alternative to --scope; combine with --labels-any)")
	scrubRequestCmd.Flags().StringSlice("labels-any", nil, "scope: labels ORed against older-than-days (alternative to --scope)")

	scrubCmd.AddCommand(scrubRequestCmd)
	scrubCmd.AddCommand(scrubShowCmd)
	scrubCmd.AddCommand(scrubApproveCmd)
	scrubCmd.AddCommand(scrubRejectCmd)
}
