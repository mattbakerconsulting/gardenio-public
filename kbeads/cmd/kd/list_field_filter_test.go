package main

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/model"
)

// TestList_FieldFilter pins how `--field` values reach req.FieldFilters:
//
//	--field key=val → exact-value match ("val")
//	--field key=*   → presence sentinel ("*"), resolved server-side to
//	                  "field present and non-empty"
//	--field key=    → absence sentinel ("")
//
// The CLI passes the raw value straight through; the present/absent
// semantics for "*" and "" are interpreted by the SQL builder
// (internal/store/postgres/queries.go).
func TestList_FieldFilter(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   map[string]string
	}{
		{"value", []string{"sprint=3"}, map[string]string{"sprint": "3"}},
		{"presence", []string{"run_id=*"}, map[string]string{"run_id": "*"}},
		{"absence", []string{"run_id="}, map[string]string{"run_id": ""}},
		{
			"mixed_repeatable",
			[]string{"run_id=*", "sprint=3"},
			map[string]string{"run_id": "*", "sprint": "3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			// One matching row, so the predicate does NOT come back empty:
			// this test pins how --field maps onto the request, and a 0-row
			// answer would additionally trip the hidden-kind nudge and its
			// probe (a second ListBeads call). The nudge itself is covered by
			// TestList_FieldKindNudge_FiresOnHiddenKindZero (this file) and the
			// TestCount_FieldKindNudge_* suite in count_field_kind_nudge_test.go.
			mc.ListBeadsResult = []*model.Bead{
				{ID: "gd-1", Title: "a", Status: "open", Type: "bug"},
			}
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()

			resetListFieldFlag(t)
			for _, f := range tc.fields {
				if err := listCmd.Flags().Set("field", f); err != nil {
					t.Fatalf("setting --field %q: %v", f, err)
				}
			}

			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})

			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			got := mc.ListBeadsCalls[0].FieldFilters
			if len(got) != len(tc.want) {
				t.Fatalf("FieldFilters = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("FieldFilters[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestList_FieldKindNudge_FiresOnHiddenKindZero covers `kd list`'s call site for
// the hidden-kind nudge. The `kd count` suite (count_field_kind_nudge_test.go)
// pins the note's content and every silence case; this pins that the OTHER
// caller is wired up at all — the two RunE bodies render separately (table vs
// JSON vs envelope), so `kd list -f <hidden-kind-field>` returning an empty list
// with no explanation is its own instance of the ka-0000004030 footgun, and an
// empty list is precisely where a reader has no footer total to notice the kind
// filter from.
func TestList_FieldKindNudge_FiresOnHiddenKindZero(t *testing.T) {
	mc := newMockClient()
	// Call 1 = the list itself (0 rows). Call 2 = the nudge probe (rows exist
	// once the implicit kind=issue clause is dropped).
	mc.ListBeadsPages = [][]*model.Bead{{}, {{ID: "gd-1", Title: "a", Status: "open", Type: "agent"}}}
	withMockClient(t, mc)
	withStdoutTTY(t, false) // the operational nudge is TTY-gated; keep it out of the way

	resetListFieldFlag(t)
	if err := listCmd.Flags().Set("field", "spawned_by=*"); err != nil {
		t.Fatalf("setting --field: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if !strings.Contains(stderr, "spawned_by=*") || !strings.Contains(stderr, "--all-types") {
		t.Errorf("kd list must emit the hidden-kind note naming the predicate and --all-types, got %q", stderr)
	}
	if len(mc.ListBeadsCalls) != 2 {
		t.Fatalf("expected 2 ListBeads calls (list + probe), got %d", len(mc.ListBeadsCalls))
	}
	if probe := mc.ListBeadsCalls[1]; len(probe.Kind) != 0 || !probe.CountOnly {
		t.Errorf("probe must clear kind and be count-only, got Kind=%v CountOnly=%v", probe.Kind, probe.CountOnly)
	}
}

// resetListFieldFlag clears the repeatable --field StringArray so a previous
// subtest's values don't leak into the next. StringArray accumulates across
// Set calls and carries the Changed bit between RunE invocations within one
// process.
//
// It ALSO clears on t.Cleanup, so --field never survives this test into the
// rest of the package. Every sibling list test keeps its own narrow reset
// helper (resetListMentionFlags, resetListPredicateFlags, …) that does not know
// about --field, so a leaked value here silently changed THEIR requests: with
// a `-f` predicate still attached and the mock's Total=0, `kd list` now emits
// the hidden-kind field nudge (maybePrintFieldKindNudge), whose probe is a
// second ListBeads call — breaking their `len(ListBeadsCalls) == 1` assertions
// far from the cause. Cleaning up at the source beats teaching six helpers
// about a flag they never set.
// It also clears the PROJECT scoping flags and their package globals. That is
// not incidental: --project defaults to defaultProject(), which reads
// $KD_PROJECT/$BOAT_PROJECT — both set in gardenio agent shells — so without this
// the implicit project label is injected, the nudge takes its extra
// disambiguation probe, and a call-count assertion here fails purely because of
// the developer's environment. A unit test must not read the ambient project.
func resetListFieldFlag(t *testing.T) {
	t.Helper()
	clear := func() {
		listCmd.Flags().VisitAll(func(f *pflag.Flag) {
			switch f.Name {
			// Slice flags: Replace(nil), never Set(DefValue) — a StringSlice's
			// DefValue renders as the literal "[]", which Set parses as a
			// one-element slice containing "[]" and fails --type validation.
			case "field", "type", "kind":
				f.Changed = false
				if sv, ok := f.Value.(pflag.SliceValue); ok {
					_ = sv.Replace(nil)
				}
			case "project", "all-projects", "all-types", "offset":
				f.Changed = false
				_ = f.Value.Set(f.DefValue)
			}
		})
		listProjectFlag = ""
		listAllProjectsFlag = false
	}
	clear()
	t.Cleanup(clear)
}
