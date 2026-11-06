package commonstest

import (
	"slices"
	"strconv"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Family is a wish head and its kin as the daemon holds them: the beads, the
// edges between them and each bead's kd comments.
type Family struct {
	Head     string
	Beads    []*beadsapi.BeadDetail
	Deps     []beadsapi.Dependency
	Comments map[string][]beadsapi.Comment
}

// The members of a WishFamily, by role, as suffixes of its head's id.
const (
	WishReply1  = "Rp1" // the person's first slack-thread-reply
	WishReply2  = "Rp2"
	WishSquawk1 = "Sq1" // the crew's squawks: label squawk, from:<agent>, no kind
	WishSquawk2 = "Sq2"
	WishSquawk3 = "Sq3"
	WishCard    = "Crd" // the agent's card, a kind=card message on the head's channel the bridge writes as the agent
	WishSession = "Ses"
	WishAgent   = "Agt" // the crew, bound to the thread by thread_id
	WishTask    = "Tsk" // the task the wish gave rise to, its parent the head
	WishMR      = "Mr"  // the task's merge-request, by a produces edge
)

// WishFamily is a Slack-origin wish head in land as the slack bridge mints
// it, posted in channel (the land's #general channel bead), with the kin a
// real thread gathers (modelled on gd-Rrf36anbQj, plan gd-gO1fTT7oGC step 1):
// two slack-thread-reply messages by the person, three squawks by the crew,
// the crew's card, its session and agent beads, the task it works with a
// produces merge-request, and two kd comments on the head. Each bead is minted
// with the fields and labels its writer sets and no others, so a reply carries
// no channel_id or author and a squawk no kind. The head is created at t0 and
// its kin at minutes after; member ids are head plus a Wish* suffix.
func WishFamily(head, land, channel string, t0 time.Time) Family {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	in := "project:" + land
	const (
		person   = "oidc:matt@example.com"
		slackUID = "U0FIXTURE01"
		slackCh  = "C0GARDEN01"
		bridge   = "slack-bridge"
	)
	agent := land + "-one-bead-view-k2p"
	root := "the workbench and bead viewer in " + land + " lands doesn't bring in related messages and wishes and squawks; " +
		"this would be most helpful for wish beads. Workbench chats should have the full chat, dense but also high level."
	ts := func(m int) string {
		return strconv.FormatInt(t0.Add(time.Duration(m)*time.Minute).Unix(), 10) + ".000100"
	}
	f := Family{Head: head, Comments: map[string][]beadsapi.Comment{}}
	put := func(b *beadsapi.BeadDetail) {
		if b.UpdatedAt.IsZero() {
			b.UpdatedAt = b.CreatedAt
		}
		f.Beads = append(f.Beads, b)
	}
	put(&beadsapi.BeadDetail{ID: head, Type: "task", Kind: "issue", Status: "open", Priority: 2, Assignee: bridge, CreatedBy: bridge, CreatedAt: at(0),
		Title:  "Slack thread: " + root[:110] + "...",
		Labels: []string{in, "slack-thread:" + slackCh + ":" + ts(0), "source:slack", "thread"},
		Fields: map[string]string{"channel_id": channel, "slack_root_text": root, "slack_root_at": ts(0), "slack_thread_channel": slackCh,
			"slack_thread_ts": ts(0), "slack_spawn_channel": slackCh, "slack_user_id": slackUID, "slack_opener_user_id": slackUID,
			"spawn_source": "slack-thread", "operated_by": person, "idempotency_key": "slack-thread:" + slackCh + ":" + ts(0)}})
	for i, r := range []struct {
		id, text string
		m        int
	}{
		{WishReply1, "and the steward's node should list the wishes of the land, not only the threads that say @steward", 3},
		{WishReply2, "keep beads-viewer's bead page whole while you do it", 41},
	} {
		put(&beadsapi.BeadDetail{ID: head + r.id, Type: "message", Kind: "data", Status: "closed", CreatedBy: bridge, CreatedAt: at(r.m), Title: r.text,
			Labels: []string{in, "slack-thread-message", "slack-thread-reply", "thread:" + head},
			Fields: map[string]string{"text": r.text, "thread_id": head, "operated_by": person, "slack_user_id": slackUID,
				"idempotency_key": "thread-reply:" + slackCh + ":" + ts(r.m+i), "slack_thread_ts": ts(0), "slack_thread_channel": slackCh}})
	}
	for _, s := range []struct {
		id, text string
		m        int
	}{
		{WishSquawk1, "crew " + agent + " (project " + land + ") on " + head + WishTask + ": fixture a wish with its family", 12},
		{WishSquawk2, "fixture in: the wish, its replies, squawks, card, session, agent, task and MR in the card world", 30},
		{WishSquawk3, "PR open with the before shots of the three views; waiting on CI", 52},
	} {
		put(&beadsapi.BeadDetail{ID: head + s.id, Type: "message", Kind: "data", Status: "closed", CreatedBy: agent, CreatedAt: at(s.m), Title: s.text,
			Labels: []string{"from:" + agent, in, "squawk"},
			Fields: map[string]string{"text": s.text, "thread_id": head, "operated_by": person, "source_agent": agent}})
	}
	card := "Agent: " + agent
	put(&beadsapi.BeadDetail{ID: head + WishCard, Type: "message", Kind: "data", Status: "closed", CreatedBy: bridge, CreatedAt: at(11), Title: card,
		Labels: []string{in},
		Fields: map[string]string{"kind": "card", "author": "agent:" + agent, "text": card, "thread_id": head, "channel_id": channel, "subject_id": head + WishAgent,
			"slack_ts": ts(11), "slack_channel": slackCh, "idempotency_key": "card:" + head + WishAgent + ":" + channel}})
	started := at(10).UTC().Format(time.RFC3339)
	put(&beadsapi.BeadDetail{ID: head + WishSession, Type: "session", Kind: "data", Status: "open", CreatedBy: agent, CreatedAt: at(10),
		Title: "session:" + agent + ":" + started, Labels: []string{in},
		Fields: map[string]string{"role": "crew", "agent": agent, "status": "active", "project": land, "hostname": "crew-" + agent,
			"thread_id": head, "started_at": started, "agent_bead_id": head + WishAgent}})
	put(&beadsapi.BeadDetail{ID: head + WishAgent, Type: "agent", Kind: "config", Status: "open", CreatedBy: land + "-plan-gardener", CreatedAt: at(9), UpdatedAt: at(52),
		Title: agent, Labels: []string{in, "role:crew"},
		Fields: map[string]string{"agent": agent, "role": "crew", "project": land, "agent_state": "working", "task_id": head + WishTask,
			"thread_id": head, "spawned_by": land + "-plan-gardener", "operated_by": person, "live_cost_usd": "3.12", "live_turns": "64"}})
	put(&beadsapi.BeadDetail{ID: head + WishTask, Type: "task", Kind: "issue", Status: "in_progress", Priority: 0, Assignee: agent, CreatedBy: "thread-" + ts(0),
		CreatedAt: at(8), UpdatedAt: at(52), Title: "Fixture a wish with its whole family, and shoot the three views as they are", Labels: []string{in},
		Description: "Add a Slack-origin wish head and its kin to the card world, then shoot the bead panel, the workbench node and the steward.",
		Fields:      map[string]string{"thread_id": head, "operated_by": person}})
	put(&beadsapi.BeadDetail{ID: head + WishMR, Type: "merge-request", Kind: "issue", Status: "open", CreatedBy: agent, CreatedAt: at(50),
		Title: "PR #2320: test(gardenhub): fixture a Slack wish with its family", Labels: []string{in},
		Fields: map[string]string{"mr_url": "https://github.com/mattbakerconsulting/gardenio/pull/2320", "mr_state": "opened"}})
	f.Deps = []beadsapi.Dependency{
		{BeadID: head + WishTask, DependsOnID: head, Type: "parent-child"},
		{BeadID: head + WishAgent, DependsOnID: head + WishTask, Type: "assigned"},
		{BeadID: head + WishTask, DependsOnID: head + WishMR, Type: "produces"},
	}
	f.Comments[head] = []beadsapi.Comment{
		{ID: 1, Author: "thread-" + ts(0), Text: "Confirmed in code: the bead panel never lists the beads bound to a thread by thread_id, so a wish shows its first message and nothing else.", CreatedAt: at(6)},
		{ID: 2, Author: person, OperatedBy: person, Text: "Plan approved; " + head + WishTask + " is the first step.", CreatedAt: at(7)},
	}
	return f
}

// Bead is the family's member id, or nil.
func (f Family) Bead(id string) *beadsapi.BeadDetail {
	i := slices.IndexFunc(f.Beads, func(b *beadsapi.BeadDetail) bool { return b.ID == id })
	if i < 0 {
		return nil
	}
	return f.Beads[i]
}

// PutIn adds the family to d: its beads put (not logged, so no stream replays
// them), its edges linked and its comments set.
func (f Family) PutIn(d *Daemon) {
	for _, b := range f.Beads {
		d.Put(b)
	}
	for _, e := range f.Deps {
		d.Link(e.BeadID, e.DependsOnID, e.Type)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Comments == nil {
		d.Comments = map[string][]beadsapi.Comment{}
	}
	for id, cs := range f.Comments {
		d.Comments[id] = append(d.Comments[id], cs...)
	}
}
