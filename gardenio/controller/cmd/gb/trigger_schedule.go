// Package main — `gb trigger create schedule` subcommand.
//
// Creates `trigger:schedule` beads — cron-driven triggers with optional
// pre-flight bead-query conditions. Authoring shape mirrors the other
// `gb trigger create` subcommands.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gardenio/controller/internal/scheduler"

	"github.com/spf13/cobra"
)

var (
	scheduleOpts      commonTriggerOpts
	scheduleCron      string
	scheduleTimezone  string
	scheduleBeadQuery []string
)

// defaultScheduleCron is the cadence applied when --cron is omitted: every 30
// minutes. Standing rule — gardeners/schedule loops should react fast, and the
// self-quiescing bead-query gate keeps idle schedules cheap.
const defaultScheduleCron = "*/30 * * * *"

var triggerCreateScheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Create a cron-based schedule trigger (with optional bead-query conditions)",
	Long: `Create a trigger that fires on a cron schedule, optionally gated by
pre-flight bead-query conditions. The trigger fires only when all
conditions pass at the cron fire-time.

A --bead-query is a comma-separated list of key=value pairs that describes a
single pre-flight check. Recognised keys:

  op       comparison operator: >, >=, <, <=, == (or eq), != (or ne).
           Default ">".
  value    integer threshold as a string. Default "0".
  labels   bead labels to filter (pipe-separated for multiple, e.g. labels=a|b).
  types    bead types to filter (pipe-separated).
  statuses bead statuses to filter (pipe-separated). Default "open".
  project  scope to a single project (also propagated as labels=project:<name>
           when no labels= is given).
  short_title_missing
           true to count only beads with no short_title (the short-title
           groomer's gate). Default false.

Examples:
  # Daily 9am, fire only when there are open issues in the gardenio project.
  gb trigger create schedule \
    --cron '0 9 * * *' \
    --tz America/Los_Angeles \
    --bead-query 'project=gardenio,types=task,statuses=open,op=>,value=0' \
    --action spawn_agent \
    --role triage \
    --agent-project gardenio \
    --prompt 'Daily triage pass for gardenio'

  # Every 30 minutes during business hours, no conditions.
  gb trigger create schedule \
    --cron '*/30 9-17 * * 1-5' \
    --action notify \
    --prompt 'Hourly check-in: {{fire_time_local}}'`,
	RunE: runTriggerCreateSchedule,
}

func runTriggerCreateSchedule(cmd *cobra.Command, args []string) error {
	if scheduleCron == "" {
		// Default to a 30-minute cadence. Gardeners/schedule loops want to react
		// fast; a multi-hour gap between supervision passes is too slow, and the
		// self-quiescing bead-query gate keeps an idle schedule from spawning
		// needless work, so the tight cadence is cheap. Operators override with
		// --cron for anything else (business-hours-only, daily, @hourly, etc.).
		scheduleCron = defaultScheduleCron
	}
	if _, err := scheduler.ParseCron(scheduleCron); err != nil {
		return fmt.Errorf("invalid --cron %q: %w", scheduleCron, err)
	}

	fields, labels, err := buildScheduleFields(scheduleOpts, scheduleCron, scheduleTimezone, scheduleBeadQuery)
	if err != nil {
		return err
	}

	id, err := createTriggerBead(cmd.Context(), scheduleOpts.title, "trigger:schedule", fields, labels, scheduleOpts.extraLabels)
	if err != nil {
		return err
	}

	info := map[string]string{
		"cron": scheduleCron,
		"tz":   firstNonEmpty(scheduleTimezone, "UTC"),
	}
	if len(scheduleBeadQuery) > 0 {
		info["conditions"] = fmt.Sprintf("%d bead-query", len(scheduleBeadQuery))
	}
	info["test"] = "wait for the next cron fire-time and check the controller logs for 'schedule trigger fired'"
	printTriggerCreated(cmd, id, "schedule", info)
	return nil
}

// buildScheduleFields constructs the bead field map and label list for a
// trigger:schedule trigger. Extracted so the field shape can be unit-tested
// without hitting the kbeads server.
func buildScheduleFields(o commonTriggerOpts, cron, tz string, beadQueries []string) (map[string]any, []string, error) {
	fields := map[string]any{
		"cron": cron,
	}
	if tz != "" {
		fields["timezone"] = tz
	}

	if len(beadQueries) > 0 {
		conds, err := parseBeadQueryConditions(beadQueries)
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
	labels := []string{"trigger:schedule"}
	return fields, labels, nil
}

// scheduleConditionJSON is the on-the-wire shape consumed by
// trigger_source_schedule.go's scheduleCondition. Re-declared locally so the
// CLI does not depend on an internal/ package's unexported type.
type scheduleConditionJSON struct {
	Type          string   `json:"type"`
	Op            string   `json:"op,omitempty"`
	Value         string   `json:"value,omitempty"`
	Project       string   `json:"project,omitempty"`
	Role          string   `json:"role,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
	Types         []string `json:"types,omitempty"`
	Statuses      []string `json:"statuses,omitempty"`
	// Parent scopes a bead_query to the transitive subtree under a parent
	// bead-id. Mirrors conditions.Condition.Parent; used by gardener
	// pre-flight conditions (the epic's open-children gate).
	Parent string `json:"parent,omitempty"`
	// ShortTitleMissing narrows the count to beads with no short_title.
	// Mirrors conditions.Condition.ShortTitleMissing; used by the short-title
	// groomer's pre-flight (gd-V5nGseRS5f).
	ShortTitleMissing bool `json:"short_title_missing,omitempty"`
}

// parseBeadQueryConditions parses one or more --bead-query flag values into a
// slice of `bead_query`-typed schedule conditions.
//
// Each value is a comma-separated list of key=value pairs. Pipe-separated
// values are split into arrays for labels/types/statuses. Defaults: op=">",
// value="0", statuses=["open"]. If `project=<name>` is given without a
// matching `labels=`, the project is automatically added as a
// "project:<name>" label so the count query stays scoped.
func parseBeadQueryConditions(in []string) ([]scheduleConditionJSON, error) {
	out := make([]scheduleConditionJSON, 0, len(in))
	for _, raw := range in {
		c, err := parseBeadQueryCondition(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// parseBeadQueryCondition parses a single --bead-query value (see
// parseBeadQueryConditions for the format).
func parseBeadQueryCondition(raw string) (scheduleConditionJSON, error) {
	c := scheduleConditionJSON{Type: "bead_query"}
	if strings.TrimSpace(raw) == "" {
		return c, fmt.Errorf("--bead-query value is empty")
	}

	hasLabels := false
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, hasEq := strings.Cut(part, "=")
		if !hasEq {
			return c, fmt.Errorf("invalid --bead-query segment %q (want key=value)", part)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "type":
			// Allow callers to pin a non-default condition type (e.g. ready_queue,
			// active_agents, gate). Documented as bead_query by default.
			c.Type = v
		case "op":
			c.Op = v
		case "value":
			c.Value = v
		case "project":
			c.Project = v
		case "role":
			c.Role = v
		case "labels":
			c.Labels = splitPipe(v)
			hasLabels = true
		case "exclude_labels":
			c.ExcludeLabels = splitPipe(v)
		case "types":
			c.Types = splitPipe(v)
		case "statuses":
			c.Statuses = splitPipe(v)
		case "parent":
			c.Parent = v
		case "short_title_missing":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return c, fmt.Errorf("invalid --bead-query short_title_missing=%q (want true or false)", v)
			}
			c.ShortTitleMissing = b
		default:
			return c, fmt.Errorf("unknown --bead-query key %q (want one of: type, op, value, project, role, labels, exclude_labels, types, statuses, parent, short_title_missing)", k)
		}
	}

	// Defaults that mirror the schedule source's expectations.
	if c.Op == "" {
		c.Op = ">"
	}
	if c.Value == "" {
		c.Value = "0"
	}
	if len(c.Statuses) == 0 && c.Type == "bead_query" {
		c.Statuses = []string{"open"}
	}
	// If the caller scoped to a project but did not list any labels, propagate
	// the project as a label so the count query stays correctly scoped.
	if c.Project != "" && !hasLabels && c.Type == "bead_query" {
		c.Labels = []string{"project:" + c.Project}
	}
	return c, nil
}

// splitPipe splits a pipe-separated value into a deduplicated slice. Empty
// segments are dropped; whitespace around segments is trimmed.
func splitPipe(in string) []string {
	if in == "" {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for p := range strings.SplitSeq(in, "|") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
