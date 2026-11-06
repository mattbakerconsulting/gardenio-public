package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gd-Ld5r7Ok2rK / PR #737 review: the controller mints a land's GitHub token
// with the App github_app names, from the installation github_installation_id
// pins, on the repo git_url (or repos' primary entry) names. A tenant (its members, its land principal,
// its agent pods on the static or a fleet token) could rewrite those on its own
// project bead and repoint the token, so only an admin or the Lands
// provisioner writes them. Every denial sits next to a positive control.

const tenantMintJSON = `{"land_kind":"tenant","github_app":"lands","github_installation_id":"9001","git_url":"https://github.com/applicant/tomatoes.git","repos":[{"url":"https://github.com/applicant/tomatoes.git","role":"primary","branch":"main"}]}`

// victimRepos repoints repos' primary entry, which the controller prefers
// over git_url for the clone and the mint (podspec.go applyCommonConfig).
const victimRepos = `[{"url":"https://github.com/victim/private.git","role":"primary"}]`

func TestChangedLandMintFields(t *testing.T) {
	for _, tc := range []struct {
		name            string
		patch, existing string
		want            []string
	}{
		{"repin", `{"github_installation_id":"9002"}`, tenantMintJSON, []string{"github_installation_id"}},
		{"repin as a number", `{"github_installation_id":9002}`, tenantMintJSON, []string{"github_installation_id"}},
		{"clear the pin", `{"github_installation_id":null}`, tenantMintJSON, []string{"github_installation_id"}},
		{"to the fleet App", `{"github_app":"fleet"}`, tenantMintJSON, []string{"github_app"}},
		{"repoint the repo", `{"git_url":"https://github.com/applicant/other.git"}`, tenantMintJSON, []string{"git_url"}},
		{"all three", `{"github_app":"fleet","github_installation_id":"1","git_url":"x"}`, tenantMintJSON, []string{"git_url", "github_app", "github_installation_id"}},
		{"repoint repos' primary", `{"repos":` + victimRepos + `}`, tenantMintJSON, []string{"repos"}},
		{"repos as a JSON string", `{"repos":"[]"}`, tenantMintJSON, []string{"repos"}},
		{"clear repos", `{"repos":null}`, tenantMintJSON, []string{"repos"}},
		{"add a reference repo", `{"repos":[{"url":"https://github.com/applicant/tomatoes.git","role":"primary","branch":"main"},{"url":"https://github.com/victim/private.git","role":"reference"}]}`, tenantMintJSON, []string{"repos"}},
		{"operator repos", `{"repos":` + victimRepos + `}`, `{"git_url":"https://github.com/joyousaversion/gardenio.git"}`, nil},
		{"repos while becoming a tenant", `{"land_kind":"tenant","repos":` + victimRepos + `}`, `{}`, []string{"repos"}},
		{"repos with a move to the Lands App", `{"github_app":"lands","repos":` + victimRepos + `}`, `{}`, []string{"github_app", "repos"}},
		{"echo unchanged", tenantMintJSON, tenantMintJSON, nil},
		{"unrelated field", `{"card_theme":"garden"}`, tenantMintJSON, nil},
		{"operator git_url", `{"git_url":"https://github.com/joyousaversion/other.git"}`, `{"git_url":"https://github.com/joyousaversion/gardenio.git"}`, nil},
		{"operator land on the Lands App", `{"git_url":"y"}`, `{"land_kind":"operator","github_app":"lands"}`, []string{"git_url"}},
		{"odd land_kind is not operator", `{"git_url":"y"}`, `{"land_kind":"tennant"}`, []string{"git_url"}},
		{"git_url with a move to the Lands App", `{"github_app":"lands","git_url":"y"}`, `{}`, []string{"git_url", "github_app"}},
		{"git_url while becoming a tenant", `{"land_kind":"tenant","git_url":"y"}`, `{}`, []string{"git_url"}},
		{"pin on an operator project", `{"github_installation_id":"7"}`, `{}`, []string{"github_installation_id"}},
		{"create a tenant land", tenantMintJSON, ``, []string{"git_url", "github_app", "github_installation_id", "repos"}},
		{"create an operator project", `{"git_url":"https://github.com/joyousaversion/gardenio.git"}`, ``, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := changedLandMintFields(json.RawMessage(tc.patch), json.RawMessage(tc.existing))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("changed = %v, want %v", got, tc.want)
			}
		})
	}
}

func machineRoleCtx(roles ...string) context.Context {
	return WithMachineRoles(withAuthMethod(context.Background(), authMethodMachine), roles)
}

// The decision point for every caller class.
func TestDenyLandMintWrite_Callers(t *testing.T) {
	s, _, _ := newTestServer()
	scopedMember := WithGroups(WithRoles(oidcCtx("clw@example.com", nil), []string{RoleViewer, RoleOperator, RoleProjectMember}), []string{"/project-claimlayer"})
	tenant := &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Status: model.StatusOpen, Fields: json.RawMessage(tenantMintJSON)}
	operator := &model.Bead{ID: "gd-op", Title: "gardenio", Type: "project", Status: model.StatusOpen, Fields: json.RawMessage(`{"git_url":"https://github.com/joyousaversion/gardenio.git"}`)}
	existing := json.RawMessage(tenantMintJSON)
	denied := map[string]context.Context{
		"land principal":       landPrincipalContext(context.Background(), nil, "claimlayer"),
		"land member":          scopedMember,
		"non-admin operator":   WithRoles(oidcCtx("op@example.com", nil), []string{RoleViewer, RoleOperator}),
		"static token":         machineUserCtx(),
		"fleet token":          fleetUserCtx(),
		"machine client":       withAuthMethod(context.Background(), authMethodMachine),
		"lands gate machine":   machineRoleCtx("gardenio-machine-lands-gate"),
		"provisioner role, v1": WithMachineRoles(withAuthMethod(context.Background(), authMethodEdge), []string{landsProvisionerRole}),
	}
	for name, ctx := range denied {
		for what, d := range map[string]*AuthzDenial{
			"repin":             s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"github_installation_id":"9002"}`), existing),
			"fleet App":         s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"github_app":"fleet"}`), existing),
			"repoint repo":      s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"git_url":"https://github.com/victim/private.git"}`), existing),
			"create pinned":     s.authorizeLandSwitchFields(ctx, "create", "project", json.RawMessage(`{"github_app":"lands","github_installation_id":"9002"}`), nil),
			"pin endpoint":      s.authorizeLandSwitchKeyWrite(ctx, "update", tenant, "github_installation_id"),
			"app endpoint":      s.authorizeLandSwitchKeyWrite(ctx, "update", operator, "github_app"),
			"git_url endpoint":  s.authorizeLandSwitchKeyWrite(ctx, "update", tenant, "git_url"),
			"git_url via merge": s.authorizeAgentBeadFieldWrite(ctx, "update", tenant, "git_url"),
			"repoint repos":     s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"repos":`+victimRepos+`}`), existing),
			"repos endpoint":    s.authorizeLandSwitchKeyWrite(ctx, "update", tenant, "repos"),
			"repos via merge":   s.authorizeAgentBeadFieldWrite(ctx, "update", tenant, "repos"),
		} {
			if d == nil {
				t.Errorf("%s: %s allowed, want denied", name, what)
			} else if d.RequiredRole != RoleAdmin || !strings.Contains(d.Message, "Lands provisioner") {
				t.Errorf("%s: %s denial = %+v", name, what, d)
			}
		}
		// Controls: an operator project's git_url, an echoed pin, other fields.
		for what, d := range map[string]*AuthzDenial{
			"operator git_url":         s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"git_url":"https://github.com/joyousaversion/other.git"}`), operator.Fields),
			"operator git_url key":     s.authorizeLandSwitchKeyWrite(ctx, "update", operator, "git_url"),
			"operator repos":           s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"repos":`+victimRepos+`}`), operator.Fields),
			"operator repos key":       s.authorizeLandSwitchKeyWrite(ctx, "update", operator, "repos"),
			"echoed pins":              s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(tenantMintJSON), existing),
			"unrelated field":          s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"card_theme":"garden"}`), existing),
			"mint field on a task":     s.authorizeLandSwitchFields(ctx, "update", "task", json.RawMessage(`{"github_installation_id":"1","git_url":"x"}`), existing),
			"mint key on a task":       s.authorizeLandSwitchKeyWrite(ctx, "update", &model.Bead{Type: "task", Fields: existing}, "git_url", "github_app"),
			"create operator, git_url": s.authorizeLandSwitchFields(ctx, "create", "project", json.RawMessage(`{"git_url":"https://github.com/joyousaversion/x.git"}`), nil),
		} {
			if d != nil {
				t.Errorf("%s: %s denied: %v", name, what, d)
			}
		}
	}
	allowed := map[string]context.Context{
		"oidc admin":        adminOIDCCtx(),
		"lands provisioner": machineRoleCtx("gardenio-machine-accounts", landsProvisionerRole),
		"escalation replay": withMachineWrite(machineUserCtx()),
		"auth disabled":     withAuthMethod(context.Background(), authMethodNone),
	}
	for name, ctx := range allowed {
		if d := s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"github_installation_id":"9002","git_url":"https://github.com/applicant/new.git"}`), existing); d != nil {
			t.Errorf("%s: repin denied: %v", name, d)
		}
		if d := s.authorizeLandSwitchFields(ctx, "update", "project", json.RawMessage(`{"repos":[{"url":"https://github.com/applicant/new.git","role":"primary"}]}`), existing); d != nil {
			t.Errorf("%s: repos write denied: %v", name, d)
		}
		if d := s.authorizeLandSwitchKeyWrite(ctx, "update", tenant, "git_url", "repos", "github_app", "github_installation_id"); d != nil {
			t.Errorf("%s: key write denied: %v", name, d)
		}
	}
	// The provisioner writes the mint pins, not the kill switches.
	if d := s.authorizeLandSwitchFields(machineRoleCtx(landsProvisionerRole), "update", "project", json.RawMessage(`{"land_frozen":false}`), json.RawMessage(`{"land_frozen":true}`)); d == nil {
		t.Error("the Lands provisioner role must not thaw a land")
	}
}

// HTTP: the full stack, with the real static token, a fleet token, land
// members, the land principal and a non-admin operator.
func TestLandMintFields_HTTP(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "false")
	f.ms.beads["proj-claimlayer"].Fields = []byte(`{"land_kind":"tenant","land_frozen":false,"github_app":"lands","github_installation_id":"9001","git_url":"https://github.com/applicant/tomatoes.git","repos":[{"url":"https://github.com/applicant/tomatoes.git","role":"primary"}]}`)
	const project = "/v1/beads/proj-claimlayer"
	snapshot := func() string { return string(f.ms.beads["proj-claimlayer"].Fields) }

	attempts := []struct {
		name         string
		method, path string
		body         any
	}{
		{"repin the installation", http.MethodPatch, project, map[string]any{"fields": map[string]any{"github_installation_id": "9002"}}},
		{"clear the pin", http.MethodPatch, project, map[string]any{"fields": map[string]any{"github_installation_id": ""}}},
		{"select the fleet App", http.MethodPatch, project, map[string]any{"fields": map[string]any{"github_app": "fleet"}}},
		{"repoint git_url", http.MethodPatch, project, map[string]any{"fields": map[string]any{"git_url": "https://github.com/victim/private.git"}}},
		{"pin field endpoint", http.MethodPost, project + "/fields/github_installation_id/append", appendArrayFieldRequest{Element: "9002"}},
		{"git_url field endpoint", http.MethodPost, project + "/fields/git_url/append", appendArrayFieldRequest{Element: "x"}},
		{"repoint on a close body", http.MethodPost, project + "/close", map[string]any{"git_url": "https://github.com/victim/private.git"}},
		{"repoint repos' primary", http.MethodPatch, project, map[string]any{"fields": map[string]any{"repos": json.RawMessage(victimRepos)}}},
		{"repos as a JSON string", http.MethodPatch, project, map[string]any{"fields": map[string]any{"repos": victimRepos}}},
		{"repos field endpoint", http.MethodPost, project + "/fields/repos/append", appendArrayFieldRequest{Element: "x"}},
		{"repos on a close body", http.MethodPost, project + "/close", map[string]any{"repos": json.RawMessage(victimRepos)}},
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
					t.Errorf("mint field changed despite 403: %s -> %s", before, after)
				}
			})
		}
	}

	t.Run("controls", func(t *testing.T) {
		for _, tok := range []string{scopedAdminToken, tokFleetBridge} {
			if code, body := f.do(t, tok, http.MethodPatch, project, map[string]any{"fields": map[string]any{"card_theme": "garden", "github_installation_id": "9001", "git_url": "https://github.com/applicant/tomatoes.git"}}); code != http.StatusOK {
				t.Fatalf("%s: unrelated project field with the pins echoed = %d %s, want 200", tok, code, body)
			}
		}
	})

	t.Run("admin repins", func(t *testing.T) {
		if code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, project, map[string]any{"fields": map[string]any{"github_installation_id": "9002"}}); code != http.StatusOK {
			t.Fatalf("admin repin = %d %s, want 200", code, body)
		}
		if !strings.Contains(snapshot(), `"github_installation_id":"9002"`) {
			t.Errorf("admin repin not stored: %s", snapshot())
		}
	})
}

// gRPC: UpdateBead maps the denial to PermissionDenied; an operator project's
// git_url stays writable.
func TestLandMintFields_GRPC(t *testing.T) {
	s, ms, _ := newTestServer()
	seed := func() {
		ms.beads["gd-proj"] = &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Kind: model.KindConfig, Status: model.StatusOpen, Fields: json.RawMessage(tenantMintJSON)}
		ms.beads["gd-op"] = &model.Bead{ID: "gd-op", Title: "gardenio", Type: "project", Kind: model.KindConfig, Status: model.StatusOpen, Fields: json.RawMessage(`{"git_url":"https://github.com/joyousaversion/gardenio.git"}`)}
	}
	updates := []*beadsv1.UpdateBeadRequest{
		{Id: "gd-proj", Fields: []byte(`{"github_installation_id":"9002"}`)},
		{Id: "gd-proj", Fields: []byte(`{"github_app":"fleet"}`)},
		{Id: "gd-proj", Fields: []byte(`{"git_url":"https://github.com/victim/private.git"}`)},
		{Id: "gd-proj", Fields: []byte(`{"repos":` + victimRepos + `}`)},
		{Id: "gd-op", Fields: []byte(`{"github_app":"lands","github_installation_id":"9001"}`)},
	}
	scopedMember := WithGroups(WithRoles(oidcCtx("clw@example.com", nil), []string{RoleViewer, RoleOperator, RoleProjectMember}), []string{"/project-claimlayer"})
	for who, ctx := range map[string]context.Context{
		"static token": machineUserCtx(),
		"fleet token":  fleetUserCtx(),
		"land member":  scopedMember,
	} {
		t.Run(who, func(t *testing.T) {
			seed()
			for _, u := range updates {
				if _, err := s.UpdateBead(ctx, u); !grpcRefused(err) {
					t.Errorf("UpdateBead %s fields=%s: err = %v, want PermissionDenied or NotFound", u.Id, u.Fields, err)
				}
			}
			if got := string(ms.beads["gd-proj"].Fields); got != tenantMintJSON {
				t.Errorf("tenant mint fields changed despite denial: %s", got)
			}
		})
	}
	seed()
	if _, err := s.UpdateBead(machineUserCtx(), &beadsv1.UpdateBeadRequest{Id: "gd-op", Fields: []byte(`{"git_url":"https://github.com/joyousaversion/other.git"}`)}); err != nil {
		t.Fatalf("static token on an operator project's git_url: %v, want allowed", err)
	}
	if _, err := s.UpdateBead(machineUserCtx(), &beadsv1.UpdateBeadRequest{Id: "gd-op", Fields: []byte(`{"repos":[{"url":"https://github.com/joyousaversion/other.git","role":"primary"}]}`)}); err != nil {
		t.Fatalf("static token on an operator project's repos: %v, want allowed", err)
	}
}

// An escalation replay skips the gate, so approving one that repins a land is
// held to the gate as the approver: a non-admin operator cannot.
func TestApprove_LandMintReplayNeedsAdmin(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "false")
	f.ms.beads["proj-claimlayer"].Fields = []byte(tenantMintJSON)
	f.seedEscalation("esc-repin", "project:gardenio", "update",
		map[string]any{"fields": map[string]any{"github_installation_id": "9002"}},
		map[string]any{"target_bead_id": "proj-claimlayer"})
	c, b := f.approve(t, tokOperator, "esc-repin")
	f.wantStatus(t, "non-admin operator approves a repin", c, b, http.StatusForbidden)
	if got := string(f.ms.beads["proj-claimlayer"].Fields); got != tenantMintJSON {
		t.Fatalf("repinned despite the refusal: %s", got)
	}
	c, b = f.approve(t, tokOwnerAdmin, "esc-repin")
	f.wantStatus(t, "admin approves a repin", c, b, http.StatusOK)
}

// landBMintJSON is a second tenant land, B ("other"), on its own repo and
// installation.
const landBMintJSON = `{"land_kind":"tenant","github_app":"lands","github_installation_id":"9002","git_url":"https://github.com/grower/beans.git","repos":[{"url":"https://github.com/grower/beans.git","role":"primary","branch":"main"}]}`

// gd-Ld5r7Ok2rK third review, the endpoint half of the two-land test (the
// controller half is TestTwoLands_* in gardenio/controller/cmd/controller):
// with two tenant lands A (claimlayer) and B (other), nobody of A's (its
// agent's own per-agent token, its land principal, its members) can steer the
// controller into minting B's token. B's mint fields cannot be written, and
// A's own cannot be pointed at B's repo or installation. Each refusal leaves
// both project beads unchanged.
func TestLandMintFields_TwoLands(t *testing.T) {
	f := newScopedFixture(t)
	const tokAgentA = "agent-claimlayer-a1-tok"
	f.fc.RegisterTokenWithSubject(tokAgentA, "agent-gd-a1", "claimlayer", nil, nil, time.Time{}, "a1")
	f.seedLandProject(t, "false")
	f.ms.beads["proj-claimlayer"].Fields = []byte(tenantMintJSON)
	f.seedTyped("proj-other", "project", "")
	f.ms.beads["proj-other"].Title = "other"
	f.ms.beads["proj-other"].Kind = "config"
	f.ms.beads["proj-other"].Fields = []byte(landBMintJSON)
	snapshot := func() string {
		return string(f.ms.beads["proj-claimlayer"].Fields) + " | " + string(f.ms.beads["proj-other"].Fields)
	}

	const landA, landB = "/v1/beads/proj-claimlayer", "/v1/beads/proj-other"
	const bRepo = "https://github.com/grower/beans.git"
	bRepos := json.RawMessage(`[{"url":"` + bRepo + `","role":"primary"}]`)
	patch := func(fields map[string]any) map[string]any { return map[string]any{"fields": fields} }
	attempts := []struct {
		name         string
		method, path string
		body         any
	}{
		{"point A at B's repo", http.MethodPatch, landA, patch(map[string]any{"git_url": bRepo})},
		{"point A's repos at B's repo", http.MethodPatch, landA, patch(map[string]any{"repos": bRepos})},
		{"pin A to B's installation", http.MethodPatch, landA, patch(map[string]any{"github_installation_id": "9002"})},
		{"take B's repo and pin together", http.MethodPatch, landA, patch(map[string]any{"git_url": bRepo, "repos": bRepos, "github_installation_id": "9002"})},
		{"point A at B's repo on a close body", http.MethodPost, landA + "/close", map[string]any{"git_url": bRepo}},
		{"repoint B", http.MethodPatch, landB, patch(map[string]any{"git_url": "https://github.com/applicant/tomatoes.git"})},
		{"repin B to A's installation", http.MethodPatch, landB, patch(map[string]any{"github_installation_id": "9001"})},
		{"move B to the fleet App", http.MethodPatch, landB, patch(map[string]any{"github_app": "fleet"})},
		{"B's repos field endpoint", http.MethodPost, landB + "/fields/repos/append", appendArrayFieldRequest{Element: "x"}},
	}
	for who, tok := range map[string]string{
		"A's agent":          tokAgentA,
		"A's land principal": tokLandClaimlayer,
		"A's member":         tokScopedWriter,
		"A's owner":          tokScopedOwner,
	} {
		for _, a := range attempts {
			t.Run(who+"/"+a.name, func(t *testing.T) {
				before := snapshot()
				code, body := f.do(t, tok, a.method, a.path, a.body)
				if code != http.StatusForbidden && code != http.StatusNotFound {
					t.Fatalf("%s %s = %d %s, want 403 or 404", a.method, a.path, code, body)
				}
				if after := snapshot(); after != before {
					t.Errorf("a mint field changed despite %d: %s -> %s", code, before, after)
				}
			})
		}
	}

	t.Run("control: an admin repins B", func(t *testing.T) {
		if code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, landB, patch(map[string]any{"github_installation_id": "9003"})); code != http.StatusOK {
			t.Fatalf("admin repin = %d %s, want 200", code, body)
		}
	})
}

// grpcRefused reports whether err refuses a write: PermissionDenied, or
// NotFound for a bead the caller cannot read (gd-7APTAIhdC5).
func grpcRefused(err error) bool {
	c := status.Code(err)
	return c == codes.PermissionDenied || c == codes.NotFound
}
