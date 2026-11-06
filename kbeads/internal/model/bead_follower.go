package model

import "time"

// BeadFollower is one row of the bead_followers join table introduced by
// migration 023 (ka-0000004114). Forwarders nominate beads to appear in
// other operators' inboxes additively — a follower row does not change the
// bead's owning `operated_by_id`.
//
// ForwardedBy is the operator who created the row; nil for system / pre-
// resolver inserts (the column is NULLABLE in schema). OperatorDisplay,
// OperatorKind, ForwardedByDisplay, and ForwardedByKind are populated only
// by ListFollowers (which JOINs the operators table); writers leave them
// empty.
type BeadFollower struct {
	BeadID             string
	OperatorID         int64
	OperatorKind       string
	OperatorSubject    string
	OperatorDisplay    string
	ForwardedBy        *int64
	ForwardedByKind    string
	ForwardedBySubject string
	ForwardedByDisplay string
	Note               string
	CreatedAt          time.Time
}
