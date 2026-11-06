package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// adminForgetOperatorCmd drives the operator right-to-be-forgotten soft-delete
// (ka-0000005150). It replaces the raw
// `UPDATE operators SET forgotten_at=now(), slug=NULL` the 2026-06-16 operator
// cleanup (ka-0000006421) had to run by hand: the server stamps forgotten_at
// (the redaction marker ResolveOperators consumes), nulls slug so the row drops
// out of the /people directory, retires the operator's live slug aliases (so
// /u/{slug} still resolves to the redacted operator rather than 404ing), and
// records the acting principal + reason as the auditable event of the forget.
//
// The opaque operator_id and every operated_by_id audit FK are left intact, so
// historical metric series stay attributable to the now-anonymous actor.
//
// Idempotent — forgetting an already-forgotten operator is a no-op reported as
// already_forgotten. Guard — when the operator owns more than --threshold audit
// rows the server refuses with a 409 unless --confirm is passed, so a
// fat-fingered id cannot silently mass-redact a heavily-attributed human.
var adminForgetOperatorCmd = &cobra.Command{
	Use:   "forget-operator <operator-id>",
	Short: "Soft-delete an operator for a right-to-be-forgotten request (forgotten_at + null slug)",
	Long: `Soft-deletes an operator for a right-to-be-forgotten request (task
ka-0000005150). The server:

  - stamps operators.forgotten_at (the redaction marker the chart backend's
    ResolveOperators consumes — display/email resolve to a redacted placeholder
    on every dashboard at once),
  - nulls operators.slug so the row drops out of the /people directory feed,
  - retires the operator's live operator_slug_aliases rows so existing
    /u/{slug} links still resolve to the (now redacted) operator instead of
    404ing,
  - records the acting principal (server-derived from your auth identity, never
    client-supplied) and --reason as the auditable event of the forget.

The opaque operator_id and every operated_by_id audit FK are LEFT INTACT — the
row is never deleted — so historical metric series stay attributable to the now
anonymous actor.

Idempotent: forgetting an already-forgotten operator is a no-op (reported as
already_forgotten with the original forget time).

Guard: when the operator owns more than --threshold (default 1000) audit rows
across beads/comments/deps/events/mutation_history, the server REFUSES with a
409 unless you pass --confirm — so a wrong id cannot silently mass-redact a
heavily-attributed human. Re-run with --confirm once you have verified the id.

Output is a JSON result (operator_id, already_forgotten, forgotten_at,
slug_cleared, slug_aliases_retired, audit_rows) — capture stdout for the audit
trail.`,
	Args: cobra.ExactArgs(1),
	RunE: runAdminForgetOperator,
}

func init() {
	adminForgetOperatorCmd.Flags().String("reason", "", "optional free-text justification recorded as forgotten_reason (e.g. an erasure-request ref)")
	adminForgetOperatorCmd.Flags().Bool("confirm", false, "proceed even when the operator owns a large audit history (bypass the guard)")
	adminForgetOperatorCmd.Flags().Int("threshold", 0, "audit-row count above which --confirm is required (0 = server default of 1000)")
	adminCmd.AddCommand(adminForgetOperatorCmd)
}

func runAdminForgetOperator(cmd *cobra.Command, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("invalid operator id %q: must be a positive integer", args[0])
	}
	reason, _ := cmd.Flags().GetString("reason")
	confirm, _ := cmd.Flags().GetBool("confirm")
	threshold, _ := cmd.Flags().GetInt("threshold")

	resp, err := beadsClient.ForgetOperator(context.Background(), id, &client.ForgetOperatorRequest{
		Reason:    reason,
		Confirm:   confirm,
		Threshold: threshold,
	})
	if err != nil {
		return fmt.Errorf("forget-operator %d: %w", id, err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return nil
}
