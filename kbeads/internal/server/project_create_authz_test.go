package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gd-WCM9rfOL8L (laptop-claude's review of PR #737): {create, project} was on
// the static/fleet allow-list, and the Lands provisioner's grant admitted a
// create with no land_kind, so a machine token could open an operator project,
// which the controller mints fleet-App GitHub tokens for. Only an admin
// creates a project bead now, and the Lands provisioner only a tenant land's.

// landsProvisionerCtx is the Lands provisioner's machine principal.
func landsProvisionerCtx() context.Context {
	return machineCtx("gardenio-controller", landsProvisionerRole)
}

// projectCreateFields are the land_kind shapes every refused caller tries.
// Any other land_kind value (empty, "Tenant", a non-string) fails the
// type:project schema with a 400 before authz runs.
var projectCreateFields = map[string]map[string]any{
	"no fields":          nil,
	"no land_kind":       {"git_url": "https://github.com/mattbakerconsulting/gardenio.git"},
	"land_kind operator": {"land_kind": "operator"},
	"land_kind tenant":   {"land_kind": "tenant"},
}

// edgeCtx is a verified v1 edge envelope naming an operator.
func edgeCtx() context.Context {
	return WithSystemOperator(withAuthMethod(
		WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "edge"}),
		authMethodEdge), "slack:U0TESTER1")
}

// HTTP, OIDC and token callers: the static token, a fleet token, the land
// principal, land members and a non-admin operator are refused every shape;
// an admin still creates.
func TestProjectCreate_HTTP_TokenAndOIDCCallers(t *testing.T) {
	f := newScopedFixture(t)
	registerUntrustedInput(t, f.ms)
	before := f.projectBeadCount()
	for who, tok := range map[string]string{
		"static token":       scopedAdminToken,
		"fleet token":        tokFleetBridge,
		"land principal":     tokLandClaimlayer,
		"land member":        tokScopedWriter,
		"land owner":         tokScopedOwner,
		"non-admin operator": tokOperator,
	} {
		for shape, fields := range projectCreateFields {
			t.Run(who+"/"+shape, func(t *testing.T) {
				if code, body := f.createProject(t, tok, "newland", fields); code != http.StatusForbidden {
					t.Fatalf("create project = %d %s, want 403", code, body)
				}
			})
		}
	}
	if got := f.projectBeadCount(); got != before {
		t.Fatalf("project beads %d -> %d despite the refusals", before, got)
	}

	// Control: an admin creates an operator project and a tenant one.
	for title, fields := range map[string]map[string]any{"opland": nil, "tenland": {"land_kind": "tenant"}} {
		if code, body := f.createProject(t, tokOwnerAdmin, title, fields); code != http.StatusCreated && code != http.StatusOK {
			t.Errorf("admin creates project %s: %d %s, want success", title, code, body)
		}
	}
	if got := f.projectBeadCount(); got != before+2 {
		t.Errorf("project beads %d -> %d after the admin's creates, want +2", before, got)
	}
}

// HTTP, machine principals with real service-account tokens: the Lands
// provisioner creates only land_kind=tenant; another machine principal (even
// one whose token carries gardenio-admin) creates nothing.
func TestProjectCreate_HTTP_MachinePrincipals(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller", "gardenio-authz-canary"})
	ms := newMockStore()
	registerUntrustedInput(t, ms)
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	srv.SetRateLimit(RateLimitConfig{})
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	provisioner := h.saToken(t, "gardenio-controller", landsProvisionerRole)
	count := func() int {
		n := 0
		for _, b := range ms.beads {
			if b.Type == "project" {
				n++
			}
		}
		return n
	}
	create := func(tok, title string, fields map[string]any) (int, string) {
		body := map[string]any{"type": "project", "title": title, "labels": []string{"project:gardenio"}}
		if fields != nil {
			body["fields"] = fields
		}
		rec := machineDo(t, handler, tok, http.MethodPost, "/v1/beads", body)
		return rec.Code, rec.Body.String()
	}

	refused := map[string]string{
		"other machine":             h.saToken(t, "gardenio-authz-canary", "gardenio-machine-accounts"),
		"other machine, admin role": h.saToken(t, "gardenio-authz-canary", RoleAdmin, "gardenio-machine-accounts"),
	}
	for who, tok := range refused {
		for shape, fields := range projectCreateFields {
			t.Run(who+"/"+shape, func(t *testing.T) {
				if code, body := create(tok, "newland", fields); code != http.StatusForbidden {
					t.Fatalf("create project = %d %s, want 403", code, body)
				}
			})
		}
	}
	for shape, fields := range projectCreateFields {
		if shape == "land_kind tenant" {
			continue
		}
		t.Run("lands provisioner/"+shape, func(t *testing.T) {
			code, body := create(provisioner, "newland", fields)
			if code != http.StatusForbidden || !strings.Contains(body, "tenant land") {
				t.Fatalf("create project = %d %s, want 403 naming the tenant-land rule", code, body)
			}
		})
	}
	if got := count(); got != 0 {
		t.Fatalf("%d project beads created despite the refusals", got)
	}

	// Control: the provisioner opens a tenant land.
	if code, body := create(provisioner, "tenland", map[string]any{"land_kind": "tenant"}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("lands provisioner creates a tenant land: %d %s, want success", code, body)
	}
	if got := count(); got != 1 {
		t.Fatalf("project beads = %d after the provisioner's create, want 1", got)
	}
}

// gRPC: CreateBead maps every refusal to PermissionDenied; the admin, an
// approved escalation replay and the provisioner (tenant only) still create.
func TestProjectCreate_GRPC(t *testing.T) {
	s, ms, _ := newTestServer()
	registerUntrustedInput(t, ms)
	scopedMember := WithGroups(WithRoles(oidcCtx("clw@example.com", nil), []string{RoleViewer, RoleOperator, RoleProjectMember}), []string{"/project-claimlayer"})
	refused := map[string]context.Context{
		"static token":                  machineUserCtx(),
		"fleet token":                   fleetUserCtx(),
		"v1 edge envelope":              edgeCtx(),
		"land principal":                withAuthMethod(context.Background(), authMethodLand),
		"land member":                   scopedMember,
		"non-admin operator":            WithRoles(oidcCtx("op@example.com", nil), []string{RoleViewer, RoleOperator}),
		"other machine":                 machineCtx("gardenio-authz-canary", "gardenio-machine-accounts"),
		"provisioner role on a v1 edge": WithMachineRoles(edgeCtx(), []string{landsProvisionerRole}),
	}
	count := func() int {
		n := 0
		for _, b := range ms.beads {
			if b.Type == "project" {
				n++
			}
		}
		return n
	}
	for who, ctx := range refused {
		for shape, fields := range projectCreateFields {
			raw, _ := json.Marshal(fields)
			if fields == nil {
				raw = nil
			}
			if _, err := s.CreateBead(ctx, &beadsv1.CreateBeadRequest{Title: "newland", Type: "project", Labels: []string{"project:gardenio"}, Fields: raw}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s / %s: err = %v, want PermissionDenied", who, shape, err)
			}
		}
	}
	for shape, fields := range projectCreateFields {
		if shape == "land_kind tenant" {
			continue
		}
		raw, _ := json.Marshal(fields)
		if _, err := s.CreateBead(landsProvisionerCtx(), &beadsv1.CreateBeadRequest{Title: "newland", Type: "project", Labels: []string{"project:gardenio"}, Fields: raw}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("lands provisioner / %s: err = %v, want PermissionDenied", shape, err)
		}
	}
	if got := count(); got != 0 {
		t.Fatalf("%d project beads created despite the refusals", got)
	}

	// Controls.
	for who, c := range map[string]struct {
		ctx    context.Context
		fields string
	}{
		"oidc admin":        {adminOIDCCtx(), ``},
		"escalation replay": {withMachineWrite(machineUserCtx()), ``},
		"lands provisioner": {landsProvisionerCtx(), `{"land_kind":"tenant"}`},
	} {
		var raw []byte
		if c.fields != "" {
			raw = []byte(c.fields)
		}
		if _, err := s.CreateBead(c.ctx, &beadsv1.CreateBeadRequest{Title: "land-" + strings.ReplaceAll(who, " ", "-"), Type: "project", Labels: []string{"project:gardenio"}, Fields: raw}); err != nil {
			t.Errorf("%s creates a project: %v", who, err)
		}
	}
	if got := count(); got != 3 {
		t.Errorf("project beads = %d after the controls, want 3", got)
	}
}

// The gate itself, independent of the stored type:project write_policy: a
// weakened policy (no min_role) still leaves project creation to the admin and
// the provisioner.
func TestAuthorizeProjectCreate_HoldsWithoutWritePolicy(t *testing.T) {
	s := &BeadsServer{store: newMockStore()}
	tenant := json.RawMessage(`{"land_kind":"tenant"}`)
	for who, ctx := range map[string]context.Context{
		"static token":       machineUserCtx(),
		"fleet token":        fleetUserCtx(),
		"non-admin operator": WithRoles(oidcCtx("op@example.com", nil), []string{RoleViewer, RoleOperator}),
		"v1 edge envelope":   edgeCtx(),
		"other machine":      machineCtx("gardenio-authz-canary", landsProvisionerRole+"-x"),
	} {
		for _, fields := range []json.RawMessage{nil, tenant} {
			if d := s.authorizeProjectCreate(ctx, "project", fields); d == nil || !d.Escalatable || d.RequiredRole != RoleAdmin {
				t.Errorf("%s with fields %s: %+v, want an escalatable admin denial", who, fields, d)
			}
		}
	}
	if d := s.authorizeProjectCreate(landsProvisionerCtx(), "project", nil); d == nil {
		t.Error("lands provisioner with no land_kind allowed")
	}
	for who, ctx := range map[string]context.Context{"oidc admin": adminOIDCCtx(), "auth disabled": withAuthMethod(context.Background(), authMethodNone)} {
		if d := s.authorizeProjectCreate(ctx, "project", nil); d != nil {
			t.Errorf("%s refused: %v", who, d)
		}
	}
	if d := s.authorizeProjectCreate(landsProvisionerCtx(), "project", tenant); d != nil {
		t.Errorf("lands provisioner tenant create refused: %v", d)
	}
	if d := s.authorizeProjectCreate(machineUserCtx(), model.TypeTask, nil); d != nil {
		t.Errorf("non-project create refused: %v", d)
	}
}
