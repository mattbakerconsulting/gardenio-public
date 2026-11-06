// Package kin reads a thread's kin: the beads bound to its head by
// fields.thread_id or parent-child (the replies, squawks, says and cards of
// its conversation, the work it gave rise to, its agents and sessions), and
// says who wrote each message. The bead panel (gardenhub) and the Commons both project through it,
// so they agree on who wrote what (plan gd-gO1fTT7oGC, step gd-58mkDiWkNq).
package kin

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

const (
	// Max is how many of a thread's newest kin one read pulls.
	Max = 500
	// TextMax is the rune cap on one conversation line's text.
	TextMax = 4000
)

// Voices: who a conversation line speaks as.
const (
	VoicePerson  = "person"
	VoiceAgent   = "agent"
	VoiceSteward = "steward"
)

// Vias: how a conversation line reached the thread.
const (
	ViaSlack   = "slack"   // a person's Slack reply the bridge forwarded or mirrored
	ViaSquawk  = "squawk"  // gb squawk: label squawk, from:<agent>
	ViaCard    = "card"    // a card the bridge posted (kind=card): a state line, never prose
	ViaCommons = "commons" // written in the Commons (origin=commons)
	ViaSay     = "say"     // anything else an agent or relay wrote into the thread (gb say, the Glass)
	ViaMention = "mention" // a message out of the thread that names the bead (the daemon's reverse mentions)
	ViaMail    = "mail"    // a mail bead (gb mail send), its subject the text
)

// Doc is a thread's kin as one block. Conversation is oldest first; Work and
// Agents are in the order the thread gained them.
type Doc struct {
	// Head is the thread head, set when the bead read is not the head itself.
	Head         *Head   `json:"head,omitempty"`
	Conversation []Line  `json:"conversation"`
	Work         []Work  `json:"work,omitempty"`
	Agents       []Agent `json:"agents,omitempty"`
	Sessions     int     `json:"sessions,omitempty"`
	// More says the read hit Max: older kin are not in the block.
	More bool `json:"more,omitempty"`
	// NoThread says the bead is in no thread: Conversation is only the messages that name it (Mentioned) or, for
	// an agent, that it wrote (Authored).
	NoThread bool `json:"no_thread,omitempty"`
}

// Head is the thread head and where it was asked: Where is "slack" (Channel
// the Slack channel id) or "commons" (a Commons wish, asked in #general).
type Head struct {
	ID         string `json:"id"`
	Title      string `json:"title,omitempty"`
	ShortTitle string `json:"short_title,omitempty"`
	Where      string `json:"where,omitempty"`
	Channel    string `json:"channel,omitempty"`
}

// Line is one message of the thread.
type Line struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Voice   string `json:"voice"`
	Via     string `json:"via"`
	Text    string `json:"text,omitempty"`
	At      int64  `json:"at,omitempty"`
	SlackTS string `json:"slack_ts,omitempty"`
	// Subject is a card's subject bead (an agent, a decision).
	Subject string `json:"subject,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

// Work is a task, epic or plan bound to the thread.
type Work struct {
	ID         string `json:"id"`
	Title      string `json:"title,omitempty"`
	ShortTitle string `json:"short_title,omitempty"`
	Type       string `json:"type"` // the bead type, or "plan" for an epic labelled plan
	State      string `json:"state"`
	PR         string `json:"pr,omitempty"`
	PRState    string `json:"pr_state,omitempty"` // mr_state: opened, merged, closed
	// Parent is the plan or epic in Work whose step_order or phase_order
	// names this row; "" for a root of the thread's work.
	Parent string `json:"parent,omitempty"`
}

// Agent is an agent bound to the thread.
type Agent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state,omitempty"`
	Live  bool   `json:"live,omitempty"`
}

// workTypes are the bead types the Work list holds.
var workTypes = map[string]bool{"task": true, "bug": true, "feature": true, "chore": true, "epic": true}

// Query is the one list read of head's kin: the newest Max beads that carry
// thread_id=head, of every type.
func Query(head string) beadsapi.ListBeadsQuery {
	return beadsapi.ListBeadsQuery{Fields: map[string]string{"thread_id": head}, Sort: "-created_at", Limit: Max}
}

// Summary is a thread's kin as a list of threads shows it: how many messages
// it has (cards are state lines, not messages), its last line (the head's own
// when nothing answers it) and the agent on it.
type Summary struct {
	Replies int
	Last    Line
	// Agent is its live agent, else the newest bound to it; nil for none.
	Agent *Agent
	// More says the read hit its limit: Replies is a floor.
	More bool
}

// SummaryQuery is the one list read of many heads' kin for Summaries: the
// newest Max messages and agents that carry thread_id in heads, with only the
// fields Speaker and the summary read.
func SummaryQuery(heads []string) beadsapi.ListBeadsQuery {
	return beadsapi.ListBeadsQuery{Types: []string{"message", "agent"}, FieldsIn: map[string][]string{"thread_id": heads},
		Sort: "-created_at", Limit: Max, NoTotal: true}.OnlyFields("thread_id", "channel_id", "text", "author", "kind", "origin", "deleted",
		"slack_user_id", "operated_by", "source_agent", "agent", "agent_state")
}

// Summaries summarises each of heads from rows, the result of
// SummaryQuery(their ids): a row bound to no head in heads is not counted, as
// a daemon that ignores field_in answers with every row.
func Summaries(heads, rows []*beadsapi.BeadDetail) map[string]*Summary {
	out := map[string]*Summary{}
	for _, h := range heads {
		if h != nil {
			out[h.ID] = &Summary{Last: AskLine(h), More: len(rows) >= Max}
		}
	}
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b *beadsapi.BeadDetail) int {
		if a == nil || b == nil {
			return 0
		}
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	for _, b := range rows {
		s := out[ThreadOf(b)]
		if s == nil || b.Fields["deleted"] == "true" {
			continue
		}
		switch {
		case b.Type == "agent":
			if a := agentOf(b); s.Agent == nil || a.Live || !s.Agent.Live {
				s.Agent = &a
			}
		case b.Type == "message" && b.Fields["kind"] != "card":
			s.Replies++
			s.Last = lineOf(b)
		}
	}
	return out
}

// AskLine is a head as its thread's first line: a head the bridge minted from
// Slack (a relay wrote it, or it names its Slack thread) is the person's who
// asked, named as their reply is, its words their opening message.
func AskLine(h *beadsapi.BeadDetail) Line {
	l := lineOf(h)
	l.Text = clip(firstNonEmpty(h.Fields["text"], h.Fields["slack_root_text"], h.Title), TextMax)
	if strings.TrimSpace(h.Fields["author"]) == "" && (RelayWriter(h.CreatedBy) || slackChannelOf(h) != "") {
		l.Author, l.Voice, l.Via = replyAuthor(h), VoicePerson, ViaSlack
	}
	return l
}

// ThreadOf is the thread head whose kin b shows: b itself when it is a wish
// head, else its fields.thread_id; "" for a bead in no thread.
func ThreadOf(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if IsWishHead(b) {
		return b.ID
	}
	return strings.TrimSpace(b.Fields["thread_id"])
}

// IsWishHead reports whether b is a thread (wish) head: any bead but a message
// or a channel that carries the thread label (internal/bridge/thread_bead.go).
func IsWishHead(b *beadsapi.BeadDetail) bool {
	if b == nil || b.Type == "message" || b.Type == "channel" {
		return false
	}
	return slices.Contains(b.Labels, "thread")
}

// Of projects rows, the result of Query(head), for the bead self: head is the
// head bead when self is not it (nil leaves Head out). A row that is self or
// the head is not repeated in Work or Agents.
func Of(self, head *beadsapi.BeadDetail, rows []*beadsapi.BeadDetail) *Doc {
	d := &Doc{Conversation: []Line{}, More: len(rows) >= Max}
	if head != nil && (self == nil || head.ID != self.ID) {
		d.Head = headOf(head)
	}
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b *beadsapi.BeadDetail) int {
		if a == nil || b == nil {
			return 0
		}
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	skip := func(b *beadsapi.BeadDetail) bool {
		return (self != nil && b.ID == self.ID) || (head != nil && b.ID == head.ID)
	}
	for _, b := range rows {
		if b == nil {
			continue
		}
		switch {
		case b.Type == "message":
			d.Conversation = append(d.Conversation, lineOf(b))
		case b.Type == "session":
			d.Sessions++
		case b.Type == "agent" && !skip(b):
			d.Agents = append(d.Agents, agentOf(b))
		case workTypes[b.Type] && !skip(b):
			d.Work = append(d.Work, workOf(b))
		}
	}
	parents := map[string]string{}
	for _, b := range rows {
		if b != nil && b.Type == "epic" && !skip(b) {
			for _, k := range []string{"step_order", "phase_order"} {
				for _, id := range idList(b.Fields[k]) {
					parents[id] = b.ID
				}
			}
		}
	}
	for i := range d.Work {
		if p := parents[d.Work[i].ID]; p != d.Work[i].ID {
			d.Work[i].Parent = p
		}
	}
	return d
}

// MentionMax is how many of the messages out of the thread one read asks for: a bead's reverse mentions, an
// agent's own messages.
const MentionMax = 50

// MentionIDs is the ids of the messages among refs, a bead's reverse mentions, once each and self left out: the
// squawks and Slack drops that name the bead in their text (as the viewer's bead chat gathers them,
// cmd/beads-viewer bead_chat.go collectMessageRefIDs).
func MentionIDs(self string, refs []beadsapi.BeadReferenceSummary) []string {
	var ids []string
	for _, r := range refs {
		if r.Type == "message" && r.ID != "" && r.ID != self && !slices.Contains(ids, r.ID) {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// Mentioned merges rows, the messages that name the bead (MentionIDs, read), into d's conversation via mention.
func (d *Doc) Mentioned(rows []*beadsapi.BeadDetail) {
	d.merge(rows, func(b *beadsapi.BeadDetail) (Line, bool) {
		l := lineOf(b)
		l.Via = ViaMention
		return l, b.Type == "message"
	})
}

// AuthoredQuery is the one list read of the messages and mail agent (an agent bead's title) wrote, which carry
// from:<agent> and may carry no thread_id and not name the agent bead (cmd/beads-viewer agentAuthoredMessagesQuery).
func AuthoredQuery(agent string) beadsapi.ListBeadsQuery {
	return beadsapi.ListBeadsQuery{Types: []string{"message", "mail"}, Labels: []string{"from:" + agent}, Sort: "-created_at",
		Limit: MentionMax, NoTotal: true}
}

// Authored merges rows, the result of AuthoredQuery(agent), into d's conversation: the messages and mail labelled
// from:<agent> that Speaker names as agent's, so a from: label another writer put on its message does not make it the
// agent's.
func (d *Doc) Authored(agent string, rows []*beadsapi.BeadDetail) {
	d.merge(rows, func(b *beadsapi.BeadDetail) (Line, bool) {
		l := lineOf(b)
		return l, (b.Type == "message" || b.Type == "mail") && slices.Contains(b.Labels, "from:"+agent) &&
			strings.EqualFold(strings.TrimPrefix(l.Author, "agent:"), agent)
	})
}

// merge adds the line of each row keep takes to d's conversation, in time order; a message the conversation already
// holds, as one of the thread's, is not repeated.
func (d *Doc) merge(rows []*beadsapi.BeadDetail, keep func(*beadsapi.BeadDetail) (Line, bool)) {
	have := map[string]bool{}
	for _, l := range d.Conversation {
		have[l.ID] = true
	}
	n := len(d.Conversation)
	for _, b := range rows {
		if b == nil || have[b.ID] {
			continue
		}
		if l, ok := keep(b); ok {
			have[b.ID] = true
			d.Conversation = append(d.Conversation, l)
		}
	}
	if len(d.Conversation) > n {
		slices.SortStableFunc(d.Conversation, func(a, b Line) int { return cmp.Compare(a.At, b.At) })
	}
}

// Unbound is the ids of closure, a thread's parent-child closure (the head left out), that no row of rows, the result
// of Query(head), holds: the half-links whose thread_id was never copied (the daemon copies it only onto a bead an
// agent in the thread creates), as cmd/beads-viewer threads.go threadMembers unions them in.
func Unbound(closure []string, rows []*beadsapi.BeadDetail) []string {
	have := map[string]bool{}
	for _, b := range rows {
		if b != nil {
			have[b.ID] = true
		}
	}
	var ids []string
	for _, id := range closure {
		if id != "" && !have[id] {
			have[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// idList is a JSON array of ids (step_order, phase_order), nil when it is not one.
func idList(raw string) []string {
	var ids []string
	if json.Unmarshal([]byte(raw), &ids) != nil {
		return nil
	}
	return ids
}

func headOf(b *beadsapi.BeadDetail) *Head {
	h := &Head{ID: b.ID, Title: b.Title, ShortTitle: strings.TrimSpace(b.ShortTitle)}
	h.Where, h.Channel = Where(b)
	return h
}

// Where is where a thread head was asked: "slack" and the Slack channel id,
// "commons" (a Commons wish, asked in #general) with no channel, or "" for a
// head with neither.
func Where(b *beadsapi.BeadDetail) (where, channel string) {
	if ch := slackChannelOf(b); ch != "" {
		return "slack", ch
	}
	if strings.TrimSpace(b.Fields["channel_id"]) != "" {
		return "commons", ""
	}
	return "", ""
}

// slackChannelOf is the Slack channel a head was asked in: its
// slack_thread_channel, else the channel of its slack-thread:<ch>:<ts> label.
func slackChannelOf(b *beadsapi.BeadDetail) string {
	if ch := strings.TrimSpace(b.Fields["slack_thread_channel"]); ch != "" {
		return ch
	}
	for _, l := range b.Labels {
		if rest, ok := strings.CutPrefix(l, "slack-thread:"); ok {
			if ch, _, _ := strings.Cut(rest, ":"); ch != "" {
				return ch
			}
		}
	}
	return ""
}

func lineOf(b *beadsapi.BeadDetail) Line {
	author, voice, via := Speaker(b)
	l := Line{ID: b.ID, Author: author, Voice: voice, Via: via, SlackTS: strings.TrimSpace(b.Fields["slack_ts"]),
		Subject: strings.TrimSpace(b.Fields["subject_id"]), Deleted: b.Fields["deleted"] == "true"}
	if !b.CreatedAt.IsZero() {
		l.At = b.CreatedAt.Unix()
	}
	if !l.Deleted {
		l.Text = clip(firstNonEmpty(b.Fields["text"], b.Title), TextMax)
	}
	return l
}

func workOf(b *beadsapi.BeadDetail) Work {
	w := Work{ID: b.ID, Title: b.Title, ShortTitle: strings.TrimSpace(b.ShortTitle), Type: b.Type,
		State: firstNonEmpty(b.Fields["plan_state"], b.Status), PR: strings.TrimSpace(b.Fields["mr_url"]),
		PRState: strings.TrimSpace(b.Fields["mr_state"])}
	if b.Type == "epic" && slices.Contains(b.Labels, "plan") {
		w.Type = "plan"
	}
	return w
}

// agentOf is the agent row; live as the Commons agent card reads it (an open
// bead whose agent_state is neither done nor failed).
func agentOf(b *beadsapi.BeadDetail) Agent {
	state := strings.TrimSpace(b.Fields["agent_state"])
	return Agent{ID: b.ID, Name: firstNonEmpty(b.Fields["agent"], b.Title, b.ID), State: state,
		Live: b.Status != "closed" && state != "done" && state != "failed"}
}

// Speaker is who a message speaks as, in which voice, and how it came:
//
//   - a squawk (label squawk) is the agent's, named from its from:<agent>
//     label, else source_agent, when its writer is that agent or a relay,
//     else from created_by (squawker);
//   - a card (kind=card) is the bridge's state line, in the agent voice;
//   - a Slack reply the bridge forwarded (label slack-thread-reply) carries
//     no author: it is the person's, named from created_by when a person
//     wrote it, else the operated_by the bridge stamped, else slack:<user id>;
//   - anything else is Author's, via commons when origin=commons, via slack
//     when the mirror wrote it from Slack, else via say.
//
// The voice is steward for one of the land's steward runs
// (<land>-steward-<suffix>), person for an oidc: author or an email, else agent.
func Speaker(b *beadsapi.BeadDetail) (author, voice, via string) {
	f := b.Fields
	switch {
	case slices.Contains(b.Labels, "squawk"):
		author, via = squawker(b), ViaSquawk
	case b.Type == "mail":
		author, via = Author(b), ViaMail
	case f["kind"] == "card":
		author, via = Author(b), ViaCard
	case slices.Contains(b.Labels, "slack-thread-reply") && strings.TrimSpace(f["author"]) == "":
		author, via = replyAuthor(b), ViaSlack
		return author, VoicePerson, via
	default:
		author = Author(b)
		switch {
		case f["origin"] == "commons":
			via = ViaCommons
		case f["origin"] == "slack" || strings.TrimSpace(f["slack_user_id"]) != "":
			via = ViaSlack
		default:
			via = ViaSay
		}
	}
	return author, VoiceOf(author, landOf(b.Labels)), via
}

// squawker names a squawk's agent. from: and source_agent are whatever the
// writer put there, so, as Author shows fields.author, they name it only when
// the daemon's created_by is that agent or a relay; else created_by does.
func squawker(b *beadsapi.BeadDetail) string {
	name, w := strings.TrimSpace(firstNonEmpty(fromLabel(b.Labels), b.Fields["source_agent"])), strings.TrimSpace(b.CreatedBy)
	if name != "" && (w == "" || RelayWriter(w) || namesWriter(name, w)) {
		return "agent:" + name
	}
	if strings.Contains(w, ":") || strings.Contains(w, "@") {
		return w
	}
	return "agent:" + w
}

func replyAuthor(b *beadsapi.BeadDetail) string {
	if w := strings.TrimSpace(b.CreatedBy); w != "" && !RelayWriter(w) {
		return w
	}
	if op := strings.TrimSpace(b.Fields["operated_by"]); strings.HasPrefix(op, "oidc:") {
		return op
	}
	if uid := strings.TrimSpace(b.Fields["slack_user_id"]); uid != "" {
		return "slack:" + uid
	}
	return strings.TrimSpace(b.CreatedBy)
}

// VoiceOf is the voice of a line by author in land: steward for one of the
// land's steward runs, person for an oidc:, slack: or email author, else agent.
func VoiceOf(author, land string) string {
	name := strings.TrimPrefix(strings.TrimPrefix(author, "agent:"), "oidc:")
	switch {
	case land != "" && strings.HasPrefix(name, land+"-steward-"):
		return VoiceSteward
	case strings.HasPrefix(author, "oidc:") || strings.HasPrefix(author, "slack:") || strings.Contains(author, "@"):
		return VoicePerson
	}
	return VoiceAgent
}

func fromLabel(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "from:"); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// landOf is the bead's single project label's value; "" for none or several.
func landOf(labels []string) string {
	land := ""
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			if land != "" && land != v {
				return ""
			}
			land = v
		}
	}
	return land
}

// Author is who a message shows as written by (gd-PjNjRhAHPn).
// fields.author is whatever its writer put there, so it is shown only when the
// daemon vouches for it, and otherwise created_by, the principal the daemon
// stamped from the writer's credential:
//
//   - an author that names its writer: created_by, or oidc:/agent:<created_by>.
//     That is every origin=commons message, which the daemon takes only from
//     the person it names (kbeads commons_origin.go), and an agent writing as
//     itself; a pre-gate origin=commons message naming someone else is not;
//   - a message a relay wrote: a bridge (a <name>-bridge fleet, bridge:<name>)
//     or a machine principal (machine:<client>), the only writers the daemon
//     lets name someone else (kbeads message_author.go), and names no agent
//     can hold. That is a person's Slack message the mirror wrote as theirs,
//     an agent's says and cards, and the controller's notices.
//
// A message with no created_by (none in a daemon with auth on) shows its author.
func Author(b *beadsapi.BeadDetail) string {
	author, writer := strings.TrimSpace(b.Fields["author"]), strings.TrimSpace(b.CreatedBy)
	if author == "" || writer == "" {
		return firstNonEmpty(author, writer)
	}
	if RelayWriter(writer) || namesWriter(author, writer) {
		return author
	}
	return writer
}

// RelayWriter reports whether created_by is a bridge or a machine principal.
func RelayWriter(createdBy string) bool {
	w := strings.ToLower(createdBy)
	return strings.HasPrefix(w, "bridge:") || strings.HasPrefix(w, "machine:") || strings.HasSuffix(w, "-bridge")
}

// namesWriter reports whether author names the principal created_by records:
// itself, or it under oidc: (a person, whose created_by is a bare email) or
// agent: (an agent, whose created_by is its name).
func namesWriter(author, createdBy string) bool {
	w := strings.TrimPrefix(createdBy, "oidc:")
	for _, name := range []string{w, "oidc:" + w, "agent:" + w} {
		if strings.EqualFold(author, name) {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
