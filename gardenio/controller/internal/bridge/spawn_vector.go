package bridge

import (
	"fmt"
	"strings"
)

// Spawn-vector icons. Distinct emoji per spawn vector make the way an agent
// was created readable from the channel scroll without expanding the card
// (ka-0000004046). These shortcodes are the theme-neutral KEY of each vector:
// the classified line is cached under them at event time (no channel, no theme
// yet), and themeSpawnVectorLine swaps in the channel theme's glyph when the
// card is rendered. They never reach Slack unthemed.
const (
	spawnIconWebhook  = ":hook:"                      // 🪝 webhook / external trigger
	spawnIconSlack    = ":speech_balloon:"            // 💬 Slack thread / reply
	spawnIconJira     = ":envelope_with_arrow:"       // 📩 jira mention
	spawnIconGitlab   = ":twisted_rightwards_arrows:" // 🔀 gitlab mention / MR review
	spawnIconCron     = ":alarm_clock:"               // ⏰ scheduled (cron)
	spawnIconGardener = ":robot_face:"                // 🤖 spawned by a gardener / gardener loop
	spawnIconManual   = ":bust_in_silhouette:"        // 🧑 manual gb spawn
	spawnIconFallback = ":robot_face:"                // 🤖 neutral fallback
)

// spawnVectorGlyph returns the theme's glyph for a spawn-vector key (one of the
// spawnIcon* constants), or "" for an unknown key.
func spawnVectorGlyph(th Theme, icon string) string {
	switch icon {
	case spawnIconWebhook:
		return th.Semantic(SemTrigger)
	case spawnIconSlack:
		return th.Semantic(SemNote)
	case spawnIconJira:
		return th.Semantic(SemLink)
	case spawnIconGitlab:
		return th.Semantic(SemReview)
	case spawnIconCron:
		return th.Semantic(SemSchedule)
	case spawnIconGardener: // spawnIconFallback shares this key
		return th.BeadType["agent"]
	case spawnIconManual:
		return th.Semantic(SemAssignee)
	}
	return ""
}

// themeSpawnVectorLine rewrites a cached spawn-vector line ("<key> <label>",
// from formatSpawnVectorLine) with the theme's glyph in place of the key. The
// glyph stays glued to the label, per the accessibility rule. An unrecognised
// line is returned as-is.
func themeSpawnVectorLine(th Theme, line string) string {
	icon, label, ok := strings.Cut(line, " ")
	if !ok {
		return line
	}
	g := spawnVectorGlyph(th, icon)
	if g == "" {
		return line
	}
	return g + " " + label
}

// classifySpawnVector maps an agent bead's spawn-context fields to a distinct
// (icon, label) pair describing how the agent was spawned. The label carries
// the operator-relevant detail (which webhook, who replied, which gardener).
//
// Classification is driven primarily by the authoritative spawn_source field,
// with the trigger source / gardener-loop context / spawned_by fields layered
// in for vectors that spawn_source alone cannot disambiguate (a plain cron run
// vs a gardener loop both carry spawn_source="schedule"; a webhook trigger spawn
// carries no spawn_source at all, only trigger_id + source).
//
// Unknown / unrecognized vectors fall back to a neutral icon plus the raw
// spawn_source value so the line is never silently empty for a real spawn.
// Returns ("", "") only when there is genuinely no spawn context to show.
func classifySpawnVector(fields map[string]string) (icon, label string) {
	if fields == nil {
		return "", ""
	}
	src := fields["spawn_source"]

	switch {
	// Slack-thread, thread-pool, thread-resume, concierge, and decision-resume
	// are all Slack-originated conversational spawns.
	case strings.HasPrefix(src, "slack-thread"), src == "concierge", src == "decision-resume":
		return spawnIconSlack, slackSpawnLabel(fields, src)

	case src == "jira-mention":
		if key := fields["jira_key"]; key != "" {
			return spawnIconJira, "jira-mention from " + key
		}
		if author := fields["mention_author"]; author != "" {
			return spawnIconJira, "jira-mention from " + author
		}
		return spawnIconJira, "jira-mention"

	case strings.HasPrefix(src, "gitlab"):
		return spawnIconGitlab, gitlabSpawnLabel(fields, src)

	// A gardener loop run carries spawn_source="schedule" plus gardener-loop
	// context (terminal goal / max runs). Surface it as a gardener (🤖) rather
	// than a plain cron (⏰) so the distinction the operator cares about is
	// visible at a glance. Also catches an explicit spawn_source="gardener".
	case src == "gardener", src == "schedule" && isGardenerLoop(fields):
		return spawnIconGardener, gardenerScheduleLabel(fields)

	case src == "schedule":
		return spawnIconCron, cronScheduleLabel(fields)

	// Crew spawned by a gardener agent: spawned_by references the gardener's
	// agent id (e.g. "gardenio-...-gardener-delivery-...").
	case isGardenerSpawner(fields["spawned_by"]):
		return spawnIconGardener, "spawned by gardener " + shortenSpawner(fields["spawned_by"])

	// Webhook / external trigger spawns set trigger_id + source but no
	// spawn_source. The match key (e.g. "FSM-1234") is the operator-relevant
	// detail; fall back to the trigger title, then the source name.
	case fields["trigger_id"] != "":
		return spawnIconWebhook, webhookSpawnLabel(fields)

	case src == "manual", src == "pool-assign-cli":
		return spawnIconManual, "manual gb spawn"

	case src != "":
		// Recognized-but-unmapped source: never drop it silently.
		return spawnIconFallback, src

	default:
		return "", ""
	}
}

// formatSpawnVectorLine renders the spawn vector as a single Slack-mrkdwn
// context line ("icon label"). Returns "" when there is no spawn context.
func formatSpawnVectorLine(fields map[string]string) string {
	icon, label := classifySpawnVector(fields)
	if icon == "" || label == "" {
		return ""
	}
	return icon + " " + label
}

// slackSpawnLabel builds the human-readable label for a Slack-originated spawn.
// When a Slack user id is present it is rendered as a real <@U…> mention so the
// operator can see who replied; resume spawns are tagged distinctly.
func slackSpawnLabel(fields map[string]string, src string) string {
	resume := strings.HasSuffix(src, "-resume")
	if uid := fields["slack_user_id"]; uid != "" {
		if resume {
			return fmt.Sprintf("Slack thread resume (<@%s>)", uid)
		}
		return fmt.Sprintf("Slack reply from <@%s>", uid)
	}
	if resume {
		return "Slack thread resume"
	}
	return "Slack thread"
}

// gitlabSpawnLabel labels a GitLab-originated spawn (MR review vs mention).
// The MR iid / mention author give the operator-relevant detail.
func gitlabSpawnLabel(fields map[string]string, src string) string {
	kind := "gitlab"
	switch src {
	case "gitlab-mr-review":
		kind = "gitlab MR review"
	case "gitlab-mention":
		kind = "gitlab-mention"
	}
	if iid := fields["gitlab_mr_iid"]; iid != "" {
		return fmt.Sprintf("%s on !%s", kind, iid)
	}
	if author := fields["mention_author"]; author != "" {
		return fmt.Sprintf("%s from %s", kind, author)
	}
	return kind
}

// isGardenerLoop reports whether schedule fields indicate a gardener loop run
// (as opposed to a plain scheduled task). Gardener loops carry a terminal goal
// and/or a max-runs budget stamped by the scheduler / trigger engine.
func isGardenerLoop(fields map[string]string) bool {
	return fields["schedule_terminal_goal"] != "" ||
		fields["schedule_max_runs"] != "" ||
		fields["schedule_runs_spawned"] != ""
}

// gardenerScheduleLabel labels a gardener-loop run, preferring the schedule title.
func gardenerScheduleLabel(fields map[string]string) string {
	if t := fields["schedule_title"]; t != "" {
		return "gardener loop: " + t
	}
	if id := fields["schedule_id"]; id != "" {
		return "gardener loop " + id
	}
	return "gardener loop"
}

// cronScheduleLabel labels a plain scheduled (cron) run.
func cronScheduleLabel(fields map[string]string) string {
	title := fields["schedule_title"]
	if title == "" {
		title = "scheduled task"
	}
	if cron := fields["schedule_cron"]; cron != "" {
		return fmt.Sprintf("%s (cron `%s`)", title, cron)
	}
	return title + " (cron)"
}

// webhookSpawnLabel labels an external-trigger / webhook spawn. Prefers the
// source-specific match key, then the trigger title, then the source name.
func webhookSpawnLabel(fields map[string]string) string {
	source := fields["source"]
	if key := triggerMatchKeyFromFields(fields); key != "" {
		if source != "" {
			return fmt.Sprintf("%s %s webhook", key, source)
		}
		return key + " webhook"
	}
	if title := fields["trigger_title"]; title != "" {
		return title + " webhook"
	}
	if source != "" {
		return source + " webhook"
	}
	return "webhook"
}

// triggerMatchKeyFromFields recovers the source-specific match key written by
// the trigger engine at "<source>_key" (see triggerSourceFieldKey).
func triggerMatchKeyFromFields(fields map[string]string) string {
	src := fields["source"]
	if src == "" {
		return ""
	}
	return fields[triggerSourceFieldKey(src)]
}

// isGardenerSpawner reports whether a spawned_by value refers to a gardener agent.
// Trigger spawns use "trigger:<id>"; gardener-spawned crew carry the gardener's
// agent name, which by convention contains "gardener".
func isGardenerSpawner(spawnedBy string) bool {
	if spawnedBy == "" || strings.HasPrefix(spawnedBy, "trigger:") {
		return false
	}
	return strings.Contains(spawnedBy, "gardener")
}

// shortenSpawner trims a gardener agent name to a compact, readable suffix for
// the card line. Gardenio agent names are "<project>-<slug>-<rand>"; the leading
// "<project>-" prefix is noise on the card, so drop one leading "gardenio-".
func shortenSpawner(spawnedBy string) string {
	return strings.TrimPrefix(spawnedBy, "gardenio-")
}
