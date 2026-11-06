package main

import (
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// ka-0000002550 (D2 — alias help divergence): `kd dep children --help` must
// carry the SAME Long help as `kd children --help`. depChildrenCmd defines only
// a Short, so cobra would print just that one line and drop the JSON-schema doc,
// the `-s`/`-t` filter hints, and the `kd list --parent -s open` recommendation.
// We mirror childrenCmd.Long onto the alias in dep.go's init(); assert they are
// byte-identical so the two can never drift.
func TestDepChildrenAlias_LongMatchesCanonical(t *testing.T) {
	if depChildrenCmd.Long == "" {
		t.Fatal("depChildrenCmd.Long is empty — `kd dep children --help` will drop the schema/filter/`kd list --parent` guidance the canonical command carries")
	}
	if depChildrenCmd.Long != childrenCmd.Long {
		t.Errorf("depChildrenCmd.Long has drifted from childrenCmd.Long:\n--- dep ---\n%s\n--- canonical ---\n%s", depChildrenCmd.Long, childrenCmd.Long)
	}
}

// ka-0000002550 (D1 — status filter): because both `kd children` and
// `kd dep children` register flags via addChildrenFlags, the alias inherits the
// new `-s`/--status flag automatically. Assert the alias exposes it so a gardener
// using the natural `kd dep children <epic> -s open` gets the filter too.
func TestDepChildrenAlias_InheritsStatusFlag(t *testing.T) {
	f := depChildrenCmd.Flags().Lookup("status")
	if f == nil {
		t.Fatal("`kd dep children` is missing the --status flag; addChildrenFlags should give it to the alias")
	}
	if f.Shorthand != "s" {
		t.Errorf("--status shorthand: want \"s\", got %q", f.Shorthand)
	}
}

// Part B (ka-0000003337 read surface): `kd dep list <id>` on a bead with zero
// OUTGOING deps must no longer read as a dead end. It points the reader at
// `kd dep children <id>` and, when the reverse-dep lookup succeeds, names the
// incoming edge count.

func TestDepList_ZeroOutgoing_WithIncoming_CrossRefs(t *testing.T) {
	mc := newMockClient()
	epic := "gd-epic0001"
	// No outgoing deps, but two children point AT the epic (reverse edges).
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: "gd-child1", DependsOnID: epic, Type: model.DepParentChild},
		{BeadID: "gd-child2", DependsOnID: epic, Type: model.DepParentChild},
	}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := depListCmd.RunE(depListCmd, []string{epic}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if !strings.Contains(out, "No dependencies found.") {
		t.Errorf("expected the base message retained; got:\n%s", out)
	}
	if !strings.Contains(out, "2 incoming") {
		t.Errorf("expected incoming count in cross-ref; got:\n%s", out)
	}
	if !strings.Contains(out, "kd dep children "+epic) {
		t.Errorf("expected pointer to `kd dep children`; got:\n%s", out)
	}
}

func TestDepList_ZeroOutgoing_NoIncoming_StaticHint(t *testing.T) {
	mc := newMockClient()
	id := "gd-lonely001"
	// No outgoing, no reverse deps either.
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := depListCmd.RunE(depListCmd, []string{id}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if !strings.Contains(out, "No dependencies found.") {
		t.Errorf("expected base message; got:\n%s", out)
	}
	// Even with zero incoming, the static hint points the reader at the
	// children command rather than dead-ending.
	if !strings.Contains(out, "kd dep children "+id) {
		t.Errorf("expected static children hint; got:\n%s", out)
	}
	// Must NOT claim a positive incoming count.
	if strings.Contains(out, "incoming: see") {
		t.Errorf("must not claim incoming edges when there are none; got:\n%s", out)
	}
}

func TestDepList_ZeroOutgoing_RevDepError_FallsBackToStaticHint(t *testing.T) {
	mc := newMockClient()
	id := "gd-noreverse01"
	mc.RevDepErr = errors.New("reverse deps not supported over gRPC") // e.g. gRPC transport
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := depListCmd.RunE(depListCmd, []string{id}); err != nil {
			t.Fatalf("RunE must still succeed when reverse-dep lookup errors: %v", err)
		}
	})
	if !strings.Contains(out, "No dependencies found.") {
		t.Errorf("expected base message; got:\n%s", out)
	}
	if !strings.Contains(out, "kd dep children "+id) {
		t.Errorf("expected static children hint on reverse-dep error; got:\n%s", out)
	}
	if strings.Contains(out, "incoming: see") {
		t.Errorf("must not claim a count when the lookup failed; got:\n%s", out)
	}
}

// Part C (ka-0000003337 read surface): `kd show <epic>` renders a Children
// line counting incoming parent-child edges; the count excludes non
// parent-child dependents.

func TestShow_ChildrenLine_HumanView(t *testing.T) {
	mc := newMockClient()
	epic := "gd-7auflllwde"
	mc.Beads[epic] = &model.Bead{ID: epic, Title: "an epic", Type: model.TypeEpic, Status: model.StatusOpen}
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: "gd-c1", DependsOnID: epic, Type: model.DepParentChild},
		{BeadID: "gd-c2", DependsOnID: epic, Type: model.DepParentChild},
		{BeadID: "gd-b1", DependsOnID: epic, Type: model.DepBlocks}, // must NOT count
	}
	withMockClient(t, mc)

	out := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{epic}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if !strings.Contains(out, "Children:    2") {
		t.Errorf("expected `Children:    2` (parent-child only); got:\n%s", out)
	}
	if !strings.Contains(out, "kd dep children "+epic) {
		t.Errorf("expected pointer to children command; got:\n%s", out)
	}
}

func TestShow_NoChildrenLine_WhenZero(t *testing.T) {
	mc := newMockClient()
	id := "gd-wpq8nklf9c"
	mc.Beads[id] = &model.Bead{ID: id, Title: "leaf", Type: model.TypeTask, Status: model.StatusOpen}
	// No reverse deps at all.
	withMockClient(t, mc)

	out := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{id}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if strings.Contains(out, "Children:") {
		t.Errorf("no Children line expected when count is 0; got:\n%s", out)
	}
}

func TestShow_JSON_ChildrenCount(t *testing.T) {
	mc := newMockClient()
	epic := "gd-imzal2vuct"
	mc.Beads[epic] = &model.Bead{ID: epic, Title: "json epic", Type: model.TypeEpic, Status: model.StatusOpen}
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: "gd-c1", DependsOnID: epic, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	jsonOutput = true

	out := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{epic}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if !strings.Contains(out, "\"children_count\": 1") {
		t.Errorf("expected children_count in JSON; got:\n%s", out)
	}
	// Base bead fields must survive alongside the added field.
	if !strings.Contains(out, "\"id\": \""+epic+"\"") {
		t.Errorf("bead id must still serialize; got:\n%s", out)
	}
}

func TestShow_JSON_NoChildrenCount_WhenZero(t *testing.T) {
	mc := newMockClient()
	id := "gd-mfqtxyzyjr"
	mc.Beads[id] = &model.Bead{ID: id, Title: "leaf", Type: model.TypeTask, Status: model.StatusOpen}
	withMockClient(t, mc)
	jsonOutput = true

	out := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{id}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if strings.Contains(out, "children_count") {
		t.Errorf("children_count must be omitted when 0 (byte-identical to plain JSON); got:\n%s", out)
	}
}
