package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminBackfillUserBeadIDCmd resolves each existing type=schedule bead's
// OperatedBy back to a user bead and stamps Fields[user_bead_id]. Once the
// schedule carries user_bead_id, the gardenio scheduler propagates it onto
// every spawned task and agent bead at run time, so server-side
// auto-inheritance (ka-0000006800) no longer has to pull the value out of
// thin air. See task ka-0000004383.
//
// Resolution ladder (per bead):
//
//  1. If Fields[user_bead_id] is already set, skip — idempotent.
//  2. Prefer the joined OperatedByEmail column (set by the operators-table
//     LEFT JOIN). When non-empty, look up a user bead with Fields.email
//     equal to that email.
//  3. Otherwise parse OperatedBy as "<kind>:<subject>":
//     - kind=oidc / gitlab / jira → subject is the email; look up by
//     Fields.email.
//     - kind=slack → subject is the Slack user id; look up by
//     Fields.slack_id.
//  4. Anything else (system: / fleet: sentinels, empty OperatedBy, no
//     matching user bead) is reported as skipped and left untouched.
//
// --dry-run prints what would change but performs no writes.
var adminBackfillUserBeadIDCmd = &cobra.Command{
	Use:   "user-bead-id",
	Short: "Stamp Fields[user_bead_id] on existing schedule beads by resolving OperatedBy → user bead",
	Long: `Stamp Fields[user_bead_id] on existing type=schedule beads.

For each schedule with a non-empty OperatedBy column and no existing
Fields[user_bead_id], the command resolves the operator back to a user
bead (type=user, kind=config) and writes the resolved user-bead id into
Fields[user_bead_id]. Re-running on already-backfilled data is a no-op.

OIDC, gitlab, and jira operators are resolved by email; slack operators
by Fields.slack_id. Sentinel operators (system:*, fleet:*) and operators
with no matching user bead are reported as "no user bead resolved" and
skipped.

--dry-run prints what would change. Without --dry-run the command writes
the resolved user_bead_id back via UpdateBead, which merges into the
schedule bead's existing Fields.`,
	RunE: runAdminBackfillUserBeadID,
}

func init() {
	adminBackfillUserBeadIDCmd.Flags().Bool("dry-run", false, "show proposed changes without writing")
	adminBackfillUserBeadIDCmd.Flags().Int("page-size", 200, "schedule beads to fetch per page")
	adminBackfillCmd.AddCommand(adminBackfillUserBeadIDCmd)
}

// adminBackfillCmd is the parent for one-shot backfills under `kd admin
// backfill ...`. The legacy operators-FK backfill keeps its top-level
// `admin backfill-operators` name for compatibility.
var adminBackfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "One-shot backfills",
}

func init() {
	adminCmd.AddCommand(adminBackfillCmd)
}

// userBeadIDPlan is one schedule bead's proposed backfill action.
type userBeadIDPlan struct {
	BeadID     string
	Title      string
	OperatedBy string
	Email      string
	SlackID    string
	UserBeadID string // resolved user bead, empty when no match
	Reason     string // why skipped (only set when UserBeadID == "")
}

func runAdminBackfillUserBeadID(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	pageSize, _ := cmd.Flags().GetInt("page-size")
	if pageSize <= 0 {
		pageSize = 200
	}

	ctx := context.Background()

	// Page through all schedule beads. Status is unfiltered — disabled and
	// in_progress schedules still need user_bead_id when they fire next.
	var schedules []*model.Bead
	offset := 0
	for {
		resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
			Type:   []string{"schedule"},
			Limit:  pageSize,
			Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("listing schedules: %w", err)
		}
		if len(resp.Beads) == 0 {
			break
		}
		schedules = append(schedules, resp.Beads...)
		if len(resp.Beads) < pageSize {
			break
		}
		offset += len(resp.Beads)
	}

	if len(schedules) == 0 {
		fmt.Println("0 schedule beads found")
		return nil
	}

	userByEmail := make(map[string]string)
	userBySlackID := make(map[string]string)

	var plans []userBeadIDPlan
	var alreadySet, noOperator, resolved, unresolved int
	for _, b := range schedules {
		// Parse Fields and short-circuit when user_bead_id is already set.
		fields := decodeFields(b.Fields)
		if existing, _ := fields["user_bead_id"].(string); existing != "" {
			alreadySet++
			continue
		}

		operatedBy := b.OperatedBy
		if operatedBy == "" {
			// Some legacy schedules stashed the value in Fields.
			if v, ok := fields["operated_by"].(string); ok {
				operatedBy = v
			}
		}
		if operatedBy == "" {
			noOperator++
			plans = append(plans, userBeadIDPlan{
				BeadID: b.ID, Title: b.Title,
				Reason: "no operated_by",
			})
			continue
		}

		email, slackID := resolveOperatorLookup(operatedBy, b.OperatedByEmail)
		if email == "" && slackID == "" {
			unresolved++
			plans = append(plans, userBeadIDPlan{
				BeadID: b.ID, Title: b.Title, OperatedBy: operatedBy,
				Reason: "sentinel operator (system:/fleet:) or unrecognized kind",
			})
			continue
		}

		userID, err := lookupUserBead(ctx, email, slackID, userByEmail, userBySlackID)
		if err != nil {
			return fmt.Errorf("looking up user bead for schedule %s: %w", b.ID, err)
		}
		if userID == "" {
			unresolved++
			plans = append(plans, userBeadIDPlan{
				BeadID: b.ID, Title: b.Title, OperatedBy: operatedBy,
				Email: email, SlackID: slackID,
				Reason: "no user bead matches",
			})
			continue
		}
		resolved++
		plans = append(plans, userBeadIDPlan{
			BeadID: b.ID, Title: b.Title, OperatedBy: operatedBy,
			Email: email, SlackID: slackID, UserBeadID: userID,
		})
	}

	printUserBeadIDPlans(plans, dryRun)

	verb := "updated"
	if dryRun {
		verb = "would update"
	}
	fmt.Printf("\nschedules scanned:     %d\n", len(schedules))
	fmt.Printf("already set (skipped): %d\n", alreadySet)
	fmt.Printf("no operated_by:        %d\n", noOperator)
	fmt.Printf("unresolved:            %d\n", unresolved)
	fmt.Printf("%s:           %d\n", verb, resolved)

	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run without --dry-run to write Fields[user_bead_id].")
		return nil
	}

	var applied, failed int
	for _, p := range plans {
		if p.UserBeadID == "" {
			continue
		}
		patch, err := json.Marshal(map[string]string{"user_bead_id": p.UserBeadID})
		if err != nil {
			return fmt.Errorf("marshalling patch for %s: %w", p.BeadID, err)
		}
		if _, err := beadsClient.UpdateBead(ctx, p.BeadID, &client.UpdateBeadRequest{
			Fields: patch,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  !! %s: update failed: %v\n", p.BeadID, err)
			failed++
			continue
		}
		applied++
	}

	fmt.Printf("\napplied=%d  failed=%d\n", applied, failed)
	if failed > 0 {
		return fmt.Errorf("%d schedule(s) failed to update", failed)
	}
	return nil
}

// resolveOperatorLookup parses an `operated_by` value and returns the lookup
// key for the user-bead search. Either email OR slackID will be set; both
// empty signals a sentinel operator the backfill cannot resolve. The
// joined OperatedByEmail column wins when populated since it's the
// canonical email from the operators table.
func resolveOperatorLookup(operatedBy, joinedEmail string) (email, slackID string) {
	if strings.TrimSpace(joinedEmail) != "" {
		return joinedEmail, ""
	}
	kind, subject, ok := strings.Cut(operatedBy, ":")
	if !ok || subject == "" {
		return "", ""
	}
	switch kind {
	case "oidc", "gitlab", "jira":
		return subject, ""
	case "slack":
		return "", subject
	}
	return "", ""
}

// lookupUserBead returns the user-bead id matching email or slack_id, or ""
// when no bead matches. Results are cached on the provided maps so a
// schedule bead set sharing operators only hits the daemon once per
// distinct lookup key.
func lookupUserBead(
	ctx context.Context,
	email, slackID string,
	byEmail, bySlackID map[string]string,
) (string, error) {
	if email != "" {
		if id, ok := byEmail[email]; ok {
			return id, nil
		}
		id, err := findUserBead(ctx, "email", email)
		if err != nil {
			return "", err
		}
		byEmail[email] = id
		return id, nil
	}
	if slackID != "" {
		if id, ok := bySlackID[slackID]; ok {
			return id, nil
		}
		id, err := findUserBead(ctx, "slack_id", slackID)
		if err != nil {
			return "", err
		}
		bySlackID[slackID] = id
		return id, nil
	}
	return "", nil
}

func findUserBead(ctx context.Context, fieldName, fieldValue string) (string, error) {
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type:         []string{"user"},
		Kind:         []string{"config"},
		FieldFilters: map[string]string{fieldName: fieldValue},
		Limit:        1,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Beads) == 0 {
		return "", nil
	}
	return resp.Beads[0].ID, nil
}

func decodeFields(raw json.RawMessage) map[string]any {
	m := make(map[string]any)
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		return make(map[string]any)
	}
	return m
}

func printUserBeadIDPlans(plans []userBeadIDPlan, dryRun bool) {
	if len(plans) == 0 {
		return
	}
	title := "PROPOSED CHANGES"
	if !dryRun {
		title = "APPLYING CHANGES"
	}
	fmt.Printf("\n%s (%d schedule beads)\n\n", title, len(plans))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tOPERATED_BY\tRESOLVED_USER\tNOTE")
	for _, p := range plans {
		userOrEmpty := p.UserBeadID
		if userOrEmpty == "" {
			userOrEmpty = "—"
		}
		note := p.Reason
		if note == "" {
			if p.Email != "" {
				note = "via email"
			} else if p.SlackID != "" {
				note = "via slack_id"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.BeadID, displayOrDash(p.OperatedBy), userOrEmpty, note)
	}
	w.Flush()
}

func displayOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
