package bridge

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// Manifest-vs-handler parity guard (gd-WY8A3gf5m8; agent_view migration
// ka-0000004985).
//
// The Slack event handlers wired into Bot.handleEventsAPI (bot.go) must stay in
// sync with what the Slack app manifest (gardenio/slack/manifest.json) declares.
// Slack only delivers events listed in the manifest's bot_events, and the
// assistant.threads.* API calls the handlers make require the assistant:write
// OAuth scope. History: when commit aad071648 disabled the agent surface it
// stripped the lifecycle bot_events and assistant:write scope from the manifest
// while leaving the handlers live — Slack silently stopped delivering the
// events. These tests fail loudly if that drift recurs.
//
// Under agent_view (Agent messaging experience, GA 2026-06-30) the lifecycle
// events are app_home_opened (tab=messages) and app_context_changed, which
// replaced assistant_thread_started / assistant_thread_context_changed. The
// user's typed text still arrives via message.im. slack-go has no typed struct
// for app_context_changed and its AppHomeOpenedEvent lacks the embedded
// context, so bot.go dispatches on the bridge-local superset types
// appHomeOpenedEvent / appContextChangedEvent registered in agent_view_events.go
// — hence this guard scans for those local types, not slackevents.* ones.

// agentEventTypeToName maps the bridge-local dispatch type handled in the bot.go
// dispatch switch to its authoritative Slack wire event-name. Add an entry here
// whenever a new agent-surface lifecycle handler case is added to bot.go — the
// test below fails if it finds a handled type with no mapping, so this map
// cannot silently fall behind the code.
var agentEventTypeToName = map[string]string{
	"appHomeOpenedEvent":     "app_home_opened",
	"appContextChangedEvent": "app_context_changed",
}

// requiredAgentBotEvents are the bot_events that MUST be declared in the
// manifest for the agent surface to function, beyond the lifecycle events
// discovered from bot.go. message.im carries the user's typed text; without it
// the pane sets up but silently eats every message (ka-0000000892).
var requiredAgentBotEvents = []string{"message.im"}

// slackManifest is a minimal projection of gardenio/slack/manifest.json covering
// just the fields this parity guard inspects.
type slackManifest struct {
	Features struct {
		AppHome struct {
			// Pointers so we can distinguish "explicitly set" from "omitted".
			// An omitted messages_tab_read_only_enabled defaults to read-only
			// for the agent surface, which silently bounces typed messages
			// (ka-0000000892) — the parity guard requires it be set false.
			MessagesTabEnabled         *bool `json:"messages_tab_enabled"`
			MessagesTabReadOnlyEnabled *bool `json:"messages_tab_read_only_enabled"`
		} `json:"app_home"`
		// AgentView is the current (post-migration) agent surface. AssistantView
		// is the deprecated predecessor; the migration to agent_view is one-way,
		// so the manifest must carry agent_view and NOT assistant_view.
		AgentView *struct {
			AgentDescription string `json:"agent_description"`
		} `json:"agent_view"`
		AssistantView *struct{} `json:"assistant_view"`
	} `json:"features"`
	OAuthConfig struct {
		Scopes struct {
			Bot []string `json:"bot"`
		} `json:"scopes"`
	} `json:"oauth_config"`
	Settings struct {
		EventSubscriptions struct {
			BotEvents []string `json:"bot_events"`
		} `json:"event_subscriptions"`
	} `json:"settings"`
}

// caseAgentEventRe matches dispatch arms of the form
//
//	case *appHomeOpenedEvent:
//
// capturing the bare local struct type name (appHomeOpenedEvent). The arms use
// bridge-local types (registered over slack-go's mapping in
// agent_view_events.go), so there is no slackevents. qualifier.
var caseAgentEventRe = regexp.MustCompile(`case \*(app(?:HomeOpened|ContextChanged)Event)\b`)

// handledAgentEventNames scans bot.go for agent-surface lifecycle dispatch arms
// and returns the set of wire event-names they handle. It fails the test if it
// encounters a handled struct type that is not present in agentEventTypeToName,
// forcing the map (and thus the manifest assertion) to track new handlers.
func handledAgentEventNames(t *testing.T) map[string]struct{} {
	t.Helper()

	src, err := os.ReadFile("bot.go")
	if err != nil {
		t.Fatalf("reading bot.go: %v", err)
	}

	names := make(map[string]struct{})
	matches := caseAgentEventRe.FindAllSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("found no `case *appHomeOpenedEvent:` / `case *appContextChangedEvent:` " +
			"dispatch arms in bot.go — did the dispatch switch move or change shape, " +
			"or did the agent_view migration get reverted? Update caseAgentEventRe.")
	}
	for _, m := range matches {
		typeName := string(m[1])
		eventName, ok := agentEventTypeToName[typeName]
		if !ok {
			t.Fatalf("bot.go handles *%s but agentEventTypeToName has no event-name "+
				"mapping for it — add %s -> \"<wire_event>\" so the manifest parity "+
				"assertion covers it", typeName, typeName)
		}
		names[eventName] = struct{}{}
	}
	return names
}

func loadSlackManifest(t *testing.T) slackManifest {
	t.Helper()
	// Test working dir is the package dir (.../internal/bridge); the manifest
	// lives at the repo's gardenio/slack/manifest.json.
	const manifestPath = "../../../slack/manifest.json"
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			// A filtered checkout may materialize only the
			// gardenio/controller Go module tree; the sibling gardenio/slack/
			// directory is absent. Skip rather than fatal so the guard still
			// runs in any checkout (local, full-repo CI) that has the file,
			// mirroring the t.Skip pattern in cmd/gb/agent_start_prompt_test.go
			// for the same out-of-module-tree condition.
			t.Skipf("%s not found from this cwd: %v", manifestPath, err)
		}
		t.Fatalf("reading %s: %v", manifestPath, err)
	}
	var m slackManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parsing %s: %v", manifestPath, err)
	}
	return m
}

// TestManifestDeclaresHandledAgentEvents asserts every agent-surface lifecycle
// event handled in bot.go — plus the required message.im delivery event — is
// declared in the manifest's bot_events. Without the declaration Slack never
// delivers the event and the handler is dead code (ka-0000000892).
func TestManifestDeclaresHandledAgentEvents(t *testing.T) {
	handled := handledAgentEventNames(t)
	manifest := loadSlackManifest(t)

	declared := make(map[string]struct{}, len(manifest.Settings.EventSubscriptions.BotEvents))
	for _, e := range manifest.Settings.EventSubscriptions.BotEvents {
		declared[e] = struct{}{}
	}

	for event := range handled {
		if _, ok := declared[event]; !ok {
			t.Errorf("bot.go handles %q but it is not declared in manifest bot_events "+
				"(gardenio/slack/manifest.json settings.event_subscriptions.bot_events). "+
				"Slack will not deliver this event — add it to the manifest.", event)
		}
	}

	for _, event := range requiredAgentBotEvents {
		if _, ok := declared[event]; !ok {
			t.Errorf("manifest bot_events is missing %q (gardenio/slack/manifest.json). "+
				"The agent surface needs it to receive the user's typed text; without "+
				"it the pane sets up but silently eats every message (ka-0000000892).", event)
		}
	}
}

// TestManifestHasAssistantWriteScope asserts the assistant:write OAuth scope is
// present whenever agent-surface handlers are wired up. The handlers call
// assistant.threads.* APIs (SetSuggestedPrompts / SetStatus / SetTitle), all of
// which require assistant:write — this scope is unchanged by the agent_view
// migration.
func TestManifestHasAssistantWriteScope(t *testing.T) {
	handled := handledAgentEventNames(t)
	if len(handled) == 0 {
		// No agent handlers wired up; the scope is not required. Guarded so a
		// future removal of all handlers doesn't make this test spuriously fail.
		return
	}

	manifest := loadSlackManifest(t)
	const wantScope = "assistant:write"
	for _, s := range manifest.OAuthConfig.Scopes.Bot {
		if s == wantScope {
			return
		}
	}
	t.Errorf("manifest is missing OAuth scope %q (gardenio/slack/manifest.json "+
		"oauth_config.scopes.bot) but bot.go wires up agent-surface handlers "+
		"that call assistant.threads.* APIs requiring it", wantScope)
}

// TestManifestUsesAgentViewNotAssistantView asserts the manifest declares the
// current agent_view feature and NOT the deprecated assistant_view. The
// migration from assistant_view to agent_view is ONE-WAY (Slack cannot revert
// it), so a manifest carrying assistant_view again would be both a regression
// and un-reinstallable against the migrated live app.
func TestManifestUsesAgentViewNotAssistantView(t *testing.T) {
	handled := handledAgentEventNames(t)
	if len(handled) == 0 {
		// No agent handlers wired up; the surface isn't in use.
		return
	}

	manifest := loadSlackManifest(t)

	if manifest.Features.AgentView == nil {
		t.Error("bot.go wires up agent-surface handlers but manifest has no " +
			"features.agent_view block (gardenio/slack/manifest.json) — the agent " +
			"pane will not be available (agent_view migration ka-0000004985)")
	} else {
		// Slack caps agent_description at 300 chars and rejects a manifest
		// update that exceeds it. Without this guard an over-length edit would
		// pass CI and only fail at operator manifest-apply/reinstall time — the
		// exact class of drift this parity test exists to catch. Also require it
		// be non-empty: agent_description is what users see for the surface.
		const maxAgentDescription = 300
		desc := manifest.Features.AgentView.AgentDescription
		if len(desc) == 0 {
			t.Error("manifest features.agent_view.agent_description is empty " +
				"(gardenio/slack/manifest.json) — set the user-facing description " +
				"for the agent surface")
		}
		if len(desc) > maxAgentDescription {
			t.Errorf("manifest features.agent_view.agent_description is %d chars, "+
				"exceeds Slack's %d-char cap — Slack will reject the manifest "+
				"apply/reinstall (gardenio/slack/manifest.json)", len(desc), maxAgentDescription)
		}
	}
	if manifest.Features.AssistantView != nil {
		t.Error("manifest still declares features.assistant_view; the agent_view " +
			"migration (ka-0000004985) is one-way — replace assistant_view with " +
			"agent_view (assistant_description -> agent_description)")
	}
}

// TestManifestAgentMessagesTabWritable asserts the App Home Messages tab is
// enabled AND not read-only whenever the agent surface is configured.
//
// This is the ka-0000000892 failure mode: a user's typed message in the agent
// pane is delivered as a message.im event ONLY if the Messages tab accepts user
// input. If messages_tab_read_only_enabled is true — or merely omitted, which
// defaults the surface to read-only — Slack bounces the typed text client-side
// with "Slack couldn't send this message" and never emits message.im, so no
// user DM is ever delivered. The lifecycle events (app_home_opened /
// app_context_changed) still arrive, making the failure silent. Pin the
// writable Messages tab in source so a future manifest edit/reinstall can't
// silently re-break it.
func TestManifestAgentMessagesTabWritable(t *testing.T) {
	handled := handledAgentEventNames(t)
	if len(handled) == 0 {
		// No agent handlers wired up; the agent pane isn't in use, so the
		// Messages-tab requirement doesn't apply.
		return
	}

	manifest := loadSlackManifest(t)

	ah := manifest.Features.AppHome
	if ah.MessagesTabEnabled == nil || !*ah.MessagesTabEnabled {
		t.Error("manifest features.app_home.messages_tab_enabled must be true so the " +
			"agent pane can receive typed messages (message.im); the agent handlers " +
			"in bot.go are dead without it (ka-0000000892)")
	}
	// Must be explicitly false: an omitted value defaults the agent surface to
	// read-only, which silently bounces typed messages.
	if ah.MessagesTabReadOnlyEnabled == nil {
		t.Error("manifest features.app_home.messages_tab_read_only_enabled is omitted; " +
			"it must be explicitly set to false. An omitted value defaults the agent " +
			"Messages tab to read-only, so Slack bounces typed messages with \"Slack " +
			"couldn't send this message\" and never delivers message.im (ka-0000000892)")
	} else if *ah.MessagesTabReadOnlyEnabled {
		t.Error("manifest features.app_home.messages_tab_read_only_enabled is true; " +
			"it must be false so users can type in the agent pane. A read-only " +
			"Messages tab bounces typed messages and never delivers message.im " +
			"(ka-0000000892)")
	}
}

// Link-unfurl parity guard (gd-savf21snA8; epic gd-HE8w8hlfyz).
//
// The link_shared surface has the same manifest-vs-handler drift risk as the
// agent surface above, but a distinct set of manifest requirements. When
// handleLinkShared is wired into the bot.go dispatch switch:
//   - Slack only delivers link_shared if it is declared in bot_events, so an
//     un-declared handler is silently dead (same failure class as the agent
//     lifecycle events).
//   - The handler calls chat.unfurl (UnfurlMessageContext / UnfurlMessageByID)
//     on the BOT token, which requires the bot links:write scope; link_shared
//     delivery itself requires the bot links:read scope. Today those two scopes
//     exist only on the USER token — the bot client that does the unfurling
//     needs its own copy.
//
// Keyed on the handler being wired (discovered from bot.go), so this stays inert
// until Phase 1 lands the dispatch arm and then fails loudly if the manifest
// wasn't updated in lockstep — mirroring the handledAgentEventNames pattern.

// caseLinkSharedRe matches the link_shared dispatch arm of the form
//
//	case *slackevents.LinkSharedEvent:
//
// The link_shared handler dispatches on slack-go's typed event (unlike the
// agent-surface arms, which use bridge-local superset types), so this looks for
// the slackevents. qualifier.
var caseLinkSharedRe = regexp.MustCompile(`case \*slackevents\.LinkSharedEvent\b`)

// linkSharedHandlerWired reports whether bot.go dispatches the link_shared event
// to a handler. Returns false (the surface isn't in use yet) until Phase 1 adds
// the dispatch arm, at which point the assertions below become active.
func linkSharedHandlerWired(t *testing.T) bool {
	t.Helper()

	src, err := os.ReadFile("bot.go")
	if err != nil {
		t.Fatalf("reading bot.go: %v", err)
	}
	return caseLinkSharedRe.Match(src)
}

// TestManifestDeclaresLinkSharedEvent asserts link_shared is declared in the
// manifest bot_events whenever bot.go wires up a link_shared handler. Without
// the declaration Slack never delivers the event and the unfurl handler is dead
// code (same failure mode as the agent lifecycle events, ka-0000000892).
func TestManifestDeclaresLinkSharedEvent(t *testing.T) {
	if !linkSharedHandlerWired(t) {
		// No link_shared handler wired up yet; the bot_event is not required.
		return
	}

	manifest := loadSlackManifest(t)

	const wantEvent = "link_shared"
	for _, e := range manifest.Settings.EventSubscriptions.BotEvents {
		if e == wantEvent {
			return
		}
	}
	t.Errorf("bot.go handles link_shared but %q is not declared in manifest "+
		"bot_events (gardenio/slack/manifest.json "+
		"settings.event_subscriptions.bot_events). Slack will not deliver the "+
		"event — add it to the manifest (gd-savf21snA8).", wantEvent)
}

// TestManifestHasBotLinksScopes asserts the bot links:read and links:write
// OAuth scopes are present whenever a link_shared handler is wired. link_shared
// delivery requires bot links:read; the chat.unfurl call the handler makes runs
// on the BOT token and requires bot links:write. Both scopes exist on the user
// token today but must also be granted to the bot token that does the unfurling.
func TestManifestHasBotLinksScopes(t *testing.T) {
	if !linkSharedHandlerWired(t) {
		// No link_shared handler wired up; the scopes are not required.
		return
	}

	manifest := loadSlackManifest(t)

	have := make(map[string]struct{}, len(manifest.OAuthConfig.Scopes.Bot))
	for _, s := range manifest.OAuthConfig.Scopes.Bot {
		have[s] = struct{}{}
	}
	for _, want := range []string{"links:read", "links:write"} {
		if _, ok := have[want]; !ok {
			t.Errorf("manifest is missing bot OAuth scope %q (gardenio/slack/"+
				"manifest.json oauth_config.scopes.bot) but bot.go wires up a "+
				"link_shared handler. links:read is required for link_shared "+
				"delivery and links:write for the chat.unfurl call, both on the "+
				"BOT token (gd-savf21snA8).", want)
		}
	}
}

// Group-DM (mpim) parity guard (gd-rJ6qFROAkq; epic ka-0000003709).
//
// The group-DM surface has the same manifest-vs-handler drift risk as the
// link_shared surface above. When handleMessageEvent grows an mpim branch
// (Phase 1, ka-0000006051):
//   - Slack only delivers message.mpim if it is declared in bot_events, so an
//     un-declared mpim branch is silently dead — gardenio stays deaf in group
//     DMs it is a member of (same failure class as ka-0000000892).
//   - Delivery and the API calls the surface makes require bot mpim scopes:
//     mpim:history for message.mpim delivery and conversations.history/replies
//     in mpims, mpim:read for conversations.info/members on mpims, and
//     mpim:write for the conversations.open call that starts new group DMs
//     (Phase 2, gd-e9FTYQGxtr). Today all three exist only on the USER token —
//     the bot token the bridge runs on needs its own copies.
//
// Keyed on the mpim branch being wired (discovered from bot.go), so this stays
// inert until Phase 1 lands the ChannelType == "mpim" handling and then fails
// loudly if the manifest wasn't updated in lockstep — mirroring the
// linkSharedHandlerWired pattern.
//
// NOTE: manifest scope changes only take effect after the Slack app is
// re-installed to the workspace (operator gate gd-5K4qH8JnqX) — this guard
// keeps source-level parity; it cannot observe the installed app.

// mpimBranchRe matches an mpim channel-type condition of the form
//
//	ev.ChannelType == "mpim"
//
// in handleMessageEvent's routing. Unlike link_shared there is no dedicated
// dispatch arm — message.mpim arrives as a MessageEvent and Phase 1 routes it
// by ChannelType, analogous to the existing "group" (private channel) checks.
var mpimBranchRe = regexp.MustCompile(`ChannelType\s*==\s*"mpim"`)

// mpimBranchWired reports whether bot.go routes mpim-channel messages to a
// handler. Returns false (the surface isn't in use yet) until Phase 1 adds the
// mpim branch, at which point the assertions below become active.
func mpimBranchWired(t *testing.T) bool {
	t.Helper()

	src, err := os.ReadFile("bot.go")
	if err != nil {
		t.Fatalf("reading bot.go: %v", err)
	}
	return mpimBranchRe.Match(src)
}

// TestManifestDeclaresMessageMpimEvent asserts message.mpim is declared in the
// manifest bot_events whenever bot.go routes mpim messages. Without the
// declaration Slack never delivers the event and the mpim branch is dead code
// (same failure mode as the agent lifecycle events, ka-0000000892).
func TestManifestDeclaresMessageMpimEvent(t *testing.T) {
	if !mpimBranchWired(t) {
		// No mpim branch wired up yet; the bot_event is not required.
		return
	}

	manifest := loadSlackManifest(t)

	const wantEvent = "message.mpim"
	for _, e := range manifest.Settings.EventSubscriptions.BotEvents {
		if e == wantEvent {
			return
		}
	}
	t.Errorf("bot.go routes mpim messages but %q is not declared in manifest "+
		"bot_events (gardenio/slack/manifest.json "+
		"settings.event_subscriptions.bot_events). Slack will not deliver the "+
		"event — add it to the manifest (gd-rJ6qFROAkq).", wantEvent)
}

// TestManifestHasBotMpimScopes asserts the bot mpim:history, mpim:read and
// mpim:write OAuth scopes are present whenever an mpim branch is wired.
// message.mpim delivery requires bot mpim:history; conversations.info/members
// on mpims require mpim:read; the conversations.open call that starts new
// group DMs (Phase 2) requires mpim:write. All three scopes exist on the user
// token today but must also be granted to the bot token the bridge runs on.
func TestManifestHasBotMpimScopes(t *testing.T) {
	if !mpimBranchWired(t) {
		// No mpim branch wired up; the scopes are not required.
		return
	}

	manifest := loadSlackManifest(t)

	have := make(map[string]struct{}, len(manifest.OAuthConfig.Scopes.Bot))
	for _, s := range manifest.OAuthConfig.Scopes.Bot {
		have[s] = struct{}{}
	}
	for _, want := range []string{"mpim:history", "mpim:read", "mpim:write"} {
		if _, ok := have[want]; !ok {
			t.Errorf("manifest is missing bot OAuth scope %q (gardenio/slack/"+
				"manifest.json oauth_config.scopes.bot) but bot.go routes mpim "+
				"(group DM) messages. mpim:history is required for message.mpim "+
				"delivery, mpim:read for conversations.info/members, and "+
				"mpim:write for conversations.open, all on the BOT token "+
				"(gd-rJ6qFROAkq). Scope changes need a Slack app re-install "+
				"(gd-5K4qH8JnqX).", want)
		}
	}
}
