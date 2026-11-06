// Package main — `gb trigger` subcommand tree.
//
// Provides ergonomic helpers for creating, listing, enabling, and disabling
// trigger beads (type=trigger, kind=data). Trigger beads drive the shared
// TriggerEngine in internal/bridge — see ParseTriggerDef and the various
// trigger_source_*.go files for the canonical schema.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// Command tree
// ---------------------------------------------------------------------------

var triggerCmd = &cobra.Command{
	Use:     "trigger",
	Short:   "Manage trigger beads (gitlab, jira, webhook, ...)",
	GroupID: "orchestration",
	Long: `Trigger beads (type=trigger, kind=data) wire external events to actions
(notify, create_bead, apply_formula, spawn_agent) via the shared TriggerEngine.

Subcommands:
  create gitlab-pipeline   Create a trigger that fires on GitLab pipeline status
  create gitlab-mr         Create a trigger that fires on GitLab merge requests
  create jira              Create a trigger driven by a JIRA JQL query
  create webhook           Create a generic inbound webhook trigger
  create bead              Create a bead-state trigger (lifecycle events)
  create schedule          Create a cron-based schedule trigger (with optional bead-query gating)
  list                     List existing triggers (optionally filter by source)
  enable <id>              Re-enable a disabled trigger
  disable <id>             Disable a trigger without deleting it
  reseal <id>...           Renew the spawn-lineage seal (gardenio-admin login)`,
}

var triggerCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new trigger bead",
}

// --- shared fields ---------------------------------------------------------

// commonTriggerOpts collects the source-agnostic flags every `gb trigger create`
// subcommand exposes. They map onto the fields parsed by ParseTriggerDef.
type commonTriggerOpts struct {
	title           string
	action          string
	role            string
	agentProject    string
	prompt          string
	formula         string
	vars            []string
	cooldown        string
	maxConcurrent   int
	enabled         bool
	allowedProjects []string
	allowedRoles    []string
	extraLabels     []string
	slackChannel    string

	// create_bead action fields. Read by ParseTriggerDef / createTaskBead
	// (internal/bridge/triggers.go) to shape the bead the create_bead action
	// (and the create_bead leg of an assert's on_violation) files. Empty
	// values leave the server/engine defaults (type=task, default priority,
	// no parent). The builtin type:trigger schema accepts these fields
	// (kbeads/internal/server/config.go).
	//
	// beadPriority is a *int so the struct zero value (nil) unambiguously means
	// "unset" — P0 (0) is a legitimate priority, so a bare int sentinel cannot
	// distinguish "leave default" from "set P0". addCommonFlags binds it via a
	// backing int + a PreRun that promotes the flag to the pointer only when
	// --bead-priority was explicitly passed.
	beadType        string
	beadPriority    *int
	beadPriorityRaw int // flag backing store; promoted to beadPriority when --bead-priority is set
	parentBead      string
	beadLabels      []string

	// wakeIntent is the operator's natural-language statement of WHEN this
	// trigger should wake — the intent, split from the cron/conditions
	// mechanism (Slack thread ka-0000007909). Stored verbatim in the
	// `wake_intent` field; read only by the periodic wake-intent reconciler,
	// never by the fire path. See internal/bridge/triggers.go TriggerDef.WakeIntent.
	wakeIntent string
}

// addCommonFlags registers the shared trigger flags onto a cobra command.
func addCommonFlags(cmd *cobra.Command, o *commonTriggerOpts) {
	cmd.Flags().StringVar(&o.title, "title", "", "human-readable trigger title (required)")
	cmd.Flags().StringVar(&o.action, "action", "notify", "action to take: notify | create_bead | apply_formula | spawn_agent | wake_pr_owner")
	cmd.Flags().StringVar(&o.role, "role", "", "agent role to spawn (spawn_agent; also the apply_formula molecule driver, default crew)")
	cmd.Flags().StringVar(&o.agentProject, "agent-project", defaultProject(), "project name applied to spawned beads (defaults to KD_PROJECT)")
	cmd.Flags().StringVar(&o.prompt, "prompt", "", "prompt template for spawn_agent / message body for notify")
	cmd.Flags().StringVar(&o.formula, "formula", "", "formula bead ID for --action=apply_formula")
	cmd.Flags().StringArrayVar(&o.vars, "var", nil, "K=V variables for the formula (repeatable)")
	cmd.Flags().StringVar(&o.cooldown, "cooldown", "5m", "minimum interval between firings (e.g. 5m, 90s, 2)")
	cmd.Flags().IntVar(&o.maxConcurrent, "max-concurrent", 3, "max actions fired per evaluation cycle")
	cmd.Flags().BoolVar(&o.enabled, "enabled", true, "enable the trigger immediately")
	cmd.Flags().StringSliceVar(&o.allowedProjects, "allowed-project", nil, "restrict targeted projects (repeatable, * = any)")
	cmd.Flags().StringSliceVar(&o.allowedRoles, "allowed-role", nil, "restrict spawned roles (repeatable, * = any)")
	cmd.Flags().StringSliceVar(&o.extraLabels, "label", nil, "additional labels to attach (repeatable)")
	cmd.Flags().StringVar(&o.slackChannel, "slack-channel", "", "Slack channel ID (e.g. C073CG65QUD) this trigger routes to: for --action=notify the notification posts here instead of the default channel; for --action=spawn_agent the spawned agent's card/thread/squawks route here")
	// create_bead action shaping (also applies to an assert's create_bead leg).
	cmd.Flags().StringVar(&o.beadType, "bead-type", "", "type of the bead filed by --action=create_bead / --on-violation create_bead|both: task (default) | bug | feature | chore")
	cmd.Flags().IntVar(&o.beadPriorityRaw, "bead-priority", 0, "priority (0-4) stamped on the filed bead for create_bead actions; unset leaves the server default")
	cmd.Flags().StringVar(&o.parentBead, "parent", "", "parent epic the filed bead is wired under (parent-child) for create_bead actions")
	cmd.Flags().StringSliceVar(&o.beadLabels, "bead-label", nil, "extra label on the FILED bead (create_bead actions), beyond source:/trigger:/project: (repeatable)")
	cmd.Flags().StringVar(&o.wakeIntent, "wake-intent", "", "natural-language statement of WHEN this trigger should wake (the intent, audited by the wake-intent reconciler vs. the cron/conditions mechanism)")

	// Promote --bead-priority into the *int only when the user explicitly set
	// it, so P0 (0) is distinguishable from "unset" (the struct zero, nil).
	prev := cmd.PreRunE
	cmd.PreRunE = func(c *cobra.Command, args []string) error {
		if c.Flags().Changed("bead-priority") {
			p := o.beadPriorityRaw
			o.beadPriority = &p
		}
		if prev != nil {
			return prev(c, args)
		}
		return nil
	}
}

// applyCommonFields merges the shared flags into the trigger fields map and
// returns the labels that should be attached to the bead.
func applyCommonFields(o commonTriggerOpts, fields map[string]any) error {
	if o.action == "" {
		o.action = "notify"
	}
	switch o.action {
	case "notify", "create_bead", "apply_formula", "spawn_agent", "assert", "wake_pr_owner":
	default:
		return fmt.Errorf("invalid --action %q (want notify|create_bead|apply_formula|spawn_agent|assert|wake_pr_owner)", o.action)
	}
	if o.action == "apply_formula" && o.formula == "" {
		return fmt.Errorf("--action=apply_formula requires --formula <bead-id>")
	}
	if o.action == "spawn_agent" && o.role == "" {
		return fmt.Errorf("--action=spawn_agent requires --role <role>")
	}

	mins, err := parseCooldownMinutes(o.cooldown)
	if err != nil {
		return err
	}
	if o.maxConcurrent < 0 {
		return fmt.Errorf("--max-concurrent must be >= 0")
	}

	fields["action"] = o.action
	fields["enabled"] = o.enabled
	fields["cooldown_minutes"] = mins
	fields["max_concurrent"] = o.maxConcurrent

	if o.role != "" {
		fields["role"] = o.role
	}
	if o.agentProject != "" {
		fields["project"] = o.agentProject
	}
	if o.prompt != "" {
		fields["prompt"] = o.prompt
	}
	if o.formula != "" {
		fields["formula_id"] = o.formula
	}
	if vars := joinKeyValuePairs(o.vars); vars != "" {
		fields["formula_vars"] = vars
	}
	if len(o.allowedProjects) > 0 {
		fields["allowed_projects"] = strings.Join(o.allowedProjects, ",")
	}
	if len(o.allowedRoles) > 0 {
		fields["allowed_roles"] = strings.Join(o.allowedRoles, ",")
	}
	// slack_channel routes this trigger's output to a specific channel: the
	// notify action posts the notification there (ka-0000008204); a spawn_agent
	// trigger stamps it as schedule_slack_channel on each spawned agent bead
	// (ka-0000005119 / ka-0000003647). Parsed into TriggerDef.SlackChannel.
	if sc := strings.TrimSpace(o.slackChannel); sc != "" {
		fields["slack_channel"] = sc
	}

	// create_bead action shaping (also the create_bead leg of an assert's
	// on_violation). These fields are read by ParseTriggerDef / createTaskBead
	// (internal/bridge/triggers.go). Validate here so a malformed bead_type /
	// priority is rejected at authoring time, not silently at fire time.
	if o.beadType != "" {
		switch o.beadType {
		case "task", "bug", "feature", "chore":
		default:
			return fmt.Errorf("invalid --bead-type %q (want task|bug|feature|chore)", o.beadType)
		}
		fields["bead_type"] = o.beadType
	}
	if o.beadPriority != nil {
		if *o.beadPriority < 0 || *o.beadPriority > 4 {
			return fmt.Errorf("--bead-priority must be 0-4, got %d", *o.beadPriority)
		}
		fields["bead_priority"] = *o.beadPriority
	}
	if o.parentBead != "" {
		fields["parent"] = o.parentBead
	}
	if len(o.beadLabels) > 0 {
		fields["extra_labels"] = strings.Join(o.beadLabels, ",")
	}
	if wi := strings.TrimSpace(o.wakeIntent); wi != "" {
		fields["wake_intent"] = wi
	}
	return nil
}

// parseCooldownMinutes accepts go-style durations ("5m", "90s") and bare
// integer minutes ("5"). Result is rounded down to whole minutes (the engine
// stores cooldown as integer minutes). 0 is allowed (no cooldown).
func parseCooldownMinutes(in string) (int, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return 5, nil
	}
	if n, err := strconv.Atoi(in); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("--cooldown must be >= 0")
		}
		return n, nil
	}
	d, err := time.ParseDuration(in)
	if err != nil {
		return 0, fmt.Errorf("--cooldown %q is neither a number nor a duration: %w", in, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("--cooldown must be >= 0")
	}
	return int(d / time.Minute), nil
}

// joinKeyValuePairs renders a list of K=V pairs as a JSON object string,
// matching the format expected by ExpandTemplate downstream. Pairs without an
// `=` are treated as keys with empty values; later pairs override earlier ones.
func joinKeyValuePairs(pairs []string) string {
	if len(pairs) == 0 {
		return ""
	}
	out := make(map[string]string, len(pairs))
	keys := make([]string, 0, len(pairs))
	for _, p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, exists := out[k]; !exists {
			keys = append(keys, k)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return ""
	}
	// Stable, deterministic key ordering for testability.
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(out[k])
		b.Write(kj)
		b.WriteByte(':')
		b.Write(vj)
	}
	b.WriteByte('}')
	return b.String()
}

// ---------------------------------------------------------------------------
// gitlab-pipeline subcommand
// ---------------------------------------------------------------------------

var (
	gitlabPipelineOpts        commonTriggerOpts
	gitlabPipelineProject     string
	gitlabPipelineStatus      string
	gitlabPipelineRef         string
	gitlabPipelineRefPattern  string
	gitlabPipelineMaxAge      string
	gitlabPipelineMinDuration string
)

var triggerCreateGitLabPipelineCmd = &cobra.Command{
	Use:   "gitlab-pipeline",
	Short: "Create a GitLab pipeline-status trigger",
	Long: `Create a trigger that fires when a GitLab pipeline reaches a given status.

Example (react to a broken main pipeline):
  gb trigger create gitlab-pipeline \
    --project example-org/monorepo \
    --status failed \
    --ref main \
    --action apply_formula \
    --formula ka-XYZ \
    --var PIPELINE_URL='{{gitlab_pipeline_url}}' \
    --cooldown 5m \
    --max-concurrent 2

Example (wake ONLY on a slow GREEN merge train — status + ref-pattern + min-duration):
  gb trigger create gitlab-pipeline \
    --project example-org/monorepo \
    --status success \
    --ref-pattern '^refs/merge-requests/[0-9]+/train$' \
    --min-duration 45m \
    --action apply_formula \
    --formula ka-XYZ`,
	RunE: runTriggerCreateGitLabPipeline,
}

func runTriggerCreateGitLabPipeline(cmd *cobra.Command, args []string) error {
	if gitlabPipelineProject == "" {
		return fmt.Errorf("--project is required (e.g. example-org/monorepo)")
	}
	if gitlabPipelineStatus == "" {
		return fmt.Errorf("--status is required (e.g. failed, success, canceled)")
	}

	fields, labels, err := buildGitLabPipelineFields(gitlabPipelineOpts, gitlabPipelineProject, gitlabPipelineStatus, gitlabPipelineRef, gitlabPipelineRefPattern, gitlabPipelineMaxAge, gitlabPipelineMinDuration)
	if err != nil {
		return err
	}

	id, err := createTriggerBead(cmd.Context(), gitlabPipelineOpts.title, "trigger:gitlab", fields, labels, gitlabPipelineOpts.extraLabels)
	if err != nil {
		return err
	}

	printTriggerCreated(cmd, id, "gitlab-pipeline", map[string]string{
		"project":      gitlabPipelineProject,
		"status":       gitlabPipelineStatus,
		"ref":          gitlabPipelineRef,
		"ref_pattern":  gitlabPipelineRefPattern,
		"min_duration": gitlabPipelineMinDuration,
		"test":         fmt.Sprintf("glab -R %s ci run --branch %s   # then watch trigger fire on failure", gitlabPipelineProject, firstNonEmpty(gitlabPipelineRef, "main")),
	})
	return nil
}

// buildGitLabPipelineFields constructs the bead field map and label list for a
// gitlab-pipeline trigger. Extracted so the field shape can be unit-tested
// without hitting the kbeads server.
func buildGitLabPipelineFields(o commonTriggerOpts, project, status, ref, refPattern, maxAge, minDuration string) (map[string]any, []string, error) {
	fields := map[string]any{
		"event_type":      "pipeline",
		"project_path":    project,
		"pipeline_status": status,
	}
	if ref != "" {
		fields["pipeline_ref"] = ref
	}
	// Optional client-side ref regexp, applied by the engine AFTER the API fetch
	// and BEFORE matches are returned. Compiled here so a bad pattern fails at
	// authoring time rather than fail-closed-at-runtime (which would silently
	// stop the trigger from ever firing). Complements the exact pipeline_ref:
	// train refs are per-MR, so a single exact ?ref= cannot select all trains.
	if refPattern != "" {
		if _, err := regexp.Compile(refPattern); err != nil {
			return nil, nil, fmt.Errorf("--ref-pattern %q: %w", refPattern, err)
		}
		fields["pipeline_ref_pattern"] = refPattern
	}
	// Recency window (updated_after) so a re-enabled/restarted trigger cannot
	// fire against pre-existing failures. Validated here so a bad value fails
	// at authoring time rather than being silently ignored by the engine
	// (which would fall back to the 2h default). Empty ⇒ engine default.
	if maxAge != "" {
		d, err := time.ParseDuration(maxAge)
		if err != nil {
			return nil, nil, fmt.Errorf("--max-age %q: %w", maxAge, err)
		}
		if d <= 0 {
			return nil, nil, fmt.Errorf("--max-age must be positive, got %q", maxAge)
		}
		fields["pipeline_max_age"] = maxAge
	}
	// Optional "slow pipeline" threshold. Validated here so a bad value fails at
	// authoring time rather than fail-closed-at-runtime (which would silently
	// stop the trigger from ever firing). Fire only when a pipeline's run
	// duration is at/over this threshold — lets the fleet wake on a train that
	// got SLOW, not only one that BROKE.
	if minDuration != "" {
		d, err := time.ParseDuration(minDuration)
		if err != nil {
			return nil, nil, fmt.Errorf("--min-duration %q: %w", minDuration, err)
		}
		if d <= 0 {
			return nil, nil, fmt.Errorf("--min-duration must be positive, got %q", minDuration)
		}
		fields["min_duration"] = minDuration
	}
	if err := applyCommonFields(o, fields); err != nil {
		return nil, nil, err
	}
	labels := []string{"trigger:gitlab", "trigger-source:gitlab-pipeline"}
	return fields, labels, nil
}

// ---------------------------------------------------------------------------
// gitlab-mr subcommand
// ---------------------------------------------------------------------------

var (
	gitlabMROpts         commonTriggerOpts
	gitlabMRProject      string
	gitlabMRState        string
	gitlabMRLabels       []string
	gitlabMRAuthor       string
	gitlabMRTargetBranch string
)

var triggerCreateGitLabMRCmd = &cobra.Command{
	Use:   "gitlab-mr",
	Short: "Create a GitLab merge-request trigger",
	RunE:  runTriggerCreateGitLabMR,
}

func runTriggerCreateGitLabMR(cmd *cobra.Command, args []string) error {
	if gitlabMRProject == "" {
		return fmt.Errorf("--project is required (e.g. example-org/monorepo)")
	}

	fields, labels, err := buildGitLabMRFields(gitlabMROpts, gitlabMRProject, gitlabMRState, gitlabMRLabels, gitlabMRAuthor, gitlabMRTargetBranch)
	if err != nil {
		return err
	}

	id, err := createTriggerBead(cmd.Context(), gitlabMROpts.title, "trigger:gitlab", fields, labels, gitlabMROpts.extraLabels)
	if err != nil {
		return err
	}

	printTriggerCreated(cmd, id, "gitlab-mr", map[string]string{
		"project": gitlabMRProject,
		"state":   firstNonEmpty(gitlabMRState, "opened"),
		"test":    strings.TrimSpace(fmt.Sprintf("glab -R %s mr list %s", gitlabMRProject, glabMRListFlagForState(gitlabMRState))),
	})
	return nil
}

func buildGitLabMRFields(o commonTriggerOpts, project, state string, mrLabels []string, author, targetBranch string) (map[string]any, []string, error) {
	fields := map[string]any{
		"event_type":   "merge_request",
		"project_path": project,
	}
	if state != "" {
		fields["state"] = state
	}
	if len(mrLabels) > 0 {
		fields["gitlab_labels"] = strings.Join(mrLabels, ",")
	}
	if author != "" {
		fields["author"] = author
	}
	if targetBranch != "" {
		fields["target_branch"] = targetBranch
	}
	if err := applyCommonFields(o, fields); err != nil {
		return nil, nil, err
	}
	labels := []string{"trigger:gitlab", "trigger-source:gitlab-mr"}
	return fields, labels, nil
}

// ---------------------------------------------------------------------------
// jira subcommand
// ---------------------------------------------------------------------------

var (
	jiraOpts    commonTriggerOpts
	jiraJQL     string
	jiraProject string
)

var triggerCreateJiraCmd = &cobra.Command{
	Use:   "jira",
	Short: "Create a JIRA JQL trigger",
	RunE:  runTriggerCreateJira,
}

func runTriggerCreateJira(cmd *cobra.Command, args []string) error {
	if jiraJQL == "" {
		return fmt.Errorf("--jql is required (e.g. 'project = DEVOPS AND status = Open')")
	}
	if jiraOpts.agentProject == "" && jiraProject != "" {
		jiraOpts.agentProject = jiraProject
	}

	fields, labels, err := buildJiraFields(jiraOpts, jiraJQL, jiraProject)
	if err != nil {
		return err
	}

	id, err := createTriggerBead(cmd.Context(), jiraOpts.title, "trigger:jira", fields, labels, jiraOpts.extraLabels)
	if err != nil {
		return err
	}

	printTriggerCreated(cmd, id, "jira", map[string]string{
		"jql":  jiraJQL,
		"test": "jira-bridge logs should report 'triggers: new matches' within one poll cycle",
	})
	return nil
}

func buildJiraFields(o commonTriggerOpts, jql, projectKey string) (map[string]any, []string, error) {
	fields := map[string]any{
		"jql": jql,
	}
	if projectKey != "" {
		fields["jira_project"] = projectKey
	}
	if err := applyCommonFields(o, fields); err != nil {
		return nil, nil, err
	}
	labels := []string{"trigger:jira"}
	return fields, labels, nil
}

// ---------------------------------------------------------------------------
// webhook subcommand
// ---------------------------------------------------------------------------

var (
	webhookOpts       commonTriggerOpts
	webhookToken      string
	webhookSecret     string
	webhookConditions []string
)

var triggerCreateWebhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Create a generic inbound webhook trigger",
	Long: `Create a trigger fired by HTTP POSTs to the webhook bridge.

Conditions use the form path:op:value, where op is one of:
  ==, !=, in, not_in, contains, present, absent

Example:
  gb trigger create webhook \
    --token deploy-failed \
    --secret $WEBHOOK_SECRET \
    --condition event.type:==:deploy_failed \
    --condition severity:in:high,critical`,
	RunE: runTriggerCreateWebhook,
}

func runTriggerCreateWebhook(cmd *cobra.Command, args []string) error {
	if webhookToken == "" {
		return fmt.Errorf("--token is required (becomes the path segment of the webhook URL)")
	}

	fields, labels, err := buildWebhookFields(webhookOpts, webhookToken, webhookSecret, webhookConditions)
	if err != nil {
		return err
	}

	id, err := createTriggerBead(cmd.Context(), webhookOpts.title, "trigger:webhook", fields, labels, webhookOpts.extraLabels)
	if err != nil {
		return err
	}

	printTriggerCreated(cmd, id, "webhook", map[string]string{
		"token": webhookToken,
		"test":  fmt.Sprintf("curl -X POST -H 'Content-Type: application/json' -d '{\"event\":{\"type\":\"deploy_failed\"}}' $GITLAB_BRIDGE_URL/webhook/%s", webhookToken),
	})
	return nil
}

func buildWebhookFields(o commonTriggerOpts, token, secret string, conditions []string) (map[string]any, []string, error) {
	fields := map[string]any{
		"webhook_token": token,
	}
	if secret != "" {
		fields["webhook_secret"] = secret
	}
	if len(conditions) > 0 {
		conds, err := parseWebhookConditions(conditions)
		if err != nil {
			return nil, nil, err
		}
		j, err := json.Marshal(conds)
		if err != nil {
			return nil, nil, fmt.Errorf("marshalling conditions: %w", err)
		}
		fields["conditions"] = string(j)
	}
	if err := applyCommonFields(o, fields); err != nil {
		return nil, nil, err
	}
	labels := []string{"trigger:webhook"}
	return fields, labels, nil
}

// webhookConditionJSON is the on-the-wire shape consumed by
// trigger_source_webhook.go's webhookCondition. We re-declare it locally so the
// CLI doesn't depend on an internal/ package's unexported type.
type webhookConditionJSON struct {
	Path  string `json:"path"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// parseWebhookConditions parses --condition path:op:value flags into the JSON
// structure stored on the trigger bead.
func parseWebhookConditions(in []string) ([]webhookConditionJSON, error) {
	out := make([]webhookConditionJSON, 0, len(in))
	for _, s := range in {
		parts := strings.SplitN(s, ":", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("invalid --condition %q (want path:op[:value])", s)
		}
		c := webhookConditionJSON{
			Path: strings.TrimSpace(parts[0]),
			Op:   strings.TrimSpace(parts[1]),
		}
		if len(parts) == 3 {
			c.Value = parts[2]
		}
		if c.Path == "" || c.Op == "" {
			return nil, fmt.Errorf("invalid --condition %q (path and op required)", s)
		}
		out = append(out, c)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// list / enable / disable
// ---------------------------------------------------------------------------

var (
	triggerListSource string
	triggerListLimit  int
	triggerListOffset int
)

// defaultTriggerListLimit is the page size `gb trigger list` requests when
// --limit is not given. It comfortably exceeds the live trigger fleet (~316 as
// of ka-0000005890) so the common case never truncates, while the total footer
// (human) and stderr note (--json) keep truncation from ever being SILENT if
// the fleet outgrows it. The old hardcoded 200 lied silently against a 316-row
// reality; this default plus the total surfacing is the fix.
const defaultTriggerListLimit = 500

var triggerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List trigger beads (optionally filter by source)",
	Long: `List trigger beads (optionally filter by source).

Pagination:
  --limit N   max rows to return (default 500; 0 = server default page cap)
  --offset N  skip the first N rows (page through large fleets)

The human table prints an accurate "N of M triggers" footer whenever the page
is short of the true total, and --json emits a one-line truncation note to
stderr (stdout stays a bare JSON array) so a count is never silently truncated.`,
	RunE: runTriggerList,
}

func runTriggerList(cmd *cobra.Command, args []string) error {
	if triggerListLimit < 0 {
		return fmt.Errorf("--limit must be >= 0, got %d", triggerListLimit)
	}
	if triggerListOffset < 0 {
		return fmt.Errorf("--offset must be >= 0, got %d", triggerListOffset)
	}

	q := beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open", "in_progress", "closed"},
		Limit:    triggerListLimit,
		Offset:   triggerListOffset,
	}
	if triggerListSource != "" {
		q.Labels = []string{"trigger:" + triggerListSource}
	}

	res, err := daemon.ListBeadsFiltered(cmd.Context(), q)
	if err != nil {
		return fmt.Errorf("listing trigger beads: %w", err)
	}

	// truncated reports whether this page is short of the true matching total —
	// either because the page cap clipped the tail or because --offset skipped
	// past some rows. Either way the count on screen is not the whole fleet.
	truncated := res.Total > triggerListOffset+len(res.Beads)

	if jsonOutput {
		// Keep stdout a BARE array (envelope was explicitly rejected,
		// ka-0000007254) so `... --json | jq` still indexes res[i] directly.
		// Surface the true total on STDERR only when truncated, so a
		// `--json | jq length` that under-counts is never a SILENT lie — the
		// operator sees the note and the paging hint. stdout is untouched.
		printJSON(res.Beads)
		if truncated {
			fmt.Fprintf(os.Stderr,
				"note: showing %d of %d triggers (raise --limit / use --offset to page; `kd count --type=trigger` for the bare total)\n",
				len(res.Beads), res.Total)
		}
		return nil
	}

	if len(res.Beads) == 0 {
		cmd.Println("No triggers found.")
		return nil
	}
	cmd.Printf("%-15s %-9s %-9s %-12s %s\n", "ID", "STATUS", "ENABLED", "SOURCE", "TITLE")
	cmd.Printf("%-15s %-9s %-9s %-12s %s\n",
		strings.Repeat("-", 15), strings.Repeat("-", 9), strings.Repeat("-", 9), strings.Repeat("-", 12), strings.Repeat("-", 40))
	for _, b := range res.Beads {
		enabled := "true"
		if b.Fields["enabled"] == "false" {
			enabled = "false"
		}
		src := triggerSourceFromLabels(b.Labels)
		cmd.Printf("%-15s %-9s %-9s %-12s %s\n", b.ID, b.Status, enabled, src, b.Title)
	}
	// Accurate total footer: only interesting when the page is short of the
	// true total (cap or offset), where the old silent 200-cap lied. A
	// full-fleet page needs no footer.
	if truncated {
		cmd.Printf("\n%d of %d triggers (raise --limit / use --offset to page)\n", len(res.Beads), res.Total)
	}
	return nil
}

// triggerSourceFromLabels returns the first "trigger:<source>" label value (or
// "?" if none is found). Used purely for the human-readable list view.
func triggerSourceFromLabels(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, "trigger:") && l != "trigger:disabled" {
			return strings.TrimPrefix(l, "trigger:")
		}
	}
	return "?"
}

var (
	// triggerEnableRebaseline, when set, resets a gardener trigger's per-bead
	// wall_clock baseline to now (through the nested `gardener` blob) so an
	// expired wall_clock window is granted a genuine fresh window, THEN enables.
	triggerEnableRebaseline bool
	// triggerEnableForce, when set, enables a gardener trigger with an
	// already-expired wall_clock window anyway (revive-as-is), accepting the
	// immediate re-close the guard warns about.
	triggerEnableForce bool
)

var triggerEnableCmd = &cobra.Command{
	Use:   "enable <trigger-id>",
	Short: "Enable a trigger (sets enabled=true)",
	Long: `Enable a trigger by setting enabled=true.

EXPIRED WALL_CLOCK GUARD. Reviving a gardener trigger whose
hard_stop wall_clock window has ALREADY expired is a silent no-op: the engine
re-closes it on the next reconcile having spawned ZERO runs (the window,
measured from max(created_at, grandfather epoch) or its last revival, is
already in the past). This command refuses that revive-into-instant-re-close by
default and points you at the two real fixes:

  --rebaseline   Reset the wall_clock baseline to now (a genuinely fresh window
                 measured from this instant) THROUGH the gardener blob, then
                 enable. This is the durable revive — use it unless you also
                 want to lengthen or re-scope, which you do by editing the blob
                 first (kd update <id> --field gardener=@file) and re-running.
  --force        Enable as-is anyway. The window is still expired, so expect an
                 immediate re-close with 0 runs (revive-as-is). Only sound if
                 you have separately confirmed the window is not expired.

Non-gardener triggers and gardeners with a still-open window are enabled directly.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setTriggerEnabled(cmd.Context(), args[0], true, triggerEnableOptions{
			rebaseline: triggerEnableRebaseline,
			force:      triggerEnableForce,
		})
	},
}

var triggerDisableCmd = &cobra.Command{
	Use:   "disable <trigger-id>",
	Short: "Disable a trigger (sets enabled=false)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setTriggerEnabled(cmd.Context(), args[0], false, triggerEnableOptions{})
	},
}

// triggerEnableOptions carries the revive-path modifiers for an enable. Both
// fields are ignored on a disable and on any non-gardener / non-expired enable.
type triggerEnableOptions struct {
	rebaseline bool // reset the wall_clock baseline to now, then enable
	force      bool // enable despite an expired wall_clock window
}

func setTriggerEnabled(ctx context.Context, beadID string, enabled bool, opts triggerEnableOptions) error {
	// Expired-wall_clock revive guard (bug ka-0000005774). Only the enable
	// direction on a GARDENER trigger with an already-expired hard_stop
	// wall_clock is at risk — a disable, a non-gardener trigger, or a gardener
	// whose window is still open all fall straight through to the flip below.
	if enabled {
		if err := guardTriggerEnableWallClock(ctx, beadID, opts); err != nil {
			return err
		}
	}

	if err := daemon.UpdateBeadFieldsTyped(ctx, beadID, map[string]any{
		"enabled": enabled,
	}); err != nil {
		return fmt.Errorf("updating trigger %s: %w", beadID, err)
	}
	// Phantom-prevention guard (ka-0000008103). The engine loads triggers BY
	// source label — an enabled, fire-shaped trigger (a `cron` field or a
	// non-empty triggers[]) with NO `trigger:*` label is a SILENT PHANTOM: it
	// reads enabled=true on every dashboard while the engine never evaluates it
	// (the exact ~3.5h dead activation on gardener ka-0000000680). We flip the
	// label at the same enable chokepoint the operator just used, closing the
	// window the twice-daily reactive sweep (gardener-autoblock-audit.py
	// is_phantom) otherwise leaves open. Best-effort + fail-open: the flip
	// already landed; a missing label surfaces on the next reactive sweep.
	if enabled {
		ensureTriggerSourceLabel(ctx, beadID)
	}
	if jsonOutput {
		printJSON(map[string]any{"id": beadID, "enabled": enabled})
	} else {
		state := "enabled"
		if !enabled {
			state = "disabled"
		}
		fmt.Printf("Trigger %s %s\n", beadID, state)
	}
	return nil
}

// ensureTriggerSourceLabel adds the computed `trigger:*` source label to a
// freshly-enabled trigger bead that is missing one, preventing the enabled-but-
// never-loaded PHANTOM class (ka-0000008103). The engine's per-source load
// queries filter by label (TriggerSourceLabelForFields), so an enabled bead
// with a fire shape (a `cron` field or a non-empty triggers[]) but NO trigger:*
// label reads alive on every dashboard while never being evaluated.
//
// It mirrors gardener-autoblock-audit.py is_phantom() as the enforced predicate:
//   - already has a trigger:* label  -> nothing to do (the common case).
//   - E2E fixture (fixture:e2e:*)     -> exempt by design (isE2EFixture; the
//     engine deliberately never fires these), so we never stamp one.
//   - no fire shape (no cron, empty triggers[]) -> not a phantom; leave it.
//   - otherwise                       -> add the computed source label. A bare
//     `cron` schedule bead gets trigger:schedule; a non-empty triggers[] gets
//     trigger:multi (via TriggerSourceLabelForFields), matching exactly what the
//     create path assembles in triggerBeadLabels.
//
// Best-effort and fail-open: the enable flip already committed, so a read/label
// error here must not wedge the operator's enable. A phantom left behind is
// caught by the twice-daily reactive sweep exactly as before — this only closes
// the window at the authoring chokepoint.
func ensureTriggerSourceLabel(ctx context.Context, beadID string) {
	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		return // fail open — never block on a read hiccup
	}
	// Already engine-loadable, or an exempt E2E fixture: nothing to do.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "trigger:") || strings.HasPrefix(l, "fixture:e2e:") {
			return
		}
	}
	// Only a fire-shaped bead can be a phantom. A bare cron field is a schedule;
	// a non-empty triggers[] is a multi.
	triggersField := bead.Fields["triggers"]
	hasCron := strings.TrimSpace(bead.Fields["cron"]) != ""
	hasTriggers := bridge.HasNonEmptyTriggers(triggersField)
	if !hasCron && !hasTriggers {
		return // no fire shape — not a phantom, don't invent a source
	}
	label := bridge.TriggerSourceLabelForFields("trigger:schedule", triggersField)
	if err := daemon.AddLabel(ctx, beadID, label); err != nil {
		fmt.Fprintf(os.Stderr, "warning: enabled trigger %s but failed to add source label %q (phantom risk until next sweep): %v\n", beadID, label, err)
		return
	}
	if !jsonOutput {
		fmt.Printf("Trigger %s: added missing source label %s (was enabled without one — phantom-prevention, ka-0000008103)\n", beadID, label)
	}
}

// guardTriggerEnableWallClock enforces the expired-wall_clock revive guard for
// `gb trigger enable` (bug ka-0000005774):
//
//   - A daemon read error, or a non-gardener / no-blob / still-open-window
//     gardener, is a clean pass (nil) — the caller enables directly.
//   - An EXPIRED window with neither --rebaseline nor --force is REFUSED with an
//     actionable error naming both fixes, so the operator never lands the
//     revive-into-instant-re-close the bug is about.
//   - --rebaseline stamps wall_clock_since=now THROUGH the gardener blob (a
//     genuine fresh window measured from now), comments the change, and returns
//     nil so the caller only has the enable flip left.
//   - --force passes with a loud warning that the window is still expired
//     (revive-as-is), returning nil.
//
// The read is best-effort: a GetBead failure must not wedge the enable of a
// perfectly healthy trigger, so it fails OPEN (the engine's own guard still
// applies at fire time). The expiry verdict reuses gardenerWallClockExpiryStatus
// so the CLI and the engine agree on "expired" to the instant.
func guardTriggerEnableWallClock(ctx context.Context, beadID string, opts triggerEnableOptions) error {
	bead, gerr := daemon.GetBead(ctx, beadID)
	if gerr != nil || bead == nil {
		// Fail open — never block enabling a trigger because a read hiccuped.
		return nil
	}
	if strings.TrimSpace(bead.Fields["gardener"]) == "" {
		return nil // not a gardener — nothing to guard
	}

	view := gardenerWallClockExpiryStatus(bead, time.Now())
	if !view.present || !view.expired {
		// No computable hard_stop wall_clock, or the window is still open — a
		// normal enable. --rebaseline on a still-open window is a no-op-with-note
		// so an operator who passes it defensively is not surprised.
		if opts.rebaseline && view.present {
			fmt.Printf("Trigger %s wall_clock window is still open (expires %s) — --rebaseline not needed, enabling as-is.\n",
				beadID, view.expiry.UTC().Format(time.RFC3339))
		}
		return nil
	}

	// The window is EXPIRED. Choose the operator's path.
	switch {
	case opts.rebaseline:
		if rerr := rebaselineGardenerWallClock(ctx, bead); rerr != nil {
			return fmt.Errorf("rebaselining wall_clock for %s: %w", beadID, rerr)
		}
		fmt.Printf("Trigger %s: reset wall_clock baseline to now (fresh %s window) — enabling.\n",
			beadID, view.duration)
		return nil
	case opts.force:
		fmt.Printf("⚠️  Trigger %s: enabling with an EXPIRED wall_clock window (elapsed %s) — expect an immediate re-close with 0 runs (revive-as-is). Pass --rebaseline instead to reset the window.\n",
			beadID, view.expiry.UTC().Format(time.RFC3339))
		return nil
	default:
		return fmt.Errorf(
			"refusing to enable gardener trigger %s: its hard_stop wall_clock window is already EXPIRED "+
				"(%s window elapsed at %s, baseline %s%s). Enabling as-is is a silent no-op — the engine "+
				"re-closes it on the next reconcile with 0 runs (bug ka-0000005774).\n"+
				"  Fix it with ONE of:\n"+
				"    • gb trigger enable %s --rebaseline   reset the wall_clock baseline to now (a fresh %s window), then enable\n"+
				"    • edit the gardener blob to lengthen/re-scope (kd update %s --field gardener=@file), then re-run enable\n"+
				"    • gb trigger enable %s --force        enable as-is anyway (accepts the immediate re-close)",
			beadID, view.duration, view.expiry.UTC().Format(time.RFC3339),
			view.baseline.UTC().Format(time.RFC3339), grandfatheredNote(view.grandfathered),
			beadID, view.duration, beadID, beadID)
	}
}

// grandfatheredNote renders the parenthetical baseline-origin hint for the
// refuse message: a grandfathered gardener measures its window from the
// migration epoch, which is why a short duration is already expired.
func grandfatheredNote(grandfathered bool) string {
	if grandfathered {
		return ", floored to the grandfather epoch — the gardener predates it"
	}
	return ""
}

// rebaselineGardenerWallClock stamps the per-bead wall_clock_since revival
// override to now, written THROUGH the nested `gardener` blob (bug ka-0000005774,
// mirroring the engine's gardenerRearmFromOperatorReopen and the ka-0000004562
// blob-is-authoritative rule — a flat `--field wall_clock_since=` copy is
// inert). The scheduler measures the hard_stop wall_clock from
// max(wall_clock_since, created_at, epoch), so this grants a genuine fresh
// window measured from the revive instant while keeping the run/comment ledger
// on one bead.
func rebaselineGardenerWallClock(ctx context.Context, bead *beadsapi.BeadDetail) error {
	var spec bridge.GardenerSpec
	if err := json.Unmarshal([]byte(strings.TrimSpace(bead.Fields["gardener"])), &spec); err != nil {
		return fmt.Errorf("decoding gardener blob: %w", err)
	}
	spec.WallClockSince = time.Now()
	raw, err := json.Marshal(&spec)
	if err != nil {
		return fmt.Errorf("re-encoding gardener blob: %w", err)
	}
	if err := daemon.UpdateBeadFieldsTyped(ctx, bead.ID, map[string]any{
		"gardener": string(raw),
	}); err != nil {
		return err
	}
	comment := fmt.Sprintf(
		"wall_clock baseline reset to %s by `gb trigger enable --rebaseline` (%s): the hard_stop wall_clock "+
			"window had already expired, so reviving as-is would have re-closed on the next reconcile with 0 runs. "+
			"Stamped wall_clock_since through the gardener blob to grant a genuine fresh window (bug ka-0000005774).",
		spec.WallClockSince.UTC().Format(time.RFC3339), actor)
	if err := daemon.AddComment(ctx, bead.ID, actor, comment); err != nil {
		// Non-fatal: the durable reset already landed; the comment is annotation.
		fmt.Printf("warning: wall_clock rebaselined but comment failed on %s: %v\n", bead.ID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// triggerBeadLabels assembles the full label set for a trigger bead from the
// source label, any source-specific labels, the user-supplied --label values,
// and the labels DERIVED from fields (project: and role:). It is pure (no
// daemon) so the field→label mirroring is unit-testable.
//
// Field-derived labels (ka-0000002971): `applyCommonFields` writes project/role
// only as FIELDS, so a gardener made via `gb trigger create schedule
// --action spawn_agent --role gardener` got role=gardener but NO role:gardener
// label. /gardeners and the /maps fleet board query role:gardener by label OR
// field (ka-0000001950) so they DID draw it — but a label-only ground-truth
// query (the maps-beauty audit, a bare `kd list -l role:gardener`) does not,
// which is exactly the node_vs_truth_delta=-4 drift this bead tracks. Mirroring
// the field into a label at this single write chokepoint keeps every query —
// field, label, viewer, audit — in agreement, for ALL roles, not just gardener.
func triggerBeadLabels(sourceLabel string, fields map[string]any, sourceLabels, extraLabels []string) []string {
	// Mislabel guardrail (bug ka-0000001688): a bead with a non-empty triggers[]
	// array is a MULTI-trigger bead — it must carry trigger:multi, never
	// trigger:schedule. A trigger:schedule bead only fires its flat `cron` field;
	// its triggers[] event wakes are silently dropped (trigger_source_schedule.go).
	// Promote the source label at this single chokepoint so an authored gardener
	// with empty cron + event triggers[] can never be born mislabelled. Also
	// rewrites any caller-passed trigger:schedule in sourceLabels for consistency.
	if tf, _ := fields["triggers"].(string); bridge.HasNonEmptyTriggers(tf) {
		sourceLabel = bridge.TriggerSourceLabelForFields(sourceLabel, tf)
		for i, l := range sourceLabels {
			sourceLabels[i] = bridge.TriggerSourceLabelForFields(l, tf)
		}
	}
	labels := uniqueStrings(append(append([]string{sourceLabel}, sourceLabels...), extraLabels...))
	if proj, _ := fields["project"].(string); proj != "" {
		labels = uniqueStrings(append(labels, "project:"+proj))
	}
	if role, _ := fields["role"].(string); role != "" {
		labels = uniqueStrings(append(labels, "role:"+role))
	}
	return labels
}

// createTriggerBead persists a trigger bead via the kbeads HTTP API. The caller
// supplies the source label (e.g. "trigger:gitlab") plus any extra source-
// specific labels and user-supplied --label values.
func createTriggerBead(ctx context.Context, title, sourceLabel string, fields map[string]any, sourceLabels, extraLabels []string) (string, error) {
	if title == "" {
		title = generateTriggerTitle(sourceLabel, fields)
	}

	labels := triggerBeadLabels(sourceLabel, fields, sourceLabels, extraLabels)

	// Footgun guard (bug ka-0000006735): a prompt claiming run_budget/terminal_goal
	// with no `gardener` block and no `conditions` pre-flight only LOOKS gated —
	// the engine never reads budget/goal prose. Warn loudly so the author backs
	// it with a real gate before the unbounded loop bites.
	if markers, unbacked := bridge.FieldsUnbackedGardenerProse(fields); unbacked {
		fmt.Fprintln(os.Stderr, bridge.GardenerGateWarning("", markers))
	}

	// Repo-less git-path guard (bug ka-0000004796, task ka-0000004167): a
	// project-less (repo-less) spawn_agent trigger whose prompt names a git path
	// spawns a pod with NO working tree — the path 404s at runtime. Warn (never
	// reject — the corrected repo-less prompt legitimately names the path only to
	// say "do NOT open it"; a fail-closed reject would refuse the fix). Inline the
	// content into an advice body / bundle instead (advice ka-0000002990).
	if paths, unreachable := bridge.FieldsUnreachableRepoPaths(fields); unreachable {
		fmt.Fprintln(os.Stderr, bridge.RepoPathWarning("", paths))
	}

	// Gardener bounded-stop guard (ka-0000006744). FAIL-CLOSED at authoring time
	// for ALL gardeners: a trigger carrying a `gardener` blob MUST declare a
	// positive run_budget AND a bounded hard_stop wall_clock (advice
	// ka-0000000974). Refuse with a non-zero exit so an unbounded gardener is
	// never persisted — the firing-time warn-with-grace is a backstop for live
	// gardeners that predate the guard, not the first line of defense. A
	// source-only trigger (no gardener blob) is not a gardener and is untouched.
	if capBlob, _ := fields["gardener"].(string); strings.TrimSpace(capBlob) != "" {
		if v := bridge.ValidateGardenerBlob(capBlob); v != nil {
			return "", fmt.Errorf("%s", v.Error())
		}
		// Standing-marker guard (bug ka-0000004311). FAIL-CLOSED: a gardener whose
		// terminal_goal DECLARES it perpetual MUST carry a machine-readable
		// standing marker (gardener.standing:true OR the perpetual:true label) —
		// the engine reads standing-ness only from markers, never prose. Without
		// one the max_runs ceiling / resume give-up path reap it into a
		// closed-but-enabled dark gardener. `labels` already carries any
		// perpetual:true the author attached, so the guard sees the persisted set.
		if v := bridge.ValidateGardenerStandingBlob(capBlob, labels); v != nil {
			return "", fmt.Errorf("%s", v.Error())
		}
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("marshalling fields: %w", err)
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
		return "", fmt.Errorf("creating trigger bead: %w", err)
	}
	return id, nil
}

// generateTriggerTitle synthesises a default title when --title was not given.
// Matches the human-friendly form used in existing trigger beads (e.g.
// "Auto-crew on new DEVOPS ticket").
func generateTriggerTitle(sourceLabel string, fields map[string]any) string {
	f := func(key string) string {
		s, _ := fields[key].(string)
		return s
	}
	src := strings.TrimPrefix(sourceLabel, "trigger:")
	switch src {
	case "gitlab":
		switch f("event_type") {
		case "pipeline":
			return fmt.Sprintf("GitLab pipeline trigger: %s (%s)", f("project_path"), f("pipeline_status"))
		case "merge_request":
			return fmt.Sprintf("GitLab MR trigger: %s", f("project_path"))
		default:
			return fmt.Sprintf("GitLab trigger: %s", f("project_path"))
		}
	case "jira":
		if jp := f("jira_project"); jp != "" {
			return fmt.Sprintf("JIRA trigger: %s", jp)
		}
		return "JIRA trigger"
	case "webhook":
		return fmt.Sprintf("Webhook trigger: %s", f("webhook_token"))
	case "bead":
		if et := f("event_types"); et != "" {
			return fmt.Sprintf("Bead-state trigger: %s", et)
		}
		return "Bead-state trigger"
	case "schedule":
		if c := f("cron"); c != "" {
			return fmt.Sprintf("Schedule trigger: %s", c)
		}
		return "Schedule trigger"
	}
	return "Trigger"
}

// printTriggerCreated outputs the created trigger ID and a one-liner describing
// how to manually exercise it. Honours --json.
func printTriggerCreated(cmd *cobra.Command, id, kind string, info map[string]string) {
	if jsonOutput {
		out := map[string]any{
			"id":   id,
			"kind": kind,
		}
		for k, v := range info {
			out[k] = v
		}
		printJSON(out)
		return
	}
	cmd.Printf("Created %s trigger %s\n", kind, id)
	for _, k := range sortedKeys(info) {
		if v := info[k]; v != "" {
			cmd.Printf("  %s: %s\n", k, v)
		}
	}
	cmd.Println()
	cmd.Printf("Disable with: gb trigger disable %s\n", id)
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// glabMRListFlagForState maps a GitLab-API MR state value (opened/merged/closed/
// all — as stored on the trigger bead and accepted by the REST API) to the
// correct `glab mr list` CLI flag. glab is NOT gh: it has NO gh-style `--state`
// flag (`glab mr list --state opened` errors "Unknown flag: --state"). The real
// filters are bare `glab mr list` (open), -M/--merged, -c/--closed, -A/--all.
// Keeping the printed copy-paste "test" hint runnable stops it from re-teaching
// the broken form (recurrence chain ka-0000000464 → ka-0000003906).
func glabMRListFlagForState(state string) string {
	switch firstNonEmpty(state, "opened") {
	case "merged":
		return "--merged"
	case "closed":
		return "--closed"
	case "all":
		return "--all"
	default: // "opened" and any unknown value → glab's default (open)
		return ""
	}
}

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

func init() {
	// gitlab-pipeline
	addCommonFlags(triggerCreateGitLabPipelineCmd, &gitlabPipelineOpts)
	triggerCreateGitLabPipelineCmd.Flags().StringVar(&gitlabPipelineProject, "project", "", "GitLab project path (e.g. example-org/monorepo)")
	triggerCreateGitLabPipelineCmd.Flags().StringVar(&gitlabPipelineStatus, "status", "failed", "pipeline status filter (failed, success, canceled, running, pending)")
	triggerCreateGitLabPipelineCmd.Flags().StringVar(&gitlabPipelineRef, "ref", "", "exact branch/tag ref filter, threaded into GitLab's ?ref= (optional)")
	triggerCreateGitLabPipelineCmd.Flags().StringVar(&gitlabPipelineRefPattern, "ref-pattern", "", "Go regexp applied to each pipeline ref AFTER the API fetch; a non-matching ref never spawns an agent. Complements --ref for cases one exact ref can't express, e.g. all merge-trains plus main: '^(main|refs/merge-requests/[0-9]+/train)$'. Invalid regexp fails closed.")
	triggerCreateGitLabPipelineCmd.Flags().StringVar(&gitlabPipelineMaxAge, "max-age", "", "recency window (Go duration, e.g. 2h) — only fire on pipelines updated within this window; keeps a re-enabled/restarted trigger from firing on stale failures (default 2h)")
	triggerCreateGitLabPipelineCmd.Flags().StringVar(&gitlabPipelineMinDuration, "min-duration", "", "slow-pipeline threshold (Go duration, e.g. 45m) — fire ONLY on pipelines whose run duration is at/over this. Compose with --status success + --ref-pattern to wake on a slow GREEN merge train. Requires a DETAIL fetch per candidate (GitLab's list endpoint omits duration); a null/unknown duration never fires (fail closed). Invalid value fails at authoring time.")

	// gitlab-mr
	addCommonFlags(triggerCreateGitLabMRCmd, &gitlabMROpts)
	triggerCreateGitLabMRCmd.Flags().StringVar(&gitlabMRProject, "project", "", "GitLab project path (e.g. example-org/monorepo)")
	triggerCreateGitLabMRCmd.Flags().StringVar(&gitlabMRState, "state", "opened", "MR state filter (opened, merged, closed, all)")
	triggerCreateGitLabMRCmd.Flags().StringSliceVar(&gitlabMRLabels, "mr-label", nil, "filter by MR label (repeatable)")
	triggerCreateGitLabMRCmd.Flags().StringVar(&gitlabMRAuthor, "author", "", "filter by author username")
	triggerCreateGitLabMRCmd.Flags().StringVar(&gitlabMRTargetBranch, "target-branch", "", "filter by target branch")

	// jira
	addCommonFlags(triggerCreateJiraCmd, &jiraOpts)
	triggerCreateJiraCmd.Flags().StringVar(&jiraJQL, "jql", "", "JIRA JQL query (required)")
	triggerCreateJiraCmd.Flags().StringVar(&jiraProject, "jira-project", "", "JIRA project key (optional)")

	// webhook
	addCommonFlags(triggerCreateWebhookCmd, &webhookOpts)
	triggerCreateWebhookCmd.Flags().StringVar(&webhookToken, "token", "", "URL path token (required)")
	triggerCreateWebhookCmd.Flags().StringVar(&webhookSecret, "secret", "", "HMAC-SHA256 secret for signature verification")
	triggerCreateWebhookCmd.Flags().StringSliceVar(&webhookConditions, "condition", nil, "path:op:value condition (repeatable)")

	// bead
	addCommonFlags(triggerCreateBeadCmd, &beadOpts)
	triggerCreateBeadCmd.Flags().StringSliceVar(&beadEventTypes, "event-types", nil, "bead lifecycle events to match: created, updated, closed (repeatable; CSV accepted)")
	triggerCreateBeadCmd.Flags().StringArrayVar(&beadConditions, "condition", nil, "field:op:value condition on the bead (repeatable; field may be a top-level field, an extensible field key, or label:<name>)")

	// schedule
	addCommonFlags(triggerCreateScheduleCmd, &scheduleOpts)
	triggerCreateScheduleCmd.Flags().StringVar(&scheduleCron, "cron", "", "5-field cron expression (default: */30 * * * * — every 30 min; e.g. '0 9 * * 1-5', '*/30 9-17 * * 1-5', or '@hourly')")
	triggerCreateScheduleCmd.Flags().StringVar(&scheduleTimezone, "tz", "", "IANA timezone for cron evaluation (e.g. America/Los_Angeles); default UTC")
	triggerCreateScheduleCmd.Flags().StringArrayVar(&scheduleBeadQuery, "bead-query", nil, "pre-flight bead-query condition: 'labels=X|Y,statuses=open,op=>,value=0' (repeatable)")

	// assert (CEL expression sugar over bead-state triggers, F4 ka-0000008047)
	addCommonFlags(triggerCreateAssertCmd, &assertOpts)
	triggerCreateAssertCmd.Flags().StringSliceVar(&assertEventTypes, "event-types", nil, "bead lifecycle events to match: created, updated, closed (repeatable; CSV accepted)")
	triggerCreateAssertCmd.Flags().StringArrayVar(&assertExprs, "expr", nil, "CEL expression the bead must satisfy (repeatable; ANDed); compiled-on-write")
	triggerCreateAssertCmd.Flags().StringArrayVar(&assertConditions, "condition", nil, "additional field:op:value condition ANDed with --expr (repeatable); in --on-violation mode this is the event pre-filter")
	triggerCreateAssertCmd.Flags().StringVar(&assertOnViolation, "on-violation", "", "fire-on-violation mode (feature F5): notify | create_bead | both — --expr clauses become an invariant; the trigger fires when VIOLATED")

	// test (dry-run a trigger's CEL expression against a bead)
	triggerTestCmd.Flags().StringVar(&triggerTestBead, "bead", "", "bead ID to evaluate the expression against (required)")
	triggerTestCmd.Flags().StringVar(&triggerTestEvent, "event", "", "event type to simulate (created|updated|closed; default updated)")

	triggerCreateCmd.AddCommand(
		triggerCreateGitLabPipelineCmd,
		triggerCreateGitLabMRCmd,
		triggerCreateJiraCmd,
		triggerCreateWebhookCmd,
		triggerCreateBeadCmd,
		triggerCreateScheduleCmd,
		triggerCreateAssertCmd,
	)

	triggerListCmd.Flags().StringVar(&triggerListSource, "source", "", "filter by source (gitlab, jira, webhook, ...)")
	triggerListCmd.Flags().IntVar(&triggerListLimit, "limit", defaultTriggerListLimit, "max triggers to return (0 = server default page cap)")
	triggerListCmd.Flags().IntVar(&triggerListOffset, "offset", 0, "skip the first N triggers (page through large fleets)")

	// Expired-wall_clock revive guard flags (bug ka-0000005774).
	triggerEnableCmd.Flags().BoolVar(&triggerEnableRebaseline, "rebaseline", false, "reset an expired gardener's wall_clock baseline to now (fresh window) before enabling")
	triggerEnableCmd.Flags().BoolVar(&triggerEnableForce, "force", false, "enable a gardener with an expired wall_clock window as-is (accepts the immediate re-close)")

	triggerCmd.AddCommand(triggerCreateCmd)
	triggerCmd.AddCommand(triggerListCmd)
	triggerCmd.AddCommand(triggerEnableCmd)
	triggerCmd.AddCommand(triggerDisableCmd)
	triggerCmd.AddCommand(triggerTestCmd)

	rootCmd.AddCommand(triggerCmd)
}
