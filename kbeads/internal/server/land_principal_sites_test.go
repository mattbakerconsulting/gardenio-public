package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// landSiteContexts are the two ways a land principal can reach a gate: the
// context AuthMiddleware builds, and the same method and roles with no land
// on it (the scoped-principal engine has nothing to scope it to). Every site
// below must refuse or scope both.
func landSiteContexts() map[string]context.Context {
	fc := NewFleetConfig("gardenio", nil)
	bare := withAuthMethod(context.Background(), authMethodLand)
	bare = WithRoles(bare, landPrincipalRoles)
	bare = WithAuthIdentity(bare, AuthIdentity{Type: "land", Subject: landSubjectPrefix + "claimlayer"})
	return map[string]context.Context{
		"land":         landPrincipalContext(context.Background(), fc, "claimlayer"),
		"land no land": bare,
	}
}

// Every kbeads site that reads authMethodOIDC (laptop-claude's list on
// gd-CuUiTpMGUb, re-grepped on main) refuses or scopes a land principal: the
// "is this a human?" gates refuse it, and the "not OIDC means a trusted
// service token" gates do not take it for one (roleBearing).
func TestLandPrincipal_AuthMethodOIDCSites(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	labels := []string{"project:claimlayer"}
	foreign := func(typ model.BeadType) *model.Bead {
		return &model.Bead{ID: "x-1", Type: typ, Labels: labels, CreatedBy: "someone@example.com"}
	}

	for name, ctx := range landSiteContexts() {
		t.Run(name, func(t *testing.T) {
			if !isScopedPrincipal(ctx) {
				t.Error("isScopedPrincipal = false: a land method is always scoped")
			}
			if d := scopeFromContext(ctx); d.Unrestricted || len(d.Projects) > 1 || (len(d.Projects) == 1 && d.Projects[0] != "claimlayer") {
				t.Errorf("scopeFromContext = %+v, want claimlayer or nothing", d)
			}

			// http_devices.go: pushDeviceScope, pushDeviceCallerEmail.
			if owner, all := pushDeviceScope(ctx); owner != "" || all {
				t.Errorf("pushDeviceScope = (%q, %v), want refused", owner, all)
			}
			if email := pushDeviceCallerEmail(ctx); email != "" {
				t.Errorf("pushDeviceCallerEmail = %q, want none", email)
			}

			// http_hooks.go: authorizeHookExecute, hookExecuteAuthzReason.
			if s.authorizeHookExecute(ctx, "session-start") {
				t.Error("authorizeHookExecute admitted a land principal")
			}
			if r := s.hookExecuteAuthzReason(ctx, "session-start"); r != "" {
				t.Errorf("hookExecuteAuthzReason = %q, want none", r)
			}

			// http_taint_grants.go: callerMayManageTaintPolicy.
			if ok, err := s.callerMayManageTaintPolicy(ctx, &model.Taint{Name: "pii"}); ok || err != nil {
				t.Errorf("callerMayManageTaintPolicy = (%v, %v), want refused", ok, err)
			}

			// http_undo.go: guardUndoAdmin blocks (true) and audits.
			req := httptest.NewRequest(http.MethodPost, "/v1/beads/x-1/undo", nil).WithContext(ctx)
			if !s.guardUndoAdmin(req) {
				t.Error("guardUndoAdmin let a land principal undo")
			}

			// read_authz.go: oidcAllowedProjects scopes, never nil.
			if got := oidcAllowedProjects(ctx, ms); got == nil || len(got) > 1 || (len(got) == 1 && got[0] != "claimlayer") {
				t.Errorf("oidcAllowedProjects = %#v, want claimlayer or the empty set", got)
			}

			// write_authz.go authorizeWriteTitled: token scopes, the machine
			// allow-list and project_scoped are not a land principal's way in.
			// actionTable alone admits it to decision create (gd-mh9ANzCHIE).
			for k := range machineAllowedWrites {
				if tierAllows(tierLand, k.BeadType, k.Action) {
					continue
				}
				if d := s.authorizeWrite(ctx, k.Action, k.BeadType, labels); d == nil {
					t.Errorf("authorizeWrite(%s %s) admitted a land principal via the machine allow-list", k.Action, k.BeadType)
				}
			}
			if d := s.authorizeWrite(ctx, "create", model.TypeDecision, labels); (d == nil) != (name == "land") {
				t.Errorf("authorizeWrite(create decision) = %v, want admitted only in its land", d)
			}
			for _, typ := range []model.BeadType{"agent", "gate"} {
				if d := s.authorizeWrite(ctx, "create", typ, labels); d == nil {
					t.Errorf("authorizeWrite(create %s) admitted a land principal on a project_scoped type", typ)
				}
			}

			// write_authz.go authorizeOwnership: no shared-queue carve-out.
			for _, typ := range []model.BeadType{model.TypeEscalation, model.TypeDecision} {
				if d := s.authorizeOwnership(ctx, "update", foreign(typ)); d == nil {
					t.Errorf("authorizeOwnership(update %s it did not file) admitted a land principal", typ)
				}
			}

			// agent_identity_authz.go, user_identity_authz.go,
			// escalation_approval_authz.go, http_admin_operator_aliases_link.go.
			if d := s.denyAgentIdentityFields(ctx, "update", "agent", []string{"service_account"}); d == nil {
				t.Error("denyAgentIdentityFields admitted a land principal")
			}
			if d := s.denyUserIdentityFields(ctx, "update", []string{"slack_id"}); d == nil {
				t.Error("denyUserIdentityFields admitted a land principal")
			}
			if callerMayApproveAsAdmin(ctx) {
				t.Error("callerMayApproveAsAdmin admitted a land principal")
			}
			if isAdminCaller(ctx) {
				t.Error("isAdminCaller admitted a land principal")
			}

			// spawn_authz.go: no spawn, no agent project or role rewrite.
			if err := authorizeAgentSpawn(ctx, ms, json.RawMessage(`{"project":"claimlayer","role":"crew"}`)); err == nil {
				t.Error("authorizeAgentSpawn admitted a land principal")
			}
			agent := &model.Bead{ID: "a-1", Type: "agent", Labels: labels, Fields: json.RawMessage(`{"project":"claimlayer"}`)}
			if err := authorizeAgentProjectUpdate(ctx, agent, json.RawMessage(`{"project":"gardenio"}`)); err == nil {
				t.Error("authorizeAgentProjectUpdate admitted a land principal")
			}
			if d := s.denyAgentRoleKeyWrite(ctx, "update", []string{agentRoleField}); d == nil {
				t.Error("denyAgentRoleKeyWrite admitted a land principal")
			}

			// interceptors.go: role gates enforce its roles, never bypass.
			for wrap, mw := range map[string]func(string, http.HandlerFunc) http.HandlerFunc{
				"requireRole": requireRole, "requireRoleMachineAllowed": requireRoleMachineAllowed,
			} {
				rec := httptest.NewRecorder()
				mw(RoleAdmin, func(http.ResponseWriter, *http.Request) {})(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s(admin) = %d, want 403", wrap, rec.Code)
				}
			}
			_, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/Unclassified"}, func(context.Context, any) (any, error) { return nil, nil })
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("RBACInterceptor(admin RPC) err = %v, want PermissionDenied", err)
			}
		})
	}
}

// oidcCredentialSites are the only functions that may stamp authMethodOIDC:
// each verifies a Keycloak bearer, or (edge) an identity the edge vouches for
// as OIDC-equivalent. A new credential gets its own method, as the land
// principal did (authMethodLand); labelling it OIDC would make every "is this
// a human?" gate admit it (laptop-claude, gd-CuUiTpMGUb).
var oidcCredentialSites = map[string]string{
	"windowAuthInterceptor": "verified Keycloak bearer, gRPC",
	"windowAuthMiddleware":  "verified Keycloak bearer, HTTP",
	"EdgeAuthMiddleware":    "edge v2 role-bearing identity and burn-in read-only, OIDC-equivalent by design",
}

// notOIDCRefusesSites are the only functions that may test
// "!= authMethodOIDC", and each refuses (or skips human-only work) on that
// branch. A gate whose not-OIDC branch admits the caller as a trusted service
// token must test !roleBearing(method) instead, or a land principal takes it.
var notOIDCRefusesSites = map[string]string{
	"pushDeviceCallerEmail":                  "returns no email",
	"BeadsServer.handleLinkSlackIdentity":    "403",
	"BeadsServer.handleUnlinkSlackIdentity":  "403",
	"BeadsServer.callerMayManageTaintPolicy": "returns false",
	"BeadsServer.handleAcceptTerms":          "403",
	"BeadsServer.handleMyTerms":              "403",
	"BeadsServer.readCursorsCaller":          "403",
	"BeadsServer.consentPushCaller":          "403",
	"BeadsServer.welcomeTourCaller":          "403",
	"commonsHumanSelf":                       "returns no person: origin=commons refused",
	"UserProvisioner.Middleware":             "skips provisioning, serves on",
	"frozenSupportPost":                      "returns false: no #support exemption, the freeze applies",
	"tokenLandsFromContext":                  "returns no land: a non-OIDC caller has no land audience",
	"BeadsServer.landDoorDenial":             "403 (after admitting a land principal of the door's land)",
	"impersonatedByFromContext":              "returns no impersonator: nothing is stamped, no session audit",
}

// TestAuthMethodOIDC_Guard fails when a non-test file in this package stamps
// authMethodOIDC outside oidcCredentialSites, or tests != authMethodOIDC
// outside notOIDCRefusesSites (gd-CuUiTpMGUb).
func TestAuthMethodOIDC_Guard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	isOIDC := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "authMethodOIDC"
	}
	sets, negations := map[string]bool{}, map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			site := funcSiteName(fn)
			ast.Inspect(fn, func(n ast.Node) bool {
				if n == nil {
					return false
				}
				pos := fset.Position(n.Pos()).String()
				switch n := n.(type) {
				case *ast.CallExpr:
					if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "withAuthMethod" && len(n.Args) == 2 && isOIDC(n.Args[1]) {
						sets[site] = true
						if _, ok := oidcCredentialSites[site]; !ok {
							t.Errorf("%s: %s stamps authMethodOIDC; a credential that is not a verified Keycloak bearer needs its own authMethod (see authMethodLand)", pos, site)
						}
					}
				case *ast.AssignStmt:
					for _, rhs := range n.Rhs {
						if isOIDC(rhs) {
							sets[site] = true
							if _, ok := oidcCredentialSites[site]; !ok {
								t.Errorf("%s: %s assigns authMethodOIDC; a credential that is not a verified Keycloak bearer needs its own authMethod (see authMethodLand)", pos, site)
							}
						}
					}
				case *ast.BinaryExpr:
					if n.Op == token.NEQ && (isOIDC(n.X) || isOIDC(n.Y)) {
						negations[site] = true
						if _, ok := notOIDCRefusesSites[site]; !ok {
							t.Errorf("%s: %s tests != authMethodOIDC; if that branch trusts the caller as a service token, test !roleBearing(method) so a land principal is not taken for one, then list the site in notOIDCRefusesSites", pos, site)
						}
					}
				}
				return true
			})
		}
	}
	for _, c := range []struct {
		name  string
		list  map[string]string
		found map[string]bool
	}{{"oidcCredentialSites", oidcCredentialSites, sets}, {"notOIDCRefusesSites", notOIDCRefusesSites, negations}} {
		var stale []string
		for site := range c.list {
			if !c.found[site] {
				stale = append(stale, site)
			}
		}
		sort.Strings(stale)
		if len(stale) > 0 {
			t.Errorf("%s lists %v, which no longer match: remove them", c.name, stale)
		}
	}
}

// funcSiteName is "Recv.Name" for a method, "Name" for a function.
func funcSiteName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
