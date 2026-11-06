package main

import (
	"strconv"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// setDepRemoveFlags sets `kd dep remove`'s flags for one test. It captures and
// restores each flag's PRIOR value (rather than assuming the registered
// default) for the same reason setDepType does in dep_canonicalize_test.go: the
// command object is package-level and shared, so hard-coding the restore value
// would silently leak a stale default into every later test if the registered
// default ever changes.
func setDepRemoveFlags(t *testing.T, depType string, ifExists bool) {
	t.Helper()
	setDepRemoveFlag(t, "type", depType)
	setDepRemoveFlag(t, "if-exists", strconv.FormatBool(ifExists))
}

func setDepRemoveFlag(t *testing.T, name, value string) {
	t.Helper()
	f := depRemoveCmd.Flags().Lookup(name)
	if f == nil {
		t.Fatalf("no --%s flag registered on depRemoveCmd", name)
	}
	prev := f.Value.String()
	if err := depRemoveCmd.Flags().Set(name, value); err != nil {
		t.Fatalf("set --%s=%s: %v", name, value, err)
	}
	t.Cleanup(func() { _ = depRemoveCmd.Flags().Set(name, prev) })
}

// TestDepRemove_WrongTypeIsNotSuccess is the headline regression for
// ka-0000001966: a real `related` edge exists, the caller fat-fingers
// `--type parent-child`, and the old code printed "Removed dependency" and
// exited 0 while the real edge survived. An agent reparenting on that signal
// ends up with BOTH edges.
func TestDepRemove_WrongTypeIsNotSuccess(t *testing.T) {
	const (
		child  = "ka-0000003755"
		parent = "gd-8bbiy0ub0x"
	)
	mc := newMockClient()
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: parent, Type: model.DepRelated},
	}
	withMockClient(t, mc)
	setDepRemoveFlags(t, string(model.DepParentChild), false)

	err := depRemoveCmd.RunE(depRemoveCmd, []string{child, parent})
	if err == nil {
		t.Fatal("removing a non-matching --type returned nil (exit 0) — the no-op reported success")
	}
	if !strings.Contains(err.Error(), "no such dependency") {
		t.Errorf("error = %q; want it to contain %q", err.Error(), "no such dependency")
	}
	// The message must name the concrete triple that failed to match, so the
	// caller can see the --type they actually passed.
	if !strings.Contains(err.Error(), child) || !strings.Contains(err.Error(), parent) {
		t.Errorf("error = %q; want both bead ids named", err.Error())
	}
	if !strings.Contains(err.Error(), string(model.DepParentChild)) {
		t.Errorf("error = %q; want the attempted type named", err.Error())
	}

	// The real edge must still be there — nothing was silently destroyed.
	if got := len(mc.Deps[child]); got != 1 {
		t.Fatalf("len(deps) = %d; want the untouched related edge to survive", got)
	}
	if d := mc.Deps[child][0]; d.Type != model.DepRelated || d.DependsOnID != parent {
		t.Errorf("surviving edge = %+v; want the original related edge", d)
	}
}

// TestDepRemove_NonexistentTargetIsNotSuccess covers the second half of the
// repro: the depends-on bead does not exist at all, so no triple can match.
func TestDepRemove_NonexistentTargetIsNotSuccess(t *testing.T) {
	const child = "ka-0000003755"
	mc := newMockClient()
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: "gd-votqkhsrpo", Type: model.DepRelated},
	}
	withMockClient(t, mc)
	setDepRemoveFlags(t, string(model.DepRelated), false)

	err := depRemoveCmd.RunE(depRemoveCmd, []string{child, "gd-DOESNOTEXIST0"})
	if err == nil {
		t.Fatal("removing an edge to a nonexistent bead returned nil (exit 0)")
	}
	if !strings.Contains(err.Error(), "no such dependency") {
		t.Errorf("error = %q; want it to contain %q", err.Error(), "no such dependency")
	}
	if got := len(mc.Deps[child]); got != 1 {
		t.Errorf("len(deps) = %d; want the unrelated real edge untouched", got)
	}
}

// TestDepRemove_RealEdgeSucceeds is the positive control: a matching triple
// still exits 0, and the success line now names the concrete edge rather than
// the old unfalsifiable bare "Removed dependency".
func TestDepRemove_RealEdgeSucceeds(t *testing.T) {
	const (
		child  = "ka-0000003755"
		parent = "gd-8bbiy0ub0x"
	)
	mc := newMockClient()
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: parent, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	setDepRemoveFlags(t, string(model.DepParentChild), false)

	var out string
	var err error
	out = captureStdout(t, func() {
		err = depRemoveCmd.RunE(depRemoveCmd, []string{child, parent})
	})
	if err != nil {
		t.Fatalf("removing a real edge failed: %v", err)
	}
	if !strings.Contains(out, "Removed dependency") {
		t.Errorf("stdout = %q; want a 'Removed dependency' line", out)
	}
	// Naming the edge is what makes the success line falsifiable.
	if !strings.Contains(out, child) || !strings.Contains(out, parent) ||
		!strings.Contains(out, string(model.DepParentChild)) {
		t.Errorf("stdout = %q; want the concrete edge named", out)
	}
	if got := len(mc.Deps[child]); got != 0 {
		t.Errorf("len(deps) = %d; want the edge actually gone", got)
	}
}

// TestDepRemove_IfExistsTreatsNoOpAsSuccess pins the scripted/idempotent escape
// hatch: --if-exists wants "already gone" as the end state, so a 0-match exits
// 0 while still saying plainly that nothing was removed.
func TestDepRemove_IfExistsTreatsNoOpAsSuccess(t *testing.T) {
	const child = "ka-0000003755"
	mc := newMockClient()
	mc.Deps[child] = []*model.Dependency{} // tracked, but empty: nothing matches
	withMockClient(t, mc)
	setDepRemoveFlags(t, string(model.DepBlocks), true)

	var out string
	var err error
	out = captureStdout(t, func() {
		err = depRemoveCmd.RunE(depRemoveCmd, []string{child, "gd-8bbiy0ub0x"})
	})
	if err != nil {
		t.Fatalf("--if-exists must exit 0 on an absent edge, got: %v", err)
	}
	if !strings.Contains(out, "No such dependency") {
		t.Errorf("stdout = %q; want it to state that nothing was removed", out)
	}
}

// TestDepRemove_ReportsSuccessWhenClientSaysRemoved checks only the CLI half of
// the version-skew story: GIVEN the client reports removed=true, the command
// must print the success line and exit 0.
//
// It deliberately does NOT prove the skew handling itself — that logic lives in
// the transport clients (grpc.go / http.go, `if resp.Removed == nil` →
// assume-success) which mockBeadsClient never runs. The real coverage for that
// is TestGRPCClient round-trip in internal/client/grpc_server_test.go (fake
// omits the field, wants true) and TestHTTPClient_RemoveDependency in
// http_relations_test.go (bodiless 204, wants true). Kept here so a future
// change to this RunE can't start reporting a successful removal as a no-op.
func TestDepRemove_ReportsSuccessWhenClientSaysRemoved(t *testing.T) {
	const (
		child  = "ka-0000003755"
		parent = "gd-8bbiy0ub0x"
	)
	mc := newMockClient()
	// Seed the edge so the mock reports removed=true. (The mock derives its
	// answer from m.Deps rather than returning a canned value, so "the client
	// says removed" has to be set up as a real matching edge.)
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: parent, Type: model.DepBlocks},
	}
	withMockClient(t, mc)
	setDepRemoveFlags(t, string(model.DepBlocks), false)

	var err error
	out := captureStdout(t, func() {
		err = depRemoveCmd.RunE(depRemoveCmd, []string{child, parent})
	})
	if err != nil {
		t.Fatalf("a client-reported removal must not be printed as a no-op: %v", err)
	}
	if !strings.Contains(out, "Removed dependency") {
		t.Errorf("stdout = %q; want the success line", out)
	}
}

// TestFormatDepEdge pins the rendered orientation: `child -type-> parent`, the
// stored direction `kd dep list` reports, so a user can compare the message
// against listed edges without re-deriving which end is which. The empty-type
// row pins the natural degradation to `child --> parent` (the wire form that
// removes an edge regardless of type).
func TestFormatDepEdge(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		beadID, dependsOnID, depType string
		want                         string
	}{
		{"typed", "ka-0000003755", "gd-8bbiy0ub0x", "parent-child", "ka-0000003755 -parent-child-> gd-8bbiy0ub0x"},
		{"empty type", "ka-0000003755", "gd-8bbiy0ub0x", "", "ka-0000003755 --> gd-8bbiy0ub0x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatDepEdge(tc.beadID, tc.dependsOnID, tc.depType); got != tc.want {
				t.Errorf("formatDepEdge() = %q; want %q", got, tc.want)
			}
		})
	}
}
