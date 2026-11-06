package bridge

// messageTypeConfigs returns the type:message and type:permission
// registrations, merged into configs(). It lives apart from init.go to keep
// that file under the quench file-size cap.
func messageTypeConfigs() map[string]any {
	return map[string]any{
		// Keep field-consistent with the kbeads builtin type:message
		// (kbeads/internal/server/config.go), which is the source of truth. The
		// daemon merges builtin fields into a store config on resolve, but a stale
		// store row that predates a producer's new field can still 400 that write
		// until re-upserted — so declare the full producer superset here too. The
		// /plans transcript fields (plan_id/turn_role/turn_id/text_truncated/
		// full_length) were missing everywhere, silently dropping every plan-chat
		// turn (surfaced by 4b beads-viewer-e2e).
		"type:message": TypeConfig{
			Kind: "data",
			Fields: []FieldDef{
				{Name: "source_agent", Type: "string"},
				{Name: "text", Type: "string"},
				{Name: "channel", Type: "string"},
				{Name: "thread_ts", Type: "string"},
				{Name: "slack_thread_channel", Type: "string"},
				{Name: "slack_thread_ts", Type: "string"},
				{Name: "slack_user_id", Type: "string"},
				{Name: "operated_by", Type: "string"},
				{Name: "thread_id", Type: "string"},
				{Name: "plan_id", Type: "string"},
				{Name: "turn_role", Type: "string"},
				{Name: "turn_id", Type: "string"},
				{Name: "text_truncated", Type: "string"},
				{Name: "full_length", Type: "string"},
				// The transcript mint sets IdempotencyKey; createBead folds it into
				// Fields[idempotency_key], so it must be declared or the mint 400s.
				{Name: "idempotency_key", Type: "string"},
				// The Commons channel bead the message was said in (Lands spec
				// §2.3); the daemon holds such a message to its channel's land.
				{Name: "channel_id", Type: "string"},
				// What the conversation router writes (§2.3): a kind=card
				// message names the bead it renders in subject_id; slack_channel
				// / slack_ts are the Slack copy's ref, so a card updates in place.
				{Name: "kind", Type: "enum", Values: []string{"text", "card", "notice", "ask"}},
				{Name: "subject_id", Type: "string"},
				{Name: "author", Type: "string"},
				{Name: "origin", Type: "enum", Values: []string{"slack", "commons", "cli", "purse_notice", "purse_refill", "land_opened", "person_joined", "glass"}},
				{Name: "slack_channel", Type: "string"},
				{Name: "slack_ts", Type: "string"},
				// A kind=notice message's rich face beside the fallback text
				// (conversation.Message Detail / Footnote).
				{Name: "detail", Type: "string"},
				{Name: "footnote", Type: "string"},
				// A death report, crash notice or death-storm roll-up's face in
				// the Commons (agent_notices.go, internal/commons/notices.go).
				{Name: "notice", Type: "enum", Values: []string{"death", "crash", "death_storm"}},
				{Name: "notice_agent", Type: "string"},
				{Name: "notice_cause", Type: "string"},
				{Name: "notice_pod", Type: "string"},
				{Name: "notice_pod_phase", Type: "string"},
				{Name: "notice_count", Type: "string"},
				{Name: "notice_window", Type: "string"},
				{Name: "notice_dead", Type: "string"},
				// The agent bead a death or crash is about; subject_id is
				// card-only (gd-JGmXQuSndB).
				{Name: "notice_subject", Type: "string"},
				// Attachments as links (conversation.BeadFiles).
				{Name: "files", Type: "json"},
				// A Slack message's file summary (slackFilesToFields): the
				// Slack mirror and thread forward write it.
				{Name: "slack_attachment_count", Type: "string"},
				{Name: "slack_has_images", Type: "string"},
				// A farm snapshot's context beside its picture (the /garden
				// Snap button, internal/commons/snap.go).
				{Name: "snap", Type: "json"},
				// A DM's target: the principal a gb say --to was sent to
				// (api_conversation_dm.go).
				{Name: "to", Type: "string"},
				// When the bridge dispatched a person's Commons message
				// (inbound_reply.go), so the sweep never runs it twice.
				{Name: "handled_at", Type: "string"},
				// A chat root's wish head once a mention promoted it
				// (inbound_compose.go).
				{Name: "promoted_to", Type: "string"},
				// The client nonce a Commons say or reply was sent with, so a
				// repeated send is dropped (internal/commons/sendonce.go).
				{Name: "nonce", Type: "string"},
				// The bead a Commons message asks about (the workbench's "Ask the
				// steward about this", internal/commons/write.go).
				{Name: "about", Type: "string"},
			},
		},
		// A tool-permission request, the subject of a Commons permission card
		// (decision_cards.go). Keep field-consistent with the kbeads builtin
		// type:permission (kbeads/internal/server/config.go), the source of truth.
		"type:permission": TypeConfig{
			Kind: "data",
			Fields: []FieldDef{
				{Name: "agent", Type: "string", Required: true},
				{Name: "agent_bead_id", Type: "string"},
				{Name: "request_id", Type: "string", Required: true},
				{Name: "tool_name", Type: "string", Required: true},
				{Name: "description", Type: "string"},
				{Name: "input_preview", Type: "string"},
				{Name: "behavior", Type: "enum", Values: []string{"allow", "deny"}},
				{Name: "answered_by", Type: "string"},
				{Name: "answered_by_slack_id", Type: "string"},
				{Name: "answered_on", Type: "string"},
				{Name: "answer_label", Type: "string"},
				{Name: "verdict_error", Type: "string"},
			},
		},
	}
}
