package main

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

// ka-0000002877 (discoverability): the nested sub-objects `kd show --json`
// emits under ".dependencies" and ".comments" used key names that do NOT match
// the conventional/top-level vocabulary, and a wrong guess yields a SILENT jq
// null rather than an error:
//
//	.dependencies[] | "\(.id)/\(.status)"   -> null/null   (real: depends_on_*)
//	.comments[]     | "\(.author): \(.body)" -> author: null (real: .text)
//
// These tests pin the additive aliases AND the canonical keys they mirror, so
// neither the discoverability fix nor the backward-compatibility guarantee can
// silently regress.

// nestedObjects extracts the array under key from a decoded bead object as a
// slice of maps, failing the test when the key is absent — the absent key IS
// the silent-jq-null defect, so it must be a hard failure and never a skip.
func nestedObjects(t *testing.T, bead map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := bead[key]
	if !ok {
		t.Fatalf("key %q absent from JSON — this is the silent-jq-null defect", key)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("key %q must decode to an array; got %T", key, raw)
	}
	out := make([]map[string]any, 0, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("%s[%d] must be an object; got %T", key, i, e)
		}
		out = append(out, m)
	}
	return out
}

// assertKeysEqual pins an alias key against the canonical key it mirrors:
// both must be present and carry the identical value. Comparing the two
// (rather than the alias against a literal) is what makes the alias provably a
// mirror instead of an independently-drifting second source of truth.
func assertKeysEqual(t *testing.T, obj map[string]any, alias, canonical, ctx string) {
	t.Helper()
	got, ok := obj[alias]
	if !ok {
		t.Errorf("%s: alias key %q absent — the conventional jq guess silently nulls", ctx, alias)
		return
	}
	want, ok := obj[canonical]
	if !ok {
		t.Errorf("%s: canonical key %q absent — backward-compat regression", ctx, canonical)
		return
	}
	if got != want {
		t.Errorf("%s: alias %q = %v, must mirror canonical %q = %v", ctx, alias, got, canonical, want)
	}
}

// beadWithDepsAndComments reproduces the exact shape the defect was found on:
// a bead carrying both dependency edges and a comment.
func beadWithDepsAndComments(id string) *model.Bead {
	return &model.Bead{
		ID: id, Title: "has deps and comments", Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: id, DependsOnID: "gd-jdo6duberu", Type: model.DepBlocks},
			{BeadID: id, DependsOnID: "gd-72m41ssp9i", Type: model.DepParentChild},
		},
		Comments: []*model.Comment{
			{ID: 1, BeadID: id, Author: "agent-alpha", Text: "first comment"},
			{ID: 2, BeadID: id, Author: "agent-beta", Text: "second comment"},
		},
	}
}

// Repro 1: `.dependencies[] | .id/.status/.title` must resolve, and must equal
// the canonical depends_on_* keys the enrichment populates.
func TestShow_JSON_DependencyAliases_MirrorDependsOnKeys(t *testing.T) {
	mc := newMockClient()
	id := "gd-nested00001"
	mc.Beads[id] = beadWithDepsAndComments(id)
	// Give the neighbors real status/title so the enrichment populates the
	// canonical keys — an alias of an empty field proves nothing.
	mc.Beads["gd-jdo6duberu"] = &model.Bead{
		ID: "gd-jdo6duberu", Title: "the blocker", Type: model.TypeBug, Status: model.StatusOpen,
	}
	mc.Beads["gd-72m41ssp9i"] = &model.Bead{
		ID: "gd-72m41ssp9i", Title: "the epic", Type: model.TypeEpic, Status: model.StatusClosed,
	}

	obj := showJSONObject(t, mc, id)

	for _, key := range []string{"dependencies", "depends_on"} {
		edges := nestedObjects(t, obj, key)
		if len(edges) != 2 {
			t.Fatalf("%s: want 2 edges, got %d", key, len(edges))
		}
		for i, e := range edges {
			ctx := key + "[" + string(rune('0'+i)) + "]"
			assertKeysEqual(t, e, "id", "depends_on_id", ctx)
			assertKeysEqual(t, e, "status", "depends_on_status", ctx)
			assertKeysEqual(t, e, "title", "depends_on_title", ctx)
		}
		// The alias must not shadow the edge's OWN identity keys: bead_id is
		// this bead and "id" is the NEIGHBOUR, so both must survive and
		// differ. A caller that conflated them would silently walk the graph
		// backwards.
		if edges[0]["bead_id"] != id {
			t.Errorf("%s: bead_id must stay the edge's own side; got %v", key, edges[0]["bead_id"])
		}
		if edges[0]["id"] == edges[0]["bead_id"] {
			t.Errorf("%s: neighbour alias 'id' must differ from 'bead_id'; both %v", key, edges[0]["id"])
		}
	}
}

// Repro 2: `.comments[] | "\(.author): \(.body)"` — the guess that printed
// "<author>: null". Both halves must now resolve, and .body must mirror .text.
func TestShow_JSON_CommentBodyAlias_MirrorsText(t *testing.T) {
	mc := newMockClient()
	id := "gd-nested00002"
	mc.Beads[id] = beadWithDepsAndComments(id)

	obj := showJSONObject(t, mc, id)

	comments := nestedObjects(t, obj, "comments")
	if len(comments) != 2 {
		t.Fatalf("want 2 comments, got %d", len(comments))
	}
	for i, c := range comments {
		ctx := "comments[" + string(rune('0'+i)) + "]"
		assertKeysEqual(t, c, "body", "text", ctx)
		// .author was ALREADY correct pre-fix (only .body was missing); pin it
		// so the wrapper's embedding cannot drop it.
		if c["author"] == nil || c["author"] == "" {
			t.Errorf("%s: author must survive the wrapper; got %v", ctx, c["author"])
		}
	}
	if got := comments[0]["body"]; got != "first comment" {
		t.Errorf("comments[0].body = %v, want %q", got, "first comment")
	}
}

// The defect's worst case: a bead with comments but NO edges and NO children.
// beadJSONValue used to early-return the BARE bead for exactly this shape, so
// the comments promoted straight off model.Bead with no "body" alias — the fix
// is only real if this path is wrapped too.
func TestShow_JSON_CommentAlias_OnBeadWithNoEdgesOrChildren(t *testing.T) {
	mc := newMockClient()
	id := "gd-comments0001"
	mc.Beads[id] = &model.Bead{
		ID: id, Title: "comments only", Type: model.TypeTask, Status: model.StatusOpen,
		Comments: []*model.Comment{
			{ID: 7, BeadID: id, Author: "agent-solo", Text: "only comment"},
		},
	}

	obj := showJSONObject(t, mc, id)

	comments := nestedObjects(t, obj, "comments")
	if len(comments) != 1 {
		t.Fatalf("want 1 comment, got %d", len(comments))
	}
	assertKeysEqual(t, comments[0], "body", "text", "comments[0]")
	// The embedded bead's own fields must still promote at the top level.
	if obj["id"] != id {
		t.Errorf("bead id must serialize at the top level; got %v", obj["id"])
	}
	// No edges means no edge keys — the wrapper must not manufacture them.
	if _, present := obj["dependencies"]; present {
		t.Errorf("dependencies must stay omitted on an edge-less bead; got %v", obj["dependencies"])
	}
}

// An empty-bodied comment must still expose "body". omitempty here would
// reintroduce the very ambiguity being fixed: a caller could not distinguish
// "key absent (wrong schema guess)" from "value empty (real data)".
func TestShow_JSON_CommentBodyAlias_PresentWhenTextEmpty(t *testing.T) {
	mc := newMockClient()
	id := "gd-emptybody001"
	mc.Beads[id] = &model.Bead{
		ID: id, Title: "empty comment", Type: model.TypeTask, Status: model.StatusOpen,
		Comments: []*model.Comment{{ID: 9, BeadID: id, Author: "agent-x", Text: ""}},
	}

	obj := showJSONObject(t, mc, id)

	comments := nestedObjects(t, obj, "comments")
	if len(comments) != 1 {
		t.Fatalf("want 1 comment, got %d", len(comments))
	}
	body, present := comments[0]["body"]
	if !present {
		t.Fatalf("body must be present even when text is empty — absent-vs-empty is the defect")
	}
	if body != "" {
		t.Errorf("body = %v, want empty string", body)
	}
}

// A comment-less, edge-less, child-less bead must encode byte-identically to a
// bare *model.Bead — the wrapper must add nothing. This is the guarantee that
// the widened early-return test did not make every bead grow keys.
func TestShow_JSON_BareBead_GrowsNoNestedKeys(t *testing.T) {
	mc := newMockClient()
	id := "gd-bare0000001"
	mc.Beads[id] = &model.Bead{ID: id, Title: "leaf", Type: model.TypeTask, Status: model.StatusOpen}

	obj := showJSONObject(t, mc, id)

	for _, key := range []string{"dependencies", "depends_on", "comments", "children_count"} {
		if _, present := obj[key]; present {
			t.Errorf("bare bead must omit %q; got %v", key, obj[key])
		}
	}
}

// The un-enriched path: when the neighbour lookup cannot resolve a bead, the
// canonical depends_on_status/title are omitted, and the aliases must be
// omitted WITH them — an alias that emitted "" while its canonical key was
// absent would be a second, contradicting source of truth. ".id" is NOT
// omitempty (DependsOnID is always known from the edge itself), so it stays.
func TestShow_JSON_DependencyAliases_OmittedWhenNeighborUnresolved(t *testing.T) {
	mc := newMockClient()
	id := "gd-unenriched01"
	mc.Beads[id] = &model.Bead{
		ID: id, Title: "dangling edge", Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			// Deliberately NOT registered in mc.Beads — unresolvable.
			{BeadID: id, DependsOnID: "gd-50e7kccfk8", Type: model.DepBlocks},
		},
	}

	obj := showJSONObject(t, mc, id)

	edges := nestedObjects(t, obj, "dependencies")
	if len(edges) != 1 {
		t.Fatalf("want 1 edge, got %d", len(edges))
	}
	// id is always present — it comes off the edge, not the neighbour lookup.
	if edges[0]["id"] != "gd-50e7kccfk8" {
		t.Errorf("id must mirror depends_on_id even un-enriched; got %v", edges[0]["id"])
	}
	for _, pair := range [][2]string{{"status", "depends_on_status"}, {"title", "depends_on_title"}} {
		alias, canonical := pair[0], pair[1]
		_, aliasPresent := edges[0][alias]
		_, canonicalPresent := edges[0][canonical]
		if aliasPresent != canonicalPresent {
			t.Errorf("alias %q presence (%v) must track canonical %q presence (%v)",
				alias, aliasPresent, canonical, canonicalPresent)
		}
	}
}
