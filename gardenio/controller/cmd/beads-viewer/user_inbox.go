package main

// Per-user bidirectional message feed on /u/{slug} (design §3 + §3.8,
// ka-0000005615; scope-expansion comment on the impl bead).
//
// The /u/{slug} inbox is a two-sided conversation feed merging FOUR bead
// populations into one chat log, reusing the chatMessage / mapChatView shapes
// (maps_chat.go) and the projectChatMessage projector verbatim:
//
//   AGENT → USER (incoming, other-side bubble):
//     - Mail        : type=mail addressed to one of the operator's recipient
//                     handles (assignee = ANY). status=open ⇒ unread.
//     - Squawks     : type=message beads that @-mention the operator's user
//                     bead (reverse-reference lookup, GetMentionedBy). Created
//                     after the operator's last inbox visit ⇒ unread.
//
//   USER → AGENT (outgoing, own-side bubble):
//     - Thread reply: type=task label slack-thread-reply, operated_by = viewer
//                     (bot_thread_forward.go).
//     - Bot mention : type=task label slack-mention, operated_by = viewer
//                     (bot_mentions.go).
//
// Both outgoing populations key on the operators(id) FK via ListBeadsQuery's
// OperatedByID filter — no new daemon primitive needed (the agent→user mail
// side reuses the merged Assignees filter, ka-0000007555). The merged log is
// sorted oldest→newest (chat-log order), capped at mapChatMaxMessages, with the
// dropped older count surfaced via mapChatView.OlderHidden.
//
// Direction styling: user-authored lines render own-side, agent-authored lines
// render other-side. gardenio is a FLAT org (ka-0000003111): a user's page is a
// total-transparency activity record, so bodies render in full for every viewer
// — there is no self-vs-other redaction. (The former §3.6 cross-user body
// redaction was removed; it contradicted "everyone can see everyone's activity"
// and surfaced the universally-disliked "message bodies are hidden" banner.)
//
// The assemble step is PURE (no http, no daemon) so it unit-tests in isolation
// against hydrated BeadDetail slices, mirroring the maps_chat / buildBeadChat
// precedent.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Inbox direction tags. directionFromUser places a line on the operator's own
// side of the feed (messages they authored); directionFromAgent on the other
// side (messages agents sent them). Empty Direction (maps / bead-show chat
// panels) renders unchanged.
const (
	directionFromUser  = "from-user"
	directionFromAgent = "from-agent"
)

// userInboxFetchLimit caps each of the four per-population daemon queries. The
// merged feed is then capped again at mapChatMaxMessages, so this just bounds
// the fan-out — generous enough that the newest-N cap, not the fetch, decides
// what shows.
const userInboxFetchLimit = 150

// userInboxView is the lazy-loaded inbox section / full-page view-model. It
// embeds the chat log (reused mapChatView) plus the affordance state: the
// unread count (badge), the distinct sender set (retained for the data
// contract), the per-agent cards the template renders, and the self flag.
//
// Cards is the redesign (ka-0000001181): the flat Chat feed regrouped into one
// independently-scrollable card per counterpart. Chat/Senders are kept intact so
// the assembleUserInbox contract and every existing test keep working.
type userInboxView struct {
	Chat         mapChatView
	Cards        []inboxAgentCard
	Slug         string
	FullURL      string
	UnreadCount  int
	Senders      []string
	IsViewerSelf bool
	HasMessages  bool
}

// inboxAgentCard groups every inbox line authored by a single counterpart into
// one card (redesign, ka-0000001181). Messages stay in oldest→newest chat-log
// order within the card; the template gives each card its own scroll box. Count
// and UnreadCount drive the header badges; Latest{Ago,RFC} stamp the newest
// line's age (relative + absolute-tooltip). Author is the counterpart handle;
// an empty handle renders as "unknown" in the template.
type inboxAgentCard struct {
	Author      string
	Messages    []chatMessage
	Count       int
	UnreadCount int
	LatestAgo   string
	LatestRFC   string
}

// groupInboxByAgent folds the flat, globally-sorted (oldest→newest) inbox feed
// into per-author cards. Because the input is already in chat-log order, each
// author's messages accumulate oldest→newest, and the position of an author's
// LAST message is that author's recency rank — so cards sort by that index
// descending (freshest counterpart first) with no timestamp re-parsing. The
// newest line's Ago/TimestampRFC become the card's latest-activity stamp. Pure
// → unit-testable, mirroring assembleUserInbox.
func groupInboxByAgent(msgs []chatMessage) []inboxAgentCard {
	if len(msgs) == 0 {
		return nil
	}
	idx := make(map[string]int) // author → index into cards
	cards := make([]inboxAgentCard, 0)
	lastPos := make([]int, 0) // parallel to cards: global index of the newest line
	for i, m := range msgs {
		ci, ok := idx[m.Author]
		if !ok {
			ci = len(cards)
			idx[m.Author] = ci
			cards = append(cards, inboxAgentCard{Author: m.Author})
			lastPos = append(lastPos, 0)
		}
		cards[ci].Messages = append(cards[ci].Messages, m)
		cards[ci].Count++
		if m.Unread {
			cards[ci].UnreadCount++
		}
		cards[ci].LatestAgo = m.Ago
		cards[ci].LatestRFC = m.TimestampRFC
		lastPos[ci] = i
	}

	order := make([]int, len(cards))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return lastPos[order[a]] > lastPos[order[b]]
	})
	out := make([]inboxAgentCard, 0, len(cards))
	for _, ci := range order {
		out = append(out, cards[ci])
	}
	return out
}

// inboxSource is one population feeding the merged feed: its hydrated beads,
// the conversation direction to stamp, and whether it is an incoming
// (agent→user) population eligible for unread marking.
type inboxSource struct {
	beads     []*beadsapi.BeadDetail
	direction string
	incoming  bool
}

// assembleUserInbox merges the supplied populations into one oldest→newest chat
// log, stamping direction + unread per line. Pure → unit-testable.
//
// lastVisit drives squawk/message unread (created strictly after ⇒ unread);
// mail unread keys on status=open. User-authored (outgoing) lines are never
// unread — they are the operator's own messages. Bodies always render in full:
// gardenio is flat, so a user's activity record is public to every viewer
// (ka-0000003111).
func assembleUserInbox(
	sources []inboxSource,
	basePath string,
	now, lastVisit time.Time,
) (mapChatView, int, []string) {
	type stamped struct {
		ts  time.Time
		msg chatMessage
	}
	rows := make([]stamped, 0)
	seen := make(map[string]struct{})
	unread := 0
	senderSet := make(map[string]struct{})

	for _, src := range sources {
		for _, b := range src.beads {
			if b == nil || b.ID == "" {
				continue
			}
			if _, dupe := seen[b.ID]; dupe {
				continue // a bead can match two populations; render once
			}
			seen[b.ID] = struct{}{}

			ts, ok := pickAgeTimestamp(b.CreatedAt, b.UpdatedAt)
			if !ok {
				ts = time.Time{} // sorts to the front; still rendered
			}

			msg := projectChatMessage(b, basePath, now, LinkifyOpts{})
			msg.Direction = src.direction
			msg.Unread = inboxMessageUnread(b, src, ts, lastVisit)
			if msg.Unread {
				unread++
			}
			if msg.Author != "" {
				senderSet[msg.Author] = struct{}{}
			}
			rows = append(rows, stamped{ts: ts, msg: msg})
		}
	}

	if len(rows) == 0 {
		return mapChatView{}, 0, nil
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].ts.Equal(rows[j].ts) {
			return rows[i].ts.Before(rows[j].ts)
		}
		return rows[i].msg.ID < rows[j].msg.ID // stable tiebreak for golden tests
	})

	total := len(rows)
	hidden := 0
	if total > mapChatMaxMessages {
		hidden = total - mapChatMaxMessages
		rows = rows[total-mapChatMaxMessages:]
	}

	msgs := make([]chatMessage, 0, len(rows))
	for _, r := range rows {
		msgs = append(msgs, r.msg)
	}

	senders := make([]string, 0, len(senderSet))
	for s := range senderSet {
		senders = append(senders, s)
	}
	sort.Strings(senders)

	return mapChatView{
		Messages:    msgs,
		Shown:       len(msgs),
		Total:       total,
		OlderHidden: hidden > 0,
		HiddenCount: hidden,
		HasMessages: true,
	}, unread, senders
}

// inboxMessageUnread reports whether an incoming line is unread. Outgoing
// (user-authored) lines are never unread. Mail is unread while open; other
// incoming lines (squawks) are unread when created after the last inbox visit.
func inboxMessageUnread(b *beadsapi.BeadDetail, src inboxSource, ts, lastVisit time.Time) bool {
	if !src.incoming {
		return false
	}
	if strings.EqualFold(b.Type, "mail") {
		return strings.EqualFold(strings.TrimSpace(b.Status), "open")
	}
	if ts.IsZero() {
		return false
	}
	return ts.After(lastVisit)
}

// inboxLastVisitField is the user-bead field storing the operator's last inbox
// view time (RFC3339). Anything created after it is "unread"; the timestamp is
// bumped when the page owner views their own inbox.
const inboxLastVisitField = "inbox_last_visit_at"

// parseInboxLastVisit reads inbox_last_visit_at off a user bead. Returns the
// zero time (everything unread) when absent or unparseable.
func parseInboxLastVisit(bead *beadsapi.BeadDetail) time.Time {
	if bead == nil || bead.Fields == nil {
		return time.Time{}
	}
	raw := strings.TrimSpace(bead.Fields[inboxLastVisitField])
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// recipientHandlesForOperator builds the operator's recipient-handle set — every
// name an agent might `gb mail send` to (design §3.2). It folds the operator's
// whole identity group (so a mail to an oidc email and one to a slack U-id both
// land in the same human's inbox), plus the slug and op-<id> fallback. Returns
// a de-duplicated, non-empty-on-success list; an empty result means the mail
// query is skipped (no addressable handle).
func (s *Server) recipientHandlesForOperator(ctx context.Context, operatorID int64, slug string) []string {
	set := make(map[string]struct{})
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" {
			set[v] = struct{}{}
		}
	}
	add(slug)
	add("op-" + itoa64(operatorID))

	resp, err := s.daemon.ListOperators(ctx, 0)
	if err != nil {
		s.logger.Warn("user inbox: list operators for recipient handles",
			"operator_id", operatorID, "slug", slug, "error", err)
	} else if resp != nil {
		// Find the dedup group containing this operator, then fold every
		// identity's subject / email / display / slug into the handle set.
		groups := dedupOperators(resp.Operators)
		for _, g := range groups {
			members := append([]beadsapi.OperatorDirectoryEntry{g.primary}, g.members...)
			inGroup := false
			for _, m := range members {
				if m.ID == operatorID {
					inGroup = true
					break
				}
			}
			if !inGroup {
				continue
			}
			for _, m := range members {
				add(m.Subject)
				add(m.Email)
				add(m.Display)
				add(m.Slug)
			}
		}
	}

	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out) // deterministic for golden tests
	return out
}

// buildUserInbox fetches the four populations and assembles the bidirectional
// feed. Every fetch follows the "return empty on error" rule (§5) so a daemon
// hiccup degrades the section to an empty state rather than 500-ing the page.
func (s *Server) buildUserInbox(ctx context.Context, operatorID int64, slug string, isViewerSelf bool) userInboxView {
	view := userInboxView{
		Slug:         slug,
		FullURL:      s.basePath + "/u/" + slug + "/inbox",
		IsViewerSelf: isViewerSelf,
	}
	now := s.now()

	// Last-visit timestamp lives on the operator's user bead alongside the
	// other profile fields. Missing bead / field ⇒ zero time (all unread).
	userBead, err := s.lookupUserBeadByOperator(ctx, operatorID)
	if err != nil {
		s.logger.Warn("user inbox: lookup user bead", "operator_id", operatorID, "slug", slug, "error", err)
	}
	lastVisit := parseInboxLastVisit(userBead)

	var sources []inboxSource

	// --- AGENT → USER: mail addressed to any recipient handle. ---
	if handles := s.recipientHandlesForOperator(ctx, operatorID, slug); len(handles) > 0 {
		res, merr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:     []string{"mail"},
			Assignees: handles,
			Sort:      "-created_at",
			Limit:     userInboxFetchLimit,
		})
		if merr != nil {
			s.logger.Warn("user inbox: mail query", "operator_id", operatorID, "slug", slug, "error", merr)
		} else if res != nil {
			sources = append(sources, inboxSource{beads: res.Beads, direction: directionFromAgent, incoming: true})
		}
	}

	// --- AGENT → USER: squawks / Slack drops that @-mention the user bead. ---
	if userBead != nil && userBead.ID != "" {
		refs, rerr := s.daemon.GetMentionedBy(ctx, userBead.ID, userInboxFetchLimit)
		if rerr != nil {
			s.logger.Warn("user inbox: mentioned-by query", "user_bead", userBead.ID, "slug", slug, "error", rerr)
		} else {
			ids := collectMessageRefIDs(userBead.ID, refs)
			if len(ids) > 0 {
				beads, berr := s.daemon.BatchGetBeads(ctx, ids)
				if berr != nil {
					s.logger.Warn("user inbox: batch get mention squawks", "slug", slug, "error", berr)
				} else {
					msgs := make([]*beadsapi.BeadDetail, 0, len(ids))
					for _, id := range ids {
						if b := beads[id]; isMessageBeadDetail(b) {
							msgs = append(msgs, b)
						}
					}
					sources = append(sources, inboxSource{beads: msgs, direction: directionFromAgent, incoming: true})
				}
			}
		}
	}

	// --- USER → AGENT: slack thread replies + bot mentions the operator authored. ---
	// Two bounded queries (the daemon Labels filter is AND, so one label each)
	// keyed on the operators(id) FK. Both are type=message own-side bubbles —
	// the unify model (ka-0000007618, decision ka-0000005492 → option C) folded
	// the per-reply tracking TASK into the conversation MESSAGE bead, so this
	// query now follows the message bead. Previously this listed type=task with
	// the same labels; the union of the two over the transition window is fine
	// (legacy task beads carry kind=issue but the label filter dominates), and
	// we want the type=message rows going forward.
	for _, label := range []string{"slack-thread-reply", "slack-mention"} {
		res, oerr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:        []string{"message"},
			Labels:       []string{label},
			OperatedByID: operatorID,
			Sort:         "-created_at",
			Limit:        userInboxFetchLimit,
		})
		if oerr != nil {
			s.logger.Warn("user inbox: outgoing query", "label", label, "operator_id", operatorID, "slug", slug, "error", oerr)
			continue
		}
		if res != nil {
			sources = append(sources, inboxSource{beads: res.Beads, direction: directionFromUser, incoming: false})
		}
	}

	chat, unread, senders := assembleUserInbox(sources, s.basePath, now, lastVisit)
	view.Chat = chat
	view.Cards = groupInboxByAgent(chat.Messages)
	view.UnreadCount = unread
	view.Senders = senders
	view.HasMessages = chat.HasMessages
	return view
}

// bumpInboxLastVisit stamps inbox_last_visit_at = now on the page owner's user
// bead so subsequent views mark only newer arrivals unread. Self-only and
// best-effort — a write failure is logged, never surfaced (the feed already
// rendered). No-op when the operator has no user bead.
func (s *Server) bumpInboxLastVisit(ctx context.Context, operatorID int64) {
	bead, err := s.lookupUserBeadByOperator(ctx, operatorID)
	if err != nil || bead == nil || bead.ID == "" {
		return
	}
	stamp := s.now().UTC().Format(time.RFC3339)
	if werr := s.daemon.UpdateBeadFieldsTyped(ctx, bead.ID, map[string]any{inboxLastVisitField: stamp}); werr != nil {
		s.logger.Warn("user inbox: bump last-visit", "user_bead", bead.ID, "operator_id", operatorID, "error", werr)
	}
}

// handleUserHomeSectionInbox renders the lazy-loaded inbox section embedded on
// /u/{slug}. Returns the user_home_inbox partial as an HTML fragment.
func (s *Server) handleUserHomeSectionInbox(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	operatorID, isSelf, status, err := s.resolveUserHomeViewer(r, slug)
	if err != nil {
		if status == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("user inbox section: resolve viewer", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", status)
		return
	}
	view := s.buildUserInbox(r.Context(), operatorID, slug, isSelf)
	// Viewing your own inbox marks everything up to now as read.
	if isSelf {
		s.bumpInboxLastVisit(r.Context(), operatorID)
	}
	s.renderPartial(w, r, "templates/partials/user_home_inbox.html", "user_home_inbox", map[string]any{
		"Inbox":    view,
		"BasePath": s.basePath,
		"Embedded": true,
	})
}

// handleUserInbox renders the dedicated full-page inbox at /u/{slug}/inbox —
// the expanded view of the same bidirectional feed. Self viewers get their
// last-visit bumped; cross-user viewers see the redacted feed (§3.6).
func (s *Server) handleUserInbox(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	operatorID, isSelf, status, err := s.resolveUserHomeViewer(r, slug)
	if err != nil {
		if status == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("user inbox page: resolve viewer", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", status)
		return
	}

	userBead, lerr := s.lookupUserBeadByOperator(r.Context(), operatorID)
	if lerr != nil {
		s.logger.Warn("user inbox page: lookup user bead for display name", "slug", slug, "error", lerr)
	}
	displayName := displayNameFromUserBead(userBead)
	if displayName == "" {
		displayName = slug
	}

	view := s.buildUserInbox(r.Context(), operatorID, slug, isSelf)
	if isSelf {
		s.bumpInboxLastVisit(r.Context(), operatorID)
	}

	s.render(w, r, "user_inbox.html", map[string]any{
		"Slug":         slug,
		"DisplayName":  displayName,
		"OperatorID":   operatorID,
		"IsViewerSelf": isSelf,
		"Inbox":        view,
		"BackURL":      s.basePath + "/u/" + slug,
	})
}
