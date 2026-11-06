package model

import "time"

// Comment represents a comment on a bead.
type Comment struct {
	ID         int64     `json:"id"`
	BeadID     string    `json:"bead_id"`
	Author     string    `json:"author"`
	Text       string    `json:"text"`
	CreatedAt  time.Time `json:"created_at"`
	OperatedBy string    `json:"operated_by,omitempty"`
	// OperatedByID is the operators(id) FK populated by the resolver during
	// the dual-write window (ADR §4.4). nil when the resolver did not match.
	OperatedByID *int64 `json:"operated_by_id,omitempty"`
	// OperatedByDisplay / OperatedByEmail are resolved server-side from the
	// operators table via LEFT JOIN on operated_by_id. Read-only.
	OperatedByDisplay string `json:"operated_by_display,omitempty"`
	OperatedByEmail   string `json:"operated_by_email,omitempty"`
	// ImpersonatedBy is the admin behind the impersonated session that wrote
	// the comment, stamped by the server from the verified token (kbeads
	// impersonation.go); empty for every other comment.
	ImpersonatedBy string `json:"impersonated_by,omitempty"`
}
