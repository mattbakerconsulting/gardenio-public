package server

import (
	"context"
	"strings"

	"gardenio/kbeads/internal/model"
)

// machineCapability is what one gardenio-machine-* realm role grants a machine
// principal. Roles are capabilities, not the linear viewer<operator<admin
// ladder, so they compose without ever implying admin.
type machineCapability struct {
	// Writes are the (action, type) pairs this role may write on admin-gated
	// types (consulted by authorizeWriteTitled for authMethodMachine only).
	Writes []machineWriteAllowKey
	// TokenMint grants POST/GET/DELETE /v1/bridge-tokens and /v1/fleet-tokens.
	TokenMint bool
	// Ownership lists the non-config bead types the principal may update or
	// close without being their created_by/assignee/owner (authorizeOwnership).
	// It is TYPED on purpose: the machine principals replaced the retired
	// static token's blanket bypass, and decision gd-ZsfxvXRiyv ("tight") refused a blanket
	// system-graph capability. Empty = ownership applies exactly as for a fleet
	// token. Config-kind beads never reach the ownership gate.
	Ownership []model.BeadType
	// ReclaimOwnership lists the issue types the principal may UPDATE (never
	// close or delete) without owning them, and only while the bead's current
	// assignee is a gone agent: no '@' (a human), no active type=agent bead of
	// that name, and a closed one (the assignee_gone_agent condition,
	// assigneeIsGoneAgent). It is the stale
	// reaper's claim release on a hard-killed crew's work, narrower than
	// Ownership on purpose (decision gd-mM3IFDGoTq "narrow").
	ReclaimOwnership []model.BeadType
	// DispatchClaim lists the issue types the principal may claim (status
	// in_progress, assignee) while open and unassigned, for the live agent
	// whose single-claim spawn names the bead as its task_id, in the bead's
	// project, and nothing else (machineMayClaimDispatchedTask, gd-GrJRP7kLBY,
	// decision gd-pjrfJOLdiY "narrow").
	DispatchClaim []model.BeadType
	// FieldOwnership lets the principal UPDATE (never close or delete) a bead
	// of the listed type it does not own when the patch changes nothing but
	// the listed fields: no column, no other key (patchOnlyChangesFields).
	// It is for bookkeeping figures one machine computes and stamps onto
	// beads other principals filed, where type-wide Ownership would let it
	// rewrite the bead itself (machineMayStampFields, gd-Hk70sS1XuS).
	FieldOwnership []machineFieldOwnership
	// ConfigTitles lists the exact config-bead titles the principal may
	// create/update (authorizeWriteTitled, beadType config). Config is admin-gated
	// and holds agent prompts and hooks, so the grant is typed to the titles
	// the one writer actually seeds, not to the whole type. An entry ending in
	// "*" is a prefix. Empty = no config writes.
	ConfigTitles []string
	// LabeledOwnership is Ownership narrowed to beads carrying a label, and to
	// the listed actions (authorizeOwnership). Used where a bead type is shared
	// with other writers (doc) and only the principal's own artifacts qualify.
	LabeledOwnership []machineLabeledOwnership
	// TriggerLineage lets the resolver attribute the principal's CREATE of a
	// bead that references a trigger/schedule bead (fields.trigger_id) to that
	// bead's human author, dereferenced server-side (resolveOperatedByPure,
	// machine case; decision gd-dI7PfsFlvr "lineage"). It honours no header.
	// Only the component that fires triggers needs it: any other machine
	// could otherwise name a human-authored trigger to have its own creates
	// attributed to that human.
	TriggerLineage bool
	// LandPurge admits the principal to POST /v1/lands/purge
	// (http_lands_purge.go): hard-delete a closed tenant land's beads, or a
	// denied/withdrawn plot application, after the retention period.
	LandPurge bool
	// PlanScope lets the principal update and close a plan's own beads, and
	// update the plan's gardener, through the plan-scope rule
	// (authorizePlanReactor in plan_scope_authz.go), and lets its enable of
	// that gardener be sealed from the owner's approval
	// (spawn_lineage_plan_approval.go). It is the beads-viewer's plan answer
	// reactor, which applies the owner's answer to a plan decision.
	PlanScope bool
	// EscalationExecutor lets the principal update and close (so label and
	// link) an executor-type bead it does not own, only while it holds the
	// claim on an open, approved, bead-only escalation executed by a
	// coordinator lane it is a member of, whose approved_targets lists the
	// bead (escalationExecutorGrant, escalation routing Task 15 C4).
	EscalationExecutor bool
}

// machineLabeledOwnership grants ownership of beads of one type that carry
// Label, for the listed actions only (e.g. update but not close).
type machineLabeledOwnership struct {
	BeadType model.BeadType
	Label    string
	Actions  []string
}

// machineFieldOwnership grants updates to Fields on beads of BeadType, and
// nothing else on them.
type machineFieldOwnership struct {
	BeadType model.BeadType
	Fields   []string
}

// machinePlanSpendFields are the plan-epic fields the controller's plan spend
// sync stamps (gardenio/controller/internal/planspend: FieldUSD,
// FieldSyncedAt, FieldCrew, FieldLive; declared on type:epic in config.go).
// KEEP IN SYNC with planSpendSync.syncAll: the grant is all-or-nothing, so one
// unlisted key in that patch sends the whole write back to the ownership gate.
var machinePlanSpendFields = []string{
	"plan_spend_usd",
	"plan_spend_synced_at",
	"plan_spend_crew",
	"plan_spend_live",
}

// machineGitHubBridgeWorkFields are the fields github-bridge stamps on a PR's
// work beads (gardenio/controller/cmd/github-bridge): mr_url and forge
// (pullrequest.go linkTask), ci_state, tests_touched and goldens_touched (the
// merge harvest), mr_has_review_comments (review.go), the resolver_* stamps
// (resolver.go), and the merge state, conflict and overlap fields
// (conflicts.go, overlaps.go). KEEP IN SYNC with those writers: the grant is
// all-or-nothing, so one unlisted key in a patch sends the whole write back to
// the ownership gate. TestGitHubBridgeWorkFields_MatchGrant in
// cmd/github-bridge pins the bridge's side.
var machineGitHubBridgeWorkFields = []string{
	"mr_url",
	"forge",
	"ci_state",
	"tests_touched",
	"goldens_touched",
	"mr_has_review_comments",
	"resolver_spawned_at",
	"resolver_spawn_count",
	"resolver_threads_hash",
	"resolver_gave_up_hash",
	"mr_merge_state",
	"mr_conflict_files",
	"mr_conflict_kind",
	"mr_conflict_since",
	"mr_conflict_base",
	"mr_conflict_cause",
	"mr_overlaps",
}

// fieldOwnershipOn grants the same fields on each of types.
func fieldOwnershipOn(fields []string, types ...model.BeadType) []machineFieldOwnership {
	out := make([]machineFieldOwnership, 0, len(types))
	for _, t := range types {
		out = append(out, machineFieldOwnership{BeadType: t, Fields: fields})
	}
	return out
}

// capTokenMint names the token-mint capability for machineHasCapability.
const capTokenMint = "token_mint"

// capTriggerLineage names the trigger-lineage attribution capability for
// machineHasCapability (machineCapability.TriggerLineage).
const capTriggerLineage = "trigger_lineage"

// capLandPurge names the land-purge capability for machineHasCapability
// (machineCapability.LandPurge).
const capLandPurge = "land_purge"

// capPlanScope names the plan-scope capability for machineHasCapability
// (machineCapability.PlanScope).
const capPlanScope = "plan_scope"

// machineCapabilityGrants maps a gardenio-machine-* realm role to its grants.
// Every entry cites the call sites that need it (same review bar as
// machineAllowedWrites). A role that is not in the table grants nothing.
// testdata/machine_grants.golden.json mirrors this table; the chart's tests
// cross-check it.
var machineCapabilityGrants = map[string]machineCapability{
	// controller: spend_sync.go:224, ratelimit_probe.go:699,767,835,
	// auth_failover.go:238,456 (gardenio/controller/cmd/controller).
	"gardenio-machine-accounts": {Writes: []machineWriteAllowKey{{Action: "update", BeadType: "account"}}},
	// controller: bridgetokenrotator (internal/bridgetokenrotator/rotator.go:348),
	// agenttoken.go:102, microvm_credentials.go:67 — design §7 option 1.
	"gardenio-machine-token-mint": {TokenMint: true},
	// beads-viewer background reactors (gardenio/controller/cmd/beads-viewer):
	// plan_step_reactor.go:366 → shouldNudgePlanAssistant stamps
	// fields.last_nudge_at on the PLAN EPIC (plans.go:4963, UpdateBeadFieldsTyped),
	// an epic a human or agent created. plan_label_reactor.go:391 creates the
	// gardener trigger (create: no ownership gate). The viewer's human-driven
	// writes carry the signed-in user's own bearer and never use this role
	// (decision gd-ZsfxvXRiyv: beads-viewer = split, ownership typed to the
	// reactors' bead types).
	// PlanScope: plan_answer_reactor.go applies the owner's answer to a plan
	// decision (steward.AnswerPlan, plangardener.Start): plan_state on the plan,
	// the close of the plan and its open steps on change / drop, and the enable
	// of the plan's gardener on approve (gd-Dl88Htipuq, owner decision
	// gd-fBSlZfEvmo item 2). Bounded to plans by authorizePlanReactor.
	"gardenio-machine-plan-reactor": {Ownership: []model.BeadType{model.TypeEpic}, PlanScope: true},
	// beads-viewer Lands Gate (Track B spec §2.4-2.5, step 7.7;
	// cmd/beads-viewer lands_apply.go). plot_application is admin-gated and the
	// applicant may never write one on their own bearer (B-D4), so the Gate
	// files it as the viewer's machine principal: create in handleLandsApply,
	// update in the same handler (decision_id, decision_digest), in the withdraw handler (state
	// withdrawn) and in the apply reactor (lands_apply_reactor.go: the state,
	// decision and ask fields from a resolved decision). The viewer created every
	// application it updates, so no ownership grant is needed. The approval
	// decision itself is a project-scoped type the viewer's operator role
	// already files.
	//
	// The same principal files and revokes member invites (step 8.9,
	// cmd/beads-viewer lands_invites.go): land_invite is admin-gated so an
	// owner never names a github_id the viewer did not resolve, and the viewer
	// writes it only after its own owner check (member_manage). Create in
	// landInvites.invite, update (revoke_requested_at/_by) in landInvites.revoke.
	// The viewer created every invite it revokes, so no ownership grant.
	//
	// The gatehouse reactor (task gd-K62coPtKkX, cmd/beads-viewer
	// lands_gatehouse_reactor.go) answers the gatehouse steward's requests:
	// update gatehouse_request (state, outcome, result_id, github_id,
	// github_login, handled_at). The steward filed them, so the update needs
	// typed ownership as well as the write. The demo pass it files is the
	// land_invite create above.
	"gardenio-machine-lands-gate": {
		Writes: []machineWriteAllowKey{
			{Action: "create", BeadType: typePlotApplication},
			{Action: "update", BeadType: typePlotApplication},
			{Action: "create", BeadType: typeLandInvite},
			{Action: "update", BeadType: typeLandInvite},
			{Action: "update", BeadType: typeGatehouseRequest},
		},
		Ownership: []model.BeadType{typeGatehouseRequest},
	},
	// controller Lands provisioner (Track B spec §3.3, step 8.6;
	// gardenio/controller/internal/lands/provision). It turns an approved
	// plot_application into a land and, on gb lands close, takes it apart:
	//   - plot_application: update provision_step / close_step, the state and
	//     the failure fields (provision.go, close.go). The Gate's machine
	//     principal created every application, so the update needs typed
	//     ownership as well as the write.
	//   - project: create the land's project bead, update its land fields
	//     (account, land_opened_at, land_default_channel, land_frozen on
	//     close) and close it with land_closed_at.
	//   - account: create lands-<land>, write its litellm_key, and close it
	//     when the key is revoked.
	//   - credit_grant: the initial grant from the approval (create only; the
	//     ledger is append-only, lands_ledger.go).
	//   - land_invite: the invite pass (invites.go, step 8.9) moves an invite
	//     to joined, expired or revoked and closes the ended ones. The Gate's
	//     machine principal created every invite, so the update and close need
	//     typed ownership as well as the write.
	//   - invite: the sign-in invite pass (email_invites.go, gd-ycE484kLyk)
	//     stamps mailed_at, moves an invite to claimed, expired or revoked and
	//     closes it. An admin created every invite, so the update and close
	//     need typed ownership as well as the write.
	// Channels and escalations are project-scoped types the controller's
	// operator role already writes; agent_state=stopping on the land's agents
	// is the same agent-bead write the purse watcher makes.
	//   - LandPurge: the purge sweep (internal/lands/purge, step 8.11) calls
	//     POST /v1/lands/purge for a land closed longer than the retention
	//     period and for old denied/withdrawn applications, then stamps
	//     land_purged_at / land_records_purged_at on the project (update
	//     project above).
	"gardenio-machine-lands-provisioner": {
		Writes: []machineWriteAllowKey{
			{Action: "update", BeadType: typePlotApplication},
			{Action: "create", BeadType: "project"},
			{Action: "update", BeadType: "project"},
			{Action: "close", BeadType: "project"},
			{Action: "create", BeadType: "account"},
			{Action: "update", BeadType: "account"},
			{Action: "close", BeadType: "account"},
			{Action: "create", BeadType: typeCreditGrant},
			{Action: "update", BeadType: typeLandInvite},
			{Action: "close", BeadType: typeLandInvite},
			{Action: "update", BeadType: typeInvite},
			{Action: "close", BeadType: typeInvite},
		},
		Ownership: []model.BeadType{typePlotApplication, typeLandInvite, typeInvite},
		LandPurge: true,
	},
	// grafana-bridge EnsureGrafanaBridgeConfigs (cmd/grafana-bridge/main.go →
	// internal/bridge/init_ensure.go): registers the `dashboard` bead type at
	// start. config is admin-gated and holds what reaches every agent pod
	// (prerun runs via `sh -c`, claude-hooks, claude-instructions, context,
	// nudge-prompts), so the grant is exactly the entries grafana-bridge OWNS
	// (bridge.GrafanaBridgeConfigKeys), nothing it merely could seed. The shared
	// prompt/prerun/context/instructions seeding stays with the bridges that
	// seed it on their own tokens. Pinned == the bridge's seed set by
	// TestMachineConfigSeedTitles_MatchBridgeSeed in gardenio/controller.
	"gardenio-machine-config-seed": {ConfigTitles: machineConfigSeedTitles},
	// grafana-bridge dashboard sync (internal/bridge): grafana_readback.go:284
	// (UpdateBeadFields on an operator/agent-authored dashboard), :353 (blocks
	// dep whose dependent is that dashboard), grafana_sync.go:207
	// (UpdateBeadFields dashboard), grafana_dashboard_store.go:97 (model doc
	// created earlier under created_by=static, which the machine subject no
	// longer matches). Typed to dashboards, plus UPDATE ONLY of the docs that
	// carry the dashboard-model label the store stamps (dashboardModelDocLabel);
	// other docs (plan bundles, design docs) stay out of reach (decision
	// gd-ZsfxvXRiyv: grafana-bridge = ownership typed to dashboard and doc).
	"gardenio-machine-dashboard-sync": {
		Ownership: []model.BeadType{"dashboard"},
		LabeledOwnership: []machineLabeledOwnership{
			{BeadType: "doc", Label: "grafana:dashboard-model", Actions: []string{"update"}},
		},
	},
	// trigger-engine (gardenio/controller/cmd/trigger-engine + internal/bridge,
	// inventory §3.3 / App. B §1; decisions gd-ZsfxvXRiyv "tight" and
	// gd-dI7PfsFlvr "lineage"). It writes no admin-gated type (no Writes, no
	// config). What it needs is ownership of beads other principals authored:
	//   - trigger: every gardener_*/trigger_source_schedule stamp, reopen,
	//     close and label on trigger beads a human or agent created
	//     (IB/triggers.go:2340-2454 service-field stamps, which for a machine
	//     fall through to this gate; :2418 gardener arm; gardener_evaluator.go
	//     :564,602,1097,1362,1440; gardener_hooks.go:131; gardener_second_life.go
	//     :181; trigger_source_schedule.go:282,537; closed_enabled_drain.go:431).
	//   - task, bug, feature, chore: the run beads the engine itself files
	//     (createTaskBead allow-list, IB/triggers.go:4030) and later closes
	//     (internal/scheduler/per_run_task_close.go:67) — including those
	//     created BEFORE the roll under created_by=static, which the machine
	//     subject no longer matches — plus the orphan / auto-intake sweeps'
	//     labels on work items nobody owns (cmd/trigger-engine/main.go:1185,
	//     :1453; scheduler/orphan_sweep.go:108 eligible types). epic is left
	//     out on purpose: the engine never files or closes one, and the orphan
	//     sweep no longer labels epics (gd-10swg8vnzy).
	//   - doc, update only, labelled uncovered-open-lane-rollup: the one doc the
	//     engine upserts (IB/uncovered_open_lane_rollup.go:433, found by that
	//     marker label), created under static before the roll.
	// Decision and schedule beads need nothing here: decisions pass the
	// shared-queue carve-out, schedule/agent are config-kind.
	// TriggerLineage: the fires' human attribution (see machineCapability).
	"gardenio-machine-trigger-ops": {
		Ownership: []model.BeadType{"trigger", model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeChore},
		LabeledOwnership: []machineLabeledOwnership{
			{BeadType: "doc", Label: "uncovered-open-lane-rollup", Actions: []string{"update"}},
		},
		TriggerLineage: true,
	},
	// slack-bridge (gardenio/controller/cmd/slack-bridge + internal/bridge,
	// inventory §3.5 / App. B §3; decision gd-ZsfxvXRiyv "tight, amended": the
	// bridge's OWN background writes only, no operated-by relay: human Slack
	// writes ride the linked person's own token, gd-OZJ6H2S3xx). Two roles, one
	// per thing the machine principal is used for:
	//
	// slack-config-seed: EnsureSlackBridgeConfigs (cmd/slack-bridge/main.go →
	// internal/bridge/init_ensure.go) registers the bead types and operator
	// queue views the bridge delivers into. config is admin-gated and holds what
	// reaches every agent pod, so the grant is exactly the entries the bridge
	// seeds (bridge.SlackBridgeConfigKeys), nothing it merely could seed: the
	// controller seeds the rest of configs() on its own token. Pinned == the
	// seed set by TestSlackBridgeConfigSeedTitles_MatchBridgeSeed in
	// gardenio/controller.
	"gardenio-machine-slack-config-seed": {ConfigTitles: machineSlackConfigSeedTitles},
	// slack-dm-delivery: the bridge's delivery records.
	//   - dm: the type=dm carve-out a fleet token has (authorizeOwnership,
	//     `id.Type == "fleet" && bead.Type == "dm"`), typed to dm for the
	//     machine principal. An agent creates the dm bead; the bridge takes it
	//     over to deliver the Slack DM and must write delivery metadata
	//     (dm_channel/dm_ts, IB/dm_delivery.go:204), close fire-and-forget DMs
	//     (:222), capture replies (:281 UpdateBeadFields, :292 AddComment) and
	//     the reminder stamp (:404).
	//   - permission (gd-RuI4Ze6vAg): type:permission is admin-gated, and the
	//     bridge is its only writer. It creates the request bead on the
	//     permission callback (IB/decision_cards.go writePermissionBead) and
	//     closes it with behavior / answered_by once it has delivered the
	//     verdict (closePermissionBead). Ownership covers the beads filed on the
	//     bridge's fleet token before the move to this principal.
	// The thread-task bookkeeping and every other bead type stay on the fleet
	// credential.
	"gardenio-machine-slack-dm-delivery": {
		Writes: []machineWriteAllowKey{
			{Action: "create", BeadType: typePermission},
			{Action: "update", BeadType: typePermission},
			{Action: "close", BeadType: typePermission},
		},
		Ownership: []model.BeadType{"dm", typePermission},
	},
	// controller default client (gd-BDirD4F7Iq, stage 3a of gd-5QYwXtV7IA;
	// inventory §3.1 / App. A (O)): the ownership bypass the controller
	// borrowed from the static token (write_authz.go, id.Type == "static"),
	// typed to the beads it reaps or stamps but does not author. Decision
	// gd-ZsfxvXRiyv "tight": no blanket system-graph capability.
	//   - task: the stale reaper's close / reopen / park / unassign / label of
	//     stranded run-instance tasks (statusreporter/stalereaper.go:614,725,
	//     905,957,1096,1249) and the reconciler's closeOrphanedMirrorTask
	//     (reconciler.go:1432), on tasks other principals filed — including
	//     those created BEFORE the roll under created_by=static, which the
	//     machine subject no longer matches (no created_by rewrite; this grant
	//     is what makes that safe).
	//   - advice: the advice sweeper's close (advice_sweeper.go:118). advice is
	//     admin-gated, so close/advice is also a Writes grant (machine
	//     principals do not consult machineAllowedWrites).
	//   - session: reconcile_session_spend.go:546 settles cost on session
	//     beads gb created.
	//   - trigger: captain_spend_sync.go:127 stamps captain_spend_* on trigger
	//     beads, which are not service-managed fields.
	//   - bug, feature, chore, epic: ReclaimOwnership only (gd-RZJk5gspFv,
	//     decision gd-mM3IFDGoTq "narrow"). The stale reaper's claim release on
	//     a hard-killed crew's work (reopenStrandedIssueTasks,
	//     parkCappedStrandedTask, flagOrphanedWithPR,
	//     clearStrandedOpenAssignees in statusreporter/stalereaper.go): update,
	//     never close, and only while the assignee is a gone agent.
	//   - bug, feature, chore: DispatchClaim, the claim at pod create
	//     (beadsapi/spawn_task_claim.go) of an unassigned one for the agent
	//     dispatched onto it (gd-GrJRP7kLBY).
	//   - epic, plan_spend_* fields only (FieldOwnership): plan_spend_sync.go
	//     rolls each plan's crew spend onto its plan epic, which a human or a
	//     gardener filed. Without it every stamp was an ownership 403, the
	//     sync re-walked every unstamped plan each 10-minute pass, and the
	//     refusals were ~12.6k/day of gardenio_authz_denials_total
	//     (gd-Hk70sS1XuS).
	// decision and every other type are left out: the controller never mutates
	// them. agent, account, schedule and config beads are config-kind and never
	// reach the ownership gate.
	"gardenio-machine-controller-ownership": {
		Writes:           []machineWriteAllowKey{{Action: "close", BeadType: "advice"}},
		Ownership:        []model.BeadType{model.TypeTask, "advice", "session", "trigger"},
		ReclaimOwnership: []model.BeadType{model.TypeBug, model.TypeFeature, model.TypeChore, model.TypeEpic},
		DispatchClaim:    []model.BeadType{model.TypeBug, model.TypeFeature, model.TypeChore},
		FieldOwnership:   []machineFieldOwnership{{BeadType: model.TypeEpic, Fields: machinePlanSpendFields}},
	},
	// controller EnsureConfigs at start (cmd/controller/main.go →
	// internal/bridge/init_ensure.go): upserts every gardenio-managed type,
	// view, prompt and prerun config bead. config is admin-gated and holds what
	// reaches every agent pod (prerun runs via `sh -c`), so the grant is exactly
	// the titles configs() defines, nothing else: no `auth` (the legacy
	// config:auth bead rewritten by auth_config_legacy.go is credential routing;
	// that write is not granted and degrades to a logged warning). Pinned == the
	// seed set by TestMachineControllerConfigSeedTitles_MatchSeed in
	// gardenio/controller.
	"gardenio-machine-controller-config-seed": {ConfigTitles: machineControllerConfigSeedTitles},
	// github-bridge release recorder (gardenio/controller/cmd/github-bridge
	// release.go, plan gd-UZKZ9H25UD step 3b): releaserecord.Recorder.Write
	// creates the release (POST /v1/releases) and fills contents/previous_tag/sha
	// on one another writer created (PATCH, so typed ownership of release).
	// Step 4a's deployment_status handler records deployments (deploy) on it too.
	"gardenio-machine-release-recorder": {
		Writes: []machineWriteAllowKey{
			{Action: "create", BeadType: model.TypeRelease},
			{Action: "update", BeadType: model.TypeRelease},
			{Action: releaseDeployAction, BeadType: model.TypeRelease},
		},
		Ownership: []model.BeadType{model.TypeRelease},
	},
	// github-bridge daemon client (gardenio/controller/cmd/github-bridge; task
	// gd-tCMXlS9fXW, decision gd-aZrncmZbkD "tight"). Every bridge write is
	// machine-origin (webhook mapping, CI-wait resolver, agent resolver,
	// conflict scanner), so no operated-by relay. It writes no admin-gated
	// type, so no Writes:
	//   - merge-request: Ownership. UpsertMergeRequestBead (beadsapi
	//     client_mr.go) updates and closes the PR's MR bead, which `gb mr link`,
	//     beads-viewer or a write on the retired static token may have created;
	//     pipeline.go apply, pullrequest.go stampMerge, conflicts.go apply and
	//     overlaps.go clearOverlaps stamp it; forgebeads.LinkTaskToMR adds its
	//     triggered-by edge.
	//   - task, bug, feature, chore, epic: FieldOwnership of exactly
	//     machineGitHubBridgeWorkFields on the work beads a PR belongs to, which
	//     agents and humans filed.
	// Agent beads (the ci_wait_* sweep, the CI wake's reopen/close) are
	// config-kind, and message, mail and spawned agent beads are its own
	// creates, so none needs a grant. Left out on purpose: the produces edge
	// from a work bead (LinkTaskToMR, best effort; `gb mr link` adds it on the
	// agent's own token) and the resolver spawn's pre-claim of the task
	// (best effort; the controller claims at pod create).
	"gardenio-machine-github-bridge": {
		Ownership:      []model.BeadType{mergeRequestType},
		FieldOwnership: fieldOwnershipOn(machineGitHubBridgeWorkFields, model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeChore, model.TypeEpic),
	},
	// release deployments (plan gd-UZKZ9H25UD step 4, releases-design.md
	// §4.3): POST /v1/releases/{id}/deployments only, which releaseWriterDenial
	// checks as deploy/release. Held by the release-deployer CronJob
	// (release-deploy.sh report_deployment, after a verified roll or a
	// rollback) and the controller (cmd/controller/release_reconciler.go, the
	// observed row when what runs on prod differs from the newest deployment).
	// It grants no update, so a holder cannot PATCH a release's sha, images or
	// contents; recording a release (create/release) stays with the release
	// captain, the operator, gardenio-admin and the release recorder.
	"gardenio-machine-release-deploy": {Writes: []machineWriteAllowKey{{Action: releaseDeployAction, BeadType: model.TypeRelease}}},
	// the in-cluster escalation coordinator (escalation routing Task 15 C4/C5,
	// owner decision gd-B71zgAalvu; client gardenio-cluster-coordinator, lane
	// coordinator:cluster): the bead writes of an owner-approved bead-only
	// escalation it has claimed, on the beads the approval named
	// (approved_targets) and nothing else. No Ownership, no Writes: config,
	// role, agent, trigger, escalation, decision and policy beads stay out of
	// reach, and deployment records ride gardenio-machine-release-deploy.
	"gardenio-machine-escalation-executor": {EscalationExecutor: true},
}

// typePermission is a tool-permission request the bridge records (config.go).
const typePermission model.BeadType = "permission"

// machineControllerConfigSeedTitles are the config titles the controller seeds
// (bridge.configs() → parseEntryKey titles). Exact titles only. Adding a title
// here is a privilege grant: prerun, context, claude-instructions and
// nudge-prompts reach every agent pod, so extend it only when configs() does.
var machineControllerConfigSeedTitles = []string{
	"claude-instructions",
	"context",
	"nudge-prompts",
	"prerun",
	"type:account",
	"type:advice",
	"type:agent",
	"type:alert_mute",
	"type:alert_rule",
	"type:audit",
	"type:dashboard",
	"type:decision",
	"type:dm",
	"type:escalation",
	"type:fleet",
	"type:formula",
	"type:jira-poller",
	"type:mail",
	"type:memory",
	"type:message",
	"type:molecule",
	"type:permission",
	"type:project",
	"type:reaction-rule",
	"type:report",
	"type:role",
	"type:schedule",
	"type:session",
	"type:task",
	"type:trigger",
	"type:user",
	"type:view",
	"view:agents:active",
	"view:agents:crew",
	"view:agents:gardeners",
	"view:agents:jobs",
	"view:agents:reviewers",
	"view:audit:recent",
	"view:decisions:pending",
	"view:escalations:pending",
	"view:fleets:active",
	"view:jira:pending",
	"view:mail:inbox",
	"view:projects",
	"view:schedules",
	"view:users",
}

// machineConfigSeedTitles are the config titles grafana-bridge owns and seeds
// (bridge.GrafanaBridgeConfigKeys → parseEntryKey titles). type:dashboard is
// the only entry: grafana-bridge defines no other bead type and no view.
// Adding a title here is a privilege grant: do not add prerun, context,
// claude-*, nudge-prompts or auth.
var machineConfigSeedTitles = []string{"type:dashboard"}

// machineSlackConfigSeedTitles are the config titles slack-bridge owns and
// seeds (bridge.SlackBridgeConfigKeys → parseEntryKey titles): the dm, message,
// mail, decision and escalation bead types it delivers into, and the three
// operator-queue views over them. Exact titles only. Adding a title here is a
// privilege grant: do not add prerun, context, claude-*, nudge-prompts, auth,
// or the type:role / type:agent / type:project / type:user write-policy types.
var machineSlackConfigSeedTitles = []string{
	"type:decision",
	"type:dm",
	"type:escalation",
	"type:mail",
	"type:message",
	"view:decisions:pending",
	"view:escalations:pending",
	"view:mail:inbox",
}

// configTitleMatches reports whether title matches an allow-list entry (exact,
// or a prefix when the entry ends in "*").
func configTitleMatches(entries []string, title string) bool {
	for _, e := range entries {
		if p, ok := strings.CutSuffix(e, "*"); ok {
			if strings.HasPrefix(title, p) {
				return true
			}
		} else if e == title {
			return true
		}
	}
	return false
}

// machineGrantsConfigWrite reports whether the request's machine principal
// holds a role that lets it create/update a config bead with this title. False
// for any other auth method, any other action, and an empty title.
func machineGrantsConfigWrite(ctx context.Context, action, title string) bool {
	if authMethodFromContext(ctx) != authMethodMachine || title == "" {
		return false
	}
	if action != "create" && action != "update" {
		return false
	}
	for _, role := range machineRolesFromContext(ctx) {
		if configTitleMatches(machineCapabilityGrants[role].ConfigTitles, title) {
			return true
		}
	}
	return false
}

// machineGrantsWrite reports whether the request's machine principal holds a
// role whose grants cover (action, beadType). False for any other auth method.
func machineGrantsWrite(ctx context.Context, action string, beadType model.BeadType) bool {
	if authMethodFromContext(ctx) != authMethodMachine {
		return false
	}
	for _, role := range machineRolesFromContext(ctx) {
		for _, w := range machineCapabilityGrants[role].Writes {
			if w.Action == action && w.BeadType == beadType {
				return true
			}
		}
	}
	return false
}

// activeAgentStatuses are the type=agent bead statuses that mean the agent may
// still be alive (everything but closed).
var activeAgentStatuses = []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred}

// machineMayStampFields reports whether the request's machine principal may
// apply this update to a bead it does not own under a FieldOwnership grant:
// one of its roles lists the bead's type, and the patch changes only that
// grant's fields (patchOnlyChangesFields: no column, so never a close, and a
// no-op patch does not qualify).
func machineMayStampFields(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || authMethodFromContext(ctx) != authMethodMachine {
		return false
	}
	for _, role := range machineRolesFromContext(ctx) {
		for _, g := range machineCapabilityGrants[role].FieldOwnership {
			if g.BeadType == bead.Type && patchOnlyChangesFields(g.Fields, in, bead.Fields) {
				return true
			}
		}
	}
	return false
}

// machineHasCapability reports whether the request's machine principal holds a
// role granting the named capability (capTokenMint). False for any other auth
// method, so it can be OR-ed into an existing admin check.
func machineHasCapability(ctx context.Context, capability string) bool {
	if authMethodFromContext(ctx) != authMethodMachine {
		return false
	}
	for _, role := range machineRolesFromContext(ctx) {
		g := machineCapabilityGrants[role]
		switch capability {
		case capTokenMint:
			if g.TokenMint {
				return true
			}
		case capTriggerLineage:
			if g.TriggerLineage {
				return true
			}
		case capLandPurge:
			if g.LandPurge {
				return true
			}
		case capPlanScope:
			if g.PlanScope {
				return true
			}
		}
	}
	return false
}
