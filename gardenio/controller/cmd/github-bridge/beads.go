package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// bridgeActor is the identity the bridge stamps on beads and deps it writes.
// The daemon's audit trail already attributes the token's writes to the bridge;
// this is the human-readable created_by on notices and mail.
const bridgeActor = "github-bridge"

// forgeGitHub is the value of the bead `forge` field for everything this
// bridge writes. GitLab beads leave the field empty.
const forgeGitHub = "github"

// beadClient is the slice of beadsapi.Client the event handlers use, as an
// interface so tests can run the handlers against an in-memory fake.
type beadClient interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	ListTaskBeadsForMR(ctx context.Context, mrURL string) ([]*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	UpsertMergeRequestBead(ctx context.Context, f beadsapi.MergeRequestBeadFields, closed bool) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

var _ beadClient = (*beadsapi.Client)(nil)

// agentTarget is the agent working a task bead: who to squawk as and mail.
type agentTarget struct {
	Name   string
	Active bool // agent bead not closed: mail has a recipient that will read it
}

// findAgentForTask resolves the agent bead whose task_id is taskBeadID, falling
// back to the task's assignee (Active unknown, so not mailed). An active agent
// wins over a closed one; among equals the newest wins. Nil means nobody ever
// worked the task.
func findAgentForTask(ctx context.Context, c beadClient, task *beadsapi.BeadDetail) (*agentTarget, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred", "closed"},
		Fields:   map[string]string{"task_id": task.ID},
		Sort:     "-created_at",
		Limit:    5,
	})
	if err != nil {
		return nil, fmt.Errorf("listing agent beads for %s: %w", task.ID, err)
	}
	var best *agentTarget
	if res != nil {
		for _, b := range res.Beads {
			t := &agentTarget{Name: firstNonEmpty(b.Fields["agent"], b.Title), Active: b.Status != "closed"}
			if best == nil || (t.Active && !best.Active) {
				best = t
			}
		}
	}
	if best == nil && task.Assignee != "" {
		best = &agentTarget{Name: task.Assignee}
	}
	return best, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// beadNotifier delivers an event to the agent working a bead: a status line in
// its Slack thread, and a mail bead to the agent itself. Both ride existing
// consumers (slack-bridge's squawk relay and the agent's mail hook), so the
// bridge needs no Slack credential and local agents receive the same events as
// pod agents.
type beadNotifier struct {
	daemon beadClient
	logger *slog.Logger
}

// postThread files a closed squawk-shaped message bead, the carrier `gb squawk`
// uses, which slack-bridge posts into the named agent's thread. key makes the
// create idempotent so a redelivered webhook cannot post the line twice.
func (n *beadNotifier) postThread(ctx context.Context, ag *agentTarget, key, text string) {
	if n == nil || ag == nil || ag.Name == "" || text == "" {
		return
	}
	fields, err := json.Marshal(map[string]string{
		"source_agent": ag.Name,
		"text":         text,
		"via":          bridgeActor,
	})
	if err != nil {
		return
	}
	title := text
	if len(title) > 500 {
		title = title[:497] + "..."
	}
	id, err := n.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:          title,
		Type:           "message",
		Kind:           "data",
		Labels:         []string{"squawk", "from:" + ag.Name},
		Fields:         fields,
		CreatedBy:      bridgeActor,
		IdempotencyKey: key,
	})
	if err != nil {
		n.logger.Warn("thread line: create failed", "agent", ag.Name, "error", err)
		return
	}
	if err := n.daemon.CloseBead(ctx, id, nil); err != nil {
		// Already closed when the idempotency key returned an earlier line.
		n.logger.Debug("thread line: close", "bead", id, "error", err)
	}
}

// mailAgent sends a normal-priority mail bead to a still-active agent, the
// event path agents (pod or local) already poll. Priority 2 with no interrupt
// label: a PR event informs the agent, it must not preempt a turn.
func (n *beadNotifier) mailAgent(ctx context.Context, ag *agentTarget, key, subject, body string) {
	if n == nil || ag == nil || !ag.Active || ag.Name == "" {
		return
	}
	if _, err := n.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:          subject,
		Type:           "mail",
		Kind:           "data",
		Description:    body,
		Assignee:       ag.Name,
		Labels:         []string{"from:" + bridgeActor, "forge:" + forgeGitHub},
		CreatedBy:      bridgeActor,
		Priority:       2,
		IdempotencyKey: key,
	}); err != nil {
		n.logger.Warn("agent mail: create failed", "agent", ag.Name, "error", err)
	}
}

// slackText makes s safe inside Slack mrkdwn: &, < and > are the control
// characters, and | would end a <url|label> link's URL early.
func slackText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "|", "/").Replace(s)
}
