//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
)

// TestFieldsContextMentions_RealPostgres pins the CLAIM of the ka-0000001507
// fix against a real Postgres: a decision-shaped bead that names a gd-id ONLY
// in fields.context (empty description, no comment) must be discoverable by
// ListBeadsMentioning — the query behind `kd list --mentions <id>`, which is
// the §5.0 pre-mutation decision gate. Before the fix the gate saw ZERO rows
// for such a bead (it indexed only title/description/notes/comments), so it
// reported "not parked" and authorized a mutation the operator still owned.
//
// This is the exact shape `gb decision create` writes: the operator-facing
// body lives in fields.context while description stays empty (measured
// 2026-08-21: 73/73 open gardenio decisions had an empty description, 71/73
// carried their body in fields.context).
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the default
// `go test ./...` workflow stays sqlmock-only per kbeads/CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestFieldsContextMentions_RealPostgres
func TestFieldsContextMentions_RealPostgres(t *testing.T) {
	dsn := requireTestDSN(t)

	st, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	fleet := "test-fields-refs-" + genTestID(t)

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	// The parked bead the decision is about. Its id lives ONLY in the
	// decision's fields.context — never in title/description/notes/comments.
	parkedID := genTestID(t)

	// A decision-shaped bead: empty description, body in fields.context, and
	// (to prove the whole-blob extraction) a second id inside fields.options.
	// idgen.Generate already returns gd-prefixed ids, so reference them verbatim.
	parkedRef := parkedID
	optionsRef := genTestID(t)
	fields, err := json.Marshal(map[string]any{
		"prompt":  "which policy wins?",
		"context": "parked behind this decision: " + parkedRef + " (operator still owns disposition)",
		"options": []map[string]string{
			{"id": "a", "label": "bless " + optionsRef},
		},
	})
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}

	decisionID := genTestID(t)
	decision := &model.Bead{
		ID:           decisionID,
		Kind:         model.KindIssue,
		Type:         "decision",
		Title:        "decision: contradictory policies", // no gd-id here
		Description:  "",                                 // gb decision create leaves this EMPTY
		Status:       model.StatusOpen,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		Fleet:        fleet,
		Fields:       fields,
		OperatedByID: &operatorID,
	}
	if err := st.CreateBead(ctx, decision); err != nil {
		t.Fatalf("create decision: %v", err)
	}

	// CLAIM 1: the gate query (untyped mentions) now sees the decision for the
	// id that lived only in fields.context.
	untyped := ""
	rows, total, err := st.ListBeadsMentioning(ctx, parkedRef, 50, 0, &untyped)
	if err != nil {
		t.Fatalf("ListBeadsMentioning(context id): %v", err)
	}
	if total == 0 {
		t.Fatalf("gate FAILED OPEN: no bead mentions %s, but decision %s names it in fields.context", parkedRef, decisionID)
	}
	if !containsBead(rows, decisionID) {
		t.Fatalf("expected decision %s in mentions of %s; got %v", decisionID, parkedRef, summarize(rows))
	}
	// Provenance: the edge came from the new "fields" bucket.
	if sf := sourceFieldFor(rows, decisionID); sf != "fields" {
		t.Fatalf("expected source_field=fields for %s, got %q", decisionID, sf)
	}

	// CLAIM 2: whole-blob extraction also captures ids nested in fields.options
	// (the residual case the two-leg CLI workaround missed).
	rows2, total2, err := st.ListBeadsMentioning(ctx, optionsRef, 50, 0, &untyped)
	if err != nil {
		t.Fatalf("ListBeadsMentioning(options id): %v", err)
	}
	if total2 == 0 || !containsBead(rows2, decisionID) {
		t.Fatalf("expected decision %s to mention options id %s; got %v", decisionID, optionsRef, summarize(rows2))
	}

	// CLAIM 3: the fields bucket is untyped-only — no typed edge, no dep is
	// minted from arbitrary JSON blob content. A `blocks`/`parent` verb inside
	// the JSON must NOT promote to the deps graph.
	parent := "parent"
	typedRows, typedTotal, err := st.ListBeadsMentioning(ctx, parkedRef, 50, 0, &parent)
	if err != nil {
		t.Fatalf("ListBeadsMentioning(typed=parent): %v", err)
	}
	if typedTotal != 0 || len(typedRows) != 0 {
		t.Fatalf("fields bucket leaked a typed edge: got %d typed=parent rows for %s", typedTotal, parkedRef)
	}
}

// requireTestDSN skips the test unless BEADS_TEST_DATABASE_URL is set.
func requireTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}
	return dsn
}

// anyOperatorID returns any operators(id) so the NOT-NULL beads.operated_by_id
// FK can be satisfied without hardcoding an instance-specific id.
func anyOperatorID(t *testing.T, dsn string) int64 {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open for operator lookup: %v", err)
	}
	defer db.Close()
	var id int64
	if err := db.QueryRow(`SELECT id FROM operators ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("resolve operator id: %v", err)
	}
	return id
}

func genTestID(t *testing.T) string {
	t.Helper()
	id, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	return id
}

func containsBead(rows []*model.BeadReferenceSummary, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

func sourceFieldFor(rows []*model.BeadReferenceSummary, id string) string {
	for _, r := range rows {
		if r.ID == id {
			return r.SourceField
		}
	}
	return ""
}

func summarize(rows []*model.BeadReferenceSummary) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID+"["+r.SourceField+"]")
	}
	return out
}
