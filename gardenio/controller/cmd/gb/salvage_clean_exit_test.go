package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// bug ka-0000001049 — false "Killed mid-work (involuntary_termination)" salvage
// notices posted on the parent epic after a CLEAN `gb stop`.
//
// Two independent fix layers are covered here:
//  1. runSalvage skips the WHOLE path when the agent exited voluntarily (its
//     bead is already closed/done, or parked open at stopping) — the strongest
//     fix, a clean-exit-vs-crash discriminator independent of WIP detection.
//  2. isSalvageablePrimary excludes the agent-infra path set (.claude/ .gb/
//     .mcp.json .state/ repos/) from primary WIP detection, so pristine infra
//     state alone never counts as salvageable primary WIP even if layer 1 is
//     ever bypassed (belt-and-braces).

// cleanExitDaemon is a minimal daemon serving GET /v1/beads/<agentBead> with a
// caller-chosen status + agent_state, and recording any comment POST so a test
// can assert the false "Killed mid-work" notice is NOT posted. Any other request
// (an S3-less salvage never hits the daemon beyond the guard read) is a 200 {}.
type cleanExitDaemon struct {
	agentBeadID string
	status      string
	agentState  string
	commentPost atomic.Int64
}

func (d *cleanExitDaemon) wire(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+d.agentBeadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     d.agentBeadID,
				"type":   "agent",
				"status": d.status,
				"fields": map[string]any{"agent_state": d.agentState},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+d.agentBeadID+"/comments":
			d.commentPost.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			// Worked-task comment / list / anything else — count comment POSTs so
			// a stray "Killed mid-work" notice on ANY bead is caught.
			if r.Method == http.MethodPost && filepath.Base(r.URL.Path) == "comments" {
				d.commentPost.Add(1)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

// TestRunSalvage_CleanStopSkipsSalvage is the direct ka-0000001049 regression:
// after a successful `gb stop` the agent bead is already status=closed,
// agent_state=done when the preStop hook fires `gb salvage`. Even with a
// genuinely dirty worktree, salvage must upload NOTHING and post NO comment —
// the false "Killed mid-work" notice must never appear.
func TestRunSalvage_CleanStopSkipsSalvage(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work) // real dirt — the guard must fire regardless

	salvageTestEnv(t)
	neutralizeSlackEnv(t)
	mem, _ := newMemS3(t)

	d := &cleanExitDaemon{agentBeadID: "gd-agent1", status: "closed", agentState: "done"}
	d.wire(t)

	if err := runSalvage(context.Background(), work, "involuntary_termination", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	if got := mem.keys(); len(got) != 0 {
		t.Errorf("clean-stop teardown must upload nothing to S3, got keys=%v", got)
	}
	if n := d.commentPost.Load(); n != 0 {
		t.Errorf("clean-stop teardown must post NO salvage comment (the false 'Killed mid-work' notice), got %d", n)
	}
}

// TestRunSalvage_SuspendSkipsSalvage covers the freeze/thaw suspend path: the
// bead is parked OPEN at agent_state=stopping and the controller checkpoints +
// tears the pod down. That teardown fires the preStop hook too, but the agent is
// resumable by design — nothing is lost, so salvage must be a no-op.
func TestRunSalvage_SuspendSkipsSalvage(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	salvageTestEnv(t)
	neutralizeSlackEnv(t)
	mem, _ := newMemS3(t)

	d := &cleanExitDaemon{agentBeadID: "gd-agent1", status: "open", agentState: "stopping"}
	d.wire(t)

	if err := runSalvage(context.Background(), work, "involuntary_termination", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	if got := mem.keys(); len(got) != 0 {
		t.Errorf("suspend teardown must upload nothing, got keys=%v", got)
	}
	if n := d.commentPost.Load(); n != 0 {
		t.Errorf("suspend teardown must post no salvage comment, got %d", n)
	}
}

// TestRunSalvage_InvoluntaryKillStillSalvages is the fail-OPEN guard for the
// clean-exit fix: a genuine involuntary kill leaves the bead OPEN and
// agent_state=working when the preStop hook fires (the involuntary close happens
// later, in the entrypoint restart loop). Such a teardown MUST still salvage —
// the fix must not suppress real WIP rescue.
func TestRunSalvage_InvoluntaryKillStillSalvages(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	salvageTestEnv(t)
	neutralizeSlackEnv(t)
	mem, _ := newMemS3(t)

	d := &cleanExitDaemon{agentBeadID: "gd-agent1", status: "open", agentState: "working"}
	d.wire(t)

	if err := runSalvage(context.Background(), work, "involuntary_termination", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	if _, ok := mem.get("coop/sessions/gd-agent1/gd-agent1/salvage/work.bundle"); !ok {
		t.Fatalf("involuntary kill must still salvage the dirty worktree; keys=%v", mem.keys())
	}
}

// TestRunSalvage_NoDaemonFailsOpen: with no daemon in scope the clean-exit guard
// cannot prove a voluntary exit, so it fails OPEN and salvage still runs. (This
// is also the shape the existing daemon-less salvage tests rely on.)
func TestRunSalvage_NoDaemonFailsOpen(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	salvageTestEnv(t)
	neutralizeSlackEnv(t)
	mem, _ := newMemS3(t)

	old := daemon
	daemon = nil
	t.Cleanup(func() { daemon = old })

	if err := runSalvage(context.Background(), work, "involuntary_termination", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	if _, ok := mem.get("coop/sessions/gd-agent1/gd-agent1/salvage/work.bundle"); !ok {
		t.Fatalf("no-daemon teardown must fail open and salvage; keys=%v", mem.keys())
	}
}

// ── belt-and-braces: infra-path WIP exclusion ────────────────────────────────

// writeInfraOnlyWorkspace builds a gardener-shaped workspace: a git repo with NO
// commits (unborn HEAD, as a gardener checkout is) whose ONLY untracked content
// is the five agent-infra paths — exactly the shape that made every clean
// gardener stop salvage a pristine 475M tree.
func writeInfraOnlyWorkspace(t *testing.T) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "gardener")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, work, "init", "-q", "-b", "master")
	mustWrite(t, filepath.Join(work, ".mcp.json"), "{}\n")
	for _, dir := range []string{".claude", ".gb", ".state", filepath.Join("repos", "refclone")} {
		if err := os.MkdirAll(filepath.Join(work, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(work, dir, "f"), "infra\n")
	}
	return work
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIsSalvageablePrimary_InfraOnlyNotSalvageable: a workspace whose only dirt
// is the agent-infra path set is NOT salvageable primary WIP.
func TestIsSalvageablePrimary_InfraOnlyNotSalvageable(t *testing.T) {
	work := writeInfraOnlyWorkspace(t)
	if isSalvageablePrimary(context.Background(), work) {
		t.Error("infra-only workspace must not be salvageable primary WIP (bug ka-0000001049)")
	}
}

// TestIsSalvageablePrimary_RealWIPIsSalvageable: the same infra-laden workspace
// becomes salvageable the moment a real (non-infra) untracked file appears.
func TestIsSalvageablePrimary_RealWIPIsSalvageable(t *testing.T) {
	work := writeInfraOnlyWorkspace(t)
	mustWrite(t, filepath.Join(work, "notes.md"), "real work product\n")
	if !isSalvageablePrimary(context.Background(), work) {
		t.Error("a real untracked file outside the infra set must be salvageable")
	}
}

// TestIsSalvageablePrimary_TrackedEditIsSalvageable: a tracked-file modification
// is always real WIP, even when everything else is infra.
func TestIsSalvageablePrimary_TrackedEditIsSalvageable(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root) // clone on main with a tracked base.txt
	// Add the infra paths (untracked) AND edit a tracked file.
	mustWrite(t, filepath.Join(work, ".mcp.json"), "{}\n")
	if err := os.MkdirAll(filepath.Join(work, ".state"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(work, ".state", "f"), "infra\n")
	svAppend(t, filepath.Join(work, "base.txt"), "tracked-wip\n")
	if !isSalvageablePrimary(context.Background(), work) {
		t.Error("a tracked-file edit must be salvageable regardless of infra dirt")
	}
}

// TestRunSalvage_InfraOnlyPrimaryNoUpload is the end-to-end belt-and-braces
// assertion, deliberately with NO daemon so the clean-exit guard fails open and
// only the infra-path filter can stop the upload: an infra-only primary checkout
// uploads nothing.
func TestRunSalvage_InfraOnlyPrimaryNoUpload(t *testing.T) {
	work := writeInfraOnlyWorkspace(t)

	salvageTestEnv(t)
	neutralizeSlackEnv(t)
	mem, _ := newMemS3(t)

	old := daemon
	daemon = nil // force fail-open on the clean-exit guard; isolate the filter
	t.Cleanup(func() { daemon = old })

	if err := runSalvage(context.Background(), work, "involuntary_termination", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	if got := mem.keys(); len(got) != 0 {
		t.Errorf("infra-only primary must upload nothing, got keys=%v", got)
	}
}

// TestSalvagePorcelainLineIsRealWIP unit-tests the porcelain classifier: tracked
// changes always count; untracked infra paths do not; untracked non-infra paths
// do.
func TestSalvagePorcelainLineIsRealWIP(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{" M base.txt", true},           // tracked modification
		{"M  staged.txt", true},         // staged modification
		{"A  added.txt", true},          // staged add
		{"D  gone.txt", true},           // tracked delete
		{"R  old.txt -> new.txt", true}, // rename
		{"?? notes.md", true},           // untracked non-infra file
		{"?? src/", true},               // untracked non-infra dir
		{"?? .claude/", false},          // untracked infra dir
		{"?? .gb/", false},
		{"?? .mcp.json", false},
		{"?? .state/claude/x", false},
		{"?? repos/", false},
		{"?? repos/refclone/wip.txt", false}, // deep infra path — head is repos
	}
	for _, c := range cases {
		if got := salvagePorcelainLineIsRealWIP(c.line); got != c.want {
			t.Errorf("salvagePorcelainLineIsRealWIP(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestSalvagePathIsInfra unit-tests the path head classifier, including a
// git-quoted path and a trailing slash.
func TestSalvagePathIsInfra(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".claude", true},
		{".claude/settings.json", true},
		{"repos/", true},
		{"repos/refclone/deep/file", true},
		{".state", true},
		{".mcp.json", true},
		{`".gb/system-prompt.md"`, true}, // git-quoted
		{"notes.md", false},
		{"src/main.go", false},
		{".claudeish", false}, // not an exact head match
	}
	for _, c := range cases {
		if got := salvagePathIsInfra(c.path); got != c.want {
			t.Errorf("salvagePathIsInfra(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
