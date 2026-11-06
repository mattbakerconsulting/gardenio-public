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

func reopenTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ---

// fakeReopenGitLab serves a canned open-MR list and per-MR re-verify fetches.
// verifyState lets a test simulate an MR that merged/closed BETWEEN the list
// snapshot and the re-verify fetch (the race the sweep guards against).
type fakeReopenGitLab struct {
	openMRs     []GitLabMR
	listErr     error
	verifyState map[string]string // mr web url -> state returned by GetMergeRequestByPath
	verifyErr   map[string]error  // mr web url -> error returned by GetMergeRequestByPath
	verifyCalls int
}

func (f *fakeReopenGitLab) ListGroupMergeRequests(_ context.Context, _ int, state string, _ time.Time) ([]GitLabMR, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if state != "opened" {
		return nil, nil
	}
	return f.openMRs, nil
}

func (f *fakeReopenGitLab) GetMergeRequestByPath(_ context.Context, projectPath string, iid int) (*GitLabMR, error) {
	f.verifyCalls++
	// Reconstruct the canonical url the sweep passed in so the test can key on it.
	url := "https://gitlab.com/" + projectPath + "/-/merge_requests/" + itoa(iid)
	if err := f.verifyErr[url]; err != nil {
		return nil, err
	}
	state := "opened"
	if s, ok := f.verifyState[url]; ok {
		state = s
	}
	return &GitLabMR{WebURL: url, State: state, IID: iid}, nil
}

// fakeReopenDaemon is an in-memory mrReopenDaemon: beads are keyed by id, and
// ListBeadsFiltered honors the Statuses + Fields[mr_url] filter the sweep sends.
type fakeReopenDaemon struct {
	beads      []*beadsapi.BeadDetail
	listErr    error
	updateErr  error
	commentErr error
	updates    map[string]string // beadID -> status written
	comments   map[string]string // beadID -> comment text
	// ignoreStatusFilter makes ListBeadsFiltered return a non-closed bead even
	// though the sweep asked for closed only — to exercise maybeReopen's
	// belt-and-suspenders status re-check independently of the daemon filter.
	ignoreStatusFilter bool
}

func newFakeReopenDaemon() *fakeReopenDaemon {
	return &fakeReopenDaemon{
		updates:  map[string]string{},
		comments: map[string]string{},
	}
}

func (f *fakeReopenDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	wantURL := q.Fields["mr_url"]
	wantClosed := len(q.Statuses) == 1 && q.Statuses[0] == "closed"
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if wantURL != "" && b.Fields["mr_url"] != wantURL {
			continue
		}
		if wantClosed && !f.ignoreStatusFilter && b.Status != "closed" {
			continue
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out}, nil
}

func (f *fakeReopenDaemon) UpdateBead(_ context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if req.Status != nil {
		f.updates[id] = *req.Status
	}
	return nil
}

func (f *fakeReopenDaemon) AddComment(_ context.Context, id, _, text string) error {
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments[id] = text
	return nil
}

// --- helpers ---

const reopenTestMRURL = "https://gitlab.com/acme/app/-/merge_requests/6183"

func openMR(url string, iid int) GitLabMR {
	return GitLabMR{WebURL: url, State: "opened", IID: iid, SourceBranch: "fix/x"}
}

func closedIssueBead(id, mrURL string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{"mr_url": mrURL}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{ID: id, Type: "bug", Kind: "issue", Status: "closed", Fields: f}
}

func newReopenSweep(gl mrReopenGitLab, d mrReopenDaemon, dryRun bool) *MRReopenDriftReconciler {
	return NewMRReopenDriftReconciler(MRReopenDriftConfig{
		GitLab: gl, Daemon: d, Logger: reopenTestLogger(),
		GroupID: 42, DryRun: dryRun,
	})
}

// --- tests ---

// TestReopenSweep_ReopensClosedBeadWithOpenMR is the core repair: a closed
// issue bead whose linked MR is still `opened` is re-opened with an explanatory
// comment. This is the exact MR !6183 drift the origin bug describes.
func TestReopenSweep_ReopensClosedBeadWithOpenMR(t *testing.T) {
	gl := &fakeReopenGitLab{openMRs: []GitLabMR{openMR(reopenTestMRURL, 6183)}}
	d := newFakeReopenDaemon()
	d.beads = []*beadsapi.BeadDetail{closedIssueBead("gd-drift0001", reopenTestMRURL, nil)}

	s := newReopenSweep(gl, d, false).scan(context.Background())

	if s.reopened != 1 {
		t.Fatalf("reopened = %d; want 1 (stats: %+v)", s.reopened, s)
	}
	if d.updates["gd-drift0001"] != "open" {
		t.Errorf("bead status written = %q; want open", d.updates["gd-drift0001"])
	}
	if d.comments["gd-drift0001"] == "" {
		t.Error("expected an explanatory re-open comment on the bead")
	}
	if gl.verifyCalls != 1 {
		t.Errorf("re-verify fetches = %d; want exactly 1 before acting", gl.verifyCalls)
	}
}

// TestReopenSweep_HonorsCloseOverride proves a bead deliberately closed with
// `kd close --allow-open-mr` (mr_close_override=true) is left closed — directions
// 1 and 2 composing, not fighting.
func TestReopenSweep_HonorsCloseOverride(t *testing.T) {
	gl := &fakeReopenGitLab{openMRs: []GitLabMR{openMR(reopenTestMRURL, 6183)}}
	d := newFakeReopenDaemon()
	d.beads = []*beadsapi.BeadDetail{
		closedIssueBead("gd-1f842ztfur", reopenTestMRURL, map[string]string{mrCloseOverrideField: "true"}),
	}

	s := newReopenSweep(gl, d, false).scan(context.Background())

	if s.overridden != 1 {
		t.Fatalf("overridden = %d; want 1 (stats: %+v)", s.overridden, s)
	}
	if s.reopened != 0 || len(d.updates) != 0 {
		t.Errorf("deliberately-closed bead was re-opened; updates=%+v", d.updates)
	}
	if gl.verifyCalls != 0 {
		t.Errorf("re-verify called %d times for an overridden bead; want 0 (skip before fetch)", gl.verifyCalls)
	}
}

// TestReopenSweep_SkipsAlreadyOpenBead proves an open/in_progress bead is never
// re-written — only genuinely CLOSED beads drift.
func TestReopenSweep_SkipsAlreadyOpenBead(t *testing.T) {
	gl := &fakeReopenGitLab{openMRs: []GitLabMR{openMR(reopenTestMRURL, 6183)}}
	d := newFakeReopenDaemon()
	d.ignoreStatusFilter = true // force the non-closed bead through the daemon filter
	// Present in the store but in_progress. The sweep's daemon filter asks for
	// closed only; simulate a daemon that returned it anyway to prove the
	// belt-and-suspenders status re-check.
	b := closedIssueBead("gd-live00001", reopenTestMRURL, nil)
	b.Status = "in_progress"
	d.beads = []*beadsapi.BeadDetail{b}

	s := newReopenSweep(gl, d, false).scan(context.Background())

	if s.notClosed != 1 {
		t.Fatalf("not_closed = %d; want 1 (stats: %+v)", s.notClosed, s)
	}
	if s.reopened != 0 {
		t.Errorf("reopened an already-open bead; stats: %+v", s)
	}
}

// TestReopenSweep_NoBeadForMR proves an open MR with no owning bead is a clean
// skip (a human-owned MR, or one not yet linked), never an error.
func TestReopenSweep_NoBeadForMR(t *testing.T) {
	gl := &fakeReopenGitLab{openMRs: []GitLabMR{openMR(reopenTestMRURL, 6183)}}
	d := newFakeReopenDaemon() // no beads

	s := newReopenSweep(gl, d, false).scan(context.Background())

	if s.noBead != 1 || s.reopened != 0 || s.errors != 0 {
		t.Fatalf("stats = %+v; want no_bead=1, reopened=0, errors=0", s)
	}
}

// TestReopenSweep_ReverifyMergedRace proves the fail-safe re-verify: if the MR
// merged BETWEEN the list snapshot and the act, the fresh fetch reports
// state!=opened and the bead is left closed (it should be — the fix landed).
func TestReopenSweep_ReverifyMergedRace(t *testing.T) {
	gl := &fakeReopenGitLab{
		openMRs:     []GitLabMR{openMR(reopenTestMRURL, 6183)},
		verifyState: map[string]string{reopenTestMRURL: "merged"},
	}
	d := newFakeReopenDaemon()
	d.beads = []*beadsapi.BeadDetail{closedIssueBead("gd-hocv4aymvw", reopenTestMRURL, nil)}

	s := newReopenSweep(gl, d, false).scan(context.Background())

	if s.notOpenNow != 1 {
		t.Fatalf("not_open_now = %d; want 1 (stats: %+v)", s.notOpenNow, s)
	}
	if s.reopened != 0 || len(d.updates) != 0 {
		t.Errorf("re-opened a bead whose MR merged on re-verify; updates=%+v", d.updates)
	}
}

// TestReopenSweep_ReverifyFetchError proves a re-verify transport failure never
// re-opens: the sweep acts only on positive evidence, so an error aborts that
// bead (fail-safe, mirroring the close-time guard).
func TestReopenSweep_ReverifyFetchError(t *testing.T) {
	gl := &fakeReopenGitLab{
		openMRs:   []GitLabMR{openMR(reopenTestMRURL, 6183)},
		verifyErr: map[string]error{reopenTestMRURL: errors.New("502 bad gateway")},
	}
	d := newFakeReopenDaemon()
	d.beads = []*beadsapi.BeadDetail{closedIssueBead("gd-err000001", reopenTestMRURL, nil)}

	s := newReopenSweep(gl, d, false).scan(context.Background())

	if s.errors != 1 || s.reopened != 0 {
		t.Fatalf("stats = %+v; want errors=1, reopened=0 (fail-safe on read error)", s)
	}
	if len(d.updates) != 0 {
		t.Errorf("re-opened on a re-verify error; updates=%+v", d.updates)
	}
}

// TestReopenSweep_NeverReopensMRBead proves the canonical type=merge-request
// bead (which ALSO carries mr_url) is never a re-open target — only the issue
// bead it tracks.
func TestReopenSweep_NeverReopensMRBead(t *testing.T) {
	gl := &fakeReopenGitLab{openMRs: []GitLabMR{openMR(reopenTestMRURL, 6183)}}
	d := newFakeReopenDaemon()
	mrBead := &beadsapi.BeadDetail{ID: "gd-7prqnd0vyy", Type: "merge-request", Kind: "issue", Status: "closed", Fields: map[string]string{"mr_url": reopenTestMRURL}}
	d.beads = []*beadsapi.BeadDetail{mrBead}

	s := newReopenSweep(gl, d, false).scan(context.Background())

	if s.reopened != 0 || len(d.updates) != 0 {
		t.Errorf("re-opened the MR bead itself; updates=%+v", d.updates)
	}
	// It is filtered out before the not-closed/override tallies, so it lands in
	// no_bead (no issue bead matched this MR).
	if s.noBead != 1 {
		t.Errorf("stats = %+v; want the MR bead dropped, leaving no_bead=1", s)
	}
}

// TestReopenSweep_DryRunDoesNotWrite proves dry-run logs the intended re-open
// but performs no comment or status write.
func TestReopenSweep_DryRunDoesNotWrite(t *testing.T) {
	gl := &fakeReopenGitLab{openMRs: []GitLabMR{openMR(reopenTestMRURL, 6183)}}
	d := newFakeReopenDaemon()
	d.beads = []*beadsapi.BeadDetail{closedIssueBead("gd-dry000001", reopenTestMRURL, nil)}

	s := newReopenSweep(gl, d, true).scan(context.Background())

	if s.reopened != 1 {
		t.Fatalf("reopened(count) = %d; want 1 even in dry-run (intent tallied)", s.reopened)
	}
	if len(d.updates) != 0 || len(d.comments) != 0 {
		t.Errorf("dry-run wrote to the daemon; updates=%+v comments=%+v", d.updates, d.comments)
	}
}

// TestReopenSweep_ListErrorTallied proves a failed open-MR listing is a tallied
// error, not a panic or a silent success.
func TestReopenSweep_ListErrorTallied(t *testing.T) {
	gl := &fakeReopenGitLab{listErr: errors.New("group MR list 500")}
	d := newFakeReopenDaemon()

	s := newReopenSweep(gl, d, false).scan(context.Background())

	if s.errors != 1 || s.openMRs != 0 {
		t.Fatalf("stats = %+v; want errors=1, open_mrs=0", s)
	}
}

// TestReopenSweep_DisabledWithoutGroupID proves Run is inert with group id 0,
// so a misconfigured bridge never scans.
func TestReopenSweep_DisabledWithoutGroupID(t *testing.T) {
	r := NewMRReopenDriftReconciler(MRReopenDriftConfig{
		GitLab: &fakeReopenGitLab{}, Daemon: newFakeReopenDaemon(),
		Logger: reopenTestLogger(), GroupID: 0,
	})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run with group id 0 returned %v; want nil (disabled no-op)", err)
	}
}
