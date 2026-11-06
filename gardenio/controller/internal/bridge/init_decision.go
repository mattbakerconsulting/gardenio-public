package bridge

// decisionTypeConfigs returns the type:decision registration, merged into
// configs(). It lives apart from init.go to keep that file under the quench
// file-size cap.
func decisionTypeConfigs() map[string]any {
	return map[string]any{
		"type:decision": TypeConfig{
			Kind: "data",
			Fields: []FieldDef{
				{Name: "prompt", Type: "string", Required: true},
				{Name: "options", Type: "json"},
				{Name: "chosen", Type: "string"},
				{Name: "rationale", Type: "string"},
				{Name: "session", Type: "string"},
				{Name: "context", Type: "string"},
				{Name: "requested_by", Type: "string"},
				{Name: "requesting_agent_bead_id", Type: "string"},
				// Server-stamped true when requesting_agent_bead_id came from the
				// filer's own per-agent credential (kbeads decision create guard).
				{Name: "requesting_agent_verified", Type: "string"},
				// Comma-separated bead ids `gb decision create --evidence`
				// stamps; surfaces show their screenshots alongside the
				// requesting task's (decisionview.EvidenceBeads).
				{Name: "evidence", Type: "string"},
				{Name: "responded_by", Type: "string"},
				{Name: "responded_at", Type: "string"},
				{Name: "response_text", Type: "string"},
				{Name: "required_artifact", Type: "string"},
				// artifact_status lifecycle: "pending" is stamped when a
				// decision resolves onto an option that owes an artifact
				// (bot_decisions_modal.go, decision.go create path);
				// "submitted" is advanced by `gb decision report` once the
				// report bead is wired + closed (decision.go report path);
				// "accepted" is reserved for a future operator-acceptance
				// transition and is intentionally not yet written by any path.
				// Keep field-for-field consistent with the kbeads builtin
				// (kbeads/internal/server/config.go "type:decision").
				{Name: "artifact_status", Type: "enum", Values: []string{"pending", "submitted", "accepted"}},
			},
		},
	}
}
