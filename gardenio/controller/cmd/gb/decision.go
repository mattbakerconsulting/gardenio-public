package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"

	"github.com/spf13/cobra"
)

var decisionCmd = &cobra.Command{
	Use:     "decision",
	Short:   "Manage decision points",
	GroupID: "orchestration",
	Long: `Manage decision points.

Reading a resolved decision (the routine read after "gb yield" returns):
  gb decision show <id>   prints a one-line "Resolved:" summary — the chosen
                          option, who answered, and when — so no jq field-
                          guessing is needed. The underlying bead fields are
                          "chosen", "responded_by", and "responded_at" (NOT
                          "resolution"/"chosen_option"/"answer"/"decided_by").`,
}

// ── decision create ─────────────────────────────────────────────────────

var decisionCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a decision point and optionally wait for response",
	Long: `Create a decision point and optionally wait for response.

Example (simple mode — --option flags auto-wrap into JSON):
  gb decision create --no-wait \
    --prompt="Deploy to staging?" \
    --option="yes:Deploy now:plan" \
    --option="no:Skip this cycle:report"

Option format for --option: "id:label:artifact_type" (artifact_type defaults
to "report"). For the JSON form, see the --options flag help; it accepts an
array of {id, short, label, artifact_type} objects.

Prompt length:
  --prompt is the decision TITLE and is capped at 500 characters (server limit).
  It is validated client-side before the request, so an over-length prompt fails
  fast with a clear message instead of a raw HTTP 400 naming the "title" field.
  Keep the prompt to a one-line question and put background, what-you-tried, and
  option detail in --context (which is not length-limited).

Screenshots:
  Do not paste copies onto the decision. Every surface that shows the decision
  also shows the images on the beads it is about: the requesting task (always)
  and up to two --evidence beads. An image is any image link in their comments
  or description, e.g. the 'gb image upload' links an acceptance grade posts.
  Newest comment first, at most 8. A screenshot added to the task after the ask
  shows up too, since --context cannot be edited later.
    gb image upload after.png --bead gd-task   # then kd comment add gd-task ...
    gb decision create --evidence gd-evidence-bundle ...

Dedup:
  Before creating, the prompt is normalised (lowercase, collapse whitespace,
  strip trailing punctuation, first 60 bytes) and compared against open
  decisions in the same project. If a sibling decision already shares this
  bucket key, the create is refused with the existing decision id. Add to
  the existing decision via 'kd comment add <id>' or block on it via
  'gb yield'. Pass --allow-duplicate to bypass when the new decision is
  genuinely distinct.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt, _ := cmd.Flags().GetString("prompt")
		title, _ := cmd.Flags().GetString("title")
		optionsJSON, _ := cmd.Flags().GetString("options")
		simpleOpts, _ := cmd.Flags().GetStringArray("option")
		requestedBy, _ := cmd.Flags().GetString("requested-by")
		requestingTask, _ := cmd.Flags().GetString("requesting-task")
		subject, _ := cmd.Flags().GetString("subject")
		forHuman, _ := cmd.Flags().GetString("for")
		decisionCtx, _ := cmd.Flags().GetString("context")
		evidence, err := evidenceBeadIDs(cmd)
		if err != nil {
			return err
		}
		noWait, _ := cmd.Flags().GetBool("no-wait")

		// Accept --title as alias for --prompt (common agent mistake).
		if prompt == "" && title != "" {
			prompt = title
		}

		if prompt == "" {
			return fmt.Errorf("--prompt is required\n\nExample:\n  gb decision create --no-wait --prompt=\"What should we do?\" --option=\"yes:Proceed:plan\" --option=\"no:Skip:report\"")
		}

		// Pre-validate the prompt length client-side (ka-0000006105). Without
		// this check an over-length prompt burned a full round-trip and returned
		// a raw "HTTP 400 ... title: must be 500 characters or fewer" —
		// confusingly naming a "title" field the agent never set.
		if err := checkDecisionPromptLength(prompt); err != nil {
			return err
		}

		// Expand curl-style @file / @- / @@ on --context, matching the
		// convention `kd` documents on its prose flags and the sibling
		// `gb decision report --content` (ka-0000003861 / ka-0000003818).
		// --context holds the not-length-limited background the docstring above
		// steers agents to source from a report/file, so a silently-stored
		// "@/path" pointer here ships the same empty-artifact footgun. A
		// missing/typo'd path fails LOUD. --prompt is deliberately NOT expanded:
		// it is the one-line, 500-char-capped decision TITLE (a leading '@' in a
		// question is a legitimate literal), and long prose belongs in --context.
		if decisionCtx != "" {
			var err error
			decisionCtx, err = resolveTextValue(decisionCtx)
			if err != nil {
				return fmt.Errorf("--context: %w", err)
			}
		}

		fields := map[string]any{
			"prompt": prompt,
		}

		// Build options from either --options JSON or --option flags.
		if optionsJSON != "" && len(simpleOpts) > 0 {
			return fmt.Errorf("use --options (JSON) or --option (simple), not both")
		}

		if optionsJSON == "" && len(simpleOpts) == 0 {
			return fmt.Errorf("at least one option is required: use --option or --options\n\nExample:\n  gb decision create --no-wait --prompt=\"What should we do?\" --option=\"yes:Proceed:plan\" --option=\"no:Skip:report\"")
		}

		if len(simpleOpts) > 0 {
			// Simple mode: --option "id:label:artifact_type"
			opts := make([]map[string]any, 0, len(simpleOpts))
			for _, s := range simpleOpts {
				opt, err := parseSimpleOption(s)
				if err != nil {
					return err
				}
				opts = append(opts, opt)
			}
			encoded, _ := json.Marshal(opts)
			fields["options"] = json.RawMessage(encoded)
		} else if optionsJSON != "" {
			var opts []map[string]any
			if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
				// Try to detect common mistakes and give helpful errors.
				var strArray []string
				if json.Unmarshal([]byte(optionsJSON), &strArray) == nil {
					return fmt.Errorf("--options must be an array of objects, not strings\n\nGot:      %s\nExpected: [{\"id\":\"a\",\"short\":\"Option A\",\"artifact_type\":\"plan\"},...]\n\nOr use --option flags instead:\n  --option \"a:Option A:plan\" --option \"b:Option B:report\"", optionsJSON)
				}
				return fmt.Errorf("invalid --options JSON: %w\n\nExpected format: [{\"id\":\"a\",\"short\":\"...\",\"label\":\"...\",\"artifact_type\":\"plan\"},...]\n\nOr use --option flags instead:\n  --option \"a:Option A:plan\" --option \"b:Option B:report\"", err)
			}
			for i, opt := range opts {
				at, _ := opt["artifact_type"].(string)
				if at == "" {
					id, _ := opt["id"].(string)
					if id == "" {
						id = fmt.Sprintf("#%d", i)
					}
					return fmt.Errorf("option %s missing required artifact_type (allowed: %s)\n\nEach option must include \"artifact_type\". Example:\n  {\"id\":\"%s\",\"short\":\"...\",\"artifact_type\":\"report\"}", id, beadsapi.ArtifactTypesList(), id)
				}
				if !validArtifactTypes(at) {
					return fmt.Errorf("unknown artifact_type %q on option %d (allowed: %s)", at, i, beadsapi.ArtifactTypesList())
				}
			}
			fields["options"] = json.RawMessage(optionsJSON)
		}
		if decisionCtx != "" {
			fields["context"] = decisionCtx
		}
		if requestedBy == "" {
			requestedBy = actor
		}
		fields["requested_by"] = requestedBy

		agentID, _ := resolveAgentID("")
		if agentID != "" {
			fields["requesting_agent_bead_id"] = agentID
			// Stamp durable lane ownership (ka-0000005031): owner_seat and
			// owner_gardener are read off the spawning agent bead so a decision
			// carries a stable owner that survives respawn. created_by is only the
			// AGENT INSTANCE name and changes every respawn (one lane filed as
			// -nhdq, -7gxg, -p8ai), so seat_key/gardener_id are the durable keys.
			// Best-effort: a lookup failure leaves the fields empty rather than
			// guessing — the decision must not be lost to an owner-stamp failure.
			if seat, gardener := resolveDecisionOwner(cmd.Context(), agentID); seat != "" || gardener != "" {
				if seat != "" {
					fields["owner_seat"] = seat
				}
				if gardener != "" {
					fields["owner_gardener"] = gardener
				}
			}
		}
		// Address the decision to a human (ka-0000000024). Without --for this
		// stamps the author's own operator; with --for it hands the decision to
		// someone else and records who gave it (decision_for.go). The bridge
		// DMs whoever operated_by names.
		addressee, err := decisionAddresseeFields(cmd.Context(), forHuman, agentID)
		if err != nil {
			return err
		}
		for k, v := range addressee {
			fields[k] = v
		}
		// Layer 3 source-prevention (ka-0000002745, design ka-0000006833 §3):
		// when --requesting-task is omitted, auto-derive it from the current
		// agent bead's fields.task_id so the decision still homes onto the
		// agent's task via the blocks-edge + requesting_plan path below,
		// instead of orphaning off every /plans surface. Placed after input
		// validation (so pure validation errors never touch the daemon) and
		// before the requesting_task field-stamp + project resolution so every
		// downstream homing path sees the derived task. Best-effort — leaves
		// requestingTask empty on any resolution failure, and the no-homing
		// warning fires after creation.
		if requestingTask == "" {
			if derived := deriveRequestingTask(cmd.Context(), ""); derived != "" {
				requestingTask = derived
				fmt.Fprintf(os.Stderr, "note: auto-derived --requesting-task %s from agent bead task_id\n", derived)
			}
		}
		if requestingTask != "" {
			fields["requesting_task"] = requestingTask
		}
		// --subject stamps a machine-readable pointer to the decision's SUBJECT
		// gardener/trigger (ka-0000001617). Revival-review decisions carried no
		// such pointer — requesting_agent_bead_id was the reviving crew and
		// requesting_task its own survey task — so a downstream cycle could not
		// tell which gardener a resolved-then-dismissed decision was about, and
		// re-derived the settled fork. Mirrors the requesting_task field stamp
		// above; the graph edge is written best-effort after create below.
		if subject != "" {
			fields["subject_trigger_id"] = subject
		}
		// --evidence names beads whose screenshots every decision surface
		// shows alongside the context (decisionview.EvidenceBeads), so the
		// asker links the bead that already carries them instead of uploading
		// copies onto the decision (gd-JjsT1AZiny). The requesting task is
		// always read too and need not be repeated here.
		if len(evidence) > 0 {
			fields["evidence"] = strings.Join(evidence, ",")
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		priority, _ := cmd.Flags().GetInt("priority")
		force, _ := cmd.Flags().GetBool("force")
		allowDuplicate, _ := cmd.Flags().GetBool("allow-duplicate")

		// Stamp the owning project onto the decision (ka-0000003942). Every
		// other bead type carries a project:<p> label; decisions did not, so
		// `kd list --type=decision -l project:gardenio` returned 0 while 20 open
		// decisions existed — the exact filter the gardener-release-owner
		// runbook (ka-0000000327) Step 1.5 uses to find release blockers.
		project := resolveDecisionProject(cmd.Context(), requestingTask)
		var labels []string
		if project != "" {
			labels = []string{"project:" + project}
		}

		// Refuse to create a decision whose normalized prompt-prefix matches an
		// existing open decision in the same project (ka-0000003825). Uses the
		// same bucket-key normaliser as `gb news` (decisionBucketKey in news.go).
		// --allow-duplicate (or --force) bypasses. Scoped to the SAME project we
		// are about to stamp, so the dedup verdict and the created label always
		// agree on which project this decision belongs to.
		if !allowDuplicate && !force {
			if dup, derr := findDuplicateOpenDecision(cmd.Context(), prompt, project); derr == nil && dup != nil {
				return fmt.Errorf("refusing to create duplicate decision: an open decision %s already shares this prompt prefix.\n\nNext step:\n  kd comment add %s --body '<your update>'   # add to the existing decision\n  gb yield                                  # block on the existing decision\n\nTo bypass (e.g. genuinely distinct decision that happens to share a prefix):\n  gb decision create --allow-duplicate ...", dup.ID, dup.ID)
			}
		}

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:     prompt,
			Type:      "decision",
			Kind:      "data",
			Priority:  priority,
			Assignee:  actor,
			CreatedBy: actor,
			Labels:    labels,
			Fields:    fieldsJSON,
			Force:     force,
		})
		if err != nil {
			return fmt.Errorf("creating decision: %w", err)
		}

		// Materialize the decision→work link as a real dependency edge
		// (ka-0000003927): the requesting task gets a forward `blocks` dep on
		// the decision — "task is blocked by decision" — mirroring what
		// `gb escalation create --requesting-task` already does for escalations
		// (escalation.go, ka-0000004626). Without this edge a decision-blocked
		// task is invisible to the /plans blocker walk, gets none of advice
		// ka-0000001746's banner/badge/nudge machinery, and silently defeats a
		// gate's exclude_blocked_by_open_types filter (conditions/leaf_daemon.go
		// blockingDepTypes only subtracts on a forward blocks/blocked-by/
		// depends-on edge — never the `related` edge decisions used to get). The
		// requesting_task string field alone is invisible to the dependency
		// graph; it is kept for back-compat. Best-effort: the decision exists
		// and must not be lost to a link failure.
		if requestingTask != "" {
			if err := daemon.AddDependency(cmd.Context(), requestingTask, id, "blocks", actor); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: decision created but could not link blocks-edge from %s: %v\n", requestingTask, err)
			}
			// Record the owning /plans plan directly (mirrors escalation.go,
			// ka-0000006860): the nearest plan-labeled epic ancestor of the
			// requesting task. The blocks edge above survives only as long as
			// the step keeps its parent; requesting_plan pins the plan the
			// decision blocked at create time even if the step is reparented.
			// Best-effort — most decisions come from non-plan work and simply
			// have no plan ancestor.
			if planID := resolveOwningPlan(cmd.Context(), requestingTask); planID != "" {
				if err := daemon.UpdateBeadFields(cmd.Context(), id, map[string]string{
					"requesting_plan": planID,
				}); err != nil {
					fmt.Fprintf(os.Stderr, "WARNING: could not record requesting_plan %s: %v\n", planID, err)
				}
			}
		} else {
			// No requesting-task resolved (neither passed nor auto-derived), so
			// no plan ancestor either — the decision homes onto NO /plans
			// surface (ka-0000002745, design ka-0000006833 §3 Layer 3). Warn so
			// the operator/agent knows to home it, or waits for Layer 1's
			// /plans unhomed backstop to surface it.
			fmt.Fprintf(os.Stderr, "WARNING: decision %s has no requesting task or plan ancestor — it is visible only in the flat /decisions inbox (and the /plans unhomed section) until an operator homes it. Pass --requesting-task <your task bead id> to home it.\n", id)
		}

		// Materialize the decision→subject link as a real dependency edge
		// (ka-0000001617): a `related` edge FROM the decision TO the subject
		// trigger, so a resolved revival-review decision is walkable back to the
		// gardener it reviewed (C3's dismissal-labeling reads the pointer; the
		// edge makes the relationship visible in the graph/viewer). Mirrors the
		// requesting-task edge above — the subject_trigger_id field alone is
		// invisible to the dependency graph, and it is kept for direct field
		// reads. Best-effort: the decision already exists and must never be lost
		// to a link failure, so a failed AddDependency is a non-fatal WARNING.
		if subject != "" {
			if err := daemon.AddDependency(cmd.Context(), id, subject, "related", actor); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: decision created but could not link related-edge to subject %s: %v\n", subject, err)
			}
		}
		// A related edge per evidence bead makes the link walkable in the
		// graph; the evidence field is what surfaces read. Best-effort, as above.
		for _, ev := range evidence {
			if err := daemon.AddDependency(cmd.Context(), id, ev, "related", actor); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: decision created but could not link related-edge to evidence %s: %v\n", ev, err)
			}
		}

		if jsonOutput {
			printJSON(map[string]string{"id": id})
		} else {
			fmt.Printf("Created decision: %s\n", id)
		}

		if noWait {
			return nil
		}

		fmt.Fprintf(os.Stderr, "Waiting for response...\n")
		return waitForDecision(cmd, id)
	},
}

// ── decision list ─────────────────────────────────────────────────────

// decisionTruncationNotice returns a one-line notice when a `gb decision list`
// page was clipped by --limit (the daemon's full match count `total` exceeds the
// `shown` rows on screen), or "" when the whole result set is already displayed.
//
// This closes the silent-clip footgun that made per-MR stale-decision sweeps
// unsound (ka-0000008161): the default --limit 20 window gave zero signal that
// 16 further open decisions existed, so a grep over the window read as a false
// all-clear. The suggested limit is `total` so the operator can re-run and see
// every match. Factored out of RunE so the shown/total → notice mapping is
// table-testable.
func decisionTruncationNotice(shown, total int) string {
	if total <= shown {
		return ""
	}
	return fmt.Sprintf("… showing %d of %d decisions — use --limit %d to see all", shown, total, total)
}

var decisionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List decision points",
	Long: `List decision points.

By default only open + in_progress decisions are shown, capped at --limit 20.
When more decisions match than are displayed, a trailing "showing N of M" notice
is printed so a clipped window is never mistaken for a complete result set.
Pass --limit 0 or a larger value to see them all.

Reference-check decisions with --search / --ref (forwarded to the daemon
full-text search over the decision prompt, description, context, and options):

  gb decision list --search 5368     # decisions mentioning MR !5368
  gb decision list --ref ka-abc123   # decisions referencing a bead id

Scope to your OWN open gates with --mine — the double-dispatch guard a gardener
wants before spawning crew. --mine matches the DURABLE lane owner (owner_seat ==
your seat_key), which survives respawn, with a created_by == actor fallback for
rows filed before the owner-stamp existed. This is what lets a respawned gardener
find decisions its lane filed under a PAST instance name:

  gb decision list --mine            # decisions MY lane filed that are open
  gb decision list --lane <seat>     # decisions ANOTHER lane filed that are open

Note: full-text search covers the decision prompt (stored as the bead title),
description, the "context" field, and the option label/short text (ka-0000005839,
migration 040), so a reference that appears ONLY inside an option label or context
now matches. Coverage of pre-migration-040 decisions depends on the one-time
reindex (kd admin backfill-search-tsv --reindex) having run; until then, widen
--limit and eyeball the list when a reference-check must be exhaustive.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		status, _ := cmd.Flags().GetStringSlice("status")
		limit, _ := cmd.Flags().GetInt("limit")
		search, _ := cmd.Flags().GetString("search")
		// --ref is an alias for --search: whichever the caller set wins, so a
		// reference-check reads naturally either way (--ref for bead/MR ids,
		// --search for arbitrary keywords).
		if ref, _ := cmd.Flags().GetString("ref"); ref != "" {
			search = ref
		}

		if len(status) == 0 {
			status = []string{"open", "in_progress"}
		}

		// --mine scopes to decisions THIS lane owns; --lane <seat> inspects
		// another lane. Ownership matches the DURABLE owner_seat (which survives
		// respawn) with a created_by == actor fallback for rows filed before the
		// owner-stamp existed — see resolveOwnershipScope (ka-0000005138).
		scope, err := resolveOwnershipScope(cmd)
		if err != nil {
			return err
		}

		result, err := listOwnedBeads(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"decision"},
			Statuses: status,
			Search:   search,
			Limit:    limit,
			// Sort by priority first (P0 critical → P4 backlog), then newest within
			// the same priority tier. Reflagged decisions bump priority and
			// therefore rise to the top of this list.
			Sort: "priority,-created_at",
		}, scope, false)
		if err != nil {
			return fmt.Errorf("listing decisions: %w", err)
		}

		if jsonOutput {
			// Keep JSON output a bare bead array — consumers parse it directly.
			printJSON(result.Beads)
		} else if len(result.Beads) == 0 {
			fmt.Println("No pending decisions")
		} else {
			for _, b := range result.Beads {
				printDecisionSummary(b)
				fmt.Println()
			}
			if notice := decisionTruncationNotice(len(result.Beads), result.Total); notice != "" {
				fmt.Println(notice)
			}
		}
		return nil
	},
}

// ── decision show ─────────────────────────────────────────────────────

var decisionShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show details of a decision point (incl. a one-line resolution summary)",
	Long: `Show details of a decision point.

For a RESOLVED decision, the output leads with a one-line summary:

  Resolved: <chosen> (<option label>) (by <responded_by> at <responded_at>)

so you never have to guess the jq field names. The bead fields behind this
are "chosen", "responded_by", and "responded_at" — the common guesses
"resolution", "chosen_option", "answer", and "decided_by" do NOT exist.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := daemon.GetBead(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("getting decision %s: %w", args[0], err)
		}

		if jsonOutput {
			printJSON(bead)
		} else {
			printDecisionDetail(bead)
		}
		return nil
	},
}

// ── decision respond ──────────────────────────────────────────────────

var decisionRespondCmd = &cobra.Command{
	Use:   "respond <id>",
	Short: "Respond to a decision point",
	Long: `Respond to a decision point by selecting an option or providing free-text.

Respawn contract (important):
  When a decision is resolved, the Slack bridge checks whether the requesting
  agent is still alive. If the agent is dead AND the chosen value is NOT one
  of the "shutdown keywords" (shut down, shutdown, stand down), NOT "dismissed",
  and NOT "_expired", the bridge respawns a fresh pod to act on the response.

  This means every open decision the operator closes with a real option may
  spawn a new pod. To dismiss a decision without triggering respawn, use
  "gb decision cancel <id>" (sets chosen=dismissed) instead of responding.

  See controller/internal/bridge/decisions.go (respawnAgentForDecision) for
  the authoritative skip list.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		selected, _ := cmd.Flags().GetString("select")
		text, _ := cmd.Flags().GetString("text")

		if selected == "" && text == "" {
			return fmt.Errorf("--select or --text is required")
		}

		fields := map[string]string{}
		if selected != "" {
			fields["chosen"] = selected
		}
		if text != "" {
			fields["response_text"] = text
		}
		fields["responded_by"] = actor
		fields["responded_at"] = time.Now().UTC().Format(time.RFC3339)

		// Look up artifact_type from the chosen option.
		if selected != "" {
			bead, err := daemon.GetBead(cmd.Context(), id)
			if err == nil {
				if at := extractArtifactType(bead.Fields["options"], selected); at != "" {
					fields["required_artifact"] = at
					fields["artifact_status"] = "pending"
				}
			}
		}

		if err := daemon.CloseBead(cmd.Context(), id, fields); err != nil {
			return fmt.Errorf("closing decision %s: %w", id, err)
		}

		if jsonOutput {
			printJSON(map[string]string{"id": id, "status": "closed"})
		} else {
			fmt.Printf("Decision %s resolved\n", id)
		}
		return nil
	},
}

// ── decision cancel ───────────────────────────────────────────────────

var decisionCancelCmd = &cobra.Command{
	Use:   "cancel [<id>...]",
	Short: "Cancel (dismiss) one or more decisions",
	Long: `Cancel one or more decisions by dismissing them.

Cancel sets chosen="dismissed" on the decision and closes the bead, which
tells the Slack bridge NOT to respawn the original agent (unlike "respond",
which respawns dead agents for non-shutdown responses). Use this to clean up
stale decisions without causing pod fan-out.

Examples:
  # Cancel a single decision
  gb decision cancel ka-abc123 --reason="stale from triage cleanup"

  # Cancel multiple decisions
  gb decision cancel ka-abc123 ka-def456 ka-ghi789

  # Bulk-cancel with filters (requires --all)
  gb decision cancel --all --filter-no-options
  gb decision cancel --all --older-than=24h
  gb decision cancel --all --assignee=sched-gardenio-1234567
  gb decision cancel --all --older-than=7d --filter-no-options

Safety: --all is required when using filter flags; IDs and filters are
mutually exclusive. Use --dry-run to preview what would be canceled.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		reason, _ := cmd.Flags().GetString("reason")
		all, _ := cmd.Flags().GetBool("all")
		filterNoOptions, _ := cmd.Flags().GetBool("filter-no-options")
		olderThan, _ := cmd.Flags().GetDuration("older-than")
		assignee, _ := cmd.Flags().GetString("assignee")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		hasFilters := filterNoOptions || olderThan > 0 || assignee != ""

		if len(args) > 0 && (all || hasFilters) {
			return fmt.Errorf("cannot combine explicit IDs with --all or filter flags")
		}
		if hasFilters && !all {
			return fmt.Errorf("filter flags (--filter-no-options, --older-than, --assignee) require --all")
		}
		if all && !hasFilters {
			return fmt.Errorf("--all requires at least one filter flag (--filter-no-options, --older-than, --assignee) for safety")
		}
		if len(args) == 0 && !all {
			return fmt.Errorf("at least one decision ID is required, or use --all with filter flags")
		}

		ids := args
		if all {
			result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
				Types:    []string{"decision"},
				Statuses: []string{"open", "in_progress"},
				Assignee: assignee,
				Sort:     "-created_at",
			})
			if err != nil {
				return fmt.Errorf("listing decisions: %w", err)
			}

			cutoff := time.Time{}
			if olderThan > 0 {
				cutoff = time.Now().Add(-olderThan)
			}
			ids = make([]string, 0, len(result.Beads))
			for _, b := range result.Beads {
				if filterNoOptions && strings.TrimSpace(b.Fields["options"]) != "" {
					continue
				}
				if olderThan > 0 && !b.CreatedAt.IsZero() && b.CreatedAt.After(cutoff) {
					continue
				}
				ids = append(ids, b.ID)
			}
		}

		if len(ids) == 0 {
			if jsonOutput {
				printJSON(map[string]any{"canceled": []string{}, "failed": []string{}})
			} else {
				fmt.Println("No decisions matched")
			}
			return nil
		}

		if dryRun {
			if jsonOutput {
				printJSON(map[string]any{"would_cancel": ids})
			} else {
				fmt.Printf("Would cancel %d decision(s):\n", len(ids))
				for _, id := range ids {
					fmt.Printf("  %s\n", id)
				}
			}
			return nil
		}

		canceled := make([]string, 0, len(ids))
		failed := make(map[string]string, 0)
		for _, id := range ids {
			if err := daemon.CancelDecision(cmd.Context(), id, reason, actor); err != nil {
				failed[id] = err.Error()
				if !jsonOutput {
					fmt.Fprintf(os.Stderr, "Failed to cancel %s: %v\n", id, err)
				}
				continue
			}
			canceled = append(canceled, id)
			if !jsonOutput {
				fmt.Printf("Canceled %s\n", id)
			}
		}

		if jsonOutput {
			printJSON(map[string]any{"canceled": canceled, "failed": failed})
		} else if len(failed) > 0 {
			fmt.Fprintf(os.Stderr, "Canceled %d, failed %d\n", len(canceled), len(failed))
		} else {
			fmt.Printf("Canceled %d decision(s)\n", len(canceled))
		}

		if len(failed) > 0 {
			return fmt.Errorf("%d of %d cancel calls failed", len(failed), len(ids))
		}
		return nil
	},
}

// ── decision reflag ───────────────────────────────────────────────────

var decisionReflagCmd = &cobra.Command{
	Use:   "reflag <id>",
	Short: "Reflag a decision: bump priority and surface it to the top of the list",
	Long: `Reflag a decision so it rises to the top of "gb decision list" and gains a
higher priority. Each reflag decrements the bead's priority by one (toward P0),
records "reflagged_at" + "reflag_count" on the bead, and (when priority reaches
P0/P1) adds the "escalated" label to trigger a Slack repost.

Reflag is idempotent at P0: once a decision is at priority 0 (critical), further
reflag calls only bump the reflag_count and refresh reflagged_at — priority does
not wrap or go negative.

Examples:
  # Bump priority by one toward P0
  gb decision reflag ka-abc123

  # Force a specific priority (skips the +1 bump; useful to jump straight to P0)
  gb decision reflag ka-abc123 --priority=0

  # Reflag with a reason (recorded on the bead and shown in Slack)
  gb decision reflag ka-abc123 --reason="user reported again"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReflag(cmd, args[0], "decision")
	},
}

// runReflag is the shared body of `gb decision reflag` and `gb escalation
// reflag` (ka-0000004918). wantType gates which bead type the command accepts;
// everything else — priority bump toward P0, reflag_count/reflagged_at/
// reflagged_by fields, escalated label at P0/P1, audit comment — is identical
// across the two verbs. The caller's flag set must define --reason and
// --priority.
func runReflag(cmd *cobra.Command, id, wantType string) error {
	reason, _ := cmd.Flags().GetString("reason")
	explicitPriority, _ := cmd.Flags().GetInt("priority")
	priorityWasSet := cmd.Flags().Changed("priority")

	bead, err := daemon.GetBead(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("getting %s %s: %w", wantType, id, err)
	}
	if bead.Type != wantType {
		return fmt.Errorf("%s is not a %s bead (type=%s)", id, wantType, bead.Type)
	}
	if bead.Status == "closed" {
		return fmt.Errorf("%s %s is already closed; reflag has no effect", wantType, id)
	}

	oldPriority := bead.Priority
	var newPriority int
	if priorityWasSet {
		if explicitPriority < 0 || explicitPriority > 4 {
			return fmt.Errorf("--priority must be between 0 (critical) and 4 (backlog), got %d", explicitPriority)
		}
		newPriority = explicitPriority
	} else {
		// Default: bump one step toward P0, capped at 0.
		newPriority = max(oldPriority-1, 0)
	}

	// Increment reflag_count and record reflag metadata.
	count := 0
	if v := bead.Fields["reflag_count"]; v != "" {
		fmt.Sscanf(v, "%d", &count)
	}
	count++
	now := time.Now().UTC().Format(time.RFC3339)

	fields := map[string]string{
		"reflag_count": fmt.Sprintf("%d", count),
		"reflagged_at": now,
		"reflagged_by": actor,
	}
	if reason != "" {
		fields["reflag_reason"] = reason
	}

	// An escalation reflag is also a re-surface request: stamp the pingback
	// handshake fields so the bridge re-posts the escalation card to Slack
	// (ka-0000004385). The "escalated" label alone only triggers a repost for
	// decision beads — the Escalations watcher keys off pingback_requested_at.
	if wantType == "escalation" {
		pingbacks := 0
		if v := bead.Fields["pingback_count"]; v != "" {
			fmt.Sscanf(v, "%d", &pingbacks)
		}
		fields["pingback_requested_at"] = now
		fields["pingback_count"] = fmt.Sprintf("%d", pingbacks+1)
		fields["pingback_by"] = actor
		fields["pingback_source"] = "reflag"
		if reason != "" {
			fields["pingback_reason"] = reason
		}
	}

	// Patch the priority via UpdateBead, fields via UpdateBeadFields. Two calls
	// are required because UpdateBeadRequest.Fields is documented as ignored
	// (handled separately).
	if newPriority != oldPriority {
		if err := daemon.UpdateBead(cmd.Context(), id, beadsapi.UpdateBeadRequest{
			Priority: &newPriority,
		}); err != nil {
			return fmt.Errorf("updating priority on %s: %w", id, err)
		}
	}
	if err := daemon.UpdateBeadFields(cmd.Context(), id, fields); err != nil {
		return fmt.Errorf("updating reflag fields on %s: %w", id, err)
	}

	// When priority is high (P0/P1), add the "escalated" label so the Slack
	// bridge re-posts a :rotating_light: ESCALATED notification. The bridge
	// dedupes per bead with a TTL so repeat reflags at P0 do not spam Slack.
	if newPriority <= 1 {
		labels := append([]string(nil), bead.Labels...)
		if !slices.Contains(labels, "escalated") {
			labels = append(labels, "escalated")
			if err := daemon.UpdateBead(cmd.Context(), id, beadsapi.UpdateBeadRequest{
				Labels: &labels,
			}); err != nil {
				// Non-fatal: priority bump already landed.
				fmt.Fprintf(os.Stderr, "warning: failed to add escalated label: %v\n", err)
			}
		}
	}

	// Audit comment.
	auditMsg := fmt.Sprintf("reflagged by %s: P%d → P%d (count=%d)", actor, oldPriority, newPriority, count)
	if reason != "" {
		auditMsg += fmt.Sprintf("\nreason: %s", reason)
	}
	if err := daemon.AddComment(cmd.Context(), id, actor, auditMsg); err != nil {
		// Non-fatal — audit comment failure should not unwind the reflag.
		fmt.Fprintf(os.Stderr, "warning: failed to record audit comment: %v\n", err)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":           id,
			"old_priority": oldPriority,
			"new_priority": newPriority,
			"reflag_count": count,
			"reflagged_at": now,
		})
	} else {
		fmt.Printf("Reflagged %s: P%d → P%d (count=%d)\n", id, oldPriority, newPriority, count)
	}
	return nil
}

// ── helpers ────────────────────────────────────────────────────────────

// renderDecisionOptions parses a decision's raw `options` field and returns
// one display line per option, with `indent` prefixed to each line. It is
// tolerant of malformed shapes so a decision is NEVER rendered as a blank
// "Options:" header (ka-0000006724):
//
//   - object array [{id,short,label,artifact_type}] → canonical rendering.
//   - string array ["a","b"] → rendered as plain labels (matching the Slack
//     []string fallback), so an old malformed decision is still visible.
//   - anything else / unparseable → a single "⚠ malformed options" warning
//     line echoing the raw JSON, so the operator can see something is wrong.
//
// Returns an empty slice only when optionsRaw is itself empty.
func renderDecisionOptions(optionsRaw, indent string) []string {
	if optionsRaw == "" {
		return nil
	}
	// Decode each element into a RawMessage first so a string-array does not
	// silently collapse into zero-value objects (the original bug).
	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(optionsRaw), &elems); err != nil {
		return []string{fmt.Sprintf("%s⚠ malformed options (not a JSON array): %s", indent, optionsRaw)}
	}
	lines := make([]string, 0, len(elems))
	for _, el := range elems {
		var opt map[string]any
		if err := json.Unmarshal(el, &opt); err != nil {
			// Element is a bare string/number — render it as a plain label.
			var s string
			if json.Unmarshal(el, &s) == nil {
				lines = append(lines, fmt.Sprintf("%s%s", indent, s))
			} else {
				lines = append(lines, fmt.Sprintf("%s⚠ malformed option: %s", indent, string(el)))
			}
			continue
		}
		id, _ := opt["id"].(string)
		label, _ := opt["label"].(string)
		short, _ := opt["short"].(string)
		at, _ := opt["artifact_type"].(string)
		if label == "" {
			label = short
		}
		var line string
		switch {
		case id != "" && short != "" && label != short && label != "":
			line = fmt.Sprintf("%s[%s] %s — %s", indent, id, short, label)
		case id != "":
			line = fmt.Sprintf("%s[%s] %s", indent, id, label)
		default:
			line = fmt.Sprintf("%s%s", indent, label)
		}
		if at != "" {
			line += fmt.Sprintf(" (artifact: %s)", at)
		}
		lines = append(lines, line)
	}
	return lines
}

func printDecisionSummary(b *beadsapi.BeadDetail) {
	prompt := b.Fields["prompt"]
	if prompt == "" {
		prompt = b.Title
	}
	status := b.Status
	chosen := b.Fields["chosen"]
	if chosen != "" {
		status = "resolved: " + chosen
	}

	prefix := fmt.Sprintf("P%d", b.Priority)
	if rc := b.Fields["reflag_count"]; rc != "" && rc != "0" {
		prefix += " reflag×" + rc
	}
	fmt.Printf("  %s [%s] [%s] %s\n", b.ID, prefix, status, prompt)

	for _, line := range renderDecisionOptions(b.Fields["options"], "    ") {
		fmt.Println(line)
	}
}

// decisionResolutionLine returns a single scannable line summarizing how a
// decision was resolved: the chosen option (with its human label when the
// options JSON carries one), who answered, and when. It returns "" for a
// decision that has not been resolved yet.
//
// This exists because the resolution fields are unguessable: agents reading a
// resolved decision after `gb yield` returns probe `.fields.resolution`,
// `.chosen_option`, `.answer`, `.decided_by` — all absent — when the real
// fields are `chosen`, `responded_by`, and `responded_at` (ka-0000000648).
// Surfacing them on one line means no jq field-guessing is ever needed.
func decisionResolutionLine(b *beadsapi.BeadDetail) string {
	chosen := b.Fields["chosen"]
	respText := b.Fields["response_text"]
	if chosen == "" && respText == "" {
		return ""
	}

	var answer string
	switch {
	case chosen != "":
		answer = chosen
		if label := optionLabelForID(b.Fields["options"], chosen); label != "" && label != chosen {
			answer = fmt.Sprintf("%s (%s)", chosen, label)
		}
		if respText != "" {
			answer += fmt.Sprintf(" — %q", respText)
		}
	default:
		answer = fmt.Sprintf("%q", respText)
	}

	by := b.Fields["responded_by"]
	at := b.Fields["responded_at"]
	switch {
	case by != "" && at != "":
		return fmt.Sprintf("%s (by %s at %s)", answer, by, at)
	case by != "":
		return fmt.Sprintf("%s (by %s)", answer, by)
	case at != "":
		return fmt.Sprintf("%s (at %s)", answer, at)
	default:
		return answer
	}
}

// optionLabelForID returns the human-readable label (short, falling back to
// label) for the option whose id matches chosenID, or "" if the options JSON
// is empty/malformed or carries no matching id. Best-effort — never errors.
func optionLabelForID(optionsRaw, chosenID string) string {
	if optionsRaw == "" || chosenID == "" {
		return ""
	}
	var opts []map[string]any
	if err := json.Unmarshal([]byte(optionsRaw), &opts); err != nil {
		return ""
	}
	for _, opt := range opts {
		id, _ := opt["id"].(string)
		if id != chosenID {
			continue
		}
		if short, _ := opt["short"].(string); short != "" {
			return short
		}
		if label, _ := opt["label"].(string); label != "" {
			return label
		}
		return ""
	}
	return ""
}

func printDecisionDetail(b *beadsapi.BeadDetail) {
	fmt.Printf("ID:       %s\n", b.ID)
	fmt.Printf("Status:   %s\n", b.Status)
	fmt.Printf("Priority: P%d\n", b.Priority)
	// One-line resolution summary up top so a `gb decision show <id> | head`
	// (the common way to peek) always surfaces chosen/by/when together.
	if summary := decisionResolutionLine(b); summary != "" {
		fmt.Printf("Resolved: %s\n", summary)
	}
	if rc := b.Fields["reflag_count"]; rc != "" && rc != "0" {
		line := "Reflag:   ×" + rc
		if at := b.Fields["reflagged_at"]; at != "" {
			line += " (last: " + at + ")"
		}
		if by := b.Fields["reflagged_by"]; by != "" {
			line += " by " + by
		}
		if reason := b.Fields["reflag_reason"]; reason != "" {
			line += " — " + reason
		}
		fmt.Println(line)
	}

	prompt := b.Fields["prompt"]
	if prompt != "" {
		fmt.Printf("Prompt:   %s\n", prompt)
	} else {
		fmt.Printf("Title:    %s\n", b.Title)
	}

	if ctx := b.Fields["context"]; ctx != "" {
		fmt.Printf("Context:  %s\n", ctx)
	}

	if optionsRaw := b.Fields["options"]; optionsRaw != "" {
		fmt.Println("Options:")
		for _, line := range renderDecisionOptions(optionsRaw, "  ") {
			fmt.Println(line)
		}
	}

	if chosen := b.Fields["chosen"]; chosen != "" {
		fmt.Printf("Chosen:   %s\n", chosen)
	}
	if respText := b.Fields["response_text"]; respText != "" {
		fmt.Printf("Response: %s\n", respText)
	}
	if respondedBy := b.Fields["responded_by"]; respondedBy != "" {
		fmt.Printf("By:       %s\n", respondedBy)
	}
	if respondedAt := b.Fields["responded_at"]; respondedAt != "" {
		fmt.Printf("At:       %s\n", respondedAt)
	}
	if ra := b.Fields["required_artifact"]; ra != "" {
		fmt.Printf("Artifact: %s (%s)\n", ra, b.Fields["artifact_status"])
	}
}

func waitForDecision(cmd *cobra.Command, id string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()

	// A land door has no daemon event stream; the land feed wakes the wait.
	if land := landDoor(); land != "" {
		return waitDecisionLandFeed(ctx, land, id)
	}

	// Try SSE first, fall back to polling.
	ch, err := daemon.EventStream(ctx, "beads.>")
	if err != nil {
		return waitDecisionPoll(ctx, id)
	}

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return waitDecisionPoll(ctx, id)
			}
			var data map[string]any
			if json.Unmarshal(evt.Data, &data) == nil && evt.Event == "beads.bead.closed" {
				// BeadClosed payload: {"bead": {"id": "...", ...}, "closed_by": "..."}
				// (not "bead_id" which is only used by BeadDeleted events)
				if bead, ok := data["bead"].(map[string]any); ok {
					if beadID, _ := bead["id"].(string); beadID == id {
						return printDecisionResult(id)
					}
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// waitDecisionLandFeed is waitForDecision on a land door: decision.resolved
// for id on the land feed (see land_feed.go) ends the wait, a slow guard poll
// runs beside it, and waitDecisionPoll takes over when the feed cannot be
// reached.
func waitDecisionLandFeed(ctx context.Context, land, id string) error {
	ch, err := streamLandFeed(ctx, land)
	if err != nil {
		return waitDecisionPoll(ctx, id)
	}
	pollTicker := time.NewTicker(landFeedGuardPoll)
	defer pollTicker.Stop()
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return waitDecisionPoll(ctx, id)
			}
			var rec landRecord
			if evt.Type == landEventDecisionResolved && json.Unmarshal(evt.Data, &rec) == nil && rec.ID == id {
				return printDecisionResult(id)
			}
		case <-pollTicker.C:
			if done, err := decisionPollOnce(ctx, id); done {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func waitDecisionPoll(ctx context.Context, id string) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}

		if done, err := decisionPollOnce(ctx, id); done {
			return err
		}
	}
}

// decisionPollOnce reads decision id once and, when it is answered, prints
// the answer and reports done.
func decisionPollOnce(ctx context.Context, id string) (bool, error) {
	bead, err := daemon.GetBead(ctx, id)
	if err != nil {
		return false, nil
	}
	if bead.Status == "closed" {
		return true, printDecisionResult(id)
	}
	if bead.Fields["chosen"] != "" || bead.Fields["response_text"] != "" {
		printDecisionDetail(bead)
		return true, nil
	}
	return false, nil
}

func printDecisionResult(id string) error {
	bead, err := daemon.GetBead(context.Background(), id)
	if err != nil {
		return err
	}
	// Surface chosen + who answered + when on one line so the routine
	// post-`gb yield` read needs no follow-up jq field-guessing (ka-0000000648).
	if summary := decisionResolutionLine(bead); summary != "" {
		fmt.Printf("Decision %s resolved: %s\n", id, summary)
	} else {
		fmt.Printf("Decision %s closed\n", id)
	}
	if ra := bead.Fields["required_artifact"]; ra != "" {
		fmt.Printf("  Artifact required: %s (%s)\n", ra, bead.Fields["artifact_status"])
	}
	return nil
}

// ── decision report ───────────────────────────────────────────────────

var decisionReportCmd = &cobra.Command{
	Use:   "report <decision-id>",
	Short: "Submit a report for a decision that requires one",
	Long: `Submit an artifact for a decision that requires one, then satisfy the gate.

When a decision option has an artifact_type, the agent's gate stays pending
after the decision resolves. This command creates a report bead, links it to
the decision, closes it, and satisfies the decision gate so the agent can
proceed.

Content can be provided via --content flag or piped from stdin. --content
supports curl-style @file expansion: "--content @report.md" reads the file body
(mirroring "kd -d @file"), "--content @-" reads stdin, and "@@" escapes a
literal leading '@'.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		decisionID := args[0]
		content, _ := cmd.Flags().GetString("content")
		reportType, _ := cmd.Flags().GetString("type")
		format, _ := cmd.Flags().GetString("format")

		// Expand curl-style @file / @- / @@ on --content, matching the
		// convention `kd` documents on its prose flags (-d/--title/--notes).
		// Without this a bare "@/path/to/report.md" was silently stored as the
		// literal pointer string, shipping an empty artifact with no warning
		// (ka-0000003861). A missing/typo'd path now fails LOUD.
		if content != "" {
			var err error
			content, err = resolveTextValue(content)
			if err != nil {
				return fmt.Errorf("--content: %w", err)
			}
		}

		// Read from stdin if no --content flag.
		if content == "" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("reading stdin: %w", err)
			}
			content = strings.TrimSpace(string(data))
		}
		if content == "" {
			return fmt.Errorf("--content or stdin is required")
		}

		// Fetch the decision bead to verify and extract artifact type.
		decBead, err := daemon.GetBead(cmd.Context(), decisionID)
		if err != nil {
			return fmt.Errorf("fetching decision %s: %w", decisionID, err)
		}
		if decBead.Type != "decision" {
			return fmt.Errorf("%s is not a decision bead (type=%s)", decisionID, decBead.Type)
		}

		// Extract report type from the decision's required_artifact field if not specified.
		if reportType == "" {
			reportType = decBead.Fields["required_artifact"]
		}
		if reportType == "" {
			reportType = "summary" // default
		}

		if format == "" {
			format = "markdown"
		}

		// tldr length validation: gated on reportType == "tldr". Params come from
		// the chosen option's artifact_params; defaults are max=500, min=0, warn=350.
		if reportType == "tldr" {
			content = strings.TrimSpace(content)
			if content == "" {
				return fmt.Errorf("tldr content is empty: provide a one-paragraph summary of the form \"Root cause: X. Fix: Y. Verified: Z.\"")
			}
			maxChars, minChars, warnAt := tldrLimits(decBead.Fields["options"], decBead.Fields["chosen"])
			n := len(content)
			if n > maxChars {
				return fmt.Errorf("tldr over limit: %d / %d chars. Tighten to the shape \"Root cause: X. Fix: Y. Verified: Z.\" and retry.", n, maxChars)
			}
			if n < minChars {
				return fmt.Errorf("tldr under limit: %d / %d chars (min). Expand to the shape \"Root cause: X. Fix: Y. Verified: Z.\" and retry.", n, minChars)
			}
			if warnAt > 0 && n > warnAt {
				fmt.Fprintf(os.Stderr, "warning: tldr is %d chars (>%d); consider tightening toward \"Root cause: X. Fix: Y. Verified: Z.\"\n", n, warnAt)
			}
		}

		// Create report bead.
		fields := map[string]any{
			"decision_id": decisionID,
			"report_type": reportType,
			"content":     content,
			"format":      format,
		}
		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		reportID, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:     fmt.Sprintf("Report (%s) for %s", reportType, decisionID),
			Type:      "report",
			Kind:      "data",
			Priority:  2,
			CreatedBy: actor,
			Fields:    fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("creating report bead: %w", err)
		}

		// Wire the report bead under at least one LIVE parent. The two
		// preferred parents — the decision and the originating task — are
		// BOTH normally closed by artifact-submit time (the operator resolved
		// the decision; a decision-resume agent's originating task closed with
		// the prior session), so the daemon's closed-parent guard
		// (ka-0000005756) refuses the edge with HTTP 409 and the report is
		// left parentless — the exact orphaning bug this fixes (ka-0000005854).
		// So: attempt each preferred edge, track whether ANY landed, and when
		// none did, fall back to the nearest live ancestor. If even that fails,
		// print a loud, actionable re-parent command instead of two silent 409s.
		parented := false

		// Link report → decision (parent-child dependency).
		if err := daemon.AddDependency(cmd.Context(), reportID, decisionID, "parent-child", actor); err != nil {
			fmt.Fprintf(os.Stderr, "info: report %s not parented on decision %s (likely closed): %v\n", reportID, decisionID, err)
		} else {
			parented = true
		}

		// Link report → originating task (parent-child) so the report shows up
		// on the task that produced the decision, not just on the decision
		// itself. Originating task = parent of the decision bead, OR parent of
		// the requesting_agent_bead_id agent. If neither pathway yields a task
		// (operator-created standalone decision), no edge is added — log INFO.
		// Spec: gardenio/docs/research/sequencer-report-decision-visibility.md
		// (R3 row).
		origTaskID, src, origErr := resolveOriginatingTask(cmd.Context(), decisionID, decBead.Fields["requesting_agent_bead_id"])
		if origErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to resolve originating task for %s: %v\n", decisionID, origErr)
		} else if origTaskID == "" {
			fmt.Fprintf(os.Stderr, "info: decision %s has no inferable originating task; report %s linked to decision only\n", decisionID, reportID)
		} else if err := daemon.AddDependency(cmd.Context(), reportID, origTaskID, "parent-child", actor); err != nil {
			fmt.Fprintf(os.Stderr, "info: report %s not parented on originating task %s (via %s, likely closed): %v\n", reportID, origTaskID, src, err)
		} else {
			parented = true
		}

		// Fallback: both preferred parents were closed (or absent), so the
		// report is still orphaned. Walk up from the decision, then from the
		// originating task, to the nearest LIVE ancestor and parent onto it.
		if !parented {
			fallback := firstOpenAncestor(cmd.Context(), decisionID)
			if fallback == "" && origTaskID != "" {
				fallback = firstOpenAncestor(cmd.Context(), origTaskID)
			}
			if fallback != "" {
				if err := daemon.AddDependency(cmd.Context(), reportID, fallback, "parent-child", actor); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to parent report %s onto live ancestor %s: %v\n", reportID, fallback, err)
				} else {
					parented = true
					fmt.Fprintf(os.Stderr, "info: report %s parented onto nearest live ancestor %s (decision + originating task were closed)\n", reportID, fallback)
				}
			}
		}

		// Last resort: nothing live to hang the report on. Emit a loud,
		// actionable message naming the exact command instead of silent 409s,
		// so the agent (or operator) can re-parent it onto a live epic.
		if !parented {
			fmt.Fprintf(os.Stderr, "Warning: report %s has NO parent — every candidate parent is closed. Re-parent it onto a live epic:\n  kd dep add %s <live-epic> --type=parent-child\n", reportID, reportID)
		}

		// Collect the report into a bundle on the origin task when the decision
		// declared a `bundle` artifact_type (ka-0000007047) OR when the T4
		// opt-in env var is set (GARDENIO_BUNDLE_AUTOCOLLECT=1, ka-0000004397).
		// For a `bundle` artifact the bundle path is unconditional — that is
		// the whole point of the type: the operator asked for a real multi-doc
		// deliverable, so the report doc is attached under a type=bundle child
		// of the origin task and the /beads/bundle/<id> URL is surfaced.
		// Default (any other type, env unset) is byte-for-byte the pre-existing
		// path. Failures here are non-fatal — the report bead is already wired
		// and the gate will still satisfy.
		var bundleViewerURL string
		wantBundle := reportType == "bundle" || os.Getenv("GARDENIO_BUNDLE_AUTOCOLLECT") == "1"
		if origErr == nil && origTaskID != "" && wantBundle {
			bid, err := autoCollectIntoBundle(cmd.Context(), origTaskID, reportID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: collect into bundle failed for origin %s: %v\n", origTaskID, err)
			} else if bid != "" {
				bundleViewerURL = bundleURL(bid)
			}
		} else if reportType == "bundle" && (origErr != nil || origTaskID == "") {
			// A bundle was requested but there is no origin task to hang it on
			// (operator-created standalone decision). The report bead is still
			// wired to a live ancestor above; warn so the missing bundle is not
			// silent.
			fmt.Fprintf(os.Stderr, "Warning: artifact_type=bundle requested but %s has no inferable origin task; report %s delivered as a flat bead\n", decisionID, reportID)
		}

		// Close the report bead.
		if err := daemon.CloseBead(cmd.Context(), reportID, nil); err != nil {
			return fmt.Errorf("closing report bead: %w", err)
		}

		// Advance the DECISION bead's artifact_status pending → submitted now
		// that the report is wired and closed. Without this the field is
		// write-once "pending" and every delivered artifact reads as
		// undelivered forever (ka-0000003940). Best-effort/non-fatal, matching
		// the SatisfyGate / gate_satisfied_by writes below: the report is
		// already delivered at this point, so a failed status write must not
		// fail the command. Only advance when the decision actually declared a
		// required_artifact — a report on a decision that required none should
		// not manufacture an artifact_status.
		if decBead.Fields["required_artifact"] != "" {
			if err := daemon.UpdateBeadFields(cmd.Context(), decisionID, map[string]string{
				"artifact_status": "submitted",
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to advance artifact_status to submitted on %s: %v\n", decisionID, err)
			}
		}

		// Satisfy the decision gate and mark it as yield-satisfied so the stop hook allows exit.
		agentID, _ := resolveAgentID("")
		if agentID != "" {
			satisfyCtx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			if err := daemon.SatisfyGate(satisfyCtx, agentID, "decision"); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to satisfy decision gate: %v\n", err)
			}
			if err := daemon.UpdateBeadFields(context.Background(), agentID, map[string]string{
				"gate_satisfied_by": "yield",
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to set gate_satisfied_by field: %v\n", err)
			}
		}

		if jsonOutput {
			out := map[string]string{"id": reportID, "decision_id": decisionID}
			if bundleViewerURL != "" {
				out["bundle_url"] = bundleViewerURL
			}
			printJSON(out)
		} else {
			fmt.Printf("Report %s submitted for decision %s\n", reportID, decisionID)
			if bundleViewerURL != "" {
				fmt.Printf("  %s\n", bundleViewerURL)
			}
		}
		return nil
	},
}

// autoCollectIntoBundle finds (or lazily creates) a type=bundle child of
// originTaskID and links reportID under that bundle parent-child. Returns the
// bundle id (whether reused or freshly created) so the caller can print the
// /bundle/<id> URL. T4: ka-0000004397.
//
// The lookup walks reverse-mention edges of the origin task and picks the
// first parent-child child whose type is "bundle". This matches how
// gb bundle show enumerates docs (bundle.go::bundleShowCmd).
func autoCollectIntoBundle(ctx context.Context, originTaskID, reportID string) (string, error) {
	bundleID, err := findExistingBundle(ctx, originTaskID)
	if err != nil {
		return "", fmt.Errorf("looking up existing bundle: %w", err)
	}
	if bundleID == "" {
		bundleID, err = createAutoBundle(ctx, originTaskID)
		if err != nil {
			return "", fmt.Errorf("creating bundle: %w", err)
		}
	}
	if err := daemon.AddDependency(ctx, reportID, bundleID, "parent-child", actor); err != nil {
		return bundleID, fmt.Errorf("linking report %s under bundle %s: %w", reportID, bundleID, err)
	}
	return bundleID, nil
}

// findExistingBundle returns the id of the first type=bundle parent-child
// child of originID, or "" when none exists. Errors propagate; an empty
// result with nil error means "no bundle yet".
func findExistingBundle(ctx context.Context, originID string) (string, error) {
	mentions, err := daemon.GetMentionedBy(ctx, originID, 500)
	if err != nil {
		return "", err
	}
	for _, m := range mentions {
		if m.Type != "bundle" {
			continue
		}
		if m.RelationshipType == nil {
			continue
		}
		// Accept legacy "parent-child" plus the surface verbs "parent" and
		// "child-of" written under surface-verb storage (M0 of ka-0000000933).
		switch *m.RelationshipType {
		case "parent-child", "parent", "child-of":
		default:
			continue
		}
		return m.ID, nil
	}
	return "", nil
}

// createAutoBundle creates a type=bundle bead parented to originID and
// returns its id. Title is derived from the origin's title (falling back to
// the origin id) so the bundle is recognisable in lists.
func createAutoBundle(ctx context.Context, originID string) (string, error) {
	originTitle := originID
	if origin, err := daemon.GetBead(ctx, originID); err == nil && origin.Title != "" {
		originTitle = origin.Title
	}

	fields, err := json.Marshal(map[string]any{"kind": "report"})
	if err != nil {
		return "", fmt.Errorf("encoding bundle fields: %w", err)
	}

	bundleID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     fmt.Sprintf("%s — Bundle", originTitle),
		Type:      "bundle",
		Kind:      "issue",
		Priority:  2,
		CreatedBy: actor,
		Fields:    fields,
	})
	if err != nil {
		return "", fmt.Errorf("creating bundle bead: %w", err)
	}
	if err := daemon.AddDependency(ctx, bundleID, originID, "parent-child", actor); err != nil {
		return bundleID, fmt.Errorf("linking bundle %s to origin %s: %w", bundleID, originID, err)
	}
	return bundleID, nil
}

// resolveOriginatingTask returns the bead id of the task a decision was filed
// against, or "" if neither pathway resolves. Pathways, in order:
//
//  1. parent-child parent of the decision bead itself (operator escalation
//     flow: gb decision create with --parent or filed under a task);
//  2. parent-child parent of the requesting_agent_bead_id agent (agent yielded
//     mid-task: the agent bead is parented under the task it was working on).
//
// The second return value names the pathway used ("decision-parent" or
// "agent-parent") for log/error messages. An empty originating task is not an
// error — operator-created standalone decisions legitimately have no task.
func resolveOriginatingTask(ctx context.Context, decisionID, requestingAgentBeadID string) (string, string, error) {
	if decisionID != "" {
		deps, err := daemon.GetDependencies(ctx, decisionID)
		if err != nil {
			return "", "", fmt.Errorf("getting deps for decision %s: %w", decisionID, err)
		}
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				return d.DependsOnID, "decision-parent", nil
			}
		}
	}
	if requestingAgentBeadID != "" {
		deps, err := daemon.GetDependencies(ctx, requestingAgentBeadID)
		if err != nil {
			return "", "", fmt.Errorf("getting deps for agent %s: %w", requestingAgentBeadID, err)
		}
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				return d.DependsOnID, "agent-parent", nil
			}
		}
	}
	return "", "", nil
}

// firstOpenAncestor walks the parent-child chain starting AT startID and returns
// the id of the first bead whose status is not "closed" — i.e. the nearest
// live ancestor a report bead can be parented onto without tripping the daemon's
// closed-parent guard (ka-0000005756, which refuses an edge onto a closed parent
// with HTTP 409, and — matching that guard — treats only "closed" as ineligible;
// deferred/blocked/in_progress parents are fine). Returns "" when the chain has
// no live bead within reportAncestorDepth levels, when startID is empty, or on
// the first lookup error (best-effort: parenting is non-fatal to the report).
//
// This is the fallback used by `gb decision report` when both preferred parents
// (the decision and the originating task) are already closed by artifact-submit
// time — the exact orphaning bug ka-0000005854 describes.
func firstOpenAncestor(ctx context.Context, startID string) string {
	visited := make(map[string]bool, 8)
	cur := startID
	for range reportAncestorDepth {
		if cur == "" || visited[cur] {
			return ""
		}
		visited[cur] = true

		b, err := daemon.GetBead(ctx, cur)
		if err != nil {
			return ""
		}
		if b.Status != "closed" {
			return cur
		}

		deps, err := daemon.GetDependencies(ctx, cur)
		if err != nil {
			return ""
		}
		next := ""
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				next = d.DependsOnID
				break
			}
		}
		cur = next
	}
	return ""
}

// reportAncestorDepth bounds the fallback parent-chain walk in firstOpenAncestor.
// Decision→task→epic trees are 2-3 levels; 32 mirrors beadsapi.maxAncestorDepth
// and terminates in O(round-trips) on a degenerate/cyclic graph.
const reportAncestorDepth = 32

// maxDecisionPromptRunes is the client-side cap on a decision --prompt. The
// prompt is stored as the decision bead's Title, which the server validates at
// 500 runes (kbeads/internal/model/validate.go ValidateBead). Keep this in sync
// with that limit so the fail-fast message matches the server's actual rule.
const maxDecisionPromptRunes = 500

// checkDecisionPromptLength fails fast when a decision --prompt exceeds the
// server's title cap (ka-0000006105). The prompt becomes the decision bead's
// Title, capped server-side at maxDecisionPromptRunes runes. Counting runes
// (not bytes) matches the server's len([]rune(...)) rule exactly, so a prompt
// of multi-byte characters is judged by the same measure. The message names
// --prompt (not the server's "title" field) and points the agent at --context
// for the overflow. Returns nil when the prompt is within the limit.
func checkDecisionPromptLength(prompt string) error {
	if n := len([]rune(prompt)); n > maxDecisionPromptRunes {
		return fmt.Errorf("--prompt is %d characters; the decision prompt is the title and must be %d or fewer.\n\nKeep --prompt to a one-line question and move the background/detail into --context (not length-limited):\n  gb decision create --prompt=\"<short question>\" --context=\"<all the detail>\" --option=\"a:...:plan\" --option=\"b:...:report\"", n, maxDecisionPromptRunes)
	}
	return nil
}

// validArtifactTypes reports whether at is an allowed artifact_type for a
// decision option. It delegates to beadsapi.IsValidArtifactType so the
// allowlist has one source of truth (beadsapi.ArtifactTypes), shared with the
// Slack modal dropdown and the error strings below.
func validArtifactTypes(at string) bool {
	return beadsapi.IsValidArtifactType(at)
}

// parseSimpleOption parses "id:label:artifact_type" into a map.
// The artifact_type defaults to "report" if omitted.
func parseSimpleOption(s string) (map[string]any, error) {
	parts := strings.SplitN(s, ":", 3)
	switch len(parts) {
	case 1:
		// Just an ID — use as both id and label.
		return map[string]any{
			"id": parts[0], "short": parts[0],
			"label": parts[0], "artifact_type": "report",
		}, nil
	case 2:
		return map[string]any{
			"id": parts[0], "short": parts[1],
			"label": parts[1], "artifact_type": "report",
		}, nil
	case 3:
		at := parts[2]
		if at == "" {
			at = "report"
		}
		if !validArtifactTypes(at) {
			return nil, fmt.Errorf("unknown artifact_type %q in --option %q (allowed: %s)", at, s, beadsapi.ArtifactTypesList())
		}
		return map[string]any{
			"id": parts[0], "short": parts[1],
			"label": parts[1], "artifact_type": at,
		}, nil
	default:
		return nil, fmt.Errorf("invalid --option format %q, expected \"id:label:artifact_type\"", s)
	}
}

// tldrLimits returns (max_chars, min_chars, warn_at) for the chosen option.
// Defaults: max=500, min=0, warn=350. Any error in parsing falls back to defaults.
func tldrLimits(optionsJSON, chosenID string) (int, int, int) {
	maxChars, minChars, warnAt := 500, 0, 350
	var opts []map[string]any
	if optionsJSON == "" || json.Unmarshal([]byte(optionsJSON), &opts) != nil {
		return maxChars, minChars, warnAt
	}
	for _, opt := range opts {
		id, _ := opt["id"].(string)
		if id != chosenID {
			continue
		}
		params, _ := opt["artifact_params"].(map[string]any)
		if params == nil {
			break
		}
		if v, ok := toInt(params["max_chars"]); ok {
			maxChars = v
		}
		if v, ok := toInt(params["min_chars"]); ok {
			minChars = v
		}
		if v, ok := toInt(params["warn_at"]); ok {
			warnAt = v
		}
		break
	}
	return maxChars, minChars, warnAt
}

// toInt coerces a JSON-decoded number (float64) or int into int.
func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

// extractArtifactType looks up the chosen option and returns its artifact_type (if any).
func extractArtifactType(optionsJSON, chosenID string) string {
	var opts []map[string]any
	if json.Unmarshal([]byte(optionsJSON), &opts) != nil {
		return ""
	}
	for _, opt := range opts {
		if id, _ := opt["id"].(string); id == chosenID {
			if at, _ := opt["artifact_type"].(string); at != "" {
				return at
			}
		}
	}
	return ""
}

// resolveDecisionProject determines which project a new decision belongs to.
// Precedence: the requesting task's own project:<p> label (the work the
// decision blocks is the most accurate owner — it is correct even when a
// gardener drives a task in another project), then the ambient
// KD_PROJECT/BOAT_PROJECT env. Returns "" when neither yields a project, in
// which case the decision is created unlabeled exactly as before.
//
// Reading the label off the requesting task rather than trusting the env alone
// is what makes the stamp correct for cross-project work; the env fallback is
// what makes the (common) no---requesting-task call still get labeled.
func resolveDecisionProject(ctx context.Context, requestingTask string) string {
	if requestingTask != "" {
		if bead, err := daemon.GetBead(ctx, requestingTask); err == nil && bead != nil {
			for _, l := range bead.Labels {
				if p, ok := strings.CutPrefix(l, "project:"); ok && p != "" {
					return p
				}
			}
		}
	}
	return defaultProject()
}

// resolveDecisionOwner reads the durable lane ownership (owner_seat,
// owner_gardener) for a decision off the spawning agent bead (ka-0000005031).
// The agent bead carries seat_key (the stable lane key, unchanged across
// respawn) and gardener_id (the owning gardener, canonically stamped at spawn),
// while created_by is only the ephemeral agent-instance name. Returns
// (seat, gardener); either may be empty when the agent bead is unresolvable or
// carries no seat_key. gardener_id is normalised through DeriveGardenerID so a
// legacy agent bead written before the at-spawn stamp still resolves to the
// same synthetic owner the stamp would have assigned. Best-effort: a daemon
// error yields ("", "") rather than failing the caller.
func resolveDecisionOwner(ctx context.Context, agentBeadID string) (seat, gardener string) {
	if agentBeadID == "" {
		return "", ""
	}
	bead, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil || bead == nil {
		return "", ""
	}
	seat = strings.TrimSpace(bead.Fields["seat_key"])
	gardener = strings.TrimSpace(beadsapi.DeriveGardenerID(bead.Fields))
	return seat, gardener
}

// findDuplicateOpenDecision returns the first open decision in the given
// project whose prompt shares a bucket key with the provided prompt. The
// normaliser (decisionBucketKey) is the same one used by `gb news`, so the
// dedup verdict from `gb news` and `gb decision create` always agree. An empty
// project searches across all projects. Returns (nil, nil) when no duplicate
// exists. Daemon errors are returned to the caller so the create can decide
// whether to soft-fail or surface the error.
//
// The project is passed in rather than re-read from the env so it matches the
// label the caller is about to stamp. While decisions carried no project label
// at all (ka-0000003942) this filter matched zero rows on every call, silently
// disabling the dedup guard; scoping both to the same resolved project is what
// makes the guard actually fire.
func findDuplicateOpenDecision(ctx context.Context, prompt, project string) (*beadsapi.BeadDetail, error) {
	target := decisionBucketKey(prompt)
	if target == "" {
		return nil, nil
	}
	var labels []string
	if project != "" {
		labels = []string{"project:" + project}
	}
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Labels:   labels,
		Sort:     "-created_at",
		Limit:    100,
	})
	if err != nil {
		return nil, err
	}
	for _, b := range res.Beads {
		p := b.Fields["prompt"]
		if p == "" {
			p = b.Title
		}
		if decisionBucketKey(p) == target {
			return b, nil
		}
	}
	return nil, nil
}

// senderFromLabels extracts the sender name from a "from:<name>" label.
func senderFromLabels(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, "from:") {
			return strings.TrimPrefix(l, "from:")
		}
	}
	return ""
}

func init() {
	decisionCmd.AddCommand(decisionCreateCmd)
	decisionCmd.AddCommand(decisionListCmd)
	decisionCmd.AddCommand(decisionShowCmd)
	decisionCmd.AddCommand(decisionRespondCmd)
	decisionCmd.AddCommand(decisionReportCmd)
	decisionCmd.AddCommand(decisionCancelCmd)
	decisionCmd.AddCommand(decisionReflagCmd)
	decisionCmd.AddCommand(newSuccessorCmd("decision"))

	decisionCreateCmd.Flags().String("prompt", "", "decision prompt (required). One-line TITLE, capped at 500 chars — a leading '@' is stored literally (no @file expansion); put file-sourced prose in --context.")
	decisionCreateCmd.Flags().String("title", "", "alias for --prompt (accepted for convenience)")
	decisionCreateCmd.Flags().String("options", "", "options JSON array")
	decisionCreateCmd.Flags().StringArray("option", nil, `simple option: "id:label:artifact_type" (repeatable, artifact_type defaults to "report")`)
	decisionCreateCmd.Flags().String("requested-by", "", "who is requesting (default: actor)")
	decisionCreateCmd.Flags().String("requesting-task", "", "your current task bead id; writes a blocks-edge from that task to the decision so the blocked work (and its /plans plan) reads as blocked and exclude_blocked_by_open_types can subtract it (ka-0000003927). If omitted, auto-derived from the current agent bead's task_id (ka-0000002745)")
	decisionCreateCmd.Flags().String("for", "", "address this decision to a specific human: an email, a Slack id/mention (U0AB1CD2EF, <@U0AB1CD2EF>), or an explicit identity (oidc:…, slack:…). Stamps operated_by, which is who the bridge DMs the decision card to. Naming someone other than yourself is allowed and expected — it hands them the decision, recording you in gifted_by (ka-0000000024).")
	decisionCreateCmd.Flags().String("subject", "", "the trigger/gardener this decision is ABOUT (e.g. a revival-review's subject gardener schedule); stamps the machine-readable field subject_trigger_id and writes a related-edge from the decision to that trigger so a resolved decision is walkable back to its subject (ka-0000001617)")
	decisionCreateCmd.Flags().StringArray("evidence", nil, "a bead whose screenshots (image links in its comments and description) the decision shows with its context; repeatable or comma-separated, at most 2: the requesting task is always read too and takes the third slot (gd-JjsT1AZiny)")
	decisionCreateCmd.Flags().String("context", "", "background context for the decision (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@')")
	decisionCreateCmd.Flags().Bool("no-wait", false, "return immediately without waiting for response")
	decisionCreateCmd.Flags().Int("priority", 2, "decision priority: 0=critical, 1=high, 2=normal, 3=low, 4=backlog")
	decisionCreateCmd.Flags().Bool("force", false, "bypass server-side decision content-shape guards (zero options, single ack/done/continue option). Use only for legitimate edge cases — see ka-0000002237.")
	decisionCreateCmd.Flags().Bool("allow-duplicate", false, "bypass client-side dedup: skip the open-decision prompt-prefix match check (ka-0000003825). Use when the new decision is genuinely distinct from an existing sibling that happens to share a prefix.")

	decisionListCmd.Flags().StringSliceP("status", "s", nil, "filter by status")
	decisionListCmd.Flags().Int("limit", 20, "maximum number of results (0 = daemon default, up to 1000)")
	decisionListCmd.Flags().String("search", "", "reference-check decisions by MR number / bead id / keyword (daemon full-text search over prompt + description)")
	decisionListCmd.Flags().String("ref", "", "alias for --search: reference-check decisions by MR number / bead id / keyword")
	decisionListCmd.Flags().Bool("mine", false, "only decisions MY lane owns (owner_seat = my seat_key, with a created_by = --actor fallback); survives respawn. Errors when neither a seat nor an actor is resolvable")
	decisionListCmd.Flags().String("lane", "", "only decisions owned by another lane, by its seat_key (owner_seat = <seat>); mutually exclusive with --mine")

	decisionRespondCmd.Flags().String("select", "", "selected option ID")
	decisionRespondCmd.Flags().String("text", "", "free-text response")

	decisionReportCmd.Flags().String("content", "", "report content (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@'), or pipe from stdin")
	decisionReportCmd.Flags().String("type", "", "report type (default: from decision label or 'summary')")
	decisionReportCmd.Flags().String("format", "markdown", "content format: markdown, json, text")

	decisionCancelCmd.Flags().String("reason", "", "reason for cancellation (recorded on the bead)")
	decisionCancelCmd.Flags().Bool("all", false, "cancel all open decisions matching the filter flags (required for bulk)")
	decisionCancelCmd.Flags().Bool("filter-no-options", false, "only cancel decisions that have no options set")
	decisionCancelCmd.Flags().Duration("older-than", 0, "only cancel decisions created more than this long ago (e.g. 24h, 7d)")
	decisionCancelCmd.Flags().String("assignee", "", "only cancel decisions with this assignee")
	decisionCancelCmd.Flags().Bool("dry-run", false, "print what would be canceled without calling the API")

	decisionReflagCmd.Flags().String("reason", "", "reason for reflag (recorded as audit comment and included in Slack repost)")
	decisionReflagCmd.Flags().Int("priority", -1, "set priority explicitly (0=critical .. 4=backlog) instead of bumping by one toward P0")

}

// evidenceBeadIDs reads --evidence: repeatable, each value may be a comma or
// space separated list. Ids are trimmed and de-duplicated; one that does not
// look like a bead id, or more than decisionview.MaxEvidenceFlag of them, is
// refused before anything is written (the last slot is the requesting task's).
func evidenceBeadIDs(cmd *cobra.Command) ([]string, error) {
	raw, _ := cmd.Flags().GetStringArray("evidence")
	var out []string
	seen := map[string]bool{}
	for _, v := range raw {
		for _, id := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			if seen[id] {
				continue
			}
			if !evidenceIDRe.MatchString(id) {
				return nil, fmt.Errorf("--evidence %q is not a bead id", id)
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > decisionview.MaxEvidenceFlag {
		return nil, fmt.Errorf("--evidence names %d beads; at most %d (the requesting task is always read and takes the last slot)", len(out), decisionview.MaxEvidenceFlag)
	}
	return out, nil
}

var evidenceIDRe = regexp.MustCompile(`^[a-z]{1,8}-[A-Za-z0-9]+$`)
