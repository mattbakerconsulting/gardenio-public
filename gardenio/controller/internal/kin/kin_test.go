package kin_test

import (
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/kin"
)

const head = "gd-wishSlack"

// threadRows is what kin.Query(head) returns for the fixture wish: every member that carries thread_id=head, newest
// first, as the daemon sorts it. The MR carries none, so it is not kin.
func threadRows(f commonstest.Family) []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	for _, b := range f.Beads {
		if b.Fields["thread_id"] == head {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b *beadsapi.BeadDetail) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// The fixture wish's kin read from its head: the two replies, three squawks and the card in one conversation, oldest
// first, each in its voice and via; the task as work, the agent live, one session, and no Head (the head is the bead).
func TestOfWishHead(t *testing.T) {
	f := commonstest.WishFamily(head, "gardenio", "gd-chGeneral", time.Unix(1_790_000_000, 0))
	d := kin.Of(f.Bead(head), f.Bead(head), threadRows(f))
	type want struct{ id, author, voice, via string }
	wants := []want{
		{head + commonstest.WishReply1, "oidc:matt@example.com", kin.VoicePerson, kin.ViaSlack},
		{head + commonstest.WishCard, "agent:gardenio-one-bead-view-k2p", kin.VoiceAgent, kin.ViaCard},
		{head + commonstest.WishSquawk1, "agent:gardenio-one-bead-view-k2p", kin.VoiceAgent, kin.ViaSquawk},
		{head + commonstest.WishSquawk2, "agent:gardenio-one-bead-view-k2p", kin.VoiceAgent, kin.ViaSquawk},
		{head + commonstest.WishReply2, "oidc:matt@example.com", kin.VoicePerson, kin.ViaSlack},
		{head + commonstest.WishSquawk3, "agent:gardenio-one-bead-view-k2p", kin.VoiceAgent, kin.ViaSquawk},
	}
	if len(d.Conversation) != len(wants) {
		t.Fatalf("conversation = %+v, want %d lines", d.Conversation, len(wants))
	}
	for i, w := range wants {
		l := d.Conversation[i]
		if l.ID != w.id || l.Author != w.author || l.Voice != w.voice || l.Via != w.via || l.Text == "" || l.At == 0 {
			t.Errorf("line %d = %+v, want %+v with text and time", i, l, w)
		}
	}
	if card := d.Conversation[1]; card.SlackTS == "" || card.Subject != head+commonstest.WishAgent {
		t.Errorf("card = %+v, want its slack_ts and its agent as subject", card)
	}
	if d.Head != nil {
		t.Errorf("Head = %+v, want none on the head itself", d.Head)
	}
	if len(d.Work) != 1 || d.Work[0].ID != head+commonstest.WishTask || d.Work[0].State != "in_progress" {
		t.Errorf("work = %+v, want the task in progress", d.Work)
	}
	if len(d.Agents) != 1 || d.Agents[0].Name != "gardenio-one-bead-view-k2p" || !d.Agents[0].Live || d.Agents[0].State != "working" {
		t.Errorf("agents = %+v, want the crew live and working", d.Agents)
	}
	if d.Sessions != 1 || d.More {
		t.Errorf("sessions = %d, more = %v, want 1 and false", d.Sessions, d.More)
	}
}

// Read from a member, the kin name the head and where it was asked, and leave the member out of its own work list.
func TestOfMemberNamesItsHead(t *testing.T) {
	f := commonstest.WishFamily(head, "gardenio", "gd-chGeneral", time.Unix(1_790_000_000, 0))
	task := f.Bead(head + commonstest.WishTask)
	d := kin.Of(task, f.Bead(head), threadRows(f))
	if d.Head == nil || d.Head.ID != head || d.Head.Where != "slack" || d.Head.Channel != "C0GARDEN01" || d.Head.Title == "" {
		t.Errorf("Head = %+v, want the wish asked in Slack C0GARDEN01", d.Head)
	}
	if len(d.Work) != 0 {
		t.Errorf("work = %+v, want the task left out of its own kin", d.Work)
	}
	if len(d.Conversation) != 6 {
		t.Errorf("conversation has %d lines, want 6", len(d.Conversation))
	}

	commons := &beadsapi.BeadDetail{ID: "gd-w", Type: "task", Labels: []string{"project:gardenio", "thread"},
		Fields: map[string]string{"channel_id": "gd-chGeneral"}}
	if h := kin.Of(task, commons, nil).Head; h == nil || h.Where != "commons" || h.Channel != "" {
		t.Errorf("Commons head = %+v, want where=commons", h)
	}
	if d := kin.Of(task, nil, nil); d.Head != nil || d.Conversation == nil {
		t.Errorf("no head read = %+v, want no Head and an empty conversation", d)
	}
}

// A thread with more than Max kin says so.
func TestOfMore(t *testing.T) {
	rows := make([]*beadsapi.BeadDetail, kin.Max)
	for i := range rows {
		rows[i] = &beadsapi.BeadDetail{ID: "gd-s", Type: "session"}
	}
	if d := kin.Of(nil, nil, rows); !d.More || d.Sessions != kin.Max {
		t.Errorf("more = %v, sessions = %d, want true and %d", d.More, d.Sessions, kin.Max)
	}
}

func TestSpeaker(t *testing.T) {
	for _, c := range []struct {
		name               string
		b                  beadsapi.BeadDetail
		author, voice, via string
	}{
		{"squawk names its from: label", beadsapi.BeadDetail{CreatedBy: "gardenio-crew-a", Labels: []string{"project:gardenio", "squawk", "from:gardenio-crew-a"}},
			"agent:gardenio-crew-a", kin.VoiceAgent, kin.ViaSquawk},
		{"squawk without from: names source_agent", beadsapi.BeadDetail{CreatedBy: "gardenio-crew-b", Labels: []string{"squawk"},
			Fields: map[string]string{"source_agent": "gardenio-crew-b"}}, "agent:gardenio-crew-b", kin.VoiceAgent, kin.ViaSquawk},
		{"squawk a relay wrote names its from: label", beadsapi.BeadDetail{CreatedBy: "machine:gb", Labels: []string{"squawk", "from:gardenio-crew-a"}},
			"agent:gardenio-crew-a", kin.VoiceAgent, kin.ViaSquawk},
		{"squawk naming another agent is its writer's", beadsapi.BeadDetail{CreatedBy: "gardenio-crew-x", Labels: []string{"squawk", "from:gardenio-crew-a"}},
			"agent:gardenio-crew-x", kin.VoiceAgent, kin.ViaSquawk},
		{"squawk a person wrote naming an agent is the person's", beadsapi.BeadDetail{CreatedBy: "ada@example.com", Labels: []string{"squawk", "from:gardenio-crew-a"}},
			"ada@example.com", kin.VoicePerson, kin.ViaSquawk},
		{"an agent's say through the bridge", beadsapi.BeadDetail{CreatedBy: "slack-bridge", Labels: []string{"project:gardenio"},
			Fields: map[string]string{"author": "agent:thread-1", "kind": "text", "origin": "cli"}}, "agent:thread-1", kin.VoiceAgent, kin.ViaSay},
		{"a card", beadsapi.BeadDetail{CreatedBy: "slack-bridge", Fields: map[string]string{"author": "agent:crew-c", "kind": "card"}},
			"agent:crew-c", kin.VoiceAgent, kin.ViaCard},
		{"a Commons message", beadsapi.BeadDetail{CreatedBy: "ada@example.com", Labels: []string{"project:gardenio"},
			Fields: map[string]string{"author": "oidc:ada@example.com", "origin": "commons"}}, "oidc:ada@example.com", kin.VoicePerson, kin.ViaCommons},
		{"the Slack mirror of a person", beadsapi.BeadDetail{CreatedBy: "slack-bridge",
			Fields: map[string]string{"author": "oidc:ada@example.com", "origin": "slack", "slack_user_id": "U1"}}, "oidc:ada@example.com", kin.VoicePerson, kin.ViaSlack},
		{"a forwarded reply with no operator", beadsapi.BeadDetail{CreatedBy: "slack-bridge", Labels: []string{"slack-thread-reply"},
			Fields: map[string]string{"slack_user_id": "U1"}}, "slack:U1", kin.VoicePerson, kin.ViaSlack},
		{"a reply a person wrote", beadsapi.BeadDetail{CreatedBy: "ada@example.com", Labels: []string{"slack-thread-reply"},
			Fields: map[string]string{"operated_by": "oidc:bob@example.com"}}, "ada@example.com", kin.VoicePerson, kin.ViaSlack},
		{"a steward run", beadsapi.BeadDetail{CreatedBy: "gardenio-steward-x7", Labels: []string{"project:gardenio"},
			Fields: map[string]string{"author": "agent:gardenio-steward-x7", "origin": "commons"}}, "agent:gardenio-steward-x7", kin.VoiceSteward, kin.ViaCommons},
		{"another land's steward is an agent", beadsapi.BeadDetail{CreatedBy: "claimlayer-steward-x7", Labels: []string{"project:gardenio"}},
			"claimlayer-steward-x7", kin.VoiceAgent, kin.ViaSay},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.b.Fields == nil {
				c.b.Fields = map[string]string{}
			}
			author, voice, via := kin.Speaker(&c.b)
			if author != c.author || voice != c.voice || via != c.via {
				t.Errorf("Speaker = %q %q %q, want %q %q %q", author, voice, via, c.author, c.voice, c.via)
			}
		})
	}
}

func TestThreadOf(t *testing.T) {
	for _, c := range []struct {
		b    *beadsapi.BeadDetail
		want string
	}{
		{&beadsapi.BeadDetail{ID: "gd-h", Type: "task", Labels: []string{"thread"}}, "gd-h"},
		{&beadsapi.BeadDetail{ID: "gd-m", Type: "message", Labels: []string{"thread"}, Fields: map[string]string{"thread_id": "gd-h"}}, "gd-h"},
		{&beadsapi.BeadDetail{ID: "gd-t", Type: "task", Fields: map[string]string{"thread_id": " gd-h "}}, "gd-h"},
		{&beadsapi.BeadDetail{ID: "gd-c", Type: "channel", Labels: []string{"thread"}}, ""},
		{nil, ""},
	} {
		if got := kin.ThreadOf(c.b); got != c.want {
			t.Errorf("ThreadOf(%+v) = %q, want %q", c.b, got, c.want)
		}
	}
}

// A plan's steps in the thread name it as their parent, so the roots of the thread's work are the rows with none; each
// row carries its pull request's state.
func TestOfWorkParents(t *testing.T) {
	row := func(id, typ string, f map[string]string, labels ...string) *beadsapi.BeadDetail {
		if f == nil {
			f = map[string]string{}
		}
		f["thread_id"] = head
		return &beadsapi.BeadDetail{ID: id, Type: typ, Status: "open", Labels: labels, Fields: f}
	}
	d := kin.Of(nil, nil, []*beadsapi.BeadDetail{
		row("gd-plan", "epic", map[string]string{"plan_state": "building", "step_order": `["gd-s1","gd-s2","gd-plan"]`}, "plan"),
		row("gd-s1", "task", map[string]string{"mr_url": "https://github.com/a/b/pull/1", "mr_state": "merged"}),
		row("gd-s2", "task", nil),
		row("gd-loose", "bug", nil),
	})
	got := map[string]kin.Work{}
	for _, w := range d.Work {
		got[w.ID] = w
	}
	if got["gd-plan"].Parent != "" || got["gd-plan"].Type != "plan" || got["gd-loose"].Parent != "" {
		t.Errorf("roots = %+v %+v, want the plan and the loose bug with no parent", got["gd-plan"], got["gd-loose"])
	}
	if got["gd-s1"].Parent != "gd-plan" || got["gd-s2"].Parent != "gd-plan" || got["gd-s1"].PRState != "merged" {
		t.Errorf("steps = %+v %+v, want both under the plan and s1's PR merged", got["gd-s1"], got["gd-s2"])
	}
}

// Summaries over many heads' kin in one read (the steward's Wishes, gd-ak4P2qXMss): the fixture wish counts its replies
// and squawks but not its card, its last line is the crew's last squawk, its agent is live; a second head with no kin
// is its asker's (a Slack head the bridge minted is the person's) and has no agent; a row bound to no listed head, as a
// daemon that ignores field_in returns, is not counted; a dead agent does not displace a live one.
func TestSummaries(t *testing.T) {
	t0 := time.Unix(1_790_000_000, 0)
	f := commonstest.WishFamily(head, "gardenio", "gd-chGeneral", t0)
	quiet := &beadsapi.BeadDetail{ID: "gd-quiet", Type: "task", Status: "open", CreatedBy: "slack-bridge", CreatedAt: t0, Title: "Slack thread: dark mode?",
		Labels: []string{"project:gardenio", "thread"}, Fields: map[string]string{"slack_root_text": "dark mode?", "operated_by": "oidc:ann@example.com"}}
	rows := append(threadRows(f),
		&beadsapi.BeadDetail{ID: "gd-old", Type: "agent", Status: "closed", CreatedAt: t0.Add(time.Hour), Fields: map[string]string{"thread_id": head, "agent": "gardenio-old", "agent_state": "done"}},
		&beadsapi.BeadDetail{ID: "gd-stray", Type: "message", CreatedAt: t0.Add(time.Hour), Fields: map[string]string{"thread_id": "gd-other", "text": "elsewhere"}})
	got := kin.Summaries([]*beadsapi.BeadDetail{f.Bead(head), quiet}, rows)
	w := got[head]
	if w == nil || w.Replies != 5 || w.More {
		t.Fatalf("wish = %+v, want 5 replies (2 replies, 3 squawks; not the card)", w)
	}
	if w.Last.ID != head+commonstest.WishSquawk3 || w.Last.Voice != kin.VoiceAgent || w.Last.Author != "agent:gardenio-one-bead-view-k2p" {
		t.Errorf("wish last = %+v, want the crew's last squawk", w.Last)
	}
	if w.Agent == nil || w.Agent.ID != head+commonstest.WishAgent || !w.Agent.Live {
		t.Errorf("wish agent = %+v, want the live crew, not the done one after it", w.Agent)
	}
	q := got["gd-quiet"]
	if q == nil || q.Replies != 0 || q.Agent != nil || q.Last.ID != "gd-quiet" || q.Last.Voice != kin.VoicePerson || q.Last.Author != "oidc:ann@example.com" || q.Last.Text != "dark mode?" {
		t.Errorf("quiet = %+v, want its asker's opening line and no agent", q)
	}
	if _, ok := got["gd-other"]; ok || len(got) != 2 {
		t.Errorf("summaries = %v, want only the two heads", got)
	}
	if q := kin.SummaryQuery([]string{head}); q.FieldsIn["thread_id"][0] != head || q.Limit != kin.Max || !slices.Contains(q.FieldsOnly, "agent_state") {
		t.Errorf("query = %+v", q)
	}
}

// The messages that name a bead (gd-UXDp9GUeMB): its reverse mentions' messages once each, itself and other types left
// out, merged into the conversation via mention in time order, a message the thread already holds not repeated.
func TestMentioned(t *testing.T) {
	refs := []beadsapi.BeadReferenceSummary{{ID: "gd-m1", Type: "message"}, {ID: "gd-task", Type: "task"}, {ID: "gd-m1", Type: "message"},
		{ID: "gd-self", Type: "message"}, {ID: "gd-m2", Type: "message"}}
	if got := kin.MentionIDs("gd-self", refs); !slices.Equal(got, []string{"gd-m1", "gd-m2"}) {
		t.Errorf("MentionIDs = %v, want gd-m1, gd-m2", got)
	}
	f := commonstest.WishFamily(head, "gardenio", "gd-chGeneral", time.Unix(1_790_000_000, 0))
	d := kin.Of(f.Bead(head), f.Bead(head), threadRows(f))
	first, last := d.Conversation[0], d.Conversation[len(d.Conversation)-1]
	early := &beadsapi.BeadDetail{ID: "gd-m1", Type: "message", Labels: []string{"squawk", "from:other-crew"}, CreatedBy: "other-crew",
		CreatedAt: time.Unix(first.At-60, 0), Fields: map[string]string{"text": "see " + head}}
	again := f.Bead(last.ID)
	d.Mentioned([]*beadsapi.BeadDetail{early, again, nil, {ID: "gd-t", Type: "task"}})
	var ids, vias []string
	for _, l := range d.Conversation {
		ids, vias = append(ids, l.ID), append(vias, l.Via)
	}
	if ids[0] != "gd-m1" || vias[0] != kin.ViaMention || d.Conversation[0].Author != "agent:other-crew" || d.Conversation[0].Text != "see "+head {
		t.Errorf("first line = %+v, want gd-m1 by other-crew via mention", d.Conversation[0])
	}
	if len(ids) != 7 || ids[6] != last.ID || vias[6] != last.Via {
		t.Errorf("conversation = %v (%v), want the six thread lines after gd-m1, the last one not repeated", ids, vias)
	}
}

// An agent's own messages and mail join its conversation (gd-GTqYxAem2S, the viewer's agentAuthoredMessagesQuery):
// a squawk and a mail it wrote, by its from: label, in time order; one another writer labelled from:<agent> is not its.
func TestAuthored(t *testing.T) {
	if q := kin.AuthoredQuery("crew-a"); !slices.Equal(q.Labels, []string{"from:crew-a"}) || !slices.Equal(q.Types, []string{"message", "mail"}) {
		t.Errorf("AuthoredQuery = %+v, want messages and mail labelled from:crew-a", q)
	}
	at := func(s int64) time.Time { return time.Unix(1_790_000_000+s, 0) }
	d := &kin.Doc{Conversation: []kin.Line{{ID: "gd-said", At: at(20).Unix()}}}
	d.Authored("crew-a", []*beadsapi.BeadDetail{
		{ID: "gd-sq", Type: "message", Labels: []string{"squawk", "from:crew-a"}, CreatedBy: "crew-a", CreatedAt: at(30), Fields: map[string]string{"text": "done"}},
		{ID: "gd-mail", Type: "mail", Title: "need a review", Labels: []string{"from:crew-a"}, CreatedBy: "crew-a", CreatedAt: at(10)},
		{ID: "gd-fake", Type: "message", Labels: []string{"squawk", "from:crew-a"}, CreatedBy: "crew-b", CreatedAt: at(40)},
		{ID: "gd-said", Type: "message", Labels: []string{"from:crew-a"}, CreatedBy: "crew-a", CreatedAt: at(20)},
	})
	var got []string
	for _, l := range d.Conversation {
		got = append(got, l.ID+" "+l.Via+" "+l.Text)
	}
	if want := []string{"gd-mail mail need a review", "gd-said  ", "gd-sq squawk done"}; !slices.Equal(got, want) {
		t.Errorf("conversation = %q, want %q", got, want)
	}
}

// Unbound is the closure members the thread_id list did not return, once each, in closure order.
func TestUnbound(t *testing.T) {
	rows := []*beadsapi.BeadDetail{{ID: "gd-a"}, nil, {ID: "gd-c"}}
	if got := kin.Unbound([]string{"gd-a", "gd-b", "", "gd-c", "gd-d", "gd-b"}, rows); !slices.Equal(got, []string{"gd-b", "gd-d"}) {
		t.Errorf("Unbound = %v, want gd-b, gd-d", got)
	}
}
