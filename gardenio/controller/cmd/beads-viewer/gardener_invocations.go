package main

// Gardener invocations drawer (thread request 2026-06-02, task ka-0000007414).
//
// GET /gardener/{id}/invocations?detail=tldr|full
//
// The /gardeners index gives each gardener row an "expand" button. Clicking it
// HTMX-loads this fragment into a per-row drawer: a compact list of the
// gardener's invocations (its spawned agent iterations), newest-first, showing
// either:
//
//   - detail=tldr (default) — just each run's one-line TLDR (or a derived
//     outcome when the run left no wrap-up), or
//   - detail=full           — the TLDR plus the run's accomplishments prose.
//
// A toggle in the drawer header swaps between the two without leaving the
// index. This is deliberately lighter than the full /gardener/{id} life-thread
// (no agent-card chrome, no pagination) — it is an at-a-glance "what has this
// gardener actually been doing" peek, with a link through to the full page.
//
// It reuses gardenerIterations (gardener.go) for the ordered run set and the
// dual-key (trigger_id / schedule_id) ownership match, so the drawer never
// drifts from the detail page's understanding of which runs belong to the
// gardener. The TLDR + accomplishments are re-parsed from each iteration's raw
// wrapup field (the embedded agentRowView only carries a single collapsed
// WrapUpSummary; the drawer needs the two fields split for the toggle).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// gardenerInvocationsLimit caps how many invocations the drawer renders. The
// drawer is an at-a-glance peek, not the full life-thread — operators wanting
// every run follow the "open full page" link to /gardener/{id}. Newest-first,
// so the cap drops the OLDEST runs, which is the right bias for a peek.
const gardenerInvocationsLimit = 30

// gardenerInvocationView is one invocation row in the drawer. It is a thin
// projection: identity + link, the per-run label, and the two wrap-up fields
// (TLDR, accomplishments) split out so the detail toggle can show one or both.
type gardenerInvocationView struct {
	ID       string
	Href     string // -> /{runID} the individual run's agent bead page
	RunLabel string
	IsLive   bool
	RunOK    bool // closed && outcome != "fail" — drives a subtle ok/▪ glyph
	RunFail  bool // closed && outcome == "fail"

	// TLDR is the run's wrap-up tldr; Headline backstops it with a derived
	// outcome / task title so the row is never blank. Accomplishments is the
	// run's accomplishments prose (shown only in detail=full).
	TLDR            string
	Headline        string
	Accomplishments string

	// Usage is this run's own cost + context + turns (bug ka-0000001009 / C2),
	// carried verbatim from the iteration's embedded agentRowView.Usage so the
	// drawer can attribute a spend spike to a specific run. HasCost/HasTurns on
	// it gate the chips; the drawer renders the shared agent_usage_chips strip.
	Usage agentUsageView

	Ago string
}

// handleGardenerInvocations renders the expandable invocations drawer for one
// gardener (the /gardeners row "expand" button target). detail=full includes the
// accomplishments prose; anything else (default) is TLDR-only.
func (s *Server) handleGardenerInvocations(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	detailFull := r.URL.Query().Get("detail") == "full"

	ctx := r.Context()
	iters, _, err := s.gardenerIterations(ctx, id, s.now())
	if err != nil {
		s.logger.Error("gardener invocations: iterations", "id", id, "error", err)
		// Degrade quietly — the index row already rendered; a failed expand
		// should show an inline note, not a 500.
		s.renderPartial(w, r, "templates/partials/gardener_invocations.html", "gardener_invocations",
			map[string]any{"GardenerID": id, "Error": "Could not load this gardener's invocations right now."})
		return
	}

	// Re-fetch raw wrapup per iteration to split TLDR from accomplishments. The
	// iteration set is already bounded (gardenerIterationPageLimit) and we cap the
	// drawer below, so this is a bounded batch even on a long-lived gardener.
	rawByID := s.invocationWrapupsByID(ctx, iters)

	total := len(iters)
	if len(iters) > gardenerInvocationsLimit {
		iters = iters[:gardenerInvocationsLimit]
	}

	rows := make([]gardenerInvocationView, 0, len(iters))
	for _, it := range iters {
		tldr, acc := parseWrapUpTLDRAndAccomplishments(rawByID[it.ID])
		headline := tldr
		if headline == "" {
			headline = invocationHeadline(it.WrapUpSummary, it.DerivedOutcome, it.TaskTitle, it.Name, it.ID)
		}
		rows = append(rows, gardenerInvocationView{
			ID:              it.ID,
			Href:            s.basePath + "/" + it.ID,
			RunLabel:        it.RunLabel,
			IsLive:          it.IsLive,
			RunOK:           !it.IsLive && it.RunOutcome != "fail",
			RunFail:         !it.IsLive && it.RunOutcome == "fail",
			TLDR:            tldr,
			Headline:        headline,
			Accomplishments: acc,
			Usage:           it.Usage, // per-run cost/tokens (C2, ka-0000001009)
			Ago:             it.Ago,
		})
	}

	data := map[string]any{
		"GardenerID":   id,
		"GardenerHref": s.basePath + "/gardener/" + id,
		"Rows":         rows,
		"DetailFull":   detailFull,
		"Total":        total,
		"Shown":        len(rows),
		"Truncated":    total > len(rows),
		// Toggle hrefs: each flips `detail` while staying on this gardener's
		// drawer endpoint, so the partial can re-request the other view.
		"TLDRURL": s.basePath + "/gardener/" + id + "/invocations?detail=tldr",
		"FullURL": s.basePath + "/gardener/" + id + "/invocations?detail=full",
	}
	s.renderPartial(w, r, "templates/partials/gardener_invocations.html", "gardener_invocations", data)
}

// invocationWrapupsByID batch-fetches the raw `wrapup` field for each closed
// iteration so the drawer can split TLDR from accomplishments. Live runs have
// no wrap-up yet, so we skip them. Returns a map keyed by iteration bead id;
// a missing/failed fetch simply yields no entry (the row falls back to its
// embedded WrapUpSummary / derived outcome).
func (s *Server) invocationWrapupsByID(ctx context.Context, iters []gardenerIterationView) map[string]string {
	ids := make([]string, 0, len(iters))
	for _, it := range iters {
		if !it.IsLive {
			ids = append(ids, it.ID)
		}
	}
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	beads, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Error("gardener invocations: batch wrapups", "error", err)
		return out
	}
	for idv, b := range beads {
		if b != nil {
			if wu := strings.TrimSpace(b.Fields["wrapup"]); wu != "" {
				out[idv] = wu
			}
		}
	}
	return out
}

// parseWrapUpTLDRAndAccomplishments pulls the `tldr` and `accomplishments`
// fields out of a raw wrapup JSON blob, both trimmed. Either may be empty.
// Mirrors parseWrapUpSummary's field shape (agents.go) so the drawer and the
// row chip agree on what "TLDR" means.
func parseWrapUpTLDRAndAccomplishments(wrapupJSON string) (tldr, accomplishments string) {
	wrapupJSON = strings.TrimSpace(wrapupJSON)
	if wrapupJSON == "" {
		return "", ""
	}
	var w struct {
		TLDR            string `json:"tldr"`
		Accomplishments string `json:"accomplishments"`
	}
	if err := json.Unmarshal([]byte(wrapupJSON), &w); err != nil {
		return "", ""
	}
	// Defensively reverse legacy pre-escaped angle brackets (ka-0000005806) so
	// the template's single render-time escape produces correct output. No-op
	// for post-fix beads, which store raw text.
	return unescapeLegacyWrapUpTLDR(strings.TrimSpace(w.TLDR)), strings.TrimSpace(w.Accomplishments)
}

// invocationHeadline returns the first trimmed-non-empty argument, or "". Used
// to backstop an empty TLDR with a derived outcome / task title so a drawer row
// is never blank.
func invocationHeadline(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
