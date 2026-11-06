package gardenhub

import (
	"context"
	"reflect"
	"testing"
)

// The bead ids free text names are found by web/workbench.js runs()'s rule: a whole word with a capital or digit, not inside
// a URL and not a path's segment, in order and once each, up to the cap (gd-vsCHW7UqmF).
func TestMentionIDs(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"see gd-AbCdEfGhIj, then (ka-0000001089).", []string{"gd-AbCdEfGhIj", "ka-0000001089"}},
		{"gd-AbCdEfGhIj and gd-AbCdEfGhIj again", []string{"gd-AbCdEfGhIj"}},
		{"re-introduced, xgd-AbCdEfGhIj, gd-AbCdEfGhIjk, gd-short1, gd-abcdefghij", nil},
		{"branch agent/some-very-long-branch-name/gd-AbCdEfGhIj; gd-AbCdEfGhIj/x", nil},
		{"MR https://h.example/a?b=gd-AbCdEfGhIj, done", nil},
		{"", nil},
	} {
		if got := mentionIDs(c.text, nil, 30); !reflect.DeepEqual(got, c.want) {
			t.Errorf("mentionIDs(%q) = %v, want %v", c.text, got, c.want)
		}
	}
	if got := mentionIDs("gd-AbCdEfGh01 gd-AbCdEfGh02 gd-AbCdEfGh03", nil, 2); len(got) != 2 {
		t.Errorf("the cap: %v", got)
	}
}

// GET /garden/bead/{id} titles the ids its free text names that are neither related nor the bead itself (rubric C12): the
// report face's "see gd-FaceWork01" reads as the step's title. An id the daemon does not return stays out of the map and
// is listed unseen, so the panel says it is not visible (gd-wbu9kFaKLH).
func TestBeadViewMentions(t *testing.T) {
	f := faceFixture()
	f.beads["gd-FaceReport1"].Description = "Graded gd-FaceReport1 for gd-FaceEpic01, on agent/x/gd-FacePlan01; gd-NotThere01 is gone."
	doc, err := readBead(context.Background(), f, "gd-FaceReport1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"gd-FaceWork01": "Typed faces for the bead panel", "gd-FaceEpic01": f.beads["gd-FaceEpic01"].Title}
	if !reflect.DeepEqual(doc.Mentions, want) {
		t.Errorf("mentions = %v, want %v", doc.Mentions, want)
	}
	if !reflect.DeepEqual(doc.Unseen, []string{"gd-NotThere01"}) {
		t.Errorf("unseen = %v, want the one the daemon did not return", doc.Unseen)
	}
	if doc, err = readBead(context.Background(), f, "gd-FaceGeneric"); err != nil || doc.Mentions != nil || doc.Unseen != nil {
		t.Errorf("no ids named: mentions %v (%v)", doc.Mentions, err)
	}
}

// A bundle titles the ids its description and markdown documents name, as the bead view does (gd-wbu9kFaKLH): "See
// gd-FaceWork01." reads as the step's title. Its own documents, the bead it was filed for and an id in a diff are left out.
func TestBundleMentions(t *testing.T) {
	f := faceFixture()
	d := &BundleDoc{ID: "gd-FaceBundle1", Description: "For gd-FaceEpic01 and gd-NotThere01.", Parent: &BundleRef{ID: "gd-FaceEpic01"},
		Docs: []BundlePart{
			{ID: "gd-FaceBndDoc1", Format: "md", Content: "See gd-FaceWork01, and gd-FaceBndDoc1 itself."},
			{ID: "gd-FaceBndDoc2", Format: "diff", Content: "+// gd-FacePlan01"},
		}}
	bundleMentions(context.Background(), f, d)
	if want := map[string]string{"gd-FaceWork01": "Typed faces for the bead panel"}; !reflect.DeepEqual(d.Mentions, want) {
		t.Errorf("mentions = %v, want %v", d.Mentions, want)
	}
	if !reflect.DeepEqual(d.Unseen, []string{"gd-NotThere01"}) {
		t.Errorf("unseen = %v", d.Unseen)
	}
}
