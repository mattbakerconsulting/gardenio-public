package main

import (
	"context"
	"time"
)

// inboxAdjacentBucketsThreshold is the inclusive stream-row count at or below
// which the inbox inline-renders previews of adjacent signal sources
// (Decisions / Escalations / Mentions). Operator decision ka-0000003122 option
// `adjacent-buckets`; parent bug ka-0000007303. Tunable — operators tweaking
// this in response to telemetry should also revisit the inbox_test cases below.
const inboxAdjacentBucketsThreshold = 3

// inboxAdjacentBucketsView is the template view model for the "adjacent
// buckets" section rendered below the primary stream when the operator's
// mine-scope row count is at or below the threshold.
type inboxAdjacentBucketsView struct {
	// Active is the gate the template branches on. When false the section
	// renders nothing — even if the embedded slices are non-empty — so a
	// stale view-model never leaks signal onto a healthy inbox.
	Active bool

	// Threshold mirrors the trigger value for the operator-facing copy.
	Threshold int

	// StreamCount is the post-filter stream row count that triggered the
	// inline render. Used for the section's intro line.
	StreamCount int

	// Decisions / DecisionsTotal preview pending decisions visible on /health.
	// Decisions is capped at inboxAdjacentBucketsPreviewCap so the section
	// stays scannable even when the global pending queue is long.
	Decisions      []decisionRowView
	DecisionsTotal int

	// Escalations / EscalationsTotal preview the escalation queue visible
	// on /health. Same cap, same intent.
	Escalations      []healthEscalationRowView
	EscalationsTotal int

	// MentionsAvailable is false in v1 — recent @-mentions to the operator
	// require a new data feed (research bead gd-?). The template renders a
	// "Mentions" placeholder card so the operator sees the third bucket is
	// planned rather than missing by accident.
	MentionsAvailable bool

	// BasePath threads the server's base path through so anchor hrefs in
	// the partial can link out to /decisions, /escalations, etc.
	BasePath string
}

// inboxAdjacentBucketsPreviewCap caps the number of items rendered per
// bucket. Three matches the /health "next 3" pattern and keeps the section
// vertically cheap.
const inboxAdjacentBucketsPreviewCap = 3

// buildInboxAdjacentBuckets assembles the adjacent-buckets view model when
// the threshold trips. Decisions are taken from already-fetched data so the
// section is free of additional queries on that axis; escalations issue a
// fresh fetch via buildEscalationsTileView — the same path /health uses, so
// no new daemon predicate is introduced.
//
// Callers MUST only invoke this when streamCount <= inboxAdjacentBucketsThreshold.
// The function does not re-check the threshold so the trigger predicate stays
// in one place at the call site (the inbox handler).
func (s *Server) buildInboxAdjacentBuckets(
	ctx context.Context,
	streamCount int,
	decisions []decisionRowView,
	decisionsTotal int,
	now time.Time,
) inboxAdjacentBucketsView {
	v := inboxAdjacentBucketsView{
		Active:            true,
		Threshold:         inboxAdjacentBucketsThreshold,
		StreamCount:       streamCount,
		Decisions:         capDecisionRows(decisions, inboxAdjacentBucketsPreviewCap),
		DecisionsTotal:    decisionsTotal,
		MentionsAvailable: false,
		BasePath:          s.basePath,
	}

	esc := s.buildEscalationsTileView(ctx, now)
	v.EscalationsTotal = esc.Pending
	if len(esc.Next) > inboxAdjacentBucketsPreviewCap {
		v.Escalations = esc.Next[:inboxAdjacentBucketsPreviewCap]
	} else {
		v.Escalations = esc.Next
	}

	return v
}

// capDecisionRows returns the first n rows or the input unchanged when it is
// already short enough. Defensive against negative/zero caps so a future
// constant tweak cannot panic on a slice header.
func capDecisionRows(rows []decisionRowView, n int) []decisionRowView {
	if n <= 0 {
		return nil
	}
	if len(rows) <= n {
		return rows
	}
	return rows[:n]
}
