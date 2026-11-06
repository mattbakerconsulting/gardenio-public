// Package main — `gb gardener` subcommand: ergonomic CLI over the
// gardener-loop primitive (role=gardener).
//
// Plan-of-record: gardenio/plans/epic-mention-looping-gardener.md §T6.
//
// Phase 3b/D10 removed type=schedule firing entirely: the trigger-engine
// only loads and fires `type=trigger` beads. `gb gardener start` therefore
// emits a `type=trigger` / `trigger:schedule` / `action=spawn_agent` bead
// (the same shape as the known-live gardeners ka-0000001510 / ka-0000002589),
// pinned to its epic via a `gardener-epic:<id>` label and a cron pre-flight
// that fires only while the epic still has open children. See bug
// ka-0000005960 — a bare type=schedule gardener loads into no trigger source
// and silently never runs.
//
// `gb gardener start <bead-id>` is idempotent: if an enabled gardener already
// exists for the epic it exits non-zero with a link to the existing gardener.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
	"gardenio/controller/internal/scheduler/progress"

	"github.com/spf13/cobra"
)

var gardenerCmd = &cobra.Command{
	Use:     "gardener",
	Aliases: []string{"captain"}, // pre-rename name: live trigger prompts still say `gb captain ...` (gd-5O9QUlA8zr)
	Short:   "Manage epic-gardener scheduled loops",
	GroupID: "orchestration",
	Long: `Subcommands for the looping-gardener primitive.

A gardener is a type=trigger / trigger:schedule bead with role=gardener,
action=spawn_agent, pinned to a parent ("epic") bead via a gardener-epic:<id>
label. A cron pre-flight fires the gardener while the epic still has open
children. See gardenio/plans/epic-mention-looping-gardener.md and
gardenio/docs/gardener-loop-runbook.md for the design and runbook.`,
}

// gardenerPlaybookPrompt is the appendix prompt from
// gardenio/plans/epic-mention-looping-gardener.md — the gardener's playbook
// for an epic loop. The {{assign_bead}} placeholder is rendered at start
// time so the prompt stored on the bead names the parent explicitly.
const gardenerPlaybookPrompt = `You are the gardener for parent bead {{.AssignBead}}.

Read your "## Scheduled Loop Context", "## Loop Progress", and
"## Epic Loop Context" sections, then work this playbook:

1. Start with gb crew. It lists every agent working inside your epic,
   including crews a previous instance of you dispatched — those carry a
   different spawned_by, so your own spawn list would miss them. A task
   tagged [OWNED] has a live agent on it and is covered; leave it be
   rather than reclaiming it. Then for each open child with no owner:
   gb spawn --task <id> --role crew.

2. For each decision raised by my crew with awaiting=gardener: assess.
   When one option clearly dominates and artifact_type is not in
   {epic, plan, bug}, gb decision report <id> --content '...' and
   gb squawk the source thread with link+choice+rationale.

3. For each decision awaiting=human older than 4h whose reflagged_at is
   empty or older than 4h: gb decision reflag <id> --reason '...'
   (bumps its priority toward P0 and reposts it to Slack so the
   operator sees it again).

4. If you OWN an open decision awaiting a human that you need resolved
   before you can continue: gb yield — it blocks until the operator
   resolves it. Reserve yield for that case; yielding merely because
   crew is in-flight has nothing to wait on and will error, so let the
   scheduler respawn you instead.

5. Otherwise (crew dispatched, or nothing left to do this turn):
   FIRST kd close <your-claimed-cron-task-id> (or kd unclaim <id> if
   not complete) — the despawn gate rejects a gb stop while you still
   hold claimed in-progress work (Exit 1 "you have claimed in-progress
   work ... close or unclaim"), which costs a wasted retry turn. THEN
   gb stop --wrapup '{"tldr": "<one-line outcome>", "accomplishments":
   "...", "progress_score": <0|1|2>, "progress_summary": "..."}'. tldr
   is required. The pod is respawned on the next trigger, so dispatched
   crew are picked back up automatically.

The scheduler handles loop closure from the stop conditions, so stop
with goal_reached only when you've personally verified every child is
closed; otherwise the bead_query stop condition handles the common
case.`

// gardenerTemplateData is the parameterisation surface of the gardener
// playbook prompt template.
type gardenerTemplateData struct {
	AssignBead string
}

// renderGardenerPrompt expands the playbook prompt template for the given
// epic bead.
func renderGardenerPrompt(assignBead string) (string, error) {
	tmpl, err := template.New("gardener-prompt").Parse(gardenerPlaybookPrompt)
	if err != nil {
		return "", fmt.Errorf("parsing gardener prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, gardenerTemplateData{AssignBead: assignBead}); err != nil {
		return "", fmt.Errorf("executing gardener prompt template: %w", err)
	}
	return buf.String(), nil
}

// findExistingGardenerSchedule looks for an active gardener bead already
// assigned to the given epic. The current shape is a type=trigger /
// trigger:schedule bead labelled gardener-epic:<epic> (Phase 3b/D10 firing
// model, bug ka-0000005960); legacy type=schedule gardeners carrying an
// `assign_bead` field are still matched so the resolver works across the
// migration. Field/label filtering happens client-side because the kbeads
// list endpoint does not expose a fields= query parameter.
func findExistingGardenerSchedule(ctx context.Context, c *beadsapi.Client, assignBead string) (*beadsapi.BeadDetail, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger", "schedule"},
		Statuses: []string{"open", "in_progress", "blocked"},
	})
	if err != nil {
		return nil, fmt.Errorf("listing gardener beads: %w", err)
	}
	for _, b := range res.Beads {
		if b == nil || b.Fields == nil {
			continue
		}
		if !beadIsGardenerForEpic(b, assignBead) {
			continue
		}
		if b.Fields["enabled"] != "true" {
			continue
		}
		return b, nil
	}
	return nil, nil
}

// beadIsGardenerForEpic reports whether a candidate bead is a gardener attached
// to the given epic — the shared plangardener predicate under its historical
// cmd/gb name (it matches both the current trigger:schedule gardener-epic
// label shape and the legacy type=schedule assign_bead field).
func beadIsGardenerForEpic(b *beadsapi.BeadDetail, epicID string) bool {
	return plangardener.IsGardenerForEpic(b, epicID)
}

// beadIsGardener reports whether a candidate trigger/schedule bead is a gardener
// loop, independent of the `role` field value — the shared plangardener
// predicate under its historical cmd/gb name (ka-0000005048: resolvers key off
// the gardener markers, not the spawn-role field).
func beadIsGardener(b *beadsapi.BeadDetail) bool {
	return plangardener.IsGardenerBead(b)
}

// gardenerEpicOf returns the epic a gardener bead is attached to: the
// assign_bead field for legacy type=schedule gardeners, or the
// gardener-epic:<id> label for the current trigger:schedule shape.
func gardenerEpicOf(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if b.Fields != nil {
		if a := b.Fields["assign_bead"]; a != "" {
			return a
		}
	}
	for _, l := range b.Labels {
		if rest, ok := strings.CutPrefix(l, "gardener-epic:"); ok {
			return rest
		}
	}
	return ""
}

// ── gb gardener start ─────────────────────────────────────────────────────

var (
	gardenerStartCron       string
	gardenerStartTimezone   string
	gardenerStartProject    string
	gardenerStartMaxRuns    int
	gardenerStartLoopWall   string
	gardenerStartRunWall    string
	gardenerStartTitle      string
	gardenerStartTerminal   string
	gardenerStartPromptOver string
	gardenerStartMaxConc    int
	gardenerStartCooldown   string
	gardenerStartLabels     []string
	gardenerStartAckGuide   bool
	gardenerStartWakeOnMR   string
	gardenerStartStopOnMR   string
)

// gardenerGuideLabel / gardenerGuideID identify the canonical gardener-authoring
// guide advice the read-the-instructions gate echoes. The gate keys on the
// LABEL (which survives an advice-id change) and falls back to the well-known
// id only when no labelled bead resolves (ka-0000007415, guide bead
// ka-0000004752).
const (
	gardenerGuideLabel = "gardener-authoring-guide"
	gardenerGuideID    = "ka-0000004752"
)

var gardenerStartCmd = &cobra.Command{
	Use:   "start <bead-id>",
	Short: "Start (or resume) a gardener loop for an epic bead",
	Long: `Idempotently create the gardener trigger bead for <bead-id>.

The command first searches for any existing enabled gardener attached to the
epic. If one is found it exits non-zero with the link to the existing
gardener — the operator can "adopt" it without duplicating loops. Otherwise
a new type=trigger / trigger:schedule / action=spawn_agent bead is created
(Phase 3b/D10 firing model): the only shape the trigger-engine actually
fires. The epic is pinned via a gardener-epic:<id> label, and a cron
pre-flight fires the gardener only while the epic still has open children.

The default cron is a deterministic staggered 30-minute expression seeded
from the epic id (so default-cadence gardeners no longer synchronize into a
spawn herd), timezone America/Los_Angeles; override with --cron / --tz.

Authoring a gardener requires --i-read-the-gardener-instructions. Without the
flag the command refuses to create anything and echoes the canonical
gardener-authoring guide (advice ` + gardenerGuideID + `, label ` + gardenerGuideLabel + `)
inline so you load the five non-negotiable rules into context first. Re-run
with the flag once you have read it.`,
	Args: cobra.ExactArgs(1),
	RunE: runGardenerStart,
}

func runGardenerStart(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return fmt.Errorf("bead id is required")
	}
	ctx := cmd.Context()

	// Read-the-instructions gate (ka-0000007415, Matt's "echo a special advice
	// label's contents" idea). Authoring a gardener is a five-non-negotiable-rules
	// affair (self-block, cadence, blob-not-flat-fields, repo-less, prompts-as-
	// advice — advice ka-0000004752); a hand-rolled or half-read gardener becomes
	// a dark trigger the fleet then has to reap. Without the
	// --i-read-the-gardener-instructions flag, refuse to create anything and echo
	// the canonical guide inline so the author loads it into context FIRST. With
	// it, proceed. Suppress usage on this refusal so the guide is the only thing
	// printed — a cobra usage dump would bury it.
	if !gardenerStartAckGuide {
		cmd.SilenceUsage = true
		echoGardenerGuide(ctx, cmd.OutOrStdout())
		return fmt.Errorf(
			"refusing to create gardener for %s without --i-read-the-gardener-instructions: "+
				"read the guide echoed above, then re-run with the flag", beadID)
	}

	// Verify the parent bead exists. Better to fail early than create a
	// schedule pointing at a typo.
	parent, err := daemon.GetBead(ctx, beadID)
	if err != nil {
		return fmt.Errorf("looking up parent bead %s: %w", beadID, err)
	}

	// Anchor-type guard (bug ka-0000000141). A gardener drives an epic's open
	// CHILDREN to closed; its cron pre-flight and terminal goal are both
	// framed around a parent-child subtree. Anchoring one to a non-container
	// bead produces a wrong-shape generic gardener that spawns crews but never
	// steers the lane it was pointed at. The GetBead call above only proves the
	// anchor EXISTS (catches a typo); a valid-but-wrong-type bead sailed
	// straight through. The prod repro: `gb gardener start` on a type=trigger
	// (a standing release-owner gardener) minted a duplicate role:gardener
	// child-drain loop whose bogus gardener-epic label then made the resulting
	// dark trigger permanently unclearable by the engine's closed-enabled
	// sweep. Reject any anchor that is not an epic (or a poured molecule, which
	// acts as an epic with children — see beadsapi.IsPhaseContainer).
	if !beadsapi.IsPhaseContainer(parent) {
		msg := fmt.Sprintf(
			"cannot create gardener for %s: its type is %q, but a gardener must anchor to an epic "+
				"(or a poured molecule) whose open children it drives to closed",
			beadID, parent.Type)
		if parent.Type == "trigger" {
			// The most common mis-target: pointing at a standing gardener
			// trigger that already covers the lane. Enabling it is what the
			// operator actually wants, not a second gardener.
			msg += fmt.Sprintf("; %s is itself a trigger — enable it with `gb trigger enable %s`", beadID, beadID)
		}
		return fmt.Errorf("%s", msg)
	}

	// Idempotency: refuse to create a duplicate gardener.
	if existing, err := findExistingGardenerSchedule(ctx, daemon, beadID); err != nil {
		return err
	} else if existing != nil {
		if jsonOutput {
			printJSON(map[string]any{
				"id":          existing.ID,
				"assign_bead": beadID,
				"already":     true,
			})
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"gardener schedule already exists for %s: %s (%s)\n",
				beadID, existing.ID, existing.Title)
		}
		return fmt.Errorf("gardener schedule already exists for %s: %s", beadID, existing.ID)
	}

	maxRuns := gardenerStartMaxRuns
	if maxRuns <= 0 {
		maxRuns = 20
	}
	terminal := strings.TrimSpace(gardenerStartTerminal)
	if terminal == "" {
		terminal = fmt.Sprintf("All children of %s are closed.", beadID)
	}

	prompt := strings.TrimSpace(gardenerStartPromptOver)
	if prompt == "" {
		prompt, err = renderGardenerPrompt(beadID)
		if err != nil {
			return err
		}
	}
	loopWall := firstNonEmpty(strings.TrimSpace(gardenerStartLoopWall), "168h")
	runWall := firstNonEmpty(strings.TrimSpace(gardenerStartRunWall), "30m")

	// The gardener now carries a REAL structured stop blob inside the `gardener`
	// JSON field (the type:trigger schema was widened to admit it, ka-0000002027),
	// so the scheduler enforces loop_end / hard_stop / run_budget itself rather
	// than relying on the agent to read prose and self-terminate. We STILL fold
	// the loop-exit guidance into the prompt as a human-readable echo so the
	// gardener agent can see its own terminal goal and wall-clock caps in
	// `gb prime`. It names no run count: the engine re-arms the budget.
	prompt = appendGardenerExitGuidance(prompt, gardenerExitGuidance{
		EpicID:        beadID,
		Terminal:      terminal,
		LoopWallClock: loopWall,
		RunWallClock:  runWall,
	})

	project := strings.TrimSpace(gardenerStartProject)
	if project == "" {
		// Inherit from parent if labelled, else fall back to environment.
		project = inheritProjectFromBead(parent)
	}
	// Authoring-time project guard (ka-0000007480, SEV-1 residual of friction
	// report ka-0000004281). Without this, a project-less gardener silently mints
	// a trigger with NO project field; the failure only surfaces LATER at fire
	// time as a repo-/credential-less agent stuck in the "Please run /login"
	// loop (the failure mode the downstream spawn guards ka-0000007608 now
	// catch). Fail loudly HERE so no project-less trigger is ever persisted.
	if project == "" {
		return fmt.Errorf("cannot create gardener for %s: project is empty; "+
			"pass --project, set $KD_PROJECT or $BOAT_PROJECT, or add a project: label to the epic", beadID)
	}

	title := strings.TrimSpace(gardenerStartTitle)
	if title == "" {
		t := strings.TrimSpace(parent.Title)
		if t == "" {
			title = fmt.Sprintf("Gardener for %s", beadID)
		} else {
			title = fmt.Sprintf("Gardener for %s (%s)", beadID, t)
		}
	}

	cron := firstNonEmptyCron(gardenerStartCron, beadID)
	tz := firstNonEmptyTZ(gardenerStartTimezone)

	// Coerce cooldown/max_concurrent to integers before the API call. The
	// type:trigger config validates these as ints; sending a string or an
	// empty value 400s with "must be an integer" (ka-0000004844). Defaults
	// mirror `gb trigger create` (max-concurrent 3, cooldown 5m).
	cooldownMins, err := parseCooldownMinutes(gardenerStartCooldown)
	if err != nil {
		return err
	}
	maxConc := gardenerStartMaxConc
	if maxConc < 0 {
		return fmt.Errorf("--max-concurrent must be >= 0")
	}

	// Build the cron pre-flight: fire only while the epic still has open
	// children. This is the §5.1 cron preflight (and the bare-shape analogue
	// of the loop_end bead_query stop): once every descendant closes the
	// trigger goes dormant instead of spawning idle gardeners forever.
	conds, err := json.Marshal([]scheduleConditionJSON{{
		Type:     "bead_query",
		Op:       ">",
		Value:    "0",
		Parent:   beadID,
		Types:    []string{"task", "bug", "epic"},
		Statuses: []string{"open", "in_progress", "blocked"},
	}})
	if err != nil {
		return fmt.Errorf("marshalling gardener pre-flight conditions: %w", err)
	}

	// Emit a `type=trigger` / `trigger:schedule` / `action=spawn_agent` bead
	// — the only shape the trigger-engine actually fires after Phase 3b/D10
	// removed type=schedule firing (bug ka-0000005960). A bare type=schedule
	// gardener loads into NO trigger source and silently never runs.
	//
	// The gardener loop spec (assign_bead / stop / progress / run_budget /
	// terminal_goal) lives inside the nested `gardener` JSON blob: that is the
	// ONLY place the trigger-engine reads it (ParseTriggerDef decodes the blob
	// into a GardenerSpec; the flat top-level fields are inert for the engine).
	// The type:trigger config schema was widened (ka-0000002027) to admit the
	// `gardener` json field, so the blob persists without a 400.
	fields := map[string]any{
		"enabled":          true,
		"action":           "spawn_agent",
		"role":             "gardener",
		"prompt":           prompt,
		"cron":             cron,
		"timezone":         tz,
		"conditions":       string(conds),
		"cooldown_minutes": cooldownMins,
		"max_concurrent":   maxConc,
	}
	if project != "" {
		fields["project"] = project
	}

	// --wake-on-mr (task ka-0000005598): wake on the FIRST update to a specific
	// MR. This is the §8.2 hybrid recipe: a near-real-time bead_event trigger on
	// the MR's mirror bead (any created/updated/closed — a new commit, comment,
	// pipeline-status flip, or merge all restamp the type=merge-request mirror
	// bead and ride the SSE full-payload path) PLUS the existing cron as a slow
	// catch-up floor in case an SSE frame is dropped. Wiring the triggers[] array
	// flips the gardener to EVENT mode (trigger:multi), so below we swap the
	// trigger:schedule label for trigger:multi.
	wakeOnMR := strings.TrimSpace(gardenerStartWakeOnMR)
	eventMode := false
	if wakeOnMR != "" {
		triggersJSON, err := buildWakeOnMRTriggers(wakeOnMR, cron, tz, beadID)
		if err != nil {
			return err
		}
		fields["triggers"] = triggersJSON
		eventMode = true
	}

	// Build the REAL structured gardener blob the scheduler enforces. This makes
	// the mandatory self-block advice (ka-0000000974) the DEFAULT: every gardener
	// `gb gardener start` emits declares a run_budget AND a progress-aware /
	// bead_query loop_end, so it terminates when the epic drains, the run budget
	// is spent, or it stops making progress — not just when a pod crashes.
	//
	// Owner stamping (ka-0000003284, Gardener Page v2 R6) is merged into the same
	// blob: resolveGardenerOwnerBlob resolves the human operator (slack:U… /
	// oidc:… / gitlab:… / jira:…) into slack_user_id + user_bead_id, which tiers
	// 1-2 of the beads-viewer owner resolver read from inside the gardener blob.
	// E2E fixture / static gardeners (empty or non-human sentinel operator) stay
	// intentionally ownerless — only the loop spec is stamped, no owner fields.
	capBlob := buildGardenerLoopBlob(ctx, gardenerLoopBlobParams{
		EpicID:         beadID,
		Project:        project,
		Terminal:       terminal,
		MaxRuns:        maxRuns,
		LoopWallClock:  loopWall,
		StopOnMRMerged: strings.TrimSpace(gardenerStartStopOnMR),
	})
	if capBlob != "" {
		fields["gardener"] = capBlob
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshalling trigger fields: %w", err)
	}

	// Event-mode gardeners (--wake-on-mr) are loaded by the MultiTriggerSource,
	// which keys on the trigger:multi label and reads the triggers[] array; the
	// ScheduleTriggerSource (trigger:schedule) ignores triggers[] entirely
	// (ka-0000002562). So a wake-on-mr gardener MUST carry trigger:multi, not
	// trigger:schedule, or its MR event wake is silently dropped.
	modeLabel := "trigger:schedule"
	if eventMode {
		modeLabel = "trigger:multi"
	}
	labels := []string{modeLabel, "role:gardener", gardenerEpicLabel(beadID)}
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	for _, l := range gardenerStartLabels {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}

	// Repo-less git-path guard (bug ka-0000004796, task ka-0000004167): a
	// project-less gardener spawns a repo-less pod, so a prompt naming a git path
	// 404s at runtime. Warn (never reject — an override prompt may name the path
	// only to say "do NOT open it"). Inline into an advice/bundle instead.
	if paths, unreachable := bridge.FieldsUnreachableRepoPaths(fields); unreachable {
		fmt.Fprintln(os.Stderr, bridge.RepoPathWarning("", paths))
	}

	// Gardener bounded-stop guard (ka-0000006744). FAIL-CLOSED at authoring time
	// for ALL gardeners: a gardener MUST carry a positive run_budget AND a bounded
	// hard_stop wall_clock (advice ka-0000000974). The default `gb gardener start`
	// blob is always bounded (run_budget=max_runs, hard_stop
	// wall_clock=loop-wall-clock), so this only bites when an operator
	// hand-overrides those to an unbounded value (e.g. --max-runs 0 or
	// --loop-wall-clock forever). Refuse with a non-zero exit so the unbounded
	// gardener is never persisted.
	if v := bridge.ValidateGardenerBlob(capBlob); v != nil {
		return fmt.Errorf("%s", v.Error())
	}

	// Standing-marker guard (bug ka-0000004311). FAIL-CLOSED at authoring time: a
	// gardener whose terminal_goal DECLARES it perpetual ("STANDING …", "does NOT
	// self-complete") MUST carry a machine-readable standing marker
	// (gardener.standing:true OR the perpetual:true label) — the engine reads
	// standing-ness only from markers, never from prose. Without one it is
	// silently treated as epic-bounded and the max_runs ceiling / resume give-up
	// path REAP it into a closed-but-enabled dark gardener. `labels` already holds
	// any --label perpetual:true the author passed, so the guard sees the exact
	// marker set that will be persisted. Resolve by adding the marker, or
	// rephrasing the goal if this gardener is NOT perpetual.
	if v := bridge.ValidateGardenerStandingBlob(capBlob, labels); v != nil {
		return fmt.Errorf("%s", v.Error())
	}

	id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "trigger",
		Kind:      "data",
		Labels:    labels,
		Fields:    json.RawMessage(fieldsJSON),
		CreatedBy: actor,
	})
	if err != nil {
		return fmt.Errorf("creating gardener trigger for %s: %w", beadID, err)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":          id,
			"assign_bead": beadID,
			"max_runs":    maxRuns,
			"cron":        cron,
			"timezone":    tz,
			"project":     project,
		})
	} else {
		fmt.Printf("Created gardener trigger %s for %s\n", id, beadID)
		fmt.Printf("  cron:     %s (%s)\n", cron, tz)
		fmt.Printf("  max_runs: %d\n", maxRuns)
		if project != "" {
			fmt.Printf("  project:  %s\n", project)
		}
		if wakeOnMR != "" {
			fmt.Printf("  wake-on-mr: %s (event mode: trigger:multi)\n", wakeOnMR)
		}
		if s := strings.TrimSpace(gardenerStartStopOnMR); s != "" {
			fmt.Printf("  stop-on-mr-merged: %s (terminal loop_end)\n", s)
		}
	}
	return nil
}

// echoGardenerGuide prints the canonical gardener-authoring guide advice inline
// so the read-the-instructions gate loads it into the author's context before
// they re-run with --i-read-the-gardener-instructions. It resolves the guide by
// LABEL first (gardener-authoring-guide, which survives an advice-id change per
// the guide author's note on ka-0000007415), falling back to the well-known id
// (ka-0000004752) only when no labelled bead is found. Best-effort: any lookup
// failure degrades to a static pointer rather than blocking the refusal, so an
// offline daemon never wedges the gate.
func echoGardenerGuide(ctx context.Context, w io.Writer) {
	guide := resolveGardenerGuide(ctx)
	fmt.Fprintln(w, "──────────────────────────────────────────────────────────────────────")
	fmt.Fprintln(w, "gb gardener start refuses without --i-read-the-gardener-instructions.")
	fmt.Fprintln(w, "Read the canonical gardener-authoring guide below, then re-run with the flag.")
	fmt.Fprintln(w, "──────────────────────────────────────────────────────────────────────")
	if guide == nil {
		fmt.Fprintf(w, "\nCould not load the guide advice (label %q / %s) from the daemon.\n",
			gardenerGuideLabel, gardenerGuideID)
		fmt.Fprintln(w, "Read it directly: `gb advice show "+gardenerGuideID+"`")
		fmt.Fprintln(w, "Long-form: gardenio/docs/gardener-authoring-guide.md")
		return
	}
	fmt.Fprintf(w, "\n%s (%s)\n", guide.Title, guide.ID)
	if body := strings.TrimSpace(guide.Description); body != "" {
		fmt.Fprintf(w, "\n%s\n", body)
	}
	fmt.Fprintln(w, "\n──────────────────────────────────────────────────────────────────────")
	fmt.Fprintln(w, "Re-run: gb gardener start <epic> --i-read-the-gardener-instructions")
	fmt.Fprintln(w, "──────────────────────────────────────────────────────────────────────")
}

// resolveGardenerGuide fetches the gardener-authoring guide advice bead, keying
// on the label first and falling back to the well-known id. Returns nil on any
// failure (caller degrades gracefully).
func resolveGardenerGuide(ctx context.Context) *beadsapi.BeadDetail {
	if res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"advice"},
		Labels:   []string{gardenerGuideLabel},
		Statuses: []string{"open", "in_progress"},
		Limit:    1,
	}); err == nil && res != nil && len(res.Beads) > 0 {
		return res.Beads[0]
	}
	if b, err := daemon.GetBead(ctx, gardenerGuideID); err == nil {
		return b
	}
	return nil
}

// firstNonEmptyCron returns the user-supplied cron, or a deterministic
// staggered 30-minute default seeded from the gardener's bead id. The old
// literal default ("7,37 * * * *") synchronized every default-cadence
// gardener into a twice-hourly spawn herd (ka-0000003410 fallout, spec
// 2026-07-13-gardener-mode-flip-design.md).
func firstNonEmptyCron(s, seed string) string {
	if strings.TrimSpace(s) == "" {
		return gardenermode.StaggeredCron(seed, 30*time.Minute)
	}
	return s
}

// firstNonEmptyTZ returns the user-supplied timezone or the §5.1 default.
func firstNonEmptyTZ(s string) string {
	if strings.TrimSpace(s) == "" {
		return "America/Los_Angeles"
	}
	return s
}

// gardenerEpicLabel is the label that pins a gardener trigger to its epic —
// the shared plangardener helper under its historical cmd/gb name.
func gardenerEpicLabel(epicID string) string {
	return plangardener.GardenerEpicLabel(epicID)
}

// gardenerLoopBlobParams / buildGardenerLoopBlob are the shared plangardener
// loop-blob builder under their historical cmd/gb names — the default
// self-block mandated by advice ka-0000000974 (run_budget + bead_query/
// progress loop_end + hard_stop wall-clock), plus the owner attribution of
// ka-0000003284. The full rationale lives on plangardener.BuildGardenerLoopBlob.
type gardenerLoopBlobParams = plangardener.LoopBlobParams

func buildGardenerLoopBlob(ctx context.Context, p gardenerLoopBlobParams) string {
	return plangardener.BuildGardenerLoopBlob(ctx, daemon, operatedBy, p)
}

// buildWakeOnMRTriggers builds the triggers[] JSON array for a --wake-on-mr
// gardener (task ka-0000005598). It is the §8.2 hybrid recipe scoped to ONE MR:
//
//   - a bead_event entry that fires on any created/updated/closed to the MR's
//     mirror bead (matched by type=merge-request AND mr_url == the exact URL).
//     A new commit, comment, pipeline-status flip, or merge all restamp that
//     mirror bead and ride the SSE full-payload path, so the gardener wakes on
//     the FIRST such update within webhook + SSE delivery latency; and
//   - the gardener's own cron as a slow catch-up floor, carrying the open-
//     children pre-flight so the floor tick is skipped when the epic has
//     drained (mirrors gardenermode.setEvent's fallback-watchdog wiring).
//
// A throttle bounds a burst of restamps into one wake. epicID scopes the
// fallback cron's open-children pre-flight. Returns the marshalled JSON string
// for the `triggers` field.
func buildWakeOnMRTriggers(mrURL, cron, tz, epicID string) (string, error) {
	entries := []scheduler.TriggerEntry{
		{
			Kind:   scheduler.TriggerKindBeadEvent,
			Label:  "wake-on-mr",
			Events: []string{"created", "updated", "closed"},
			Match: &conditions.Expression{All: []conditions.Expression{
				{Leaf: conditions.Leaf{Field: "type", Op: "==", Value: "merge-request"}},
				{Leaf: conditions.Leaf{Field: "mr_url", Op: "==", Value: mrURL}},
			}},
			Throttle: "60s",
		},
	}
	// Fallback cron floor: only when the gardener carries a cron (the default
	// always does). Carries the open-children pre-flight so an idle floor tick is
	// suppressed once the epic drains — MultiTriggerSource does not read the
	// bead's top-level `conditions` field, so the gate must ride the entry as a
	// per-entry preflight (mirrors gardenermode.setEvent).
	if strings.TrimSpace(cron) != "" {
		entry := scheduler.TriggerEntry{
			Kind:     scheduler.TriggerKindCron,
			Label:    "fallback-watchdog",
			Cron:     cron,
			Timezone: tz,
		}
		if strings.TrimSpace(epicID) != "" {
			entry.Preflight = &conditions.Expression{Leaf: conditions.Leaf{
				Type:     "bead_query",
				Op:       ">",
				Value:    "0",
				Parent:   epicID,
				Statuses: []string{"open", "in_progress", "blocked"},
			}}
		}
		entries = append(entries, entry)
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshalling --wake-on-mr triggers: %w", err)
	}
	return string(raw), nil
}

// resolveGardenerOwner maps an operated-by alias to the gardener owner pair
// (slack_user_id, user_bead_id) via the shared plangardener resolver, bound to
// this CLI's daemon client.
func resolveGardenerOwner(ctx context.Context, operator string) (slackUserID, userBeadID string) {
	return plangardener.ResolveGardenerOwner(ctx, daemon, operator)
}

// gardenerExitGuidance / appendGardenerExitGuidance fold the loop-exit contract
// into the gardener prompt — the shared plangardener implementation under its
// historical cmd/gb names.
type gardenerExitGuidance = plangardener.ExitGuidance

func appendGardenerExitGuidance(prompt string, g gardenerExitGuidance) string {
	return plangardener.AppendExitGuidance(prompt, g)
}

// inheritProjectFromBead picks the project label off a parent bead when one
// is present (e.g. project:gardenio). Falls back to the gb default project.
func inheritProjectFromBead(b *beadsapi.BeadDetail) string {
	if b != nil {
		for _, l := range b.Labels {
			if rest, ok := strings.CutPrefix(l, "project:"); ok {
				return rest
			}
		}
		if p := b.Fields["project"]; p != "" {
			return p
		}
	}
	return defaultProject()
}

// ── gb gardener status ────────────────────────────────────────────────────

var gardenerStatusCmd = &cobra.Command{
	Use:   "status <bead-id>",
	Short: "Show gardener schedule status, last run, progress, bounds, and stop conditions",
	Long: `Pretty-print the gardener schedule attached to <bead-id>.

Resolves the schedule via (assign_bead=<bead-id>, role=gardener). Prints
the schedule core fields, last run / agent, progress windows (when the
T5 progress field is present on the bead), and the parsed stop block
(when the T4 stop field is present).

The 'bounded' line (and the --json bounded/bounds_reason fields) reports
whether the gardener carries its mandatory self-block (run_budget + a
hard_stop wall_clock), computed via the SAME bridge.ValidateGardenerBlob
guard the engine enforces at fire time. Consult this instead of eyeballing
the flat top-level fields (run_budget/stop/loop_end/terminal_goal), which
the daemon never reads and which are null on every normally-authored
gardener (bug ka-0000004852).

A '⚠️  DIVERGENCE' banner (and the --json field_divergences[] array) fires
when a flat self-block field is present AND disagrees with the nested
gardener blob the engine actually reads — the silent-no-op footgun of bug
ka-0000004562, where 'kd update --field terminal_goal=…' writes the inert
flat field so a tightened goal never takes effect. Edit the blob
('kd update <trigger> --field gardener=@file'), not the flat field.`,
	Args: cobra.ExactArgs(1),
	RunE: runGardenerStatus,
}

// gardenerRunStateView holds the runtime counters the scheduler keeps inside
// the nested `gardener` JSON blob (NOT the flat schedule fields). Each value is
// pre-stringified ("" when unknown) so callers can firstNonEmpty() it against
// the corresponding flat field without re-formatting.
type gardenerRunStateView struct {
	runsSpawned   string
	runsCompleted string
	maxRuns       string
	lastAgentID   string
	lastRun       string // most-recent run_log entry's end time (RFC3339), best-effort
}

// gardenerRunState parses the schedule bead's `gardener` blob and surfaces the
// engine-owned runtime counters gardenerPostFire writes there. `gb gardener
// status` historically read only the flat schedule fields (runs_spawned,
// last_run, …), which the scheduler never populates for gardeners — so a
// gardener that had genuinely fired N times rendered as spawned=0 / last_run=—.
// This reconciles the display with the authoritative blob (ka-0000005388).
func gardenerRunState(sched *beadsapi.BeadDetail) gardenerRunStateView {
	var v gardenerRunStateView
	raw := strings.TrimSpace(sched.Fields["gardener"])
	if raw == "" {
		return v
	}
	var spec bridge.GardenerSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return v
	}
	if spec.RunsSpawned > 0 {
		v.runsSpawned = strconv.Itoa(spec.RunsSpawned)
	}
	if spec.RunsCompleted > 0 {
		v.runsCompleted = strconv.Itoa(spec.RunsCompleted)
	}
	if m := firstPositive(spec.MaxRuns, spec.RunBudget); m > 0 {
		v.maxRuns = strconv.Itoa(m)
	}
	v.lastAgentID = strings.TrimSpace(spec.LastAgentID)
	// The blob has no dedicated last-fire timestamp, but the rolling run_log's
	// newest entry carries the most recent run's end time — a good proxy for
	// "last run" when the flat last_run field is absent.
	if rl, err := scheduler.ParseRunLog(string(spec.RunLog)); err == nil && len(rl) > 0 {
		last := rl[len(rl)-1]
		t := last.EndedAt
		if t.IsZero() {
			t = last.StartedAt
		}
		if !t.IsZero() {
			v.lastRun = t.UTC().Format(time.RFC3339)
		}
	}
	return v
}

// firstPositive returns the first argument greater than zero, or zero.
func firstPositive(vals ...int) int {
	for _, n := range vals {
		if n > 0 {
			return n
		}
	}
	return 0
}

// gardenerBoundedVerdict adjudicates whether a gardener schedule bead carries the
// engine-enforced BACKSTOP (advice ka-0000000974) by running the SAME guard the
// engine uses at fire time — bridge.ValidateGardenerBlob against the nested
// `gardener` JSON blob — NOT by eyeballing the flat top-level fields
// (run_budget / stop / loop_end / terminal_goal), which the daemon never reads
// and which are null on every normally-authored gardener.
//
// This is the single deterministic answer any surveyor (the trigger-reviver,
// an operator, a future evaluator) must consult so reviewer and engine can
// never disagree on what the BACKSTOP means (bug ka-0000004852): the reviver was
// false-negativing on EVERY bounded gardener because it inspected the inert flat
// fields instead of the blob that carries the real self-block.
//
// bounded is true when the blob passes the guard. reason is empty when bounded
// and otherwise carries the guard's actionable violation text (missing
// run_budget and/or hard_stop wall_clock). A bead with no `gardener` blob is
// reported unbounded with an explicit "no gardener loop spec" reason (the same
// fail-closed verdict the guard returns for an empty blob).
//
// NOTE: this is only the BACKSTOP axis. It is deliberately NOT the same question
// gardener-autoblock-audit.py answers — that tool scores the progress-aware
// SELF-BLOCK (advice ka-0000000974's intelligent loop_end), a genuinely
// different safety property. `gb gardener status` surfaces BOTH axes via
// bridge.ClassifyGardenerBlob (bug ka-0000000849); this helper stays as the pure
// backstop verdict for the engine-parity test and the one-line status header.
func gardenerBoundedVerdict(sched *beadsapi.BeadDetail) (bounded bool, reason string) {
	blob := strings.TrimSpace(sched.Fields["gardener"])
	if v := bridge.ValidateGardenerBlob(blob); v != nil {
		return false, v.Error()
	}
	return true, ""
}

// gardenerWallClockExpiryView describes where a gardener's hard_stop wall_clock
// window lands relative to now, measured from the SAME baseline the engine
// uses (max(created_at, grandfather epoch)). It is the readout behind the
// already-expired warning (ka-0000007024): the bounds guard only checks that a
// duration PARSES into (0, ceiling], never that the resulting expiry is in the
// FUTURE — so a gardener older than the grandfather epoch can pass the guard
// ("bounded: yes") yet re-close on the very next reconcile because its window,
// floored to the epoch, has already elapsed. This surfaces that at the exact
// surface an operator consults before reviving one.
type gardenerWallClockExpiryView struct {
	// present is true when the blob carries a parseable hard_stop wall_clock
	// AND the bead's created_at is known, so an expiry is computable.
	present bool
	// duration is the declared hard_stop wall_clock (the largest one, if the
	// gardener lists several — that is the effective ceiling).
	duration time.Duration
	// baseline is max(created_at, grandfather epoch) — the instant the engine
	// measures the window from.
	baseline time.Time
	// expiry is baseline+duration: when the hard_stop fires.
	expiry time.Time
	// grandfathered is true when created_at predates the epoch, so the
	// baseline was floored (the cohort the wrong-hint bug bites).
	grandfathered bool
	// expired is true when expiry is at/before now — the window has already
	// elapsed, so the gardener re-closes on the next reconcile.
	expired bool
}

// gardenerWallClockExpiryStatus computes where a bounded gardener's hard_stop
// wall_clock lands. It reads the largest hard_stop wall_clock duration from the
// nested `gardener` blob (the effective ceiling) and combines it with the bead's
// created_at via scheduler.GardenerWallClockExpirySince, so the CLI reproduces the
// engine's floored baseline exactly rather than re-deriving the epoch. Returns
// a zero-value view (present=false) when there is no computable expiry: no
// blob, no parseable hard_stop wall_clock, or an unknown created_at.
func gardenerWallClockExpiryStatus(sched *beadsapi.BeadDetail, now time.Time) gardenerWallClockExpiryView {
	var view gardenerWallClockExpiryView
	blob := strings.TrimSpace(sched.Fields["gardener"])
	if blob == "" || sched.CreatedAt.IsZero() {
		return view
	}
	var spec bridge.GardenerSpec
	if err := json.Unmarshal([]byte(blob), &spec); err != nil {
		return view
	}
	stop, err := scheduler.ParseStopSpec(strings.TrimSpace(string(spec.Stop)))
	if err != nil {
		return view
	}
	// The effective ceiling is the LARGEST hard_stop wall_clock — a gardener
	// that lists several closes when the last one elapses. A wall_clock in
	// loop_end/run_end is NOT a hard ceiling (mirrors hasBoundedHardStopWallClock).
	var maxDur time.Duration
	for _, e := range stop.HardStop.Any {
		if e.Kind != scheduler.StopKindWallClock {
			continue
		}
		d, perr := time.ParseDuration(strings.TrimSpace(e.Duration))
		if perr != nil || d <= 0 {
			continue
		}
		if d > maxDur {
			maxDur = d
		}
	}
	// Honor the per-bead wall_clock_since revival override (bug ka-0000006465):
	// a gardener rebaselined in place (by the engine reopen re-arm, or by
	// `gb trigger enable --rebaseline`, ka-0000005774) measures its window from
	// that instant, not its ancient created_at. Reading it here keeps the CLI
	// readout — and the enable guard that reuses this helper — from reporting a
	// stale EXPIRED on a gardener that was just given a genuine fresh window.
	//
	// Pass sched.ID and maxDur so the exported baseline includes the engine's
	// per-gardener STAGGER offset (ka-0000001868): the stagger is seeded from the
	// bead id (the same trigger bead the engine hashes) and scaled to the window,
	// so the CLI reproduces the engine's real expiry rather than the shared
	// unstaggered floor that false-alarmed EXPIRED on eight live gardeners.
	expiry, ok := scheduler.GardenerWallClockExpirySince(sched.ID, sched.CreatedAt, spec.WallClockSince, maxDur)
	if !ok {
		return view
	}
	view.present = true
	view.duration = maxDur
	view.baseline = scheduler.GardenerWallClockBaselineSince(sched.ID, sched.CreatedAt, spec.WallClockSince, maxDur)
	view.expiry = expiry
	// grandfathered ⇔ the epoch floor was actually applied, i.e. the latest
	// per-bead term (max(created_at, wall_clock_since)) predates the epoch. A
	// gardener rebaselined to now (wall_clock_since post-epoch) is never floored,
	// so it must not be flagged grandfathered. Test that floor CONDITION directly
	// rather than inferring it from the baseline: now that view.baseline carries
	// the stagger offset (ka-0000001868), reading "was it floored?" off the
	// baseline value would couple this flag to the stagger arithmetic — the floor
	// condition is the primitive fact, so assert it as such.
	latest := sched.CreatedAt
	if spec.WallClockSince.After(latest) {
		latest = spec.WallClockSince
	}
	view.grandfathered = !latest.IsZero() && latest.Before(scheduler.GardenerWallClockGrandfatherEpoch())
	view.expired = !expiry.After(now)
	return view
}

// jsonValue renders the wall-clock expiry view for `gb gardener status --json`.
// Returns nil (a JSON null) when no expiry is computable, so a consumer can
// cleanly distinguish "not applicable" from a real, present readout.
func (v gardenerWallClockExpiryView) jsonValue() any {
	if !v.present {
		return nil
	}
	return map[string]any{
		"duration":      v.duration.String(),
		"baseline":      v.baseline.UTC().Format(time.RFC3339),
		"expiry":        v.expiry.UTC().Format(time.RFC3339),
		"grandfathered": v.grandfathered,
		"expired":       v.expired,
	}
}

// gardenerSelfBlockFields are the self-block keys that exist in BOTH a flat
// top-level trigger field AND the nested `gardener` JSON blob. Each pair is a
// booby trap: the engine reads ONLY the blob (ParseTriggerDef -> GardenerSpec),
// so the flat copy is inert — yet `kd update <trigger> --field terminal_goal=…`
// writes the FLAT one, and both render side-by-side in `kd show`. The obvious
// way to tighten a gardener's goal therefore writes a dead field while the
// engine keeps using the stale blob value, and nothing warns (bug
// ka-0000004562). `flat` is the top-level field name; `blobJQ` is the jq-ish
// dotted path into the decoded blob (only `stop.loop_end` nests today).
var gardenerSelfBlockFields = []struct {
	flat   string // top-level bead field name
	blobJQ string // path into the gardener blob
}{
	{flat: "terminal_goal", blobJQ: "terminal_goal"},
	{flat: "run_budget", blobJQ: "run_budget"},
	{flat: "stop", blobJQ: "stop"},
	{flat: "loop_end", blobJQ: "stop.loop_end"},
}

// gardenerFieldDivergence is one flat-field-vs-blob mismatch surfaced by
// gardenerSelfBlockDivergences: the inert flat copy holds `flat`, the
// authoritative blob holds `blob`.
type gardenerFieldDivergence struct {
	Field string `json:"field"` // the flat field name (e.g. "run_budget")
	Flat  string `json:"flat"`  // value in the inert flat top-level field
	Blob  string `json:"blob"`  // value the engine actually reads (from the blob)
}

// gardenerSelfBlockDivergences reports every self-block field whose inert flat
// top-level copy DISAGREES with the authoritative nested `gardener` blob — the
// silent-no-op footgun of bug ka-0000004562. A field is flagged ONLY when its
// flat copy is present (non-empty) AND differs from the blob value the engine
// reads; a normally-authored gardener (all flat self-block fields null, only the
// blob populated by `gb gardener start`) therefore produces ZERO divergences, so
// this never false-positives on the common shape. The comparison is
// JSON-value-aware for structured fields (stop / loop_end) so cosmetic
// key-order/whitespace differences are not mistaken for a real divergence; the
// scalar fields (terminal_goal, run_budget) compare canonicalized string forms.
func gardenerSelfBlockDivergences(sched *beadsapi.BeadDetail) []gardenerFieldDivergence {
	blobRaw := strings.TrimSpace(sched.Fields["gardener"])
	if blobRaw == "" {
		// No blob to disagree with — a legacy/bare gardener. gardenerBoundedVerdict
		// already reports it unbounded; there is no two-copies footgun here.
		return nil
	}
	var blob map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blobRaw), &blob); err != nil {
		// An unparseable blob is fail-closed by the bounded verdict elsewhere;
		// we cannot meaningfully diff against it, so report nothing here.
		return nil
	}

	var out []gardenerFieldDivergence
	for _, f := range gardenerSelfBlockFields {
		flatVal := strings.TrimSpace(sched.Fields[f.flat])
		if flatVal == "" {
			continue // flat copy never set — the safe, normal case.
		}
		flatCanon := canonicalizeJSONValue(flatVal)
		blobCanon := canonicalizeJSONValue(gardenerBlobPath(blob, f.blobJQ))
		if flatCanon != blobCanon {
			out = append(out, gardenerFieldDivergence{
				Field: f.flat,
				Flat:  flatCanon,
				Blob:  blobCanon,
			})
		}
	}
	return out
}

// gardenerBlobPath extracts a dotted path (e.g. "stop.loop_end") out of the
// decoded gardener blob as its RAW JSON bytes, or "" when any path segment is
// absent. Canonicalization is left to the caller so each value is normalized
// exactly once. Only one level of nesting is needed today (stop.loop_end), but
// the walk generalizes.
func gardenerBlobPath(blob map[string]json.RawMessage, path string) string {
	segs := strings.Split(path, ".")
	cur := blob
	for i, seg := range segs {
		raw, ok := cur[seg]
		if !ok {
			return ""
		}
		if i == len(segs)-1 {
			return string(raw)
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(raw, &next); err != nil {
			return "" // intermediate segment is not an object — path is absent.
		}
		cur = next
	}
	return ""
}

// canonicalizeJSONValue returns a stable, comparison-safe string form of one
// self-block value, normalizing across the two ways the SAME value is encoded
// in the flat field vs the blob:
//
//   - A JSON string ("All children closed.") -> its decoded CONTENT
//     (All children closed.), so a blob's quoted terminal_goal compares equal
//     to the flat field's bare prose (which is stored unquoted in the Fields
//     map and does not parse as JSON).
//   - A JSON number/object/array -> its key-sorted, whitespace-free re-marshal,
//     so {"a":1,"b":2} == {"b":2,"a":1} and the flat integer 60 == a blob 60.
//   - Anything that does not parse as JSON (bare prose) -> the trimmed raw.
//
// This makes the divergence check robust to cosmetic encoding differences and
// flag ONLY genuine value mismatches.
func canonicalizeJSONValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s // not JSON (e.g. a plain terminal_goal sentence) — compare raw.
	}
	if str, ok := v.(string); ok {
		return str // JSON string -> its decoded content (unquoted).
	}
	// json.Marshal sorts map keys, giving a canonical form regardless of the
	// original key order or whitespace.
	if enc, err := json.Marshal(v); err == nil {
		return string(enc)
	}
	return s
}

func runGardenerStatus(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return fmt.Errorf("bead id is required")
	}
	ctx := cmd.Context()

	sched, err := resolveGardenerSchedule(ctx, daemon, beadID)
	if err != nil {
		return err
	}

	// The scheduler records runtime counters (runs_spawned, runs_completed,
	// last_agent_id, max_runs) inside the nested `gardener` JSON blob via
	// gardenerPostFire — it does NOT mirror them into the flat schedule fields.
	// Read the blob and fall back to it so a gardener that has actually fired N
	// times doesn't render as spawned=0 / last_run=— (which made a live,
	// progressing gardener look asleep — see ka-0000005388).
	rs := gardenerRunState(sched)

	// Resolve the run counters from the two mutable sources (flat field wins
	// over the nested blob), then reconcile each against GROUND TRUTH: the
	// actual agent beads this gardener spawned (bug ka-0000005640). Both mutable
	// counters can silently freeze at 0 — the flat field was never populated for
	// type=trigger gardeners, and gardenerPostFire's blob write was atomically
	// dropped when it PATCHed an integer-typed field as a string (ka-0000007976).
	// Deriving from the spawned beads makes the readout self-healing: a live
	// gardener can never again render spawned=0 while the engine is firing runs.
	// reconcileRunCount takes the MAX(counter, ground-truth) so a pruned-beads
	// gardener keeps its higher lifetime tally rather than regressing.
	spawnedStr := firstNonEmpty(sched.Fields["runs_spawned"], rs.runsSpawned)
	completedStr := firstNonEmpty(sched.Fields["runs_completed"], rs.runsCompleted)
	gt := gardenerGroundTruthCounts(ctx, daemon, sched.ID)
	spawnedStr = reconcileRunCount(spawnedStr, gt.Spawned, gt.Ok)
	completedStr = reconcileRunCount(completedStr, gt.Completed, gt.Ok)

	// Deterministic bounded-stop verdict, computed via the SAME guard the engine
	// uses at fire time (bridge.ValidateGardenerBlob against the nested `gardener`
	// blob). Surfacing it here gives any surveyor — notably the trigger-reviver —
	// an authoritative answer to "does this gardener carry the mandatory
	// self-block?" instead of eyeballing the inert flat fields (bug ka-0000004852).
	bounded, boundsReason := gardenerBoundedVerdict(sched)

	// Wall-clock expiry readout (bug ka-0000007024). The bounded verdict only
	// checks that a hard_stop wall_clock PARSES into (0, ceiling] — never that
	// its expiry, measured from the real (epoch-floored) baseline, is in the
	// future. A gardener older than the grandfather epoch can therefore be
	// "bounded: yes" yet already past its ceiling, so reviving it produces a
	// clean-looking reopen that re-closes on the next reconcile. Compute the
	// real baseline+expiry from created_at (the ONE place a call site has it,
	// which the pure ValidateGardenerBounds does not) and warn when it has
	// already elapsed.
	wallExpiry := gardenerWallClockExpiryStatus(sched, time.Now())

	// Two-axis safety verdict (bug ka-0000000849). "bounded" above is only the
	// BACKSTOP (run_budget + hard_stop wall_clock — the engine's emergency
	// brake). gardener-autoblock-audit.py scores a genuinely different property:
	// the progress-aware SELF-BLOCK (an intelligent loop_end per advice
	// ka-0000000974). The two disagreed silently — same bead, "bounded: NO" here
	// yet "ok" in the audit — so a surveyor reading one tool saw a different
	// offender set than the other. Surface BOTH axes off the SAME decoded blob so
	// the two verdicts are consistent and no one mistakes one for the other.
	safety := bridge.ClassifyGardenerBlob(strings.TrimSpace(sched.Fields["gardener"]))

	// D6 loop-hygiene score (ka-0000000452). The effectiveness rubric's D6
	// dimension is a STATIC checklist over the gardener's own config — the exact
	// fields the structured-stop work made first-class. Compute it here off the
	// SAME decoded blob the safety axes use, so `gb gardener status` renders the
	// deterministic D6 the meta-gardener scores instead of the operator eyeballing
	// the rubric. Purely advisory — it changes no engine behavior.
	d6 := bridge.ScoreGardenerD6Blob(strings.TrimSpace(sched.Fields["gardener"]), sched.Fields["cron"])

	// D1 progress-yield score (ka-0000004982). The rubric's most-weighted
	// dimension (w30) — the fraction of recent wakes that made progress — was
	// hand-computed by the meta-gardener and read a FALSE 0 for every gardener in
	// the 2026-08-26 scorecard because the scorer looked for the per-run score in
	// run_log (always null there) or in state.Events (empty for a
	// wrapup_field-only spec). Compute it here off the SAME progress_state blob
	// the engine persists, implementing the rubric's "score>=1 OR credited-event"
	// union, so the meta-scorer reads a deterministic `.d1` instead of re-deriving
	// a wrong 0. Purely advisory — it changes no engine behavior. HasRuns=false
	// means UNMEASURABLE (no runs recorded yet), NOT a "pure spin" D1=0 grade.
	d1 := bridge.ScoreGardenerD1Blob(strings.TrimSpace(sched.Fields["gardener"]), 0)

	// D2 throughput score + the D1/D2 consistency guard (task ka-0000003456,
	// Phase 5). D2 is the rubric's independent cross-check on D1 — it reads the
	// SAME fact (the epic's children closing) from net epic closures rather than
	// per-run credit — and the guard (rubric §2.1) is MANDATORY: if D2 shows
	// children actively closing (D2>=4) but the raw D1 yield landed below 3, the
	// D1 count is definitionally a measurement bug (the async delegator false-0,
	// ka-0000003502), so D1 is clamped up to 3. Compute D2 off the same graph the
	// meta-scorer would read (children closed in the window / open now), apply
	// the guard here, and surface both so the guard is deterministic instead of
	// eyeballed. Best-effort: a daemon-query failure leaves an EpicDrained-zero
	// D2 (the guard then no-ops) rather than failing the whole status readout.
	d2 := gardenerD2Verdict(ctx, daemon, gardenerEpicOf(sched), gardenerD2Window)
	d1 = bridge.ApplyD1D2Guard(d1, d2)

	// Cron-preflight-gating verdict (ka-0000006215). The meta-scorer judges a
	// gardener's compute economy partly on whether its scheduled (cron) wakes are
	// gated by a world-state preflight — but WHERE that gate lives depends on the
	// trigger SHAPE, and the scorer kept reading the wrong field: on a
	// trigger:multi gardener the gate is the per-entry triggers[].preflight (the
	// engine's fail-closed cron gate), while the flat top-level `conditions` field
	// is inert (read only by trigger_source_schedule.go). Reading the empty
	// legacy `conditions=[]` produced a false "no preflight gate / fires hourly
	// into empty gb stop calls" C grade + duplicate tune-tasks against a gardener
	// whose per-entry preflight already carried the exact gate. Compute the
	// shape-aware verdict here off the SAME fields the engine dispatches on, so
	// the scorer reads `.cron_gating` instead of hand-inspecting the wrong field.
	// Purely advisory — changes no engine behavior.
	cronGating := bridge.ScoreGardenerCronGating(
		sched.Fields["triggers"], sched.Fields["cron"], sched.Fields["conditions"])

	// progress_summary co-require conformance (audit finding F2, ka-0000005466).
	// The wrap-up enforcer pairs progress_score with progress_summary, but that
	// hard-require holds only under in_wrapup+hard enforcement; --force stops and
	// soft schedules leak, so a run can record a non-zero score with an empty
	// summary. Count those gaps off the rolling progress_state so `gb gardener
	// status` (and the meta scorecard reading its --json) flags the hygiene slip
	// deterministically instead of eyeballing per-run records. Purely advisory.
	pState, _ := progress.ParseState(strings.TrimSpace(sched.Fields["progress_state"]))
	summaryGaps, scoredRuns := pState.ProgressSummaryGaps()

	// Flat-vs-blob self-block divergence (bug ka-0000004562). The engine reads
	// the self-block ONLY from the nested `gardener` blob, but the obvious CLI
	// path (`kd update <trigger> --field terminal_goal=…`) writes the INERT flat
	// top-level field, so a goal/budget "tightened" that way silently no-ops.
	// Surface every diverging field here so the footgun is loud at exactly the
	// surface surveyors (the trigger-reviver) and operators already read, rather
	// than a silent wrong-goal retirement.
	divergences := gardenerSelfBlockDivergences(sched)

	if jsonOutput {
		printJSON(map[string]any{
			"id":            sched.ID,
			"title":         sched.Title,
			"status":        sched.Status,
			"assign_bead":   gardenerEpicOf(sched),
			"role":          sched.Fields["role"],
			"enabled":       sched.Fields["enabled"],
			"cron":          sched.Fields["cron"],
			"timezone":      sched.Fields["timezone"],
			"last_run":      firstNonEmpty(sched.Fields["last_run"], rs.lastRun),
			"last_agent_id": firstNonEmpty(sched.Fields["last_agent_id"], rs.lastAgentID),
			// Fire-error diagnostic (bug ka-0000002991): the hard fireAction
			// failure that spawns no agent and so is invisible in every run
			// counter. Cleared on the next successful fire, so a non-empty value
			// is a LIVE failure a scripted survey can key on.
			"last_fire_error":      sched.Fields["last_fire_error"],
			"last_fire_error_at":   sched.Fields["last_fire_error_at"],
			"consecutive_failures": defaultIfEmpty(sched.Fields["consecutive_failures"], "0"),
			// Emit "0", never "", for a never-fired gardener. A caller writing the
			// natural liveness guard `gb gardener status --json | jq -r '.runs_spawned'`
			// then numeric-comparing gets "" for a gardener that never ran — and
			// `"" != "0"` is TRUE, so the guard falsely certifies a dark gardener as
			// live (ka-0000002620, compounding the jq `//` footgun ka-0000006874).
			// The text render already floors these via defaultIfEmpty below; mirror
			// it here so the two projections agree.
			"runs_spawned":   defaultIfEmpty(spawnedStr, "0"),
			"runs_completed": defaultIfEmpty(completedStr, "0"),
			"max_runs":       firstNonEmpty(sched.Fields["max_runs"], rs.maxRuns),
			"force_close":    sched.Fields["force_close"],
			"bounded":        bounded,
			"bounds_reason":  boundsReason,
			"wall_clock":     wallExpiry.jsonValue(),
			// Two-axis safety (bug ka-0000000849). `bounded`/`bounds_reason` above
			// remain the BACKSTOP axis for backward-compat with existing surveyors;
			// these mirror them plus the progress-aware SELF-BLOCK axis the
			// auto-block audit scores, so both tools speak one vocabulary.
			"has_backstop":                  safety.HasBackstop,
			"self_block":                    safety.SelfBlock,
			"self_block_reason":             safety.SelfBlockReason,
			"loop_end_kinds":                safety.LoopEndKinds,
			"has_progress_spec":             safety.HasProgressSpec,
			"progress_aware_wedge_detector": safety.ProgressAware,
			// D6 loop-hygiene score (ka-0000000452): the effectiveness-rubric §D6
			// checklist, computed deterministically off the same blob so the meta
			// scorer and this status readout can never disagree.
			"d6": map[string]any{
				"raw":      d6.Raw,
				"max":      bridge.D6MaxRaw,
				"weighted": d6.Weighted,
				"checks":   d6.Checks,
			},
			// D1 progress-yield score (ka-0000004982): the effectiveness-rubric §D1
			// dimension, computed deterministically off progress_state.runs +
			// events (the "score>=1 OR credited-event" union) so the meta scorer
			// reads a correct D1 instead of hand-deriving a false 0. When
			// has_runs=false the gardener has recorded no runs — D1 is UNMEASURABLE
			// (raw 0 but NOT a "pure spin" grade); the D1/D2 consistency guard then
			// grades on D2/D3/D4.
			"d1": map[string]any{
				"raw":            d1.Raw,
				"max":            bridge.D1MaxRaw,
				"weighted":       d1.Weighted,
				"yield":          d1.Yield,
				"progress_runs":  d1.ProgressRuns,
				"scored_runs":    d1.ScoredRuns,
				"self_reported":  d1.SelfReported,
				"event_credited": d1.EventCredited,
				"window":         d1.Window,
				"has_runs":       d1.HasRuns,
				// guard_applied (task ka-0000003456): the D1/D2 consistency guard
				// clamped Raw up to 3 because D2>=4 (children closing) contradicted
				// a sub-3 raw yield. When true, read Raw as the guarded floor, not
				// the yield band.
				"guard_applied": d1.GuardApplied,
			},
			// D2 throughput score (task ka-0000003456): the effectiveness-rubric §D2
			// dimension, computed deterministically off the epic's child closures
			// this window vs. open frontier now (burn-rate normalized to the 15%
			// full-mark floor). It is the independent cross-check that feeds the
			// D1/D2 guard. epic_drained=true (no open children, none closed this
			// window) is NOT a throughput failure — it is a stop candidate.
			"d2": map[string]any{
				"raw":              d2.Raw,
				"max":              bridge.D2MaxRaw,
				"weighted":         d2.Weighted,
				"burn_rate":        d2.BurnRate,
				"closed_in_window": d2.ClosedInWindow,
				"open_now":         d2.OpenNow,
				"epic_drained":     d2.EpicDrained,
				"window_hours":     int(gardenerD2Window.Hours()),
			},
			// Cron-preflight-gating verdict (ka-0000006215): shape-aware answer to
			// "are this gardener's cron wakes gated by a world-state preflight?",
			// read for trigger:multi from triggers[].preflight (NOT the inert
			// top-level conditions field). The meta-scorer keys D3/tuning findings
			// off `.cron_gating.gated` instead of hand-inspecting the wrong field.
			"cron_gating": map[string]any{
				"shape":              cronGating.Shape,
				"has_cron":           cronGating.HasCron,
				"gated":              cronGating.Gated,
				"gated_cron_entries": cronGating.GatedCronEntries,
				"total_cron_entries": cronGating.TotalCronEntries,
				"reason":             cronGating.Reason,
			},
			// progress_summary co-require conformance (audit F2, ka-0000005466):
			// runs that scored progress but left progress_summary empty, over the
			// count of scored runs. gaps>0 flags the soft-enforcement leak the
			// meta scorecard reads.
			"progress_summary_gaps":   summaryGaps,
			"progress_summary_scored": scoredRuns,
			"field_divergences":       divergences,
			"triggers":                jsonRaw(sched.Fields["triggers"]),
			"stop":                    jsonRaw(sched.Fields["stop"]),
			"progress":                jsonRaw(sched.Fields["progress"]),
			"progress_state":          jsonRaw(sched.Fields["progress_state"]),
			"pour_stall":              sched.Fields["pour_stall"] == "true",
		})
		return nil
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Gardener schedule %s\n", sched.ID)
	fmt.Fprintf(out, "  title:        %s\n", sched.Title)
	fmt.Fprintf(out, "  status:       %s\n", sched.Status)
	fmt.Fprintf(out, "  assign_bead:  %s\n", gardenerEpicOf(sched))
	fmt.Fprintf(out, "  role:         %s\n", sched.Fields["role"])
	fmt.Fprintf(out, "  enabled:      %s\n", sched.Fields["enabled"])
	// Two safety axes, rendered side by side (bug ka-0000000849). They measure
	// DIFFERENT properties and can legitimately disagree, so the display names
	// each explicitly instead of collapsing to one ambiguous "bounded".
	//   backstop   — engine-enforced emergency brake (run_budget + hard_stop
	//                wall_clock); the SAME verdict the engine enforces at fire time.
	//   self-block — progress/goal-aware loop_end (advice ka-0000000974); what
	//                gardener-autoblock-audit.py scores (ok / weak / offender).
	if bounded {
		fmt.Fprintf(out, "  backstop:     yes (run_budget + hard_stop wall_clock present)\n")
	} else {
		// Show only the first line of the guard's multi-line violation text so
		// the one-line status field stays scannable; --json carries the full
		// reason.
		firstLine := strings.SplitN(boundsReason, "\n", 2)[0]
		fmt.Fprintf(out, "  backstop:     NO — %s\n", firstLine)
	}
	switch safety.SelfBlock {
	case bridge.SelfBlockOK:
		fmt.Fprintf(out, "  self-block:   ok (progress-aware wedge detector present)\n")
	case bridge.SelfBlockWeak:
		fmt.Fprintf(out, "  self-block:   weak — %s\n", safety.SelfBlockReason)
	default:
		fmt.Fprintf(out, "  self-block:   NONE — %s\n", safety.SelfBlockReason)
	}
	// D6 loop-hygiene score (ka-0000000452) — the effectiveness-rubric §D6
	// checklist rolled up to a raw score and its weighted contribution. List each
	// FAILED check inline so an operator sees exactly which hygiene rule to fix,
	// rather than re-deriving the rubric by hand. A perfect 5/5 prints just the
	// headline.
	guardNote := ""
	if d1.GuardApplied {
		guardNote = " [D1/D2 guard: clamped to >=3 — D2>=4 contradicts a lower yield]"
	}
	if d1.HasRuns {
		fmt.Fprintf(out, "  D1 progress:  %d/%d (weighted %.1f/30) — yield %.0f%% (%d/%d runs; %d self-reported, %d event-credited)%s\n",
			d1.Raw, bridge.D1MaxRaw, d1.Weighted, d1.Yield*100, d1.ProgressRuns, d1.ScoredRuns, d1.SelfReported, d1.EventCredited, guardNote)
	} else if d1.GuardApplied {
		fmt.Fprintf(out, "  D1 progress:  %d/%d (weighted %.1f/30) — no runs recorded%s\n",
			d1.Raw, bridge.D1MaxRaw, d1.Weighted, guardNote)
	} else {
		fmt.Fprintf(out, "  D1 progress:  unmeasurable (no runs recorded yet)\n")
	}
	if d2.EpicDrained {
		fmt.Fprintf(out, "  D2 throughput: unmeasurable (epic drained — no open children; stop candidate)\n")
	} else {
		fmt.Fprintf(out, "  D2 throughput: %d/%d (weighted %.1f/20) — burn %.0f%% (%d closed/%dh, %d open now)\n",
			d2.Raw, bridge.D2MaxRaw, d2.Weighted, d2.BurnRate*100, d2.ClosedInWindow, int(gardenerD2Window.Hours()), d2.OpenNow)
	}
	fmt.Fprintf(out, "  D6 hygiene:   %d/%d (weighted %.1f/10)\n", d6.Raw, bridge.D6MaxRaw, d6.Weighted)
	for _, c := range d6.Checks {
		if !c.Pass {
			fmt.Fprintf(out, "      ✗ %s: %s\n", c.Name, c.Reason)
		}
	}
	// Cron-preflight-gating (ka-0000006215) — is this gardener's scheduled wake
	// gated by a world-state preflight? Read shape-aware: for a trigger:multi
	// gardener from triggers[].preflight, NOT the inert top-level conditions
	// field. This is the readout the meta-scorer must use before grading a
	// gardener "fires unconditionally into empty gb stop calls" — an ungated
	// (gated:NO) verdict is the ONLY shape in which that D3/tuning finding is
	// legitimate.
	if !cronGating.HasCron {
		fmt.Fprintf(out, "  cron gating:  n/a (%s)\n", cronGating.Reason)
	} else if cronGating.Gated {
		fmt.Fprintf(out, "  cron gating:  gated (%s)\n", cronGating.Reason)
	} else {
		fmt.Fprintf(out, "  cron gating:  UNGATED — %s\n", cronGating.Reason)
	}
	// progress_summary co-require conformance (audit finding F2, ka-0000005466).
	// The wrap-up enforcer pairs progress_score with progress_summary, but that
	// hard-require holds only under in_wrapup+hard enforcement — --force stops and
	// soft schedules leak, so a run can record a scored-but-unsummarized wrap-up.
	// Flag the gap only when at least one scored run left its summary empty; a
	// fully-conformant gardener prints nothing so the readout stays scannable.
	if summaryGaps > 0 {
		fmt.Fprintf(out, "  ⚠️  progress_summary gap: %d of %d scored run(s) set progress_score but left progress_summary empty\n",
			summaryGaps, scoredRuns)
		fmt.Fprintf(out, "      (the score/summary co-require leaks past --force stops and soft schedules — audit F2 ka-0000005466)\n")
	}
	// Wall-clock expiry readout (bug ka-0000007024). "bounded: yes" only means
	// the duration parses into range — NOT that the window is still open. Print
	// the real baseline+expiry so an operator revising/reviving a gardener sees
	// whether the ceiling is in the past. An ALREADY-elapsed window gets a loud
	// banner: reviving without lengthening the duration re-closes it on the next
	// reconcile (a clean-looking reopen that silently reverts itself).
	if wallExpiry.present {
		baselineNote := ""
		if wallExpiry.grandfathered {
			baselineNote = " (floored to grandfather epoch — gardener predates it)"
		}
		if wallExpiry.expired {
			fmt.Fprintf(out, "  wall_clock:   ⚠️  EXPIRED — %s window elapsed at %s%s\n",
				wallExpiry.duration, wallExpiry.expiry.UTC().Format(time.RFC3339), baselineNote)
			fmt.Fprintf(out, "      The hard_stop wall_clock is already past, so this gardener re-closes on the next reconcile\n")
			fmt.Fprintf(out, "      even though `bounded` is yes. To actually revive it, RESET the baseline to now:\n")
			fmt.Fprintf(out, "        gb trigger enable %s --rebaseline   (a fresh %s window measured from now)\n",
				sched.ID, wallExpiry.duration)
			// Lengthening the duration only helps while there is headroom below
			// the ceiling — for a gardener already AT the max (the 720h cohort
			// this bug bit) it is impossible, so it is offered strictly as a
			// conditional secondary, not the primary fix.
			if wallExpiry.duration < bridge.GardenerMaxWallClock {
				fmt.Fprintf(out, "      Or, if you have headroom below the %s ceiling, lengthen the duration so\n", bridge.GardenerMaxWallClock)
				fmt.Fprintf(out, "      baseline(%s)+duration lands in the FUTURE.\n",
					wallExpiry.baseline.UTC().Format(time.RFC3339))
			}
			fmt.Fprintf(out, "      See bug ka-0000007024 / ka-0000001868.\n")
		} else {
			fmt.Fprintf(out, "  wall_clock:   %s window, expires %s%s\n",
				wallExpiry.duration, wallExpiry.expiry.UTC().Format(time.RFC3339), baselineNote)
		}
	}
	// Loud divergence banner (bug ka-0000004562): the flat top-level field and
	// its authoritative blob counterpart disagree. The engine reads ONLY the
	// blob, so the flat value is a dead edit — almost always the sign that
	// someone tightened the goal/budget via `kd update --field …` and it
	// silently no-op'd. Print the actual pair so the operator sees exactly which
	// value the engine is (and is NOT) using.
	for _, d := range divergences {
		fmt.Fprintf(out, "  ⚠️  DIVERGENCE %s: flat=%q is IGNORED; engine reads blob=%q\n",
			d.Field, truncate(d.Flat, 80), truncate(d.Blob, 80))
	}
	if len(divergences) > 0 {
		fmt.Fprintf(out, "      (the flat top-level field is inert — the daemon reads only the nested `gardener` blob;\n")
		fmt.Fprintf(out, "       edit the blob via `kd update %s --field gardener=@file`, NOT `--field <name>=` — bug ka-0000004562)\n", sched.ID)
	}
	if force := sched.Fields["force_close"]; force != "" {
		fmt.Fprintf(out, "  force_close:  %s\n", force)
	}
	if cron := sched.Fields["cron"]; cron != "" {
		fmt.Fprintf(out, "  cron:         %s (%s)\n", cron, defaultIfEmpty(sched.Fields["timezone"], "UTC"))
	}
	fmt.Fprintf(out, "  runs:         spawned=%s completed=%s max=%s\n",
		defaultIfEmpty(spawnedStr, "0"),
		defaultIfEmpty(completedStr, "0"),
		defaultIfEmpty(firstNonEmpty(sched.Fields["max_runs"], rs.maxRuns), "—"))
	fmt.Fprintf(out, "  last_run:     %s\n", defaultIfEmpty(firstNonEmpty(sched.Fields["last_run"], rs.lastRun), "—"))
	fmt.Fprintf(out, "  last_agent:   %s\n", defaultIfEmpty(firstNonEmpty(sched.Fields["last_agent_id"], rs.lastAgentID), "—"))
	fmt.Fprintf(out, "  last_checked: %s\n", defaultIfEmpty(sched.Fields["last_checked"], "—"))

	// Decline reason (bug ka-0000001513). On every evaluation that reaches the
	// source (past the write-free cooldown / blocked_by / pre-fire gates), the
	// engine folds last_outcome into the last_checked stamp, naming WHY the
	// cycle did what it did — fired, or the specific decline (no source matches
	// / seen-set dedup / concurrency lease full). Without it, a gardener that is
	// evaluated every cycle but never fires reads identically to a healthy one
	// on last_checked alone, which is exactly the dark-gardener trap this bead
	// was filed on. Flag any non-fire outcome so status stops reporting a dark
	// gardener as all-green.
	if outcome := strings.TrimSpace(sched.Fields["last_outcome"]); outcome != "" {
		marker := ""
		if !strings.HasPrefix(outcome, "fired") {
			marker = "  ⚠️  gardener evaluated but did not fire — see reason"
		}
		fmt.Fprintf(out, "  last_outcome: %s (%s)%s\n",
			outcome, defaultIfEmpty(sched.Fields["last_outcome_at"], "?"), marker)
	}

	// Fire error (bug ka-0000002991). Distinct from last_outcome's decline
	// reason: last_fire_error is set ONLY when a fireAction actually RAISED —
	// e.g. spawn_agent's CreateBead 400'd — a hard failure that spawns no agent
	// and so leaves every run counter untouched. It is the exact signal that was
	// invisible in the data plane when three plan-gardeners failed for hours
	// (ka-0000004529) and two agents mis-diagnosed it as a silent decline. It is
	// cleared on the next successful fire, so a present value is a LIVE failure.
	// Print it prominently — this is the command an operator reaches for first.
	if fireErr := strings.TrimSpace(sched.Fields["last_fire_error"]); fireErr != "" {
		fmt.Fprintf(out, "  ⛔ last_fire_error: %s (%s) — fireAction raised; no agent spawned\n",
			fireErr, defaultIfEmpty(sched.Fields["last_fire_error_at"], "?"))
	}

	// Pouring-but-never-worked stall verdict (ka-0000000957), stamped by the
	// engine's pouring-stall sweep. A gardener can look alive here (fresh
	// last_run, non-zero runs) yet be pouring molecules nobody works; this
	// banner surfaces that stall so it is caught in hours, not days.
	if sched.Fields["pour_stall"] == "true" {
		fmt.Fprintf(out, "  ⚠️  pouring-but-never-worked: %s inert molecule(s), oldest quiet for %s (since %s)\n",
			defaultIfEmpty(sched.Fields["pour_stall_molecules"], "?"),
			defaultIfEmpty(sched.Fields["pour_stall_oldest_age"], "?"),
			defaultIfEmpty(sched.Fields["pour_stall_detected_at"], "?"))
	}

	// Progress block (T5). Even if T5 isn't merged the field may not exist;
	// that is fine — we just skip the section.
	if state := strings.TrimSpace(sched.Fields["progress_state"]); state != "" {
		fmt.Fprintln(out, "  progress:")
		printIndentedJSON(out, "    ", state)
	} else if cfg := strings.TrimSpace(sched.Fields["progress"]); cfg != "" {
		fmt.Fprintln(out, "  progress (config):")
		printIndentedJSON(out, "    ", cfg)
	}

	// Stop conditions (T4).
	if stopBlob := strings.TrimSpace(sched.Fields["stop"]); stopBlob != "" {
		fmt.Fprintln(out, "  stop:")
		printIndentedJSON(out, "    ", stopBlob)
	}

	// Recent stop-condition evaluations are stored as comments tagged
	// [stop-eval] by T4. We surface up to 5 of them when present.
	evals, err := recentStopEvaluations(ctx, daemon, sched.ID, 5)
	if err == nil && len(evals) > 0 {
		fmt.Fprintln(out, "  recent stop evaluations:")
		for _, line := range evals {
			fmt.Fprintf(out, "    %s\n", line)
		}
	}
	return nil
}

// recentStopEvaluations returns up to n recent stop-condition evaluation
// comments on the schedule bead (best-effort: comments tagged with the
// `[stop-eval]` marker that T4's StopEvaluator writes).
func recentStopEvaluations(ctx context.Context, c *beadsapi.Client, scheduleID string, n int) ([]string, error) {
	comments, err := c.GetComments(ctx, scheduleID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	// Walk newest-first.
	for i := len(comments) - 1; i >= 0 && len(out) < n; i-- {
		text := comments[i].Text
		if !strings.Contains(text, "[stop-eval]") {
			continue
		}
		// Trim to a single line for the status output.
		line := strings.SplitN(text, "\n", 2)[0]
		out = append(out, line)
	}
	return out, nil
}

// ── gb gardener stop ──────────────────────────────────────────────────────

var (
	gardenerStopReason        string
	gardenerStopIgnoreOpenKid bool
)

// gardenerStopOpenChildTypes is the set of issue-kind types whose open members
// keep a gardener's epic "live" for the fail-closed stop guard. Bundles (the
// plan's type=bundle narrative child) are intentionally excluded: a bundle is
// a doc container, not a unit of remaining work, and it never closes on its
// own — counting it would make every plan gardener permanently unstoppable.
var gardenerStopOpenChildTypes = []string{"task", "bug", "epic", "feature", "chore"}

// gardenerStopOpenChildStatuses are the non-terminal statuses that mark a child
// as still-open work for the guard. deferred is deliberately omitted: a
// deferred child is parked, not active, so it should not block a stop.
var gardenerStopOpenChildStatuses = []string{"open", "in_progress", "blocked"}

var gardenerStopCmd = &cobra.Command{
	Use:   "stop <bead-id>",
	Short: "Stop the gardener schedule for an epic bead",
	Long: `Close the gardener schedule attached to <bead-id> (i.e. wraps
'gb schedule complete' for the resolved schedule). Subsequent ticks are
skipped.

FAIL-CLOSED GUARD: stop refuses to close the gardener when the
epic's transitive parent-child subtree still holds any open/in_progress/blocked
issue-kind child (task, bug, epic, feature, chore — bundles excluded). This
catches the meta-gardener "terminal goal met" undercount, where a subtree with
cross-project children read as childless and proposed stopping a still-active
plan. Pass --ignore-open-children to override once you have confirmed the
remaining children are genuinely not blocking closure.`,
	Args: cobra.ExactArgs(1),
	RunE: runGardenerStop,
}

func runGardenerStop(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return fmt.Errorf("bead id is required")
	}
	reason := strings.TrimSpace(gardenerStopReason)
	if reason == "" {
		return fmt.Errorf("--reason is required")
	}
	ctx := cmd.Context()

	sched, err := resolveGardenerSchedule(ctx, daemon, beadID)
	if err != nil {
		return err
	}
	if sched.Status == "closed" {
		if jsonOutput {
			printJSON(map[string]string{"id": sched.ID, "status": "closed", "noop": "already closed"})
		} else {
			fmt.Printf("Gardener schedule %s already closed\n", sched.ID)
		}
		return nil
	}

	// Fail-closed open-children guard (ka-0000008241). A gardener whose epic
	// still has open work must not be stopped — stopping severs the auto-wake
	// that re-dispatches when escalations clear, silently stalling the plan.
	// The meta-gardener's "terminal goal met, stop the gardener" path was the
	// primary offender: it undercounted the subtree to zero (implicit
	// project-label scoping dropped cross-project children) and proposed stops
	// on live plans. Re-check the subtree authoritatively here so a bad count
	// upstream can never close a still-active gardener. --ignore-open-children
	// overrides after the operator has confirmed the remaining children are
	// genuinely not blocking closure.
	if epicID := gardenerEpicOf(sched); epicID != "" && !gardenerStopIgnoreOpenKid {
		open, err := openSubtreeChildren(ctx, daemon, epicID)
		if err != nil {
			return fmt.Errorf("checking open children of %s before stop: %w", epicID, err)
		}
		if len(open) > 0 {
			return fmt.Errorf(
				"refusing to stop gardener for %s: %d open child(ren) remain in its subtree (%s%s); "+
					"stopping now would sever the auto-wake and stall the plan. "+
					"Close/reparent the children, or pass --ignore-open-children to override",
				epicID, len(open), strings.Join(firstN(open, 8), ", "), moreSuffix(len(open), 8))
		}
	}

	comment := fmt.Sprintf("gardener stopped by %s: %s", actor, reason)
	if err := daemon.AddComment(ctx, sched.ID, actor, comment); err != nil {
		return fmt.Errorf("adding stop comment to %s: %w", sched.ID, err)
	}
	if err := daemon.CloseBead(ctx, sched.ID, nil); err != nil {
		return fmt.Errorf("closing gardener schedule %s: %w", sched.ID, err)
	}

	if jsonOutput {
		printJSON(map[string]string{"id": sched.ID, "assign_bead": beadID, "status": "closed", "reason": reason})
	} else {
		fmt.Printf("Stopped gardener schedule %s for %s: %s\n", sched.ID, beadID, reason)
	}
	return nil
}

// gardenerD2Window is the default scoring window `gb gardener status` uses for
// the D2 throughput count. The rubric §D2 window is "since the previous
// meta-gardener run (~12h)"; the status readout does not know when the last meta
// run fired, so it uses this fixed 12h lookback as the deterministic default —
// the same cadence the twice-daily meta-gardener runs on. The meta-scorer can
// still compute a precise per-cycle window; this is the default any surveyor or
// human reads off `gb gardener status`.
const gardenerD2Window = 12 * time.Hour

// gardenerD2Verdict computes the rubric §D2 throughput score for a gardener's
// epic off the graph: the number of the epic's children CLOSED inside `window`
// vs. the number OPEN now. It is the independent cross-check that feeds the
// D1/D2 consistency guard (bridge.ApplyD1D2Guard).
//
// It issues two cheap count-only subtree queries (the same reverse-subtree CTE
// `openSubtreeChildren` uses, scoped to the issue-kind work types). A query
// failure is fail-soft: it returns a zero verdict (EpicDrained, so the guard
// no-ops) rather than failing the whole status readout — D2 is advisory. An
// empty epicID (a gardener with no resolvable assign-bead) likewise yields the
// zero verdict.
func gardenerD2Verdict(ctx context.Context, c *beadsapi.Client, epicID string, window time.Duration) bridge.D2Verdict {
	epicID = strings.TrimSpace(epicID)
	if epicID == "" || c == nil {
		return bridge.D2Verdict{EpicDrained: true}
	}

	openRes, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:    epicID,
		Types:     gardenerStopOpenChildTypes,
		Statuses:  gardenerStopOpenChildStatuses,
		CountOnly: true,
	})
	if err != nil {
		return bridge.D2Verdict{EpicDrained: true}
	}

	closedRes, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:      epicID,
		Types:       gardenerStopOpenChildTypes,
		Statuses:    []string{"closed"},
		ClosedAfter: time.Now().Add(-window),
		CountOnly:   true,
	})
	if err != nil {
		return bridge.D2Verdict{EpicDrained: true}
	}

	return bridge.ScoreGardenerD2(closedRes.Total, openRes.Total)
}

// openSubtreeChildren returns the ids of open/in_progress/blocked issue-kind
// children anywhere in the epic's transitive parent-child subtree. It queries
// the same cycle-safe reverse-subtree CTE `kd list --parent` uses (server-side
// `parent=` param), scoped to the issue-kind work types and non-terminal
// statuses that keep a plan live — bundles and deferred children are excluded.
// The query is cross-project by construction: the beadsapi client never injects
// an implicit project label, so a plan's project:monorepo children are counted
// too (the fix for the ka-0000008241 undercount).
func openSubtreeChildren(ctx context.Context, c *beadsapi.Client, epicID string) ([]string, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:   epicID,
		Types:    gardenerStopOpenChildTypes,
		Statuses: gardenerStopOpenChildStatuses,
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Beads))
	for _, b := range res.Beads {
		ids = append(ids, b.ID)
	}
	return ids, nil
}

// firstN returns the first n elements of s (or all of s when it is shorter).
func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// moreSuffix renders " +N more" when total exceeds shown, else "".
func moreSuffix(total, shown int) string {
	if total > shown {
		return fmt.Sprintf(" +%d more", total-shown)
	}
	return ""
}

// ── gb gardener force-close ───────────────────────────────────────────────

var gardenerForceCloseReason string

var gardenerForceCloseCmd = &cobra.Command{
	Use:   "force-close <bead-id>",
	Short: "Force the gardener loop down immediately",
	Long: `Tear down the gardener loop for <bead-id> immediately.

For the current trigger:schedule gardener shape (Phase 3b/D10) there is no
in-band hard_stop.manual_force evaluator — the bare trigger has no stop
spec — so force-close disables the trigger (enabled=false) and closes the
gardener bead in one step. Equivalent to 'gb gardener stop' for these
gardeners, kept as a distinct verb for operator muscle memory.

For legacy type=schedule gardeners it sets fields.force_close="true" so the
scheduler's hard_stop.manual_force entry tears the loop down on the next
tick, preserving the old in-band hard-stop accounting.`,
	Args: cobra.ExactArgs(1),
	RunE: runGardenerForceClose,
}

func runGardenerForceClose(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return fmt.Errorf("bead id is required")
	}
	reason := strings.TrimSpace(gardenerForceCloseReason)
	if reason == "" {
		return fmt.Errorf("--reason is required")
	}
	ctx := cmd.Context()

	sched, err := resolveGardenerSchedule(ctx, daemon, beadID)
	if err != nil {
		return err
	}

	// Trigger:schedule gardeners carry no stop spec, and the type:trigger
	// config schema rejects the force_close field outright. Force-close them
	// by disabling + closing the bead immediately.
	if sched.Type == "trigger" {
		if sched.Status == "closed" {
			if jsonOutput {
				printJSON(map[string]string{"id": sched.ID, "status": "closed", "noop": "already closed"})
			} else {
				fmt.Printf("Gardener trigger %s already closed\n", sched.ID)
			}
			return nil
		}
		comment := fmt.Sprintf("gardener force-closed by %s: %s", actor, reason)
		if err := daemon.AddComment(ctx, sched.ID, actor, comment); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to add audit comment to %s: %v\n", sched.ID, err)
		}
		// `enabled` is boolean in the type:trigger schema — write a JSON bool,
		// not the string "false" (ka-0000003627).
		if err := daemon.UpdateBeadFieldsTyped(ctx, sched.ID, map[string]any{"enabled": false}); err != nil {
			return fmt.Errorf("disabling gardener trigger %s: %w", sched.ID, err)
		}
		if err := daemon.CloseBead(ctx, sched.ID, nil); err != nil {
			return fmt.Errorf("closing gardener trigger %s: %w", sched.ID, err)
		}
		if jsonOutput {
			printJSON(map[string]string{"id": sched.ID, "assign_bead": beadID, "status": "closed", "reason": reason})
		} else {
			fmt.Printf("Force-closed gardener trigger %s for %s: %s\n", sched.ID, beadID, reason)
		}
		return nil
	}

	if sched.Fields["force_close"] == "true" {
		if jsonOutput {
			printJSON(map[string]string{"id": sched.ID, "force_close": "true", "noop": "already set"})
		} else {
			fmt.Printf("Gardener schedule %s already has force_close=true\n", sched.ID)
		}
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := daemon.UpdateBeadFields(ctx, sched.ID, map[string]string{
		"force_close":        "true",
		"force_close_reason": reason,
		"force_close_set_at": now,
		"force_close_set_by": actor,
	}); err != nil {
		return fmt.Errorf("setting force_close on %s: %w", sched.ID, err)
	}

	comment := fmt.Sprintf("force-close set by %s: %s (hard_stop.manual_force will tear down on next tick)", actor, reason)
	if err := daemon.AddComment(ctx, sched.ID, actor, comment); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to add audit comment to %s: %v\n", sched.ID, err)
	}

	if jsonOutput {
		printJSON(map[string]string{
			"id":          sched.ID,
			"assign_bead": beadID,
			"force_close": "true",
			"reason":      reason,
		})
	} else {
		fmt.Printf("Set force_close=true on gardener schedule %s for %s: %s\n", sched.ID, beadID, reason)
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────

// resolveGardenerSchedule turns an epic bead-id into the gardener schedule
// bead attached to it. Returns a clear error when no gardener is present.
// If <bead-id> is itself a schedule bead with role=gardener, that bead is
// returned directly so the CLI is also operable from the schedule's own
// perspective (tests rely on this).
func resolveGardenerSchedule(ctx context.Context, c *beadsapi.Client, beadID string) (*beadsapi.BeadDetail, error) {
	b, err := c.GetBead(ctx, beadID)
	if err != nil {
		return nil, fmt.Errorf("looking up bead %s: %w", beadID, err)
	}
	// If <bead-id> is itself the gardener bead (trigger:schedule or legacy
	// type=schedule), return it directly. Use beadIsGardener so a gardener whose
	// `role` field names its spawn-role (not "gardener") still resolves via the
	// role:gardener label / gardener blob (ka-0000005048).
	if (b.Type == "trigger" || b.Type == "schedule") && beadIsGardener(b) {
		return b, nil
	}
	existing, err := findExistingGardenerSchedule(ctx, c, beadID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		// Fall back to *any* gardener bead (closed/disabled included) so the
		// user gets a useful message rather than a silent miss.
		all, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Types: []string{"trigger", "schedule"}})
		if err == nil {
			for _, candidate := range all.Beads {
				if beadIsGardenerForEpic(candidate, beadID) {
					return candidate, nil
				}
			}
		}
		return nil, fmt.Errorf("no gardener schedule found for %s (run 'gb gardener start %s' first)", beadID, beadID)
	}
	return existing, nil
}

func defaultIfEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// jsonRaw returns the parsed JSON value of s, or s itself if the parse
// fails. Used to embed already-rendered JSON blobs into the --json output
// without double-encoding them.
func jsonRaw(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}

// printIndentedJSON writes a 2-space-indented re-encoding of s, prefixed
// with prefix on each line. Falls back to the raw string when s is not
// valid JSON.
func printIndentedJSON(out interface{ Write(p []byte) (int, error) }, prefix, s string) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		fmt.Fprintf(out, "%s%s\n", prefix, s)
		return
	}
	enc, err := json.MarshalIndent(v, prefix, "  ")
	if err != nil {
		fmt.Fprintf(out, "%s%s\n", prefix, s)
		return
	}
	fmt.Fprintf(out, "%s%s\n", prefix, string(enc))
}

func init() {
	gardenerCmd.AddCommand(gardenerStartCmd)
	gardenerCmd.AddCommand(gardenerStatusCmd)
	gardenerCmd.AddCommand(gardenerStopCmd)
	gardenerCmd.AddCommand(gardenerForceCloseCmd)
	gardenerCmd.AddCommand(gardenerModeCmd)
	gardenerCmd.AddCommand(gardenerCadenceCmd)
	gardenerCmd.AddCommand(gardenerFireNowCmd)

	gardenerFireNowCmd.Flags().BoolVar(&gardenerFireNowForce, "force", false, "fire a cycle even when a live cycle for this gardener is already running (bypasses the singleton guard)")
	gardenerFireNowCmd.Flags().StringVar(&gardenerFireNowPrompt, "prompt", "", "override the cycle prompt (advanced; defaults to the gardener's own stored prompt)")

	gardenerStartCmd.Flags().StringVar(&gardenerStartCron, "cron", "", "cron expression (default: deterministic staggered 30m cron seeded from the epic id)")
	gardenerStartCmd.Flags().StringVar(&gardenerStartTimezone, "tz", "", "timezone (default: America/Los_Angeles)")
	gardenerStartCmd.Flags().StringVar(&gardenerStartProject, "project", "", "project label (default: inherited from parent bead)")
	gardenerStartCmd.Flags().IntVar(&gardenerStartMaxRuns, "max-runs", 20, "max number of runs the gardener may spawn before loop_end")
	gardenerStartCmd.Flags().StringVar(&gardenerStartLoopWall, "loop-wall-clock", "168h", "wall-clock cap for the entire loop (surfaced in the gardener prompt)")
	gardenerStartCmd.Flags().StringVar(&gardenerStartRunWall, "run-wall-clock", "30m", "wall-clock cap for a single run (surfaced in the gardener prompt)")
	gardenerStartCmd.Flags().StringVar(&gardenerStartTitle, "title", "", "schedule bead title (default: 'Gardener for <bead-id> (<parent-title>)')")
	gardenerStartCmd.Flags().StringVar(&gardenerStartTerminal, "terminal-goal", "", "terminal goal for the loop (default: 'All children of <bead-id> are closed.')")
	gardenerStartCmd.Flags().StringVar(&gardenerStartPromptOver, "prompt", "", "override the gardener playbook prompt (advanced; usually leave unset)")
	gardenerStartCmd.Flags().IntVar(&gardenerStartMaxConc, "max-concurrent", 3, "max gardener agents fired per evaluation cycle (mirrors gb trigger create)")
	gardenerStartCmd.Flags().StringVar(&gardenerStartCooldown, "cooldown", "5m", "minimum interval between gardener firings (e.g. 5m, 90s, 2); coerced to integer minutes")
	gardenerStartCmd.Flags().StringSliceVar(&gardenerStartLabels, "label", nil, "extra label(s) on the gardener bead (e.g. monorepo-dark); repeatable. Dark gardeners are bounds-enforced (R6-FU6)")
	gardenerStartCmd.Flags().BoolVar(&gardenerStartAckGuide, "i-read-the-gardener-instructions", false, "acknowledge you have read the gardener-authoring guide (advice "+gardenerGuideID+"). REQUIRED: without it the command refuses and echoes the guide inline (ka-0000007415)")
	gardenerStartCmd.Flags().StringVar(&gardenerStartWakeOnMR, "wake-on-mr", "", "canonical MR URL: wake the gardener on the FIRST update to that MR (new commit/comment/status change/merge). Adds a near-real-time bead_event trigger on the MR's mirror bead + a fallback cron floor (the §8.2 hybrid recipe). Optional; omit if not needed.")
	gardenerStartCmd.Flags().StringVar(&gardenerStartStopOnMR, "stop-on-mr-merged", "", "canonical MR URL: stop the gardener and report success once THAT MR is merged. Adds a specific_mr_merged loop_end stop entry. Optional; omit if not needed.")

	gardenerStopCmd.Flags().StringVar(&gardenerStopReason, "reason", "", "reason the gardener is being stopped (required)")
	gardenerStopCmd.Flags().BoolVar(&gardenerStopIgnoreOpenKid, "ignore-open-children", false, "override the fail-closed guard and stop even when the epic subtree has open children (ka-0000008241)")
	gardenerForceCloseCmd.Flags().StringVar(&gardenerForceCloseReason, "reason", "", "reason force_close is being set (required)")

	gardenerModeCmd.Flags().BoolVar(&gardenerModeEvent, "event", false, "flip the gardener to event mode (subtree bead events + slow fallback cron)")
	gardenerModeCmd.Flags().BoolVar(&gardenerModeTimed, "timed", false, "flip the gardener to timed mode (cron schedule)")
	gardenerModeCmd.Flags().StringVar(&gardenerModeFallbackCron, "fallback-cron", "", "override the event-mode fallback watchdog cron (default: deterministic staggered 6h cron seeded from the epic id; plan gardeners default to none)")
	gardenerModeCmd.Flags().BoolVar(&gardenerModeNoFallbackCron, "no-fallback-cron", false, "author the event-mode gardener with NO fallback watchdog cron — purely event-driven, no clock floor (mutually exclusive with --fallback-cron)")

	gardenerCadenceCmd.Flags().StringVar(&gardenerCadenceCron, "cron", "", "cron expression to set (timed: the firing schedule; event: the fallback watchdog)")
	gardenerCadenceCmd.Flags().BoolVar(&gardenerCadenceNone, "none", false, "clear the cadence — event mode only, leaving the gardener purely event-driven (mutually exclusive with --cron)")
}
