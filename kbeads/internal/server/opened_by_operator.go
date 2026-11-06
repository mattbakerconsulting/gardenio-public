package server

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// openedByOperatorField names the operator who opened the thread a task was
// filed in (local-agents spec §7 rule 2). gb ready on a local runner keeps an
// unplaced task only when this equals the runner's owner (filterForRunner,
// gardenio/controller/cmd/gb/ready.go). Declared with placementFields.
const openedByOperatorField = "opened_by_operator"

// openedByOperatorTypes are the types that declare opened_by_operator: the
// leaf-work types gb ready lists (the placementFields set in config.go).
var openedByOperatorTypes = map[model.BeadType]bool{
	"task": true, "feature": true, "chore": true, "bug": true,
}

// maybeStampOpenedByOperator stamps fields.opened_by_operator on a freshly
// created leaf-work bead that belongs to a Slack thread. The value is the
// thread head's fields.operated_by: the bridge resolves the head's
// slack_opener_user_id to oidc:<email> when it mints the head
// (getOrCreateThreadBead, bridge/thread_bead.go), so the daemon reuses that
// resolution instead of needing a Slack directory of its own.
//
// It runs after maybeCopyThreadID, so it sees a thread_id the caller set or the
// daemon just copied. Best-effort and copy-if-empty, like that copy: a head with
// no opener, an unresolved opener (slack:U… or fleet:…), a non-head thread_id
// or a store failure leaves the field unset, so a runner simply does not list
// the task.
func (s *BeadsServer) maybeStampOpenedByOperator(ctx context.Context, bead *model.Bead, now time.Time) {
	if bead == nil || s == nil || s.store == nil || !openedByOperatorTypes[bead.Type] {
		return
	}
	if hasLabel(bead.Labels, threadHeadLabel) {
		return
	}
	if v, _, _ := exactFieldString(bead.Fields, openedByOperatorField); v != "" {
		return
	}
	wish := threadIDFromFields(bead.Fields)
	if wish == "" || wish == bead.ID {
		return
	}
	head, err := s.store.GetBead(ctx, wish)
	if err != nil || head == nil || !hasLabel(head.Labels, threadHeadLabel) {
		return
	}
	if opener, _, _ := exactFieldString(head.Fields, "slack_opener_user_id"); opener == "" {
		return
	}
	op, _, _ := exactFieldString(head.Fields, "operated_by")
	if !strings.HasPrefix(op, "oidc:") || len(op) == len("oidc:") {
		return
	}

	updated := *bead
	updated.Fields = setField(bead.Fields, openedByOperatorField, op)
	updated.UpdatedAt = now
	if err := s.store.UpdateBead(ctx, &updated); err != nil {
		slog.Warn("opened_by_operator stamp failed", "bead_id", bead.ID, "thread_id", wish, "err", err.Error())
		return
	}
	bead.Fields = updated.Fields
	bead.UpdatedAt = updated.UpdatedAt

	s.recordAndPublish(ctx, events.TopicBeadUpdated, bead.ID, bead.CreatedBy, events.BeadUpdated{
		Bead:    bead,
		Changes: map[string]any{"fields." + openedByOperatorField: op},
	})
	s.recordMutation(ctx, bead.ID, bead.CreatedBy, "fields."+openedByOperatorField, "", op, model.MutationTypeUpdate)
}
