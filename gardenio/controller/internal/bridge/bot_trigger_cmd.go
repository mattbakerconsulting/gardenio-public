package bridge

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// handleTriggerCommand processes the /trigger slash command — the emergency
// kill switch for trigger-bead evaluation. Mirrors /schedule.
//
//	/trigger off            — pause ALL triggers globally (creates a gate bead)
//	/trigger on             — resume ALL triggers globally (closes the gate bead)
//	/trigger                — show trigger status (global pause + per-project flags)
//	/trigger off <project>  — disable triggers for a specific project
//	/trigger on <project>   — enable triggers for a specific project
//	/trigger <project>      — show triggers for a specific project only
func (b *Bot) handleTriggerCommand(ctx context.Context, cmd slack.SlashCommand) {
	parts := strings.Fields(strings.TrimSpace(cmd.Text))

	if len(parts) == 0 {
		b.showTriggerStatus(ctx, cmd, "")
		return
	}

	action := strings.ToLower(parts[0])

	switch action {
	case "on":
		if len(parts) >= 2 {
			b.enableProjectTriggers(ctx, cmd, parts[1])
		} else {
			b.unpauseTriggers(ctx, cmd)
		}
	case "off":
		if len(parts) >= 2 {
			b.disableProjectTriggers(ctx, cmd, parts[1])
		} else {
			b.pauseTriggers(ctx, cmd)
		}
	default:
		b.showTriggerStatus(ctx, cmd, action)
	}
}

// pauseTriggers creates (or finds) a "triggers-pause" gate bead so the trigger
// engine skips ALL evaluation. Individual trigger beads are NOT modified — their
// enabled states are preserved.
func (b *Bot) pauseTriggers(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	gate, err := b.findTriggersPauseGate(ctx)
	if err != nil {
		b.logger.Error("trigger command: failed to check pause state", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to check trigger state: "+err.Error(), false))
		return
	}

	if gate != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemPaused)+" Triggers are already paused.", false))
		return
	}

	if _, err := b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     triggersPauseTitle,
		Type:      "gate",
		CreatedBy: cmd.UserID,
	}); err != nil {
		b.logger.Error("trigger command: failed to create pause gate", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to pause triggers: "+err.Error(), false))
		return
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(th.Semantic(SemPaused)+" Triggers paused. No triggers will fire until `/trigger on`.", false))

	b.logger.Info("trigger command: triggers paused globally", "user", cmd.UserID)
}

// unpauseTriggers closes the "triggers-pause" gate bead so the trigger engine
// resumes normal evaluation. Each trigger's individual enabled state is unchanged.
func (b *Bot) unpauseTriggers(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	gate, err := b.findTriggersPauseGate(ctx)
	if err != nil {
		b.logger.Error("trigger command: failed to check pause state", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to check trigger state: "+err.Error(), false))
		return
	}

	if gate == nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemResumed)+" Triggers are already running.", false))
		return
	}

	if err := b.daemon.CloseBead(ctx, gate.ID, nil); err != nil {
		b.logger.Error("trigger command: failed to close pause gate", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to unpause triggers: "+err.Error(), false))
		return
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(th.Semantic(SemResumed)+" Triggers resumed. They will fire on their next evaluation.", false))

	b.logger.Info("trigger command: triggers unpaused globally", "user", cmd.UserID)
}

// disableProjectTriggers sets triggers_enabled=false on the project bead and
// records an audit comment.
func (b *Bot) disableProjectTriggers(ctx context.Context, cmd slack.SlashCommand, projectName string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	projectID, err := b.findProjectBeadID(ctx, projectName)
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" "+err.Error(), false))
		return
	}

	// triggers_enabled is boolean in the type:project schema — a first write
	// as the string "false" is 400-rejected by field validation (ka-0000003627).
	if err := b.daemon.UpdateBeadFieldsTyped(ctx, projectID, map[string]any{
		"triggers_enabled": false,
	}); err != nil {
		b.logger.Error("trigger command: failed to disable project triggers",
			"project", projectName, "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to disable triggers for "+projectName+": "+err.Error(), false))
		return
	}

	if err := b.daemon.AddComment(ctx, projectID, cmd.UserID,
		fmt.Sprintf(th.Semantic(SemPaused)+" triggers disabled by <@%s> via /trigger off", cmd.UserID)); err != nil {
		b.logger.Warn("trigger command: failed to add audit comment",
			"project", projectName, "error", err)
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemPaused)+" Triggers disabled for project *%s*. Use `/trigger on %s` to re-enable.", projectName, projectName), false))

	b.logger.Info("trigger command: project triggers disabled",
		"project", projectName, "user", cmd.UserID)
}

// enableProjectTriggers sets triggers_enabled=true on the project bead and
// records an audit comment.
func (b *Bot) enableProjectTriggers(ctx context.Context, cmd slack.SlashCommand, projectName string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	projectID, err := b.findProjectBeadID(ctx, projectName)
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" "+err.Error(), false))
		return
	}

	if err := b.daemon.UpdateBeadFieldsTyped(ctx, projectID, map[string]any{
		"triggers_enabled": true,
	}); err != nil {
		b.logger.Error("trigger command: failed to enable project triggers",
			"project", projectName, "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to enable triggers for "+projectName+": "+err.Error(), false))
		return
	}

	if err := b.daemon.AddComment(ctx, projectID, cmd.UserID,
		fmt.Sprintf(th.Semantic(SemResumed)+" triggers re-enabled by <@%s> via /trigger on", cmd.UserID)); err != nil {
		b.logger.Warn("trigger command: failed to add audit comment",
			"project", projectName, "error", err)
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemResumed)+" Triggers enabled for project *%s*.", projectName), false))

	b.logger.Info("trigger command: project triggers enabled",
		"project", projectName, "user", cmd.UserID)
}

// showTriggerStatus shows the global pause state, any per-project disabled
// flags, and the count of trigger beads (optionally filtered by project).
func (b *Bot) showTriggerStatus(ctx context.Context, cmd slack.SlashCommand, projectFilter string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	gate, err := b.findTriggersPauseGate(ctx)
	if err != nil {
		b.logger.Error("trigger command: failed to check pause state", "error", err)
	}

	result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		b.logger.Error("trigger command: failed to list triggers", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to list triggers: "+err.Error(), false))
		return
	}

	projects, _ := b.daemon.ListProjectBeads(ctx)

	paused := gate != nil

	var header string
	if paused {
		header = th.Semantic(SemPaused) + " *Triggers are PAUSED globally*\n"
	} else {
		header = th.Semantic(SemResumed) + " *Triggers are running*\n"
	}

	if projectFilter != "" {
		if pi, ok := projects[projectFilter]; ok && !pi.TriggersEnabled {
			header += fmt.Sprintf(th.Semantic(SemDenied)+" Project *%s* triggers are *disabled*\n", projectFilter)
		}
	}

	disabledProjects := make(map[string]bool)
	for name, pi := range projects {
		if !pi.TriggersEnabled {
			disabledProjects[name] = true
		}
	}

	// Count triggers by project and source, plus enabled/disabled counts.
	byProject := make(map[string]int)
	bySource := make(map[string]int)
	enabledCount := 0
	disabledCount := 0
	var lines []string

	for _, bead := range result.Beads {
		project := bead.Fields["project"]
		if projectFilter != "" && !strings.EqualFold(project, projectFilter) {
			continue
		}
		byProject[project]++

		source := triggerSourceFromBead(bead)
		bySource[source]++

		enabled := bead.Fields["enabled"] != "false"
		projectDisabled := disabledProjects[project]

		icon := th.Semantic(SemPaused)
		if enabled && !projectDisabled && !paused {
			icon = th.Semantic(SemResumed)
			enabledCount++
		} else {
			disabledCount++
		}

		line := fmt.Sprintf("%s %s *%s*", icon, b.beadLink(bead.ID, fmt.Sprintf("`%s`", bead.ID)), bead.Title)
		if source != "" {
			line += fmt.Sprintf(" · `%s`", source)
		}
		if project != "" && projectFilter == "" {
			line += fmt.Sprintf(" · _%s_", project)
		}
		if (projectDisabled || paused) && enabled {
			reason := "_project disabled_"
			if paused {
				reason = "_globally paused_"
			}
			line += " · " + th.Semantic(SemDenied) + " " + reason
		}

		lines = append(lines, line)
	}

	if len(result.Beads) == 0 {
		header += "_No trigger beads found._\n"
		header += "Usage: `/trigger on` · `/trigger off` · `/trigger on <project>` · `/trigger off <project>`"
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(header, false))
		return
	}

	if projectFilter != "" && len(lines) == 0 {
		header += fmt.Sprintf("_No triggers found for project %q._\n", projectFilter)
		header += "Usage: `/trigger on <project>` · `/trigger off <project>`"
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(header, false))
		return
	}

	header += fmt.Sprintf("%d enabled, %d disabled\n", enabledCount, disabledCount)
	header += "Usage: `/trigger on` · `/trigger off` · `/trigger on <project>` · `/trigger off <project>`\n"

	if projectFilter == "" && len(disabledProjects) > 0 {
		names := make([]string, 0, len(disabledProjects))
		for name := range disabledProjects {
			names = append(names, name)
		}
		sort.Strings(names)
		header += fmt.Sprintf(th.Semantic(SemDenied)+" Projects with triggers disabled: %s\n", strings.Join(names, ", "))
	}

	if projectFilter == "" && len(byProject) > 0 {
		names := make([]string, 0, len(byProject))
		for n := range byProject {
			if n == "" {
				continue
			}
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) > 0 {
			parts := make([]string, 0, len(names))
			for _, n := range names {
				parts = append(parts, fmt.Sprintf("%s=%d", n, byProject[n]))
			}
			header += "Counts by project: " + strings.Join(parts, ", ") + "\n"
		}
	}

	limit := min(len(lines), 20)
	body := strings.Join(lines[:limit], "\n")
	if len(lines) > limit {
		body += fmt.Sprintf("\n_...and %d more_", len(lines)-limit)
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(header+body, false))
}

// findTriggersPauseGate looks for an open gate bead with the triggers-pause
// title. Returns nil if no such bead exists.
func (b *Bot) findTriggersPauseGate(ctx context.Context) (*beadsapi.BeadDetail, error) {
	result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"gate"},
		Statuses: []string{"open", "in_progress"},
		Search:   triggersPauseTitle,
		Limit:    5,
	})
	if err != nil {
		return nil, err
	}
	for _, bead := range result.Beads {
		if bead.Title == triggersPauseTitle {
			return bead, nil
		}
	}
	return nil, nil
}

// findProjectBeadID looks up a project by name and returns its bead ID.
func (b *Bot) findProjectBeadID(ctx context.Context, projectName string) (string, error) {
	result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"project"},
		Statuses: []string{"open", "in_progress"},
		Search:   projectName,
		Limit:    50,
	})
	if err != nil {
		return "", fmt.Errorf("failed to search for project %q: %w", projectName, err)
	}

	for _, bead := range result.Beads {
		name := strings.TrimPrefix(bead.Title, "Project: ")
		if strings.EqualFold(name, projectName) {
			return bead.ID, nil
		}
	}
	return "", fmt.Errorf("project %q not found", projectName)
}

// triggerSourceFromBead extracts the trigger source ("jira", "gitlab", etc.)
// from the trigger:* label on a trigger bead. Returns "" when no source label
// is present.
func triggerSourceFromBead(bead *beadsapi.BeadDetail) string {
	for _, lbl := range bead.Labels {
		if v, ok := strings.CutPrefix(lbl, "trigger:"); ok {
			return v
		}
	}
	return ""
}
