package model

import "time"

// OperatorHistoryRow is one entry from the bead_operator_history VIEW
// (kbeads/internal/store/postgres/migrations/014_operated_by_dibs.up.sql).
//
// The VIEW is a named projection over mutation_history filtered to rows
// where field='operated_by'. It powers the beads-viewer "ownership" tab
// (R5 §2 row S4) and any future `kd ownership` command — every reader
// targets one stable shape so the source of truth stays in one place.
//
// SourceKind is the classification the VIEW applies to each transition,
// per R4 §4 Option C:
//
//	"create"            — bead was created with this operator stamp.
//	"refresh"           — operated_by was re-asserted to its existing value.
//	"sentinel-attempt"  — system:* / fleet:* operator (transparent, never
//	                       takes dibs).
//	"unknown-attempt"   — empty operator id (pre-cutover / unresolved).
//	"dibs-flip"         — a real human operator transition.
type OperatorHistoryRow struct {
	ID             int64     `json:"id"`
	BeadID         string    `json:"bead_id"`
	OperatorID     string    `json:"operator_id"`
	BecameOwnerAt  time.Time `json:"became_owner_at"`
	PrevOperator   string    `json:"prev_operator"`
	Actor          string    `json:"actor"`
	SourceKind     string    `json:"source_kind"`
}
