package gardenhub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The land header's steward notes (GET /garden/notes/{land}, POST .../holds; gd-q4NgYNGpy8): the scope is checked before
// any read, a guest reads but may not save, and a save changes the Holds body alone, every byte outside it as stored.

// acmeNotes is a land's notes as the steward writes them (steward.EmptyNotes and notesDoc.String's layout), with an entry
// in every section.
const acmeNotes = "Steward notes for acme. Read on every wake, written at the end of every run.\n" +
	"The land's people may edit Holds; the rest is the steward's.\n" +
	"\n## Holds\n- never merge PR 12\n" +
	"\n## Answers (newest first)\n- passkeys only (gd-dOld001)\n" +
	"\n## Promises\n- digest at 18:00\n" +
	"\n## In flight\n- crew on step 2\n"

type fakeNotes struct {
	beads   []*beadsapi.BeadDetail
	lists   int
	writes  []map[string]string
	deny    int    // refuse this many writes with a 403
	between string // content the steward writes before the first refused write is answered
}

func (f *fakeNotes) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.lists++
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if slices.Contains(b.Labels, q.Labels[0]) && strings.Contains(b.Title, q.Search) {
			c := *b
			c.Fields = map[string]string{}
			for k, v := range b.Fields {
				c.Fields[k] = v
			}
			out = append(out, &c)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out}, nil
}

func (f *fakeNotes) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	if f.deny > 0 {
		f.deny--
		if f.between != "" {
			f.beads[len(f.beads)-1].Fields["content"], f.between = f.between, ""
		}
		return &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "re-read the notes and change only Holds"}
	}
	f.writes = append(f.writes, fields)
	for _, b := range f.beads {
		if b.ID == id {
			for k, v := range fields {
				b.Fields[k] = v
			}
		}
	}
	return nil
}

func notesFake(content string) *fakeNotes {
	return &fakeNotes{beads: []*beadsapi.BeadDetail{
		{ID: "gd-other", Title: "steward-notes:acme draft", Type: "doc", Labels: []string{"project:acme"}, Fields: map[string]string{"content": "## Holds\n- not this one\n"}},
		{ID: "gd-nts1", Title: "steward-notes:acme", Type: "doc", Labels: []string{"project:acme"}, Fields: map[string]string{"content": content, "format": "md"}},
	}}
}

// notesHandler admits acme and zeta; editors are the lands the viewer may edit Holds in.
func notesHandler(src NotesStore, editors ...string) *Handler {
	return &Handler{
		Notes:        src,
		InLand:       func(_ *http.Request, land string) bool { return land == "acme" || land == "zeta" },
		MayEditHolds: func(_ *http.Request, land string) bool { return slices.Contains(editors, land) },
	}
}

func notesDo(t *testing.T, h *Handler, method, path, body string, hdr ...string) (*httptest.ResponseRecorder, landNotesDoc) {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var d landNotesDoc
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, w.Body)
		}
	}
	return w, d
}

func holdsBody(holds, was string) string {
	b, _ := json.Marshal(map[string]string{"holds": holds, "was": was})
	return string(b)
}

func TestSplitNotesHolds(t *testing.T) {
	for _, c := range []struct {
		name, content          string
		before, heading, after string
		body                   string
		ok                     bool
	}{
		{"the steward's layout", acmeNotes, acmeNotes[:strings.Index(acmeNotes, "## Holds")], "## Holds\n", acmeNotes[strings.Index(acmeNotes, "## Answers"):], "- never merge PR 12\n\n", true},
		{"a heading with more words", "## Holds (people's)\n- x\n## Answers\n", "", "## Holds (people's)\n", "## Answers\n", "- x\n", true},
		{"Holds last, no line end", "## Answers\n- a\n## Holds", "## Answers\n- a\n", "## Holds", "", "", true},
		{"### is not a heading", "## Holds\n### mine\n- x\n", "", "## Holds\n", "", "### mine\n- x\n", true},
		{"## Holdsx is another section", "## Holdsx\n## Holds\n- y\n", "## Holdsx\n", "## Holds\n", "", "- y\n", true},
		{"no Holds", "## Answers\n- a\n", "", "", "", "", false},
		{"two Holds", "## Holds\n- a\n## Answers\n## Holds\n- b\n", "", "", "", "", false},
	} {
		before, heading, body, after, ok := splitNotesHolds(c.content)
		if ok != c.ok || before != c.before || heading != c.heading || body != c.body || after != c.after {
			t.Errorf("%s: splitNotesHolds = %q %q %q %q %v, want %q %q %q %q %v", c.name, before, heading, body, after, ok, c.before, c.heading, c.body, c.after, c.ok)
		}
	}
}

// TestSpliceHoldsLeavesTheRest is the owner's rule kbeads enforces (scopedHoldsDenial): a Holds edit leaves every byte
// outside the Holds section as stored, whatever the edit, and the notes keep exactly one Holds section.
func TestSpliceHoldsLeavesTheRest(t *testing.T) {
	sb, sh, _, sa, _ := splitNotesHolds(acmeNotes)
	for _, holds := range []string{"", "- never merge PR 12\n- no deploys on Friday", "\n\n- padded\n\n\n", "a line\r\nwith CRLF", "# a top heading is not a section"} {
		got, ok := spliceHolds(acmeNotes, holds)
		nb, nh, body, na, ok2 := splitNotesHolds(got)
		if !ok || !ok2 || nb != sb || nh != sh || na != sa {
			t.Errorf("splice %q: outside Holds changed:\n%q\nwas\n%q", holds, got, acmeNotes)
		}
		if holdsText(body) != holdsText(holds) {
			t.Errorf("splice %q: Holds body = %q", holds, body)
		}
	}
	if got, _ := spliceHolds(acmeNotes, "- never merge PR 12"); got != acmeNotes {
		t.Errorf("the same Holds spliced back changed the notes:\n%q", got)
	}
	if got, ok := spliceHolds("## Answers\n- a\n\n## Holds", "- x"); !ok || got != "## Answers\n- a\n\n## Holds\n- x\n" {
		t.Errorf("Holds last = %q %v", got, ok)
	}
	if _, ok := spliceHolds("## Answers\n", "- x"); ok {
		t.Error("notes with no Holds section spliced")
	}
}

func TestLandNotesRead(t *testing.T) {
	src := notesFake(acmeNotes)
	h := notesHandler(src, "acme")

	for _, p := range []string{"/garden/notes/other", "/garden/notes/a%20b"} {
		if w, _ := notesDo(t, h, "GET", p, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (outside the scope, or no land's shape)", p, w.Code)
		}
	}
	if src.lists != 0 {
		t.Fatalf("a land outside the scope was read (%d lists)", src.lists)
	}

	w, d := notesDo(t, h, "GET", "/garden/notes/acme", "")
	if w.Code != http.StatusOK || d.ID != "gd-nts1" || d.Content != acmeNotes || d.Holds != "- never merge PR 12" || !d.HoldsOK || !d.Edit {
		t.Errorf("a member's read = %d %+v", w.Code, d)
	}
	if src.lists != 1 {
		t.Errorf("one read took %d lists, want 1", src.lists)
	}
	guest := notesHandler(src)
	if w, d := notesDo(t, guest, "GET", "/garden/notes/acme", ""); w.Code != http.StatusOK || d.Content != acmeNotes || d.Edit {
		t.Errorf("a guest's read = %d %+v; want the whole notes, no edit", w.Code, d)
	}
	if w, _ := notesDo(t, h, "GET", "/garden/notes/zeta", ""); w.Code != http.StatusNotFound {
		t.Errorf("a land with no notes bead = %d, want 404", w.Code)
	}
	if w, _ := notesDo(t, &Handler{InLand: h.InLand}, "GET", "/garden/notes/acme", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no notes store = %d, want 503", w.Code)
	}
}

// TestLandNotesSaveRefused: a save the route refuses never reads the notes or writes: a guest, another site or origin, a
// land outside the scope, a form post, no "was", a line opening a section of its own, a Holds too long.
func TestLandNotesSaveRefused(t *testing.T) {
	const path = "/garden/notes/acme/holds"
	newHolds := "- never merge PR 12\n- no deploys on Friday"
	{
		src := notesFake(acmeNotes)
		for _, c := range []struct {
			name string
			h    *Handler
			path string
			body string
			hdr  []string
			want int
		}{
			{"a guest", notesHandler(src), path, holdsBody(newHolds, "- never merge PR 12"), nil, http.StatusForbidden},
			{"another site", notesHandler(src, "acme"), path, holdsBody(newHolds, "- never merge PR 12"), []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
			{"another origin", notesHandler(src, "acme"), path, holdsBody(newHolds, "- never merge PR 12"), []string{"Origin", "https://evil.example"}, http.StatusForbidden},
			{"outside the scope", notesHandler(src, "acme", "other"), "/garden/notes/other/holds", holdsBody(newHolds, ""), nil, http.StatusNotFound},
			{"no was", notesHandler(src, "acme"), path, `{"holds":"- x"}`, nil, http.StatusBadRequest},
			{"a section of its own", notesHandler(src, "acme"), path, holdsBody("- x\n## Answers\n- rewritten", "- never merge PR 12"), nil, http.StatusBadRequest},
			{"too long", notesHandler(src, "acme"), path, holdsBody(strings.Repeat("x", notesHoldsMax+1), "- never merge PR 12"), nil, http.StatusRequestEntityTooLarge},
		} {
			if w, _ := notesDo(t, c.h, "POST", c.path, c.body, c.hdr...); w.Code != c.want {
				t.Errorf("%s: %d %s, want %d", c.name, w.Code, strings.TrimSpace(w.Body.String()), c.want)
			}
		}
		// a form post (no JSON body) is refused too
		mux := http.NewServeMux()
		notesHandler(src, "acme").Register(mux)
		r := httptest.NewRequest("POST", path, strings.NewReader("holds=x&was="))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("a form post = %d, want 403", w.Code)
		}
		if src.lists != 0 || len(src.writes) != 0 {
			t.Errorf("a refused save read %d times and wrote %v", src.lists, src.writes)
		}
	}
}

func TestLandNotesSaveHolds(t *testing.T) {
	const path = "/garden/notes/acme/holds"
	newHolds := "- never merge PR 12\n- no deploys on Friday"

	t.Run("a member's edit changes Holds alone", func(t *testing.T) {
		src := notesFake(acmeNotes)
		w, d := notesDo(t, notesHandler(src, "acme"), "POST", path, holdsBody(newHolds, "- never merge PR 12"), "Sec-Fetch-Site", "same-origin")
		if w.Code != http.StatusOK || !d.Saved || d.Holds != newHolds {
			t.Fatalf("save = %d %+v", w.Code, d)
		}
		if len(src.writes) != 1 || len(src.writes[0]) != 1 {
			t.Fatalf("writes = %v, want one patch of fields.content alone", src.writes)
		}
		got := src.writes[0]["content"]
		ob, _, _, oa, _ := splitNotesHolds(acmeNotes)
		nb, _, _, na, _ := splitNotesHolds(got)
		if nb != ob || na != oa || !strings.Contains(got, "## Holds\n- never merge PR 12\n- no deploys on Friday\n\n## Answers") {
			t.Errorf("saved notes:\n%s", got)
		}
	})

	t.Run("the steward wrote meanwhile: spliced onto its write", func(t *testing.T) {
		src := notesFake(acmeNotes)
		stewardWrote := strings.Replace(acmeNotes, "- crew on step 2", "- crew on step 3", 1)
		src.deny, src.between = 1, stewardWrote
		w, d := notesDo(t, notesHandler(src, "acme"), "POST", path, holdsBody(newHolds, "- never merge PR 12"))
		if w.Code != http.StatusOK || !d.Saved || len(src.writes) != 1 {
			t.Fatalf("save after a refused write = %d %+v, writes %d", w.Code, d, len(src.writes))
		}
		if got := src.writes[0]["content"]; !strings.Contains(got, "- crew on step 3") || !strings.Contains(got, "- no deploys on Friday") {
			t.Errorf("the retry lost the steward's write or the edit:\n%s", got)
		}
	})

	t.Run("refused twice: the daemon's word", func(t *testing.T) {
		src := notesFake(acmeNotes)
		src.deny = 2
		if w, _ := notesDo(t, notesHandler(src, "acme"), "POST", path, holdsBody(newHolds, "- never merge PR 12")); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "re-read") {
			t.Errorf("twice refused = %d %s", w.Code, w.Body)
		}
	})

	t.Run("someone else changed Holds: 409 with the notes now", func(t *testing.T) {
		src := notesFake(strings.Replace(acmeNotes, "- never merge PR 12", "- never merge PR 12\n- freeze until Monday", 1))
		w, d := notesDo(t, notesHandler(src, "acme"), "POST", path, holdsBody(newHolds, "- never merge PR 12"))
		if w.Code != http.StatusConflict || d.Holds != "- never merge PR 12\n- freeze until Monday" || d.Error == "" || len(src.writes) != 0 {
			t.Errorf("conflict = %d %+v, writes %v", w.Code, d, src.writes)
		}
	})

	t.Run("no single Holds section: 409, nothing written", func(t *testing.T) {
		src := notesFake("## Answers\n- a\n")
		w, d := notesDo(t, notesHandler(src, "acme"), "POST", path, holdsBody("- x", ""))
		if w.Code != http.StatusConflict || d.HoldsOK || !strings.Contains(d.Error, "#support") || len(src.writes) != 0 {
			t.Errorf("no Holds = %d %+v", w.Code, d)
		}
	})

	t.Run("unchanged: saved, nothing written", func(t *testing.T) {
		src := notesFake(acmeNotes)
		if w, d := notesDo(t, notesHandler(src, "acme"), "POST", path, holdsBody("- never merge PR 12\n", "- never merge PR 12")); w.Code != http.StatusOK || !d.Saved || len(src.writes) != 0 {
			t.Errorf("unchanged = %d %+v, writes %v", w.Code, d, src.writes)
		}
	})
}

// A Holds that fits alone but overflows the whole notes: 413, nothing written.
func TestLandNotesSaveHoldsOverflow(t *testing.T) {
	const path = "/garden/notes/acme/holds"
	src, big := notesFake(acmeNotes), strings.Repeat("x", notesHoldsMax-100)
	w, _ := notesDo(t, notesHandler(src, "acme"), "POST", path, holdsBody(big, "- never merge PR 12"))
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "cut Holds by") || len(src.writes) != 0 {
		t.Errorf("overflowing notes = %d %s, writes %d", w.Code, w.Body, len(src.writes))
	}
	// NotesOver (steward.NotesOverBytes in beads-viewer) is the measure when wired: it sees the spliced notes.
	h, seen := notesHandler(src, "acme"), ""
	h.NotesOver = func(notes string) int { seen = notes; return 0 }
	if w, _ := notesDo(t, h, "POST", path, holdsBody(big, "- never merge PR 12")); w.Code != http.StatusOK || !strings.Contains(seen, "## Holds\n"+big+"\n") {
		t.Errorf("NotesOver 0 = %d %s", w.Code, w.Body)
	}
}
