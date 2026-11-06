package main

import (
	"encoding/json"
	"testing"

	"github.com/spf13/pflag"
)

// TestCreateCmd_BundleKindRoutedToField verifies that `kd create --type=bundle
// -k <v>` routes the value into the required `kind` FIELD (fields.kind) rather
// than the server-ignored top-level bead Kind. Before ka-0000002997 every
// bundle create 400'd "kind: is required" because the -k value never reached
// the field the type=bundle schema requires.
func TestCreateCmd_BundleKindRoutedToField(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	setFlag(t, "type", "bundle")
	setFlag(t, "kind", "plan")
	setFlag(t, "project", "gardenio")
	defer resetCreateFlags(t)

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a bundle deliverable"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", got)
	}
	req := mc.CreateBeadCalls[0]

	// The top-level request Kind must NOT carry the bundle kind — it is
	// server-ignored (bead.Kind is always derived from the type config), so
	// leaving it set would send a dead, misleading value.
	if req.Kind != "" {
		t.Errorf("top-level Kind = %q, want empty (bundle kind belongs in fields.kind)", req.Kind)
	}

	// fields.kind must carry the value so the server-side required-field check
	// on type=bundle passes.
	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("unmarshal Fields %q: %v", string(req.Fields), err)
	}
	if got := fields["kind"]; got != "plan" {
		t.Errorf("fields.kind = %v, want %q", got, "plan")
	}
}

// TestCreateCmd_BundleKindConflict verifies that supplying both --kind and
// -f kind=… for a bundle is a hard error rather than a silent winner-pick,
// mirroring the --format vs -f format=… guard.
func TestCreateCmd_BundleKindConflict(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	setFlag(t, "type", "bundle")
	setFlag(t, "kind", "plan")
	setFlag(t, "project", "gardenio")
	if err := createCmd.Flags().Set("field", "kind=report"); err != nil {
		t.Fatalf("set field: %v", err)
	}
	defer resetCreateFlags(t)

	err := createCmd.RunE(createCmd, []string{"a conflicted bundle"})
	if err == nil {
		t.Fatalf("expected error when both --kind and -f kind=… are set")
	}
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("expected no CreateBead call on conflict, got %d", len(mc.CreateBeadCalls))
	}
}

// TestCreateCmd_NonBundleKindUntouched verifies the routing is scoped to
// type=bundle: for a plain task, -k stays on the top-level request Kind (a
// no-op server-side, but not stuffed into fields where it would be rejected as
// an unknown field).
func TestCreateCmd_NonBundleKindUntouched(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	setFlag(t, "type", "task")
	setFlag(t, "kind", "issue")
	setFlag(t, "project", "gardenio")
	defer resetCreateFlags(t)

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a plain task"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", got)
	}
	req := mc.CreateBeadCalls[0]
	if req.Kind != "issue" {
		t.Errorf("top-level Kind = %q, want issue (unchanged for non-bundle types)", req.Kind)
	}
	// No kind field should have been injected.
	if len(req.Fields) > 0 {
		var fields map[string]any
		if err := json.Unmarshal(req.Fields, &fields); err != nil {
			t.Fatalf("unmarshal Fields: %v", err)
		}
		if _, ok := fields["kind"]; ok {
			t.Errorf("fields.kind should not be set for a non-bundle type; got %v", fields)
		}
	}
}

// setFlag sets a create-command flag, failing the test on error.
func setFlag(t *testing.T, name, val string) {
	t.Helper()
	if err := createCmd.Flags().Set(name, val); err != nil {
		t.Fatalf("set %s=%s: %v", name, val, err)
	}
}

// resetCreateFlags restores the create-command flags mutated by these tests to
// their zero values so cross-test flag state does not leak (cobra flags are
// package-global on the shared createCmd). Slice flags (--field) must be
// Replace(nil)'d — Set("") would APPEND an empty element, not clear them.
func resetCreateFlags(t *testing.T) {
	t.Helper()
	for _, name := range []string{"type", "kind", "project", "field"} {
		f := createCmd.Flags().Lookup(name)
		if f == nil {
			continue
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
			continue
		}
		_ = createCmd.Flags().Set(name, "")
	}
	// Restore the type flag's default so a later test that omits --type still
	// sees "task" rather than the empty string this reset leaves behind.
	_ = createCmd.Flags().Set("type", "task")
}
