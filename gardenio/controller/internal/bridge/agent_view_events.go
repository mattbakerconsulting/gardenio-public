package bridge

import (
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// Slack agent_view (Agent messaging experience, GA 2026-06-30) migration glue.
//
// agent_view replaced the deprecated assistant_view lifecycle events:
//   - assistant_thread_started        -> app_home_opened (tab == "messages")
//   - assistant_thread_context_changed -> app_context_changed
//
// slack-go's typing has two gaps we bridge here (verified against v0.18.0 and
// still true at v0.27.0, so a library bump does NOT help):
//
//  1. app_context_changed has NO typed struct in slack-go at all. Its parser
//     drops unknown inner events as EventTypeErrorBadMessage — they never reach
//     handleEventsAPI. So we must register a struct for it or the event is lost.
//  2. slack-go's typed AppHomeOpenedEvent lacks the `context` field Slack now
//     embeds in app_home_opened once an app subscribes to app_context_changed.
//     We register a superset struct so the viewed-channel context survives the
//     parse and we can resolve the project the user is looking at on open.
//
// slackevents.EventsAPIInnerEventMapping is an exported, mutable map the library
// consults first when parsing (slackevents.eventsMap). Registering our types
// there makes ParseEvent unmarshal both events and deliver them through the
// normal CallbackEvent dispatch in bot.go — no raw json.RawMessage handling in
// the socket loop. Nothing else in the bridge handles app_home_opened, so
// overriding its native mapping with our superset is safe.

// appContextChangedEventType is the wire name of the app_context_changed event.
// slack-go has no EventsAPIType constant for it, so we declare our own.
const appContextChangedEventType = slackevents.EventsAPIType("app_context_changed")

// agentContextChannelIDType is the entity type Slack uses for a channel_id in an
// agent context (`context.entities[].type`).
const agentContextChannelIDType = "slack#/types/channel_id"

// agentContextEntity is one entry in an agent context's `entities` array, e.g.
// {"type":"slack#/types/channel_id","value":"C0123","team_id":"T0123"}. Entities
// are ordered by relevance (most-relevant first).
type agentContextEntity struct {
	Type   string `json:"type"`
	Value  string `json:"value"`
	TeamID string `json:"team_id"`
}

// agentContext mirrors the `context` object Slack sends on app_context_changed
// (and embeds as `context` in app_home_opened / `app_context` in message.im once
// app_context_changed is subscribed). When the user is not viewing anything that
// applies, Slack sends an empty context object, so Entities may be nil.
type agentContext struct {
	Entities []agentContextEntity `json:"entities"`
}

// channelID returns the most-relevant channel_id entity value, or "" if the
// context carries no channel the user is viewing.
func (c agentContext) channelID() string {
	for _, e := range c.Entities {
		if e.Type == agentContextChannelIDType && e.Value != "" {
			return e.Value
		}
	}
	return ""
}

// appContextChangedEvent is the app_context_changed inner event. It fires when
// the user's active context changes while the app is open. Note: unlike the old
// assistant_thread_context_changed event, it carries NEITHER the DM channel nor
// the user id, so it cannot be attributed to a specific DM at the bridge level —
// see handleAppContextChanged for how we treat it.
type appContextChangedEvent struct {
	Type    string       `json:"type"`
	Context agentContext `json:"context"`
}

// appHomeOpenedEvent is a superset of slackevents.AppHomeOpenedEvent that also
// captures the `context` Slack embeds when app_context_changed is subscribed.
// Field tags mirror slack-go's AppHomeOpenedEvent so the native fields parse
// identically; Context is the added field.
type appHomeOpenedEvent struct {
	Type           string       `json:"type"`
	User           string       `json:"user"`
	Channel        string       `json:"channel"`
	EventTimeStamp string       `json:"event_ts"`
	Tab            string       `json:"tab"`
	View           *slack.View  `json:"view,omitempty"`
	Context        agentContext `json:"context"`
}

func init() {
	// Override the native app_home_opened mapping with our superset so the
	// embedded viewed-channel context is not dropped on parse.
	slackevents.EventsAPIInnerEventMapping[slackevents.AppHomeOpened] = appHomeOpenedEvent{}
	// Register the (otherwise unknown) app_context_changed inner event so
	// slack-go parses and delivers it instead of dropping it as a bad message.
	slackevents.EventsAPIInnerEventMapping[appContextChangedEventType] = appContextChangedEvent{}
}
