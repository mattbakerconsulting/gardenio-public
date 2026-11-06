package releaserecord

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// AppendReleaseContents appends to the open release as the daemon does: a
// row it carries by (merge_sha, pr) is kept once; a release that is not the
// open one is cut (409).
func (d *fakeDaemon) AppendReleaseContents(_ context.Context, ref string, rows any) (*beadsapi.Release, error) {
	r, ok := d.releases[""]
	if !ok || r.ID != ref {
		return nil, &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "release is cut"}
	}
	have, _ := ParseRows(r.Contents)
	raw, _ := json.Marshal(rows)
	add, _ := ParseRows(raw)
	for _, row := range add {
		if !slices.ContainsFunc(have, func(h Row) bool { return h.MergeSHA == row.MergeSHA && h.PR == row.PR }) {
			have = append(have, row)
		}
	}
	r.Contents, _ = json.Marshal(have)
	cp := *r
	return &cp, nil
}

// Each PR of the fixture's 2026.300.2 boards the open release at its merge,
// oldest first, a redelivery boarding nothing more; the cut's reconcile then
// drops none and rewrites none: every row it writes is the row the merge
// boarded.
func TestBoard_CutReconcileChangesNoBoardedRow(t *testing.T) {
	dir, shas := fixtureRepo(t)
	ctx := context.Background()
	d := newFakeDaemon()
	d.addMR("gd-MRbead0011", repoURL+"/pull/999", "The hinge, fixed", stepB1).Fields["merge_sha"] = shas["squash"]
	open := openWith(t, d)

	merges, _, err := GitSource{Dir: dir}.FirstParent(ctx, "2026.300.1", "2026.300.2")
	if err != nil || len(merges) != 4 {
		t.Fatalf("fixture merges = %d, %v; want 4", len(merges), err)
	}
	slices.Reverse(merges)
	for i, c := range append(merges, merges[0]) {
		b, err := newRecorder(d, dir).Board(ctx, d, "widget", c.SHA)
		if err != nil {
			t.Fatalf("board %s: %v", c.SHA, err)
		}
		if b.Release == nil || b.Release.ID != open.ID || len(b.Rows) != 1 {
			t.Fatalf("merge %d boarded %+v, want one row on %s", i, b, open.ID)
		}
	}
	boarded, bad := ParseRows(d.releases[""].Contents)
	if len(boarded) != 4 || bad != 0 {
		t.Fatalf("open release carries %d rows (%d bad), want 4: %+v", len(boarded), bad, boarded)
	}

	r := cutRecorder(d, dir)
	rec, err := r.Plan(ctx, "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Dropped) != 0 {
		t.Errorf("the cut drops %+v", rec.Dropped)
	}
	byKey := func(rows []Row) map[[2]any]Row {
		m := map[[2]any]Row{}
		for _, row := range rows {
			m[[2]any{row.MergeSHA, row.PR}] = row
		}
		return m
	}
	if got, want := byKey(rec.Rows), byKey(boarded); !reflect.DeepEqual(got, want) {
		t.Errorf("the cut's rows differ from the boarded ones:\ncut     %+v\nboarded %+v", rec.Rows, boarded)
	}
	if _, err := r.Write(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if cut := d.releases["2026.300.2"]; cut == nil || cut.ID != open.ID {
		t.Fatalf("2026.300.2 = %+v, want the open bead %s cut", cut, open.ID)
	}
}

// A project with no open release boards nothing and opens none: its first
// open release comes with its next cut.
func TestBoard_NoOpenReleaseBoardsNothing(t *testing.T) {
	dir, shas := fixtureRepo(t)
	d := newFakeDaemon()
	b, err := newRecorder(d, dir).Board(context.Background(), d, "widget", shas["squash"])
	if err != nil || b.Release != nil || len(d.releases) != 0 || d.creates != 0 {
		t.Fatalf("board = %+v, %v; releases %v, creates %d; want nothing", b, err, d.releases, d.creates)
	}
}

// A merge whose open release was cut under the append boards the release the
// cut opened.
func TestBoard_CutUnderTheAppendBoardsTheNext(t *testing.T) {
	dir, shas := fixtureRepo(t)
	d := newFakeDaemon()
	openWith(t, d)
	a := appendFunc(func(ctx context.Context, ref string, rows any) (*beadsapi.Release, error) {
		if ref == "gd-rel-next" {
			d.releases[""] = &beadsapi.Release{ID: "gd-rel-after", Project: "widget", Status: beadsapi.ReleaseStatusOpen}
			return nil, &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "release is cut"}
		}
		return d.AppendReleaseContents(ctx, ref, rows)
	})
	b, err := newRecorder(d, dir).Board(context.Background(), a, "widget", shas["direct"])
	if err != nil || b.Release == nil || b.Release.ID != "gd-rel-after" {
		t.Fatalf("board = %+v, %v; want it on gd-rel-after", b, err)
	}
}

type appendFunc func(ctx context.Context, ref string, rows any) (*beadsapi.Release, error)

func (f appendFunc) AppendReleaseContents(ctx context.Context, ref string, rows any) (*beadsapi.Release, error) {
	return f(ctx, ref, rows)
}
