package beadsapi

// Slack routing fields on an agent bead (gd-jOHtHdkdIt).
//
// The bridge's resolveChannel (internal/bridge/bot_agents.go) and its thread
// lookup read these fields to decide where an agent's card, decisions and
// squawks land. Every path that mints a new agent bead to CONTINUE an existing
// one — gb stop --successor=new (cmd/gb/stop_successor.go) and the
// decision-resolution respawn (internal/bridge/decisions.go) — must carry the
// whole set, or the continuation posts somewhere its predecessor never did: a
// scheduled agent drops to channel_roles / the project primary, a thread
// agent's successor leaves its thread, and an inherited spawn channel becomes
// an explicit pin. Both paths copy from this one list so they cannot drift.

// FieldSlackSpawnChannelInheritedFromRole marks slack_spawn_channel as copied
// from a spawner (gb spawn propagation) rather than pinned explicitly; its
// value is the spawner's role. resolveChannel ranks such a channel below the
// project's channel_roles entry unless the roles match (gd-3JHijlgZrE).
const FieldSlackSpawnChannelInheritedFromRole = "slack_spawn_channel_inherited_from_role"

// SlackRoutingFields lists every agent-bead field the bridge uses to pick a
// Slack channel or thread for the agent.
var SlackRoutingFields = []string{
	"slack_spawn_channel",
	FieldSlackSpawnChannelInheritedFromRole,
	"schedule_slack_channel",
	"slack_thread_channel",
	"slack_thread_ts",
}

// CopySlackRoutingFields copies each non-empty SlackRoutingFields entry from src
// into dst, so a bead with no Slack routing gains none. dst must be non-nil.
func CopySlackRoutingFields(dst, src map[string]string) {
	for _, k := range SlackRoutingFields {
		if v := src[k]; v != "" {
			dst[k] = v
		}
	}
}
