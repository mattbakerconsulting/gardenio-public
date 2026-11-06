package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// threadHeadLabel is the bare, origin-agnostic label stamped on every thread
// (= user "wish") head bead. Mirrors threadKindLabel in
// gardenio/controller/internal/bridge/thread_bead.go. The daemon keeps its own
// copy so the auto-parent step can recognize a wish head without importing the
// controller package. A head IS the wish, so it must never inherit a
// thread_id field pointing at some other wish.
const threadHeadLabel = "thread"

// autoParentSkipTypes are the bead TYPES the server-side auto-parent step must
// NOT wire a parent-child edge for (bug ka-0000002199). These are pure
// machine-bookkeeping beads that a gardener/crew agent mints as a side effect of
// running — an agent registration bead (type=agent), a per-run session bead
// (type=session), a squawk/notify bead (type=message), a mail bead
// (type=mail) and an advice bead (type=advice). They are NOT units of work.
//
// The invariant (advice ka-0000002123), stated the way this list must be read:
// a non-work bookkeeping bead a gardener mints while running is NEVER a
// parent-child child of its goal epic. type=agent/session/message/mail/advice
// are the currently-known offenders; a NEW offending type is a list-extension, not a
// surprise — assert the invariant, not the enumeration.
//
// The trap they cause: a gardener whose own task_id IS a goal-bearing epic
// (assign_bead = the epic) creates these beads while it runs; maybeAddAutoParent
// then reads the gardener's task_id and parents each one under the epic. Because
// none of them close on the WORK lifecycle (an agent bead flips to
// agent_state=done on despawn, a session bead closes on its own cadence, and a
// message/mail bead auto-closes on the comms cadence — none track the epic's
// backlog), an epic whose gardener's terminal_goal is "all children closed"
// becomes UNREACHABLE by construction: every spawn/squawk manufactures a fresh
// non-issue child of the goal. The gardener loop can never go dormant and the
// open-children preflight fires forever on a drained epic. It also scales with
// dispatch AND comms volume — every squawk from a gardener whose task_id is a
// goal-bearing epic mints a fresh non-issue child (bug ka-0000004462: 41 message
// children already on one epic, plus 6 mail — a green regression test scoped to
// only agent/session certified the invariant held while message/mail leaked).
//
// Why TYPE, not kind!=issue: the auto-parent step was BUILT for kind=data beads
// too — a type=decision (kind=data) clarifying question MUST parent under its
// plan epic so the cockpit's blocked_on_decision walk finds it (bug
// ka-0000005744), and a type=report/bundle deliverable parents under its
// originating task. Gating on kind!=issue would silently kill both. The
// offenders are specifically the agent-lifecycle + comms bookkeeping types, so
// the skip is scoped to them by type.
//
// The legitimate linkage each keeps is untouched: an agent bead is tied to its
// task by the `assigned` edge + fields.task_id (SpawnAgent, client.go), a
// session bead by the `session_of` edge to its agent bead (RegisterSession,
// client_session.go), and a message/mail bead by its `thread_id` field +
// slack-thread labels (thread_bead.go / comment_forward.go) — none of which this
// parent-child skip touches. gb crew / gb news / the gardeners page / owned-epic
// rollups / the Slack thread forwarders all resolve through those, not through
// this parent-child edge — so the bead stays fully traceable without polluting
// the epic child set.
var autoParentSkipTypes = map[model.BeadType]bool{
	model.BeadType("agent"):   true,
	model.BeadType("session"): true,
	model.BeadType("message"): true,
	model.BeadType("mail"):    true,
	// Advice is standing guidance matched by labels, not work: it never closes
	// on the task lifecycle, and updating it (dep remove included) needs
	// gardenio-admin, so a crew that ran `gb advice add` could not undo the
	// edge (gd-NoMRNVzAkp).
	model.BeadType("advice"): true,
}

// maybeAddAutoParent wires a parent-child dep from the freshly-created bead to
// the agent's current task_id when the caller is an agent actor and did not opt
// out via X-No-Auto-Parent. Errors are non-fatal — logged and swallowed — so
// the bead create itself is never undone by a missing/invalid task_id.
//
// Called from createBead (internal/server/beads.go) for the static-actor
// auto-parent step (ka-0000007224); tests live in auto_parent_test.go.
//
// IDENTITY GATE (Guard 1, ka-0000007300). Both the auto-parent dep and the
// transitive thread_id copy fire ONLY for id.Type=="static" — the agent
// shared-token path, where the daemon must infer lineage from the acting
// agent bead because the client did not stamp it. Other CreateBead callers
// bypass this step entirely by identity, by design:
//
//	| Caller identity (AuthIdentity.Type) | Examples                                   | Auto-parent? | Rationale                                                              |
//	|-------------------------------------|--------------------------------------------|--------------|------------------------------------------------------------------------|
//	| static                              | agent pods sharing the primary token       | YES          | the only path with no per-call author; daemon infers from agent bead   |
//	| oidc                                | human via beads-viewer; Phase-2 composer   | NO           | the composer stamps thread_id itself on the new-wish/derivative write  |
//	| fleet                               | slack/jira/gitlab bridges (incl Privileged)| NO           | bridges set lineage explicitly (thread_bead.go, gardener_chain_spawn.go)|
//	| edge                                | gardenio-edge-auth HMAC envelope            | NO           | not an agent actor                                                     |
//	| none                                | unauthenticated (rejected upstream)        | NO           | never reaches createBead                                              |
//
// The OIDC/fleet bypass is load-bearing: the Phase-2 composer (task 2-2,
// ka-0000005643) writes thread_id onto the bead it mints, so a second
// daemon-side copy keyed off a non-existent acting-agent bead would be both
// redundant and wrong.
func (s *BeadsServer) maybeAddAutoParent(ctx context.Context, bead *model.Bead, now time.Time) {
	if bead == nil || s == nil || s.store == nil {
		return
	}
	if noAutoParentFromContext(ctx) {
		return
	}
	subject, ok := actingAgentRef(ctx)
	if !ok {
		return
	}
	// Skip agent-lifecycle + comms bookkeeping beads (type=agent / session /
	// message / mail): they are not work and must never join a goal-bearing
	// epic's child set, or its gardener's "all children closed" goal becomes
	// unreachable (bug ka-0000002199, ka-0000004462; advice ka-0000002123).
	// Their real linkage — the `assigned` / `session_of` edges, the `thread_id`
	// field + slack-thread labels — is written elsewhere and is what every
	// consumer reads. See autoParentSkipTypes for the full rationale.
	if autoParentSkipTypes[bead.Type] {
		return
	}
	taskID := s.autoParentFromAgentBead(ctx, subject).DepTarget
	if taskID == "" || taskID == bead.ID {
		return
	}
	// Post-cutover (migration 022): deps.operated_by_id is NOT NULL.
	// Inherit the FK from the parent bead so the auto-parent insert does
	// not violate the constraint (ka-0000007722). bead.OperatedByID is
	// guaranteed non-nil here because the resolver upstream returns ok=false
	// on a missing FK, which short-circuits createBead before reaching the
	// auto-parent step.
	dep := &model.Dependency{
		BeadID:       bead.ID,
		DependsOnID:  taskID,
		Type:         model.DepParentChild,
		CreatedAt:    now,
		CreatedBy:    bead.CreatedBy,
		OperatedBy:   bead.OperatedBy,
		OperatedByID: bead.OperatedByID,
	}
	// Data-hygiene guard (ka-0000006880): the auto-parent edge always puts
	// the freshly-created bead on the child (bead_id) side. If that bead is
	// itself an epic and the agent's task is a non-epic, we'd be auto-slotting
	// an epic as a child of a task — the reversed shape that breaks the
	// subtree CTE. An epic should essentially never be the bead_id (child)
	// side of a parent-child edge, so SKIP rather than swap here: the agent's
	// task is not necessarily the epic's true parent, and swapping would wire
	// the task under the epic. Explicit parentage can still be set later.
	if s.epicOnChildSide(ctx, dep) {
		slog.Warn("skipping auto-parent: new bead is an epic (would reverse subtree edge)",
			"epic", bead.ID,
			"task_id", taskID,
			"agent", subject,
			"bead", "ka-0000006880",
		)
		return
	}
	// Closed-parent guard (ka-0000005756): the agent's task_id resolved to an
	// already-CLOSED epic. Silently grafting new open work under a dead epic
	// makes it invisible to the epic's lifecycle — the auto-close cascade skips
	// closed parents, so this child can never drain and sits claimable in
	// `gb ready` indefinitely. SKIP + warn rather than swap: the agent's task
	// is not necessarily the right live parent, so leave the bead an orphan
	// (which the groomer can re-lane) instead of wiring it under a corpse.
	if parent := s.closedParentSide(ctx, dep); parent != nil {
		slog.Warn("skipping auto-parent: agent task_id resolves to a CLOSED epic",
			"child", bead.ID,
			"closed_parent", parent.ID,
			"parent_closed_at", closedAtString(parent),
			"agent", subject,
			"bead", "ka-0000005756",
		)
		return
	}
	if err := s.store.AddDependency(ctx, dep); err != nil {
		slog.Warn("auto-parent dep insert failed",
			"bead_id", bead.ID,
			"task_id", taskID,
			"agent", subject,
			"err", err.Error(),
		)
		return
	}
	s.recordAndPublish(ctx, events.TopicDependencyAdded, bead.ID, bead.CreatedBy, events.DependencyAdded{
		Dependency: dep,
		Fleet:      FleetFromContext(ctx),
	})
}

// maybeCopyThreadID propagates the acting agent's carried wish pointer
// (fields.thread_id) onto a freshly-created bead so thread membership flows
// transitively: a derivative bead inherits thread_id, an agent spawned on that
// derivative carries it forward, and the grandchild inherits in turn
// (ka-0000007300). This is a NET-NEW field mutation, separate from the
// parent-child dep written by maybeAddAutoParent — the two are decoupled so a
// field-copy failure never undoes the durable dep (and vice versa). The copy
// is best-effort: every failure path logs and returns without disturbing the
// already-persisted bead.
//
// explicitParent is the parent-child edge the caller passed at create time
// (nil when none). It is the cross-wish guard input: if it resolves to a wish
// different from the agent's carried thread_id, the new bead belongs to the
// EXPLICIT parent's wish, not the agent's, so the copy is skipped.
//
// Guard 1 (identity): same static-only gate + X-No-Auto-Parent opt-out as
// maybeAddAutoParent — the suppress header turns off BOTH the dep and the copy.
//
// Guard 2 (cross-wish mis-bind): when the new bead has an explicit parent that
// resolves to a different wish than the agent's thread_id, WARN and SKIP. An
// agent working wish A that files a bead explicitly parented under a bead in
// wish B must not stamp wish A onto it.
//
// Copy-if-empty: a bead that already carries a thread_id (e.g. a caller that
// stamped it directly) is left untouched. A bead that is itself a wish head
// (carries the bare `thread` label) is never given a thread_id — the head IS
// the wish and must not point at another.
func (s *BeadsServer) maybeCopyThreadID(ctx context.Context, bead *model.Bead, explicitParent *model.Dependency, now time.Time) {
	if bead == nil || s == nil || s.store == nil {
		return
	}
	if noAutoParentFromContext(ctx) {
		return
	}
	subject, ok := actingAgentRef(ctx)
	if !ok {
		return
	}

	// A wish head must never inherit a thread_id (it IS the wish). Guard before
	// any store read so heads are cheap to skip.
	if hasLabel(bead.Labels, threadHeadLabel) {
		return
	}

	threadID := s.autoParentFromAgentBead(ctx, subject).ThreadID
	if threadID == "" || threadID == bead.ID {
		// No wish to propagate, or it would point at itself.
		return
	}

	// Copy-if-empty: never clobber a thread_id the caller already set.
	if existing := threadIDFromFields(bead.Fields); existing != "" {
		return
	}

	// Guard 2 — cross-wish mis-bind. If the new bead has an explicit parent
	// that resolves to a DIFFERENT wish than the agent's carried thread_id,
	// the bead belongs to the explicit parent's wish; do not silently stamp
	// the agent's wish over it.
	if explicitParent != nil && explicitParent.DependsOnID != "" {
		parentWish := s.wishOfBead(ctx, explicitParent.DependsOnID)
		if parentWish != "" && parentWish != threadID {
			slog.Warn("skipping transitive thread_id copy: explicit parent belongs to a different wish",
				"bead_id", bead.ID,
				"agent", subject,
				"agent_thread_id", threadID,
				"explicit_parent", explicitParent.DependsOnID,
				"parent_wish", parentWish,
				"bead", "ka-0000007300",
			)
			return
		}
	}

	// Persist the copy. UpdateBead rewrites the whole fields blob, so mutate a
	// local copy of the bead's Fields and write it back.
	updated := *bead
	updated.Fields = setField(bead.Fields, "thread_id", threadID)
	updated.UpdatedAt = now
	if err := s.store.UpdateBead(ctx, &updated); err != nil {
		slog.Warn("transitive thread_id copy failed",
			"bead_id", bead.ID,
			"agent", subject,
			"thread_id", threadID,
			"err", err.Error(),
		)
		return
	}
	// Reflect the persisted change on the caller's bead so the create response
	// carries the stamped thread_id.
	bead.Fields = updated.Fields
	bead.UpdatedAt = updated.UpdatedAt

	s.recordAndPublish(ctx, events.TopicBeadUpdated, bead.ID, bead.CreatedBy, events.BeadUpdated{
		Bead:    bead,
		Changes: map[string]any{"fields.thread_id": threadID},
	})
	s.recordMutation(ctx, bead.ID, bead.CreatedBy, "fields.thread_id", "", threadID, model.MutationTypeUpdate)
}

// wishOfBead resolves which wish (thread head) a bead belongs to: its
// fields.thread_id pointer, or — if the bead is itself a wish head (carries
// the bare `thread` label) — its own id. Returns "" when the bead cannot be
// loaded or carries no wish association. Used by the cross-wish guard so a
// new bead explicitly parented under a wish member (or the head itself) is
// recognized as part of that wish.
func (s *BeadsServer) wishOfBead(ctx context.Context, beadID string) string {
	if s == nil || s.store == nil || beadID == "" {
		return ""
	}
	b, err := s.store.GetBead(ctx, beadID)
	if err != nil || b == nil {
		return ""
	}
	// A wish head IS its own wish.
	if hasLabel(b.Labels, threadHeadLabel) {
		return b.ID
	}
	return threadIDFromFields(b.Fields)
}

// threadIDFromFields extracts a non-empty, trimmed fields.thread_id from a raw
// fields blob, or "" when absent/blank/unparseable.
func threadIDFromFields(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	if v, ok := fields["thread_id"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
