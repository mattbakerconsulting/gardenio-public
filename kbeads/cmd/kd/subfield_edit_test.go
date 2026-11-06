package main

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

// resetSubFieldFlags clears the sub-field-edit flags between table cases so
// accumulated values don't leak across invocations in this test binary.
func resetSubFieldFlags(t *testing.T) {
	t.Helper()
	for _, name := range []string{"field-set", "field-unset-key", "field-append", "field-remove"} {
		if f := updateCmd.Flags().Lookup(name); f != nil {
			_ = f.Value.(interface{ Replace([]string) error }).Replace(nil)
			f.Changed = false
		}
	}
}

// TestParseFieldSet covers the <field>.<key>=<value> grammar, including values
// that themselves contain '=' and '.'.
func TestParseFieldSet(t *testing.T) {
	cases := []struct {
		spec              string
		field, key, value string
		wantErr           bool
	}{
		{"channel_roles.C123=ops-lead", "channel_roles", "C123", "ops-lead", false},
		{"m.k=a=b", "m", "k", "a=b", false},     // value may contain '='
		{"m.k.sub=v", "m", "k.sub", "v", false}, // key may contain '.' after the first
		{"nodot=v", "", "", "", true},           // no '.'
		{"channel_roles.=v", "", "", "", true},  // empty key
		{".k=v", "", "", "", true},              // empty field
		{"channel_roles.C1", "", "", "", true},  // no '='
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			e, err := parseFieldSet(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for %q, got %+v", tc.spec, e)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", tc.spec, err)
			}
			if e.field != tc.field || e.req.Op != "set" || e.req.Key != tc.key || e.req.Value != tc.value {
				t.Fatalf("parsed %q → field=%q op=%q key=%q value=%q, want field=%q key=%q value=%q",
					tc.spec, e.field, e.req.Op, e.req.Key, e.req.Value, tc.field, tc.key, tc.value)
			}
		})
	}
}

// TestParseFieldUnsetKey covers the <field>.<key> grammar.
func TestParseFieldUnsetKey(t *testing.T) {
	e, err := parseFieldUnsetKey("channel_roles.C123")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.field != "channel_roles" || e.req.Op != "unset-key" || e.req.Key != "C123" {
		t.Fatalf("parsed → field=%q op=%q key=%q, want channel_roles/unset-key/C123", e.field, e.req.Op, e.req.Key)
	}
	if _, err := parseFieldUnsetKey("nodot"); err == nil {
		t.Fatal("want error for spec with no '.'")
	}
}

// TestParseFieldCSV covers the <field>=<element> grammar for append/remove.
func TestParseFieldCSV(t *testing.T) {
	e, err := parseFieldCSV("slack_channel=C123", "append")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.field != "slack_channel" || e.req.Op != "append" || e.req.Element != "C123" {
		t.Fatalf("parsed → field=%q op=%q element=%q, want slack_channel/append/C123", e.field, e.req.Op, e.req.Element)
	}
	if _, err := parseFieldCSV("slack_channel=", "append"); err == nil {
		t.Fatal("want error for empty element")
	}
	if _, err := parseFieldCSV("noeq", "remove"); err == nil {
		t.Fatal("want error for spec with no '='")
	}
}

// TestUpdate_FieldSetRoutesToEditNotUpdate verifies that a --field-set-only
// invocation routes to EditBeadSubField and does NOT fire a whole-field
// UpdateBead PATCH (which would clobber the aggregate and bump updated_at).
func TestUpdate_FieldSetRoutesToEditNotUpdate(t *testing.T) {
	mc := mockClientWithBead("gd-proj")
	mc.Beads["gd-proj"] = &model.Bead{ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen}
	withMockClient(t, mc)

	resetUpdateFlags(t)
	resetSubFieldFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t); resetSubFieldFlags(t) })

	if err := updateCmd.Flags().Set("field-set", "channel_roles.C123=ops-lead"); err != nil {
		t.Fatalf("set --field-set: %v", err)
	}
	if err := updateCmd.RunE(updateCmd, []string{"gd-proj"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("UpdateBead should NOT be called for a --field-set-only invocation, got %d", len(mc.UpdateBeadCalls))
	}
	if len(mc.EditSubFieldCalls) != 1 {
		t.Fatalf("EditBeadSubField called %d times, want 1", len(mc.EditSubFieldCalls))
	}
	call := mc.EditSubFieldCalls[0]
	if call.Field != "channel_roles" || call.Req.Op != "set" || call.Req.Key != "C123" || call.Req.Value != "ops-lead" {
		t.Fatalf("edit call = %+v, want channel_roles set C123=ops-lead", call.Req)
	}
}

// TestUpdate_FieldAppendRoutesToEdit verifies --field-append routes to a CSV
// append edit.
func TestUpdate_FieldAppendRoutesToEdit(t *testing.T) {
	mc := mockClientWithBead("gd-proj")
	mc.Beads["gd-proj"] = &model.Bead{ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen}
	withMockClient(t, mc)

	resetUpdateFlags(t)
	resetSubFieldFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t); resetSubFieldFlags(t) })

	if err := updateCmd.Flags().Set("field-append", "slack_channel=C999"); err != nil {
		t.Fatalf("set --field-append: %v", err)
	}
	if err := updateCmd.RunE(updateCmd, []string{"gd-proj"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(mc.EditSubFieldCalls) != 1 {
		t.Fatalf("EditBeadSubField called %d times, want 1", len(mc.EditSubFieldCalls))
	}
	call := mc.EditSubFieldCalls[0]
	if call.Field != "slack_channel" || call.Req.Op != "append" || call.Req.Element != "C999" {
		t.Fatalf("edit call = %+v, want slack_channel append C999", call.Req)
	}
}

// TestUpdate_BareFieldStillReplaces is the back-compat guard: bare -f keeps its
// whole-field REPLACE semantics — it drives UpdateBead, not the sub-field edit.
func TestUpdate_BareFieldStillReplaces(t *testing.T) {
	mc := mockClientWithBead("gd-proj")
	mc.Beads["gd-proj"] = &model.Bead{ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen}
	withMockClient(t, mc)

	resetUpdateFlags(t)
	resetSubFieldFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t); resetSubFieldFlags(t) })

	if err := updateCmd.Flags().Set("field", `channel_roles={"C2":"crew"}`); err != nil {
		t.Fatalf("set -f: %v", err)
	}
	if err := updateCmd.RunE(updateCmd, []string{"gd-proj"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("bare -f must drive UpdateBead (replace), got %d calls", len(mc.UpdateBeadCalls))
	}
	if len(mc.EditSubFieldCalls) != 0 {
		t.Errorf("bare -f must NOT route to EditBeadSubField, got %d", len(mc.EditSubFieldCalls))
	}
}

// TestUpdate_MalformedFieldSetAborts verifies a malformed --field-set aborts
// before any write.
func TestUpdate_MalformedFieldSetAborts(t *testing.T) {
	mc := mockClientWithBead("gd-proj")
	mc.Beads["gd-proj"] = &model.Bead{ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen}
	withMockClient(t, mc)

	resetUpdateFlags(t)
	resetSubFieldFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t); resetSubFieldFlags(t) })

	if err := updateCmd.Flags().Set("field-set", "channel_roles_no_dot"); err != nil {
		t.Fatalf("set --field-set: %v", err)
	}
	if err := updateCmd.RunE(updateCmd, []string{"gd-proj"}); err == nil {
		t.Fatal("expected error for malformed --field-set, got nil")
	}
	if len(mc.EditSubFieldCalls) != 0 || len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("no write should occur on malformed spec: edits=%d updates=%d", len(mc.EditSubFieldCalls), len(mc.UpdateBeadCalls))
	}
}

// TestUpdate_FieldSetWithScalarStillUpdates verifies that combining --field-set
// with a scalar change (e.g. --status) fires BOTH UpdateBead and the edit.
func TestUpdate_FieldSetWithScalarStillUpdates(t *testing.T) {
	mc := mockClientWithBead("gd-proj")
	mc.Beads["gd-proj"] = &model.Bead{ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen}
	withMockClient(t, mc)

	resetUpdateFlags(t)
	resetSubFieldFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t); resetSubFieldFlags(t) })

	if err := updateCmd.Flags().Set("status", "open"); err != nil {
		t.Fatalf("set --status: %v", err)
	}
	if err := updateCmd.Flags().Set("field-set", "channel_roles.C1=ops"); err != nil {
		t.Fatalf("set --field-set: %v", err)
	}
	if err := updateCmd.RunE(updateCmd, []string{"gd-proj"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Errorf("scalar change must still drive UpdateBead, got %d", len(mc.UpdateBeadCalls))
	}
	if len(mc.EditSubFieldCalls) != 1 {
		t.Errorf("--field-set must still edit, got %d", len(mc.EditSubFieldCalls))
	}
}
