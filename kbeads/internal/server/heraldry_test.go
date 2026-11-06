package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Arms on the daemon (heraldry.go, steps 1.1 gd-pTtoEKamiI and 5.3
// gd-InkEAAqGNM): storage as the run, the per-project register (409 naming the
// holder), the warning for the same emoji in another order, the kinds that may
// bear arms, the re-check on a project move, and values still stored in the
// shield form reading as their run.

const (
	armsRun    = "🔥🧭📦"
	armsLegacy = "blue 🔥 . 🧭 . 📦" // the same arms, stored in the shield form
	armsOrder  = "📦🔥🧭"            // the same emoji, in another order
)

func hasWarning(res *http.Response, parts ...string) bool {
	for _, w := range res.Header.Values("X-Beads-Warning") {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(w, p)
		}
		if all {
			return true
		}
	}
	return false
}

func createArmed(t *testing.T, h http.Handler, title, arms string, labels ...string) (*model.Bead, *http.Response, map[string]any) {
	t.Helper()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": title, "type": "epic", "heraldry": arms, "labels": labels,
	})
	res := rec.Result()
	if rec.Code != http.StatusCreated {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return nil, res, body
	}
	var b model.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode bead: %v", err)
	}
	return &b, res, nil
}

func TestHeraldryCreateStoresRun(t *testing.T) {
	_, ms, h := newTestServer()
	b, res, body := createArmed(t, h, "first", armsRun, "project:alpha")
	if b == nil {
		t.Fatalf("create: %d %v", res.StatusCode, body)
	}
	if b.Heraldry != armsRun || ms.beads[b.ID].Heraldry != armsRun {
		t.Errorf("heraldry = %q (stored %q); want %q", b.Heraldry, ms.beads[b.ID].Heraldry, armsRun)
	}
	if hasWarning(res, "same emoji") {
		t.Errorf("unexpected near-duplicate warning on the first arms: %q", res.Header.Values("X-Beads-Warning"))
	}
	// An old shield form is stored as its run, the colour dropped.
	b, res, body = createArmed(t, h, "old form", "tl:🔥 c:🧭 br:📦 on blue", "project:beta")
	if b == nil || b.Heraldry != armsRun {
		t.Errorf("old form: %v %d %v; want stored as %q", b, res.StatusCode, body, armsRun)
	}
}

func TestHeraldryNoneOnCreateIsNoArms(t *testing.T) {
	_, _, h := newTestServer()
	b, res, body := createArmed(t, h, "plain", "none")
	if b == nil {
		t.Fatalf("create with none: %d %v", res.StatusCode, body)
	}
	if b.Heraldry != "" {
		t.Errorf("heraldry = %q; want none", b.Heraldry)
	}
}

func TestHeraldryRegisterConflict(t *testing.T) {
	_, _, h := newTestServer()
	first, _, _ := createArmed(t, h, "first", armsRun, "project:alpha")
	if first == nil {
		t.Fatal("first create failed")
	}

	for _, same := range []string{armsRun, armsLegacy} {
		b, res, body := createArmed(t, h, "second", same, "project:alpha")
		if b != nil || res.StatusCode != http.StatusConflict {
			t.Fatalf("same arms %q in the same project: status %d; want 409", same, res.StatusCode)
		}
		if body["code"] != heraldryConflictCode || body["bead_id"] != first.ID || body["project"] != "alpha" {
			t.Errorf("409 body = %v; want code %s naming %s in alpha", body, heraldryConflictCode, first.ID)
		}
		if _, ok := body["colour"]; ok {
			t.Errorf("409 body = %v; arms have no colour", body)
		}
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, first.ID) || !strings.Contains(msg, armsRun) || strings.Contains(msg, "🔥 . 🧭") || strings.Contains(msg, "colour") {
			t.Errorf("409 message %q must name %s and the run, and nothing of the shield", msg, first.ID)
		}
	}

	// Another project has its own register.
	if b, res, body := createArmed(t, h, "elsewhere", armsRun, "project:beta"); b == nil {
		t.Errorf("same arms in another project: %d %v; want 201", res.StatusCode, body)
	}
}

// TestHeraldryRegisterReadsTheShieldForm: a bead still stored in the shield
// form holds its run in the register.
func TestHeraldryRegisterReadsTheShieldForm(t *testing.T) {
	_, ms, h := newTestServer()
	seedEpic(ms, "gd-old", armsLegacy)
	b, res, body := createArmed(t, h, "new", armsRun, "project:alpha")
	if b != nil || res.StatusCode != http.StatusConflict || body["bead_id"] != "gd-old" {
		t.Errorf("the run of a shield-form holder: %v %d %v; want 409 naming gd-old", b, res.StatusCode, body)
	}
}

func TestHeraldryNearDuplicateWarns(t *testing.T) {
	_, _, h := newTestServer()
	first, _, _ := createArmed(t, h, "first", armsRun, "project:alpha")
	b, res, body := createArmed(t, h, "reordered", armsOrder, "project:alpha")
	if b == nil {
		t.Fatalf("same emoji in another order: %d %v; want 201", res.StatusCode, body)
	}
	if b.Heraldry != armsOrder {
		t.Errorf("heraldry = %q; want %q, the order kept", b.Heraldry, armsOrder)
	}
	if !hasWarning(res, "same emoji", "another order", first.ID) {
		t.Errorf("X-Beads-Warning = %q; want the near-duplicate warning naming %s", res.Header.Values("X-Beads-Warning"), first.ID)
	}
}

func TestHeraldryRefusals(t *testing.T) {
	_, _, h := newTestServer()
	cases := []struct {
		name, arms string
		labels     []string
		want       string
	}{
		{"no project", armsRun, nil, "needs a project:<name> label"},
		{"two projects", armsRun, []string{"project:alpha", "project:beta"}, "only one project: label"},
		{"used twice", "🔥 🔥 🧭", []string{"project:alpha"}, "🔥 is used twice"},
		{"theme glyph", "🌱🔥📦", []string{"project:alpha"}, "🌱 is not in the library"},
		{"too many", "🔥🧭📦🎈🎨🎯", []string{"project:alpha"}, "three to five emoji"},
		{"too few", "🔥🧭", []string{"project:alpha"}, "three to five emoji"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, res, body := createArmed(t, h, tc.name, tc.arms, tc.labels...)
			if b != nil || res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d; want 400", res.StatusCode)
			}
			if msg, _ := body["error"].(string); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q; want it to contain %q", msg, tc.want)
			}
		})
	}
}

func TestHeraldryTypes(t *testing.T) {
	s, _, _ := newTestServer()
	// R6, narrowed in round 1: epics (plans, phases) and projects only.
	for _, bt := range []model.BeadType{"task", "bug", "feature", "chore", "wish", "decision", "escalation", "agent", "message", "session", "bundle", "advice", "role"} {
		err := s.checkHeraldry(context.Background(), &model.Bead{ID: "x", Type: bt, Heraldry: armsRun, Labels: []string{"project:alpha"}})
		var ie inputError
		if !errors.As(err, &ie) || !strings.Contains(err.Error(), "cannot bear arms") {
			t.Errorf("type %s: err = %v; want a refusal", bt, err)
		}
		// The refusal carries the one help text, which says which beads bear arms.
		if err != nil && !strings.Contains(err.Error(), model.HeraldryHelp) {
			t.Errorf("type %s: the refusal must carry HeraldryHelp; got %v", bt, err)
		}
	}
	if len(heraldryTypes) != 2 || !heraldryTypes[model.TypeEpic] || !heraldryTypes[model.TypeProject] {
		t.Errorf("heraldryTypes = %v; want epic and project", heraldryTypes)
	}
	for bt := range heraldryTypes {
		if err := s.checkHeraldry(context.Background(), &model.Bead{ID: "x", Type: bt, Heraldry: armsRun, Labels: []string{"project:alpha"}}); err != nil {
			t.Errorf("type %s: %v; want allowed", bt, err)
		}
	}
}

func TestHeraldryUpdate(t *testing.T) {
	_, _, h := newTestServer()
	first, _, _ := createArmed(t, h, "first", armsRun, "project:alpha")
	second, _, _ := createArmed(t, h, "second", "", "project:alpha")

	rec := doJSON(t, h, "PATCH", "/v1/beads/"+second.ID, map[string]any{"heraldry": armsRun})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), first.ID) {
		t.Fatalf("update to taken arms: %d %s; want 409 naming %s", rec.Code, rec.Body.String(), first.ID)
	}

	// Closing does not release arms (R5): the register still holds them.
	if rec := doJSON(t, h, "POST", "/v1/beads/"+first.ID+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("close: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, "PATCH", "/v1/beads/"+second.ID, map[string]any{"heraldry": armsLegacy}); rec.Code != http.StatusConflict {
		t.Fatalf("update to a closed bead's arms: %d; want 409", rec.Code)
	}

	// "none" releases them (R7), and then the other bead may take them.
	if rec := doJSON(t, h, "PATCH", "/v1/beads/"+first.ID, map[string]any{"heraldry": "none"}); rec.Code != http.StatusOK {
		t.Fatalf("clear arms: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, "PATCH", "/v1/beads/"+second.ID, map[string]any{"heraldry": armsRun})
	if rec.Code != http.StatusOK {
		t.Fatalf("take released arms: %d %s", rec.Code, rec.Body.String())
	}
	var got model.Bead
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Heraldry != armsRun {
		t.Errorf("heraldry = %q; want %q", got.Heraldry, armsRun)
	}

	// Rewriting a bead's own arms, in any form, is not a collision with
	// itself and stores the run.
	rec = doJSON(t, h, "PATCH", "/v1/beads/"+second.ID, map[string]any{"heraldry": "tl:🔥 c:🧭 br:📦", "title": "renamed"})
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Heraldry != armsRun {
		t.Errorf("rewrite own arms: %d %q", rec.Code, got.Heraldry)
	}

	// Invalid arms on update are refused in plain words.
	for arms, want := range map[string]string{"🎈🎈🎨": "used twice", "🎈": "three to five emoji"} {
		rec = doJSON(t, h, "PATCH", "/v1/beads/"+second.ID, map[string]any{"heraldry": arms})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("invalid arms %s: %d %s; want 400 saying %q", arms, rec.Code, rec.Body.String(), want)
		}
	}
}

// TestHeraldryShortShieldArmsSurviveALabelChange: arms of one or two emoji
// stored before arms were a run are not refused when a label change
// re-checks them; they wait for the register rewrite (step 5.6).
func TestHeraldryShortShieldArmsSurviveALabelChange(t *testing.T) {
	_, ms, h := newTestServer()
	seedEpic(ms, "gd-short", "purple . . 🔥 . .")
	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-short", map[string]any{"labels": []string{"project:alpha", "area:x"}})
	if rec.Code != http.StatusOK {
		t.Errorf("label change on a bead with one-emoji shield arms: %d %s; want 200", rec.Code, rec.Body.String())
	}
}

func TestHeraldryUpdateWarnsNearDuplicate(t *testing.T) {
	_, _, h := newTestServer()
	first, _, _ := createArmed(t, h, "first", armsRun, "project:alpha")
	second, _, _ := createArmed(t, h, "second", "", "project:alpha")
	rec := doJSON(t, h, "PATCH", "/v1/beads/"+second.ID, map[string]any{"heraldry": armsOrder})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	w := rec.Header().Values("X-Beads-Warning")
	if len(w) != 1 || !strings.Contains(w[0], first.ID) {
		t.Errorf("X-Beads-Warning = %q; want one naming %s", w, first.ID)
	}
}

func TestHeraldryProjectMove(t *testing.T) {
	_, _, h := newTestServer()
	_, _, _ = createArmed(t, h, "holder", armsRun, "project:beta")
	mover, _, _ := createArmed(t, h, "mover", armsRun, "project:alpha")

	// A label replace that moves the bead into a register holding its arms.
	rec := doJSON(t, h, "PATCH", "/v1/beads/"+mover.ID, map[string]any{"labels": []string{"project:beta"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("move into a register holding the arms: %d %s; want 409", rec.Code, rec.Body.String())
	}
	// Or one that leaves it with no project.
	rec = doJSON(t, h, "PATCH", "/v1/beads/"+mover.ID, map[string]any{"labels": []string{"area:x"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("drop the project of a bead with arms: %d %s; want 400", rec.Code, rec.Body.String())
	}
	// A move to a free register is fine.
	rec = doJSON(t, h, "PATCH", "/v1/beads/"+mover.ID, map[string]any{"labels": []string{"project:gamma"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("move to a free register: %d %s", rec.Code, rec.Body.String())
	}

	// The single-label endpoints: a second project label, or removing the
	// only one, would leave the arms without one register.
	rec = doJSON(t, h, "POST", "/v1/beads/"+mover.ID+"/labels", map[string]any{"label": "project:delta"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("add a second project label: %d %s; want 400", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, "DELETE", "/v1/beads/"+mover.ID+"/labels/project:gamma", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("remove the only project label: %d %s; want 400", rec.Code, rec.Body.String())
	}
	// Other labels are never checked.
	rec = doJSON(t, h, "POST", "/v1/beads/"+mover.ID+"/labels", map[string]any{"label": "area:y"})
	if rec.Code != http.StatusCreated {
		t.Errorf("add an unrelated label: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHeraldryListFilter(t *testing.T) {
	_, ms, h := newTestServer()
	first, _, _ := createArmed(t, h, "first", armsRun, "project:alpha")
	_, _, _ = createArmed(t, h, "other", "🎈🎨🎯", "project:alpha")
	_, _, _ = createArmed(t, h, "reordered", armsOrder, "project:alpha")
	// A bead of another project still stored in the shield form.
	old := seedEpic(ms, "gd-old", "red 🔥 . 🧭 . 📦")
	old.Labels = []string{"project:beta"}
	ms.labels["gd-old"] = old.Labels

	want := []string{"gd-old", first.ID}
	slices.Sort(want)
	for _, filter := range []string{armsRun, armsLegacy} {
		rec := doJSON(t, h, "GET", "/v1/beads?heraldry="+url.QueryEscape(filter), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		if got := ms.lastListFilter; got.Heraldry != armsRun {
			t.Errorf("heraldry=%s: filter = %q; want the run %q", filter, got.Heraldry, armsRun)
		}
		var out struct {
			Beads []model.Bead `json:"beads"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		var ids []string
		for _, b := range out.Beads {
			ids = append(ids, b.ID)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, want) {
			t.Errorf("list heraldry=%s returned %v; want %v", filter, ids, want)
		}
	}

	rec := doJSON(t, h, "GET", "/v1/beads?heraldry="+url.QueryEscape("🌱🔥📦"), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("list with invalid arms: %d; want 400", rec.Code)
	}
}

// fakeUniqueViolation is a lib/pq-shaped 23505 on the register index.
type fakeUniqueViolation struct{ msg string }

func (e fakeUniqueViolation) Error() string    { return e.msg }
func (e fakeUniqueViolation) SQLState() string { return "23505" }

func TestHeraldryRaceConflict(t *testing.T) {
	s, ms, h := newTestServer()
	winner, _, _ := createArmed(t, h, "winner", armsRun, "project:alpha")
	_ = ms

	loser := &model.Bead{ID: "gd-loser", Type: "epic", Heraldry: armsRun, Labels: []string{"project:alpha"}}
	raw := errFailedCreate(fakeUniqueViolation{`pq: duplicate key value violates unique constraint "idx_beads_heraldry_register"`})
	hc, ok := asHeraldryConflict(s.heraldryRaceConflict(context.Background(), loser, raw))
	if !ok {
		t.Fatalf("a register 23505 must become the register conflict")
	}
	if hc.HolderID != winner.ID || hc.Project != "alpha" || hc.Run != armsRun {
		t.Errorf("conflict = %+v; want holder %s in alpha", hc, winner.ID)
	}

	other := errFailedCreate(fakeUniqueViolation{`pq: duplicate key value violates unique constraint "idx_beads_idempotency_key"`})
	if got := s.heraldryRaceConflict(context.Background(), loser, other); got != other {
		t.Errorf("another index's 23505 must pass through; got %v", got)
	}
}

func errFailedCreate(err error) error {
	return errors.Join(errors.New("failed to create bead"), err)
}

func TestNormalizeHeraldry(t *testing.T) {
	for in, want := range map[string]string{
		"":        "",
		"  ":      "",
		"none":    "",
		"NONE":    "",
		armsRun:   armsRun,
		armsOrder: armsOrder,
		"☺🔥📦":     "☺️🔥📦", // VS16 added (R12)
		" 🔥 🧭 📦 ": armsRun,
		// The shield forms read as their run, the colour dropped.
		armsLegacy:             armsRun,
		"🔥 . 🧭 . 📦":            armsRun,
		"tl:🔥 c:🧭 br:📦":        armsRun,
		armsRun + " on sand":   armsRun,
		"tl:🔥 c:🧭 br:📦 on red": armsRun,
	} {
		got, err := normalizeHeraldry(in)
		if err != nil || got != want {
			t.Errorf("normalizeHeraldry(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"🔥 🔥 🧭", "🔥", "🔥🧭"} {
		if _, err := normalizeHeraldry(bad); err == nil || !strings.Contains(err.Error(), model.HeraldryHelp) {
			t.Errorf("normalizeHeraldry(%q): the error must carry HeraldryHelp; got %v", bad, err)
		}
	}
}

func TestListBeads_HeraldryProject(t *testing.T) {
	_, _, h := newTestServer()
	armed, _, _ := createArmed(t, h, "armed", armsRun, "project:alpha")
	if armed == nil {
		t.Fatal("create failed")
	}
	if b, _, _ := createArmed(t, h, "elsewhere", armsRun, "project:beta"); b == nil {
		t.Fatal("create in beta failed")
	}
	if b, _, _ := createArmed(t, h, "plain", "", "project:alpha"); b == nil {
		t.Fatal("create without arms failed")
	}

	rec := doJSON(t, h, "GET", "/v1/beads?heraldry_project=alpha", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Beads) != 1 || result.Beads[0].ID != armed.ID {
		ids := make([]string, 0, len(result.Beads))
		for _, b := range result.Beads {
			ids = append(ids, b.ID)
		}
		t.Fatalf("heraldry_project=alpha returned %v; want only %s, the one bead with arms in alpha", ids, armed.ID)
	}
}
