package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gd-QVP0Q6tzYB / PR #669 review: only an OIDC gardenio-admin throws a Lands
// kill switch. {update, project} is on the machine allow-list, so before this
// gate the shared static token (every agent pod holds it) and a fleet token
// could clear land_frozen and thaw a land, or clear land_kind so the wall
// stopped applying; and the static token, which bypasses ownership, could
// close the lands-pause gate. Every denial sits next to a positive control.

func TestChangedLandSwitchFields(t *testing.T) {
	frozen := json.RawMessage(`{"land_kind":"tenant","land_frozen":true,"land_frozen_reason":"abuse","card_theme":"garden"}`)
	for _, tc := range []struct {
		name            string
		patch, existing string
		want            []string
	}{
		{"thaw", `{"land_frozen":false}`, string(frozen), []string{"land_frozen"}},
		{"thaw as string", `{"land_frozen":"false"}`, string(frozen), []string{"land_frozen"}},
		{"clear kind", `{"land_kind":""}`, string(frozen), []string{"land_kind"}},
		{"null kind", `{"land_kind":null}`, string(frozen), []string{"land_kind"}},
		{"operator kind", `{"land_kind":"operator"}`, string(frozen), []string{"land_kind"}},
		{"clear reason", `{"land_frozen_reason":""}`, string(frozen), []string{"land_frozen_reason"}},
		{"freeze a thawed land", `{"land_frozen":true,"land_frozen_by":"x"}`, `{"land_kind":"tenant"}`, []string{"land_frozen", "land_frozen_by"}},
		{"echo unchanged", `{"land_kind":"tenant","land_frozen":true,"card_theme":"business"}`, string(frozen), nil},
		{"string echo of a bool", `{"land_frozen":"true"}`, string(frozen), nil},
		{"unrelated field", `{"card_theme":"business"}`, string(frozen), nil},
		{"create with kind", `{"land_kind":"tenant"}`, ``, []string{"land_kind"}},
		{"create without land fields", `{"prefix":"cl"}`, ``, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := changedLandSwitchFields(json.RawMessage(tc.patch), json.RawMessage(tc.existing))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("changed = %v, want %v", got, tc.want)
			}
		})
	}
}

// The decision point for every caller class.
func TestDenyKillSwitchWrite_Callers(t *testing.T) {
	s, _, _ := newTestServer()
	scopedMember := WithGroups(WithRoles(oidcCtx("clw@example.com", nil), []string{RoleViewer, RoleOperator, RoleProjectMember}), []string{"/project-claimlayer"})
	gate := &model.Bead{ID: "gd-gate", Title: landsPauseGateTitle, Type: "gate", Status: model.StatusOpen}
	project := &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Status: model.StatusOpen}
	frozenTenant := &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"land_kind":"tenant","land_frozen":true}`)}
	closed, renamed := string(model.StatusClosed), "claimlayer-x"
	provisionerCtx := WithMachineRoles(withAuthMethod(context.Background(), authMethodMachine), []string{landsProvisionerRole})
	denied := map[string]context.Context{
		"land principal":     landPrincipalContext(context.Background(), nil, "claimlayer"),
		"land member":        scopedMember,
		"non-admin operator": WithRoles(oidcCtx("op@example.com", nil), []string{RoleViewer, RoleOperator}),
		"static token":       machineUserCtx(),
		"fleet token":        fleetUserCtx(),
		"machine client":     withAuthMethod(context.Background(), authMethodMachine),
		"lands provisioner":  provisionerCtx,
		"v1 edge":            withAuthMethod(context.Background(), authMethodEdge),
	}
	for name, ctx := range denied {
		for what, d := range map[string]*AuthzDenial{
			"thaw":             s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"land_frozen":false}`), json.RawMessage(`{"land_frozen":true}`)),
			"gatehouse tools":  s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"land_gatehouse_tools":true}`), json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true}`)),
			"tools endpoint":   s.authorizeLandSwitchKeyWrite(ctx, "update", project, "land_gatehouse_tools"),
			"field endpoint":   s.authorizeLandSwitchKeyWrite(ctx, "update", project, "land_kind"),
			"close gate":       s.authorizeKillSwitchLifecycle(ctx, "close", gate, nil, nil),
			"delete gate":      s.authorizeKillSwitchLifecycle(ctx, "delete", gate, nil, nil),
			"delete project":   s.authorizeKillSwitchLifecycle(ctx, "delete", frozenTenant, nil, nil),
			"rename gate":      s.authorizeKillSwitchLifecycle(ctx, "update", gate, nil, &renamed),
			"close via status": s.authorizeKillSwitchLifecycle(ctx, "update", project, &closed, nil),
			"rename project":   s.authorizeKillSwitchLifecycle(ctx, "update", project, nil, &renamed),
		} {
			if d == nil {
				t.Errorf("%s: %s allowed, want denied", name, what)
			} else if d.RequiredRole != RoleAdmin {
				t.Errorf("%s: %s RequiredRole = %q, want %q", name, what, d.RequiredRole, RoleAdmin)
			}
		}
		// Controls: the same caller, nothing gated.
		open, same := string(model.StatusOpen), "claimlayer"
		otherGate := &model.Bead{ID: "gd-g2", Title: "gardeners-pause", Type: "gate", Status: model.StatusOpen}
		for what, d := range map[string]*AuthzDenial{
			"unrelated field":    s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"card_theme":"garden"}`), nil),
			"echoed freeze":      s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"land_frozen":true}`), json.RawMessage(`{"land_frozen":true}`)),
			"land field on task": s.authorizeLandSwitchFields(ctx, "update", "task", json.RawMessage(`{"land_frozen":false}`), nil),
			"unchanged status":   s.authorizeKillSwitchLifecycle(ctx, "update", project, &open, &same),
			"close another gate": s.authorizeKillSwitchLifecycle(ctx, "close", otherGate, nil, nil),
		} {
			if d != nil {
				t.Errorf("%s: %s denied: %v", name, what, d)
			}
		}
	}
	allowed := map[string]context.Context{
		"oidc admin":        adminOIDCCtx(),
		"escalation replay": withMachineWrite(machineUserCtx()),
		"auth disabled":     withAuthMethod(context.Background(), authMethodNone),
	}
	for name, ctx := range allowed {
		if d := s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"land_frozen":false,"land_kind":""}`), json.RawMessage(`{"land_frozen":true,"land_kind":"tenant"}`)); d != nil {
			t.Errorf("%s: thaw denied: %v", name, d)
		}
		if d := s.authorizeKillSwitchLifecycle(ctx, "close", gate, nil, nil); d != nil {
			t.Errorf("%s: closing the lands-pause gate denied: %v", name, d)
		}
	}
}

// The Lands provisioner opens, marks and closes a land (gd-t92sawEzFJ,
// gd-Ymdaxee0sX): exactly these writes, and none of them for a machine client
// without the role.
func TestProvisionerLandSwitchWrites(t *testing.T) {
	s, _, _ := newTestServer()
	project := &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Status: model.StatusOpen}
	frozenTenant := &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"land_kind":"tenant","land_frozen":true}`)}
	provisionerCtx := WithMachineRoles(withAuthMethod(context.Background(), authMethodMachine), []string{landsProvisionerRole})
	tenant := json.RawMessage(`{"land_kind":"tenant"}`)
	provisionerWrites := map[string]func(context.Context) *AuthzDenial{
		"create a tenant land": func(ctx context.Context) *AuthzDenial {
			return s.authorizeLandSwitchFields(ctx, "create", "project", json.RawMessage(`{"land_kind":"tenant","account":"lands-claimlayer"}`), nil)
		},
		"freeze a tenant land": func(ctx context.Context) *AuthzDenial {
			return s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"land_frozen":true}`), tenant)
		},
		"freeze, string value": func(ctx context.Context) *AuthzDenial {
			return s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"land_frozen":"true","land_kind":"tenant"}`), tenant)
		},
		"close a frozen tenant land": func(ctx context.Context) *AuthzDenial {
			return s.authorizeKillSwitchLifecycle(ctx, "close", frozenTenant, nil, nil)
		},
		"create the gatehouse": func(ctx context.Context) *AuthzDenial {
			return s.authorizeLandSwitchFields(ctx, "create", "project", json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true}`), nil)
		},
		"mark the gatehouse": func(ctx context.Context) *AuthzDenial {
			return s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"land_gatehouse":true}`), tenant)
		},
		"stamp the backfill": func(ctx context.Context) *AuthzDenial {
			return s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"land_gatehouse_backfilled_at":"2026-10-31T18:00:00Z"}`), json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true}`))
		},
	}
	for what, write := range provisionerWrites {
		if d := write(provisionerCtx); d != nil {
			t.Errorf("lands provisioner: %s denied: %v", what, d)
		}
		if d := write(withAuthMethod(context.Background(), authMethodMachine)); d == nil {
			t.Errorf("machine client: %s allowed, want denied", what)
		}
		if d := write(WithMachineRoles(withAuthMethod(context.Background(), authMethodEdge), []string{landsProvisionerRole})); d == nil {
			t.Errorf("provisioner role on a v1 edge: %s allowed, want denied", what)
		}
	}
	unfrozenTenant := &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Status: model.StatusOpen, Fields: tenant}
	for what, d := range map[string]*AuthzDenial{
		"create an operator land":     s.authorizeLandSwitchFields(provisionerCtx, "create", "project", json.RawMessage(`{"land_kind":"operator"}`), nil),
		"create a frozen land":        s.authorizeLandSwitchFields(provisionerCtx, "create", "project", json.RawMessage(`{"land_kind":"tenant","land_frozen":true}`), nil),
		"set land_kind on update":     s.authorizeLandSwitchFields(provisionerCtx, "update", "project", tenant, nil),
		"change land_kind":            s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_kind":"operator"}`), tenant),
		"clear land_kind":             s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_kind":""}`), tenant),
		"freeze an operator project":  s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_frozen":true}`), nil),
		"freeze and retag":            s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_frozen":true,"land_kind":"tenant"}`), json.RawMessage(`{"land_kind":"operator"}`)),
		"freeze with a reason":        s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_frozen":true,"land_frozen_reason":"x"}`), tenant),
		"thaw by clearing":            s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_frozen":null}`), json.RawMessage(`{"land_kind":"tenant","land_frozen":true}`)),
		"close an unfrozen land":      s.authorizeKillSwitchLifecycle(provisionerCtx, "close", unfrozenTenant, nil, nil),
		"close an operator project":   s.authorizeKillSwitchLifecycle(provisionerCtx, "close", project, nil, nil),
		"turn the gatehouse tools on": s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_gatehouse_tools":true}`), json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true}`)),
		"create with the tools on":    s.authorizeLandSwitchFields(provisionerCtx, "create", "project", json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true,"land_gatehouse_tools":true}`), nil),
		"unmark the gatehouse":        s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_gatehouse":false}`), json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true}`)),
		"mark an operator project":    s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_gatehouse":true}`), nil),
		"rewind the backfill":         s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_gatehouse_backfilled_at":"2026-11-01T00:00:00Z"}`), json.RawMessage(`{"land_kind":"tenant","land_gatehouse_backfilled_at":"2026-10-31T18:00:00Z"}`)),
		"clear the backfill":          s.authorizeLandSwitchFields(provisionerCtx, "update", "project", json.RawMessage(`{"land_gatehouse_backfilled_at":null}`), json.RawMessage(`{"land_kind":"tenant","land_gatehouse_backfilled_at":"2026-10-31T18:00:00Z"}`)),
	} {
		if d == nil {
			t.Errorf("lands provisioner: %s allowed, want denied", what)
		}
	}
}

// seedLandsPauseGate seeds an open lands-pause gate an operator opened, and
// an ordinary gardeners-pause gate as a control.
func (f *scopedFixture) seedLandsPauseGate() {
	for id, title := range map[string]string{"gate-lands": landsPauseGateTitle, "gate-gardeners": "gardeners-pause"} {
		f.seedTyped(id, "gate", "")
		f.ms.beads[id].Title = title
		f.ms.beads[id].Kind = "data"
		f.ms.beads[id].CreatedBy = "owner@example.com"
	}
}

// HTTP: the full stack, with the real static token, a fleet token, land
// members, the land principal and a non-admin operator.
func TestLandKillSwitches_HTTP(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "true")
	f.seedLandsPauseGate()
	const project = "/v1/beads/proj-claimlayer"
	snapshot := func() string {
		p, g := f.ms.beads["proj-claimlayer"], f.ms.beads["gate-lands"]
		if g == nil {
			return "gate deleted"
		}
		return string(p.Fields) + "|" + string(p.Status) + "|" + p.Title + "|" + string(g.Status) + "|" + g.Title
	}

	attempts := []struct {
		name         string
		method, path string
		body         any
	}{
		{"thaw", http.MethodPatch, project, map[string]any{"fields": map[string]any{"land_frozen": false}}},
		{"thaw with a string", http.MethodPatch, project, map[string]any{"fields": map[string]any{"land_frozen": "false"}}},
		{"clear land_kind", http.MethodPatch, project, map[string]any{"fields": map[string]any{"land_kind": ""}}},
		{"make it an operator land", http.MethodPatch, project, map[string]any{"fields": map[string]any{"land_kind": "operator"}}},
		{"rewrite the reason", http.MethodPatch, project, map[string]any{"fields": map[string]any{"land_frozen_reason": "a test"}}},
		{"turn the gatehouse tools on", http.MethodPatch, project, map[string]any{"fields": map[string]any{"land_gatehouse_tools": true}}},
		{"mark the gatehouse", http.MethodPatch, project, map[string]any{"fields": map[string]any{"land_gatehouse": true}}},
		{"close the project via status", http.MethodPatch, project, map[string]any{"status": "closed"}},
		{"defer the project", http.MethodPatch, project, map[string]any{"status": "deferred"}},
		{"rename the project", http.MethodPatch, project, map[string]any{"title": "claimlayer-old"}},
		{"close the project", http.MethodPost, project + "/close", map[string]any{}},
		{"project field endpoint", http.MethodPost, project + "/fields/land_frozen/append", appendArrayFieldRequest{Element: "false"}},
		{"thaw on a close body", http.MethodPost, project + "/close", map[string]any{"land_frozen": false}},
		{"close the gate", http.MethodPost, "/v1/beads/gate-lands/close", map[string]any{}},
		{"close the gate via status", http.MethodPatch, "/v1/beads/gate-lands", map[string]any{"status": "closed"}},
		{"rename the gate", http.MethodPatch, "/v1/beads/gate-lands", map[string]any{"title": "lands-pause-off"}},
		{"delete the gate", http.MethodDelete, "/v1/beads/gate-lands", nil},
	}
	for who, tok := range map[string]string{
		"static token":       scopedAdminToken,
		"fleet token":        tokFleetBridge,
		"land member":        tokScopedWriter,
		"land owner":         tokScopedOwner,
		"land principal":     tokLandClaimlayer,
		"non-admin operator": tokOperator,
	} {
		for _, a := range attempts {
			t.Run(who+"/"+a.name, func(t *testing.T) {
				before := snapshot()
				code, body := f.do(t, tok, a.method, a.path, a.body)
				// A land token cannot read the bead at all, so it is refused
				// as for a missing one (gd-7APTAIhdC5).
				if code != http.StatusForbidden && code != http.StatusNotFound {
					t.Fatalf("%s %s = %d %s, want 403 or 404", a.method, a.path, code, body)
				}
				if after := snapshot(); after != before {
					t.Errorf("kill switch changed despite 403: %s -> %s", before, after)
				}
			})
		}
	}

	t.Run("controls", func(t *testing.T) {
		// The static token's 403s above are this gate: it still writes the
		// project's other fields, echoes the freeze back, and closes a gate
		// that is not lands-pause.
		for _, tok := range []string{scopedAdminToken, tokFleetBridge} {
			if code, body := f.do(t, tok, http.MethodPatch, project, map[string]any{"fields": map[string]any{"card_theme": "garden", "land_frozen": true, "land_kind": "tenant"}}); code != http.StatusOK {
				t.Fatalf("%s: unrelated project field with the freeze echoed = %d %s, want 200", tok, code, body)
			}
		}
		if code, body := f.do(t, scopedAdminToken, http.MethodPost, "/v1/beads/gate-gardeners/close", map[string]any{}); code != http.StatusOK {
			t.Fatalf("static token closes gardeners-pause = %d %s, want 200", code, body)
		}
		if fr, err := landFrozen(context.Background(), f.ms, "claimlayer"); err != nil || !fr {
			t.Fatalf("claimlayer frozen = %v, %v after the controls, want still frozen", fr, err)
		}
	})

	t.Run("admin thaws and lifts the pause", func(t *testing.T) {
		if code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, project, map[string]any{"fields": map[string]any{"land_frozen": false, "land_frozen_reason": ""}}); code != http.StatusOK {
			t.Fatalf("admin thaw = %d %s, want 200", code, body)
		}
		if fr, _ := landFrozen(context.Background(), f.ms, "claimlayer"); fr {
			t.Error("claimlayer still frozen after the admin thawed it")
		}
		if code, body := f.do(t, tokOwnerAdmin, http.MethodPost, "/v1/beads/gate-lands/close", map[string]any{}); code != http.StatusOK {
			t.Fatalf("admin closes lands-pause = %d %s, want 200", code, body)
		}
	})
}

// gRPC: UpdateBead, CloseBead and DeleteBead map the denial to
// PermissionDenied for the static token, a fleet token and a land member.
func TestLandKillSwitches_GRPC(t *testing.T) {
	s, ms, _ := newTestServer()
	const frozenJSON = `{"land_kind":"tenant","land_frozen":true,"land_frozen_reason":"abuse"}`
	seed := func() {
		ms.beads["gd-proj"] = &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Kind: model.KindConfig, Status: model.StatusOpen, Fields: json.RawMessage(frozenJSON)}
		ms.beads["gd-gate"] = &model.Bead{ID: "gd-gate", Title: landsPauseGateTitle, Type: "gate", Kind: model.KindData, Status: model.StatusOpen, CreatedBy: "owner@example.com"}
	}
	seed()
	str := func(v string) *string { return &v }
	updates := []*beadsv1.UpdateBeadRequest{
		{Id: "gd-proj", Fields: []byte(`{"land_frozen":false}`)},
		{Id: "gd-proj", Fields: []byte(`{"land_kind":""}`)},
		{Id: "gd-proj", Fields: []byte(`{"land_kind":"operator"}`)},
		{Id: "gd-proj", Status: str("closed")},
		{Id: "gd-proj", Title: str("claimlayer-old")},
		{Id: "gd-gate", Status: str("closed")},
		{Id: "gd-gate", Title: str("lands-pause-off")},
	}
	scopedMember := WithGroups(WithRoles(oidcCtx("clw@example.com", nil), []string{RoleViewer, RoleOperator, RoleProjectMember}), []string{"/project-claimlayer"})
	for who, ctx := range map[string]context.Context{
		"static token": machineUserCtx(),
		"fleet token":  fleetUserCtx(),
		"land member":  scopedMember,
	} {
		t.Run(who, func(t *testing.T) {
			for _, u := range updates {
				if _, err := s.UpdateBead(ctx, u); !grpcRefused(err) {
					t.Errorf("UpdateBead %s fields=%s status=%v title=%v: err = %v, want PermissionDenied or NotFound", u.Id, u.Fields, u.Status, u.Title, err)
				}
			}
			if _, err := s.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-gate"}); !grpcRefused(err) {
				t.Errorf("CloseBead lands-pause: err = %v, want PermissionDenied or NotFound", err)
			}
			if _, err := s.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "gd-gate"}); !grpcRefused(err) {
				t.Errorf("DeleteBead lands-pause: err = %v, want PermissionDenied or NotFound", err)
			}
			p, g := ms.beads["gd-proj"], ms.beads["gd-gate"]
			if g == nil || string(p.Fields) != frozenJSON || p.Status != model.StatusOpen || p.Title != "claimlayer" || g.Status != model.StatusOpen || g.Title != landsPauseGateTitle {
				t.Errorf("kill switch changed despite denial: project %+v gate %+v", p, g)
				seed()
			}
		})
	}

	t.Run("controls", func(t *testing.T) {
		// The static token still writes the project's other fields.
		if _, err := s.UpdateBead(machineUserCtx(), &beadsv1.UpdateBeadRequest{Id: "gd-proj", Fields: []byte(`{"card_theme":"garden"}`)}); err != nil {
			t.Errorf("static token unrelated project field: %v", err)
		}
		admin := adminOIDCCtx()
		if _, err := s.UpdateBead(admin, &beadsv1.UpdateBeadRequest{Id: "gd-proj", Fields: []byte(`{"land_frozen":false}`)}); err != nil {
			t.Errorf("admin thaw: %v", err)
		}
		if _, err := s.CloseBead(admin, &beadsv1.CloseBeadRequest{Id: "gd-gate"}); err != nil {
			t.Errorf("admin closes lands-pause: %v", err)
		}
	})
}

// createProject POSTs a type:project create with fields (nil for none). The
// static token has to name an operator (X-Operated-By) to create anything; the
// others ignore it.
func (f *scopedFixture) createProject(t *testing.T, token, title string, fields map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"type": "project", "title": title, "labels": []string{"project:gardenio"}}
	if fields != nil {
		body["fields"] = fields
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+"/v1/beads", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operated-By", "oidc:op@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// projectBeadCount counts the type:project beads in the mock store.
func (f *scopedFixture) projectBeadCount() int {
	n := 0
	for _, b := range f.ms.beads {
		if b.Type == "project" {
			n++
		}
	}
	return n
}

// A second project bead with a land's name and no land_kind would shadow the
// land's kill switch, so only an admin creates a project under a taken name
// (laptop-claude's 15:40Z HIGH on PR #669). HTTP: the static token and a
// fleet token are refused under every spelling the controller reads as the
// same land. Since gd-WCM9rfOL8L they create no project at all
// (authorizeProjectCreate); the Lands provisioner, which may, is refused a
// duplicate in TestDuplicateProjectName_GRPC.
func TestDuplicateProjectName_HTTP(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "true")
	before := f.projectBeadCount()
	for who, tok := range map[string]string{"static token": scopedAdminToken, "fleet token": tokFleetBridge} {
		for _, title := range []string{"claimlayer", "Project: claimlayer", " claimlayer ", "Project:  claimlayer"} {
			code, body := f.createProject(t, tok, title, map[string]any{"land_kind": "tenant"})
			if code != http.StatusForbidden {
				t.Errorf("%s creates project %q: %d %s, want 403", who, title, code, body)
			}
		}
	}
	if got := f.projectBeadCount(); got != before {
		t.Fatalf("project beads %d -> %d despite the refusals", before, got)
	}
	if fr, err := landFrozen(context.Background(), f.ms, "claimlayer"); err != nil || !fr {
		t.Fatalf("claimlayer frozen = %v, %v, want still frozen", fr, err)
	}

	// Control: an admin may create the duplicate (an operator decision).
	if code, body := f.createProject(t, tokOwnerAdmin, "claimlayer", nil); code != http.StatusCreated && code != http.StatusOK {
		t.Errorf("admin creates a duplicate claimlayer: %d %s, want success", code, body)
	}
}

// gRPC: CreateBead maps the refusal to PermissionDenied, the Lands provisioner
// (the one non-admin that creates a project) is refused a duplicate, and a
// failed name lookup refuses too.
func TestDuplicateProjectName_GRPC(t *testing.T) {
	s, ms, _ := newTestServer()
	registerUntrustedInput(t, ms)
	ms.beads["gd-proj"] = &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Kind: model.KindConfig, Status: model.StatusOpen, Fields: json.RawMessage(`{"land_kind":"tenant","land_frozen":true}`)}
	tenant := []byte(`{"land_kind":"tenant"}`)
	for who, ctx := range map[string]context.Context{"static token": machineUserCtx(), "fleet token": fleetUserCtx(), "lands provisioner": landsProvisionerCtx()} {
		for _, title := range []string{"claimlayer", "Project: claimlayer", "claimlayer\t"} {
			if _, err := s.CreateBead(ctx, &beadsv1.CreateBeadRequest{Title: title, Type: "project", Fields: tenant}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s CreateBead project %q: err = %v, want PermissionDenied", who, title, err)
			}
		}
	}
	for id, b := range ms.beads {
		if b.Type == "project" && id != "gd-proj" {
			t.Errorf("duplicate project bead %s created despite the refusal", id)
		}
	}
	if _, err := s.CreateBead(landsProvisionerCtx(), &beadsv1.CreateBeadRequest{Title: "newland", Type: "project", Fields: tenant}); err != nil {
		t.Errorf("lands provisioner CreateBead project newland: %v", err)
	}
	if d := s.authorizeProjectNameUnique(machineUserCtx(), "project", "claimlayer"); d == nil {
		t.Error("authorizeProjectNameUnique allowed a duplicate")
	}
	fs := &BeadsServer{store: failingProjectStore{ms}}
	if d := fs.authorizeProjectNameUnique(machineUserCtx(), "project", "anything"); d == nil || !strings.Contains(d.Message, "cannot be checked") {
		t.Errorf("a failed name lookup = %v, want a refusal", d)
	}
	if d := fs.authorizeProjectNameUnique(adminOIDCCtx(), "project", "anything"); d != nil {
		t.Errorf("admin refused on a failed lookup it never needs: %v", d)
	}
}

// Duplicates that already exist fail closed: the daemon's project lookup
// prefers the bead with a land_kind (then a frozen one) over a lower id, so a
// land member still cannot write in a frozen land through a shadow bead.
func TestFindProjectByName_DuplicateFailsClosed(t *testing.T) {
	f := newScopedFixture(t)
	f.seedChannel(t, "ch-general", "claimlayer", map[string]any{"name": "general", "channel_kind": "general"})
	f.seedLandProject(t, "true")
	f.seedTyped("proj-aaa", "project", "")
	f.ms.beads["proj-aaa"].Title = "claimlayer"
	f.ms.beads["proj-aaa"].Kind = "config"
	if p, err := f.ms.FindProjectByName(context.Background(), "claimlayer"); err != nil || p.ID != "proj-claimlayer" {
		t.Fatalf("FindProjectByName(claimlayer) = %v, %v, want the land bead proj-claimlayer", p, err)
	}
	if fr, err := landFrozen(context.Background(), f.ms, "claimlayer"); err != nil || !fr {
		t.Fatalf("claimlayer frozen = %v, %v with a shadow bead, want frozen", fr, err)
	}
	if code, body := f.say(t, tokScopedWriter, "ch-general"); code != http.StatusForbidden || !strings.Contains(body, "land claimlayer is frozen") {
		t.Errorf("member says in a frozen land with a shadow bead: %d %s, want 403", code, body)
	}

	// Two land beads: the frozen one wins over a lower id.
	f.ms.beads["proj-aaa"].Fields = []byte(`{"land_kind":"tenant","land_frozen":false}`)
	if fr, _ := landFrozen(context.Background(), f.ms, "claimlayer"); !fr {
		t.Error("an unfrozen land_kind duplicate with a lower id must not thaw the land")
	}
}

// Escalation replays run as machine writes, which skip the kill-switch gates,
// and type:gate has no min_role, so approving one is held to the gates as the
// approver (the 15:40Z "kill-switch writes in escalation-replay contexts"):
// a non-admin operator cannot lift the lands-pause gate, thaw a land or seed
// a duplicate project by approving an escalation that does it.
func TestApprove_KillSwitchReplayNeedsAdmin(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "true")
	f.seedLandsPauseGate()
	esc := map[string]struct {
		op     string
		spec   map[string]any
		target string
	}{
		"esc-close-gate":  {"close", map[string]any{}, "gate-lands"},
		"esc-status-gate": {"update", map[string]any{"status": "closed"}, "gate-lands"},
		"esc-rename-gate": {"update", map[string]any{"title": "lands-pause-off"}, "gate-lands"},
		"esc-thaw":        {"update", map[string]any{"fields": map[string]any{"land_frozen": false}}, "proj-claimlayer"},
		"esc-dup":         {"create", map[string]any{"type": "project", "title": "claimlayer", "labels": []string{"project:gardenio"}}, ""},
		"esc-kind":        {"create", map[string]any{"type": "project", "title": "elsewhere", "fields": map[string]any{"land_kind": "operator"}}, ""},
		"esc-new":         {"create", map[string]any{"type": "project", "title": "freshland", "labels": []string{"project:gardenio"}}, ""},
	}
	for id, e := range esc {
		extra := map[string]any{}
		if e.target != "" {
			extra["target_bead_id"] = e.target
		}
		f.seedEscalation(id, "project:gardenio", e.op, e.spec, extra)
	}
	before := f.projectBeadCount()
	for id := range esc {
		c, b := f.approve(t, tokOperator, id)
		f.wantStatus(t, "non-admin operator approves "+id, c, b, http.StatusForbidden)
		if got := f.escalationStatus(id); got != model.StatusOpen {
			t.Errorf("%s status %s after a refused approval, want open", id, got)
		}
	}
	g, p := f.ms.beads["gate-lands"], f.ms.beads["proj-claimlayer"]
	if g.Status != model.StatusOpen || g.Title != landsPauseGateTitle || f.projectBeadCount() != before {
		t.Fatalf("kill switch thrown despite the refusals: gate %s %q, projects %d -> %d", g.Status, g.Title, before, f.projectBeadCount())
	}
	if fr, _ := landFrozen(context.Background(), f.ms, "claimlayer"); !fr || p.Status != model.StatusOpen {
		t.Fatal("claimlayer thawed or closed despite the refusals")
	}

	// Control: the same operator approves closing a gate that is not
	// lands-pause; an admin approves lifting the pause.
	f.seedEscalation("esc-close-other", "project:gardenio", "close", map[string]any{}, map[string]any{"target_bead_id": "gate-gardeners"})
	c, b := f.approve(t, tokOperator, "esc-close-other")
	f.wantStatus(t, "operator approves closing gardeners-pause", c, b, http.StatusOK)
	c, b = f.approve(t, tokOwnerAdmin, "esc-close-gate")
	f.wantStatus(t, "admin approves closing lands-pause", c, b, http.StatusOK)
	if f.ms.beads["gate-lands"].Status != model.StatusClosed {
		t.Errorf("lands-pause status %s after the admin's approval, want closed", f.ms.beads["gate-lands"].Status)
	}
}
