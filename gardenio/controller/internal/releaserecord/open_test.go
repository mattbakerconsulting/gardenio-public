package releaserecord

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// openWith gives d an open release carrying rows, as merged PRs board it.
func openWith(t *testing.T, d *fakeDaemon, rows ...Row) *beadsapi.Release {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	r := &beadsapi.Release{ID: "gd-rel-next", Project: "widget", Status: beadsapi.ReleaseStatusOpen, Contents: raw}
	d.releases[""] = r
	return r
}

func cutRecorder(d *fakeDaemon, dir string) *Recorder {
	r := newRecorder(d, dir)
	r.CutOpen = true
	return r
}

// A tag cut onto the open release carries exactly the contents the mint path
// writes for it: the git rows win, so a row the merge stamp added that git
// does not show (#99) is dropped and reported, and a stamped row git shows
// (#10) is rewritten from git. The dry run (Plan) already names the open
// release and the dropped row; the write PATCHes the open bead, creates no
// release for the tag, and opens the next one.
func TestCutOpen_SameContentsAsMintAndOpensNext(t *testing.T) {
	dir, _ := fixtureRepo(t)
	ctx := context.Background()

	minted := newFakeDaemon()
	r := newRecorder(minted, dir)
	rec, err := r.Plan(ctx, "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write(ctx, rec); err != nil {
		t.Fatal(err)
	}
	want := minted.releases["2026.300.2"]

	d := newFakeDaemon()
	open := openWith(t, d, Row{PR: 10, Title: "stamped at merge", MergeSHA: "stampsha"}, Row{PR: 99, Title: "Reverted before the cut", MergeSHA: "gone"})
	r = cutRecorder(d, dir)
	rec, err = r.Plan(ctx, "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Open == nil || rec.Open.ID != open.ID {
		t.Fatalf("plan open = %+v, want %s", rec.Open, open.ID)
	}
	if len(rec.Dropped) != 1 || rec.Dropped[0].PR != 99 {
		t.Fatalf("dropped = %+v, want only #99", rec.Dropped)
	}
	if d.creates+d.updates != 0 {
		t.Fatalf("the dry run wrote: creates %d updates %d", d.creates, d.updates)
	}

	res, err := r.Write(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	got := d.releases["2026.300.2"]
	if !res.CutOpen || res.Created || got == nil || got.ID != open.ID || res.Release.ID != open.ID {
		t.Fatalf("result = %+v, release = %+v, want the open bead %s cut", res, got, open.ID)
	}
	if string(got.Contents) != string(want.Contents) {
		t.Errorf("contents on the open bead =\n%s\nwant the mint path's\n%s", got.Contents, want.Contents)
	}
	if got.SHA != want.SHA || got.PreviousTag != want.PreviousTag || got.Source != SourceScript || got.CutBy != want.CutBy {
		t.Errorf("cut = %+v, want sha, previous, source and tagger of %+v", got, want)
	}
	if d.updates != 1 {
		t.Errorf("updates = %d, want the one cut PATCH", d.updates)
	}
	next := d.releases[""]
	if next == nil || next.ID == open.ID || !res.NextOpened || res.Next.ID != next.ID || d.creates != 1 {
		t.Errorf("next = %+v (creates %d, result %+v), want one new open release", next, d.creates, res)
	}
	if len(d.comments[planA]) != 1 {
		t.Errorf("plan A comments = %d, want 1", len(d.comments[planA]))
	}
}

// Recording a tag already cut onto the open release again writes nothing and
// opens no second release.
func TestCutOpen_TwiceWritesOnce(t *testing.T) {
	dir, _ := fixtureRepo(t)
	d := newFakeDaemon()
	openWith(t, d)
	ctx := context.Background()
	for run := 0; run < 2; run++ {
		r := cutRecorder(d, dir)
		rec, err := r.Plan(ctx, "widget", "2026.300.2", SourceScript)
		if err != nil {
			t.Fatal(err)
		}
		if run == 1 && (rec.Existing == nil || rec.Open != nil) {
			t.Fatalf("second plan = existing %v open %v, want the cut release and no open", rec.Existing, rec.Open)
		}
		if _, err := r.Write(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	if d.creates != 1 || d.updates != 1 || len(d.comments[planA]) != 1 {
		t.Errorf("creates %d updates %d plan A comments %d, want 1 (the next), 1 (the cut), 1", d.creates, d.updates, len(d.comments[planA]))
	}
}

// With no open release the tag is minted as before, and the next opens.
func TestCutOpen_NoOpenReleaseMintsThenOpens(t *testing.T) {
	dir, _ := fixtureRepo(t)
	d := newFakeDaemon()
	r := cutRecorder(d, dir)
	rec, err := r.Plan(context.Background(), "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Write(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.CutOpen || !res.NextOpened || d.releases[""] == nil || d.creates != 2 {
		t.Errorf("result = %+v, creates %d, want a minted release and one open release", res, d.creates)
	}
}

// A tag older than a recorded release (recorded late, or by hand) is never
// cut onto the open release: that belongs to the next tag.
func TestCutOpen_OlderTagIsMinted(t *testing.T) {
	dir, _ := fixtureRepo(t)
	d := newFakeDaemon()
	d.releases["2026.300.10"] = &beadsapi.Release{ID: "gd-rel-10", Tag: "2026.300.10", Status: "cut"}
	open := openWith(t, d)
	r := cutRecorder(d, dir)
	rec, err := r.Plan(context.Background(), "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Open != nil {
		t.Fatalf("2026.300.2 planned onto the open release though 2026.300.10 is recorded")
	}
	if _, err := r.Write(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if d.releases[""] != open || d.releases["2026.300.2"].ID == open.ID {
		t.Errorf("open release = %+v, want it untouched", d.releases[""])
	}
}

// A 409 on the cut (the contents changed under it) is re-read and written once
// more; a second 409 is the caller's error and leaves the release open.
func TestCutOpen_RetriesOnceOnConflict(t *testing.T) {
	dir, _ := fixtureRepo(t)
	for _, tc := range []struct {
		conflicts int
		wantErr   bool
	}{{1, false}, {2, true}} {
		d := newFakeDaemon()
		open := openWith(t, d)
		d.cutConflicts = tc.conflicts
		r := cutRecorder(d, dir)
		rec, err := r.Plan(context.Background(), "widget", "2026.300.2", SourceScript)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Write(context.Background(), rec)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%d conflicts: err = %v, want error %v", tc.conflicts, err, tc.wantErr)
		}
		cut := d.releases["2026.300.2"]
		if tc.wantErr && (cut != nil || d.releases[""] != open) {
			t.Errorf("%d conflicts: the release was cut anyway", tc.conflicts)
		}
		if !tc.wantErr && (cut == nil || cut.ID != open.ID) {
			t.Errorf("%d conflicts: release = %+v, want the open bead cut", tc.conflicts, cut)
		}
	}
}

// The webhook and gb release record both plan a cut onto the same open
// release; the second write finds it already cut with its tag and records
// nothing more.
func TestCutOpen_SecondWriterFindsItCut(t *testing.T) {
	dir, _ := fixtureRepo(t)
	d := newFakeDaemon()
	open := openWith(t, d)
	ctx := context.Background()
	a, b := cutRecorder(d, dir), cutRecorder(d, dir)
	recA, err := a.Plan(ctx, "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	recB, err := b.Plan(ctx, "widget", "2026.300.2", SourceWebhook)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(ctx, recA); err != nil {
		t.Fatal(err)
	}
	res, err := b.Write(ctx, recB)
	if err != nil {
		t.Fatalf("second writer: %v", err)
	}
	if res.Release.ID != open.ID || d.releases["2026.300.2"].Source != SourceScript || d.creates != 1 {
		t.Errorf("result = %+v, creates %d; want the first cut kept and one next release", res.Release, d.creates)
	}
}

// OpenNext opens once: a second call returns the open release, and a 409 from
// a racing writer resolves to the release it opened.
func TestOpenNext_Idempotent(t *testing.T) {
	d := newFakeDaemon()
	ctx := context.Background()
	first, created, err := OpenNext(ctx, d, "widget")
	if err != nil || !created {
		t.Fatalf("first OpenNext = %v %v", created, err)
	}
	again, created, err := OpenNext(ctx, d, "widget")
	if err != nil || created || again.ID != first.ID || d.creates != 1 {
		t.Errorf("second OpenNext = %+v %v %v, creates %d", again, created, err, d.creates)
	}
	raced := racingOpener{d}
	got, created, err := OpenNext(ctx, raced, "widget")
	if err != nil || created || got.ID != first.ID {
		t.Errorf("after a 409 = %+v %v %v, want %s", got, created, err, first.ID)
	}
}

// racingOpener finds no open release on its first read, as when another writer
// opens one between the read and the create.
type racingOpener struct{ d *fakeDaemon }

func (o racingOpener) ListReleases(ctx context.Context, project, status string, limit int) ([]beadsapi.Release, error) {
	if o.d.updates == 0 {
		o.d.updates++
		return nil, nil
	}
	return o.d.ListReleases(ctx, project, status, limit)
}

func (o racingOpener) CreateRelease(ctx context.Context, req beadsapi.CreateReleaseRequest) (*beadsapi.Release, error) {
	return o.d.CreateRelease(ctx, req)
}
