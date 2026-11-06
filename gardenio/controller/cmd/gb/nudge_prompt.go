package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var nudgePromptCmd = &cobra.Command{
	Use:   "nudge-prompt",
	Short: "Resolve the nudge prompt for this agent's context",
	Long: `Resolves the initial nudge prompt from nudge-prompts config beads,
falling back to hardcoded defaults.

The prompt type is auto-detected from environment variables:
  - SLACK_THREAD_CHANNEL set → "thread"
  - BOAT_PROMPT set          → "adhoc"
  - BOAT_AGENT_STATE=prewarmed → "prewarmed"
  - otherwise                → "default"

Override with --type flag.

The resolved prompt has variable placeholders substituted:
  {{.Project}}, {{.TaskHint}}, {{.MonorepoHint}}, {{.ProjectHint}}, {{.BoatPrompt}}

Examples:
  gb nudge-prompt
  gb nudge-prompt --type thread
  gb nudge-prompt --type default --project myproject`,
	GroupID: "session",
	RunE:    runNudgePrompt,
}

var nudgePromptType string

func init() {
	nudgePromptCmd.Flags().StringVar(&nudgePromptType, "type", "", "prompt type: thread, adhoc, default, prewarmed (auto-detected if omitted)")
}

func runNudgePrompt(cmd *cobra.Command, args []string) error {
	promptType := nudgePromptType
	if promptType == "" {
		promptType = detectNudgeType()
	}

	// Build substitution variables from environment.
	vars := buildNudgeVars()

	// Resolve from config beads (seeded by EnsureConfigs on startup).
	prompt := resolveNudgeFromConfig(promptType, vars)
	if prompt != "" {
		fmt.Fprint(os.Stdout, prompt)
		return nil
	}

	return fmt.Errorf("no nudge-prompts config bead found for type %q — run EnsureConfigs or seed nudge-prompts config beads", promptType)
}

// standbyAssignedSentinelPath is written by standbyWait once the pool manager
// has assigned work to a prewarmed pod. Its presence means "this pod was
// prewarmed but is now assigned" — the signal that BOAT_AGENT_STATE=prewarmed
// (which is never rewritten in the pod env) should no longer be treated as
// parked. A var rather than a const so tests can redirect it off /tmp.
var standbyAssignedSentinelPath = "/tmp/standby_assigned"

// isUnassignedPrewarmed reports whether this pod is a prewarmed agent that is
// still parked in the idle pool (not yet handed work).
//
// BOAT_AGENT_STATE=prewarmed is set once at pod creation and never cleared, so
// it cannot by itself distinguish "parked, waiting for a nudge" from "assigned,
// now running a thread/adhoc task". standbyWait drops the assigned sentinel
// the moment the pool manager assigns work; until then a prewarmed pod is
// genuinely unassigned. Gating the prewarmed standby behavior on this avoids
// handing an assigned agent the "do nothing, wait for a nudge" text — the bug
// where assigned thread agents never wrapped up (ka-0000004623).
func isUnassignedPrewarmed() bool {
	if os.Getenv("BOAT_AGENT_STATE") != "prewarmed" {
		return false
	}
	_, err := os.Stat(standbyAssignedSentinelPath)
	return err != nil // sentinel absent → still parked
}

// detectNudgeType determines the nudge type from environment.
func detectNudgeType() string {
	if isUnassignedPrewarmed() {
		return "prewarmed"
	}
	if os.Getenv("SLACK_THREAD_CHANNEL") != "" {
		return "thread"
	}
	// effectiveBoatPrompt (not the raw env var) so a STALE plan step-dispatch —
	// a "dispatch a crew agent to work step <id>" nudge whose step already
	// closed — no longer selects the "adhoc" template and re-emits a dead
	// dispatch on every respawn (ka-0000002376); the wake falls back to the
	// plain "default" nudge instead.
	if effectiveBoatPrompt() != "" {
		return "adhoc"
	}
	return "default"
}

// stepDispatchSeedPrefix is the fixed lead-in of the plan step-dispatch nudge
// the /plans Dispatch buttons hand a plan gardener (beads-viewer plans.go
// handlePlanDispatchStep / handlePlanRowDispatch emit
// "Please dispatch a crew agent to work step <id> …"). It is duplicated here
// rather than imported because the emitter (cmd/beads-viewer) and this consumer
// (cmd/gb) are separate main packages — the same cross-package-duplication the
// plan-gardener marker label uses (beads-viewer plans_gardener.go). Keep the two
// literals in sync.
const stepDispatchSeedPrefix = "Please dispatch a crew agent to work step "

// stepDispatchTarget extracts the target step id from a plan step-dispatch nudge
// prompt, returning ("", false) for any prompt that is not a step-dispatch. The
// seed shape is `<prefix><stepID>[ ("title")] now — it's ready to start.`, so the
// id is the first whitespace-delimited token right after the prefix.
func stepDispatchTarget(prompt string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(prompt), stepDispatchSeedPrefix)
	if !ok {
		return "", false
	}
	id := strings.TrimSpace(rest)
	if i := strings.IndexAny(id, " \t\n"); i >= 0 {
		id = id[:i]
	}
	if id == "" {
		return "", false
	}
	return id, true
}

// effectiveBoatPrompt returns the pod's BOAT_PROMPT unless it is a STALE plan
// step-dispatch — a "dispatch a crew agent to work step <id>" nudge whose target
// step is already closed/deferred. A plan gardener persists its last dispatch as
// fields.prompt; the controller replays that as BOAT_PROMPT on EVERY respawn
// (podspec.go), so once the step closes the gardener is woken over and over to
// re-dispatch a done step and immediately no-op — burning respawns and spamming
// the operator on a finished plan (ka-0000002376). Dropping the stale seed at
// this single seam — every respawn's nudge flows through it via buildNudgeVars +
// detectNudgeType — makes the wake fall back to the plain "default" nudge instead
// of re-emitting the dead dispatch. This mirrors the closed/deferred BOAT_TASK_ID
// guard in buildNudgeVars, which drops an unclaimable pre-assigned task for the
// same anti-respawn-loop reason (ka-0000006195).
//
// Narrow by construction: a non-dispatch prompt (a real operator message) never
// matches the seed prefix and is returned untouched, and a dispatch for a
// still-OPEN step is preserved so the gardener still acts on live work. A daemon
// error or unknown step id keeps the prompt rather than silently swallowing a
// possibly-live dispatch.
func effectiveBoatPrompt() string {
	prompt := os.Getenv("BOAT_PROMPT")
	stepID, ok := stepDispatchTarget(prompt)
	if !ok || daemon == nil {
		return prompt
	}
	bead, err := daemon.GetBead(context.Background(), stepID)
	if err != nil || bead == nil {
		return prompt
	}
	if bead.Status == "closed" || bead.Status == "deferred" {
		return ""
	}
	return prompt
}

// nudgeVars holds substitution variables for nudge prompt templates.
type nudgeVars struct {
	Project      string
	Role         string
	ProjectHint  string
	TaskHint     string
	MonorepoHint string
	BoatPrompt   string
}

func buildNudgeVars() nudgeVars {
	project := defaultGBProject()

	var projectHint string
	if project != "" {
		projectHint = fmt.Sprintf(" Focus on tasks for project `%s` — skip work that belongs to a different project unless you are explicitly assigned to it.", project)
	}

	// Resolve task ID: prefer env var, then resolve from agent bead dependencies.
	taskID := os.Getenv("BOAT_TASK_ID")
	if taskID == "" {
		taskID = resolveTaskFromDeps()
	}
	// Skip unclaimable-by-status tasks to avoid respawn loops: if the pod env var
	// points to a closed (or deferred) task, the agent would find it unclaimable,
	// call gb stop, kubelet restarts, and gb init re-injects the same stale task —
	// looping every ~18s. kd claim rejects BOTH statuses (kbeads/cmd/kd/claim.go:
	// "status is closed"/"deferred"), so both must drop the pre-assignment hint,
	// not just closed (ka-0000006195).
	//
	// The same GetBead also yields the bead TYPE, which the hint wording depends
	// on: kd claim hard-rejects type=epic, so an epic-anchored spawn must NOT be
	// told to `kd claim` (ka-0000005855). Capture the type at zero extra cost.
	var beadType string
	if taskID != "" && daemon != nil {
		if bead, err := daemon.GetBead(context.Background(), taskID); err == nil {
			if bead.Status == "closed" || bead.Status == "deferred" {
				taskID = ""
			} else {
				beadType = bead.Type
			}
		}
	}
	var taskHint string
	if taskID != "" {
		taskHint = taskHintFor(taskID, beadType)
	}

	var monorepoHint string
	if os.Getenv("BOAT_REFERENCE_REPOS") != "" {
		cwd, _ := os.Getwd()
		monorepoHint = referenceReposHint(project, cwd, os.Getenv("KD_WORKSPACE"))
	}

	if project == "" {
		project = "gardenio"
	}

	return nudgeVars{
		Project:      project,
		Role:         os.Getenv("BOAT_ROLE"),
		ProjectHint:  projectHint,
		TaskHint:     taskHint,
		MonorepoHint: monorepoHint,
		// effectiveBoatPrompt, not the raw env var, so a stale plan step-dispatch
		// (target step already closed) resolves to "" and the adhoc template's
		// "Here is your task: " tail renders empty instead of re-emitting a dead
		// dispatch on every respawn (ka-0000002376).
		BoatPrompt: effectiveBoatPrompt(),
	}
}

// taskHintFor builds the pre-assignment nudge sentence for a task ID, choosing
// the wording based on the bead's TYPE.
//
// gb and kd disagreed about what an "assignment" is (ka-0000005855): the nudge
// spoke pure task-claim language ("Run `kd claim <id>`") for ANY non-closed
// pre-assigned bead, but `kd claim` hard-rejects type=epic
// (kbeads/cmd/kd/claim.go — "cannot claim an epic — epics are planning
// containers, not actionable work"). Standing gardeners are legitimately anchored
// to a standing epic via an `assigned` dependency (no claim), so every such
// spawn opened with a guaranteed `kd claim` error — a recurring token/latency
// tax on lanes like triage-intake (~48 contradictions/day).
//
// For claimable types (task/bug/feature and anything else kd accepts) the wording
// is unchanged. For type=epic we emit anchor language that steers the agent to its
// mission/children instead of an impossible claim. kd rejects ONLY type=epic, so
// that is the sole special case here — widening it (e.g. to molecule, which kd DOES
// let you claim) would reintroduce the inverse contradiction.
func taskHintFor(taskID, beadType string) string {
	if beadType == "epic" {
		return fmt.Sprintf(" You are anchored to standing epic `%s`. Run `kd show %s` for your mission; do NOT claim it — epics are planning containers, not claimable work. Work its child tasks (`kd list --parent %s -s open`) or your mission loop.", taskID, taskID, taskID)
	}
	return fmt.Sprintf(" You have been pre-assigned to task `%s`. Run `kd show %s` for details, then `kd claim %s` to start work on it.", taskID, taskID, taskID)
}

// referenceReposHint builds the nudge sentence that tells an agent where its
// reference repos actually live. Two pod layouts exist and the earlier static
// hint (ka-0000003185) only described the first, so a workspace/job-mode pod was
// told the exact inverse of the truth and burned commands re-orienting
// (ka-0000005484). gb nudge-prompt runs IN-POD at spawn (init_agent.go resolves
// it after clones), so we can stat the candidates and emit the real path:
//
//   - init-clone pods: repos at /home/agent/bot/{project}/repos, a SIBLING of the
//     primary checkout ({project}/work == cwd) — reached via `ls ../repos/`.
//   - workspace/job-mode pods: setup_repos.go + init_agent_setup.go clone into
//     {workspace}/repos where workspace == cwd — reached via `ls repos/`.
//
// The `ls ../repos/` sibling command is only correct when cwd is the init-clone
// checkout ({botHome}/{project}/work); from a workspace/job-mode cwd it points at
// the wrong directory (ka-0000005591). So we order the candidates by which layout
// the agent's cwd actually makes valid — cwd == {project}/work prefers the sibling,
// anything else prefers {cwd}/repos — then describe the first ordered candidate
// that exists. If none exist yet (nudge resolved before clones finish) we name
// both so the agent tries each rather than trusting a guess.
func referenceReposHint(project, cwd, kdWorkspace string) string {
	return referenceReposHintFrom("/home/agent/bot", project, cwd, kdWorkspace)
}

// referenceReposHintFrom is the testable core of referenceReposHint. botHome is
// the mount root under which init-clone pods place repos ({botHome}/{project}/repos);
// production passes "/home/agent/bot", tests pass a t.TempDir().
func referenceReposHintFrom(botHome, project, cwd, kdWorkspace string) string {
	const suffix = " Your primary repo is your current working directory."

	type candidate struct{ path, ls string }
	var candidates []candidate
	add := func(path, ls string) {
		if path == "" {
			return
		}
		for _, c := range candidates {
			if c.path == path {
				return
			}
		}
		candidates = append(candidates, candidate{path: path, ls: ls})
	}

	// Two layout candidates, added in the order the AGENT's cwd makes correct —
	// not merely whichever path happens to exist at prompt-generation time
	// (ka-0000005591). The `ls ../repos/` sibling form is only valid when cwd is
	// the init-clone checkout ({botHome}/{project}/work), because only then does
	// `../repos` resolve to {botHome}/{project}/repos. From a workspace/job-mode
	// cwd it is structurally wrong regardless of whether a (possibly stale)
	// {botHome}/{project}/repos exists, so we must NOT let that shadow the real
	// {cwd}/repos the agent actually reads.
	initReposDir := ""
	if botHome != "" && project != "" {
		initReposDir = filepath.Join(botHome, project, "repos")
	}
	// Init-clone layout: sibling of the {project}/work checkout, reached via
	// `ls ../repos/`.
	addInitClone := func() {
		if initReposDir != "" {
			add(initReposDir, "ls ../repos/")
		}
	}
	// Workspace/job-mode layout: repos under cwd, reached via `ls repos/`.
	addWorkspace := func() {
		if cwd != "" {
			add(filepath.Join(cwd, "repos"), "ls repos/")
		}
	}
	// cwd IS the init-clone checkout => `ls ../repos/` is provably correct, and
	// {cwd}/repos would be a subdir of the primary repo, not the reference repos.
	// Prefer the sibling. Otherwise the agent runs in a workspace root, so the
	// cwd-relative `ls repos/` is the one that actually works — prefer it.
	if botHome != "" && project != "" && cwd == filepath.Join(botHome, project, "work") {
		addInitClone()
		addWorkspace()
	} else {
		addWorkspace()
		addInitClone()
	}
	// KD_WORKSPACE/repos when it differs from cwd (defensive: some pods set
	// KD_WORKSPACE to the checkout root while cwd is elsewhere).
	if kdWorkspace != "" && kdWorkspace != cwd {
		p := filepath.Join(kdWorkspace, "repos")
		add(p, "ls "+p)
	}

	for _, c := range candidates {
		if dirExists(c.path) {
			return fmt.Sprintf(" Your workspace has additional (reference) repos cloned at `%s` (absolute: `%s`). Run `%s` to see them.%s", c.ls, c.path, c.ls, suffix)
		}
	}

	// Neither exists yet — the nudge resolved before clones finished. Name every
	// candidate so the agent probes rather than trusting a stale guess.
	paths := make([]string, 0, len(candidates))
	for _, c := range candidates {
		paths = append(paths, "`"+c.path+"`")
	}
	if len(paths) == 0 {
		// No project and no cwd — degenerate; keep a generic hint.
		return " Your workspace has additional (reference) repos under a `repos/` directory near your checkout." + suffix
	}
	return fmt.Sprintf(" Your workspace has additional (reference) repos cloned under a `repos/` directory — depending on pod layout it is at %s. Try each until one lists the repos.%s", strings.Join(paths, " or "), suffix)
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// resolveTaskFromDeps resolves a pre-assigned task ID from the agent bead's
// dependencies (type "assigned"). This replaces the entrypoint's bash-based
// dependency resolution, moving it into Go for reliability.
func resolveTaskFromDeps() string {
	agentBeadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if agentBeadID == "" || daemon == nil {
		return ""
	}
	ctx := context.Background()
	deps, err := daemon.GetDependencies(ctx, agentBeadID)
	if err != nil {
		return ""
	}
	for _, d := range deps {
		if d.Type == "assigned" {
			return d.DependsOnID
		}
	}
	return ""
}

// resolveNudgeFromConfig queries nudge-prompts config beads and returns
// the resolved prompt for the given type. Returns "" if no config beads exist.
func resolveNudgeFromConfig(promptType string, vars nudgeVars) string {
	tmpl, err := resolveNudgeTemplate(promptType)
	if err != nil {
		return ""
	}
	return substituteNudgeVars(tmpl, vars)
}

// nudgeSubscriptions is the config-bead subscription set the nudge prompt
// resolves against: global, then project:<p>, then role:<r>.
func nudgeSubscriptions() []string {
	subs := []string{"global"}
	if project := defaultGBProject(); project != "" {
		subs = append(subs, "project:"+project)
	}
	if role := os.Getenv("BOAT_ROLE"); role != "" {
		subs = append(subs, "role:"+role)
	}
	return subs
}

// resolveNudgeTemplate returns the raw (unsubstituted) nudge-prompts template
// for promptType, or an error saying WHY it could not: no daemon client, the
// config-bead listing failed, no bead matched the subscriptions, or the merged
// config has no template for this type.
//
// The listing goes through resolveConfigBeadsWithRetry. gd-PPzLrS2k8e: a plan
// gardener resolved its prompt at boot, then fell back to "Run gb ready" on 16
// straight re-injects after a credential-switch restart, and the old single-shot
// call discarded the error, so the log never said which of these it was.
func resolveNudgeTemplate(promptType string) (string, error) {
	subs := nudgeSubscriptions()
	if daemon == nil {
		return "", fmt.Errorf("no beads daemon client (subscriptions %v)", subs)
	}

	merged, count, err := resolveConfigBeadsWithRetry(context.Background(), daemon, "nudge-prompts", subs)
	if err != nil {
		return "", fmt.Errorf("listing nudge-prompts config beads (subscriptions %v): %w", subs, err)
	}
	if count == 0 {
		return "", fmt.Errorf("no nudge-prompts config bead matches subscriptions %v", subs)
	}

	val, ok := merged[promptType].(string)
	if !ok || val == "" {
		return "", fmt.Errorf("nudge-prompts config (%d layers, subscriptions %v) has no %q template", count, subs, promptType)
	}
	return val, nil
}

// substituteNudgeVars replaces {{.Field}} placeholders in the template.
func substituteNudgeVars(tmpl string, vars nudgeVars) string {
	r := strings.NewReplacer(
		"{{.Project}}", vars.Project,
		"{{.Role}}", vars.Role,
		"{{.ProjectHint}}", vars.ProjectHint,
		"{{.TaskHint}}", vars.TaskHint,
		"{{.MonorepoHint}}", vars.MonorepoHint,
		"{{.BoatPrompt}}", vars.BoatPrompt,
	)
	return r.Replace(tmpl)
}
