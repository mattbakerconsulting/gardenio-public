package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// setDepType sets the shared depAddCmd's --type flag for the duration of a
// test and restores its prior value on cleanup, so flag state never leaks
// between tests that reuse the package-level command object.
func setDepType(t *testing.T, typ string) {
	t.Helper()
	f := depAddCmd.Flags().Lookup("type")
	prev := f.Value.String()
	if err := depAddCmd.Flags().Set("type", typ); err != nil {
		t.Fatalf("set --type=%s: %v", typ, err)
	}
	t.Cleanup(func() { _ = depAddCmd.Flags().Set("type", prev) })
}

// depWasCanonicalized is the pure predicate behind the loud flip note
// (ka-0000007975 / ka-0000006880). It must fire ONLY when the server returned
// the exact reverse of a parent-child request, and stay silent for the
// unchanged orientation, non-parent-child types, self-edges, and nil deps.
func TestDepWasCanonicalized(t *testing.T) {
	pc := func(bead, dependsOn string) *model.Dependency {
		return &model.Dependency{BeadID: bead, DependsOnID: dependsOn, Type: model.DepParentChild}
	}
	cases := []struct {
		name              string
		reqBead, reqDepOn string
		dep               *model.Dependency
		want              bool
	}{
		{
			name: "reversed parent-child is flagged",
			// caller typed `kd dep add <epic> <child>` → server stored
			// child child-of epic, i.e. endpoints swapped.
			reqBead: "gd-epic", reqDepOn: "gd-child",
			dep:  pc("gd-child", "gd-epic"),
			want: true,
		},
		{
			name:    "unchanged orientation is not flagged",
			reqBead: "gd-child", reqDepOn: "gd-epic",
			dep:  pc("gd-child", "gd-epic"),
			want: false,
		},
		{
			name:    "non parent-child type never flags even if swapped",
			reqBead: "gd-a", reqDepOn: "gd-b",
			dep:  &model.Dependency{BeadID: "gd-b", DependsOnID: "gd-a", Type: model.DepBlocks},
			want: false,
		},
		{
			name:    "self edge is not flagged",
			reqBead: "gd-x", reqDepOn: "gd-x",
			dep:  pc("gd-x", "gd-x"),
			want: false,
		},
		{
			name:    "nil dep is not flagged",
			reqBead: "gd-a", reqDepOn: "gd-b",
			dep:  nil,
			want: false,
		},
		{
			name:    "unrelated endpoints not flagged",
			reqBead: "gd-a", reqDepOn: "gd-b",
			dep:  pc("gd-c", "gd-d"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := depWasCanonicalized(tc.reqBead, tc.reqDepOn, tc.dep); got != tc.want {
				t.Errorf("depWasCanonicalized(%q,%q,%+v) = %v, want %v",
					tc.reqBead, tc.reqDepOn, tc.dep, got, tc.want)
			}
		})
	}
}

// TestDepAdd_LoudFlipNote_LastLine is the end-to-end lock for Part A of
// gd-4oDyZnJwbm: when the server auto-canonicalizes a reversed parent-child
// edge, `kd dep add` must print an UNMISSABLE note AS THE LAST stdout line so
// it survives the ubiquitous `... 2>&1 | tail -N` truncation (ka-0000007975).
func TestDepAdd_LoudFlipNote_LastLine(t *testing.T) {
	mc := newMockClient()
	// Simulate the server flip: caller asks for (epic, child) but the stored
	// row comes back swapped to (child, epic).
	mc.AddDepFn = func(reqBead, reqDepOn, typ string) *model.Dependency {
		return &model.Dependency{
			BeadID:      reqDepOn, // child
			DependsOnID: reqBead,  // epic
			Type:        model.DependencyType(typ),
			CreatedBy:   "test-actor",
		}
	}
	withMockClient(t, mc)
	setDepType(t, "parent-child")

	out := captureStdout(t, func() {
		if err := depAddCmd.RunE(depAddCmd, []string{"gd-epic", "gd-child"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if !strings.Contains(out, "orientation auto-canonicalized") {
		t.Fatalf("expected loud flip note; got:\n%s", out)
	}
	if !strings.Contains(out, "stored gd-child child-of gd-epic") {
		t.Errorf("note must name stored child-of parent orientation; got:\n%s", out)
	}
	if !strings.Contains(out, "ka-0000006880") {
		t.Errorf("note must cross-reference ka-0000006880; got:\n%s", out)
	}

	// THE load-bearing invariant: the note is the LAST non-empty line so
	// `tail -1` keeps it. Trailing blank lines are tolerated.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "NOTE: orientation auto-canonicalized") {
		t.Errorf("flip note must be the LAST stdout line (survives `tail -1`); last line was:\n%q\nfull:\n%s", last, out)
	}
}

// TestDepAdd_NoFlipNote_WhenNotCanonicalized locks the negative: an ordinary
// add whose orientation the server left alone must NOT print the note.
func TestDepAdd_NoFlipNote_WhenNotCanonicalized(t *testing.T) {
	mc := newMockClient() // default AddDependency echoes request unchanged
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := depAddCmd.RunE(depAddCmd, []string{"gd-child", "gd-epic"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if strings.Contains(out, "auto-canonicalized") {
		t.Errorf("no flip note expected for unchanged orientation; got:\n%s", out)
	}
}

// TestDepAdd_JSON_CanonicalizedField locks the --json Part-A surface: a flipped
// add carries a top-level "canonicalized": true, and the original dependency
// fields survive alongside it.
func TestDepAdd_JSON_CanonicalizedField(t *testing.T) {
	mc := newMockClient()
	mc.AddDepFn = func(reqBead, reqDepOn, typ string) *model.Dependency {
		return &model.Dependency{
			BeadID:      reqDepOn,
			DependsOnID: reqBead,
			Type:        model.DependencyType(typ),
		}
	}
	withMockClient(t, mc)
	setDepType(t, "parent-child")
	jsonOutput = true

	out := captureStdout(t, func() {
		if err := depAddCmd.RunE(depAddCmd, []string{"gd-epic", "gd-child"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	var parsed struct {
		BeadID        string `json:"bead_id"`
		DependsOnID   string `json:"depends_on_id"`
		Type          string `json:"type"`
		Canonicalized bool   `json:"canonicalized"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if !parsed.Canonicalized {
		t.Errorf("expected canonicalized=true in JSON; got %s", out)
	}
	if parsed.BeadID != "gd-child" || parsed.DependsOnID != "gd-epic" {
		t.Errorf("stored orientation not preserved in JSON; got %s", out)
	}
}

// TestDepAdd_JSON_NotCanonicalized locks the JSON negative: an unchanged add
// serializes canonicalized=false.
func TestDepAdd_JSON_NotCanonicalized(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	jsonOutput = true

	out := captureStdout(t, func() {
		if err := depAddCmd.RunE(depAddCmd, []string{"gd-child", "gd-epic"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	var parsed struct {
		Canonicalized bool `json:"canonicalized"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if parsed.Canonicalized {
		t.Errorf("expected canonicalized=false for unchanged add; got %s", out)
	}
}
