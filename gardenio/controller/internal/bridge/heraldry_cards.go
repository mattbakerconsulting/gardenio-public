package bridge

// heraldry_cards.go puts a bead's arms on Slack as text (heraldry spec R9,
// Q5, Q9; the owner's linear-form verdict, step 5.5a gd-nLVyw2i9fN). Slack
// shows text emoji, so wherever the bridge names a bead bearing arms (prose,
// the References footer, link hydration, the unfurl and the cards) the arms
// are the emoji run glued before the bead's name: 🔥🧭📦 Short title. No
// surface here draws or uploads a picture.
//
// The run is read through heraldry.Parse, never a split of the stored string,
// so the stored six-token form and the run form (step 5.3) read alike and a
// value that is not arms shows nothing.
//
// On a card (the agent card, thread card and wrap-up, whose subject is the
// agent's task, and the decision and escalation cards, whose subject is their
// requesting_task) the run and the subject's linked short title join the
// header line; the type badge and the state glyph stay. For C7's budget the
// run counts as one glyph, glued to the title's first word. The agent cards'
// subject is a task, which bears no arms (R6), so they show its line's: the
// nearest armed ancestor's (withTaskRun, gd-ASLjQPkHJz).

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/badge"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

// armsHeadBlocks is how far into a card the run looks for the header section
// to join: the head of the card, not its body.
const armsHeadBlocks = 3

// armsRun is the emoji run of arms as stored (BeadDetail.Heraldry), "" when
// they are none or do not parse.
func armsRun(stored string) string {
	a, err := heraldry.Parse(stored)
	if err != nil {
		return ""
	}
	return a.Run()
}

// beadTitleLinkArms is beadTitleLink with the bead's emoji run before the
// link when it bears arms (spec R9, Q5), the plain link otherwise. heraldry is
// the stored form.
func (b *Bot) beadTitleLinkArms(id, title, heraldry string) string {
	link := b.beadTitleLink(id, title)
	if run := armsRun(heraldry); run != "" {
		return run + " " + link
	}
	return link
}

// withSubjectRun is blocks with the card subject's run and linked short title
// at the end of the header line: the first line of the card's first mrkdwn
// section among its first armsHeadBlocks blocks. A card that leads with a
// header block (the wrap-up) takes it on the section under the header. Only
// epics bear arms in v1 (spec R6), so most cards are returned unchanged, as
// is a card with no such section. The section is copied, never edited, so the
// caller's blocks are not mutated.
func (b *Bot) withSubjectRun(blocks []slack.Block, subject *beadsapi.BeadDetail) []slack.Block {
	if subject == nil {
		return blocks
	}
	return b.withSubjectArms(blocks, subject, subject.Heraldry)
}

// withTaskRun is withSubjectRun for the agent cards, whose subject is the
// agent's task: the run is the task's line arms (taskLineArms), before the
// task's linked short title (gd-ASLjQPkHJz).
func (b *Bot) withTaskRun(ctx context.Context, blocks []slack.Block, task *beadsapi.BeadDetail) []slack.Block {
	if task == nil {
		return blocks
	}
	return b.withSubjectArms(blocks, task, b.taskLineArms(ctx, task))
}

// withSubjectArms is withSubjectRun with the arms to draw given as stored.
func (b *Bot) withSubjectArms(blocks []slack.Block, subject *beadsapi.BeadDetail, stored string) []slack.Block {
	if armsRun(stored) == "" {
		return blocks
	}
	for i, bl := range blocks[:min(len(blocks), armsHeadBlocks)] {
		sec, ok := bl.(*slack.SectionBlock)
		if !ok || sec == nil || sec.Text == nil || sec.Text.Type != slack.MarkdownType {
			continue
		}
		name := b.beadTitleLinkArms(subject.ID, beadDisplayName(subject), stored)
		text := *sec.Text
		head, rest, more := strings.Cut(text.Text, "\n")
		text.Text = head + " · " + name
		if more {
			text.Text += "\n" + rest
		}
		cp := *sec
		cp.Text = &text
		out := slices.Clone(blocks)
		out[i] = &cp
		return out
	}
	return blocks
}

// cardSubjectBead is the bead a card is about, read by id: a decision's or
// escalation's requesting_task, or the task_id an agent bead names (by the
// wrap-up the task is usually closed, so it is not read through the agent's
// held work). nil when id is empty or the bead cannot be read.
func (b *Bot) cardSubjectBead(ctx context.Context, id string) *beadsapi.BeadDetail {
	id = strings.TrimSpace(id)
	if b == nil || b.daemon == nil || id == "" {
		return nil
	}
	bead, err := b.daemon.GetBead(ctx, id)
	if err != nil {
		return nil
	}
	return bead
}

const (
	// taskRunTTL is how long a task's line arms are trusted on the agent
	// card, which is redrawn on every state change: arms change rarely, and a
	// changed one shows within this.
	taskRunTTL = 2 * time.Minute
	// taskRunMax bounds the cache; past it, it starts over.
	taskRunMax = 1024
)

type taskRun struct {
	stored string
	at     time.Time
}

// taskRuns caches taskLineArms by task id. The zero value is ready to use.
type taskRuns struct {
	mu sync.Mutex
	m  map[string]taskRun
}

// taskLineArms is the stored arms of task's line: the nearest armed
// ancestor's along its parent-child edges (badge.ResolveLine, the walk
// /garden and the viewer read too), "" when the line bears none. A daemon
// client that cannot read dependencies falls back to the task's own arms.
func (b *Bot) taskLineArms(ctx context.Context, task *beadsapi.BeadDetail) string {
	src, ok := b.daemon.(badge.LineReader)
	if !ok {
		return task.Heraldry
	}
	now := time.Now()
	c := &b.taskRuns
	c.mu.Lock()
	if r, ok := c.m[task.ID]; ok && now.Sub(r.at) < taskRunTTL {
		c.mu.Unlock()
		return r.stored
	}
	c.mu.Unlock()
	fwd, err := src.GetDependencies(ctx, task.ID)
	if err != nil {
		return "" // not cached: tried again on the next redraw
	}
	a, _ := badge.ResolveLine(ctx, src, task, fwd)
	c.mu.Lock()
	if c.m == nil || len(c.m) >= taskRunMax {
		c.m = map[string]taskRun{}
	}
	c.m[task.ID] = taskRun{stored: a.Heraldry, at: now}
	c.mu.Unlock()
	return a.Heraldry
}
