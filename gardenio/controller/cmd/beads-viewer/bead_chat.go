package main

// Bead-show — message-bead chat thread (feature ka-0000007791, follow-on).
//
// The maps view (maps_chat.go) pulls type=message beads OUT of the per-gardener
// constellation and interleaves their literal contents into a chat-history
// panel. That cut was scoped to /maps/{gardenerID} only; the bead-show page
// (/{id}) — the view an operator actually lands on when they open a thread or
// task bead — still rendered every squawk / Slack drop that mentions the bead
// as an ordinary constellation node disk, with the chat text hidden behind a
// click. This file closes that gap.
//
// A message is in-scope for a bead's chat thread when it MENTIONS the bead or
// is MENTIONED BY it (the same reverse-mention wiring the constellation walks:
// a squawk's prose references the bead it is about). collectMessageRefIDs pulls
// those ids out of the mention summaries so the caller can (a) hydrate them and
// (b) strip them from the refs the constellation builder sees, so the message
// nodes stop being drawn. buildBeadChat then projects the hydrated message
// beads to the SAME chatMessage / mapChatView shapes maps_chat.go renders, so
// the bead_chat template panel reuses the maps chat styling verbatim.
//
// Both functions are PURE (no http, no daemon) — they operate on the mention
// summaries the handler already fetched and the hydrated BeadDetail map — so
// they unit-test in isolation, mirroring the maps_chat precedent.

import (
	"sort"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// collectMessageRefIDs returns the de-duplicated ids of every type=message
// bead among the supplied mention summaries (a bead's forward mentions and its
// mentioned-by reverse refs). These are the squawks / Slack drops / delivery
// reports that reference the bead in prose; the handler hydrates them for the
// chat panel and strips them from the constellation refs so they stop drawing
// as node disks. selfID is never collected (a bead never chats with itself).
func collectMessageRefIDs(selfID string, refSets ...[]beadsapi.BeadReferenceSummary) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, refs := range refSets {
		for _, r := range refs {
			if r.Type != "message" {
				continue
			}
			if r.ID == "" || r.ID == selfID {
				continue
			}
			if _, dupe := seen[r.ID]; dupe {
				continue
			}
			seen[r.ID] = struct{}{}
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// stripMessageRefs returns the subset of refs that are NOT type=message — the
// references the constellation should still draw once the message beads are
// pulled into the chat panel. Order is preserved so constellation layout stays
// stable.
func stripMessageRefs(refs []beadsapi.BeadReferenceSummary) []beadsapi.BeadReferenceSummary {
	if len(refs) == 0 {
		return refs
	}
	out := make([]beadsapi.BeadReferenceSummary, 0, len(refs))
	for _, r := range refs {
		if r.Type == "message" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// agentAuthoredMessagesQuery builds the ListBeads query that pulls an agent
// bead's OWN authored messages — squawks (type=message) and sent mail
// (type=mail) — for its chat panel. Agent squawks/mail are NOT wired by typed
// mention; they are linked to their author by a `from:<agent-name>` label,
// where the agent name IS the agent bead's title (ka-0000002848). Newest-first
// so the panel survives the mapChatMaxMessages cap; buildBeadChat re-sorts
// oldest→newest for display.
func agentAuthoredMessagesQuery(agentTitle string) beadsapi.ListBeadsQuery {
	return beadsapi.ListBeadsQuery{
		Types:  []string{"message", "mail"},
		Labels: []string{"from:" + agentTitle},
		Sort:   "-created_at",
		Limit:  mapChatMaxMessages,
	}
}

// isChatRenderableBeadDetail reports whether a hydrated bead can render as a
// chat line. The mention-scoped path only ever feeds type=message beads, but
// the agent-authored path (agentAuthoredMessagesQuery) also pulls type=mail
// (agent-sent mail carries the same `from:<name>` label). projectChatMessage /
// messageKind already handle both, so both are renderable here. A nil bead is
// never renderable.
func isChatRenderableBeadDetail(b *beadsapi.BeadDetail) bool {
	return b != nil && (b.Type == "message" || b.Type == "mail")
}

// buildBeadChat projects the hydrated in-scope message beads into the chat-thread
// panel view-model. ids is the message-id set from collectMessageRefIDs; beads
// is the hydrated BeadDetail map (BatchGetBeads). Messages are sorted
// oldest→newest by created-else-updated time and capped to the newest
// mapChatMaxMessages — the dropped older count is reported so the panel can say
// so, reusing maps_chat's mapChatView shape and the projectChatMessage projector
// verbatim. opts is the host bead's linkifier gates, so a bare `!N` in a chat
// line resolves to the same repo the description and comments do instead of the
// process-wide gardenio default (ka-0000005612). Pure → unit-testable.
func buildBeadChat(
	ids []string,
	beads map[string]*beadsapi.BeadDetail,
	basePath string,
	now time.Time,
	opts LinkifyOpts,
) mapChatView {
	if len(ids) == 0 {
		return mapChatView{}
	}

	type stamped struct {
		ts  time.Time
		msg chatMessage
	}
	rows := make([]stamped, 0, len(ids))
	for _, id := range ids {
		b := beads[id]
		if !isChatRenderableBeadDetail(b) {
			continue // un-hydrated or not a message/mail bead — nothing to render
		}
		// Prefer created-time for chat ordering (mirrors maps_chat.go): a chat
		// log reads by when each line was said, not when it was last edited.
		ts, ok := pickAgeTimestamp(b.CreatedAt, b.UpdatedAt)
		if !ok {
			ts = time.Time{} // sorts to the front; still rendered
		}
		rows = append(rows, stamped{ts: ts, msg: projectChatMessage(b, basePath, now, opts)})
	}
	if len(rows) == 0 {
		return mapChatView{}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].ts.Equal(rows[j].ts) {
			return rows[i].ts.Before(rows[j].ts)
		}
		return rows[i].msg.ID < rows[j].msg.ID // stable tiebreak for golden tests
	})

	total := len(rows)
	hidden := 0
	// Keep the NEWEST mapChatMaxMessages (the tail of the ascending list) so the
	// live conversation survives the cap; the dropped head is the older history.
	if total > mapChatMaxMessages {
		hidden = total - mapChatMaxMessages
		rows = rows[total-mapChatMaxMessages:]
	}

	msgs := make([]chatMessage, 0, len(rows))
	for _, r := range rows {
		msgs = append(msgs, r.msg)
	}
	return mapChatView{
		Messages:    msgs,
		Shown:       len(msgs),
		Total:       total,
		OlderHidden: hidden > 0,
		HiddenCount: hidden,
		HasMessages: true,
	}
}
