package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/pflag"
)

// TestValidateTypeFilter_BuiltinFloor covers the unit-level contract of the
// shared validator: builtin types pass, unknown values fail loud, and the
// error names the offending value — even when ListConfigs returns nothing
// (the builtin floor alone must accept every legitimate built-in type).
func TestValidateTypeFilter_BuiltinFloor(t *testing.T) {
	mc := newMockClient() // ListConfigs returns nil → builtin floor only
	withMockClient(t, mc)

	cases := []struct {
		name      string
		values    []string
		wantErr   bool
		errSubstr string
	}{
		{"empty slice", nil, false, ""},
		{"single builtin task", []string{"task"}, false, ""},
		{"builtin bug", []string{"bug"}, false, ""},
		{"builtin epic/feature/chore", []string{"epic", "feature", "chore"}, false, ""},
		{"builtin trigger", []string{"trigger"}, false, ""},
		{"builtin schedule", []string{"schedule"}, false, ""},
		{"builtin advice/molecule/formula", []string{"advice", "molecule", "formula"}, false, ""},
		{"deprecated alias template", []string{"template"}, false, ""},
		{"unknown garbage", []string{"zzznotatype"}, true, `unknown bead type "zzznotatype"`},
		{"role-name gardener", []string{"gardener"}, true, `unknown bead type "gardener"`},
		{"one good one bad", []string{"task", "zzz"}, true, `unknown bead type "zzz"`},
		{"empty value is not unknown", []string{""}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateTypeFilter(context.Background(), tc.values)
			if tc.wantErr && err == nil {
				t.Fatalf("validateTypeFilter(%v) = nil, want error", tc.values)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateTypeFilter(%v) = %v, want nil", tc.values, err)
			}
			if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

// TestValidateTypeFilter_Normalizes is the regression guard for the trim
// divergence the code-review surfaced: pflag splits `--type=task, bug` into
// `["task", " bug"]` and preserves a trailing space on `--type=task `. The
// validator must (a) accept the padded values and (b) return the TRIMMED
// slice, because the server matches the type column verbatim — sending the raw
// value downstream would silently match nothing (re-opening ka-0000006244).
func TestValidateTypeFilter_Normalizes(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"trailing space", []string{"task "}, []string{"task"}},
		{"leading space from comma split", []string{"task", " bug"}, []string{"task", "bug"}},
		{"surrounding whitespace", []string{"  epic  "}, []string{"epic"}},
		{"empty preserved as empty", []string{""}, []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateTypeFilter(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("validateTypeFilter(%q) errored: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("validateTypeFilter(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("validateTypeFilter(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestValidateTypeFilter_CustomTypeFromRegistry verifies that a custom type
// registered on the daemon (returned by ListConfigs as a type:* key) is
// accepted even though it is not in the compiled-in builtin floor.
func TestValidateTypeFilter_CustomTypeFromRegistry(t *testing.T) {
	mc := newMockClient()
	mc.ListConfigsByNamespace = map[string][]*model.Config{
		"type": {
			{Key: "type:custom-widget"},
			{Key: "type:task"}, // overlaps a builtin — must remain valid
		},
	}
	withMockClient(t, mc)

	if _, err := validateTypeFilter(context.Background(), []string{"custom-widget"}); err != nil {
		t.Errorf("custom registered type rejected: %v", err)
	}
	if len(mc.ListConfigsCalls) == 0 || mc.ListConfigsCalls[0] != "type" {
		t.Errorf("expected ListConfigs(ctx, \"type\") to be called, got calls %v", mc.ListConfigsCalls)
	}
}

// TestValidateTypeFilter_ListConfigsErrorDegrades verifies that a transport
// error from ListConfigs degrades to the builtin floor rather than failing a
// legitimate built-in type. A daemon hiccup must never turn `--type=task` into
// a hard error.
func TestValidateTypeFilter_ListConfigsErrorDegrades(t *testing.T) {
	mc := newMockClient()
	mc.ListConfigsErr = errors.New("connection refused")
	withMockClient(t, mc)

	if _, err := validateTypeFilter(context.Background(), []string{"task"}); err != nil {
		t.Errorf("builtin type rejected when ListConfigs errored: %v", err)
	}
	// A custom type that only the (now-unreachable) registry knew about is
	// correctly rejected — we cannot confirm it, so failing loud is right.
	if _, err := validateTypeFilter(context.Background(), []string{"custom-widget"}); err == nil {
		t.Error("expected unknown custom type to be rejected when registry is unavailable")
	}
}

// TestValidateTypeFilter_BuiltinFastPath verifies the efficiency property: a
// value already covered by the in-memory builtin floor must NOT trigger a
// ListConfigs round-trip. Only an uncovered (potentially custom) value should.
func TestValidateTypeFilter_BuiltinFastPath(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	if _, err := validateTypeFilter(context.Background(), []string{"task", "bug"}); err != nil {
		t.Fatalf("builtin types errored: %v", err)
	}
	if len(mc.ListConfigsCalls) != 0 {
		t.Errorf("builtin-only --type should not call ListConfigs, got %d calls", len(mc.ListConfigsCalls))
	}
}

// TestUnknownTypeMessage_Hints covers the did-you-mean and role-label hints.
func TestUnknownTypeMessage_Hints(t *testing.T) {
	known := map[string]struct{}{"task": {}, "epic": {}, "trigger": {}, "bug": {}}

	t.Run("near-miss appends did-you-mean", func(t *testing.T) {
		msg := unknownTypeMessage("tas", known)
		if !strings.Contains(msg, `did you mean "task"?`) {
			t.Errorf("expected did-you-mean task hint, got: %s", msg)
		}
	})
	t.Run("role namespace appends label hint", func(t *testing.T) {
		msg := unknownTypeMessage("gardener", known)
		if !strings.Contains(msg, "-l role:gardener") {
			t.Errorf("expected role-label hint, got: %s", msg)
		}
	})
	t.Run("far value gets no spurious did-you-mean", func(t *testing.T) {
		msg := unknownTypeMessage("zzznotatype", known)
		if strings.Contains(msg, "did you mean") {
			t.Errorf("unexpected did-you-mean for unrelated value: %s", msg)
		}
	})
	t.Run("lists valid types sorted", func(t *testing.T) {
		msg := unknownTypeMessage("zzz", known)
		if !strings.Contains(msg, "valid types: bug, epic, task, trigger") {
			t.Errorf("expected sorted valid-type list, got: %s", msg)
		}
	})
	t.Run("short value gets no spurious did-you-mean", func(t *testing.T) {
		// 1-2 char inputs must not match a short type name within distance 2
		// (e.g. would-be `dm`/`do` suggestions). len<4 → maxDist 1.
		for _, v := range []string{"x", "a", "do", "zz"} {
			msg := unknownTypeMessage(v, known)
			if strings.Contains(msg, "did you mean") {
				t.Errorf("unexpected did-you-mean for short value %q: %s", v, msg)
			}
		}
	})
	t.Run("role value gets role hint and no contradictory did-you-mean", func(t *testing.T) {
		// `triage` is a role namespace AND within edit distance 2 of `trigger`;
		// the role hint must win and the type suggestion must be suppressed so
		// the two hints never contradict.
		msg := unknownTypeMessage("triage", known)
		if !strings.Contains(msg, "-l role:triage") {
			t.Errorf("expected role hint for triage, got: %s", msg)
		}
		if strings.Contains(msg, "did you mean") {
			t.Errorf("role value must not also emit a did-you-mean, got: %s", msg)
		}
	})
}

func TestClosestType(t *testing.T) {
	valid := []string{"task", "bug", "epic", "trigger", "dm", "doc", "decision"}
	cases := []struct {
		value string
		want  string
	}{
		{"tas", "task"},       // 3 chars, distance 1 → suggest
		{"bg", "bug"},         // 2 chars, distance 1 (insert) → suggest
		{"epicc", "epic"},     // 5 chars, distance 1 → suggest
		{"triger", "trigger"}, // 6 chars, distance 1 → suggest
		{"x", ""},             // 1 char → maxDist 0, never suggest
		{"zz", ""},            // 2 chars, every type ≥ distance 2 → no suggest
		{"zzznotatype", ""},   // far from everything
	}
	for _, tc := range cases {
		if got := closestType(tc.value, valid); got != tc.want {
			t.Errorf("closestType(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"task", "task", 0},
		{"tas", "task", 1},
		{"task", "tsak", 2},
		{"epic", "epicc", 1},
		{"", "abc", 3},
		{"abc", "", 3},
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// resetTypeFlag clears the --type slice flag on a command between subtests so a
// prior value does not leak. Slice flags cannot be reset via Set(DefValue)
// (pflag's slice DefValue is the literal "[]"), so use the SliceValue
// interface — same pattern as resetListProjectFlags.
func resetTypeFlag(t *testing.T, flags *pflag.FlagSet) {
	t.Helper()
	if f := flags.Lookup("type"); f != nil {
		f.Changed = false
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		}
	}
}

// TestListCmd_UnknownType_FailsLoud is the end-to-end regression for
// ka-0000006244 on the `kd list` path: an unknown --type value must return a
// non-nil error (→ non-zero exit) carrying the "unknown bead type" message,
// and must NOT reach the ListBeads RPC.
func TestListCmd_UnknownType_FailsLoud(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetListProjectFlags(t)
	resetTypeFlag(t, listCmd.Flags())

	if err := listCmd.Flags().Set("type", "gardener"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}

	err := listCmd.RunE(listCmd, nil)
	if err == nil {
		t.Fatal("kd list --type=gardener returned nil error; want failure")
	}
	if !strings.Contains(err.Error(), `unknown bead type "gardener"`) {
		t.Errorf("error %q missing 'unknown bead type \"gardener\"'", err.Error())
	}
	if len(mc.ListBeadsCalls) != 0 {
		t.Errorf("ListBeads should not be called for an invalid --type, got %d calls", len(mc.ListBeadsCalls))
	}
}

// TestListCmd_ValidType_Succeeds is the complementary guard: a valid built-in
// --type still reaches the RPC and returns no error.
func TestListCmd_ValidType_Succeeds(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetListProjectFlags(t)
	resetTypeFlag(t, listCmd.Flags())

	if err := listCmd.Flags().Set("type", "task"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("kd list --type=task: %v", err)
		}
	})
	if len(mc.ListBeadsCalls) != 1 {
		t.Errorf("expected 1 ListBeads call for valid --type, got %d", len(mc.ListBeadsCalls))
	}
}

// TestSearchCmd_UnknownType_FailsLoud mirrors the list regression on the
// `kd search` path.
func TestSearchCmd_UnknownType_FailsLoud(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetTypeFlag(t, searchCmd.Flags())
	t.Cleanup(func() { resetTypeFlag(t, searchCmd.Flags()) })

	if err := searchCmd.Flags().Set("type", "zzznotatype"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}

	err := searchCmd.RunE(searchCmd, []string{"anything"})
	if err == nil {
		t.Fatal("kd search --type=zzznotatype returned nil error; want failure")
	}
	if !strings.Contains(err.Error(), `unknown bead type "zzznotatype"`) {
		t.Errorf("error %q missing 'unknown bead type \"zzznotatype\"'", err.Error())
	}
	if len(mc.ListBeadsCalls) != 0 {
		t.Errorf("ListBeads should not be called for an invalid --type, got %d calls", len(mc.ListBeadsCalls))
	}
}
