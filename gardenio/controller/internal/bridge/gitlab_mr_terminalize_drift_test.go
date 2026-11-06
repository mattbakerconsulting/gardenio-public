package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func terminalizeTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ---

// fakeTerminalizeGitLab serves per-MR re-verify fetches keyed on the canonical
// mr_url the sweep reconstructs from (projectPath, iid).
type fakeTerminalizeGitLab struct {
	state map[string]string // mr web url -> state returned by GetMergeRequestByPath
	err   map[string]error  // mr web url -> error returned by GetMergeRequestByPath
	calls int
}

func (f *fakeTerminalizeGitLab) GetMergeRequestByPath(_ context.Context, projectPath string, iid int) (*GitLabMR, error) {
	f.calls++
	url := "https://gitlab.com/" + projectPath + "/-/merge_requests/" + itoa(iid)
	if err := f.err[url]; err != nil {
		return nil, err
	}
	state := "opened"
	if s, ok := f.state[url]; ok {
		state = s
	}
	return &GitLabMR{WebURL: url, State: state, IID: iid}, nil
}

// fakeTerminalizeDaemon is an in-memory mrTerminalizeDaemon. ListBeadsFiltered
// honors the Fields[mr_state] filter and the MaxPerScan limit; the candidate
// ordering is whatever the test seeds (the daemon owns real sort ordering).
type fakeTerminalizeDaemon struct {
	beads       []*beadsapi.BeadDetail
	listErr     error
	updateErr   error
	closeErr    error
	fieldWrites map[string]map[string]string // beadID -> fields stamped via UpdateBeadFields
	closeWrites map[string]map[string]string // beadID -> fields stamped via CloseBead
}

func newFakeTerminalizeDaemon() *fakeTerminalizeDaemon {
	return &fakeTerminalizeDaemon{
		fieldWrites: map[string]map[string]string{},
		closeWrites: map[string]map[string]string{},
	}
}

func (f *fakeTerminalizeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	wantState := q.Fields["mr_state"]
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if wantState != "" && b.Fields["mr_state"] != wantState {
			continue
		}
		out = append(out, b)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeTerminalizeDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.fieldWrites[id] = fields
	return nil
}

func (f *fakeTerminalizeDaemon) CloseBead(_ context.Context, id string, fields map[string]string) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closeWrites[id] = fields
	return nil
}

// --- helpers ---

const terminalizeTestMRURL = "https://gitlab.com/acme/app/-/merge_requests/7505"

// openStateMRBead builds a type=merge-request bead still carrying
// mr_state=opened, at the given lifecycle status.
func openStateMRBead(id, mrURL, status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "merge-request",
		Kind:   "issue",
		Status: status,
		Fields: map[string]string{"mr_url": mrURL, "mr_state": "opened"},
	}
}

func newTerminalizeSweep(gl mrTerminalizeGitLab, d mrTerminalizeDaemon, dryRun bool) *MRTerminalizeDriftReconciler {
	return NewMRTerminalizeDriftReconciler(MRTerminalizeDriftConfig{
		GitLab: gl, Daemon: d, Logger: terminalizeTestLogger(),
		DryRun: dryRun,
	})
}

// --- tests ---

// TestTerminalizeSweep_ClosesMergedMRBead is the core repair: an OPEN
// mr_state=opened bead whose MR has actually merged is closed with terminal
// mr_* fields stamped. This is the exact historical drift ka-0000001293 measured.
func TestTerminalizeSweep_ClosesMergedMRBead(t *testing.T) {
	gl := &fakeTerminalizeGitLab{state: map[string]string{terminalizeTestMRURL: "merged"}}
	d := newFakeTerminalizeDaemon()
	d.beads = []*beadsapi.BeadDetail{openStateMRBead("gd-drift0001", terminalizeTestMRURL, "open")}

	s := newTerminalizeSweep(gl, d, false).scan(context.Background())

	if s.terminalized != 1 || s.closed != 1 {
		t.Fatalf("stats = %+v; want terminalized=1, closed=1", s)
	}
	got := d.closeWrites["gd-drift0001"]
	if got["mr_state"] != "merged" || got["mr_outcome"] != "merged" || got["mr_merged"] != "true" {
		t.Errorf("close fields = %+v; want merged/merged/true", got)
	}
	if gl.calls != 1 {
		t.Errorf("re-verify fetches = %d; want exactly 1 before acting", gl.calls)
	}
}

// TestTerminalizeSweep_StampsClosedBeadWithoutReClosing proves a bead that is
// ALREADY status=closed but whose mr_state never terminalized gets its mr_*
// fields repaired via UpdateBeadFields (not a second close).
func TestTerminalizeSweep_StampsClosedBeadWithoutReClosing(t *testing.T) {
	gl := &fakeTerminalizeGitLab{state: map[string]string{terminalizeTestMRURL: "closed"}}
	d := newFakeTerminalizeDaemon()
	d.beads = []*beadsapi.BeadDetail{openStateMRBead("gd-stale0001", terminalizeTestMRURL, "closed")}

	s := newTerminalizeSweep(gl, d, false).scan(context.Background())

	if s.terminalized != 1 || s.closed != 0 {
		t.Fatalf("stats = %+v; want terminalized=1, closed=0 (field-only repair)", s)
	}
	if len(d.closeWrites) != 0 {
		t.Errorf("re-closed an already-closed bead; closeWrites=%+v", d.closeWrites)
	}
	got := d.fieldWrites["gd-stale0001"]
	if got["mr_state"] != "closed" || got["mr_outcome"] != "closed" || got["mr_merged"] != "false" {
		t.Errorf("stamped fields = %+v; want closed/closed/false", got)
	}
}

// TestTerminalizeSweep_LeavesStillOpenBeadAlone proves the fail-safe: if the MR
// is genuinely still `opened` on re-verify, the bead is NOT terminalized (the
// candidate mr_state is untrusted; only positive terminal state acts).
func TestTerminalizeSweep_LeavesStillOpenBeadAlone(t *testing.T) {
	gl := &fakeTerminalizeGitLab{state: map[string]string{terminalizeTestMRURL: "opened"}}
	d := newFakeTerminalizeDaemon()
	d.beads = []*beadsapi.BeadDetail{openStateMRBead("gd-live0001", terminalizeTestMRURL, "open")}

	s := newTerminalizeSweep(gl, d, false).scan(context.Background())

	if s.stillOpen != 1 || s.terminalized != 0 {
		t.Fatalf("stats = %+v; want still_open=1, terminalized=0", s)
	}
	if len(d.closeWrites) != 0 || len(d.fieldWrites) != 0 {
		t.Errorf("wrote to a still-open bead; close=%+v fields=%+v", d.closeWrites, d.fieldWrites)
	}
}

// TestTerminalizeSweep_ReverifyFetchErrorFailsSafe proves a re-verify transport
// failure never terminalizes — the sweep acts only on positive evidence.
func TestTerminalizeSweep_ReverifyFetchErrorFailsSafe(t *testing.T) {
	gl := &fakeTerminalizeGitLab{err: map[string]error{terminalizeTestMRURL: errors.New("502 bad gateway")}}
	d := newFakeTerminalizeDaemon()
	d.beads = []*beadsapi.BeadDetail{openStateMRBead("gd-err00001", terminalizeTestMRURL, "open")}

	s := newTerminalizeSweep(gl, d, false).scan(context.Background())

	if s.gitlabErrors != 1 || s.terminalized != 0 {
		t.Fatalf("stats = %+v; want gitlab_errors=1, terminalized=0 (fail-safe)", s)
	}
	if len(d.closeWrites) != 0 || len(d.fieldWrites) != 0 {
		t.Errorf("terminalized on a re-verify error; close=%+v fields=%+v", d.closeWrites, d.fieldWrites)
	}
}

// TestTerminalizeSweep_SkipsUnparseableURL proves a bead with no parseable
// mr_url is a clean skip — never a GitLab fetch, never a write.
func TestTerminalizeSweep_SkipsUnparseableURL(t *testing.T) {
	gl := &fakeTerminalizeGitLab{}
	d := newFakeTerminalizeDaemon()
	b := openStateMRBead("gd-noref0001", "not-a-url", "open")
	d.beads = []*beadsapi.BeadDetail{b}

	s := newTerminalizeSweep(gl, d, false).scan(context.Background())

	if s.skippedNoRef != 1 || s.terminalized != 0 {
		t.Fatalf("stats = %+v; want skipped_no_ref=1, terminalized=0", s)
	}
	if gl.calls != 0 {
		t.Errorf("fetched GitLab for an unparseable url; calls=%d", gl.calls)
	}
}

// TestTerminalizeSweep_DryRunDoesNotWrite proves dry-run tallies the intent but
// performs no daemon write.
func TestTerminalizeSweep_DryRunDoesNotWrite(t *testing.T) {
	gl := &fakeTerminalizeGitLab{state: map[string]string{terminalizeTestMRURL: "merged"}}
	d := newFakeTerminalizeDaemon()
	d.beads = []*beadsapi.BeadDetail{openStateMRBead("gd-dry00001", terminalizeTestMRURL, "open")}

	s := newTerminalizeSweep(gl, d, true).scan(context.Background())

	if s.terminalized != 1 || s.closed != 1 {
		t.Fatalf("stats = %+v; want terminalized=1, closed=1 even in dry-run (intent tallied)", s)
	}
	if len(d.closeWrites) != 0 || len(d.fieldWrites) != 0 {
		t.Errorf("dry-run wrote to the daemon; close=%+v fields=%+v", d.closeWrites, d.fieldWrites)
	}
}

// TestTerminalizeSweep_ListErrorTallied proves a failed candidate listing is a
// tallied error, not a panic or a silent clean pass.
func TestTerminalizeSweep_ListErrorTallied(t *testing.T) {
	gl := &fakeTerminalizeGitLab{}
	d := newFakeTerminalizeDaemon()
	d.listErr = errors.New("candidate list 500")

	s := newTerminalizeSweep(gl, d, false).scan(context.Background())

	if s.gitlabErrors != 1 || s.candidates != 0 {
		t.Fatalf("stats = %+v; want gitlab_errors=1, candidates=0", s)
	}
}

// TestTerminalizeSweep_RespectsMaxPerScan proves the fan-out cap: with more
// candidates than MaxPerScan, only MaxPerScan are re-fetched this pass so the
// sweep can never issue an unbounded burst of GitLab round-trips.
func TestTerminalizeSweep_RespectsMaxPerScan(t *testing.T) {
	gl := &fakeTerminalizeGitLab{state: map[string]string{}}
	d := newFakeTerminalizeDaemon()
	for i := 0; i < 5; i++ {
		url := "https://gitlab.com/acme/app/-/merge_requests/" + itoa(1000+i)
		gl.state[url] = "merged"
		d.beads = append(d.beads, openStateMRBead("gd-cap"+itoa(i), url, "open"))
	}
	r := NewMRTerminalizeDriftReconciler(MRTerminalizeDriftConfig{
		GitLab: gl, Daemon: d, Logger: terminalizeTestLogger(), MaxPerScan: 3,
	})

	s := r.scan(context.Background())

	if s.candidates != 3 {
		t.Fatalf("candidates = %d; want 3 (capped by MaxPerScan)", s.candidates)
	}
	if gl.calls != 3 {
		t.Errorf("GitLab fetches = %d; want 3 (one per capped candidate)", gl.calls)
	}
}

// TestTerminalizeSweep_Defaults proves the constructor applies sane defaults for
// a zero-value config.
func TestTerminalizeSweep_Defaults(t *testing.T) {
	r := NewMRTerminalizeDriftReconciler(MRTerminalizeDriftConfig{
		GitLab: &fakeTerminalizeGitLab{}, Daemon: newFakeTerminalizeDaemon(),
		Logger: terminalizeTestLogger(),
	})
	if r.interval != time.Hour {
		t.Errorf("interval = %v; want 1h default", r.interval)
	}
	if r.staleAge != 7*24*time.Hour {
		t.Errorf("staleAge = %v; want 7d default", r.staleAge)
	}
	if r.maxPerScan != 200 {
		t.Errorf("maxPerScan = %d; want 200 default", r.maxPerScan)
	}
}
