package bridge

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/pausegate"

	"github.com/slack-go/slack"
)

// handleScheduleCommand processes the /schedule slash command — the operator
// kill switch for ALL time-based / scheduled activity: cron-fired triggers AND
// every gardener loop. This is the command Matt asked for: "turn off all
// gardeners and triggers ... time-based (so gitlab and jira are not impacted)".
//
//	/schedule off            — pause ALL time-based firing globally: opens the
//	                           scheduler-pause gate (silences trigger:schedule +
//	                           schedule-mirror cron sources) AND the gardeners-pause
//	                           gate (halts every gardener loop fleet-wide).
//	/schedule on             — resume: closes both gates.
//	/schedule                — show scheduler + gardener status.
//	/schedule off <project>  — disable schedules for a specific project only
//	                           (sets schedules_enabled=false on the project bead).
//	/schedule on <project>   — enable schedules for a specific project.
//	/schedule <project>      — show schedule status for a specific project only.
//
// Event-driven trigger sources (jira, gitlab, webhook, slack-reaction, …) are
// NOT gated by this command — those are managed by /trigger. The two gates are
// orthogonal: /schedule targets time/loop-based activity, /trigger targets the
// whole trigger engine.
func (b *Bot) handleScheduleCommand(ctx context.Context, cmd slack.SlashCommand) {
	parts := strings.Fields(strings.TrimSpace(cmd.Text))

	if len(parts) == 0 {
		b.showScheduleStatus(ctx, cmd, "")
		return
	}

	action := strings.ToLower(parts[0])

	switch action {
	case "on":
		if len(parts) >= 2 {
			b.enableProjectSchedules(ctx, cmd, parts[1])
		} else {
			b.resumeSchedules(ctx, cmd)
		}
	case "off":
		if len(parts) >= 2 {
			b.disableProjectSchedules(ctx, cmd, parts[1])
		} else {
			b.pauseSchedules(ctx, cmd)
		}
	default:
		b.showScheduleStatus(ctx, cmd, action)
	}
}

// pauseSchedules engages the global kill switch for time-based activity. It
// opens two gate beads that the engine already honors:
//
//   - scheduler-pause: read in triggers.go loadGateState → skipForSchedule,
//     silences only trigger:schedule + schedule-mirror sources (gitlab/jira/
//     webhook keep firing).
//   - gardeners-pause: read in gardener_evaluator.go gardenerPreFireCheck, halts
//     ALL gardener firing fleet-wide regardless of source.
//
// Both are idempotent: an already-open gate is left untouched. Individual
// trigger/gardener beads are never modified — their enabled states are preserved
// so `/schedule on` restores the fleet exactly as it was.
func (b *Bot) pauseSchedules(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	schedGate, capGate, err := b.findScheduleGates(ctx)
	if err != nil {
		b.logger.Error("schedule command: failed to check pause state", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to check schedule state: "+err.Error(), false))
		return
	}

	var opened []string
	if schedGate == nil {
		if err := b.openGate(ctx, schedulerPauseTitleForTriggers, cmd.UserID); err != nil {
			b.logger.Error("schedule command: failed to open scheduler-pause gate", "error", err)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(th.Semantic(SemFail)+" Failed to pause schedules: "+err.Error(), false))
			return
		}
		opened = append(opened, "scheduled triggers")
	}
	if capGate == nil {
		if err := b.openGate(ctx, gardenersPauseTitle, cmd.UserID); err != nil {
			b.logger.Error("schedule command: failed to open gardeners-pause gate", "error", err)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(th.Semantic(SemFail)+" Failed to pause gardeners: "+err.Error(), false))
			return
		}
		opened = append(opened, "gardeners")
	}

	if len(opened) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemPaused)+" Schedules and gardeners are already paused.", false))
		return
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(th.Semantic(SemPaused)+" Time-based activity paused ("+strings.Join(opened, " + ")+"). "+
			"No cron schedules or gardener loops will fire until `/schedule on`. GitLab & Jira triggers are unaffected.", false))

	b.logger.Info("schedule command: time-based activity paused globally",
		"user", cmd.UserID, "opened", strings.Join(opened, ","))
}

// resumeSchedules releases the global kill switch by closing both the
// scheduler-pause and gardeners-pause gate beads. Each gardener/schedule resumes
// on its next evaluation with its individual enabled state intact.
func (b *Bot) resumeSchedules(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	schedGate, capGate, err := b.findScheduleGates(ctx)
	if err != nil {
		b.logger.Error("schedule command: failed to check pause state", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to check schedule state: "+err.Error(), false))
		return
	}

	var closed []string
	if schedGate != nil {
		if err := b.daemon.CloseBead(ctx, schedGate.ID, nil); err != nil {
			b.logger.Error("schedule command: failed to close scheduler-pause gate", "error", err)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(th.Semantic(SemFail)+" Failed to resume schedules: "+err.Error(), false))
			return
		}
		closed = append(closed, "scheduled triggers")
	}
	if capGate != nil {
		if err := b.daemon.CloseBead(ctx, capGate.ID, nil); err != nil {
			b.logger.Error("schedule command: failed to close gardeners-pause gate", "error", err)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(th.Semantic(SemFail)+" Failed to resume gardeners: "+err.Error(), false))
			return
		}
		closed = append(closed, "gardeners")
	}

	if len(closed) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemResumed)+" Schedules and gardeners are already running.", false))
		return
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(th.Semantic(SemResumed)+" Time-based activity resumed ("+strings.Join(closed, " + ")+"). "+
			"Schedules and gardeners will fire on their next evaluation.", false))

	b.logger.Info("schedule command: time-based activity resumed globally",
		"user", cmd.UserID, "closed", strings.Join(closed, ","))
}

// disableProjectSchedules sets schedules_enabled=false on the project bead and
// records an audit comment. This silences trigger:schedule + schedule-mirror
// firing for that project only (loadGateState → projectSchedulesDisabled →
// skipForSchedule).
func (b *Bot) disableProjectSchedules(ctx context.Context, cmd slack.SlashCommand, projectName string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	projectID, err := b.findProjectBeadID(ctx, projectName)
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" "+err.Error(), false))
		return
	}

	// schedules_enabled is boolean in the type:project schema — a first write
	// as the string "false" is 400-rejected by field validation (ka-0000003627).
	if err := b.daemon.UpdateBeadFieldsTyped(ctx, projectID, map[string]any{
		"schedules_enabled": false,
	}); err != nil {
		b.logger.Error("schedule command: failed to disable project schedules",
			"project", projectName, "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to disable schedules for "+projectName+": "+err.Error(), false))
		return
	}

	if err := b.daemon.AddComment(ctx, projectID, cmd.UserID,
		fmt.Sprintf(th.Semantic(SemPaused)+" schedules disabled by <@%s> via /schedule off", cmd.UserID)); err != nil {
		b.logger.Warn("schedule command: failed to add audit comment",
			"project", projectName, "error", err)
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemPaused)+" Schedules disabled for project *%s*. Use `/schedule on %s` to re-enable.", projectName, projectName), false))

	b.logger.Info("schedule command: project schedules disabled",
		"project", projectName, "user", cmd.UserID)
}

// enableProjectSchedules sets schedules_enabled=true on the project bead and
// records an audit comment.
func (b *Bot) enableProjectSchedules(ctx context.Context, cmd slack.SlashCommand, projectName string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	projectID, err := b.findProjectBeadID(ctx, projectName)
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" "+err.Error(), false))
		return
	}

	if err := b.daemon.UpdateBeadFieldsTyped(ctx, projectID, map[string]any{
		"schedules_enabled": true,
	}); err != nil {
		b.logger.Error("schedule command: failed to enable project schedules",
			"project", projectName, "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to enable schedules for "+projectName+": "+err.Error(), false))
		return
	}

	if err := b.daemon.AddComment(ctx, projectID, cmd.UserID,
		fmt.Sprintf(th.Semantic(SemResumed)+" schedules re-enabled by <@%s> via /schedule on", cmd.UserID)); err != nil {
		b.logger.Warn("schedule command: failed to add audit comment",
			"project", projectName, "error", err)
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemResumed)+" Schedules enabled for project *%s*.", projectName), false))

	b.logger.Info("schedule command: project schedules enabled",
		"project", projectName, "user", cmd.UserID)
}

// showScheduleStatus reports the global pause state of both gates, any
// per-project schedules_enabled=false flags, and a count of the scheduled
// triggers and gardeners that are (or would be) affected.
func (b *Bot) showScheduleStatus(ctx context.Context, cmd slack.SlashCommand, projectFilter string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	schedGate, capGate, err := b.findScheduleGates(ctx)
	if err != nil {
		b.logger.Error("schedule command: failed to check pause state", "error", err)
	}
	schedPaused := schedGate != nil
	capPaused := capGate != nil

	result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		b.logger.Error("schedule command: failed to list triggers", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to list schedules: "+err.Error(), false))
		return
	}

	projects, _ := b.daemon.ListProjectBeads(ctx)
	disabledProjects := make(map[string]bool)
	for name, pi := range projects {
		if !pi.SchedulesEnabled {
			disabledProjects[name] = true
		}
	}

	var header string
	if schedPaused {
		header += th.Semantic(SemPaused) + " *Scheduled triggers are PAUSED globally*\n"
	} else {
		header += th.Semantic(SemResumed) + " *Scheduled triggers are running*\n"
	}
	if capPaused {
		header += th.Semantic(SemPaused) + " *Gardeners are PAUSED globally*\n"
	} else {
		header += th.Semantic(SemResumed) + " *Gardeners are running*\n"
	}

	if projectFilter != "" {
		if pi, ok := projects[projectFilter]; ok && !pi.SchedulesEnabled {
			header += fmt.Sprintf(th.Semantic(SemDenied)+" Project *%s* schedules are *disabled*\n", projectFilter)
		}
	}

	// Count schedule-flavored triggers and gardeners (the beads this command
	// actually gates), optionally scoped to a project.
	schedEnabled, schedGated := 0, 0
	gardenerCount := 0
	var lines []string

	for _, bead := range result.Beads {
		source := triggerSourceFromBead(bead)
		isSchedule := scheduleSourceNames[source]
		isGardener := beadIsGardenerBead(bead)
		if !isSchedule && !isGardener {
			continue // event-driven trigger — managed by /trigger, not /schedule
		}

		project := bead.Fields["project"]
		if projectFilter != "" && !strings.EqualFold(project, projectFilter) {
			continue
		}

		if isGardener {
			gardenerCount++
		}

		enabled := bead.Fields["enabled"] != "false"
		projectDisabled := disabledProjects[project]
		// A bead is silenced if its own gate is engaged, its project is
		// disabled, or its individual enabled flag is off.
		gatedByGlobal := (isGardener && capPaused) || (isSchedule && schedPaused)
		silenced := gatedByGlobal || (isSchedule && projectDisabled) || !enabled

		icon := th.Semantic(SemResumed)
		if silenced {
			icon = th.Semantic(SemPaused)
			schedGated++
		} else {
			schedEnabled++
		}

		kind := "schedule"
		if isGardener {
			kind = "gardener"
		}
		line := fmt.Sprintf("%s %s *%s* · `%s`", icon, b.beadLink(bead.ID, fmt.Sprintf("`%s`", bead.ID)), bead.Title, kind)
		if project != "" && projectFilter == "" {
			line += fmt.Sprintf(" · _%s_", project)
		}
		if gatedByGlobal && enabled {
			line += " · " + th.Semantic(SemDenied) + " _globally paused_"
		} else if isSchedule && projectDisabled && enabled {
			line += " · " + th.Semantic(SemDenied) + " _project disabled_"
		}
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		if projectFilter != "" {
			header += fmt.Sprintf("_No schedules or gardeners found for project %q._\n", projectFilter)
		} else {
			header += "_No scheduled triggers or gardeners found._\n"
		}
		header += "Usage: `/schedule on` · `/schedule off` · `/schedule on <project>` · `/schedule off <project>`"
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(header, false))
		return
	}

	header += fmt.Sprintf("%d active, %d silenced (%d gardeners)\n", schedEnabled, schedGated, gardenerCount)
	header += "Usage: `/schedule on` · `/schedule off` · `/schedule on <project>` · `/schedule off <project>`\n"

	if projectFilter == "" && len(disabledProjects) > 0 {
		names := make([]string, 0, len(disabledProjects))
		for name := range disabledProjects {
			names = append(names, name)
		}
		sort.Strings(names)
		header += fmt.Sprintf(th.Semantic(SemDenied)+" Projects with schedules disabled: %s\n", strings.Join(names, ", "))
	}

	limit := min(len(lines), 20)
	body := strings.Join(lines[:limit], "\n")
	if len(lines) > limit {
		body += fmt.Sprintf("\n_...and %d more_", len(lines)-limit)
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(header+body, false))
}

// openGate creates a well-known gate bead with the given title. Used for the
// scheduler-pause and gardeners-pause kill-switch gates. Delegates to the shared
// pausegate.Open so the /plans master switch and this command open the exact
// same gate the same way (ka-0000004048).
func (b *Bot) openGate(ctx context.Context, title, createdBy string) error {
	_, err := pausegate.Open(ctx, b.daemon, title, createdBy)
	return err
}

// findScheduleGates looks for the open scheduler-pause and gardeners-pause gate
// beads. Either return may be nil (that gate is not engaged).
func (b *Bot) findScheduleGates(ctx context.Context) (schedGate, capGate *beadsapi.BeadDetail, err error) {
	schedGate, err = b.findGateByTitle(ctx, schedulerPauseTitleForTriggers)
	if err != nil {
		return nil, nil, err
	}
	capGate, err = b.findGateByTitle(ctx, gardenersPauseTitle)
	if err != nil {
		return nil, nil, err
	}
	return schedGate, capGate, nil
}

// findGateByTitle returns the first open gate bead whose title exactly matches
// the given well-known title, or nil if none exists. Delegates to the shared
// pausegate.Find (same query + exact-title match) so every surface that reads a
// pause gate agrees on what "engaged" means.
func (b *Bot) findGateByTitle(ctx context.Context, title string) (*beadsapi.BeadDetail, error) {
	return pausegate.Find(ctx, b.daemon, title)
}

// beadIsGardenerBead reports whether a trigger bead is a gardener: a role=gardener
// field, a role:gardener label, or a non-empty gardener blob field. Mirrors
// beadIsGardener in cmd/gb/gardener.go (kept local so the bridge package does not
// depend on the CLI package).
func beadIsGardenerBead(bead *beadsapi.BeadDetail) bool {
	if bead == nil {
		return false
	}
	if bead.Fields["role"] == "gardener" {
		return true
	}
	if slices.Contains(bead.Labels, "role:gardener") {
		return true
	}
	return strings.TrimSpace(bead.Fields["gardener"]) != ""
}
