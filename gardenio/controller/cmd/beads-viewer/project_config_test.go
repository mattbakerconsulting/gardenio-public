package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// seedProjectConfigFake builds a fakeDaemon with one type:project config bead
// (gd-proj) registered under the project name "gardenio", carrying a git_url and
// one existing secret. ListProjectBeads resolves the name → bead ID so the
// editor can load and PATCH it.
func seedProjectConfigFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", BeadID: "gd-proj", Status: "open"},
	}
	f.beads["gd-proj"] = &beadsapi.BeadDetail{
		ID:     "gd-proj",
		Title:  "gardenio",
		Type:   "project",
		Status: "open",
		Fields: map[string]string{
			"git_url": "https://example.com/repo.git",
			"secrets": `[{"env":"EXISTING","secret":"s0","key":"k0"}]`,
		},
	}
	return f
}

// pcReq builds a request for the project-config routes. roles, when non-nil,
// are injected into the request context so the adminOnly route gate is
// exercised; the X-Auth-Request-Email header satisfies EmailFromRequest.
func pcReq(method, path string, form url.Values, roles []string) *http.Request {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-Auth-Request-Email", "admin@example.com")
	if roles != nil {
		req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), roles))
	}
	return req
}

// TestProjectConfigForm_RendersCurrentConfig verifies GET /p/{project}/edit
// pre-fills the form from the config bead and exposes the add-secret inputs.
func TestProjectConfigForm_RendersCurrentConfig(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("GET", "/p/gardenio/edit", nil, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("edit form: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="git_url"`,
		"https://example.com/repo.git", // current value pre-filled
		`name="card_theme"`,
		`value="business"`,
		`name="add_secret_env"`,
		`name="add_secret_secret"`,
		`name="add_secret_key"`,
		"EXISTING", // existing secret rendered by reference
		"/p/gardenio/config", // posts back to save
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q", want)
		}
	}
	// SECURITY: the existing secret's value-bearing reference is shown, but no
	// raw value is ever present (the form has none to render).
}

// TestProjectConfigSave_AddSecretAppends verifies the flagship add-secret form:
// a valid (env, secret, key) trio appends a SecretEntry to the secrets JSON,
// preserving the existing entry, and the scalar edit lands too.
func TestProjectConfigSave_AddSecretAppends(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")
	form.Set("add_secret_env", "NEW_KEY")
	form.Set("add_secret_secret", "s1")
	form.Set("add_secret_key", "k1")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("save: expected 1 field update, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-proj" {
		t.Errorf("save: patched %q, want gd-proj", f.updateFieldsLastID)
	}
	// secrets persisted as a typed []SecretEntry — re-marshal/parse to assert.
	raw, _ := json.Marshal(f.updateFieldsLastVal["secrets"])
	var got []beadsapi.SecretEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("save: secrets not a SecretEntry slice: %v (raw=%s)", err, raw)
	}
	if len(got) != 2 {
		t.Fatalf("save: expected 2 secrets, got %d (%+v)", len(got), got)
	}
	// Existing entry preserved; new entry appended.
	if got[0].Env != "EXISTING" {
		t.Errorf("save: first secret = %q, want EXISTING (preserved)", got[0].Env)
	}
	if got[1].Env != "NEW_KEY" || got[1].Secret != "s1" || got[1].Key != "k1" {
		t.Errorf("save: appended secret = %+v, want {NEW_KEY s1 k1}", got[1])
	}
}

// TestProjectConfigSave_ScalarOnly verifies a scalar-only save (no add-secret)
// persists the edited scalar and leaves the secret list untouched.
func TestProjectConfigSave_ScalarOnly(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/new.git")
	form.Set("docker", "1")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastVal["git_url"] != "https://example.com/new.git" {
		t.Errorf("save: git_url = %v, want new.git", f.updateFieldsLastVal["git_url"])
	}
	if f.updateFieldsLastVal["docker"] != true {
		t.Errorf("save: docker = %v, want true", f.updateFieldsLastVal["docker"])
	}
	// secrets carried through (existing entry preserved).
	raw, _ := json.Marshal(f.updateFieldsLastVal["secrets"])
	var got []beadsapi.SecretEntry
	_ = json.Unmarshal(raw, &got)
	if len(got) != 1 || got[0].Env != "EXISTING" {
		t.Errorf("save: secrets = %+v, want the single existing entry preserved", got)
	}
}

// TestProjectConfigSave_AddSecretMissingField re-renders with inline errors and
// does NOT persist when the add-secret trio is incomplete.
func TestProjectConfigSave_AddSecretMissingField(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("add_secret_env", "NEW_KEY")
	// secret + key omitted.

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (incomplete secret): expected 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("save (incomplete secret): must not persist; got %d updates", f.updateFieldsCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "K8s Secret name is required") && !strings.Contains(body, "Secret key is required") {
		t.Errorf("save (incomplete secret): expected inline field error; body=%s", body)
	}
	// The admin's raw input is preserved on re-render.
	if !strings.Contains(body, `value="NEW_KEY"`) {
		t.Errorf("save (incomplete secret): expected env input preserved; body=%s", body)
	}
}

// TestProjectConfigSave_DuplicateSecretEnv rejects an add-secret whose env var
// already maps a secret (a silent last-wins footgun at pod spec time).
func TestProjectConfigSave_DuplicateSecretEnv(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("add_secret_env", "EXISTING") // already present
	form.Set("add_secret_secret", "s1")
	form.Set("add_secret_key", "k1")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (dup env): expected 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("save (dup env): must not persist; got %d updates", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "already maps a secret") {
		t.Errorf("save (dup env): expected duplicate error; body=%s", w.Body.String())
	}
}

// TestProjectConfigSave_BadQuantity flags an invalid K8s quantity inline and
// does NOT persist.
func TestProjectConfigSave_BadQuantity(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("cpu_limit", "notaquantity")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (bad qty): expected 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("save (bad qty): must not persist; got %d updates", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "not a valid Kubernetes quantity") {
		t.Errorf("save (bad qty): expected quantity error; body=%s", w.Body.String())
	}
}

// TestProjectConfigSave_CardTheme verifies the card_theme <select>: an explicit
// "business" selection persists, and omitting the field (a daemon-level
// grandfathered state, or any non-browser POST) normalizes to "garden" rather
// than sending the empty string the daemon's enum field would reject.
func TestProjectConfigSave_CardTheme(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")
	form.Set("card_theme", "business")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (card_theme=business): expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastVal["card_theme"] != "business" {
		t.Errorf("save: card_theme = %v, want business", f.updateFieldsLastVal["card_theme"])
	}
}

// TestProjectConfigSave_CardThemeOmittedNotSent verifies a save that omits
// card_theme entirely (e.g. a stale client) does NOT add card_theme to the
// PATCH when the stored project has never set it: the <select> has no blank
// option, so every browser POST carries an explicit "garden"/"business" even
// when the admin never touched that control, and the resolved default
// ("garden") matches the implicit unset default. Sending it anyway would risk
// a 400 on the whole save against a daemon that predates the card_theme
// schema field (kbeads never reads config beads back out to pick up a
// mid-rollout schema change, ka-0000001549) — only an explicit, differing
// choice should cross that wire.
func TestProjectConfigSave_CardThemeOmittedNotSent(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (card_theme omitted): expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if _, sent := f.updateFieldsLastVal["card_theme"]; sent {
		t.Errorf("save: card_theme = %v, want it omitted from the PATCH (unset -> garden is a no-op)", f.updateFieldsLastVal["card_theme"])
	}
	// The rest of the scalar edit still persists.
	if f.updateFieldsLastVal["git_url"] != "https://example.com/repo.git" {
		t.Errorf("save: git_url = %v, want persisted alongside the omitted card_theme", f.updateFieldsLastVal["git_url"])
	}
}

// TestProjectConfigSave_CardThemeUnchangedNotResent verifies that re-saving a
// project whose card_theme is already explicitly "business" without touching
// the select does not resend card_theme (no-op diff) — only a value that
// actually changed from the stored one crosses the wire.
func TestProjectConfigSave_CardThemeUnchangedNotResent(t *testing.T) {
	f := seedProjectConfigFake(t)
	f.beads["gd-proj"].Fields["card_theme"] = "business"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")
	form.Set("card_theme", "business") // the <select>'s current/unchanged value

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (card_theme unchanged): expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if _, sent := f.updateFieldsLastVal["card_theme"]; sent {
		t.Errorf("save: card_theme = %v, want it omitted from the PATCH (unchanged)", f.updateFieldsLastVal["card_theme"])
	}
}

// TestProjectConfigSave_CardThemeInvalid rejects a card_theme outside
// garden/business (a non-browser POST — the <select> itself offers no other
// value) with an inline field error, without persisting.
func TestProjectConfigSave_CardThemeInvalid(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("card_theme", "lunar")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (bad card_theme): expected 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("save (bad card_theme): must not persist; got %d updates", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "not a valid card theme") {
		t.Errorf("save (bad card_theme): expected card theme error; body=%s", w.Body.String())
	}
}

// TestProjectConfig_OperatorRejected pins the load-bearing auth contract: an
// operator-scoped viewer (lvl 2 < gardenio-admin lvl 3) is rejected by the route
// gate on BOTH the edit form and the save — type:project is admin-gated and not
// project-scoped (design doc §6).
func TestProjectConfig_OperatorRejected(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	// GET edit form as operator → 403.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("GET", "/p/gardenio/edit", nil, []string{oidcauth.RoleOperator}))
	if w.Code != http.StatusForbidden {
		t.Errorf("edit form as operator: expected 403, got %d", w.Code)
	}

	// POST save as operator → 403, and nothing persisted.
	form := url.Values{}
	form.Set("git_url", "https://evil.example.com/x.git")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleOperator}))
	if w.Code != http.StatusForbidden {
		t.Errorf("save as operator: expected 403, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("save as operator: must not persist; got %d updates", f.updateFieldsCalls)
	}
}

// TestProjectConfig_AdminAllowed confirms the admin role passes the gate (the
// positive direction of the auth contract).
func TestProjectConfig_AdminAllowed(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("GET", "/p/gardenio/edit", nil, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Errorf("edit form as admin: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestProjectConfig_LabelOnlyProject returns 404 for a project with no
// type:project config bead — there is nothing to edit.
func TestProjectConfig_LabelOnlyProject(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{} // no config bead
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("GET", "/p/ghost/edit", nil, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusNotFound {
		t.Errorf("label-only edit: expected 404, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestProjectConfigSave_MalformedExistingSecrets refuses to save when the
// existing secrets blob is malformed — saving would otherwise erase it.
func TestProjectConfigSave_MalformedExistingSecrets(t *testing.T) {
	f := seedProjectConfigFake(t)
	f.beads["gd-proj"].Fields["secrets"] = `{not json`
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (malformed secrets): expected 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("save (malformed secrets): must not persist; got %d updates", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "malformed") {
		t.Errorf("save (malformed secrets): expected malformed error; body=%s", w.Body.String())
	}
}

// TestProjectConfigForm_RendersBudget verifies the edit form shows the budget
// inputs pre-filled from the bead, with the stored period selected.
func TestProjectConfigForm_RendersBudget(t *testing.T) {
	f := seedProjectConfigFake(t)
	f.beads["gd-proj"].Fields["budget_usd"] = "250.5"
	f.beads["gd-proj"].Fields["budget_period"] = "7d"
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("GET", "/p/gardenio/edit", nil, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("edit form: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="budget_usd" value="250.5"`,
		`name="budget_period"`,
		`<option value="7d" selected>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q", want)
		}
	}
}

// TestProjectConfigSave_BudgetSet verifies a budget save PATCHes budget_usd as
// a number (the daemon's float field rejects a string) plus the period.
func TestProjectConfigSave_BudgetSet(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")
	form.Set("budget_usd", "250")
	form.Set("budget_period", "7d")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save (budget): expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got, ok := f.updateFieldsLastVal["budget_usd"].(float64); !ok || got != 250 {
		t.Errorf("save: budget_usd = %#v, want float64 250", f.updateFieldsLastVal["budget_usd"])
	}
	if f.updateFieldsLastVal["budget_period"] != "7d" {
		t.Errorf("save: budget_period = %v, want 7d", f.updateFieldsLastVal["budget_period"])
	}
}

// TestProjectConfigSave_BudgetPeriodDefaultsToMonth verifies a budget saved
// without a period (a non-browser POST) stores the "month" default.
func TestProjectConfigSave_BudgetPeriodDefaultsToMonth(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("budget_usd", "100")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastVal["budget_period"] != "month" {
		t.Errorf("save: budget_period = %v, want month", f.updateFieldsLastVal["budget_period"])
	}
}

// TestProjectConfigSave_NoBudgetNotSent verifies a project that never had a
// budget sends neither budget key: the browser always posts budget_period
// ("month" from the <select>), and against a daemon that predates the field
// sending it would 400 the whole save.
func TestProjectConfigSave_NoBudgetNotSent(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")
	form.Set("budget_usd", "")
	form.Set("budget_period", "month")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("save: expected 1 field update, got %d", f.updateFieldsCalls)
	}
	for _, k := range []string{"budget_usd", "budget_period"} {
		if v, sent := f.updateFieldsLastVal[k]; sent {
			t.Errorf("save: %s = %#v, want it omitted from the PATCH", k, v)
		}
	}
}

// TestProjectConfigSave_BudgetUnchangedNotResent verifies re-saving a project
// whose budget is unchanged does not resend either budget key, even when the
// stored and submitted amounts are spelled differently ("250" vs "250.00").
func TestProjectConfigSave_BudgetUnchangedNotResent(t *testing.T) {
	f := seedProjectConfigFake(t)
	f.beads["gd-proj"].Fields["budget_usd"] = "250"
	f.beads["gd-proj"].Fields["budget_period"] = "month"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")
	form.Set("budget_usd", "250.00")
	form.Set("budget_period", "month")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	for _, k := range []string{"budget_usd", "budget_period"} {
		if v, sent := f.updateFieldsLastVal[k]; sent {
			t.Errorf("save: %s = %#v, want it omitted from the PATCH (unchanged)", k, v)
		}
	}
}

// TestProjectConfigSave_BudgetCleared verifies blanking the budget deletes both
// stored fields (a JSON null unsets the key) instead of writing 0 or "".
func TestProjectConfigSave_BudgetCleared(t *testing.T) {
	f := seedProjectConfigFake(t)
	f.beads["gd-proj"].Fields["budget_usd"] = "250"
	f.beads["gd-proj"].Fields["budget_period"] = "7d"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("git_url", "https://example.com/repo.git")
	form.Set("budget_usd", "")
	form.Set("budget_period", "7d")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	for _, k := range []string{"budget_usd", "budget_period"} {
		v, sent := f.updateFieldsLastVal[k]
		if !sent || v != nil {
			t.Errorf("save: %s = %#v (sent=%v), want an explicit nil to unset it", k, v, sent)
		}
	}
}

// TestProjectConfigSave_BudgetZeroRejected pins the "never a fake $0" rule: a
// literal 0 is rejected inline and nothing is persisted.
func TestProjectConfigSave_BudgetZeroRejected(t *testing.T) {
	for _, bad := range []string{"0", "-10", "abc"} {
		t.Run(bad, func(t *testing.T) {
			f := seedProjectConfigFake(t)
			mux := mkServer(t, f)

			form := url.Values{}
			form.Set("budget_usd", bad)
			form.Set("budget_period", "month")

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
			if w.Code != http.StatusOK {
				t.Fatalf("save: expected 200, got %d", w.Code)
			}
			if f.updateFieldsCalls != 0 {
				t.Errorf("save: must not persist; got %d updates", f.updateFieldsCalls)
			}
			body := w.Body.String()
			if !strings.Contains(body, `class="pc-field-error"`) {
				t.Errorf("save: expected an inline field error; body=%s", body)
			}
			if !strings.Contains(body, `value="`+bad+`"`) {
				t.Errorf("save: expected the raw input %q preserved; body=%s", bad, body)
			}
		})
	}
}

// TestProjectConfigSave_BudgetPeriodInvalid rejects a period outside month|7d.
func TestProjectConfigSave_BudgetPeriodInvalid(t *testing.T) {
	f := seedProjectConfigFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("budget_usd", "100")
	form.Set("budget_period", "fortnight")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, pcReq("POST", "/p/gardenio/config", form, []string{oidcauth.RoleAdmin}))
	if w.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("save: must not persist; got %d updates", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "not a valid budget period") {
		t.Errorf("save: expected budget period error; body=%s", w.Body.String())
	}
}
