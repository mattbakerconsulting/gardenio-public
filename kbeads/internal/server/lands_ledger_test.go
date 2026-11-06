package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Lands step 8.1: the credit ledger, the rate table and the tenant-land
// mirror rule (lands_ledger.go).

// createLand files a project bead as an OIDC admin and returns its id.
func createLand(t *testing.T, h http.Handler, ms *mockStore, name, kind string) string {
	t.Helper()
	if kind == landKindTenant {
		registerUntrustedInput(t, ms)
	}
	fields := map[string]any{"prefix": "tp"}
	if kind != "" {
		fields["land_kind"] = kind
	}
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", map[string]any{
		"title": name, "type": "project", "kind": "config", "fields": fields,
	})
	requireStatus(t, rec, http.StatusCreated)
	var b model.Bead
	decodeJSON(t, rec, &b)
	return b.ID
}

func creditGrantBody(fields map[string]any, labels ...string) map[string]any {
	if labels == nil {
		labels = []string{"project:tomato-patch"}
	}
	return map[string]any{
		"title":  "Credit grant: tomato-patch",
		"type":   string(typeCreditGrant),
		"status": "closed",
		"labels": labels,
		"fields": fields,
	}
}

func validCreditGrantFields() map[string]any {
	return map[string]any{
		"land":           "tomato-patch",
		"amount_credits": 50.0,
		"reason":         "welcome grant",
		"granted_by":     "oidc:op@example.com",
		"decision_id":    "kd-dec",
		"rate_version":   1,
	}
}

func grantWith(k string, v any) map[string]any {
	f := validCreditGrantFields()
	if v == nil {
		delete(f, k)
	} else {
		f[k] = v
	}
	return f
}

func validCreditRateFields() map[string]any {
	return map[string]any{
		"version":                        1,
		"effective_at":                   "2026-10-19T15:00:00Z",
		"llm_multiplier":                 1.5,
		"compute_credits_per_agent_hour": 0.0,
		"set_by":                         "oidc:op@example.com",
	}
}

func creditRateBody(fields map[string]any) map[string]any {
	return map[string]any{"title": "Credit rate v1", "type": string(typeCreditRate), "kind": "config", "fields": fields}
}

func TestLandsLedger_TypesRegistered(t *testing.T) {
	s, _, _ := newTestServer()
	for typ, kind := range map[model.BeadType]model.Kind{typeCreditGrant: model.KindData, typeCreditRate: model.KindConfig} {
		if !strings.Contains(strings.Join(BuiltinBeadTypeNames(), ","), string(typ)) {
			t.Fatalf("%s is not a builtin type", typ)
		}
		tc, err := s.resolveTypeConfig(context.Background(), typ)
		if err != nil || tc == nil {
			t.Fatalf("resolveTypeConfig(%s): %v %v", typ, tc, err)
		}
		if tc.Kind != kind || tc.WritePolicy == nil || tc.WritePolicy.MinRole != RoleAdmin {
			t.Errorf("%s must be %s-kind and admin-gated: %+v", typ, kind, tc)
		}
	}
	// The §3.2 land fields are on type:project.
	tc := parseBuiltinSchema(t, "project")
	declared := map[string]model.FieldDef{}
	for _, f := range tc.Fields {
		declared[f.Name] = f
	}
	for name, typ := range map[string]model.FieldType{
		"land_kind": model.FieldTypeEnum, "land_owner_sub": model.FieldTypeString,
		"land_application_id": model.FieldTypeString, "land_opened_at": model.FieldTypeTimestamp,
		"land_frozen": model.FieldTypeBoolean, "land_slack_mirror": model.FieldTypeEnum,
		"github_app": model.FieldTypeEnum, "github_installation_id": model.FieldTypeString,
		"max_live_agents": model.FieldTypeInteger, "namespace": model.FieldTypeString,
		"runtime_class": model.FieldTypeString,
		"land_frozen_by": model.FieldTypeString, "land_frozen_at": model.FieldTypeString,
		"land_frozen_reason": model.FieldTypeString,
	} {
		f, ok := declared[name]
		if !ok || f.Type != typ {
			t.Errorf("type:project %s = %+v, want type %s", name, f, typ)
		}
	}
	if got := declared["github_app"].Values; strings.Join(got, ",") != "fleet,lands" {
		t.Errorf("github_app values = %v", got)
	}
}

func TestCreditGrant_CreateValid(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", creditGrantBody(validCreditGrantFields()))
	requireStatus(t, rec, http.StatusCreated)
	var b model.Bead
	decodeJSON(t, rec, &b)
	if got := ms.labels[b.ID]; len(got) != 1 || got[0] != "project:tomato-patch" {
		t.Fatalf("labels = %v", got)
	}
	// A negative grant with a reason is a refund.
	rec = doJSON(t, h, http.MethodPost, "/v1/beads", creditGrantBody(grantWith("amount_credits", -5.0)))
	requireStatus(t, rec, http.StatusCreated)
}

func TestCreditGrant_CreateRejected(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	createLand(t, h, ms, "gardenio-own", "")
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no project label", creditGrantBody(validCreditGrantFields(), "lands"), "needs the label project:tomato-patch"},
		{"wrong project label", creditGrantBody(validCreditGrantFields(), "project:other"), "exactly one project label"},
		{"two project labels", creditGrantBody(validCreditGrantFields(), "project:tomato-patch", "project:gardenio"), "exactly one project label"},
		{"zero amount", creditGrantBody(grantWith("amount_credits", 0.0)), "nonzero"},
		{"negative without reason", nil, ""}, // set below: two changes at once
		{"no rate version", creditGrantBody(grantWith("rate_version", nil)), "rate_version"},
		{"rate version 0", creditGrantBody(grantWith("rate_version", 0)), "rate_version"},
		{"unknown land", creditGrantBody(grantWith("land", "nowhere"), "project:nowhere"), "not a project"},
		{"operator land", creditGrantBody(grantWith("land", "gardenio-own"), "project:gardenio-own"), "not a tenant land"},
	}
	neg := validCreditGrantFields()
	neg["amount_credits"] = -5.0
	delete(neg, "reason")
	cases[4].body = creditGrantBody(neg)
	cases[4].want = "needs a reason"
	for _, tc := range cases {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: code %d body %s, want 400 containing %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestCreditGrant_Immutable(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", creditGrantBody(validCreditGrantFields()))
	requireStatus(t, rec, http.StatusCreated)
	var b model.Bead
	decodeJSON(t, rec, &b)
	id := b.ID

	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"amount_credits": 500.0}})
	requireStatus(t, rec, http.StatusBadRequest)
	// Re-sending the same values is not a change.
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"amount_credits": 50.0}})
	requireStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"notes": "checked"})
	requireStatus(t, rec, http.StatusOK)

	// The land label is fixed.
	rec = doJSON(t, h, http.MethodDelete, "/v1/beads/"+id+"/labels/project:tomato-patch", nil)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads/"+id+"/labels", map[string]any{"label": "project:gardenio"})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads/"+id+"/labels", map[string]any{"label": "sla-credit"})
	requireStatus(t, rec, http.StatusCreated)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"labels": []string{"project:other"}})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"labels": []string{"project:tomato-patch", "seen"}})
	requireStatus(t, rec, http.StatusOK)
	if got := ms.beads[id]; got == nil {
		t.Fatal("grant gone")
	}
}

func TestCreditRate_CreateAndImmutable(t *testing.T) {
	_, _, h := newTestServer()
	for _, tc := range []struct {
		name string
		k    string
		v    any
		want string
	}{
		{"zero multiplier", "llm_multiplier", 0.0, "llm_multiplier"},
		{"negative multiplier", "llm_multiplier", -1.5, "llm_multiplier"},
		{"no multiplier", "llm_multiplier", nil, "llm_multiplier"},
		{"negative compute", "compute_credits_per_agent_hour", -0.25, "compute_credits_per_agent_hour"},
		{"version 0", "version", 0, "version"},
		{"no effective_at", "effective_at", nil, "effective_at"},
	} {
		f := validCreditRateFields()
		if tc.v == nil {
			delete(f, tc.k)
		} else {
			f[tc.k] = tc.v
		}
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", creditRateBody(f))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: code %d body %s, want 400 containing %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}

	f := validCreditRateFields()
	delete(f, "compute_credits_per_agent_hour") // optional, defaults to 0
	body := creditRateBody(f)
	body["idempotency_key"] = "credit_rate:v1" // what gb lands rates set sends
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", body)
	requireStatus(t, rec, http.StatusCreated)
	var b model.Bead
	decodeJSON(t, rec, &b)
	// A second write of v1 returns the first instead of a second v1.
	rec = doJSON(t, h, http.MethodPost, "/v1/beads", body)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("repeat v1: code %d body %s", rec.Code, rec.Body.String())
	}
	var again model.Bead
	decodeJSON(t, rec, &again)
	if again.ID != b.ID {
		t.Fatalf("repeat v1 made a second bead %s (first %s)", again.ID, b.ID)
	}
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+b.ID, map[string]any{"fields": map[string]any{"llm_multiplier": 2.0}})
	requireStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "next version") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestTenantLand_MirrorStaysOff(t *testing.T) {
	_, ms, h := newTestServer()
	registerUntrustedInput(t, ms)
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", map[string]any{
		"title": "tomato-patch", "type": "project", "kind": "config",
		"fields": map[string]any{"land_kind": "tenant", "land_slack_mirror": "on"},
	})
	requireStatus(t, rec, http.StatusBadRequest)

	id := createLand(t, h, ms, "tomato-patch", "tenant")
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"land_slack_mirror": "on"}})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"land_slack_mirror": "off", "land_frozen": true, "max_live_agents": 3}})
	requireStatus(t, rec, http.StatusOK)

	// Operator lands mirror as they like.
	op := createLand(t, h, ms, "gardenio-own", "operator")
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+op, map[string]any{"fields": map[string]any{"land_slack_mirror": "on"}})
	requireStatus(t, rec, http.StatusOK)
}

func TestLedgerRecordUpdateError_OtherTypesUntouched(t *testing.T) {
	a, b := json.RawMessage(`{"x":1}`), json.RawMessage(`{"x":2}`)
	if err := ledgerRecordUpdateError(model.TypeTask, a, b); err != nil {
		t.Fatalf("task field change refused: %v", err)
	}
	if err := ledgerRecordUpdateError(typeCreditGrant, json.RawMessage(`{"a":1,"b":2}`), json.RawMessage(`{ "b":2, "a":1 }`)); err != nil {
		t.Fatalf("reordered keys counted as a change: %v", err)
	}
	task := &model.Bead{ID: "kd-t", Type: model.TypeTask, Labels: []string{"project:a"}}
	if err := creditGrantLabelChangeError(task, "project:a", false); err != nil {
		t.Fatalf("task label change refused: %v", err)
	}
}

// --- Merge-desk send-back on PR #627: who may touch a ledger record. ---

// ledgerAuthzServer wires the three non-admin callers an agent pod or a
// signed-in operator can present (the shared static token, a fleet token, an
// operator-role OIDC user) plus an OIDC admin, and seeds a tenant land, one
// grant and one rate version straight into the store.
func ledgerAuthzServer(t *testing.T) (*mockStore, http.Handler) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	fc := NewFleetConfig("default", map[string]string{"fleet-token": "alpha"})
	verifier := &taintTestVerifier{roles: map[string][]string{
		"admin@example.com": {RoleAdmin},
		"op@example.com":    {RoleOperator},
	}}
	h := s.NewHTTPHandler(verifier, withTestToken(fc, "static-token"))

	fields := func(m map[string]any) json.RawMessage {
		b, _ := json.Marshal(m)
		return b
	}
	ms.beads["gd-land1"] = &model.Bead{ID: "gd-land1", Title: "tomato-patch", Type: "project", Kind: model.KindConfig,
		Status: model.StatusOpen, Fields: fields(map[string]any{"prefix": "tp", "land_kind": "tenant"})}
	ms.beads["gd-grant1"] = &model.Bead{ID: "gd-grant1", Title: "Credit grant: tomato-patch", Type: typeCreditGrant,
		Kind: model.KindData, Status: model.StatusClosed, Labels: []string{"project:tomato-patch"},
		CreatedBy: "oidc:admin@example.com", Fields: fields(validCreditGrantFields())}
	ms.beads["gd-rate1"] = &model.Bead{ID: "gd-rate1", Title: "Credit rate v1", Type: typeCreditRate,
		Kind: model.KindConfig, Status: model.StatusOpen, CreatedBy: "oidc:admin@example.com",
		Fields: fields(validCreditRateFields())}
	return ms, h
}

type ledgerWriteCase struct {
	name   string
	method string
	path   string
	body   any
	// fieldOnly: a field-endpoint write, which an admin is refused too (400).
	fieldOnly bool
}

func ledgerWriteCases() []ledgerWriteCase {
	edit := editSubFieldRequest{Op: "set", Key: "x", Value: "1"}
	var cases []ledgerWriteCase
	for _, rec := range []struct {
		id, num string
		create  map[string]any
	}{
		{"gd-grant1", "amount_credits", creditGrantBody(validCreditGrantFields())},
		{"gd-rate1", "llm_multiplier", creditRateBody(validCreditRateFields())},
	} {
		p := "/v1/beads/" + rec.id
		f := func(field, op string) string { return p + "/fields/" + field + "/" + op }
		cases = append(cases,
			ledgerWriteCase{name: rec.id + "/create", method: http.MethodPost, path: "/v1/beads", body: rec.create},
			ledgerWriteCase{name: rec.id + "/PATCH fields", method: http.MethodPatch, path: p, body: map[string]any{"fields": map[string]any{rec.num: 10000.0}}},
			ledgerWriteCase{name: rec.id + "/PATCH labels", method: http.MethodPatch, path: p, body: map[string]any{"labels": []string{"project:gardenio"}}},
			ledgerWriteCase{name: rec.id + "/add label", method: http.MethodPost, path: p + "/labels", body: map[string]any{"label": "project:gardenio"}},
			ledgerWriteCase{name: rec.id + "/increment", method: http.MethodPost, path: p + "/increment", body: map[string]float64{rec.num: 10000}, fieldOnly: true},
			ledgerWriteCase{name: rec.id + "/increment negative", method: http.MethodPost, path: p + "/increment", body: map[string]float64{rec.num: -1.4}, fieldOnly: true},
			ledgerWriteCase{name: rec.id + "/fields edit", method: http.MethodPost, path: f("decision_id", "edit"), body: edit, fieldOnly: true},
			ledgerWriteCase{name: rec.id + "/fields append", method: http.MethodPost, path: f("decision_id", "append"), body: appendArrayFieldRequest{Element: "x"}, fieldOnly: true},
			ledgerWriteCase{name: rec.id + "/fields reorder", method: http.MethodPost, path: f("decision_id", "reorder"), body: casArrayFieldRequest{Expected: nil, Next: []string{"x"}}, fieldOnly: true},
		)
	}
	cases = append(cases, ledgerWriteCase{name: "gd-rate1/close with fields", method: http.MethodPost,
		path: "/v1/beads/gd-rate1/close", body: map[string]any{"llm_multiplier": 0.1}, fieldOnly: true})
	return cases
}

func ledgerSnapshot(ms *mockStore) map[string]string {
	out := map[string]string{}
	for _, b := range ms.beads {
		if isLedgerRecordType(b.Type) {
			out[b.ID] = string(b.Fields) + "|" + strings.Join(b.Labels, ",") + "|" + string(b.Status)
		}
	}
	return out
}

// The static token, a fleet token and an operator-role OIDC user are each
// refused (403) on every route that writes a credit_grant or a credit_rate,
// and the ledger is unchanged.
func TestLandsLedger_NonAdminCallersRefused(t *testing.T) {
	for _, caller := range []string{"static-token", "fleet-token", "op@example.com"} {
		for _, c := range ledgerWriteCases() {
			t.Run(caller+"/"+c.name, func(t *testing.T) {
				ms, h := ledgerAuthzServer(t)
				before := ledgerSnapshot(ms)
				operator := ""
				if !strings.Contains(caller, "@") {
					operator = "slack:U0TESTER1" // a token caller names who it acts for
				}
				rec := fieldWriteRequest(t, h, fieldWriteCase{method: c.method, path: c.path, body: c.body}, caller, operator)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
				}
				after := ledgerSnapshot(ms)
				if len(after) != len(before) {
					t.Fatalf("ledger record count %d -> %d despite 403", len(before), len(after))
				}
				for id, v := range before {
					if after[id] != v {
						t.Errorf("%s changed despite 403:\n before %s\n after  %s", id, v, after[id])
					}
				}
			})
		}
	}
}

// An OIDC admin gets past the role gate on the field endpoints, but a ledger
// record is never edited, so the write is still refused (400).
func TestLandsLedger_FieldEndpointsRefuseAdminToo(t *testing.T) {
	for _, c := range ledgerWriteCases() {
		if !c.fieldOnly {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			ms, h := ledgerAuthzServer(t)
			before := ledgerSnapshot(ms)
			rec := fieldWriteRequest(t, h, fieldWriteCase{method: c.method, path: c.path, body: c.body}, "admin@example.com", "")
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "never edited") {
				t.Fatalf("code = %d, want 400 'never edited'; body %s", rec.Code, rec.Body.String())
			}
			after := ledgerSnapshot(ms)
			for id, v := range before {
				if after[id] != v {
					t.Errorf("%s changed:\n before %s\n after  %s", id, v, after[id])
				}
			}
		})
	}
}

// A stored type override cannot lower the ledger types' admin floor.
func TestLandsLedger_MinRoleFloor(t *testing.T) {
	for _, key := range []string{"type:credit_grant", "type:credit_rate"} {
		if !minRoleFloorTypes[key] {
			t.Errorf("%s is not in minRoleFloorTypes", key)
		}
	}
}
