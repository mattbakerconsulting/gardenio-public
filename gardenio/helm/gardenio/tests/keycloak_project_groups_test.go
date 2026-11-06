package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	groupsTpl = "templates/keycloak/groups.yaml"
	rolesTpl  = "templates/keycloak/roles.yaml"

	projectMemberRole = "gardenio-project-member"
)

// A role a project group must never carry: holding any of these makes the
// member a global (cross-project) reader, operator, admin or approver.
var globalRoles = []string{
	"gardenio-viewer", "gardenio-operator", "gardenio-admin", "escalation-approver",
}

type renderedGroup struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Name       string   `yaml:"name"`
		RealmRoles []string `yaml:"realmRoles"`
		ClientRole any      `yaml:"clientRoles"`
	} `yaml:"spec"`
}

type renderedRole struct {
	Spec struct {
		Name      string `yaml:"name"`
		IsDefault bool   `yaml:"isDefault"`
	} `yaml:"spec"`
}

// decodeDocs decodes every YAML document of a helm render into T, skipping
// the empty ones (comment-only docs between `---`).
func decodeDocs[T any](t *testing.T, rendered string) []T {
	t.Helper()
	var out []T
	for _, doc := range strings.Split(rendered, "\n---") {
		if strings.TrimSpace(regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(doc, "")) == "" {
			continue
		}
		var v T
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("decode doc: %v\n%s", err, doc)
		}
		out = append(out, v)
	}
	return out
}

func renderGroups(t *testing.T, setFlags ...string) []renderedGroup {
	t.Helper()
	out, err := helmTemplateGardenio(t, groupsTpl, setFlags...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return decodeDocs[renderedGroup](t, out)
}

func projectGroupsOf(groups []renderedGroup) []renderedGroup {
	var out []renderedGroup
	for _, g := range groups {
		if strings.HasPrefix(g.Spec.Name, "project-") {
			out = append(out, g)
		}
	}
	return out
}

// One KeycloakRealmGroup per keycloak.projectGroups entry, named
// project-<name>, carrying exactly the project-member role.
func TestKeycloakProjectGroups_RendersGroupPerProject(t *testing.T) {
	t.Parallel()
	got := projectGroupsOf(renderGroups(t,
		"keycloak.projectGroups[0].name=claimlayer",
		"keycloak.projectGroups[1].name=gardenio",
	))
	if len(got) != 2 {
		t.Fatalf("want 2 project groups, got %d: %+v", len(got), got)
	}
	for i, want := range []string{"project-claimlayer", "project-gardenio"} {
		g := got[i]
		if g.Spec.Name != want {
			t.Errorf("group %d name = %q, want %q", i, g.Spec.Name, want)
		}
		if len(g.Spec.RealmRoles) != 1 || g.Spec.RealmRoles[0] != projectMemberRole {
			t.Errorf("%s realmRoles = %v, want exactly [%s]", want, g.Spec.RealmRoles, projectMemberRole)
		}
		if g.Spec.ClientRole != nil {
			t.Errorf("%s must carry no clientRoles, got %v", want, g.Spec.ClientRole)
		}
	}
}

// The default (empty) projectGroups list adds no group, so existing consumers
// see only the seven chart groups.
func TestKeycloakProjectGroups_DefaultRendersNone(t *testing.T) {
	t.Parallel()
	groups := renderGroups(t)
	if got := projectGroupsOf(groups); len(got) != 0 {
		t.Fatalf("default render must emit no project group, got %+v", got)
	}
	if len(groups) != 7 {
		t.Errorf("default render: want the 7 chart groups, got %d", len(groups))
	}
}

// lands-tenants (Track B step 7.8, B-D2) is admitted by the viewer edge and
// joined by every tenant, so it must carry gardenio-project-member and
// nothing else: that role makes its member a scoped principal, and the group
// names no project, so a tenant reads only their /project-<land> groups. A
// global role here would hand every tenant the whole farm.
func TestKeycloakGroups_LandsTenantsIsScopedOnly(t *testing.T) {
	t.Parallel()
	var found bool
	for _, g := range renderGroups(t) {
		if g.Spec.Name != "lands-tenants" {
			continue
		}
		found = true
		if len(g.Spec.RealmRoles) != 1 || g.Spec.RealmRoles[0] != projectMemberRole {
			t.Errorf("lands-tenants realmRoles = %v, want [%s]", g.Spec.RealmRoles, projectMemberRole)
		}
		if g.Spec.ClientRole != nil {
			t.Errorf("lands-tenants must carry no clientRoles, got %v", g.Spec.ClientRole)
		}
	}
	if !found {
		t.Fatal("chart default renders no lands-tenants group")
	}
}

// invited (sign-in invites, gd-ycE484kLyk) is what gets a GitHub sign-in past
// the viewer edge's /lands/ check, and the Lands provisioner joins anyone
// whose invite email matches. It must hold no role at all, as the chart
// renders it and as the k3s overlay does: any role there would go to every
// invitee.
func TestKeycloakGroups_InvitedHoldsNoRole(t *testing.T) {
	t.Parallel()
	overlay, err := exec.Command("helm", "template", "gardenio", chartDir(t),
		"--show-only", groupsTpl, "-f", deployValuesK3s(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template with prod overlay failed: %v\n%s", err, overlay)
	}
	for name, groups := range map[string][]renderedGroup{
		"chart default": renderGroups(t),
		"prod overlay":  decodeDocs[renderedGroup](t, string(overlay)),
	} {
		var found int
		for _, g := range groups {
			if g.Spec.Name != "invited" {
				continue
			}
			found++
			if len(g.Spec.RealmRoles) != 0 || g.Spec.ClientRole != nil {
				t.Errorf("%s: invited must hold no role, got realmRoles %v clientRoles %v", name, g.Spec.RealmRoles, g.Spec.ClientRole)
			}
		}
		if found != 1 {
			t.Errorf("%s renders %d invited groups, want 1", name, found)
		}
	}
}

// A project group is a scope grant, never a global one.
func TestKeycloakProjectGroups_NeverGrantsGlobalRole(t *testing.T) {
	t.Parallel()
	got := projectGroupsOf(renderGroups(t,
		"keycloak.projectGroups[0].name=claimlayer",
		"keycloak.projectGroups[1].name=gardenio",
	))
	if len(got) == 0 {
		t.Fatal("no project group rendered")
	}
	for _, g := range got {
		for _, r := range g.Spec.RealmRoles {
			for _, bad := range globalRoles {
				if r == bad {
					t.Errorf("%s maps to global role %s", g.Spec.Name, bad)
				}
			}
		}
	}
	// A hand-written chart group must not be able to smuggle one in through
	// projectGroups either: the role list is not an input.
	smuggle := projectGroupsOf(renderGroups(t,
		"keycloak.projectGroups[0].name=claimlayer",
		"keycloak.projectGroups[0].realmRoles[0]=gardenio-admin",
	))
	for _, g := range smuggle {
		for _, r := range g.Spec.RealmRoles {
			if r != projectMemberRole {
				t.Errorf("projectGroups[].realmRoles leaked into %s: %v", g.Spec.Name, g.Spec.RealmRoles)
			}
		}
	}
}

// The project-member role itself is declared.
func TestKeycloakRoles_DeclaresProjectMember(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, rolesTpl)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	for _, r := range decodeDocs[renderedRole](t, out) {
		if r.Spec.Name == projectMemberRole {
			return
		}
	}
	t.Errorf("role %s not rendered:\n%s", projectMemberRole, out)
}

// A brand-new realm user must hold no gardenio role: no role is isDefault.
func TestKeycloakRoles_NoDefaultRole(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, rolesTpl)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	roles := decodeDocs[renderedRole](t, out)
	if len(roles) == 0 {
		t.Fatal("no KeycloakRealmRole rendered")
	}
	for _, r := range roles {
		if r.Spec.IsDefault {
			t.Errorf("role %s renders isDefault: true", r.Spec.Name)
		}
	}
}

// Positive control: the assertion above fires when a role is made default.
func TestKeycloakRoles_NoDefaultRole_PositiveControl(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, rolesTpl, "keycloak.roles[0].isDefault=true")
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	found := false
	for _, r := range decodeDocs[renderedRole](t, out) {
		if r.Spec.IsDefault {
			found = true
		}
	}
	if !found {
		t.Fatal("render given isDefault=true showed no default role: the NoDefaultRole check cannot detect a regression")
	}
}

func deployValuesK3s(t *testing.T) string {
	t.Helper()
	return filepath.Join(chartDir(t), "..", "..", "..", "deploy-values", "gardenio-k3s.yaml")
}

// The k3s overlay re-declares the whole groups list (a Helm list override
// replaces rather than merges), so dropping a chart group there silently
// drops its roles. All seven must survive, each with its chart realm role
// (invited has none), and the project groups must be present.
func TestDeployValuesGroupsKeepsFiveChartGroups(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(deployValuesK3s(t))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Keycloak struct {
			Groups []struct {
				Name       string   `yaml:"name"`
				RealmRoles []string `yaml:"realmRoles"`
			} `yaml:"groups"`
		} `yaml:"keycloak"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, g := range doc.Keycloak.Groups {
		got[g.Name] = g.RealmRoles
	}
	want := map[string]string{
		"gardenio-admins":               "gardenio-admin",
		"gardenio-operators":            "gardenio-operator",
		"gardenio-viewers":              "gardenio-viewer",
		"gardenio-mentioners":           "gardenio-mention",
		"gardenio-escalation-approvers": "escalation-approver",
		"project-claimlayer":            projectMemberRole,
		// TEST FIXTURE for the Task 12 live check (gd-llFKMS8Pkc); removed
		// with the overlay entry in Task 12 step 8.
		"project-scopetest": projectMemberRole,
		// Lands tenants (Track B 7.8): the viewer edge admits this group.
		"lands-tenants": projectMemberRole,
		// Sign-in invites (gd-ycE484kLyk): admitted to /lands/, no role.
		"invited": "",
	}
	for name, role := range want {
		roles, ok := got[name]
		if !ok {
			t.Errorf("deploy-values keycloak.groups missing %s", name)
			continue
		}
		if role == "" && len(roles) != 0 {
			t.Errorf("%s realmRoles = %v, want none", name, roles)
		} else if role != "" && (len(roles) != 1 || roles[0] != role) {
			t.Errorf("%s realmRoles = %v, want [%s]", name, roles, role)
		}
	}
	if len(got) != len(want) {
		t.Errorf("deploy-values keycloak.groups has %d groups, want %d: %v", len(got), len(want), got)
	}
}

// The prod overlay renders the project groups and does not declare the
// teammate, who signs in with GitHub through a sign-in invite.
func TestKeycloakProjectGroups_ProdOverlay(t *testing.T) {
	t.Parallel()
	overlay := deployValuesK3s(t)
	render := func(tpl string) string {
		out, err := exec.Command("helm", "template", "gardenio", chartDir(t),
			"--show-only", tpl, "-f", overlay).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template %s with prod overlay failed: %v\n%s", tpl, err, out)
		}
		return string(out)
	}
	groups := render(groupsTpl)
	// project-scopetest is the Task 12 live-check fixture (gd-llFKMS8Pkc).
	if p := projectGroupsOf(decodeDocs[renderedGroup](t, groups)); len(p) != 2 || p[0].Spec.Name != "project-claimlayer" || p[1].Spec.Name != "project-scopetest" {
		t.Errorf("prod overlay project groups = %+v, want [project-claimlayer project-scopetest]", p)
	}
	users := render(realmUsersTpl)
	// The teammate signs in with GitHub through a sign-in invite (decision
	// gd-Iyj3cmi7lJ): a declared realm user would hold his email, and the GitHub
	// first-login flow refuses an email that already has an account.
	if strings.Contains(users, `akashdixit@gmail.com`) {
		t.Errorf("prod overlay must not declare the teammate as a realm user:\n%s", users)
	}
	// The default-role mapping must not be re-added by the overlay either.
	roles := render(rolesTpl)
	for _, r := range decodeDocs[renderedRole](t, roles) {
		if r.Spec.IsDefault {
			t.Errorf("prod overlay role %s renders isDefault: true", r.Spec.Name)
		}
	}
}
