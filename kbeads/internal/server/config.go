package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mrFields are the GitLab MR-tracking fields maintained by the gitlab-bridge
// for any issue-kind bead that has an MR linked via `gb mr link`. They are
// shared across issue-kind types (task, bug, epic, feature, chore) so the
// bridge can attach MR state to whatever bead a human or agent chose to link.
//
// mr_close_override is the deliberate-close marker (ka-0000000267, direction 2
// of ka-0000003953): `kd close --allow-open-mr` stamps it "true" so the
// gitlab-bridge reconcile sweep — which re-opens closed beads whose linked MR
// is still open — knows THIS close was intentional (e.g. a won't-fix bug whose
// MR was abandoned) and must not be re-opened. Without it, directions 1 and 2
// would fight: the guard lets you force-close, the sweep would immediately
// undo it. A string (not boolean) to match every other field here and keep the
// bridge's string-map projection uniform.
//
// trickle_checklist is the structured trickle-eligibility decision (bundle
// ka-0000005053 §3/§5, task ka-0000004967) that `gb mr trickle-gate --write`
// stamps onto an MR bead: a JSON blob {eligible, reason, checklist[]} the
// beads-viewer trickle-checklist renderer (cmd/beads-viewer/trickle_checklist.go)
// reads back to draw the per-MR HARD/ADVISORY grid. Declared here (issue-kind
// types set allow_additional_fields=false) so the write is accepted rather than
// 400'd — the closed-schema prerequisite §6.17 flags; no DDL, a daemon-config
// addition only. A string carrying the serialized JSON, matching the uniform
// string-map projection.
//
// mr_merge_state, mr_conflict_* and mr_overlaps are the PR conflict facts the
// github-bridge writes on MR beads and their task beads, so triggers can fire
// on a conflict (plan gd-nTCENSwMOH). Values, writers and clearing rules are
// in docs/pr-conflicts.md. Guarded by TestBuiltinMRConflictFieldsDeclared.
const mrFields = `
		{"name": "forge",                  "type": "string"},
		{"name": "mr_url",                 "type": "string"},
		{"name": "mr_iid",                 "type": "string"},
		{"name": "mr_title",               "type": "string"},
		{"name": "mr_state",               "type": "string"},
		{"name": "mr_merged",              "type": "string"},
		{"name": "mr_pipeline_status",     "type": "string"},
		{"name": "mr_pipeline_url",        "type": "string"},
		{"name": "mr_approved",            "type": "string"},
		{"name": "mr_approvers",           "type": "string"},
		{"name": "mr_has_review_comments", "type": "string"},
		{"name": "gitlab_mr_iid",          "type": "string"},
		{"name": "gitlab_project_id",      "type": "string"},
		{"name": "gitlab_discussion_id",   "type": "string"},
		{"name": "gitlab_mr_source_branch","type": "string"},
		{"name": "mr_close_override",      "type": "string"},
		{"name": "trickle_checklist",      "type": "string"},
		{"name": "mr_merge_state",         "type": "string"},
		{"name": "mr_conflict_files",      "type": "string"},
		{"name": "mr_conflict_kind",       "type": "string"},
		{"name": "mr_conflict_since",      "type": "string"},
		{"name": "mr_conflict_base",       "type": "string"},
		{"name": "mr_conflict_cause",      "type": "string"},
		{"name": "mr_overlaps",            "type": "string"}`

// auditFields are the cross-cutting human-attribution fields the bridge
// stamps on issue- and agent-kind beads at create time so the server-side
// resolver fallback (operatedByFromAgentBead) and audit trail can identify
// the originating human. operated_by is also persisted as a top-level
// bead column via the X-Operated-By header; the Fields entry is the
// inheritance source for downstream static-token writes from the spawned
// agent that don't carry the header (R1 #10 push-at-create).
//
// slack_user_id rides alongside operated_by so the server-side auto-inherit
// (ka-0000005990) can stamp it from a "slack:U…" operated_by on trigger,
// gardener→crew, and Slack /spawn task beads. Bot-DM notification targeting
// reads this field directly, so the legacy per-site pushes in
// bot_commands.go and scheduler.go remain idempotent. idempotency_key rides
// alongside for `kd create --idempotency-key` dedup (ka-0000002856).
const auditFields = `
		{"name": "operated_by",     "type": "string"},
		{"name": "user_bead_id",    "type": "string"},
		{"name": "slack_user_id",   "type": "string"},
		{"name": "gitlab_username", "type": "string"},
		{"name": "jira_user_id",    "type": "string"},
		{"name": "idempotency_key", "type": "string"}`

// reproFields are the repro-harness gate fields the repro-harness formula's
// Phase-4 emit step (ka-0000002084) writes onto the emit-step bead, and mirrors
// (gate_passed + repro_evidence + repro_scorecard) onto the triage bead per
// formula step 3. The fsm-triage routing gate blocks on gate_passed=true, so
// these must be queryable typed fields, not label surrogates (ka-0000002579):
//
//   - gate_passed:     did the repro clear the per-axis critic gate. Optional,
//     default null (absent).
//   - repro_evidence:  URL of the mandatory video/screenshot evidence backing a
//     passed gate. Optional. The "must be a valid URL when gate_passed=true"
//     cross-field constraint isn't expressible in FieldDef, so it's enforced by
//     validateReproFields at the create/update choke point.
//   - repro_ticket:    free-form ticket the repro pertains to (FSM-XXXX or
//     ka-XXXXXXXX). Optional.
//   - repro_scorecard: the per-axis critic panel scores (ka-0000008255) as a
//     queryable JSON blob so the repro-cockpit catalog can read them without
//     scraping a comment. Optional; typed string (the panel serializes to
//     JSON text) so a scorecard of any shape is accepted (ka-0000003501).
//
// Declared on task (the emit-step bead type; formula step beads default to
// task), molecule (the pour-level epic), and rich-report (the type fsm-triage
// actually creates its triage beads as) so the emit write AND the triage mirror
// are accepted on every bead type the formula touches instead of 400'ing on an
// unknown field (ka-0000003501). Reference the const from each type's schema
// rather than re-listing the fields inline so a new gate field can never be
// added to one type and silently missed on another (the drift that left
// repro_scorecard unprovisioned and the fields absent on rich-report).
// Guarded by TestBuiltinReproFieldsDeclared.
const reproFields = `
		{"name": "gate_passed",     "type": "boolean"},
		{"name": "repro_evidence",  "type": "string"},
		{"name": "repro_ticket",    "type": "string"},
		{"name": "repro_scorecard", "type": "string"}`

// workspaceField is the per-bead worktree-recovery metadata `gb workspace
// setup` stores so a crew/gardener can recover its isolated worktree after a
// pod restart. storeWorkspaceInfo (gardenio/controller/cmd/gb/workspace.go)
// PATCHes it as a JSON string ({"branch","worktree_path","base_branch"});
// resolveBaseBranch reads it back off the bead — and off PARENT beads (usually
// epics) to stack a child worktree on its parent's branch. It is shared across
// every issue-kind type because setup runs on whatever bead the agent claimed
// (task dominant, but bug/feature/epic are valid targets) and the reader walks
// parents of any type. Its absence 400'd EVERY `gb workspace setup` write
// ("workspace: unknown field") — the worktree was created on disk but the
// recovery-metadata write was rejected, silently making worktree
// recovery-after-restart a no-op for the standard MR-isolation workflow
// (ka-0000006623). Same unknown-field 400 family as the formula-step
// (ka-0000002559) and repro-gate (ka-0000002579) fields. Guarded by
// TestBuiltinWorkspaceFieldDeclared.
const workspaceField = `
		{"name": "workspace", "type": "string"}`

// resumeChainField is the S3 transcript-continuity breadcrumb the gardenio
// controller stamps on a WORKED TASK bead when a crew agent is involuntarily
// killed (reopenOrphanedClaimedTask, gardenio controller/cmd/gb/hook.go). Because
// a gardener-driven respawn mints a brand-new agent bead ID — and coop keys its
// S3 session prefix on that id — the new pod would probe an empty archive and
// cold-start, orphaning the prior transcript under the ORIGINAL bead ID
// (ka-0000003160). The dying pod records the canonical S3 key here (the one
// field stable across the respawn is the task_id), and the next pod
// (resolveCanonicalS3Key, gardenio controller/cmd/gb/init_s3_restore.go) reads it
// back to resume under the canonical prefix. Declared on every issue-kind type
// because the claimed work can be a task/bug/feature/epic. Its absence would
// 400 the handoff write ("resume_s3_key: unknown field") — same unknown-field
// family as the workspace (ka-0000006623) and formula-step (ka-0000002559)
// fields. Guarded by TestBuiltinResumeChainFieldDeclared.
const resumeChainField = `
		{"name": "resume_s3_key", "type": "string"}`

// reopenFields are the reopen-attempt accounting fields (ka-0000002985 DEFECT 3)
// stamped on a WORKED ISSUE bead each time an involuntary agent death returns it
// to the ready queue, and when the cap parks it for a human instead. The names
// are the beadsapi.Reopen*Field constants in gardenio
// controller/internal/beadsapi/reopen.go; call sites use the constants, so grep
// for those, not the literals. Two writers send them, each in ONE PATCH together
// with the status change and the assignee clear:
//
//   - the controller SIGKILL backstop (statusreporter/stalereaper.go
//     reopenStrandedIssueTasks + parkCappedStrandedTask), and
//   - the in-pod stop-gate (cmd/gb/hook_beadclose.go reopenOrphanedClaimedTask +
//     parkCappedTask).
//
// Both select by kind=issue with no type filter, so the fields go on every
// issue-kind type via issueFields. All three are strings: reopen_attempts is
// written with strconv.Itoa and read back by ParseReopenAttempts from the
// string-map projection, so an "integer" type would reject the write;
// reopen_capped is written as "true". While undeclared, every one of those
// PATCHes 400'd "reopen_attempts: unknown field" and the reopen rolled back
// with it, so the stale-reaper retried the same bead every pass and never
// reopened anything (gd-aJze4IIIEC). Guarded by
// TestReopenAccountingFields_MatchControllerConstants and
// TestReopenStrandedIssue_PatchValidatesAgainstBuiltinSchema.
//
// reaped_assignee rides the same patches, and the stale reaper's half-reap
// clear: the gone agent whose assignee was cleared. The controller's reclaim
// grant reads it (the assignee_reaped_agent condition, gd-8Irb2vDFf7).
const reopenFields = `
		{"name": "reopen_attempts",   "type": "string"},
		{"name": "reopen_capped",     "type": "string"},
		{"name": "reopen_cap_reason", "type": "string"},
		{"name": "reaped_assignee",   "type": "string"}`

// triggerSourceFields are the provenance + bead-source match fields the trigger
// engine's create_bead / assert actions stamp onto the ISSUE bead they file
// (gardenio controller/internal/bridge/triggers.go createTaskBead +
// trigger_source_bead.go beadEventToMatch / beadAssertViolationToMatch):
//
//   - trigger_id / trigger_name / trigger_created_by — createTaskBead stamps
//     these on EVERY filed bead as trigger provenance, regardless of source.
//   - bead_event_key — the per-event dedup key beadEventToMatch writes into
//     TriggerMatch.Fields. createTaskBead copies TriggerMatch.Fields onto the
//     bead, and CatchUpKeys (trigger_source_bead.go) reads bead_event_key BACK
//     off the filed beads after an engine restart to re-seed the seen set — so
//     it must be a PERSISTED typed field, not filtered out.
//   - source_bead_id / source_event — the id + event name of the bead whose
//     state change fired the trigger, for lineage (writeTriggerLineage also
//     reads source_bead_id back off the match).
//   - assert_violated / assert_values — the failed CEL clause + observed values
//     an assert trigger with on_violation=create_bead|both records on the filed
//     bead (beadAssertViolationToMatch).
//
// Declared on every issue-kind type because a create_bead trigger may file any
// type in createBeadTypeAllowlist (task, bug, feature, chore) — and an assert
// trigger's create_bead commonly files a bug. type:task/bug/feature/chore set
// allow_additional_fields=false, so a missing registration 400'd EVERY
// bead-source create_bead / assert fire at bead creation
// ("bead_event_key: unknown field"), the sibling of the agent-spawn bead_key
// 400 fixed for the apply_formula path (ka-0000001885). This is the create_bead
// task/bug path (ka-0000005502). Guarded by TestResolveTypeConfig_BeadSourceCreateBeadFields.
const triggerSourceFields = `
		{"name": "trigger_id",            "type": "string"},
		{"name": "trigger_name",          "type": "string"},
		{"name": "trigger_created_by",    "type": "string"},
		{"name": "bead_event_key",        "type": "string"},
		{"name": "source_bead_id",        "type": "string"},
		{"name": "source_event",          "type": "string"},
		{"name": "assert_violated",       "type": "string"},
		{"name": "assert_values",         "type": "string"}`

// harvestFields are the quality signals written on a step when its work lands,
// which the Lands harvest cell reads (spec
// gardenio/docs/superpowers/specs/2026-10-22-lands-harvest-icons-design.md §2.3,
// §2.4). github-bridge writes ci_state (the MR bead's mr_pipeline_status at
// merge), tests_touched and goldens_touched (counts over the PR's files, "0"
// when none, absent when the files read failed) on each producing task; the
// merge desk writes review_rounds. Strings, like mrFields, because the bridges
// write a string map. Guarded by TestBuiltinHarvestFieldsDeclared.
const harvestFields = `
		{"name": "ci_state",        "type": "string"},
		{"name": "tests_touched",   "type": "string"},
		{"name": "goldens_touched", "type": "string"},
		{"name": "review_rounds",   "type": "string"}`

// awaitsReleaseFields holds the awaits_release opt-in (releases_awaits.go,
// gd-ZM7NEBPAqZ): merged work stays open until a deployment of a release that
// carries it is recorded. Declared on every issue-kind type and on
// type:escalation. Guarded by TestAwaitsRelease_FieldDeclared.
const awaitsReleaseFields = `
		{"name": "awaits_release", "type": "boolean"}`

// placementFields pin a claimable issue to a local runner (local-agents spec
// §7, D4): placement=local plus runner_id (a runner bead id) or runner_tag (one
// of a runner's tags). The slack bridge stamps them on a /spawn --runner task;
// gb ready on a runner keeps only the tasks placed on it, and gb spawn refuses
// to pod-spawn one. opened_by_operator (oidc:<email>) is the operator who opened
// the Slack thread the task was filed in, stamped at create
// (maybeStampOpenedByOperator); a runner also lists an unplaced task its owner
// opened. Declared on every type gb ready lists as leaf work. Guarded by
// TestBuiltinPlacementFieldsDeclared.
const placementFields = `
		{"name": "placement",  "type": "enum", "values": ["local"]},
		{"name": "runner_tag", "type": "string"},
		{"name": "runner_id",  "type": "string"},
		{"name": "opened_by_operator", "type": "string"}`

// issueFields returns a JSON config snippet for an issue-kind type, embedding
// the shared MR fields, audit fields, workspace-recovery field, resume-chain
// field, reopen-accounting fields, trigger-source create_bead fields, harvest
// fields, and any extras the caller appends.
func issueFields(extras string) json.RawMessage {
	if extras == "" {
		return json.RawMessage(fmt.Sprintf(`{"kind":"issue","fields":[%s,%s,%s,%s,%s,%s,%s,%s]}`, mrFields, auditFields, workspaceField, resumeChainField, reopenFields, triggerSourceFields, harvestFields, awaitsReleaseFields))
	}
	return json.RawMessage(fmt.Sprintf(`{"kind":"issue","fields":[%s,%s,%s,%s,%s,%s,%s,%s,%s]}`, mrFields, auditFields, workspaceField, resumeChainField, reopenFields, triggerSourceFields, harvestFields, awaitsReleaseFields, extras))
}

// builtinConfigs provides default config values that are returned when no
// user-defined config exists for a key.  The namespace index groups them by
// prefix so ListConfigs can merge them in.
var builtinConfigs = map[string]*model.Config{
	"view:ready": {
		Key:   "view:ready",
		Value: json.RawMessage(`{"filter":{"status":["open","in_progress"],"kind":["issue"]},"sort":"priority","limit":5}`),
	},
	"view:users": {
		Key:   "view:users",
		Value: json.RawMessage(`{"filter":{"status":["open","in_progress"],"type":["user"],"kind":["config"]},"columns":["id","title","slack_id","email","gitlab_username","jira_user","team"],"limit":100}`),
	},
	"view:audit:recent": {
		Key:   "view:audit:recent",
		Value: json.RawMessage(`{"filter":{"status":["open"],"type":["audit"],"kind":["data"]},"sort":"-created_at","columns":["id","title","actor","action","resource","outcome"],"limit":50}`),
	},
	// phase_order is the plan-viewer's explicit, reorderable ordering of a
	// plan epic's phase sub-epics: a JSON array of the child-epic ids in
	// display order (ka-0000005926). It doubles as the phase-MEMBERSHIP set —
	// a direct-child epic renders as an inline phase iff its id appears here,
	// otherwise it stays a freestanding linked sub-plan (operator decision
	// ka-0000001034). Declared on type:epic so the first write is accepted
	// (epics don't allowAdditionalFields, so an undeclared key 400s).
	//
	// plan_state is the from-scratch authoring lifecycle a plan epic moves
	// through — draft → ready → building → {complete | abandoned} — that gates
	// the gardener build (task ka-0000005350, D5 ka-0000003301, risk R6) and
	// records terminal completion (task ka-0000000616). The write-through cockpit
	// persists every bead immediately; this single field is the ONLY authoring
	// gate. `draft` = the assistant is still researching/clarifying; `ready` =
	// finalized and awaiting the operator's deliberate "Approve & build";
	// `building` = approved, the bound gardener loop is enabled and dispatching
	// crew; `complete` = the terminal state stamped when the plan is marked
	// complete (the plan epic is closed and its bound gardener disabled — the
	// viewer's POST /plans/{id}/complete verb, mirrored by `gb plan complete`);
	// `abandoned` = the DISTINCT terminal state stamped when a plan is dropped
	// WITHOUT finishing (the "cheap exit" — POST /plans/{id}/abandon, `gb plan
	// abandon` — task ka-0000001377). Both terminal states close the epic + pause
	// the gardener and reopen to building; they differ only in meaning. An UNSET
	// plan_state means the plan never opted into the lifecycle
	// (every plan minted before this field, plus any epic marked a plan by hand)
	// and is left UNGATED — the gardener toggle behaves exactly as it did before.
	// Declared here (not allowAdditionalFields) so the viewer's first write is
	// accepted rather than 400'd.
	//
	// step_order is the SYMMETRIC ordering overlay for a plan/phase epic's
	// issue-type step children: a JSON array of the child step ids in display
	// order (in-cockpit reorder, ka-0000007445). It is ADVISORY, not a
	// membership set — a step whose id is absent from step_order still renders,
	// falling in creation order after every listed step. Absent field ⇒ pure
	// creation-order (the pre-reorder behavior). Stored on whichever epic is the
	// step's direct parent (the plan epic for flat steps, the phase sub-epic for
	// phased steps), so both the detail timeline and the /plans index chip
	// consult it identically and #N stays consistent (ka-0000007589).
	//
	// last_nudge_at is the RFC3339 timestamp of the last reactive add-step
	// nudge for a plan — the DURABLE backing for the per-plan debounce that
	// coalesces a burst of step-adds into one assistant nudge/spawn
	// (ka-0000002855). It replaced a volatile in-memory viewer map that was
	// lost on restart and shared by no other writer, so a viewer restart (or a
	// concurrent CLI/browser/controller-reactor add) could double-fire the
	// nudge. Persisting it on the plan epic makes the window survive restarts
	// and coordinate every writer. Declared here (epics don't
	// allowAdditionalFields) so the debounce write is accepted rather than 400'd.
	//
	// icon_svg_url is the object-store URL of the per-epic badge SVG rendered by
	// the Phase 2 generator/cache (parent ka-0000002812, thread ka-0000004695):
	// each gardenio epic gets one reproducible badge, and the generator writes its
	// URL back onto the epic bead so the viewer can render it. Declared here
	// (epics don't allowAdditionalFields) so that write-back is accepted rather
	// than 400'd "icon_svg_url: unknown field".
	//
	// plan_spend_usd / plan_spend_synced_at / plan_spend_crew / plan_spend_live
	// are the controller's per-plan crew spend rollup (gd-kcy4IFNzv5,
	// internal/planspend): the summed crew dollars as a 4-decimal string (the
	// gardener_spend_usd shape), the RFC3339 time of the write, and how many
	// crew (and live crew) the figure sums. gardenhub reads them into
	// FarmPlan.Cost. Declared here so the controller's write is not 400'd.
	"type:epic": {Key: "type:epic", Value: issueFields(`
			{"name": "phase_order", "type": "string[]"},
			{"name": "plan_state",  "type": "enum", "values": ["draft", "ready", "building", "complete", "abandoned"]},
			{"name": "step_order", "type": "string[]"},
			{"name": "last_nudge_at", "type": "string"},
			{"name": "icon_svg_url", "type": "string"},
			{"name": "plan_spend_usd", "type": "string"},
			{"name": "plan_spend_synced_at", "type": "string"},
			{"name": "plan_spend_crew", "type": "integer"},
			{"name": "plan_spend_live", "type": "integer"}`)},
	// type:task carries the formula step-machinery fields (waits_for,
	// on_complete, expansion_var, dwf_result, gate_type, timeout) below
	// alongside the schedule/jira/gitlab/slack producer fields. The pour paths
	// (http_formula_pour.go and cmd/kd/formula_apply.go) stamp
	// waits_for/on_complete onto step task beads; cmd/kd/formula_expand.go
	// stamps expansion_var on each expanded step; and the documented
	// output-threading convention has a host step write dwf_result so
	// `kd formula expand --from-field dwf_result.X` and {{step.output.X}} can
	// thread structured output onto downstream molecules.
	// Their absence 400'd every pour of an on_complete/waits_for formula
	// mid-molecule and every deferred expand, breaking the on_complete feature
	// end-to-end on the live daemon (ka-0000002559). Guarded by
	// TestBuiltinFormulaStepFieldsDeclared.
	//
	// gate_type/timeout are the SAME class, recurred (ka-0000006567): BOTH pour
	// paths stamp gate_type (and timeout when set) onto EVERY gate step, but
	// only a `decision` gate flips typ to "decision" — a `manual`/`sync` gate
	// step keeps the default type=task. type:decision already declared both
	// (below), so decision gates poured fine, but a manual/sync gate step
	// 400'd "gate_type: unknown field" on type:task and aborted the pour
	// mid-molecule, leaving a half-built molecule whose one dropped step was
	// precisely the gate — the molecule silently lost its enforcement point
	// (the PE-ticket-to-completion `gardener-signoff` manual gate). Declaring
	// them here (matching type:decision) lets a non-decision gate step pour.
	//
	// suggest_new_agent/step_role/molecule_id/step_project/dispatched_agent/
	// dispatched_by are the same class, recurred: they are the step-role
	// dispatch machinery's fields, stamped by three writers that were never
	// declared here, so EVERY pour 400'd — which is why no fix-and-mr /
	// fix-proof / publish-fix molecule could be poured at all, breaking the FSM
	// §7→§8 pipeline (ka-0000002741, ka-0000001148). The writers:
	//   - cmd/kd/formula_apply.go stamps suggest_new_agent (a real bool) +
	//     step_role on any CLI-poured step opting into the dispatcher.
	//   - gardenio controller bridge/formula_instantiate.go stamps molecule_id
	//     UNCONDITIONALLY on every bridge-poured step (Slack /formula pour and
	//     the apply_formula trigger), plus suggest_new_agent/step_role/
	//     step_project — so a bridge pour 400'd on step 1 on molecule_id alone,
	//     leaving a childless molecule, even with suggest_new_agent registered.
	//   - bridge/formula_step_dispatch.go stamps dispatched_agent/dispatched_by
	//     BEFORE spawning; that write 400'd unconditionally, so the dispatcher
	//     declined-and-returned forever and could never spawn.
	// Declared on type:decision too: a decision-gate step flips typ to
	// "decision" BEFORE formula_apply.go's writer runs (formula_apply.go:429 vs
	// :450), so a gated opted-in step lands there.
	// mention_instruction is the raw @gardenio instruction text lifted from a
	// GitLab note. The gitlab-bridge stamps it UNCONDITIONALLY on every mention
	// task bead (bridge/gitlab_mentions.go spawnMentionAgent), so while it went
	// undeclared the create 400'd and NO agent was ever spawned for a GitLab
	// @mention — the webhook logged "mention detected" and then silently
	// dropped it, so @gardenio on an MR simply never answered (ka-0000003933).
	// Added by the catchup watchdog (ka-0000000279), which reads it back to
	// rebuild the respawn prompt; the prompt is not otherwise recoverable from
	// the task bead alone. TestBuiltinGitLabMentionFieldsDeclared guards the
	// whole spawnMentionAgent field set against a repeat.
	// herder_verdict / herder_checked_at are the stamps the scheduled herder
	// (role herder, trigger gd-AHklx9rH70) writes on each wish head it judges,
	// next to its "[herder] verdict:" comment. While undeclared, every stamp
	// 400'd "unknown field" (gd-K1CMS3JmCi). close_reason is declared so the
	// herder can stamp close_reason=herder-fulfilled with kd update before it
	// closes a fulfilled wish (kd close sends no fields, gd-EfbganTatT); a close
	// body's fields were never schema-checked, so the bridge's own closes are
	// unchanged. herder_woke_at is when the herder last woke a wish's thread
	// agent (gb say --wake) or asked its asker a question: it does either at
	// most once per wish per 24 hours (gd-y8LrXMOa46). herder_decision is the
	// decision the herder filed to ask the land's owner about a stale wish; it
	// files no second one while that one is open, and the bridge acts on the
	// answer only when the head names the decision (gd-g6lGiK8KXv).
	// TestBuiltinHerderFieldsDeclared.
	// pg_budget_key is the statement or table a Postgres budget task is about
	// (controller internal/pgbudget filer, gd-ylgk1jBhLi): the filer dedups on
	// it, so a breach two days running files one task. Guarded by
	// TestBuiltinPgBudgetWritesValidate.
	"type:task": {Key: "type:task", Value: issueFields(`
			{"name": "schedule_id",            "type": "string"},
			{"name": "schedule_key",           "type": "string"},
			{"name": "schedule_fire_key",      "type": "string"},
			{"name": "schedule_fire_time",     "type": "string"},
			{"name": "schedule_cron",          "type": "string"},
			{"name": "mention_author",         "type": "string"},
			{"name": "mention_file",           "type": "string"},
			{"name": "mention_line",           "type": "string"},
			{"name": "mention_instruction",    "type": "string"},
			{"name": "spawn_source",           "type": "string"},
			{"name": "jira_key",              "type": "string"},
			{"name": "jira_status",           "type": "string"},
			{"name": "jira_priority",         "type": "string"},
			{"name": "jira_url",              "type": "string"},
			{"name": "jira_has_comments",     "type": "string"},
			{"name": "jira_comment_id",       "type": "string"},
			{"name": "jira_link_count",       "type": "string"},
			{"name": "jira_type",             "type": "string"},
			{"name": "jira_assignee",         "type": "string"},
			{"name": "jira_assignee_id",      "type": "string"},
			{"name": "jira_reporter",         "type": "string"},
			{"name": "jira_reporter_id",      "type": "string"},
			{"name": "jira_reporter_email",   "type": "string"},
			{"name": "recommended_affects_version", "type": "string"},
			{"name": "gitlab_key",            "type": "string"},
			{"name": "gitlab_pipeline_id",    "type": "string"},
			{"name": "gitlab_pipeline_status","type": "string"},
			{"name": "gitlab_pipeline_url",   "type": "string"},
			{"name": "gitlab_project",        "type": "string"},
			{"name": "source_formula",        "type": "string"},
			{"name": "source_step_id",        "type": "string"},
			{"name": "source_schedule_id",    "type": "string"},
			{"name": "suggest_new_agent",     "type": "boolean"},
			{"name": "step_role",             "type": "string"},
			{"name": "molecule_id",           "type": "string"},
			{"name": "step_project",          "type": "string"},
			{"name": "dispatched_agent",      "type": "string"},
			{"name": "dispatched_by",         "type": "string"},
			{"name": "waits_for",             "type": "string"},
			{"name": "on_complete",           "type": "json"},
			{"name": "expansion_var",         "type": "string"},
			{"name": "dwf_result",            "type": "json"},
			{"name": "gate_type",             "type": "string"},
			{"name": "timeout",               "type": "string"},
			{"name": "mirror_fire_key",       "type": "string"},
			{"name": "mirror_fire_time",      "type": "string"},
			{"name": "mirror_entry_idx",      "type": "string"},
			{"name": "mirror_entry_kind",     "type": "string"},
			{"name": "mirror_entry_label",    "type": "string"},
			{"name": "mirror_cause",          "type": "string"},
			{"name": "multi_fire_key",        "type": "string"},
			{"name": "multi_fire_time",       "type": "string"},
			{"name": "multi_entry_idx",       "type": "string"},
			{"name": "multi_entry_kind",      "type": "string"},
			{"name": "multi_entry_label",     "type": "string"},
			{"name": "multi_cause",           "type": "string"},
			{"name": "slack_thread_channel",  "type": "string"},
			{"name": "slack_thread_ts",       "type": "string"},
			{"name": "slack_thread_bead_id",  "type": "string"},
			{"name": "thread_id",             "type": "string"},
			{"name": "slack_spawn_channel",   "type": "string"},
			{"name": "slack_opener_user_id",  "type": "string"},
			{"name": "slack_root_text",       "type": "string"},
			{"name": "slack_root_at",         "type": "string"},
			{"name": "slack_permalink",       "type": "string"},
			{"name": "channel_id",            "type": "string"},
			{"name": "promoted_from",         "type": "string"},
			{"name": "herder_verdict",        "type": "string"},
			{"name": "herder_checked_at",     "type": "string"},
			{"name": "close_reason",          "type": "string"},
			{"name": "herder_woke_at",        "type": "string"},
			{"name": "herder_decision",       "type": "string"},
			{"name": "support_message_id",    "type": "string"},
			{"name": "support_channel_id",    "type": "string"},
			{"name": "support_notified_at",   "type": "string"},
			{"name": "support_notified_via",  "type": "enum", "values": ["dm", "channel"]},
			{"name": "pg_budget_key",         "type": "string"},` + reproFields + `,` + placementFields)},
	"type:feature": {Key: "type:feature", Value: issueFields(placementFields)},
	"type:chore":   {Key: "type:chore", Value: issueFields(placementFields)},
	"type:bug":     {Key: "type:bug", Value: issueFields(placementFields)},

	// type:merge-request — first-class MR bead tracking a GitLab merge
	// request. Upserted by the gitlab-bridge on MR webhook and poll
	// events. Status mirrors GitLab state: open while opened/draft,
	// closed when merged or closed-without-merge (mr_outcome records
	// which). Producing task beads link via a `produces` dependency.
	// Promotes MRs from task-bead fields to first-class beads (epic
	// ka-0000001613).
	//
	// merged_into, dedup_reason, dedup_former_mr_url are the marker fields the
	// HALF-2 dedup reconciler (`kd admin migrate mr-bead-dedup`,
	// ka-0000005686) stamps on each LOSER bead, so a duplicate merge-request
	// bead records which survivor absorbed it, why, and the mr_url it used to
	// carry (they survive on a closed bead — they are not transient-closed
	// keys). The reconciler ALSO clears the loser's own mr_url so it leaves
	// the (status-agnostic) UNIQUE-index population migration 042 builds.
	// Without these registered the reconciler's UpdateBead is rejected
	// `unknown field` and the pre-migration-042 sweep cannot complete
	// (ka-0000000451).
	//
	// merge_sha and merged_at are stamped by github-bridge when the PR merges
	// (the merge commit GitHub reports and when), so a release's contents join
	// a first-parent commit to its MR bead on sha, not only on the PR number
	// or the branch name in the merge subject (releases-design.md §3.4, plan
	// gd-UZKZ9H25UD step 3).
	"type:merge-request": {Key: "type:merge-request", Value: issueFields(`
			{"name": "project_path",        "type": "string"},
			{"name": "merge_sha",           "type": "string"},
			{"name": "merged_at",           "type": "string"},
			{"name": "source_branch",       "type": "string"},
			{"name": "target_branch",       "type": "string"},
			{"name": "mr_outcome",          "type": "enum", "values": ["merged", "closed"]},
			{"name": "author",              "type": "string"},
			{"name": "merged_into",         "type": "string"},
			{"name": "dedup_reason",        "type": "string"},
			{"name": "dedup_former_mr_url", "type": "string"}`)},
	"type:jira-poller": {Key: "type:jira-poller", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "projects",        "type": "string"},
			{"name": "statuses",        "type": "string"},
			{"name": "issue_types",     "type": "string"},
			{"name": "fix_versions",    "type": "string"},
			{"name": "required_labels", "type": "string"},
			{"name": "excluded_labels", "type": "string"},
			{"name": "filter_projects", "type": "string"},
			{"name": "custom_jql",      "type": "string"},
			{"name": "skip_assigned",   "type": "string"},
			{"name": "poll_interval",   "type": "string"}
		]
	}`)},
	"type:advice": {Key: "type:advice", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin", "escalatable": true},
		"fields": [
			{"name": "hook_command",   "type": "string"},
			{"name": "hook_trigger",   "type": "enum", "values": ["session-end", "before-commit", "before-push", "before-handoff"]},
			{"name": "hook_timeout",   "type": "integer"},
			{"name": "hook_on_failure","type": "enum", "values": ["block", "warn", "ignore"]},
			{"name": "subscriptions",  "type": "string[]"},
			{"name": "subscriptions_exclude", "type": "string[]"},
			{"name": "confidence",     "type": "float"},
			{"name": "confirmations",  "type": "integer"},
			{"name": "last_confirmed", "type": "string"},
			{"name": "source",         "type": "string"},
			{"name": "author_agent",   "type": "string"},
			{"name": "content_hash",     "type": "string"},
			{"name": "memory_category",  "type": "string"},
			{"name": "tags",             "type": "string"},
			{"name": "supersedes",       "type": "string"},
			{"name": "expires_at",       "type": "timestamp"}
		]
	}`)},
	"type:jack": {Key: "type:jack", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "jack_target",          "type": "string",  "required": true},
			{"name": "jack_reason",          "type": "string",  "required": true},
			{"name": "jack_revert_plan",     "type": "string",  "required": true},
			{"name": "jack_ttl",             "type": "string"},
			{"name": "jack_expires_at",      "type": "string"},
			{"name": "jack_original_ttl",    "type": "string"},
			{"name": "jack_extension_count", "type": "integer"},
			{"name": "jack_cumulative_ttl",  "type": "string"},
			{"name": "jack_reverted",        "type": "boolean"},
			{"name": "jack_closed_reason",   "type": "string"},
			{"name": "jack_closed_at",       "type": "string"},
			{"name": "jack_escalated",       "type": "boolean"},
			{"name": "jack_escalated_at",    "type": "string"},
			{"name": "jack_changes",         "type": "json"},
			{"name": "jack_rig",             "type": "string"}
		]
	}`)},
	// delivered_at: the recipient pod's mail subscriber (gardenio
	// controller/internal/mailsub stampDelivered) stamps it, RFC 3339, once coop
	// takes the mail. Mail stays open until `gb mail read`, so the bridge's
	// watchMailDelivery tells "a live pod has it" from "no pod received it" by
	// this stamp alone. Undeclared, every stamp 400'd "delivered_at: unknown
	// field" and the bridge respawned healthy thread agents two minutes after
	// every @mention (PR #1424 send-back). Mirrored in bridge/init.go.
	// request_id, behavior: a control:permission mail's Slack verdict
	// (mailproto.FieldPermissionRequestID/FieldPermissionBehavior), which the
	// recipient's subscriber POSTs to the loopback gardenio-channel /permission.
	// Undeclared, the bridge's MailPermissionVerdict create 400s "unknown
	// field" and every Slack Allow/Deny fails (PR #1705 send-back). Mirrored in
	// bridge/init_mail.go.
	"type:mail": {Key: "type:mail", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "source_agent",      "type": "string"},
			{"name": "decision_bead_id",  "type": "string"},
			{"name": "delivered_at",      "type": "string"},
			{"name": "request_id",        "type": "string"},
			{"name": "behavior",          "type": "string"},
			{"name": "escalation_id",     "type": "string"}
		]
	}`)},
	// idempotency_key: createBead folds CreateBeadRequest.IdempotencyKey into
	// Fields[idempotency_key] BEFORE field validation, so any mint carrying an
	// IdempotencyKey 400s "idempotency_key: unknown field" unless the type
	// declares it. The slack-bridge thread-agent resume path (respawnThreadAgent,
	// gardenio controller/internal/bridge/bot_agent_kill.go) started carrying one
	// in gardenio!5958, so every Slack wake-up of a done thread agent 400'd —
	// the same failure type:message already hit and fixed. Declared on the
	// builtin (not just the store row) so the builtin-merge in resolveTypeConfig
	// repairs already-persisted type:agent configs on upgrade with no manual
	// SetConfig roll-forward.
	//
	// model: the per-agent model override the controller stamps on a `--model`
	// spawn/resume (gardenio bridge bot_mentions.go / bot_agent_kill.go set
	// fields["model"]) and the two agent-card render seams READ back to size the
	// context-window occupancy percentage per-model — contextwindow.OccupancyPct
	// takes fields["model"] in both cmd/beads-viewer/agent_usage.go and
	// internal/bridge/bot_agents_usage.go (ka-0000005244). The schema never
	// declared it, so every write that set it 400'd "model: unknown field" and
	// the field never persisted: OccupancyPct always received "" and fell back
	// to the fleet-default 200k window, so a long-context (1M) agent's bar
	// silently mis-rendered how close it was to compaction. Declaring it here
	// (builtin, so resolveTypeConfig's builtin-merge repairs persisted configs on
	// upgrade) lets the override round-trip and the denominator become genuinely
	// per-model (ka-0000001124).
	//
	// respawn_count / max_respawns: the controller-authoritative respawn-budget
	// wake fields (AgentBeadFieldRespawnCount / AgentBeadFieldMaxRespawns in gardenio
	// cmd/gb/wrapup.go; mirrored as beadsapi.FieldRespawnCount / FieldMaxRespawns).
	// successor + next_respawn_at (the sibling succession/wake fields) are already
	// declared above via ka-0000006664; these two complete the respawn field family.
	// The live wake-scanner's revive PATCH writes respawn_count in the SAME atomic
	// UpdateBead as the status reopen + next_respawn_at clear
	// (gardenio/controller/internal/statusreporter/wake_scanner.go), so an
	// unregistered respawn_count 400s the WHOLE revive and a parked identity fails to
	// reopen on every scan pass (ka-0000004509). Both are string-coded like the
	// existing flat agent fields. Declared on the builtin (not just a store row) so
	// resolveTypeConfig's builtin-merge repairs already-persisted type:agent configs
	// on upgrade with no manual SetConfig roll-forward (prod has a stored type:agent
	// row that would otherwise keep shadowing this — ka-0000000061).
	//
	// prior_wrapup / error_category / error_detail: three fields that were only
	// ever registered in the gardenio BRIDGE's config-BEAD path
	// (controller/internal/bridge/init.go, fed to EnsureConfigs), which
	// resolveTypeConfig NEVER reads — it consults builtinConfigs and the configs
	// KV table only. That is the same gap that killed grafana-bridge's
	// type=dashboard creates (ka-0000001549); it went unnoticed for the agent type
	// because almost every OTHER bridge-registered field happens to also be a
	// builtin here. All three 400'd "unknown field" on prod until ka-0000000811.
	//   - prior_wrapup: archived by the revive-in-place PATCH before it nil-deletes
	//     `wrapup` (bridge/bot_agent_kill.go) so a resumed agent's card falls back
	//     to the previous turn's tl;dr instead of rendering blank (ka-0000003692).
	//     That PATCH is ONE atomic UpdateBead carrying the closed->in_progress
	//     flip, so the 400 failed the WHOLE revive and dropped the respawn through
	//     to a cold mint — new bead id, new COOP_S3_SESSION_KEY prefix, empty S3,
	//     conversation lost. Exactly what revive-in-place exists to prevent
	//     (ka-0000006807).
	//   - error_category / error_detail: the terminal-auth-failure diagnostics
	//     persisted by the NATS listener (controller/cmd/controller/
	//     terminal_auth_failure.go) so a first-turn Unauthorized / OutOfCredits has
	//     a durable home instead of living only in a live /api/v1/agent poll
	//     (ka-0000003962 — the 2026-07-25 opus-5 outage ran ~4.5h with
	//     total_errors=0 on every affected bead). Every write silently 400'd.
	//
	// human_survey_prompt_ts: same bridge-only registration, but latent rather
	// than live-broken — prod's persisted type:agent KV row happens to carry it, so
	// the survey path works THERE while a fresh daemon (a new env, a restored
	// snapshot, a local dev stack) would 400 every survey-prompt write. Surfaced by
	// TestBridgeTypeConfig_SubsetOfBuiltin, which is exactly the class of drift
	// that guard exists to catch before it becomes an incident.
	//
	// owner / resolved_by: the live-agent-card footer navigability pair
	// (ka-0000003131). gardenio!… (merged 2026-08-21, commit f8dab085f) rolled the
	// spawner FORWARD to stamp both at spawn — triggers.go stamps `owner` (the human
	// the agent was spawned on behalf of) and stampGardenerLoopContext stamps
	// `resolved_by` (the resolved gardener's display name) — but registered them ONLY
	// in the gardenio bridge init.go EnsureConfigs path, which resolveTypeConfig NEVER
	// reads. Same gap as prior_wrapup/error_* above (ka-0000000811) and grafana's
	// type=dashboard (ka-0000001549). type:agent has no allow_additional_fields, so
	// from ~2026-08-21T14:17Z every trigger/gardener spawn 400'd "owner: unknown
	// field" (or "resolved_by") and the WHOLE SpawnAgent create failed — silently
	// dark-firing all trigger-spawned agents (fsm-triage, wedge-watch, gardener-bugfix,
	// merge-queue-sweep, delivery-freshness, daily-syndicate, editor-in-chief, …),
	// so e.g. FSM-528 was never picked up (ka-0000000428). Declared on the builtin so
	// resolveTypeConfig's builtin-merge repairs the already-persisted prod type:agent
	// row on upgrade with no manual SetConfig roll-forward (ka-0000000061).
	//
	// restart_requested_at / restart_requested_by / restart_answered / restart_result:
	// the garden's restart-in-place request for a stale gardener whose pod is still up
	// (gd-LUmZlr6CKM). beads-viewer writes the request on the agent bead (it can reach
	// neither coop nor the controller), the controller answers on the same bead after
	// asking coop to restart the session in place (gardenio beadsapi/restart_fields.go).
	//
	// ci_wait_state / ci_wait_key / ci_wait_workflows / ci_wait_task /
	// ci_wait_deadline / ci_wait_result: a CI wait parked on the agent's OWN bead
	// (gardenio plan wake-on-ci-completion.md §2.1, task gd-AwuRDezONo). `gb wait ci`
	// writes them, `gb stop` reads them to park the claimed task, and the bridge
	// resolver flips ci_wait_state and writes ci_wait_result when the run concludes.
	// ci_wait_ledger is the resolver's own JSON run ledger for the sha (gardenio
	// internal/ciwait, task gd-WKrO492yYP), mirrored so a bridge restart keeps it.
	// Plain strings: the state is cleared/overwritten by later waits, and keeping it
	// off the enum list means a resolver write never 400s on a new state.
	//
	// allow_additional_fields: true — G3 decision ka-0000007573 (HYBRID / option c),
	// epic ka-0000002054. type:agent is the churn hotspot: 4 same-class outages in 3
	// months, each a controller/bridge write of a field that was never declared on the
	// builtin, silently 400'ing the WHOLE create/revive (see the narrative above). An
	// open schema turns an unknown key into a bag-of-attributes write instead of a
	// hard 400, so a future undeclared field degrades gracefully rather than
	// dark-firing agents. DECLARED fields still get FULL type validation (enums like
	// agent_state/pod_phase, typed cost/token fields) — only truly unknown keys are
	// accepted loose (validate_fields_test.go). This is armed AFTER G1's CI drift-guard
	// (bridge/init.go in the test-kbeads CI filter, skip->fatal; MR !7729, ka-0000001411)
	// so builtin/bridge drift still fails loudly in CI. Residual "typo lands silently"
	// risk is owned by the G4 runtime alarm (ka-0000007249). type:project and type:role
	// stay STRICT by decision — their 400s are desirable and had zero of the 4 outages.
	"type:agent": {Key: "type:agent", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"project_scoped": true, "escalatable": true},
		"allow_additional_fields": true,
		"fields": [
			{"name": "agent",         "type": "string", "required": true},
			{"name": "role",          "type": "string", "required": true},
			{"name": "project",       "type": "string", "required": true},
			{"name": "mode",          "type": "string"},
			{"name": "model",         "type": "string"},
			{"name": "harness",       "type": "enum", "values": ["claude", "codex", "gemini"]},
			{"name": "resolved_harness", "type": "enum", "values": ["claude", "codex", "gemini"]},
			{"name": "agent_state",   "type": "enum", "values": ["idle", "spawning", "assigning", "working", "stopping", "done", "failed", "rate_limited", "budget_exceeded", "prewarmed", "resuming", "resume_failed", "queued", "model_rate_limited", "stop_requested", "killed", "paused"]},
			{"name": "pod_phase",     "type": "enum", "values": ["pending", "running", "succeeded", "failed"]},
			{"name": "pod_name",      "type": "string"},
			{"name": "pod_namespace", "type": "string"},
			{"name": "pod_ready",     "type": "boolean"},
			{"name": "coop_url",      "type": "string"},
			{"name": "coop_token",    "type": "string"},
			{"name": "image",         "type": "string"},
			{"name": "source_image",  "type": "string"},
			{"name": "runtime_class", "type": "string"},
			{"name": "prompt",        "type": "string"},
			{"name": "task_id",       "type": "string"},
			{"name": "plan_id",       "type": "string"},
			{"name": "mock_scenario", "type": "string"},
			{"name": "stop_requested",    "type": "string"},
			{"name": "close_reason",      "type": "string"},
			{"name": "gate_satisfied_by", "type": "string"},
			{"name": "yield_count",       "type": "integer"},
			{"name": "auto_assign",       "type": "boolean"},
			{"name": "wrapup",            "type": "string"},
			{"name": "stranded_ask_detected",    "type": "string"},
			{"name": "stranded_ask_kind",        "type": "string"},
			{"name": "stranded_ask_disposition", "type": "string"},
			{"name": "stranded_ask_filed",       "type": "string"},
			{"name": "schedule_id",               "type": "string"},
			{"name": "schedule_title",            "type": "string"},
			{"name": "schedule_cron",             "type": "string"},
			{"name": "schedule_slack_channel",    "type": "string"},
			{"name": "schedule_name",             "type": "string"},
			{"name": "schedule_max_runs",         "type": "string"},
			{"name": "schedule_runs_spawned",     "type": "string"},
			{"name": "schedule_runs_completed",   "type": "string"},
			{"name": "schedule_terminal_goal",    "type": "string"},
			{"name": "schedule_prior_agent_id",   "type": "string"},
			{"name": "prior_agent_id",            "type": "string"},
			{"name": "lineage_root_id",           "type": "string"},
			{"name": "trigger_lease_id",          "type": "string"},
			{"name": "successor",                 "type": "string"},
			{"name": "next_respawn_at",           "type": "string"},
			{"name": "seat_key",                  "type": "string"},
			{"name": "gardener_id",                "type": "string"},
			{"name": "owner",                     "type": "string"},
			{"name": "resolved_by",               "type": "string"},
			{"name": "advice_subscriptions",         "type": "string[]"},
			{"name": "advice_subscriptions_exclude",  "type": "string[]"},
			{"name": "slack_spawn_channel",  "type": "string"},
			{"name": "slack_thread_channel", "type": "string"},
			{"name": "slack_thread_ts",      "type": "string"},
			{"name": "slack_thread_bead_id", "type": "string"},
			{"name": "thread_id",            "type": "string"},
			{"name": "slack_user_id",        "type": "string"},
			{"name": "slack_opener_user_id", "type": "string"},
			{"name": "slack_permalink",      "type": "string"},
			{"name": "slack_card_channel",   "type": "string"},
			{"name": "slack_card_ts",        "type": "string"},
			{"name": "user_bead_id",         "type": "string"},
			{"name": "operated_by",          "type": "string"},
			{"name": "gitlab_username",      "type": "string"},
			{"name": "jira_user_id",         "type": "string"},
			{"name": "spawn_source",            "type": "string"},
			{"name": "spawned_by",              "type": "string"},
			{"name": "spawn_approval_mode",     "type": "enum", "values": ["human", "auto"]},
			{"name": "spawn_approved_by",       "type": "string"},
			{"name": "escalation_id",           "type": "string"},
			{"name": "decision_bead_id",        "type": "string"},
			{"name": "gitlab_mr_url",           "type": "string"},
			{"name": "gitlab_mr_iid",           "type": "string"},
			{"name": "gitlab_project_id",       "type": "string"},
			{"name": "gitlab_mr_source_branch", "type": "string"},
			{"name": "mention_task",            "type": "string"},
			{"name": "gitlab_key",              "type": "string"},
			{"name": "gitlab_pipeline_id",      "type": "string"},
			{"name": "gitlab_pipeline_status",  "type": "string"},
			{"name": "gitlab_pipeline_url",     "type": "string"},
			{"name": "gitlab_project",          "type": "string"},
			{"name": "trigger_id",              "type": "string"},
			{"name": "trigger_title",           "type": "string"},
			{"name": "trigger_created_by",      "type": "string"},
			{"name": "source",                  "type": "string"},
			{"name": "jira_key",                "type": "string"},
			{"name": "jira_comment_id",         "type": "string"},
			{"name": "schedule_key",            "type": "string"},
			{"name": "schedule_mirror_key",     "type": "string"},
			{"name": "multi_key",               "type": "string"},
			{"name": "source_key",              "type": "string"},
			{"name": "molecule_id",             "type": "string"},
			{"name": "source_formula",          "type": "string"},
			{"name": "source_step_id",          "type": "string"},
			{"name": "fire_now",                "type": "string"},
			{"name": "fire_now_by",             "type": "string"},
			{"name": "total_input_tokens",       "type": "integer"},
			{"name": "total_output_tokens",      "type": "integer"},
			{"name": "total_thinking_tokens",    "type": "integer"},
			{"name": "total_cache_read_tokens",  "type": "integer"},
			{"name": "total_cache_write_tokens", "type": "integer"},
			{"name": "total_cost_usd",           "type": "float"},
			{"name": "total_request_count",      "type": "integer"},
			{"name": "total_tool_calls",         "type": "integer"},
			{"name": "total_errors",             "type": "integer"},
			{"name": "total_compactions",        "type": "integer"},
			{"name": "session_count",            "type": "integer"},
			{"name": "last_session_id",          "type": "string"},
			{"name": "session_resume",           "type": "string"},
			{"name": "s3_session_key",           "type": "string"},
			{"name": "coop_session_id",          "type": "string"},
			{"name": "litellm_spend_baseline",   "type": "float"},
			{"name": "litellm_spend_close",      "type": "float"},
			{"name": "litellm_spend_first_baseline", "type": "float"},
			{"name": "spend_logs_cost_usd",      "type": "float"},
			{"name": "context_tokens",           "type": "integer", "service_owned_by": ["*"]},
			{"name": "live_cost_usd",            "type": "float",   "service_owned_by": ["*"]},
			{"name": "live_turns",               "type": "integer", "service_owned_by": ["*"]},
			{"name": "execution_target",         "type": "string"},
			{"name": "runtime_id",               "type": "string"},
			{"name": "runtime_state",            "type": "string"},
			{"name": "runtime_account",          "type": "string"},
			{"name": "runtime_region",           "type": "string"},
			{"name": "runtime_started_at",       "type": "string"},
			{"name": "runtime_suspended_at",     "type": "string"},
			{"name": "microvm_git_token_id",     "type": "string"},
			{"name": "microvm_git_project",      "type": "string"},
			{"name": "idempotency_key",          "type": "string"},
			{"name": "interrupted_breadcrumb_posted", "type": "string"},
			{"name": "controller_restart_reason", "type": "string"},
			{"name": "last_progress_at", "type": "string"},
			{"name": "respawn_count",            "type": "string"},
			{"name": "max_respawns",             "type": "string"},
			{"name": "prior_wrapup",             "type": "string"},
			{"name": "error_category",           "type": "string"},
			{"name": "error_detail",             "type": "string"},
			{"name": "spawn_wall",               "type": "string"},
			{"name": "spawn_wall_reason",        "type": "string"},
			{"name": "purse_nudged_at",          "type": "string"},
			{"name": "park_reason",              "type": "string"},
			{"name": "human_survey_prompt_ts",   "type": "string"},
			{"name": "agent_handle",             "type": "string"},
			{"name": "restart_requested_at",     "type": "string"},
			{"name": "restart_requested_by",     "type": "string"},
			{"name": "restart_answered",         "type": "string"},
			{"name": "restart_result",           "type": "string"},
			{"name": "ci_wait_state",            "type": "string"},
			{"name": "ci_wait_key",              "type": "string"},
			{"name": "ci_wait_workflows",        "type": "string"},
			{"name": "ci_wait_task",             "type": "string"},
			{"name": "ci_wait_deadline",         "type": "string"},
			{"name": "ci_wait_result",           "type": "string"},
			{"name": "ci_wait_ledger",           "type": "string"}
		]
	}`)},
	// consent_push* and mr_url are what gb consent-to-push stamps; undeclared,
	// every consent-to-push decision 400'd "unknown field". standing_consent_user
	// is the user bead whose standing consent answered one (user_standing_consent.go).
	// evidence is the comma-separated bead ids `gb decision create --evidence`
	// stamps; surfaces show those beads' screenshots (decisionview.EvidenceBeads).
	"type:decision": {Key: "type:decision", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"project_scoped": true, "escalatable": true},
		"fields": [
			{"name": "prompt",                  "type": "string", "required": true},
			{"name": "options",                 "type": "json"},
			{"name": "context",                 "type": "string"},
			{"name": "session",                 "type": "string"},
			{"name": "requested_by",            "type": "string"},
			{"name": "requesting_agent_bead_id","type": "string"},
			{"name": "requesting_agent_verified","type": "string"},
			{"name": "requesting_task",         "type": "string"},
			{"name": "requesting_plan",         "type": "string"},
			{"name": "evidence",                "type": "string"},
			{"name": "operated_by",             "type": "string"},
			{"name": "gifted_by",               "type": "string"},
			{"name": "owner_seat",              "type": "string"},
			{"name": "owner_gardener",           "type": "string"},
			{"name": "chosen",                  "type": "string"},
			{"name": "rationale",               "type": "string"},
			{"name": "response_text",           "type": "string"},
			{"name": "responded_by",            "type": "string"},
			{"name": "responded_at",            "type": "string"},
			{"name": "resolver_subject",        "type": "string"},
			{"name": "resolver_admin",          "type": "boolean"},
			{"name": "resolver_client",         "type": "string"},
			{"name": "consent_push",            "type": "string"},
			{"name": "consent_push_mr",         "type": "string"},
			{"name": "consent_push_branch",     "type": "string"},
			{"name": "consent_push_target_branch", "type": "string"},
			{"name": "mr_url",                  "type": "string"},
			{"name": "standing_consent_user",   "type": "string"},
			{"name": "required_artifact",       "type": "string"},
			{"name": "artifact_status",         "type": "enum", "values": ["pending", "submitted", "accepted"]},
			{"name": "reflag_count",            "type": "string"},
			{"name": "reflag_reason",           "type": "string"},
			{"name": "reflagged_at",            "type": "string"},
			{"name": "reflagged_by",            "type": "string"},
			{"name": "gate_type",               "type": "string"},
			{"name": "timeout",                 "type": "string"},
			{"name": "source_formula",          "type": "string"},
			{"name": "source_step_id",          "type": "string"},
			{"name": "waits_for",               "type": "string"},
			{"name": "on_complete",             "type": "json"},
			{"name": "expansion_var",           "type": "string"},
			{"name": "dwf_result",              "type": "json"},
			{"name": "suggest_new_agent",       "type": "boolean"},
			{"name": "step_role",               "type": "string"},
			{"name": "molecule_id",             "type": "string"},
			{"name": "step_project",            "type": "string"},
			{"name": "dispatched_agent",        "type": "string"},
			{"name": "dispatched_by",           "type": "string"},
			{"name": "idempotency_key",         "type": "string"},
			{"name": "drain_candidate_ids",     "type": "json"},
			{"name": "drain_executed_at",       "type": "string"},
			{"name": "gardener_delivery_gate_nudged_at", "type": "string"},
			{"name": "sla_breached",            "type": "boolean"},
			{"name": "decision_pingback_requested_at", "type": "string"},
			{"name": "decision_pingback_count", "type": "string"},
			{"name": "decision_pingback_source","type": "string"},
			{"name": "sla_base_priority",       "type": "string"}
		]
	}`)},
	"type:rating": {Key: "type:rating", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"escalatable": true},
		"fields": [
			{"name": "rated_bead_id",   "type": "string",  "required": true},
			{"name": "score",           "type": "integer", "required": true, "min": 0, "max": 5},
			{"name": "axes",            "type": "json"},
			{"name": "tags",            "type": "string[]"},
			{"name": "rater",           "type": "string",  "required": true},
			{"name": "score_native",    "type": "integer"},
			{"name": "source",          "type": "string"},
			{"name": "slack_channel",   "type": "string"},
			{"name": "slack_thread_ts", "type": "string"},
			{"name": "slack_user_id",   "type": "string"},
			{"name": "feedback",        "type": "string"},
			{"name": "feedback_raw",    "type": "string"},
			{"name": "answered_at",     "type": "string"}
		]
	}`)},
	// type=evidence: one quality record about a step, parented to it and
	// closed at create (spec 2026-10-22-lands-harvest-icons-design.md §2.1).
	// kind says what was measured (only "ux" today; the field stays open).
	// step repeats the parent's id so the farm joins without an edge read.
	// No write_policy: any authenticated caller, crew pods included, files one.
	// Who graded and when are the bead's own created_by / created_at.
	// perf_* carry a ux grade's perf budget result beside the C1-C12 score
	// (gardenio/docs/commons-ux-rubric.md §4): perf_pass/perf_of are budgets
	// passed and graded, perf_fail/perf_ungraded comma-joined budget ids.
	"type:evidence": {Key: "type:evidence", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "kind",            "type": "string",  "required": true},
			{"name": "step",            "type": "string",  "required": true},
			{"name": "subject",         "type": "string"},
			{"name": "score",           "type": "integer", "min": 0},
			{"name": "of",              "type": "integer", "min": 0},
			{"name": "percent",         "type": "integer", "min": 0, "max": 100},
			{"name": "perf_pass",       "type": "integer", "min": 0},
			{"name": "perf_of",         "type": "integer", "min": 0},
			{"name": "perf_fail",       "type": "string"},
			{"name": "perf_ungraded",   "type": "string"},
			{"name": "urls",            "type": "string"},
			{"name": "pr",              "type": "string"},
			{"name": "operated_by",     "type": "string"},
			{"name": "idempotency_key", "type": "string"}
		]
	}`)},
	// type=release: one tag of one project (gardenio/docs/releases-design.md
	// §3.1, plan gd-UZKZ9H25UD step 2). Parented to its project bead, keyed by
	// (project, tag) through idempotency_key, closed at create: it is a ledger
	// record, not work, and status is a field. A release with no tag is the
	// project's open release (releases_open.go): kept open, keyed
	// release:<project>/next, stamped opened_at and opened_by, and closed by
	// its cut, which writes the tag. gardener_id names the release's
	// gardener trigger (controller internal/releasegardener). contents, images and
	// deployments are json lists (releases.go has their row shapes).
	// deployments and status are written only by the daemon: status is a pure
	// function of built_at and deployments, and a deployment row is appended
	// through POST /v1/releases/{id}/deployments, which closes the row of the
	// release that held the target. The write_policy keeps the generic bead
	// routes to admins; releaseWriterDenial also admits the release captain
	// and a machine principal granted the type (releases.go).
	"type:release": {Key: "type:release", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "project",           "type": "string",  "required": true},
			{"name": "tag",               "type": "string"},
			{"name": "sha",               "type": "string"},
			{"name": "previous",          "type": "string"},
			{"name": "previous_tag",      "type": "string"},
			{"name": "cut_at",            "type": "string"},
			{"name": "cut_by",            "type": "string"},
			{"name": "source",            "type": "enum", "values": ["webhook", "script", "backfill", "manual"]},
			{"name": "built_at",          "type": "string"},
			{"name": "images",            "type": "json"},
			{"name": "contents",          "type": "json"},
			{"name": "contents_complete", "type": "boolean"},
			{"name": "deployments",       "type": "json"},
			{"name": "status",            "type": "enum", "values": ["open", "cut", "built", "live", "superseded"]},
			{"name": "opened_at",         "type": "string"},
			{"name": "opened_by",         "type": "string"},
			{"name": "gardener_id",       "type": "string"},
			{"name": "idempotency_key",   "type": "string"}
		]
	}`)},
	"type:report": {Key: "type:report", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin", "escalatable": true},
		"fields": [
			{"name": "decision_id",      "type": "string", "required": true},
			{"name": "report_type",      "type": "string", "required": true},
			{"name": "content",          "type": "string", "required": true},
			{"name": "format",           "type": "string"},
			{"name": "escalation_id",    "type": "string"},
			{"name": "sensitivity",      "type": "enum", "values": ["public", "internal", "sensitive", "secret"]},
			{"name": "content_type",     "type": "enum", "values": ["text", "json", "script_output", "credential", "structured"]},
			{"name": "redacted_summary", "type": "string"},
			{"name": "access_log",       "type": "json"},
			{"name": "expires_at",       "type": "string"}
		]
	}`)},

	// Incident Intelligence — rich-report bead is an issue-kind container that
	// holds widget-instance children. The beads-viewer renders the bead as a
	// dense user-facing page composed of those widgets. See
	// docs/research/incident-intelligence/synthesis.md (epic ka-0000006762).
	//
	// The repro-harness gate fields (reproFields) are declared here too because
	// fsm-triage creates its triage beads as type=rich-report, and the repro
	// formula's Phase-4 step 3 mirrors gate_passed / repro_evidence /
	// repro_scorecard onto the triage bead. Without them the mirror PATCH 400'd
	// ("gate_passed: unknown field") on the exact bead type triage produces, so
	// the repro-cockpit catalog could not query gate_passed on triage beads at
	// all (ka-0000003501). The gate_passed→valid-URL cross-field rule
	// (validateReproFields) is enforced on rich-report too, so a mirrored pass
	// can't record gate_passed=true without citing the evidence URL — the same
	// "no unbacked pass" invariant the emit-step task gate protects. The rule is
	// inert unless gate_passed=true, so incident-intelligence rich-reports that
	// never set it are unaffected.
	"type:rich-report": {Key: "type:rich-report", Value: issueFields(`
			{"name": "report_kind",  "type": "string"},
			{"name": "headline",     "type": "string"},
			{"name": "severity",     "type": "string"},
			{"name": "source_ref",   "type": "string"},` + reproFields)},

	// widget-type defines a renderable widget shape registered in the
	// beads-viewer renderer registry. The slug field is the registry key.
	// Body schema is documented inline; v1 keeps this loose so new widget
	// types can be introduced from agent code without a migration.
	//
	// renderer_kind discriminates between built-in Go renderers ("builtin")
	// and free-htmx renderers ("htmx") that ship a Go html/template body in
	// template_source. When renderer_kind=htmx, template_source is required
	// and is compiled at write-time and at controller startup (fail-loud).
	// template_engine pins the template runtime; only go-html-template-v1
	// is currently supported. See gardenio/docs/research/free-htmx-widget-wire-shape.md.
	"type:widget-type": {Key: "type:widget-type", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "slug",            "type": "string", "required": true},
			{"name": "widget_version",  "type": "string"},
			{"name": "summary",         "type": "string"},
			{"name": "schema",          "type": "json"},
			{"name": "renderer_kind",   "type": "enum", "values": ["builtin", "htmx"]},
			{"name": "template_source", "type": "string"},
			{"name": "template_engine", "type": "enum", "values": ["go-html-template-v1"]},
			{"name": "render_endpoint", "type": "string"}
		]
	}`)},

	// widget-instance is a single widget rendered on a rich-report page.
	// widget_type names the registered widget renderer; data carries the
	// payload the renderer interprets. position controls display order
	// among siblings (lower first); ties break on created_at.
	"type:widget-instance": {Key: "type:widget-instance", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "widget_type",    "type": "string", "required": true},
			{"name": "widget_version", "type": "string"},
			{"name": "position",       "type": "integer"},
			{"name": "headline",       "type": "string"},
			{"name": "data",           "type": "json"},
			{"name": "widget_span",    "type": "string"}
		]
	}`)},

	// type:project is the schema for a gardenio project config bead. It is the
	// single source of truth consumed by the controller ProjectInfo parser
	// (gardenio/controller/internal/beadsapi/projects.go) — the daemon rejects
	// unknown keys, wrong types, and bad enums on create/update (design doc
	// project-first-class-hub-design.md §5.2/§5.3).
	//
	// Two field classes carry validation the daemon FieldDef cannot express,
	// so their shape is validated in a LAYERED fashion (editor pre-POST +
	// controller parse-time warning), not here:
	//
	//   - K8s quantity strings — cpu_request, cpu_limit, memory_request,
	//     memory_limit are plain "string" here; their resource.Quantity shape
	//     (e.g. "500m", "2Gi") is checked by the editor and flagged by the
	//     viewer projectValidityReport. The daemon only enforces "is a string".
	//
	//   - Structured-JSON fields — secrets, repos, env, env_json,
	//     prewarmed_pool, resource_mapping, claude_config, nudge_prompts, and
	//     the channel_* maps are "json" here; the daemon only enforces "valid
	//     JSON". Their concrete shape (SecretEntry{env,secret,key},
	//     RepoEntry{url,branch,role}, etc., defined by Go structs in
	//     projects.go) is validated in the editor pre-POST and flagged by the
	//     viewer projectValidityReport on read.
	//
	// Do NOT add "required" to fields legacy project beads omit — it would
	// wedge existing beads on their next update (§5.2).
	//
	// A project bead is also a land (Lands Commons spec
	// gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md §2.1):
	// land_default_channel is the type:channel bead id the land opens on
	// (normally #general), land_slack_mirror says whether new channels in the
	// land default to mirroring to Slack. Both are written by gb channel seed
	// and, like every project field, only by an admin (write_policy below).
	// land_kind (Lands Track B spec §3.2) is "operator" (unset: today's
	// projects) or "tenant". The rest of the §3.2 fields are admin- or
	// provisioner-written: land_owner_sub (the owner's Keycloak subject),
	// land_application_id (the plot_application it came from), land_opened_at
	// (when provisioning finished; the purse counts spend from here),
	// land_frozen (the kill switch; land_frozen_by, land_frozen_at and
	// land_frozen_reason say who flipped it, when and why, and a thaw clears
	// them: gb lands freeze|thaw, step 10.5), github_app (fleet or lands: which App
	// mints the project's tokens), github_installation_id (the pinned
	// installation), max_live_agents (per-land override of
	// COOP_MAX_PODS_PER_PROJECT), namespace (Phase 10) and land_closed_at
	// (stamped by gb lands close when the provisioner closes the land, §3.3).
	// land_purged_at and land_records_purged_at are stamped by the purge sweep
	// (step 8.11) when the land's data, and later its credit ledger, are gone.
	// A tenant land keeps land_slack_mirror off (B-D11); lands_ledger.go
	// refuses "on". steward_authority is the land's grant to its steward
	// (steward amendment 2026-10-23 §4): ask (unset: the default), merge-on-green
	// or operate; the steward reads anything else as ask.
	// land_gatehouse marks the one gatehouse land (step 9.12): the tenant-shaped
	// land every signed-in person with no land is a guest of; its steward plays
	// the front door. land_gatehouse_backfilled_at is when the provisioner's
	// one-time pass made every invited person with no land a guest there.
	// Both are provisioner-written. land_gatehouse_tools turns on the gatehouse
	// steward's apply and demo tools (gatehouse_request.go): unset is off, and
	// the operator sets it after the 10.14 security review (gd-Ahv1F4wPUO).
	// All three are admin-only writes, bar the provisioner's marks
	// (land_switch_authz.go landSwitchFields).
	// land_steward_approves is the operation types the land owner lets the
	// land's steward approve (escalation-routing D8). It is written only through
	// POST /v1/lands/{land}/steward-delegation; a create or PATCH that sets it
	// is a 400 (land_steward_delegation.go). The daemon stamps a new tenant
	// land with defaultLandStewardApproves (["github_pr_merge"]).
	//
	// budget_usd / budget_period are the per-project budget in US dollars
	// (decision gd-PtNuTyjQoN; display-only). Unset budget_usd = no budget.
	// The daemon only enforces "is a number"; "> 0, never 0" is checked by the
	// viewer editor pre-POST and flagged by projectValidityReport on read.
	// budget_period is "month" when unset.
	//
	// release_tag_pattern, release_targets and release_branch are the
	// project's release source (gardenio/docs/releases-design.md §4): a Go
	// regexp a tag must match to be a release (unset = the project has no
	// releases, and a release bead for it is refused), the named places it
	// runs as [{"name","kind"}], and the branch whose first-parent line the
	// contents are counted on (default: default_branch).
	"type:project": {Key: "type:project", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "prefix",          "type": "string"},
			{"name": "git_url",         "type": "string"},
			{"name": "default_branch",  "type": "string"},
			{"name": "image",           "type": "string"},
			{"name": "account",         "type": "string"},
			{"name": "model",           "type": "string"},
			{"name": "harness",         "type": "enum", "values": ["claude", "codex", "gemini"]},
			{"name": "runtime_class",   "type": "string"},
			{"name": "storage_class",   "type": "string"},
			{"name": "service_account", "type": "string"},
			{"name": "docker",          "type": "boolean"},
			{"name": "postgres",        "type": "boolean"},
			{"name": "triggers_enabled","type": "boolean"},
			{"name": "dm_default",      "type": "boolean"},
			{"name": "resource_mapping","type": "json"},
			{"name": "claude_config",   "type": "json"},
			{"name": "cpu_request",     "type": "string"},
			{"name": "cpu_limit",       "type": "string"},
			{"name": "memory_request",  "type": "string"},
			{"name": "memory_limit",    "type": "string"},
			{"name": "secrets",         "type": "json"},
			{"name": "env",             "type": "json"},
			{"name": "env_json",        "type": "json"},
			{"name": "repos",           "type": "json"},
			{"name": "slack_channel",   "type": "string"},
			{"name": "channel_roles",   "type": "json"},
			{"name": "channel_modes",   "type": "json"},
			{"name": "channel_runtime_class", "type": "json"},
			{"name": "auto_assign",     "type": "string"},
			{"name": "prewarmed_pool",  "type": "json"},
			{"name": "nudge_prompts",   "type": "json"},
			{"name": "default_timezone","type": "string"},
			{"name": "schedules_enabled","type": "boolean"},
			{"name": "needs_k8s_api",   "type": "boolean"},
			{"name": "ziti_access",     "type": "boolean"},
			{"name": "ziti_service_id", "type": "string"},
			{"name": "ziti_bind",       "type": "boolean"},
			{"name": "ziti_ttl",        "type": "string"},
			{"name": "ziti_optional",   "type": "boolean"},
			{"name": "session_resume",  "type": "enum", "values": ["true", "false", "auto"]},
			{"name": "ultracode",       "type": "string"},
			{"name": "execution_target","type": "string"},
			{"name": "intake_taints",   "type": "json"},
			{"name": "card_theme",      "type": "enum", "values": ["garden", "business"]},
			{"name": "land_default_channel", "type": "string"},
			{"name": "land_slack_mirror",    "type": "enum", "values": ["on", "off"]},
			{"name": "land_kind",            "type": "enum", "values": ["operator", "tenant"]},
			{"name": "land_owner_sub",       "type": "string"},
			{"name": "land_steward_approves", "type": "json"},
			{"name": "land_application_id",  "type": "string"},
			{"name": "land_opened_at",       "type": "timestamp"},
			{"name": "land_frozen",          "type": "boolean"},
			{"name": "land_frozen_by",       "type": "string"},
			{"name": "land_frozen_at",       "type": "string"},
			{"name": "land_frozen_reason",   "type": "string"},
			{"name": "github_app",           "type": "enum", "values": ["fleet", "lands"]},
			{"name": "github_installation_id", "type": "string"},
			{"name": "max_live_agents",      "type": "integer", "min": 0},
			{"name": "namespace",            "type": "string"},
			{"name": "nats_epoch",           "type": "string"},
			{"name": "land_closed_at",       "type": "timestamp"},
			{"name": "land_gatehouse",       "type": "boolean"},
			{"name": "land_gatehouse_backfilled_at", "type": "timestamp"},
			{"name": "land_gatehouse_tools", "type": "boolean"},
			{"name": "land_purged_at",       "type": "timestamp"},
			{"name": "land_records_purged_at", "type": "timestamp"},
			{"name": "steward_authority",    "type": "enum", "values": ["ask", "merge-on-green", "operate"]},
			{"name": "budget_usd",           "type": "float"},
			{"name": "budget_period",        "type": "enum", "values": ["month", "7d"]},
			{"name": "release_tag_pattern",  "type": "string"},
			{"name": "release_targets",      "type": "json"},
			{"name": "release_branch",       "type": "string"}
		]
	}`)},

	"type:fleet": {Key: "type:fleet", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "namespace",        "type": "string", "required": true},
			{"name": "description",      "type": "string"},
			{"name": "token_secret",     "type": "string", "required": true},
			{"name": "created_by_fleet", "type": "string"},
			{"name": "max_agents",       "type": "string"},
			{"name": "parent_fleet",     "type": "string"}
		]
	}`)},

	// consent_push_to_mr_auto is a person's standing consent to have CI fixes
	// pushed to their MRs; it shares the identity fields' write gate
	// (userStandingConsentFields, user_standing_consent.go). So does
	// welcome_tour_seen_at, when the person had the /garden welcome tour
	// (http_welcome_tour.go).
	"type:user": {Key: "type:user", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "slack_id",          "type": "string"},
			{"name": "slack_channel_id",  "type": "string"},
			{"name": "gitlab_username",   "type": "string"},
			{"name": "github_username",   "type": "string"},
			{"name": "gitlab_user_id",    "type": "string"},
			{"name": "github_user_id",    "type": "string"},
			{"name": "email",             "type": "string"},
			{"name": "email_aliases",     "type": "json"},
			{"name": "identity_link",     "type": "json"},
			{"name": "display_name",      "type": "string"},
			{"name": "pronouns",          "type": "string"},
			{"name": "status_message",    "type": "string"},
			{"name": "team",              "type": "string"},
			{"name": "jira_user",         "type": "string"},
			{"name": "preferences",       "type": "json"},
			{"name": "profile_prefs",     "type": "json"},
			{"name": "capabilities",      "type": "json"},
			{"name": "auth_methods",      "type": "json"},
			{"name": "allowed_projects",  "type": "json"},
			{"name": "land_roles",        "type": "json"},
			{"name": "allowed_roles",     "type": "json"},
			{"name": "terms_version",     "type": "string"},
			{"name": "terms_accepted_at", "type": "timestamp"},
			{"name": "read_cursors",      "type": "json"},
			{"name": "consent_push_to_mr_auto", "type": "boolean"},
			{"name": "welcome_tour_seen_at", "type": "timestamp"}
		]
	}`)},
	// type:plot_application is a Lands application for a plot (spec
	// gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md §2.4,
	// B-D4). The Gate files it, labelled project:lands-intake, and the approval
	// decision moves its state. model is the tenant agents' model the operator
	// set at approval (Sonnet 5.5 unless the approval says otherwise, OQ3
	// gd-Se3rF0T5pu); question and asked_at are the operator's latest "ask".
	// decision_digest pins what the approval decision named by decision_id asks
	// the operator; the Gate refuses that decision once it says anything else
	// (beads-viewer lands_apply.go). The provisioner (controller
	// internal/lands/provision, §3.3) records its progress here: provision_step
	// is the last provisioning step done (it resumes after it),
	// provision_attempt counts starts (one escalation per attempt), and
	// provision_error and provision_escalation_id are the failure it stopped on;
	// close_requested_at/_by and close_reason are gb lands close's request, and
	// close_step the last deprovisioning step done. Admin-gated so an agent's static token cannot
	// file or approve one; the Gate writes as a granted machine principal. The
	// label rule, field shapes, born-submitted rule and state machine are in
	// plot_application.go.
	"type:plot_application": {Key: "type:plot_application", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "applicant_sub",           "type": "string",  "required": true},
			{"name": "applicant_email",         "type": "string",  "required": true},
			{"name": "github_login",            "type": "string",  "required": true},
			{"name": "github_id",               "type": "string",  "required": true},
			{"name": "land_name",               "type": "string",  "required": true},
			{"name": "repo",                    "type": "string",  "required": true},
			{"name": "repo_private",            "type": "boolean"},
			{"name": "github_installation_id",  "type": "string",  "required": true},
			{"name": "purpose",                 "type": "string"},
			{"name": "credits_requested_usd",   "type": "float"},
			{"name": "terms_version",           "type": "string",  "required": true},
			{"name": "aup_version",             "type": "string",  "required": true},
			{"name": "github_account_age_days", "type": "integer", "min": 0},
			{"name": "github_public_repos",     "type": "integer", "min": 0},
			{"name": "state",                   "type": "enum",    "required": true, "values": ["submitted", "approved", "denied", "withdrawn", "provisioning", "provisioned", "failed", "closed"]},
			{"name": "decision_id",             "type": "string"},
			{"name": "decision_digest",         "type": "string"},
			{"name": "decided_by",              "type": "string"},
			{"name": "decided_at",              "type": "timestamp"},
			{"name": "deny_reason",             "type": "string"},
			{"name": "credits_granted_usd",     "type": "float"},
			{"name": "model",                   "type": "string"},
			{"name": "question",                "type": "string"},
			{"name": "asked_at",                "type": "timestamp"},
			{"name": "project",                 "type": "string"},
			{"name": "provision_step",          "type": "enum",    "values": ["project", "purse", "keycloak", "channels", "namespace", "steward", "mail"]},
			{"name": "provision_attempt",       "type": "integer", "min": 0},
			{"name": "provision_error",         "type": "string"},
			{"name": "provision_failed_at",     "type": "timestamp"},
			{"name": "provision_escalation_id", "type": "string"},
			{"name": "close_requested_at",      "type": "timestamp"},
			{"name": "close_requested_by",      "type": "string"},
			{"name": "close_reason",            "type": "string"},
			{"name": "close_step",              "type": "enum",    "values": ["freeze", "steward", "agents", "members", "key", "namespace", "project", "mail"]}
		]
	}`)},
	// type:land_invite is an owner's invitation of a GitHub user into their
	// land (Lands spec §3.7, step 8.9). The viewer files it, labelled
	// project:<land>, after resolving github_login to GitHub's numeric
	// github_id; the Lands provisioner joins the Keycloak user carrying that
	// github_id when they sign in (joined_user_id, joined_at), expires it at
	// expires_at, and carries out an owner's revoke (revoke_requested_at/_by,
	// revoked_at). outcome_note says why an invite ended without a join (the
	// person already held a role in the land). Admin-gated so an owner can
	// never name a github_id the viewer did not look up; the viewer and the
	// provisioner write as granted machine principals. open_invite_cap, set
	// only by the demo door, files the invite only while the land holds fewer
	// open invites. The label rule, field shapes, fixed fields, state machine
	// and the one-open-invite-per-account rule are in land_invite.go.
	"type:land_invite": {Key: "type:land_invite", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "land",                "type": "string",    "required": true},
			{"name": "github_id",           "type": "string",    "required": true},
			{"name": "github_login",        "type": "string",    "required": true},
			{"name": "role",                "type": "enum",      "required": true, "values": ["member", "guest"]},
			{"name": "invited_by",          "type": "string",    "required": true},
			{"name": "expires_at",          "type": "timestamp", "required": true},
			{"name": "open_invite_cap",     "type": "integer",   "min": 1},
			{"name": "state",               "type": "enum",      "required": true, "values": ["pending", "joined", "revoked", "expired"]},
			{"name": "joined_user_id",      "type": "string"},
			{"name": "joined_at",           "type": "timestamp"},
			{"name": "revoke_requested_at", "type": "timestamp"},
			{"name": "revoke_requested_by", "type": "string"},
			{"name": "revoked_at",          "type": "timestamp"},
			{"name": "outcome_note",        "type": "string"}
		]
	}`)},
	// type:gatehouse_request is the gatehouse steward's apply or demo tool
	// (task gd-K62coPtKkX, gatehouse_request.go): kind demo or apply, the
	// person's message (message_id), and for an application what the steward
	// took down. The beads-viewer's gatehouse reactor takes the person from the
	// message's daemon-stamped author_github_id and writes the outcome:
	// outcome is what the steward tells them, result_id the land_invite filed,
	// github_id/github_login whom it was for, handled_at when. Admin-gated; the
	// gatehouse's steward files one through the opGatehouseRequest carve-out
	// while land_gatehouse_tools is on, and the viewer updates it as a granted
	// machine principal. Label rule, fixed fields and state machine in
	// gatehouse_request.go.
	"type:gatehouse_request": {Key: "type:gatehouse_request", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "kind",                  "type": "enum",      "required": true, "values": ["demo", "apply"]},
			{"name": "message_id",            "type": "string",    "required": true},
			{"name": "land_name",             "type": "string"},
			{"name": "purpose",               "type": "string"},
			{"name": "credits_requested_usd", "type": "float"},
			{"name": "state",                 "type": "enum",      "required": true, "values": ["pending", "drafted", "done", "refused"]},
			{"name": "outcome",               "type": "string"},
			{"name": "result_id",             "type": "string"},
			{"name": "github_id",             "type": "string"},
			{"name": "github_login",          "type": "string"},
			{"name": "handled_at",            "type": "timestamp"}
		]
	}`)},
	// type:window is a sharable read-only window on a plan (Lands: the Window,
	// PW.1 gd-sCLo2acEFb, window_routes.go). A child of the plan epic, labelled
	// with the plan's land. target_kind land (LW.1 gd-BXNhQf4ZqV) puts it on a
	// whole land instead: target is then the land's project bead, and it is
	// that bead's child; absent means plan. token_hash is the sha256 of the gw_ bearer the
	// daemon generated and returned once; the token itself is never stored.
	// view_count and last_viewed_at are flushed by the daemon at most every
	// five minutes. Admin-gated: POST/DELETE /v1/windows write it server-side
	// after their own owner-or-operator gate, so no fleet or static token can
	// forge a window or move one to another plan.
	"type:window": {Key: "type:window", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "target",         "type": "string",    "required": true},
			{"name": "target_kind",    "type": "enum",      "values": ["plan", "land"]},
			{"name": "land",           "type": "string",    "required": true},
			{"name": "token_hash",     "type": "string",    "required": true},
			{"name": "expires_at",     "type": "timestamp", "required": true},
			{"name": "created_by",     "type": "string",    "required": true},
			{"name": "revoked_at",     "type": "timestamp"},
			{"name": "revoked_by",     "type": "string"},
			{"name": "view_count",     "type": "integer",   "min": 0},
			{"name": "last_viewed_at", "type": "timestamp"}
		]
	}`)},
	// type:invite is an admin's invitation of a person, by email, to sign in
	// with GitHub (owner ruling gd-hCMlZ5jnvB = b; gb lands invite). The Lands
	// provisioner mails the invitee (mailed_at), and once a Keycloak user with
	// exactly that verified email and a GitHub link exists, adds them to the
	// static group invited, and to land with role when the invite names one,
	// and marks it claimed (claimed_user_id, claimed_at; claimed_gatehouse
	// names the gatehouse land when an invite naming no land made its person a
	// guest there, step 9.12). It expires at
	// expires_at and carries out a revoke of a pending invite
	// (revoke_requested_at/_by, revoked_at). Admin-gated, and unlabelled so
	// only admins and the machine principals read a stranger's address. The
	// field shapes, fixed fields and state machine are in invite.go.
	"type:invite": {Key: "type:invite", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "email",               "type": "string",    "required": true},
			{"name": "land",                "type": "string"},
			{"name": "role",                "type": "enum",      "values": ["member", "guest"]},
			{"name": "invited_by",          "type": "string",    "required": true},
			{"name": "expires_at",          "type": "timestamp", "required": true},
			{"name": "state",               "type": "enum",      "required": true, "values": ["pending", "claimed", "revoked", "expired"]},
			{"name": "mailed_at",           "type": "timestamp"},
			{"name": "claimed_user_id",     "type": "string"},
			{"name": "claimed_at",          "type": "timestamp"},
			{"name": "claimed_gatehouse",   "type": "string"},
			{"name": "revoke_requested_at", "type": "timestamp"},
			{"name": "revoke_requested_by", "type": "string"},
			{"name": "revoked_at",          "type": "timestamp"},
			{"name": "outcome_note",        "type": "string"}
		]
	}`)},
	// type:credit_grant is one entry in a tenant land's credit ledger (Lands
	// Track B spec §3.4, B-D7; step 8.1). gb lands credit add writes it, born
	// closed like any append-only ledger record, and raises the land key's
	// LiteLLM max_budget in the same operation. amount_credits is in credits
	// (1 credit = US$1 billed to the tenant, OQ1 gd-7sTt20714N); rate_version
	// is the credit_rate in force when it was granted. Admin-gated. The label
	// rule, the negative-needs-a-reason rule and immutability are in
	// lands_ledger.go.
	"type:credit_grant": {Key: "type:credit_grant", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "land",           "type": "string",  "required": true},
			{"name": "amount_credits", "type": "float",   "required": true},
			{"name": "reason",         "type": "string"},
			{"name": "granted_by",     "type": "string",  "required": true},
			{"name": "decision_id",    "type": "string"},
			{"name": "rate_version",   "type": "integer", "required": true, "min": 1}
		]
	}`)},
	// type:credit_rate is one version of the Lands rate table (spec §3.4,
	// OQ1 gd-7sTt20714N markup). Each gb lands rates set writes the next
	// version; the highest version is the rate in force. llm_multiplier is
	// credits charged per US$1 of LiteLLM spend; compute_credits_per_agent_hour
	// defaults to 0. No version ships: until the operator sets one, credit
	// grants are refused (fail closed). gb sends idempotency key
	// credit_rate:v<N>, so two writers of one version get one bead. Admin-gated;
	// lands_ledger.go checks the amounts and keeps each version immutable.
	"type:credit_rate": {Key: "type:credit_rate", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "version",                        "type": "integer",   "required": true, "min": 1},
			{"name": "effective_at",                   "type": "timestamp", "required": true},
			{"name": "llm_multiplier",                 "type": "float",     "required": true},
			{"name": "compute_credits_per_agent_hour", "type": "float"},
			{"name": "set_by",                         "type": "string"},
			{"name": "decision_id",                    "type": "string"},
			{"name": "reason",                         "type": "string"},
			{"name": "idempotency_key",                "type": "string"}
		]
	}`)},
	// type:permission is a tool-permission request an agent's harness raised
	// (the bridge's /api/permission/callback), the subject of a Commons
	// permission card (Lands spec §2.5). Open while it waits, closed with
	// behavior allow or deny once someone answers; answered_by is who (the
	// person, oidc:<email>), answered_by_slack_id the Slack user they clicked
	// as, and verdict_error says the agent never got the answer. A request
	// answered in Claude Code's own dialog on the Glass sends no verdict and
	// closes with answered_on "glass" and answer_label, the option's words
	// (behavior is empty when the option says neither). The bridge writes it
	// in the agent's land.
	//
	// Admin-gated (gd-RuI4Ze6vAg): behavior and answered_by are the record of a
	// verdict, and the Commons answers from these cards (3.5), so only the
	// bridge that delivered the verdict may write one. That is its machine
	// principal's gardenio-machine-slack-dm-delivery grant (machine_grants.go)
	// and an OIDC admin. Not in machineAllowedWrites: the shared static token
	// and every fleet token, an agent's own included, are refused.
	"type:permission": {Key: "type:permission", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "agent",          "type": "string", "required": true},
			{"name": "agent_bead_id",  "type": "string"},
			{"name": "request_id",     "type": "string", "required": true},
			{"name": "tool_name",      "type": "string", "required": true},
			{"name": "description",    "type": "string"},
			{"name": "input_preview",  "type": "string"},
			{"name": "behavior",       "type": "enum", "values": ["allow", "deny"]},
			{"name": "answered_by",    "type": "string"},
			{"name": "answered_by_slack_id", "type": "string"},
			{"name": "answered_on",    "type": "string"},
			{"name": "answer_label",   "type": "string"},
			{"name": "verdict_error",  "type": "string"}
		]
	}`)},
	"type:dm": {Key: "type:dm", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "target_user",       "type": "string", "required": true},
			{"name": "source_agent",      "type": "string"},
			{"name": "subject",           "type": "string"},
			{"name": "body",              "type": "string"},
			{"name": "dm_channel",        "type": "string"},
			{"name": "dm_ts",             "type": "string"},
			{"name": "dm_mode",           "type": "string"},
			{"name": "task_context",      "type": "string"},
			{"name": "timeout_minutes",   "type": "integer"},
			{"name": "reminder_sent",     "type": "boolean"},
			{"name": "replied",           "type": "boolean"},
			{"name": "reply_text",        "type": "string"},
			{"name": "reply_user",        "type": "string"},
			{"name": "mr_url",            "type": "string"},
			{"name": "mr_title",          "type": "string"},
			{"name": "mr_pipeline_status","type": "string"}
		]
	}`)},

	"type:account": {Key: "type:account", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "provider",                "type": "string", "required": true},
			{"name": "base_url",                "type": "string"},
			{"name": "credential",              "type": "string"},
			{"name": "model",                   "type": "string"},
			{"name": "probe_model",             "type": "string"},
			{"name": "max_budget",              "type": "float"},
			{"name": "fallback",                "type": "string"},
			{"name": "exclusive_to_role",       "type": "string"},
			{"name": "litellm_key",             "type": "string"},
			{"name": "status",                  "type": "string"},
			{"name": "exhausted_at",            "type": "string"},
			{"name": "cooldown_until",          "type": "string"},
			{"name": "rotation_count",          "type": "integer"},
			{"name": "last_rotated_at",         "type": "string"},
			{"name": "billing_period_hours",    "type": "string"},
			{"name": "throttle_status",         "type": "string"},
			{"name": "probe_auth_status",       "type": "string"},
			{"name": "codex_rejected_refresh_fp","type": "string"},
			{"name": "codex_auth_error",        "type": "string"},
			{"name": "utilization_5h",          "type": "string"},
			{"name": "utilization_7d",          "type": "string"},
			{"name": "representative_claim",    "type": "string"},
			{"name": "overage_status",          "type": "string"},
			{"name": "overage_disabled_reason", "type": "string"},
			{"name": "probe_checked_at",        "type": "string"},
			{"name": "throttle_reset_at",       "type": "string"},
			{"name": "throttle_reset_5h_at",    "type": "string"},
			{"name": "throttle_reset_7d_at",    "type": "string"},
			{"name": "spend_at_last_reset",     "type": "string"},
			{"name": "last_reset_at",           "type": "string"},
			{"name": "session_count",           "type": "integer"},
			{"name": "total_input_tokens",      "type": "integer"},
			{"name": "total_output_tokens",     "type": "integer"},
			{"name": "total_thinking_tokens",   "type": "integer"},
			{"name": "total_cache_read_tokens", "type": "integer"},
			{"name": "total_cache_write_tokens","type": "integer"},
			{"name": "total_cost_usd",          "type": "float"},
			{"name": "daily_cost_usd",          "type": "float"},
			{"name": "weekly_cost_usd",         "type": "float"},
			{"name": "spend_5h_window_usd",     "type": "float"},
			{"name": "spend_7d_window_usd",     "type": "float"},
			{"name": "spend_limit_usd",         "type": "float"},
			{"name": "spend_reset_at",          "type": "string"},
			{"name": "spend_synced_at",         "type": "string"},
			{"name": "live_cost_baseline",      "type": "string"},
			{"name": "purse_notice_level",      "type": "string"},
			{"name": "purse_granted_seen",      "type": "string"},
			{"name": "purse_granted_credits",   "type": "string"},
			{"name": "purse_used_credits",      "type": "string"},
			{"name": "purse_state",             "type": "string"},
			{"name": "purse_read_at",           "type": "string"},
			{"name": "steward_park_reason",     "type": "string"},
			{"name": "land_key_blocked",        "type": "string"},
			{"name": "shadow_total_cost_usd",   "type": "float"},
			{"name": "shadow_daily_cost_usd",   "type": "float"},
			{"name": "shadow_weekly_cost_usd",  "type": "float"},
			{"name": "total_request_count",     "type": "integer"},
			{"name": "total_tool_calls",        "type": "integer"},
			{"name": "total_errors",            "type": "integer"},
			{"name": "total_compactions",       "type": "integer"},
			{"name": "primary",                 "type": "string"},
			{"name": "soft_failover_state",     "type": "string"},
			{"name": "soft_failover_threshold", "type": "string"},
			{"name": "spawn_weight",            "type": "string"},
			{"name": "billing_type",            "type": "string"}
		]
	}`)},

	// type:team is the principal kind ADR docs/adr/taints.md §8.1 defines: in
	// the same family as type:project and type:account — strict schema
	// (unknown fields rejected; AllowAdditionalFields is not set), admin-only
	// writes, and (via model.TypeTeam in crossFleetTypes) visible to every
	// fleet the same way type:project/type:account are today. Only `team` is
	// required so `{"team": "..."}` alone is a valid team bead (§8.1).
	//
	// `members` and `oidc_groups` back membership resolution
	// (server.(*BeadsServer).TeamsOf, §8.1): `members` lists operator ids (or
	// aliases such as emails/Slack ids, resolved through operator_aliases at
	// match time — see teamMatchesMember); `oidc_groups` lists IdP group
	// names matched against the caller's token groups. `owners` and
	// `projects` are informational per §8.1 — they grant nothing by
	// themselves and are not consulted by TeamsOf.
	"type:team": {Key: "type:team", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "team",        "type": "string", "required": true},
			{"name": "display",     "type": "string"},
			{"name": "members",     "type": "json"},
			{"name": "oidc_groups", "type": "json"},
			{"name": "owners",      "type": "json"},
			{"name": "projects",    "type": "json"}
		]
	}`)},

	"type:reaction-rule": {Key: "type:reaction-rule", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "emoji",   "type": "string", "required": true},
			{"name": "match",   "type": "string"},
			{"name": "role",    "type": "string"},
			{"name": "prompt",  "type": "string", "required": true},
			{"name": "project", "type": "string", "required": true}
		]
	}`)},

	// type:runner — one bead per registered laptop (local-agents spec §4.1,
	// Task 7 gd-zPtKJKzZhE). The token secret is never on the bead; token_id
	// is a prefix of the runner_tokens row's hash. state is maintained by
	// heartbeat and revoke (runners.go) and later the stale reaper.
	// credential_mode=broker is M2 (D1). Admin-only through the generic bead
	// API: owner is what a runner's writes are attributed to (D6), so only the
	// daemon's own runner handlers (runners.go, withMachineWrite) and an admin
	// may write it.
	"type:runner": {Key: "type:runner", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "owner",             "type": "string", "required": true},
			{"name": "hostname",          "type": "string", "required": true},
			{"name": "os_arch",           "type": "string"},
			{"name": "tags",              "type": "json"},
			{"name": "projects",          "type": "json",   "required": true},
			{"name": "token_id",          "type": "string"},
			{"name": "credential_mode",   "type": "enum",   "values": ["owner_login", "broker"]},
			{"name": "escalation_policy", "type": "json"},
			{"name": "last_seen_at",      "type": "string"},
			{"name": "state",             "type": "enum",   "values": ["online", "away", "revoked"]}
		]
	}`)},
	// type:runner_invite — single-use, 24h invite minted by /runner invite
	// (spec §4.2 step 1). code_hash is sha256 of the code the invitee types;
	// POST /v1/runners/register redeems it. An invite is the trust root for a
	// runner token, so creating one is admin-only.
	"type:runner_invite": {Key: "type:runner_invite", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "code_hash",   "type": "string", "required": true},
			{"name": "invitee",     "type": "string", "required": true},
			{"name": "projects",    "type": "json",   "required": true},
			{"name": "expires_at",  "type": "string", "required": true},
			{"name": "redeemed_at", "type": "string"},
			{"name": "redeemed_by", "type": "string"}
		]
	}`)},
	// Infrastructure types — config kind.
	// capability_tiers is the role's set of named capability tiers (P9
	// gd-Bcqj2r4m7E; model/capability_tiers.go). Its values are the registered
	// tier names, model.CapabilityTiers. privileged is derived from it: the
	// daemon keeps the two in step on every role write
	// (NormalizeRoleCapabilityTiers). The spawn gate and escalation authz read
	// the set (roleHoldsTier, gd-Pcyz0ftcTg); the controller's and kd's
	// display mirrors still read the flag.
	"type:role": {Key: "type:role", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "secrets",          "type": "json"},
			{"name": "env",              "type": "json"},
			{"name": "env_json",         "type": "json"},
			{"name": "image",            "type": "string"},
			{"name": "cpu_request",      "type": "string"},
			{"name": "cpu_limit",        "type": "string"},
			{"name": "memory_request",   "type": "string"},
			{"name": "memory_limit",     "type": "string"},
			{"name": "docker",           "type": "boolean"},
			{"name": "postgres",         "type": "boolean"},
			{"name": "service_account",  "type": "string"},
			{"name": "storage_class",    "type": "string"},
			{"name": "claude_config",    "type": "json"},
			{"name": "account",          "type": "string"},
			{"name": "model",            "type": "string"},
			{"name": "harness",          "type": "enum", "values": ["claude", "codex", "gemini"]},
			{"name": "allowed_harnesses","type": "string"},
			{"name": "account_whitelist","type": "json"},
			{"name": "no_account_spill", "type": "boolean"},
			{"name": "enabled_skills",   "type": "json"},
			{"name": "disabled_skills",  "type": "json"},
			{"name": "enabled_agents",   "type": "json"},
			{"name": "disabled_agents",  "type": "json"},
			{"name": "needs_k8s_api",    "type": "boolean"},
			{"name": "verifier",         "type": "boolean"},
			{"name": "machine_client",   "type": "string"},
			{"name": "ziti_access",      "type": "boolean"},
			{"name": "ziti_service_id",  "type": "string"},
			{"name": "ziti_bind",        "type": "boolean"},
			{"name": "ziti_ttl",         "type": "string"},
			{"name": "ziti_optional",    "type": "boolean"},
			{"name": "session_resume",   "type": "string"},
			{"name": "ultracode",        "type": "string"},
			{"name": "runtime_class",    "type": "string"},
			{"name": "execution_target", "type": "string"},
			{"name": "privileged",       "type": "boolean"},
			{"name": "capability_tiers", "type": "enum[]", "values": ["privileged", "lands-tenant"]},
			{"name": "ownership_grants", "type": "json"}
		]
	}`)},
	// type:spawn_policy declares the auto-approval rules for privileged-role
	// agent spawns (design doc gd-sGW5Y1B8sg §4, gd-vmLhgqydEa). One bead per
	// privileged role, keyed by the `role` field (mirrors type:role's Title
	// keying, but spawn_policy uses a field so a role's policy bead can be
	// found by ListBeads(Fields:{"role": ...}) the same way lookupSpawnPermissions
	// finds a user bead by email). `rules` is a JSON array evaluated first-match
	// by the system:spawn-policy evaluator (spawn_policy.go); each rule matches
	// on {project, namespace, requester_role} (empty/absent = wildcard) and
	// declares a mode of "auto" (requires positive max_per_day + max_concurrent,
	// enforced at write time by validateSpawnPolicyFields) or "human" (optional
	// approvers list — enforced: only those approvers (oidc:/slack:/bare email
	// via operator aliases, or team:<id>) may approve a matching spawn, and with
	// no approvers a privileged role is admin-only; an optional four_eyes=true stamps requires_four_eyes and is
	// refused at write unless approvers name >=2 distinct operators). A role with no spawn_policy bead is untouched by the evaluator
	// (opt-in per role, mirroring decision_aging's opt-in-per-decision model).
	"type:spawn_policy": {Key: "type:spawn_policy", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "role",  "type": "string", "required": true},
			{"name": "rules", "type": "json",    "required": true}
		]
	}`)},
	// type:escalation_policy routes escalations (escalation_policy.go): who may
	// approve, who executes, who holds it now. The newest open bead is active;
	// none means the built-in D7. Validated on write (validateEscalationPolicy).
	// Changing it changes who may approve, so it is admin-only.
	"type:escalation_policy": {Key: "type:escalation_policy", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "rules",        "type": "json", "required": true},
			{"name": "coordinators", "type": "json", "required": true}
		]
	}`)},
	"type:rig":    {Key: "type:rig", Value: json.RawMessage(`{"kind":"config","write_policy":{"min_role":"gardenio-admin"},"fields":[]}`)},
	"type:convoy": {Key: "type:convoy", Value: json.RawMessage(`{"kind":"config","write_policy":{"min_role":"gardenio-admin"},"fields":[]}`)},
	"type:config": {Key: "type:config", Value: json.RawMessage(`{"kind":"config","write_policy":{"min_role":"gardenio-admin"},"allow_additional_fields":true,"fields":[{"name":"value","type":"json"}]}`)},

	// Infrastructure types — data kind.
	// authz-denied-text is a non-type config for customizing the advisory text
	// returned in 403 responses. Operators can override the template.
	"authz-denied-text": {Key: "authz-denied-text", Value: json.RawMessage(`{
		"template": "Write denied: {action} on type \"{type}\" was rejected. {reason}\n\n{escalate_hint}"
	}`)},

	"type:event": {Key: "type:event", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "trigger_id",    "type": "string"},
			{"name": "trigger_title", "type": "string"},
			{"name": "source",        "type": "string"},
			{"name": "action",        "type": "string"},
			{"name": "match_key",     "type": "string"},
			{"name": "match_title",   "type": "string"},
			{"name": "project",       "type": "string"},
			{"name": "role",          "type": "string"},
			{"name": "created_by",    "type": "string"},
			{"name": "outcome",       "type": "string"},
			{"name": "reason",        "type": "string"},
			{"name": "timestamp",     "type": "string"}
		]
	}`)},
	"type:gate": {Key: "type:gate", Value: json.RawMessage(`{"kind":"data","write_policy":{"project_scoped":true,"escalatable":true},"fields":[]}`)},
	// type:message — ephemeral conversation message bead. Created-then-closed
	// by the Slack-facing producers (gb squawk, gb slack reply, the beads-viewer
	// thread composer's "send", and the slack-bridge inbound thread-forward that
	// records a human's reply — recordThreadConversationMessage); the slack-bridge
	// ThreadMessageForward watcher relays outbound closed beads into Slack. They
	// stamp the origin-agnostic wish-pointer field `thread_id` (== the thread
	// head's ka-id) so the beads-viewer /threads timeline renders the message as
	// a conversation member — membership is UNION(fields.thread_id == headID,
	// parent-child closure) (cmd/beads-viewer/threads.go).
	//
	// This field list MUST stay a superset of what every producer marshals — a
	// producer that writes a key absent here gets a hard 400 ("<key>: unknown
	// field") and the message is silently dropped, so the human's reply never
	// reaches the thread timeline. This has now bitten twice: `thread_id` was
	// added to the producers but not the schema (ka-0000003279 / ka-0000005416),
	// and then `slack_user_id` was added by recordThreadConversationMessage's
	// inbound path (the per-reply author id) but not the schema, 400'ing 100% of
	// human thread follow-ups (ka-0000004306).
	//
	// `operated_by` is carried because the unify change (ka-0000007618, decision
	// ka-0000005492 → option C) folds the per-reply tracking TASK into this one
	// conversation bead: the inbound human-reply message now carries the
	// originating operator (slack:U…/oidc:…) in this field so the beads-viewer
	// per-user inbox can attribute the "USER → AGENT" bubble to the human without
	// a separate slack-thread-reply/slack-mention task bead.
	//
	// The plan_id / turn_role / turn_id / text_truncated / full_length fields are
	// the /plans in-viewer chat transcript producer's set (planTranscriptTurnBead,
	// beads-viewer plan_transcript_spike.go; the operator half mintOperatorTurn and
	// the assistant half AssistantTurnMinter). A plan-chat turn is a type=message
	// bead bound to its plan epic by fields.plan_id and rendered on the operator or
	// assistant side from fields.turn_role. These were absent from the schema, so
	// EVERY transcript-turn mint 400'd "plan_id: unknown field"; because the mint is
	// best-effort (the error is logged and swallowed so the message still delivers),
	// plan transcript turns silently never persisted — the third instance of this
	// writer-vs-schema drift class (thread_id: ka-0000003279/ka-0000005416;
	// slack_user_id: ka-0000004306). Surfaced by the 4b beads-viewer-e2e coverage
	// dogfooding the transcript against a real daemon (unit tests use a fake daemon
	// that does not validate fields). Guarded going forward by
	// TestBuiltinMessageProducerFieldsDeclared.
	//
	// idempotency_key is persisted whenever a create carries an IdempotencyKey
	// (createBead folds it into Fields[idempotency_key], beads.go), so it is
	// validated against this schema like any other field. The transcript mint sets
	// it (planTurnIdempotencyKey, "plan-turn:<planID>:<turn_id>") to collapse a
	// double-submit / re-delivered turn to ONE bead — so it MUST be declared here
	// too, or the mint 400s "idempotency_key: unknown field" even once the
	// transcript fields above are present. It rides in the shared auditFields for
	// issue/agent types; type:message (data-kind) doesn't use auditFields, so it is
	// declared explicitly.
	//
	// trigger_title is stamped by the controller's NoticeNotifier
	// (gardenio/controller/internal/bridge/notice.go) — the trigger-engine notify
	// action, the gitlab-bridge release-drought watchdog, and slack-bridge reaction
	// notices all publish a closed type=message notice bead carrying the triggering
	// trigger's title so the Notice watcher can attribute the Slack post. The field
	// was absent from this schema, so every notice with a non-empty trigger title
	// 400'd "trigger_title: unknown field" and — because the publish helper only
	// logs+swallows the create error — the notice was silently dropped (never
	// closed, never posted). The fourth instance of this producer-vs-schema drift
	// class (thread_id: ka-0000003279/ka-0000005416; slack_user_id: ka-0000004306;
	// plan_id: 4b e2e). Guarded going forward by
	// TestBuiltinMessageProducerFieldsDeclared (ka-0000005920).
	//
	// jira_key / visibility are the `gb jira reply` provenance producer's set
	// (recordJiraReplyBead, gardenio/controller/cmd/gb/jira.go) — the Jira twin of
	// the slack_thread_* fields recordSlackReplyBead stamps. They were absent from
	// this schema, so EVERY successful `gb jira reply` 400'd
	// "jira_key: unknown field; visibility: unknown field" on the follow-up bead
	// create and — because the record is best-effort (logged+swallowed) — the
	// type=message provenance bead was silently never created, leaving bridge
	// replies invisible in beads-viewer. The fifth instance of this
	// producer-vs-schema drift class (ka-0000000661).
	//
	// channel_id is the Commons channel bead the message was said in (Lands
	// spec §2.3). A message that carries it is land-bound: it needs exactly one
	// project: label, equal to its channel's (channel_validation.go).
	//
	// reactions is a map of reaction to the list of principals who reacted
	// (oidc:<email> / agent:<name>), Lands spec §2.3. A scoped principal may
	// patch it, and only its own entries (scopedReactionDenial).
	//
	// kind, subject_id, author and origin are the conversation fields the
	// bridge's router writes (Lands spec §2.3); slack_channel / slack_ts are
	// the Slack copy's ref, stored back on the bead so a card is updated in
	// place. origin purse_notice / purse_refill marks the purse watcher's
	// threshold notice and refill post (lands/watcher), and land_opened the
	// Lands provisioner's one wake post in a new land's #machines
	// (internal/steward ProvisionHook.Wake), and person_joined its wake post
	// when a person joins a land (ProvisionHook.Greet, step 9.14); a land's
	// steward wakes on each.
	// origin glass is a Glass nudge written into its agent's thread as the
	// person's words (gardenhub GlassOrigin), which the bridge mirrors into the
	// agent's Slack thread without delivering it again. A kind=card message names the bead it renders in subject_id and
	// is unique per (subject_id, channel_id) (card_validation.go). detail and
	// footnote are a kind=notice message's rich face (body and footnote) beside
	// the fallback text. notice and the notice_* fields are a death report,
	// crash notice or death-storm roll-up's face in the Commons; notice_dead
	// is the roll-up's held-back deaths, [{"id","name"}], and notice_subject
	// is the agent bead a death or crash is about (subject_id stays card-only,
	// gd-JGmXQuSndB). files is a message's
	// attachments as an array of {name, mimetype, size, url} links (never
	// bytes), and slack_attachment_count / slack_has_images summarize a Slack
	// message's files; the bridge's Slack mirror and thread forward write all three
	// (commons_mirror.go, bot_thread_forward.go slackFilesToFields).
	// snap is a farm snapshot's context beside its picture in files (the
	// /garden Snap button, gd-GbF2JS04h5): where the camera was, what it
	// framed and the view's link, a JSON object the Commons writes bounded.
	// to is a DM's target, the principal a gb say --to was sent to (Lands
	// spec §2.3; the message is in a channel_kind=dm channel, kind=ask for
	// gb say --ask).
	// pg_report_day is the UTC day (YYYY-MM-DD) of a daily Postgres efficiency
	// report in #machines (controller internal/pgbudget runner, gd-ylgk1jBhLi),
	// which the runner looks up so it posts once a day. Guarded by
	// TestBuiltinPgBudgetWritesValidate.
	// nonce is the client nonce a Commons say or reply was sent with, which
	// drops a repeated send (gardenio/controller/internal/commons/sendonce.go).
	// about is the bead a Commons message asks about: the workbench's "Ask the
	// steward about this" sends it (gardenio/controller/internal/commons/write.go).
	"type:message": {Key: "type:message", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "source_agent",         "type": "string"},
			{"name": "text",                 "type": "string"},
			{"name": "trigger_title",        "type": "string"},
			{"name": "channel",              "type": "string"},
			{"name": "thread_ts",            "type": "string"},
			{"name": "slack_thread_channel", "type": "string"},
			{"name": "slack_thread_ts",      "type": "string"},
			{"name": "slack_user_id",        "type": "string"},
			{"name": "operated_by",          "type": "string"},
			{"name": "thread_id",            "type": "string"},
			{"name": "jira_key",             "type": "string"},
			{"name": "visibility",           "type": "string"},
			{"name": "plan_id",              "type": "string"},
			{"name": "turn_role",            "type": "string"},
			{"name": "turn_id",              "type": "string"},
			{"name": "text_truncated",       "type": "string"},
			{"name": "full_length",          "type": "string"},
			{"name": "idempotency_key",      "type": "string"},
			{"name": "channel_id",           "type": "string"},
			{"name": "reactions",            "type": "json"},
			{"name": "kind",                 "type": "enum", "values": ["text", "card", "notice", "ask"]},
			{"name": "subject_id",           "type": "string"},
			{"name": "author",               "type": "string"},
			{"name": "author_github_id",     "type": "string"},
			{"name": "author_github_login",  "type": "string"},
			{"name": "origin",               "type": "enum", "values": ["slack", "commons", "cli", "purse_notice", "purse_refill", "land_opened", "person_joined", "glass"]},
			{"name": "slack_channel",        "type": "string"},
			{"name": "slack_ts",             "type": "string"},
			{"name": "detail",               "type": "string"},
			{"name": "footnote",             "type": "string"},
			{"name": "notice",               "type": "enum", "values": ["death", "crash", "death_storm"]},
			{"name": "notice_agent",         "type": "string"},
			{"name": "notice_cause",         "type": "string"},
			{"name": "notice_pod",           "type": "string"},
			{"name": "notice_pod_phase",     "type": "string"},
			{"name": "notice_count",         "type": "string"},
			{"name": "notice_window",        "type": "string"},
			{"name": "notice_dead",          "type": "string"},
			{"name": "notice_subject",       "type": "string"},
			{"name": "files",                "type": "json"},
			{"name": "slack_attachment_count", "type": "string"},
			{"name": "slack_has_images",     "type": "string"},
			{"name": "snap",                 "type": "json"},
			{"name": "to",                   "type": "string"},
			{"name": "handled_at",           "type": "string"},
			{"name": "promoted_to",          "type": "string"},
			{"name": "pg_report_day",        "type": "string"},
			{"name": "nonce",                "type": "string"},
			{"name": "about",                "type": "string"}
		]
	}`)},
	// type:channel is a Commons channel (Lands spec
	// gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md §2.2).
	// Status open is a live channel, closed is archived; the title is the display
	// name and the description the topic. channel_roles replaces the project's
	// per-channel channel_roles entry, default_role and runtime_class the
	// per-channel default role and channel_runtime_class; card_theme is read by
	// the Slack surface only. The cross-field land rules (exactly one project:
	// label, name unique per land, slack_channel_id unique across lands, members
	// only for user and dm and required for dm) are in channel_validation.go,
	// because a FieldDef cannot express them; who may post in and join a
	// members-only channel is in channel_membership.go. Only an admin sets,
	// changes or clears slack_channel_id and mirror (channel_binding_authz.go).
	// Created with `gb channel create`, joined with `gb channel join`.
	// channel_kind machines is the land's machine channel (steward amendment
	// 2026-10-23 §2): never mirrored, so channel_validation.go refuses a
	// Slack binding or a mirror on it.
	"type:channel": {Key: "type:channel", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"project_scoped": true},
		"fields": [
			{"name": "name",             "type": "string", "required": true},
			{"name": "channel_kind",     "type": "enum", "required": true, "values": ["general", "crew", "decisions", "escalations", "machines", "user", "dm"]},
			{"name": "channel_roles",    "type": "string[]"},
			{"name": "default_role",     "type": "string"},
			{"name": "runtime_class",    "type": "string"},
			{"name": "card_theme",       "type": "enum", "values": ["garden", "business"]},
			{"name": "slack_channel_id", "type": "string"},
			{"name": "mirror",           "type": "enum", "values": ["off", "out", "both"]},
			{"name": "members",          "type": "string[]"}
		]
	}`)},
	// type:notification is one person's notice that a message mentioned them
	// (Lands Commons spec §7.3 step 6, notification.go).
	"type:notification": {Key: "type:notification", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"project_scoped": true},
		"fields": [
			{"name": "to",         "type": "string"},
			{"name": "reason",     "type": "enum", "values": ["mention"]},
			{"name": "message_id", "type": "string"},
			{"name": "channel_id", "type": "string"},
			{"name": "thread_id",  "type": "string"}
		]
	}`)},
	"type:formula": {Key: "type:formula", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"project_scoped": true, "escalatable": true},
		"fields": [
			{"name": "vars",  "type": "json"},
			{"name": "steps", "type": "json"},
			{"name": "default_roles", "type": "json"},
			{"name": "assigned_agent", "type": "string"},
			{"name": "formula_type", "type": "enum", "values": ["workflow", "expansion", "aspect"]},
			{"name": "extends", "type": "json"},
			{"name": "phase", "type": "enum", "values": ["vapor", "liquid"]},
			{"name": "requires_skills", "type": "json"},
			{"name": "aspects", "type": "json"},
			{"name": "match", "type": "string"},
			{"name": "before", "type": "json"},
			{"name": "after", "type": "json"}
		]
	}`)},
	// Agent-artifact bundles (T1) — see
	// gardenio/plans/agent-artifact-bundles-multi-doc-viewer.md §3.3 for the
	// schema. A bundle is an issue-kind container holding type=doc children
	// linked via parent-child edges. The renderer reads `kind` (display
	// classification) and optional `layout` JSON; standard MR fields are
	// included so a bundle can carry MR/audit metadata like any other
	// issue-kind bead.
	//
	// severity / source_ref / report_kind + reproFields are carried here too so
	// type=bundle is a strict SUPERSET of the fields fsm-triage stamps on its
	// triage beads (headline / severity / source_ref / report_kind — see a live
	// triage bead, and the type:rich-report schema below). The widget/report
	// deprecation (operator directive, thread ka-0000004862; epic ka-0000007561)
	// migrates fsm-triage off type=rich-report onto type=bundle (ka-0000004066),
	// and fsm-triage is the only live rich-report producer. Without these fields
	// the emit would 400 ("severity: unknown field") on the exact bead type the
	// migrated triage produces — the same unknown-field trap the rich-report
	// schema was itself patched for (ka-0000003501). reproFields is included for
	// the same reason it is on rich-report: the repro-harness formula
	// (ka-0000002084) mirrors gate_passed / repro_evidence / repro_scorecard onto
	// the triage bead, and the gate_passed→valid-URL invariant (validateReproFields,
	// enforced on bundle in beads.go — ka-0000002579) must hold on the mirror
	// target. The rule is inert unless gate_passed=true, so agent-artifact bundles
	// that never set it are unaffected. Guarded by TestBuiltinReproFieldsDeclared
	// and TestBuiltinBundleSupersetOfRichReport.
	"type:bundle": {Key: "type:bundle", Value: issueFields(`
			{"name": "kind",        "type": "enum", "values": ["plan", "post-mortem", "spec", "report", "other"], "required": true},
			{"name": "headline",    "type": "string"},
			{"name": "summary",     "type": "string"},
			{"name": "layout",      "type": "json"},
			{"name": "report_kind", "type": "string"},
			{"name": "severity",    "type": "string"},
			{"name": "source_ref",  "type": "string"},` + reproFields)},

	// type:doc is a single document inside a bundle. Position controls
	// display order among siblings (lower first); ties break on created_at.
	// content is capped at 256KB inline; over-cap writes must use content_url.
	// The cap is enforced by validateDocFields (internal/server/bundles.go),
	// not the type-config schema, since FieldDef has no native size constraint.
	// machines_high_water is a land's steward-notes bead's #machines read mark
	// (RFC 3339), written by the steward at the end of each run (step 9.2a;
	// the controller's steward.NotesHighWaterField). digest_at and
	// digest_used_credits are where the last daily digest stopped: its as-of
	// time (RFC 3339) and the land's used credits then, written by
	// `gb lands digest --post` after the digest is said (step 9.6; the
	// controller's steward.DigestAtField / DigestUsedField).
	"type:doc": {Key: "type:doc", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "format",      "type": "enum", "values": ["md", "diff", "csv", "code", "image", "link", "html"], "required": true},
			{"name": "headline",    "type": "string"},
			{"name": "position",    "type": "integer"},
			{"name": "content",     "type": "string"},
			{"name": "content_url", "type": "string"},
			{"name": "language",    "type": "string"},
			{"name": "idempotency_key", "type": "string"},
			{"name": "machines_high_water", "type": "string"},
			{"name": "digest_at",           "type": "string"},
			{"name": "digest_used_credits", "type": "string"}
		]
	}`)},

	// idempotency_key: the pour handler (http_formula_pour.go, ka-0000005875)
	// stamps a deterministic key on the molecule create so a double-submit or a
	// concurrent replay of the same pour collapses to ONE molecule via the
	// createBead lookup-or-return (migration 029) instead of double-pouring.
	// createBead folds CreateBeadRequest.IdempotencyKey into Fields[idempotency_key]
	// BEFORE ValidateTypeFields, so the molecule type MUST declare the field or the
	// keyed create 400s "idempotency_key: unknown field" — the same unknown-field
	// trap type:agent (ka-0000003501) and the transcript types already hit. Unlike
	// the issue-kind builtins, type:molecule carries a hand-rolled field list (not
	// issueFields/auditFields), so it is declared here explicitly. On builtin
	// (not just the store row) so resolveTypeConfig's builtin-merge repairs an
	// already-persisted type:molecule config on upgrade with no SetConfig roll-forward.
	"type:molecule": {Key: "type:molecule", Value: json.RawMessage(`{
		"kind": "issue",
		"fields": [
			{"name": "formula_id",       "type": "string"},
			{"name": "applied_vars",     "type": "json"},
			{"name": "ephemeral",        "type": "boolean"},
			{"name": "trigger_bead_ids", "type": "json"},
			{"name": "source_bead_ids",  "type": "json"},
			{"name": "idempotency_key",  "type": "string"},` + reproFields + `
		]
	}`)},
	// type:script is a probe-execution payload (probe-pods plan ka-0000000544).
	// It names a shell script that the controller / `gb probe run` realizes as a
	// one-off K8s pod running under execution_target=probe: a bare `/bin/sh -lc
	// $SCRIPT` container with NO coop lifecycle, NO Claude OAuth, and a slim
	// (50m/128Mi) envelope — the cheap "did anything change?" pre-check that
	// replaces a full no-op gardener inference. Pod exit 0 closes the bead done;
	// non-zero marks it failed (statusreporter.PhaseToAgentState, exactly like an
	// agent pod). kind=data: a script is a payload, not a tracked issue.
	//
	//   - script:  the shell body executed as `/bin/sh -lc "$script"`. Required.
	//   - activity_deadline_seconds: pod activeDeadlineSeconds ceiling. A probe
	//     is a fast pre-check; the pod is killed (→ failed) past this. Default
	//     120s is applied by the runner when unset/zero.
	//   - exit_code: the observed container exit code, stamped by the runner at
	//     completion for post-hoc inspection.
	//   - agent_state: the terminal outcome the runner stamps at close —
	//     "done" (exit 0) or "failed" (non-zero) — mirroring the agent-bead
	//     terminal states so a probe bead reads the same as an agent bead.
	//   - project: the owning project (the probe pod's project label). A probe
	//     bead SHOULD carry one so pods land in the right namespace/scope.
	"type:script": {Key: "type:script", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"project_scoped": true},
		"fields": [
			{"name": "script",                    "type": "string", "required": true},
			{"name": "activity_deadline_seconds", "type": "integer"},
			{"name": "exit_code",                 "type": "integer"},
			{"name": "close_reason",              "type": "string"},
			{"name": "agent_state",               "type": "enum", "values": ["done", "failed"]},
			{"name": "execution_target",          "type": "string"},
			{"name": "project",                   "type": "string"}
		]
	}`)},
	"type:memory": {Key: "type:memory", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "category",           "type": "enum", "values": ["insight", "pattern", "feedback", "context", "reference", "decision_log"]},
			{"name": "role",               "type": "string"},
			{"name": "project",            "type": "string"},
			{"name": "author_agent",       "type": "string"},
			{"name": "tags",               "type": "string"},
			{"name": "supersedes",         "type": "string"},
			{"name": "confidence",         "type": "float"},
			{"name": "confirmations",      "type": "integer"},
			{"name": "last_confirmed",     "type": "string"},
			{"name": "content_hash",       "type": "string"},
			{"name": "layer",              "type": "enum", "values": ["buffer", "episodic", "semantic"]},
			{"name": "strength",           "type": "float"},
			{"name": "half_life_days",     "type": "float"},
			{"name": "emotional_valence",  "type": "enum", "values": ["neutral", "positive", "negative", "critical"]},
			{"name": "schema_fit",         "type": "float"},
			{"name": "retrieval_count",    "type": "integer"},
			{"name": "last_retrieved",     "type": "string"},
			{"name": "pinned",             "type": "boolean"},
			{"name": "outcome_positive",   "type": "integer"},
			{"name": "outcome_negative",   "type": "integer"},
			{"name": "outcome_score",      "type": "integer"},
			{"name": "conflicts_with",     "type": "string"},
			{"name": "source",             "type": "string"}
		]
	}`)},
	"type:mention":  {Key: "type:mention", Value: json.RawMessage(`{"kind":"data","fields":[]}`)},
	"type:artifact": {Key: "type:artifact", Value: json.RawMessage(`{"kind":"data","fields":[],"allow_additional_fields":true}`)},
	"type:runbook":  {Key: "type:runbook", Value: json.RawMessage(`{"kind":"data","fields":[]}`)},
	// type:credential — the queryable inventory of every long-lived secret that
	// grants this system access to something. Operator decision gd-NnaRPh0AY0
	// chose `structured-field` over leaving expiries as decision-comment prose,
	// and the case that forced it is instructive: the fine-grained GitHub PAT
	// every agent pod mounts (gardenio-crew-contents, minted in gd-QoziM1L954)
	// expires 2026-10-19, and that date existed ONLY as free text inside one
	// bead's comment. Nothing could query it, so nothing could alert on it, so
	// the first signal would have been every crew's git push failing at once.
	//
	// expires_at is therefore a real `timestamp`, not a string: it is the whole
	// point of the type. A reaper or dashboard filters on it; prose cannot be
	// filtered. minted_at/revoked_at match so a rotation is a pair of beads
	// with an overlap window rather than an edit that destroys the history.
	//
	// secret_ref is string[] because one credential routinely lands in several
	// places — the shared PAT above is in BOTH `gardenio-git-credentials` and
	// `gascity-git-credentials`, and a single-valued ref would have quietly
	// recorded half the blast radius. Likewise scopes is string[] so a grant
	// can be diffed against what the consumer actually needs.
	//
	// Admin-only writes, like type:audit: this is the inventory an attacker
	// would most want to edit, and no crew agent has any reason to.
	// See gardenio/plans/github-app-onboarding.md §2.3 for the seeded rows and
	// sibling decision gd-OICMBdBfIo for the capability_grants ledger this
	// inventory is the static half of.
	"type:credential": {Key: "type:credential", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "token_name",      "type": "string",    "required": true},
			{"name": "credential_type", "type": "enum",      "required": true, "values": ["pat-classic", "pat-fine-grained", "github-app-key", "github-app-installation", "github-actions-token", "aws-secret", "kubernetes-secret", "ssh-key", "other"]},
			{"name": "consumer",        "type": "string"},
			{"name": "issuer",          "type": "string"},
			{"name": "scopes",          "type": "string[]"},
			{"name": "secret_ref",      "type": "string[]"},
			{"name": "minted_at",       "type": "timestamp"},
			{"name": "expires_at",      "type": "timestamp"},
			{"name": "revoked_at",      "type": "timestamp"},
			{"name": "rotation_doc",    "type": "string"},
			{"name": "supersedes",      "type": "string"},
			{"name": "operated_by",     "type": "string"},
			{"name": "user_bead_id",    "type": "string"},
			{"name": "slack_user_id",   "type": "string"}
		]
	}`)},
	// type:audit — the server's authz record (audit_log.go). action and
	// auth_method must admit every AuthzDenial.Action and AuthIdentity.Type
	// that reaches auditDenial, or that denial's record is refused as invalid
	// fields (gardenio_audit_write_failures_total{reason="invalid"}). The prod
	// store row predates most values; the enum union in resolveTypeConfig
	// carries new ones to it. An impersonated session's record (action
	// impersonate, impersonation.go) adds subject, session_id, client_id,
	// user_agent and idempotency_key; the builtin merge adds missing fields.
	"type:audit": {Key: "type:audit", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "actor",         "type": "string",  "required": true},
			{"name": "action",        "type": "enum",    "required": true, "values": ["create", "update", "close", "delete", "spawn", "approve", "reject", "hook-execute", "revoke_token", "undo", "react", "join", "comment", "hook-emit", "edit_holds", "steward_delegate", "window_manage", "result", "deploy", "release_deploy", "impersonate"]},
			{"name": "resource",      "type": "string"},
			{"name": "resource_type", "type": "string"},
			{"name": "outcome",       "type": "enum",    "required": true, "values": ["allow", "deny"]},
			{"name": "reason",        "type": "string"},
			{"name": "auth_method",   "type": "enum",    "values": ["oidc", "static", "fleet", "machine", "land", "runner", "edge", "window", "anonymous"]},
			{"name": "source_ip",     "type": "string"},
			{"name": "escalation_id", "type": "string"},
			{"name": "subject",       "type": "string"},
			{"name": "session_id",    "type": "string"},
			{"name": "client_id",     "type": "string"},
			{"name": "user_agent",    "type": "string"},
			{"name": "idempotency_key", "type": "string"}
		]
	}`)},
	// type:escalation — a request for a privileged operator to perform a
	// concrete action an agent cannot. Beyond the create-time operation fields,
	// the re-surface bookkeeping fields (reflag_*, pingback_*, auto_pingback_count,
	// queue_alert_*, notified_at, slack_*) MUST be declared here or the daemon
	// 400s "unknown field" on the very PATCH that re-surfaces a stalled P0 —
	// the exact failure ka-0000004605 hit: `gb escalation reflag`/`pingback` both
	// 400'd, so gardeners could not re-notify the operator. `gb escalation reflag`
	// (runReflag) stamps reflag_* + pingback_* (source="reflag"); `gb escalation
	// pingback` stamps pingback_* (source="manual"); the auto-pingback loop stamps
	// auto_pingback_count + pingback_* (source="auto") + queue_alert_requested_at;
	// the bridge notify handshake stamps notified_at/slack_*/pingback_notified_at/
	// queue_alert_notified_at; the bridge staleness-digest loop stamps
	// last_digested_at (per-bead dedup for the consolidated stale-P0/P1 digest,
	// ka-0000006001); `gb escalation create --requesting-task` stamps
	// requesting_plan (the nearest plan-labeled epic ancestor — ka-0000006860),
	// which was missed by the ka-0000004605 sweep and 400'd every plan-anchored
	// escalation's plan pin until ka-0000000658 declared it here. An escalation
	// raised inside a Slack thread carries the wish-pointer thread_id — the
	// server's transitive copy (maybeCopyThreadID, auto_parent.go) lands it via
	// a direct store write that bypasses ValidateTypeFields, but an explicit
	// create that supplies thread_id in the request 400'd until ka-0000000226
	// declared it here; without it the escalation is invisible in the thread
	// timeline (UNION(fields.thread_id==head, parent-child closure) —
	// cmd/beads-viewer/threads.go, ka-0000003279). `gb escalation create
	// --requesting-task` also stamps close_requesting_task (true/false, always
	// written alongside requesting_task) — the gd-91b7zMOR2z fix that moved the
	// close-on-execute opt-in off the sticky task label and onto the escalation
	// itself; omitting it from this schema would 400 "unknown field" on every
	// --requesting-task create, the exact failure class this comment already
	// documents twice above.
	// TestBuiltinEscalationReflagPingbackFieldsDeclared guards this superset
	// against writer drift.
	"type:escalation": {Key: "type:escalation", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "operation_type",           "type": "string",  "required": true},
			{"name": "operation_spec",           "type": "json",    "required": true},
			{"name": "target_bead_id",           "type": "string"},
			{"name": "context",                  "type": "string"},
			{"name": "reason",                   "type": "string"},
			{"name": "taint_names",              "type": "json"},
			{"name": "taint_owner_teams",        "type": "json"},
			{"name": "requesting_agent",         "type": "string"},
			{"name": "requesting_task",          "type": "string"},
			{"name": "requesting_plan",          "type": "string"},
			{"name": "implements_decision",      "type": "string"},
			{"name": "close_requesting_task",    "type": "boolean"},
			{"name": "awaits_release",           "type": "boolean"},
			{"name": "spawn_role",               "type": "string"},
			{"name": "execution_script",         "type": "string"},
			{"name": "credential_requirements",  "type": "json"},
			{"name": "data_requirements",        "type": "json"},
			{"name": "expected_outputs",         "type": "json"},
			{"name": "sensitivity",              "type": "enum", "values": ["public", "internal", "sensitive", "secret"]},
			{"name": "audience",                 "type": "enum", "values": ["owner", "operator", "desk"]},
			{"name": "requires_four_eyes",       "type": "boolean"},
			{"name": "approvers",                "type": "json"},
			{"name": "approved_by",              "type": "string"},
			{"name": "rejected_by",              "type": "string"},
			{"name": "rejected_client",          "type": "string"},
			{"name": "approved_by_email",        "type": "string"},
			{"name": "approved_at",              "type": "string"},
			{"name": "approval_method",          "type": "enum", "values": ["web_ui", "api_token", "cli", "policy", "coordinator", "commons", "edge", "decision"]},
			{"name": "approval_comment",         "type": "string"},
			{"name": "policy_rule",              "type": "string"},
			{"name": "permissions_at_approval",  "type": "json"},
			{"name": "snapshot_at",              "type": "string"},
			{"name": "approved_as",              "type": "string"},
			{"name": "held_by",                  "type": "string"},
			{"name": "held_at",                  "type": "string"},
			{"name": "execute_by",               "type": "string"},
			{"name": "operator_only",            "type": "boolean"},
			{"name": "operator_class",           "type": "enum", "values": ["1", "2", "3"]},
			{"name": "claimed_by",               "type": "string"},
			{"name": "claimed_at",               "type": "string"},
			{"name": "handed_up_by",             "type": "string"},
			{"name": "handed_up_at",             "type": "string"},
			{"name": "hand_up_reason",           "type": "string"},
			{"name": "held_by_offline_at",       "type": "string"},
			{"name": "held_by_last_seen_at",     "type": "string"},
			{"name": "held_by_offline_notified_at", "type": "string"},
			{"name": "approved_targets",         "type": "json"},
			{"name": "execution_target",         "type": "enum", "values": ["local", "service_account", "server"]},
			{"name": "target_runner",            "type": "string"},
			{"name": "execution_dispatch",       "type": "enum", "values": ["server", "local", "service_account", "none"]},
			{"name": "target_owner",             "type": "string"},
			{"name": "requesting_runner",        "type": "string"},
			{"name": "executor_mail_id",         "type": "string"},
			{"name": "executor",                 "type": "string"},
			{"name": "result",                   "type": "json"},
			{"name": "result_status",            "type": "enum", "values": ["success", "failure", "partial"]},
			{"name": "result_client",            "type": "string"},
			{"name": "prompt_delivered_at",      "type": "string"},
			{"name": "prompt_delivered_to",      "type": "string"},
			{"name": "redacted_summary",         "type": "string"},
			{"name": "reflag_count",             "type": "string"},
			{"name": "reflag_reason",            "type": "string"},
			{"name": "reflagged_at",             "type": "string"},
			{"name": "reflagged_by",             "type": "string"},
			{"name": "pingback_count",           "type": "string"},
			{"name": "pingback_reason",          "type": "string"},
			{"name": "pingback_by",              "type": "string"},
			{"name": "pingback_source",          "type": "string"},
			{"name": "pingback_requested_at",    "type": "string"},
			{"name": "pingback_notified_at",     "type": "string"},
			{"name": "auto_pingback_count",      "type": "string"},
			{"name": "queue_alert_requested_at", "type": "string"},
			{"name": "queue_alert_notified_at",  "type": "string"},
			{"name": "last_digested_at",         "type": "string"},
			{"name": "notified_at",              "type": "string"},
			{"name": "operator_dm_at",           "type": "string"},
			{"name": "operator_dm_channel",      "type": "string"},
			{"name": "mr_landed",                "type": "string"},
			{"name": "mr_landed_state",          "type": "string"},
			{"name": "mr_landed_iid",            "type": "string"},
			{"name": "mr_landed_at",             "type": "string"},
			{"name": "mr_landed_url",            "type": "string"},
			{"name": "slack_channel",            "type": "string"},
			{"name": "slack_ts",                 "type": "string"},
			{"name": "slack_thread_ts",          "type": "string"},
			{"name": "thread_id",                "type": "string"},
			{"name": "gardener_delivery_gate_nudged_at", "type": "string"},
			{"name": "replay_in_progress",       "type": "boolean"},
			{"name": "sla_breached",             "type": "boolean"},
			{"name": "sla_base_priority",        "type": "string"},
			{"name": "idempotency_key",          "type": "string"}
		]
	}`)},

	// type:investigation-request — a low-privilege request to run a acme-scif
	// investigation against real (PHI-bearing) clinical data (acme-scif v1.1
	// Phase B; plan-of-record ka-0000002842 §1.2/§7). ANY agent may create one
	// (no min_role write_policy — creation is low-privilege), but a fresh
	// request is INERT: it does nothing until a human approves it through the
	// Phase D approval portal. project_scoped + escalatable mirror decision/gate.
	//
	// The invariant "creating does nothing until human-approved" is enforced at
	// create time by validateInvestigationRequestFields (beads.go): approval /
	// launch / egress fields are REJECTED on create, approval_status is forced
	// to "pending", and requested_by is server-stamped from the authenticated
	// identity so a requester cannot forge who asked. WHO may flip a request to
	// approved (tenant-scoped approver groups + four-eyes approver!=requester) is
	// Phase D's contract (D3, ka-0000005982) — NOT enforced here; B1 only
	// guarantees the request is born un-approved and inert.
	//
	// The full downstream lifecycle field superset is declared so the Phase B
	// launcher (B2, ka-0000001274), the fail-closed egress worker (C1,
	// ka-0000007911) and the approval portal (D1, ka-0000004280) can stamp their
	// state without tripping the unknown-field 400 (the recurring writer-vs-schema
	// drift class guarded by TestBuiltin*FieldsDeclared). Two gate-state fields
	// make a waiting human gate BEAD-VISIBLE so it can announce itself (epic
	// ka-0000007110): approval_status is Gate-1 (the RUN gate); release_status is
	// Gate-2 (the RELEASE gate) — "pending" the moment a finished run is awaiting
	// human release, then "released"/"denied" once the marker is written
	// (ka-0000002770). Both are born-inert (forbidden at create). Author fields:
	//   - prompt: the investigation question (required).
	//   - data_scope: JSON {tenant, db, tables:[...]} the request may read
	//     (required) — the SELECT-only surface the run is confined to.
	//   - origin_bead: the bead this investigation was spawned from (provenance).
	//   - requesting_agent: agent bead id of the creator (advisory, echoed).
	//   - requested_by: server-stamped authenticated identity (NEVER client-set).
	"type:investigation-request": {Key: "type:investigation-request", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"project_scoped": true, "escalatable": true},
		"fields": [
			{"name": "prompt",           "type": "string", "required": true},
			{"name": "data_scope",       "type": "json",   "required": true},
			{"name": "origin_bead",      "type": "string"},
			{"name": "requesting_agent", "type": "string"},
			{"name": "requested_by",     "type": "string"},
			{"name": "context",          "type": "string"},
			{"name": "approval_status",  "type": "enum", "values": ["pending", "approved", "denied"]},
			{"name": "approved_by",      "type": "string"},
			{"name": "approved_by_email","type": "string"},
			{"name": "approved_at",      "type": "string"},
			{"name": "approval_method",  "type": "enum", "values": ["web_ui", "api_token", "cli"]},
			{"name": "denied_by",        "type": "string"},
			{"name": "denied_at",        "type": "string"},
			{"name": "denial_reason",    "type": "string"},
			{"name": "run_id",           "type": "string"},
			{"name": "run_status",       "type": "enum", "values": ["queued", "running", "success", "failure"]},
			{"name": "launched_at",      "type": "string"},
			{"name": "launched_by",      "type": "string"},
			{"name": "confidence",       "type": "string"},
			{"name": "release_status",   "type": "enum", "values": ["pending", "released", "denied"]},
			{"name": "beads_doc_id",     "type": "string"},
			{"name": "redacted_summary", "type": "string"},
			{"name": "operated_by",      "type": "string"},
			{"name": "user_bead_id",     "type": "string"},
			{"name": "slack_user_id",    "type": "string"},
			{"name": "idempotency_key",  "type": "string"}
		]
	}`)},
	// type:scrub-request — a low-privilege request to scrub (detaint or purge)
	// a set of tainted beads (ADR docs/adr/taints.md §6.2, decision
	// gd-Swd7mnGf3L). Any grant-holder may create one; a fresh request is
	// INERT — it does nothing until it clears the taint's approval policy
	// (§6.3) and p2-scrub-execute runs it. project_scoped mirrors
	// decision/escalation/investigation-request; NOT escalatable — a create
	// denial here (missing grant, unreadable purge-scope bead) must not be
	// escalation-replayable, since that would let a requester route around
	// the grant/read checks that guard destructive execution.
	//
	// The invariant "creating does nothing until approved+executed" is
	// enforced at create time by applyScrubRequestGuard (scrub_request.go):
	// approval / execution / result fields are REJECTED on create,
	// requested_by is server-stamped from the authenticated identity, and
	// resolved_bead_ids is computed by evaluating scope exactly once and
	// frozen — it never changes even if the scope predicate would later
	// match a different set of beads. attestation is written later, only by
	// the dedicated POST /v1/scrub-requests/{id}/attest endpoint
	// (handleAttestScrubRequest, scrub_attestation.go, gd-eRKKim4OJc) — like
	// the approval/execution fields below, it is forbidden at create and on
	// plain PATCH (scrubRequestCreateForbiddenFields).
	//
	// Deliberately absent: any detaint_mode / mode field. The fork-vs-strip
	// choice lives solely on the named taint's own scrub_policy.detaint_mode,
	// set by the taint owner via `kd taint create/update --policy` — never by
	// the requester, who could otherwise downgrade fork to strip on their own
	// beads to dodge the "original stays tainted forever" guarantee. A create
	// carrying either field name is rejected as an unknown field by the
	// generic schema validator (no schema entry for it, and
	// allow_additional_fields is unset) — this is the whole enforcement
	// mechanism; there is no extra guard code for it.
	//
	// Author fields (settable at create):
	//   - taint: name of the taint this request concerns (required).
	//   - kind: "detaint" or "purge" (required).
	//   - scope: JSON {bead_ids:[...]} or {older_than_days:N, labels_any:[...]}
	//     (required) — evaluated once at create; see resolved_bead_ids.
	// Server-computed (client value, if any, is ignored/overwritten):
	//   - requested_by: authenticated identity of the creator.
	//   - resolved_bead_ids: the frozen bead-id list scope matched at create.
	// Forbidden at create AND on plain update (scrubRequestCreateForbiddenFields,
	// declared here so each privileged writer's own bypass write doesn't 400
	// "unknown field"):
	//   approval_status, approved_by, approved_at, approval_mode,
	//   approval_reason, executed_at, result, artifact_sha, attestation.
	"type:scrub-request": {Key: "type:scrub-request", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"project_scoped": true},
		"fields": [
			{"name": "taint",             "type": "string", "required": true},
			{"name": "kind",              "type": "enum",   "required": true, "values": ["detaint", "purge"]},
			{"name": "scope",             "type": "json",   "required": true},
			{"name": "requested_by",      "type": "string"},
			{"name": "resolved_bead_ids", "type": "string[]"},
			{"name": "attestation",       "type": "json"},
			{"name": "approval_status",   "type": "enum", "values": ["pending", "approved", "denied"]},
			{"name": "approved_by",       "type": "string"},
			{"name": "approved_at",       "type": "string"},
			{"name": "approval_mode",     "type": "enum", "values": ["auto", "human", "human-four-eyes"]},
			{"name": "approval_reason",   "type": "string"},
			{"name": "executed_at",       "type": "string"},
			{"name": "result",            "type": "json"},
			{"name": "artifact_sha",      "type": "string"},
			{"name": "scrub_execution_claimed", "type": "boolean"}
		]
	}`)},
	"type:session": {Key: "type:session", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "agent",                "type": "string"},
			{"name": "agent_id",             "type": "string"},
			{"name": "agent_bead_id",        "type": "string"},
			{"name": "account_name",         "type": "string"},
			{"name": "project",              "type": "string"},
			{"name": "role",                 "type": "string"},
			{"name": "hostname",             "type": "string"},
			{"name": "session_id",           "type": "string"},
			{"name": "session_log",          "type": "string"},
			{"name": "started_at",           "type": "string"},
			{"name": "ended_at",             "type": "string"},
			{"name": "exit_code",            "type": "integer"},
			{"name": "exit_reason",          "type": "string"},
			{"name": "resumed",              "type": "boolean"},
			{"name": "archived",             "type": "boolean"},
			{"name": "archive_path",         "type": "string"},
			{"name": "archive_url",          "type": "string"},
			{"name": "status",               "type": "string"},
			{"name": "restart_cause",        "type": "string"},
			{"name": "total_tokens",         "type": "integer"},
			{"name": "input_tokens",         "type": "integer"},
			{"name": "output_tokens",        "type": "integer"},
			{"name": "thinking_tokens",      "type": "integer"},
			{"name": "cache_read_tokens",    "type": "integer"},
			{"name": "cache_write_tokens",   "type": "integer"},
			{"name": "total_cost",           "type": "float"},
			{"name": "total_cost_usd",       "type": "float"},
			{"name": "settled_cost_usd",     "type": "float"},
			{"name": "request_count",        "type": "integer"},
			{"name": "tool_calls",           "type": "integer"},
			{"name": "tool_call_count",      "type": "integer"},
			{"name": "tool_errors",          "type": "integer"},
			{"name": "error_count",          "type": "integer"},
			{"name": "duration_seconds",     "type": "integer"},
			{"name": "duration_minutes",     "type": "integer"},
			{"name": "compactions",          "type": "integer"},
			{"name": "compaction_count",     "type": "integer"},
			{"name": "interrupt_count",      "type": "integer"},
			{"name": "last_interrupt_at",    "type": "string"},
			{"name": "transcript_count",     "type": "integer"},
			{"name": "recording_size",       "type": "integer"},
			{"name": "slowest_calls",        "type": "string"}
		]
	}`)},

	// type:trigger also carries the gardener loop-spec field set (name,
	// assign_bead, stop, progress, run_budget, max_runs, terminal_goal,
	// exit_condition, run_timeout, consecutive_failures, … below). A gardener is
	// a type=trigger bead carrying these fields: the trigger-engine decodes them
	// via GardenerSpec (gardenio/controller/internal/bridge/triggers.go) and
	// `gb gardener` / the gardener-loop runbook author and render them as flat
	// top-level fields. type:schedule already declares this set, and the
	// schedule-mirror source (trigger_source_schedule_mirror.go) copies between
	// the two gardener-carrying types, so type:trigger mirrors it. Without these,
	// a gardener bead with a structured stop/progress/assign_bead blob 400s on
	// create with "unknown field" (ka-0000002027) and the stop-firing terminate
	// path 400s when stamping gardener_terminated_slot/kind via CloseBead.
	// The `captain` entry is the pre-rename twin of `gardener`: nothing writes or
	// reads it any more (gd-5O9QUlA8zr phase 5), but it stays declared so
	// historical trigger beads that still carry the blob remain updatable.
	//
	// type:trigger also carries the flat source-match fields that the
	// account-cost and slack-reaction trigger SOURCES read off the bead at load
	// time — condition_type/field/op/threshold/account_filter for account-cost
	// (gardenio/controller/internal/bridge/trigger_source_account.go) and
	// emoji/match/channel_filter for slack-reaction (trigger_source_slack_
	// reaction.go). Both sources are wired into the deployed trigger-engine
	// (cmd/trigger-engine/main.go NewAccountTriggerSource), so without these
	// declared such a trigger is live-but-unauthorable: the create/update path
	// 400s "condition_type: unknown field" and no operator or agent can author
	// an account-cost or slack-reaction trigger (ka-0000001277). threshold is
	// strconv.ParseFloat'd by the account source, so it is a float like
	// type:account's total_cost_usd; the rest are read as raw strings.
	"type:trigger": {Key: "type:trigger", Value: json.RawMessage(`{
		"kind": "data",
		"write_policy": {"project_scoped": true, "escalatable": true},
		"fields": [
			{"name": "source",           "type": "enum",    "values": ["jira", "gitlab", "webhook", "bead-state", "schedule", "slack-reaction", "account-cost", "schedule-mirror"]},
			{"name": "jql",              "type": "string"},
			{"name": "jira_project",     "type": "string"},
			{"name": "project_path",     "type": "string"},
			{"name": "event_type",       "type": "enum",    "values": ["merge_request", "pipeline"]},
			{"name": "state",            "type": "string"},
			{"name": "gitlab_labels",    "type": "string"},
			{"name": "author",           "type": "string"},
			{"name": "target_branch",    "type": "string"},
			{"name": "pipeline_ref",         "type": "string"},
			{"name": "pipeline_ref_pattern", "type": "string"},
			{"name": "pipeline_status",      "type": "string"},
			{"name": "pipeline_max_age",     "type": "string"},
			{"name": "min_duration",         "type": "string"},
			{"name": "ignore_failed_statuses", "type": "string"},
			{"name": "webhook_token",    "type": "string"},
			{"name": "webhook_secret",   "type": "string"},
			{"name": "conditions",       "type": "json"},
			{"name": "alert_route",      "type": "string"},
			{"name": "allow_unsigned",   "type": "string"},
			{"name": "event_types",      "type": "string"},
			{"name": "assert",           "type": "json"},
			{"name": "on_violation",     "type": "string"},
			{"name": "condition_type",   "type": "string"},
			{"name": "field",            "type": "string"},
			{"name": "op",               "type": "string"},
			{"name": "threshold",        "type": "float"},
			{"name": "account_filter",   "type": "string"},
			{"name": "emoji",            "type": "string"},
			{"name": "match",            "type": "string"},
			{"name": "channel_filter",   "type": "string"},
			{"name": "cron",             "type": "string"},
			{"name": "timezone",         "type": "string"},
			{"name": "action",           "type": "enum",    "required": true, "values": ["spawn_agent", "apply_formula", "notify", "create_bead", "assert", "wake_pr_owner"]},
			{"name": "role",             "type": "string"},
			{"name": "project",          "type": "string"},
			{"name": "prompt",           "type": "string"},
			{"name": "bead_type",        "type": "string"},
			{"name": "bead_priority",    "type": "integer"},
			{"name": "parent",           "type": "string"},
			{"name": "extra_labels",     "type": "string"},
			{"name": "formula_id",       "type": "string"},
			{"name": "formula_vars",     "type": "string"},
			{"name": "cross_pour_upstream_step", "type": "string"},
			{"name": "enabled",          "type": "boolean"},
			{"name": "max_concurrent",   "type": "integer"},
			{"name": "max_concurrent_runs", "type": "integer"},
			{"name": "cooldown_minutes", "type": "integer"},
			{"name": "last_checked",     "type": "string", "service_owned_by": ["*"]},
			{"name": "last_run",         "type": "string", "service_owned_by": ["*"]},
			{"name": "last_match",       "type": "string", "service_owned_by": ["*"]},
			{"name": "last_outcome",     "type": "string", "service_owned_by": ["*"]},
			{"name": "last_outcome_at",  "type": "string", "service_owned_by": ["*"]},
			{"name": "last_fire_error",    "type": "string", "service_owned_by": ["*"]},
			{"name": "last_fire_error_at", "type": "string", "service_owned_by": ["*"]},
			{"name": "allowed_projects", "type": "string"},
			{"name": "allowed_roles",    "type": "string"},
			{"name": "blocked_by",       "type": "string"},
			{"name": "triggers",         "type": "json"},
			{"name": "timed_cron",       "type": "string"},
			{"name": "gardener",         "type": "json"},
			{"name": "captain",          "type": "json"},
			{"name": "chain",            "type": "json"},
			{"name": "schedule_id",      "type": "string"},
			{"name": "last_agent_id",    "type": "string", "service_owned_by": ["*"]},
			{"name": "closed_reason",    "type": "string"},
			{"name": "run_timeout",          "type": "string"},
			{"name": "max_retries",          "type": "integer"},
			{"name": "consecutive_failures", "type": "integer"},
			{"name": "retry_on_failure",     "type": "string"},

			{"name": "name",                 "type": "string"},
			{"name": "assign_bead",          "type": "string"},
			{"name": "terminal_goal",        "type": "string"},
			{"name": "exit_condition",       "type": "string"},
			{"name": "on_exit",              "type": "string"},
			{"name": "run_budget",           "type": "integer"},
			{"name": "max_runs",             "type": "integer"},
			{"name": "runs_spawned",         "type": "integer"},
			{"name": "runs_completed",       "type": "integer"},
			{"name": "max_second_lives",     "type": "integer"},
			{"name": "second_life_count",    "type": "integer"},
			{"name": "timeout",              "type": "string"},
			{"name": "trigger_mode",         "type": "string"},
			{"name": "stop",                 "type": "json"},
			{"name": "progress",             "type": "json"},
			{"name": "progress_spec",        "type": "json"},
			{"name": "progress_state",       "type": "json"},
			{"name": "run_log",              "type": "json"},
			{"name": "trigger_state",        "type": "json"},
			{"name": "agent_state",          "type": "string"},
			{"name": "close_reason",         "type": "string"},
			{"name": "last_checked_agent",   "type": "string"},
			{"name": "gardener_terminated_slot", "type": "string"},
			{"name": "gardener_terminated_kind", "type": "string"},
			{"name": "gardener_stuck_escalated", "type": "string"},
			{"name": "gardener_spend_usd",       "type": "string"},
			{"name": "gardener_spend_synced_at", "type": "string"},
			{"name": "force_close",          "type": "string"},
			{"name": "force_close_reason",   "type": "string"},
			{"name": "force_close_set_at",   "type": "string"},
			{"name": "force_close_set_by",   "type": "string"},
			{"name": "wrapup",               "type": "string"},
			{"name": "slack_channel",        "type": "string"},
			{"name": "slack_user_id",        "type": "string"},
			{"name": "user_bead_id",         "type": "string"},
			{"name": "operated_by",          "type": "string"},

			{"name": "mirror_fire_key",      "type": "string"},
			{"name": "mirror_entry_idx",     "type": "string"},
			{"name": "mirror_entry_kind",    "type": "string"},
			{"name": "mirror_entry_label",   "type": "string"},
			{"name": "mirror_cause",         "type": "string"},
			{"name": "mirror_fire_time",     "type": "string"},
			{"name": "source_schedule_id",   "type": "string"},

			{"name": "execution_target",     "type": "string"},

			{"name": "wake_intent",          "type": "string"},

			{"name": "awaiting",             "type": "string"},
			{"name": "wake_on",              "type": "string"},

			{"name": "drain_reviewed_at",    "type": "string"}
		]
	}`)},

	"type:schedule": {Key: "type:schedule", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"project_scoped": true, "escalatable": true},
		"fields": [
			{"name": "name",                   "type": "string"},
			{"name": "cron",                   "type": "string"},
			{"name": "project",                "type": "string",  "required": true},
			{"name": "role",                   "type": "string"},
			{"name": "prompt",                 "type": "string",  "required": true},
			{"name": "enabled",                "type": "boolean"},
			{"name": "timezone",               "type": "string"},
			{"name": "slack_channel",          "type": "string"},
			{"name": "slack_user_id",          "type": "string"},
			{"name": "assign_bead",            "type": "string"},
			{"name": "last_run",               "type": "string",  "service_owned_by": ["*"]},
			{"name": "last_agent_id",          "type": "string",  "service_owned_by": ["*"]},
			{"name": "timeout",                "type": "string"},
			{"name": "max_concurrent",         "type": "integer"},
			{"name": "max_retries",            "type": "integer"},
			{"name": "retry_on_failure",       "type": "string"},
			{"name": "consecutive_failures",   "type": "integer", "service_owned_by": ["*"]},
			{"name": "last_checked_agent",     "type": "string",  "service_owned_by": ["*"]},
			{"name": "max_runs",               "type": "integer"},
			{"name": "run_budget",             "type": "integer"},
			{"name": "runs_spawned",           "type": "integer", "service_owned_by": ["*"]},
			{"name": "runs_completed",         "type": "integer", "service_owned_by": ["*"]},
			{"name": "max_second_lives",       "type": "integer"},
			{"name": "second_life_count",      "type": "integer"},
			{"name": "terminal_goal",          "type": "string"},
			{"name": "exit_condition",         "type": "string"},
			{"name": "on_exit",                "type": "string"},
			{"name": "run_timeout",            "type": "string"},
			{"name": "triggers",               "type": "json"},
			{"name": "timed_cron",             "type": "string"},
			{"name": "stop",                   "type": "json"},
			{"name": "progress",               "type": "json"},
			{"name": "progress_spec",          "type": "json"},
			{"name": "progress_state",         "type": "json",    "service_owned_by": ["*"]},
			{"name": "run_log",                "type": "json",    "service_owned_by": ["*"]},
			{"name": "trigger_state",          "type": "json",    "service_owned_by": ["*"]},
			{"name": "trigger_mode",           "type": "string"},
			{"name": "watch_bead_id",          "type": "string"},
			{"name": "watch_channel",          "type": "string"},
			{"name": "watch_from",             "type": "string"},
			{"name": "watch_labels",           "type": "string"},
			{"name": "watch_project",          "type": "string"},
			{"name": "watch_role",             "type": "string"},
			{"name": "watch_types",            "type": "string"},
			{"name": "agent_state",            "type": "string"},
			{"name": "close_reason",           "type": "string"},
			{"name": "gardener_terminated_slot", "type": "string"},
			{"name": "gardener_terminated_kind", "type": "string"},
			{"name": "gardener_stuck_escalated", "type": "string"},
			{"name": "gardener_spend_usd",       "type": "string"},
			{"name": "gardener_spend_synced_at", "type": "string"},
			{"name": "awaiting",               "type": "string"},
			{"name": "wake_on",                "type": "string"},
			{"name": "operated_by",            "type": "string"},
			{"name": "user_bead_id",           "type": "string"}
		]
	}`)},

	// Observability bridge types (epic ka-0000006776). grafana-bridge (and,
	// until its 2026-10-31 deletion, alertmanager-bridge) author these via
	// EnsureConfigs, which only writes a config *bead* — but the type
	// resolver consults builtins and the configs KV table, never config
	// beads (ka-0000001549). So a genuinely-new type
	// like dashboard 400s "unknown bead type" on every create. Defining them
	// here as authoritative builtins — matching the bridge's TypeConfig in
	// controller/internal/bridge/init.go — closes that gap; the bridge's
	// config-bead write becomes redundant for these but stays harmless.

	// Dashboard: a Grafana dashboard authored/versioned as a bead and synced
	// to Grafana by grafana-bridge (W2). The large JSON model lives in an
	// attached type=doc bundle artifact; only metadata lives in fields.
	"type:dashboard": {Key: "type:dashboard", Value: json.RawMessage(`{
		"kind": "issue",
		"fields": [
			{"name": "dashboard_uid",               "type": "string"},
			{"name": "dashboard_folder",            "type": "string"},
			{"name": "dashboard_version",           "type": "string"},
			{"name": "dashboard_hash",              "type": "string"},
			{"name": "dashboard_source",            "type": "enum", "values": ["beads", "grafana"]},
			{"name": "dashboard_configmap_key",     "type": "string"},
			{"name": "dashboard_conflict_critical", "type": "boolean"}
		]
	}`)},

	// Alert rule: a vmalert alerting/recording rule authored as a bead (W3).
	// Its renderer, alertmanager-bridge, was deleted 2026-10-31; the type
	// stays so existing alert_rule beads remain valid.
	"type:alert_rule": {Key: "type:alert_rule", Value: json.RawMessage(`{
		"kind": "issue",
		"fields": [
			{"name": "rule_kind",        "type": "enum", "values": ["alerting", "recording"]},
			{"name": "rule_expr",        "type": "string"},
			{"name": "rule_for",         "type": "string"},
			{"name": "rule_severity",    "type": "enum", "values": ["page", "warn", "info"]},
			{"name": "rule_group",       "type": "string"},
			{"name": "rule_target",      "type": "string"},
			{"name": "rule_labels",      "type": "string"},
			{"name": "rule_annotations", "type": "string"}
		]
	}`)},

	// Alert mute: a bead-side silence suppressing bead-creation for a noisy
	// firing alert without touching Alertmanager (W3 §5b). Its reader,
	// alertmanager-bridge, was deleted 2026-10-31; the type stays so existing
	// alert_mute beads remain valid.
	"type:alert_mute": {Key: "type:alert_mute", Value: json.RawMessage(`{
		"kind": "issue",
		"fields": [
			{"name": "mute_matchers", "type": "string"},
			{"name": "mute_until",    "type": "timestamp"},
			{"name": "mute_reason",   "type": "string"}
		]
	}`)},
}

var builtinConfigsByNamespace = func() map[string][]*model.Config {
	m := map[string][]*model.Config{}
	for key, cfg := range builtinConfigs {
		if i := strings.Index(key, ":"); i > 0 {
			ns := key[:i]
			m[ns] = append(m[ns], cfg)
		}
	}
	return m
}()

// resolveTypeConfig looks up the type config for a bead type, first from the
// store, then from builtin defaults. When both exist, builtin fields that are
// missing from the store config are merged in so that new fields added to
// builtins are recognised even if the store config was written by an older
// version of the code.
func (s *BeadsServer) resolveTypeConfig(ctx context.Context, beadType model.BeadType) (*model.TypeConfig, error) {
	key := "type:" + string(beadType)

	// Try user-defined config in the store first.
	storeConfig, err := s.getTypeConfigRow(ctx, key)
	if err != nil {
		return nil, err
	}

	builtin := builtinConfigs[key]

	if storeConfig == nil && builtin == nil {
		return nil, nil
	}

	// Only builtin exists.
	if storeConfig == nil {
		var tc model.TypeConfig
		if err := json.Unmarshal(builtin.Value, &tc); err != nil {
			return nil, err
		}
		tc.TypeName = string(beadType)
		addUniversalFields(&tc)
		return &tc, nil
	}

	var tc model.TypeConfig
	if err := json.Unmarshal(storeConfig.Value, &tc); err != nil {
		return nil, err
	}
	// Stamp the concrete type name (not part of the persisted schema JSON) so
	// field-validation errors can route the author to `kd fields <type>`
	// (ka-0000000365).
	tc.TypeName = string(beadType)

	// Merge builtin fields and write policy that are missing from the store config.
	//
	// For fields present in BOTH store and builtin, union the enum values so
	// new builtin enum members (e.g. a new `source` value added in a daemon
	// release) propagate to a previously-persisted user/snapshot config
	// without requiring a manual SetConfig roll-forward. The user's
	// existing values are preserved — union only grows the accept-list.
	// Without this, a stale store row for an authoritative builtin type
	// (e.g. type:trigger) pins the enum to the old value set and silently
	// 400s every write that uses the new value (ka-0000003824).
	if builtin != nil {
		var btc model.TypeConfig
		if err := json.Unmarshal(builtin.Value, &btc); err == nil {
			builtinByName := make(map[string]model.FieldDef, len(btc.Fields))
			for _, f := range btc.Fields {
				builtinByName[f.Name] = f
			}
			existing := make(map[string]bool, len(tc.Fields))
			for i, f := range tc.Fields {
				existing[f.Name] = true
				// For enum / enum[] fields, union the store's enum values with the
				// builtin's so newly-added builtin enum values are recognised even
				// if the store config predates them. The store config is authoritative
				// for everything else (Required, Min, Max, Type) — this only widens
				// the allowed-values set for fields the daemon already owns.
				bf, ok := builtinByName[f.Name]
				if !ok {
					continue
				}
				if f.Type != bf.Type {
					continue
				}
				if f.Type != model.FieldTypeEnum && f.Type != model.FieldTypeEnums {
					continue
				}
				tc.Fields[i].Values = unionEnumValues(f.Values, bf.Values)
			}
			for _, f := range btc.Fields {
				if !existing[f.Name] {
					tc.Fields = append(tc.Fields, f)
				}
			}
			// Merge write policy from builtin if store config has none.
			if tc.WritePolicy == nil && btc.WritePolicy != nil {
				tc.WritePolicy = btc.WritePolicy
			} else if tc.WritePolicy != nil && btc.WritePolicy != nil && minRoleFloorTypes[key] &&
				btc.WritePolicy.MinRole != "" && !roleAtLeast(tc.WritePolicy.MinRole, btc.WritePolicy.MinRole) {
				// The builtin min_role is a FLOOR for identity-bearing types:
				// a stored policy may raise it but not lower or blank it
				// (gd-32dXqWb95Y).
				tc.WritePolicy.MinRole = btc.WritePolicy.MinRole
			}
			// Restore AllowAdditionalFields from the builtin when the store
			// row leaves it at the false zero value but the builtin declares
			// the type open-schema. Like the enum-union and write-policy
			// merges above this only WIDENS the accept-set — it never turns a
			// builtin-open type closed. Without it, a stale/minimal store row
			// for an authoritative open-schema builtin (e.g. type:config,
			// whose prod row predated allow_additional_fields) pins the type
			// to reject-unknown-fields and silently 400s every `kd -f key=val`
			// on a bead of that type — the config-bead runtime-tunable path
			// went dead on prod for any NEW key this way (ka-0000002372).
			if !tc.AllowAdditionalFields && btc.AllowAdditionalFields {
				tc.AllowAdditionalFields = true
			}
		}
	}

	addUniversalFields(&tc)
	return &tc, nil
}

// universalFields are declared on every bead type that has a type config:
// they describe the bead itself rather than any one type.
//
// icon_svg_url and icon_png_url are the viewer URLs of the bead's badge, the
// picture of its arms, as an SVG and a PNG. `gb badge <id> --upload` draws it
// for a bead of any type (plan gd-pQZO9ie11D) and writes both back
// (gd-NyYxIxxbuw), so they cannot live on type:epic alone, where icon_svg_url
// was first declared.
//
// impersonated_by is the admin behind the last impersonated session that wrote
// the bead, and created_impersonated_by the one behind the session that
// created it. Only the daemon writes them (impersonation.go), on a bead of any
// type.
var universalFields = []model.FieldDef{
	{Name: "icon_svg_url", Type: model.FieldTypeString},
	{Name: "icon_png_url", Type: model.FieldTypeString},
	{Name: impersonatedByField, Type: model.FieldTypeString},
	{Name: createdImpersonatedByField, Type: model.FieldTypeString},
}

// addUniversalFields appends each universal field tc does not already declare.
// Like the builtin merge above it only widens the accept-set.
func addUniversalFields(tc *model.TypeConfig) {
	for _, u := range universalFields {
		declared := false
		for _, f := range tc.Fields {
			if f.Name == u.Name {
				declared = true
				break
			}
		}
		if !declared {
			tc.Fields = append(tc.Fields, u)
		}
	}
}

// minRoleFloorTypes are the type:* configs whose builtin write_policy.min_role
// cannot be lowered by a stored override. type:user is the one that gates
// identity fields (slack_id, email_aliases, allowed_roles, ...): a stored row
// without min_role would otherwise let any signed-in caller write them. The
// Lands credit ledger types are floored too: a lowered min_role there would
// let a non-admin mint credit (lands_ledger.go). type:permission is floored so
// the controller's seed of it (no write_policy) or a later stored row cannot
// reopen verdict records to agents (gd-RuI4Ze6vAg).
var minRoleFloorTypes = map[string]bool{"type:user": true, "type:credit_grant": true, "type:credit_rate": true, "type:permission": true}

// roleAtLeast reports whether role ranks at or above floor. An empty or
// unknown role ranks below every known floor.
func roleAtLeast(role, floor string) bool {
	return hasMinimumRole([]string{role}, floor)
}

// unionEnumValues returns the union of a and b preserving the order of a
// (with new values from b appended). Used by resolveTypeConfig to merge
// enum members added in a builtin into a previously-persisted store
// config that lacks them.
func unionEnumValues(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, v := range a {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// SetConfig creates or updates a config entry.
func (s *BeadsServer) SetConfig(ctx context.Context, req *beadsv1.SetConfigRequest) (*beadsv1.SetConfigResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	config := &model.Config{
		Key:   req.GetKey(),
		Value: json.RawMessage(req.GetValue()),
	}

	if denial := s.authorizeSetConfig(ctx, config); denial != nil {
		return nil, denial
	}

	err := s.store.SetConfig(ctx, config)
	s.typeConfigs.invalidate(config.Key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to set config: %v", err)
	}

	return &beadsv1.SetConfigResponse{Config: configToProto(config)}, nil
}

// GetConfig retrieves a config by key.
func (s *BeadsServer) GetConfig(ctx context.Context, req *beadsv1.GetConfigRequest) (*beadsv1.GetConfigResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}
	if denial := authorizeGetConfigKey(ctx, req.GetKey()); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Message)
	}

	config, err := s.store.GetConfig(ctx, req.GetKey())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if builtin, ok := builtinConfigs[req.GetKey()]; ok {
				return &beadsv1.GetConfigResponse{Config: configToProto(builtin)}, nil
			}
		}
		return nil, storeError(err, "config")
	}

	return &beadsv1.GetConfigResponse{Config: configToProto(config)}, nil
}

// listConfigsWithBuiltins fetches configs from the store and merges in builtin
// defaults that haven't been overridden, minus the sensitive rows the caller
// may not read (readableConfigs).
func (s *BeadsServer) listConfigsWithBuiltins(ctx context.Context, namespace string) ([]*model.Config, error) {
	configs, err := s.store.ListConfigs(ctx, namespace)
	if err != nil {
		return nil, err
	}

	stored := make(map[string]struct{}, len(configs))
	for _, c := range configs {
		stored[c.Key] = struct{}{}
	}
	for _, b := range builtinConfigsByNamespace[namespace] {
		if _, ok := stored[b.Key]; !ok {
			configs = append(configs, b)
		}
	}

	return readableConfigs(ctx, configs), nil
}

// ListConfigs returns configs matching a namespace prefix, merging in any
// builtin defaults that haven't been overridden by user-defined configs.
func (s *BeadsServer) ListConfigs(ctx context.Context, req *beadsv1.ListConfigsRequest) (*beadsv1.ListConfigsResponse, error) {
	if req.GetNamespace() == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required")
	}
	if denial := authorizeListConfigNamespace(ctx, req.GetNamespace()); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Message)
	}

	configs, err := s.listConfigsWithBuiltins(ctx, req.GetNamespace())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list configs: %v", err)
	}

	pbConfigs := make([]*beadsv1.Config, 0, len(configs))
	for _, c := range configs {
		pbConfigs = append(pbConfigs, configToProto(c))
	}

	return &beadsv1.ListConfigsResponse{Configs: pbConfigs}, nil
}

// DeleteConfig removes a config by key.
func (s *BeadsServer) DeleteConfig(ctx context.Context, req *beadsv1.DeleteConfigRequest) (*beadsv1.DeleteConfigResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	err := s.store.DeleteConfig(ctx, req.GetKey())
	s.typeConfigs.invalidate(req.GetKey())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "config not found")
		}
		return nil, status.Errorf(codes.Internal, "failed to delete config: %v", err)
	}

	return &beadsv1.DeleteConfigResponse{}, nil
}
