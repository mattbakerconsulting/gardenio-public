package main

import (
	"context"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// gardenQueues is the Queues panel's reader (gardenhub.QueueReader, Lands
// workbench-glass spec WB-6, step 5.5): each tab of GET /garden/queue/{kind}
// is read by the builders its viewer page uses — decisions.go's
// openDecisionsQuery + decisionRows, escalations.go's escalationRows,
// inbox_stream.go's fetchInboxStreamSources + buildInboxStream — on the
// request's bearer, so the daemon cuts each tab exactly as it cuts the page.
type gardenQueues struct{ s *Server }

// Queue implements gardenhub.QueueReader.
func (q gardenQueues) Queue(ctx context.Context, kind string, all bool) (*gardenhub.QueueDoc, error) {
	switch kind {
	case gardenhub.QueueDecisions:
		return q.s.decisionsQueue(ctx, all)
	case gardenhub.QueueEscalations:
		return q.s.escalationsQueue(ctx, all)
	default:
		return q.s.inboxQueue(ctx)
	}
}

// decisionsQueue is /decisions as a tab: the viewer's open decisions, or
// everyone's with all. A viewer with no operator identity has no "mine", so
// they see every open decision and are told why (as the page does).
func (s *Server) decisionsQueue(ctx context.Context, all bool) (*gardenhub.QueueDoc, error) {
	opID, _, err := s.resolveInboxViewerOperator(ctx, oidcauth.EmailFromContext(ctx))
	if err != nil {
		s.logger.Warn("garden queue: resolving viewer operator", "error", err)
		opID = 0
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, openDecisionsQuery(opID, all))
	if err != nil {
		return nil, err
	}
	if opID != 0 && !all {
		res = s.withAddressedLandsDecisions(ctx, res, oidcauth.EmailFromContext(ctx))
	}
	doc := &gardenhub.QueueDoc{All: all || opID == 0, Scoped: opID != 0}
	if opID == 0 {
		doc.Note = "Showing every open decision: there is no operator identity for you yet, so none is marked yours."
	}
	if res.Total > len(res.Beads) {
		doc.More = res.Total - len(res.Beads)
	}
	for _, row := range s.decisionRows(ctx, res.Beads) {
		r := gardenhub.QueueRow{
			ID: row.ID, Title: row.Title, Kind: "decision", Type: "decision", Project: row.Project, Status: "open",
			Text: row.Prompt, Actor: row.RequestedBy, Since: unixSec(row.created),
			Agent: originRef(row.Origin.Agent), Task: originRef(row.Origin.Task), Plan: originRef(row.Origin.Plan),
		}
		if r.Agent != nil {
			r.Actor = "" // the origin strip names the agent
		}
		if opID != 0 && row.operatedByID == opID {
			r.NeedsYou, r.Why = true, "asked of you"
		}
		for _, o := range row.Options {
			if o.ID == "" {
				continue // an option with no id cannot be chosen; the free answer takes it (gardenhub render.go OptionList)
			}
			r.Options = append(r.Options, gardenhub.QueueOption{ID: o.ID, Text: firstNonBlank(o.Short, o.Label, o.ID), Label: o.Label})
		}
		doc.Rows = append(doc.Rows, r)
	}
	return doc, nil
}

// escalationsQueue is /escalations as a tab: the owner's escalations, with the
// desk's and the operators' folded into one "In hand" line (gd-bmOiteQi36),
// or every one with all.
func (s *Server) escalationsQueue(ctx context.Context, all bool) (*gardenhub.QueueDoc, error) {
	rows, fetched, err := s.escalationRows(ctx, "", "")
	if err != nil {
		return nil, err
	}
	doc := &gardenhub.QueueDoc{All: all, Scoped: true}
	if fetched >= escalationsListLimit {
		doc.Note = "Only the newest escalations are listed; there may be more."
	}
	var tally beadsapi.InHand
	for _, row := range rows {
		if row.InHand && !all {
			if row.LandFold {
				tally.AddLand()
			} else {
				tally.Add(row.Audience)
			}
			continue
		}
		r := gardenhub.QueueRow{
			ID: row.ID, Title: row.Title, Kind: "escalation", Type: "escalation", Project: row.Project, Status: row.Status, State: row.State,
			Text: row.Context, Operation: row.OperationType, Sensitivity: row.Sensitivity, Audience: row.Audience, Held: row.Agent,
			Actor: row.Origin.AgentName, Since: unixSec(row.created),
			Agent: originRef(row.Origin.Agent), Task: originRef(row.Origin.Task), Plan: originRef(row.Origin.Plan),
		}
		if !row.InHand && row.State == "pending" { // an approved one waits on its executor, not on the owner (gd-yuOG6cX6VI)
			r.NeedsYou, r.Why = true, "for the owner"
		}
		doc.Rows = append(doc.Rows, r)
	}
	doc.InHand = tally.Line()
	return doc, nil
}

// inboxQueue is the inbox stream (/inbox) as a tab, less what has a tab or a
// place of its own: pending decisions are the Decisions tab, and the agents
// that finished in the last day are history, not a queue. Pinned (the
// viewer's pin or flag:inbox-pinned), forwarded and operator-blocking rows
// need the viewer. The inbox has only a "mine".
func (s *Server) inboxQueue(ctx context.Context) (*gardenhub.QueueDoc, error) {
	email := oidcauth.EmailFromContext(ctx)
	opID, banner, err := s.resolveInboxViewerOperator(ctx, email)
	if err != nil {
		return nil, err
	}
	doc := &gardenhub.QueueDoc{}
	if banner == "mode_a" {
		doc.Note = "Your inbox needs an operator identity, and there is none for you yet."
		return doc, nil
	}
	intake, forwardOrder, _, _, _, active, activeBeads, _, _, epics, epicBeads, intakeTotal, err := s.fetchInboxStreamSources(ctx, opID)
	if err != nil {
		return nil, err
	}
	if intakeTotal > len(intake) {
		doc.More = intakeTotal - len(intake)
	}
	mr := newInboxMRResolver()
	if s.daemon != nil {
		mr.Prewarm(ctx, s.daemon, intake)
	}
	stream := buildInboxStream(intake, forwardOrder, nil, nil, active, activeBeads, nil, nil, epics, epicBeads, mr, s.basePath, time.Now())
	pins := s.viewerPinnedSet(ctx, email)
	for _, row := range stream {
		_, pinned := pins[row.ID]
		r := gardenhub.QueueRow{
			ID: row.ID, Title: row.Title, ShortTitle: row.ShortTitle, Type: row.Type, Kind: row.Kind, Project: row.Project,
			Priority: row.Priority, Text: row.DescPreview, Actor: row.Actor, Since: unixSec(row.ActivityAt),
			Pinned: pinned || row.Pinned, Worthy: row.InboxWorthy, MR: row.MRState, MRURL: row.MRURL,
		}
		if row.Kind == inboxKindAgent {
			r.Status, r.Text = row.AgentState, firstNonBlank(row.AgentTaskTitle, r.Text)
		} else if row.Kind == inboxKindEpic {
			r.Status = row.EpicStatus
		}
		switch {
		case r.Pinned:
			r.NeedsYou, r.Why = true, "pinned"
		case row.Forwarded:
			r.NeedsYou, r.Why = true, "forwarded to you"
		case row.Flagged:
			r.NeedsYou, r.Why = true, "blocking an operator"
		}
		doc.Rows = append(doc.Rows, r)
	}
	return doc, nil
}

func originRef(l *originLink) *gardenhub.QueueRef {
	if l == nil || l.ID == "" {
		return nil
	}
	return &gardenhub.QueueRef{ID: l.ID, Title: l.Title}
}

func unixSec(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
