package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// heraldry gate (badges plan step 2, gd-fn7uPujOqj). These tests mirror
// short_title_gate_test.go case for case: the BEADS_HERALDRY_GATE parse, the
// gated type set, the X-Beads-Warning header in warn mode and the agent-only
// refusal of a plan or phase epic in enforce. The tenant-land env is pinned
// on top.

func TestHeraldryGateModeFromEnv(t *testing.T) {
	cases := []struct {
		val        string
		want       heraldryGateMode
		wantTenant heraldryGateMode
	}{
		{"", heraldryGateWarn, heraldryGateOff}, // unset → home warns, lands off
		{"  ", heraldryGateWarn, heraldryGateOff},
		{"warn", heraldryGateWarn, heraldryGateWarn},
		{"WARN", heraldryGateWarn, heraldryGateWarn},
		{"enforce", heraldryGateEnforce, heraldryGateEnforce},
		{"1", heraldryGateEnforce, heraldryGateEnforce},
		{"true", heraldryGateEnforce, heraldryGateEnforce},
		{"on", heraldryGateEnforce, heraldryGateEnforce},
		{"off", heraldryGateOff, heraldryGateOff},
		{"0", heraldryGateOff, heraldryGateOff},
		{"false", heraldryGateOff, heraldryGateOff},
		{"bogus", heraldryGateOff, heraldryGateOff}, // a typo never refuses
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("BEADS_HERALDRY_GATE", tc.val)
			t.Setenv("BEADS_HERALDRY_GATE_TENANT_LANDS", tc.val)
			if got := heraldryGateModeFromEnv(); got != tc.want {
				t.Errorf("heraldryGateModeFromEnv() with %q = %v; want %v", tc.val, got, tc.want)
			}
			if got := heraldryGateTenantModeFromEnv(); got != tc.wantTenant {
				t.Errorf("heraldryGateTenantModeFromEnv() with %q = %v; want %v", tc.val, got, tc.wantTenant)
			}
		})
	}
}

func TestIsHeraldryGatedType(t *testing.T) {
	if !isHeraldryGatedType(model.TypeEpic) {
		t.Errorf("isHeraldryGatedType(epic) = false; want true")
	}
	for _, bt := range []model.BeadType{
		model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeDecision,
		model.TypeEscalation, model.TypeProject, "agent", "session", "message",
		"bundle", "molecule", "trigger", "schedule", model.TypeFormula, model.TypeAdvice, "",
	} {
		if isHeraldryGatedType(bt) {
			t.Errorf("isHeraldryGatedType(%q) = true; want false", bt)
		}
	}
}

func TestIsPlanEpicCreate(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic}
	ms.labels["gd-plan"] = []string{"plan", "project:gardenio"}
	ms.beads["gd-epic"] = &model.Bead{ID: "gd-epic", Type: model.TypeEpic}
	ms.beads["gd-task"] = &model.Bead{ID: "gd-task", Type: model.TypeTask}
	ms.labels["gd-task"] = []string{"plan"}

	cases := map[string]struct {
		in   createBeadInput
		want bool
	}{
		"plan label":        {createBeadInput{Labels: []string{"project:gardenio", "plan"}}, true},
		"no parent":         {createBeadInput{Labels: []string{"project:gardenio"}}, false},
		"parent is plan":    {createBeadInput{ParentBeadID: "gd-plan"}, true},
		"parent not plan":   {createBeadInput{ParentBeadID: "gd-epic"}, false},
		"parent task label": {createBeadInput{ParentBeadID: "gd-task"}, false},
		"parent unknown":    {createBeadInput{ParentBeadID: "gd-none"}, false},
	}
	for name, tc := range cases {
		if got := s.isPlanEpicCreate(shortTitleGateCtx(shortTitleGateCallers["fleet"].id), tc.in); got != tc.want {
			t.Errorf("%s: isPlanEpicCreate = %v; want %v", name, got, tc.want)
		}
	}
}

// heraldryGateEnv sets the heraldry gate mode and turns the short_title gate
// off, so the only X-Beads-Warning a create returns is the heraldry one.
func heraldryGateEnv(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("BEADS_SHORT_TITLE_GATE", "off")
	t.Setenv("BEADS_HERALDRY_GATE", mode)
	heraldryGateTotal.Reset()
}

func planEpicBody(extra map[string]any) map[string]any {
	body := map[string]any{"title": "Badges from arms", "type": "epic", "labels": []string{"project:gardenio", "plan"}}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func requireHeraldryWarning(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	got := rec.Header().Values("X-Beads-Warning")
	if len(got) != 1 {
		t.Fatalf("X-Beads-Warning = %q; want exactly one", got)
	}
	var bead model.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &bead); err != nil || bead.ID == "" {
		t.Fatalf("decode created bead: %v (%s)", err, rec.Body.String())
	}
	if want := `heraldry missing (kd update ` + bead.ID + ` --heraldry "...")`; got[0] != want {
		t.Errorf("X-Beads-Warning = %q; want %q", got[0], want)
	}
}

func TestCreateBead_HeraldryGate_WarnAddsHeader(t *testing.T) {
	for _, mode := range []string{"", "warn"} {
		t.Run("mode="+mode, func(t *testing.T) {
			heraldryGateEnv(t, mode)
			s, _, _ := newTestServer()

			rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["fleet"].id), planEpicBody(nil))
			requireStatus(t, rec, http.StatusCreated)
			requireHeraldryWarning(t, rec)
			if got := testutil.ToFloat64(heraldryGateTotal.WithLabelValues("agent", "allowed_warn")); got != 1 {
				t.Errorf("agent/allowed_warn = %v; want 1", got)
			}

			// Supplying arms clears the warning and counts as supplied.
			rec = postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["fleet"].id),
				planEpicBody(map[string]any{"heraldry": model.HeraldryExample}))
			requireStatus(t, rec, http.StatusCreated)
			if got := rec.Header().Values("X-Beads-Warning"); len(got) != 0 {
				t.Errorf("X-Beads-Warning = %q; want none when heraldry is supplied", got)
			}
			if got := testutil.ToFloat64(heraldryGateTotal.WithLabelValues("agent", "supplied")); got != 1 {
				t.Errorf("agent/supplied = %v; want 1", got)
			}
		})
	}
}

func TestCreateBead_HeraldryGate_EnforceRefusesAgent(t *testing.T) {
	for _, name := range []string{"fleet", "machine"} {
		t.Run(name, func(t *testing.T) {
			heraldryGateEnv(t, "enforce")
			s, ms, _ := newTestServer()
			before := len(ms.beads)

			rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers[name].id), planEpicBody(nil))
			requireStatus(t, rec, http.StatusBadRequest)
			var body struct {
				Error string `json:"error"`
			}
			decodeJSON(t, rec, &body)
			if want := string(heraldryRequiredError()); body.Error != want {
				t.Errorf("error = %q; want %q", body.Error, want)
			}
			if !strings.HasPrefix(body.Error, `heraldry missing (kd create --heraldry "...")`) {
				t.Errorf("error = %q; want the heraldry missing words first", body.Error)
			}
			if len(ms.beads) != before {
				t.Errorf("refused create persisted a bead")
			}
			if got := testutil.ToFloat64(heraldryGateTotal.WithLabelValues("agent", "refused")); got != 1 {
				t.Errorf("agent/refused = %v; want 1", got)
			}

			// The same caller with arms is allowed.
			rec = postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers[name].id),
				planEpicBody(map[string]any{"heraldry": model.HeraldryExample}))
			requireStatus(t, rec, http.StatusCreated)
		})
	}
}

// TestCreateBead_HeraldryGate_EnforceRefusesPhase pins the second enforce
// arm: an epic whose parent_bead_id is a plan (a phase) is refused too, while
// an epic outside any plan is only warned.
func TestCreateBead_HeraldryGate_EnforceRefusesPhase(t *testing.T) {
	heraldryGateEnv(t, "enforce")
	s, ms, _ := newTestServer()
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic}
	ms.labels["gd-plan"] = []string{"plan", "project:gardenio"}
	ctx := shortTitleGateCtx(shortTitleGateCallers["fleet"].id)

	rec := postCreate(t, s, ctx, map[string]any{
		"title": "Phase 1", "type": "epic", "labels": []string{"project:gardenio"}, "parent_bead_id": "gd-plan",
	})
	requireStatus(t, rec, http.StatusBadRequest)

	rec = postCreate(t, s, ctx, map[string]any{"title": "Loose epic", "type": "epic", "labels": []string{"project:gardenio"}})
	requireStatus(t, rec, http.StatusCreated)
	requireHeraldryWarning(t, rec)
	if got := testutil.ToFloat64(heraldryGateTotal.WithLabelValues("agent", "refused")); got != 1 {
		t.Errorf("agent/refused = %v; want 1", got)
	}
	if got := testutil.ToFloat64(heraldryGateTotal.WithLabelValues("agent", "allowed_warn")); got != 1 {
		t.Errorf("agent/allowed_warn = %v; want 1", got)
	}
}

func TestCreateBead_HeraldryGate_EnforceWarnsHuman(t *testing.T) {
	heraldryGateEnv(t, "enforce")
	s, _, _ := newTestServer()

	rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["oidc"].id), planEpicBody(nil))
	requireStatus(t, rec, http.StatusCreated)
	requireHeraldryWarning(t, rec)
	if got := testutil.ToFloat64(heraldryGateTotal.WithLabelValues("human", "allowed_warn")); got != 1 {
		t.Errorf("human/allowed_warn = %v; want 1", got)
	}
}

// TestCreateBead_HeraldryGate_EnforceRefusesGRPCPath pins that the refusal
// lives at the createBead choke point, so the gRPC transport (no warning
// collector) is held to it too, and a warn-mode create with no collector
// still succeeds.
func TestCreateBead_HeraldryGate_EnforceRefusesGRPCPath(t *testing.T) {
	s, _, _ := newTestServer()
	ctx := shortTitleGateCtx(shortTitleGateCallers["fleet"].id)
	in := createBeadInput{Title: "a plan", ShortTitle: "a plan", Type: "epic", Labels: []string{"plan"}}

	heraldryGateEnv(t, "enforce")
	_, err := s.createBead(ctx, in)
	var ie inputError
	if !errors.As(err, &ie) {
		t.Fatalf("createBead error = %v; want inputError", err)
	}

	heraldryGateEnv(t, "warn")
	if _, err := s.createBead(ctx, in); err != nil {
		t.Fatalf("createBead (warn, no collector) error = %v; want success", err)
	}
}

func TestCreateBead_HeraldryGate_OffIsSilent(t *testing.T) {
	heraldryGateEnv(t, "off")
	s, _, _ := newTestServer()

	rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["fleet"].id), planEpicBody(nil))
	requireStatus(t, rec, http.StatusCreated)
	if got := rec.Header().Values("X-Beads-Warning"); len(got) != 0 {
		t.Errorf("X-Beads-Warning = %q; want none when off", got)
	}
	if got := testutil.CollectAndCount(heraldryGateTotal); got != 0 {
		t.Errorf("heraldry gate series = %d; want 0 when off", got)
	}
}

func TestCreateBead_HeraldryGate_OtherTypesUntouched(t *testing.T) {
	heraldryGateEnv(t, "enforce")
	s, _, _ := newTestServer()

	for _, tc := range []struct {
		typ    string
		fields string
	}{
		{"task", ""},
		{"message", ""},
		{"trigger", `{"action":"notify"}`},
	} {
		body := map[string]any{"title": "plain " + tc.typ, "type": tc.typ, "labels": []string{"plan"}}
		if tc.fields != "" {
			body["fields"] = json.RawMessage(tc.fields)
		}
		rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["fleet"].id), body)
		requireStatus(t, rec, http.StatusCreated)
		if got := rec.Header().Values("X-Beads-Warning"); len(got) != 0 {
			t.Errorf("%s: X-Beads-Warning = %q; want none", tc.typ, got)
		}
	}
	if got := testutil.CollectAndCount(heraldryGateTotal); got != 0 {
		t.Errorf("heraldry gate series = %d; want 0 for other types", got)
	}
}

// TestCreateBead_HeraldryGate_TenantLandMode pins the per-land split: an epic
// in a tenant land follows BEADS_HERALDRY_GATE_TENANT_LANDS (default off), an
// epic in the home garden follows BEADS_HERALDRY_GATE.
func TestCreateBead_HeraldryGate_TenantLandMode(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beads["proj-claimlayer"] = &model.Bead{
		ID: "proj-claimlayer", Type: "project", Kind: "config", Title: "claimlayer",
		Fields: json.RawMessage(`{"land_kind":"tenant"}`),
	}
	ctx := shortTitleGateCtx(shortTitleGateCallers["oidc"].id)
	tenantEpic := map[string]any{"title": "Land plan", "type": "epic", "labels": []string{"project:claimlayer", "plan"}}

	heraldryGateEnv(t, "enforce")
	rec := postCreate(t, s, ctx, tenantEpic)
	requireStatus(t, rec, http.StatusCreated)
	if got := rec.Header().Values("X-Beads-Warning"); len(got) != 0 {
		t.Errorf("tenant land, default: X-Beads-Warning = %q; want none", got)
	}
	if got := testutil.CollectAndCount(heraldryGateTotal); got != 0 {
		t.Errorf("tenant land, default: heraldry gate series = %d; want 0", got)
	}

	rec = postCreate(t, s, ctx, planEpicBody(nil))
	requireStatus(t, rec, http.StatusCreated)
	requireHeraldryWarning(t, rec)

	t.Setenv("BEADS_HERALDRY_GATE_TENANT_LANDS", "warn")
	rec = postCreate(t, s, ctx, tenantEpic)
	requireStatus(t, rec, http.StatusCreated)
	requireHeraldryWarning(t, rec)
}

// TestCreateBead_HeraldryGate_KdRoundtrip is the kd acceptance check without a
// Postgres: the REAL client.HTTPClient `kd create` uses, against the REAL HTTP
// handler over a socket (only the store is mocked). An epic created with no
// arms comes back with the warning kd create prints as "warning: …".
func TestCreateBead_HeraldryGate_KdRoundtrip(t *testing.T) {
	heraldryGateEnv(t, "")
	_, _, h := newTestServer()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := client.NewHTTPClient(srv.URL, "")

	ctx, warnings := client.WithWarnings(context.Background())
	bead, err := c.CreateBead(ctx, &client.CreateBeadRequest{
		Title: "Badges from arms", ShortTitle: "badges from arms", Type: "epic",
		Labels: []string{"project:gardenio"},
	})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	want := []string{`heraldry missing (kd update ` + bead.ID + ` --heraldry "...")`}
	if got := warnings.List(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("warnings = %q; want %q", got, want)
	}
}
