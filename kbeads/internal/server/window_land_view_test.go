package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// Land windows (LW.1 gd-BXNhQf4ZqV). The fixture land claimlayer holds
// seedWindowPlan's plan, a second open plan with an open decision and an open
// escalation, a closed plan, an epic that is not a plan, and a plan that also
// carries another land's label. Land other holds its own plan with a step.
// Every value that must never leave a claimlayer land window carries
// "SECRET", or is named in winLandForeign.
const (
	winLandProject = "proj-claimlayer"
	winPlan2       = "cl-plan2"
	winPlan2Step   = "cl-p2-s1"
	winPlan2Dec    = "cl-p2-dec"
	winPlan2Esc    = "cl-p2-esc"
	winClosedPlan  = "cl-closed"
	winNotPlan     = "cl-notplan"
	winBothPlan    = "cl-both"
	winOtherStep   = "ot-s1"
	winOtherProj   = "proj-other"
)

// winLandForeign are the titles a claimlayer land window must never show.
var winLandForeign = []string{"Other plan", "Other land step", "Both lands plan", "Not a plan", "project:other"}

func seedWindowLand(t *testing.T, f *scopedFixture) {
	t.Helper()
	seedWindowPlan(t, f)
	f.seedLandProject(t, "false")
	f.seedTyped(winOtherProj, "project", "")
	f.ms.beads[winOtherProj].Title = "other"
	f.ms.beads[winOtherProj].Kind = "config"
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	add := func(id string, typ model.BeadType, title string, status model.Status, fields, parent string, labels []string, offset time.Duration) {
		b := &model.Bead{
			ID: id, Type: typ, Kind: model.KindIssue, Title: title, Status: status, Fleet: "default",
			Description: "SECRET-DESC " + id, CreatedBy: "SECRET-creator@example.com", Assignee: "SECRET-assignee",
			CreatedAt: base.Add(offset), UpdatedAt: base.Add(offset),
		}
		if fields != "" {
			b.Fields = json.RawMessage(fields)
		}
		if status == model.StatusClosed {
			at := base.Add(offset + time.Hour)
			b.ClosedAt = &at
		}
		f.ms.beads[id] = b
		f.ms.labels[id] = labels
		if parent != "" {
			f.ms.deps[id] = append(f.ms.deps[id], &model.Dependency{BeadID: id, DependsOnID: parent, Type: model.DepParentChild})
		}
	}
	cl := []string{"project:claimlayer"}
	clPlan := []string{"plan", "project:claimlayer"}
	add(winPlan2, model.TypeEpic, "Second plan", model.StatusInProgress, `{"plan_state":"approved","cost_usd":"SECRET-cost"}`, "", clPlan, 0)
	f.ms.beads[winPlan2].ShortTitle, f.ms.beads[winPlan2].Heraldry = "second plan", "🔥🧭📦"
	add(winPlan2Step, model.TypeTask, "Second plan step", model.StatusOpen, "", winPlan2, cl, time.Minute)
	add(winPlan2Dec, model.TypeDecision, "Ship it?", model.StatusOpen, `{"prompt":"SECRET-prompt","options":[{"id":"y","label":"Yes"}]}`, winPlan2Step, cl, 2*time.Minute)
	add(winPlan2Esc, model.TypeEscalation, "SECRET-ESC-TITLE deploy", model.StatusOpen, `{"operation_type":"helm_deploy","context":"SECRET-esc"}`, winPlan2, cl, 3*time.Minute)
	add(winClosedPlan, model.TypeEpic, "Closed plan", model.StatusClosed, `{"plan_state":"done"}`, "", clPlan, -24*time.Hour)
	add(winNotPlan, model.TypeEpic, "Not a plan", model.StatusOpen, "", "", cl, 4*time.Minute)
	add(winBothPlan, model.TypeEpic, "Both lands plan", model.StatusOpen, "", "", []string{"plan", "project:claimlayer", "project:other"}, 5*time.Minute)
	f.ms.labels[winOtherPlan] = []string{"plan", "project:other"}
	add(winOtherStep, model.TypeTask, "Other land step", model.StatusOpen, "", winOtherPlan, []string{"project:other"}, 6*time.Minute)
	// The store's list fills each bead's labels; the mock keeps them apart.
	for id, labels := range f.ms.labels {
		if b := f.ms.beads[id]; b != nil {
			b.Labels = labels
		}
	}
}

// mintLandWindow mints a window on land as token and returns the response.
func mintLandWindow(t *testing.T, f *scopedFixture, token, land string, days int) (int, createWindowResponse) {
	t.Helper()
	code, body := windowDo(t, f, http.MethodPost, "/v1/windows", token, createWindowRequest{Land: land, ExpiresInDays: days})
	var out createWindowResponse
	if code == http.StatusCreated {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode mint: %v (%s)", err, body)
		}
	}
	return code, out
}

// landViewLeaks lists what a claimlayer land view must not carry: secrets,
// emails, every bead id, the token, and anything of land other.
func landViewLeaks(tok string) []string {
	leaks := []string{"SECRET", "@example.com", "token", tok, winLandProject, winOtherProj,
		winPlan, winPhase, winStepDone, winStepOpen, winStepFlat, winBundle, winDoc, winDecision, winEsc, winAgent,
		winPlan2, winPlan2Step, winPlan2Dec, winPlan2Esc, winClosedPlan, winNotPlan, winBothPlan, winOtherPlan, winOtherStep,
		"Building now", "# Hello", "Which way?"}
	return append(leaks, winLandForeign...)
}

func TestWindow_LandMintViewRevoke(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)

	code, minted := mintLandWindow(t, f, tokScopedOwner, "claimlayer", 0)
	if code != http.StatusCreated {
		t.Fatalf("mint as owner: %d", code)
	}
	tok := minted.Token
	if !wellFormedWindowToken(tok) || minted.Path != "/w/"+tok {
		t.Fatalf("mint returned token %q path %q", tok, minted.Path)
	}
	if r := minted.Window; r.TargetKind != windowTargetLand || r.PlanID != "" || r.Land != "claimlayer" || r.State != "live" {
		t.Errorf("record = %+v", r)
	}
	if got := time.Until(minted.Window.ExpiresAt); got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Errorf("default expiry in %v, want 30 days", got)
	}
	win := f.ms.beads[minted.Window.ID]
	if wf := windowFields(win); wf.Target != winLandProject || wf.TargetKind != windowTargetLand || wf.Land != "claimlayer" || wf.TokenHash != windowTokenHash(tok) {
		t.Errorf("window fields = %+v", wf)
	}

	code, body := windowDo(t, f, http.MethodGet, windowViewPath, tok, nil)
	if code != http.StatusOK {
		t.Fatalf("view: %d %s", code, body)
	}
	for _, leak := range landViewLeaks(tok) {
		if strings.Contains(body, leak) {
			t.Errorf("land view leaks %q: %s", leak, body)
		}
	}
	for _, field := range []string{`"narrative"`, `"recent"`, `"escalations"`, "cost", "comment", "description", `"id"`} {
		if strings.Contains(body, field) {
			t.Errorf("land view carries %s: %s", field, body)
		}
	}
	var v windowLandView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, p := range v.Plans {
		titles = append(titles, p.Title)
	}
	if strings.Join(titles, "|") != "The plan|Second plan|Closed plan" || v.Land != "claimlayer" || v.OpenPlans != 2 {
		t.Fatalf("land view plans %q, land %q, open %d", titles, v.Land, v.OpenPlans)
	}
	p1, p2, closed := v.Plans[0], v.Plans[1], v.Plans[2]
	if p1.State != "building" || p1.CurrentPhase != 1 || p1.StepsDone != 1 || p1.StepsTotal != 3 || len(p1.Phases) != 1 || len(p1.Phases[0].Steps) != 2 || len(p1.Steps) != 1 {
		t.Errorf("first plan = %+v", p1)
	}
	if p1.OpenDecisions != 0 || len(p1.Decisions) != 0 || p1.OpenEscalations != 1 {
		t.Errorf("first plan waits: %d decisions %q, %d escalations", p1.OpenDecisions, p1.Decisions, p1.OpenEscalations)
	}
	if len(p1.Crew) != 1 || p1.Crew[0].Name != "crew-a" || !p1.Crew[0].Live {
		t.Errorf("first plan crew = %+v", p1.Crew)
	}
	if p2.State != "approved" || p2.Status != "in_progress" || len(p2.Steps) != 1 || p2.Steps[0].Title != "Second plan step" || p2.Steps[0].Kind != "task" {
		t.Errorf("second plan = %+v", p2)
	}
	if p2.OpenDecisions != 1 || len(p2.Decisions) != 1 || p2.Decisions[0] != "Ship it?" || p2.OpenEscalations != 1 || len(p2.Crew) != 0 {
		t.Errorf("second plan waits = %+v", p2)
	}
	if closed.Status != "closed" || closed.State != "done" || closed.Withheld {
		t.Errorf("closed plan = %+v", closed)
	}

	code, body = windowDo(t, f, http.MethodGet, "/v1/windows?land=claimlayer", tokScopedOwner, nil)
	if code != http.StatusOK || !strings.Contains(body, `"target_kind":"land"`) || strings.Contains(body, tok) || strings.Contains(body, windowTokenHash(tok)) {
		t.Errorf("list as owner: %d %s", code, body)
	}
	if code, _ := windowDo(t, f, http.MethodDelete, "/v1/windows/"+minted.Window.ID, tokScopedOwner, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if code, body := windowDo(t, f, http.MethodGet, windowViewPath, tok, nil); code != http.StatusUnauthorized || !strings.Contains(body, "invalid token") {
		t.Errorf("view after revoke: %d %s, want 401 invalid token", code, body)
	}
}

func TestWindow_LandMintAuthorization(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	for name, c := range map[string]struct {
		token string
		body  createWindowRequest
		want  int
	}{
		"owner":                {tokScopedOwner, createWindowRequest{Land: "claimlayer"}, http.StatusCreated},
		"operator":             {tokOperator, createWindowRequest{Land: "claimlayer"}, http.StatusCreated},
		"admin":                {tokOwnerAdmin, createWindowRequest{Land: "claimlayer"}, http.StatusCreated},
		"operator, any land":   {tokOperator, createWindowRequest{Land: "other"}, http.StatusCreated},
		"member":               {tokScopedWriter, createWindowRequest{Land: "claimlayer"}, http.StatusForbidden},
		"guest":                {tokScopedGuest, createWindowRequest{Land: "claimlayer"}, http.StatusForbidden},
		"static token":         {scopedAdminToken, createWindowRequest{Land: "claimlayer"}, http.StatusForbidden},
		"fleet token":          {tokFleetBridge, createWindowRequest{Land: "claimlayer"}, http.StatusForbidden},
		"land principal":       {tokLandClaimlayer, createWindowRequest{Land: "claimlayer"}, http.StatusForbidden},
		"owner, another land":  {tokScopedOwner, createWindowRequest{Land: "other"}, http.StatusNotFound},
		"operator, no land":    {tokOperator, createWindowRequest{Land: "nope"}, http.StatusNotFound},
		"owner, plan and land": {tokScopedOwner, createWindowRequest{PlanID: winPlan, Land: "claimlayer"}, http.StatusBadRequest},
		"owner, neither":       {tokScopedOwner, createWindowRequest{}, http.StatusBadRequest},
		"owner, too long":      {tokScopedOwner, createWindowRequest{Land: "claimlayer", ExpiresInDays: 366}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if code, body := windowDo(t, f, http.MethodPost, "/v1/windows", c.token, c.body); code != c.want {
				t.Errorf("mint: %d %s, want %d", code, body, c.want)
			}
		})
	}
	if code, minted := mintLandWindow(t, f, tokScopedOwner, "claimlayer", 365); code != http.StatusCreated || time.Until(minted.Window.ExpiresAt) < 364*24*time.Hour {
		t.Errorf("mint for 365 days: %d, expires %v", code, minted.Window.ExpiresAt)
	}
}

func TestWindow_LandMintFrozenAndCapped(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	f.seedLandProject(t, "true")
	if code, body := windowDo(t, f, http.MethodPost, "/v1/windows", tokScopedOwner, createWindowRequest{Land: "claimlayer"}); code != http.StatusForbidden || !strings.Contains(body, "frozen") {
		t.Errorf("mint on a frozen land: %d %s, want 403 frozen", code, body)
	}
	f.seedLandProject(t, "false")
	// Plan and land windows share the land's cap; cl-old-window is one.
	for i := 1; i < windowMaxLivePerLand; i++ {
		if code, _ := mintWindow(t, f, tokScopedOwner, winPlan, 0); code != http.StatusCreated {
			t.Fatalf("mint %d: %d", i+1, code)
		}
	}
	if code, _ := mintLandWindow(t, f, tokScopedOwner, "claimlayer", 0); code != http.StatusConflict {
		t.Errorf("land mint past the cap: %d, want 409", code)
	}
}

// TestWindow_LandViewStaysInItsLand: a land window shows only plans whose one
// project label is its land, however the store's label filter matched, and
// a plan relabelled into a second land leaves it on the next read.
func TestWindow_LandViewStaysInItsLand(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	_, mine := mintLandWindow(t, f, tokScopedOwner, "claimlayer", 0)
	_, theirs := mintLandWindow(t, f, tokOperator, "other", 0)

	_, body := windowDo(t, f, http.MethodGet, windowViewPath, mine.Token, nil)
	for _, leak := range winLandForeign {
		if strings.Contains(body, leak) {
			t.Errorf("claimlayer's window shows %q: %s", leak, body)
		}
	}
	_, body = windowDo(t, f, http.MethodGet, windowViewPath, theirs.Token, nil)
	var v windowLandView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Land != "other" || len(v.Plans) != 1 || v.Plans[0].Title != "Other plan" || v.Plans[0].Steps[0].Title != "Other land step" {
		t.Errorf("other's window = %s", body)
	}
	for _, leak := range []string{"claimlayer", "The plan", "Second plan", "Both lands plan", "crew-a"} {
		if strings.Contains(body, leak) {
			t.Errorf("other's window shows %q: %s", leak, body)
		}
	}

	// Relabel the second plan into land other as well: it is in no land now.
	// Under beadsMu: the views above armed window-view flushes that read the
	// store off this goroutine (gd-AHNBxcUXAB).
	f.ms.beadsMu.Lock()
	f.ms.labels[winPlan2] = append(f.ms.labels[winPlan2], "project:other")
	f.ms.beads[winPlan2].Labels = f.ms.labels[winPlan2]
	f.ms.beadsMu.Unlock()
	f.srv.landViews.forget() // as windowLandViewTTL passing would
	for _, tok := range []string{mine.Token, theirs.Token} {
		if _, body := windowDo(t, f, http.MethodGet, windowViewPath, tok, nil); strings.Contains(body, "Second plan") {
			t.Errorf("a plan in two lands shows: %s", body)
		}
	}

	// A window whose target is no longer its land's project bead is unknown.
	f.ms.beadsMu.Lock()
	f.ms.beads[winLandProject].Title = "renamed"
	f.ms.beadsMu.Unlock()
	if code, _ := windowDo(t, f, http.MethodGet, windowViewPath, mine.Token, nil); code != http.StatusUnauthorized {
		t.Errorf("view after the land's project bead changed name: %d, want 401", code)
	}
}

// TestWindow_LandViewBounded: past the land's walk bound a plan shows only its
// headline, and the open plans shown are capped.
func TestWindow_LandViewBounded(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	ctx := context.Background()
	wf := windowFieldSet{Target: winLandProject, TargetKind: windowTargetLand, Land: "claimlayer"}
	lp, read, err := f.srv.projectLandPlan(ctx, f.ms.beads[winPlan], 3)
	if err != nil || !lp.Withheld || lp.Title != "The plan" || lp.State != "building" || len(lp.Phases) != 0 || read != 3 {
		t.Errorf("plan past the bound: %+v read %d, %v", lp, read, err)
	}
	for i := 0; i < windowLandPlansMax+5; i++ {
		id := "cl-many-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		f.ms.beads[id] = &model.Bead{ID: id, Type: model.TypeEpic, Kind: model.KindIssue, Title: "Many", Status: model.StatusOpen,
			Fleet: "default", Labels: []string{"plan", "project:claimlayer"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		f.ms.labels[id] = f.ms.beads[id].Labels
	}
	v, err := f.srv.buildLandWindowView(ctx, wf)
	if err != nil {
		t.Fatal(err)
	}
	open := 0
	for _, p := range v.Plans {
		if p.Status != "closed" {
			open++
		}
	}
	// 67 open plans in claimlayer alone; the plan in two lands counts only
	// when it falls past the page.
	if open != windowLandPlansMax || v.OpenPlans < windowLandPlansMax+5+2 || v.OpenPlans > windowLandPlansMax+5+3 {
		t.Errorf("open plans shown %d (cap %d), counted %d", open, windowLandPlansMax, v.OpenPlans)
	}
}

// TestWindow_LandPreview: the land preview is the view a land window serves,
// but for the expiry, and it mints nothing.
func TestWindow_LandPreview(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	code, body := windowDo(t, f, http.MethodGet, "/v1/windows/preview?land=claimlayer", tokScopedOwner, nil)
	if code != http.StatusOK {
		t.Fatalf("preview as owner: %d %s", code, body)
	}
	var pv windowLandView
	if err := json.Unmarshal([]byte(body), &pv); err != nil {
		t.Fatal(err)
	}
	_, minted := mintLandWindow(t, f, tokScopedOwner, "claimlayer", 0)
	_, viewBody := windowDo(t, f, http.MethodGet, windowViewPath, minted.Token, nil)
	var wv windowLandView
	if err := json.Unmarshal([]byte(viewBody), &wv); err != nil {
		t.Fatal(err)
	}
	pv.ExpiresAt, wv.ExpiresAt = time.Time{}, time.Time{}
	a, _ := json.Marshal(pv)
	b, _ := json.Marshal(wv)
	if !bytes.Equal(a, b) {
		t.Errorf("preview differs from the window's view:\npreview %s\nwindow  %s", a, b)
	}
	for name, c := range map[string]struct {
		token, query string
		want         int
	}{
		"member":               {tokScopedWriter, "land=claimlayer", http.StatusForbidden},
		"window":               {minted.Token, "land=claimlayer", http.StatusForbidden},
		"owner, another land":  {tokScopedOwner, "land=other", http.StatusNotFound},
		"owner, plan and land": {tokScopedOwner, "land=claimlayer&plan=" + winPlan, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if code, body := windowDo(t, f, http.MethodGet, "/v1/windows/preview?"+c.query, c.token, nil); code != c.want {
				t.Errorf("preview: %d %s, want %d", code, body, c.want)
			}
		})
	}
}

// windowLandViewGolden is the frozen GET /v1/windows/self/view body of a land
// window on seedWindowLand's claimlayer: the LW payload contract. LW.2
// (gd-y7CRpIXUTI, internal/planwindow) copies this file byte for byte. A
// field change here is a contract change: comment it on LW.1 gd-BXNhQf4ZqV
// and LW.2.
const windowLandViewGolden = "testdata/window_land_view.golden.json"

func TestWindowLandView_Golden(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	win := &model.Bead{ID: "cl-golden-land-window", Type: typeWindow, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"target":"` + winLandProject + `","target_kind":"land","land":"claimlayer","expires_at":"2026-10-31T12:00:00Z"}`)}
	ctx := windowPrincipalContext(context.Background(), f.fc, win)
	rec := httptest.NewRecorder()
	f.srv.handleWindowView(rec, httptest.NewRequest(http.MethodGet, windowViewPath, nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}
	got := rec.Body.Bytes()
	if *updateMachineGolden {
		if err := os.WriteFile(windowLandViewGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(windowLandViewGolden)
	if err != nil {
		t.Fatalf("read golden (regenerate with `go test -run TestWindowLandView_Golden -update`): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the land window view differs from %s, the LW payload contract (regenerate with -update and tell LW.2):\n got: %s\nwant: %s", windowLandViewGolden, got, want)
	}
}
