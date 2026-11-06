package main

import (
	"strconv"
	"strings"
	"testing"
)

// setLabelRemoveIfExists sets `kd label remove`'s --if-exists flag for one test,
// capturing and restoring the PRIOR value (rather than assuming the registered
// default) because labelRemoveCmd is package-level and shared — the same reason
// setDepRemoveFlag does it for dep remove.
func setLabelRemoveIfExists(t *testing.T, ifExists bool) {
	t.Helper()
	f := labelRemoveCmd.Flags().Lookup("if-exists")
	if f == nil {
		t.Fatal("no --if-exists flag registered on labelRemoveCmd")
	}
	prev := f.Value.String()
	if err := labelRemoveCmd.Flags().Set("if-exists", strconv.FormatBool(ifExists)); err != nil {
		t.Fatalf("set --if-exists=%v: %v", ifExists, err)
	}
	t.Cleanup(func() { _ = labelRemoveCmd.Flags().Set("if-exists", prev) })
}

// TestLabelRemove_AbsentLabelIsNotSuccess is the headline regression for
// ka-0000000424: the bead carries `frontend`, the caller asks to remove
// `not-a-real-label`, and the old code printed "Removed label(s)" and exited 0
// while nothing was removed. An agent scripting `kd label remove ... ||
// <fallback>` never saw its fallback fire.
func TestLabelRemove_AbsentLabelIsNotSuccess(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	mc.Labels[bead] = []string{"frontend"}
	withMockClient(t, mc)
	setLabelRemoveIfExists(t, false)

	err := labelRemoveCmd.RunE(labelRemoveCmd, []string{bead, "not-a-real-label"})
	if err == nil {
		t.Fatal("removing a label the bead never had returned nil (exit 0) — the no-op reported success")
	}
	if !strings.Contains(err.Error(), "no such label") {
		t.Errorf("error = %q; want it to contain %q", err.Error(), "no such label")
	}
	// The message must name the concrete label that failed to match.
	if !strings.Contains(err.Error(), "not-a-real-label") {
		t.Errorf("error = %q; want the absent label named", err.Error())
	}
	// The real label must survive — nothing was silently destroyed.
	if got := len(mc.Labels[bead]); got != 1 || mc.Labels[bead][0] != "frontend" {
		t.Errorf("labels = %v; want the untouched frontend label to survive", mc.Labels[bead])
	}
}

// TestLabelRemove_RealLabelSucceeds is the positive control: a label the bead
// carries still exits 0, and the success line names the concrete label rather
// than the old unfalsifiable bare "Removed label(s) from X".
func TestLabelRemove_RealLabelSucceeds(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	mc.Labels[bead] = []string{"frontend", "urgent"}
	withMockClient(t, mc)
	setLabelRemoveIfExists(t, false)

	var err error
	out := captureStdout(t, func() {
		err = labelRemoveCmd.RunE(labelRemoveCmd, []string{bead, "urgent"})
	})
	if err != nil {
		t.Fatalf("removing a real label failed: %v", err)
	}
	if !strings.Contains(out, "Removed label(s)") || !strings.Contains(out, "urgent") {
		t.Errorf("stdout = %q; want a 'Removed label(s)' line naming urgent", out)
	}
	if got := len(mc.Labels[bead]); got != 1 || mc.Labels[bead][0] != "frontend" {
		t.Errorf("labels = %v; want only urgent gone", mc.Labels[bead])
	}
}

// TestLabelRemove_PartialMatchStillErrors pins the multi-label contract: with
// one present label and one absent label, the present one IS removed, but the
// command still exits non-zero and names exactly which label was absent. A
// scripted caller must see the failure even though something was removed.
func TestLabelRemove_PartialMatchStillErrors(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	mc.Labels[bead] = []string{"frontend"}
	withMockClient(t, mc)
	setLabelRemoveIfExists(t, false)

	var err error
	out := captureStdout(t, func() {
		err = labelRemoveCmd.RunE(labelRemoveCmd, []string{bead, "frontend", "ghost"})
	})
	if err == nil {
		t.Fatal("a request with an absent label returned nil (exit 0); want a non-zero exit")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q; want the absent label named", err.Error())
	}
	if strings.Contains(err.Error(), "frontend") {
		t.Errorf("error = %q; frontend was removed and must not be listed as absent", err.Error())
	}
	// The present label was still actually removed, and the success line reports it.
	if !strings.Contains(out, "Removed label(s)") || !strings.Contains(out, "frontend") {
		t.Errorf("stdout = %q; want the removed frontend reported", out)
	}
	if got := len(mc.Labels[bead]); got != 0 {
		t.Errorf("labels = %v; want frontend actually gone", mc.Labels[bead])
	}
}

// TestLabelRemove_IfExistsTreatsNoOpAsSuccess pins the scripted/idempotent
// escape hatch: --if-exists wants "already gone" as the end state, so an absent
// label exits 0 while still saying plainly that nothing was removed.
func TestLabelRemove_IfExistsTreatsNoOpAsSuccess(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	mc.Labels[bead] = []string{} // tracked, but empty: nothing matches
	withMockClient(t, mc)
	setLabelRemoveIfExists(t, true)

	var err error
	out := captureStdout(t, func() {
		err = labelRemoveCmd.RunE(labelRemoveCmd, []string{bead, "ghost"})
	})
	if err != nil {
		t.Fatalf("--if-exists must exit 0 on an absent label, got: %v", err)
	}
	if !strings.Contains(out, "Not present") {
		t.Errorf("stdout = %q; want it to state that nothing was removed", out)
	}
}
