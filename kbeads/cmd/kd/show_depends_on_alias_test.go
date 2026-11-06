package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// ka-0000007268 (discoverability): `kd show --json` exposed dependency edges
// ONLY under ".dependencies", while the text view labels the same data
// "Depends On:" and `kd dep list --json` records use "depends_on_id" — three
// vocabularies for one concept. Scripting the obvious ".depends_on" path
// yielded a silent jq null (an absent key is not an error), pushing agents
// into fragile text parsing. These tests pin the additive "depends_on" alias
// and the backward-compatibility of the canonical ".dependencies" key.

// beadWithEdges is a bead carrying one edge of each type the alias must
// surface.
func beadWithEdges(id string) *model.Bead {
	return &model.Bead{
		ID: id, Title: "has edges", Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: id, DependsOnID: "gd-jdo6duberu", Type: model.DepBlocks},
			{BeadID: id, DependsOnID: "gd-72m41ssp9i", Type: model.DepParentChild},
		},
	}
}

// depTypes extracts the "type" of every edge under key from a decoded bead
// object, so a test can assert the alias and the canonical key agree.
func depTypes(t *testing.T, bead map[string]any, key string) []string {
	t.Helper()
	raw, ok := bead[key]
	if !ok {
		t.Fatalf("key %q absent from JSON — this is the silent-jq-null defect", key)
	}
	edges, ok := raw.([]any)
	if !ok {
		t.Fatalf("key %q must decode to an array; got %T", key, raw)
	}
	types := make([]string, 0, len(edges))
	for _, e := range edges {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("edge under %q must be an object; got %T", key, e)
		}
		ty, _ := m["type"].(string)
		types = append(types, ty)
	}
	return types
}

// showJSONObject runs `kd show <id> --json` against the mock and decodes the
// single-bead object.
func showJSONObject(t *testing.T, mc *mockBeadsClient, id string) map[string]any {
	t.Helper()
	wireShowMock(t, mc)
	jsonOutput = true

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{id})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	var obj map[string]any
	if uErr := json.Unmarshal([]byte(out), &obj); uErr != nil {
		t.Fatalf("unmarshal: %v\noutput:\n%s", uErr, out)
	}
	return obj
}

// Verify 1 + 2: ".depends_on" resolves to the edge list, and ".dependencies"
// still carries the identical data (no regression for existing consumers).
func TestShow_JSON_DependsOnAlias_MirrorsDependencies(t *testing.T) {
	mc := newMockClient()
	id := "gd-qeimcbgw5k"
	mc.Beads[id] = beadWithEdges(id)

	obj := showJSONObject(t, mc, id)

	alias := depTypes(t, obj, "depends_on")
	canonical := depTypes(t, obj, "dependencies")

	want := []string{string(model.DepBlocks), string(model.DepParentChild)}
	if strings.Join(alias, ",") != strings.Join(want, ",") {
		t.Errorf("depends_on edge types: want %v, got %v", want, alias)
	}
	if strings.Join(canonical, ",") != strings.Join(alias, ",") {
		t.Errorf("the alias must mirror the canonical key exactly:\n  dependencies: %v\n  depends_on:   %v", canonical, alias)
	}
}

// A bead with no edges must not grow an empty "depends_on" key — omitempty
// keeps the no-edge/no-children encoding byte-identical to a bare bead, the
// same contract children_count holds.
func TestShow_JSON_DependsOnAlias_OmittedWhenNoEdges(t *testing.T) {
	mc := newMockClient()
	id := "gd-noedges0001"
	mc.Beads[id] = &model.Bead{ID: id, Title: "leaf", Type: model.TypeTask, Status: model.StatusOpen}

	obj := showJSONObject(t, mc, id)

	if _, present := obj["depends_on"]; present {
		t.Errorf("depends_on must be omitted when the bead has no edges; got %v", obj["depends_on"])
	}
	if _, present := obj["dependencies"]; present {
		t.Errorf("dependencies must stay omitted when the bead has no edges; got %v", obj["dependencies"])
	}
}

// The alias must compose with the children_count read-surface (ka-0000003337)
// rather than displace it — both wrappers now share one struct.
func TestShow_JSON_DependsOnAlias_CoexistsWithChildrenCount(t *testing.T) {
	mc := newMockClient()
	id := "gd-both0000001"
	mc.Beads[id] = beadWithEdges(id)
	mc.RevDeps[id] = []*model.Dependency{
		{BeadID: "gd-pxtnwu1uht", DependsOnID: id, Type: model.DepParentChild},
	}

	obj := showJSONObject(t, mc, id)

	if cc, ok := obj["children_count"].(float64); !ok || cc != 1 {
		t.Errorf("children_count must survive alongside the alias; got %v", obj["children_count"])
	}
	if got := depTypes(t, obj, "depends_on"); len(got) != 2 {
		t.Errorf("depends_on must carry both edges alongside children_count; got %v", got)
	}
	// The embedded bead's own fields must still promote to the top level.
	if obj["id"] != id {
		t.Errorf("bead id must still serialize at the top level; got %v", obj["id"])
	}
}

// Verify 3: the alias is present per-entry in the multi-id array surface too,
// so `kd show <a> <b> --json | jq '.[].depends_on'` works.
func TestShow_MultiID_JSON_DependsOnAlias(t *testing.T) {
	mc := newMockClient()
	withEdges, leaf := "gd-multiedge01", "gd-multileaf01"
	mc.Beads[withEdges] = beadWithEdges(withEdges)
	mc.Beads[leaf] = &model.Bead{ID: leaf, Title: "leaf", Type: model.TypeTask, Status: model.StatusOpen}
	wireShowMock(t, mc)
	jsonOutput = true

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{withEdges, leaf})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	var arr []map[string]any
	if uErr := json.Unmarshal([]byte(out), &arr); uErr != nil {
		t.Fatalf("unmarshal: %v\noutput:\n%s", uErr, out)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 beads; got %d\n%s", len(arr), out)
	}
	if got := depTypes(t, arr[0], "depends_on"); len(got) != 2 {
		t.Errorf("array entry 0 must carry depends_on with both edges; got %v\n%s", got, out)
	}
	if _, present := arr[1]["depends_on"]; present {
		t.Errorf("edge-less array entry must omit depends_on; got %v\n%s", arr[1]["depends_on"], out)
	}
}

// The text view is the label that sent readers looking for ".depends_on" in
// the first place. Pin it so the alias and the label cannot drift apart.
func TestShow_Text_DependsOnLabelMatchesAliasKey(t *testing.T) {
	mc := newMockClient()
	id := "gd-textlabel01"
	mc.Beads[id] = beadWithEdges(id)
	wireShowMock(t, mc)

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{id})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(out, "Depends On:") {
		t.Fatalf("text view must still print the \"Depends On:\" section; got:\n%s", out)
	}
}
