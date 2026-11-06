package main

// Message-bead chat projection — the shared model for every chat-history panel.
//
// type=message beads (squawks, Slack drops, delivery reports, mail) render as a
// scrollable chat log rather than as graph node disks. This file holds the
// view-model (chatMessage, mapChatView) and the pure projector
// (projectChatMessage + its field extractors) that turn a hydrated message bead
// into a renderable line.
//
// Three pages share this machinery:
//   - the bead-show chat thread (bead_chat.go, buildBeadChat)
//   - the bidirectional user inbox (user_inbox.go)
//   - the gardener constellation chat panel (maps_chat.go, buildMapChat) — until
//     the old /maps view was retired (ka-0000000247), buildMapChat lived
//     alongside this code in maps_chat.go. The maps-only chat builder was
//     removed with the rest of /maps; the shared projector below was relocated
//     here so bead_chat.go and user_inbox.go keep compiling.
//
// All functions are PURE (no http, no daemon) so they unit-test in isolation
// against a hydrated BeadDetail map.

import (
	"html/template"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mapChatMaxMessages caps a chat panel at its newest N messages. A hub
// gardener's footprint can touch thousands of squawks; rendering them all would
// bloat the page and bury the recent, relevant context. We keep the NEWEST N
// (the live conversation) and surface a count of the older ones we dropped.
const mapChatMaxMessages = 200

// chatMessage is one rendered line in a chat-history panel — a single
// type=message bead projected to its literal contents. BodyHTML is the linkified
// message text; Author is the source agent / author handle (plain text, no
// per-message resolver round-trip). TimestampRFC drives a hover tooltip; Ago is
// the compact relative stamp shown inline.
//
// The bidirectional user inbox (user_inbox.go, design §3.8) reuses this shape
// verbatim and sets the otherwise-zero fields below. Direction places the line
// on the own-side (user-authored) or other-side (agent-authored) of the
// conversation feed; Unread drives the unread rail; SourceID/SourceHref render
// the "in: ka-X" jump-to-source chip. The bead-show chat panel leaves all four
// at their zero values, so its rendering is unchanged.
type chatMessage struct {
	ID           string
	Href         string
	Author       string
	Kind         string // "squawk" | "slack" | "mail" | "message" — drives the badge
	Ago          string
	TimestampRFC string
	BodyHTML     template.HTML

	// Direction is one of the inbox directions: directionFromUser (own-side
	// bubble) / directionFromAgent (other-side bubble), or "" for a nil bead.
	// projectChatMessage classifies it from the bead shape (messageDirection),
	// so BOTH the bead-show chat panel and the user inbox get agent-vs-user
	// attribution; the user-inbox projector then OVERRIDES it with the
	// query-derived direction (which knows the operator the page belongs to).
	Direction string
	// Unread marks an inbox line newer than the operator's last visit (squawks
	// / user-authored) or still-open (mail). Drives the unread rail + badge.
	Unread bool
	// SourceID/SourceHref power the jump-to-source chip ("in: ka-X") for
	// mention squawks that reference a bead the operator owns. Empty otherwise.
	SourceID   string
	SourceHref string

	// Tool-call fields (Phase A, ka-0000004601) — set ONLY for a plan-transcript
	// tool turn (turn_role=tool), by buildPlanTranscript → decorateToolTurn. Every
	// other chat surface (bead-show panel, user inbox) leaves IsTool=false, so the
	// bead_chat partial renders those lines as prose exactly as before. When
	// IsTool is true the partial renders a compact, collapsible tool entry (tool
	// name + args summary, optional result) styled distinctly from a prose bubble,
	// so an operator can see WHAT the plan agent did — not only what it said.
	IsTool     bool
	ToolName   string // e.g. "Bash", "Read", "Grep" — the summary badge
	ToolArgs   string // compact one-line arg summary (coop's extract_tool_label)
	ToolResult string // optional result summary shown when the entry is expanded
}

// mapChatView is a chat-history panel's view-model. Messages are oldest→newest
// (chat-log order, newest at the bottom). OlderHidden is how many in-scope
// messages were dropped by the newest-N cap; when non-zero the template shows a
// "N older not shown" note at the top. HasMessages gates the whole panel.
type mapChatView struct {
	Messages    []chatMessage
	Shown       int
	Total       int
	OlderHidden bool
	HiddenCount int
	HasMessages bool
}

// isMessageBeadDetail reports whether a hydrated bead is a type=message bead.
// A nil bead is not a message (we can't render its text either way).
func isMessageBeadDetail(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Type == "message"
}

// projectChatMessage turns a hydrated message bead into a rendered chat line:
// literal text (fields.text → description → title, linkified), author handle
// (fields.source_agent → created_by → operated_by display), a kind badge, and
// the relative + absolute timestamps.
// opts carries the per-bead linkifier gates (which repo a bare `!N` MR ref
// resolves against, or whether to leave it as plain text) — pass the zero
// value from surfaces that span projects and so can't pin one down.
func projectChatMessage(b *beadsapi.BeadDetail, basePath string, now time.Time, opts LinkifyOpts) chatMessage {
	text := messageBodyText(b)
	author := messageAuthor(b)
	ts, _ := pickAgeTimestamp(b.CreatedAt, b.UpdatedAt)

	m := chatMessage{
		ID:        b.ID,
		Href:      basePath + "/" + b.ID,
		Author:    author,
		Kind:      messageKind(b),
		Direction: messageDirection(b),
		BodyHTML:  LinkifyTextWithOpts(text, basePath, opts),
	}
	if !ts.IsZero() {
		m.Ago = fmtTimeAgo(ts, now)
		m.TimestampRFC = ts.UTC().Format(time.RFC3339)
	}
	return m
}

// messageBodyText extracts the literal chat text of a message bead: the squawk
// fields.text first (where gb squawk / the bridge store the literal line), then
// the bead description, then the title as a last resort. Whitespace-trimmed.
func messageBodyText(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if b.Fields != nil {
		if t := strings.TrimSpace(b.Fields["text"]); t != "" {
			return t
		}
	}
	if d := strings.TrimSpace(b.Description); d != "" {
		return d
	}
	return strings.TrimSpace(b.Title)
}

// messageAuthor returns a compact author handle for a message line: a mail
// bead's `from:<sender>` label first (mail carries no source_agent), then the
// source_agent field (set by gb squawk and the Slack bridge), then created_by,
// then the resolved operated_by display / raw operated_by with its provider
// prefix stripped. Returns "" when nothing is attributable.
func messageAuthor(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if from := mailFromLabel(b); from != "" {
		return from
	}
	if b.Fields != nil {
		if a := strings.TrimSpace(b.Fields["source_agent"]); a != "" {
			return a
		}
	}
	if c := strings.TrimSpace(b.CreatedBy); c != "" {
		return c
	}
	cand := strings.TrimSpace(b.OperatedByDisplay)
	if cand == "" {
		cand = strings.TrimSpace(b.OperatedBy)
	}
	if i := strings.Index(cand, ":"); i >= 0 && i < len(cand)-1 {
		cand = cand[i+1:]
	}
	return cand
}

// messageKind classifies a message bead for its panel badge. A type=mail bead
// reads as "mail"; a "squawk" label wins next; a Slack-sourced message reads as
// "slack" — recognized either by a legacy source label/field OR by the
// slack-bridge conversation labels the unified thread-reply / bot-mention path
// stamps (slack-thread-reply / slack-mention / slack-thread-message; see
// recordUnifiedThreadReply in internal/bridge/bot_thread_forward.go). Those
// human-authored replies carry NO "source:slack" label and NO fields.source, so
// without this arm they degraded to a generic "message" badge (ka-0000000676).
// Everything else is a generic "message".
func messageKind(b *beadsapi.BeadDetail) string {
	if b == nil {
		return "message"
	}
	if strings.EqualFold(b.Type, "mail") {
		return "mail"
	}
	for _, l := range b.Labels {
		if strings.EqualFold(l, "squawk") {
			return "squawk"
		}
		if strings.HasPrefix(strings.ToLower(l), "source:slack") {
			return "slack"
		}
		if isSlackConversationLabel(l) {
			return "slack"
		}
	}
	if b.Fields != nil {
		if strings.Contains(strings.ToLower(b.Fields["source"]), "slack") {
			return "slack"
		}
	}
	return "message"
}

// slackConversationLabels are the labels the slack-bridge stamps on the unified
// conversation-message bead for a human's thread reply or @-mention
// (recordUnifiedThreadReply). Any one of them marks the bead as a Slack-sourced
// message for the kind badge AND as a user-authored (from-user) chat line.
var slackConversationLabels = []string{
	"slack-thread-reply",
	"slack-mention",
	"slack-thread-message",
}

// isSlackConversationLabel reports whether a label is one of the slack-bridge
// conversation-message labels (case-insensitive).
func isSlackConversationLabel(label string) bool {
	for _, s := range slackConversationLabels {
		if strings.EqualFold(label, s) {
			return true
		}
	}
	return false
}

// messageDirection classifies a chat line as user-authored (directionFromUser)
// or agent-authored (directionFromAgent) for the bead-view Chat panel, WITHOUT
// depending on the per-page inbox query wiring that user_inbox.go uses. The
// signal, in priority order:
//
//   - A slack-bridge conversation label (slack-thread-reply / slack-mention /
//     slack-thread-message) marks a HUMAN reply forwarded from Slack — the
//     bead is created_by="slack-bridge" with the human's operated_by, and it
//     carries no source_agent. These are from-user.
//   - fields.source_agent set marks an AGENT squawk (gb squawk stamps it) —
//     from-agent, even though operated_by is the human who spawned the agent.
//   - created_by="slack-bridge" (a Slack-forwarded message without the
//     conversation label, e.g. a raw mention drop) is from-user.
//   - Otherwise attribute to the agent side: squawks/mail/delivery reports and
//     anything else authored by an agent are the common case on a bead view.
//
// Returns "" only for a nil bead (the template then renders it undirected).
func messageDirection(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if slices.ContainsFunc(b.Labels, isSlackConversationLabel) {
		return directionFromUser
	}
	if b.Fields != nil {
		if strings.TrimSpace(b.Fields["source_agent"]) != "" {
			return directionFromAgent
		}
	}
	if strings.EqualFold(strings.TrimSpace(b.CreatedBy), "slack-bridge") {
		return directionFromUser
	}
	return directionFromAgent
}

// mailFromLabel extracts the sender handle from a mail bead's `from:<sender>`
// label (set by `gb mail send`). Returns "" when no such label is present.
func mailFromLabel(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	for _, l := range b.Labels {
		if from, ok := strings.CutPrefix(l, "from:"); ok {
			if from = strings.TrimSpace(from); from != "" {
				return from
			}
		}
	}
	return ""
}
