package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func backlinkTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeMRLister returns canned MR batches keyed by state.
type fakeMRLister struct {
	byState map[string][]GitLabMR
	err     error
	calls   []string // states requested, in order
}

func (f *fakeMRLister) ListGroupMergeRequests(_ context.Context, _ int, state string, _ time.Time) ([]GitLabMR, error) {
	f.calls = append(f.calls, state)
	if f.err != nil {
		return nil, f.err
	}
	return f.byState[state], nil
}

// fakeBacklinkDaemon is an in-memory mrBacklinkDaemon for the reconciler tests.
type fakeBacklinkDaemon struct {
	beads      map[string]*beadsapi.BeadDetail // by ID, for GetBead
	byLabel    map[string][]*beadsapi.BeadDetail
	mrByURL    map[string]*beadsapi.BeadDetail
	updates    map[string]map[string]string // beadID -> fields set
	created    []beadsapi.CreateBeadRequest
	updateErr  error
	createErr  error
	mrByURLErr error
	listErr    error
}

func newFakeBacklinkDaemon() *fakeBacklinkDaemon {
	return &fakeBacklinkDaemon{
		beads:   map[string]*beadsapi.BeadDetail{},
		byLabel: map[string][]*beadsapi.BeadDetail{},
		mrByURL: map[string]*beadsapi.BeadDetail{},
		updates: map[string]map[string]string{},
	}
}

func (f *fakeBacklinkDaemon) MRBeadByURL(_ context.Context, url string) (*beadsapi.BeadDetail, error) {
	if f.mrByURLErr != nil {
		return nil, f.mrByURLErr
	}
	return f.mrByURL[url], nil
}

func (f *fakeBacklinkDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*beadsapi.BeadDetail
	for _, l := range q.Labels {
		out = append(out, f.byLabel[l]...)
	}
	return &beadsapi.ListBeadsResult{Beads: out}, nil
}

func (f *fakeBacklinkDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	b, ok := f.beads[id]
	if !ok {
		return nil, nil
	}
	return b, nil
}

func (f *fakeBacklinkDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if f.updates[id] == nil {
		f.updates[id] = map[string]string{}
	}
	maps.Copy(f.updates[id], fields)
	return nil
}

func (f *fakeBacklinkDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, req)
	return "gd-unlinked-1", nil
}

func newReconciler(gl mrLister, d mrBacklinkDaemon) *MRBacklinkReconciler {
	return NewMRBacklinkReconciler(MRBacklinkConfig{
		GitLab:  gl,
		Daemon:  d,
		Logger:  backlinkTestLogger(),
		GroupID: 42,
	})
}

func actionableBead(id, typ string, updated time.Time, labels ...string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Kind: "issue", Type: typ, Labels: labels,
		Fields: map[string]string{}, UpdatedAt: updated,
	}
}

const backlinkMRURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/321"

// JIRA key in the branch resolves to the single actionable bead carrying the
// jira:<key> label and sets mr_url on it.
func TestBacklink_JiraKeyInBranch(t *testing.T) {
	d := newFakeBacklinkDaemon()
	task := actionableBead("gd-task1", "bug", time.Now(), "jira:PE-7083")
	d.byLabel["jira:PE-7083"] = []*beadsapi.BeadDetail{task}

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "fix/PE-7083-eCRF-save", Title: "Fix eCRF save"}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if got := d.updates["gd-task1"]["mr_url"]; got != backlinkMRURL {
		t.Fatalf("expected mr_url set on gd-task1, got %q (updates=%v)", got, d.updates)
	}
	if s.linked != 1 {
		t.Fatalf("expected linked=1, got %+v", s)
	}
}

// Explicit gd-ref in the branch resolves directly via GetBead and wins.
func TestBacklink_ExplicitKDRef(t *testing.T) {
	d := newFakeBacklinkDaemon()
	d.beads["gd-AbCdEf12"] = actionableBead("gd-AbCdEf12", "task", time.Now())

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"merged": {{WebURL: backlinkMRURL, SourceBranch: "feat/gd-AbCdEf12-thing"}},
	}}
	r := newReconciler(gl, d)
	r.scan(context.Background())

	if d.updates["gd-AbCdEf12"]["mr_url"] != backlinkMRURL {
		t.Fatalf("expected kd ref to be linked, got updates=%v", d.updates)
	}
}

// Regression for bug ka-0000002276 (real MR !6258 shape): the branch/title name
// the SUBJECT bead, while the description also cross-references two OTHER beads
// that sort alphabetically BEFORE it. The reconciler must link ONLY the subject
// bead — not the alphabetically-first description cross-ref. Before the fix the
// flat sort made ka-0000007080 (a description-only "related:" ref) win over the
// real subject ka-0000004189, clobbering that other bead's own merged mr_url.
func TestBacklink_SubjectBeatsAlphabeticallyEarlierDescriptionRefs(t *testing.T) {
	d := newFakeBacklinkDaemon()
	// Subject bead named on the branch/title. Its ID sorts AFTER the two
	// description cross-refs (lowercase 'h' > uppercase 'G'/'R' under byte sort),
	// so an alphabetical picker would wrongly skip it.
	d.beads["ka-0000004189"] = actionableBead("ka-0000004189", "bug", time.Now())
	// Description-only cross-refs — both actionable, both sort earlier. The
	// pre-fix bug linked whichever sorted first (ka-0000007080).
	d.beads["ka-0000007080"] = actionableBead("ka-0000007080", "bug", time.Now())
	d.beads["ka-0000002370"] = actionableBead("ka-0000002370", "bug", time.Now())

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{
			WebURL:       backlinkMRURL,
			SourceBranch: "feat/ka-0000004189-deployprod-helm-ownership-preflight",
			Title:        "fix(deploy): preflight adopts un-owned resources (ka-0000004189)",
			Description:  "parent P0 ka-0000002370; same remedy proven on ka-0000007080",
		}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if d.updates["ka-0000004189"]["mr_url"] != backlinkMRURL {
		t.Fatalf("expected the branch/title subject ka-0000004189 to be linked, got updates=%v", d.updates)
	}
	if _, wrong := d.updates["ka-0000007080"]; wrong {
		t.Fatalf("must NOT link the alphabetically-first description cross-ref ka-0000007080, got updates=%v", d.updates)
	}
	if _, wrong := d.updates["ka-0000002370"]; wrong {
		t.Fatalf("must NOT link the description cross-ref ka-0000002370, got updates=%v", d.updates)
	}
	if s.linked != 1 {
		t.Fatalf("expected linked=1, got %+v", s)
	}
}

// When the branch/title name NO bead but the description cross-references
// several DISTINCT actionable beads, the reconciler must refuse (ambiguous)
// rather than pick the alphabetically-first one. This is the body-tier
// ambiguity refusal — real MR !6115 carried 8 description refs with no
// subject-tier resolvable bead of its own in this store.
func TestBacklink_MultipleDescriptionRefsWithoutSubjectAreAmbiguous(t *testing.T) {
	d := newFakeBacklinkDaemon()
	// Two distinct description-only refs, both actionable, no branch/title bead.
	d.beads["ka-0000001755"] = actionableBead("ka-0000001755", "bug", time.Now())
	d.beads["gd-0k4F1OWBO9"] = actionableBead("gd-0k4F1OWBO9", "task", time.Now())

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"merged": {{
			WebURL:       backlinkMRURL,
			SourceBranch: "feat/ka-0000001730-formula-step-role-dispatch", // subject unknown to store
			Title:        "feat(gardenio): formula step-role dispatch (ka-0000001730)",
			Description:  "answers gd-0k4F1OWBO9; also references ka-0000001755 in the proof list",
		}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("expected no link for ambiguous multi-description-ref MR, got %v", d.updates)
	}
	if s.ambiguous != 1 {
		t.Fatalf("expected ambiguous=1, got %+v", s)
	}
}

// The description cross-references a bead, but the branch/title subject bead is
// present too: subject wins outright and the body tier is never consulted, even
// though the body ref sorts earlier.
func TestBacklink_SubjectTierShortCircuitsBodyTier(t *testing.T) {
	d := newFakeBacklinkDaemon()
	d.beads["gd-AX61RVkIwb"] = actionableBead("gd-AX61RVkIwb", "task", time.Now())
	d.beads["gd-5hT73wCvJR"] = actionableBead("gd-5hT73wCvJR", "task", time.Now())

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{
			WebURL:       backlinkMRURL,
			SourceBranch: "feat/gd-AX61RVkIwb-thing",
			Description:  "related: gd-5hT73wCvJR",
		}},
	}}
	r := newReconciler(gl, d)
	r.scan(context.Background())

	if d.updates["gd-AX61RVkIwb"]["mr_url"] != backlinkMRURL {
		t.Fatalf("expected subject gd-AX61RVkIwb linked, got %v", d.updates)
	}
	if _, wrong := d.updates["gd-5hT73wCvJR"]; wrong {
		t.Fatalf("body-tier ref must not be linked when subject resolves, got %v", d.updates)
	}
}

// Clobber guard (bug ka-0000002276, second defect): the reconciler resolves to a
// bead that ALREADY carries a different mr_url (its own, correct, already-merged
// fix). It must refuse to overwrite it, not silently repoint it at this open MR.
func TestBacklink_RefusesToOverwriteExistingMRURL(t *testing.T) {
	d := newFakeBacklinkDaemon()
	subject := actionableBead("gd-hasMRurl1", "bug", time.Now())
	subject.Fields["mr_url"] = "https://gitlab.com/acme/app/-/merge_requests/6099" // its correct merged fix
	d.beads["gd-hasMRurl1"] = subject

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{
			WebURL:       backlinkMRURL, // a DIFFERENT, unrelated open MR
			SourceBranch: "feat/gd-hasMRurl1-unrelated",
		}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("must not overwrite an existing mr_url, got updates=%v", d.updates)
	}
	if s.clobberBlocked != 1 || s.linked != 0 {
		t.Fatalf("expected clobberBlocked=1 linked=0, got %+v", s)
	}
}

// Idempotency: when the resolved bead already points at THIS SAME MR, the guard
// does not fire — a no-op re-link is fine (the mr_url is simply re-set).
func TestBacklink_ReLinkSameMRURLIsAllowed(t *testing.T) {
	d := newFakeBacklinkDaemon()
	subject := actionableBead("gd-sameMR001", "bug", time.Now())
	subject.Fields["mr_url"] = backlinkMRURL // already points at this exact MR
	d.beads["gd-sameMR001"] = subject

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "feat/gd-sameMR001-x"}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if s.clobberBlocked != 0 {
		t.Fatalf("re-link to the same MR must not trip the clobber guard, got %+v", s)
	}
	if d.updates["gd-sameMR001"]["mr_url"] != backlinkMRURL || s.linked != 1 {
		t.Fatalf("expected idempotent re-link to succeed, got updates=%v stats=%+v", d.updates, s)
	}
}

// A JIRA key resolving to BOTH an epic and an actionable child prefers the
// most-recently-updated actionable child over the epic.
func TestBacklink_PrefersActionableChildOverEpic(t *testing.T) {
	d := newFakeBacklinkDaemon()
	epic := &beadsapi.BeadDetail{ID: "gd-epic", Kind: "issue", Type: "epic", Labels: []string{"jira:PE-10133"}, UpdatedAt: time.Now()}
	olderChild := actionableBead("gd-old", "task", time.Now().Add(-2*time.Hour), "jira:PE-10133")
	newerChild := actionableBead("gd-new", "task", time.Now(), "jira:PE-10133")
	d.byLabel["jira:PE-10133"] = []*beadsapi.BeadDetail{epic, olderChild, newerChild}

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "feat/PE-10133-slice-5"}},
	}}
	r := newReconciler(gl, d)
	r.scan(context.Background())

	if _, linkedEpic := d.updates["gd-epic"]; linkedEpic {
		t.Fatalf("must never link MR to an epic, got updates=%v", d.updates)
	}
	if d.updates["gd-new"]["mr_url"] != backlinkMRURL {
		t.Fatalf("expected newest actionable child gd-new to be linked, got updates=%v", d.updates)
	}
}

// Already-linked MR (canonical MR bead exists) → no update, counted skipped.
func TestBacklink_AlreadyLinked(t *testing.T) {
	d := newFakeBacklinkDaemon()
	d.mrByURL[backlinkMRURL] = &beadsapi.BeadDetail{ID: "gd-mr1", Type: "merge-request"}
	d.byLabel["jira:PE-7083"] = []*beadsapi.BeadDetail{actionableBead("gd-task1", "bug", time.Now(), "jira:PE-7083")}

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "fix/PE-7083-x"}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("expected no updates for already-linked MR, got %v", d.updates)
	}
	if s.alreadyLinked != 1 || s.linked != 0 {
		t.Fatalf("expected alreadyLinked=1 linked=0, got %+v", s)
	}
}

// No recoverable key → skipped, no update.
func TestBacklink_NoKey(t *testing.T) {
	d := newFakeBacklinkDaemon()
	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "chore/cleanup-logs", Title: "tidy up"}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("expected no updates with no key, got %v", d.updates)
	}
	if s.noKey != 1 {
		t.Fatalf("expected noKey=1, got %+v", s)
	}
}

// Key present but NO actionable bead found → ambiguous, never guesses.
func TestBacklink_KeyButNoBead(t *testing.T) {
	d := newFakeBacklinkDaemon()
	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "fix/PE-9999-missing"}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("expected no updates when no bead matches, got %v", d.updates)
	}
	if s.ambiguous != 1 {
		t.Fatalf("expected ambiguous=1, got %+v", s)
	}
}

// Two DISTINCT JIRA keys each matching a bead → ambiguous, never guesses.
func TestBacklink_MultipleDistinctKeysAmbiguous(t *testing.T) {
	d := newFakeBacklinkDaemon()
	d.byLabel["jira:PE-1"] = []*beadsapi.BeadDetail{actionableBead("gd-a", "task", time.Now(), "jira:PE-1")}
	d.byLabel["jira:PE-2"] = []*beadsapi.BeadDetail{actionableBead("gd-b", "task", time.Now(), "jira:PE-2")}

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, Title: "Touches PE-1 and PE-2"}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("expected no updates for ambiguous multi-key MR, got %v", d.updates)
	}
	if s.ambiguous != 1 {
		t.Fatalf("expected ambiguous=1, got %+v", s)
	}
}

// gd-ref pointing at an epic is rejected — never link an MR to an epic.
func TestBacklink_KDRefToEpicRejected(t *testing.T) {
	d := newFakeBacklinkDaemon()
	d.beads["gd-epic9XYZ"] = &beadsapi.BeadDetail{ID: "gd-epic9XYZ", Kind: "issue", Type: "epic"}

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "feat/gd-epic9XYZ-umbrella"}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("expected no update for gd-ref to epic, got %v", d.updates)
	}
	if s.ambiguous != 1 {
		t.Fatalf("expected ambiguous=1, got %+v", s)
	}
}

// Dry-run logs the intended link and counts it but performs no write.
func TestBacklink_DryRun(t *testing.T) {
	d := newFakeBacklinkDaemon()
	d.byLabel["jira:PE-7083"] = []*beadsapi.BeadDetail{actionableBead("gd-task1", "bug", time.Now(), "jira:PE-7083")}
	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "fix/PE-7083-x"}},
	}}
	r := NewMRBacklinkReconciler(MRBacklinkConfig{
		GitLab: gl, Daemon: d, Logger: backlinkTestLogger(), GroupID: 42, DryRun: true,
	})
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("dry-run must not write, got %v", d.updates)
	}
	if s.linked != 1 {
		t.Fatalf("dry-run should count intended link, got %+v", s)
	}
}

// FileUnlinkedBeads files exactly one unlinked-MR bead per URL, idempotent
// across repeated scans of the same still-unresolvable MR.
func TestBacklink_FilesUnlinkedBeadOnce(t *testing.T) {
	d := newFakeBacklinkDaemon()
	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{WebURL: backlinkMRURL, SourceBranch: "fix/PE-9999-missing"}},
	}}
	r := NewMRBacklinkReconciler(MRBacklinkConfig{
		GitLab: gl, Daemon: d, Logger: backlinkTestLogger(), GroupID: 42,
		FileUnlinkedBeads: true, UnlinkedBeadProject: "gardenio",
	})

	r.scan(context.Background())
	r.scan(context.Background()) // second pass: same unresolvable MR

	if len(d.created) != 1 {
		t.Fatalf("expected exactly one unlinked-MR bead filed across two scans, got %d", len(d.created))
	}
	got := d.created[0]
	if got.Type != "task" || got.Priority != 3 {
		t.Errorf("unexpected unlinked-MR bead shape: type=%q priority=%d", got.Type, got.Priority)
	}
	var hasProject, hasUnlinked bool
	for _, l := range got.Labels {
		if l == "project:gardenio" {
			hasProject = true
		}
		if l == "unlinked-mr" {
			hasUnlinked = true
		}
	}
	if !hasProject || !hasUnlinked {
		t.Errorf("expected project:gardenio + unlinked-mr labels, got %v", got.Labels)
	}
}

// Both states are scanned and a GitLab error on one state doesn't abort the other.
func TestBacklink_ScansBothStates(t *testing.T) {
	d := newFakeBacklinkDaemon()
	d.byLabel["jira:PE-7083"] = []*beadsapi.BeadDetail{actionableBead("gd-task1", "bug", time.Now(), "jira:PE-7083")}
	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"merged": {{WebURL: backlinkMRURL, SourceBranch: "fix/PE-7083-x"}},
	}}
	r := newReconciler(gl, d)
	r.scan(context.Background())

	if len(gl.calls) != 2 || gl.calls[0] != "opened" || gl.calls[1] != "merged" {
		t.Fatalf("expected scans of opened then merged, got %v", gl.calls)
	}
	if d.updates["gd-task1"]["mr_url"] != backlinkMRURL {
		t.Fatalf("expected merged MR to be linked, got %v", d.updates)
	}
}

// A list error increments errors but does not panic; the other state still runs.
func TestBacklink_ListError(t *testing.T) {
	d := newFakeBacklinkDaemon()
	gl := &fakeMRLister{err: errors.New("boom")}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())
	if s.errors != 2 {
		t.Fatalf("expected errors=2 (one per state), got %+v", s)
	}
}

// Zero group ID disables the reconciler — Run returns nil without scanning.
func TestBacklink_DisabledWithoutGroup(t *testing.T) {
	d := newFakeBacklinkDaemon()
	gl := &fakeMRLister{}
	r := NewMRBacklinkReconciler(MRBacklinkConfig{GitLab: gl, Daemon: d, Logger: backlinkTestLogger(), GroupID: 0})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("expected nil error for disabled reconciler, got %v", err)
	}
	if len(gl.calls) != 0 {
		t.Fatalf("disabled reconciler must not scan, got calls=%v", gl.calls)
	}
}

func TestExtractMRBeadKeys(t *testing.T) {
	eq := func(t *testing.T, got, want []string, label string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", label, got, want)
			}
		}
	}
	tests := []struct {
		name        string
		branch      string
		title       string
		description string
		subjectKD   []string
		bodyKD      []string
		jira        []string
	}{
		{"jira in branch", "fix/PE-7083-eCRF", "", "", nil, nil, []string{"PE-7083"}},
		{"kd ref in branch is subject", "feat/gd-AbCdEf12-x", "PE-1 mentioned", "", []string{"gd-AbCdEf12"}, nil, []string{"PE-1"}},
		{"fsm key", "", "FSM-375 tumor assessment", "", nil, nil, []string{"FSM-375"}},
		{"dedup jira across sources", "fix/PE-7083-x", "Fix PE-7083", "addresses PE-7083", nil, nil, []string{"PE-7083"}},
		{"multiple distinct jira sorted", "", "touches PE-2 and PE-1", "", nil, nil, []string{"PE-1", "PE-2"}},
		{"none", "chore/cleanup", "tidy logs", "", nil, nil, nil},
		// Source precedence (bug ka-0000002276): the subject-tier ref stays in
		// subjectKD; description-only cross-refs land in bodyKD and are NOT
		// promoted into the subject tier even though they sort earlier.
		{
			"subject vs description cross-refs (real MR !6258 shape)",
			"feat/ka-0000004189-deployprod-helm-ownership-preflight",
			"fix(deploy): preflight adopts un-owned resources (ka-0000004189)",
			"parent P0 ka-0000002370; same remedy proven on ka-0000007080",
			[]string{"ka-0000004189"},
			[]string{"ka-0000002370", "ka-0000007080"}, // sorted within tier
			nil,
		},
		// A ref that appears in BOTH the subject and the description belongs to
		// the subject tier only — never double-counted into bodyKD.
		{
			"ref in both subject and body dedups to subject",
			"feat/gd-AbCdEf12-thing", "", "fixes gd-AbCdEf12 as described",
			[]string{"gd-AbCdEf12"}, nil, nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMRBeadKeys(tt.branch, tt.title, tt.description)
			eq(t, got.subjectKD, tt.subjectKD, "subjectKD")
			eq(t, got.bodyKD, tt.bodyKD, "bodyKD")
			eq(t, got.jira, tt.jira, "jira")
		})
	}
}

func TestIsActionableBead(t *testing.T) {
	cases := []struct {
		b    *beadsapi.BeadDetail
		want bool
	}{
		{actionableBead("gd-1", "task", time.Now()), true},
		{actionableBead("gd-2", "feature", time.Now()), true},
		{actionableBead("gd-3", "bug", time.Now()), true},
		{&beadsapi.BeadDetail{Kind: "issue", Type: "epic"}, false},
		{&beadsapi.BeadDetail{Kind: "config", Type: "agent"}, false},
		{&beadsapi.BeadDetail{Kind: "data", Type: "decision"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isActionableBead(c.b); got != c.want {
			t.Errorf("isActionableBead(%+v) = %v, want %v", c.b, got, c.want)
		}
	}
}
