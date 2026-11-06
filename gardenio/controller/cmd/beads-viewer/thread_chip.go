package main

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Phase 2 — task 2-6 (ka-0000008112): the altitude-UP "part of wish" chip on
// bead-show. It is the inverse of the wish→members roll-up that threads.go
// computes: where threadMembers walks DOWN from a wish head to its members,
// this chip walks UP from any member bead to its containing wish, making
// "every bead optionally belongs to a thread" visible on the bead detail page.
//
// The link target is the thread (wish) DETAIL route `/threads/{thread_id}`,
// registered by task 2-3 (ka-0000003348). 2-6 ships the inbound link; if 2-3
// has not merged when this lands, the chip points at the route 2-3 will
// register and the dependency is noted in the MR. Once 2-3 merges the chip is
// live end-to-end.
//
// Data model (source of truth: internal/bridge/thread_bead.go, mirrored in
// threads.go): a thread head IS the wish and carries NO thread_id field, so it
// does not point at itself. Every DERIVATIVE (member task, message, agent)
// carries fields.thread_id == the head's ka-id. The chip therefore renders
// exactly for beads that carry a non-empty thread_id field whose value is not
// the bead's own id (a head never points at itself, but the self-guard makes
// the contract explicit and survives malformed data).

// threadChipTitleLimit clips the wish title rendered in the chip so a long
// Slack opening line does not blow out the bead-show header. The full title is
// preserved on the chip's title= attribute for hover. Mirrors the 120-rune
// cap the bridge applies to the head title (thread_bead.go) loosely — the chip
// is a one-line affordance, not the wish narrative, so it stays tighter.
const threadChipTitleLimit = 80

// threadChipView is the rendered "part of wish" chip on bead-show. It is nil
// (the zero value, surfaced as an absent chip) for any bead that does not carry
// a thread_id field, so the template can guard with a single `{{with}}`.
type threadChipView struct {
	// ThreadID is the ka-id of the containing wish (thread head).
	ThreadID string
	// Title is the wish's human-facing title — the head's wish text
	// (slack_root_text, else head title), clipped to threadChipTitleLimit.
	// Falls back to the bare ThreadID when the head bead cannot be resolved
	// (deleted head, daemon error), so the chip still links somewhere useful.
	Title string
	// FullTitle is the unclipped wish text for the chip's title= hover
	// attribute. Equals Title when no clipping occurred.
	FullTitle string
	// Href is the absolute link to the wish's /threads detail (route 2-3
	// registers): basePath + "/threads/" + ThreadID.
	Href string
	// Resolved reports whether the head bead's title was successfully
	// fetched. False means Title fell back to the ThreadID; the template can
	// soften the label ("part of wish gd-…") accordingly.
	Resolved bool
}

// buildThreadChip resolves the altitude-up "part of wish" chip for a bead, or
// returns nil when the bead carries no thread_id field (the head itself, or any
// bead never bound to a wish). It is best-effort: a daemon failure resolving
// the head title degrades to an unresolved chip that still links to the wish
// detail rather than failing the whole bead-show render.
func (s *Server) buildThreadChip(ctx context.Context, bead *beadsapi.BeadDetail) *threadChipView {
	if bead == nil {
		return nil
	}
	threadID := strings.TrimSpace(bead.Fields[threadIDField])
	if threadID == "" {
		// No wish pointer — either a thread head (which carries none, it IS
		// the wish) or a bead that was never part of a thread. No chip.
		return nil
	}
	if threadID == bead.ID {
		// Defensive: a head must never point at itself, but if malformed data
		// makes a bead its own wish, suppress the self-referential chip.
		return nil
	}

	view := &threadChipView{
		ThreadID: threadID,
		Href:     s.basePath + "/threads/" + threadID,
	}

	// Resolve the head's wish text for a human-readable chip label. Best-effort
	// — a missing/deleted head or a daemon error leaves Resolved false and the
	// chip falls back to the bare thread_id, which still links correctly.
	head, err := s.daemon.GetBead(ctx, threadID)
	if err == nil && head != nil {
		if wish := strings.TrimSpace(threadWishText(head)); wish != "" {
			view.FullTitle = wish
			view.Title = clipThreadChipTitle(wish)
			view.Resolved = true
			return view
		}
	}

	// Fallback: link is still good, label is the id.
	view.FullTitle = threadID
	view.Title = threadID
	return view
}

// clipThreadChipTitle clips a wish title to threadChipTitleLimit runes at a
// word boundary, appending an ellipsis when it had to cut. Rune-aware so a
// multi-byte title is not split mid-codepoint.
func clipThreadChipTitle(s string) string {
	r := []rune(s)
	if len(r) <= threadChipTitleLimit {
		return s
	}
	head := string(r[:threadChipTitleLimit])
	if cut := strings.LastIndexAny(head, " \t\n"); cut > 0 {
		head = head[:cut]
	}
	return strings.TrimRight(head, " \t\n") + "…"
}
