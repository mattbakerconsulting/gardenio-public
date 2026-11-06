//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
)

// TestSearchRelevanceEphemeralDownRank_RealPostgres exercises the ka-0000000838
// ephemeral-type down-rank in the LEXICAL search path against a real Postgres
// backend.
//
// Symptom (reproduced live): an agent's pre-file dedup search returns a top-N
// dominated by CLOSED ephemeral type=message / type=agent gardener-lifecycle
// beads whose "Gardener-*" titles lexically match the query, burying the OPEN
// issue-like bead the agent is actually hunting.
//
// Two behaviors are pinned:
//
//  1. DOWN-RANK (the fix). At EQUAL lexical tier — both matching the query as a
//     title SUBSTRING (tier 2) — an OPEN issue bead outranks a CLOSED
//     type=message bead. The -0.5 ephemeral penalty (which also suppresses the
//     openness boost the message bead would otherwise NOT get anyway, since it
//     is closed) is the sole differentiator; without it the closed message
//     bead's created_at-DESC recency tiebreak could win.
//
//  2. NO CROSS-TIER JUMP (Symptom-B guarantee preserved). The penalty only
//     SUBTRACTS, so a message bead whose TITLE-PREFIX matches (tier 3) is
//     knocked from 3.x down to ~2.5+ — still comfortably above a genuine
//     description-only (tier 0) open bug. The penalty is a within-tier
//     tiebreaker, not a wholesale tier inversion: it never buries a
//     strong-title-match ephemeral bead beneath a merely-description-match
//     issue bead a full tier lower. (This guards against over-penalizing.)
//
// Gated identically to the Option-1/2/3 tests (//go:build integration +
// BEADS_TEST_DATABASE_URL).
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestSearchRelevanceEphemeralDownRank_RealPostgres
func TestSearchRelevanceEphemeralDownRank_RealPostgres(t *testing.T) {
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
	fleet := "test-search-rel-ephemeral-" + suffix

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

	createBead := func(t *testing.T, kind model.Kind, typ model.BeadType, status model.Status, title, description string, createdAt time.Time) string {
		t.Helper()
		b := &model.Bead{
			ID: genID(), Kind: kind, Type: typ,
			Title: title, Description: description, Status: status,
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

	// Query: "escalation". Both of the first two fixtures carry it as a
	// non-leading TITLE substring → tier 2, so they tie on the integer tier and
	// the ephemeral penalty is the sole differentiator.
	//
	// openIssue: kind=issue task, OPEN. Title-substring match, tier 2. Made the
	// OLDER row so that, absent the penalty, created_at DESC would sort it BELOW
	// the newer closed message bead — isolating the penalty as the cause of the
	// intended order. Score = 2 + ts_rank + 0.1 (open boost).
	openIssue := createBead(t, model.KindIssue, model.TypeTask, model.StatusOpen,
		"Handle escalation reassignment cleanly",
		"Wire the escalation into the operator queue.", older)

	// closedMessage: kind=data type=message, CLOSED — the ephemeral gardener-
	// lifecycle shape. Title-substring match, tier 2 (SAME lexical tier as
	// openIssue). Made the NEWER row so created_at DESC would otherwise put it
	// ABOVE openIssue. Score = 2 + ts_rank - 0.5 (ephemeral penalty).
	closedMessage := createBead(t, model.KindData, model.BeadType("message"), model.StatusClosed,
		"Gardener wakes while blocked on escalation",
		"Gardener-lifecycle heartbeat while an escalation is pending.", newer)

	// titlePrefixMessage: type=message whose TITLE STARTS WITH the query term →
	// tier 3. Even after the -0.5 penalty its score (~2.5+) stays above a
	// description-only (tier 0) open bug — the penalty must NOT invert tiers.
	titlePrefixMessage := createBead(t, model.KindData, model.BeadType("message"), model.StatusClosed,
		"Escalation queue heartbeat from gardener",
		"Gardener-lifecycle bead.", newer)

	// descOnlyOpenBug: kind=issue bug, OPEN, matches the query ONLY in its
	// DESCRIPTION → tier 0. It must stay BELOW the tier-3 titlePrefixMessage even
	// though that message bead is penalized (no cross-tier inversion).
	descOnlyOpenBug := createBead(t, model.KindIssue, model.TypeBug, model.StatusOpen,
		"Investigate operator queue latency",
		"The escalation never surfaced to the operator.", newer)

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

	// 1. DOWN-RANK: at equal lexical tier (both title-substring, tier 2), the
	//    OPEN issue bead outranks the CLOSED ephemeral message bead.
	t.Run("open_issue_outranks_closed_message_within_tier", func(t *testing.T) {
		ids, scores := searchResults(t, "escalation")
		oi, mi := indexOf(ids, openIssue), indexOf(ids, closedMessage)
		if oi < 0 {
			t.Fatalf("open issue %s not returned for 'escalation'; got %v", openIssue, ids)
		}
		if mi < 0 {
			t.Fatalf("closed message %s not returned for 'escalation'; got %v", closedMessage, ids)
		}
		if oi >= mi {
			t.Errorf("expected open issue %s (pos %d, score %.6f) to outrank same-tier "+
				"closed ephemeral message %s (pos %d, score %.6f); got %v",
				openIssue, oi, scores[openIssue], closedMessage, mi, scores[closedMessage], ids)
		}
		if scores[openIssue] <= scores[closedMessage] {
			t.Errorf("ephemeral penalty did not sink the message bead: open issue %.6f <= closed message %.6f",
				scores[openIssue], scores[closedMessage])
		}
	})

	// 2. NO CROSS-TIER JUMP: a tier-3 (title-prefix) ephemeral message bead,
	//    even after the -0.5 penalty, still outranks a tier-0 description-only
	//    open bug. The penalty is a within-tier tiebreaker, not a tier inversion.
	t.Run("penalized_title_prefix_message_still_outranks_desc_only_bug", func(t *testing.T) {
		ids, scores := searchResults(t, "escalation")
		ti, di := indexOf(ids, titlePrefixMessage), indexOf(ids, descOnlyOpenBug)
		if ti < 0 {
			t.Fatalf("title-prefix message %s not returned for 'escalation'; got %v", titlePrefixMessage, ids)
		}
		if di < 0 {
			t.Fatalf("desc-only open bug %s not returned for 'escalation'; got %v", descOnlyOpenBug, ids)
		}
		if ti >= di {
			t.Errorf("ephemeral penalty must NOT cross a tier boundary: expected penalized "+
				"tier-3 title-prefix message %s (pos %d, score %.6f) to still outrank tier-0 "+
				"description-only open bug %s (pos %d, score %.6f); got %v",
				titlePrefixMessage, ti, scores[titlePrefixMessage], descOnlyOpenBug, di, scores[descOnlyOpenBug], ids)
		}
	})
}

// TestSemanticSearchEphemeralDownRank_RealPostgres exercises the ka-0000000838
// ephemeral-type down-rank in the SEMANTIC (pgvector) search path against a
// real Postgres backend.
//
// Symptom (reproduced live): `kd search '<gardener-flavored query>' --semantic`
// returned a top-N of ALL closed type=message / type=agent gardener-lifecycle
// beads (scores 0.35-0.46) with ZERO open issue-like beads, because the ORDER
// BY was bare cosine distance with no type/status weighting.
//
// The fix adds an additive DISTANCE penalty in the ORDER BY (+0.15 for
// ephemeral message/agent, +0.05 for closed) while keeping the DISPLAYED score
// pure cosine similarity. This test pins two behaviors:
//
//  1. DOWN-RANK: at EQUAL vector proximity to the query, an OPEN issue bead
//     sorts ABOVE a CLOSED type=message bead. Without the penalty the two tie on
//     distance and order is undefined; with it the +0.15 ephemeral penalty
//     sinks the message bead deterministically.
//
//  2. SCORE NOT DISTORTED: the reported Score/Distance remain pure cosine —
//     the equal-proximity fixtures still report equal Score even though they
//     sort differently (the penalty lives only in ORDER BY, not the projection).
//
// Gated identically (//go:build integration + BEADS_TEST_DATABASE_URL). The
// bead_embeddings ivfflat index is approximate, but with a tiny fleet-scoped
// corpus every row is scanned, so nearest-neighbor order is exact here.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestSemanticSearchEphemeralDownRank_RealPostgres
func TestSemanticSearchEphemeralDownRank_RealPostgres(t *testing.T) {
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
	fleet := "test-semantic-ephemeral-" + suffix

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

	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	when := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// embDim mirrors migration 005's vector(1024). unitVec builds a 1024-dim
	// vector that is `val` in slot `slot` and 0 elsewhere; two such vectors in
	// distinct slots are orthogonal (cosine distance 1), and a vector equal to
	// one of them has cosine distance 0. We give the two ephemeral-vs-issue
	// fixtures the SAME embedding so they are EQUIDISTANT from the query — the
	// ephemeral penalty is then the only thing that can separate them.
	const embDim = 1024
	unitVec := func(slot int, val float32) []float32 {
		v := make([]float32, embDim)
		v[slot] = val
		return v
	}

	create := func(t *testing.T, kind model.Kind, typ model.BeadType, status model.Status, title string, emb []float32) string {
		t.Helper()
		b := &model.Bead{
			ID: genID(), Kind: kind, Type: typ,
			Title: title, Status: status,
			CreatedAt: when, UpdatedAt: when, Fleet: fleet, OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, b); err != nil {
			t.Fatalf("create bead %q: %v", title, err)
		}
		if err := store.UpsertEmbedding(ctx, b.ID, emb, "test-model", "hash-"+b.ID); err != nil {
			t.Fatalf("upsert embedding for %q: %v", title, err)
		}
		return b.ID
	}

	// The query vector points along slot 0. Both the open issue and the closed
	// message carry the IDENTICAL slot-0 embedding, so both are at cosine
	// distance 0 from the query — perfectly equidistant. A third far bead along
	// an orthogonal slot confirms the penalty never lifts an unrelated bead.
	queryVec := unitVec(0, 1)
	sharedEmb := unitVec(0, 1)

	openIssue := create(t, model.KindIssue, model.TypeTask, model.StatusOpen,
		"Gardener resumes after escalation resolves", sharedEmb)
	closedMessage := create(t, model.KindData, model.BeadType("message"), model.StatusClosed,
		"Gardener wakes while blocked on escalation", sharedEmb)
	farBead := create(t, model.KindIssue, model.TypeTask, model.StatusOpen,
		"Unrelated billing reconciliation", unitVec(1, 1))

	results, err := store.SemanticSearch(ctx, queryVec, model.BeadFilter{Fleet: fleet}, 100)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}

	pos := map[string]int{}
	score := map[string]float64{}
	for i, r := range results {
		pos[r.Bead.ID] = i
		score[r.Bead.ID] = r.Score
	}
	inResults := func(id string) bool { _, ok := pos[id]; return ok }

	// 1. DOWN-RANK: equal proximity, open issue sorts above closed message.
	if !inResults(openIssue) {
		t.Fatalf("open issue %s missing from semantic results; got %v", openIssue, results)
	}
	if !inResults(closedMessage) {
		t.Fatalf("closed message %s missing from semantic results; got %v", closedMessage, results)
	}
	if pos[openIssue] >= pos[closedMessage] {
		t.Errorf("expected open issue %s (pos %d) to sort ABOVE equidistant closed ephemeral "+
			"message %s (pos %d) — ephemeral distance penalty should break the tie",
			openIssue, pos[openIssue], closedMessage, pos[closedMessage])
	}

	// 2. SCORE NOT DISTORTED: the reported cosine similarity is unchanged by the
	//    ordering penalty, so the two equidistant beads report EQUAL Score.
	if d := score[openIssue] - score[closedMessage]; d < -1e-6 || d > 1e-6 {
		t.Errorf("displayed Score must stay pure cosine (penalty lives only in ORDER BY): "+
			"open issue Score %.6f != closed message Score %.6f", score[openIssue], score[closedMessage])
	}
	// The equidistant beads are both ~1.0 similarity; the orthogonal far bead is
	// ~0.0 and must rank last regardless of the penalty.
	if inResults(farBead) && pos[farBead] < pos[openIssue] {
		t.Errorf("orthogonal far bead %s (pos %d) must not outrank the on-topic open issue %s (pos %d)",
			farBead, pos[farBead], openIssue, pos[openIssue])
	}
}
