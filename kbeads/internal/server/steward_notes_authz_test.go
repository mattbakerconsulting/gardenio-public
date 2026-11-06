package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Step 9.2a (gd-mN1TLhGpGe): the land's people edit the Holds section of the
// steward-notes bead and nothing else; the land's steward writes it.

const testNotes = `Steward notes.

## Holds
- hold PR 12 until I've read it

## Answers (newest first)
- 2026-10-23 gd-dec1: approve plan A

## Promises
- re-run the check at 07:20Z

## In flight
- plan A step 2, PR 14
`

func (f *scopedFixture) seedNotes(t *testing.T, id, title, project string, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	f.seedTyped(id, typeDoc, "project:"+project)
	f.ms.beads[id].Title = title
	f.ms.beads[id].Kind = "data"
	f.ms.beads[id].Fields = raw
	f.ms.beads[id].CreatedBy = "controller"
}

func (f *scopedFixture) notesContent(id string) string {
	return parseFieldsToStringMap(f.ms.beads[id].Fields)["content"]
}

func TestSplitHolds(t *testing.T) {
	before, holds, after, ok := splitHolds(testNotes)
	if !ok {
		t.Fatal("testNotes has one Holds section")
	}
	if before != "Steward notes.\n\n" || holds != "## Holds\n- hold PR 12 until I've read it\n\n" || !strings.HasPrefix(after, "## Answers") {
		t.Errorf("split = %q | %q | %q", before, holds, after)
	}
	for name, content := range map[string]string{
		"none":    "## Answers\n- x\n",
		"twice":   "## Holds\n- a\n## Holds\n- b\n",
		"h3 only": "### Holds\n- a\n",
	} {
		if _, _, _, ok := splitHolds(content); ok {
			t.Errorf("%s: splitHolds ok, want not", name)
		}
	}
	// A heading with a trailing note is still the Holds heading; one that
	// only starts with the word is not.
	if _, _, _, ok := splitHolds("## Holds (the tenant's)\n- a\n"); !ok {
		t.Error("'## Holds (the tenant's)' is the Holds heading")
	}
	if _, _, _, ok := splitHolds("## Holdsworth\n- a\n"); ok {
		t.Error("'## Holdsworth' is not the Holds heading")
	}
}

func TestStewardNotes_PeopleEditHoldsOnly(t *testing.T) {
	const id = "cl-notes"
	reset := func(f *scopedFixture) {
		f.seedNotes(t, id, "steward-notes:claimlayer", "claimlayer", map[string]any{
			"format": "md", "content": testNotes, "machines_high_water": "2026-10-23T05:00:00Z",
		})
	}
	newHold := strings.Replace(testNotes, "- hold PR 12 until I've read it\n", "- hold PR 12 until I've read it\n- never merge on a Friday\n", 1)
	editAnswers := strings.Replace(testNotes, "approve plan A", "drop plan A", 1)
	addSection := strings.Replace(testNotes, "- hold PR 12 until I've read it\n", "- hold PR 12\n## Answers (newest first)\n- forged\n", 1)
	dropHolds := strings.Replace(testNotes, "## Holds\n- hold PR 12 until I've read it\n\n", "", 1)

	for _, c := range []struct {
		name  string
		token string
		patch map[string]any
		want  int
		after string // content after the patch ("" = unchanged)
	}{
		{"owner adds a hold", tokScopedOwner, map[string]any{"fields": map[string]any{"content": newHold}}, http.StatusOK, newHold},
		{"member adds a hold", tokScopedWriter, map[string]any{"fields": map[string]any{"content": newHold}}, http.StatusOK, newHold},
		{"guest may not", tokScopedGuest, map[string]any{"fields": map[string]any{"content": newHold}}, http.StatusForbidden, ""},
		{"answers are the steward's", tokScopedOwner, map[string]any{"fields": map[string]any{"content": editAnswers}}, http.StatusForbidden, ""},
		{"no new section from inside Holds", tokScopedOwner, map[string]any{"fields": map[string]any{"content": addSection}}, http.StatusForbidden, ""},
		{"Holds must stay", tokScopedOwner, map[string]any{"fields": map[string]any{"content": dropHolds}}, http.StatusForbidden, ""},
		{"the read mark is the steward's", tokScopedOwner, map[string]any{"fields": map[string]any{"machines_high_water": "2026-10-24T00:00:00Z"}}, http.StatusForbidden, ""},
		{"content and another field", tokScopedOwner, map[string]any{"fields": map[string]any{"content": newHold, "headline": "x"}}, http.StatusForbidden, ""},
		{"content and the description", tokScopedOwner, map[string]any{"fields": map[string]any{"content": newHold}, "description": "x"}, http.StatusForbidden, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newScopedFixture(t)
			reset(f)
			code, body := f.do(t, c.token, http.MethodPatch, "/v1/beads/"+id, c.patch)
			if code != c.want {
				t.Fatalf("PATCH = %d %s, want %d", code, body, c.want)
			}
			want := c.after
			if want == "" {
				want = testNotes
			}
			if got := f.notesContent(id); got != want {
				t.Errorf("content after = %q, want %q", got, want)
			}
		})
	}
}

// A refused Holds edit leaves its audit bead: the deny is recorded as an
// edit_holds action, which type:audit must list or the record 400s and is only
// logged (recordAuditEntry is best-effort).
func TestStewardNotes_DeniedEditIsAudited(t *testing.T) {
	const id = "cl-notes"
	f := newScopedFixture(t)
	f.seedNotes(t, id, "steward-notes:claimlayer", "claimlayer", map[string]any{"format": "md", "content": testNotes})
	editAnswers := strings.Replace(testNotes, "approve plan A", "drop plan A", 1)
	code, body := f.do(t, tokScopedOwner, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"content": editAnswers}})
	if code != http.StatusForbidden {
		t.Fatalf("PATCH = %d %s, want 403", code, body)
	}
	for _, b := range f.ms.beads {
		if b.Type != "audit" {
			continue
		}
		fields := parseFieldsToStringMap(b.Fields)
		if fields["action"] == scopedActionEditHolds && fields["outcome"] == "deny" && fields["resource"] == id {
			if fields["actor"] != "clo@example.com" {
				t.Errorf("audit actor = %q, want clo@example.com", fields["actor"])
			}
			return
		}
	}
	t.Fatal("no deny audit bead for the refused edit_holds")
}

// Only the steward-notes bead of the land itself: any other doc in the land,
// and a doc titled for another land, stay closed to people.
func TestStewardNotes_OnlyTheLandsNotesBead(t *testing.T) {
	f := newScopedFixture(t)
	f.seedNotes(t, "cl-other-doc", "design notes", "claimlayer", map[string]any{"format": "md", "content": testNotes})
	f.seedNotes(t, "cl-misnamed", "steward-notes:elsewhere", "claimlayer", map[string]any{"format": "md", "content": testNotes})
	newHold := strings.Replace(testNotes, "- hold PR 12 until I've read it\n", "- another\n", 1)
	for _, id := range []string{"cl-other-doc", "cl-misnamed"} {
		code, body := f.do(t, tokScopedOwner, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"content": newHold}})
		if code != http.StatusForbidden {
			t.Errorf("%s: PATCH = %d %s, want 403", id, code, body)
		}
	}
}

// The land's steward, a new agent each run, owns its land's notes bead for
// update and comment; nobody else gains anything.
func TestStewardOwnsNotes(t *testing.T) {
	lf := newLandFixture(t)
	notes := &model.Bead{ID: "gd-n", Type: typeDoc, Kind: "data", Title: "steward-notes:plot-a", Labels: []string{"project:plot-a"}, CreatedBy: "controller"}
	otherLand := &model.Bead{ID: "gd-n2", Type: typeDoc, Kind: "data", Title: "steward-notes:plot-b", Labels: []string{"project:plot-b"}, CreatedBy: "controller"}
	otherDoc := &model.Bead{ID: "gd-d", Type: typeDoc, Kind: "data", Title: "plan notes", Labels: []string{"project:plot-a"}, CreatedBy: "controller"}
	task := &model.Bead{ID: "gd-t", Type: model.TypeTask, Kind: "issue", Title: "steward-notes:plot-a", Labels: []string{"project:plot-a"}, CreatedBy: "controller"}
	srv := &BeadsServer{store: lf.ms}

	for _, c := range []struct {
		name   string
		ctx    string
		action string
		bead   *model.Bead
		allow  bool
	}{
		{"steward updates its notes", "steward", "update", notes, true},
		{"steward may not delete them", "steward", "delete", notes, false},
		{"steward may not close them", "steward", "close", notes, false},
		{"not another land's notes", "steward", "update", otherLand, false},
		{"not another doc", "steward", "update", otherDoc, false},
		{"not a task with the title", "steward", "update", task, false},
		{"a lands-gardener does not own them", "gardener", "update", notes, false},
		{"a crew agent does not own them", "crew", "update", notes, false},
	} {
		ctx := map[string]context.Context{"steward": lf.steward, "gardener": lf.gardener, "crew": lf.crew}[c.ctx]
		denial := srv.authorizeOwnership(ctx, c.action, c.bead)
		if got := denial == nil; got != c.allow {
			t.Errorf("%s: allowed = %v (%v), want %v", c.name, got, denial, c.allow)
		}
	}
}
