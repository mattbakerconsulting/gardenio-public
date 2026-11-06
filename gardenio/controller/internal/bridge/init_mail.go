package bridge

// mailTypeConfigs returns the type:mail and type:dm registrations, merged
// into configs(). They live apart from init.go to keep that file under the
// quench file-size cap.
func mailTypeConfigs() map[string]any {
	return map[string]any{
		"type:mail": TypeConfig{
			Kind: "data",
			Fields: []FieldDef{
				{Name: "source_agent", Type: "string"},
				{Name: "decision_bead_id", Type: "string"},
				// Stamped by the recipient's mailsub when coop takes the mail
				// (mailproto.FieldDeliveredAt); watchMailDelivery reads it.
				// Mirrors the kbeads builtin.
				{Name: "delivered_at", Type: "string"},
				// A control:permission mail's Slack verdict
				// (mailproto.FieldPermissionRequestID/FieldPermissionBehavior,
				// sent by MailPermissionVerdict). Mirrors the kbeads builtin.
				{Name: "request_id", Type: "string"},
				{Name: "behavior", Type: "string"},
			},
		},
		"type:dm": TypeConfig{
			Kind: "data",
			Fields: []FieldDef{
				{Name: "target_user", Type: "string", Required: true},
				{Name: "source_agent", Type: "string"},
				{Name: "subject", Type: "string"},
				{Name: "body", Type: "string"},
				{Name: "dm_channel", Type: "string"},
				{Name: "dm_ts", Type: "string"},
				{Name: "dm_mode", Type: "string"},
				{Name: "task_context", Type: "string"},
				{Name: "timeout_minutes", Type: "integer"},
				{Name: "reminder_sent", Type: "boolean"},
				{Name: "replied", Type: "boolean"},
				{Name: "reply_text", Type: "string"},
				{Name: "reply_ts", Type: "string"},
				{Name: "reply_user", Type: "string"},
				// Review mode fields — set when dm_mode=review.
				{Name: "mr_url", Type: "string"},
				{Name: "mr_title", Type: "string"},
				{Name: "mr_pipeline_status", Type: "string"},
			},
		},
	}
}
