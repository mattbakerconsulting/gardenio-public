package model

import "time"

// GateRow represents a session gate state row.
type GateRow struct {
	AgentBeadID string
	GateID      string
	Status      string // "pending" | "satisfied"
	SatisfiedAt *time.Time
}
