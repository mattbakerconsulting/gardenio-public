package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Machine principals (gd-s7aPIf0Bgp, design gardenio/docs/machine-principal-design.md).
//
// A platform component authenticates to kbeads as the Keycloak SERVICE ACCOUNT
// of its own confidential client. This file is the chart/Go cross-check of
// design §4.1 for the k3s overlay (deploy-values/gardenio-k3s.yaml):
//
//   - every machine client is confidential, service-account only, carries the
//     kbeads audience and a multivalued realm_roles mapper;
//   - BEADS_MACHINE_CLIENTS on the beads Deployment is exactly the set of
//     service-account clients (a client missing there takes the HUMAN path);
//   - every gardenio-machine-* role a client holds exists both as a
//     KeycloakRealmRole and in kbeads' reviewed grants table, and every
//     granted role is held by someone;
//   - the consuming Deployment dials the in-cluster token endpoint, mounts the
//     secret as an optional file, and never puts it in env.
//
// Adding a component (bridge, viewer, trigger-engine): add ONE row to
// machineClientRows and the matching deploy-values / values entries. The
// assertions are table-driven over the rows.

// machineClientRow is one machine principal the k3s overlay registers.
type machineClientRow struct {
	ClientID string
	// MachineRoles are the gardenio-machine-* capability roles the client's
	// service account must hold — exactly these, no more. Nil for the canary.
	MachineRoles []string
	// NoOperator pins that the service account does not hold
	// gardenio-operator: its machine roles are all it needs.
	NoOperator bool
	// Consumer describes the Deployment that authenticates as this client.
	// Zero for a client no component consumes (the canary).
	Consumer *machineConsumer
}

type machineConsumer struct {
	// Template is the Deployment template that renders BEADS_OIDC_*.
	Template string
	// EnableKey is the values key that turns the principal on/off.
	EnableKey string
	// SecretName is the operator-created Secret holding key clientSecret.
	SecretName string
	// ContainerName is the container carrying the env; "" = first container.
	ContainerName string
	// StaticTokenEnv is the env var carrying the shared static daemon token
	// the consumer keeps for everything that is not a machine write;
	// "" = BEADS_DAEMON_TOKEN.
	StaticTokenEnv string
	// NoStaticToken marks a consumer that holds no static daemon token at all,
	// principal on or off (beads-viewer, gd-j9uB0cduHA).
	NoStaticToken bool
	// ChartDefaultSet are the --set flags that make the Deployment render at
	// all with chart defaults and no overlay (the component's own enable
	// switch); nil when it renders by default.
	ChartDefaultSet []string
	// OverlaySet are the --set flags the k3s overlay render needs because the
	// overlay does not itself enable the component (github-bridge); nil when
	// the overlay renders the Deployment.
	OverlaySet []string
}

func (c *machineConsumer) staticTokenEnv() string {
	if c.StaticTokenEnv != "" {
		return c.StaticTokenEnv
	}
	return "BEADS_DAEMON_TOKEN"
}

var machineClientRows = []machineClientRow{
	{
		ClientID:     "gardenio-controller",
		MachineRoles: []string{"gardenio-machine-accounts", "gardenio-machine-token-mint", "gardenio-machine-controller-ownership", "gardenio-machine-controller-config-seed", "gardenio-machine-lands-provisioner", "gardenio-machine-release-deploy"},
		Consumer: &machineConsumer{
			Template:        "templates/controller/deployment.yaml",
			EnableKey:       "agents.machinePrincipal.enabled",
			SecretName:      "gardenio-machine-gardenio-controller",
			ChartDefaultSet: []string{"agents.enabled=true"},
		},
	},
	{
		// coopmux (gd-Wcu0DOkHfN, inventory item 8): one account-bead write
		// (status=exhausted on failover), Rust token source mirroring
		// beadsapi/machine_token.go. No token minting.
		ClientID:     "gardenio-coopmux",
		MachineRoles: []string{"gardenio-machine-accounts"},
		Consumer: &machineConsumer{
			Template:        "templates/coopmux/deployment.yaml",
			EnableKey:       "coopmux.machinePrincipal.enabled",
			SecretName:      "gardenio-machine-gardenio-coopmux",
			StaticTokenEnv:  "BEADS_TOKEN",
			ChartDefaultSet: []string{"coopmux.enabled=true"},
		},
	},
	{
		// beads-viewer reactors (gd-Hc34EENbO9): typed ownership of plan
		// epics, and the Lands Gate's create/update of plot_application
		// (step 7.7); no relay.
		ClientID:     "gardenio-beads-viewer",
		MachineRoles: []string{"gardenio-machine-lands-gate", "gardenio-machine-plan-reactor"},
		Consumer: &machineConsumer{
			Template:   "templates/beads-viewer/deployment.yaml",
			EnableKey:  "beadsViewer.machinePrincipal.enabled",
			SecretName: "gardenio-machine-gardenio-beads-viewer",
			// No rollback path to the shared token (gd-j9uB0cduHA).
			NoStaticToken: true,
			// beadsViewer.enabled defaults false; the chart-default control
			// needs the Deployment to render at all.
			ChartDefaultSet: []string{"beadsViewer.enabled=true"},
		},
	},
	{
		// grafana-bridge (gd-Hc34EENbO9): config seeding + typed ownership of the
		// dashboard/doc beads it syncs; every write is machine-origin.
		ClientID:     "gardenio-grafana-bridge",
		MachineRoles: []string{"gardenio-machine-config-seed", "gardenio-machine-dashboard-sync"},
		Consumer: &machineConsumer{
			Template:   "templates/grafana-bridge/deployment.yaml",
			EnableKey:  "grafanaBridge.machinePrincipal.enabled",
			SecretName: "gardenio-machine-gardenio-grafana-bridge",
			// The chart ships grafanaBridge.enabled=false; the control needs the
			// deployment to render at all.
			ChartDefaultSet: []string{"grafanaBridge.enabled=true"},
		},
	},
	{
		// trigger-engine (gd-Hc34EENbO9 component 2): typed ownership of trigger
		// beads and the run work items it files/sweeps, update of its rollup doc,
		// and the trigger_lineage attribution for its spawns; no admin-gated write.
		ClientID:     "gardenio-trigger-engine",
		MachineRoles: []string{"gardenio-machine-trigger-ops"},
		Consumer: &machineConsumer{
			Template:   "templates/trigger-engine/deployment.yaml",
			EnableKey:  "triggerEngine.machinePrincipal.enabled",
			SecretName: "gardenio-machine-gardenio-trigger-engine",
			// The chart ships triggerEngine.enabled=false; the control needs the
			// deployment to render at all.
			ChartDefaultSet: []string{"triggerEngine.enabled=true"},
		},
	},
	{
		// slack-bridge (gd-Hc34EENbO9 component 4): its own background writes only
		// (config seed typed to the titles it owns; dm delivery = ownership typed
		// to dm). No relay: human Slack writes ride the person's own token.
		ClientID:     "gardenio-slack-bridge",
		MachineRoles: []string{"gardenio-machine-slack-config-seed", "gardenio-machine-slack-dm-delivery"},
		Consumer: &machineConsumer{
			Template:   "templates/slack-bridge/deployment.yaml",
			EnableKey:  "slackBridge.machinePrincipal.enabled",
			SecretName: "gardenio-machine-gardenio-slack-bridge",
			// The chart ships slackBridge.enabled=false; the control needs the
			// deployment to render at all.
			ChartDefaultSet: []string{"slackBridge.enabled=true"},
		},
	},
	{
		// github-bridge release recorder (gd-305kriuXdT, plan gd-UZKZ9H25UD
		// step 3b): create/update of release beads and typed ownership of
		// them; github-bridge (gd-tCMXlS9fXW): MR-bead ownership and the PR
		// bookkeeping fields on work beads. No gardenio-operator (ruling
		// gd-lbWcuKYYtp): a machine principal is not roleBearing, so the
		// RoleOperator route gate admits it and the release writer check
		// decides (TestMachineGrants_ReleaseRecorder_HTTP).
		ClientID:     "gardenio-github-bridge",
		MachineRoles: []string{"gardenio-machine-release-recorder", "gardenio-machine-github-bridge"},
		NoOperator:   true,
		Consumer: &machineConsumer{
			Template:   "templates/github-bridge/deployment.yaml",
			EnableKey:  "githubBridge.machinePrincipal.enabled",
			SecretName: "gardenio-machine-gardenio-github-bridge",
			// The chart ships githubBridge.enabled=false and the k3s overlay
			// does not set it; both renders need the Deployment to exist.
			ChartDefaultSet: []string{"githubBridge.enabled=true"},
			OverlaySet:      []string{"githubBridge.enabled=true"},
		},
	},
	{
		// release-deployer (gd-5QYwXtV7IA): a CronJob, not a Deployment, so no
		// Consumer here (TestReleaseDeployer_* covers its wiring). Its daemon
		// calls are best-effort bead creates and the deployment on the release
		// (release-deploy.sh rule 9, plan gd-UZKZ9H25UD step 4).
		ClientID:     "gardenio-release-deployer",
		MachineRoles: []string{"gardenio-machine-release-deploy"},
	},
	{
		// laptop-operator (escalation routing T13, gd-WxmVN15PQe): the laptop
		// agent session, acting in the coordinator:laptop lane
		// (BEADS_ESCALATION_COORDINATOR_CLIENTS). Off-cluster, so no Consumer;
		// gardenio-operator only, no capability role.
		ClientID: "gardenio-laptop-operator",
	},
	{
		// cluster-coordinator (escalation routing Task 15 C5, gd-yT5xAPB0uJ):
		// the coordinator:cluster lane, executing approved bead-only
		// escalations. Its pod consumer is the machine_client mount (Task 15
		// Step 2), not a Deployment, so no Consumer here.
		ClientID:     "gardenio-cluster-coordinator",
		MachineRoles: []string{"gardenio-machine-escalation-executor", "gardenio-machine-release-deploy"},
	},
	{
		// No capability role: exists so the wrong-principal 403 can be shown
		// live (Task 7, gd-eiCV6zrLUS).
		ClientID: "gardenio-authz-canary",
	},
}

// nonKbeadsServiceAccountClients are k3s service-account clients that never
// call kbeads, so they are deliberately NOT machine principals: absent from
// BEADS_MACHINE_CLIENTS and from machineClientRows, and holding no
// gardenio-machine-* role. Each entry names why.
var nonKbeadsServiceAccountClients = map[string]string{
	// Lands provisioner (Track B 8.5, gd-8LjF2lzb4q): a Keycloak admin-API
	// credential for project-* groups and membership only.
	"gardenio-lands-provisioner": "Keycloak admin API only (internal/keycloakadmin)",
	// Role-drift probe CronJob (gd-L2MyfEe9lL): reads default-roles-<realm>
	// composites with realm-management view-realm.
	"gardenio-role-drift-probe": "Keycloak admin API only (files/keycloak/role-drift-probe.sh)",
}

const (
	machineGrantsGolden   = "../../../../kbeads/internal/server/testdata/machine_grants.golden.json"
	machineRolePrefix     = "gardenio-machine-"
	machineSecretMountDir = "/var/run/secrets/gardenio-machine"
	tokenEndpointPath     = "/protocol/openid-connect/token"
)

// --- rendering + decoding -------------------------------------------------

// helmRenderFiles renders showOnly with the given values files (paths as
// given: absolute or relative to the test cwd) and --set flags.
func helmRenderFiles(t *testing.T, showOnly string, valuesFiles []string, setFlags ...string) string {
	t.Helper()
	args := []string{"template", "test", chartDir(t), "--namespace", "gardenio", "--show-only", showOnly}
	for _, f := range valuesFiles {
		args = append(args, "-f", f)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "could not find template") {
			return ""
		}
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

func k3sOverlayPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(chartDir(t), k3sOverlay)
}

// yamlDocs splits a rendered manifest stream into non-empty documents.
func yamlDocs(out string) []string {
	var docs []string
	for _, d := range strings.Split(out, "\n---") {
		d = strings.TrimSpace(d)
		if d == "" || strings.HasPrefix(d, "---") && strings.TrimSpace(strings.TrimPrefix(d, "---")) == "" {
			continue
		}
		docs = append(docs, strings.TrimPrefix(d, "---\n"))
	}
	return docs
}

type kcProtocolMapper struct {
	Name           string            `json:"name"`
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config"`
}

type kcClient struct {
	Kind string `json:"kind"`
	Spec struct {
		ClientID            string `json:"clientId"`
		Public              bool   `json:"public"`
		StandardFlowEnabled bool   `json:"standardFlowEnabled"`
		DirectAccess        bool   `json:"directAccess"`
		Secret              string `json:"secret"`
		Attributes          map[string]string
		ServiceAccount      struct {
			Enabled    bool     `json:"enabled"`
			RealmRoles []string `json:"realmRoles"`
		} `json:"serviceAccount"`
		ProtocolMappers []kcProtocolMapper `json:"protocolMappers"`
	} `json:"spec"`
}

func renderKeycloakClients(t *testing.T, valuesFiles []string) map[string]kcClient {
	t.Helper()
	out := helmRenderFiles(t, "templates/keycloak/clients.yaml", valuesFiles)
	clients := map[string]kcClient{}
	for _, doc := range yamlDocs(out) {
		var c kcClient
		if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
			t.Fatalf("decode KeycloakClient: %v\n%s", err, doc)
		}
		if c.Kind == "KeycloakClient" {
			clients[c.Spec.ClientID] = c
		}
	}
	return clients
}

func renderKeycloakRoleNames(t *testing.T, valuesFiles []string) map[string]bool {
	t.Helper()
	out := helmRenderFiles(t, "templates/keycloak/roles.yaml", valuesFiles)
	roles := map[string]bool{}
	for _, doc := range yamlDocs(out) {
		var r struct {
			Kind string `json:"kind"`
			Spec struct {
				Name string `json:"name"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatalf("decode KeycloakRealmRole: %v\n%s", err, doc)
		}
		if r.Kind == "KeycloakRealmRole" {
			roles[r.Spec.Name] = true
		}
	}
	return roles
}

type k8sEnvVar struct {
	Name      string         `json:"name"`
	Value     string         `json:"value"`
	ValueFrom map[string]any `json:"valueFrom"`
}

type k8sDeployment struct {
	Kind string `json:"kind"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name         string      `json:"name"`
					Env          []k8sEnvVar `json:"env"`
					VolumeMounts []struct {
						Name      string `json:"name"`
						MountPath string `json:"mountPath"`
						ReadOnly  bool   `json:"readOnly"`
					} `json:"volumeMounts"`
				} `json:"containers"`
				Volumes []struct {
					Name   string `json:"name"`
					Secret *struct {
						SecretName string `json:"secretName"`
						Optional   *bool  `json:"optional"`
					} `json:"secret"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func renderDeployment(t *testing.T, template string, valuesFiles []string, setFlags ...string) k8sDeployment {
	t.Helper()
	out := helmRenderFiles(t, template, valuesFiles, setFlags...)
	for _, doc := range yamlDocs(out) {
		var d k8sDeployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatalf("decode %s: %v\n%s", template, err, doc)
		}
		if d.Kind == "Deployment" {
			return d
		}
	}
	t.Fatalf("no Deployment in %s:\n%s", template, out)
	return k8sDeployment{}
}

func (d k8sDeployment) env(container, name string) (k8sEnvVar, bool) {
	for i, c := range d.Spec.Template.Spec.Containers {
		if container != "" && c.Name != container || container == "" && i != 0 {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e, true
			}
		}
	}
	return k8sEnvVar{}, false
}

func loadMachineGrants(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(machineGrantsGolden)
	if err != nil {
		t.Fatalf("read kbeads grants golden: %v", err)
	}
	var g map[string]json.RawMessage
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("decode kbeads grants golden: %v", err)
	}
	return g
}

func machineRolesOf(c kcClient) []string {
	var out []string
	for _, r := range c.Spec.ServiceAccount.RealmRoles {
		if strings.HasPrefix(r, machineRolePrefix) {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// checkMachineClientShape returns every way c fails the machine-client
// contract (assertion 1). Returned, not asserted, so the mutated-values control
// can prove the check bites.
func checkMachineClientShape(c kcClient, audience string) []string {
	var problems []string
	if !c.Spec.ServiceAccount.Enabled {
		problems = append(problems, "serviceAccount.enabled is not true")
	}
	if c.Spec.Public {
		problems = append(problems, "public must be false")
	}
	if c.Spec.StandardFlowEnabled {
		problems = append(problems, "standardFlowEnabled must be false (no browser flow)")
	}
	if c.Spec.DirectAccess {
		problems = append(problems, "directAccess must be false (no password grant)")
	}
	if !strings.HasPrefix(c.Spec.Secret, "$") || !strings.Contains(c.Spec.Secret, ":") {
		problems = append(problems, fmt.Sprintf("secret %q must be an EDP $<secret>:<key> reference, never a literal", c.Spec.Secret))
	}
	var audOK, rolesOK bool
	for _, m := range c.Spec.ProtocolMappers {
		switch m.ProtocolMapper {
		case "oidc-audience-mapper":
			if m.Config["included.client.audience"] == audience && m.Config["access.token.claim"] == "true" {
				audOK = true
			}
		case "oidc-usermodel-realm-role-mapper":
			if m.Config["claim.name"] == "realm_roles" && m.Config["multivalued"] == "true" && m.Config["access.token.claim"] == "true" {
				rolesOK = true
			}
		}
	}
	if !audOK {
		problems = append(problems, fmt.Sprintf("no audience mapper for %q in the access token", audience))
	}
	if !rolesOK {
		problems = append(problems, "no multivalued realm_roles mapper in the access token")
	}
	return problems
}

// --- tests ------------------------------------------------------------------

// Assertions 1 + 3 (per row): client shape, and exactly the expected
// gardenio-machine-* roles, each of which is a rendered KeycloakRealmRole and a
// kbeads grant.
func TestMachinePrincipal_K3sClients(t *testing.T) {
	t.Parallel()
	overlay := []string{k3sOverlayPath(t)}
	clients := renderKeycloakClients(t, overlay)
	roles := renderKeycloakRoleNames(t, overlay)
	grants := loadMachineGrants(t)
	beads := renderDeployment(t, "templates/beads/deployment.yaml", overlay)
	aud, ok := beads.env("", "BEADS_OIDC_AUDIENCE")
	if !ok || aud.Value == "" {
		t.Fatalf("beads Deployment must render BEADS_OIDC_AUDIENCE")
	}

	for _, row := range machineClientRows {
		t.Run(row.ClientID, func(t *testing.T) {
			c, ok := clients[row.ClientID]
			if !ok {
				t.Fatalf("KeycloakClient %q not rendered; have %v", row.ClientID, clientIDs(clients))
			}
			for _, p := range checkMachineClientShape(c, aud.Value) {
				t.Error(p)
			}
			want := append([]string(nil), row.MachineRoles...)
			sort.Strings(want)
			if got := machineRolesOf(c); !slices.Equal(got, want) {
				t.Errorf("machine roles = %v, want %v", got, want)
			}
			if row.NoOperator && slices.Contains(c.Spec.ServiceAccount.RealmRoles, "gardenio-operator") {
				t.Errorf("realm roles = %v, want no gardenio-operator", c.Spec.ServiceAccount.RealmRoles)
			}
			for _, r := range machineRolesOf(c) {
				if !roles[r] {
					t.Errorf("role %q is assigned but not in keycloak.roles (no KeycloakRealmRole rendered)", r)
				}
				if _, ok := grants[r]; !ok {
					t.Errorf("role %q is assigned but kbeads grants nothing for it (%s)", r, machineGrantsGolden)
				}
			}
			if c.Spec.Secret != "$gardenio-machine-"+row.ClientID+":clientSecret" {
				t.Errorf("secret ref = %q, want $gardenio-machine-%s:clientSecret (the OPERATOR step creates that Secret)", c.Spec.Secret, row.ClientID)
			}
		})
	}
}

// Assertion 2: BEADS_MACHINE_CLIENTS == the set of service-account clients,
// less the named nonKbeadsServiceAccountClients.
func TestMachinePrincipal_K3sRegistryMatchesServiceAccountClients(t *testing.T) {
	t.Parallel()
	overlay := []string{k3sOverlayPath(t)}
	clients := renderKeycloakClients(t, overlay)
	var want []string
	for id, c := range clients {
		if !c.Spec.ServiceAccount.Enabled {
			continue
		}
		if _, ok := nonKbeadsServiceAccountClients[id]; ok {
			if roles := machineRolesOf(c); len(roles) > 0 {
				t.Errorf("%s is a non-kbeads service account but holds machine roles %v", id, roles)
			}
			continue
		}
		want = append(want, id)
	}
	sort.Strings(want)
	for id := range nonKbeadsServiceAccountClients {
		if c, ok := clients[id]; !ok || !c.Spec.ServiceAccount.Enabled {
			t.Errorf("nonKbeadsServiceAccountClients lists %s, which is not a rendered serviceAccount client: drop the entry", id)
		}
	}
	beads := renderDeployment(t, "templates/beads/deployment.yaml", overlay)
	reg, ok := beads.env("", "BEADS_MACHINE_CLIENTS")
	if !ok {
		t.Fatalf("beads Deployment must render BEADS_MACHINE_CLIENTS for %v", want)
	}
	got := strings.Split(reg.Value, ",")
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("BEADS_MACHINE_CLIENTS = %v, want every serviceAccount client %v (an unlisted client takes the HUMAN path)", got, want)
	}
	// Every row is a service-account client, and vice versa: a new service
	// account client needs a row here.
	var rows []string
	for _, r := range machineClientRows {
		rows = append(rows, r.ClientID)
	}
	sort.Strings(rows)
	if !slices.Equal(rows, want) {
		t.Errorf("machineClientRows %v must list exactly the serviceAccount clients %v", rows, want)
	}
}

// Assertion 3 (global half): every kbeads grant is held by at least one client.
func TestMachinePrincipal_EveryGrantIsAssigned(t *testing.T) {
	t.Parallel()
	clients := renderKeycloakClients(t, []string{k3sOverlayPath(t)})
	held := map[string]bool{}
	for _, c := range clients {
		for _, r := range machineRolesOf(c) {
			held[r] = true
		}
	}
	for r := range loadMachineGrants(t) {
		if !held[r] {
			t.Errorf("kbeads grants %q but no k3s client's service account holds it", r)
		}
	}
	roles := renderKeycloakRoleNames(t, []string{k3sOverlayPath(t)})
	for r := range loadMachineGrants(t) {
		if !roles[r] {
			t.Errorf("kbeads grants %q but keycloak.roles renders no such KeycloakRealmRole", r)
		}
	}
}

// Assertions 4 + 5 (per consumer): in-cluster token URL, optional secret
// volume mounted read-only, secret never in env.
func TestMachinePrincipal_K3sConsumers(t *testing.T) {
	t.Parallel()
	overlay := []string{k3sOverlayPath(t)}
	for _, row := range machineClientRows {
		if row.Consumer == nil {
			continue
		}
		t.Run(row.ClientID, func(t *testing.T) {
			// Force the principal on: a consumer can ship enabled: false in
			// deploy-values for its first roll (the KeycloakClient and roles
			// reconcile before the pod is pointed at them; slack-bridge,
			// gd-Hc34EENbO9), and this test is about the wiring it renders when on.
			d := renderDeployment(t, row.Consumer.Template, overlay, append([]string{row.Consumer.EnableKey + "=true"}, row.Consumer.OverlaySet...)...)
			cn := row.Consumer.ContainerName

			url, ok := d.env(cn, "BEADS_OIDC_TOKEN_URL")
			if !ok {
				t.Fatalf("%s must render BEADS_OIDC_TOKEN_URL", row.Consumer.Template)
			}
			// The k3s overlay serves Keycloak's in-cluster https port and moves
			// its clients onto it (keycloak.runtime.tls, gd-lYPZlslOM7).
			switch {
			case !strings.HasPrefix(url.Value, "https://"):
				t.Errorf("token URL %q must dial the in-cluster Service over https", url.Value)
			case !strings.Contains(url.Value, ".svc:8443/"):
				t.Errorf("token URL %q must be the Keycloak Service (<svc>.<ns>.svc:8443)", url.Value)
			case !strings.HasSuffix(url.Value, tokenEndpointPath):
				t.Errorf("token URL %q must end in %s", url.Value, tokenEndpointPath)
			}
			for _, bad := range []string{"auth.clonk.tv", ".ziti"} {
				if strings.Contains(url.Value, bad) {
					t.Errorf("token URL %q must never go through %s", url.Value, bad)
				}
			}
			if id, ok := d.env(cn, "BEADS_OIDC_CLIENT_ID"); !ok || id.Value != row.ClientID {
				t.Errorf("BEADS_OIDC_CLIENT_ID = %q, want %q", id.Value, row.ClientID)
			}
			file, ok := d.env(cn, "BEADS_OIDC_CLIENT_SECRET_FILE")
			if !ok || !strings.HasPrefix(file.Value, machineSecretMountDir+"/") {
				t.Errorf("BEADS_OIDC_CLIENT_SECRET_FILE = %q, want a path under %s", file.Value, machineSecretMountDir)
			}
			if ca, ok := d.env(cn, "BEADS_OIDC_CA_FILE"); !ok || ca.Value != kcClientsCAFile {
				t.Errorf("BEADS_OIDC_CA_FILE = %q, want %s (the https token URL's CA)", ca.Value, kcClientsCAFile)
			}

			// The secret is a file, never an env var (literal or secretKeyRef).
			for _, c := range d.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					if ref, ok := e.ValueFrom["secretKeyRef"].(map[string]any); ok && ref["name"] == row.Consumer.SecretName {
						t.Errorf("env %s reads the machine client secret via secretKeyRef; it must be a file", e.Name)
					}
					if strings.Contains(e.Value, "$gardenio-machine") {
						t.Errorf("env %s carries a secret reference %q", e.Name, e.Value)
					}
				}
			}

			var vol, mount bool
			for _, v := range d.Spec.Template.Spec.Volumes {
				if v.Secret != nil && v.Secret.SecretName == row.Consumer.SecretName {
					vol = true
					if v.Secret.Optional == nil || !*v.Secret.Optional {
						t.Errorf("volume %s must be optional: true so a missing Secret still starts the pod", v.Name)
					}
					for _, c := range d.Spec.Template.Spec.Containers {
						for _, m := range c.VolumeMounts {
							if m.Name == v.Name {
								mount = true
								if m.MountPath != machineSecretMountDir || !m.ReadOnly {
									t.Errorf("mount %s = %q readOnly=%v, want %s read-only", m.Name, m.MountPath, m.ReadOnly, machineSecretMountDir)
								}
							}
						}
					}
				}
			}
			if !vol {
				t.Errorf("no Secret volume for %s", row.Consumer.SecretName)
			}
			if !mount {
				t.Errorf("Secret %s is not mounted into a container", row.Consumer.SecretName)
			}
		})
	}
}

// Assertion 6a (control): with the principal off and the registry empty,
// nothing above renders.
func TestMachinePrincipal_DisabledRendersNothing(t *testing.T) {
	t.Parallel()
	overlay := []string{k3sOverlayPath(t)}
	var off []string
	for _, row := range machineClientRows {
		if row.Consumer != nil {
			off = append(off, row.Consumer.EnableKey+"=false")
		}
	}
	// The Lands Gate files applications only as the viewer's machine principal, so it goes off with it.
	off = append(off, "beads.machineClients=null", "beadsViewer.landsApply.enabled=false")

	beads := renderDeployment(t, "templates/beads/deployment.yaml", overlay, off...)
	if e, ok := beads.env("", "BEADS_MACHINE_CLIENTS"); ok {
		t.Errorf("BEADS_MACHINE_CLIENTS=%q rendered with an empty registry", e.Value)
	}
	for _, row := range machineClientRows {
		if row.Consumer == nil {
			continue
		}
		d := renderDeployment(t, row.Consumer.Template, overlay, append(off, row.Consumer.OverlaySet...)...)
		for _, name := range []string{"BEADS_OIDC_TOKEN_URL", "BEADS_OIDC_CLIENT_ID", "BEADS_OIDC_CLIENT_SECRET_FILE"} {
			if _, ok := d.env(row.Consumer.ContainerName, name); ok {
				t.Errorf("%s renders %s with %s=false", row.Consumer.Template, name, row.Consumer.EnableKey)
			}
		}
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == row.Consumer.SecretName {
				t.Errorf("%s renders the %s volume with %s=false", row.Consumer.Template, v.Name, row.Consumer.EnableKey)
			}
		}
		// The static daemon token is untouched either way, except for a
		// consumer that holds none.
		_, ok := d.env(row.Consumer.ContainerName, row.Consumer.staticTokenEnv())
		if row.Consumer.NoStaticToken && ok {
			t.Errorf("%s renders %s with %s=false; it holds no static token", row.Consumer.Template, row.Consumer.staticTokenEnv(), row.Consumer.EnableKey)
		} else if !row.Consumer.NoStaticToken && !ok {
			t.Errorf("%s must keep %s", row.Consumer.Template, row.Consumer.staticTokenEnv())
		}
	}

	// Chart defaults alone: nothing either.
	if out := helmRenderFiles(t, "templates/beads/deployment.yaml", nil); strings.Contains(out, "BEADS_MACHINE_CLIENTS") {
		t.Errorf("chart default renders BEADS_MACHINE_CLIENTS")
	}
	for _, row := range machineClientRows {
		if row.Consumer == nil {
			continue
		}
		out := helmRenderFiles(t, row.Consumer.Template, nil, row.Consumer.ChartDefaultSet...)
		if out == "" {
			t.Errorf("%s renders nothing with chart defaults + %v; the control proves nothing", row.Consumer.Template, row.Consumer.ChartDefaultSet)
		}
		if strings.Contains(out, "BEADS_OIDC_TOKEN_URL") || strings.Contains(out, "gardenio-machine") {
			t.Errorf("%s renders machine-principal env/volume with chart defaults", row.Consumer.Template)
		}
	}
}

// Assertion 6b (control): a client missing the audience mapper fails the
// shape check — the test actually bites.
func TestMachinePrincipal_MissingAudienceMapperIsCaught(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(k3sOverlayPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("decode overlay: %v", err)
	}
	kc := values["keycloak"].(map[string]any)
	clients := kc["clients"].([]any)
	mutated := false
	for _, c := range clients {
		cm := c.(map[string]any)
		if cm["clientId"] != machineClientRows[0].ClientID {
			continue
		}
		var kept []any
		for _, m := range cm["protocolMappers"].([]any) {
			if m.(map[string]any)["protocolMapper"] == "oidc-audience-mapper" {
				mutated = true
				continue
			}
			kept = append(kept, m)
		}
		cm["protocolMappers"] = kept
	}
	if !mutated {
		t.Fatalf("overlay has no audience mapper on %s to remove", machineClientRows[0].ClientID)
	}
	out, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	mutatedFile := filepath.Join(t.TempDir(), "mutated.yaml")
	if err := os.WriteFile(mutatedFile, out, 0o600); err != nil {
		t.Fatal(err)
	}

	rendered := renderKeycloakClients(t, []string{mutatedFile})
	c, ok := rendered[machineClientRows[0].ClientID]
	if !ok {
		t.Fatalf("mutated render lost client %s", machineClientRows[0].ClientID)
	}
	beads := renderDeployment(t, "templates/beads/deployment.yaml", []string{mutatedFile})
	aud, _ := beads.env("", "BEADS_OIDC_AUDIENCE")
	problems := checkMachineClientShape(c, aud.Value)
	if len(problems) == 0 {
		t.Fatalf("shape check must fail without the audience mapper")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "audience") {
		t.Errorf("problems %v do not name the audience mapper", problems)
	}
	// And the unmutated overlay passes it (so the failure is the mutation).
	if p := checkMachineClientShape(renderKeycloakClients(t, []string{k3sOverlayPath(t)})[machineClientRows[0].ClientID], aud.Value); len(p) != 0 {
		t.Errorf("unmutated overlay fails the shape check: %v", p)
	}
}

func clientIDs(m map[string]kcClient) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
