package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// settingsFake builds a fake daemon seeded with a user bead whose email
// matches authorEmail and whose preferences carry recognizable values.
// findUserBeadByEmail lists all type=user kind=config beads and filters
// client-side, so the bead must carry the email field for the lookup to
// succeed.
func settingsFake(prefs Preferences) *fakeDaemon {
	return settingsFakeWith(prefs, Identity{})
}

// settingsFakeWith is the variant used by identity-edit tests: it seeds the
// user bead with both preferences and identity fields so the read-only
// renders + edit pre-fills surface them correctly.
func settingsFakeWith(prefs Preferences, identity Identity) *fakeDaemon {
	fields := map[string]string{
		"email": authorEmail,
	}
	if encoded, err := json.Marshal(prefs); err == nil && string(encoded) != "{}" {
		fields["preferences"] = string(encoded)
	}
	if identity.DisplayName != "" {
		fields["display_name"] = identity.DisplayName
	}
	if identity.Pronouns != "" {
		fields["pronouns"] = identity.Pronouns
	}
	if identity.Team != "" {
		fields["team"] = identity.Team
	}
	if identity.SlackChannelID != "" {
		fields["slack_channel_id"] = identity.SlackChannelID
	}
	bead := &beadsapi.BeadDetail{
		ID:     "gd-user-op",
		Title:  "User: operator@example.com",
		Type:   "user",
		Status: "open",
		Fields: fields,
	}
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{"gd-user-op": bead},
		listResp: &beadsapi.ListBeadsResult{
			Beads: []*beadsapi.BeadDetail{bead},
		},
	}
}

// TestNav_SettingsLink ensures the new Settings nav link is rendered for
// authenticated users.
func TestNav_SettingsLink(t *testing.T) {
	mux := mkServer(t, settingsFake(Preferences{}))
	req := httptest.NewRequest("GET", "/active", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/settings"`) {
		t.Errorf("expected Settings nav link, body=%s", w.Body.String())
	}
}

// TestSettings_PageRender renders the settings page for an authenticated
// operator with seeded preferences and asserts the read-only block surfaces
// each value.
func TestSettings_PageRender(t *testing.T) {
	prefs := Preferences{
		DefaultSpawnProject:        "gardenio",
		DefaultSpawnRole:           "crew",
		DefaultSpawnPromptTemplate: "Investigate {{thing}}.",
		SpawnPerSelection:          "per-bead",
	}
	mux := mkServer(t, settingsFake(prefs))
	req := httptest.NewRequest("GET", "/settings", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Settings",
		`id="settings-block"`,
		"gardenio",
		"crew",
		"Investigate {{thing}}.",
		"per-bead",
		`hx-get="/settings/edit"`,
		"Edit preferences",
		authorEmail,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q\nbody=%s", want, body)
		}
	}
}

// TestSettings_PageRender_Unauthenticated rejects anonymous requests with a
// 401 — settings is a per-user surface with no useful empty-state.
func TestSettings_PageRender_Unauthenticated(t *testing.T) {
	mux := mkServer(t, settingsFake(Preferences{}))
	req := httptest.NewRequest("GET", "/settings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// TestSettings_PageRender_NoUserBead renders the empty-state when the
// authenticated email has no matching user bead.
func TestSettings_PageRender_NoUserBead(t *testing.T) {
	mux := mkServer(t, &fakeDaemon{
		beads:    map[string]*beadsapi.BeadDetail{},
		listResp: &beadsapi.ListBeadsResult{},
	})
	req := httptest.NewRequest("GET", "/settings", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No user bead found") {
		t.Errorf("expected empty-state message, body=%s", body)
	}
	if strings.Contains(body, "Edit preferences") {
		t.Errorf("Edit button should be hidden when no user bead, body=%s", body)
	}
}

// TestSettingsEdit_RendersForm verifies GET /settings/edit returns the editor
// partial pre-filled with current preferences.
func TestSettingsEdit_RendersForm(t *testing.T) {
	prefs := Preferences{
		DefaultSpawnProject:        "kbeads",
		DefaultSpawnRole:           "crew-lite",
		DefaultSpawnPromptTemplate: "draft body",
		SpawnPerSelection:          "single",
	}
	mux := mkServer(t, settingsFake(prefs))
	req := httptest.NewRequest("GET", "/settings/edit", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("editor must be a partial, not a full page\nbody=%s", body)
	}
	for _, want := range []string{
		`id="settings-block"`,
		`hx-post="/settings"`,
		`name="default_spawn_project"`,
		`value="kbeads"`,
		`name="default_spawn_role"`,
		`value="crew-lite"`,
		`name="default_spawn_prompt_template"`,
		"draft body",
		`name="spawn_per_selection"`,
		`Save`,
		`Cancel`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor missing %q\nbody=%s", want, body)
		}
	}
}

// TestSettingsEdit_Unauthenticated rejects anonymous edit requests.
func TestSettingsEdit_Unauthenticated(t *testing.T) {
	mux := mkServer(t, settingsFake(Preferences{}))
	req := httptest.NewRequest("GET", "/settings/edit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// TestSettingsView_RendersBlock verifies GET /settings/view returns the
// read-only block (used by the editor's Cancel button).
func TestSettingsView_RendersBlock(t *testing.T) {
	prefs := Preferences{DefaultSpawnProject: "gardenio"}
	mux := mkServer(t, settingsFake(prefs))
	req := httptest.NewRequest("GET", "/settings/view", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("block must be a partial\nbody=%s", body)
	}
	for _, want := range []string{
		`id="settings-block"`,
		"gardenio",
		"Edit preferences",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="default_spawn_project"`) {
		t.Errorf("read-only block should not contain form inputs\nbody=%s", body)
	}
}

// TestUpdateSettings_HappyPath posts a complete preferences form and verifies
// UpdateBeadFieldsTyped is called with the expected payload, then the
// read-only block is re-rendered with the new values.
func TestUpdateSettings_HappyPath(t *testing.T) {
	f := settingsFake(Preferences{})
	mux := mkServer(t, f)
	form := url.Values{
		"default_spawn_project":         []string{"  gardenio  "},
		"default_spawn_role":            []string{"crew"},
		"default_spawn_prompt_template": []string{"Investigate {{topic}}."},
		"spawn_per_selection":           []string{"per-bead"},
	}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("expected 1 UpdateBeadFieldsTyped call, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-user-op" {
		t.Errorf("update id = %q, want gd-user-op", f.updateFieldsLastID)
	}
	got, ok := f.updateFieldsLastVal["preferences"].(Preferences)
	if !ok {
		t.Fatalf("expected Preferences in fields[preferences], got %T", f.updateFieldsLastVal["preferences"])
	}
	if got.DefaultSpawnProject != "gardenio" {
		t.Errorf("DefaultSpawnProject = %q, want gardenio (trimmed)", got.DefaultSpawnProject)
	}
	if got.DefaultSpawnRole != "crew" {
		t.Errorf("DefaultSpawnRole = %q, want crew", got.DefaultSpawnRole)
	}
	if got.DefaultSpawnPromptTemplate != "Investigate {{topic}}." {
		t.Errorf("DefaultSpawnPromptTemplate = %q", got.DefaultSpawnPromptTemplate)
	}
	if got.SpawnPerSelection != "per-bead" {
		t.Errorf("SpawnPerSelection = %q, want per-bead", got.SpawnPerSelection)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="settings-block"`,
		"gardenio",
		"crew",
		"per-bead",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-update fragment missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="default_spawn_project"`) {
		t.Errorf("happy-path response should be the read-only block, got editor\nbody=%s", body)
	}
}

// TestUpdateSettings_CoercesUnknownSpawnPerSelection silently downgrades an
// unrecognized spawn_per_selection to the empty default rather than failing.
func TestUpdateSettings_CoercesUnknownSpawnPerSelection(t *testing.T) {
	f := settingsFake(Preferences{})
	mux := mkServer(t, f)
	form := url.Values{"spawn_per_selection": []string{"bananas"}}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got, ok := f.updateFieldsLastVal["preferences"].(Preferences)
	if !ok {
		t.Fatalf("expected Preferences, got %T", f.updateFieldsLastVal["preferences"])
	}
	if got.SpawnPerSelection != "" {
		t.Errorf("expected unrecognized spawn_per_selection coerced to empty, got %q", got.SpawnPerSelection)
	}
}

// TestUpdateSettings_Unauthenticated rejects POSTs without an auth header.
func TestUpdateSettings_Unauthenticated(t *testing.T) {
	f := settingsFake(Preferences{})
	mux := mkServer(t, f)
	form := url.Values{"default_spawn_project": []string{"gardenio"}}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("UpdateBeadFieldsTyped should not be called: %d", f.updateFieldsCalls)
	}
}

// TestUpdateSettings_NoUserBead returns 404 when the authenticated email has
// no matching user bead.
func TestUpdateSettings_NoUserBead(t *testing.T) {
	f := &fakeDaemon{
		beads:    map[string]*beadsapi.BeadDetail{},
		listResp: &beadsapi.ListBeadsResult{},
	}
	mux := mkServer(t, f)
	form := url.Values{"default_spawn_project": []string{"gardenio"}}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("UpdateBeadFieldsTyped should not be called: %d", f.updateFieldsCalls)
	}
}

// TestUpdateSettings_DaemonError_RendersInline surfaces a daemon-side failure
// as 502 with the editor re-rendered, an inline error, and the operator's
// draft preserved so they can retry.
func TestUpdateSettings_DaemonError_RendersInline(t *testing.T) {
	f := settingsFake(Preferences{})
	f.updateFieldsErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := url.Values{
		"default_spawn_project": []string{"draft-project"},
		"default_spawn_role":    []string{"draft-role"},
	}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to save preferences") {
		t.Errorf("expected inline error, body=%s", body)
	}
	for _, want := range []string{"draft-project", "draft-role", `hx-post="/settings"`} {
		if !strings.Contains(body, want) {
			t.Errorf("draft / editor missing %q after error\nbody=%s", want, body)
		}
	}
}

// TestUpdateSettings_OversizedReturns413 rejects payloads larger than the
// inline editor cap with 413 and skips the daemon call.
func TestUpdateSettings_OversizedReturns413(t *testing.T) {
	f := settingsFake(Preferences{})
	mux := mkServer(t, f)
	big := strings.Repeat("x", maxPrefStringLen+1)
	form := url.Values{"default_spawn_prompt_template": []string{big}}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("UpdateBeadFieldsTyped should not be called for oversized: %d", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "too long") {
		t.Errorf("expected too-long error, body=%s", w.Body.String())
	}
}

// TestSettings_RoundTrip simulates the editor open/save cycle: open the
// editor, post new prefs, verify the updated values appear on a subsequent
// settings page render.
func TestSettings_RoundTrip(t *testing.T) {
	f := settingsFake(Preferences{
		DefaultSpawnProject: "old-project",
	})
	// Mutate the fake's stored bead on a successful UpdateBeadFieldsTyped so
	// the second GET reflects the saved state.
	bead := f.beads["gd-user-op"]
	mux := mkServer(t, &updatingFake{fakeDaemon: f, bead: bead})

	openReq := httptest.NewRequest("GET", "/settings/edit", nil)
	openReq.Header.Set("X-Auth-Request-Email", authorEmail)
	openW := httptest.NewRecorder()
	mux.ServeHTTP(openW, openReq)
	if openW.Code != http.StatusOK {
		t.Fatalf("open: expected 200, got %d", openW.Code)
	}
	if !strings.Contains(openW.Body.String(), `value="old-project"`) {
		t.Errorf("open: editor should pre-fill current value\nbody=%s", openW.Body.String())
	}

	form := url.Values{
		"default_spawn_project": []string{"new-project"},
		"default_spawn_role":    []string{"new-role"},
		"spawn_per_selection":   []string{"per-bead"},
	}
	saveReq := authedPost("/settings", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", saveW.Code, saveW.Body.String())
	}
	if !strings.Contains(saveW.Body.String(), "new-project") {
		t.Errorf("save: response should reflect new project\nbody=%s", saveW.Body.String())
	}

	showReq := httptest.NewRequest("GET", "/settings", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	body := showW.Body.String()
	if !strings.Contains(body, "new-project") {
		t.Errorf("show: persisted prefs missing on full page render\nbody=%s", body)
	}
	if !strings.Contains(body, "new-role") {
		t.Errorf("show: new role missing\nbody=%s", body)
	}
	if strings.Contains(body, "old-project") {
		t.Errorf("show: previous project should be replaced\nbody=%s", body)
	}
}

// updatingFake wraps fakeDaemon to mutate the seeded user bead's preferences
// field on each successful UpdateBeadFieldsTyped — the round-trip test relies
// on the second GET reflecting the saved state.
type updatingFake struct {
	*fakeDaemon
	bead *beadsapi.BeadDetail
}

func (u *updatingFake) UpdateBeadFieldsTyped(ctx context.Context, id string, fields map[string]any) error {
	if err := u.fakeDaemon.UpdateBeadFieldsTyped(ctx, id, fields); err != nil {
		return err
	}
	if prefs, ok := fields["preferences"].(Preferences); ok {
		if encoded, err := json.Marshal(prefs); err == nil {
			if u.bead.Fields == nil {
				u.bead.Fields = map[string]string{}
			}
			u.bead.Fields["preferences"] = string(encoded)
		}
	}
	if v, ok := fields["display_name"].(string); ok {
		u.bead.Fields["display_name"] = v
	}
	if v, ok := fields["pronouns"].(string); ok {
		u.bead.Fields["pronouns"] = v
	}
	if v, ok := fields["team"].(string); ok {
		u.bead.Fields["team"] = v
	}
	if v, ok := fields["slack_channel_id"].(string); ok {
		u.bead.Fields["slack_channel_id"] = v
	}
	if v, ok := fields["profile_prefs"].(string); ok {
		u.bead.Fields["profile_prefs"] = v
	}
	return nil
}

// TestSettings_IdentityFieldsRender asserts the read-only block surfaces the
// seeded identity fields alongside spawn preferences.
func TestSettings_IdentityFieldsRender(t *testing.T) {
	identity := Identity{
		DisplayName:    "Operator Op",
		Pronouns:       "they/them",
		Team:           "platform",
		SlackChannelID: "D0123ABC",
	}
	mux := mkServer(t, settingsFakeWith(Preferences{}, identity))
	req := httptest.NewRequest("GET", "/settings", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Identity",
		"Operator Op",
		"they/them",
		"platform",
		"D0123ABC",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q\nbody=%s", want, body)
		}
	}
}

// TestSettingsEdit_IdentityFieldsPrefilled verifies the editor pre-fills
// identity fields with the seeded values.
func TestSettingsEdit_IdentityFieldsPrefilled(t *testing.T) {
	identity := Identity{
		DisplayName:    "Operator Op",
		Pronouns:       "she/her",
		Team:           "fleet",
		SlackChannelID: "C0CHAN42",
	}
	mux := mkServer(t, settingsFakeWith(Preferences{}, identity))
	req := httptest.NewRequest("GET", "/settings/edit", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="display_name"`,
		`value="Operator Op"`,
		`name="pronouns"`,
		`value="she/her"`,
		`name="team"`,
		`value="fleet"`,
		`name="slack_channel_id"`,
		`value="C0CHAN42"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor missing %q\nbody=%s", want, body)
		}
	}
}

// TestUpdateSettings_IdentityHappyPath posts new identity values and asserts
// they are forwarded to UpdateBeadFieldsTyped as separate top-level fields
// (not nested in preferences) and that the read-only block re-renders with
// the new values.
func TestUpdateSettings_IdentityHappyPath(t *testing.T) {
	f := settingsFake(Preferences{})
	bead := f.beads["gd-user-op"]
	mux := mkServer(t, &updatingFake{fakeDaemon: f, bead: bead})

	form := url.Values{
		"display_name":     []string{"  Operator Op  "},
		"pronouns":         []string{"they/them"},
		"team":             []string{"platform"},
		"slack_channel_id": []string{"D0123ABC"},
	}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("expected 1 UpdateBeadFieldsTyped call, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-user-op" {
		t.Errorf("update id = %q, want gd-user-op", f.updateFieldsLastID)
	}
	for k, want := range map[string]string{
		"display_name":     "Operator Op",
		"pronouns":         "they/them",
		"team":             "platform",
		"slack_channel_id": "D0123ABC",
	} {
		got, ok := f.updateFieldsLastVal[k].(string)
		if !ok {
			t.Errorf("expected %s as string, got %T", k, f.updateFieldsLastVal[k])
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	body := w.Body.String()
	for _, want := range []string{"Operator Op", "they/them", "platform", "D0123ABC"} {
		if !strings.Contains(body, want) {
			t.Errorf("post-update fragment missing %q\nbody=%s", want, body)
		}
	}
}

// TestUpdateSettings_SlackChannelIDInvalid rejects malformed Slack IDs with
// 400 and skips the daemon call.
func TestUpdateSettings_SlackChannelIDInvalid(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"lowercase", "c0123abc"},
		{"wrong-prefix", "X0123ABC"},
		{"with-spaces", "C 0123 ABC"},
		{"non-alnum", "C0123-ABC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := settingsFake(Preferences{})
			mux := mkServer(t, f)
			form := url.Values{"slack_channel_id": []string{tc.val}}
			req := authedPost("/settings", form)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if f.updateFieldsCalls != 0 {
				t.Errorf("UpdateBeadFieldsTyped should not be called for invalid slack id: %d", f.updateFieldsCalls)
			}
			body := w.Body.String()
			if !strings.Contains(body, "Slack channel id") {
				t.Errorf("expected inline slack-id error, body=%s", body)
			}
			// Draft must be preserved so the operator can fix and retry.
			if !strings.Contains(body, tc.val) {
				t.Errorf("draft %q missing from re-render\nbody=%s", tc.val, body)
			}
		})
	}
}

// TestUpdateSettings_SlackChannelIDEmpty allows empty values to clear the
// field — useful when a user wants to opt out of DMs.
func TestUpdateSettings_SlackChannelIDEmpty(t *testing.T) {
	f := settingsFake(Preferences{})
	mux := mkServer(t, f)
	form := url.Values{"slack_channel_id": []string{""}}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("expected 1 UpdateBeadFieldsTyped call, got %d", f.updateFieldsCalls)
	}
	got, _ := f.updateFieldsLastVal["slack_channel_id"].(string)
	if got != "" {
		t.Errorf("slack_channel_id = %q, want empty", got)
	}
}

// TestUpdateSettings_IdentityOversize rejects identity values past the
// per-field cap with 413.
func TestUpdateSettings_IdentityOversize(t *testing.T) {
	f := settingsFake(Preferences{})
	mux := mkServer(t, f)
	big := strings.Repeat("x", maxIdentityStringLen+1)
	form := url.Values{"display_name": []string{big}}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("UpdateBeadFieldsTyped should not be called for oversized: %d", f.updateFieldsCalls)
	}
}

// TestUpdateSettings_OtherUserCannotEdit asserts a different OIDC user's
// POST cannot route a write to this user's bead. The handler always looks
// up the bead by the requester's email; a crafted POST from another user
// resolves to that user's bead (or 404 when none exists), never the
// victim's. This is the negative case named in the UC-1 acceptance bar.
func TestUpdateSettings_OtherUserCannotEdit(t *testing.T) {
	const victimEmail = "operator@example.com"   // == authorEmail
	const attackerEmail = "attacker@example.com" // does not match the seeded bead

	// Seed a bead that belongs to the victim only. There is no second user
	// bead — so an attacker request must 404, not silently update the victim.
	f := settingsFake(Preferences{DefaultSpawnProject: "victim-project"})
	mux := mkServer(t, f)

	form := url.Values{
		"display_name":     []string{"hijacked"},
		"slack_channel_id": []string{"D0EVIL00"},
		// Try to embed any "target" hint a naive handler might honor — the
		// real handler ignores form-supplied bead ids by construction, but
		// the test pins the contract.
		"bead_id": []string{"gd-user-op"},
		"id":      []string{"gd-user-op"},
	}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", attackerEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("attacker POST: expected 404 (no user bead for %s), got %d body=%s",
			attackerEmail, w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 0 {
		t.Fatalf("attacker POST must not call UpdateBeadFieldsTyped, got %d calls", f.updateFieldsCalls)
	}

	// And the victim's bead is untouched. Re-render the victim's settings
	// page and assert the seeded values are still present.
	showReq := httptest.NewRequest("GET", "/settings", nil)
	showReq.Header.Set("X-Auth-Request-Email", victimEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	if !strings.Contains(showW.Body.String(), "victim-project") {
		t.Errorf("victim's seeded prefs lost after attacker POST\nbody=%s", showW.Body.String())
	}
	if strings.Contains(showW.Body.String(), "hijacked") || strings.Contains(showW.Body.String(), "D0EVIL00") {
		t.Errorf("victim's bead was modified by attacker POST\nbody=%s", showW.Body.String())
	}
}

// TestUpdateSettings_IdentityRoundTrip simulates editing identity fields:
// open the editor, post new identity values, verify subsequent renders
// reflect the saved state.
func TestUpdateSettings_IdentityRoundTrip(t *testing.T) {
	f := settingsFakeWith(Preferences{}, Identity{
		DisplayName: "Old Name",
		Team:        "old-team",
	})
	bead := f.beads["gd-user-op"]
	mux := mkServer(t, &updatingFake{fakeDaemon: f, bead: bead})

	openReq := httptest.NewRequest("GET", "/settings/edit", nil)
	openReq.Header.Set("X-Auth-Request-Email", authorEmail)
	openW := httptest.NewRecorder()
	mux.ServeHTTP(openW, openReq)
	if !strings.Contains(openW.Body.String(), `value="Old Name"`) {
		t.Errorf("editor must pre-fill seeded identity\nbody=%s", openW.Body.String())
	}

	form := url.Values{
		"display_name":     []string{"New Name"},
		"pronouns":         []string{"they/them"},
		"team":             []string{"new-team"},
		"slack_channel_id": []string{"C0NEW123"},
	}
	saveReq := authedPost("/settings", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", saveW.Code, saveW.Body.String())
	}

	showReq := httptest.NewRequest("GET", "/settings", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	body := showW.Body.String()
	for _, want := range []string{"New Name", "they/them", "new-team", "C0NEW123"} {
		if !strings.Contains(body, want) {
			t.Errorf("show: persisted identity missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "Old Name") || strings.Contains(body, "old-team") {
		t.Errorf("show: previous identity should be replaced\nbody=%s", body)
	}
}

// ── vibe_mode preference (gb-ux epic ka-0000005059) ──────────────────────

// TestFlexBool_UnmarshalAcceptsBothForms pins the interop contract: a
// vibe_mode value reaches the preferences blob either as the beads-viewer's
// own struct marshal OR as the string form written by `gb prefs set` (which
// round-trips the whole blob through map[string]string). parsePreferences
// must read both without error.
func TestFlexBool_UnmarshalAcceptsBothForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"native-true", `{"vibe_mode":true}`, true},
		{"native-false", `{"vibe_mode":false}`, false},
		{"string-true", `{"vibe_mode":"true"}`, true},    // gb prefs set … true
		{"string-false", `{"vibe_mode":"false"}`, false}, // gb prefs set … false
		{"string-one", `{"vibe_mode":"1"}`, true},
		{"string-zero", `{"vibe_mode":"0"}`, false},
		{"empty-string", `{"vibe_mode":""}`, false},
		{"null", `{"vibe_mode":null}`, false},
		{"absent", `{}`, false},
		{"garbage-string", `{"vibe_mode":"yes"}`, false}, // unparseable → false, no error
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parsePreferences(map[string]string{"preferences": tc.raw})
			if bool(p.VibeMode) != tc.want {
				t.Errorf("parsePreferences(%s).VibeMode = %v, want %v", tc.raw, bool(p.VibeMode), tc.want)
			}
		})
	}
}

// TestFlexBool_MarshalsToStringForm asserts the persisted blob stays an
// all-strings object so the map[string]string readers (gb prefs, gb prime,
// quick_spawn) can still decode it. A native JSON bool would make those
// readers error and silently drop every preference.
func TestFlexBool_MarshalsToStringForm(t *testing.T) {
	// True marshals to the STRING "true", not the native bool true.
	enc, err := json.Marshal(Preferences{VibeMode: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(enc), `"vibe_mode":"true"`) {
		t.Errorf("expected string form, got %s", enc)
	}
	if strings.Contains(string(enc), `"vibe_mode":true`) {
		t.Errorf("vibe_mode must not serialize as a native JSON bool: %s", enc)
	}

	// The whole blob must decode into map[string]string (the shape gb prefs /
	// gb prime / quick_spawn require).
	blob, err := json.Marshal(Preferences{
		DefaultSpawnProject: "gardenio",
		VibeMode:            true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]string
	if err := json.Unmarshal(blob, &asMap); err != nil {
		t.Fatalf("blob not decodable as map[string]string (would break gb prefs/prime): %v\nblob=%s", err, blob)
	}
	if asMap["vibe_mode"] != "true" {
		t.Errorf("map[vibe_mode] = %q, want \"true\"", asMap["vibe_mode"])
	}

	// False + omitempty drops the key entirely — a simple-mode blob carries
	// no vibe_mode at all.
	off, err := json.Marshal(Preferences{VibeMode: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(off), "vibe_mode") {
		t.Errorf("false VibeMode should be omitted, got %s", off)
	}
}

// TestSettings_VibeModeRendersInBlock asserts the read-only block surfaces the
// vibe_mode state (on when set, off otherwise).
func TestSettings_VibeModeRendersInBlock(t *testing.T) {
	cases := []struct {
		name   string
		prefs  Preferences
		wantOn bool
	}{
		{"on", Preferences{VibeMode: true}, true},
		{"off", Preferences{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := mkServer(t, settingsFake(tc.prefs))
			req := httptest.NewRequest("GET", "/settings", nil)
			req.Header.Set("X-Auth-Request-Email", authorEmail)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "Vibe mode") {
				t.Errorf("read-only block missing Vibe mode row\nbody=%s", body)
			}
			if tc.wantOn && !strings.Contains(body, "full functionality") {
				t.Errorf("expected 'on (full functionality)' when vibe_mode set\nbody=%s", body)
			}
			if !tc.wantOn && !strings.Contains(body, "simpler default") {
				t.Errorf("expected 'off (simpler default)' when vibe_mode unset\nbody=%s", body)
			}
		})
	}
}

// TestSettingsEdit_VibeModeCheckbox verifies the editor renders the vibe_mode
// checkbox, checked only when the pref is on.
func TestSettingsEdit_VibeModeCheckbox(t *testing.T) {
	t.Run("checked-when-on", func(t *testing.T) {
		mux := mkServer(t, settingsFake(Preferences{VibeMode: true}))
		req := httptest.NewRequest("GET", "/settings/edit", nil)
		req.Header.Set("X-Auth-Request-Email", authorEmail)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		body := w.Body.String()
		if !strings.Contains(body, `name="vibe_mode"`) {
			t.Errorf("editor missing vibe_mode checkbox\nbody=%s", body)
		}
		if !strings.Contains(body, "Vibe mode (full functionality)") {
			t.Errorf("editor missing vibe_mode label\nbody=%s", body)
		}
		if !strings.Contains(body, "checked") {
			t.Errorf("vibe_mode checkbox should be checked when on\nbody=%s", body)
		}
	})
	t.Run("unchecked-when-off", func(t *testing.T) {
		mux := mkServer(t, settingsFake(Preferences{}))
		req := httptest.NewRequest("GET", "/settings/edit", nil)
		req.Header.Set("X-Auth-Request-Email", authorEmail)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		body := w.Body.String()
		if !strings.Contains(body, `name="vibe_mode"`) {
			t.Errorf("editor missing vibe_mode checkbox\nbody=%s", body)
		}
		// The vibe_mode input line itself must not carry `checked`. Isolate the
		// input tag so a `checked` elsewhere (e.g. a homepage section) doesn't
		// give a false pass.
		idx := strings.Index(body, `name="vibe_mode"`)
		tagStart := strings.LastIndex(body[:idx], "<input")
		tagEnd := strings.Index(body[idx:], ">") + idx
		tag := body[tagStart:tagEnd]
		if strings.Contains(tag, "checked") {
			t.Errorf("vibe_mode checkbox should NOT be checked when off\ntag=%s", tag)
		}
	})
}

// TestUpdateSettings_VibeModeChecked posts the form with vibe_mode checked and
// asserts VibeMode=true is forwarded to the daemon write.
func TestUpdateSettings_VibeModeChecked(t *testing.T) {
	f := settingsFake(Preferences{})
	mux := mkServer(t, f)
	form := url.Values{"vibe_mode": []string{"1"}}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got, ok := f.updateFieldsLastVal["preferences"].(Preferences)
	if !ok {
		t.Fatalf("expected Preferences, got %T", f.updateFieldsLastVal["preferences"])
	}
	if !got.VibeMode {
		t.Errorf("VibeMode = %v, want true", bool(got.VibeMode))
	}
}

// TestUpdateSettings_VibeModeUnchecked confirms an omitted checkbox persists as
// false — standard unchecked-checkbox semantics.
func TestUpdateSettings_VibeModeUnchecked(t *testing.T) {
	f := settingsFake(Preferences{VibeMode: true})
	mux := mkServer(t, f)
	// No vibe_mode key in the form → unchecked → false.
	form := url.Values{"default_spawn_project": []string{"gardenio"}}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got, ok := f.updateFieldsLastVal["preferences"].(Preferences)
	if !ok {
		t.Fatalf("expected Preferences, got %T", f.updateFieldsLastVal["preferences"])
	}
	if got.VibeMode {
		t.Errorf("VibeMode = %v, want false (unchecked)", bool(got.VibeMode))
	}
}

// TestSettings_VibeModeRoundTrip drives the full open→save→show cycle: enable
// vibe_mode via the editor, persist it, and confirm a subsequent page render
// reflects the on state and pre-fills the checkbox as checked.
func TestSettings_VibeModeRoundTrip(t *testing.T) {
	f := settingsFake(Preferences{})
	bead := f.beads["gd-user-op"]
	mux := mkServer(t, &updatingFake{fakeDaemon: f, bead: bead})

	form := url.Values{"vibe_mode": []string{"1"}}
	saveReq := authedPost("/settings", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", saveW.Code, saveW.Body.String())
	}

	// The stored blob must be the string form so gb prefs / gb prime can read it.
	if raw := bead.Fields["preferences"]; !strings.Contains(raw, `"vibe_mode":"true"`) {
		t.Errorf("persisted blob should carry string vibe_mode, got %q", raw)
	}

	editReq := httptest.NewRequest("GET", "/settings/edit", nil)
	editReq.Header.Set("X-Auth-Request-Email", authorEmail)
	editW := httptest.NewRecorder()
	mux.ServeHTTP(editW, editReq)
	if !strings.Contains(editW.Body.String(), "checked") {
		t.Errorf("editor should pre-fill vibe_mode as checked after save\nbody=%s", editW.Body.String())
	}

	showReq := httptest.NewRequest("GET", "/settings", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	if !strings.Contains(showW.Body.String(), "full functionality") {
		t.Errorf("show: settings page should reflect vibe_mode on\nbody=%s", showW.Body.String())
	}
}

// TestSettings_ProfileSectionsBlockRendered asserts the read-only settings
// block surfaces a row per homepage section so the operator can see what
// will render on /u/{slug}.
func TestSettings_ProfileSectionsBlockRendered(t *testing.T) {
	mux := mkServer(t, settingsFake(Preferences{}))
	req := httptest.NewRequest("GET", "/settings", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Homepage sections",
		`data-slug="pinned"`,
		`data-slug="forwarded-mentions"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q\nbody=%s", want, body)
		}
	}
}

// TestSettingsEdit_ProfileSectionsForm asserts the editor renders one
// checkbox per section plus the hidden section_order input the JS shim
// keeps in sync with the row order.
func TestSettingsEdit_ProfileSectionsForm(t *testing.T) {
	mux := mkServer(t, settingsFake(Preferences{}))
	req := httptest.NewRequest("GET", "/settings/edit", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="section_order"`,
		`name="section_visible_pinned"`,
		`name="section_visible_open-mrs"`,
		`name="section_visible_forwarded-mentions"`,
		`data-dir="up"`,
		`data-dir="down"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor missing %q\nbody=%s", want, body)
		}
	}
}

// TestUpdateSettings_ProfilePrefsPersisted posts the section-customization
// form and asserts the daemon write carries a profile_prefs blob with the
// expected hidden + order fields.
func TestUpdateSettings_ProfilePrefsPersisted(t *testing.T) {
	f := settingsFake(Preferences{})
	mux := mkServer(t, f)
	form := url.Values{
		// active-agents intentionally omitted → hidden
		"section_visible_pinned":             []string{"1"},
		"section_visible_open-decisions":     []string{"1"},
		"section_visible_recent-comments":    []string{"1"},
		"section_visible_open-mrs":           []string{"1"},
		"section_visible_recent-commits":     []string{"1"},
		"section_visible_recent-merged":      []string{"1"},
		"section_visible_forwarded-mentions": []string{"1"},
		"section_visible_their-automations":  []string{"1"},
		"section_order":                      []string{"open-mrs,pinned,bogus,active-agents,open-decisions,recent-comments,recent-commits,recent-merged,their-automations,forwarded-mentions"},
	}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	raw, ok := f.updateFieldsLastVal["profile_prefs"].(string)
	if !ok {
		t.Fatalf("profile_prefs missing or wrong type: %T", f.updateFieldsLastVal["profile_prefs"])
	}
	if !strings.Contains(raw, `"hidden_sections":["active-agents"]`) {
		t.Errorf("hidden_sections not normalized in saved blob: %s", raw)
	}
	if !strings.Contains(raw, `"section_order":["open-mrs","pinned","active-agents","open-decisions","recent-comments","recent-commits","recent-merged","their-automations","forwarded-mentions"]`) {
		t.Errorf("section_order missing or contains unknown slugs: %s", raw)
	}
}

// TestUpdateSettings_ProfilePrefsPinsPreserved guards against the editor
// (which only carries section visibility + order) accidentally clobbering
// PinnedBeadIDs that were already on the user bead.
func TestUpdateSettings_ProfilePrefsPinsPreserved(t *testing.T) {
	f := settingsFake(Preferences{})
	f.beads["gd-user-op"].Fields["profile_prefs"] = `{"pinned_bead_ids":["gd-keepme"]}`
	mux := mkServer(t, f)
	form := url.Values{
		// All visible — only customizing nothing here.
	}
	for _, slug := range allProfileSections {
		form.Set("section_visible_"+slug, "1")
	}
	req := authedPost("/settings", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	raw, _ := f.updateFieldsLastVal["profile_prefs"].(string)
	if !strings.Contains(raw, `"pinned_bead_ids":["gd-keepme"]`) {
		t.Errorf("PinnedBeadIDs were dropped: %s", raw)
	}
}

// TestSettings_ProfilePrefsRoundTrip persists a hide-and-reorder choice and
// verifies a subsequent /u/{slug} render honors it.
func TestSettings_ProfilePrefsRoundTrip(t *testing.T) {
	f := settingsFake(Preferences{})
	bead := f.beads["gd-user-op"]
	// Seed the operator_id so the homepage's lookupProfilePrefByOperator
	// can route from operatorID → user bead. The settings fake seeds an
	// email but not the operator id, so wire it up here.
	bead.Fields["operator_id"] = "42"
	// Wire the slug → operator id mapping the user_home handler relies on.
	wrapped := &updatingFake{fakeDaemon: f, bead: bead}
	wrapped.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, wrapped)

	form := url.Values{
		"section_order":                      []string{"open-mrs,pinned,open-decisions,recent-comments,recent-commits,recent-merged,forwarded-mentions"},
		"section_visible_pinned":             []string{"1"},
		"section_visible_open-mrs":           []string{"1"},
		"section_visible_open-decisions":     []string{"1"},
		"section_visible_recent-comments":    []string{"1"},
		"section_visible_recent-commits":     []string{"1"},
		"section_visible_recent-merged":      []string{"1"},
		"section_visible_forwarded-mentions": []string{"1"},
		// active-agents omitted → hidden
	}
	saveReq := authedPost("/settings", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", saveW.Code, saveW.Body.String())
	}

	// The settings write path persists the hide+reorder choice into the
	// profile_prefs blob on the user bead.
	raw := wrapped.bead.Fields["profile_prefs"]
	if !strings.Contains(raw, "open-mrs") {
		t.Errorf("section_order not persisted into profile_prefs: %s", raw)
	}
	if !strings.Contains(raw, `"hidden_sections"`) || !strings.Contains(raw, "active-agents") {
		t.Errorf("active-agents (omitted) should persist as hidden: %s", raw)
	}

	// The homepage now renders ONE merged activity spine (bug ka-0000002041)
	// rather than per-section cards, so the former section-order-on-homepage
	// assertion no longer applies. What still holds: the page renders 200 with
	// the merged spine, and applyProfilePref keeps hidden populations out of
	// the stream (verified structurally in the activity-stream tests).
	homeReq := httptest.NewRequest("GET", "/u/alice", nil)
	homeW := httptest.NewRecorder()
	mux.ServeHTTP(homeW, homeReq)
	if homeW.Code != http.StatusOK {
		t.Fatalf("home: expected 200, got %d body=%s", homeW.Code, homeW.Body.String())
	}
	body := homeW.Body.String()
	if !strings.Contains(body, `id="activity"`) {
		t.Errorf("expected the merged activity spine on the homepage\nbody=%s", body)
	}
	// The obsolete per-section homepage cards must be gone.
	if strings.Contains(body, `id="active-agents"`) || strings.Contains(body, `id="open-mrs"`) {
		t.Errorf("stale per-section homepage cards still present\nbody=%s", body)
	}
}
