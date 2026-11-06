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

// adminBackfillGardenerAttributionCmd attributes a human owner onto existing gardeners
// and their historical agent iterations so the beads-viewer Gardener page
// (epic ka-0000001579, design doc ka-0000003219 §7) can resolve a per-user
// owner without falling through to "whoever last operated the trigger bead".
//
// It is the sibling of `kd admin backfill user-bead-id`
// (admin_backfill_user_bead_id.go) and copies that command's shape: a pure
// plan step, an idempotency guard that skips already-set fields, a --dry-run
// that performs no writes, and a field-merge UpdateBead apply.
//
// The owner attribution lives in two places that the gardener loop stamps
// (stampGardenerLoopContext, gardenio/controller/internal/bridge/triggers.go):
//
//   - The gardener trigger bead's `gardener` JSON blob carries the authored
//     `slack_user_id` / `user_bead_id` (NB: the real field names — NOT
//     `schedule_slack_user_id`; see design doc §1.2 / §11).
//   - Every spawned agent iteration carries flat `slack_user_id` /
//     `user_bead_id` fields plus the `schedule_id` back-pointer to its
//     gardener.
//
// Phase A — gardener trigger beads (type=trigger + role:gardener). For each
// whose `gardener` JSON lacks both `slack_user_id` and `user_bead_id`, resolve
// the owner from the trigger bead's OperatedBy (`kind:subject`) back to a
// type=user bead and stamp the resolved Slack id + user-bead id into the
// gardener JSON.
//
// Phase B — historical agent iterations (type=agent with a `schedule_id`).
// For each missing `slack_user_id`, inherit the owner from its gardener
// (grouped by `schedule_id`, looked up once per group) and stamp the flat
// `slack_user_id` / `user_bead_id` fields to match the loop's own stamp.
//
// --dry-run prints PHASE/BEAD_ID/FIELD/OLD/NEW/STATUS tab rows and performs no
// writes. Re-running after an apply produces an all-SKIP_IDEMPOTENT plan.
// Exit is non-zero when any row is ERROR (malformed `gardener` JSON, or an
// apply write failure).
var adminBackfillGardenerAttributionCmd = &cobra.Command{
	Use:   "gardener-attribution",
	Short: "Attribute owner (slack_user_id + user_bead_id) onto gardeners and their agent iterations",
	Long: `Backfill per-user ownership onto existing gardeners and their historical
agent iterations (Gardener Page epic ka-0000001579, design doc ka-0000003219 §7).

Phase A scans type=trigger + role:gardener beads. For each gardener whose
` + "`gardener`" + ` JSON lacks both slack_user_id and user_bead_id, the command
resolves the trigger bead's operated_by ("kind:subject") back to a type=user
bead and stamps the resolved Slack id (user bead Fields.slack_id) and
user-bead id into the gardener JSON.

Phase B scans type=agent beads carrying a schedule_id. For each iteration
missing slack_user_id, the owner is inherited from its gardener (grouped by
schedule_id, resolved once per group) and stamped as the flat slack_user_id /
user_bead_id fields — matching the names stampGardenerLoopContext writes.

Field names are slack_user_id and user_bead_id (NOT schedule_slack_user_id).
Already-set fields are skipped (idempotent); a re-run after an apply yields an
all-SKIP_IDEMPOTENT plan.

--dry-run prints the proposed PHASE/BEAD_ID/FIELD/OLD/NEW/STATUS rows and
writes nothing. Exit is non-zero when any row is ERROR.`,
	RunE: runAdminBackfillGardenerAttribution,
}

func init() {
	adminBackfillGardenerAttributionCmd.Flags().Bool("dry-run", false, "show proposed changes without writing")
	adminBackfillGardenerAttributionCmd.Flags().Int("limit", 0, "cap the number of beads scanned per phase (0 = no cap)")
	adminBackfillGardenerAttributionCmd.Flags().Int("page-size", 200, "beads to fetch per page")
	adminBackfillCmd.AddCommand(adminBackfillGardenerAttributionCmd)
}

// gardenerAttributionStatus is the STATUS column value for one planned row.
type gardenerAttributionStatus string

const (
	statusWouldUpdate    gardenerAttributionStatus = "WOULD_UPDATE"
	statusUpdated        gardenerAttributionStatus = "UPDATED"
	statusSkipIdempotent gardenerAttributionStatus = "SKIP_IDEMPOTENT"
	statusSkipNoOwner    gardenerAttributionStatus = "SKIP_NO_OWNER"
	statusSkipNoGardener gardenerAttributionStatus = "SKIP_NO_GARDENER"
	statusError          gardenerAttributionStatus = "ERROR"
)

// gardenerAttributionRow is one planned field change. Phase A rows mutate the
// gardener JSON blob (collected per-bead and applied as one UpdateBead);
// Phase B rows mutate a flat agent field.
type gardenerAttributionRow struct {
	Phase  string // "A" or "B"
	BeadID string
	Field  string // slack_user_id / user_bead_id, or "—" for status-only rows
	Old    string // current value, "(unset)" when empty
	New    string // proposed value, "—" when none
	Status gardenerAttributionStatus
	Note   string // diagnostic, not printed in the tab table
}

// gardenerAttribution is the resolved owner for one gardener: the Slack user id and
// user-bead id derived from the gardener's operator. Either may be empty when
// the user bead carries only one of the two.
type gardenerAttribution struct {
	SlackUserID string
	UserBeadID  string
}

func runAdminBackfillGardenerAttribution(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	limit, _ := cmd.Flags().GetInt("limit")
	pageSize, _ := cmd.Flags().GetInt("page-size")
	if pageSize <= 0 {
		pageSize = 200
	}

	ctx := context.Background()

	// Caches shared across both phases: operator lookup key → user bead, and
	// user bead id → its resolved owner (slack + bead id).
	userByEmail := make(map[string]string)
	userBySlackID := make(map[string]string)
	ownerByGardenerID := make(map[string]gardenerAttribution) // gardener trigger id → owner

	var rows []gardenerAttributionRow

	// --- Phase A: gardener trigger beads -------------------------------------
	gardeners, err := listAllBeads(ctx, &client.ListBeadsRequest{
		Type:   []string{"trigger"},
		Labels: []string{"role:gardener"},
	}, pageSize, limit)
	if err != nil {
		return fmt.Errorf("listing gardeners: %w", err)
	}

	// gardenerPatches collects the merged gardener JSON per bead so a gardener
	// missing both fields produces one UpdateBead, not two.
	gardenerPatches := make(map[string]json.RawMessage)
	for _, b := range gardeners {
		owner, phaseRows, patch := planGardenerAttribution(ctx, b, userByEmail, userBySlackID)
		rows = append(rows, phaseRows...)
		if len(patch) > 0 {
			gardenerPatches[b.ID] = patch
		}
		// Record the resolved owner (even when Phase A skipped because it was
		// already set) so Phase B can inherit it without re-resolving.
		if owner.SlackUserID != "" || owner.UserBeadID != "" {
			ownerByGardenerID[b.ID] = owner
		}
	}

	// --- Phase B: historical agent iterations -------------------------------
	iterations, err := listAllBeads(ctx, &client.ListBeadsRequest{
		Type:         []string{"agent"},
		FieldFilters: map[string]string{"schedule_id": "*"},
	}, pageSize, limit)
	if err != nil {
		return fmt.Errorf("listing agent iterations: %w", err)
	}

	// iterPatches collects the merged flat-field patch per iteration bead.
	iterPatches := make(map[string]json.RawMessage)
	for _, b := range iterations {
		phaseRows, patch := planIterationOwner(ctx, b, ownerByGardenerID, userByEmail, userBySlackID)
		rows = append(rows, phaseRows...)
		if len(patch) > 0 {
			iterPatches[b.ID] = patch
		}
	}

	printGardenerAttributionRows(rows, dryRun)
	printGardenerAttributionSummary(rows, len(gardeners), len(iterations), dryRun)

	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run without --dry-run to write slack_user_id / user_bead_id.")
		// A dry-run still fails loudly if the plan surfaced unrecoverable
		// ERROR rows (e.g. malformed gardener JSON) so CI catches them.
		if countStatus(rows, statusError) > 0 {
			return fmt.Errorf("%d row(s) errored during planning", countStatus(rows, statusError))
		}
		return nil
	}

	applied, failed := applyGardenerAttributionPatches(ctx, gardenerPatches, iterPatches)
	fmt.Printf("\napplied=%d  failed=%d\n", applied, failed)
	if failed > 0 || countStatus(rows, statusError) > 0 {
		return fmt.Errorf("%d update(s) failed, %d planning error(s)", failed, countStatus(rows, statusError))
	}
	return nil
}

// planGardenerAttribution resolves and plans the Phase A backfill for one gardener
// trigger bead. It returns the resolved owner (for Phase B inheritance), the
// planned rows, and the merged gardener-JSON patch to apply (nil when nothing
// changes). The patch carries the full re-encoded gardener blob under the
// "gardener" key so UpdateBead's field-merge writes it back intact.
func planGardenerAttribution(
	ctx context.Context,
	b *model.Bead,
	userByEmail, userBySlackID map[string]string,
) (gardenerAttribution, []gardenerAttributionRow, json.RawMessage) {
	fields := decodeFields(b.Fields)

	// Decode the gardener JSON blob. A malformed blob is an ERROR row, not a
	// silent skip — the operator must fix it before the backfill can proceed.
	capRaw := fields["gardener"]
	capMap, err := decodeGardenerBlob(capRaw)
	if err != nil {
		return gardenerAttribution{}, []gardenerAttributionRow{{
			Phase: "A", BeadID: b.ID, Field: "—", Old: "—", New: "—",
			Status: statusError, Note: "malformed gardener JSON: " + err.Error(),
		}}, nil
	}

	existingSlack, _ := capMap["slack_user_id"].(string)
	existingUserBead, _ := capMap["user_bead_id"].(string)
	owner := gardenerAttribution{SlackUserID: existingSlack, UserBeadID: existingUserBead}

	// Idempotency: skip when EITHER field is already set (the loop stamps both
	// from the same operator, so one present implies the gardener is attributed).
	if existingSlack != "" || existingUserBead != "" {
		return owner, []gardenerAttributionRow{{
			Phase: "A", BeadID: b.ID, Field: "slack_user_id",
			Old: orUnset(existingSlack), New: orUnset(existingSlack),
			Status: statusSkipIdempotent,
		}}, nil
	}

	resolved, note := resolveOwnerFromOperator(ctx, b.OperatedBy, b.OperatedByEmail, userByEmail, userBySlackID)
	if resolved.SlackUserID == "" && resolved.UserBeadID == "" {
		return gardenerAttribution{}, []gardenerAttributionRow{{
			Phase: "A", BeadID: b.ID, Field: "—", Old: "—", New: "—",
			Status: statusSkipNoOwner, Note: note,
		}}, nil
	}

	// Stamp the resolved owner into the gardener blob and re-encode it.
	rows := make([]gardenerAttributionRow, 0, 2)
	if resolved.SlackUserID != "" {
		capMap["slack_user_id"] = resolved.SlackUserID
		rows = append(rows, gardenerAttributionRow{
			Phase: "A", BeadID: b.ID, Field: "slack_user_id",
			Old: orUnset(existingSlack), New: resolved.SlackUserID, Status: statusWouldUpdate,
		})
	}
	if resolved.UserBeadID != "" {
		capMap["user_bead_id"] = resolved.UserBeadID
		rows = append(rows, gardenerAttributionRow{
			Phase: "A", BeadID: b.ID, Field: "user_bead_id",
			Old: orUnset(existingUserBead), New: resolved.UserBeadID, Status: statusWouldUpdate,
		})
	}

	encoded, mErr := json.Marshal(capMap)
	if mErr != nil {
		return gardenerAttribution{}, []gardenerAttributionRow{{
			Phase: "A", BeadID: b.ID, Field: "—", Old: "—", New: "—",
			Status: statusError, Note: "re-encoding gardener JSON: " + mErr.Error(),
		}}, nil
	}
	patch, mErr := json.Marshal(map[string]json.RawMessage{"gardener": encoded})
	if mErr != nil {
		return gardenerAttribution{}, []gardenerAttributionRow{{
			Phase: "A", BeadID: b.ID, Field: "—", Old: "—", New: "—",
			Status: statusError, Note: "marshalling gardener patch: " + mErr.Error(),
		}}, nil
	}
	return resolved, rows, patch
}

// planIterationOwner plans the Phase B backfill for one agent iteration. It
// inherits the owner from the iteration's gardener (looked up once per group
// via ownerByGardenerID; populated lazily from the gardener trigger bead when a
// gardener was scanned but already-attributed in Phase A is not in the map).
func planIterationOwner(
	ctx context.Context,
	b *model.Bead,
	ownerByGardenerID map[string]gardenerAttribution,
	userByEmail, userBySlackID map[string]string,
) ([]gardenerAttributionRow, json.RawMessage) {
	fields := decodeFields(b.Fields)
	scheduleID, _ := fields["schedule_id"].(string)
	if scheduleID == "" {
		return nil, nil // defensive — the filter guarantees presence
	}

	existingSlack, _ := fields["slack_user_id"].(string)
	existingUserBead, _ := fields["user_bead_id"].(string)

	// Idempotency: skip iterations that already carry slack_user_id.
	if existingSlack != "" {
		return []gardenerAttributionRow{{
			Phase: "B", BeadID: b.ID, Field: "slack_user_id",
			Old: orUnset(existingSlack), New: orUnset(existingSlack),
			Status: statusSkipIdempotent,
		}}, nil
	}

	owner, ok := resolveGardenerAttributionForGroup(ctx, scheduleID, ownerByGardenerID, userByEmail, userBySlackID)
	if !ok {
		return []gardenerAttributionRow{{
			Phase: "B", BeadID: b.ID, Field: "—", Old: "—", New: "—",
			Status: statusSkipNoGardener, Note: "gardener " + scheduleID + " missing or unattributed",
		}}, nil
	}

	patch := make(map[string]string)
	rows := make([]gardenerAttributionRow, 0, 2)
	if owner.SlackUserID != "" {
		patch["slack_user_id"] = owner.SlackUserID
		rows = append(rows, gardenerAttributionRow{
			Phase: "B", BeadID: b.ID, Field: "slack_user_id",
			Old: orUnset(existingSlack), New: owner.SlackUserID, Status: statusWouldUpdate,
		})
	}
	if owner.UserBeadID != "" && existingUserBead == "" {
		patch["user_bead_id"] = owner.UserBeadID
		rows = append(rows, gardenerAttributionRow{
			Phase: "B", BeadID: b.ID, Field: "user_bead_id",
			Old: orUnset(existingUserBead), New: owner.UserBeadID, Status: statusWouldUpdate,
		})
	}
	if len(patch) == 0 {
		return []gardenerAttributionRow{{
			Phase: "B", BeadID: b.ID, Field: "—", Old: "—", New: "—",
			Status: statusSkipNoOwner, Note: "gardener has no slack_user_id to inherit",
		}}, nil
	}

	encoded, mErr := json.Marshal(patch)
	if mErr != nil {
		return []gardenerAttributionRow{{
			Phase: "B", BeadID: b.ID, Field: "—", Old: "—", New: "—",
			Status: statusError, Note: "marshalling iteration patch: " + mErr.Error(),
		}}, nil
	}
	return rows, encoded
}

// resolveGardenerAttributionForGroup returns the owner for a gardener (schedule) id,
// resolving it once per group and caching the result. The owner may already
// be cached from Phase A; otherwise the gardener trigger bead is fetched and
// its authored slack_user_id / user_bead_id read directly. Returns ok=false
// when the gardener is missing or carries no owner to inherit.
func resolveGardenerAttributionForGroup(
	ctx context.Context,
	scheduleID string,
	ownerByGardenerID map[string]gardenerAttribution,
	userByEmail, userBySlackID map[string]string,
) (gardenerAttribution, bool) {
	if owner, seen := ownerByGardenerID[scheduleID]; seen {
		return owner, owner.SlackUserID != "" || owner.UserBeadID != ""
	}

	bead, err := beadsClient.GetBead(ctx, scheduleID)
	if err != nil || bead == nil {
		ownerByGardenerID[scheduleID] = gardenerAttribution{} // negative-cache the dangling group
		return gardenerAttribution{}, false
	}

	owner := gardenerAttribution{}
	capMap, decErr := decodeGardenerBlob(decodeFields(bead.Fields)["gardener"])
	if decErr == nil {
		owner.SlackUserID, _ = capMap["slack_user_id"].(string)
		owner.UserBeadID, _ = capMap["user_bead_id"].(string)
	}
	// Gardener not yet attributed in its own blob — fall back to resolving the
	// trigger bead's operator so a single backfill run can attribute both the
	// gardener (Phase A) and its iterations (Phase B) end to end.
	if owner.SlackUserID == "" && owner.UserBeadID == "" {
		owner, _ = resolveOwnerFromOperator(ctx, bead.OperatedBy, bead.OperatedByEmail, userByEmail, userBySlackID)
	}

	ownerByGardenerID[scheduleID] = owner
	return owner, owner.SlackUserID != "" || owner.UserBeadID != ""
}

// resolveOwnerFromOperator resolves an operated_by ("kind:subject") back to a
// type=user bead and derives the owner's Slack user id (user bead
// Fields.slack_id) and user-bead id. The joined OperatedByEmail column wins as
// the lookup key when populated. Returns a zero owner and a diagnostic note
// when the operator is a sentinel or matches no user bead.
func resolveOwnerFromOperator(
	ctx context.Context,
	operatedBy, joinedEmail string,
	userByEmail, userBySlackID map[string]string,
) (gardenerAttribution, string) {
	email, slackID := resolveOperatorLookup(operatedBy, joinedEmail)
	if email == "" && slackID == "" {
		return gardenerAttribution{}, "sentinel operator (system:/fleet:) or unrecognized kind"
	}

	userID, err := lookupUserBead(ctx, email, slackID, userByEmail, userBySlackID)
	if err != nil {
		return gardenerAttribution{}, "user-bead lookup failed: " + err.Error()
	}
	if userID == "" {
		return gardenerAttribution{}, "no user bead matches operator"
	}

	owner := gardenerAttribution{UserBeadID: userID}
	// Derive the canonical Slack user id from the resolved user bead's
	// slack_id field. When the operator was itself a slack:<id>, that id is
	// authoritative even if the user bead omits slack_id.
	owner.SlackUserID = slackID
	if owner.SlackUserID == "" {
		if ub, ubErr := beadsClient.GetBead(ctx, userID); ubErr == nil && ub != nil {
			if sid, _ := decodeFields(ub.Fields)["slack_id"].(string); sid != "" {
				owner.SlackUserID = sid
			}
		}
	}
	return owner, "resolved"
}

// decodeGardenerBlob decodes the gardener JSON field, which the daemon may
// surface either as a JSON string (HTTP) or an already-decoded object. Returns
// an empty map for an absent/empty blob and an error only for malformed JSON.
func decodeGardenerBlob(raw any) (map[string]any, error) {
	switch v := raw.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return v, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return map[string]any{}, nil
		}
		m := make(map[string]any)
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, err
		}
		return m, nil
	case json.RawMessage:
		if len(v) == 0 {
			return map[string]any{}, nil
		}
		m := make(map[string]any)
		if err := json.Unmarshal(v, &m); err != nil {
			return nil, err
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unexpected gardener field type %T", raw)
	}
}

// applyGardenerAttributionPatches writes the collected Phase A gardener-JSON patches
// and Phase B flat-field patches via UpdateBead's field merge. Returns the
// applied and failed counts.
func applyGardenerAttributionPatches(
	ctx context.Context,
	gardenerPatches, iterPatches map[string]json.RawMessage,
) (applied, failed int) {
	apply := func(beadID string, patch json.RawMessage) {
		if _, err := beadsClient.UpdateBead(ctx, beadID, &client.UpdateBeadRequest{
			Fields: patch,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  !! %s: update failed: %v\n", beadID, err)
			failed++
			return
		}
		applied++
	}
	for id, patch := range gardenerPatches {
		apply(id, patch)
	}
	for id, patch := range iterPatches {
		apply(id, patch)
	}
	return applied, failed
}

// listAllBeads pages through every bead matching req, stopping at the optional
// limit (0 = no cap). The caller's req.Limit/Offset are managed here.
func listAllBeads(ctx context.Context, req *client.ListBeadsRequest, pageSize, limit int) ([]*model.Bead, error) {
	var out []*model.Bead
	offset := 0
	for {
		req.Limit = pageSize
		req.Offset = offset
		resp, err := beadsClient.ListBeads(ctx, req)
		if err != nil {
			return nil, err
		}
		if len(resp.Beads) == 0 {
			break
		}
		out = append(out, resp.Beads...)
		if limit > 0 && len(out) >= limit {
			out = out[:limit]
			break
		}
		if len(resp.Beads) < pageSize {
			break
		}
		offset += len(resp.Beads)
	}
	return out, nil
}

func countStatus(rows []gardenerAttributionRow, want gardenerAttributionStatus) int {
	n := 0
	for _, r := range rows {
		if r.Status == want {
			n++
		}
	}
	return n
}

func orUnset(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unset)"
	}
	return s
}

func printGardenerAttributionRows(rows []gardenerAttributionRow, dryRun bool) {
	if len(rows) == 0 {
		fmt.Println("no gardeners or iterations found")
		return
	}
	title := "PROPOSED CHANGES"
	if !dryRun {
		title = "APPLYING CHANGES"
	}
	fmt.Printf("\n%s (%d rows)\n\n", title, len(rows))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PHASE\tBEAD_ID\tFIELD\tOLD\tNEW\tSTATUS")
	for _, r := range rows {
		// When applying, WOULD_UPDATE rows become UPDATED in the printed table.
		status := r.Status
		if !dryRun && status == statusWouldUpdate {
			status = statusUpdated
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Phase, r.BeadID, r.Field, r.Old, r.New, status)
	}
	w.Flush()
}

func printGardenerAttributionSummary(rows []gardenerAttributionRow, gardeners, iterations int, dryRun bool) {
	verb := "would update"
	if !dryRun {
		verb = "updated"
	}
	fmt.Printf("\ngardeners scanned:       %d\n", gardeners)
	fmt.Printf("iterations scanned:     %d\n", iterations)
	fmt.Printf("%s (rows):     %d\n", verb, countStatus(rows, statusWouldUpdate))
	fmt.Printf("skip idempotent:        %d\n", countStatus(rows, statusSkipIdempotent))
	fmt.Printf("skip no owner:          %d\n", countStatus(rows, statusSkipNoOwner))
	fmt.Printf("skip no gardener:        %d\n", countStatus(rows, statusSkipNoGardener))
	fmt.Printf("errors:                 %d\n", countStatus(rows, statusError))
}
