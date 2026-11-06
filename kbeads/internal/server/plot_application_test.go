package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Lands step 7.4: the plot_application type, the lands-intake label rule, the
// field rules and the state machine (plot_application.go).

func validPlotApplicationFields() map[string]any {
	return map[string]any{
		"applicant_sub":           "kc-sub-1",
		"applicant_email":         "stranger@example.com",
		"github_login":            "stranger",
		"github_id":               "4242",
		"land_name":               "tomato-patch",
		"repo":                    "stranger/tomatoes",
		"repo_private":            false,
		"github_installation_id":  "777",
		"purpose":                 "Grow a small web app.",
		"credits_requested_usd":   50.0,
		"terms_version":           "2026-10-19",
		"aup_version":             "2026-10-19",
		"github_account_age_days": 900,
		"github_public_repos":     12,
		"state":                   "submitted",
	}
}

func plotApplicationBody(fields map[string]any, labels ...string) map[string]any {
	if labels == nil {
		labels = []string{"project:" + landsIntakeProject}
	}
	return map[string]any{
		"title":  "Plot application: tomato-patch",
		"type":   string(typePlotApplication),
		"labels": labels,
		"fields": fields,
	}
}

func withField(k string, v any) map[string]any {
	f := validPlotApplicationFields()
	if v == nil {
		delete(f, k)
	} else {
		f[k] = v
	}
	return f
}

// createPlotApplication files a valid application as an OIDC admin and returns
// its id.
func createPlotApplication(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", plotApplicationBody(validPlotApplicationFields()))
	requireStatus(t, rec, http.StatusCreated)
	var b model.Bead
	decodeJSON(t, rec, &b)
	return b.ID
}

func TestPlotApplication_TypeRegistered(t *testing.T) {
	if !strings.Contains(strings.Join(BuiltinBeadTypeNames(), ","), string(typePlotApplication)) {
		t.Fatalf("plot_application is not a builtin type: %v", BuiltinBeadTypeNames())
	}
	s, _, _ := newTestServer()
	tc, err := s.resolveTypeConfig(context.Background(), typePlotApplication)
	if err != nil || tc == nil {
		t.Fatalf("resolveTypeConfig: %v %v", tc, err)
	}
	if tc.Kind != model.KindData || tc.WritePolicy == nil || tc.WritePolicy.MinRole != RoleAdmin {
		t.Fatalf("plot_application must be data-kind and admin-gated: %+v", tc)
	}
	var state *model.FieldDef
	for i := range tc.Fields {
		if tc.Fields[i].Name == "state" {
			state = &tc.Fields[i]
		}
	}
	if state == nil || state.Type != model.FieldTypeEnum || !state.Required {
		t.Fatalf("state must be a required enum: %+v", state)
	}
	// Every state the lifecycle knows is an enum value, and the reverse.
	if len(state.Values) != len(plotStateEdges) {
		t.Fatalf("state enum %v and plotStateEdges disagree", state.Values)
	}
	for _, v := range state.Values {
		if _, ok := plotStateEdges[v]; !ok {
			t.Errorf("state enum value %q has no lifecycle entry", v)
		}
	}
}

func TestPlotApplication_CreateValid(t *testing.T) {
	_, ms, h := newTestServer()
	id := createPlotApplication(t, h)
	b := ms.beads[id]
	if b == nil || b.Type != typePlotApplication {
		t.Fatalf("bead not stored as plot_application: %+v", b)
	}
	if got := ms.labels[id]; len(got) != 1 || got[0] != "project:lands-intake" {
		t.Fatalf("labels = %v", got)
	}
}

func TestPlotApplication_CreateRejected(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no project label", plotApplicationBody(validPlotApplicationFields(), "lands"), "needs the label project:lands-intake"},
		{"another project", plotApplicationBody(validPlotApplicationFields(), "project:gardenio"), "exactly one project label"},
		{"two projects", plotApplicationBody(validPlotApplicationFields(), "project:lands-intake", "project:gardenio"), "exactly one project label"},
		{"born approved", plotApplicationBody(withField("state", "approved")), "created in state"},
		{"unknown state", plotApplicationBody(withField("state", "pending")), "invalid fields"},
		{"missing state", plotApplicationBody(withField("state", nil)), "invalid fields"},
		{"missing repo", plotApplicationBody(withField("repo", nil)), "invalid fields"},
		{"decided_by at create", plotApplicationBody(withField("decided_by", "oidc:op@example.com")), "decided_by"},
		{"credits_granted at create", plotApplicationBody(withField("credits_granted_usd", 10.0)), "credits_granted_usd"},
		{"project at create", plotApplicationBody(withField("project", "tomato-patch")), "project"},
		{"model at create", plotApplicationBody(withField("model", "claude-opus-5-5")), "model"},
		{"question at create", plotApplicationBody(withField("question", "Which branch?")), "question"},
		{"land_name uppercase", plotApplicationBody(withField("land_name", "Tomato")), "DNS label"},
		{"land_name too short", plotApplicationBody(withField("land_name", "ab")), "DNS label"},
		{"land_name too long", plotApplicationBody(withField("land_name", strings.Repeat("a", 31))), "DNS label"},
		{"land_name trailing dash", plotApplicationBody(withField("land_name", "tomato-")), "DNS label"},
		{"land_name reserved", plotApplicationBody(withField("land_name", "gardenio")), "reserved"},
		{"land_name intake", plotApplicationBody(withField("land_name", "lands-intake")), "reserved"},
		{"repo not owner/name", plotApplicationBody(withField("repo", "tomatoes")), "owner/name"},
		{"repo url", plotApplicationBody(withField("repo", "https://github.com/a/b")), "owner/name"},
		{"purpose too long", plotApplicationBody(withField("purpose", strings.Repeat("x", 1001))), "limit is 1000"},
		{"negative credits", plotApplicationBody(withField("credits_requested_usd", -1.0)), "negative"},
		{"negative account age", plotApplicationBody(withField("github_account_age_days", -3)), "invalid fields"},
		{"unknown field", plotApplicationBody(withField("favourite_vegetable", "kale")), "invalid fields"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			rec := doJSON(t, h, http.MethodPost, "/v1/beads", c.body)
			requireStatus(t, rec, http.StatusBadRequest)
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("body %s does not mention %q", rec.Body.String(), c.want)
			}
			for _, b := range ms.beads {
				if b.Type == typePlotApplication {
					t.Fatalf("a rejected application was stored: %s", b.ID)
				}
			}
		})
	}
}

func TestPlotApplication_LandNameIsExistingProject(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["kd-proj-tomato"] = &model.Bead{ID: "kd-proj-tomato", Title: "tomato-patch", Type: model.TypeProject, Kind: model.KindConfig, Status: model.StatusOpen}
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", plotApplicationBody(validPlotApplicationFields()))
	requireStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "already a project") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// A name already in use other than as a live project bead is refused too: a
// fleet project known only by its project: label, or a soft-deleted project
// bead. A land granted the name would get owner tier over those beads
// (gd-i74esUHCTg item 5).
func TestPlotApplication_LandNameInUseByLabelOrDeletedProject(t *testing.T) {
	gone := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		seed func(ms *mockStore)
	}{
		{"a bead labelled project:<name>", func(ms *mockStore) {
			ms.beads["kd-fleet-task"] = &model.Bead{ID: "kd-fleet-task", Title: "fleet work", Type: model.TypeTask, Status: model.StatusOpen}
			ms.labels["kd-fleet-task"] = []string{"project:tomato-patch"}
		}},
		{"a soft-deleted project bead", func(ms *mockStore) {
			ms.beads["kd-proj-old"] = &model.Bead{ID: "kd-proj-old", Title: "Project: tomato-patch", Type: model.TypeProject, Kind: model.KindConfig, Status: model.StatusClosed, DeletedAt: &gone}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			tc.seed(ms)
			rec := doJSON(t, h, http.MethodPost, "/v1/beads", plotApplicationBody(validPlotApplicationFields()))
			requireStatus(t, rec, http.StatusBadRequest)
			if !strings.Contains(rec.Body.String(), "already in use") {
				t.Fatalf("body = %s", rec.Body.String())
			}
			for _, f := range ms.listFilters {
				if len(f.Titles) > 0 || len(f.Labels) > 0 {
					if !f.IncludeDeleted {
						t.Errorf("name lookup %+v skips soft-deleted beads", f)
					}
				}
			}
		})
	}
}

// A name that comes into use after the application was filed is caught when
// it is approved, and a rename before approval is checked like a new name.
func TestPlotApplication_NameRecheckedAtApproval(t *testing.T) {
	_, ms, h := newTestServer()
	id := createPlotApplication(t, h)
	ms.beads["kd-fleet-task"] = &model.Bead{ID: "kd-fleet-task", Title: "fleet work", Type: model.TypeTask, Status: model.StatusOpen}
	ms.labels["kd-fleet-task"] = []string{"project:tomato-patch", "project:bean-row"}

	approve := map[string]any{"state": "approved", "decided_by": "oidc:op@example.com", "decided_at": "2026-10-19T08:00:00Z"}
	rec := doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": approve})
	requireStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "already in use") {
		t.Fatalf("approve body = %s", rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"land_name": "bean-row"}})
	requireStatus(t, rec, http.StatusBadRequest)

	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"land_name": "pea-pod"}})
	requireStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": approve})
	requireStatus(t, rec, http.StatusOK)
}

// The type is admin-gated: the shared static token every agent pod holds may
// not file an application, so an agent cannot forge one for a stranger.
func TestPlotApplication_StaticTokenCannotCreate(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
	rec := fieldWriteRequest(t, h, fieldWriteCase{method: http.MethodPost, path: "/v1/beads",
		body: plotApplicationBody(validPlotApplicationFields())}, "primary-token", "slack:U0TESTER1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("static token create: code = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	for _, b := range ms.beads {
		if b.Type == typePlotApplication {
			t.Fatalf("static token stored an application: %s", b.ID)
		}
	}
}

func TestPlotApplication_StateMachine(t *testing.T) {
	_, _, h := newTestServer()
	id := createPlotApplication(t, h)
	patch := func(fields map[string]any) int {
		return doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": fields}).Code
	}
	steps := []struct {
		fields map[string]any
		want   int
	}{
		{map[string]any{"state": "provisioned"}, http.StatusBadRequest},
		// The Gate points the application at its decision and pins what it asks.
		{map[string]any{"decision_id": "kd-dec1", "decision_digest": "9f2c"}, http.StatusOK},
		// An "ask" keeps the application submitted and records the question.
		{map[string]any{"question": "Which branch should agents use?", "asked_at": "2026-10-19T07:00:00Z"}, http.StatusOK},
		{map[string]any{"state": "approved", "decided_by": "oidc:op@example.com", "decided_at": "2026-10-19T08:00:00Z", "credits_granted_usd": 50.0, "model": "claude-sonnet-5-5"}, http.StatusOK},
		{map[string]any{"state": "submitted"}, http.StatusBadRequest},
		{map[string]any{"purpose": "edited after approval"}, http.StatusOK},
		{map[string]any{"credits_granted_usd": -5.0}, http.StatusBadRequest},
		{map[string]any{"state": "provisioning"}, http.StatusOK},
		{map[string]any{"state": "failed"}, http.StatusOK},
		{map[string]any{"state": "provisioning"}, http.StatusOK},
		{map[string]any{"state": "provisioned", "project": "tomato-patch"}, http.StatusOK},
		{map[string]any{"state": "closed"}, http.StatusOK},
		{map[string]any{"state": "approved"}, http.StatusBadRequest},
	}
	for i, st := range steps {
		if got := patch(st.fields); got != st.want {
			t.Fatalf("step %d %v: code %d, want %d", i, st.fields, got, st.want)
		}
	}
}

func TestPlotApplication_StateEdges(t *testing.T) {
	legal := map[[2]string]bool{}
	for from, tos := range plotStateEdges {
		for _, to := range tos {
			legal[[2]string{from, to}] = true
		}
	}
	base := validPlotApplicationFields()
	enc := func(state string) json.RawMessage {
		f := map[string]any{}
		for k, v := range base {
			f[k] = v
		}
		f["state"] = state
		b, _ := json.Marshal(f)
		return b
	}
	for from := range plotStateEdges {
		for to := range plotStateEdges {
			err := validatePlotApplicationUpdate(enc(from), enc(to))
			want := from == to || legal[[2]string{from, to}]
			if (err == nil) != want {
				t.Errorf("%s → %s: err = %v, want legal = %v", from, to, err, want)
			}
		}
	}
	// The spec's chain (§2.4) is all there.
	for _, e := range [][2]string{
		{"submitted", "approved"}, {"submitted", "denied"}, {"submitted", "withdrawn"},
		{"approved", "provisioning"}, {"provisioning", "provisioned"}, {"provisioning", "failed"},
		{"provisioned", "closed"}, {"failed", "closed"}, {"denied", "closed"}, {"withdrawn", "closed"},
	} {
		if !legal[e] {
			t.Errorf("spec edge %s → %s is missing", e[0], e[1])
		}
	}
	if len(plotStateEdges[plotStateClosed]) != 0 {
		t.Errorf("closed must be terminal")
	}
}

func TestPlotApplication_StaysInLandsIntake(t *testing.T) {
	_, ms, h := newTestServer()
	id := createPlotApplication(t, h)

	// Label routes: the intake label cannot be removed and no project label
	// can be added; any other label is fine.
	rec := doJSON(t, h, http.MethodDelete, "/v1/beads/"+id+"/labels/project:lands-intake", nil)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads/"+id+"/labels", map[string]any{"label": "project:gardenio"})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads/"+id+"/labels", map[string]any{"label": "needs-review"})
	requireStatus(t, rec, http.StatusCreated)

	// PATCH labels: dropping or replacing the intake label is refused.
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"labels": []string{"needs-review"}})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"labels": []string{"project:tomato-patch"}})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"labels": []string{"project:lands-intake", "seen"}})
	requireStatus(t, rec, http.StatusOK)

	found := false
	for _, l := range ms.labels[id] {
		if l == "project:lands-intake" {
			found = true
		}
		if strings.HasPrefix(l, "project:") && l != "project:lands-intake" {
			t.Errorf("foreign project label stored: %s", l)
		}
	}
	if !found {
		t.Fatalf("intake label lost: %v", ms.labels[id])
	}
}

// The label rule touches plot applications only.
func TestPlotApplicationLabelChangeError_OtherTypesUntouched(t *testing.T) {
	task := &model.Bead{ID: "kd-t", Type: model.TypeTask, Labels: []string{"project:a"}}
	if err := plotApplicationLabelChangeError(task, "project:a", false); err != nil {
		t.Fatalf("task label change refused: %v", err)
	}
	app := &model.Bead{ID: "kd-p", Type: typePlotApplication, Labels: []string{"project:lands-intake"}}
	if err := plotApplicationLabelChangeError(app, "triage", true); err != nil {
		t.Fatalf("non-project label refused: %v", err)
	}
	if err := plotApplicationLabelChangeError(app, "project:lands-intake", true); err != nil {
		t.Fatalf("re-adding the intake label refused: %v", err)
	}
}
