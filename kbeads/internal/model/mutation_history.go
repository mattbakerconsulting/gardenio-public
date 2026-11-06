package model

import "time"

// Mutation type constants used in the mutation_history.mutation_type column.
const (
	MutationTypeCreate  = "create"
	MutationTypeUpdate  = "update"
	MutationTypeDelete  = "delete"
	MutationTypeRestore = "restore"
)

// MutationRow is a single append-only entry in the mutation_history table.
// Each row records one field-level change to a bead.
type MutationRow struct {
	ID           int64     `json:"id"`
	BeadID       string    `json:"bead_id"`
	Actor        string    `json:"actor"`
	Field        string    `json:"field"`
	OldValue     string    `json:"old_value"`
	NewValue     string    `json:"new_value"`
	MutationType string    `json:"mutation_type"`
	CreatedAt    time.Time `json:"created_at"`
	OperatedBy   string    `json:"operated_by,omitempty"`
	// OperatedByID is the operators(id) FK populated by the resolver during
	// the dual-write window (ADR §4.4). nil when the resolver did not match.
	OperatedByID *int64 `json:"operated_by_id,omitempty"`
}
