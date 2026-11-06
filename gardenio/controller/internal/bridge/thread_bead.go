package bridge

import (
	"context"
	"encoding/json"
	"fmt"

	"gardenio/controller/internal/beadsapi"
)

// threadBeadLabel returns the canonical label that identifies the bead
// representing a Slack thread. It is unique per (channel, threadTS) pair and
// is the sole race-guard surface for resolve-or-create lookups.
func threadBeadLabel(channel, threadTS string) string {
	return "slack-thread:" + channel + ":" + threadTS
}

// threadKindLabel is the bare, origin-agnostic label stamped on every thread
// (= user "wish") head bead regardless of how it was minted (Slack mention,
// agent/manual creation, viewer "+ new wish"). It lets enumeration queries
// (the Phase 2 /threads view) find all wish heads via a single label,
// independent of the Slack-specific slack-thread:{c}:{ts} dedup label.
const threadKindLabel = "thread"

// wishHeadCtx is ctx for a write of a wish head's marks (threadKindLabel,
// promoted_from, promoted_to): on the bridge's own credential, never the
// linked person's bearer, since kbeads refuses those marks from a land member
// (kbeads wish_head_authz.go, gd-3HAfTiByJK).
func wishHeadCtx(ctx context.Context) context.Context {
	return beadsapi.WithoutUserBearer(ctx)
}

// threadBeadPriority is the priority stamped on a newly-minted thread (wish)
// head. It MUST be set explicitly: beadsapi.CreateBeadRequest.Priority is a
// plain `int` with NO omitempty, so leaving it unset marshals as an explicit
// "priority":0 — P0, the HIGHEST band — rather than inheriting the daemon's
// default. That silent P0 is what let 83 open Slack-conversation beads sort
// above every real P1-P3 bead in `gb ready` (which orders priority,-created_at
// and defaults to --limit 20), so the default ready view was 20/20 thread beads
// and zero actionable work — starving gardener-delivery's bug lane, which
// dispatches from a bare `gb ready`. See ka-0000003760.
//
// P2 matches what `kd create` gives a bead with no explicit --priority, so a
// thread head minted by the bridge and one minted from the CLI now agree.
const threadBeadPriority = 2

// threadIdentityLabel returns the membership label used to bind a derivative
// bead (message, member task, agent) to its thread (wish) head by ka-id. It is
// the origin-agnostic counterpart to threadBeadLabel for the future
// agent/manual/viewer-minted case, where there is no (channel, threadTS) pair.
//
// Wish-pointer invariant (all origins): fields.thread_id == the thread head's
// ka-id. The head itself carries NO thread_id field — it IS the wish, so it
// does not point at itself. Derivatives carry fields.thread_id pointing back at
// the head, and may additionally carry threadIdentityLabel(headID). The legacy
// fields.slack_thread_bead_id is retained as a compatibility alias holding the
// same value as thread_id.
//
// IMPORTANT — this label is a provenance MARKER, NOT a membership query key
// (ka-0000006057 gap 4). The viewer resolves thread membership as
// UNION(fields.thread_id == headID, parent-child closure) (threadMembers in
// beads-viewer/threads.go) and the index enumerates via fields.thread_id
// (threadList); NEITHER ever queries on this label. A producer that stamped ONLY
// threadIdentityLabel(headID) WITHOUT also setting fields.thread_id (and without
// a parent-child edge to the head) would be INVISIBLE to the thread views. Every
// real producer sets fields.thread_id — the composer (beads-viewer
// thread_compose.go) and the forward path (bot_thread_forward.go) set BOTH the
// field and this label — so all are safe; the label is redundant belt-and-braces
// provenance, not an authoritative membership signal. Do not document or rely on
// it as a "query-time facet": honoring it in the membership query is a deliberate
// non-goal (it would broaden membership semantics for zero current benefit).
func threadIdentityLabel(kdID string) string {
	return "thread:" + kdID
}

// threadBeadFields collects the metadata recorded on a thread bead when it is
// first created. Callers may leave fields empty; only non-empty values are
// serialized into the bead's Fields JSON.
type threadBeadFields struct {
	Channel       string
	ThreadTS      string
	SpawnChannel  string
	OpenerUserID  string
	Permalink     string
	RootText      string
	RootAtSlackTS string
}

// threadBeadAssignee is the daemon identity the bridge's own fleet token
// authenticates as. A linked person's Slack interaction creates the thread
// head AS that person (created_by = their email, gd-OZJ6H2S3xx), so without
// this the bridge's later fleet-token writes to its own head (terminal-witness
// stamp, autoclose, slack fields) fail the daemon ownership check with a 403
// "requires created_by, assignee, owner, or admin role" and the person's thread
// tasks pile up open (gd-PQCcv8sndp). Naming the bridge as assignee satisfies
// that check while created_by keeps the person for attribution.
const threadBeadAssignee = "slack-bridge"

// getOrCreateThreadBead resolves the bead representing a Slack thread, or
// creates it if none exists. The lookup uses the canonical
// slack-thread:{channel}:{ts} label so that bridge restarts (and concurrent
// mentions in parallel goroutines) converge on a single thread bead.
//
// The resolve-or-create is race-safe at the DAEMON: the create passes the
// canonical thread label as IdempotencyKey, so two callers racing past the
// pre-lookup both attempt the insert and converge on exactly ONE bead — the
// daemon's lookup-or-return plus the per-fleet partial unique index (kbeads
// migration 029) catch the loser's 23505 and hand back the winner's id. This
// guard lives in the daemon, so it holds even when the racers are SEPARATE
// binaries (the slack-bridge AND the beads-viewer "+ new wish" mint), where no
// shared process-local in-flight set exists. See kbeads
// internal/server/beads_idempotency.go (ka-0000002856).
//
// Returns the bead ID, a best-effort created flag, and any error. created is
// false when the pre-lookup resolved an existing bead; it is true when this
// call issued the create. Note the flag is approximate under a concurrent
// race: a caller that loses the insert race still gets created=true even
// though the daemon returned the winner's existing bead (dedup), because the
// create endpoint returns only an id and cannot signal "was a no-op". All
// current callers ignore the flag. Errors are reported but the caller may
// choose to proceed without a thread bead — agent-bead creation must not be
// blocked by a thread-bead daemon failure.
func (b *Bot) getOrCreateThreadBead(
	ctx context.Context,
	project string,
	tbf threadBeadFields,
) (beadID string, created bool, err error) {
	if !isValidThreadBinding(tbf.Channel, tbf.ThreadTS) {
		return "", false, fmt.Errorf("invalid thread binding: channel=%q ts=%q", tbf.Channel, tbf.ThreadTS)
	}
	label := threadBeadLabel(tbf.Channel, tbf.ThreadTS)

	// Fast-path pre-lookup on the canonical label. This resolves the common
	// steady-state hit (the thread bead already exists) without a create
	// attempt, yields an accurate created flag, and — critically — resolves
	// LEGACY thread beads that were minted before the idempotency-key guard
	// and therefore carry the label but no fields.idempotency_key. The create
	// below is the race backstop for the concurrent-miss case.
	existing, listErr := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{label},
		Limit:  1,
	})
	if listErr == nil && existing != nil && len(existing.Beads) > 0 {
		return existing.Beads[0].ID, false, nil
	}

	// Stamp the bare `thread` kind label alongside the Slack-specific dedup
	// label so the head is discoverable by origin-agnostic enumeration. The
	// head carries NO thread_id field — it IS the wish (see threadIdentityLabel).
	labels := []string{"source:slack", label, threadKindLabel}
	if project != "" {
		labels = append(labels, "project:"+project)
	}

	fields := map[string]string{
		"slack_thread_channel": tbf.Channel,
		"slack_thread_ts":      tbf.ThreadTS,
		"spawn_source":         "slack-thread",
	}
	if tbf.SpawnChannel != "" {
		fields["slack_spawn_channel"] = tbf.SpawnChannel
	}
	if tbf.OpenerUserID != "" {
		fields["slack_opener_user_id"] = tbf.OpenerUserID
		// Mirror the opener as slack_user_id + operated_by so legacy-attribution
		// heuristics (Pass-1 A, ka-0000001582) can recover the originating
		// human from the thread bead's fields rather than depending on the
		// audit row's operated_by column, which is rebound on dibs handoffs.
		fields["slack_user_id"] = tbf.OpenerUserID
		fields["operated_by"] = b.operatorFromSlackUser(ctx, tbf.OpenerUserID)
	}
	if tbf.Permalink != "" {
		fields["slack_permalink"] = tbf.Permalink
	}
	if tbf.RootText != "" {
		fields["slack_root_text"] = truncateText(tbf.RootText, 4000)
	}
	if tbf.RootAtSlackTS != "" {
		fields["slack_root_at"] = tbf.RootAtSlackTS
	}
	// Commons dual-write: the channel bead the thread was said in, when the
	// Slack channel is bound to one in this head's land, else the land's
	// default channel (channel_stamp.go headChannelFor).
	if chID := b.headChannelFor(ctx, tbf.Channel, labels); chID != "" {
		fields["channel_id"] = chID
	}

	fieldsJSON, mErr := json.Marshal(fields)
	if mErr != nil {
		return "", false, fmt.Errorf("marshalling thread-bead fields: %w", mErr)
	}

	title := fmt.Sprintf("Slack thread %s/%s", tbf.Channel, tbf.ThreadTS)
	if tbf.RootText != "" {
		title = truncateText("Slack thread: "+tbf.RootText, 120)
	}

	// IdempotencyKey = the canonical thread label. The daemon does a
	// lookup-or-return on this key (scoped to the same project) and the
	// per-fleet partial unique index (migration 029) backstops the
	// concurrent-insert race: if two callers — even in different binaries —
	// both miss the pre-lookup and race into the insert, exactly one wins and
	// the loser's create returns the winner's bead instead of a duplicate.
	id, cErr := b.daemon.CreateBead(wishHeadCtx(ctx), beadsapi.CreateBeadRequest{
		Title:          title,
		Type:           "task",
		Kind:           "issue",
		Priority:       threadBeadPriority,
		Assignee:       threadBeadAssignee,
		Labels:         labels,
		Fields:         json.RawMessage(fieldsJSON),
		IdempotencyKey: label,
	})
	if cErr != nil {
		return "", false, fmt.Errorf("creating thread bead: %w", cErr)
	}
	return id, true, nil
}
