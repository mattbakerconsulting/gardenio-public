//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
)

// TestSearchRelevance_RealPostgres exercises the ka-0000004745 scoped
// `kd search` relevance fix (queryListBeads filter.Search tokenization +
// title-match relevance boost in ORDER BY) against a real Postgres backend.
//
// The sqlmock unit tests in postgres_test.go pin the SQL SHAPE; this test pins
// the actual BEHAVIOR end-to-end so the two symptoms of ka-0000000507 can never
// silently regress:
//
//	A. Multi-word, out-of-order queries match. Before the fix the whole joined
//	   query was a single literal ILIKE substring, so `litellm ProxyException`
//	   required that exact contiguous phrase and returned nothing even when both
//	   words were present (just not adjacent / in that order).
//	B. An exact-title match ranks above a merely-recent match for a
//	   high-frequency single term. Before the fix the default sort was
//	   `created_at DESC`, so a brand-new bead that merely mentions the term in
//	   its description buried the old exact-title bug.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the default
// `go test ./...` workflow stays sqlmock-only per CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestSearchRelevance_RealPostgres
//
// The test scopes its fixtures to a unique fleet and soft-deletes everything it
// created on cleanup so it is safe against a shared DB.
func TestSearchRelevance_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-search-rel-" + suffix

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}

	// create inserts a bead at an explicit created_at so recency ordering is
	// deterministic regardless of wall-clock insert order.
	create := func(t *testing.T, title, description string, createdAt time.Time) string {
		t.Helper()
		b := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: model.TypeBug,
			Title: title, Description: description, Status: model.StatusOpen,
			CreatedAt: createdAt, UpdatedAt: createdAt, Fleet: fleet, OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, b); err != nil {
			t.Fatalf("create bead %q: %v", title, err)
		}
		return b.ID
	}

	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	old := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)

	// canonicalBug: the real-world shape from gd-nthTHEQCWJ — both query terms
	// appear in the title but NON-CONTIGUOUSLY and OUT OF ORDER vs the query
	// `litellm ProxyException` (title has "ProxyExceptions" before "LiteLLM"...
	// actually title leads with LiteLLM, then ProxyExceptions later — the words
	// are separated by other tokens, which is what broke the single-phrase
	// ILIKE). It is also the OLDEST bead so recency alone would bury it.
	canonicalBug := create(t,
		"LiteLLM: stale key ProxyExceptions (368/24h) and haiku model Not Found errors",
		"Investigate the proxy exception storm.", old)

	// descOnlyMatch: contains both terms only in the DESCRIPTION (not the
	// title), so it matches the multi-term filter but must rank BELOW the
	// title-substring matches. Mid-recency.
	descOnlyMatch := create(t,
		"Proxy investigation follow-up",
		"Root-caused the litellm ProxyException spam to a stale key.",
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))

	// recentNoise: a brand-new bead that mentions only the high-frequency single
	// term `litellm` in its description. For symptom B it must NOT outrank the
	// exact-title canonicalBug even though it is far more recent.
	recentNoise := create(t,
		"Daily litellm cost report",
		"Routine litellm spend summary for the day.", recent)

	searchIDs := func(t *testing.T, query string) []string {
		t.Helper()
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{
			Fleet:  fleet,
			Search: query,
			Limit:  100,
		})
		if err != nil {
			t.Fatalf("ListBeads(search=%q): %v", query, err)
		}
		ids := make([]string, len(beads))
		for i, b := range beads {
			ids[i] = b.ID
		}
		return ids
	}

	indexOf := func(ids []string, id string) int {
		for i, v := range ids {
			if v == id {
				return i
			}
		}
		return -1
	}

	// Symptom A: out-of-order, multi-word query matches a bead whose title +
	// description contain both terms non-contiguously. The pre-fix single-phrase
	// ILIKE returned nothing here.
	t.Run("multi_word_out_of_order_matches", func(t *testing.T) {
		ids := searchIDs(t, "litellm ProxyException")
		if indexOf(ids, canonicalBug) < 0 {
			t.Errorf("canonical bug %s not returned for out-of-order query "+
				"'litellm ProxyException'; got %v", canonicalBug, ids)
		}
		if indexOf(ids, descOnlyMatch) < 0 {
			t.Errorf("description-only match %s not returned for "+
				"'litellm ProxyException'; got %v", descOnlyMatch, ids)
		}
		// recentNoise has only `litellm` (no ProxyException) → must NOT match the
		// ANDed multi-term filter.
		if indexOf(ids, recentNoise) >= 0 {
			t.Errorf("recentNoise %s should NOT match 'litellm ProxyException' "+
				"(missing the second term); got %v", recentNoise, ids)
		}
	})

	// Symptom A, ranking refinement: the title-substring match (canonicalBug)
	// ranks ABOVE the description-only match for the same multi-term query.
	t.Run("title_match_outranks_description_only", func(t *testing.T) {
		ids := searchIDs(t, "litellm ProxyException")
		ci, di := indexOf(ids, canonicalBug), indexOf(ids, descOnlyMatch)
		if ci < 0 || di < 0 {
			t.Fatalf("expected both canonicalBug and descOnlyMatch in results; got %v", ids)
		}
		if ci >= di {
			t.Errorf("expected title match %s (pos %d) to rank above "+
				"description-only %s (pos %d); got %v", canonicalBug, ci, descOnlyMatch, di, ids)
		}
	})

	// Symptom B: for a high-frequency single term, the exact-title match
	// outranks a more-recent description-only match. Pre-fix, created_at DESC
	// put recentNoise first and buried canonicalBug.
	t.Run("exact_title_outranks_recency", func(t *testing.T) {
		ids := searchIDs(t, "litellm")
		ci, ni := indexOf(ids, canonicalBug), indexOf(ids, recentNoise)
		if ci < 0 {
			t.Fatalf("canonical bug %s not returned for single-term 'litellm'; got %v", canonicalBug, ids)
		}
		if ni < 0 {
			t.Fatalf("recentNoise %s not returned for single-term 'litellm'; got %v", recentNoise, ids)
		}
		if ci >= ni {
			t.Errorf("expected exact-title match %s (pos %d) to outrank "+
				"more-recent description-only %s (pos %d) for term 'litellm'; got %v",
				canonicalBug, ci, recentNoise, ni, ids)
		}
	})

	// Single-term behavior preserved: a one-word query still matches plain
	// substrings (the empty/single-term path must be unchanged).
	t.Run("single_term_substring_still_matches", func(t *testing.T) {
		ids := searchIDs(t, "haiku")
		if indexOf(ids, canonicalBug) < 0 {
			t.Errorf("single-term 'haiku' should match canonicalBug %s; got %v", canonicalBug, ids)
		}
	})
}

// TestSearchSeparatorNormalization_RealPostgres pins the ka-0000002772 fix
// end-to-end against a real Postgres: a query term's separators (- _ /) are
// expanded to the '%' ILIKE wildcard so a hyphenated multi-term query matches a
// bead REGARDLESS of how that bead spells the compound. The live repro was
//
//	kd search "cluster-admin break-glass" -> 0 beads
//	kd search "cluster admin break glass" -> 3+ beads
//
// because each hyphenated term was a strict literal substring, so the ANDed
// filter zeroed out whenever the bead spelled a compound differently. This test
// creates one bead per spelling (spaced / hyphenated / joined / underscored) and
// asserts every query hyphenation finds every spelling.
//
// Gated by //go:build integration + BEADS_TEST_DATABASE_URL like the sibling
// tests, so the default sqlmock-only `go test ./...` path is unaffected.
func TestSearchSeparatorNormalization_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-search-sep-" + suffix

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}

	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	create := func(t *testing.T, title, description string) string {
		t.Helper()
		b := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: model.TypeBug,
			Title: title, Description: description, Status: model.StatusOpen,
			CreatedAt: at, UpdatedAt: at, Fleet: fleet, OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, b); err != nil {
			t.Fatalf("create bead %q: %v", title, err)
		}
		return b.ID
	}

	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	// One bead per spelling of the two compounds "cluster-admin" and
	// "break-glass". Descriptions carry a shared unique token so we can sanity
	// check the fixture set size independently of the compound spelling.
	spaced := create(t, "Grant cluster admin for break glass rotation", "sepfixture ticket")
	hyphenated := create(t, "Grant cluster-admin for break-glass rotation", "sepfixture ticket")
	joined := create(t, "Grant clusteradmin for breakglass rotation", "sepfixture ticket")
	underscored := create(t, "Grant cluster_admin for break_glass rotation", "sepfixture ticket")
	allSpellings := []string{spaced, hyphenated, joined, underscored}

	searchIDs := func(t *testing.T, query string) map[string]bool {
		t.Helper()
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{Fleet: fleet, Search: query, Limit: 100})
		if err != nil {
			t.Fatalf("ListBeads(search=%q): %v", query, err)
		}
		got := make(map[string]bool, len(beads))
		for _, b := range beads {
			got[b.ID] = true
		}
		return got
	}

	// Each query spelling must find spaced / hyphenated / underscored — the three
	// where a '%' wildcard bridges the separator gap. The fully-joined spelling
	// ("clusteradmin"/"breakglass") only matches queries whose separator-expanded
	// term collapses to a substring of it, i.e. every spelling EXCEPT the spaced
	// one (whose spaces stay literal). We assert the spellings that MUST match per
	// query and never require a spelling the wildcard can't reach.
	//
	// wildcardBridged spellings (spaced, hyphenated, underscored) are matched by
	// every query form here because the query's own separators expand to '%'.
	wildcardBridged := []string{spaced, hyphenated, underscored}
	for _, q := range []string{
		"cluster-admin break-glass",
		"cluster admin break glass",
		"cluster_admin break_glass",
		"cluster/admin break/glass",
	} {
		t.Run("query="+q, func(t *testing.T) {
			got := searchIDs(t, q)
			for _, id := range wildcardBridged {
				if !got[id] {
					t.Errorf("query %q did not match bead %s (should match spaced/hyphenated/underscored spellings); matched=%v",
						q, id, got)
				}
			}
		})
	}

	// The joined spelling is reachable when the query itself uses separators (so
	// each expanded term is a substring of the joined compound). The pre-fix
	// literal-substring behavior returned NOTHING for a hyphenated query here.
	t.Run("hyphenated_query_matches_joined_spelling", func(t *testing.T) {
		got := searchIDs(t, "cluster-admin break-glass")
		if !got[joined] {
			t.Errorf("hyphenated query should match the joined spelling %s; matched=%v", joined, got)
		}
	})

	// Sanity: a single spaced query term matches every spelling whose compound is
	// separated (spaced/hyphenated/underscored) plus, via '%', has no effect on
	// recall for the joined form for the single-term case. Guards that the
	// expansion never REMOVES a previously-matching row.
	t.Run("all_spellings_present_in_fixture", func(t *testing.T) {
		got := searchIDs(t, "rotation")
		for _, id := range allSpellings {
			if !got[id] {
				t.Errorf("expected fixture bead %s to be present for single-term 'rotation'; matched=%v", id, got)
			}
		}
	})
}

// TestSearchRelevanceOption2_RealPostgres exercises the ka-0000003832 Option-2
// FTS upgrade against a real Postgres backend:
//
//  1. RANKING ORDER — the composite score (Option-1 title tier + FTS
//     ts_rank_cd) ranks an exact-title match above a description-only match,
//     and a description-only FTS match above a non-matching bead.
//  2. SCORE EXPOSURE — model.Bead.RelevanceScore is populated and strictly
//     positive for matches, monotonically non-increasing across the result
//     order (it IS the sort key), and 0 on a non-search listing.
//  3. BACKFILL — a row whose search_tsv was forced to NULL (simulating a legacy
//     row that pre-dates migration 039) is filled by BackfillSearchTSVPage, and
//     its FTS-only match (a description term with no ILIKE-adjacent recall gap)
//     then contributes a non-zero ts_rank component.
//
// The trigger (migration 039) keeps new rows current, so the fixtures created
// here already carry a search_tsv; the backfill sub-test forces one back to
// NULL to prove the sweep fills it. Gated identically to the Option-1 test.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestSearchRelevanceOption2_RealPostgres
func TestSearchRelevanceOption2_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-search-rel-o2-" + suffix

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}
	create := func(t *testing.T, title, description string, createdAt time.Time) string {
		t.Helper()
		b := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: model.TypeBug,
			Title: title, Description: description, Status: model.StatusOpen,
			CreatedAt: createdAt, UpdatedAt: createdAt, Fleet: fleet, OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, b); err != nil {
			t.Fatalf("create bead %q: %v", title, err)
		}
		return b.ID
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	when := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// titleHit: the query term appears in the TITLE → highest tier.
	titleHit := create(t, "Kafka consumer rebalance storm", "see the linked dashboards", when)
	// descHit: the query term appears only in the DESCRIPTION → lower tier, but
	// still matches (ILIKE + FTS) and ranks above a non-match.
	descHit := create(t, "Broker latency investigation", "root-caused to a kafka rebalance loop", when)
	// noMatch: contains the term in neither field → must not appear.
	noMatch := create(t, "Unrelated billing reconciliation", "monthly invoice export", when)

	score := func(t *testing.T, query string) map[string]float64 {
		t.Helper()
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{Fleet: fleet, Search: query, Limit: 100})
		if err != nil {
			t.Fatalf("ListBeads(search=%q): %v", query, err)
		}
		out := make(map[string]float64, len(beads))
		// Also assert the result order is non-increasing by score (score IS the
		// primary sort key, so this must always hold).
		prev := math.Inf(1)
		for _, b := range beads {
			out[b.ID] = b.RelevanceScore
			if b.RelevanceScore > prev+1e-9 {
				t.Errorf("results not ordered by descending score: bead %s score %.6f > prev %.6f",
					b.ID, b.RelevanceScore, prev)
			}
			prev = b.RelevanceScore
		}
		return out
	}

	t.Run("ranking_order_and_score_exposure", func(t *testing.T) {
		scores := score(t, "kafka rebalance")
		ts, ok := scores[titleHit]
		if !ok {
			t.Fatalf("titleHit %s not returned for 'kafka rebalance'; scores=%v", titleHit, scores)
		}
		ds, ok := scores[descHit]
		if !ok {
			t.Fatalf("descHit %s not returned for 'kafka rebalance'; scores=%v", descHit, scores)
		}
		if _, present := scores[noMatch]; present {
			t.Errorf("noMatch %s should not be returned for 'kafka rebalance'", noMatch)
		}
		// Score exposure: both matches carry a strictly positive relevance score.
		if ts <= 0 {
			t.Errorf("titleHit relevance score must be > 0, got %.6f", ts)
		}
		if ds <= 0 {
			t.Errorf("descHit relevance score must be > 0, got %.6f", ds)
		}
		// Ranking: the title hit (tier 1 — both terms in title? no — but 'kafka'
		// and 'rebalance' both appear in titleHit's title) outranks the
		// description-only hit by a full integer tier.
		if ts <= ds {
			t.Errorf("expected titleHit score %.6f to exceed descHit score %.6f", ts, ds)
		}
	})

	t.Run("non_search_listing_has_zero_score", func(t *testing.T) {
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{Fleet: fleet, Limit: 100})
		if err != nil {
			t.Fatalf("ListBeads(no search): %v", err)
		}
		for _, b := range beads {
			if b.RelevanceScore != 0 {
				t.Errorf("non-search listing must carry 0 relevance score; bead %s had %.6f",
					b.ID, b.RelevanceScore)
			}
		}
	})

	t.Run("backfill_fills_legacy_null_search_tsv", func(t *testing.T) {
		// Simulate a legacy row that pre-dates migration 039: force its
		// search_tsv back to NULL. (A direct UPDATE of search_tsv would re-fire
		// the BEFORE UPDATE OF title,description trigger only if those columns
		// change; setting search_tsv alone does not, so it stays NULL.)
		if _, err := store.db.ExecContext(ctx,
			`UPDATE beads SET search_tsv = NULL WHERE id = $1`, descHit); err != nil {
			t.Fatalf("force search_tsv NULL: %v", err)
		}

		// The ILIKE branch still matches it (recall is backfill-independent), but
		// its ts_rank component is now 0. Capture the pre-backfill score.
		pre := score(t, "kafka rebalance")[descHit]

		// Run the batched backfill to completion.
		afterID := ""
		for {
			st, err := store.BackfillSearchTSVPage(ctx, afterID, 500, false)
			if err != nil {
				t.Fatalf("BackfillSearchTSVPage(after=%q): %v", afterID, err)
			}
			if st.Done {
				break
			}
			if st.LastID == "" {
				t.Fatalf("non-final backfill page returned no cursor")
			}
			afterID = st.LastID
		}

		// Verify the row is no longer NULL.
		var filled bool
		if err := store.db.QueryRowContext(ctx,
			`SELECT search_tsv IS NOT NULL FROM beads WHERE id = $1`, descHit).Scan(&filled); err != nil {
			t.Fatalf("check search_tsv filled: %v", err)
		}
		if !filled {
			t.Fatalf("descHit %s search_tsv still NULL after backfill", descHit)
		}

		// After backfill the ts_rank component is restored, so the score is >=
		// the pre-backfill (FTS-zero) score and strictly positive.
		post := score(t, "kafka rebalance")[descHit]
		if post <= 0 {
			t.Errorf("descHit post-backfill score must be > 0, got %.6f", post)
		}
		if post < pre-1e-9 {
			t.Errorf("descHit score regressed after backfill: pre=%.6f post=%.6f", pre, post)
		}
	})
}

// TestSearchRelevanceOption3_RealPostgres exercises the ka-0000004282 Option-3
// openness/priority/type WITHIN-TIER boost (design ka-0000000863 §2/§4,
// remediating ka-0000002396) against a real Postgres backend.
//
// The boost appended to searchRelevanceExpr is a within-tier tiebreaker:
//
//   - (CASE WHEN status IN ('open','in_progress') AND type IN ('escalation','decision') THEN 0.5
//     WHEN status IN ('open','in_progress') AND priority <= 0                      THEN 0.3
//     WHEN status IN ('open','in_progress')                                        THEN 0.1
//     ELSE 0 END)
//
// Two behaviors are pinned so the ship-vs-cross-tier decision is captured in a
// test and can never silently drift:
//
//  1. WITHIN-TIER LIFT (the fix). At EQUAL lexical tier — both matching the
//     query only in their DESCRIPTION (tier 0) — an OPEN P0 escalation outranks
//     a CLOSED bug. Pre-boost these tied on the integer tier and the closed bug
//     could win on ts_rank / created_at; the +0.5 escalation boost now lifts it.
//     This is exactly the ka-0000002396 miss (a closed bead outranking an open
//     P0 escalation an agent must never miss).
//
//  2. NO CROSS-TIER JUMP (the deliberately-preserved guarantee, Symptom B). A
//     CLOSED bug whose TITLE matches (tier 2) STILL outranks the open P0
//     escalation whose description matches (tier 0). The boost maxes at 0.5 and
//     ts_rank_cd < 1.0, so the escalation tops out below 1.5 — under the tier-2
//     floor of 2.0. Design §2/§3 shipped the WITHIN-tier boost first; making an
//     open P0 escalation beat a CLOSED title-match bug would need boost >= 1.0 +
//     a tier redesign + a Symptom-B test change, filed as a follow-up decision
//     rather than done silently here. This sub-test is the guard for that line.
//
// Gated identically to the Option-1/Option-2 tests (//go:build integration +
// BEADS_TEST_DATABASE_URL).
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestSearchRelevanceOption3_RealPostgres
func TestSearchRelevanceOption3_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-search-rel-o3-" + suffix

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}

	// createBead inserts a bead with an explicit kind/type/status/priority AND an
	// explicit created_at so the Option-3 boost CASE (which reads exactly the
	// status/type/priority columns) is exercised on real rows and recency is
	// deterministic — letting each sub-test isolate the boost from the
	// created_at DESC tiebreak.
	createBead := func(t *testing.T, kind model.Kind, typ model.BeadType, status model.Status, priority int, title, description string, createdAt time.Time) string {
		t.Helper()
		b := &model.Bead{
			ID: genID(), Kind: kind, Type: typ,
			Title: title, Description: description, Status: status, Priority: priority,
			CreatedAt: createdAt, UpdatedAt: createdAt, Fleet: fleet, OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, b); err != nil {
			t.Fatalf("create bead %q: %v", title, err)
		}
		return b.ID
	}

	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	older := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	// Query term: "orphansweeper" — a rare token so ts_rank is comparable across
	// the two description-only fixtures (each contains it exactly once at weight
	// B) and no unrelated fixture matches. With flag 32 (rank/(rank+1), no length
	// normalization) their ts_rank components are effectively equal, so the +0.5
	// escalation boost is the sole differentiator in the within-tier sub-test.
	//
	// openEscalation: kind=data escalation, OPEN, P0. Term appears ONLY in the
	// description → tier 0. Deliberately the OLDER row so that, absent the boost,
	// created_at DESC would sort it BELOW the newer closed bug. Score =
	// 0 + ts_rank_cd(<1) + 0.5 (escalation boost).
	openEscalation := createBead(t, model.KindData, model.TypeEscalation, model.StatusOpen, 0,
		"Run one kubectl to break the fight-loop",
		"The orphansweeper controller is stuck in a reconcile fight-loop.", older)

	// closedBugDescMatch: kind=issue bug, CLOSED. Term appears ONLY in the
	// description → tier 0 (SAME lexical tier as openEscalation). Made the NEWER
	// of the two so that, absent the boost, created_at DESC would put it ABOVE
	// the escalation — isolating the boost as the sole cause of the intended
	// order. Score = 0 + ts_rank_cd(<1) + 0 (closed → no boost).
	closedBugDescMatch := createBead(t, model.KindIssue, model.TypeBug, model.StatusClosed, 2,
		"Investigate reconcile churn",
		"Root-caused the orphansweeper reconcile churn to a stale finalizer.", newer)

	// closedBugTitleMatch: kind=issue bug, CLOSED, with the term as a NON-leading
	// TITLE substring → tier 2 (title-substring, not exact-title-prefix). Score =
	// 2 + ts_rank_cd(<1) + 0. This is the cross-tier control: it MUST stay above
	// the open escalation (tier 0 + 0.5 boost < 1.5 < 2.0).
	closedBugTitleMatch := createBead(t, model.KindIssue, model.TypeBug, model.StatusClosed, 2,
		"Postmortem: OrphanSweeper crashloop",
		"Closed after the finalizer fix landed.", newer)

	searchResults := func(t *testing.T, query string) ([]string, map[string]float64) {
		t.Helper()
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{Fleet: fleet, Search: query, Limit: 100})
		if err != nil {
			t.Fatalf("ListBeads(search=%q): %v", query, err)
		}
		ids := make([]string, len(beads))
		scores := make(map[string]float64, len(beads))
		for i, b := range beads {
			ids[i] = b.ID
			scores[b.ID] = b.RelevanceScore
		}
		return ids, scores
	}

	indexOf := func(ids []string, id string) int {
		for i, v := range ids {
			if v == id {
				return i
			}
		}
		return -1
	}

	// 1. WITHIN-TIER: at equal lexical tier (both description-only, tier 0), the
	//    open P0 escalation outranks the closed bug — the ka-0000002396 fix.
	t.Run("open_escalation_outranks_closed_bug_within_tier", func(t *testing.T) {
		ids, scores := searchResults(t, "orphansweeper")
		ei, bi := indexOf(ids, openEscalation), indexOf(ids, closedBugDescMatch)
		if ei < 0 {
			t.Fatalf("open escalation %s not returned for 'orphansweeper'; got %v", openEscalation, ids)
		}
		if bi < 0 {
			t.Fatalf("closed desc-match bug %s not returned for 'orphansweeper'; got %v", closedBugDescMatch, ids)
		}
		if ei >= bi {
			t.Errorf("expected open P0 escalation %s (pos %d, score %.6f) to outrank "+
				"same-tier closed bug %s (pos %d, score %.6f); got %v",
				openEscalation, ei, scores[openEscalation], closedBugDescMatch, bi, scores[closedBugDescMatch], ids)
		}
		// The lift is exactly the +0.5 escalation boost over the boost-less closed
		// bug; both share the tier-0 floor and a comparable ts_rank, so the
		// escalation's score must strictly exceed the closed bug's.
		if scores[openEscalation] <= scores[closedBugDescMatch] {
			t.Errorf("escalation boost did not lift score: escalation %.6f <= closed bug %.6f",
				scores[openEscalation], scores[closedBugDescMatch])
		}
	})

	// 2. NO CROSS-TIER JUMP (Symptom B preserved): a CLOSED title-match bug
	//    (tier 2) still outranks the open P0 escalation (tier 0 + boost). This
	//    pins the deliberate ship-vs-cross-tier decision from design §2/§3.
	t.Run("closed_title_match_still_outranks_open_escalation_cross_tier", func(t *testing.T) {
		ids, scores := searchResults(t, "orphansweeper")
		ti, ei := indexOf(ids, closedBugTitleMatch), indexOf(ids, openEscalation)
		if ti < 0 {
			t.Fatalf("closed title-match bug %s not returned for 'orphansweeper'; got %v", closedBugTitleMatch, ids)
		}
		if ei < 0 {
			t.Fatalf("open escalation %s not returned for 'orphansweeper'; got %v", openEscalation, ids)
		}
		if ti >= ei {
			t.Errorf("within-tier boost must NOT cross a tier boundary: expected closed "+
				"TITLE-match bug %s (pos %d, score %.6f, tier 2) to still outrank open "+
				"escalation %s (pos %d, score %.6f, tier 0 + boost); got %v",
				closedBugTitleMatch, ti, scores[closedBugTitleMatch], openEscalation, ei, scores[openEscalation], ids)
		}
		// The escalation's total score stays below the tier-2 floor of 2.0
		// (max boost 0.5 + ts_rank_cd < 1.0 => < 1.5), which is the invariant
		// that keeps the boost within-tier.
		if scores[openEscalation] >= 2.0 {
			t.Errorf("open escalation score %.6f must stay below the tier-2 floor (2.0) "+
				"for the within-tier guarantee to hold", scores[openEscalation])
		}
	})
}

// TestSearchRelevanceOption2Fields_RealPostgres exercises migration 040's
// fields extension (ka-0000005839): the search_tsv vector now indexes a
// decision's context field and option label/short text, so a reference that
// appears ONLY inside an option label or context matches
// `gb decision list --search <ref>` (which forwards to ListBeads' FTS path).
//
// It also pins the reindex backfill (BackfillSearchTSVPage reindex=true): a row
// that carries the PRE-040 title+description-only vector (simulated by forcing
// search_tsv to the 039-form value) is refreshed by the reindex sweep so its
// fields text becomes searchable — the exact migration path an operator runs
// once after deploying 040.
//
// Gated identically to the other relevance tests (//go:build integration +
// BEADS_TEST_DATABASE_URL).
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestSearchRelevanceOption2Fields_RealPostgres
func TestSearchRelevanceOption2Fields_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-search-rel-o2f-" + suffix

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}
	// createDecision makes a type=decision bead whose only occurrence of the
	// probe terms is inside the fields JSON (context + one option label).
	createDecision := func(t *testing.T, title, fields string) string {
		t.Helper()
		b := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: model.TypeDecision,
			Title: title, Description: "no probe terms in the description", Status: model.StatusOpen,
			Fleet: fleet, Fields: json.RawMessage(fields), OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, b); err != nil {
			t.Fatalf("create decision %q: %v", title, err)
		}
		return b.ID
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	matches := func(t *testing.T, query, id string) bool {
		t.Helper()
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{Fleet: fleet, Search: query, Limit: 100})
		if err != nil {
			t.Fatalf("ListBeads(search=%q): %v", query, err)
		}
		for _, b := range beads {
			if b.ID == id {
				return true
			}
		}
		return false
	}

	// The probe term "kubeflow" appears only in an option label; "quarantine"
	// only in the context field. Neither is in the title or description.
	const fields = `{
		"context": "the operator must quarantine the affected node first",
		"options": [
			{"id":"a","short":"migrate","label":"move the kubeflow workload","artifact_type":"plan"},
			{"id":"b","short":"wait","label":"hold for owner","artifact_type":"report"}
		]
	}`
	id := createDecision(t, "how should we handle the node", fields)

	t.Run("fields_indexed_after_040_trigger", func(t *testing.T) {
		// CreateBead ran under the 040 trigger, so the vector already covers the
		// fields text: an option-only and a context-only term both match.
		if !matches(t, "kubeflow", id) {
			t.Errorf("option-label term 'kubeflow' should match via FTS after migration 040")
		}
		if !matches(t, "quarantine", id) {
			t.Errorf("context term 'quarantine' should match via FTS after migration 040")
		}
	})

	t.Run("reindex_backfill_refreshes_pre040_vector", func(t *testing.T) {
		// Simulate a decision written BEFORE 040: overwrite search_tsv with the
		// 039-form vector (title + description only), so the fields terms are
		// absent. A bare UPDATE of search_tsv does not re-fire the trigger (it
		// fires on title/description/fields changes, not on search_tsv), so the
		// stale value sticks.
		if _, err := store.db.ExecContext(ctx, `
			UPDATE beads SET search_tsv =
				setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
				setweight(to_tsvector('english', COALESCE(description, '')), 'B')
			WHERE id = $1`, id); err != nil {
			t.Fatalf("force 039-form vector: %v", err)
		}
		// Pre-reindex: the fields terms no longer match (the whole point).
		if matches(t, "kubeflow", id) {
			t.Fatalf("precondition failed: 'kubeflow' still matched with the forced 039-form vector")
		}

		// Run the reindex backfill to completion.
		afterID := ""
		for {
			st, err := store.BackfillSearchTSVPage(ctx, afterID, 500, true)
			if err != nil {
				t.Fatalf("BackfillSearchTSVPage(reindex, after=%q): %v", afterID, err)
			}
			if st.Done {
				break
			}
			if st.LastID == "" {
				t.Fatalf("non-final reindex page returned no cursor")
			}
			afterID = st.LastID
		}

		// Post-reindex: the fields terms match again.
		if !matches(t, "kubeflow", id) {
			t.Errorf("option-label term 'kubeflow' should match after reindex backfill")
		}
		if !matches(t, "quarantine", id) {
			t.Errorf("context term 'quarantine' should match after reindex backfill")
		}
	})
}

// TestSearchCommentBodyRecall_RealPostgres exercises the ka-0000008243 fix:
// `kd search` (queryListBeads filter.Search) now covers COMMENT bodies via the
// per-term EXISTS recall branch. Before the fix, the FTS vector
// (beads.search_tsv) and the title/description ILIKE floor both omitted
// comments entirely — comments live in their own table and the search_tsv
// trigger only fires on title/description/fields — so a reference that appears
// ONLY in a comment was invisible to search. This burned the §5.0 pre-mutation
// gate's leg 2: a decision that names a bead only in a comment read CLEAN.
//
// Gated by `//go:build integration` + BEADS_TEST_DATABASE_URL like the other
// real-Postgres tests in this package.
func TestSearchCommentBodyRecall_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-search-comment-" + suffix

	// beads.operated_by_id and comments.operated_by_id are both NOT NULL
	// (migration 022); resolve a real operator FK from the DB rather than
	// hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}

	when := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC)

	// A bead whose title/description/fields NEVER contain the probe token — the
	// only place it appears is a comment body. This is the exact shape from the
	// bug: an agent adds evidence to an open decision as a comment rather than
	// editing fields.context.
	b := &model.Bead{
		ID: genID(), Kind: model.KindIssue, Type: model.TypeDecision,
		Title: "Approve the rollout plan", Description: "Deliberation in progress.",
		Status: model.StatusOpen, CreatedAt: when, UpdatedAt: when, Fleet: fleet, OperatedByID: &operatorID,
	}
	if err := store.CreateBead(ctx, b); err != nil {
		t.Fatalf("create bead: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	// A token guaranteed unique to this run so recall is unambiguous.
	token := "commentonlyprobe" + suffix
	if err := store.AddComment(ctx, &model.Comment{
		BeadID: b.ID, Author: "tester",
		Text: "Root cause: the " + token + " reference lives only here.", OperatedByID: &operatorID,
	}); err != nil {
		t.Fatalf("add comment: %v", err)
	}

	searchIDs := func(t *testing.T, query string) []string {
		t.Helper()
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{
			Fleet: fleet, Search: query, Limit: 100,
		})
		if err != nil {
			t.Fatalf("ListBeads(search=%q): %v", query, err)
		}
		ids := make([]string, len(beads))
		for i, bd := range beads {
			ids[i] = bd.ID
		}
		return ids
	}
	contains := func(ids []string, id string) bool {
		for _, v := range ids {
			if v == id {
				return true
			}
		}
		return false
	}

	// The core assertion: a single-term query whose only match is a comment body
	// now returns the bead. Pre-fix this returned nothing (the reported MISS).
	t.Run("comment_only_reference_is_found", func(t *testing.T) {
		ids := searchIDs(t, token)
		if !contains(ids, b.ID) {
			t.Errorf("bead %s (token only in a comment) not returned for search %q; got %v",
				b.ID, token, ids)
		}
	})

	// Multi-term AND recall floor: each term may be satisfied by title OR
	// description OR a comment. Here one term is in the title ("rollout") and the
	// other only in a comment (the token), so the ANDed floor must still match.
	t.Run("multi_term_split_across_title_and_comment", func(t *testing.T) {
		ids := searchIDs(t, "rollout "+token)
		if !contains(ids, b.ID) {
			t.Errorf("bead %s not returned for split multi-term search 'rollout %s'; got %v",
				b.ID, token, ids)
		}
	})

	// A token that appears NOWHERE (not title/description/fields/comment) must
	// NOT match — the comment branch widens recall for real matches only, it does
	// not spuriously return every bead.
	t.Run("absent_token_does_not_match", func(t *testing.T) {
		ids := searchIDs(t, "absentzzz"+suffix)
		if contains(ids, b.ID) {
			t.Errorf("bead %s should NOT match a token that appears nowhere; got %v", b.ID, ids)
		}
	})
}
