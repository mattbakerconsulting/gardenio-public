package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// resetUpdateFlags clears the repeatable field flags on the package-global
// updateCmd so accumulated values from a prior test in this binary don't leak
// into the next invocation.
func resetUpdateFlags(t *testing.T) {
	t.Helper()
	for _, name := range []string{"field", "unset-field", "add-label", "remove-label"} {
		if f := updateCmd.Flags().Lookup(name); f != nil {
			_ = f.Value.(interface{ Replace([]string) error }).Replace(nil)
			f.Changed = false
		}
	}
	// Scalar string flags reset to empty + Changed=false so a value set by one
	// test does not bleed into the next (they lack a Replace method).
	for _, name := range []string{"title", "description", "notes"} {
		if f := updateCmd.Flags().Lookup(name); f != nil {
			_ = f.Value.Set("")
			f.Changed = false
		}
	}
	if f := updateCmd.Flags().Lookup("allow-empty"); f != nil {
		_ = f.Value.Set("false")
		f.Changed = false
	}
}

// mockClientWithBead returns a mock client seeded with a single task bead so
// the update command's GetBead (used for field linting) resolves.
func mockClientWithBead(id string) *mockBeadsClient {
	mc := newMockClient()
	mc.Beads[id] = &model.Bead{ID: id, Type: model.TypeTask, Status: model.StatusOpen}
	return mc
}

// TestUpdate_UnsetFieldFlagProducesNullWire drives `kd update --unset-field`
// end to end through the mock client and asserts the request's Fields JSON
// carries an explicit null for the unset key — the wire form the daemon turns
// into a delete (ka-0000007246).
func TestUpdate_UnsetFieldFlagProducesNullWire(t *testing.T) {
	mc := mockClientWithBead("gd-upd1")
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("unset-field", "model"); err != nil {
		t.Fatalf("set --unset-field: %v", err)
	}

	if err := updateCmd.RunE(updateCmd, []string{"gd-upd1"}); err != nil {
		t.Fatalf("updateCmd.RunE: %v", err)
	}

	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times, want 1", len(mc.UpdateBeadCalls))
	}
	raw := mc.UpdateBeadCalls[0].Req.Fields
	if raw == nil {
		t.Fatal("req.Fields is nil, want a patch with {\"model\":null}")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("req.Fields not valid JSON: %s", raw)
	}
	v, ok := m["model"]
	if !ok {
		t.Fatalf("req.Fields missing model key: %s", raw)
	}
	if v != nil {
		t.Errorf("req.Fields[model] = %v, want explicit null (delete intent)", v)
	}
}

// TestUpdate_TaintDenial_AdvisoryTextVerbatim proves a write denied by the
// server's taint gate surfaces the server's exact advisory text through `kd
// update`, never a generic 403 (ADR docs/adr/taints.md §9.4, gd-KT7mKtvYwt).
// update.go wraps the client error with fmt.Errorf("...: %w", err), and %w
// preserves TaintDenialError.Error()'s verbatim text inside the result.
func TestUpdate_TaintDenial_AdvisoryTextVerbatim(t *testing.T) {
	mc := mockClientWithBead("gd-tainted")
	advisory := "Write denied: update on gd-tainted carries taint \"acme-production\" that you do not hold.\n" +
		"Ask team acme-prod-owners for a grant, or retry with --escalate to file a grant request."
	mc.UpdateErr = &client.TaintDenialError{
		StatusCode:      403,
		AdvisoryText:    advisory,
		TaintNames:      []string{"acme-production"},
		TaintOwnerTeams: []string{"acme-prod-owners"},
	}
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("title", "changed"); err != nil {
		t.Fatalf("set --title: %v", err)
	}

	err := updateCmd.RunE(updateCmd, []string{"gd-tainted"})
	if err == nil {
		t.Fatal("expected an error from a taint-denied update")
	}
	if !strings.Contains(err.Error(), advisory) {
		t.Errorf("expected the server's advisory text verbatim, got: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "forbidden") {
		t.Errorf("expected the advisory text, not a generic 403/forbidden message, got: %v", err)
	}
}

// TestUpdate_ConflictingSetAndUnset asserts the CLI rejects a key given to both
// -f and --unset-field, so a contradictory intent never reaches the daemon.
func TestUpdate_ConflictingSetAndUnset(t *testing.T) {
	mc := mockClientWithBead("gd-upd2")
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("field", "model=claude"); err != nil {
		t.Fatalf("set -f: %v", err)
	}
	if err := updateCmd.Flags().Set("unset-field", "model"); err != nil {
		t.Fatalf("set --unset-field: %v", err)
	}

	if err := updateCmd.RunE(updateCmd, []string{"gd-upd2"}); err == nil {
		t.Fatal("expected error for conflicting -f/--unset-field on same key, got nil")
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("UpdateBead should not be called on conflict, got %d calls", len(mc.UpdateBeadCalls))
	}
}

// TestUpdate_AddLabelFlag verifies `kd update <id> --add-label x` routes to
// AddLabel — the inline path added so agents' natural `--label`/`--add-label`
// guess works instead of a bare cobra usage dump (bug ka-0000002558).
func TestUpdate_AddLabelFlag(t *testing.T) {
	mc := mockClientWithBead("gd-lbl1")
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("add-label", "urgent"); err != nil {
		t.Fatalf("set --add-label: %v", err)
	}

	if err := updateCmd.RunE(updateCmd, []string{"gd-lbl1"}); err != nil {
		t.Fatalf("updateCmd.RunE: %v", err)
	}
	if len(mc.AddLabelCalls) != 1 || mc.AddLabelCalls[0].Label != "urgent" || mc.AddLabelCalls[0].BeadID != "gd-lbl1" {
		t.Fatalf("AddLabel calls = %+v, want one {gd-lbl1 urgent}", mc.AddLabelCalls)
	}
	if len(mc.RemoveLabelCalls) != 0 {
		t.Errorf("RemoveLabel should not be called, got %+v", mc.RemoveLabelCalls)
	}
}

// TestUpdate_LabelAlias verifies the `--label` alias normalizes to `--add-label`
// so `kd update <id> --label x` — the exact form agents reach for — adds the
// label instead of erroring with "unknown flag: --label".
func TestUpdate_LabelAlias(t *testing.T) {
	mc := mockClientWithBead("gd-lbl2")
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })

	// Parse through the flag set so the normalizer runs, as it would on a real
	// invocation (Flags().Set bypasses normalization).
	if err := updateCmd.Flags().Parse([]string{"--label", "kind:ced"}); err != nil {
		t.Fatalf("parsing --label alias must not error, got: %v", err)
	}
	if err := updateCmd.RunE(updateCmd, []string{"gd-lbl2"}); err != nil {
		t.Fatalf("updateCmd.RunE: %v", err)
	}
	if len(mc.AddLabelCalls) != 1 || mc.AddLabelCalls[0].Label != "kind:ced" {
		t.Fatalf("AddLabel calls = %+v, want one {gd-lbl2 kind:ced} via --label alias", mc.AddLabelCalls)
	}
}

// TestUpdate_RemoveLabelFlag verifies `kd update <id> --remove-label x` routes
// to RemoveLabel.
func TestUpdate_RemoveLabelFlag(t *testing.T) {
	mc := mockClientWithBead("gd-lbl3")
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("remove-label", "stale"); err != nil {
		t.Fatalf("set --remove-label: %v", err)
	}

	if err := updateCmd.RunE(updateCmd, []string{"gd-lbl3"}); err != nil {
		t.Fatalf("updateCmd.RunE: %v", err)
	}
	if len(mc.RemoveLabelCalls) != 1 || mc.RemoveLabelCalls[0].Label != "stale" || mc.RemoveLabelCalls[0].BeadID != "gd-lbl3" {
		t.Fatalf("RemoveLabel calls = %+v, want one {gd-lbl3 stale}", mc.RemoveLabelCalls)
	}
	if len(mc.AddLabelCalls) != 0 {
		t.Errorf("AddLabel should not be called, got %+v", mc.AddLabelCalls)
	}
}

// TestUpdate_DescriptionAtFileExpands is the end-to-end regression guard for
// ka-0000003715: `kd update <id> -d @file` must send the file's CONTENTS as the
// description — never the literal "@/path" pointer that previously clobbered an
// 18KB plan-of-record body.
func TestUpdate_DescriptionAtFileExpands(t *testing.T) {
	dir := t.TempDir()
	body := "# Real body\n\nThe actual 18KB design doc content.\n"
	path := filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}

	mc := mockClientWithBead("gd-desc1")
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("description", "@"+path); err != nil {
		t.Fatalf("set -d: %v", err)
	}

	if err := updateCmd.RunE(updateCmd, []string{"gd-desc1"}); err != nil {
		t.Fatalf("updateCmd.RunE: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times, want 1", len(mc.UpdateBeadCalls))
	}
	desc := mc.UpdateBeadCalls[0].Req.Description
	if desc == nil {
		t.Fatal("req.Description is nil, want the expanded file body")
	}
	if *desc != body {
		t.Errorf("req.Description = %q, want the file body %q", *desc, body)
	}
}

// TestUpdate_DescriptionMissingAtFileFailsLoud asserts the CLI refuses to write
// when -d points at a non-existent file. Before the fix this silently persisted
// the literal "@/path" string as the description; now it must error and make no
// UpdateBead call, so a stale scratchpad pointer never destroys a body again.
func TestUpdate_DescriptionMissingAtFileFailsLoud(t *testing.T) {
	dir := t.TempDir()
	mc := mockClientWithBead("gd-desc2")
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("description", "@"+filepath.Join(dir, "gone.md")); err != nil {
		t.Fatalf("set -d: %v", err)
	}

	if err := updateCmd.RunE(updateCmd, []string{"gd-desc2"}); err == nil {
		t.Fatal("expected error for -d @missing-file, got nil — data-loss footgun regressed")
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("UpdateBead must not be called when -d @file fails to read, got %d calls", len(mc.UpdateBeadCalls))
	}
}

// TestUpdate_DescriptionAtEscapeLiteral verifies the `@@` escape lets a caller
// intentionally store a description that begins with a single '@' (e.g. an
// "@mention") without triggering file expansion.
func TestUpdate_DescriptionAtEscapeLiteral(t *testing.T) {
	mc := mockClientWithBead("gd-desc3")
	withMockClient(t, mc)

	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("description", "@@channel see the note"); err != nil {
		t.Fatalf("set -d: %v", err)
	}

	if err := updateCmd.RunE(updateCmd, []string{"gd-desc3"}); err != nil {
		t.Fatalf("updateCmd.RunE: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times, want 1", len(mc.UpdateBeadCalls))
	}
	desc := mc.UpdateBeadCalls[0].Req.Description
	if desc == nil || *desc != "@channel see the note" {
		t.Errorf("req.Description = %v, want literal %q", desc, "@channel see the note")
	}
}

// TestUpdateArgs_StrayLabelPositionalIsExplained covers the `--remove-label a b`
// footgun (ka-0000006934): --remove-label binds ONE value per occurrence, so "b"
// arrives as a second positional and cobra's default ExactArgs would report only
// "accepts 1 arg(s), received 2". updateArgs replaces that with a message that
// names the stray label AND prints the corrected repeat/comma-join forms.
func TestUpdateArgs_StrayLabelPositionalIsExplained(t *testing.T) {
	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("remove-label", "a"); err != nil {
		t.Fatalf("set --remove-label: %v", err)
	}

	// `kd update gd-x --remove-label a b` → args = [gd-x, b] (b is the stray).
	err := updateArgs(updateCmd, []string{"gd-x", "b"})
	if err == nil {
		t.Fatal("updateArgs accepted a stray label positional; want an explanatory error")
	}
	msg := err.Error()
	for _, want := range []string{
		"ONE label per flag",
		"--remove-label a --remove-label b", // repeat form reconstructs a AND b
		"--remove-label a,b",                // comma-join form
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\ngot: %s", want, msg)
		}
	}
}

// TestUpdateArgs_StrayPositionalReportsAddLabel confirms the hint names whichever
// label flag the caller actually used, not a hard-coded --remove-label.
func TestUpdateArgs_StrayPositionalReportsAddLabel(t *testing.T) {
	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("add-label", "x"); err != nil {
		t.Fatalf("set --add-label: %v", err)
	}

	err := updateArgs(updateCmd, []string{"gd-x", "y"})
	if err == nil {
		t.Fatal("updateArgs accepted a stray label positional; want an explanatory error")
	}
	if !strings.Contains(err.Error(), "--add-label x --add-label y") {
		t.Errorf("hint did not reference --add-label with reconstructed values\ngot: %s", err.Error())
	}
}

// TestUpdateArgs_ExtraPositionalWithoutLabelFlagStillRejected verifies the guard
// only special-cases the label-flag footgun: a second positional with no label
// flag in play falls through to the ordinary exactly-one-id rejection.
func TestUpdateArgs_ExtraPositionalWithoutLabelFlagStillRejected(t *testing.T) {
	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })

	if err := updateArgs(updateCmd, []string{"gd-x", "gd-y"}); err == nil {
		t.Fatal("updateArgs accepted two bead ids; want ExactArgs rejection")
	}
}

// TestUpdateArgs_SingleIDAccepted is the happy path: exactly one positional,
// with or without label flags, is valid.
func TestUpdateArgs_SingleIDAccepted(t *testing.T) {
	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	if err := updateCmd.Flags().Set("remove-label", "a,b"); err != nil {
		t.Fatalf("set --remove-label: %v", err)
	}
	if err := updateArgs(updateCmd, []string{"gd-x"}); err != nil {
		t.Errorf("updateArgs rejected a valid single-id call: %v", err)
	}
}

// TestUpdateLabelFlagHelpTeachesOnePerFlag pins the help text so the one-label-
// per-flag guidance cannot silently regress out of the flag usage string.
func TestUpdateLabelFlagHelpTeachesOnePerFlag(t *testing.T) {
	for _, flag := range []string{"add-label", "remove-label"} {
		f := updateCmd.Flags().Lookup(flag)
		if f == nil {
			t.Fatalf("--%s flag not registered", flag)
		}
		if !strings.Contains(f.Usage, "ONE per flag") {
			t.Errorf("--%s usage does not teach one-per-flag: %q", flag, f.Usage)
		}
	}
}
