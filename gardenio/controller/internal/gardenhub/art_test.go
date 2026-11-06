package gardenhub

import (
	"slices"
	"testing"
)

// The escalation sprites (web/art.js, plan gd-PJB3D7glwF step 3 gd-UDx80F47JN) are pure maps, so their contract runs under
// node (testdata/art_test.js): a distinct sprite per family, the state drawn on top as a shape (flag, turning gear, crack),
// the plot cell drawn from a FarmTask's family and wait, and whole device px per sprite px. The families it drew must be the
// Go table's, so a family added on the Go side alone has no sprite.
func TestArtUnderNode(t *testing.T) {
	r := nodeScript(t, "art_test.js")
	var js []string
	for _, f := range r["families"].([]any) {
		js = append(js, f.(string))
	}
	slices.Sort(js)
	if goFams := escFamilyConsts(t); !slices.Equal(js, goFams) {
		t.Errorf("art_test.js drew families %v, the Go family table's (farm_model.go Esc*) %v", js, goFams)
	}
	if n := r["sprites"].(float64); n != 9*4*2 {
		t.Errorf("drew %v sprites, want a family x state x frame grid of 72", n)
	}
}
