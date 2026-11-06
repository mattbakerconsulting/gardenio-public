package main

import "strings"

// maps_view.go — the pure, unit-testable view model behind the /maps board
// (epic ka-0000001644, build ka-0000005805, promote ka-0000002415).
//
// /maps answers the one question an operator actually has about the gardener
// fleet: "what are my gardeners, what fires each one, and how have their recent
// runs gone?" It renders every gardener as a board of connected cards, each
// reading left→right: a TRIGGER node (what fires it) → an IDENTITY+verdict body
// (what it is, how healthy it is) → a RECENT-RUN strip (how its last handful of
// runs went). It shares the SAME projection spine as /gardeners (loadGardenerRows
// → gardenerRowView → sortGardenerRows) so the two surfaces never drift, and adds
// zero daemon fan-out: the recent-run glyphs ride on the row's embedded
// trajectory, already built from the gardener bead's ProgressState.
//
// buildMapsBoard is pure (no http, no daemon, no clock): it maps each already-
// sorted gardenerRowView to a mapsCardView and computes the summary counts. The
// caller (handleMaps) owns the fetch, the filter, and the sort; this file owns
// the shape.

// mapsRecentRunStrip caps how many recent-run glyphs a card's strip draws. The
// row carries the full bounded trajectory sparkline (gardenerTrajectorySparkRuns
// = 24); the board only wants the last handful so the strip stays a glanceable
// "how did the last few runs go", not a dense sparkline. Oldest→newest within
// the tail.
const mapsRecentRunStrip = 6

// mapsTriggerKind classifies a gardener row's trigger primitive into the card's
// trigger-node color/label hook. The template colors the node by this and
// dashes/greys it when !Enabled. The values:
//
//   - "schedule"   — the legacy type=schedule primitive (Kind == "schedule"),
//     which no longer fires after D10 but still exists in the fleet (amber).
//   - "cron"       — a clock fires this gardener (blue).
//   - "bead_query" — a world-state predicate fires it: a conditions gate or a
//     bead_event/decision_resolved trigger entry (red).
//   - "event"      — fired by some other event source, no cron/predicate (purple).
//
// The cron/bead_query/event split comes from the row's TriggerSubKind, which
// gardenerRowFromBead projects from the trigger def (no extra fetch) — richer
// than NextFire alone, so a cron-bearing gardener whose cron failed to parse
// (NextFireError, no NextFire) still classifies as "cron" (design.md §6,
// ka-0000003781). TriggerSubKind is empty only for a legacy schedule bead or a
// pre-projection fixture; fall back to the NextFire-presence heuristic then so
// the node is never left unclassified.
func mapsTriggerKind(r gardenerRowView) string {
	if r.Kind == "schedule" {
		return "schedule"
	}
	switch r.TriggerSubKind {
	case "cron", "bead_query", "event":
		return r.TriggerSubKind
	}
	if r.NextFire != "" || r.NextFireError != "" {
		return "cron"
	}
	return "event"
}

// mapsTriggerKindLabel is the human-facing text for the trigger node, derived
// from the CSS-class kind. It exists only to render "bead query" instead of the
// raw "bead_query" CSS hook (the template uppercases it to "BEAD QUERY"); every
// other kind is its own label.
func mapsTriggerKindLabel(kind string) string {
	if kind == "bead_query" {
		return "bead query"
	}
	return kind
}

// mapsCardView is one gardener card on the /maps board. Every field is a
// rendered, template-ready scalar or a small glyph slice — no http, no daemon,
// no further projection. Built room-to-grow: owned-epic rollups and per-gardener
// cost are deliberately deferred (design.md §6); the live-agent overlay landed
// in maps2 v2 (ka-0000000595) via the LiveAgent* fields below.
type mapsCardView struct {
	ID    string
	Name  string
	Title string // raw bead title, shown as a subtitle only when it diverges from Name
	Href  string // /gardener/{id}

	TerminalGoal string
	OwnerDisplay string
	OwnerSlug    string
	Initials     string // actorInitials(Name) — computed here, not on the row

	// SearchText is the lowercased haystack the client-side /maps search box
	// matches against (maps2 v2, ka-0000006434): name + bead title + terminal
	// goal + owner (display + slug) + id, space-joined and lowercased once here
	// so the template can emit it as a single `data-search` attribute and the
	// JS filter is a cheap substring test per card. Built in buildMapsBoard via
	// mapsCardSearchText so it stays pure + unit-testable.
	SearchText string

	// Trigger node.
	TriggerKind      string // "cron" | "bead_query" | "event" | "schedule" (see mapsTriggerKind)
	TriggerKindLabel string // human text for the node ("bead query" not "bead_query")
	NextFire         string
	NextFireRelative string
	NextFireError    string
	TriggerHref      string // /triggers

	// Counters + verdict.
	RunsSpawned    int
	RunsCompleted  int
	RunsDerived    bool
	Verdict        string
	VerdictKind    string // "progress" | "stalled" | "stuck" | "neutral" — CSS hook
	VerdictDetail  string
	NeedsAttention bool
	Status         string // "active" | "disabled" | "closed"
	GoalState      string // "achieved" | "unachieved" | ""
	Enabled        bool
	IsClosed       bool

	// Cost + context chips (maps2 v2, ka-0000001668). Both ride on the row with
	// NO new daemon fan-out — a thin view addition, not a new pipeline:
	//   - COST is the controller-maintained gardener_spend_usd roll-up (the
	//     gardener's runs + the crew they transitively spawned), read off the
	//     gardener bead by applyGardenerSpend. HasSpend gates the chip; SpendLabel
	//     is the pre-formatted "$12.34"; SpendStale marks a roll-up older than
	//     gardenerSpendStaleAfter (rendered with a "~" prefix).
	//   - CONTEXT is the currently-live agent's context-window occupancy,
	//     projected off the SAME last-agent bead the live-agent overlay already
	//     fetched (applyLastRun → buildAgentUsageView). HasContext gates the chip;
	//     ContextLabel is the humanized token count ("128k"); ContextPct is the
	//     occupancy percent for the mini-bar; ContextTitle is the hover detail.
	//     Only present while a live agent is reporting context_tokens.
	HasSpend     bool
	SpendLabel   string
	SpendStale   bool
	HasContext   bool
	ContextLabel string
	ContextPct   int
	ContextTitle string

	// Turns (🔁 server round-trips) — the third metric in the triad (epic
	// ka-0000001199). Carried off the same gardenerRowView the context/spend chips
	// ride on; buildAgentUsageView's live→total fallback keeps the last-run
	// round-trip count showing after the run closes. HasTurns gates the chip;
	// TurnsLabel is the humanized count ("47", "1.2k").
	HasTurns   bool
	TurnsLabel string

	// Owned-epic rollup (task ka-0000001266): the epic this gardener drives and
	// how far its issue subtree has drained. HasOwnedEpic gates the chip; the
	// card renders "<closed>/<total> done · <pct>%" linking to OwnedEpicID.
	HasOwnedEpic    bool
	OwnedEpicID     string
	OwnedEpicTotal  int
	OwnedEpicClosed int
	OwnedEpicOpen   int
	OwnedEpicPct    int

	// Recent runs (oldest→newest, last-N). Pill color by .Outcome. Kept as the
	// toggle/fallback for the sparkline (task ka-0000001693): the card shows the
	// verdict-over-time Sparkline by default and reveals these discrete pills
	// behind a client-only toggle, falling back to them automatically when there
	// is too little history to draw a line (Sparkline.HasData == false).
	Runs []gardenerRunGlyph

	// Sparkline is the compact verdict-over-time trajectory line drawn from the
	// gardener's FULL bounded run history (up to gardenerTrajectorySparkRuns),
	// tinted by VerdictKind (task ka-0000001693). It is the default recent-run
	// display; HasData is false when the gardener has < mapsSparkMinRuns runs, in
	// which case the card renders the discrete strip / counters instead.
	Sparkline mapsSparkView

	// Last-run inline result.
	IsRunning        bool
	LastRunTLDR      string
	LastRunOutcome   string
	LastCompletedAgo string

	// Live-agent overlay (maps2 v2, ka-0000000595). The currently-live agent on
	// this gardener, projected off the same last-agent bead the last-run peek
	// already fetched (no extra fan-out — see gardenerRowView.LiveAgent*).
	// LiveAgentName is the working agent's display name; LiveAgentHref links to
	// its bead; LiveAgentAlive gates the pulsing live dot to a genuinely-alive
	// agent (the /agents liveness predicate), so a closed/wedged shell renders
	// the name as a plain link without the dot. All empty/false when no live
	// agent is known.
	LiveAgentName  string
	LiveAgentHref  string
	LiveAgentAlive bool

	// Inline attention signal (populated only on NeedsAttention rows).
	AttentionKind       string // "decision" | "blocker" | ""
	AttentionSignal     string
	AttentionDecisionID string
}

// NameDivergesFromTitle mirrors gardenerRowView's reconciliation so the card can
// surface the underlying bead title as a subtitle when the displayed spec name
// differs from it (bug ka-0000008054), rather than silently showing only one.
func (c mapsCardView) NameDivergesFromTitle() bool {
	n := strings.TrimSpace(c.Name)
	t := strings.TrimSpace(c.Title)
	if n == "" || t == "" {
		return false
	}
	return !strings.EqualFold(n, t)
}

// mapsCardSearchText builds the lowercased substring haystack for a card's
// client-side search (maps2 v2, ka-0000006434). It folds the fields an operator
// would type to find a gardener — display name, the underlying bead title (which
// can diverge from the name), the terminal goal, the owner (both the display
// label and the slug), and the bead id — into one space-joined, lowercased
// string. Empty parts are skipped so the haystack carries no stray separators.
func mapsCardSearchText(r gardenerRowView) string {
	parts := make([]string, 0, 6)
	for _, p := range []string{r.Name, r.Title, r.TerminalGoal, r.OwnerDisplay, r.OwnerSlug, r.ID} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// mapsBoardView is the whole /maps page model: the cards plus a fleet-health
// summary strip that leads the board with signal (total / needs-attention /
// running), echoing the /gardeners summary idiom.
// Workflows (task ka-0000004778 / "C2") are the connected components of the
// `chains_to` edge set among the gardeners — rendered as left→right rails of
// gardener cards joined by directed handoff arrows, ABOVE the plain card grid.
// Cards that belong to no chain stay in Cards (the standalone grid). When no
// chaining edges exist (or the edge fetch is skipped/over-cap), Workflows is
// empty and Cards holds every gardener — i.e. the board renders exactly as it
// did before C2.
type mapsBoardView struct {
	Cards          []mapsCardView
	Workflows      []mapsWorkflowView
	Total          int
	NeedsAttention int
	Running        int
	// WorkflowCount / DanglingChains lead the workflow strip with signal; only
	// meaningful when Workflows is non-empty. PendingChains counts workflows
	// carrying a drawn-but-not-yet-spawned `chain.to` successor (task
	// ka-0000005273 / "C3"), surfaced as its own pill distinct from dangling.
	WorkflowCount  int
	DanglingChains int
	PendingChains  int
	BasePath       string
	// Owner grouping (maps2 v2, ka-0000002262). When Grouped is true the
	// standalone Cards grid is rendered as the per-owner collapsible sections in
	// OwnerGroups instead of a single flat grid (see maps_group.go). Cards is
	// left intact so the summary counts and the empty-state check still read off
	// it; the template just renders OwnerGroups when Grouped.
	Grouped     bool
	OwnerGroups []mapsOwnerGroup
}

// buildMapsBoard projects already-sorted gardener rows into the board model. It
// is pure and deterministic: it does NOT sort (the caller applies
// sortGardenerRows so /maps and /gardeners share one order) and does NOT fetch.
// Each row → one card; the summary counts are tallied over the rendered set.
func buildMapsBoard(rows []gardenerRowView, basePath string) mapsBoardView {
	board := mapsBoardView{
		Cards:    make([]mapsCardView, 0, len(rows)),
		Total:    len(rows),
		BasePath: basePath,
	}
	for _, r := range rows {
		triggerKind := mapsTriggerKind(r)
		card := mapsCardView{
			ID:                  r.ID,
			Name:                r.Name,
			Title:               r.Title,
			Href:                r.Href,
			TerminalGoal:        r.TerminalGoal,
			OwnerDisplay:        r.OwnerDisplay,
			OwnerSlug:           r.OwnerSlug,
			Initials:            actorInitials(r.Name),
			SearchText:          mapsCardSearchText(r),
			TriggerKind:         triggerKind,
			TriggerKindLabel:    mapsTriggerKindLabel(triggerKind),
			NextFire:            r.NextFire,
			NextFireRelative:    r.NextFireRelative,
			NextFireError:       r.NextFireError,
			TriggerHref:         basePath + "/triggers",
			RunsSpawned:         r.RunsSpawned,
			RunsCompleted:       r.RunsCompleted,
			RunsDerived:         r.RunsDerived,
			Verdict:             r.Verdict,
			VerdictKind:         r.VerdictKind,
			VerdictDetail:       r.VerdictDetail,
			NeedsAttention:      r.NeedsAttention,
			Status:              r.Status,
			GoalState:           r.GoalState,
			Enabled:             r.Enabled,
			IsClosed:            r.IsClosed,
			HasSpend:            r.HasSpend,
			SpendLabel:          r.SpendLabel,
			SpendStale:          r.SpendStale,
			HasContext:          r.HasContext,
			ContextLabel:        r.ContextLabel,
			ContextPct:          r.ContextPct,
			ContextTitle:        r.ContextTitle,
			HasTurns:            r.HasTurns,
			TurnsLabel:          r.TurnsLabel,
			HasOwnedEpic:        r.HasOwnedEpic,
			OwnedEpicID:         r.OwnedEpicID,
			OwnedEpicTotal:      r.OwnedEpicTotal,
			OwnedEpicClosed:     r.OwnedEpicClosed,
			OwnedEpicOpen:       r.OwnedEpicOpen,
			OwnedEpicPct:        r.OwnedEpicPct,
			Runs:                mapsRecentRuns(r.Runs),
			Sparkline:           mapsSparkline(r.Runs),
			IsRunning:           r.IsRunning,
			LastRunTLDR:         r.LastRunTLDR,
			LastRunOutcome:      r.LastRunOutcome,
			LastCompletedAgo:    r.LastCompletedAgo,
			LiveAgentName:       r.LiveAgentName,
			LiveAgentHref:       r.LiveAgentHref,
			LiveAgentAlive:      r.LiveAgentAlive,
			AttentionKind:       r.AttentionKind,
			AttentionSignal:     r.AttentionSignal,
			AttentionDecisionID: r.AttentionDecisionID,
		}
		board.Cards = append(board.Cards, card)
		if r.NeedsAttention {
			board.NeedsAttention++
		}
		if r.IsRunning {
			board.Running++
		}
	}
	return board
}

// mapsRecentRuns returns the last mapsRecentRunStrip glyphs (oldest→newest),
// leaving the row's full sparkline untouched. A legacy gardener with no
// ProgressState carries an empty slice — the card then renders counters + the
// last-run chip only.
func mapsRecentRuns(runs []gardenerRunGlyph) []gardenerRunGlyph {
	if len(runs) <= mapsRecentRunStrip {
		return runs
	}
	return runs[len(runs)-mapsRecentRunStrip:]
}

// applyMapsChainEdges layers gardener-chaining workflows onto an already-built
// board (task ka-0000004778 / "C2"). It partitions the board's cards into
// connected components of the `chains_to` edge set: each component becomes a
// workflow rail (board.Workflows), and the cards that belong to no chain remain
// in board.Cards as the standalone grid. The summary counts (WorkflowCount,
// DanglingChains) are tallied here.
//
// It is pure and deterministic — the fetch of edges happens in the handler
// (loadGardenerChainEdges); this owns the shape. With no edges it is a no-op:
// board.Cards is left untouched and Workflows stays empty, so the board renders
// exactly as it did before C2.
//
// The card order within board.Cards is preserved for the standalone remainder
// (the caller already applied sortGardenerRows), so /maps and /gardeners still
// agree on ordering for the un-chained fleet.
func applyMapsChainEdges(board mapsBoardView, edges []mapsChainEdge) mapsBoardView {
	if len(edges) == 0 || len(board.Cards) == 0 {
		return board
	}
	cardsByID := make(map[string]mapsCardView, len(board.Cards))
	for _, c := range board.Cards {
		cardsByID[c.ID] = c
	}
	workflows, standaloneIDs := buildMapsWorkflows(cardsByID, edges)
	if len(workflows) == 0 {
		// Every edge was off-board / dropped — leave the grid intact.
		return board
	}

	// Rebuild the standalone grid preserving the caller's original card order,
	// keeping only the ids that belong to no workflow.
	standaloneSet := make(map[string]struct{}, len(standaloneIDs))
	for _, id := range standaloneIDs {
		standaloneSet[id] = struct{}{}
	}
	remainder := make([]mapsCardView, 0, len(standaloneIDs))
	for _, c := range board.Cards {
		if _, ok := standaloneSet[c.ID]; ok {
			remainder = append(remainder, c)
		}
	}

	board.Cards = remainder
	board.Workflows = workflows
	board.WorkflowCount = len(workflows)
	for _, wf := range workflows {
		if wf.Dangling {
			board.DanglingChains++
		}
		if wf.Pending {
			board.PendingChains++
		}
	}
	return board
}
