package tests

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Lands controller wiring (gd-l1CTMNSH3A, follow-up to step 8.6 gd-G9ZfREbe8z;
// runbook gardenio/docs/lands/provisioner.md).
//
// Invariants locked here:
//   - Everything is off by default: no LANDS_* or MAILER_* env, no lands
//     volumes.
//   - Enabled: the Keycloak client secret and the Lands App key are mounted
//     read-only files; neither is ever an env var (no secretKeyRef to either
//     Secret).
//   - Reserved groups are rendered from keycloak.groups and
//     keycloak.projectGroups, never a hand-kept list.
//   - Enabled without the Keycloak URL, App id or account template fails the
//     render, and a non-https Keycloak URL fails it too (except the one
//     in-cluster http host, lands_keycloak_url_test.go).

const (
	landsProvOn = "agents.enabled=true,agents.lands.provisioner.enabled=true," +
		"agents.lands.provisioner.keycloak.url=https://auth.example.com/auth," +
		"agents.lands.provisioner.githubApp.appId=4242," +
		"agents.lands.provisioner.accountTemplate=gd-template"
	landsKCSecret  = "gardenio-lands-provisioner"
	landsAppSecret = "gardenio-lands-github-app"
)

// controllerPodSpec is the part of the controller Deployment these tests read.
type controllerPodSpec struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name string `yaml:"name"`
					Env  []struct {
						Name      string         `yaml:"name"`
						Value     string         `yaml:"value"`
						ValueFrom map[string]any `yaml:"valueFrom"`
					} `yaml:"env"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Name   string `yaml:"name"`
					Secret *struct {
						SecretName string `yaml:"secretName"`
						Optional   bool   `yaml:"optional"`
						Items      []struct {
							Key  string `yaml:"key"`
							Path string `yaml:"path"`
						} `yaml:"items"`
					} `yaml:"secret"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func renderControllerPodSpec(t *testing.T, flags ...string) (string, controllerPodSpec) {
	t.Helper()
	out := helmTemplate(t, "templates/controller/deployment.yaml", flags...)
	var d controllerPodSpec
	if err := yaml.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("parse controller Deployment: %v\n%s", err, out)
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("controller Deployment has no containers:\n%s", out)
	}
	return out, d
}

func TestLands_DefaultRendersNoEnvOrVolumes(t *testing.T) {
	t.Parallel()
	out, _ := renderControllerPodSpec(t, "agents.enabled=true")
	for _, forbidden := range []string{"LANDS_", "MAILER_", "lands-keycloak-client-secret", "lands-github-app-private-key", landsKCSecret, landsAppSecret} {
		if strings.Contains(out, forbidden) {
			t.Errorf("default controller Deployment must not reference %q", forbidden)
		}
	}
}

func TestLands_ProvisionerEnabledWiresSecretsAsReadOnlyFiles(t *testing.T) {
	t.Parallel()
	out, d := renderControllerPodSpec(t, landsProvOn,
		"keycloak.projectGroups[0].name=claimlayer",
		"agents.lands.provisioner.keycloak.extraReservedGroups[0]=edge-only")
	c := d.Spec.Template.Spec.Containers[0]

	env := map[string]string{}
	for _, e := range c.Env {
		if strings.HasPrefix(e.Name, "LANDS_") && e.ValueFrom != nil {
			t.Errorf("%s must be a literal value, never valueFrom", e.Name)
		}
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"LANDS_PROVISION_ENABLED":           "true",
		"LANDS_KEYCLOAK_URL":                "https://auth.example.com/auth",
		"LANDS_KEYCLOAK_REALM":              "test",
		"LANDS_KEYCLOAK_CLIENT_ID":          "gardenio-lands-provisioner",
		"LANDS_KEYCLOAK_CLIENT_SECRET_PATH": "/var/run/secrets/gardenio-lands-provisioner/clientSecret",
		"LANDS_GITHUB_APP_ID":               "4242",
		"LANDS_GITHUB_APP_PRIVATE_KEY_PATH": "/etc/gardenio/lands-github-app/private-key",
		"LANDS_ACCOUNT_TEMPLATE":            "gd-template",
	} {
		if got, ok := env[name]; !ok || got != want {
			t.Errorf("env %s = %q (set %v), want %q", name, got, ok, want)
		}
	}

	// Every chart group is reserved, plus the extra one.
	reserved := map[string]bool{}
	for g := range strings.SplitSeq(env["LANDS_KEYCLOAK_RESERVED_GROUPS"], ",") {
		reserved[g] = true
	}
	for _, g := range []string{"gardenio-admins", "gardenio-escalation-approvers", "project-claimlayer", "edge-only"} {
		if !reserved[g] {
			t.Errorf("LANDS_KEYCLOAK_RESERVED_GROUPS = %q, missing %q", env["LANDS_KEYCLOAK_RESERVED_GROUPS"], g)
		}
	}

	// No secret value or Secret reference rides env.
	for _, secret := range []string{landsKCSecret, landsAppSecret} {
		if strings.Contains(out, "secretKeyRef:\n                  name: "+secret) {
			t.Errorf("Secret %s must be a mounted file, never a secretKeyRef env", secret)
		}
	}

	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		if strings.HasPrefix(m.Name, "lands-") {
			if !m.ReadOnly {
				t.Errorf("lands mount %s must be readOnly", m.Name)
			}
			mounts[m.Name] = m.MountPath
		}
	}
	if mounts["lands-keycloak-client-secret"] != "/var/run/secrets/gardenio-lands-provisioner" ||
		mounts["lands-github-app-private-key"] != "/etc/gardenio/lands-github-app" {
		t.Errorf("lands mounts = %v", mounts)
	}

	vols := map[string]string{}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if !strings.HasPrefix(v.Name, "lands-") {
			continue
		}
		if v.Secret == nil || len(v.Secret.Items) != 1 {
			t.Errorf("lands volume %s must be a Secret projecting exactly one key", v.Name)
			continue
		}
		vols[v.Name] = v.Secret.SecretName + ":" + v.Secret.Items[0].Key
	}
	if vols["lands-keycloak-client-secret"] != landsKCSecret+":clientSecret" ||
		vols["lands-github-app-private-key"] != landsAppSecret+":private-key" {
		t.Errorf("lands volumes = %v", vols)
	}
}

func TestLands_ProvisionerRealmFollowsChartRealm(t *testing.T) {
	t.Parallel()
	_, d := renderControllerPodSpec(t, landsProvOn, "keycloak.realm.name=gardenio")
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "LANDS_KEYCLOAK_REALM" && e.Value != "gardenio" {
			t.Errorf("LANDS_KEYCLOAK_REALM = %q, want the chart realm gardenio", e.Value)
		}
	}
}

func TestLands_ProvisionerEnabledRequiresSettings(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set, want string
	}{
		"no keycloak url":       {"agents.lands.provisioner.keycloak.url=", "agents.lands.provisioner.keycloak.url is required"},
		"http keycloak url":     {"agents.lands.provisioner.keycloak.url=http://keycloak:8080", "must be an https URL (keycloakadmin refuses anything else)"},
		"no app id":             {"agents.lands.provisioner.githubApp.appId=", "agents.lands.provisioner.githubApp.appId is required"},
		"no account template":   {"agents.lands.provisioner.accountTemplate=", "agents.lands.provisioner.accountTemplate is required"},
		"mailer without from":   {"agents.mailer.enabled=true,agents.mailer.baseUrl=https://x.example", "agents.mailer.from is required"},
		"mailer without origin": {"agents.mailer.enabled=true,agents.mailer.from=lands@x.example", "agents.mailer.baseUrl is required"},
	} {
		t.Run(name, func(t *testing.T) {
			out := helmTemplateExpectError(t, "templates/controller/deployment.yaml", "--set", landsProvOn, "--set", tc.set)
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected %q, got:\n%s", tc.want, out)
			}
		})
	}
}

func TestLands_SpawnWallAndMailerEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true,agents.lands.tenantSpawns=true,agents.lands.spawnsPerHour=6,"+
			"agents.mailer.enabled=true,agents.mailer.from=lands@x.example,agents.mailer.baseUrl=https://x.example,"+
			"agents.mailer.sesRegion=us-east-1")
	for name, want := range map[string]string{
		"LANDS_TENANT_SPAWNS":   "true",
		"LANDS_SPAWNS_PER_HOUR": "6",
		"MAILER_ENABLED":        "true",
		"MAILER_FROM":           "lands@x.example",
		"MAILER_BASE_URL":       "https://x.example",
		"MAILER_SES_REGION":     "us-east-1",
	} {
		if got := envValue(t, out, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// The spawn wall and mailer alone do not turn the provisioner on.
	if strings.Contains(out, "LANDS_PROVISION_ENABLED") || strings.Contains(out, "lands-keycloak-client-secret") {
		t.Error("provisioner env/volumes must not render unless agents.lands.provisioner.enabled")
	}
}

// The trigger engine runs the gardener pre-fire spawn wall, so it needs the
// same switch as the controller or a tenant land's gardeners never fire.
func TestLands_TenantSpawnsReachTheTriggerEngine(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/trigger-engine/deployment.yaml", "triggerEngine.enabled=true,agents.lands.tenantSpawns=true")
	if got := envValue(t, out, "LANDS_TENANT_SPAWNS"); got != "true" {
		t.Errorf("trigger-engine LANDS_TENANT_SPAWNS = %q, want true", got)
	}
	off := helmTemplate(t, "templates/trigger-engine/deployment.yaml", "triggerEngine.enabled=true,agents.lands.tenantSpawns=false")
	if strings.Contains(off, "LANDS_TENANT_SPAWNS") {
		t.Error("trigger-engine must not render LANDS_TENANT_SPAWNS while tenant spawns are off")
	}
}

func TestLands_GvisorMirrorCannotCarryProvisionerSecrets(t *testing.T) {
	t.Parallel()
	for name, mirror := range map[string]string{
		"keycloak client secret": "gvisor.namespace.secretMirrors[0].name=" + landsKCSecret,
		"lands app key":          "gvisor.namespace.secretMirrors[0].name=" + landsAppSecret,
	} {
		t.Run(name, func(t *testing.T) {
			out := helmTemplateExpectError(t, "templates/gvisor-namespace.yaml",
				"--set", gvisorMirrorBase, "--set", mirror,
				"--set", "gvisor.namespace.secretMirrors[0].remoteRef=gardenio/x")
			if !strings.Contains(out, "must never be mirrored into the gVisor namespace") {
				t.Errorf("expected the exclusion guard to fire, got:\n%s", out)
			}
		})
	}
}

// The purge sweep (step 8.11) deletes data: it renders only when enabled, and
// the retentions render only when set, so empty keeps the binary defaults.
func TestLands_PurgeEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true,agents.lands.purge.enabled=true")
	if got := envValue(t, out, "LANDS_PURGE_ENABLED"); got != "true" {
		t.Errorf("LANDS_PURGE_ENABLED = %q, want true", got)
	}
	for _, unset := range []string{"LANDS_RETENTION_DAYS", "LANDS_RECORDS_RETENTION_DAYS", "LANDS_APPLICATION_RETENTION_DAYS", "LANDS_PROVISION_ENABLED"} {
		if strings.Contains(out, unset) {
			t.Errorf("%s must not render unless set", unset)
		}
	}

	out = helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true,agents.lands.purge.enabled=true,agents.lands.purge.retentionDays=30,"+
			"agents.lands.purge.recordsRetentionDays=730,agents.lands.purge.applicationRetentionDays=90")
	for name, want := range map[string]string{
		"LANDS_RETENTION_DAYS":             "30",
		"LANDS_RECORDS_RETENTION_DAYS":     "730",
		"LANDS_APPLICATION_RETENTION_DAYS": "90",
	} {
		if got := envValue(t, out, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	out = helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true,agents.lands.purge.retentionDays=30")
	if strings.Contains(out, "LANDS_") {
		t.Error("purge env must not render unless agents.lands.purge.enabled")
	}
}

// The sweep is a dry run unless dryRun is exactly false: the chart default,
// an explicit true and a false all render what the controller reads.
func TestLands_PurgeDryRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ set, want string }{
		{"", "true"},
		{",agents.lands.purge.dryRun=true", "true"},
		{",agents.lands.purge.dryRun=false", "false"},
	} {
		out := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true,agents.lands.purge.enabled=true"+tc.set)
		if got := envValue(t, out, "LANDS_PURGE_DRY_RUN"); got != tc.want {
			t.Errorf("set %q: LANDS_PURGE_DRY_RUN = %q, want %q", tc.set, got, tc.want)
		}
	}
	out := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true,agents.lands.purge.dryRun=false")
	if strings.Contains(out, "LANDS_PURGE_DRY_RUN") {
		t.Error("LANDS_PURGE_DRY_RUN must not render unless agents.lands.purge.enabled")
	}
}

// The controller holds a land's repo to the same size cap as the viewer's
// setup check, from the one beadsViewer.landsGithubApp.maxRepoSizeMB value
// (gd-95ksM47sDp); empty leaves the binary default on both.
func TestLands_ProvisionerMaxRepoSizeFollowsViewer(t *testing.T) {
	out, _ := renderControllerPodSpec(t, landsProvOn)
	if v, ok := envValueAfter(out, "LANDS_MAX_REPO_SIZE_MB"); ok {
		t.Errorf("controller LANDS_MAX_REPO_SIZE_MB = %q at the default; the binary default must hold", v)
	}
	for _, size := range []string{"0", "512"} {
		out, _ := renderControllerPodSpec(t, landsProvOn, "beadsViewer.landsGithubApp.maxRepoSizeMB="+size)
		if got, ok := envValueAfter(out, "LANDS_MAX_REPO_SIZE_MB"); !ok || got != size {
			t.Errorf("controller LANDS_MAX_REPO_SIZE_MB = %q (present=%v), want %q", got, ok, size)
		}
	}
	out, _ = renderControllerPodSpec(t, "agents.enabled=true", "beadsViewer.landsGithubApp.maxRepoSizeMB=512")
	if _, ok := envValueAfter(out, "LANDS_MAX_REPO_SIZE_MB"); ok {
		t.Errorf("controller LANDS_MAX_REPO_SIZE_MB rendered with the provisioner off")
	}
}
