package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The gatehouse reactor (lands_gatehouse_reactor.go, gd-K62coPtKkX).

type gatehouseFixture struct {
	d       *fakeIntake
	r       *gatehouseReactor
	demoFix *demoFixture
}

// newGatehouseFixture is a gatehouse "gatehouse" with its tools on or off, the
// demo land "demo", the Gate on the same daemon, and the person
// stranger@example.com (GitHub 4242, @Hubot) who accepted the current terms.
func newGatehouseFixture(t *testing.T, toolsOn bool) *gatehouseFixture {
	t.Helper()
	df := newDemoFixture(t, nil)
	d := df.d
	tools := "false"
	if toolsOn {
		tools = "true"
	}
	d.beads["gd-gh"] = &beadsapi.BeadDetail{ID: "gd-gh", Type: "project", Title: "Project: gatehouse", Status: "open", Fields: map[string]string{
		"land_kind": "tenant", "land_opened_at": "2026-10-20T18:00:00Z", "land_gatehouse": "true", gatehouseToolsField: tools,
	}}
	d.beads["gd-user"] = &beadsapi.BeadDetail{ID: "gd-user", Type: "user", Status: "open", Fields: map[string]string{
		"email": "stranger@example.com", "terms_version": mustLandsLegal(t).Version,
	}}
	d.order = append(d.order, "gd-gh", "gd-user")
	gate, err := landsGateFromEnv(func(string) string { return "" }, d, nil, nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	gate.now = func() time.Time { return ghNow }
	r := newGatehouseReactor("gatehouse", machineCheckingIntake{d}, staticTokenSource("machine-token"), df.srv.landsDemo, gate, mustLandsLegal(t).Version, testLogger())
	r.now = func() time.Time { return ghNow }
	return &gatehouseFixture{d: d, r: r, demoFix: df}
}

// message seeds a message in land written by the person, stamped by the
// daemon with their GitHub account unless stamped is false.
func (f *gatehouseFixture) message(id, land string, age time.Duration, stamped bool) {
	fields := map[string]string{"text": "Can I try a land?", "origin": "commons"}
	if stamped {
		fields["author_github_id"], fields["author_github_login"] = "4242", "Hubot"
	}
	f.d.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "message", Status: "open", Labels: []string{"project:" + land},
		Fields: fields, CreatedBy: "stranger@example.com", CreatedAt: ghNow.Add(-age)}
	f.d.order = append(f.d.order, id)
}

// request seeds a pending request, as the steward files it.
func (f *gatehouseFixture) request(id string, fields map[string]string) {
	fields["state"] = gatehousePending
	f.d.beads[id] = &beadsapi.BeadDetail{ID: id, Type: gatehouseRequestType, Status: "open", Labels: []string{"project:gatehouse"},
		Fields: fields, CreatedBy: "steward-gatehouse", CreatedAt: ghNow.Add(-time.Minute)}
	f.d.order = append(f.d.order, id)
}

func (f *gatehouseFixture) invites() []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	for _, id := range f.d.order {
		if b := f.d.beads[id]; b.Type == "land_invite" {
			out = append(out, b)
		}
	}
	return out
}

// A demo request files nothing: it is a draft the person sees at /lands/demo,
// quoting their message, and only their own press files the pass
// (gd-EaTO1lFPAG).
func TestGatehouseReactor_DemoDraftWaitsForThePerson(t *testing.T) {
	f := newGatehouseFixture(t, false)
	f.demoFix.srv.landsGatehouseTools = f.r
	f.message("gd-m1", "gatehouse", time.Hour, true)
	f.request("gd-r1", map[string]string{"kind": "demo", "message_id": "gd-m1"})

	// Off: nothing is answered and nothing is filed.
	if n := f.r.sweep(context.Background()); n != 0 || len(f.invites()) != 0 || f.d.beads["gd-r1"].Fields["state"] != gatehousePending {
		t.Fatalf("tools off: answered %d, %d invites, state %q", n, len(f.invites()), f.d.beads["gd-r1"].Fields["state"])
	}

	f.d.beads["gd-gh"].Fields[gatehouseToolsField] = "true"
	if n := f.r.sweep(context.Background()); n != 1 {
		t.Fatalf("answered %d, want 1", n)
	}
	req := f.d.beads["gd-r1"].Fields
	if req["state"] != gatehouseDrafted || req["github_id"] != "4242" || !strings.Contains(req["outcome"], "/lands/demo") || len(f.invites()) != 0 {
		t.Fatalf("request = %v, %d invites; want a draft and no pass", req, len(f.invites()))
	}
	if strings.Contains(req["outcome"], "stranger@example.com") {
		t.Fatalf("the outcome, which every guest can read, names the email: %q", req["outcome"])
	}

	// Another person's page offers nothing; the person's own quotes their message.
	other := demoGuest()
	other.githubID = 9999
	if a := landsDo(f.demoFix.h, http.MethodGet, "/lands/demo", other, demoBearer, nil); strings.Contains(a.Body.String(), "Can I try a land?") {
		t.Fatal("another person's page offers this draft")
	}
	page := f.demoFix.do(http.MethodGet, demoGuest())
	if page.Code != http.StatusOK || !strings.Contains(page.Body, "gatehouse steward asked") || !strings.Contains(page.Body, "Can I try a land?") {
		t.Fatalf("demo page = %d\n%s", page.Code, page.Body)
	}

	if a := f.demoFix.do(http.MethodPost, demoGuest()); a.Code != http.StatusSeeOther {
		t.Fatalf("ask = %d\n%s", a.Code, a.Body)
	}
	inv := f.invites()
	if len(inv) != 1 || inv[0].Fields["github_id"] != "4242" || inv[0].Fields["invited_by"] != "oidc:stranger@example.com" {
		t.Fatalf("after the person's ask: invites = %v", inv)
	}
	if req := f.d.beads["gd-r1"].Fields; req["state"] != gatehouseDone || req["result_id"] != inv[0].ID {
		t.Fatalf("draft after the ask = %v", req)
	}

	// Asked again once they hold the pass: done, pointing at the one they hold.
	f.message("gd-m2", "gatehouse", time.Minute, true)
	f.request("gd-r2", map[string]string{"kind": "demo", "message_id": "gd-m2"})
	f.r.sweep(context.Background())
	if r2 := f.d.beads["gd-r2"].Fields; len(f.invites()) != 1 || r2["state"] != gatehouseDone || r2["result_id"] != inv[0].ID {
		t.Fatalf("second ask: %d invites, request %v", len(f.invites()), r2)
	}
}

func TestGatehouseReactor_RefusesWhatIsNotAVerifiedAsk(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(f *gatehouseFixture)
		want  string
	}{
		"no stamp":        {func(f *gatehouseFixture) { f.message("gd-m1", "gatehouse", time.Hour, false) }, "signed in with GitHub"},
		"another land":    {func(f *gatehouseFixture) { f.message("gd-m1", "plot-a", time.Hour, true) }, "signed in with GitHub"},
		"an old message":  {func(f *gatehouseFixture) { f.message("gd-m1", "gatehouse", 25*time.Hour, true) }, "signed in with GitHub"},
		"no such message": {func(f *gatehouseFixture) {}, "signed in with GitHub"},
		"terms not taken": {func(f *gatehouseFixture) {
			f.message("gd-m1", "gatehouse", time.Hour, true)
			f.d.beads["gd-user"].Fields["terms_version"] = "old"
		}, "/lands/accept"},
		"demo full": {func(f *gatehouseFixture) {
			f.message("gd-m1", "gatehouse", time.Hour, true)
			f.r.demo.maxGuests = 0
		}, "Every demo guest pass is taken"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGatehouseFixture(t, true)
			tc.setup(f)
			f.request("gd-r1", map[string]string{"kind": "demo", "message_id": "gd-m1"})
			f.r.sweep(context.Background())
			req := f.d.beads["gd-r1"].Fields
			if req["state"] != gatehouseRefused || !strings.Contains(req["outcome"], tc.want) || len(f.invites()) != 0 {
				t.Fatalf("request = %v, %d invites; want refused with %q", req, len(f.invites()), tc.want)
			}
		})
	}
}

func TestGatehouseReactor_ApplicationDraft(t *testing.T) {
	f := newGatehouseFixture(t, true)
	f.message("gd-m1", "gatehouse", time.Hour, true)
	f.request("gd-r1", map[string]string{"kind": "apply", "message_id": "gd-m1", "land_name": "tomato-patch", "purpose": "Keep the docs in step.", "credits_requested_usd": "40"})
	f.request("gd-r2", map[string]string{"kind": "apply", "message_id": "gd-m1", "land_name": "taken-land", "purpose": "p"})
	f.request("gd-r3", map[string]string{"kind": "apply", "message_id": "gd-m1", "land_name": "admin", "purpose": "p"})
	f.d.beads["gd-app"] = &beadsapi.BeadDetail{ID: "gd-app", Type: landsApplicationType, Status: "open", Labels: []string{landsIntakeLabel},
		Fields: map[string]string{"land_name": "taken-land", "state": landsAppSubmitted}, CreatedAt: ghNow}
	f.d.order = append(f.d.order, "gd-app")

	if n := f.r.sweep(context.Background()); n != 3 {
		t.Fatalf("answered %d, want 3", n)
	}
	if r := f.d.beads["gd-r1"].Fields; r["state"] != gatehouseDrafted || r["github_id"] != "4242" || !strings.Contains(r["outcome"], "/lands/apply") {
		t.Fatalf("draft = %v", r)
	}
	if r := f.d.beads["gd-r2"].Fields; r["state"] != gatehouseRefused || !strings.Contains(r["outcome"], "taken") {
		t.Fatalf("taken name = %v", r)
	}
	if r := f.d.beads["gd-r3"].Fields; r["state"] != gatehouseRefused || !strings.Contains(r["outcome"], "reserved") {
		t.Fatalf("reserved name = %v", r)
	}
	for _, b := range f.d.beads {
		if b.Type == landsApplicationType && b.ID != "gd-app" {
			t.Fatalf("the reactor filed an application %s; only the person's own Apply does", b.ID)
		}
	}
}

// The person's own Apply files the drafted application: GET /lands/apply is
// filled in from the draft, and the submit marks it done.
func TestGatehouseReactor_FormPrefillsFromTheDraft(t *testing.T) {
	fx := newApplyFixture(t)
	p := ghStranger()
	fx.intake.beads["gd-gh"] = &beadsapi.BeadDetail{ID: "gd-gh", Type: "project", Title: "Project: gatehouse", Status: "open",
		Fields: map[string]string{"land_kind": "tenant", "land_gatehouse": "true", gatehouseToolsField: "true"}}
	fx.intake.beads["gd-r1"] = &beadsapi.BeadDetail{ID: "gd-r1", Type: gatehouseRequestType, Status: "open", Labels: []string{"project:gatehouse"},
		Fields: map[string]string{"kind": "apply", "state": gatehouseDrafted, "github_id": "4242", "message_id": "gd-m1",
			"land_name": "tomato-patch", "purpose": "Keep the docs in step.", "credits_requested_usd": "40"},
		CreatedAt: ghNow.Add(-time.Hour)}
	fx.intake.beads["gd-m1"] = &beadsapi.BeadDetail{ID: "gd-m1", Type: "message", Status: "open", Labels: []string{"project:gatehouse"},
		Fields:    map[string]string{"text": "I would like a land called tomato-patch for my docs.", "author_github_id": "4242", "author_github_login": "Hubot"},
		CreatedBy: "stranger@example.com", CreatedAt: ghNow.Add(-2 * time.Hour)}
	fx.intake.order = append(fx.intake.order, "gd-gh", "gd-r1", "gd-m1")
	r := newGatehouseReactor("gatehouse", fx.intake, staticTokenSource("machine-token"), nil, fx.srv.landsGate, mustLandsLegal(t).Version, testLogger())
	r.now = func() time.Time { return ghNow }
	fx.srv.landsGatehouseTools = r

	// Another person's draft never fills this person's form.
	other := stranger("other@example.com", "sub-other", 9999)
	if body := fx.do(http.MethodGet, "/lands/apply", other, nil, fx.ghResultCookie(t, other, "octo/other")).Body.String(); strings.Contains(body, "tomato-patch") {
		t.Fatal("another person's form was filled from this draft")
	}

	p.githubID = 4242
	gh := fx.ghResultCookie(t, p, "octo/garden")
	// The form is offered the draft, quoting the message, and is not filled
	// until the person chooses to use it (gd-EaTO1lFPAG).
	page := fx.do(http.MethodGet, "/lands/apply", p, nil, gh)
	body := page.Body.String()
	if page.Code != http.StatusOK || strings.Contains(body, `value="tomato-patch"`) || strings.Contains(body, "Keep the docs in step.") ||
		!strings.Contains(body, "I would like a land called tomato-patch for my docs.") || !strings.Contains(body, "draft=use") {
		t.Fatalf("offered form = %d\n%s", page.Code, body)
	}
	page = fx.do(http.MethodGet, "/lands/apply?draft=use", p, nil, gh)
	body = page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, `value="tomato-patch"`) || !strings.Contains(body, "Keep the docs in step.") ||
		!strings.Contains(body, "I would like a land called tomato-patch for my docs.") {
		t.Fatalf("prefilled form = %d\n%s", page.Code, body)
	}
	// A draft whose message another account wrote is not offered.
	fx.intake.beads["gd-m1"].Fields["author_github_id"] = "9999"
	if body := fx.do(http.MethodGet, "/lands/apply?draft=use", p, nil, gh).Body.String(); strings.Contains(body, `value="tomato-patch"`) {
		t.Fatal("a draft quoting another account's message filled the form")
	}
	fx.intake.beads["gd-m1"].Fields["author_github_id"] = "4242"

	rec := fx.do(http.MethodPost, "/lands/apply", p, applyForm("tomato-patch"), gh)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("apply = %d\n%s", rec.Code, rec.Body.String())
	}
	var appID string
	for _, b := range fx.intake.beads {
		if b.Type == landsApplicationType {
			appID = b.ID
		}
	}
	if d := fx.intake.beads["gd-r1"].Fields; d["state"] != gatehouseDone || d["result_id"] != appID || appID == "" {
		t.Fatalf("draft after apply = %v (application %q)", d, appID)
	}
}
