package model

import "testing"

func TestKind_IsValid(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		want bool
	}{
		{KindIssue, true},
		{KindData, true},
		{KindConfig, true},
		{Kind(""), false},
		{Kind("bogus"), false},
	} {
		if got := tc.kind.IsValid(); got != tc.want {
			t.Errorf("Kind(%q).IsValid() = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestKind_String(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		want string
	}{
		{KindIssue, "issue"},
		{KindData, "data"},
		{KindConfig, "config"},
	} {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("Kind(%q).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestBeadType_IsValid(t *testing.T) {
	for _, tc := range []struct {
		typ  BeadType
		want bool
	}{
		{TypeEpic, true},
		{TypeTask, true},
		{TypeFeature, true},
		{TypeChore, true},
		{TypeBug, true},
		{BeadType("workflow"), true},
		{BeadType(""), false},
	} {
		if got := tc.typ.IsValid(); got != tc.want {
			t.Errorf("BeadType(%q).IsValid() = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

func TestBeadType_String(t *testing.T) {
	for _, tc := range []struct {
		typ  BeadType
		want string
	}{
		{TypeTask, "task"},
		{TypeBug, "bug"},
		{BeadType("custom"), "custom"},
	} {
		if got := tc.typ.String(); got != tc.want {
			t.Errorf("BeadType(%q).String() = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

func TestStatus_IsValid(t *testing.T) {
	for _, tc := range []struct {
		status Status
		want   bool
	}{
		{StatusOpen, true},
		{StatusInProgress, true},
		{StatusDeferred, true},
		{StatusClosed, true},
		{StatusBlocked, true},
		{StatusPartial, true},
		{Status(""), false},
		{Status("deleted"), false},
	} {
		if got := tc.status.IsValid(); got != tc.want {
			t.Errorf("Status(%q).IsValid() = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestStatus_IsTerminalForProgression(t *testing.T) {
	for _, tc := range []struct {
		status Status
		want   bool
	}{
		// Terminal-for-progression: unblock dependents + fire on_complete.
		{StatusClosed, true},
		{StatusPartial, true},
		// Non-terminal: still active, dependents stay blocked.
		{StatusOpen, false},
		{StatusInProgress, false},
		{StatusBlocked, false},
		{StatusDeferred, false},
		{Status(""), false},
	} {
		if got := tc.status.IsTerminalForProgression(); got != tc.want {
			t.Errorf("Status(%q).IsTerminalForProgression() = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestStatus_String(t *testing.T) {
	for _, tc := range []struct {
		status Status
		want   string
	}{
		{StatusOpen, "open"},
		{StatusClosed, "closed"},
		{StatusInProgress, "in_progress"},
	} {
		if got := tc.status.String(); got != tc.want {
			t.Errorf("Status(%q).String() = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestTypeAliases(t *testing.T) {
	for _, tc := range []struct {
		deprecated BeadType
		canonical  BeadType
	}{
		{"template", TypeFormula},
	} {
		got, ok := TypeAliases[tc.deprecated]
		if !ok {
			t.Errorf("TypeAliases[%q] not found", tc.deprecated)
			continue
		}
		if got != tc.canonical {
			t.Errorf("TypeAliases[%q] = %q, want %q", tc.deprecated, got, tc.canonical)
		}
	}

	// Canonical types should not be aliased.
	for _, canonical := range []BeadType{TypeFormula, TypeMolecule} {
		if _, ok := TypeAliases[canonical]; ok {
			t.Errorf("TypeAliases[%q] should not exist; canonical types must not alias", canonical)
		}
	}
}

func TestIsCrossFleetType(t *testing.T) {
	for _, tc := range []struct {
		t    BeadType
		want bool
	}{
		{TypeProject, true},
		{TypeAccount, true},
		{TypeTeam, true},
		{TypeFleet, false},
		{TypeTask, false},
		{TypeEpic, false},
		{"", false},
		{"custom-type", false},
	} {
		if got := IsCrossFleetType(tc.t); got != tc.want {
			t.Errorf("IsCrossFleetType(%q) = %v, want %v", tc.t, got, tc.want)
		}
	}
}

func TestAllTypesCrossFleet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		types []BeadType
		want  bool
	}{
		{"empty returns false (no opinion)", nil, false},
		{"single cross-fleet", []BeadType{TypeProject}, true},
		{"both cross-fleet", []BeadType{TypeProject, TypeAccount}, true},
		{"mixed cross+scoped", []BeadType{TypeProject, TypeTask}, false},
		{"all scoped", []BeadType{TypeTask, TypeBug}, false},
		{"unknown type", []BeadType{TypeProject, "custom"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AllTypesCrossFleet(tc.types); got != tc.want {
				t.Errorf("AllTypesCrossFleet(%v) = %v, want %v", tc.types, got, tc.want)
			}
		})
	}
}

func TestDependencyType_IsValid(t *testing.T) {
	for _, tc := range []struct {
		dep  DependencyType
		want bool
	}{
		{DepBlocks, true},
		{DepParentChild, true},
		{DepRelated, true},
		{DepDuplicates, true},
		{DepSupersedes, true},
		{DepRates, true},
		{DepTriggeredBy, true},
		{DependencyType("custom-dep"), true},
		{DependencyType(""), false},
		{DependencyType("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), false}, // 51 chars
	} {
		if got := tc.dep.IsValid(); got != tc.want {
			t.Errorf("DependencyType(%q).IsValid() = %v, want %v", tc.dep, got, tc.want)
		}
	}
}

func TestDepTriggeredBy_Value(t *testing.T) {
	if DepTriggeredBy != "triggered-by" {
		t.Errorf("DepTriggeredBy = %q, want %q", DepTriggeredBy, "triggered-by")
	}
}

// TestParentChildDepAliases pins the parent-child hierarchy alias set that the
// read filters resolve against (ka-0000008107). The live graph carries a large
// body of legacy "parent" edges alongside canonical "parent-child" ones; both
// must resolve as the same hierarchy edge on the read side.
func TestParentChildDepAliases(t *testing.T) {
	if DepParentChildLegacy != "parent" {
		t.Errorf("DepParentChildLegacy = %q, want %q", DepParentChildLegacy, "parent")
	}
	// The canonical type MUST sort first — the postgres IN-list and its pinned
	// test patterns emit them in this order.
	want := []string{"parent-child", "parent"}
	if len(ParentChildDepTypes) != len(want) {
		t.Fatalf("ParentChildDepTypes = %v, want %v", ParentChildDepTypes, want)
	}
	for i := range want {
		if ParentChildDepTypes[i] != want[i] {
			t.Fatalf("ParentChildDepTypes = %v, want %v", ParentChildDepTypes, want)
		}
	}

	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"parent-child", true},   // canonical
		{"parent", true},         // legacy alias — the ka-0000008107 case
		{"blocks", false},        // genuine blocker, never a hierarchy edge
		{"related", false},       // structural but not parent-child
		{"child-of", false},      // surface verb, not a stored deps.type
		{"", false},              // empty is never a match
		{"parent-child ", false}, // exact match only — no trimming
	} {
		if got := IsParentChildDepType(tc.in); got != tc.want {
			t.Errorf("IsParentChildDepType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsValidJackAction(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   bool
	}{
		{"edit", true},
		{"exec", true},
		{"patch", true},
		{"delete", true},
		{"create", true},
		{"", false},
		{"drop", false},
		{"Edit", false}, // case-sensitive
	} {
		if got := IsValidJackAction(tc.action); got != tc.want {
			t.Errorf("IsValidJackAction(%q) = %v, want %v", tc.action, got, tc.want)
		}
	}
}
