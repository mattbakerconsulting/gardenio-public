package model

import (
	"encoding/json"
	"time"
)

// Event is a persisted event record, mirroring what is published to NATS.
type Event struct {
	ID         int64           `json:"id"`
	Topic      string          `json:"topic"`
	BeadID     string          `json:"bead_id"`
	Actor      string          `json:"actor,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
	OperatedBy string          `json:"operated_by,omitempty"`
	// OperatedByID is the operators(id) FK populated by the resolver during
	// the dual-write window (ADR §4.4). nil when the resolver did not match.
	OperatedByID *int64 `json:"operated_by_id,omitempty"`
}

// EventHistogramBucket is a single time-bucket count from the events table,
// used by the sequencer heatmap (GET /v1/events/histogram).
type EventHistogramBucket struct {
	T     time.Time `json:"t"`
	Count int64     `json:"count"`
}

// EventReadScope carries the caller's project-visibility predicates into the
// global event-aggregation store queries (GetEventsByTimeRange[Desc],
// EventHistogram). Events are keyed by bead_id, not by labels, so the store
// query must JOIN through the bead's labels to apply these predicates —
// there is no label column on the events table to filter on directly.
//
// This is the events-feed analogue of the label scoping ListBeads applies via
// BeadFilter.Labels / BeadFilter.LabelsAny, and mirrors blockerReadScope in the
// postgres package. It closes the cross-project read-authz gap on the two
// global event surfaces (ka-0000006863): without it, a project-scoped OIDC
// viewer or project-bound fleet token could read event payloads — which embed
// the FULL bead for create/update/close topics — for projects it is not
// authorized to read.
//
//   - RequireAllLabels: the event's bead must carry EVERY label (project-bound
//     fleet token's "project:<name>" label — ANDed in).
//   - RequireAnyLabels: the event's bead must carry AT LEAST ONE label (an OIDC
//     user's allowed "project:<name>" labels — ORed in, narrowing never widening).
//
// The zero value (both nil/empty) is an unscoped caller (static/primary token,
// admin, wildcard, or unrestricted OIDC user) and leaves the query unrestricted,
// preserving existing cross-project read access for those callers.
//
// TaintGrantIDs carries the taint read gate's THIRD, narrower predicate (ADR
// docs/adr/taints.md §4.3/§5.1): events carry no taint columns of their own,
// so the store query joins through bead_taints on events.bead_id the same way
// it joins through labels for RequireAllLabels/RequireAnyLabels above. Nil
// means the gate does not apply (exempt caller, or an empty taints registry);
// non-nil-but-empty means the caller holds zero grants, excluding every event
// for a tainted bead. See BeadFilter.TaintGrantIDs for the identical contract.
//
// ExcludeBeadTypes drops events whose bead has one of the listed types. It is
// a caller-requested content filter (GET /v1/events?exclude_types=): the /feed
// seed uses it to keep bookkeeping beads (trigger, agent, ...) out of its
// bounded scan (gd-Mfr8MRXbYG). The server also adds the types a tenant caller
// may not read (account, gd-q8jegjvKI3), so it is an authorization predicate
// too, and a caller-requested list may only add to it.
type EventReadScope struct {
	RequireAllLabels []string
	RequireAnyLabels []string
	TaintGrantIDs    *[]int64
	ExcludeBeadTypes []string
}

// IsUnrestricted reports whether the scope imposes no project, taint or
// bead-type predicate at all.
func (s EventReadScope) IsUnrestricted() bool {
	return len(s.RequireAllLabels) == 0 && len(s.RequireAnyLabels) == 0 && s.TaintGrantIDs == nil && len(s.ExcludeBeadTypes) == 0
}
