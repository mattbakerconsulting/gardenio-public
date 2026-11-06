package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// ka-0000000053 (discoverability): a `kd show --json` dependency edge carried
// only the endpoint IDs and the edge type, so nothing about the NEIGHBOR was
// readable from it. Any dependency-frontier scan ("are my blockers closed?")
// had to issue a second call per edge just to read a status — the N+1 loop the
// CED inspector reproduced live. These tests pin the denormalized
// depends_on_status / depends_on_title fields, the single-round-trip promise
// that motivated them, and the back-compat contract that nothing an existing
// consumer reads changed shape.

// mixedStatusEpic returns an epic depending on two beads with DIFFERENT
// statuses, so a test asserting the status field can prove it reflects the
// neighbor rather than a constant.
func mixedStatusEpic(t *testing.T) (*mockBeadsClient, string) {
	t.Helper()
	mc := newMockClient()
	epic := "gd-epic0000001"
	openDep, closedDep := "gd-open0000001", "gd-closed00001"

	mc.Beads[epic] = &model.Bead{
		ID: epic, Title: "frontier epic", Type: model.TypeEpic, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: epic, DependsOnID: openDep, Type: model.DepBlocks},
			{BeadID: epic, DependsOnID: closedDep, Type: model.DepParentChild},
		},
	}
	mc.Beads[openDep] = &model.Bead{
		ID: openDep, Title: "still blocking", Type: model.TypeTask, Status: model.StatusOpen,
	}
	mc.Beads[closedDep] = &model.Bead{
		ID: closedDep, Title: "already done", Type: model.TypeTask, Status: model.StatusClosed,
	}
	return mc, epic
}

// showJSONArray runs `kd show <ids...> --json` against the mock and decodes
// the multi-id array surface. The single-id sibling lives in
// show_depends_on_alias_test.go.
func showJSONArray(t *testing.T, mc *mockBeadsClient, ids ...string) []map[string]any {
	t.Helper()
	wireShowMock(t, mc)
	jsonOutput = true

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, ids)
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	var arr []map[string]any
	if uErr := json.Unmarshal([]byte(out), &arr); uErr != nil {
		t.Fatalf("unmarshal: %v\noutput:\n%s", uErr, out)
	}
	return arr
}

// edgeObjects pulls the decoded edge array from under key.
func edgeObjects(t *testing.T, bead map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := bead[key]
	if !ok {
		t.Fatalf("key %q absent from JSON", key)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("key %q must decode to an array; got %T", key, raw)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("edge under %q must be an object; got %T", key, e)
		}
		out = append(out, m)
	}
	return out
}

// THE headline claim: neighbor status and title are readable straight off the
// edge, and the status genuinely VARIES per neighbor (open vs closed) rather
// than echoing the bead under inspection.
func TestShow_JSON_DepEdges_CarryNeighborStatusAndTitle(t *testing.T) {
	mc, epic := mixedStatusEpic(t)

	obj := showJSONObject(t, mc, epic)
	edges := edgeObjects(t, obj, "dependencies")
	if len(edges) != 2 {
		t.Fatalf("expected 2 edges; got %d", len(edges))
	}

	for _, want := range []struct{ id, status, title string }{
		{"gd-open0000001", "open", "still blocking"},
		{"gd-closed00001", "closed", "already done"},
	} {
		var found map[string]any
		for _, e := range edges {
			if e["depends_on_id"] == want.id {
				found = e
				break
			}
		}
		if found == nil {
			t.Fatalf("no edge for %s in %v", want.id, edges)
		}
		if got := found["depends_on_status"]; got != want.status {
			t.Errorf("%s: depends_on_status = %v, want %q", want.id, got, want.status)
		}
		if got := found["depends_on_title"]; got != want.title {
			t.Errorf("%s: depends_on_title = %v, want %q", want.id, got, want.title)
		}
	}
}

// Back-compat: every key an existing consumer read must still be present with
// its original value. The new fields are strictly ADDITIVE — old key set ⊆ new
// key set — which is what lets this ship without a version gate.
func TestShow_JSON_DepEdges_OriginalKeysPreserved(t *testing.T) {
	mc, epic := mixedStatusEpic(t)

	obj := showJSONObject(t, mc, epic)
	edges := edgeObjects(t, obj, "dependencies")

	for _, e := range edges {
		for _, k := range []string{"bead_id", "depends_on_id", "type"} {
			if _, ok := e[k]; !ok {
				t.Errorf("edge lost pre-existing key %q: %v", k, e)
			}
		}
		if e["bead_id"] != epic {
			t.Errorf("bead_id must still name the subject bead; got %v", e["bead_id"])
		}
	}
	// The edge type is the EDGE's, never the neighbor's — the denormalized
	// fields must not have overwritten it.
	types := map[string]bool{}
	for _, e := range edges {
		ty, _ := e["type"].(string)
		types[ty] = true
	}
	if !types[string(model.DepBlocks)] || !types[string(model.DepParentChild)] {
		t.Errorf("edge types must survive denormalization; got %v", types)
	}
}

// The whole point: reading both neighbors' statuses costs ONE batched
// round-trip, not one per edge. This is the assertion that would catch a
// refactor quietly reintroducing the N+1.
func TestShow_JSON_DepEdges_SingleBatchRoundTrip(t *testing.T) {
	mc, epic := mixedStatusEpic(t)

	showJSONObject(t, mc, epic)

	if len(mc.BatchGetCalls) != 1 {
		t.Fatalf("neighbor lookup must be ONE batched call; got %d: %v",
			len(mc.BatchGetCalls), mc.BatchGetCalls)
	}
	if got := len(mc.BatchGetCalls[0]); got != 2 {
		t.Errorf("the one batch call must carry both neighbor ids; got %d: %v",
			got, mc.BatchGetCalls[0])
	}
	// Only the subject bead is fetched individually — never the neighbors.
	if len(mc.GetBeadCalls) != 1 || mc.GetBeadCalls[0] != epic {
		t.Errorf("neighbors must not be fetched one-by-one; GetBead calls: %v", mc.GetBeadCalls)
	}
}

// Two edges of different types between the same pair are common (a blocks edge
// AND a parent-child edge). The neighbor must be requested once, not twice.
func TestShow_JSON_DepEdges_DeduplicatesNeighborIDs(t *testing.T) {
	mc := newMockClient()
	subject, neighbor := "gd-subject0001", "gd-neighbor001"
	mc.Beads[subject] = &model.Bead{
		ID: subject, Title: "subject", Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: subject, DependsOnID: neighbor, Type: model.DepBlocks},
			{BeadID: subject, DependsOnID: neighbor, Type: model.DepParentChild},
		},
	}
	mc.Beads[neighbor] = &model.Bead{
		ID: neighbor, Title: "shared neighbor", Type: model.TypeEpic, Status: model.StatusInProgress,
	}

	obj := showJSONObject(t, mc, subject)

	if len(mc.BatchGetCalls) != 1 || len(mc.BatchGetCalls[0]) != 1 {
		t.Errorf("a repeated neighbor must be fetched once; batch calls: %v", mc.BatchGetCalls)
	}
	// Both edges still get enriched from the single lookup.
	for _, e := range edgeObjects(t, obj, "dependencies") {
		if e["depends_on_status"] != string(model.StatusInProgress) {
			t.Errorf("both edges must carry the shared neighbor's status; got %v", e["depends_on_status"])
		}
	}
}

// Fail-soft: `kd show` must never lose its PRIMARY output because the
// auxiliary neighbor join failed. The bead still renders, the edges still
// carry their original fields, and the denormalized keys are simply absent
// (omitempty) rather than present-and-empty.
func TestShow_JSON_DepEdges_DegradeWhenNeighborLookupFails(t *testing.T) {
	mc, epic := mixedStatusEpic(t)
	mc.BatchGetErr = errors.New("daemon unreachable")

	obj := showJSONObject(t, mc, epic)

	if obj["id"] != epic {
		t.Fatalf("the bead itself must still render; got %v", obj["id"])
	}
	edges := edgeObjects(t, obj, "dependencies")
	if len(edges) != 2 {
		t.Fatalf("edges must survive a failed join; got %d", len(edges))
	}
	for _, e := range edges {
		if _, present := e["depends_on_status"]; present {
			t.Errorf("un-enriched edge must OMIT depends_on_status, not emit empty; got %v", e)
		}
		if _, present := e["depends_on_title"]; present {
			t.Errorf("un-enriched edge must OMIT depends_on_title, not emit empty; got %v", e)
		}
		if _, ok := e["depends_on_id"]; !ok {
			t.Errorf("the original edge fields must survive a failed join; got %v", e)
		}
	}
}

// A neighbor the caller cannot read (or that no longer exists) must leave just
// THAT edge un-enriched — its sibling still gets its status.
func TestShow_JSON_DepEdges_UnreadableNeighborLeavesOnlyThatEdgeBare(t *testing.T) {
	mc := newMockClient()
	subject := "gd-partial0001"
	mc.Beads[subject] = &model.Bead{
		ID: subject, Title: "partial", Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: subject, DependsOnID: "gd-visible0001", Type: model.DepBlocks},
			{BeadID: subject, DependsOnID: "gd-ghost000001", Type: model.DepBlocks},
		},
	}
	mc.Beads["gd-visible0001"] = &model.Bead{
		ID: "gd-visible0001", Title: "readable", Type: model.TypeTask, Status: model.StatusOpen,
	}
	// gd-ghost000001 is deliberately absent → BatchGetBeads reports not-found.

	obj := showJSONObject(t, mc, subject)

	for _, e := range edgeObjects(t, obj, "dependencies") {
		switch e["depends_on_id"] {
		case "gd-visible0001":
			if e["depends_on_status"] != "open" {
				t.Errorf("readable neighbor must still enrich; got %v", e)
			}
		case "gd-ghost000001":
			if _, present := e["depends_on_status"]; present {
				t.Errorf("unresolvable neighbor must stay bare; got %v", e)
			}
		default:
			t.Errorf("unexpected edge: %v", e)
		}
	}
}

// The depends_on alias (ka-0000007268) must carry the SAME denormalized edges
// as the canonical key — the two must not drift into different shapes.
func TestShow_JSON_DepEdges_AliasCarriesEnrichmentToo(t *testing.T) {
	mc, epic := mixedStatusEpic(t)

	obj := showJSONObject(t, mc, epic)

	canonical := edgeObjects(t, obj, "dependencies")
	alias := edgeObjects(t, obj, "depends_on")
	if len(canonical) != len(alias) {
		t.Fatalf("alias/canonical length mismatch: %d vs %d", len(canonical), len(alias))
	}
	for i := range canonical {
		if canonical[i]["depends_on_status"] != alias[i]["depends_on_status"] {
			t.Errorf("edge %d: alias must mirror the canonical enrichment:\n  dependencies: %v\n  depends_on:   %v",
				i, canonical[i], alias[i])
		}
	}
}

// A leaf bead must cost NO neighbor round-trip at all — the join is free when
// there is nothing to join.
func TestShow_JSON_DepEdges_NoRoundTripForLeafBead(t *testing.T) {
	mc := newMockClient()
	leaf := "gd-leaf0000001"
	mc.Beads[leaf] = &model.Bead{ID: leaf, Title: "leaf", Type: model.TypeTask, Status: model.StatusOpen}

	showJSONObject(t, mc, leaf)

	if len(mc.BatchGetCalls) != 0 {
		t.Errorf("a bead with no edges must not trigger a neighbor lookup; got %v", mc.BatchGetCalls)
	}
}

// The text view shows the same neighbor status a human needs to answer "can I
// start yet?" — and keeps the id/(type) prefix so line-slicing callers still
// work.
func TestShow_Text_DepEdges_ShowNeighborStatusAndTitle(t *testing.T) {
	mc, epic := mixedStatusEpic(t)
	wireShowMock(t, mc)

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{epic})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}

	for _, want := range []string{
		"gd-open0000001 (blocks) [open] still blocking",
		"gd-closed00001 (parent-child) [closed] already done",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text Depends On line missing %q:\n%s", want, out)
		}
	}
}

// When the join fails the text line must fall back to EXACTLY the
// pre-denormalization format, so a failed auxiliary lookup cannot garble the
// human view (or leave a dangling "[]").
func TestShow_Text_DepEdges_FallBackToBareLineOnJoinFailure(t *testing.T) {
	mc, epic := mixedStatusEpic(t)
	mc.BatchGetErr = errors.New("daemon unreachable")
	wireShowMock(t, mc)

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{epic})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(out, "  gd-open0000001 (blocks)\n") {
		t.Errorf("expected the bare pre-denormalization line; got:\n%s", out)
	}
	if strings.Contains(out, "[]") {
		t.Errorf("a failed join must not leave empty brackets:\n%s", out)
	}
}

// A long neighbor title is truncated so one verbose epic cannot wrap the
// Depends On section into unreadability.
func TestShow_Text_DepEdges_TruncateLongNeighborTitle(t *testing.T) {
	mc := newMockClient()
	subject, neighbor := "gd-longtitle01", "gd-verbose0001"
	longTitle := strings.Repeat("x", depEdgeTitleWidth+40)
	mc.Beads[subject] = &model.Bead{
		ID: subject, Title: "subject", Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: subject, DependsOnID: neighbor, Type: model.DepBlocks},
		},
	}
	mc.Beads[neighbor] = &model.Bead{
		ID: neighbor, Title: longTitle, Type: model.TypeEpic, Status: model.StatusOpen,
	}
	wireShowMock(t, mc)

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{subject})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if strings.Contains(out, longTitle) {
		t.Errorf("long neighbor title must be truncated:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("truncated title must carry the ellipsis marker:\n%s", out)
	}
}

// The multi-id array surface must enrich every entry, so
// `kd show <a> <b> --json | jq '.[].dependencies[].depends_on_status'` works —
// the batch surface is exactly where a frontier scanner operates.
func TestShow_MultiID_JSON_DepEdges_EnrichEveryEntry(t *testing.T) {
	mc, epic := mixedStatusEpic(t)
	second := "gd-second00001"
	mc.Beads[second] = &model.Bead{
		ID: second, Title: "second", Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: second, DependsOnID: "gd-closed00001", Type: model.DepBlocks},
		},
	}

	arr := showJSONArray(t, mc, epic, second)
	if len(arr) != 2 {
		t.Fatalf("expected 2 beads; got %d", len(arr))
	}
	for i, bead := range arr {
		for _, e := range edgeObjects(t, bead, "dependencies") {
			if _, ok := e["depends_on_status"]; !ok {
				t.Errorf("array entry %d edge lacks depends_on_status: %v", i, e)
			}
		}
	}
}
