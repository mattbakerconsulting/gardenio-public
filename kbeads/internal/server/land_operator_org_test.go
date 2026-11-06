package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// gd-72m3STiqya: a project that mints with the Lands App is never bound to a
// repo on an operator org, whoever writes it. Only an admin or the Lands
// provisioner writes the pin (land_mint_authz_test.go), so every caller here
// is one that would otherwise be allowed. Every refusal sits next to a
// positive control.

const operatorProjectJSON = `{"git_url":"https://github.com/mattbakerconsulting/gardenio.git"}`

func operatorOrgServer(t *testing.T) *BeadsServer {
	t.Helper()
	s, _, _ := newTestServer()
	s.SetLandsOperatorGitHubOrgs([]string{" MattBakerConsulting ", "claimlayer", ""})
	return s
}

func TestOperatorOrgPin_RefusedForEveryWriter(t *testing.T) {
	s := operatorOrgServer(t)
	tenant := json.RawMessage(tenantMintJSON)
	operator := json.RawMessage(operatorProjectJSON)
	writers := map[string]context.Context{
		"oidc admin":        adminOIDCCtx(),
		"lands provisioner": machineRoleCtx("gardenio-machine-accounts", landsProvisionerRole),
		"escalation replay": withMachineWrite(machineUserCtx()),
		"auth disabled":     withAuthMethod(context.Background(), authMethodNone),
	}
	refused := map[string]struct {
		action          string
		patch, existing json.RawMessage
	}{
		"open a tenant land on an operator repo":    {"create", json.RawMessage(`{"land_kind":"tenant","github_app":"lands","github_installation_id":"9001","git_url":"https://github.com/mattbakerconsulting/gardenio.git"}`), nil},
		"operator repo only in repos":               {"create", json.RawMessage(`{"land_kind":"tenant","github_app":"lands","github_installation_id":"9001","git_url":"https://github.com/applicant/tomatoes.git","repos":[{"url":"https://github.com/applicant/tomatoes.git","role":"primary"},{"url":"https://github.com/claimlayer/app.git","role":"reference"}]}`), nil},
		"move an operator project to the Lands App": {"update", json.RawMessage(`{"github_app":"lands","github_installation_id":"9001"}`), operator},
		"make an operator project a tenant land":    {"update", json.RawMessage(`{"land_kind":"tenant"}`), operator},
		"repoint a land, other case":                {"update", json.RawMessage(`{"git_url":"https://GitHub.com/MattBakerConsulting/gardenio"}`), tenant},
		"repoint a land, scp form":                  {"update", json.RawMessage(`{"git_url":"git@github.com:mattbakerconsulting/gardenio.git"}`), tenant},
		"repoint a land, ssh form":                  {"update", json.RawMessage(`{"git_url":"ssh://git@github.com/claimlayer/app.git"}`), tenant},
		"repoint repos":                             {"update", json.RawMessage(`{"repos":[{"url":"https://github.com/mattbakerconsulting/gardenio.git","role":"primary"}]}`), tenant},
		"repos as a JSON string":                    {"update", json.RawMessage(`{"repos":"[{\"url\":\"https://github.com/claimlayer/app\",\"role\":\"primary\"}]"}`), tenant},
	}
	allowed := map[string]struct {
		action          string
		patch, existing json.RawMessage
	}{
		"open a tenant land on the applicant's repo": {"create", tenant, nil},
		"repin a land on the applicant's account":    {"update", json.RawMessage(`{"github_installation_id":"9002","git_url":"https://github.com/applicant/new.git"}`), tenant},
		"operator project on the fleet App":          {"update", json.RawMessage(`{"github_app":"fleet","git_url":"https://github.com/mattbakerconsulting/other.git"}`), operator},
		"create an operator project":                 {"create", operator, nil},
		"an operator org's name is not a prefix":     {"update", json.RawMessage(`{"git_url":"https://github.com/mattbakerconsulting-fan/tomatoes.git"}`), tenant},
		"not github.com":                             {"update", json.RawMessage(`{"git_url":"https://gitlab.com/mattbakerconsulting/gardenio.git"}`), tenant},
		// A land already in the bad state can still be frozen and edited.
		"freeze a land on an operator repo":        {"update", json.RawMessage(`{"land_frozen":true}`), json.RawMessage(`{"land_kind":"tenant","github_app":"lands","git_url":"https://github.com/mattbakerconsulting/gardenio.git"}`)},
		"echoed pins":                              {"update", json.RawMessage(`{"github_app":"lands","git_url":"https://github.com/mattbakerconsulting/gardenio.git","card_theme":"garden"}`), json.RawMessage(`{"land_kind":"tenant","github_app":"lands","git_url":"https://github.com/mattbakerconsulting/gardenio.git"}`)},
		"clear the Lands App off an operator repo": {"update", json.RawMessage(`{"github_app":null,"github_installation_id":null}`), json.RawMessage(`{"github_app":"lands","github_installation_id":"7","git_url":"https://github.com/mattbakerconsulting/gardenio.git"}`)},
	}
	for who, ctx := range writers {
		for what, tc := range refused {
			if who == "lands provisioner" && strings.Contains(what, "tenant land") && tc.action == "update" {
				continue // land_kind on update is a kill switch it may not throw at all
			}
			d := s.authorizeLandSwitchFields(ctx, tc.action, "project", tc.patch, tc.existing)
			if d == nil {
				t.Errorf("%s: %s allowed, want refused", who, what)
			} else if !strings.Contains(d.Message, "operator org") || d.Escalatable {
				t.Errorf("%s: %s denial = %+v", who, what, d)
			}
		}
		for what, tc := range allowed {
			if who == "lands provisioner" && (tc.action == "create" || strings.Contains(what, "freeze")) {
				continue // its create and freeze rules are land_switch_authz.go's
			}
			if d := s.authorizeLandSwitchFields(ctx, tc.action, "project", tc.patch, tc.existing); d != nil {
				t.Errorf("%s: %s refused: %v", who, what, d)
			}
		}
	}

	// No list, no check.
	none, _, _ := newTestServer()
	for what, tc := range refused {
		if d := none.authorizeLandSwitchFields(adminOIDCCtx(), tc.action, "project", tc.patch, tc.existing); d != nil {
			t.Errorf("no operator orgs: %s refused: %v", what, d)
		}
	}
}

// The single-field endpoints and the extra-fields merge only know the key,
// so a key that could bind a Lands pin is refused there while the list is
// set, even for an admin; an operator project's repo stays writable.
func TestOperatorOrgPin_KeyOnlyWrites(t *testing.T) {
	s := operatorOrgServer(t)
	tenant := &model.Bead{ID: "gd-proj", Title: "claimlayer", Type: "project", Status: model.StatusOpen, Fields: json.RawMessage(tenantMintJSON)}
	operator := &model.Bead{ID: "gd-op", Title: "gardenio", Type: "project", Status: model.StatusOpen, Fields: json.RawMessage(operatorProjectJSON)}
	for who, ctx := range map[string]context.Context{
		"oidc admin":        adminOIDCCtx(),
		"lands provisioner": machineRoleCtx(landsProvisionerRole),
		"escalation replay": withMachineWrite(machineUserCtx()),
	} {
		for what, d := range map[string]*AuthzDenial{
			"pin endpoint":       s.authorizeLandSwitchKeyWrite(ctx, "update", tenant, "github_installation_id"),
			"app endpoint":       s.authorizeLandSwitchKeyWrite(ctx, "update", operator, "github_app"),
			"land repos":         s.authorizeLandSwitchKeyWrite(ctx, "update", tenant, "repos"),
			"land git_url merge": s.authorizeAgentBeadFieldWrite(ctx, "update", tenant, "git_url"),
		} {
			if d == nil || !strings.Contains(d.Message, "operator org") {
				t.Errorf("%s: %s = %v, want the operator-org refusal", who, what, d)
			}
		}
		if who == "oidc admin" {
			if d := s.authorizeLandSwitchKeyWrite(ctx, "update", operator, "land_kind"); d == nil || !strings.Contains(d.Message, "operator org") {
				t.Errorf("%s: land_kind key = %v, want the operator-org refusal", who, d)
			}
		}
		for what, d := range map[string]*AuthzDenial{
			"operator git_url": s.authorizeLandSwitchKeyWrite(ctx, "update", operator, "git_url", "repos"),
			"unrelated key":    s.authorizeLandSwitchKeyWrite(ctx, "update", tenant, "card_theme"),
		} {
			if d != nil {
				t.Errorf("%s: %s refused: %v", who, what, d)
			}
		}
	}
	none, _, _ := newTestServer()
	if d := none.authorizeLandSwitchKeyWrite(adminOIDCCtx(), "update", tenant, "github_installation_id", "repos"); d != nil {
		t.Errorf("no operator orgs: key write refused: %v", d)
	}
}

// HTTP: an admin's PATCH that repoints a land at an operator repo is a 403
// and changes nothing; the same admin repoints it at the applicant's repo.
func TestOperatorOrgPin_HTTP(t *testing.T) {
	f := newScopedFixture(t)
	f.srv.SetLandsOperatorGitHubOrgs([]string{"mattbakerconsulting", "claimlayer"})
	f.seedLandProject(t, "false")
	f.ms.beads["proj-claimlayer"].Fields = []byte(tenantMintJSON)
	const project = "/v1/beads/proj-claimlayer"
	snapshot := func() string { return string(f.ms.beads["proj-claimlayer"].Fields) }

	for name, a := range map[string]struct {
		method, path string
		body         any
	}{
		"repoint git_url":    {http.MethodPatch, project, map[string]any{"fields": map[string]any{"git_url": "https://github.com/mattbakerconsulting/gardenio.git"}}},
		"repoint repos":      {http.MethodPatch, project, map[string]any{"fields": map[string]any{"repos": []map[string]string{{"url": "https://github.com/claimlayer/app.git", "role": "primary"}}}}},
		"pin field endpoint": {http.MethodPost, project + "/fields/github_installation_id/append", appendArrayFieldRequest{Element: "9002"}},
		"repoint on a close": {http.MethodPost, project + "/close", map[string]any{"git_url": "https://github.com/mattbakerconsulting/gardenio.git"}},
	} {
		t.Run(name, func(t *testing.T) {
			before := snapshot()
			code, body := f.do(t, tokOwnerAdmin, a.method, a.path, a.body)
			if code != http.StatusForbidden || !strings.Contains(body, "operator org") {
				t.Fatalf("%s %s = %d %s, want 403 naming the operator org", a.method, a.path, code, body)
			}
			if after := snapshot(); after != before {
				t.Errorf("fields changed despite 403: %s -> %s", before, after)
			}
		})
	}

	if code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, project, map[string]any{"fields": map[string]any{"git_url": "https://github.com/applicant/new.git"}}); code != http.StatusOK {
		t.Fatalf("admin repoint on the applicant's account = %d %s, want 200", code, body)
	}
}

// An approved escalation replay runs as a machine write, which no role gate
// stops; it still cannot bind a land to an operator repo.
func TestOperatorOrgPin_ReplayRefused(t *testing.T) {
	f := newScopedFixture(t)
	f.srv.SetLandsOperatorGitHubOrgs([]string{"mattbakerconsulting"})
	f.seedLandProject(t, "false")
	f.ms.beads["proj-claimlayer"].Fields = []byte(tenantMintJSON)
	f.seedEscalation("esc-repoint", "project:gardenio", "update",
		map[string]any{"fields": map[string]any{"git_url": "https://github.com/mattbakerconsulting/gardenio.git"}},
		map[string]any{"target_bead_id": "proj-claimlayer"})
	c, b := f.approve(t, tokOwnerAdmin, "esc-repoint")
	if c != http.StatusOK || !strings.Contains(b, `"result_status":"failure"`) || !strings.Contains(b, "operator org") {
		t.Fatalf("approve = %d %s, want the replay to fail on the operator org", c, b)
	}
	if got := string(f.ms.beads["proj-claimlayer"].Fields); got != tenantMintJSON {
		t.Fatalf("replay repointed the land at an operator repo: %s", got)
	}
}

// Every URL form git clones a github.com repo from names its owner; one read
// as "" skipped the operator-org guard (gd-i74esUHCTg item 6).
func TestGithubRepoOwner_URLForms(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://github.com/MattBakerConsulting/gardenio.git", "mattbakerconsulting"},
		{"https://x-access-token:t@github.com/acme/repo", "acme"},
		{"http://www.github.com/acme/repo", "acme"},
		{"ssh://git@github.com/acme/repo.git", "acme"},
		{"ssh://git@ssh.github.com:443/acme/repo.git", "acme"},
		{"git+ssh://git@github.com/acme/repo", "acme"},
		{"git://github.com/acme/repo", "acme"},
		{"git@github.com:acme/repo.git", "acme"},
		{"org-1@github.com:acme/repo.git", "acme"},
		{"github.com:acme/repo", "acme"},
		{"git@ssh.github.com:acme/repo", "acme"},
		{"github.com/acme/repo", "acme"},
		{"GitHub.com/Acme/repo", "acme"},
		{"www.github.com/acme/repo", "acme"},
		{"github.com./acme/repo", "acme"},
		{" github.com/acme/repo ", "acme"},
		{"https://gitlab.com/acme/repo", ""},
		{"gitlab.com/acme/repo", ""},
		{"git@gitlab.com:acme/repo", ""},
		{"https://github.com.evil.example/acme/repo", ""},
		{"./local/repo", ""},
		{"", ""},
	} {
		if got := githubRepoOwner(tc.url); got != tc.want {
			t.Errorf("githubRepoOwner(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// A Lands-pinned project whose git_url names an operator org in a scheme-less
// or ssh.github.com form is refused like the https form.
func TestOperatorOrgPin_RefusedForEveryURLForm(t *testing.T) {
	s := operatorOrgServer(t)
	for _, u := range []string{
		"github.com/mattbakerconsulting/gardenio",
		"ssh://git@ssh.github.com:443/mattbakerconsulting/gardenio.git",
		"git@ssh.github.com:mattbakerconsulting/gardenio.git",
	} {
		patch, _ := json.Marshal(map[string]any{"github_app": "lands", "git_url": u})
		if d := s.denyOperatorOrgPin("create", "project", patch, nil); d == nil {
			t.Errorf("git_url %q on a Lands-pinned project was not refused", u)
		}
	}
}
