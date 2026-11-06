// Package plangardener is the ONE gardener-spec builder for /plans plan
// gardeners, shared by `gb plan gardener` (cmd/gb/plan_gardener.go) and the
// beads-viewer plan surfaces (POST /plans auto-bind + the hand-mark path,
// task ka-0000002863). Before this package the gardener-build primitives
// (loop blob, exit guidance, labels, playbook prompt) lived in package main
// under cmd/gb and were unimportable by cmd/beads-viewer, which forced the
// viewer to duplicate constants and made a second, drifting builder the path
// of least resistance. Lifting the core here means both binders emit
// byte-identical gardener beads (a parity test in cmd/gb locks this).
//
// A plan carries ONE first-class plan-gardener: the trigger bead keeps the
// canonical gardener markers (role:gardener label + gardener blob +
// gardener-epic:<plan-id> label, the ka-0000005048 convention), but its SPAWN
// role field is `plan-gardener` (the unified role of the ka-0000000081 merge)
// — so every iteration the engine fires comes up in the fable+runsc-pinned
// plan-gardener role, carries task_id=<plan> + the `assigned` edge, and
// therefore resolves as the plan's bound agent in beads-viewer
// (plans_assistant.go planAssistant / isPlanAssistantBead, which key on
// role:plan-gardener). One agent lineage — the same agent both watches the
// plan and answers operator messages from /plans.
//
// The trigger spec absorbs the closed steering-gardener C4 (ka-0000006602):
// event-mode firing (plan-activity bead events + a staggered daily watchdog
// cron, gd-gsvlS6YDj3; timed ~30m cron only on request), run_budget ~8 per
// steering session, the mandatory self-block (advice ka-0000000974) with
// loop_end.any =
// [bead_query drain over the plan's open children, max_runs,
// no_progress_for(3)] paired with a progress spec, one_spawn_at_a_time, and
// optional Slack routing to the planner channel.
//
// Idempotent-adopt: if a gardener is already pinned to the plan (gardener-epic
// label or legacy assign_bead) — enabled OR paused — Bind reports it and
// returns WITHOUT creating a duplicate, so a re-mark / double-submit / retry
// never mints a second loop.
package plangardener

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/template"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/progress"
)

// SpawnRole is the role every plan-gardener iteration spawns in — the
// fable+runsc-pinned unified plan-agent role (epic ka-0000000081; role bead
// ka-0000003712), the same role the viewer's plan-agent lane spawns
// (cmd/beads-viewer/plans_assistant.go planGardenerRole). Keeping the two in
// lockstep is what makes the plan's gardener and its interactive agent one
// lineage: the viewer resolves the gardener's iterations as the plan's bound
// agent via the role:plan-gardener label + task_id binding.
const SpawnRole = "plan-gardener"

// MarkerLabel tags the trigger bead as a plan-bound gardener so the /plans
// surfaces (Step 2 control bar, Step 4 stalled badge) can list plan gardeners
// without scanning every gardener's epic for the plan label.
const MarkerLabel = "plan-gardener"

// DefaultMaxRuns is the per-session run budget carried over from the
// steering-gardener C4 spec (ka-0000006602): a steering session is a handful
// of nudged turns, not an open-ended loop. The resume master-gate
// (GardenerSpec.ResumeBudget) re-arms the gardener when the budget is spent
// with the terminal goal unmet, so 8 bounds a session, not the plan's life.
const DefaultMaxRuns = 8

// DefaultLoopWallClock is the hard wall-clock ceiling for the whole loop —
// a 1-week absolute cap so a wedged gardener that somehow evades every
// loop_end can never run forever.
const DefaultLoopWallClock = "168h"

// defaultRunWallClock caps a single run (surfaced in the prompt's exit
// guidance; the engine's run_timeout is separate).
const defaultRunWallClock = "30m"

// DefaultTimezone is the §5.1 default cron timezone.
const DefaultTimezone = "America/Los_Angeles"

// PlanLabel is the /plans marker label on the plan epic itself (mirrors
// cmd/beads-viewer/plans.go planLabel). Bind requires it so "the plan's one
// gardener" stays a meaningful invariant.
const PlanLabel = "plan"

// planStateFieldKey is the plan epic field holding the authoring lifecycle
// state (mirrors cmd/beads-viewer/plans_lifecycle.go planStateFieldKey; the
// R6 approve→build gate of task ka-0000005350).
const planStateFieldKey = "plan_state"

// GardenerEpicLabel is the label that pins a gardener trigger to its epic. The
// bare trigger:schedule shape has no structured `assign_bead` field (the
// type:trigger config schema rejects unknown keys), so the epic linkage lives
// on a label instead. Used both at creation time and by the resolvers so
// `gb gardener status|stop|force-close <epic>` and the viewer's planGardener
// lookup keep working.
func GardenerEpicLabel(epicID string) string {
	return "gardener-epic:" + epicID
}

// Client is the daemon surface Bind needs — satisfied by *beadsapi.Client
// (cmd/gb's daemon) and by cmd/beads-viewer's beadAPI interface alike.
type Client interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// PlaybookPrompt is the unified plan-gardener playbook: the WATCHDOG duty
// (epic ka-0000002022), the STEER duty (ka-0000001282), and the AUTHOR duty
// (ka-0000001409) folded into the single bound agent. Step 3 (ka-0000000912)
// authors the full playbook doc under gardenio/docs; this prompt is the
// self-contained operational core.
const PlaybookPrompt = `You are the bound plan-gardener for plan {{.PlanID}} on /plans — the plan's
single watchdog AND the agent operators message from the plan page. You must
never let its work orphan silently, and you must never silently park yourself.

Read your "## Scheduled Loop Context" and "## Loop Progress" sections,
then run this playbook each wake:

0. ARMS — at the start of the run, if {{.PlanID}} or any phase epic in its
   phase_order bears no arms ('kd show <id>' prints no Arms line; an
   "## Arms" section in your preamble names them), give each its arms: read
   it, choose three to five charges with 'kd heraldry library --search
   <word>', check them with 'kd heraldry check "<run>" --project <project>',
   and write them with 'kd update <id> --heraldry "<run>"'. Arms are chosen,
   not generated: nobody else will pick them. Name the arms you chose in
   this run's comment on {{.PlanID}}.

1. ORPHAN WATCHDOG — scan the plan subtree in TWO batch calls, never a
   per-child probe loop:
   - 'gb open-children {{.PlanID}}' — one call prints the open (non-closed)
     child frontier, one id per line: exactly the steps still needing you.
   - 'kd show <id1> <id2> ...' — pass that whole frontier to ONE multi-id
     kd show (add --json for a JSON array to script against). This replaces
     any 'for b in ...; do kd show $b --json | jq .status; done' loop and
     any repeated 'kd list --parent' probing — do NOT hand-roll the N+1.
   Then, across that frontier, look for:
   a. Steps in_progress whose agent is dead or absent for >1h (advice
      ka-0000007726): re-open the step (kd update <id> --status=open
      --assignee "") or spawn a fresh crew agent on it (gb spawn --task <id>
      --role crew). Clearing the assignee is MANDATORY when you re-open:
      a status-only reset strands the step (open + dead-assignee falls through
      every dispatch lane, which all require assignee=="") — ka-0000000908.
   b. Escalations or decisions gating downstream steps that are stale
      (>4h old, not recently reflagged): freshen by bead type —
      'gb decision reflag <id> --reason ...' for type=decision,
      'gb escalation reflag <id> --reason ...' for type=escalation
      (both bump priority toward P0 and repost to Slack) — and comment
      the blockage on the plan epic.
   c. CLOSE CRITERIA — the plan is ready to complete when ALL THREE hold:
      (1) every transitive open child is closed ('gb open-children {{.PlanID}}'
          prints nothing); (2) no open decision or escalation is gating the
          plan (none in the frontier scan above, none owned by you awaiting a
          human); (3) the bundle/narrative has converged to its VP-altitude
          final status (step 4). When all three are met, COMPLETE THE PLAN
          YOURSELF and tell, do not ask (owner ruling 1 on gd-7qUpu15BBK,
          which reverses ka-0000004595). File no decision:
          - verify each criterion and comment the evidence on {{.PlanID}}:
            what you ran and what it printed for (1), (2) and (3);
          - run 'gb plan complete {{.PlanID}}' — it disables this gardener,
            stamps plan_state=complete and closes the plan epic. The
            daemon's plan-scope rule admits your writes on your own plan,
            so this verb is yours;
          - post one line to the plan's channel:
              gb squawk "Plan {{.PlanID}} complete: <one-line outcome>. Reopen with gb plan reopen {{.PlanID}} if you disagree."
          - 'gb stop'.
          The owner reopens the plan if they disagree. If 'gb plan complete'
          refuses, comment the error on {{.PlanID}} and file a
          'gb escalation create --operation-type plan_complete' naming it;
          do not file a decision. If only some criteria hold, comment which
          are still open and keep working the plan.
   Surface every finding as a comment on {{.PlanID}} plus a gb squawk —
   findings must be visible on the plan, never only in your own session.

2. STEER — when the plan needs a new step, MINT IT WITH kd:
     kd create "<step title>" --type=task -l project:<the plan's project>
     kd dep add <step-id> {{.PlanID}} --type parent-child
   That parent-child edge onto the plan epic IS what makes the bead a
   step — nothing else is required for it to render on /plans.
   A step never waits on a clock ("bake", "soak", "wait N hours, then"):
   gate it on an event or evidence (a first occurrence, N successes, one
   measurement). A deadline is fine; only the owner can ask for a wait.

   Do NOT reach for 'gb plan add-step' here (the one 'gb plan' write
   that is yours is 'gb plan complete' in step 1c): every other 'gb plan'
   WRITE verb is stamped on behalf of a human operator and REJECTS your identity
   ("operated-by \"slack:U…\" is not an oidc:<email> id"), because agent
   sessions carry a slack:/agent id, not an oidc:<email>. It is the
   HUMAN operator's verb (from /plans or a human shell); the kd path
   above is yours. They converge on the same graph write — add-step is
   itself just "create a task, link it parent-child" — so you lose
   nothing by using kd.

   You do NOT bypass the reactive collaboration contract by doing this.
   The nudge fires from a reactor that observes the daemon's
   beads.dependency.added event for ANY writer (browser, CLI, or your
   'kd dep add'), so your step edge nudges/wakes a plan's already-bound
   agent exactly like an operator's click. What that reactor never does
   is COLD-SPAWN on a plan with no live agent — so still dispatch the
   step yourself in step 3 rather than assuming an agent appears.

   PHASES — to GROUP steps into ordered phases (research / design /
   implementation / validation — whatever vocab fits), a phase is NOT a
   task titled "Phase N". A phase is its own step-grouping sub-epic, and
   it takes BOTH a parent-child edge AND a phase_order entry:
     kd create "<phase title>" --type=epic -l project:<the plan's project>
     kd dep add <phase-id> {{.PlanID}} --type parent-child
     kd update {{.PlanID}} -f phase_order='["<phase-id>", …]'
   Presence in the plan's fields.phase_order is the ONLY thing that makes
   a child epic render as an inline phase (a bare child epic not listed
   stays a freestanding sub-plan). Naming a flat task "Phase 3: ..." does
   NOT create a phase — it renders as one more ungrouped step.
   - Put a step INTO a phase by parenting it to the PHASE epic instead of
     the plan: 'kd dep add <step-id> <phase-epic-id> --type parent-child'.
   - Reorder phases by rewriting that same phase_order list.
   CAUTION: phase_order is a whole-list write, so read the current value
   ('kd show {{.PlanID}} --json') and append to it — never clobber
   existing phases. ('gb plan add-phase'/'reorder-phase' do this
   atomically, but are the same operator-only verbs, closed to you.)
   Reach for phases only when the plan is large enough to warrant grouped
   stages; a small plan is fine as a flat step list.

3. DRIVE — for each open, unblocked step without a live agent:
   gb spawn --task <id> --role crew. Skip steps that already have an
   in_progress agent. To see just this plan's ready frontier (open +
   unblocked, blocked steps hidden), run 'gb ready --plan {{.PlanID}}'
   — it scopes the ready set to your plan's subtree so your steps never
   sink below the limit under project-wide work.
   OVERLAP HOLD (gd-h7awQXGs8Z): run it as 'gb ready --plan {{.PlanID}}
   --hold-overlaps'. A row marked HELD names a file (in backticks) that an
   open PR of another step in this plan already changes: do not dispatch it
   yet. Comment once on {{.PlanID}} "held <step> behind #<pr> (<files>)",
   and dispatch it on a later wake once that PR merges and the row is no
   longer HELD; comment "dispatched <step>, #<pr> merged" when you do. A
   row marked HOLD EXPIRED (held over 12h) is advisory, not a lock:
   dispatch it now and comment "dispatched <step> after a 12h hold behind
   #<pr>" on the step. Steps that name no files are never held.

4. CONVERGE THE BUNDLE — before you wrap up (and continuously), AUDIT the
   plan's attached bundle against your current knowledge, biased toward
   DELETING stale content rather than amending it. The epics/tasks hold the
   HISTORY; the plan (its bundle) holds the CURRENT STATUS, with enough
   context to bring a new person in cold. Converge the bundle on this
   altitude shape:
     - first PARAGRAPH -> the VP: one-line outcome / status.
     - first PAGE -> Product: what & why, where it stands, what's next.
     - below the fold -> the engineering manager: design, decisions, risks,
       open threads.
     - the epics/tasks -> the engineers (agents): the how, and the full
       history.
   Push durable history DOWN into the relevant epic/task instead of leaving
   it in the plan. Edit the bundle in place — never fork a parallel status
   doc. When in doubt, cut.

5. If you OWN an open decision awaiting a human that you need
   resolved before you can continue: gb yield (it blocks until the
   operator resolves it). Otherwise — crew dispatched, or nothing
   left this turn — FIRST kd close <your-claimed-cron-task-id> (or
   kd unclaim <id> if not complete): the despawn gate REJECTS a gb
   stop while you still hold claimed in-progress work (Exit 1 "you
   have claimed in-progress work ... close or unclaim"), costing a
   wasted retry turn. THEN gb stop --wrapup '{"tldr":"<one-line outcome>",
   "accomplishments":"...",
   "progress_score":<0|1|2>,"progress_summary":"..."}'. You are
   respawned on the next trigger, so in-flight crew are picked back
   up automatically; do NOT yield just to wait on them (yield has
   nothing to block on and will error).

You are the plan's interactive agent too: operator messages from the /plans
page arrive as nudges in this same session — answer them in place, then resume
the playbook.`

// RenderPrompt expands the unified plan-gardener playbook for the given plan
// epic.
func RenderPrompt(planID string) (string, error) {
	tmpl, err := template.New("plan-gardener-prompt").Parse(PlaybookPrompt)
	if err != nil {
		return "", fmt.Errorf("parsing plan-gardener prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ PlanID string }{PlanID: planID}); err != nil {
		return "", fmt.Errorf("executing plan-gardener prompt template: %w", err)
	}
	return buf.String(), nil
}

// ExitGuidance carries the loop-exit parameters folded into the gardener
// prompt.
type ExitGuidance struct {
	EpicID        string
	Terminal      string
	LoopWallClock string
	RunWallClock  string
}

// AppendExitGuidance folds the loop-exit contract into the gardener prompt.
// The bare trigger:schedule shape can't carry the §5.1 structured
// `stop`/`max_runs` fields (the type:trigger config schema rejects them), so
// the gardener agent owns its own loop closure: the cron pre-flight stops
// firing once the epic's children all close, and this guidance tells the
// agent to gb stop when the terminal goal is reached.
func AppendExitGuidance(prompt string, g ExitGuidance) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(prompt, "\n"))
	b.WriteString("\n\n## Loop exit\n\n")
	fmt.Fprintf(&b, "Terminal goal: %s\n", g.Terminal)
	// The prompt names no run count: the engine re-arms the budget, so a count
	// here only invited "extend budget?" cards (P8.2, gd-WHnRx2Gj3A).
	b.WriteString("The engine re-arms your budget; never ask to extend it. If your " +
		"runs stop making progress it parks you until the plan changes.\n")
	fmt.Fprintf(&b, "Wall-clock caps: loop %s, single run %s.\n", g.LoopWallClock, g.RunWallClock)
	fmt.Fprintf(&b,
		"When the terminal goal is met (verify with `kd show %s` and its open "+
			"children), call `gb stop --wrapup` to end the loop. The cron "+
			"pre-flight on this trigger only fires while %s still has open "+
			"children, so the loop goes dormant on its own once everything closes.\n",
		g.EpicID, g.EpicID)
	b.WriteString(
		"Leave your own trigger enabled — dormancy is automatic. The cron " +
			"pre-flight only fires while the epic has open children, so " +
			"disabling the trigger yourself (e.g. `kd update <trigger-id> " +
			"--field enabled=false`) severs the auto-wake path: children " +
			"unblocked later (e.g. when a human-shared MR finally merges) " +
			"would never re-dispatch, and the epic stalls silently. When the " +
			"plan is genuinely waiting on one bead (a PR's merge, a decision, " +
			"an escalation), you may idle your trigger until it closes: " +
			"`kd update <trigger-id> --field enabled=false --field " +
			"awaiting=<bead-id>`. The daemon refuses an idle that names no " +
			"open bead to await, and turns the trigger back on the moment " +
			"that bead closes. When you have dispatched " +
			"crew or finished this turn, `gb stop` — you are respawned on the " +
			"next trigger, so in-flight crew are picked back up automatically. " +
			"Yielding just to wait on them has nothing to block on and will " +
			"error, so save `gb yield` for when you are genuinely wedged on " +
			"something only a human can do (e.g. a human-shared monorepo MR " +
			"awaiting a named reviewer): file a `gb decision` or " +
			"`gb escalation`, then `gb yield` blocks until the operator " +
			"resolves the decision you own.\n")
	return b.String()
}

// LoopBlobParams carries the inputs needed to build the structured gardener
// loop blob (the `gardener` JSON field the trigger-engine decodes into a
// bridge.GardenerSpec).
type LoopBlobParams struct {
	EpicID        string
	Project       string
	Terminal      string
	MaxRuns       int
	LoopWallClock string
	// OneSpawnAtATime serializes iterations: the engine (and the run-now
	// lever) refuse a new spawn while the prior iteration is still live.
	// Plan gardeners set it so a plan never has two assistants racing.
	OneSpawnAtATime bool
	// SlackChannel routes the iterations' agent-card reporting (stamped as
	// schedule_slack_channel on each spawned agent bead).
	SlackChannel string
	// StopOnMRMerged is an optional canonical MR URL (task ka-0000005598, the
	// `--stop-on-mr-merged <mr_url>` flag). When non-empty a specific_mr_merged
	// loop_end entry targeting that URL is added so the gardener closes as success
	// the moment that exact MR merges — a terminal, first-class "my goal is to
	// land this one MR" exit.
	StopOnMRMerged string
}

// BuildGardenerLoopBlob constructs the REAL structured gardener loop blob that
// the trigger-engine enforces. It is the default self-block mandated by advice
// ka-0000000974: every gardener built here declares both a run_budget AND a
// progress-aware / bead_query loop_end, plus a hard_stop wall-clock, so the
// scheduler closes the loop when the epic drains, the run budget is spent,
// progress stalls, or the failure streak / wall-clock cap is hit — instead of
// looping forever or relying on the agent to read prose and self-terminate.
//
// The blob is also where owner attribution lives (ka-0000003284, Gardener Page
// v2 R6): ResolveGardenerOwner maps the operator alias (slack:U… / oidc:… /
// gitlab:… / jira:…) to slack_user_id + user_bead_id, which tiers 1-2 of the
// beads-viewer owner resolver read from inside the gardener blob. E2E fixture /
// static gardeners (empty or non-human sentinel operator) get the loop spec but
// no owner fields — they stay intentionally ownerless.
//
// Returns "" only if marshalling fails (degrade to no blob rather than
// aborting gardener creation) — the loop spec is always non-empty, so a gardener
// is never emitted without its self-block.
func BuildGardenerLoopBlob(ctx context.Context, c Client, operator string, p LoopBlobParams) string {
	// loop_end.any: any entry firing closes the gardener cleanly.
	//   - bead_query open-children==0: the epic's transitive subtree has drained
	//     (the structured analogue of the cron pre-flight). Closed-root
	//     short-circuit (ka-0000008051) also makes this terminal once the epic
	//     itself closes.
	//   - max_runs: the gardener has spawned its run budget (RunsSpawned >=
	//     RunBudget) — the self-block run cap.
	//   - no_progress_for runs=3: the last 3 runs all scored zero progress
	//     (wedge detection), enforced against the progress spec below.
	loopEnd := scheduler.StopSlot{Any: []scheduler.StopEntry{
		{
			Kind:     scheduler.StopKindBeadQuery,
			Op:       "==",
			Value:    "0",
			Parent:   p.EpicID,
			Types:    []string{"task", "bug", "epic"},
			Statuses: []string{"open", "in_progress", "blocked"},
		},
		{Kind: scheduler.StopKindMaxRuns},
		{Kind: scheduler.StopKindNoProgressFor, Runs: 3},
	}}
	// Optional first-class "stop when THIS MR merges" (task ka-0000005598). A
	// specific_mr_merged entry is a TERMINAL loop_end: reaching the merge is the
	// gardener's declared goal, so a match closes it as success. Appended to
	// loop_end.any alongside the default self-block entries.
	if url := strings.TrimSpace(p.StopOnMRMerged); url != "" {
		loopEnd.Any = append(loopEnd.Any, scheduler.StopEntry{
			Kind:  scheduler.StopKindSpecificMRMerged,
			MRURL: url,
		})
	}
	// hard_stop.any: any entry firing tears the loop down immediately.
	//   - consecutive_failures=5: the gardener keeps failing — stop the bleed.
	//   - wall_clock=168h: a 1-week absolute ceiling so a wedged gardener that
	//     somehow evades every loop_end can never run forever.
	hardStop := scheduler.StopSlot{Any: []scheduler.StopEntry{
		{Kind: scheduler.StopKindConsecutiveFailures, Value: "5"},
		{Kind: scheduler.StopKindWallClock, Duration: firstNonEmpty(strings.TrimSpace(p.LoopWallClock), DefaultLoopWallClock)},
	}}
	stopSpec := scheduler.StopSpec{LoopEnd: loopEnd, HardStop: hardStop, OnLoopEnd: "close"}
	stopRaw, err := json.Marshal(stopSpec)
	if err != nil {
		return ""
	}

	// progress: what counts as forward progress + the windows no_progress_for
	// judges against. surface.in_wrapup=true makes `gb stop` reject a gardener
	// wrap-up missing progress_score / progress_summary, so the gardener must
	// self-attest progress each run (which feeds the no_progress_for wedge
	// detector above). Filter fields use the literal epic id (not the
	// {{assign_bead}} template) so the spec is self-contained.
	progSpec := progress.Spec{
		Events: []progress.EventMatcher{
			{Kind: progress.EventKindChildClosed, Parent: p.EpicID},
			{Kind: progress.EventKindMRMerged, Parent: p.EpicID},
			{Kind: progress.EventKindWrapupField, Field: "progress_score", Op: ">=", Value: "1"},
		},
		Windows: []progress.Window{
			{Name: "this_run", Scope: progress.WindowScopeSinceRunStart},
			{Name: "last_3_runs", Scope: progress.WindowScopeLastNRuns, N: 3},
		},
		Surface: progress.Surface{InPrime: true, InSquawkOnLoopEnd: true, InWrapup: true},
	}
	progRaw, err := json.Marshal(progSpec)
	if err != nil {
		return ""
	}

	spec := bridge.GardenerSpec{
		AssignBead:      p.EpicID,
		TerminalGoal:    p.Terminal,
		RunBudget:       p.MaxRuns,
		MaxRuns:         p.MaxRuns,
		OneSpawnAtATime: p.OneSpawnAtATime,
		SlackChannel:    p.SlackChannel,
		Stop:            json.RawMessage(stopRaw),
		Progress:        json.RawMessage(progRaw),
	}

	// Owner attribution (ka-0000003284): stamp slack_user_id + user_bead_id when
	// there is a human operator to attribute. Non-human sentinels resolve to
	// ("", "") and leave the gardener ownerless.
	spec.SlackUserID, spec.UserBeadID = ResolveGardenerOwner(ctx, c, operator)

	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(raw)
}

// ResolveGardenerOwner maps an operated-by alias ("slack:U…", "oidc:…",
// "gitlab:…", "jira:…") to the gardener owner pair (slack_user_id,
// user_bead_id). It mirrors the kbeads backfill resolver
// (resolveOwnerFromOperator / resolveOperatorLookup in
// kd/admin_backfill_*.go): slack: aliases are authoritative for the slack id;
// email-shaped kinds resolve via the user bead's email field; the user bead
// id is looked up by the matching identity field. Non-human sentinels and
// unrecognized kinds resolve to ("", "").
func ResolveGardenerOwner(ctx context.Context, c Client, operator string) (slackUserID, userBeadID string) {
	operator = strings.TrimSpace(operator)
	if operator == "" {
		return "", ""
	}
	kind, subject, ok := strings.Cut(operator, ":")
	if !ok || strings.TrimSpace(subject) == "" {
		return "", ""
	}
	subject = strings.TrimSpace(subject)

	var emailFilter, slackFilter string
	switch kind {
	case "slack":
		slackFilter = subject
		slackUserID = subject
	case "oidc", "gitlab", "jira":
		emailFilter = subject
	default:
		// system:/fleet:/sched- and any other non-human sentinel: ownerless.
		return "", ""
	}

	// Look up the matching user bead to resolve user_bead_id. A lookup miss is
	// non-fatal — we still stamp the slack id when we have one, so tier-2
	// resolution works even before the user bead exists.
	field, value := "slack_id", slackFilter
	if emailFilter != "" {
		field, value = "email", emailFilter
	}
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Fields:   map[string]string{field: value},
		Limit:    1,
	})
	if err == nil && res != nil && len(res.Beads) > 0 {
		ub := res.Beads[0]
		userBeadID = ub.ID
		// For email-shaped operators, derive the canonical slack id from the
		// resolved user bead so tier-2 resolution works too.
		if slackUserID == "" {
			if sid := strings.TrimSpace(ub.Fields["slack_id"]); sid != "" {
				slackUserID = sid
			}
		}
	}
	return slackUserID, userBeadID
}

// IsGardenerBead reports whether a candidate trigger/schedule bead is a gardener
// loop, independent of the `role` field value. A gardener trigger may set
// role=<other> so its *spawned agents* take a specific role (e.g. a plan
// gardener's role=plan-gardener), yet the bead itself still carries the
// `role:gardener` label + a gardener blob. Resolvers must key off the gardener
// markers, not the spawn-role field, or those gardeners become unresolvable
// (ka-0000005048). Mirrors beads-viewer's isGardenerBeadView
// (cmd/beads-viewer/gardener_beadview.go) so the surfaces never drift.
func IsGardenerBead(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if b.Fields["role"] == "gardener" {
		return true
	}
	if slices.Contains(b.Labels, "role:gardener") {
		return true
	}
	return strings.TrimSpace(b.Fields["gardener"]) != ""
}

// IsGardenerForEpic reports whether a candidate bead is a gardener attached to
// the given epic. Matches both the current trigger:schedule shape (epic
// pinned via the gardener-epic:<id> label) and the legacy type=schedule shape
// (epic in the assign_bead field).
func IsGardenerForEpic(b *beadsapi.BeadDetail, epicID string) bool {
	if b == nil || b.Fields == nil {
		return false
	}
	if !IsGardenerBead(b) {
		return false
	}
	if b.Fields["assign_bead"] == epicID {
		return true
	}
	return slices.Contains(b.Labels, GardenerEpicLabel(epicID))
}

// FindAnyGardenerForEpic returns a gardener bound to the epic regardless of its
// enabled state, or nil when none is bound. Bind adopts on this so the "one
// gardener per plan" invariant holds even when the gardener was bound PAUSED
// behind the approve→build gate (task ka-0000005350) — re-running the bind on
// a still-draft plan must adopt the existing paused gardener, not mint a
// duplicate. Field/label filtering happens client-side because the legacy
// assign_bead match has no server-side query.
func FindAnyGardenerForEpic(ctx context.Context, c Client, epicID string) (*beadsapi.BeadDetail, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger", "schedule"},
		Statuses: []string{"open", "in_progress", "blocked"},
	})
	if err != nil {
		return nil, fmt.Errorf("listing gardener beads: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	for _, b := range res.Beads {
		if b == nil || b.Fields == nil {
			continue
		}
		if !IsGardenerForEpic(b, epicID) {
			continue
		}
		return b, nil
	}
	return nil, nil
}

// EnabledOnBind computes the approve→build gate (task ka-0000005350, D5
// ka-0000003301, R6): if the plan explicitly entered the authoring lifecycle
// and is NOT yet approved for build (plan_state ∈ {draft, ready}), the
// gardener is born PAUSED so the scheduler never fires it behind the
// operator's back — "Approve & build" in the cockpit is what enables it. A
// plan that never opted into the lifecycle (no plan_state — every legacy /
// hand-marked plan) OR one already `building` keeps the historical
// enabled-on-bind behaviour, so this is backward compatible. Without this, an
// enabled-by-default gardener would defeat the cockpit gate: the toggle button
// would read "paused" but the loop would still fire on its cron.
func EnabledOnBind(plan *beadsapi.BeadDetail) bool {
	if plan == nil || plan.Fields == nil {
		return true
	}
	switch strings.TrimSpace(plan.Fields[planStateFieldKey]) {
	case "draft", "ready":
		return false
	}
	return true
}

// scheduleConditionJSON is the on-the-wire shape consumed by the trigger
// engine's scheduleCondition (internal/bridge/trigger_source_schedule.go).
// Re-declared here (as in cmd/gb/trigger_schedule.go) because the engine's
// type is unexported.
type scheduleConditionJSON struct {
	Type     string   `json:"type"`
	Op       string   `json:"op,omitempty"`
	Value    string   `json:"value,omitempty"`
	Types    []string `json:"types,omitempty"`
	Statuses []string `json:"statuses,omitempty"`
	// Parent scopes a bead_query to the transitive subtree under a parent
	// bead-id — the gardener pre-flight's open-children gate.
	Parent string `json:"parent,omitempty"`
	// ExcludeBlockedByOpenTypes subtracts from the open-children count any
	// child that is transitively blocked_by an OPEN bead of one of these
	// types (escalation / decision). The daemon (conditions/leaf_daemon.go,
	// ka-0000004214) walks the forward blocking-dep chain to a fixpoint, so a
	// child gated several hops behind an open operator escalation is correctly
	// treated as non-actionable. Wired into the pre-flight so a plan whose
	// ENTIRE open frontier is operator-gated stops waking a spin-only run every
	// tick (ka-0000000318). Fail-safe: any uncertainty leaves the child counted,
	// so the gardener keeps waking while real work may remain.
	ExcludeBlockedByOpenTypes []string `json:"exclude_blocked_by_open_types,omitempty"`
}

// BindSpec parameterizes Bind. Every field is optional: zero values take the
// plan-gardener defaults (event mode with a staggered daily watchdog,
// DefaultMaxRuns, playbook prompt, project inherited from the plan's labels).
// `gb plan gardener` threads its flags through here; the viewer's auto-bind
// passes only Operator+CreatedBy.
type BindSpec struct {
	// Project scopes the gardener (project:<name> label + fields.project).
	// Empty inherits from the plan epic's project label / project field.
	Project string
	// Operator is the operated-by alias (slack:U…, oidc:…) attributed as the
	// gardener's owner inside the loop blob (ka-0000003284). Empty or a
	// non-human sentinel leaves the gardener ownerless.
	Operator string
	// CreatedBy is the actor recorded on the trigger bead.
	CreatedBy string
	// Cron opts into TIMED mode (trigger:schedule) with this cron. Empty, and
	// Timed false, authors the default EVENT mode gardener (gd-gsvlS6YDj3).
	Cron string
	// Timed opts into TIMED mode with the deterministic staggered 30m cron
	// seeded from the plan epic id (or Cron, when set). Owner ruling
	// 2026-11-01: timed plan gardeners ran 779 times a day and at least 80% of
	// those runs dispatched nothing, so new plan gardeners start in event mode
	// and the clock is opt-in.
	Timed bool
	// FallbackCron overrides the event-mode watchdog cron (default: a
	// deterministic staggered daily cron seeded from the plan epic id). It is a
	// usage error together with Cron or Timed.
	FallbackCron string
	// Timezone overrides DefaultTimezone.
	Timezone string
	// MaxRuns overrides DefaultMaxRuns (<=0 takes the default).
	MaxRuns int
	// LoopWallClock overrides DefaultLoopWallClock.
	LoopWallClock string
	// Title overrides the "Plan gardener for <id> (<title>)" default.
	Title string
	// SlackChannel routes iteration agent cards (schedule_slack_channel).
	SlackChannel string
	// Prompt overrides the rendered playbook prompt (advanced).
	Prompt string
	// ExecutionTarget pins the iterations' runtime backend (ka-0000003026),
	// e.g. lambda_microvm; empty inherits the role/project default.
	ExecutionTarget string
}

// BindResult reports what Bind did.
type BindResult struct {
	// ID is the gardener trigger bead — freshly created, or the adopted
	// existing gardener's id.
	ID string
	// Adopted is true when an existing gardener was found and no bead was
	// created (the idempotent path). Existing then carries that bead.
	Adopted  bool
	Existing *beadsapi.BeadDetail
	// SpawnRole is fields.role on the gardener — SpawnRole for a fresh bind,
	// the existing bead's (possibly stale) role field on adopt.
	SpawnRole string
	// Enabled reports the born enabled/paused state of a fresh bind (the
	// approve→build gate verdict). Meaningless on adopt.
	Enabled bool
	// Effective parameters of a fresh bind. Mode is the firing mode the
	// gardener was born in; Cron is its effective cadence — the cron field in
	// timed mode, the fallback-watchdog cron in event mode (the same meaning as
	// gardenermode.Result.Cron).
	Mode     gardenermode.Mode
	Cron     string
	Timezone string
	MaxRuns  int
	Project  string
}

// Bind idempotently binds the ONE durable plan-gardener to a /plans plan epic:
// the shared core behind `gb plan gardener`, the POST /plans auto-bind, and
// the hand-mark path (task ka-0000002863). The caller passes the already
// loaded plan bead; Bind validates the plan shape, adopts any existing
// gardener, and otherwise mints the type=trigger / action=spawn_agent gardener
// bead — born paused whenever the plan's authoring lifecycle forbids enabling
// (EnabledOnBind).
//
// The gardener is born in EVENT mode (trigger:multi + the
// gardenermode.EventTriggers array: plan-activity bead events plus a daily
// fallback-watchdog cron carrying the open-children pre-flight), the same shape
// `gb gardener mode --event` writes. BindSpec.Cron or BindSpec.Timed opts into
// the TIMED shape (trigger:schedule + cron) instead.
func Bind(ctx context.Context, c Client, plan *beadsapi.BeadDetail, spec BindSpec) (*BindResult, error) {
	if plan == nil || strings.TrimSpace(plan.ID) == "" {
		return nil, fmt.Errorf("a plan epic is required")
	}
	planID := plan.ID
	// The bind is plan-scoped by contract: a /plans plan is an epic wearing
	// the 'plan' label (cmd/beads-viewer/plans.go planLabel). Refuse anything
	// else so "the plan's one gardener" stays a meaningful invariant — generic
	// epic gardeners keep using `gb gardener start`.
	if plan.Type != "epic" {
		return nil, fmt.Errorf("%s is type=%s, not an epic — gb plan gardener binds gardeners to /plans plan epics only", planID, plan.Type)
	}
	if !slices.Contains(plan.Labels, PlanLabel) {
		return nil, fmt.Errorf("%s does not carry the 'plan' label — mark it as a plan on /plans (or `kd label add %s plan`), or use `gb gardener start` for a plain epic gardener", planID, planID)
	}

	// Idempotent-adopt: one gardener per plan. A gardener already pinned to this
	// epic (gardener-epic label / legacy assign_bead) is THE plan's gardener —
	// report and succeed instead of duplicating the loop, REGARDLESS of
	// enabled state, so a gardener bound PAUSED behind the approve→build gate
	// is still recognised on a re-mark / double-submit / retry.
	if existing, err := FindAnyGardenerForEpic(ctx, c, planID); err != nil {
		return nil, err
	} else if existing != nil {
		return &BindResult{
			ID:        existing.ID,
			Adopted:   true,
			Existing:  existing,
			SpawnRole: strings.TrimSpace(existing.Fields["role"]),
		}, nil
	}

	project := strings.TrimSpace(spec.Project)
	if project == "" {
		project = inheritProjectFromBead(plan)
	}
	// Authoring-time project guard (ka-0000007480, SEV-1 residual of friction
	// report ka-0000004281). A project-less plan gardener would mint a trigger
	// with NO project field; the failure only surfaces later at fire time as a
	// repo-/credential-less agent stuck in the "Please run /login" loop. Fail
	// loudly HERE — before any CreateBead — so no project-less gardener persists.
	if project == "" {
		return nil, fmt.Errorf("cannot bind gardener to plan %s: project is empty; "+
			"pass --project, set $KD_PROJECT or $BOAT_PROJECT, or add a project: label to the plan epic", planID)
	}

	terminal := fmt.Sprintf("All steps of plan %s are closed (or the operator has retired the plan).", planID)

	prompt := strings.TrimSpace(spec.Prompt)
	if prompt == "" {
		var err error
		prompt, err = RenderPrompt(planID)
		if err != nil {
			return nil, err
		}
	}
	loopWall := firstNonEmpty(strings.TrimSpace(spec.LoopWallClock), DefaultLoopWallClock)
	maxRuns := spec.MaxRuns
	if maxRuns <= 0 {
		maxRuns = DefaultMaxRuns
	}
	prompt = AppendExitGuidance(prompt, ExitGuidance{
		EpicID:        planID,
		Terminal:      terminal,
		LoopWallClock: loopWall,
		RunWallClock:  defaultRunWallClock,
	})

	title := strings.TrimSpace(spec.Title)
	if title == "" {
		if t := strings.TrimSpace(plan.Title); t != "" {
			title = fmt.Sprintf("Plan gardener for %s (%s)", planID, t)
		} else {
			title = fmt.Sprintf("Plan gardener for %s", planID)
		}
	}

	// Firing mode: event by default, timed on an explicit Cron or Timed.
	mode, cron, fallbackCron, err := bindCadence(planID, spec)
	if err != nil {
		return nil, err
	}
	tz := strings.TrimSpace(spec.Timezone)
	if tz == "" {
		tz = DefaultTimezone
	}

	// Pre-flight: fire only while the plan still has AGENT-ACTIONABLE open
	// work. The base gate is the open-children==0 dormancy `gb gardener start`
	// emits — once every step closes the trigger goes dormant instead of
	// spawning idle iterations. exclude_blocked_by_open_types additionally
	// subtracts children that are transitively blocked_by an open
	// escalation/decision, so a plan whose ENTIRE open frontier is waiting on an
	// operator does NOT keep waking a spin-only run every tick — the recurring
	// low-yield failure mode this fixes (ka-0000000318: 26% productive runs, the
	// rest empty operator-gated wakes). The gate self-heals: the subtree
	// bead_event trigger re-fires the moment the operator closes the gate, so
	// the gardener is NOT closed (unlike a loop_end match) and picks work back up
	// without any manual re-enable. A timed gardener's schedule loader reads it
	// from the top-level conditions field; an event gardener carries it on the
	// watchdog entry as a per-entry preflight, and keeps the field too so a
	// later flip or cadence edit (gardenermode) carries it again.
	conds, err := json.Marshal([]scheduleConditionJSON{{
		Type:                      "bead_query",
		Op:                        ">",
		Value:                     "0",
		Parent:                    planID,
		Types:                     []string{"task", "bug", "epic"},
		Statuses:                  []string{"open", "in_progress", "blocked"},
		ExcludeBlockedByOpenTypes: []string{"escalation", "decision"},
	}})
	if err != nil {
		return nil, fmt.Errorf("marshalling plan-gardener pre-flight conditions: %w", err)
	}

	enabledOnBind := EnabledOnBind(plan)

	// Plan-agent seam: `role` here is the SPAWN role the engine fires
	// iterations in (actionSpawnAgent → SpawnAgent stamps fields.role + the
	// role:<role> label on the agent bead), so every iteration resolves as the
	// plan's bound agent. The trigger bead itself remains a gardener via the
	// role:gardener label + gardener blob below (ka-0000005048).
	fields := map[string]any{
		"enabled":          enabledOnBind,
		"action":           "spawn_agent",
		"role":             SpawnRole,
		"prompt":           prompt,
		"timezone":         tz,
		"conditions":       string(conds),
		"cooldown_minutes": 5,
		"max_concurrent":   1,
	}
	if project != "" {
		fields["project"] = project
	}
	modeLabel := gardenermode.LabelSchedule
	effectiveCron := cron
	if mode == gardenermode.ModeTimed {
		fields["cron"] = cron
	} else {
		// The exact shape `gb gardener mode --event` writes, minus timed_cron:
		// there is no earlier cron to restore, so a flip back to timed takes
		// the staggered 30m default, the cron this gardener would have had.
		triggers, err := json.Marshal(gardenermode.EventTriggers(planID, project, fallbackCron, tz, gardenermode.PreflightFromConditions(string(conds))))
		if err != nil {
			return nil, fmt.Errorf("marshalling plan-gardener triggers: %w", err)
		}
		fields["triggers"] = string(triggers)
		modeLabel = gardenermode.LabelMulti
		effectiveCron = fallbackCron
	}
	// Per-gardener runtime backend opt-in (ka-0000003026): the engine stamps
	// this onto every iteration's agent bead (agent-metadata precedence layer),
	// so ONE plan gardener can run on e.g. lambda_microvm without flipping the
	// whole plan-gardener role.
	if t := strings.TrimSpace(spec.ExecutionTarget); t != "" {
		fields["execution_target"] = t
	}

	capBlob := BuildGardenerLoopBlob(ctx, c, spec.Operator, LoopBlobParams{
		EpicID:          planID,
		Project:         project,
		Terminal:        terminal,
		MaxRuns:         maxRuns,
		LoopWallClock:   loopWall,
		OneSpawnAtATime: true,
		SlackChannel:    strings.TrimSpace(spec.SlackChannel),
	})
	if capBlob != "" {
		fields["gardener"] = capBlob
	}
	// Bounded-stop guard (advice ka-0000000974): fail closed before persisting.
	if v := bridge.ValidateGardenerBlob(capBlob); v != nil {
		return nil, fmt.Errorf("%s", v.Error())
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("marshalling plan-gardener trigger fields: %w", err)
	}

	labels := []string{modeLabel, "role:gardener", GardenerEpicLabel(planID), MarkerLabel}
	if project != "" {
		labels = append(labels, "project:"+project)
	}

	// Standing-marker guard (bug ka-0000004311): fail closed before persisting.
	// A plan gardener whose terminal_goal DECLARES it perpetual MUST carry a
	// machine-readable standing marker (gardener.standing:true OR perpetual:true
	// label); otherwise the engine treats it as epic-bounded and reaps it into a
	// closed-but-enabled dark gardener. Checked against the exact label set above.
	if v := bridge.ValidateGardenerStandingBlob(capBlob, labels); v != nil {
		return nil, fmt.Errorf("%s", v.Error())
	}

	id, err := c.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "trigger",
		Kind:      "data",
		Labels:    labels,
		Fields:    json.RawMessage(fieldsJSON),
		CreatedBy: spec.CreatedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("creating plan gardener trigger for %s: %w", planID, err)
	}

	return &BindResult{
		ID:        id,
		SpawnRole: SpawnRole,
		Enabled:   enabledOnBind,
		Mode:      mode,
		Cron:      effectiveCron,
		Timezone:  tz,
		MaxRuns:   maxRuns,
		Project:   project,
	}, nil
}

// Starter is the daemon surface Start needs: Bind's, plus the enable write.
type Starter interface {
	Client
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// Start binds the plan's gardener and makes sure it is enabled: the same
// end state as the cockpit's "Approve & build", for a plan already building.
// gb lands plan answer calls it when the owner approves a home-garden plan
// (step 9.11), so no operator binds or resumes it by hand. A fresh bind on a
// building plan is born enabled (EnabledOnBind); an adopted gardener that is
// paused is enabled, with an audit comment, and enabled reports that write.
// A plan not yet building is refused before anything is written: the daemon's
// gardener-enable gate would refuse the enable anyway.
func Start(ctx context.Context, c Starter, plan *beadsapi.BeadDetail, spec BindSpec) (res *BindResult, enabled bool, err error) {
	if !EnabledOnBind(plan) {
		return nil, false, fmt.Errorf("plan %s is %s; its gardener starts once it is building", plan.ID, plan.Fields[planStateFieldKey])
	}
	res, err = Bind(ctx, c, plan, spec)
	if err != nil {
		return nil, false, err
	}
	if !res.Adopted || res.Existing == nil || strings.TrimSpace(res.Existing.Fields["enabled"]) != "false" {
		return res, false, nil
	}
	if err := c.UpdateBeadFieldsTyped(ctx, res.ID, map[string]any{"enabled": true}); err != nil {
		return res, false, fmt.Errorf("enabling plan gardener %s: %w", res.ID, err)
	}
	_ = c.AddComment(ctx, res.ID, spec.CreatedBy, fmt.Sprintf("enabled → true by %s: plan %s is building", spec.CreatedBy, plan.ID))
	return res, true, nil
}

// inheritProjectFromBead picks the project label off the plan epic when one
// is present (e.g. project:gardenio), falling back to its project field.
// Unlike cmd/gb's helper of the same name there is NO environment fallback —
// callers with an ambient default project (gb) resolve it before Bind.
func inheritProjectFromBead(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	for _, l := range b.Labels {
		if rest, ok := strings.CutPrefix(l, "project:"); ok {
			return rest
		}
	}
	return b.Fields["project"]
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// bindCadence resolves a bind's firing mode and its crons. Event is the
// default (gd-gsvlS6YDj3); an explicit Cron or Timed opts into the clock.
// Every cron here is deterministic and staggered, seeded from the plan epic
// id, so no shared literal can herd the fleet (spec
// 2026-07-13-gardener-mode-flip-design.md). The daily event-mode watchdog is
// a floor so a plan never goes fully silent if the event stream drops; plan
// activity is what wakes it day to day.
func bindCadence(planID string, spec BindSpec) (mode gardenermode.Mode, cron, fallbackCron string, err error) {
	cron = strings.TrimSpace(spec.Cron)
	fallbackCron = strings.TrimSpace(spec.FallbackCron)
	if cron == "" && !spec.Timed {
		if fallbackCron == "" {
			fallbackCron = gardenermode.StaggeredCron(planID, 24*time.Hour)
		}
		return gardenermode.ModeEvent, "", fallbackCron, nil
	}
	if fallbackCron != "" {
		return "", "", "", fmt.Errorf("a fallback cron applies only to an event-mode gardener; drop it, or drop the timed cron")
	}
	if cron == "" {
		cron = gardenermode.StaggeredCron(planID, 30*time.Minute)
	}
	return gardenermode.ModeTimed, cron, "", nil
}
