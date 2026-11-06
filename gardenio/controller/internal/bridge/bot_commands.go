package bridge

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// SlashCommandCapability is the capability string checked on user beads when
// the slash command permission gate is enabled.
const SlashCommandCapability = "slash_command"

// gatedSlashCommands lists slash commands that require the "slash_command"
// capability when the gate is enabled. All commands that mutate server-side
// state (spawn agents, change infrastructure, wipe mappings, flip scheduler)
// MUST be listed here — see AUDIT-2026-04-18.md §N1.
//
// /autodestruct is intentionally absent: it enforces its own allowlist via
// isAutoDestructAllowed. /roster and /unreleased are read-only.
var gatedSlashCommands = map[string]bool{
	"/spawn":         true,
	"/start":         true,
	"/kill":          true,
	"/kill-thread":   true,
	"/decisions":     true,
	"/decide":        true,
	"/formula":       true,
	"/bouncer":       true,
	"/clear-threads": true,
	"/trigger":       true,
	"/schedule":      true,
	"/runner":        true,
}

// handleSlashCommand processes Slack slash commands.
func (b *Bot) handleSlashCommand(ctx context.Context, cmd slack.SlashCommand) {
	// Stamp operator on every daemon call made downstream so server-side
	// human-attribution records this Slack user. Post-R2 (ka-0000005772) the
	// daemon fail-closes write paths when no operator can be resolved, so this
	// is required, not just preferred. See operatorFromSlackUser for the
	// slack→email resolution policy.
	if cmd.UserID != "" {
		ctx = beadsapi.WithOperator(ctx, b.operatorFromSlackUser(ctx, cmd.UserID))
		// Account linking (gd-OZJ6H2S3xx): a slash command runs as the linked
		// person (their own token is the bearer); an unlinked user gets the
		// link prompt and nothing runs.
		if b.slackLinker() != nil {
			actorCtx, refusal, ok := b.withSlackActor(ctx, cmd.UserID)
			if !ok {
				b.postSlackRefusal(ctx, cmd.ChannelID, cmd.UserID, cmd.TeamID, "", refusal)
				return
			}
			ctx = actorCtx
		}
	}
	// Permission gate: check if the user is allowed to use gated commands.
	if b.slashGateEnabled && gatedSlashCommands[cmd.Command] {
		if !b.checkSlashPermission(ctx, cmd) {
			return
		}
	}

	switch cmd.Command {
	case "/decisions", "/decide":
		b.handleDecisionsCommand(ctx, cmd)
	case "/roster":
		b.handleRosterCommand(ctx, cmd)
	case "/spawn":
		b.handleSpawnCommand(ctx, cmd)
	case "/start":
		b.handleStartCommand(ctx, cmd)
	case "/kill":
		b.handleKillCommand(ctx, cmd)
	case "/kill-thread":
		b.handleKillThreadCommand(ctx, cmd)
	case "/unreleased":
		b.handleUnreleasedCommand(ctx, cmd)
	case "/clear-threads":
		b.handleClearThreadsCommand(ctx, cmd)
	case "/bouncer":
		b.handleBouncerCommand(ctx, cmd)
	case "/autodestruct":
		b.handleAutoDestructCommand(ctx, cmd)
	case "/formula":
		b.handleFormulaCommand(ctx, cmd)
	case "/trigger":
		b.handleTriggerCommand(ctx, cmd)
	case "/schedule":
		b.handleScheduleCommand(ctx, cmd)
	case "/runner":
		b.handleRunnerCommand(ctx, cmd)
	default:
		b.logger.Debug("unhandled slash command", "command", cmd.Command)
	}
}

// checkSlashPermission verifies that the Slack user is authorized to use
// a gated slash command. Returns true if allowed.
// When denied, an ephemeral Block Kit message is posted and false is returned.
func (b *Bot) checkSlashPermission(ctx context.Context, cmd slack.SlashCommand) bool {
	th := b.themeFor(ctx, cmd.ChannelID)
	user := b.UserDirectory().LookupBySlackID(ctx, cmd.UserID)
	if user == nil {
		b.logSlashAudit("denied", "unknown_user", cmd.UserID, "", "", "", cmd.ChannelID, cmd.Command)
		b.postSlashDenied(cmd, th.Semantic(SemDenied)+" Access Required",
			"Your account is not registered with this bot. An administrator needs to create a user profile for you before you can use slash commands.",
			"Ask a workspace admin to register your Slack account.")
		return false
	}

	if !user.HasCapability(SlashCommandCapability) {
		b.logSlashAudit("denied", "missing_capability", cmd.UserID, user.BeadID, user.DisplayName, user.Team, cmd.ChannelID, cmd.Command)
		greeting := "your"
		if user.DisplayName != "" {
			greeting = user.DisplayName + "'s"
		}
		b.postSlashDenied(cmd, th.Semantic(SemDenied)+" Permission Denied",
			fmt.Sprintf("I found %s profile, but it doesn't include permission to use slash commands. The `%s` capability is required.", greeting, SlashCommandCapability),
			"Ask a workspace admin to add the slash_command capability to your user profile.")
		return false
	}

	b.logSlashAudit("allowed", "", cmd.UserID, user.BeadID, user.DisplayName, user.Team, cmd.ChannelID, cmd.Command)
	return true
}

// logSlashAudit emits a structured log entry for a slash command permission check.
func (b *Bot) logSlashAudit(outcome, reason, slackUserID, beadID, displayName, team, channel, command string) {
	args := []any{
		"outcome", outcome,
		"slack_user_id", slackUserID,
		"channel", channel,
		"command", command,
	}
	if reason != "" {
		args = append(args, "reason", reason)
	}
	if beadID != "" {
		args = append(args, "user_bead", beadID)
	}
	if displayName != "" {
		args = append(args, "display_name", displayName)
	}
	if team != "" {
		args = append(args, "team", team)
	}
	b.logger.Info("slash_audit: "+outcome, args...)
}

// postSlashDenied sends a Block Kit ephemeral message to deny a slash command.
func (b *Bot) postSlashDenied(cmd slack.SlashCommand, header, body, help string) {
	if b.api == nil {
		return
	}
	blocks := b.mentionDeniedBlocks(header, body, help)
	fallback := header + " — " + body
	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(blocks...),
	)
}

// handleSpawnCommand processes the /spawn slash command.
// Usage: /spawn [task|ticket] [--role <role>] [--project <project>]
//
// The new /spawn does NOT require an agent name — it auto-generates one.
// Project is inferred from the channel's default project (via project beads'
// slack_channel field), unless --project is specified to override.
func (b *Bot) handleSpawnCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	args := splitQuotedArgs(strings.TrimSpace(cmd.Text))
	positional, role, projectFlag, runnerFlag, ultracode, ultracodeBad := extractSpawnFlags(args)
	b.warnUltracodeBad(ctx, cmd, ultracodeBad)

	// --runner <hostname> places the task on that local runner instead of a
	// pod (spec §7). Resolved before anything is created so a refusal leaves
	// no stray task behind.
	var placed Placement
	if runnerFlag != "" {
		var ok bool
		if placed, ok = b.spawnPlacement(ctx, cmd, runnerFlag); !ok {
			return
		}
		if placed.RunnerID != "" && len(positional) == 0 {
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(th.Semantic(SemFail)+fmt.Sprintf(" Name a task or describe one to place it on %s.", runnerFlag), false))
			return
		}
	}

	// Resolve project: explicit --project flag overrides channel inference.
	project := projectFlag
	if project == "" {
		project = b.projectFromChannel(ctx, cmd.ChannelID)
	}
	// DM fallback: auto-select sole project or use dm_default when in a DM channel
	// (DM channel IDs start with "D" and are never in project channel mappings).
	if project == "" && strings.HasPrefix(cmd.ChannelID, "D") {
		project = b.resolveDMProject(ctx, cmd.ChannelID, cmd.UserID, "spawn")
	}

	taskID := ""

	// Detect task description:
	//   /spawn "fix the helm chart"             → positional[0] has spaces (quoted)
	//   /spawn gardenio "fix the helm chart"      → positional[1] has spaces
	//   /spawn fix the helm chart                → multiple unquoted words (no ticket ref)
	taskDescription := ""
	if len(positional) > 0 && strings.Contains(positional[0], " ") {
		taskDescription = positional[0]
	} else if len(positional) >= 2 && strings.Contains(positional[1], " ") {
		// /spawn <project> "task description"
		project = positional[0]
		taskDescription = positional[1]
	} else if len(positional) > 1 && !isTicketRef(positional[0]) && !isValidAgentName(positional[0]) {
		// Multiple unquoted words that aren't a ticket or agent name — treat as description.
		taskDescription = strings.Join(positional, " ")
	}

	if taskDescription != "" {
		// Reject task-first spawns in regular channels with no project.
		if project == "" && !strings.HasPrefix(cmd.ChannelID, "D") {
			msg := th.Semantic(SemFail) + " Cannot spawn agent — no project is mapped to this channel. Use `--project <name>` to specify one."
			if projects, err := b.daemon.ListProjectBeads(ctx); err == nil && len(projects) > 0 {
				names := make([]string, 0, len(projects))
				for name := range projects {
					names = append(names, name)
				}
				sort.Strings(names)
				msg += fmt.Sprintf("\nAvailable projects: %s", strings.Join(names, ", "))
			}
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(msg, false))
			return
		}

		// Task-first mode: create a task bead, auto-generate name from description.
		// Use LLM to derive a meaningful slug and short title when available.
		agentName, llmTitle := b.generateAgentNameLLM(ctx, project, taskDescription)

		// Validate project exists.
		if project != "" {
			if !b.validateProject(ctx, cmd, project) {
				return
			}
		}

		// Create a task bead for the description. Use LLM-generated title
		// if available, otherwise truncate the description to fit the
		// 500-char bead title limit. Full text goes in description.
		taskTitle := llmTitle
		if taskTitle == "" {
			taskTitle = truncateTitle(taskDescription, 500)
		}
		var taskDesc string
		if taskDescription != taskTitle {
			taskDesc = taskDescription
		}
		var labels []string
		if project != "" {
			labels = []string{"project:" + project}
		}
		var err error
		taskID, err = b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:       taskTitle,
			Description: taskDesc,
			Type:        "task",
			Kind:        "issue",
			Labels:      labels,
			Priority:    2,
		})
		if err != nil {
			b.logger.Error("failed to create task bead", "error", err)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Failed to create task: %s", err.Error()), false))
			return
		}

		// Apply channel role override if no explicit --role was given.
		if role == "" && project != "" {
			role = b.roleFromChannel(ctx, cmd.ChannelID, project)
		}

		if placed.RunnerID != "" {
			b.placeSpawnOnRunner(ctx, cmd, placed, runnerFlag, taskID, taskDescription)
			return
		}
		b.spawnAndRespond(ctx, cmd, agentName, project, taskID, role, taskDescription, ultracode)
		return
	}

	// If a positional arg is a ticket/bead reference, resolve it.
	if len(positional) > 0 && isTicketRef(positional[0]) {
		bead, err := b.daemon.ResolveTicket(ctx, positional[0])
		if err != nil {
			b.logger.Error("failed to resolve ticket", "ticket", positional[0], "error", err)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Could not resolve ticket %q: %s", positional[0], err.Error()), false))
			return
		}
		taskID = bead.ID
		// Infer project from ticket if channel didn't provide one.
		if project == "" {
			project = projectFromLabels(bead.Labels)
			if project == "" {
				project = b.projectFromTicketPrefix(ctx, positional[0])
			}
		}
	}

	// Auto-generate agent name.
	agentName := generateSpawnName(project)
	if taskID != "" {
		// If we have a task, try to generate a better name from the task title.
		taskBead, err := b.daemon.GetBead(ctx, taskID)
		if err == nil && taskBead.Title != "" {
			agentName = generateAgentName(project, taskBead.Title)
		}
	}

	// Reject spawns in regular channels when no project was resolved from
	// any source (channel mapping, ticket labels, or --project flag).
	// Without a project the agent gets no config, secrets, or repos and
	// its card lands in the default bot channel — confusing for the user.
	if project == "" && !strings.HasPrefix(cmd.ChannelID, "D") {
		msg := th.Semantic(SemFail) + " Cannot spawn agent — no project is mapped to this channel. Use `--project <name>` to specify one."
		if projects, err := b.daemon.ListProjectBeads(ctx); err == nil && len(projects) > 0 {
			names := make([]string, 0, len(projects))
			for name := range projects {
				names = append(names, name)
			}
			sort.Strings(names)
			msg += fmt.Sprintf("\nAvailable projects: %s", strings.Join(names, ", "))
		}
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(msg, false))
		return
	}

	// Validate project exists.
	if project != "" {
		if !b.validateProject(ctx, cmd, project) {
			return
		}
	}

	// Apply channel role override if no explicit --role was given.
	if role == "" && project != "" {
		role = b.roleFromChannel(ctx, cmd.ChannelID, project)
	}

	if placed.RunnerID != "" {
		if taskID == "" {
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(th.Semantic(SemFail)+fmt.Sprintf(" Name a task or describe one to place it on %s.", runnerFlag), false))
			return
		}
		b.placeSpawnOnRunner(ctx, cmd, placed, runnerFlag, taskID, "")
		return
	}
	b.spawnAndRespond(ctx, cmd, agentName, project, taskID, role, "", ultracode)
}

// handleTaskFirstSpawn handles /start "task description" [project] [--role <role>]
// and the /spawn "task description" flow. It auto-generates an agent name from
// the task description, creates a task bead, and spawns an agent assigned to
// that task with the description as the initial prompt.
func (b *Bot) handleTaskFirstSpawn(ctx context.Context, cmd slack.SlashCommand, positional []string, role, ultracode string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	taskDescription := positional[0]

	project := ""
	if len(positional) >= 2 {
		project = positional[1]
	}
	// Fall back to channel-based project inference.
	if project == "" {
		project = b.projectFromChannel(ctx, cmd.ChannelID)
	}
	// DM fallback: auto-select sole project or use dm_default when in a DM channel.
	if project == "" && strings.HasPrefix(cmd.ChannelID, "D") {
		project = b.resolveDMProject(ctx, cmd.ChannelID, cmd.UserID, "start task-first")
	}

	// Reject spawns in regular channels with no project mapping.
	if project == "" && !strings.HasPrefix(cmd.ChannelID, "D") {
		msg := th.Semantic(SemFail) + " Cannot spawn agent — no project is mapped to this channel. Use `--project <name>` to specify one."
		if projects, err := b.daemon.ListProjectBeads(ctx); err == nil && len(projects) > 0 {
			names := make([]string, 0, len(projects))
			for name := range projects {
				names = append(names, name)
			}
			sort.Strings(names)
			msg += fmt.Sprintf("\nAvailable projects: %s", strings.Join(names, ", "))
		}
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(msg, false))
		return
	}

	// Validate project exists.
	if project != "" {
		if !b.validateProject(ctx, cmd, project) {
			return
		}
	}

	agentName, llmTitle := b.generateAgentNameLLM(ctx, project, taskDescription)

	// Create a task bead for the description. Use LLM-generated title
	// if available, otherwise truncate to fit 500-char bead title limit.
	taskTitle := llmTitle
	if taskTitle == "" {
		taskTitle = truncateTitle(taskDescription, 500)
	}
	var taskDesc string
	if taskDescription != taskTitle {
		taskDesc = taskDescription
	}
	var labels []string
	if project != "" {
		labels = []string{"project:" + project}
	}
	taskID, err := b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       taskTitle,
		Description: taskDesc,
		Type:        "task",
		Kind:        "issue",
		Labels:      labels,
		Priority:    2,
	})
	if err != nil {
		b.logger.Error("failed to create task bead", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Failed to create task: %s", err.Error()), false))
		return
	}

	// Apply channel role override if no explicit --role was given.
	if role == "" && project != "" {
		role = b.roleFromChannel(ctx, cmd.ChannelID, project)
	}

	b.spawnAndRespond(ctx, cmd, agentName, project, taskID, role, taskDescription, ultracode)
}

// handleStartCommand processes the /start slash command.
// Usage: /start <agent> [project|ticket|"PROMPT TEXT"] [task] [--role <role>] [--project <project>]
//
//	/start "task description" [project] [--role <role>] [--project <project>]
//
// This is the original /spawn behavior, preserved for power users who want to
// specify an explicit agent name.
func (b *Bot) handleStartCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	args := splitQuotedArgs(strings.TrimSpace(cmd.Text))
	if len(args) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Usage: `/start <agent> [project|ticket|\"PROMPT TEXT\"] [task] [--role <role>] [--project <project>]`\nor: `/start \"task description\" [project] [--role <role>]`", false))
		return
	}

	positional, role, projectFlag, runnerFlag, ultracode, ultracodeBad := extractSpawnFlags(args)
	if runnerFlag != "" {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" `--runner` works with `/spawn`, not `/start`.", false))
		return
	}
	b.warnUltracodeBad(ctx, cmd, ultracodeBad)

	// Flags-only input (e.g. `/start --role crew`) passes the len(args)==0 guard
	// above but leaves positional empty, so the positional[0] accesses below (and
	// in handleTaskFirstSpawn) would panic. Guard it like handleKillCommand /
	// handleSpawnCommand do.
	if len(positional) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Usage: `/start <agent> [project|ticket|\"PROMPT TEXT\"] [task] [--role <role>] [--project <project>]`\nor: `/start \"task description\" [project] [--role <role>]`", false))
		return
	}

	// Task-first mode: if the first positional arg contains spaces, it was a
	// quoted task description rather than an agent name.
	if strings.Contains(positional[0], " ") {
		b.handleTaskFirstSpawn(ctx, cmd, positional, role, ultracode)
		return
	}

	agentName := positional[0]
	if !isValidAgentName(agentName) {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Invalid agent name %q — use lowercase letters, digits, and hyphens only", agentName), false))
		return
	}

	project := ""
	customPrompt := ""
	taskID := ""

	if len(positional) >= 2 {
		arg2 := positional[1]
		// Quoted strings are custom prompts, not project names.
		if strings.Contains(arg2, " ") {
			customPrompt = arg2
		} else if isTicketRef(arg2) {
			// Ticket reference (e.g., "PE-1234", "ka-abc123") — resolve to
			// bead ID and infer project from the ticket.
			bead, err := b.daemon.ResolveTicket(ctx, arg2)
			if err != nil {
				b.logger.Error("failed to resolve ticket", "ticket", arg2, "error", err)
				_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
					slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Could not resolve ticket %q: %s", arg2, err.Error()), false))
				return
			}
			taskID = bead.ID
			// Infer project from the bead's project label.
			project = projectFromLabels(bead.Labels)
			// If no project label on the bead, try the prefix → project mapping.
			if project == "" {
				project = b.projectFromTicketPrefix(ctx, arg2)
			}
		} else {
			project = arg2
		}
	}

	if len(positional) >= 3 && taskID == "" {
		taskID = positional[2]
	}

	// Explicit --project flag overrides any inferred project.
	if projectFlag != "" {
		project = projectFlag
	}

	// Fall back to channel-based project inference.
	if project == "" {
		project = b.projectFromChannel(ctx, cmd.ChannelID)
	}
	// DM fallback: auto-select sole project or use dm_default when in a DM channel
	// (DM channel IDs start with "D" and are never in project channel mappings).
	if project == "" && strings.HasPrefix(cmd.ChannelID, "D") {
		project = b.resolveDMProject(ctx, cmd.ChannelID, cmd.UserID, "start")
	}

	// Validate project exists.
	if project != "" {
		if !b.validateProject(ctx, cmd, project) {
			return
		}
	}

	// Apply channel role override if no explicit --role was given.
	if role == "" && project != "" {
		role = b.roleFromChannel(ctx, cmd.ChannelID, project)
	}

	b.spawnAndRespond(ctx, cmd, agentName, project, taskID, role, customPrompt, ultracode)
}

// spawnAndRespond creates an agent bead and sends a confirmation ephemeral message.
// Extracted from the old handleSpawnCommand to share between /spawn and /start.
func (b *Bot) spawnAndRespond(ctx context.Context, cmd slack.SlashCommand, agentName, project, taskID, role, customPrompt, ultracode string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	if project == "" {
		b.logger.Warn("spawn rejected — no project resolved",
			"agent", agentName, "channel", cmd.ChannelID, "user", cmd.UserID)
		msg := th.Semantic(SemFail) + " Cannot spawn agent — no project is mapped to this channel. Use `--project <name>` to specify one."
		if projects, err := b.daemon.ListProjectBeads(ctx); err == nil && len(projects) > 0 {
			names := make([]string, 0, len(projects))
			for name := range projects {
				names = append(names, name)
			}
			msg += fmt.Sprintf("\nAvailable projects: %s", strings.Join(names, ", "))
		}
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(msg, false))
		return
	}

	// Push operator + Slack identity at create-time so the agent bead carries
	// operated_by and slack_user_id from the first row, instead of being
	// patched after-the-fact (R1 #3 fold). This also lets the resolver's
	// agent-bead fallback attribute the agent's own writes to this user.
	// Operator was already stamped onto ctx at the top of this handler via
	// operatorFromSlackUser; mirror that value into spawnFields so the bead's
	// persisted operated_by matches the audited write.
	spawnFields := map[string]string{}
	if op := beadsapi.OperatorFromContext(ctx); op != "" {
		spawnFields["operated_by"] = op
	}
	if cmd.UserID != "" {
		spawnFields["slack_user_id"] = cmd.UserID
	}
	if cmd.ChannelID != "" {
		spawnFields["slack_spawn_channel"] = cmd.ChannelID
	}
	// Per-spawn --ultracode override: stamp the canonical "on"/"off" so the pod
	// receives BOAT_ULTRACODE and gb setup claude forces the ultracode knob.
	// SpawnAgent merges extraFields into the agent bead's fields.
	if ultracode != "" {
		spawnFields["ultracode"] = ultracode
	}
	beadID, err := b.daemon.SpawnAgent(ctx, agentName, project, taskID, role, customPrompt, spawnFields)
	if err != nil {
		b.logger.Error("failed to spawn agent", "agent", agentName, "project", project, "task", taskID, "role", role, "prompt", customPrompt, "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Failed to spawn agent %q: %s", agentName, err.Error()), false))
		return
	}

	b.logger.Info("spawned agent via Slack", "agent", agentName, "project", project, "task", taskID, "role", role, "prompt", customPrompt, "bead", beadID, "user", cmd.UserID)

	// Operator-activity backbone counter (ka-0000006613): record this Slack
	// spawn as activity_type=spawn, surface=slack. The operator was stamped
	// onto ctx at the top of the command handler via operatorFromSlackUser;
	// an unattributable operator collapses to "0".
	b.recordOperatorActivity(beadsapi.OperatorFromContext(ctx), project, activityTypeSpawn, surfaceSlack)

	text := fmt.Sprintf(th.Semantic(SemSpawn)+" Spawning agent *%s*", agentName)
	if project != "" {
		text += fmt.Sprintf(" in project *%s*", project)
	}
	if role != "" {
		text += fmt.Sprintf(" with role *%s*", role)
	}
	if taskID != "" {
		text += fmt.Sprintf(" assigned to task `%s`", taskID)
	}
	if customPrompt != "" {
		promptPreview := customPrompt
		if len(promptPreview) > 60 {
			promptPreview = promptPreview[:57] + "..."
		}
		text += fmt.Sprintf("\nPrompt: _%s_", promptPreview)
	}
	text += fmt.Sprintf("\nBead: %s · Use `/roster` to check status.", b.beadLink(beadID, fmt.Sprintf("`%s`", beadID)))
	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(text, false))
}

// validateProject checks whether a project name is known and sends an ephemeral
// error if not. Returns true if the project is valid (or validation failed
// silently), false if an error was sent to the user.
func (b *Bot) validateProject(ctx context.Context, cmd slack.SlashCommand, project string) bool {
	th := b.themeFor(ctx, cmd.ChannelID)
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		b.logger.Error("failed to list projects for validation", "error", err)
		return true // allow through on error
	}
	if _, ok := projects[project]; !ok {
		names := make([]string, 0, len(projects))
		for name := range projects {
			names = append(names, name)
		}
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Unknown project %q — available: %s", project, strings.Join(names, ", ")), false))
		return false
	}
	return true
}

// generateAgentName creates a valid agent name from a task description by
// slugifying the first 3 words and appending a random suffix.
// When project is non-empty it is prepended for disambiguation across projects.
// Examples:
//
//	generateAgentName("gardenio", "fix the login bug") → "gardenio-fix-the-login-a7k"
//	generateAgentName("", "fix the login bug")        → "fix-the-login-a7k"
func generateAgentName(project, description string) string {
	words := strings.Fields(strings.ToLower(description))
	var slugWords []string
	for _, w := range words {
		var clean strings.Builder
		for _, c := range w {
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
				clean.WriteRune(c)
			}
		}
		if clean.Len() > 0 {
			slugWords = append(slugWords, clean.String())
		}
		if len(slugWords) == 3 {
			break
		}
	}
	if len(slugWords) == 0 {
		slugWords = []string{"agent"}
	}
	slug := strings.Join(slugWords, "-") + "-" + randomSuffix(3)
	if project != "" {
		return project + "-" + slug
	}
	return slug
}

// generateAgentNameLLM tries the LLM name generator first, falling back to
// the mechanical generateAgentName. Returns (agentName, beadTitle) where
// beadTitle is a short LLM-generated title or empty if LLM was not used.
func (b *Bot) generateAgentNameLLM(ctx context.Context, project, description string) (agentName string, beadTitle string) {
	if b.nameGen != nil {
		slug, title, err := b.nameGen.GenerateName(ctx, description)
		if err == nil && slug != "" {
			slug = slug + "-" + randomSuffix(3)
			if project != "" {
				agentName = project + "-" + slug
			} else {
				agentName = slug
			}
			return agentName, title
		}
		b.logger.Warn("LLM name generation failed, falling back to mechanical",
			"error", err)
	}
	return generateAgentName(project, description), ""
}

// truncateTitle returns a title that fits within kbeads' 500-char limit.
// If the text is short enough, it's returned as-is. Otherwise, it is
// truncated at a word boundary with an ellipsis appended.
func truncateTitle(text string, maxLen int) string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	// Truncate and find a word boundary.
	truncated := string(runes[:maxLen-3])
	if idx := strings.LastIndexByte(truncated, ' '); idx > maxLen/2 {
		truncated = truncated[:idx]
	}
	return truncated + "..."
}

// randomSuffix returns a random string of n lowercase alphanumeric characters.
func randomSuffix(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

// extractSpawnFlags extracts --role, --project, --runner and --ultracode flags
// from a split arg list, returning the remaining positional args and the
// extracted values. Supports both "--flag value" and "--flag=value" syntax for
// role/project/runner; --ultracode takes "--ultracode", "--ultracode=on", or
// "--ultracode=off" and yields a canonical "on"/"off"/"" plus an
// ultracodeBad token for any unrecognized value (so the caller can warn).
func extractSpawnFlags(args []string) (positional []string, role, project, runner, ultracode, ultracodeBad string) {
	positional = args[:0]
	for i := 0; i < len(args); i++ {
		if args[i] == "--role" && i+1 < len(args) {
			role = args[i+1]
			i++ // skip value
		} else if v, ok := strings.CutPrefix(args[i], "--role="); ok {
			role = v
		} else if args[i] == "--project" && i+1 < len(args) {
			project = args[i+1]
			i++ // skip value
		} else if v, ok := strings.CutPrefix(args[i], "--project="); ok {
			project = v
		} else if args[i] == "--runner" && i+1 < len(args) {
			runner = args[i+1]
			i++ // skip value
		} else if v, ok := strings.CutPrefix(args[i], "--runner="); ok {
			runner = v
		} else if args[i] == "--ultracode" {
			ultracode = "on" // bare flag == on
		} else if v, ok := strings.CutPrefix(args[i], "--ultracode="); ok {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "on", "true", "1":
				ultracode = "on"
			case "off", "false", "0":
				ultracode = "off"
			default:
				ultracodeBad = v
			}
		} else {
			positional = append(positional, args[i])
		}
	}
	return
}

// warnUltracodeBad posts an ephemeral warning when a slash-command --ultracode
// value could not be parsed. The spawn still proceeds on the default ultracode
// setting; this only nudges the operator toward the on|off vocabulary.
func (b *Bot) warnUltracodeBad(ctx context.Context, cmd slack.SlashCommand, bad string) {
	if bad == "" || b.api == nil {
		return
	}
	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(
			b.themeFor(ctx, cmd.ChannelID).Semantic(SemWarn)+" Unrecognized `--ultracode=%s` — use `--ultracode=on` or `--ultracode=off`. Spawning with the default ultracode setting.",
			bad), false))
}

// splitQuotedArgs splits a command string into arguments, respecting double-quoted
// strings as single arguments. Quotes are stripped from the resulting tokens.
// Example: `my-bot "fix the login bug" --role crew` → ["my-bot", "fix the login bug", "--role", "crew"]
func splitQuotedArgs(s string) []string {
	// Normalize smart quotes (Slack converts " to \u201c/\u201d).
	s = strings.NewReplacer("\u201c", "\"", "\u201d", "\"", "\u2018", "'", "\u2019", "'").Replace(s)

	var args []string
	var current strings.Builder
	inQuotes := false
	for _, ch := range s {
		switch {
		case ch == '"':
			inQuotes = !inQuotes
		case ch == ' ' && !inQuotes:
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// isValidAgentName reports whether s is a valid agent name.
// Valid names are non-empty and contain only lowercase letters, digits, and hyphens.
func isValidAgentName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// ticketRefRe matches external ticket references like "PE-1234" or "DEVOPS-42".
var ticketRefRe = regexp.MustCompile(`^[A-Za-z]+-\d+$`)

// isTicketRef reports whether s looks like a ticket reference.
// Matches JIRA-style keys (PE-1234, DEVOPS-42) and internal bead IDs (ka-xxx).
func isTicketRef(s string) bool {
	return (strings.HasPrefix(s, "gd-") || strings.HasPrefix(s, "ka-")) || ticketRefRe.MatchString(s)
}

// projectFromLabels extracts the project name from a bead's labels.
// Returns "" if no project label is found.
func projectFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			return v
		}
	}
	return ""
}

// generateSpawnName creates a valid agent name for /spawn when no task is provided.
// Uses the project name as a prefix with a random suffix for uniqueness.
// Examples: "gardenio-k7x2", "monorepo-m3nq", "agent-a1b2" (no project).
func generateSpawnName(project string) string {
	prefix := project
	if prefix == "" {
		prefix = "agent"
	}
	return prefix + "-" + randomSuffix(4)
}

// projectFromChannel resolves a Slack channel ID to a project name by checking
// the slack_channel field on project beads. Supports multiple comma-separated
// channels per project. Returns "" if no project matches or if multiple
// projects claim the same channel (ambiguous — logged as error).
func (b *Bot) projectFromChannel(ctx context.Context, channelID string) string {
	project, _ := b.projectForChannel(ctx, channelID)
	return project
}

// channelProjectMiss says why projectForChannel found no project for a
// channel: the daemon could not list the projects, or more than one project
// claims the channel. The zero value is a plain miss: no project maps it.
type channelProjectMiss struct {
	conflicts []string
	err       error
}

// reason is the clause a person is told when a spawn finds no project because
// of the miss, or "" for a plain miss, which the caller words itself.
func (m channelProjectMiss) reason() string {
	switch {
	case m.err != nil:
		return "I couldn't read the project list from the beads daemon just now. Try again in a moment."
	case len(m.conflicts) > 0:
		return fmt.Sprintf("this channel is claimed by more than one project (%s). Use `--project <name>` to pick one.",
			strings.Join(m.conflicts, ", "))
	}
	return ""
}

// projectForChannel is projectFromChannel with the reason for an empty
// project: a daemon error, or the projects that all claim the channel.
func (b *Bot) projectForChannel(ctx context.Context, channelID string) (string, channelProjectMiss) {
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		b.logger.Error("failed to list projects for channel lookup", "error", err)
		return "", channelProjectMiss{err: err}
	}
	project, conflicts := beadsapi.ProjectForChannel(projects, channelID)
	if len(conflicts) > 0 {
		b.logger.Error("multiple projects claim the same Slack channel — cannot resolve",
			"channel", channelID, "projects", conflicts)
		return "", channelProjectMiss{conflicts: conflicts}
	}
	if project != "" {
		b.logger.Debug("projectFromChannel: matched",
			"channel", channelID, "project", project)
		return project, channelProjectMiss{}
	}
	// Log all projects and their channels for debugging.
	projectChannels := make(map[string][]string, len(projects))
	for name, info := range projects {
		projectChannels[name] = info.SlackChannels
	}
	b.logger.Info("no project matched channel",
		"channel", channelID, "project_count", len(projects),
		"project_channels", projectChannels)
	return "", channelProjectMiss{}
}

// resolveDMProject resolves the project for a DM-spawned agent. Priority:
// 1. Auto-select if exactly one project exists.
// 2. The user's own dm_project preference (user bead, by slack_user_id).
// 3. Env var default (defaultDMProject).
// 4. Project bead with dm_default=true.
// 5. User's most recent agent bead project (by slack_user_id).
// Returns "" if no project can be resolved.
func (b *Bot) resolveDMProject(ctx context.Context, channelID, userID, caller string) string {
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		b.logger.Error("resolveDMProject: failed to list projects", "error", err)
		return ""
	}
	if len(projects) == 1 {
		for name := range projects {
			b.logger.Info(caller+": auto-selected sole project for DM",
				"project", name, "channel", channelID)
			return name
		}
	}
	if len(projects) > 1 {
		// Priority 0: the user's own dm_project preference. A per-user choice
		// outranks the fleet-wide env/bead defaults (dm_project.go).
		if pref := b.dmProjectForSlackUser(ctx, userID); pref != "" {
			if _, ok := projects[pref]; ok {
				b.logger.Info(caller+": using user's dm_project preference",
					"project", pref, "user", userID, "channel", channelID)
				return pref
			}
			b.logger.Warn(caller+": user's dm_project preference names an unknown project, ignoring",
				"dm_project", pref, "user", userID, "channel", channelID)
		}
		// Priority 1: env var default.
		if b.defaultDMProject != "" {
			if _, ok := projects[b.defaultDMProject]; ok {
				b.logger.Info(caller+": using default DM project (env)",
					"project", b.defaultDMProject, "channel", channelID)
				return b.defaultDMProject
			}
			b.logger.Warn(caller+": configured default DM project not found",
				"default_dm_project", b.defaultDMProject, "channel", channelID)
		}
		// Priority 2: project bead with dm_default=true.
		for name, info := range projects {
			if info.DMDefault {
				b.logger.Info(caller+": using default DM project (bead field)",
					"project", name, "channel", channelID)
				return name
			}
		}
		// Priority 3: user's most recent agent bead project.
		if userID != "" {
			if proj := b.projectFromUserHistory(ctx, userID, projects); proj != "" {
				b.logger.Info(caller+": using project from user history",
					"project", proj, "user", userID, "channel", channelID)
				return proj
			}
		}
	}
	return ""
}

// projectFromUserHistory looks up the most recent agent bead created by the
// given Slack user and returns its project, provided that project still exists.
func (b *Bot) projectFromUserHistory(ctx context.Context, userID string, projects map[string]beadsapi.ProjectInfo) string {
	result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"},
		Sort:  "-created_at",
		Limit: 50,
	})
	if err != nil {
		b.logger.Debug("projectFromUserHistory: failed to list agent beads", "error", err)
		return ""
	}
	for _, bead := range result.Beads {
		if bead.Fields["slack_user_id"] == userID {
			proj := bead.Fields["project"]
			if proj == "" {
				continue
			}
			if _, ok := projects[proj]; ok {
				return proj
			}
		}
	}
	return ""
}

// roleFromChannel looks up the channel_roles override for the given channel
// within the named project. Returns "" if no override is configured.
func (b *Bot) roleFromChannel(ctx context.Context, channelID, project string) string {
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		return ""
	}
	if info, ok := projects[project]; ok {
		return info.ChannelRole(channelID)
	}
	return ""
}

// runtimeClassFromChannel looks up the channel_runtime_class override for the
// given channel within the named project. Returns "" if none — the agent then
// inherits the role/project/cluster default RuntimeClass. A non-empty value
// (e.g. "runsc") pins this channel's thread agents onto that RuntimeClass,
// opting them into gVisor freeze/thaw fast hot resume (epic ka-0000003080).
func (b *Bot) runtimeClassFromChannel(ctx context.Context, channelID, project string) string {
	if project == "" {
		return ""
	}
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		return ""
	}
	if info, ok := projects[project]; ok {
		return info.ChannelRuntimeClass(channelID)
	}
	return ""
}

// projectFromTicketPrefix looks up the project name for a ticket's prefix
// (e.g., "PE" from "PE-1234") by checking registered project beads' Prefix fields.
func (b *Bot) projectFromTicketPrefix(ctx context.Context, ticket string) string {
	parts := strings.SplitN(ticket, "-", 2)
	if len(parts) < 2 {
		return ""
	}
	prefix := strings.ToLower(parts[0])

	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		b.logger.Error("failed to list projects for prefix lookup", "error", err)
		return ""
	}
	for name, info := range projects {
		if strings.EqualFold(info.Prefix, prefix) {
			return name
		}
	}
	return ""
}

// handleDecisionsCommand shows pending decisions as an ephemeral message.
func (b *Bot) handleDecisionsCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	decisions, err := b.daemon.ListDecisionBeads(ctx)
	if err != nil {
		b.logger.Error("failed to list decisions", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to fetch decisions", false))
		return
	}

	if len(decisions) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemOK)+" No pending decisions! All decisions have been resolved.", false))
		return
	}

	// Count escalated decisions.
	escalatedCount := 0
	for _, d := range decisions {
		for _, label := range d.Labels {
			if label == "escalated" {
				escalatedCount++
				break
			}
		}
	}

	// Build summary header.
	headerText := fmt.Sprintf("%s *%d Pending Decision", th.BeadType["decision"], len(decisions))
	if len(decisions) != 1 {
		headerText += "s"
	}
	headerText += "*"
	if escalatedCount > 0 {
		headerText += fmt.Sprintf(" (%d %s escalated)", escalatedCount, th.BeadType["escalation"])
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", headerText, false, false),
			nil, nil),
		slack.NewDividerBlock(),
	}

	// Per-decision entries (limit to 15 to stay within Slack block limits).
	limit := 15
	if len(decisions) < limit {
		limit = len(decisions)
	}
	for _, d := range decisions[:limit] {
		question := d.Fields["question"]
		if question == "" {
			question = d.Title
		}
		if len(question) > 100 {
			question = question[:97] + "..."
		}

		// Urgency indicator.
		urgency := th.Glyph(LifecycleNeedsDecision)
		for _, label := range d.Labels {
			if label == "escalated" {
				urgency = th.BeadType["escalation"]
				break
			}
		}

		// Build text line.
		line := fmt.Sprintf("%s `%s`", urgency, d.ID)
		if d.Assignee != "" {
			line += fmt.Sprintf(" — `%s`", d.Assignee)
		}
		line += fmt.Sprintf("\n%s", question)

		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", line, false, false),
				nil,
				slack.NewAccessory(
					slack.NewButtonBlockElement(
						"view_decision_"+d.ID,
						d.ID,
						slack.NewTextBlockObject("plain_text", "View", false, false)))))
	}

	if len(decisions) > limit {
		blocks = append(blocks,
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("_...and %d more_", len(decisions)-limit), false, false)))
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionBlocks(blocks...))
}

// handleKillCommand processes the /kill slash command.
// Usage: /kill <agent> [--force]
//
// Without --force, sends ESC to the agent's coop and waits for a clean exit
// before closing the bead. With --force, closes the bead immediately.
func (b *Bot) handleKillCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	args := strings.Fields(strings.TrimSpace(cmd.Text))
	if len(args) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Usage: `/kill <agent> [--force]`", false))
		return
	}

	// Extract --force flag.
	force := false
	positional := args[:0]
	for _, a := range args {
		if a == "--force" {
			force = true
		} else {
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Usage: `/kill <agent> [--force]`", false))
		return
	}

	agentName := positional[0]

	// Only the agent's owner may kill it (bot_kill_owner.go). The kill is held
	// to the bead that was checked, and runs on the caller's ctx so the daemon
	// sees the person, not the bridge.
	bead, err := b.daemon.FindAgentBead(ctx, extractAgentName(agentName))
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Failed to kill agent %q: find agent bead: %s", agentName, err.Error()), false))
		return
	}
	if !b.callerMayKillAgent(ctx, cmd, bead) {
		return
	}
	actorCtx := context.WithoutCancel(ctx)

	// Run kill asynchronously — graceful shutdown can take 30s+ which exceeds
	// Slack's slash command response window.
	go func() {
		if err := b.killAgentBead(actorCtx, agentName, bead.ID, force); err != nil {
			b.logger.Error("kill command: failed to kill agent", "agent", agentName, "force", force, "error", err)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Failed to kill agent %q: %s", agentName, err.Error()), false))
			return
		}
		b.logger.Info("killed agent via Slack slash command", "agent", agentName, "force", force, "user", cmd.UserID)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemStop)+" Agent *%s* terminated.", agentName), false))
	}()

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(th.Glyph(LifecycleWorking)+" Killing agent *%s*…", agentName), false))
}

// handleKillThreadCommand kills a thread-bound agent by name or lists thread agents
// in the current channel. Since slash commands don't carry thread context, this
// requires the agent name as an argument.
// Usage: /kill-thread <agent> [--force] [--restart]
func (b *Bot) handleKillThreadCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	args := strings.Fields(strings.TrimSpace(cmd.Text))
	if len(args) == 0 {
		// No agent specified — list thread agents in this channel.
		b.listChannelThreadAgents(ctx, cmd)
		return
	}

	force := false
	restart := false
	positional := args[:0]
	for _, a := range args {
		switch a {
		case "--force":
			force = true
		case "--restart":
			restart = true
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Usage: `/kill-thread <agent> [--force] [--restart]`", false))
		return
	}

	agentName := positional[0]

	// Look up the agent bead to check its owner (bot_kill_owner.go) and capture
	// thread metadata before killing. This runs before the debounce so a refused
	// caller never opens the owner's restart window.
	bead, err := b.daemon.FindAgentBead(ctx, extractAgentName(agentName))
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Agent %q not found: %s", agentName, err), false))
		return
	}
	if !b.callerMayKillAgent(ctx, cmd, bead) {
		return
	}

	// First-class reopen debounce (ka-0000000034): only the --restart form is a
	// reopen (kill + session-resume respawn as a unit); a plain kill is a
	// deliberate terminal action and is never debounced. When a restart is
	// already in flight for this agent, skip the WHOLE command — NOT just the
	// respawn — because the kill here fires unconditionally before the respawn.
	// Debouncing only the respawn would kill the freshly-restarted agent and
	// then decline to restart it, leaving it dead — strictly worse than a
	// no-op. Coalescing the entire command neither re-kills nor re-restarts.
	if restart && !b.reopenDebouncer.Allow(extractAgentName(agentName)) {
		b.logger.Info("kill-thread --restart: coalesced rapid restart (debounced)",
			"agent", agentName, "user", cmd.UserID)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Glyph(LifecycleRestarted)+" Agent *%s* is already restarting — coalescing your request.", agentName), false))
		return
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(th.Glyph(LifecycleWorking)+" Killing thread agent *%s*…", agentName), false))

	threadChannel := bead.Fields["slack_thread_channel"]
	threadTS := bead.Fields["slack_thread_ts"]
	actorCtx := context.WithoutCancel(ctx)

	go func() {
		if err := b.killAgentBead(actorCtx, agentName, bead.ID, force); err != nil {
			b.logger.Error("kill-thread command: failed", "agent", agentName, "error", err)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Failed to kill thread agent %q: %s", agentName, err.Error()), false))
			return
		}

		b.logger.Info("killed thread agent via slash command", "agent", agentName, "user", cmd.UserID, "restart", restart)

		if restart && threadChannel != "" && threadTS != "" {
			b.respawnThreadAgent(actorCtx, threadChannel, threadTS, agentName,
				"Restarted via /kill-thread --restart", cmd.UserID)
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(fmt.Sprintf(th.Glyph(LifecycleRestarted)+" Thread agent *%s* restarted with session resume.", agentName), false))
		} else {
			_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
				slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemStop)+" Thread agent *%s* terminated.", agentName), false))
		}
	}()
}

// listChannelThreadAgents shows thread agents bound to threads in the given channel.
func (b *Bot) listChannelThreadAgents(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	if b.state == nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" State manager not available", false))
		return
	}

	agents := b.state.GetThreadAgentsByChannel(cmd.ChannelID)
	if len(agents) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemInfo)+" No thread agents in this channel.\nUsage: `/kill-thread <agent> [--force] [--restart]`", false))
		return
	}

	var sb strings.Builder
	sb.WriteString(th.BeadType["agent"] + " *Thread agents in this channel:*\n")
	for _, a := range agents {
		sb.WriteString(fmt.Sprintf("• `%s` — `/kill-thread %s`\n", a, a))
	}
	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(sb.String(), false))
}

// handleClearThreadsCommand removes all thread→agent mappings from state.
// This is an admin escape hatch for when stale bindings prevent new agents
// from spawning in threads.
func (b *Bot) handleClearThreadsCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	if b.state == nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" State manager not available", false))
		return
	}

	n, err := b.state.ClearAllThreadAgents()
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Failed to clear thread mappings: %s", err), false))
		return
	}

	// Also clear the in-memory lastThreadNudge map.
	b.mu.Lock()
	b.lastThreadNudge = make(map[string]time.Time)
	b.mu.Unlock()

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf("%s Cleared %d thread→agent mappings.", th.Semantic(SemOK), n), false))
}

// handleRosterCommand shows the agent dashboard as an ephemeral message.
func (b *Bot) handleRosterCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	agents, err := b.daemon.ListAgentBeads(ctx)
	if err != nil {
		b.logger.Error("failed to list agents", "error", err)
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemFail)+" Failed to fetch agent roster", false))
		return
	}

	if len(agents) == 0 {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.BeadType["agent"]+" No active agents", false))
		return
	}

	// Build roster blocks.
	headerText := fmt.Sprintf("%s *Agent Roster* — %d active", th.BeadType["agent"], len(agents))

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", headerText, false, false),
			nil, nil),
		slack.NewDividerBlock(),
	}

	// Limit display to 20 agents.
	limit := 20
	if len(agents) < limit {
		limit = len(agents)
	}
	// Runner hostnames for the laptop badge, listed only when a laptop agent
	// is on the page.
	var hostnames map[string]string
	for _, a := range agents[:limit] {
		if laptopAgentFields(a.Metadata) {
			hostnames = b.runnerHostnames(ctx)
			break
		}
	}
	for _, a := range agents[:limit] {
		name := a.AgentName
		if name == "" {
			name = a.ID
		}
		line := fmt.Sprintf("%s *%s*", agentStateBadge(th, a.AgentState), name)
		if a.Project != "" {
			line += fmt.Sprintf(" · _%s_", a.Project)
		}
		if a.Role != "" {
			line += fmt.Sprintf(" (%s/%s)", a.Mode, a.Role)
		}
		local := laptopAgentFields(a.Metadata)
		line += rosterLineSuffix(th, a, hostnames)
		if !b.notifConfig.SuppressCostReporting {
			// A laptop agent is owner-billed (the suffix says so): no cost line.
			if costLine := formatCostSummary(a.Metadata); costLine != "" && !local {
				line += fmt.Sprintf("\n%s", costLine)
			}
			// Turns + context (ka-0000003171). Unlike the terminal-only
			// notification paths, this listing mixes live and finished agents, so
			// liveness is read per-row from the agent's own state — a running row
			// gets its current context, a finished one the frozen "at exit"
			// reading and the authoritative total_request_count.
			if ct := formatAgentContextTurns(th, a.Metadata, agentStateIsLive(a.AgentState)); ct != "" {
				line += fmt.Sprintf("\n%s", ct)
			}
		}

		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", line, false, false),
				nil, nil))
	}

	if len(agents) > limit {
		blocks = append(blocks,
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("_...and %d more_", len(agents)-limit), false, false)))
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionBlocks(blocks...))
}
