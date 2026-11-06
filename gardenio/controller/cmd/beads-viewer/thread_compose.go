package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/oidcauth"
)

// Phase 2 — net-new interactive /threads view (epic ka-0000000679, task
// ka-0000005643). This file is the composer WRITE path: the operator-gated
// POST handlers that (a) post a message INTO an existing wish thread and
// (b) MINT a new wish on the first message to a no-wish surface, idempotently.
//
// Bridge-mediated send (LOCKED arch, QC rubric gate A.7): the beads-viewer
// holds NO Slack bot token — only slack-bridge does. So the composer never
// calls Slack. It writes a closed type=message bead carrying the wish head's
// slack_thread_channel/slack_thread_ts plus the `thread-say` label; the
// slack-bridge ThreadMessageForward watcher (internal/bridge/
// thread_message_forward.go) relays it into the Slack thread on the bridge's
// credentials, reusing the bridge's token, the shared slackCardLimiter, and
// the same PostMessage+MsgOptionTS thread-reply primitive `gb slack reply`
// (handlePostReply) and postThreadStateReply already use. This is the same
// "producer writes a closed message bead, slack-bridge forwards it" pattern as
// trigger-notice (notice.go) and squawk (squawk.go) — the credential surface
// stays in one binary.
//
// Idempotent mint (rubric gate A.8): the head is minted via the 1a-3 daemon
// resolve-or-create dedup guard (ka-0000003315) — CreateBead with
// IdempotencyKey == the canonical slack-thread:{channel}:{ts} identity. The
// daemon's lookup-or-return plus the per-fleet partial unique index (kbeads
// migration 029) collapse a double-submit (and even a viewer↔slack-bridge
// race across binaries) to exactly ONE head. This mirrors the bridge's
// getOrCreateThreadBead (internal/bridge/thread_bead.go) so both origins
// converge on the same head.

// threadComposeMaxLen bounds a composed message. It matches the comment box
// ceiling (maxCommentLen) so the composer and the comment box share one limit.
const threadComposeMaxLen = maxCommentLen

// threadSayLabel is the cross-binary protocol label the slack-bridge
// ThreadMessageForward watcher filters closed message beads on. It is the
// EXPORTED bridge constant (not a local copy): the producer (this viewer) and
// the consumer (the bridge forwarder) MUST agree on the exact string, so it
// lives in exactly one place — if it ever drifted, the viewer would silently
// write beads the bridge never forwards, with no compile error to catch it.
const threadSayLabel = bridge.ThreadSayLabel

// threadComposeFields are the parsed, validated inputs shared by both compose
// handlers.
type threadComposeFields struct {
	channel  string
	threadTS string
	text     string
}

// handleThreadReply posts a message INTO an existing wish thread. It mirrors
// handleAddComment (server.go) for its operator gate, parsing, and validation,
// but instead of commenting on a bead it writes a bridge-forwarded thread-say
// message bead bound to the wish head's Slack thread.
//
// Route: POST /b/{id}/thread-reply (operatorOnly). The /b/ prefix keeps the
// write surface clear of the /threads read routes (registered by 2-3) so this
// handler does not depend on or collide with that route-shadowing work.
func (s *Server) handleThreadReply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing thread id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		// Defense-in-depth: operatorOnly + the OIDC middleware should already
		// block this, but a middleware reorder must not silently let an
		// anonymous write through (mirrors handleAddComment).
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.PostForm.Get("text"))
	if text == "" {
		http.Error(w, "Message cannot be empty.", http.StatusBadRequest)
		return
	}
	if len(text) > threadComposeMaxLen {
		http.Error(w, "Message is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}

	head, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Thread not found", http.StatusNotFound)
			return
		}
		s.logger.Error("thread-reply: getting head bead", "id", id, "error", err)
		http.Error(w, "Failed to load thread", http.StatusInternalServerError)
		return
	}

	channel := strings.TrimSpace(head.Fields[threadSlackChannelField])
	threadTS := strings.TrimSpace(head.Fields[threadSlackTSField])
	if !validThreadBinding(channel, threadTS) {
		// The bead is not a Slack-bound wish head — there is no thread to post
		// into. 409 (not 500): the request is well-formed but the target's
		// state forbids it.
		s.logger.Warn("thread-reply: head has no valid slack binding",
			"id", id, "channel", channel, "thread_ts", threadTS)
		http.Error(w, "This thread is not bound to a Slack conversation.", http.StatusConflict)
		return
	}

	// Replying into a fulfilled (closed) wish revives it, mirroring how a fresh
	// Slack reply reopens a resolved thread (ka-0000000329: "reopen a wish and
	// add messages, just like in slack"). Reopen BEFORE posting so the thread is
	// open when the message lands.
	s.reopenClosedWishHead(r.Context(), head)

	if _, err := s.postThreadSayMessage(r.Context(), id, headLand(head.Labels), email, threadComposeFields{
		channel:  channel,
		threadTS: threadTS,
		text:     text,
	}); err != nil {
		s.logger.Error("thread-reply: posting message", "id", id, "actor", email, "error", err)
		http.Error(w, "Failed to post message.", http.StatusBadGateway)
		return
	}

	// 2-4/2-5 own the rendered fused-timeline fragment; until they ship, ack
	// with 204 so the htmx composer clears its input without a swap target.
	w.WriteHeader(http.StatusNoContent)
}

// handleThreadCompose mints (or idempotently resolves) a wish for a Slack
// surface that has no head yet, then posts the operator's first message into
// that thread. This is the "first message to a no-wish surface mints exactly
// one wish" path (rubric gate A.8).
//
// Route: POST /threads/compose (operatorOnly). Inputs (form): channel,
// thread_ts, text, optional project.
func (s *Server) handleThreadCompose(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	fields := threadComposeFields{
		channel:  strings.TrimSpace(r.PostForm.Get("channel")),
		threadTS: strings.TrimSpace(r.PostForm.Get("thread_ts")),
		text:     strings.TrimSpace(r.PostForm.Get("text")),
	}
	project := strings.TrimSpace(r.PostForm.Get("project"))
	if fields.text == "" {
		http.Error(w, "Message cannot be empty.", http.StatusBadRequest)
		return
	}
	if len(fields.text) > threadComposeMaxLen {
		http.Error(w, "Message is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}
	if !validThreadBinding(fields.channel, fields.threadTS) {
		// The mint identity is slack-thread:{channel}:{ts}; without a valid
		// (channel, ts) the dedup guard has no key and the "exactly one wish"
		// guarantee cannot hold, so refuse rather than mint an un-deduped head.
		http.Error(w, "A valid Slack channel and thread timestamp are required.", http.StatusBadRequest)
		return
	}

	headID, existing, err := s.resolveOrMintWishHead(r.Context(), fields.channel, fields.threadTS, project, fields.text)
	if err != nil {
		s.logger.Error("thread-compose: mint wish head",
			"channel", fields.channel, "thread_ts", fields.threadTS, "actor", email, "error", err)
		http.Error(w, "Failed to create thread.", http.StatusBadGateway)
		return
	}

	// Composing into a PRE-EXISTING wish that was fulfilled (closed) revives it,
	// mirroring how a fresh Slack reply reopens a resolved thread (ka-0000000329).
	// A freshly-minted head (existing == nil) is already open, so this is a no-op
	// for the new-wish path. Reopen BEFORE posting so the message lands on an open
	// thread.
	if existing != nil {
		s.reopenClosedWishHead(r.Context(), existing)
	}

	if _, err := s.postThreadSayMessage(r.Context(), headID, s.mintedHeadLand(r.Context(), headID, existing), email, fields); err != nil {
		s.logger.Error("thread-compose: posting first message",
			"id", headID, "actor", email, "error", err)
		http.Error(w, "Thread created but message failed to post.", http.StatusBadGateway)
		return
	}

	// Hand the freshly-resolved head id back so an htmx caller can navigate to
	// the wish detail (the route 2-3 registers). HX-Redirect is the same
	// affordance handleCreateAdvice uses.
	w.Header().Set("HX-Redirect", s.basePath+"/threads/"+headID)
	w.WriteHeader(http.StatusNoContent)
}

// resolveOrMintWishHead returns the ka-id of the thread (wish) head for the
// given Slack (channel, threadTS), minting it via the daemon resolve-or-create
// dedup guard when absent. It replicates the bridge's getOrCreateThreadBead
// (internal/bridge/thread_bead.go) shape so a viewer-minted head is byte-for-
// byte interchangeable with a slack-bridge-minted one, and so a double-submit
// (or a viewer↔bridge race) collapses to ONE head via the IdempotencyKey.
//
// The second return value is the PRE-EXISTING head detail when the label
// fast-path resolved one (so the caller can reopen it if it was fulfilled —
// ka-0000000329), and nil when the head was freshly minted (a new head is
// already open, so there is nothing to reopen). It is nil on the create path
// regardless of whether the daemon's IdempotencyKey guard deduped, because the
// fast-path miss means we did not read the existing head's status — the
// new-head case and the concurrent-race-dedup case are indistinguishable here
// and both are correct to treat as "no reopen needed" (a head minted moments
// ago is open).
func (s *Server) resolveOrMintWishHead(ctx context.Context, channel, threadTS, project, rootText string) (string, *beadsapi.BeadDetail, error) {
	label := threadBeadIdentityLabel(channel, threadTS)

	// Fast-path pre-lookup on the canonical label: resolves the steady-state
	// hit (the head already exists, minted by the bridge on a Slack mention or
	// by an earlier compose) without a create attempt, and resolves legacy
	// heads that predate the idempotency-key guard. The IdempotencyKey create
	// below is the race backstop for the concurrent-miss case.
	existing, listErr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{label},
		Limit:  1,
	})
	if listErr == nil && existing != nil && len(existing.Beads) > 0 && existing.Beads[0] != nil {
		return existing.Beads[0].ID, existing.Beads[0], nil
	}

	labels := []string{"source:slack", label, threadHeadLabel}
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	fieldsMap := map[string]string{
		threadSlackChannelField: channel,
		threadSlackTSField:      threadTS,
		"spawn_source":          "viewer-composer",
	}
	if rootText != "" {
		fieldsMap[threadRootTextField] = truncate(rootText, 4000)
	}
	fieldsJSON, mErr := json.Marshal(fieldsMap)
	if mErr != nil {
		return "", nil, mErr
	}
	title := "Slack thread " + channel + "/" + threadTS
	if rootText != "" {
		title = truncate("Slack thread: "+rootText, 120)
	}

	// IdempotencyKey = the canonical thread identity. The daemon does a
	// lookup-or-return on this key (project-scoped) and the per-fleet partial
	// unique index backstops the concurrent-insert race so two racers — even
	// in different binaries (this viewer AND slack-bridge) — converge on ONE
	// head. See beadsapi.CreateBeadRequest.IdempotencyKey and kbeads
	// migration 029.
	id, err := s.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:          title,
		Type:           "task",
		Kind:           "issue",
		Priority:       threadHeadPriority,
		Labels:         labels,
		Fields:         json.RawMessage(fieldsJSON),
		IdempotencyKey: label,
	})
	return id, nil, err
}

// reopenClosedWishHead reopens a fulfilled (closed) wish head so a new message
// revives the thread, mirroring how a fresh Slack reply reopens a resolved
// conversation (ka-0000000329: "reopen a wish and add messages, just like in
// slack"). It is a no-op for a head that is already open/in_progress, so both
// compose handlers can call it unconditionally on the resolved head.
//
// Best-effort by design: a reopen failure is logged but NOT surfaced to the
// caller. The operator's message is the point — it still posts (and the bridge
// still forwards it into Slack) even if the status flip fails; a thread that
// stays "fulfilled" while carrying a new reply is a cosmetic lag the next poll
// or a manual reopen corrects, not a reason to 502 the send. kbeads remains
// authoritative for transition legality.
func (s *Server) reopenClosedWishHead(ctx context.Context, head *beadsapi.BeadDetail) {
	if head == nil || head.Status != "closed" {
		return
	}
	open := "open"
	if err := s.daemon.UpdateBead(ctx, head.ID, beadsapi.UpdateBeadRequest{Status: &open}); err != nil {
		s.logger.Warn("thread-compose: reopening fulfilled wish head failed (message still posts)",
			"id", head.ID, "error", err)
	}
}

// headLand is a wish head's land: its one project label, "" for none or
// several.
func headLand(labels []string) string {
	land := ""
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			if land != "" {
				return ""
			}
			land = v
		}
	}
	return land
}

// mintedHeadLand is the land of the head handleThreadCompose resolved: the
// existing head's labels, else the head's as the daemon now holds it (the
// create may have converged on a head the bridge minted). Never the form's
// project: the land is the head's. A read that fails gives "", and the
// message is relayed to Slack but reaches no agent.
func (s *Server) mintedHeadLand(ctx context.Context, headID string, existing *beadsapi.BeadDetail) string {
	if existing != nil {
		return headLand(existing.Labels)
	}
	head, err := s.daemon.GetBead(ctx, headID)
	if err != nil || head == nil {
		s.logger.Warn("thread-compose: reading the minted head's land failed; the message will reach no agent",
			"id", headID, "error", err)
		return ""
	}
	return headLand(head.Labels)
}

// postThreadSayMessage writes the closed type=message bead that the
// slack-bridge ThreadMessageForward watcher relays into the Slack thread. The
// bead carries the (channel, threadTS) binding on its OWN fields (copied from
// the head) plus the wish-pointer thread_id and the thread-say label. It is
// created then immediately closed because the forwarder triggers on
// beads.bead.closed (matching the notice/squawk create→close lifecycle).
//
// land is the head's land (headLand), stamped as project:<land>: the bridge
// reads a thread-say's land only from that label and gives a message of no
// land no agent outcome (gd-cVqdya4Zfo), so without it a reply relays to
// Slack but never nudges, respawns or spawns the thread's agent
// (ka-0000005407, gd-oG21yWzoKL).
//
// Returns the message bead id. Operator attribution flows automatically: the
// operatorMiddleware stamps X-Operated-By onto the context, and CreatedBy is
// set to the operator email so the forwarder/timeline can attribute the
// message to the person, not an agent.
func (s *Server) postThreadSayMessage(ctx context.Context, headID, land, email string, f threadComposeFields) (string, error) {
	fieldsMap := map[string]string{
		"text":                  f.text,
		threadSlackChannelField: f.channel,
		threadSlackTSField:      f.threadTS,
		threadIDField:           headID, // wish-pointer invariant (thread derivatives carry thread_id == head id)
	}
	fieldsJSON, err := json.Marshal(fieldsMap)
	if err != nil {
		return "", err
	}
	labels := []string{threadSayLabel, threadMembershipLabel(headID)}
	if land != "" {
		labels = append(labels, "project:"+land)
	}
	msgID, err := s.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     truncate(f.text, 500),
		Type:      "message",
		Kind:      "data",
		Labels:    labels,
		Fields:    json.RawMessage(fieldsJSON),
		CreatedBy: email,
	})
	if err != nil {
		return "", err
	}
	// Close the message bead to fire the beads.bead.closed event the bridge
	// forwarder watches. A close failure leaves an unforwarded message bead;
	// surface it so the caller can report the post failed.
	if err := s.daemon.CloseBeadAs(ctx, msgID, email, nil); err != nil {
		return msgID, err
	}
	return msgID, nil
}

// threadBeadIdentityLabel returns the canonical slack-thread:{channel}:{ts}
// identity label used as the mint dedup key. Mirrors bridge.threadBeadLabel
// (internal/bridge/thread_bead.go); the viewer keeps its own copy because the
// bridge helper is unexported and the packages do not depend on each other.
func threadBeadIdentityLabel(channel, threadTS string) string {
	return "slack-thread:" + channel + ":" + threadTS
}

// threadMembershipLabel returns the origin-agnostic membership label binding a
// derivative bead to its wish head by ka-id. Mirrors bridge.threadIdentityLabel.
//
// This label is a provenance MARKER, not a membership query key (ka-0000006057
// gap 4): thread membership is resolved by fields.thread_id ∪ parent-child
// closure (threadMembers) and the index by fields.thread_id (threadList) —
// nothing queries on this label. The message bead below therefore ALSO sets
// fields.thread_id (the authoritative wish-pointer); the label is redundant
// belt-and-braces provenance. See bridge.threadIdentityLabel for the full note.
func threadMembershipLabel(headID string) string {
	return "thread:" + headID
}

// validThreadBinding reports whether (channel, threadTS) is a usable Slack
// thread binding. Mirrors bridge.isValidThreadBinding: non-empty channel of at
// least two chars and a timestamp in seconds.microseconds form.
func validThreadBinding(channel, threadTS string) bool {
	if channel == "" || threadTS == "" {
		return false
	}
	// Slack channel IDs start with C, D, or G — a single char is never valid.
	if len(channel) < 2 {
		return false
	}
	// Slack timestamps are "seconds.microseconds"; the dot is load-bearing.
	return strings.Contains(threadTS, ".")
}
