package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailer"
)

// The plot application (lands_apply.go, lands_apply_reactor.go; Track B §2.4,
// §2.5, step 7.7).

// fakeIntake is the daemon as the Gate sees it: beads in memory, list filters
// applied (unless leaky), decisions cancellable. Every write moves updated_at,
// and a close stamps closed_at with the same instant, as kbeads' CloseBead does.
type fakeIntake struct {
	mu       sync.Mutex
	beads    map[string]*beadsapi.BeadDetail
	keys     map[string]string // idempotency key -> bead id
	order    []string
	n        int
	writes   int
	now      time.Time
	leaky    bool // ignore Fields filters, as a daemon bug would
	canceled []string
	creates  int
}

// touch records a write to b, later than every create and earlier write.
func (f *fakeIntake) touch(b *beadsapi.BeadDetail) {
	f.writes++
	b.UpdatedAt = f.now.Add(24*time.Hour + time.Duration(f.writes)*time.Millisecond)
}

// closeAt closes b in the write that touched it last.
func closeAt(b *beadsapi.BeadDetail) {
	b.Status, b.ClosedAt = "closed", b.UpdatedAt.Format(time.RFC3339Nano)
}

func newFakeIntake() *fakeIntake {
	return &fakeIntake{beads: map[string]*beadsapi.BeadDetail{}, keys: map[string]string{}, now: ghNow}
}

func fieldString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func (f *fakeIntake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := &beadsapi.ListBeadsResult{}
	for _, id := range f.order {
		b := f.beads[id]
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		if !f.leaky {
			for k, v := range q.Fields {
				ok = ok && b.Fields[k] == v
			}
		}
		if !q.CreatedAfter.IsZero() && b.CreatedAt.Before(q.CreatedAfter) {
			ok = false
		}
		if !ok {
			continue
		}
		res.Total++
		if !q.CountOnly {
			c := *b
			c.Fields = map[string]string{}
			for k, v := range b.Fields {
				c.Fields[k] = v
			}
			res.Beads = append(res.Beads, &c)
		}
	}
	return res, nil
}

func (f *fakeIntake) CreateBeadDetail(_ context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.keys[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		return f.beads[id], nil
	}
	f.creates++
	f.n++
	id := fmt.Sprintf("gd-%s%d", req.Type[:3], f.n)
	var raw map[string]any
	_ = json.Unmarshal(req.Fields, &raw)
	fields := map[string]string{}
	for k, v := range raw {
		fields[k] = fieldString(v)
	}
	b := &beadsapi.BeadDetail{ID: id, Title: req.Title, Type: req.Type, Kind: req.Kind, Status: "open", Labels: req.Labels,
		Fields: fields, CreatedBy: "machine:gardenio-beads-viewer", CreatedAt: f.now.Add(time.Duration(f.n) * time.Second)}
	b.UpdatedAt = b.CreatedAt
	f.beads[id] = b
	f.order = append(f.order, id)
	if req.IdempotencyKey != "" {
		f.keys[req.IdempotencyKey] = id
	}
	return b, nil
}

func (f *fakeIntake) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.beads[id]
	if !ok {
		return &beadsapi.APIError{StatusCode: http.StatusNotFound}
	}
	for k, v := range fields {
		b.Fields[k] = fieldString(v)
	}
	f.touch(b)
	return nil
}

func (f *fakeIntake) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.beads[id]
	if !ok {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound}
	}
	c := *b
	return &c, nil
}

func (f *fakeIntake) CancelDecision(_ context.Context, id, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, id)
	if b, ok := f.beads[id]; ok {
		b.Fields["chosen"] = "dismissed"
		f.touch(b)
		closeAt(b)
	}
	return nil
}

// edit writes to decision id as its owner may, open or closed: a PATCH of its
// fields (gd-qZER8OOiIb).
func (f *fakeIntake) edit(id string, fn func(f map[string]string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.beads[id]
	fn(b.Fields)
	f.touch(b)
}

// resolve answers decision id as the approver (LANDS_APPROVER in
// newApplyFixture) would, in their own OIDC session.
func (f *fakeIntake) resolve(id, chosen, text string) {
	f.resolveAs(id, chosen, text, "op@example.com", false)
}

// resolveAs answers decision id as kbeads' resolve handler records it:
// responded_by and the daemon-stamped resolver_subject / resolver_admin.
func (f *fakeIntake) resolveAs(id, chosen, text, resolver string, admin bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.beads[id]
	if chosen != "" {
		b.Fields["chosen"] = chosen
	}
	if text != "" {
		b.Fields["response_text"] = text
	}
	b.Fields["responded_by"] = resolver
	b.Fields["responded_at"] = "2026-10-19T19:00:00Z"
	b.Fields["resolver_subject"] = resolver
	b.Fields["resolver_admin"] = strconv.FormatBool(admin)
	f.touch(b)
	closeAt(b)
}

// plant files a bead as another principal would, under key.
func (f *fakeIntake) plant(key, createdBy string, fields map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("gd-planted%d", f.n)
	f.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "decision", Status: "open", Labels: []string{landsIntakeLabel},
		Fields: fields, CreatedBy: createdBy, CreatedAt: f.now}
	f.order = append(f.order, id)
	if key != "" {
		f.keys[key] = id
	}
	return id
}

func (f *fakeIntake) of(typ string) []*beadsapi.BeadDetail {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, id := range f.order {
		if f.beads[id].Type == typ {
			out = append(out, f.beads[id])
		}
	}
	return out
}

// seedApplication files an application directly, as another applicant's.
func (f *fakeIntake) seedApplication(sub, land, repo, state string) string {
	raw, _ := json.Marshal(map[string]any{"applicant_sub": sub, "applicant_email": sub + "@example.com", "land_name": land, "repo": repo, "state": state, "github_login": sub})
	b, _ := f.CreateBeadDetail(context.Background(), beadsapi.CreateBeadRequest{Type: landsApplicationType, Labels: []string{landsIntakeLabel}, Fields: raw})
	return b.ID
}

// applyFixture is the viewer with the Lands App and the Gate configured.
type applyFixture struct {
	h      http.Handler
	srv    *Server
	gh     *landsGitHub
	intake *fakeIntake
	mail   *mailer.Recorder
	terms  *fakeLandsTerms
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	h, g, srv := ghViewerServer(t, &fakeLandsGitHubApp{})
	terms := &fakeLandsTerms{version: mustLandsLegal(t).Version, acceptedAt: "2026-10-19T15:20:00Z"}
	srv.landsTerms = terms
	intake := newFakeIntake()
	rec := mailer.NewRecorder()
	gate, err := landsGateFromEnv(func(k string) string {
		if k == landsApproverEnv {
			return "oidc:op@example.com"
		}
		return ""
	}, intake, nil, rec, mailer.NewTemplates(mailer.Config{BaseURL: "https://gardenio.example.com"}), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	gate.now = func() time.Time { return ghNow }
	srv.landsGate = gate
	return &applyFixture{h: h, srv: srv, gh: g, intake: intake, mail: rec, terms: terms}
}

// stranger is an applicant with a subject, signed in through GitHub.
func stranger(email, sub string, githubID int64) principal {
	p := applicantPrincipal()
	p.email, p.sub, p.githubID = email, sub, githubID
	return p
}

func ghStranger() principal { return stranger("stranger@example.com", "sub-stranger", ghApplicantID) }

// ghResultCookie is a passed setup check for p, on repo.
func (fx *applyFixture) ghResultCookie(t *testing.T, p principal, repo string) *http.Cookie {
	t.Helper()
	v, err := fx.gh.sign(landsGitHubResult{
		Email: p.email, GitHubID: p.githubID, GitHubLogin: "octocat", InstallationID: 777, Repo: repo, RepoID: 1001,
		Visibility: "public", AccountCreated: time.Date(2015, 1, 2, 0, 0, 0, 0, time.UTC), PublicRepos: 12,
		Expires: ghNow.Add(landsGitHubResultTTL).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: landsGitHubResultCookie, Value: v}
}

func (fx *applyFixture) do(method, path string, p principal, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r = r.WithContext(requestAs(method, path, p).Context())
	r.Header.Set("X-Auth-Request-Access-Token", "stranger-access-token")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, r)
	return rec
}

func applyForm(name string) url.Values {
	return url.Values{"land_name": {name}, "purpose": {"Keep the docs in step with the code."}, "credits": {"40"}}
}

func TestLandsApply_Flow(t *testing.T) {
	fx := newApplyFixture(t)
	p := ghStranger()
	gh := fx.ghResultCookie(t, p, "octo/garden")

	page := fx.do(http.MethodGet, "/lands/apply", p, nil, gh)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "octo/garden") || !strings.Contains(page.Body.String(), `name="land_name"`) {
		t.Fatalf("form = %d\n%s", page.Code, page.Body.String())
	}

	// The repository comes from the signed setup result; a form field cannot change it.
	form := applyForm("Tomato-Patch")
	form.Set("repo", "evil/other")
	rec := fx.do(http.MethodPost, "/lands/apply", p, form, gh)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/lands/applications?submitted=1" {
		t.Fatalf("apply = %d %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if c := ghCookie(rec, landsGitHubResultCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("the setup result cookie is not cleared after applying: %+v", c)
	}

	apps := fx.intake.of(landsApplicationType)
	if len(apps) != 1 {
		t.Fatalf("%d applications filed", len(apps))
	}
	a := apps[0]
	want := map[string]string{
		"applicant_sub": "sub-stranger", "applicant_email": "stranger@example.com", "github_login": "octocat",
		"github_id": strconv.FormatInt(ghApplicantID, 10), "land_name": "tomato-patch", "repo": "octo/garden",
		"repo_private": "false", "github_installation_id": "777", "credits_requested_usd": "40", "state": "submitted",
		"terms_version": fx.terms.version, "aup_version": fx.terms.version, "github_public_repos": "12",
	}
	for k, v := range want {
		if a.Fields[k] != v {
			t.Errorf("application %s = %q, want %q", k, a.Fields[k], v)
		}
	}
	if !slices.Equal(a.Labels, []string{landsIntakeLabel}) {
		t.Errorf("application labels = %v", a.Labels)
	}

	decs := fx.intake.of("decision")
	if len(decs) != 1 {
		t.Fatalf("%d decisions filed", len(decs))
	}
	d := decs[0]
	if a.Fields["decision_id"] != d.ID || d.Fields["requesting_task"] != a.ID || !slices.Equal(d.Labels, []string{landsIntakeLabel}) || d.Fields["operated_by"] != "oidc:op@example.com" {
		t.Errorf("decision %+v does not belong to application %s", d, a.ID)
	}
	var opts []map[string]string
	if err := json.Unmarshal([]byte(d.Fields["options"]), &opts); err != nil || len(opts) != 3 || opts[0]["id"] != "approve" || opts[1]["id"] != "deny" || opts[2]["id"] != "ask" {
		t.Errorf("options = %s (%v)", d.Fields["options"], err)
	}
	for _, w := range []string{"stranger@example.com", "octocat", "https://github.com/octo/garden (public)", "Keep the docs in step", "Credits asked for: 40", "public repositories"} {
		if !strings.Contains(d.Fields["context"], w) {
			t.Errorf("decision context lacks %q:\n%s", w, d.Fields["context"])
		}
	}

	sent := fx.mail.Sent()
	if len(sent) != 1 || sent[0].Kind != mailer.KindApplicationReceived || sent[0].To[0] != "stranger@example.com" || !strings.Contains(sent[0].Subject, "tomato-patch") {
		t.Fatalf("mail = %+v", sent)
	}

	list := fx.do(http.MethodGet, "/lands/applications?submitted=1", p, nil)
	body := list.Body.String()
	for _, w := range []string{"tomato-patch", "Waiting for the operator", "octo/garden", "/lands/applications/" + a.ID + "/withdraw", "We have emailed you a copy"} {
		if !strings.Contains(body, w) {
			t.Errorf("applications page lacks %q", w)
		}
	}
}

// With the Gate on, a connected repository leads straight on to the form.
func TestLandsGitHub_VerifiedLeadsToTheForm(t *testing.T) {
	fx := newApplyFixture(t)
	fx.gh.app = &fakeLandsGitHubApp{result: ghVerified()}
	state := ghStart(t, fx.h)
	rec := ghDo(fx.h, ghSetupPath(state.Value, nil), ghApplicant(), state)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `href="/lands/apply"`) || strings.Contains(rec.Body.String(), "not open yet") {
		t.Fatalf("verified setup page = %d\n%s", rec.Code, rec.Body.String())
	}
}

func TestLandsApply_OneOpenApplicationPerApplicant(t *testing.T) {
	fx := newApplyFixture(t)
	p := ghStranger()
	if rec := fx.do(http.MethodPost, "/lands/apply", p, applyForm("tomato-patch"), fx.ghResultCookie(t, p, "octo/garden")); rec.Code != http.StatusSeeOther {
		t.Fatalf("first = %d", rec.Code)
	}
	rec := fx.do(http.MethodPost, "/lands/apply", p, applyForm("bean-row"), fx.ghResultCookie(t, p, "octo/beans"))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "You have an open application") {
		t.Fatalf("second = %d\n%s", rec.Code, rec.Body.String())
	}
	if n := len(fx.intake.of(landsApplicationType)); n != 1 {
		t.Fatalf("%d applications after a refused second", n)
	}
	page := fx.do(http.MethodGet, "/lands/apply", p, nil, fx.ghResultCookie(t, p, "octo/beans"))
	if !strings.Contains(page.Body.String(), "tomato-patch") || strings.Contains(page.Body.String(), `name="land_name"`) {
		t.Fatalf("the form is offered beside an open application:\n%s", page.Body.String())
	}

	// Once that one is answered, the applicant may apply again.
	app := fx.intake.of(landsApplicationType)[0]
	_ = fx.intake.UpdateBeadFieldsTyped(context.Background(), app.ID, map[string]any{"state": "denied"})
	if rec := fx.do(http.MethodPost, "/lands/apply", p, applyForm("bean-row"), fx.ghResultCookie(t, p, "octo/beans")); rec.Code != http.StatusSeeOther {
		t.Fatalf("after a denial = %d\n%s", rec.Code, rec.Body.String())
	}
}

func TestLandsApply_Refusals(t *testing.T) {
	p := ghStranger()
	cases := []struct {
		name   string
		setup  func(fx *applyFixture)
		form   url.Values
		cookie bool
		status int
		want   string
	}{
		{"repo held by another application", func(fx *applyFixture) { fx.intake.seedApplication("sub-other", "other-land", "octo/garden", "submitted") }, applyForm("tomato-patch"), true, http.StatusConflict, "already has a land or an open application"},
		{"repo already a land", func(fx *applyFixture) { fx.intake.seedApplication("sub-other", "other-land", "octo/garden", "provisioned") }, applyForm("tomato-patch"), true, http.StatusConflict, "already has a land"},
		{"name taken", func(fx *applyFixture) { fx.intake.seedApplication("sub-other", "tomato-patch", "octo/else", "approved") }, applyForm("tomato-patch"), true, http.StatusConflict, "is taken"},
		{"daily limit", func(fx *applyFixture) {
			fx.srv.landsGate.dailyLimit = 1
			fx.intake.seedApplication("sub-other", "other-land", "octo/else", "denied")
		}, applyForm("tomato-patch"), true, http.StatusTooManyRequests, "as many applications as we can read today"},
		{"reserved name", nil, applyForm("gardenio"), true, http.StatusBadRequest, "reserved"},
		{"bad name", nil, applyForm("x"), true, http.StatusBadRequest, "3 to 30 characters"},
		{"no purpose", nil, url.Values{"land_name": {"tomato-patch"}}, true, http.StatusBadRequest, "what you want the agents to work on"},
		{"long purpose", nil, url.Values{"land_name": {"tomato-patch"}, "purpose": {strings.Repeat("é", 1001)}}, true, http.StatusBadRequest, "1000 characters"},
		{"bad credits", nil, url.Values{"land_name": {"tomato-patch"}, "purpose": {"x"}, "credits": {"-3"}}, true, http.StatusBadRequest, "Credits asked for"},
		{"no repository connected", nil, applyForm("tomato-patch"), false, http.StatusConflict, "Connect your repository first"},
		{"terms not accepted", func(fx *applyFixture) { fx.terms.version = "" }, applyForm("tomato-patch"), true, http.StatusConflict, "Accept the terms first"},
		{"terms stale", func(fx *applyFixture) { fx.terms.version = "v0" }, applyForm("tomato-patch"), true, http.StatusConflict, "have changed since you accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newApplyFixture(t)
			if tc.setup != nil {
				tc.setup(fx)
			}
			before := len(fx.intake.of(landsApplicationType))
			var cookies []*http.Cookie
			if tc.cookie {
				cookies = append(cookies, fx.ghResultCookie(t, p, "octo/garden"))
			}
			rec := fx.do(http.MethodPost, "/lands/apply", p, tc.form, cookies...)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("= %d, want %d with %q\n%s", rec.Code, tc.status, tc.want, rec.Body.String())
			}
			if n := len(fx.intake.of(landsApplicationType)); n != before {
				t.Fatalf("a refused application was filed (%d → %d)", before, n)
			}
			if len(fx.mail.Sent()) != 0 {
				t.Fatalf("a refused application sent mail")
			}
		})
	}

	// Another applicant's setup result is not yours.
	fx := newApplyFixture(t)
	other := stranger("other@example.com", "sub-other", 99)
	rec := fx.do(http.MethodPost, "/lands/apply", p, applyForm("tomato-patch"), fx.ghResultCookie(t, other, "octo/garden"))
	if rec.Code != http.StatusConflict || len(fx.intake.of(landsApplicationType)) != 0 {
		t.Fatalf("someone else's setup result = %d", rec.Code)
	}
	// A cross-site form is refused before anything is read.
	r := httptest.NewRequest(http.MethodPost, "/lands/apply", strings.NewReader(applyForm("tomato-patch").Encode()))
	r = r.WithContext(requestAs(http.MethodPost, "/lands/apply", p).Context())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(fx.ghResultCookie(t, p, "octo/garden"))
	w := httptest.NewRecorder()
	fx.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(fx.intake.of(landsApplicationType)) != 0 {
		t.Fatalf("cross-site apply = %d", w.Code)
	}
}

// A refusal about one field shows at that field, between its hint and the
// field, so it stays in view above a phone's keyboard while the field is fixed;
// the alert at the top links down to it (gd-KiRStJd56I). A refusal about no
// field stays at the top alone.
func TestLandsApply_ErrorAtItsField(t *testing.T) {
	p := ghStranger()
	cases := []struct {
		name  string
		setup func(fx *applyFixture)
		form  url.Values
		field string
	}{
		{"bad name", nil, applyForm("x"), "land_name"},
		{"name taken", func(fx *applyFixture) { fx.intake.seedApplication("sub-other", "tomato-patch", "octo/else", "approved") }, applyForm("tomato-patch"), "land_name"},
		{"no purpose", nil, url.Values{"land_name": {"tomato-patch"}}, "purpose"},
		{"bad credits", nil, url.Values{"land_name": {"tomato-patch"}, "purpose": {"x"}, "credits": {"-3"}}, "credits"},
		{"repo held", func(fx *applyFixture) { fx.intake.seedApplication("sub-other", "other-land", "octo/garden", "submitted") }, applyForm("tomato-patch"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newApplyFixture(t)
			if tc.setup != nil {
				tc.setup(fx)
			}
			body := fx.do(http.MethodPost, "/lands/apply", p, tc.form, fx.ghResultCookie(t, p, "octo/garden")).Body.String()
			for _, f := range []string{"land_name", "purpose", "credits"} {
				hint, errAt, field := strings.Index(body, `id="`+f+`-hint"`), strings.Index(body, `id="`+f+`-error"`), strings.Index(body, `id="`+f+`" name=`)
				invalid := strings.Contains(body[field:field+strings.Index(body[field:], ">")], `aria-invalid="true"`)
				if f != tc.field {
					if errAt >= 0 || invalid {
						t.Errorf("%s carries an error it is not about (error at %d, invalid %v)", f, errAt, invalid)
					}
					continue
				}
				if hint < 0 || !(hint < errAt && errAt < field) || !invalid {
					t.Errorf("%s: hint at %d, error at %d, field at %d, invalid %v; want the error between hint and field, invalid", f, hint, errAt, field, invalid)
				}
				if !strings.Contains(body, `<a href="#`+f+`-error">Go to the field</a>`) {
					t.Errorf("the alert does not link to %s's error", f)
				}
			}
			if tc.field == "" && strings.Contains(body, "Go to the field") {
				t.Errorf("a refusal about no field links to one")
			}
		})
	}
}

// Without the Gate the pages answer 503 and say so; the GitHub page keeps its
// "not open yet" wording.
func TestLandsApply_ClosedWithoutGate(t *testing.T) {
	h, _, _ := ghViewerServer(t, &fakeLandsGitHubApp{})
	for _, path := range []string{"/lands/apply", "/lands/applications"} {
		rec := ghDo(h, path, ghStranger())
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not open on this server yet") {
			t.Errorf("GET %s = %d\n%s", path, rec.Code, rec.Body.String())
		}
	}
}

// An applicant never sees, nor withdraws, another's application: the list is
// keyed on the caller's subject and re-checked even when the daemon ignores
// the filter.
func TestLandsApplications_OnlyTheCallersOwn(t *testing.T) {
	fx := newApplyFixture(t)
	mine := fx.intake.seedApplication("sub-stranger", "tomato-patch", "octo/garden", "submitted")
	theirs := fx.intake.seedApplication("sub-other", "secret-land", "rival/private", "submitted")
	p := ghStranger()

	for _, leaky := range []bool{false, true} {
		fx.intake.leaky = leaky
		body := fx.do(http.MethodGet, "/lands/applications", p, nil).Body.String()
		if !strings.Contains(body, "tomato-patch") || strings.Contains(body, "secret-land") || strings.Contains(body, "rival/private") || strings.Contains(body, theirs) {
			t.Fatalf("leaky=%v: the list shows someone else's application:\n%s", leaky, body)
		}
	}

	rec := fx.do(http.MethodPost, "/lands/applications/"+theirs+"/withdraw", p, url.Values{})
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "secret-land") {
		t.Fatalf("withdrawing another's = %d\n%s", rec.Code, rec.Body.String())
	}
	if got, _ := fx.intake.GetBead(context.Background(), theirs); got.Fields["state"] != "submitted" {
		t.Fatalf("another's application moved to %q", got.Fields["state"])
	}
	if rec := fx.do(http.MethodPost, "/lands/applications/gd-nope/withdraw", p, url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("withdrawing a missing id = %d", rec.Code)
	}
	if len(fx.mail.Sent()) != 0 {
		t.Fatal("a refused withdrawal sent mail")
	}
	_ = mine
}

func TestLandsApplications_Withdraw(t *testing.T) {
	fx := newApplyFixture(t)
	p := ghStranger()
	if rec := fx.do(http.MethodPost, "/lands/apply", p, applyForm("tomato-patch"), fx.ghResultCookie(t, p, "octo/garden")); rec.Code != http.StatusSeeOther {
		t.Fatalf("apply = %d", rec.Code)
	}
	app := fx.intake.of(landsApplicationType)[0]
	rec := fx.do(http.MethodPost, "/lands/applications/"+app.ID+"/withdraw", p, url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/lands/applications?withdrawn=1" {
		t.Fatalf("withdraw = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	got, _ := fx.intake.GetBead(context.Background(), app.ID)
	if got.Fields["state"] != "withdrawn" || !slices.Equal(fx.intake.canceled, []string{app.Fields["decision_id"]}) {
		t.Fatalf("state %q, canceled %v", got.Fields["state"], fx.intake.canceled)
	}
	sent := fx.mail.Sent()
	if len(sent) != 2 || sent[1].Kind != mailer.KindApplicationWithdrawn {
		t.Fatalf("mail = %+v", sent)
	}
	again := fx.do(http.MethodPost, "/lands/applications/"+app.ID+"/withdraw", p, url.Values{})
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), "this one is withdrawn") {
		t.Fatalf("withdrawing twice = %d\n%s", again.Code, again.Body.String())
	}
	// The reactor leaves a withdrawn application alone even if its decision is answered.
	fx.intake.resolve(app.Fields["decision_id"], "approve", "")
	if moved := fx.srv.landsGate.sweep(context.Background()); moved != 0 {
		t.Fatalf("the reactor moved a withdrawn application")
	}
}

// The reactor applies approve, deny and ask from a resolved decision (§2.5).
func TestLandsApplyReactor_ResolvedDecisions(t *testing.T) {
	type outcome struct {
		state, granted, model, reason, question string
		mail                                    mailer.Kind
		refiled                                 bool
	}
	cases := []struct {
		name         string
		chosen, text string
		want         outcome
	}{
		{"approve with terms", "approve", "credits=50 model=opus", outcome{state: "approved", granted: "50", model: landsModelOpus, mail: mailer.KindApplicationApproved}},
		{"approve plain", "approve", "", outcome{state: "approved", granted: "25", model: landsModelSonnet, mail: mailer.KindApplicationApproved}},
		{"approve with plain-words terms", "approve", "give model opus, and give 500 credits", outcome{state: "approved", granted: "500", model: landsModelOpus, mail: mailer.KindApplicationApproved}},
		{"approve with credits before the number", "approve", "credits 40 please", outcome{state: "approved", granted: "40", model: landsModelSonnet, mail: mailer.KindApplicationApproved}},
		{"approve capped", "approve", "credits=999999", outcome{state: "approved", granted: "10000", model: landsModelSonnet, mail: mailer.KindApplicationApproved}},
		{"deny with a reason", "deny", "We cannot host crypto miners.", outcome{state: "denied", reason: "We cannot host crypto miners.", mail: mailer.KindApplicationDenied}},
		{"deny by free text", "", "deny: looks like spam", outcome{state: "denied", reason: "looks like spam", mail: mailer.KindApplicationDenied}},
		{"ask", "ask", "Which branch should the agents use?", outcome{state: "submitted", question: "Which branch should the agents use?", mail: mailer.KindApplicationQuestion, refiled: true}},
		{"ask without a question", "ask", "", outcome{state: "submitted", refiled: true}},
		{"dismissed", "dismissed", "", outcome{state: "submitted", refiled: true}},
		{"free text that is not an answer", "", "hmm, not sure", outcome{state: "submitted", refiled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newApplyFixture(t)
			p := ghStranger()
			if rec := fx.do(http.MethodPost, "/lands/apply", p, applyForm("tomato-patch"), fx.ghResultCookie(t, p, "octo/garden")); rec.Code != http.StatusSeeOther {
				t.Fatalf("apply = %d", rec.Code)
			}
			app := fx.intake.of(landsApplicationType)[0]
			first := app.Fields["decision_id"]

			// An open decision changes nothing.
			if moved := fx.srv.landsGate.sweep(context.Background()); moved != 0 {
				t.Fatalf("an unanswered decision moved the application")
			}
			fx.intake.resolve(first, tc.chosen, tc.text)
			if moved := fx.srv.landsGate.sweep(context.Background()); moved != 1 {
				t.Fatalf("sweep moved %d", moved)
			}
			got, _ := fx.intake.GetBead(context.Background(), app.ID)
			f := got.Fields
			if f["state"] != tc.want.state || f["credits_granted_usd"] != tc.want.granted || f["model"] != tc.want.model || f["deny_reason"] != tc.want.reason || f["question"] != tc.want.question {
				t.Fatalf("application fields = %v, want %+v", f, tc.want)
			}
			if tc.want.state == "approved" || tc.want.state == "denied" {
				if f["decided_by"] != "op@example.com" || f["decided_at"] != "2026-10-19T19:00:00Z" {
					t.Errorf("decided_by/at = %q %q", f["decided_by"], f["decided_at"])
				}
			}
			refiled := f["decision_id"] != first
			if refiled != tc.want.refiled {
				t.Fatalf("refiled = %v (decision %s → %s)", refiled, first, f["decision_id"])
			}
			if refiled {
				d, _ := fx.intake.GetBead(context.Background(), f["decision_id"])
				if d.Status != "open" || d.Fields["requesting_task"] != app.ID {
					t.Fatalf("the fresh decision = %+v", d)
				}
				if tc.chosen == "" && tc.text != "" && !strings.Contains(d.Fields["context"], "picked no option") {
					t.Errorf("the fresh decision does not say the reply was not acted on:\n%s", d.Fields["context"])
				}
				if tc.want.question != "" && !strings.Contains(d.Fields["context"], tc.want.question) {
					t.Errorf("the fresh decision does not repeat the question:\n%s", d.Fields["context"])
				}
			}
			sent := fx.mail.Sent()
			var kinds []mailer.Kind
			for _, m := range sent[1:] {
				kinds = append(kinds, m.Kind)
			}
			if tc.want.mail == "" && len(kinds) != 0 || tc.want.mail != "" && !slices.Equal(kinds, []mailer.Kind{tc.want.mail}) {
				t.Fatalf("mail after the answer = %v, want %q", kinds, tc.want.mail)
			}
			if tc.want.mail == mailer.KindApplicationApproved && !strings.Contains(sent[1].Text, "$"+tc.want.granted) {
				t.Errorf("approval mail does not state the grant:\n%s", sent[1].Text)
			}
			if tc.want.reason != "" && !strings.Contains(sent[1].Text, tc.want.reason) {
				t.Errorf("denial mail does not carry the reason:\n%s", sent[1].Text)
			}

			// A second sweep is quiet: an answered application has left the set,
			// and a re-filed one waits on its fresh, open decision.
			if moved := fx.srv.landsGate.sweep(context.Background()); moved != 0 {
				t.Fatalf("a second sweep moved %d", moved)
			}
			// The page says what the mail said.
			body := fx.do(http.MethodGet, "/lands/applications", p, nil).Body.String()
			for _, w := range []string{tc.want.reason, tc.want.question} {
				if w != "" && !strings.Contains(body, w) {
					t.Errorf("applications page lacks %q", w)
				}
			}
		})
	}
}

// An application whose decision could not be filed at submit gets one from the
// next sweep, and only one however often it sweeps.
func TestLandsApplyReactor_FilesAMissingDecision(t *testing.T) {
	fx := newApplyFixture(t)
	id := fx.intake.seedApplication("sub-stranger", "tomato-patch", "octo/garden", "submitted")
	for i := 0; i < 3; i++ {
		fx.srv.landsGate.sweep(context.Background())
	}
	got, _ := fx.intake.GetBead(context.Background(), id)
	if got.Fields["decision_id"] == "" || len(fx.intake.of("decision")) != 1 {
		t.Fatalf("decision_id %q, %d decisions", got.Fields["decision_id"], len(fx.intake.of("decision")))
	}
}

// applied files one application through the form and returns it with its first
// decision.
func (fx *applyFixture) applied(t *testing.T) (app *beadsapi.BeadDetail, decision string) {
	t.Helper()
	p := ghStranger()
	if rec := fx.do(http.MethodPost, "/lands/apply", p, applyForm("tomato-patch"), fx.ghResultCookie(t, p, "octo/garden")); rec.Code != http.StatusSeeOther {
		t.Fatalf("apply = %d", rec.Code)
	}
	app = fx.intake.of(landsApplicationType)[0]
	return app, app.Fields["decision_id"]
}

// pinned reports why the application's recorded decision is not pinned to
// what it asks, or "".
func (fx *applyFixture) pinned(appID string) string {
	f := fx.intake.beads[appID].Fields
	d := fx.intake.beads[f["decision_id"]]
	if d == nil {
		return "no decision " + f["decision_id"]
	}
	if got, ok := landsDecisionDigest(d.Fields); !ok || f["decision_digest"] == "" || got != f["decision_digest"] {
		return fmt.Sprintf("decision_digest %q, decision %s digests to %q", f["decision_digest"], d.ID, got)
	}
	return ""
}

// A decision planted under the Gate's predictable key by another principal (an
// agent on the shared token, say, with option "approve" labelled "Deny: spam")
// is never recorded: the Gate files its own under a fresh key, and an approval
// on the planted one moves nothing. created_by proves nothing: a static-token
// caller forges "machine:gardenio-beads-viewer" with X-Actor (gd-qZER8OOiIb).
func TestLandsApply_PlantedDecisionIsRefused(t *testing.T) {
	fx := newApplyFixture(t)
	app, _ := fx.applied(t)
	planted := map[string]struct {
		by     string
		fields func(after string) map[string]string
	}{
		"created_by forged via X-Actor, options relabelled": {"machine:gardenio-beads-viewer", func(string) map[string]string {
			return map[string]string{"requesting_task": app.ID, "options": `[{"id":"approve","label":"Deny: spam"}]`}
		}},
		"the Gate's own words for another application": {"machine:gardenio-beads-viewer", func(after string) map[string]string {
			f := maps.Clone(fx.intake.beads[after].Fields)
			f["requesting_task"] = "gd-plo999"
			return f
		}},
		"the Gate's words with a relabelled option": {"machine:gardenio-beads-viewer", func(after string) map[string]string {
			f := maps.Clone(fx.intake.beads[after].Fields)
			f["options"] = strings.Replace(f["options"], `"label":"Approve:`, `"label":"Deny: spam. Approve:`, 1)
			return f
		}},
		"another creator": {"static", func(string) map[string]string {
			return map[string]string{"requesting_task": app.ID, "options": `[{"id":"approve","label":"Approve"}]`}
		}},
	}
	for name, p := range planted {
		t.Run(name, func(t *testing.T) {
			after := fx.intake.beads[app.ID].Fields["decision_id"]
			id := fx.intake.plant("lands-gate:"+app.ID+":after:"+after, p.by, p.fields(after))
			// The operator asks; the re-file hits the planted key.
			fx.intake.resolve(after, "ask", "Which branch?")
			fx.srv.landsGate.sweep(context.Background())
			got := fx.intake.beads[app.ID].Fields["decision_id"]
			if got == id || got == after {
				t.Fatalf("decision_id = %s (planted %s, previous %s)", got, id, after)
			}
			if d := fx.intake.beads[got]; d.Fields["requesting_task"] != app.ID || d.Status != "open" {
				t.Fatalf("recorded decision %+v", d)
			}
			if why := fx.pinned(app.ID); why != "" {
				t.Fatal(why)
			}
			// Answering the planted one does nothing.
			fx.intake.resolve(id, "approve", "credits=10000")
			fx.srv.landsGate.sweep(context.Background())
			if st := fx.intake.beads[app.ID].Fields["state"]; st != landsAppSubmitted {
				t.Fatalf("state = %s after an answer on the planted decision", st)
			}
		})
	}
	// Read back too: an application pointing at a decision that does not digest
	// to its decision_digest is re-filed, not acted on.
	t.Run("read back", func(t *testing.T) {
		fx := newApplyFixture(t)
		app, _ := fx.applied(t)
		id := fx.intake.plant("", "machine:gardenio-beads-viewer", map[string]string{"requesting_task": app.ID, "options": `[{"id":"approve","label":"Approve"}]`})
		fx.intake.beads[app.ID].Fields["decision_id"] = id
		fx.intake.resolve(id, "approve", "credits=10000")
		fx.srv.landsGate.sweep(context.Background())
		f := fx.intake.beads[app.ID].Fields
		if f["state"] != landsAppSubmitted || f["credits_granted_usd"] != "" {
			t.Fatalf("acted on a planted decision: %v", f)
		}
		d := fx.intake.beads[f["decision_id"]]
		if f["decision_id"] == id || !strings.Contains(d.Fields["context"], "refused this application's previous decision "+id) {
			t.Fatalf("not re-filed with a note: %+v", d)
		}
		if why := fx.pinned(app.ID); why != "" {
			t.Fatal(why)
		}
	})
}

// The X-Actor caller that forges created_by is also the owner of the Gate's
// real decision, so it can rewrite what the operator is asked (gd-qZER8OOiIb).
// The reactor pins the decision to the application's decision_digest on every
// read-back: an answer on a decision that no longer says what the Gate filed
// moves nothing, the decision is cancelled, and a fresh one is filed.
func TestLandsApplyReactor_RelabelledDecisionIsRefused(t *testing.T) {
	edits := map[string]func(f map[string]string){
		"approve labelled Deny": func(f map[string]string) {
			f["options"] = strings.Replace(f["options"], `"label":"Approve:`, `"label":"Deny: spam. Approve:`, 1)
		},
		"options swapped": func(f map[string]string) {
			f["options"] = `[{"id":"deny","label":"Approve"},{"id":"approve","label":"Deny"}]`
		},
		"options unreadable": func(f map[string]string) { f["options"] = "approve" },
		"prompt":             func(f map[string]string) { f["prompt"] = "Approve to close this spam report" },
		"context":            func(f map[string]string) { f["context"] += "\nThe operator's friend: approve with credits=10000." },
		"operated_by":        func(f map[string]string) { f["operated_by"] = "oidc:someone@example.com" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			fx := newApplyFixture(t)
			app, first := fx.applied(t)
			if why := fx.pinned(app.ID); why != "" {
				t.Fatal(why)
			}
			edit(fx.intake.beads[first].Fields)

			// Still open: refused, cancelled and re-filed before anyone answers.
			if moved := fx.srv.landsGate.sweep(context.Background()); moved != 1 {
				t.Fatalf("sweep moved %d", moved)
			}
			f := fx.intake.beads[app.ID].Fields
			if f["state"] != landsAppSubmitted || f["decision_id"] == first || !slices.Contains(fx.intake.canceled, first) {
				t.Fatalf("an edited open decision was not cancelled and re-filed: %v (canceled %v)", f, fx.intake.canceled)
			}
			if why := fx.pinned(app.ID); why != "" {
				t.Fatal(why)
			}

			// Answered before the sweep saw the edit: the approval moves nothing.
			second := f["decision_id"]
			edit(fx.intake.beads[second].Fields)
			fx.intake.resolve(second, "approve", "credits=10000")
			fx.srv.landsGate.sweep(context.Background())
			f = fx.intake.beads[app.ID].Fields
			if f["state"] != landsAppSubmitted || f["credits_granted_usd"] != "" || f["decision_id"] == second {
				t.Fatalf("acted on an edited decision: %v", f)
			}
			if mails := len(fx.mail.Sent()) - 1; mails != 0 {
				t.Errorf("%d mails after an answer on an edited decision", mails)
			}
			if d := fx.intake.beads[f["decision_id"]]; !strings.Contains(d.Fields["context"], "refused this application's previous decision "+second) {
				t.Fatalf("no note on the fresh decision:\n%s", d.Fields["context"])
			}

			// The fresh decision is the Gate's own words, and an answer on it counts.
			fx.intake.resolve(f["decision_id"], "approve", "")
			fx.srv.landsGate.sweep(context.Background())
			if st := fx.intake.beads[app.ID].Fields["state"]; st != landsAppApproved {
				t.Fatalf("state = %s after the approver answered the fresh decision", st)
			}
		})
	}
}

// A closed decision stays editable and the pin is read at sweep time, so an
// owner could relabel the options, let the approver answer, and put the Gate's
// words back before the sweep. A decision written after it closed counts for
// nothing, whatever it says by then.
func TestLandsApplyReactor_EditAnswerRestoreIsRefused(t *testing.T) {
	relabels := map[string]func(f map[string]string){
		"approve labelled Deny": func(f map[string]string) {
			f["options"] = strings.Replace(f["options"], `"label":"Approve:`, `"label":"Deny: spam. Approve:`, 1)
		},
		"options swapped": func(f map[string]string) {
			f["options"] = `[{"id":"deny","label":"Approve"},{"id":"approve","label":"Deny"}]`
		},
		"prompt": func(f map[string]string) { f["prompt"] = "Approve to close this spam report" },
	}
	for name, relabel := range relabels {
		t.Run(name, func(t *testing.T) {
			fx := newApplyFixture(t)
			app, first := fx.applied(t)
			orig := maps.Clone(fx.intake.beads[first].Fields)

			fx.intake.edit(first, relabel)
			fx.intake.resolve(first, "approve", "credits=10000")
			fx.intake.edit(first, func(f map[string]string) {
				for _, k := range []string{"prompt", "options", "context", "operated_by"} {
					f[k] = orig[k]
				}
			})
			if why := fx.pinned(app.ID); why != "" {
				t.Fatalf("the restore left the digest off, so this is not the bypass: %s", why)
			}

			fx.srv.landsGate.sweep(context.Background())
			f := fx.intake.beads[app.ID].Fields
			if f["state"] != landsAppSubmitted || f["credits_granted_usd"] != "" || f["decision_id"] == first {
				t.Fatalf("acted on a decision written after it closed: %v", f)
			}
			if mails := len(fx.mail.Sent()) - 1; mails != 0 {
				t.Errorf("%d mails after an answer on a decision written after it closed", mails)
			}
			if d := fx.intake.beads[f["decision_id"]]; !strings.Contains(d.Fields["context"], "refused this application's previous decision "+first) ||
				!strings.Contains(d.Fields["context"], "after it closed") {
				t.Fatalf("no note on the fresh decision:\n%s", d.Fields["context"])
			}

			// The fresh decision, answered and left alone, counts.
			fx.intake.resolve(f["decision_id"], "approve", "")
			fx.srv.landsGate.sweep(context.Background())
			if st := fx.intake.beads[app.ID].Fields["state"]; st != landsAppApproved {
				t.Fatalf("state = %s after the approver answered the fresh decision", st)
			}
		})
	}
}

// Only a write after the close refuses the decision: the answer itself lands
// in the closing write, and a closed decision kbeads reports without a
// closed_at is refused rather than trusted.
func TestVerifyLandsDecision_ClosedAt(t *testing.T) {
	fx := newApplyFixture(t)
	b, id := fx.applied(t)
	app, ok := landsApplicationFromBead(fx.intake.beads[b.ID])
	if !ok {
		t.Fatal("not an application")
	}
	fx.intake.resolve(id, "approve", "")
	d, _ := fx.intake.GetBead(context.Background(), id)
	if why := verifyLandsDecision(app, d, fx.intake.beads[app.ID].Fields["decision_digest"]); why != "" {
		t.Fatalf("an answered decision left alone is refused: %s", why)
	}
	digest := fx.intake.beads[app.ID].Fields["decision_digest"]
	for name, mut := range map[string]func(d *beadsapi.BeadDetail){
		"written a nanosecond after": func(d *beadsapi.BeadDetail) { d.UpdatedAt = d.UpdatedAt.Add(time.Nanosecond) },
		"no closed_at":               func(d *beadsapi.BeadDetail) { d.ClosedAt = "" },
		"unreadable closed_at":       func(d *beadsapi.BeadDetail) { d.ClosedAt = "yesterday" },
	} {
		c := *d
		mut(&c)
		if why := verifyLandsDecision(app, &c, digest); why == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The daemon may hand options back with their keys in another order; the pin
// compares them as data.
func TestLandsDecisionDigest_OptionsAsData(t *testing.T) {
	f := map[string]string{"prompt": "p", "context": "c", "operated_by": "oidc:op@example.com",
		"options": `[{"id":"approve","label":"Approve"},{"id":"deny","label":"Deny"}]`}
	want, ok := landsDecisionDigest(f)
	if !ok {
		t.Fatal("no digest")
	}
	f["options"] = `[ {"label": "Approve", "id": "approve"}, {"label":"Deny","id":"deny"} ]`
	if got, _ := landsDecisionDigest(f); got != want {
		t.Fatalf("key order changed the digest: %s != %s", got, want)
	}
	f["options"] = `[{"id":"deny","label":"Deny"},{"id":"approve","label":"Approve"}]`
	if got, _ := landsDecisionDigest(f); got == want {
		t.Fatal("reordered options digest the same")
	}
	for _, bad := range []string{"", "[]", "approve", `["approve","deny"]`} {
		f["options"] = bad
		if _, ok := landsDecisionDigest(f); ok {
			t.Errorf("options %q digested", bad)
		}
	}
}

// gd-ekhj3MJjEa: only LANDS_APPROVER or a gardenio-admin, as the daemon's
// resolve recorded them, moves an application.
func TestLandsApplyReactor_OnlyTheApproverOrAnAdmin(t *testing.T) {
	cases := []struct {
		name     string
		resolver string
		admin    bool
		accepted bool
	}{
		{"the approver in their own session", "op@example.com", false, true},
		{"the approver relayed by the bridge", "oidc:OP@example.com", false, true},
		{"a gardenio-admin", "owner@example.com", true, true},
		{"a machine principal", "machine:gardenio-controller", false, false},
		{"another OIDC user", "someone@example.com", false, false},
		{"the bridge relaying a non-approver Slack user", "slack:U0123ABCD", false, false},
		{"the bridge relaying another linked user", "oidc:someone@example.com", false, false},
		{"no daemon stamp", "", false, false},
	}
	for _, tc := range cases {
		for _, chosen := range []string{"approve", "deny", "ask"} {
			t.Run(tc.name+"/"+chosen, func(t *testing.T) {
				fx := newApplyFixture(t)
				app, first := fx.applied(t)
				fx.intake.resolveAs(first, chosen, "credits=10000 because", tc.resolver, tc.admin)
				if tc.resolver == "" {
					delete(fx.intake.beads[first].Fields, "resolver_subject")
					delete(fx.intake.beads[first].Fields, "resolver_admin")
					fx.intake.beads[first].Fields["responded_by"] = "oidc:op@example.com" // responded_by alone is not trusted
				}
				fx.srv.landsGate.sweep(context.Background())
				f := fx.intake.beads[app.ID].Fields
				acted := f["state"] != landsAppSubmitted || f["question"] != ""
				if acted != tc.accepted {
					t.Fatalf("acted = %v, want %v (fields %v)", acted, tc.accepted, f)
				}
				mails := len(fx.mail.Sent()) - 1
				if !tc.accepted {
					if mails != 0 {
						t.Errorf("%d mails after a refused answer", mails)
					}
					d := fx.intake.beads[f["decision_id"]]
					if f["decision_id"] == first || d.Status != "open" || !strings.Contains(d.Fields["context"], "was not acted on") {
						t.Fatalf("a refused answer was not re-filed with a note: %+v", d)
					}
				} else if chosen != "ask" && f["decided_by"] != tc.resolver {
					t.Errorf("decided_by = %q, want the stamped resolver %q", f["decided_by"], tc.resolver)
				}
			})
		}
	}
}

func TestLandsGateFromEnv(t *testing.T) {
	env := map[string]string{landsApplyDailyLimitEnv: "5", landsApplyDefaultCreditsEnv: "12.5"}
	g, err := landsGateFromEnv(func(k string) string { return env[k] }, newFakeIntake(), nil, nil, nil, nil)
	if err != nil || g.dailyLimit != 5 || g.defaultCredits != 12.5 || mailer.Enabled(g.mail) {
		t.Fatalf("gate = %+v, %v", g, err)
	}
	for k, v := range map[string]string{landsApplyDailyLimitEnv: "0", landsApplyDefaultCreditsEnv: "-1"} {
		if _, err := landsGateFromEnv(func(key string) string {
			if key == k {
				return v
			}
			return env[key]
		}, newFakeIntake(), nil, nil, nil, nil); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

// The Gate's daemon calls carry the machine principal and none of the
// request's identity: the applicant's bearer would outrank the machine on the
// wire, and kbeads refuses plot_application on an applicant's token.
func TestLandsGate_CallCtxDropsTheRequestIdentity(t *testing.T) {
	g := &landsGate{machine: staticTokenSource("machine-token")}
	parent := beadsapi.WithUserBearer(context.Background(), "stranger-access-token")
	parent = beadsapi.WithEdgeAuthOperator(parent, "stranger@example.com")
	ctx, cancel := g.callCtx(parent)
	defer cancel()
	if beadsapi.UserBearerFromContext(ctx) != "" || beadsapi.EdgeAuthOperatorFromContext(ctx) != "" {
		t.Fatal("the request identity leaked into a Gate call")
	}
	if beadsapi.MachinePrincipalFromContext(ctx) == nil {
		t.Fatal("a Gate call does not carry the machine principal")
	}
	pctx, pcancel := context.WithCancel(context.Background())
	cctx, ccancel := g.callCtx(pctx)
	defer ccancel()
	pcancel()
	select {
	case <-cctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelling the request does not cancel the Gate call")
	}
}

type staticTokenSource string

func (s staticTokenSource) Token(context.Context) (string, error) { return string(s), nil }
