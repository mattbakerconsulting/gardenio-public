package main

import (
	"os"
	"testing"
)

// The committed round-2 arms load and pass the round's own validation: every
// charge in the library and glossed, none twice, every phase's plan present.
func TestRound2ArmsLoad(t *testing.T) {
	f, err := os.Open("../../gardenio/docs/heraldry/samples/round-2/arms.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := LoadArms(f)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := LoadLibrary("../../gardenio/docs/heraldry/library.md")
	if err != nil {
		t.Fatal(err)
	}
	r := &Round2{Entries: entries, ByID: map[string]*Entry{}}
	for _, e := range entries {
		r.ByID[e.ID] = e
	}
	if err := r.validate(lib); err != nil {
		t.Fatal(err)
	}
	if len(entries) < 100 {
		t.Errorf("%d arms, want every open epic (~137)", len(entries))
	}
	grades, err := LoadGrades("../../gardenio/docs/heraldry/samples/round-2/grades.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := grades[e.ID]; !ok {
			t.Errorf("%s has no grades row", e.ID)
		}
	}
}

// A phase that shares no charge with its plan fails S6 (a); one that shares
// a corner with the same gloss passes.
func TestPhaseFamily(t *testing.T) {
	plan := &Entry{ID: "p", Arms: Arms{"a", "", "b", "", "c"}, Gloss: map[string]string{"a": "x", "b": "y", "c": "z"}}
	r := &Round2{}
	if g := r.phaseFamily(&Entry{Arms: Arms{"d", "", "e", "", ""}, Gloss: map[string]string{"d": "q", "e": "w"}}, plan); g.V != "F" {
		t.Errorf("no shared charge: %v", g)
	}
	if g := r.phaseFamily(&Entry{Arms: Arms{"b", "", "e", "", ""}, Gloss: map[string]string{"b": "y", "e": "w"}}, plan); g.V != "P" {
		t.Errorf("shared corner: %v", g)
	}
	if g := r.phaseFamily(&Entry{Arms: Arms{"a", "", "e", "", "c"}, Gloss: map[string]string{"a": "x", "e": "w", "c": "z"}}, plan); g.V != "F" {
		t.Errorf("plan with one place changed: %v", g)
	}
}
