package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rivo/uniseg"
	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/heraldry"
	"gardenio/kbeads/internal/model"
)

// The arms these tests give, and the same arms as a bead stored before arms
// were a run still carries them (the shield form, read as the run).
const (
	testArmsRun    = "🔥🧭📦"
	testArmsStored = "blue 🔥 . 🧭 . 📦"
)

// noStoredForm fails when out shows the stored form, which no person sees.
func noStoredForm(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "🔥 . 🧭") || strings.Contains(out, ". 📦") {
		t.Errorf("output shows the stored form:\n%s", out)
	}
}

func TestCheckArmsFlag(t *testing.T) {
	for _, ok := range []string{"", "none", "NONE", testArmsRun, "🔥🧭📦🎯🦊", testArmsRun + " on blue", "tl:🔥 c:🧭 br:📦", testArmsStored} {
		if err := checkArmsFlag(ok); err != nil {
			t.Errorf("checkArmsFlag(%q) = %v; want ok", ok, err)
		}
	}
	for in, msg := range map[string]string{
		"🔥🔥🧭": "🔥 is used twice",
		"🔥🧭":  "arms are three to five emoji",
	} {
		err := checkArmsFlag(in)
		if err == nil {
			t.Fatalf("checkArmsFlag(%s) passed; want the daemon's refusal", in)
		}
		if want := "invalid arms: " + msg + ". " + model.HeraldryHelp; err.Error() != want {
			t.Errorf("error = %q; want the daemon's words %q", err, want)
		}
	}
}

func TestPrintArmsShowsOneLine(t *testing.T) {
	for _, stored := range []string{testArmsRun, testArmsStored} {
		var out bytes.Buffer
		printArms(&out, stored)
		if want := "Arms:        🔥🧭📦\n"; out.String() != want {
			t.Errorf("printArms(%q) =\n%s\nwant\n%s", stored, out.String(), want)
		}
	}

	var out bytes.Buffer

	out.Reset()
	printArms(&out, "")
	if out.Len() != 0 {
		t.Errorf("printArms with no arms printed %q", out.String())
	}
}

func TestPrintBeadTableShowsArmsUnderShortTitle(t *testing.T) {
	b := &model.Bead{ID: "gd-p1", Title: "a plan", ShortTitle: "ship the run", Type: model.TypeEpic, Heraldry: testArmsRun}
	out := captureStdout(t, func() { printBeadTable(b) })
	if !strings.Contains(out, "Short title: ship the run\nArms:        🔥🧭📦\n") {
		t.Errorf("kd show lacks the Arms: line under the short title:\n%s", out)
	}
	if strings.Contains(out, "📦\n             ") {
		t.Errorf("kd show prints something under the Arms: line:\n%s", out)
	}
	noStoredForm(t, out)

	out = captureStdout(t, func() { printBeadTableFiltered(b, []string{"id", "arms"}) })
	if !strings.Contains(out, "Arms:        🔥🧭📦\n") {
		t.Errorf("kd show --fields arms lacks the Arms: line:\n%s", out)
	}
}

// columnOf is the column on screen where needle starts in line, -1 if absent.
func columnOf(line, needle string) int {
	i := strings.Index(line, needle)
	if i < 0 {
		return -1
	}
	return uniseg.StringWidth(line[:i])
}

func TestPrintBeadListTableArmsColumn(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-a", Status: model.StatusOpen, Type: model.TypeEpic, ShortTitle: "first", Title: "First", Heraldry: testArmsStored},
		{ID: "gd-b", Status: model.StatusOpen, Type: model.TypeTask, ShortTitle: "second", Title: "Second"},
		{ID: "gd-c", Status: model.StatusOpen, Type: model.TypeEpic, ShortTitle: "third", Title: "Third", Heraldry: "🗝️🦊🐙"},
	}
	out := captureStdout(t, func() { printBeadListTable(beads, len(beads)) })
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "ARMS") {
		t.Fatalf("header lacks ARMS when a row bears arms: %q", lines[0])
	}
	if !strings.Contains(lines[1], testArmsRun) || !strings.Contains(lines[3], "🗝️🦊🐙") {
		t.Errorf("rows lack their emoji run:\n%s", out)
	}
	noStoredForm(t, out)
	// The emoji are two columns wide but one rune; the SHORT column must
	// still start at one column on screen on every line.
	want := columnOf(lines[0], "SHORT")
	for i, short := range []string{"first", "second", "third"} {
		if got := columnOf(lines[i+1], short); got != want {
			t.Errorf("row %d: short title at column %d; want %d, under SHORT\n%s", i+1, got, want, out)
		}
	}

	out = captureStdout(t, func() { printBeadListTable(beads[1:2], 1) })
	if strings.Contains(out, "ARMS") {
		t.Errorf("table with no arms grew an ARMS column:\n%s", out)
	}
}

func TestPrintBeadListTableWithScoreArmsColumn(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-a", Status: model.StatusOpen, Type: model.TypeEpic, Title: "First", Heraldry: testArmsStored},
		{ID: "gd-b", Status: model.StatusOpen, Type: model.TypeTask, Title: "Second"},
	}
	out := captureStdout(t, func() { printBeadListTableWithScore(beads, 2) })
	lines := strings.Split(out, "\n")
	want := columnOf(lines[0], "TITLE")
	for i, title := range []string{"First", "Second"} {
		if got := columnOf(lines[i+1], title); got != want {
			t.Errorf("row %d: title at column %d; want %d\n%s", i+1, got, want, out)
		}
	}

	out = captureStdout(t, func() { printBeadListCustom(beads, 2, []string{"id", "arms", "title"}) })
	lines = strings.Split(out, "\n")
	want = columnOf(lines[0], "TITLE")
	for i, title := range []string{"First", "Second"} {
		if got := columnOf(lines[i+1], title); got != want {
			t.Errorf("--fields row %d: title at column %d; want %d\n%s", i+1, got, want, out)
		}
	}
}

func TestLibraryMatching(t *testing.T) {
	got := libraryMatching("Compass")
	if len(got) != 1 || got[0].Emoji != "🧭" {
		t.Errorf("libraryMatching(Compass) = %v; want just 🧭", got)
	}
	if n := len(libraryMatching("")); n != len(heraldry.Library()) {
		t.Errorf("libraryMatching(\"\") = %d emoji; want the whole library, %d", n, len(heraldry.Library()))
	}
	var out bytes.Buffer
	printLibrary(&out, libraryMatching("compass"), "compass")
	if !strings.HasPrefix(out.String(), "🧭 compass\n") {
		t.Errorf("printLibrary = %q", out.String())
	}
}

func mustArms(t *testing.T, s string) heraldry.Arms {
	t.Helper()
	a, err := heraldry.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return a
}

func TestCheckArms(t *testing.T) {
	bead := func(id, arms string) *model.Bead {
		return &model.Bead{ID: id, ShortTitle: id + " title", Heraldry: arms}
	}

	t.Run("free, no project", func(t *testing.T) {
		r := checkArms(mustArms(t, testArmsRun), "", nil)
		if r.Taken || r.Arms != testArmsRun || r.Words != "arms: fire, compass, package" {
			t.Errorf("report = %+v; want free, the run and its words", r)
		}
	})
	t.Run("the same run is taken", func(t *testing.T) {
		r := checkArms(mustArms(t, testArmsRun), "alpha", []*model.Bead{bead("gd-x", testArmsRun)})
		if !r.Taken || r.TakenBy == nil || r.TakenBy.ID != "gd-x" {
			t.Fatalf("report = %+v; want taken by gd-x", r)
		}
		want := "the arms 🔥🧭📦 are already borne by gd-x (gd-x title) in project alpha; arms are unique in a project, so choose other emoji or another order"
		if r.TakenWords != want {
			t.Errorf("taken words = %q; want the daemon's %q", r.TakenWords, want)
		}
	})
	t.Run("a bead still in the shield form takes its run", func(t *testing.T) {
		r := checkArms(mustArms(t, testArmsRun), "alpha", []*model.Bead{bead("gd-x", "red 🔥 . 🧭 . 📦")})
		if !r.Taken || r.TakenBy == nil || r.TakenBy.Arms != testArmsRun {
			t.Errorf("report = %+v; want taken by gd-x, shown as the run", r)
		}
	})
	t.Run("the same emoji in another order", func(t *testing.T) {
		r := checkArms(mustArms(t, testArmsRun), "alpha", []*model.Bead{bead("gd-y", "📦🧭🔥"), bead("gd-z", "🦊🐙🎯")})
		if r.Taken || len(r.SameEmoji) != 1 || r.SameEmoji[0].ID != "gd-y" {
			t.Errorf("report = %+v; want gd-y alone as a look-alike", r)
		}
	})
}

func TestHeraldryCheckCmd(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{{"id": "gd-x", "short_title": "the other plan", "type": "epic", "heraldry": "red 🔥 . 🧭 . 📦"}},
			"total": 1,
		})
	}))
	t.Cleanup(srv.Close)
	withHTTPClient(t, srv)
	if err := heraldryCheckCmd.Flags().Set("project", "alpha"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = heraldryCheckCmd.Flags().Set("project", "") })

	var out bytes.Buffer
	heraldryCheckCmd.SetOut(&out)
	t.Cleanup(func() { heraldryCheckCmd.SetOut(nil) })

	if err := heraldryCheckCmd.RunE(heraldryCheckCmd, []string{"📦", "🧭", "🔥"}); err != nil {
		t.Fatalf("check 📦🧭🔥: %v", err)
	}
	if !strings.Contains(query, "heraldry_project=alpha") {
		t.Errorf("register query = %q; want heraldry_project=alpha", query)
	}
	want := "Arms:       📦🧭🔥\n" +
		"In words:   arms: package, compass, fire\n" +
		"Project:    alpha: free, no other bead bears these arms\n" +
		"Looks like: gd-x (the other plan), 🔥🧭📦: the same emoji, in another order\n"
	if out.String() != want {
		t.Errorf("check output =\n%s\nwant\n%s", out.String(), want)
	}
	noStoredForm(t, out.String())

	out.Reset()
	err := heraldryCheckCmd.RunE(heraldryCheckCmd, []string{testArmsRun})
	if err == nil || !strings.Contains(err.Error(), "already borne by gd-x (the other plan) in project alpha") {
		t.Errorf("check %s = %v; want the taken refusal", testArmsRun, err)
	}
	if !strings.Contains(out.String(), "Project:    alpha: taken by gd-x (the other plan)\n") {
		t.Errorf("check output lacks the taken line:\n%s", out.String())
	}

	if err := heraldryCheckCmd.RunE(heraldryCheckCmd, []string{"🔥🧭"}); err == nil || !strings.HasPrefix(err.Error(), "invalid arms: arms are three to five emoji. ") {
		t.Errorf("check 🔥🧭 = %v; want the three-to-five refusal", err)
	}
	if err := heraldryCheckCmd.RunE(heraldryCheckCmd, []string{"🌱"}); err == nil || !strings.HasPrefix(err.Error(), "invalid arms: 🌱 is not in the library") {
		t.Errorf("check 🌱 = %v; want the library refusal", err)
	}
}

func TestCreateAndUpdateSendHeraldry(t *testing.T) {
	srv, body := shortTitleServer(t)
	withHTTPClient(t, srv)
	resetCreateShortTitleFlags(t)
	resetHeraldryFlags(t)

	if err := createCmd.Flags().Set("heraldry", testArmsRun); err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a plan"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	if got := (*body)["heraldry"]; got != testArmsRun {
		t.Errorf("POST heraldry = %v; want the flag value", got)
	}

	*body = nil
	if err := createCmd.Flags().Set("heraldry", "🔥🔥🧭"); err != nil {
		t.Fatal(err)
	}
	if err := createCmd.RunE(createCmd, []string{"a plan"}); err == nil || !strings.Contains(err.Error(), "🔥 is used twice") {
		t.Errorf("create with 🔥🔥🧭 = %v; want the refusal before sending", err)
	}
	if *body != nil {
		t.Errorf("invalid arms were sent: %v", *body)
	}

	resetUpdateFlags(t)
	if err := updateCmd.Flags().Set("heraldry", "none"); err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		if err := updateCmd.RunE(updateCmd, []string{"gd-st1"}); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	if got, ok := (*body)["heraldry"]; !ok || got != "none" {
		t.Errorf("PATCH heraldry = %v (sent %v); want none, which removes the arms", got, ok)
	}
}

// resetHeraldryFlags clears --heraldry on create and update.
func resetHeraldryFlags(t *testing.T) {
	t.Helper()
	clear := func() {
		for _, fs := range []*pflag.FlagSet{createCmd.Flags(), updateCmd.Flags()} {
			_ = fs.Set("heraldry", "")
			fs.Lookup("heraldry").Changed = false
		}
	}
	clear()
	t.Cleanup(clear)
}

// TestHeraldryHelpTellsAgentsHow: `kd heraldry --help` says the agent picks
// arms for an epic and how (spec R7, §7), shows the example as kd show's one
// Arms: line, and carries the one help text.
func TestHeraldryHelpTellsAgentsHow(t *testing.T) {
	long := heraldryCmd.Long
	for _, want := range []string{
		"the agent that files or holds an epic",
		"kd heraldry library --search",
		"kd heraldry check \"" + model.HeraldryExample + "\" --project",
		"kd update <id> --heraldry \"" + model.HeraldryExample + "\"",
		"  Arms:        " + model.HeraldryExample + "\n",
		"three to five emoji",
		model.HeraldryHelp,
	} {
		if !strings.Contains(long, want) {
			t.Errorf("kd heraldry --help does not say %q:\n%s", want, long)
		}
	}
	noStoredForm(t, long)
	for _, gone := range []string{"shield", "colour", "tl:", "·"} {
		if strings.Contains(long, gone) {
			t.Errorf("kd heraldry --help still says %q:\n%s", gone, long)
		}
	}
}
