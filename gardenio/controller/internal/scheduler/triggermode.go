package scheduler

// Trigger mode names: the values of the legacy `trigger_mode` schedule field.
// Empty TriggerMode is treated as TriggerModeCron for backwards compatibility.
const (
	TriggerModeCron             = "cron"
	TriggerModeManual           = "manual"
	TriggerModeDecisionResolved = "decision_resolved"
	TriggerModeBeadClosed       = "bead_closed"
	// TriggerModeMailReceived fires when a bead with type="mail" is created.
	TriggerModeMailReceived = "mail_received"
)
