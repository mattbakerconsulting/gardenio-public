package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// storeBackfillStatsCovOwnerBF returns a zeroed BackfillOwnerStats with an
// initialized RebindCount map, matching what queryBackfillOwner builds before
// calling applyOwnerRebinds.
func storeBackfillStatsCovOwnerBF() *store.BackfillOwnerStats {
	return &store.BackfillOwnerStats{RebindCount: map[string]int64{}}
}

// These tests exercise the zero-coverage pure helpers and the DB-backed
// resolver helpers in queries_owner_backfill.go. The pure helpers
// (slackUserIDFromFields, decodeFieldsOwner, isOwnerSentinelSpawner,
// isHumanShapedOperator) carry no DB dependency; the DB-backed helpers
// (lookupAgentBySlugOrID, humanFromUserBead, humanFromFieldsForOwner,
// walkSpawnChainForOwner, resolveOwnerForBead, applyOwnerRebinds) take a
// *sql.DB which we wire from newMockDB.

func TestCovOwnerBFSlackUserIDFromFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"canonical", map[string]any{"slack_user_id": "U123"}, "U123"},
		{"canonicalTrimmed", map[string]any{"slack_user_id": "  U777  "}, "U777"},
		{"legacySlackID", map[string]any{"slack_id": "U456"}, "U456"},
		{"canonicalPreferredOverLegacy", map[string]any{"slack_user_id": "U1", "slack_id": "U2"}, "U1"},
		{"emptyCanonicalFallsToLegacy", map[string]any{"slack_user_id": "  ", "slack_id": "U9"}, "U9"},
		{"none", map[string]any{"other": "x"}, ""},
		{"wrongType", map[string]any{"slack_user_id": 42}, ""},
		{"empty", map[string]any{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := slackUserIDFromFields(tc.fields); got != tc.want {
				t.Errorf("slackUserIDFromFields(%v) = %q, want %q", tc.fields, got, tc.want)
			}
		})
	}
}

func TestCovOwnerBFDecodeFieldsOwner(t *testing.T) {
	// Empty / nil raw -> empty (non-nil) map.
	if m := decodeFieldsOwner(nil); m == nil || len(m) != 0 {
		t.Errorf("decodeFieldsOwner(nil) = %v, want empty non-nil map", m)
	}
	if m := decodeFieldsOwner(json.RawMessage{}); m == nil || len(m) != 0 {
		t.Errorf("decodeFieldsOwner({}) = %v, want empty non-nil map", m)
	}
	// Valid JSON object decodes.
	m := decodeFieldsOwner(json.RawMessage(`{"slack_user_id":"U1","n":3}`))
	if m["slack_user_id"] != "U1" {
		t.Errorf("decodeFieldsOwner slack_user_id = %v, want U1", m["slack_user_id"])
	}
	// Malformed JSON swallows the error and returns the (empty) map.
	if m := decodeFieldsOwner(json.RawMessage(`{not json`)); m == nil {
		t.Errorf("decodeFieldsOwner(malformed) = nil, want non-nil map")
	}
}

func TestCovOwnerBFIsOwnerSentinelSpawner(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"scheduler:cron-1", true},
		{"trigger:bead-event", true},
		{"escalation-bridge", true},
		{"escalation-bridge:foo", true},
		{"system:unowned", true},
		{"fleet:production", true},
		{"  scheduler:x  ", true}, // trimmed before prefix check
		{"agent-slug", false},
		{"thread-abc", false},
		{"", false},
		{"oidc:matt@x.ai", false},
		{"slack:U123", false},
	} {
		if got := isOwnerSentinelSpawner(tc.in); got != tc.want {
			t.Errorf("isOwnerSentinelSpawner(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCovOwnerBFIsHumanShapedOperator(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", false},
		{"system:unowned", false}, // sentinel
		{"fleet:prod", false},     // sentinel
		{"scheduler:x", false},    // sentinel
		{"slack:U123", true},
		{"oidc:matt@x.ai", true},
		{"weird:value", true}, // any non-sentinel colon value is accepted
		{"barevalue", false},  // no colon -> ambiguous, treated non-human
	} {
		if got := isHumanShapedOperator(tc.in); got != tc.want {
			t.Errorf("isHumanShapedOperator(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// --- DB-backed helpers ---

func TestCovOwnerBFLookupAgentBySlugOrID_EmptySlug(t *testing.T) {
	db, _ := newMockDB(t)
	got, err := lookupAgentBySlugOrID(context.Background(), db, "   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for empty slug, got %+v", got)
	}
}

func TestCovOwnerBFLookupAgentBySlugOrID_Found(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(fields, '\{\}'::jsonb\)\s+FROM beads\s+WHERE type = 'agent'`).
		WithArgs("agent-x").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fields"}).
			AddRow("gd-agent1", []byte(`{"agent":"agent-x"}`)))

	got, err := lookupAgentBySlugOrID(context.Background(), db, "agent-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.id != "gd-agent1" {
		t.Fatalf("expected agent gd-agent1, got %+v", got)
	}
}

func TestCovOwnerBFLookupAgentBySlugOrID_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`WHERE type = 'agent'`).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)

	got, err := lookupAgentBySlugOrID(context.Background(), db, "missing")
	if err != nil {
		t.Fatalf("expected nil error on no rows, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil agent, got %+v", got)
	}
}

func TestCovOwnerBFLookupAgentBySlugOrID_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`WHERE type = 'agent'`).
		WithArgs("boom").
		WillReturnError(errors.New("boom"))

	if _, err := lookupAgentBySlugOrID(context.Background(), db, "boom"); err == nil {
		t.Fatal("expected non-nil error for query failure")
	}
}

func TestCovOwnerBFHumanFromUserBead_Email(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT COALESCE\(fields, '\{\}'::jsonb\)\s+FROM beads\s+WHERE id = \$1`).
		WithArgs("gd-user1").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).
			AddRow([]byte(`{"email":"alice@example.com"}`)))

	if got := humanFromUserBead(context.Background(), db, "gd-user1"); got != "oidc:alice@example.com" {
		t.Fatalf("got %q, want oidc:alice@example.com", got)
	}
}

func TestCovOwnerBFHumanFromUserBead_SlackFallback(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`WHERE id = \$1`).
		WithArgs("gd-user2").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).
			AddRow([]byte(`{"slack_user_id":"U321"}`)))

	if got := humanFromUserBead(context.Background(), db, "gd-user2"); got != "slack:U321" {
		t.Fatalf("got %q, want slack:U321", got)
	}
}

func TestCovOwnerBFHumanFromUserBead_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`WHERE id = \$1`).
		WithArgs("gd-missing").
		WillReturnError(sql.ErrNoRows)

	if got := humanFromUserBead(context.Background(), db, "gd-missing"); got != "" {
		t.Fatalf("got %q, want empty for missing user bead", got)
	}
}

func TestCovOwnerBFHumanFromUserBead_NoResolvableField(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`WHERE id = \$1`).
		WithArgs("gd-user3").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).
			AddRow([]byte(`{"other":"x"}`)))

	if got := humanFromUserBead(context.Background(), db, "gd-user3"); got != "" {
		t.Fatalf("got %q, want empty when no email/slack field", got)
	}
}

func TestCovOwnerBFHumanFromFieldsForOwner_Empty(t *testing.T) {
	db, _ := newMockDB(t)
	if got := humanFromFieldsForOwner(context.Background(), db, map[string]any{}); got != "" {
		t.Fatalf("got %q, want empty for empty fields", got)
	}
}

func TestCovOwnerBFHumanFromFieldsForOwner_UserBeadID(t *testing.T) {
	db, mock := newMockDB(t)
	// fields.user_bead_id is dereferenced via humanFromUserBead.
	mock.ExpectQuery(`WHERE id = \$1`).
		WithArgs("gd-ub1").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).
			AddRow([]byte(`{"email":"bob@example.com"}`)))

	got := humanFromFieldsForOwner(context.Background(), db, map[string]any{"user_bead_id": "gd-ub1"})
	if got != "oidc:bob@example.com" {
		t.Fatalf("got %q, want oidc:bob@example.com", got)
	}
}

func TestCovOwnerBFHumanFromFieldsForOwner_SlackUserID(t *testing.T) {
	db, _ := newMockDB(t)
	// No user_bead_id -> no DB call; slack_user_id resolves directly.
	got := humanFromFieldsForOwner(context.Background(), db, map[string]any{"slack_user_id": "U55"})
	if got != "slack:U55" {
		t.Fatalf("got %q, want slack:U55", got)
	}
}

func TestCovOwnerBFHumanFromFieldsForOwner_OperatedBy(t *testing.T) {
	db, _ := newMockDB(t)
	got := humanFromFieldsForOwner(context.Background(), db, map[string]any{"operated_by": "oidc:carol@example.com"})
	if got != "oidc:carol@example.com" {
		t.Fatalf("got %q, want oidc:carol@example.com", got)
	}
}

func TestCovOwnerBFHumanFromFieldsForOwner_OperatedBySentinelRejected(t *testing.T) {
	db, _ := newMockDB(t)
	got := humanFromFieldsForOwner(context.Background(), db, map[string]any{"operated_by": "system:unowned"})
	if got != "" {
		t.Fatalf("got %q, want empty (sentinel operated_by rejected)", got)
	}
}

func TestCovOwnerBFWalkSpawnChain_ThreadSlackUser(t *testing.T) {
	db, _ := newMockDB(t)
	b := ownerCandidateRow{
		id:        "gd-1",
		createdBy: "thread-abc",
		fields:    json.RawMessage(`{"slack_user_id":"U100"}`),
	}
	if got := walkSpawnChainForOwner(context.Background(), db, b); got != "slack:U100" {
		t.Fatalf("got %q, want slack:U100", got)
	}
}

func TestCovOwnerBFWalkSpawnChain_ThreadTriggerSubject(t *testing.T) {
	db, _ := newMockDB(t)
	b := ownerCandidateRow{
		id:        "gd-2",
		createdBy: "thread-xyz",
		fields:    json.RawMessage(`{"trigger_created_by_subject":"U200"}`),
	}
	if got := walkSpawnChainForOwner(context.Background(), db, b); got != "slack:U200" {
		t.Fatalf("got %q, want slack:U200", got)
	}
}

func TestCovOwnerBFWalkSpawnChain_ThreadNoUser(t *testing.T) {
	db, _ := newMockDB(t)
	b := ownerCandidateRow{
		id:        "gd-3",
		createdBy: "thread-empty",
		fields:    json.RawMessage(`{}`),
	}
	if got := walkSpawnChainForOwner(context.Background(), db, b); got != "" {
		t.Fatalf("got %q, want empty for thread agent with no user", got)
	}
}

func TestCovOwnerBFWalkSpawnChain_SentinelCreatorOwnFields(t *testing.T) {
	db, _ := newMockDB(t)
	// scheduler: creator falls through to bead's own fields (no DB call
	// because slack_user_id resolves directly without user_bead_id).
	b := ownerCandidateRow{
		id:        "gd-4",
		createdBy: "scheduler:cron",
		fields:    json.RawMessage(`{"slack_user_id":"U400"}`),
	}
	if got := walkSpawnChainForOwner(context.Background(), db, b); got != "slack:U400" {
		t.Fatalf("got %q, want slack:U400", got)
	}
}

func TestCovOwnerBFWalkSpawnChain_AgentSlugRootResolves(t *testing.T) {
	db, mock := newMockDB(t)
	// created_by is an agent slug; the agent bead has spawned_by=sentinel so
	// the walk stops at this agent, then resolves the human off its fields.
	mock.ExpectQuery(`WHERE type = 'agent'`).
		WithArgs("agent-root").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fields"}).
			AddRow("gd-agentR", []byte(`{"spawned_by":"scheduler:cron","slack_user_id":"U500"}`)))

	b := ownerCandidateRow{
		id:        "gd-5",
		createdBy: "agent-root",
		fields:    json.RawMessage(`{}`),
	}
	if got := walkSpawnChainForOwner(context.Background(), db, b); got != "slack:U500" {
		t.Fatalf("got %q, want slack:U500", got)
	}
}

func TestCovOwnerBFWalkSpawnChain_AgentNotFoundFallsToOwnFields(t *testing.T) {
	db, mock := newMockDB(t)
	// Agent lookup returns no rows -> rootFields stays nil -> fall back to
	// the candidate bead's own fields.
	mock.ExpectQuery(`WHERE type = 'agent'`).
		WithArgs("ghost-agent").
		WillReturnError(sql.ErrNoRows)

	b := ownerCandidateRow{
		id:        "gd-6",
		createdBy: "ghost-agent",
		fields:    json.RawMessage(`{"slack_user_id":"U600"}`),
	}
	if got := walkSpawnChainForOwner(context.Background(), db, b); got != "slack:U600" {
		t.Fatalf("got %q, want slack:U600", got)
	}
}

func TestCovOwnerBFResolveOwnerForBead_Kept(t *testing.T) {
	db, _ := newMockDB(t)
	// Thread agent resolving to the target -> ownerDecisionKept.
	b := ownerCandidateRow{
		id:        "gd-7",
		createdBy: "thread-1",
		fields:    json.RawMessage(`{"slack_user_id":"U0OPERATOR1"}`),
	}
	owner, kind := resolveOwnerForBead(context.Background(), db, b, "slack:U0OPERATOR1", "system:unowned")
	if owner != "slack:U0OPERATOR1" || kind != ownerDecisionKept {
		t.Fatalf("got owner=%q kind=%v, want target/Kept", owner, kind)
	}
}

func TestCovOwnerBFResolveOwnerForBead_Rebound(t *testing.T) {
	db, _ := newMockDB(t)
	// Resolves to a different human -> ownerDecisionRebound.
	b := ownerCandidateRow{
		id:        "gd-8",
		createdBy: "thread-2",
		fields:    json.RawMessage(`{"slack_user_id":"UOTHER"}`),
	}
	owner, kind := resolveOwnerForBead(context.Background(), db, b, "slack:U0OPERATOR1", "system:unowned")
	if owner != "slack:UOTHER" || kind != ownerDecisionRebound {
		t.Fatalf("got owner=%q kind=%v, want slack:UOTHER/Rebound", owner, kind)
	}
}

func TestCovOwnerBFResolveOwnerForBead_Unresolved(t *testing.T) {
	db, _ := newMockDB(t)
	// Thread agent with no resolvable user -> "" -> ownerDecisionUnresolved
	// with the sentinel.
	b := ownerCandidateRow{
		id:        "gd-9",
		createdBy: "thread-3",
		fields:    json.RawMessage(`{}`),
	}
	owner, kind := resolveOwnerForBead(context.Background(), db, b, "slack:U0OPERATOR1", "system:unowned")
	if owner != "system:unowned" || kind != ownerDecisionUnresolved {
		t.Fatalf("got owner=%q kind=%v, want system:unowned/Unresolved", owner, kind)
	}
}

func TestCovOwnerBFApplyOwnerRebinds_NoGroups(t *testing.T) {
	db, _ := newMockDB(t)
	// Only Kept decisions -> no groups -> no tx, returns nil. KeptMattCount
	// is incremented.
	stats := storeBackfillStatsCovOwnerBF()
	err := applyOwnerRebinds(context.Background(), db, "slack:U0OPERATOR1",
		[]ownerRebindDecision{{id: "gd-a", kind: ownerDecisionKept}}, stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.KeptMattCount != 1 {
		t.Fatalf("KeptMattCount = %d, want 1", stats.KeptMattCount)
	}
}

func TestCovOwnerBFApplyOwnerRebinds_ReboundCommits(t *testing.T) {
	db, mock := newMockDB(t)
	stats := storeBackfillStatsCovOwnerBF()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE beads\s+SET owner = \$1\s+WHERE id = ANY\(\$2::text\[\]\)\s+AND owner = \$3`).
		WithArgs("slack:UNEW", sqlmock.AnyArg(), "slack:U0OPERATOR1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-b", "kd admin backfill-owner", "owner", "slack:U0OPERATOR1", "slack:UNEW", "backfill-owner-matt-default").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := applyOwnerRebinds(context.Background(), db, "slack:U0OPERATOR1",
		[]ownerRebindDecision{{id: "gd-b", newOwner: "slack:UNEW", kind: ownerDecisionRebound}}, stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.RebindCount["slack:UNEW"] != 1 {
		t.Fatalf("RebindCount[slack:UNEW] = %d, want 1", stats.RebindCount["slack:UNEW"])
	}
}

func TestCovOwnerBFApplyOwnerRebinds_UnresolvedZeroRowsSkipsHistory(t *testing.T) {
	db, mock := newMockDB(t)
	stats := storeBackfillStatsCovOwnerBF()

	mock.ExpectBegin()
	// 0 rows affected -> the per-id mutation_history INSERT loop is skipped.
	mock.ExpectExec(`UPDATE beads`).
		WithArgs("system:unowned", sqlmock.AnyArg(), "slack:U0OPERATOR1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	err := applyOwnerRebinds(context.Background(), db, "slack:U0OPERATOR1",
		[]ownerRebindDecision{{id: "gd-c", newOwner: "system:unowned", kind: ownerDecisionUnresolved}}, stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.UnresolvedCount != 1 {
		t.Fatalf("UnresolvedCount = %d, want 1", stats.UnresolvedCount)
	}
	if stats.RebindCount["system:unowned"] != 0 {
		t.Fatalf("RebindCount[system:unowned] = %d, want 0 (no rows affected)", stats.RebindCount["system:unowned"])
	}
}

func TestCovOwnerBFApplyOwnerRebinds_UpdateError(t *testing.T) {
	db, mock := newMockDB(t)
	stats := storeBackfillStatsCovOwnerBF()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE beads`).
		WithArgs("slack:UNEW", sqlmock.AnyArg(), "slack:U0OPERATOR1").
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	err := applyOwnerRebinds(context.Background(), db, "slack:U0OPERATOR1",
		[]ownerRebindDecision{{id: "gd-d", newOwner: "slack:UNEW", kind: ownerDecisionRebound}}, stats)
	if err == nil {
		t.Fatal("expected non-nil error for UPDATE failure")
	}
}
