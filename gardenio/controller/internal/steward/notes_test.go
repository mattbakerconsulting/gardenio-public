package steward

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

const storedNotes = `Steward notes for tomato-patch.

## Holds
- PR 12: ada, 10-23: not until she has read it
- never spawn on Fridays

## Answers (newest first)
- 2026-10-23 tp-dec2: approve the watering plan
- 2026-10-22 tp-dec1: no Stripe

## Promises
- re-run the tests at 07:20Z

## In flight
- tp-plan step 2, PR 14
`

func TestEmptyNotes_HasTheFourSections(t *testing.T) {
	e := EmptyNotes(land)
	d := parseNotes(e)
	for _, s := range sections {
		if d.section(s.name) < 0 {
			t.Errorf("empty notes have no %s section:\n%s", s.name, e)
		}
	}
	if _, _, err := CapNotes(e, NotesCapBytes); err != nil {
		t.Errorf("empty notes over the cap: %v", err)
	}
	if got, err := MergeNotes(e, NotesEdit{}); err != nil || got != e {
		t.Errorf("an empty edit of empty notes changed them: %v\n%q\n%q", err, got, e)
	}
}

func TestMergeNotes_KeepsStoredHolds(t *testing.T) {
	// The steward writes its sections and a Holds section from an old read;
	// a person added "never spawn on Fridays" during the run.
	written := `## Holds
- PR 12: ada, 10-23: not until she has read it

## Answers (newest first)
- 2026-10-24 tp-dec3: drop the fence plan
- 2026-10-23 tp-dec2: approve the watering plan
- 2026-10-22 tp-dec1: no Stripe

## Promises

## In flight
- tp-plan step 3, PR 15
`
	got, err := MergeNotes(storedNotes, NotesEdit{Written: written})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- never spawn on Fridays", "- 2026-10-24 tp-dec3: drop the fence plan", "- tp-plan step 3, PR 15"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged notes missing %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"re-run the tests", "PR 14", "Steward notes for"} {
		if strings.Contains(got, gone) {
			t.Errorf("merged notes kept %q from the stored sections:\n%s", gone, got)
		}
	}
	if strings.Count(got, "## Holds") != 1 {
		t.Errorf("want one Holds section:\n%s", got)
	}

	// Written with no Holds and no Promises: Holds goes first, Promises is
	// added empty.
	got, err = MergeNotes(storedNotes, NotesEdit{Written: "## Answers\n- a\n\n## In flight\n- b\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "## Holds\n- PR 12") || !strings.HasSuffix(got, "## Promises\n") {
		t.Errorf("merged notes:\n%s", got)
	}
}

func TestMergeNotes_AddAndLiftHolds(t *testing.T) {
	got, err := MergeNotes(storedNotes, NotesEdit{AddHolds: []string{"  - hold PR 20:   bob  ", "never spawn on Fridays"}, LiftHolds: []string{"PR 12"}})
	if err != nil {
		t.Fatal(err)
	}
	holds := got[strings.Index(got, "## Holds"):strings.Index(got, "## Answers")]
	if holds != "## Holds\n- never spawn on Fridays\n- hold PR 20: bob\n\n" {
		t.Errorf("holds = %q", holds)
	}
	if !strings.Contains(got, "- re-run the tests at 07:20Z") {
		t.Errorf("no Written keeps the steward's stored sections:\n%s", got)
	}

	for _, c := range []struct{ lift, want string }{
		{"PR 99", "no hold says that"},
		{"n", "more than one hold"},
	} {
		if _, err := MergeNotes(storedNotes, NotesEdit{LiftHolds: []string{c.lift}}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("lift %q: err = %v, want %q", c.lift, err, c.want)
		}
	}
	// The whole text wins over a part another hold shares.
	two := "## Holds\n- PR 1\n- PR 12\n"
	if got, err := MergeNotes(two, NotesEdit{LiftHolds: []string{"PR 1"}}); err != nil || strings.Contains(got, "- PR 1\n") || !strings.Contains(got, "- PR 12") {
		t.Errorf("lift by whole text: %v\n%s", err, got)
	}
}

// NotesOverBytes is 0 exactly when CapNotes can hold the notes, else the cut
// that brings them to the cap with every Answer rolled out (gardenhub refuses
// a Holds save past it).
func TestNotesOverBytes_MatchesCapNotes(t *testing.T) {
	answers := "## Answers (newest first)\n" + strings.Repeat("- 2026-10-01 tp-dec1: "+strings.Repeat("a", 150)+"\n", 20)
	for _, holds := range []int{10, 3000, 3900, 5000} {
		notes := "## Holds\n- " + strings.Repeat("h", holds) + "\n\n" + answers + "\n## Promises\n- p\n\n## In flight\n- f\n"
		over := NotesOverBytes(notes, NotesCapBytes)
		_, _, err := CapNotes(notes, NotesCapBytes)
		if (over == 0) != (err == nil) {
			t.Errorf("Holds %d: NotesOverBytes %d, CapNotes err %v", holds, over, err)
		}
		if over > 0 {
			cut := strings.Replace(notes, strings.Repeat("h", holds), strings.Repeat("h", holds-over), 1)
			if _, _, err := CapNotes(cut, NotesCapBytes); err != nil || NotesOverBytes(cut, NotesCapBytes) != 0 {
				t.Errorf("Holds %d cut by %d still over: %v", holds, over, err)
			}
		}
	}
}

func TestCapNotes_RollsOldestAnswers(t *testing.T) {
	var b strings.Builder
	b.WriteString("## Holds\n- h\n\n## Answers (newest first)\n")
	for i := 30; i > 0; i-- {
		fmt.Fprintf(&b, "- 2026-10-%02d tp-dec%d: %s\n", i, i, strings.Repeat("x", 150))
	}
	b.WriteString("\n## Promises\n- p\n\n## In flight\n- f\n")
	capped, rolled, err := CapNotes(b.String(), NotesCapBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) > NotesCapBytes || len(rolled) == 0 {
		t.Fatalf("capped %d bytes, rolled %d", len(capped), len(rolled))
	}
	// The newest answers stay, the oldest left, newest first, nothing lost.
	if !strings.Contains(capped, "tp-dec30:") || strings.Contains(capped, "tp-dec1:") {
		t.Errorf("kept the wrong end of Answers:\n%s", capped)
	}
	if !strings.HasPrefix(rolled[len(rolled)-1], "- 2026-10-01 tp-dec1:") || strings.Count(capped, "tp-dec")+len(rolled) != 30 {
		t.Errorf("rolled %v", rolled)
	}
	for _, keep := range []string{"- h\n", "- p\n", "- f\n"} {
		if !strings.Contains(capped, keep) {
			t.Errorf("capping touched another section (%q):\n%s", keep, capped)
		}
	}

	// Under the cap: unchanged, nothing rolled.
	if got, rolled, err := CapNotes(storedNotes, NotesCapBytes); err != nil || len(rolled) != 0 || got != storedNotes {
		t.Errorf("under the cap: %v %v\n%q", err, rolled, got)
	}
	// Over the cap with no Answers left to roll: refused.
	long := "## Holds\n\n## Answers\n- a\n\n## In flight\n- " + strings.Repeat("y", NotesCapBytes) + "\n"
	if _, _, err := CapNotes(long, NotesCapBytes); err == nil || !strings.Contains(err.Error(), "shorten In flight") {
		t.Errorf("err = %v, want a refusal that says what to shorten", err)
	}
}

// notesFarm is the farm plus the writes WriteNotes makes.
type notesFarm struct {
	*fakeDaemon
	comments []string
}

func (f *notesFarm) bead(id string) *beadsapi.BeadDetail {
	for _, b := range f.beads {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (f *notesFarm) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	b := f.bead(id)
	if b == nil {
		return fmt.Errorf("no bead %s", id)
	}
	for k, v := range fields {
		b.Fields[k] = v
	}
	return nil
}

func (f *notesFarm) AddComment(_ context.Context, id, _, text string) error {
	f.comments = append(f.comments, id+": "+text)
	return nil
}

func (f *notesFarm) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	return f.bead(id), nil
}

// Two steward runs on one land: the hold a person gave is in the notes the
// first run writes back, survives a second write that leaves Holds out, and
// is rendered before the land snapshot of the second run with the rule that
// a PR a hold names is refused. The overdue promise is rendered the same
// way, with the rule that it comes first.
func TestNotes_HoldHonouredAcrossTwoRuns(t *testing.T) {
	f := &notesFarm{fakeDaemon: newFarm()}
	ctx := context.Background()
	res, err := Bind(ctx, f.fakeDaemon, land, BindSpec{CreatedBy: "gardenio-provisioner"})
	if err != nil {
		t.Fatal(err)
	}
	trig := bridge.TriggerDef{ID: res.ID, Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel(land)}}
	render := TriggerPromptRenderer(f)

	// Run 1: ada asks for a hold in #general; the steward records it and an
	// answer, makes a promise, and marks #machines.
	mark := time.Date(2026, 10, 23, 5, 12, 9, 500_000_000, time.UTC).Format(time.RFC3339Nano)
	run1 := "## Answers (newest first)\n- 2026-10-23 tp-dec2: approve the watering plan\n\n## Promises\n- 2026-10-23 06:00Z: re-run the tests on PR 12\n\n## In flight\n- tp-plan step 2, PR 12 open\n"
	if _, err := WriteNotes(ctx, f, f, land, NotesEdit{Written: run1, AddHolds: []string{"PR 12: ada, 10-23: do not merge until she has read it"}}, mark, "agent:"+land+"-steward-a1"); err != nil {
		t.Fatal(err)
	}
	n := f.bead(res.NotesID)
	if n.Fields[NotesHighWaterField] != mark {
		t.Errorf("mark = %q, want %q", n.Fields[NotesHighWaterField], mark)
	}

	// Run 2 renders the hold and the promise before the land snapshot.
	got, err := render(ctx, trig, bridge.FireEvent{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Index(got, "## The land now")
	for _, want := range []string{"- PR 12: ada, 10-23: do not merge until she has read it", "- 2026-10-23 06:00Z: re-run the tests on PR 12", "- 2026-10-23 tp-dec2: approve the watering plan"} {
		if i := strings.Index(got, want); i < 0 || i > snapshot {
			t.Errorf("run 2 prompt: %q not rendered before the land snapshot:\n%s", want, got)
		}
	}
	for _, rule := range []string{"a step or PR a hold names is\n  refused while the hold stands", "An overdue promise is the\n  first thing you do this run"} {
		if !strings.Contains(got, rule) {
			t.Errorf("run 2 prompt missing the rule %q", rule)
		}
	}
	if !strings.Contains(got, "gb lands notes tomato-patch --write notes.md --machines-high-water "+mark) {
		t.Errorf("run 2 prompt does not carry the stored mark into its write:\n%s", got)
	}

	// Run 2 writes back without Holds: the hold stands for run 3.
	if _, err := WriteNotes(ctx, f, f, land, NotesEdit{Written: "## Answers\n- x\n\n## Promises\n\n## In flight\n"}, "", "agent:"+land+"-steward-b2"); err != nil {
		t.Fatal(err)
	}
	if body := NotesBody(f.bead(res.NotesID)); !strings.Contains(body, "do not merge until she has read it") {
		t.Errorf("the hold did not survive run 2's write:\n%s", body)
	}
	if f.bead(res.NotesID).Fields[NotesHighWaterField] != mark {
		t.Error("a write without a mark moved the stored mark")
	}
}

func TestWriteNotes_RollsIntoAComment(t *testing.T) {
	f := &notesFarm{fakeDaemon: newFarm()}
	ctx := context.Background()
	id, _, err := EnsureNotes(ctx, f.fakeDaemon, land, "gardenio-provisioner")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("## Answers (newest first)\n")
	for i := 40; i > 0; i-- {
		fmt.Fprintf(&b, "- tp-dec%d: %s\n", i, strings.Repeat("z", 120))
	}
	res, err := WriteNotes(ctx, f, f, land, NotesEdit{Written: b.String()}, "", "agent:x")
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes > NotesCapBytes || len(res.Rolled) == 0 || len(f.comments) != 1 ||
		!strings.HasPrefix(f.comments[0], id+": Answers rolled out of steward-notes:tomato-patch") ||
		!strings.Contains(f.comments[0], "- tp-dec1: ") {
		t.Fatalf("result %+v, comments %v", res, f.comments)
	}
	// Ensure is idempotent.
	if again, created, err := EnsureNotes(ctx, f.fakeDaemon, land, "x"); err != nil || created || again != id {
		t.Fatalf("second ensure: %s %v %v", again, created, err)
	}
	if _, err := WriteNotes(ctx, f, f, "bean-field", NotesEdit{Written: "x"}, "", "agent:x"); err == nil {
		t.Fatal("wrote notes for a land with no notes bead")
	}
}
