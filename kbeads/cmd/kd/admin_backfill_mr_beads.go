package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"text/tabwriter"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminBackfillMRBeadsCmd projects existing task beads that carry mr_url
// onto type=merge-request beads (epic ka-0000001613 / task ka-0000001632).
// Sibling task ka-0000007548 wired the gitlab-bridge dual-write path; this
// command sweeps the historical task beads that pre-date that path so the
// MR-bead view is complete.
//
// Per matching task bead the command:
//
//  1. Upserts a type=merge-request bead keyed by canonical mr_url. Fields
//     are copied from the task bead's mr_* / gitlab_* keys (see
//     mrBackfillFieldKeys). Status reflects mr_state: open when the MR is
//     opened/draft, closed when merged/closed.
//  2. Adds a `produces` dependency from the task bead → MR bead.
//
// Dry-run by default (--apply commits). Idempotent — re-running on already-
// backfilled data is a no-op.
var adminBackfillMRBeadsCmd = &cobra.Command{
	Use:   "mr-beads",
	Short: "Backfill type=merge-request beads from existing task beads with mr_url",
	Long: `Walks task beads in all statuses and, for those carrying an mr_url
field, upserts the corresponding type=merge-request bead and adds a
` + "`produces`" + ` dep from the task bead → MR bead.

Default mode is --dry-run. Pass --apply to commit. Re-runs are idempotent:
fields are merged via PATCH, the MR bead is closed only if the source mr_state
is terminal (merged/closed), and the produces dep is only added when it does
not already exist.`,
	RunE: runAdminBackfillMRBeads,
}

func init() {
	adminBackfillMRBeadsCmd.Flags().Bool("dry-run", true, "report changes without writing (default true)")
	adminBackfillMRBeadsCmd.Flags().Bool("apply", false, "commit changes (overrides --dry-run=true)")
	adminBackfillMRBeadsCmd.Flags().Int("page-size", 200, "task beads per ListBeads page")
	adminBackfillMRBeadsCmd.Flags().Int("limit", 0, "stop after N task beads with mr_url (0 = unbounded)")
	adminBackfillMRBeadsCmd.Flags().StringSlice("type", []string{"task"}, "source bead types to scan (comma-separated)")
	adminBackfillCmd.AddCommand(adminBackfillMRBeadsCmd)
}

// mrBackfillTaskStatuses scans every status — closed task beads still need
// their MR bead projected (review comments / merges may post-date the close).
var mrBackfillTaskStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// mrURLPattern mirrors gardenio/controller/internal/bridge.mrURLPattern so the
// kbeads admin can validate URLs without taking a dependency on the gardenio
// module. Format:
//
//	https://<host>/<project-path>/-/merge_requests/<iid>
var mrURLPattern = regexp.MustCompile(`^https?://[^/]+/(.+?)/-/merge_requests/(\d+)`)

func runAdminBackfillMRBeads(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	apply, _ := cmd.Flags().GetBool("apply")
	if apply {
		dryRun = false
	}
	pageSize, _ := cmd.Flags().GetInt("page-size")
	if pageSize <= 0 {
		pageSize = 200
	}
	limit, _ := cmd.Flags().GetInt("limit")
	types, _ := cmd.Flags().GetStringSlice("type")
	if len(types) == 0 {
		types = []string{"task"}
	}

	ctx := context.Background()
	start := time.Now()

	// mrBeadCache memoizes (mr_url → MR bead) so two task beads pointing at
	// the same MR share one daemon lookup.
	mrBeadCache := map[string]*model.Bead{}

	var (
		scanned     int
		withMRURL   int
		created     int
		updated     int
		closedFlip  int
		depAdded    int
		depExisting int
		skippedBad  int
		failed      int
		plans       []mrBackfillPlan
		offset      int
	)

pageLoop:
	for {
		resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
			Type:   types,
			Status: mrBackfillTaskStatuses,
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created_at",
		})
		if err != nil {
			return fmt.Errorf("listing task beads (offset=%d): %w", offset, err)
		}
		if len(resp.Beads) == 0 {
			break
		}

		for _, b := range resp.Beads {
			scanned++
			fields := decodeFields(b.Fields)
			mrURL, _ := fields["mr_url"].(string)
			if mrURL == "" {
				continue
			}
			withMRURL++

			if !mrURLPattern.MatchString(mrURL) {
				skippedBad++
				plans = append(plans, mrBackfillPlan{
					TaskBeadID: b.ID,
					MRURL:      mrURL,
					Skipped:    "malformed mr_url",
				})
				continue
			}

			plan, err := planMRBackfill(ctx, b, fields, mrURL, mrBeadCache)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  !! %s: plan failed: %v\n", b.ID, err)
				failed++
				continue
			}
			plans = append(plans, plan)

			if !dryRun {
				if err := applyMRBackfill(ctx, &plan, mrBeadCache); err != nil {
					fmt.Fprintf(os.Stderr, "  !! %s: apply failed: %v\n", b.ID, err)
					failed++
					continue
				}
			}

			switch plan.MRAction {
			case "create":
				created++
			case "update":
				updated++
			}
			if plan.CloseFlip {
				closedFlip++
			}
			switch plan.DepAction {
			case "add":
				depAdded++
			case "exists":
				depExisting++
			}

			if limit > 0 && withMRURL >= limit {
				break pageLoop
			}
		}

		offset += len(resp.Beads)
		if len(resp.Beads) < pageSize {
			break
		}
	}

	printMRBackfillPlans(plans)

	verb := "would"
	if !dryRun {
		verb = "did"
	}
	fmt.Printf("\nsource beads scanned:    %d\n", scanned)
	fmt.Printf("with mr_url:             %d\n", withMRURL)
	fmt.Printf("malformed mr_url:        %d\n", skippedBad)
	fmt.Printf("MR beads %s create:    %d\n", verb, created)
	fmt.Printf("MR beads %s update:    %d\n", verb, updated)
	fmt.Printf("MR beads %s close:     %d\n", verb, closedFlip)
	fmt.Printf("produces deps %s add:  %d\n", verb, depAdded)
	fmt.Printf("produces deps existed:   %d\n", depExisting)
	if failed > 0 {
		fmt.Printf("failed:                  %d\n", failed)
	}
	fmt.Printf("elapsed:                 %s\n", time.Since(start).Round(time.Millisecond))

	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run with --apply to commit.")
	}
	if failed > 0 {
		return fmt.Errorf("%d task bead(s) failed during MR backfill", failed)
	}
	return nil
}

// mrBackfillPlan describes the proposed change for one task bead.
type mrBackfillPlan struct {
	TaskBeadID string
	MRURL      string
	MRBeadID   string
	// MRAction is "create", "update", or "noop".
	MRAction string
	// CloseFlip is true when the planned write transitions the MR bead to
	// status=closed (merged / closed-without-merge).
	CloseFlip bool
	// DepAction is "add", "exists", or "noop" (the last when the MR bead
	// could not be reified, e.g. dry-run create).
	DepAction string
	// MRFields is the projected fields-map written onto the MR bead.
	MRFields map[string]string
	// Status is the status the MR bead should land in ("open" or "closed").
	Status string
	// Skipped, when non-empty, explains why this bead was not actionable.
	Skipped string
}

// mrBackfillFieldKeys are the task-bead fields the backfill projects onto
// the MR bead. Mirrors mrFieldsMap in
// gardenio/controller/internal/beadsapi/client_mr.go so the two paths produce
// the same on-bead shape.
var mrBackfillFieldKeys = []string{
	"mr_url",
	"mr_iid",
	"project_path",
	"source_branch",
	"target_branch",
	"mr_state",
	"mr_outcome",
	"mr_pipeline_status",
	"mr_pipeline_url",
	"author",
	"mr_title",
	"gitlab_mr_iid",
	"gitlab_project_id",
	"gitlab_mr_source_branch",
	"mr_approved",
	"mr_approvers",
	"mr_has_review_comments",
}

// planMRBackfill builds the proposed write for one task bead. It hits the
// daemon to look up the existing MR bead (and caches the result) but performs
// no mutations.
func planMRBackfill(
	ctx context.Context,
	task *model.Bead,
	fields map[string]any,
	mrURL string,
	cache map[string]*model.Bead,
) (mrBackfillPlan, error) {
	plan := mrBackfillPlan{
		TaskBeadID: task.ID,
		MRURL:      mrURL,
		MRFields:   projectMRFields(fields),
	}

	mrState, _ := fields["mr_state"].(string)
	closed := mrState == "merged" || mrState == "closed"
	plan.Status = "open"
	if closed {
		plan.Status = "closed"
		if _, ok := plan.MRFields["mr_outcome"]; !ok {
			plan.MRFields["mr_outcome"] = inferMROutcome(mrState)
		}
		// Track mr_merged parity with the gitlab-bridge dual-write so
		// downstream consumers see the same signal here.
		if mrState == "merged" {
			plan.MRFields["mr_merged"] = "true"
		} else {
			plan.MRFields["mr_merged"] = "false"
		}
	}

	existing, err := findMRBead(ctx, mrURL, cache)
	if err != nil {
		return plan, fmt.Errorf("looking up MR bead for %s: %w", mrURL, err)
	}
	if existing == nil {
		plan.MRAction = "create"
		plan.CloseFlip = closed
		plan.DepAction = "add" // dep will be added against the newly-created bead
		return plan, nil
	}

	plan.MRBeadID = existing.ID
	plan.MRAction = "update"
	if closed && existing.Status != model.StatusClosed {
		plan.CloseFlip = true
	}

	depExists, err := producesDepExists(ctx, task.ID, existing.ID)
	if err != nil {
		return plan, fmt.Errorf("checking produces dep for %s → %s: %w", task.ID, existing.ID, err)
	}
	if depExists {
		plan.DepAction = "exists"
	} else {
		plan.DepAction = "add"
	}
	return plan, nil
}

// applyMRBackfill commits one planned change. Updates the cache so the next
// task bead pointing at the same MR sees the freshly-created bead.
func applyMRBackfill(ctx context.Context, plan *mrBackfillPlan, cache map[string]*model.Bead) error {
	mrBeadID := plan.MRBeadID

	switch plan.MRAction {
	case "create":
		fieldsJSON, err := json.Marshal(plan.MRFields)
		if err != nil {
			return fmt.Errorf("marshalling MR fields: %w", err)
		}
		req := &client.CreateBeadRequest{
			Title:  plan.MRURL,
			Type:   "merge-request",
			Kind:   "issue",
			Fields: json.RawMessage(fieldsJSON),
		}
		created, err := beadsClient.CreateBead(ctx, req)
		if err != nil {
			return fmt.Errorf("creating MR bead: %w", err)
		}
		mrBeadID = created.ID
		plan.MRBeadID = created.ID
		if plan.CloseFlip {
			if _, err := beadsClient.CloseBead(ctx, mrBeadID, ""); err != nil {
				return fmt.Errorf("closing MR bead %s: %w", mrBeadID, err)
			}
		}
		// Populate the cache so sibling task beads pointing at the same MR
		// reuse this bead instead of trying to create a duplicate.
		cached := &model.Bead{
			ID:     created.ID,
			Type:   "merge-request",
			Status: model.StatusOpen,
		}
		if plan.CloseFlip {
			cached.Status = model.StatusClosed
		}
		cache[plan.MRURL] = cached
	case "update":
		fieldsJSON, err := json.Marshal(plan.MRFields)
		if err != nil {
			return fmt.Errorf("marshalling MR fields: %w", err)
		}
		update := &client.UpdateBeadRequest{Fields: fieldsJSON}
		if plan.CloseFlip {
			closedStatus := string(model.StatusClosed)
			update.Status = &closedStatus
		}
		if _, err := beadsClient.UpdateBead(ctx, mrBeadID, update); err != nil {
			return fmt.Errorf("updating MR bead %s: %w", mrBeadID, err)
		}
		if plan.CloseFlip {
			if cached, ok := cache[plan.MRURL]; ok {
				cached.Status = model.StatusClosed
			}
		}
	}

	if plan.DepAction == "add" && mrBeadID != "" {
		_, err := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
			BeadID:      plan.TaskBeadID,
			DependsOnID: mrBeadID,
			Type:        "produces",
			// Attribute the edge to the invoking human, matching every other
			// AddDependency caller (create/dep/reparent). The server ignores
			// this field anyway (AUDIT §C3: created_by is derived from the auth
			// identity, operated_by from resolveAuditOperatorRequired), so the
			// old hardcoded "system:ka-admin-backfill-mr-beads" was both dead
			// and misleading — it read as a system-operator stamp the daemon
			// never honors (ka-0000007735 / epic ka-0000004835).
			CreatedBy: actor,
		})
		if err != nil {
			return fmt.Errorf("adding produces dep %s → %s: %w", plan.TaskBeadID, mrBeadID, err)
		}
	}
	return nil
}

// findMRBead returns the MR bead whose mr_url field equals mrURL, or nil if
// none exists. Cache hits short-circuit the daemon call.
func findMRBead(ctx context.Context, mrURL string, cache map[string]*model.Bead) (*model.Bead, error) {
	if cached, ok := cache[mrURL]; ok {
		return cached, nil
	}
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type: []string{"merge-request"},
		Status: []string{
			string(model.StatusOpen),
			string(model.StatusInProgress),
			string(model.StatusBlocked),
			string(model.StatusDeferred),
			string(model.StatusClosed),
		},
		FieldFilters: map[string]string{"mr_url": mrURL},
		Limit:        1,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Beads) == 0 {
		cache[mrURL] = nil
		return nil, nil
	}
	cache[mrURL] = resp.Beads[0]
	return resp.Beads[0], nil
}

// producesDepExists reports whether a `produces` dep already links taskID →
// mrBeadID, so the backfill stays idempotent across re-runs.
func producesDepExists(ctx context.Context, taskID, mrBeadID string) (bool, error) {
	deps, err := beadsClient.GetDependencies(ctx, taskID)
	if err != nil {
		return false, err
	}
	for _, d := range deps {
		if d.DependsOnID == mrBeadID && string(d.Type) == "produces" {
			return true, nil
		}
	}
	return false, nil
}

// projectMRFields collects the mr_* / gitlab_* keys from a task bead into a
// string map suitable for writing onto an MR bead. Empty values are dropped
// so PATCH no-ops can fire when the bridge re-polls.
func projectMRFields(fields map[string]any) map[string]string {
	out := map[string]string{}
	for _, k := range mrBackfillFieldKeys {
		if v := stringifyField(fields[k]); v != "" {
			out[k] = v
		}
	}
	return out
}

// stringifyField projects a JSON-decoded field value (string / bool /
// number) into the string shape MR beads carry on the wire.
func stringifyField(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers unmarshal as float64. Render integers as decimal
		// integers; the rare fractional case (none exist on real MR fields)
		// keeps the original textual form.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	return ""
}

// inferMROutcome maps a GitLab MR state into the mr_outcome enum used on MR
// beads. Mirrors inferOutcome in
// gardenio/controller/internal/beadsapi/client_mr.go.
func inferMROutcome(state string) string {
	if state == "merged" {
		return "merged"
	}
	return "closed"
}

func printMRBackfillPlans(plans []mrBackfillPlan) {
	if len(plans) == 0 {
		return
	}
	fmt.Printf("\nPLAN (%d source beads)\n\n", len(plans))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tMR_URL\tMR_BEAD\tMR_ACTION\tCLOSE\tDEP\tNOTE")
	for _, p := range plans {
		mrBead := p.MRBeadID
		if mrBead == "" {
			mrBead = "—"
		}
		closeStr := "—"
		if p.CloseFlip {
			closeStr = "close"
		}
		mrAction := p.MRAction
		if mrAction == "" {
			mrAction = "—"
		}
		dep := p.DepAction
		if dep == "" {
			dep = "—"
		}
		note := p.Skipped
		if note == "" {
			note = p.Status
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.TaskBeadID, p.MRURL, mrBead, mrAction, closeStr, dep, note)
	}
	w.Flush()
}
