package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// ka-0000004855 / gd-IIVuBeq9wg: `kd children --json` row schema is aligned with
// `kd list --json` — rows carry `id` (== bead_id), the BEAD `type`, `dep_type`
// for the edge, plus status/priority/assignee/title from the fetched bead. These
// tests lock the enriched schema so it can never silently regress to the
// bead_id-only shape that made gardeners reverse-engineer it by trial and error.

// unmarshalChildrenJSON runs `kd children <epic> --json` against the mock and
// returns the decoded rows as generic maps so a test can assert on exact key
// presence/absence (not just Go struct field values).
func unmarshalChildrenJSON(t *testing.T, mc *mockBeadsClient, epic string) []map[string]any {
	t.Helper()
	withMockClient(t, mc)
	jsonOutput = true
	defer func() { jsonOutput = false }()

	out := captureStdout(t, func() {
		if err := childrenCmd.RunE(childrenCmd, []string{epic}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal children JSON: %v\noutput:\n%s", err, out)
	}
	return rows
}

func TestChildrenJSON_EnrichedSchema_AlignsWithList(t *testing.T) {
	mc := newMockClient()
	epic := "gd-s8fe2jsggl"
	child := "gd-mgbvv7yjt7"
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: child, DependsOnID: epic, Type: model.DepParentChild},
	}
	mc.Beads[child] = &model.Bead{
		ID:       child,
		Type:     model.TypeTask,
		Status:   model.StatusInProgress,
		Priority: 2,
		Assignee: "alice",
		Title:    "a real child task",
	}

	rows := unmarshalChildrenJSON(t, mc, epic)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]

	// id must be present AND equal bead_id (the whole point of the alignment).
	if r["id"] != child {
		t.Errorf("id: want %q, got %v", child, r["id"])
	}
	if r["id"] != r["bead_id"] {
		t.Errorf("id (%v) must equal bead_id (%v)", r["id"], r["bead_id"])
	}
	// type is the BEAD type (like kd list), dep_type is the edge type.
	if r["type"] != string(model.TypeTask) {
		t.Errorf("type: want bead type %q, got %v", model.TypeTask, r["type"])
	}
	if r["dep_type"] != string(model.DepParentChild) {
		t.Errorf("dep_type: want %q, got %v", model.DepParentChild, r["dep_type"])
	}
	if r["status"] != string(model.StatusInProgress) {
		t.Errorf("status: want %q, got %v", model.StatusInProgress, r["status"])
	}
	// JSON numbers decode to float64.
	if p, ok := r["priority"].(float64); !ok || int(p) != 2 {
		t.Errorf("priority: want 2, got %v (%T)", r["priority"], r["priority"])
	}
	if r["assignee"] != "alice" {
		t.Errorf("assignee: want alice, got %v", r["assignee"])
	}
	if r["title"] != "a real child task" {
		t.Errorf("title: want the child title, got %v", r["title"])
	}
}

// A P0 child (priority 0) MUST still serialize its priority. Using a plain int
// with omitempty would drop it — reintroducing the "PNone" trap from the bug.
func TestChildrenJSON_P0Priority_IsEmitted(t *testing.T) {
	mc := newMockClient()
	epic := "gd-epicp0001"
	child := "gd-ov2jooliky"
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: child, DependsOnID: epic, Type: model.DepParentChild},
	}
	mc.Beads[child] = &model.Bead{
		ID:       child,
		Type:     model.TypeBug,
		Status:   model.StatusOpen,
		Priority: 0, // P0
		Title:    "critical bug",
	}

	rows := unmarshalChildrenJSON(t, mc, epic)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	p, ok := rows[0]["priority"]
	if !ok {
		t.Fatalf("priority key missing for a real P0 bead — the PNone trap is back")
	}
	if f, ok := p.(float64); !ok || int(f) != 0 {
		t.Errorf("priority: want 0 (P0), got %v (%T)", p, p)
	}
}

// A row whose bead fetch failed keeps id/bead_id/dep_type but omits the
// bead-derived fields (including priority — the *int stays nil).
func TestChildrenJSON_NilBead_OmitsBeadFields(t *testing.T) {
	mc := newMockClient()
	epic := "gd-jesoffdnnd"
	child := "gd-z7hk30alxp"
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: child, DependsOnID: epic, Type: model.DepBlocks},
	}
	// No entry in mc.Beads → GetBead errors → rd.Bead stays nil.

	rows := unmarshalChildrenJSON(t, mc, epic)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r["id"] != child || r["bead_id"] != child {
		t.Errorf("id/bead_id must survive a fetch error; got id=%v bead_id=%v", r["id"], r["bead_id"])
	}
	if r["dep_type"] != string(model.DepBlocks) {
		t.Errorf("dep_type must survive a fetch error; got %v", r["dep_type"])
	}
	for _, k := range []string{"type", "status", "priority", "assignee", "title", "updated_at", "created_at"} {
		if _, present := r[k]; present {
			t.Errorf("bead-derived key %q must be omitted for a nil-bead row; got %v", k, r[k])
		}
	}
}

// ka-0000004797: a successfully-resolved row MUST carry non-null updated_at and
// created_at, so a caller can time-filter children rows the same way it filters
// `kd list` rows. The pre-fix bug omitted both fields entirely, so jq saw `null`
// for a key that was never in the payload — every `select(.updated_at > "<date>")`
// silently returned zero rows, indistinguishable from a genuinely stale lane.
func TestChildrenJSON_ResolvedRow_CarriesTimestamps(t *testing.T) {
	mc := newMockClient()
	epic := "gd-sabcwftogc"
	child := "gd-t9tjp9yxwo"
	updated := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: child, DependsOnID: epic, Type: model.DepParentChild},
	}
	mc.Beads[child] = &model.Bead{
		ID:        child,
		Type:      model.TypeTask,
		Status:    model.StatusInProgress,
		Priority:  2,
		Title:     "recently updated child",
		UpdatedAt: updated,
		CreatedAt: created,
	}

	rows := unmarshalChildrenJSON(t, mc, epic)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]

	// Both keys must be PRESENT (the pre-fix bug omitted them → jq saw null).
	rawU, okU := r["updated_at"]
	if !okU {
		t.Fatalf("updated_at key missing — the staleness-filter trap is back")
	}
	rawC, okC := r["created_at"]
	if !okC {
		t.Fatalf("created_at key missing — the staleness-filter trap is back")
	}
	// And non-null, parseable RFC3339 equal to the source bead's times. A null
	// or zero time would silently break `select(.updated_at > "<date>")`.
	uStr, ok := rawU.(string)
	if !ok || uStr == "" {
		t.Fatalf("updated_at must be a non-empty RFC3339 string, got %v (%T)", rawU, rawU)
	}
	gotU, err := time.Parse(time.RFC3339, uStr)
	if err != nil {
		t.Fatalf("updated_at not RFC3339: %q (%v)", uStr, err)
	}
	if !gotU.Equal(updated) {
		t.Errorf("updated_at: want %s, got %s", updated, gotU)
	}
	cStr, ok := rawC.(string)
	if !ok || cStr == "" {
		t.Fatalf("created_at must be a non-empty RFC3339 string, got %v (%T)", rawC, rawC)
	}
	gotC, err := time.Parse(time.RFC3339, cStr)
	if err != nil {
		t.Fatalf("created_at not RFC3339: %q (%v)", cStr, err)
	}
	if !gotC.Equal(created) {
		t.Errorf("created_at: want %s, got %s", created, gotC)
	}

	// End-to-end: the exact staleness filter from the bug report must now select
	// this row (updated 2026-07-27 > 2026-07-25), proving the fix closes the trap
	// rather than just adding a field.
	if !gotU.After(time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("staleness filter (updated_at > 2026-07-25) must select this row; got %s", gotU)
	}
}

// The --help / Long text must document the JSON schema and cross-advertise
// `kd list --parent` so a reader finds the full-row subtree query at the moment
// of need (Fix site B).
func TestChildrenCmd_LongText_DocumentsSchemaAndListParent(t *testing.T) {
	long := childrenCmd.Long
	for _, want := range []string{"dep_type", "bead_id", "kd list --parent"} {
		if !strings.Contains(long, want) {
			t.Errorf("children --help Long must mention %q; got:\n%s", want, long)
		}
	}
	// The in-place `-s` filter must be advertised so a reader stops grep-hacking
	// open-only children (ka-0000002550).
	if !strings.Contains(long, "-s open") {
		t.Errorf("children --help Long must document the `-s open` in-place filter; got:\n%s", long)
	}
}

// runChildrenJSONWithFlags runs `kd children <epic> --json` with the given
// children-command flags set (e.g. status="open"), decoding the rows to maps so
// tests can assert on exact key presence. Flags are reset on cleanup so tests
// stay independent (childrenCmd is a package-global cobra command).
func runChildrenJSONWithFlags(t *testing.T, mc *mockBeadsClient, epic string, flags map[string]string) []map[string]any {
	t.Helper()
	withMockClient(t, mc)
	jsonOutput = true
	defer func() { jsonOutput = false }()
	for k, v := range flags {
		if err := childrenCmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set flag %s=%s: %v", k, v, err)
		}
	}
	t.Cleanup(func() {
		for k := range flags {
			_ = childrenCmd.Flags().Set(k, "")
		}
	})

	out := captureStdout(t, func() {
		if err := childrenCmd.RunE(childrenCmd, []string{epic}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal children JSON: %v\noutput:\n%s", err, out)
	}
	return rows
}

// The `-s`/--status filter keeps only children whose resolved status matches
// (mirrors `kd list -s` semantics) — the load-bearing fix for ka-0000002550,
// which forced gardeners to `| grep -iE "\bopen\b" | grep -v assigned`.
func TestChildrenJSON_StatusFilter_KeepsOnlyMatching(t *testing.T) {
	mc := newMockClient()
	epic := "gd-kf3peczvvr"
	openC, ipC, closedC := "gd-open00001", "gd-betgev3i5j", "gd-l3pbvxlyz5"
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: openC, DependsOnID: epic, Type: model.DepParentChild},
		{BeadID: ipC, DependsOnID: epic, Type: model.DepParentChild},
		{BeadID: closedC, DependsOnID: epic, Type: model.DepParentChild},
	}
	mc.Beads[openC] = &model.Bead{ID: openC, Type: model.TypeTask, Status: model.StatusOpen, Title: "open one"}
	mc.Beads[ipC] = &model.Bead{ID: ipC, Type: model.TypeTask, Status: model.StatusInProgress, Title: "in progress one"}
	mc.Beads[closedC] = &model.Bead{ID: closedC, Type: model.TypeTask, Status: model.StatusClosed, Title: "closed one"}

	rows := runChildrenJSONWithFlags(t, mc, epic, map[string]string{"status": string(model.StatusOpen)})
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 open row, got %d: %v", len(rows), rows)
	}
	if rows[0]["id"] != openC {
		t.Errorf("expected the open child %q, got %v", openC, rows[0]["id"])
	}
	if rows[0]["status"] != string(model.StatusOpen) {
		t.Errorf("expected status open, got %v", rows[0]["status"])
	}
}

// A child whose bead fetch FAILS has an UNKNOWN status. A status filter cannot
// prove such a row fails, so it must be KEPT (surfaced), never silently dropped
// — dropping it could hide a genuinely-matching child behind a transient fetch
// error.
func TestChildrenJSON_StatusFilter_KeepsUnknownStatusRow(t *testing.T) {
	mc := newMockClient()
	epic := "gd-7uirvuloiq"
	knownClosed, missing := "gd-ott6gx64dm", "gd-gu8wzzokag"
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: knownClosed, DependsOnID: epic, Type: model.DepParentChild},
		{BeadID: missing, DependsOnID: epic, Type: model.DepParentChild}, // no Beads entry → fetch error → unknown status
	}
	mc.Beads[knownClosed] = &model.Bead{ID: knownClosed, Type: model.TypeTask, Status: model.StatusClosed, Title: "closed"}

	rows := runChildrenJSONWithFlags(t, mc, epic, map[string]string{"status": string(model.StatusOpen)})
	// knownClosed is provably != open → dropped. missing is unknown → kept.
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (the unknown-status one kept, the closed one dropped), got %d: %v", len(rows), rows)
	}
	if rows[0]["id"] != missing {
		t.Errorf("expected the unknown-status child %q to be kept, got %v", missing, rows[0]["id"])
	}
	// Its status is unknown, so no status key is emitted for that row.
	if _, present := rows[0]["status"]; present {
		t.Errorf("unknown-status row must omit the status key; got %v", rows[0]["status"])
	}
}
