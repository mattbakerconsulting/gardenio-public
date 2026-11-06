package decisionview

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestEvidenceBeads(t *testing.T) {
	if EvidenceBeads(nil) != nil {
		t.Error("nil bead has no evidence")
	}
	b := &beadsapi.BeadDetail{ID: "gd-d", Fields: map[string]string{
		"evidence":        "gd-e1, gd-e2 gd-d gd-e1",
		"requesting_task": "gd-t",
	}}
	got := EvidenceBeads(b)
	if len(got) != 3 || got[0] != "gd-e1" || got[1] != "gd-e2" || got[2] != "gd-t" {
		t.Errorf("explicit evidence first, then the requesting task, no dups, no self: %v", got)
	}
	b.Fields = map[string]string{"evidence": "a b c d", "requesting_task": "t"}
	if got := EvidenceBeads(b); len(got) != maxEvidenceBeads || got[0] != "a" || got[1] != "b" || got[2] != "t" {
		t.Errorf("capped at %d with the requesting task's slot reserved: %v", maxEvidenceBeads, got)
	}
	b.Fields = map[string]string{"evidence": "a,b,c", "requesting_task": "t"}
	if got := EvidenceBeads(b); len(got) != 3 || got[2] != "t" {
		t.Errorf("three evidence ids never push out the requesting task: %v", got)
	}
	b.Fields = map[string]string{"evidence": "t a b c"}
	if got := EvidenceBeads(b); len(got) != 3 || got[0] != "t" || got[2] != "b" {
		t.Errorf("no requesting task: all %d slots go to evidence: %v", maxEvidenceBeads, got)
	}
	b.Fields = map[string]string{"evidence": "t a", "requesting_task": "t"}
	if got := EvidenceBeads(b); len(got) != 2 || got[0] != "a" || got[1] != "t" {
		t.Errorf("the requesting task named as evidence too is read once: %v", got)
	}
	b.Fields = map[string]string{"requesting_task": "gd-t"}
	if got := EvidenceBeads(b); len(got) != 1 || got[0] != "gd-t" {
		t.Errorf("requesting task alone: %v", got)
	}
}

func TestImagesIn(t *testing.T) {
	text := "Grade done.\n" +
		"- phone: https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-27/01ad2133-opening-phone-light.png\n" +
		"![after, desktop](https://h/image/k/after.PNG \"title\") and again https://h/image/k/after.PNG.\n" +
		"not images: https://h/garden?bead=gd-x https://h/x.svg ftp://h/a.png https://h/doc.pdf\n" +
		"query ok: https://h/u/shot.webp?sig=1, end."
	got := ImagesIn(text)
	if len(got) != 3 {
		t.Fatalf("want 3 images, got %+v", got)
	}
	if got[0].Alt != "01ad2133-opening-phone-light.png" {
		t.Errorf("bare URL alt is the file name: %+v", got[0])
	}
	if got[1].URL != "https://h/image/k/after.PNG" || got[1].Alt != "after, desktop" {
		t.Errorf("markdown image keeps its alt and is not repeated by its bare twin: %+v", got[1])
	}
	if got[2].URL != "https://h/u/shot.webp?sig=1" {
		t.Errorf("trailing punctuation trimmed, query kept: %+v", got[2])
	}
	if ImagesIn("") != nil {
		t.Error("empty text has no images")
	}
}

func TestEvidenceImagesOrderAndCap(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC)
	task := EvidenceSource{
		Bead: &beadsapi.BeadDetail{ID: "gd-t", Description: "plan: https://h/a/desc.png"},
		Comments: []beadsapi.Comment{
			{Text: "before https://h/a/old.png", CreatedAt: t0},
			{Text: "after https://h/a/new.png and https://h/a/desc.png", CreatedAt: t0.Add(time.Hour)},
		},
	}
	ev := EvidenceSource{Bead: &beadsapi.BeadDetail{ID: "gd-e", Description: "https://h/a/new.png https://h/a/ev.jpg"}}
	got := EvidenceImages([]EvidenceSource{task, {}, ev}, 0)
	want := []string{"https://h/a/new.png", "https://h/a/desc.png", "https://h/a/old.png", "https://h/a/ev.jpg"}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].URL != w {
			t.Errorf("[%d] = %s, want %s", i, got[i].URL, w)
		}
	}
	if got[0].Source != "gd-t" || !got[0].At.Equal(t0.Add(time.Hour)) || got[3].Source != "gd-e" || !got[3].At.IsZero() {
		t.Errorf("source and time stamped: %+v", got)
	}
	if got := EvidenceImages([]EvidenceSource{task, ev}, 2); len(got) != 2 {
		t.Errorf("cap: %+v", got)
	}
}

type fakeReader struct {
	beads    map[string]*beadsapi.BeadDetail
	comments map[string][]beadsapi.Comment
}

func (f fakeReader) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, errors.New("not found")
}

func (f fakeReader) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	if c, ok := f.comments[id]; ok {
		return c, nil
	}
	return nil, errors.New("comments down")
}

func TestLoadEvidence(t *testing.T) {
	d := &beadsapi.BeadDetail{ID: "gd-d", Fields: map[string]string{"evidence": "gd-gone gd-e", "requesting_task": "gd-t"}}
	r := fakeReader{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-e": {ID: "gd-e", Description: "https://h/e.png"},
			"gd-t": {ID: "gd-t"},
		},
		comments: map[string][]beadsapi.Comment{"gd-t": {{Text: "https://h/t.png"}}},
	}
	got := LoadEvidence(context.Background(), r, d, 0)
	if len(got) != 2 || got[0].URL != "https://h/e.png" || got[1].URL != "https://h/t.png" {
		t.Errorf("unreadable bead and failed comments are skipped, never fatal: %+v", got)
	}
	if LoadEvidence(context.Background(), nil, d, 0) != nil || LoadEvidence(context.Background(), r, &beadsapi.BeadDetail{ID: "x"}, 0) != nil {
		t.Error("no reader or no links: no evidence")
	}
}
