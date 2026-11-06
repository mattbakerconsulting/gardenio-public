// Package bridge provides configurable notification suppression.
//
// NotificationConfig controls which Slack notifications the bridge emits.
// When thread status indicators (reactions, assistant thread status) are
// active, operators can suppress redundant notifications via config beads.
//
// Config bead category: "bridge-notifications"
// Merge strategy: MergeOverride (role/project overrides global)
package bridge

// NotificationConfig controls which Slack notifications the bridge emits.
// Fields default to false (all notifications enabled). Set to true to suppress.
type NotificationConfig struct {
	// SuppressBeadActivity suppresses per-bead thread replies (created,
	// claimed, closed notifications). The streaming status + reactions
	// already show this activity in real time.
	SuppressBeadActivity bool `json:"suppress_bead_activity"`

	// SuppressSpawnConfirmation suppresses the "Spinning up..." or
	// "Assigned a prewarmed agent..." message in threads. The 👀 reaction
	// on the parent message already indicates the agent was seen.
	SuppressSpawnConfirmation bool `json:"suppress_spawn_confirmation"`

	// SuppressToolStream suppresses the live tool-use streaming messages
	// in assistant threads. The thread status text already shows what
	// the agent is doing.
	SuppressToolStream bool `json:"suppress_tool_stream"`

	// SuppressSquawk suppresses agent squawk messages from being posted
	// to Slack threads. Squawks are informal status updates from agents.
	SuppressSquawk bool `json:"suppress_squawk"`

	// SuppressMentionHint suppresses the "Tip: @mention me so agent sees
	// your message" hint posted when someone replies in an agent thread
	// without @mentioning the bot.
	SuppressMentionHint bool `json:"suppress_mention_hint"`

	// SuppressCostReporting suppresses per-session and per-agent cost
	// display in agent cards, thread replies, dashboard, and streaming
	// usage updates. Cost data is sourced from LiteLLM spend tracking,
	// which provides accurate per-account and per-session cost data.
	SuppressCostReporting bool `json:"suppress_cost_reporting"`

	// HoldAssistantStatus controls whether the bridge continuously re-holds
	// the Slack assistant-thread status footer ("running commands... (44 tools)
	// [9m57s]") for the entire duration of an agent run. This is the ONLY axis
	// on which the assistant pane diverged from the main threaded interface —
	// the main interface never sets a status, and it accepts follow-up messages
	// while inference is in progress; the assistant pane held the status
	// populated every flush tick and could not (ka-0000003303, ka-0000000115).
	//
	// Slack does not document a composer lock, but a continuously-held status on
	// a DM-style surface (which the assistant pane is) is community-reported to
	// lock the composer. Defaulting this OFF makes the pane mirror the known-good
	// interface: live progress still renders in the streaming message (purple
	// shimmer / task_update chunks) and the agent card — only the duplicate
	// footer status is dropped, and the terminal status clear still fires.
	//
	// Set true to restore the old always-on footer status (e.g. if a live repro
	// shows the composer lock has another cause and the footer is desirable).
	HoldAssistantStatus bool `json:"hold_assistant_status"`
}

// DefaultNotificationConfig returns the default config.
// SuppressMentionHint defaults to true because the native Slack assistant
// framework handles thread interactions without requiring @mentions.
func DefaultNotificationConfig() NotificationConfig {
	return NotificationConfig{
		SuppressMentionHint: true,
	}
}

// ThreadCardsConfig is the runtime kill switch + project allow-list for the
// thread-cards feature (agent cards posted inside user-owned Slack threads).
// It is loaded from "bridge-thread-cards" config beads and layered on top of
// the deploy-time SLACK_THREAD_CARDS env mode (see Bot.threadCardsEnabled).
//
// Config bead category: "bridge-thread-cards"
type ThreadCardsConfig struct {
	// Enabled is the runtime kill switch. When a config bead sets
	// enabled=false, thread cards are OFF fleet-wide regardless of the env
	// mode — an operator can disable instantly with no redeploy. Defaults to
	// true so that the absence of a config bead leaves the env mode in
	// charge (and the env mode itself defaults to "off").
	Enabled bool `json:"enabled"`

	// Projects is an optional allow-list. When non-empty, thread cards are
	// posted only for agents whose project is in the list (e.g. canary in
	// "gardenio-e2e" before promoting to "gardenio"). Empty = all projects,
	// subject to Enabled and the env mode.
	Projects []string `json:"projects"`

	// ExcludeChannels is an optional channel deny-list (Slack channel IDs).
	// Threads in a listed channel never get an agent card, regardless of the
	// env mode or Projects; they take the legacy thread-reply path instead.
	ExcludeChannels []string `json:"exclude_channels"`

	// PingThresholdSeconds is the run-duration cutoff (in seconds) for the
	// terminal completion ping (UX Q1 / D3,
	// docs/research/thread-cards/02-ux-design.md §Q1). At completion a thread
	// card is silently edited into its wrap-up form; a one-line ping reply is
	// then posted to re-notify the thread ONLY when the run lasted longer than
	// this OR produced MRs/PRs. Short, MR-less runs get the silent edit only,
	// keeping the thread clean. Default 120 (2 min). Set to 0 to disable
	// duration-based pings (MR/PR-based pings still fire). Operators can retune
	// it via the config bead with no redeploy.
	PingThresholdSeconds int `json:"ping_threshold_seconds"`
}

// DefaultThreadCardsConfig returns the default thread-cards config: Enabled
// true with no project allow-list, so absence of a config bead defers to the
// SLACK_THREAD_CARDS env mode (which defaults to "off"). The completion-ping
// threshold defaults to 120s (UX Q1).
func DefaultThreadCardsConfig() ThreadCardsConfig {
	return ThreadCardsConfig{Enabled: true, PingThresholdSeconds: 120}
}
