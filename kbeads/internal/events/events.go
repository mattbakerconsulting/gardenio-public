package events

import (
	"context"
	"encoding/json"

	"gardenio/kbeads/internal/model"
)

// Event topic constants
const (
	TopicBeadCreated       = "beads.bead.created"
	TopicBeadUpdated       = "beads.bead.updated"
	TopicBeadClosed        = "beads.bead.closed"
	TopicBeadDeleted       = "beads.bead.deleted"
	TopicDependencyAdded   = "beads.dependency.added"
	TopicDependencyRemoved = "beads.dependency.removed"
	TopicLabelAdded        = "beads.label.added"
	TopicLabelRemoved      = "beads.label.removed"
	TopicCommentAdded      = "beads.comment.added"
	TopicCommentDeleted    = "beads.comment.deleted"
	TopicFollowerAdded     = "beads.follower.added"
	TopicFollowerRemoved   = "beads.follower.removed"

	// Advice events
	TopicAdviceCreated = "beads.advice.created"
	TopicAdviceUpdated = "beads.advice.updated"
	TopicAdviceDeleted = "beads.advice.deleted"

	// Session lifecycle events (emitted by agents, consumed by hooks handler).
	TopicSessionEnd       = "beads.session.end"
	TopicSessionPreCommit = "beads.session.pre_commit"
	TopicSessionPrePush   = "beads.session.pre_push"
	TopicSessionHandoff   = "beads.session.handoff"

	// Fleet events
	TopicBeadTransferred = "beads.bead.transferred"

	// Plan events. TopicPlanStepAdded fires when a new step (a non-epic child)
	// is linked under a /plans plan epic — or under one of its phase sub-epics.
	// It is a DISTINGUISHED, self-describing signal: unlike the generic
	// TopicDependencyAdded (which fires on EVERY parent-child edge in the fleet
	// and forces a consumer to re-walk the graph to decide plan membership),
	// this event carries the resolved plan id, phase id, and step metadata so
	// the controller's reactive-collaboration reactor can debounce and
	// nudge/wake/spawn without a round-trip. The daemon resolves plan membership
	// once at the emit site (server-side, fail-closed) and only emits when the
	// edge is genuinely a plan step. Producer half of P3 (ka-0000004264, epic
	// ka-0000002170); the controller reactor is the consumer (ka-0000002739).
	//
	// Durability: this event is ALSO published to the durable JetStream
	// PLAN_EVENTS stream (internal/eventbus) with a deterministic Nats-Msg-Id so
	// a step edge survives a consumer restart (at-least-once) and a duplicate
	// re-emit (double-write, reconnect replay) is collapsed by the stream's
	// dedupe window. The core-NATS Publish here remains the fire-and-forget live
	// fast-path; the JetStream copy is the durable backbone.
	TopicPlanStepAdded = "beads.plan.step.added"

	// Mail events. TopicMailSent fires when a bead with type="mail" is
	// created. The controller's scheduler subscribes to this subject for
	// trigger_mode=mail_received schedules. Payload mirrors the bead's
	// title/assignee/created_by plus a derived channel taken from a
	// `channel:<name>` label when present (otherwise empty).
	TopicMailSent = "beads.mail.sent"

	// Jack events
	TopicJackOn       = "beads.jack.on"
	TopicJackOff      = "beads.jack.off"
	TopicJackExtended = "beads.jack.extended"
	TopicJackExpired  = "beads.jack.expired"

	// Taint events (docs/adr/taints.md §3/§7)
	TopicTaintGranted = "beads.taint.granted"
	TopicTaintRevoked = "beads.taint.revoked"
	TopicTaintApplied = "beads.taint.applied"
)

// Event types

type BeadCreated struct {
	Bead *model.Bead `json:"bead"`
}

type BeadUpdated struct {
	Bead    *model.Bead    `json:"bead"`
	Changes map[string]any `json:"changes"` // field name -> new value
	// Actor is the server-resolved subject that performed the update (agent
	// name for X-Actor static-token writes, email for OIDC) — the same value
	// recordUpdates stamps on mutation_history rows. Lets live subscribers
	// (e.g. the beads-viewer plan-narrative stream, Plans v1 S3
	// ka-0000002473) attribute an edit without a second lookup. Empty when
	// auth is disabled or the caller is anonymous.
	Actor string `json:"actor,omitempty"`
	// FieldsBefore is the bead's Fields blob before the update, when the
	// emitter has it. It is never marshaled, so live subscribers see the
	// same payload as before. The daemon uses it to persist a
	// changed-keys-only delta of a trigger or agent update in the events
	// table instead of the whole blob (gd-1F8R0SQtiQ).
	FieldsBefore json.RawMessage `json:"-"`
}

type BeadClosed struct {
	Bead     *model.Bead `json:"bead"`
	ClosedBy string      `json:"closed_by,omitempty"`
	// DecisionAnswer is populated when a decision-type bead is resolved or
	// cancelled so that `trigger:bead` watchers can match on the chosen
	// option without rehydrating the bead's Fields blob. nil for non-decision
	// closes. See GAP-4 / ka-0000005470.
	DecisionAnswer *DecisionAnswer `json:"decision_answer,omitempty"`
}

// DecisionAnswer carries the operator's response payload for a resolved or
// cancelled decision bead. Mirrors the keys merged into bead.Fields by
// handleResolveDecision / handleCancelDecision so subscribers see one
// canonical shape.
type DecisionAnswer struct {
	Chosen       string `json:"chosen,omitempty"`
	ResponseText string `json:"response_text,omitempty"`
	RespondedBy  string `json:"responded_by,omitempty"`
	RespondedAt  string `json:"responded_at,omitempty"`
	Cancelled    bool   `json:"cancelled,omitempty"`
}

type BeadDeleted struct {
	BeadID string `json:"bead_id"`
	Fleet  string `json:"fleet,omitempty"`
}

type DependencyAdded struct {
	Dependency *model.Dependency `json:"dependency"`
	Fleet      string            `json:"fleet,omitempty"`
}

type DependencyRemoved struct {
	BeadID      string `json:"bead_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	Fleet       string `json:"fleet,omitempty"`
}

type LabelAdded struct {
	BeadID string `json:"bead_id"`
	Label  string `json:"label"`
	Fleet  string `json:"fleet,omitempty"`
}

type LabelRemoved struct {
	BeadID string `json:"bead_id"`
	Label  string `json:"label"`
	Fleet  string `json:"fleet,omitempty"`
}

type CommentAdded struct {
	Comment *model.Comment `json:"comment"`
	Fleet   string         `json:"fleet,omitempty"`
}

// CommentDeleted fires when a comment row is removed via DeleteComment or
// TrimComments. CommentID is the deleted row's id; Author preserves the
// original comment's author so subscribers can reason about who wrote it
// without rehydrating the (now-gone) row. Reason is "delete" for a single-id
// delete and "trim" when the deletion came from a trim sweep.
type CommentDeleted struct {
	BeadID    string `json:"bead_id"`
	CommentID int64  `json:"comment_id"`
	Author    string `json:"author,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Fleet     string `json:"fleet,omitempty"`
}

// FollowerAdded fires when a bead is forwarded to an operator (ka-0000004114).
// ForwardedBy is *int64 because the bead_followers.forwarded_by column is
// NULLABLE — system / pre-resolver inserts may have no operator-of-record.
type FollowerAdded struct {
	BeadID      string `json:"bead_id"`
	OperatorID  int64  `json:"operator_id"`
	ForwardedBy *int64 `json:"forwarded_by,omitempty"`
	Note        string `json:"note,omitempty"`
	Fleet       string `json:"fleet,omitempty"`
}

type FollowerRemoved struct {
	BeadID     string `json:"bead_id"`
	OperatorID int64  `json:"operator_id"`
	Fleet      string `json:"fleet,omitempty"`
}

// Fleet events

type BeadTransferred struct {
	Bead      *model.Bead `json:"bead"`
	FromFleet string      `json:"from_fleet"`
	ToFleet   string      `json:"to_fleet"`
}

// Plan events

// PlanStepAdded is published when a new step is linked under a plan epic (or
// one of its phase sub-epics). It is the first-class, self-describing signal the
// controller's reactive-collaboration reactor consumes to debounce and
// nudge/wake/spawn the plan's live agent (P3, epic ka-0000002170). Every field
// is resolved server-side at the emit chokepoint so the consumer never re-walks
// the graph:
//
//   - PlanID     the plan epic the step belongs to (the debounce/nudge key).
//   - PhaseID    the phase sub-epic the step was added under, or "" for a flat
//     step parented directly to the plan.
//   - StepID     the newly-linked step bead.
//   - StepTitle  the step's title — seeds the collaboration message.
//   - StepType   the step bead's type (normally "task"); carried so a consumer
//     can distinguish a real step from an unusual child without a
//     fetch.
//   - Operator   the resolved operator identity the daemon stamped on the edge
//     (oidc:<email> / slack:U…), so the consumer's nudge/wake writes
//     are attributed identically to a browser add. Falls back to the
//     edge's created_by when no operator resolved.
//   - Fleet      audit-attribution fleet of the write.
//
// The event is emitted ONLY when the daemon resolves the new parent-child edge
// to a genuine plan step (fail-closed membership: parent is a plan epic, or a
// phase listed in a plan's phase_order). A stray epic-under-epic or a
// non-parent-child edge never produces one.
type PlanStepAdded struct {
	PlanID    string `json:"plan_id"`
	PhaseID   string `json:"phase_id,omitempty"`
	StepID    string `json:"step_id"`
	StepTitle string `json:"step_title,omitempty"`
	StepType  string `json:"step_type,omitempty"`
	Operator  string `json:"operator,omitempty"`
	Fleet     string `json:"fleet,omitempty"`
}

// Mail events

// MailSent is published when a mail-type bead is created. From defaults to
// the bead's CreatedBy; To defaults to the Assignee. Channel is non-empty
// only when the mail bead carries a `channel:<name>` label (used by Slack
// trigger watchers and by humans tagging conversational threads).
type MailSent struct {
	BeadID  string `json:"bead_id"`
	Channel string `json:"channel,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// Advice events

type AdviceCreated struct {
	Bead *model.Bead `json:"bead"`
}

type AdviceUpdated struct {
	Bead    *model.Bead    `json:"bead"`
	Changes map[string]any `json:"changes"`
}

type AdviceDeleted struct {
	BeadID string `json:"bead_id"`
	Fleet  string `json:"fleet,omitempty"`
}

// Jack events

type JackOn struct {
	Bead *model.Bead `json:"bead"`
}

type JackOff struct {
	Bead   *model.Bead `json:"bead"`
	Reason string      `json:"reason"`
}

type JackExtended struct {
	Bead *model.Bead `json:"bead"`
	TTL  string      `json:"ttl"`
}

type JackExpired struct {
	BeadID string `json:"bead_id"`
	Target string `json:"target"`
	Fleet  string `json:"fleet,omitempty"`
}

// Taint events (docs/adr/taints.md §3/§7)

type TaintGranted struct {
	TaintID       int64  `json:"taint_id"`
	TaintName     string `json:"taint_name"`
	PrincipalKind string `json:"principal_kind"`
	Principal     string `json:"principal"`
	GrantedBy     int64  `json:"granted_by"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	Fleet         string `json:"fleet,omitempty"`
}

type TaintRevoked struct {
	TaintID       int64  `json:"taint_id"`
	TaintName     string `json:"taint_name"`
	PrincipalKind string `json:"principal_kind"`
	Principal     string `json:"principal"`
	Fleet         string `json:"fleet,omitempty"`
}

type TaintApplied struct {
	BeadID    string `json:"bead_id"`
	TaintID   int64  `json:"taint_id"`
	TaintName string `json:"taint_name"`
	AppliedBy string `json:"applied_by"`
	Fleet     string `json:"fleet,omitempty"`
}

// Publisher is the interface for emitting events.
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
	Close() error
}
