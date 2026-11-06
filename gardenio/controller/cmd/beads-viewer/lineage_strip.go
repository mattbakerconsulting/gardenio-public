package main

import (
	"context"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Agent lineage strip (persistent-agents Phase 1, task ka-0000007852; design
// memo ka-0000001420 §3.4). Renders an agent's succession lineage as a LINEAR,
// PAGINATED strip anchored on the live head — NOT a radial orbit (the
// constellation would clip a long chain silently against its fixed viewBox).
//
// The chain is the neutral succession back-pointer written on every recreate
// (thread respawn, gardener iteration, wrapup successor): the viewer reads
// `prior_agent_id` and falls back to `schedule_prior_agent_id` (the untouched
// legacy gardener pointer) so both lineage families walk through one path. The
// head is opaque (the agent bead being viewed); the last K predecessors are
// shown as chips newest-first; everything older is folded into a "+N earlier"
// pill that deep-links to the lineage root.
//
// EngineCount / EnginePruned framing (gardener_counters.go) is preserved so a
// gap in the rendered chain reads as an EXPLAINED lifetime count, not lost
// data: when the walk stops because a predecessor pointer dangles (the bead was
// pruned/reaped) or because the hard cap was hit before the root, the strip is
// flagged Pruned and the template appends "(older runs pruned)".

// lineageFieldPrior / lineageFieldSchedulePrior are the two back-pointer fields
// the walk reads, in precedence order. prior_agent_id is the neutral pointer
// stamped on every recreate (task ka-0000006931); schedule_prior_agent_id is
// the legacy gardener-iteration pointer, left un-migrated in Phase 1, so the
// viewer reads EITHER (design memo §2.2 / §3.4).
const (
	lineageFieldPrior         = "prior_agent_id"
	lineageFieldSchedulePrior = "schedule_prior_agent_id"
)

// lineageStripShownK is how many predecessor chips (excluding the head) render
// before the rest fold into the "+N earlier" pill. Small enough to keep the
// strip on one comfortable row; the fold keeps a 200-deep gardener chain from
// blowing out the card.
const lineageStripShownK = 4

// lineageWalkCap bounds the backward walk so a corrupt/cyclic chain can never
// spin the page or the daemon. A chain longer than this is truncated and
// flagged Pruned — the honest "older runs pruned" framing, same as the gardener
// header's engine-count reconciliation.
const lineageWalkCap = 64

// agentLineageStripView is the template view model for the agent-card lineage
// strip. Empty (Head == nil, or a chain of length 1) suppresses the whole
// section — a first-of-its-line agent with no predecessor has no lineage to
// show.
type agentLineageStripView struct {
	// Head is the opaque live head — the agent bead being viewed. Always the
	// newest run in the chain. Non-nil whenever the strip renders.
	Head *lineageChipView
	// Shown are the predecessor chips, newest-first (the run just before Head
	// leads), capped at lineageStripShownK.
	Shown []lineageChipView
	// EarlierCount is how many predecessors older than the shown window exist
	// (folded into the "+N earlier" pill). Zero suppresses the pill.
	EarlierCount int
	// RootID is the lineage root (oldest reachable run) the "+N earlier" pill
	// links to. Empty when the chain never reached a distinct root (e.g. it
	// truncated at the cap), in which case the pill is a non-link.
	RootID string
	// Pruned is true when the walk did NOT reach a natural root — a predecessor
	// pointer dangled (the bead was reaped) or the hard cap was hit. Drives the
	// "(older runs pruned)" caption so the gap reads as explained, not lost.
	Pruned bool
	// TotalKnown is the number of runs actually walked and rendered (Head +
	// Shown + folded), i.e. the visible depth of the life-thread. Not a
	// lifetime tally — pruned/capped chains under-count deliberately, matching
	// the EngineCount contract of never advertising more than we can show.
	TotalKnown int
}

// HasEarlier reports whether the "+N earlier" pill should render.
func (v *agentLineageStripView) HasEarlier() bool { return v != nil && v.EarlierCount > 0 }

// lineageChipView is one run in the strip. All fields are pre-formatted so the
// template stays dumb.
type lineageChipView struct {
	ID       string
	Name     string // the agent field (handle), falls back to ID when absent
	Href     string // /{id}
	Status   string // open | in_progress | closed | ""
	Color    string // status fill
	Stroke   string // status stroke
	AgeLabel string // humanized created-at age, e.g. "3h", empty when unknown
	IsHead   bool
}

// buildAgentLineageStrip walks the succession back-pointer chain from `bead`
// (the live head) toward its root and assembles the paginated strip view.
// Returns nil when there is nothing worth rendering: a non-agent bead, no
// predecessor pointer, or a chain that resolves to a single run.
//
// The walk hydrates each predecessor with a single-ID GetBead so it can render
// the handle/status/age; a missing predecessor (404, reaped) terminates the
// walk and flags Pruned rather than failing the whole card — the strip is
// decorative, never load-bearing.
func (s *Server) buildAgentLineageStrip(ctx context.Context, bead *beadsapi.BeadDetail) *agentLineageStripView {
	if bead == nil || bead.Type != "agent" {
		return nil
	}
	priorID := lineagePriorID(bead.Fields)
	if priorID == "" {
		// First of its line — no succession to show.
		return nil
	}

	now := s.now()
	head := lineageChipFromBead(bead, s.basePath, now, true)
	view := &agentLineageStripView{
		Head:       &head,
		TotalKnown: 1,
	}

	// Walk backward from the head. `chain` collects predecessors newest-first.
	seen := map[string]bool{bead.ID: true}
	var chain []lineageChipView
	rootID := ""
	cur := priorID
	pruned := false
	for {
		if cur == "" {
			// Reached a natural root: the previous iteration had no prior
			// pointer. rootID (set on the prior loop turn) is the oldest run.
			break
		}
		if seen[cur] {
			// A cycle — corrupt chain. Stop and flag as pruned/broken.
			pruned = true
			break
		}
		if len(chain) >= lineageWalkCap {
			// Hard cap hit before the root: older runs exist but we refuse to
			// keep walking. Honest "pruned" framing (there is more we won't show).
			pruned = true
			break
		}
		seen[cur] = true
		pb, err := s.daemon.GetBead(ctx, cur)
		if err != nil || pb == nil {
			// Predecessor missing (reaped/pruned) — the chain is broken here.
			s.logger.Debug("lineage strip: predecessor missing", "id", cur, "error", err)
			pruned = true
			break
		}
		chain = append(chain, lineageChipFromBead(pb, s.basePath, now, false))
		view.TotalKnown++
		rootID = pb.ID
		cur = lineagePriorID(pb.Fields)
	}

	if len(chain) == 0 {
		// The head pointed at a predecessor we could not resolve at all (a
		// dangling/reaped first hop). The strip needs at least one resolved
		// predecessor to be legible, so suppress it entirely — the card's other
		// spawn/predecessor affordances still cover the single missing hop.
		return nil
	}

	// Paginate: the newest lineageStripShownK predecessors render as chips; the
	// rest fold into "+N earlier".
	if len(chain) > lineageStripShownK {
		view.Shown = chain[:lineageStripShownK]
		view.EarlierCount = len(chain) - lineageStripShownK
	} else {
		view.Shown = chain
	}

	view.Pruned = pruned
	// Only offer the root deep-link when the walk actually reached a natural
	// root (not truncated/broken) and it is distinct from the last shown chip.
	if !pruned {
		view.RootID = rootID
	}
	return view
}

// lineagePriorID returns the succession back-pointer from an agent bead's
// fields: prior_agent_id, falling back to the legacy schedule_prior_agent_id
// (design memo §2.2 / §3.4 — the viewer reads EITHER). Empty when neither is
// set (a first-of-line agent).
func lineagePriorID(fields map[string]string) string {
	if fields == nil {
		return ""
	}
	if p := fields[lineageFieldPrior]; p != "" {
		return p
	}
	return fields[lineageFieldSchedulePrior]
}

// lineageChipFromBead projects an agent bead onto a strip chip. `isHead` marks
// the opaque live head so the template can render it emphasized.
func lineageChipFromBead(b *beadsapi.BeadDetail, basePath string, now time.Time, isHead bool) lineageChipView {
	name := ""
	if b.Fields != nil {
		name = b.Fields["agent"]
	}
	if name == "" {
		name = b.ID
	}
	return lineageChipView{
		ID:       b.ID,
		Name:     name,
		Href:     basePath + "/" + b.ID,
		Status:   b.Status,
		Color:    statusFill(b.Status),
		Stroke:   statusStroke(b.Status),
		AgeLabel: fmtTimeAgo(b.CreatedAt, now),
		IsHead:   isHead,
	}
}
