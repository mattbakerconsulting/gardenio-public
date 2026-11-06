package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// Escalation audience (gd-bmOiteQi36): owner | operator | desk, the shared
// rules in internal/beadsapi/escalation_audience.go. The daemon stamps it at
// create (a filer may only ask for owner) and freezes it; route re-routes it
// through POST /v1/escalations/{id}/audience (operators and admins only), and
// backfill-audience has the daemon stamp the escalations filed before it did.

const audienceFlagHelp = `Send the escalation to the owner (the only value). The daemon decides who an escalation is for: desk for a merge (gitlab_mr_merge), operator for everything else, and owner for one that needs credentials (--credentials), is --sensitivity secret or names an approver. Say owner for a product or policy choice, anything only the owner can do (passkey/MFA, GitHub sudo, legal), or spending money. Fixed once filed`

// escalationCreateAudience is the audience gb escalation create asks for:
// owner, or nothing (the daemon derives it).
func escalationCreateAudience(flag string) (string, error) {
	switch flag = strings.ToLower(strings.TrimSpace(flag)); flag {
	case "", beadsapi.AudienceOwner:
		return flag, nil
	}
	return "", fmt.Errorf("--audience takes only owner: the daemon stamps desk for a merge and operator for everything else (and owner when the escalation needs credentials, is secret or names an approver)")
}

// escalationAudienceLabel is the audience as gb escalation show prints it,
// saying why when the forced-owner rule or the default decided it.
func escalationAudienceLabel(f map[string]string) string {
	a := beadsapi.EscalationAudience(f)
	switch {
	case beadsapi.EscalationAudienceForced(f):
		return a + " (forced: needs credentials, is secret or names an approver)"
	case !beadsapi.ValidAudience(strings.ToLower(strings.TrimSpace(f["audience"]))):
		return a + " (default for " + f["operation_type"] + ")"
	}
	return a
}

// audienceUnknown reports whether err is a daemon's 400 refusing the audience
// field as unknown: a daemon that predates it.
func audienceUnknown(err error) bool {
	var ae *beadsapi.APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusBadRequest &&
		strings.Contains(ae.Message, "audience") && strings.Contains(ae.Message, "unknown field")
}

var escalationRouteCmd = &cobra.Command{
	Use:   "route <escalation-id>",
	Short: "Re-route an escalation to the owner, the operators or the merge desk",
	Long: `Change who an escalation is for (its audience): owner, operator or desk.
Only owner-audience escalations show in full on the owner's Commons and
beads-viewer and ping the owner; operator and desk ones fold into the
"In hand" line and go to their own channels. Approval is unchanged: anyone who
could approve it before still can.

Only a gardenio-operator or admin, signed in as themselves, may re-route; the
daemon refuses an agent token. An escalation that needs credentials or is
secret, or names an approver, always stays with the owner. The daemon records
the change as a comment.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		to, _ := cmd.Flags().GetString("audience")
		reason, _ := cmd.Flags().GetString("reason")
		to = strings.ToLower(strings.TrimSpace(to))
		if !beadsapi.ValidAudience(to) {
			return fmt.Errorf("--audience must be one of: %s", strings.Join(beadsapi.Audiences, ", "))
		}
		ctx := cmd.Context()
		id := args[0]
		b, err := daemon.GetBead(ctx, id)
		if err != nil {
			return fmt.Errorf("getting %s: %w", id, err)
		}
		if b.Type != "escalation" {
			return fmt.Errorf("%s is a %s, not an escalation", id, b.Type)
		}
		if beadsapi.EscalationAudienceForced(b.Fields) && to != beadsapi.AudienceOwner {
			return fmt.Errorf("%s needs credentials, is secret or names an approver, so it always goes to the owner and cannot be routed to %s", id, to)
		}
		from := beadsapi.EscalationAudience(b.Fields)
		if from == to && b.Fields["audience"] == to {
			fmt.Printf("%s is already for %s\n", id, to)
			return nil
		}
		got, err := daemon.RouteEscalation(ctx, id, beadsapi.RouteEscalationRequest{Audience: to, Reason: strings.TrimSpace(reason)})
		if err != nil {
			return fmt.Errorf("re-routing %s (only a gardenio-operator or admin signed in as themselves may): %w", id, err)
		}
		fmt.Printf("%s: %s → %s\n", id, from, got)
		return nil
	},
}

var escalationBackfillAudienceCmd = &cobra.Command{
	Use:   "backfill-audience",
	Short: "Stamp the audience on open escalations filed before it existed",
	Long: `One-shot backfill: for every OPEN/IN-PROGRESS escalation whose stored audience
is missing, invalid, or differs from its effective one, the daemon stamps the
audience it derives: owner for one that needs credentials, is secret, names an
approver or needs an admin to approve; else a valid stored audience (a filer's
or router's choice); else the default from operation_type.

Every surface already reads an unstamped escalation through the same defaults,
so this only makes the stored field match. Writing audience is an operator
action: run it signed in as a gardenio-operator or admin. Idempotent; bounded
at 200 escalations; --dry-run previews.`,
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
		stamped, skipped, failed := 0, 0, 0
		var tally beadsapi.InHand
		owner := 0
		for _, b := range open.Beads {
			want := beadsapi.EscalationAudience(b.Fields)
			if want == beadsapi.AudienceOwner {
				owner++
			}
			tally.Add(want)
			if b.Fields["audience"] == want {
				skipped++
				continue
			}
			if dryRun {
				fmt.Printf("would stamp: %s audience=%s (%s) — %s\n", b.ID, want, b.Fields["operation_type"], b.Title)
				stamped++
				continue
			}
			got, err := daemon.RouteEscalation(ctx, b.ID, beadsapi.RouteEscalationRequest{Reason: "backfill-audience"})
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: %s: %v\n", b.ID, err)
				failed++
				continue
			}
			if got != want {
				fmt.Printf("%s: the daemon stamped %s (gb read %s)\n", b.ID, got, want)
			}
			stamped++
		}
		verb := "stamped"
		if dryRun {
			verb = "would stamp"
		}
		fmt.Printf("%s %d, already right %d, failed %d; owner %d, desk %d, operator %d\n", verb, stamped, skipped, failed, owner, tally.Desk, tally.Operator)
		if failed > 0 {
			return fmt.Errorf("%d escalation(s) could not be stamped (re-run signed in as a gardenio-operator or admin)", failed)
		}
		return nil
	},
}

func init() {
	escalationRouteCmd.Flags().String("audience", "", "owner, operator or desk (required)")
	escalationRouteCmd.Flags().String("reason", "", "why, recorded in the audit comment")
	escalationBackfillAudienceCmd.Flags().Bool("dry-run", false, "Preview without writing")
	escalationCmd.AddCommand(escalationRouteCmd)
	escalationCmd.AddCommand(escalationBackfillAudienceCmd)
}
