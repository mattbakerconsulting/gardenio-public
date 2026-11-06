package advice

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"

	"gardenio/controller/internal/beadsapi"
)

// MatchedAdvice pairs an advice bead with the labels that caused it to match.
type MatchedAdvice struct {
	Bead          *beadsapi.BeadDetail
	MatchedLabels []string
	Scope         string
	ScopeTarget   string
	ScopeHeader   string
}

// IsFixture reports whether the advice bead is an E2E/test fixture and must
// not be loaded into real agent sessions. Detection is label-based: any label
// with the `fixture:` prefix marks the bead as a fixture (e.g.
// `fixture:e2e:advice`, `fixture:e2e`, `kind:fixture`). The `kind:fixture`
// label is also recognized for symmetry with the bead description.
func IsFixture(bead *beadsapi.BeadDetail) bool {
	for _, l := range bead.Labels {
		clean := StripGroupPrefix(l)
		if strings.HasPrefix(clean, "fixture:") || clean == "kind:fixture" {
			return true
		}
	}
	return false
}

// ListAdviceForAgent fetches open advice beads and filters them by the agent's
// subscriptions. Fixture advice (see IsFixture) is unconditionally excluded so
// E2E test fixtures cannot leak into a real agent's loadout, and bare-project
// broadcasts without the scope:project-broadcast opt-in are denied (see
// IsDeniedBroadcast). Returns matched advice and the computed subscription
// list.
func ListAdviceForAgent(ctx context.Context, daemon *beadsapi.Client, agentID string) ([]MatchedAdvice, []string, error) {
	allAdvice, err := ListOpenAdvice(ctx, daemon)
	if err != nil {
		return nil, nil, err
	}

	subs := BuildAgentSubscriptions(agentID, nil)
	subs = EnrichAgentSubscriptions(ctx, daemon, agentID, subs)

	var matched []MatchedAdvice
	for _, bead := range allAdvice {
		if IsFixture(bead) {
			continue
		}
		if IsDeniedBroadcast(bead) {
			slog.Warn("advice: dropping bare-project broadcast bead — add role:/agent: targeting or the "+BroadcastOptInLabel+" label",
				"bead", bead.ID, "labels", bead.Labels)
			continue
		}
		if MatchesSubscriptions(bead.Labels, subs) {
			ml := FindMatchedLabels(bead.Labels, subs)
			scope, target := CategorizeScopeForMatch(bead.Labels, ml)
			matched = append(matched, MatchedAdvice{
				Bead:          bead,
				MatchedLabels: ml,
				Scope:         scope,
				ScopeTarget:   target,
				ScopeHeader:   BuildScopeHeader(scope, target),
			})
		}
	}

	matched = ApplySupersessions(matched)

	return matched, subs, nil
}

// SortByPriorityRecency orders advice within a scope group so that, when the
// byte-budget cascade in the prime render path must drop items, the
// highest-value advice is the last to be evicted (F1, ka-0000008175).
//
// The ordering axes, most significant first, are:
//
//  1. Priority — ascending numeric. kbeads priority is 0 = P0 (critical) …
//     4 = P4 (lowest), so ascending numeric surfaces the most important advice
//     first ("priority desc" in the human sense).
//  2. Recency (UpdatedAt) — most recently updated first. A freshly written or
//     freshly edited/superseding bead outranks a stale one of equal priority.
//  3. Recency (CreatedAt) — most recently created first, breaking ties when
//     UpdatedAt is equal (e.g. two never-edited beads).
//  4. Bead ID — ascending, as a final total-order tiebreak so an identical
//     input corpus always yields an identical retained/evicted set and order,
//     regardless of the (arbitrary) server ListOpenAdvice order it arrived in.
//
// The sort is in place and total (the ID tiebreak guarantees no two distinct
// beads ever compare equal), so it is fully deterministic and reproducible.
// Callers sort each scope group independently, preserving the documented
// cross-group order (global → project → role → agent) as the primary priority
// axis and applying this intra-group ordering underneath it.
func SortByPriorityRecency(items []MatchedAdvice) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i].Bead, items[j].Bead
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

// IsExpired reports whether the advice bead has an expires_at timestamp in the
// past. An absent or unparseable value is treated as never-expired.
func IsExpired(bead *beadsapi.BeadDetail) bool {
	s := bead.Fields["expires_at"]
	if s == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return false
	}
	return time.Now().After(t)
}

// ListOpenAdvice fetches all open advice beads from the daemon. Closed and
// expired beads are excluded so they don't appear in gb prime or any other
// advice read path.
//
// It PAGES the fetch (ListAllBeadsFiltered) rather than a single capped call.
// The old single-shot Limit:500 was a silent correctness bug (F10, ka-0000005760):
// once the open-advice pool exceeded 500, every bead past the cap was dropped
// BEFORE matching / supersession / budget ever ran — invisible to every
// byte-budget metric and truncation footer, and fatally close (open advice was
// 354 and growing ~110/month when this was measured 2026-08-27). Paging exhausts
// the whole open set no matter how large it grows, so no advice is ever silently
// dropped upstream of budgeting.
func ListOpenAdvice(ctx context.Context, daemon *beadsapi.Client) ([]*beadsapi.BeadDetail, error) {
	result, err := daemon.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"advice"},
		Statuses: []string{"open"},
	})
	if err != nil {
		return nil, err
	}
	// Belt-and-suspenders: filter out any non-open beads client-side
	// in case the server doesn't honor the status filter.
	open := make([]*beadsapi.BeadDetail, 0, len(result.Beads))
	for _, b := range result.Beads {
		if b.Status == "closed" {
			continue
		}
		if IsExpired(b) {
			continue
		}
		open = append(open, b)
	}
	return open, nil
}

// FleetArchetypeWindow bounds how far back the archetype derivation looks for
// agent beads. Agent beads close within minutes of a pod despawning, so
// active-only status filtering yields a near-empty set (often <10 beads); the
// meaningful archetype population is the fleet that has run recently. This
// window is the balance point: wide enough to capture every real (project, role)
// pair the fleet runs (a 14-day window covered all of them, incl. the once-a-day
// archetypes), narrow enough that the query stays cheap and a retired archetype
// eventually ages out of the audit. Measured for ka-0000002153.
const FleetArchetypeWindow = 21 * 24 * time.Hour

// FleetArchetypePageSize bounds how many agent beads are held in memory at once
// while walking FleetArchetypeWindow. The window itself is large — 8,838 beads /
// 66.4 MiB of JSON when measured on 2026-07-27 — but the derived output is only a
// few dozen (project, role) pairs, so there is never a reason to materialize the
// whole window. Fetching it in one shot is what OOMKilled the 256Mi controller and
// took the cost gauges dark for ~7.75h (ka-0000006669); at 500 beads/page the peak
// is ~4 MiB regardless of how large the fleet grows.
//
// Paging also fixes a silent CORRECTNESS bug in the old single-shot call. It asked
// for Limit: 20000, but the daemon clamps any list request to maxListBeadsLimit =
// 10000 (kbeads/internal/server/http_beads.go) without erroring — so once the
// window exceeds 10k agent beads, the one-shot query would quietly return a
// truncated set and archetypes would vanish from the audit with no signal. At 8,838
// beads that ceiling was weeks away. A paged walk has no such cap.
const FleetArchetypePageSize = 500

// fleetArchetypeMaxPages caps the paged walk so a daemon that ignores offset (or
// a fleet that grows without bound) cannot spin forever. 200 pages × 500 = 100k
// agent beads, ~11× the measured window — far beyond any real fleet, but finite.
const fleetArchetypeMaxPages = 200

// ArchetypePageLister is the subset of the beads client the paged archetype walk
// needs. Narrow interface so both the CLI path (*beadsapi.Client) and the metrics
// collector's querier can share one implementation, and so tests can drive the
// paging directly.
type ArchetypePageLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// FleetArchetypesPaged derives the real (project, role) archetype set from the
// agent beads created within FleetArchetypeWindow, folding each page into the
// result set and DISCARDING it before fetching the next. Peak memory is one page,
// not the whole window — the fix for the unbounded fetch in ka-0000006669.
//
// Correctness note: paging with a stable sort matters. Agent beads are created
// continuously, so a walk ordered by anything mutable could skip or repeat rows as
// the underlying set shifts. created_at is immutable for a bead, and the window is
// a closed lower bound, so ordering by it gives a stable page sequence; duplicates
// across page boundaries are harmless anyway because the accumulator is a set.
//
// now is the reference time for the window; callers that lack a clock should pass
// time.Now().
func FleetArchetypesPaged(ctx context.Context, daemon ArchetypePageLister, now time.Time) ([]Archetype, error) {
	seen := map[Archetype]struct{}{}
	cutoff := now.Add(-FleetArchetypeWindow)

	for page := range fleetArchetypeMaxPages {
		// ArchetypesFromAgentBeads reads only project and role (fields or
		// labels), so the walk skips the rest of each ~8 KB row (gd-6PXLuR3juD).
		result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:        []string{"agent"},
			CreatedAfter: cutoff,
			Sort:         "created_at",
			Limit:        FleetArchetypePageSize,
			Offset:       page * FleetArchetypePageSize,
		}.OnlyFields("project", "role"))
		if err != nil {
			return nil, err
		}
		// Fold this page into the set, then drop the reference to the page's
		// beads so the GC can reclaim it before the next fetch.
		for _, a := range ArchetypesFromAgentBeads(result.Beads) {
			seen[a] = struct{}{}
		}
		if len(result.Beads) < FleetArchetypePageSize {
			break // short page = last page
		}
	}

	return sortedArchetypes(seen), nil
}

// ListFleetArchetypes fetches the agent beads created within FleetArchetypeWindow
// and derives the real (project, role) archetype set from them
// (ArchetypesFromAgentBeads). This is the fleet the advice audit measures — the
// replacement for the phantom advice-corpus cross-product (ka-0000002153). now is
// the reference time for the window; callers that lack a clock should pass
// time.Now().
//
// Delegates to FleetArchetypesPaged so this path is memory-bounded too — it is the
// same window that OOMKilled the controller (ka-0000006669), and `gb advice audit`
// runs it in an agent pod.
func ListFleetArchetypes(ctx context.Context, daemon *beadsapi.Client, now time.Time) ([]Archetype, error) {
	return FleetArchetypesPaged(ctx, daemon, now)
}

// SimilarAdvice holds an existing advice bead and its similarity score.
type SimilarAdvice struct {
	Bead  *beadsapi.BeadDetail
	Score float64
}

// DuplicateThreshold is the minimum word-overlap score to consider advice a
// likely duplicate.
const DuplicateThreshold = 0.4

// FindSimilar checks existing open advice beads for title similarity with the
// given title. Returns beads whose word-overlap Jaccard score meets or exceeds
// DuplicateThreshold, sorted by score descending.
func FindSimilar(ctx context.Context, daemon *beadsapi.Client, title string) ([]SimilarAdvice, error) {
	existing, err := ListOpenAdvice(ctx, daemon)
	if err != nil {
		return nil, err
	}

	newWords := tokenize(title)
	if len(newWords) == 0 {
		return nil, nil
	}

	var similar []SimilarAdvice
	for _, bead := range existing {
		existingWords := tokenize(bead.Title)
		score := jaccardSimilarity(newWords, existingWords)
		if score >= DuplicateThreshold {
			similar = append(similar, SimilarAdvice{Bead: bead, Score: score})
		}
	}

	sort.Slice(similar, func(i, j int) bool {
		return similar[i].Score > similar[j].Score
	})
	return similar, nil
}

// tokenize splits text into lowercase words, filtering out short stop words.
func tokenize(text string) map[string]bool {
	words := make(map[string]bool)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) > 2 { // skip short words like "a", "an", "to", etc.
			words[w] = true
		}
	}
	return words
}

// jaccardSimilarity computes |A∩B| / |A∪B| for two word sets.
func jaccardSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	intersection := 0
	for w := range a {
		if b[w] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
