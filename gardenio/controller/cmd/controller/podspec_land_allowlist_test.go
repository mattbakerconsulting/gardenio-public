package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// What a tenant pod holds (Lands Track B §5.3, step 10.3): a per-agent daemon
// token bound to its project; the land's LiteLLM key; a Lands App
// installation token for the one repo; its coop credentials; its land's own
// NATS user, when configured (gd-sS7bSXOdGs). The test renders
// a tenant agent's Pod through both spec builders and the pod manager, with a
// role, a project, agent pins and every controller setting trying to add more,
// and fails on any env var, volume or secret outside these lists.

// tenantAllowedEnv is every env name a tenant pod's containers may carry: the
// pod manager's own plumbing and identity, then what fenceTenantPodContents
// keeps. A name added to either needs adding here, on purpose.
var tenantAllowedEnv = map[string]bool{
	// podmanager: home, state dir, downward API, identity, attribution.
	"HOME": true, "XDG_STATE_HOME": true, "POD_IP": true, "POD_NAME": true,
	"BOAT_ROLE": true, "BOAT_PROJECT": true, "BOAT_AGENT": true, "BOAT_MODE": true,
	"BEADS_ACTOR": true, "KD_ACTOR": true, "KD_AGENT_ID": true, "GIT_AUTHOR_NAME": true,
	"BEADS_AGENT_NAME": true, "BOAT_AGENT_BEAD_ID": true, "DISABLE_AUTOUPDATER": true,
	"BOAT_TASK_ID": true, "BOAT_TRIGGER_BEAD_ID": true, "BEADS_OPERATED_BY": true,
	"BEADS_USER_BEAD_ID": true, "BOAT_SESSION_RESUME": true, "GOMEMLIMIT": true,
	"BOAT_RUNTIME_CLASS": true,
	// The per-agent daemon token (secret-sourced, checked below).
	"BEADS_DAEMON_TOKEN": true, "BEADS_AUTH_TOKEN": true, "BEADS_REFRESH_TOKEN": true,
	// Daemon and coop.
	"BEADS_GRPC_ADDR": true, "BEADS_HTTP_ADDR": true, "COOP_HOST": true,
	"COOP_BROKER_URL": true, "COOP_MUX_URL": true, "COOP_BROKER_TOKEN": true, "COOP_MUX_TOKEN": true,
	// The land's account.
	"BOAT_ACCOUNT": true, "ANTHROPIC_BASE_URL": true, "ANTHROPIC_CUSTOM_HEADERS": true,
	"ANTHROPIC_MODEL": true, "CLAUDE_MODEL": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	"ANTHROPIC_DEFAULT_FABLE_MODEL": true, "ANTHROPIC_DEFAULT_OPUS_MODEL": true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL": true, "BOAT_MODEL_PINNED": true, "GARDENIO_HARNESS": true,
	"CODEX_HOME": true, "CODEX_MODEL": true, "GEMINI_MODEL": true, "GEMINI_AUTH_MODE": true,
	"GEMINI_CLI_TRUST_WORKSPACE": true, "BOAT_ULTRACODE": true,
	"CLAUDE_TEAMS_ENABLED": true, "CLAUDE_TEAMMATE_MODE": true, "CLAUDE_TEAMS_MAX_TEAMMATES": true,
	// gb say and public links.
	"SLACK_BRIDGE_URL": true, "BEADS_VIEWER_URL": true, "GARDENIO_DEFAULT_HOST": true,
	"GARDENIO_CHANNEL": true,
	// The agent's turn.
	"BOAT_PROMPT": true, "BOAT_WAKE_REASON": true, "SLACK_THREAD_CHANNEL": true,
	"SLACK_THREAD_TS": true, "BOAT_AGENT_STATE": true, "BOAT_STANDBY": true, "BOAT_STANDBY_POLL": true,
}

const allowlistBead = "gd-tenant1"

// allowlistCfg is landFenceCfg with a land account, a fleet account a role
// pins, and a project and roles that try every way to add env, secrets and
// volumes. Everything that must not reach the pod has "fleet" in its name or
// value. fill sets every scalar controller setting, so a setting that injects
// something new is caught.
func allowlistCfg(fill bool) *config.Config {
	cfg := &config.Config{}
	if fill {
		fillConfig(reflect.ValueOf(cfg).Elem())
		// A configured allow-list would refuse the fixture's bead secrets
		// before the fence sees them.
		cfg.AgentSecretAllowlist = nil
		cfg.FreezeThawRoleAllowlist = nil
		cfg.LandsRuntimeClass = ""
	}
	base := landFenceCfg()
	cfg.Namespace, cfg.GVisorNamespace, cfg.FreezeThawEnabled = base.Namespace, base.GVisorNamespace, base.FreezeThawEnabled
	cfg.CoopmuxTokenSecret = "coopmux-fleet-token"
	cfg.CoopmuxURL = "http://coopmux:9800"
	cfg.CoopmuxLandTokenKeyFile = "/etc/coopmux-land/key"
	// Tenant pods reach the same server as their land's user
	// (fenceTenantNats), so its URL is not a fleet value.
	cfg.NatsURL = "nats://nats:4222"
	cfg.NatsAgentUserSecret = "nats-agent-user-fleet"
	cfg.NatsTokenSecret = "nats-fleet-shared"
	cfg.BeadsTokenSecret = "beads-fleet-static"
	cfg.SlackBridgeTokenSecret = "slack-bridge-fleet"
	cfg.JiraBridgeTokenSecret = "jira-bridge-fleet"
	cfg.ClaudeOAuthSecret = "claude-oauth-fleet"
	cfg.LandsAccountTemplate = "lands-template"
	cfg.CoopS3Endpoint, cfg.CoopS3CredentialsSecret = "http://minio:9000", "minio-agent-fleet"
	cfg.GrafanaEnvAPIKeySecret, cfg.GrafanaEnvList = "grafana-fleet", []string{"prod"}
	cfg.ImagePullSecrets = []string{"pull-fleet"}
	cfg.SlackViewSessionSecret, cfg.SlackViewSessionRoles = "slack-view-fleet", "plain,escape"
	cfg.AgentMachineClientMounts = []string{"fleet-coordinator"}

	secrets := []beadsapi.SecretEntry{
		{Env: "GIT_TOKEN", Secret: "acme-fleet-pat", Key: "token"},
		{Env: "GITLAB_TOKEN", Secret: "acme-fleet-gitlab", Key: "token"},
		{Env: "CLAUDE_CODE_OAUTH_TOKEN", Secret: "acme-fleet-oauth", Key: "token"},
		{Env: "AWS_SECRET_ACCESS_KEY", Secret: "acme-fleet-aws", Key: "key"},
	}
	env := []beadsapi.EnvEntry{{Name: "FLEET_PLAIN", Value: "fleet"}, {Name: "GOMEMLIMIT", Value: "fleet"}}
	cfg.ProjectCache = map[string]config.ProjectCacheEntry{
		"acme": {
			LandKind: "tenant", Namespace: "land-acme", Account: "lands-acme",
			DockerEnabled: true, PostgresEnabled: true, ZitiAccessEnabled: true, ZitiServiceID: "fleet-svc",
			Secrets: secrets, EnvVars: env, EnvOverrides: map[string]string{"FLEET_OVERRIDE": "fleet"},
			Repos: []beadsapi.RepoEntry{
				{URL: "https://github.com/acme/app.git", Role: "primary"},
				{URL: "https://github.com/acme/fleet-ref.git", Role: "reference"},
			},
			GitURL: "https://github.com/acme/app.git", Prefix: "ac",
		},
		"gardenio": {GitURL: "https://github.com/fleet/gardenio.git", Prefix: "gd"},
	}
	roleSecrets := func(role string) []beadsapi.SecretEntry {
		return []beadsapi.SecretEntry{
			{Env: "GIT_TOKEN", Secret: role + "-fleet-pat", Key: "token"},
			{Env: "COOP_BROKER_TOKEN", Secret: role + "-fleet-broker", Key: "token"},
			{Env: "SLACK_BOT_TOKEN", Secret: role + "-fleet-slack", Key: "token"},
		}
	}
	cfg.RoleCache = map[string]config.RoleCacheEntry{
		"escape": {
			RuntimeClassName: "runc", ServiceAccount: "cluster-admin-sa", NeedsK8sAPI: true, MachineClient: "fleet-coordinator",
			ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM, Account: "fleet-acct",
			DockerEnabled: true, PostgresEnabled: true, ZitiAccessEnabled: true,
			Secrets: roleSecrets("escape"), EnvVars: env, EnvOverrides: map[string]string{"FLEET_ROLE": "fleet"},
		},
		"plain": {Secrets: roleSecrets("plain")},
	}
	cfg.AccountCache = map[string]config.AccountCacheEntry{
		"lands-acme": {Provider: "anthropic", BaseURL: "http://litellm:4000", LiteLLMKey: "sk-land",
			Credential: "secret:lands-acme", Labels: []string{"project:acme"}},
		"lands-template": {Provider: "anthropic", BaseURL: "http://litellm:4000", Credential: "secret:claude-oauth"},
		"fleet-acct": {Provider: "anthropic", BaseURL: "http://litellm:4000", LiteLLMKey: "sk-fleet",
			Credential: "secret:claude-oauth-fleet-acct", Labels: []string{"project:gardenio"}},
	}
	return cfg
}

// fillConfig sets every zero exported scalar, string slice and string map in
// v to a non-zero value, so every setting-gated injection fires.
func fillConfig(v reflect.Value) {
	t := v.Type()
	for i := range t.NumField() {
		f, sf := v.Field(i), t.Field(i)
		if !sf.IsExported() || !f.IsZero() {
			continue
		}
		switch {
		case f.Kind() == reflect.String:
			f.SetString("cfg-" + strings.ToLower(sf.Name))
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		case f.Type() == reflect.TypeOf(time.Duration(0)):
			f.SetInt(int64(time.Second))
		case f.CanInt():
			f.SetInt(1)
		case f.CanFloat():
			f.SetFloat(1)
		case f.Type() == reflect.TypeOf([]string(nil)):
			f.Set(reflect.ValueOf([]string{"cfg-" + strings.ToLower(sf.Name)}))
		case f.Type() == reflect.TypeOf(map[string]string(nil)):
			f.Set(reflect.ValueOf(map[string]string{"prod": "cfg-" + strings.ToLower(sf.Name)}))
		}
	}
}

// allowlistPins are agent metadata that try to add to a tenant pod.
func allowlistPins() map[string]string {
	return map[string]string{
		"namespace": "gardenio", "runtime_class": "runc", "execution_target": "kubernetes",
		"docker": "true", "postgres": "true", "configmap": "fleet-cm", "ziti_access": "true",
		"mock_scenario": "fleet", "jira_key": "FLEET-1", "prompt": "hello",
		"slack_thread_channel": "C1", "slack_thread_ts": "1.2", "task_id": "gd-task",
	}
}

// renderTenantPod builds the spec, does what the spawn door does after the
// builders (mount the per-agent daemon token and the Lands App token), and
// renders the Pod with the shared coverage PVC on.
func renderTenantPod(spec podmanager.AgentPodSpec) *corev1.Pod {
	spec.BeadID = allowlistBead
	spec.DaemonTokenSecret = agentTokenSecretName(allowlistBead)
	spec.GitCredentialsSecret = githubTokenSecretName(allowlistBead)
	m := podmanager.New(fake.NewSimpleClientset(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Coverage = podmanager.CoverageConfig{Enabled: true, ClaimName: "coverage-fleet"}
	return m.BuildPod(spec)
}

func TestTenantPodAllowlist(t *testing.T) {
	for _, fill := range []bool{false, true} {
		for _, role := range []string{"plain", "escape"} {
			name := role
			if fill {
				name += "/every-setting"
			}
			t.Run(name+"/event", func(t *testing.T) {
				cfg := allowlistCfg(fill)
				spec := buildAgentPodSpec(cfg, subscriber.Event{
					Project: "acme", Role: role, AgentName: "acme-crew-1", Mode: "crew", BeadID: allowlistBead, Metadata: allowlistPins(),
				})
				assertTenantPodAllowed(t, cfg, renderTenantPod(spec))
			})
			t.Run(name+"/reconciler", func(t *testing.T) {
				cfg := allowlistCfg(fill)
				spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", role, "acme-crew-1", allowlistPins())
				assertTenantPodAllowed(t, cfg, renderTenantPod(spec))
			})
		}
	}
}

// No fleet NATS token reaches a tenant pod, the agent user's or the legacy
// shared one: both are fleet-wide, and the agent user may subscribe to every
// relay session's input (gd-ApgYe740ps). Without land NATS users it gets no
// NATS at all.
func TestTenantPodAllowlist_NoNatsToken(t *testing.T) {
	for _, agentUser := range []string{"nats-agent-user-fleet", ""} {
		cfg := allowlistCfg(false)
		cfg.NatsAgentUserSecret = agentUser
		pod := renderTenantPod(BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{}))
		assertTenantPodAllowed(t, cfg, pod)
		for _, v := range pod.Spec.Volumes {
			if v.Name == podmanager.VolumeNatsToken {
				t.Errorf("agent user %q: tenant pod mounts a NATS token %+v", agentUser, v.Secret)
			}
		}
		for _, e := range pod.Spec.Containers[0].Env {
			if strings.Contains(e.Name, "NATS") {
				t.Errorf("agent user %q: tenant pod carries %s", agentUser, e.Name)
			}
		}
	}
}

// With land NATS users configured, a tenant pod's NATS is its land's alone:
// user land-<land>, its password from the land's own Secret, and coop events
// under the land's prefix, whichever fleet NATS token the builders set
// (gd-sS7bSXOdGs). Lacking any of NATS, the land token key or the callout
// issuer, it gets none.
func TestTenantPodAllowlist_LandNatsUser(t *testing.T) {
	for _, agentUser := range []string{"nats-agent-user-fleet", ""} {
		cfg := allowlistCfg(false)
		cfg.NatsAgentUserSecret = agentUser
		cfg.NatsLandCalloutIssuerSeedFile = "/etc/nats-land-callout/seed"
		spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
		if !spec.LandNatsToken || spec.NatsTokenSecret != podmanager.LandNatsTokenSecret {
			t.Fatalf("agent user %q: LandNatsToken=%v NatsTokenSecret=%q, want the land's", agentUser, spec.LandNatsToken, spec.NatsTokenSecret)
		}
		pod := renderTenantPod(spec)
		assertTenantPodAllowed(t, cfg, pod)
		got := map[string]string{}
		for _, e := range pod.Spec.Containers[0].Env {
			if strings.Contains(e.Name, "NATS") {
				got[e.Name] = e.Value
			}
		}
		want := map[string]string{
			"COOP_NATS_URL":        "nats://nats:4222",
			"COOP_NATS_FORWARD":    "1",
			"COOP_NATS_USER":       "land-acme",
			"COOP_NATS_PREFIX":     "coop.events.land.acme",
			"COOP_NATS_TOKEN_FILE": podmanager.MountNatsToken + "/" + podmanager.NatsTokenFileName,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("agent user %q: NATS env = %v, want %v", agentUser, got, want)
		}
	}
	for name, unset := range map[string]func(*config.Config){
		"no nats":         func(c *config.Config) { c.NatsURL = "" },
		"no land key":     func(c *config.Config) { c.CoopmuxLandTokenKeyFile = "" },
		"no callout seed": func(c *config.Config) { c.NatsLandCalloutIssuerSeedFile = "" },
	} {
		cfg := allowlistCfg(false)
		cfg.NatsLandCalloutIssuerSeedFile = "/etc/nats-land-callout/seed"
		unset(cfg)
		spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
		if spec.LandNatsToken || spec.NatsTokenSecret != "" {
			t.Errorf("%s: LandNatsToken=%v NatsTokenSecret=%q, want no NATS", name, spec.LandNatsToken, spec.NatsTokenSecret)
		}
		for k := range spec.Env {
			if strings.Contains(k, "NATS") {
				t.Errorf("%s: tenant spec carries %s", name, k)
			}
		}
	}
}

// An operator land's pod is not fenced: the role pin, the shared tokens and
// the bead secrets all still reach it.
func TestTenantPodAllowlist_OperatorLandUntouched(t *testing.T) {
	cfg := allowlistCfg(false)
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "escape", "gd-crew-1", map[string]string{})
	if spec.LandFenced || spec.DaemonTokenSecret != "beads-fleet-static" || spec.ResolvedAccount != "fleet-acct" ||
		spec.MachineClientSecret != "gardenio-machine-fleet-coordinator" {
		t.Errorf("operator pod fenced: fenced=%v daemon=%q account=%q machine=%q", spec.LandFenced, spec.DaemonTokenSecret, spec.ResolvedAccount, spec.MachineClientSecret)
	}
}

func assertTenantPodAllowed(t *testing.T, cfg *config.Config, pod *corev1.Pod) {
	t.Helper()
	ps := pod.Spec
	if len(ps.ImagePullSecrets) > 0 {
		t.Errorf("image pull secrets = %v, want none", ps.ImagePullSecrets)
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("tenant pod mounts a Kubernetes API token")
	}
	if len(ps.Containers) != 1 {
		t.Errorf("containers = %d, want the agent alone", len(ps.Containers))
	}
	for _, c := range ps.InitContainers {
		if c.Name != podmanager.InitCloneName {
			t.Errorf("init container %q, want init-clone only", c.Name)
		}
	}
	for _, c := range append(slices.Clone(ps.InitContainers), ps.Containers...) {
		assertTenantEnv(t, cfg, c)
	}
	assertTenantVolumes(t, cfg, ps.Volumes)

	// Nothing the fixture marked fleet, and the land's own LiteLLM key, not
	// the fleet account the escape role pins.
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); strings.Contains(strings.ToLower(s), "fleet") {
		i := strings.Index(strings.ToLower(s), "fleet")
		t.Errorf("rendered pod carries a fleet value near %q", s[max(0, i-80):min(len(s), i+40)])
	}
	if !strings.Contains(string(raw), "x-litellm-api-key: Bearer sk-land") {
		t.Error("tenant pod lacks the land's LiteLLM key")
	}
}

// assertTenantEnv fails on any env var of c outside tenantAllowedEnv, and on
// any secret-sourced one not from its allowed Secret.
func assertTenantEnv(t *testing.T, cfg *config.Config, c corev1.Container) {
	t.Helper()
	// Secrets a tenant pod may reference: env name -> Secret. The Claude
	// credential is the one the land's account names (landCredential).
	credential, _ := strings.CutPrefix(cfg.AccountCache["lands-acme"].Credential, "secret:")
	secretEnv := map[string]string{
		"BEADS_DAEMON_TOKEN":      agentTokenSecretName(allowlistBead),
		"BEADS_AUTH_TOKEN":        agentTokenSecretName(allowlistBead),
		"BEADS_REFRESH_TOKEN":     agentTokenSecretName(allowlistBead),
		"COOP_BROKER_TOKEN":       podmanager.LandCoopmuxTokenSecret,
		"COOP_MUX_TOKEN":          podmanager.LandCoopmuxTokenSecret,
		"CLAUDE_CODE_OAUTH_TOKEN": credential,
	}
	if len(c.EnvFrom) > 0 {
		t.Errorf("%s: envFrom %+v", c.Name, c.EnvFrom)
	}
	landNats := landNatsEnabled(cfg)
	for _, e := range c.Env {
		if slices.Contains(tenantNatsEnv, e.Name) {
			if !landNats || e.ValueFrom != nil {
				t.Errorf("%s: env %s without the land's NATS user (landNats=%v)", c.Name, e.Name, landNats)
			}
			continue
		}
		if !tenantAllowedEnv[e.Name] {
			t.Errorf("%s: env %s is not on the tenant allowlist", c.Name, e.Name)
			continue
		}
		if e.ValueFrom == nil {
			if _, secret := secretEnv[e.Name]; secret {
				t.Errorf("%s: env %s is a plain value, want it from its Secret", c.Name, e.Name)
			}
			continue
		}
		switch src := e.ValueFrom; {
		case src.SecretKeyRef != nil:
			if want, ok := secretEnv[e.Name]; !ok || src.SecretKeyRef.Name != want {
				t.Errorf("%s: env %s from Secret %q, want %q", c.Name, e.Name, src.SecretKeyRef.Name, want)
			}
		case src.FieldRef != nil:
		default:
			t.Errorf("%s: env %s from %+v", c.Name, e.Name, src)
		}
	}
}

// assertTenantVolumes fails on any volume outside the tenant allowlist, and
// on an allowed one that mounts anything but its allowed source.
func assertTenantVolumes(t *testing.T, cfg *config.Config, volumes []corev1.Volume) {
	t.Helper()
	for _, v := range volumes {
		vs := v.VolumeSource
		switch v.Name {
		case podmanager.VolumeWorkspace:
			if vs.PersistentVolumeClaim == nil && vs.EmptyDir == nil {
				t.Errorf("workspace volume %+v", vs)
			}
		case podmanager.VolumeTmp:
			if vs.EmptyDir == nil {
				t.Errorf("tmp volume %+v", vs)
			}
		case podmanager.VolumeNatsToken:
			if !landNatsEnabled(cfg) || vs.Secret == nil || vs.Secret.SecretName != podmanager.LandNatsTokenSecret {
				t.Errorf("nats token volume %+v, want the land's own %s and only with land NATS users", vs.Secret, podmanager.LandNatsTokenSecret)
			}
		case podmanager.VolumeGitCreds:
			if vs.Projected == nil || len(vs.Projected.Sources) != 1 || vs.Projected.Sources[0].Secret == nil ||
				vs.Projected.Sources[0].Secret.Name != githubTokenSecretName(allowlistBead) {
				t.Errorf("git creds volume %+v, want the agent's Lands App token alone", vs.Projected)
			}
		default:
			t.Errorf("volume %q is not on the tenant allowlist", v.Name)
		}
	}
}

// A tenant pod's gb say goes to the bridge's land listener, the one bridge
// port its land's NetworkPolicy reaches; with no land listener configured it
// gets no bridge URL rather than the full mux's (gd-pXik57wZA9).
func TestTenantPodAllowlist_BridgeURLIsTheLandListener(t *testing.T) {
	cfg := allowlistCfg(false)
	cfg.SlackBridgeURL = "http://gardenio-slack-bridge:8090"
	cfg.LandsBridgeURL = "http://gardenio-slack-bridge:8091"
	spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
	if got := spec.Env["SLACK_BRIDGE_URL"]; got != cfg.LandsBridgeURL {
		t.Errorf("tenant SLACK_BRIDGE_URL = %q, want the land listener %q", got, cfg.LandsBridgeURL)
	}
	cfg.LandsBridgeURL = ""
	spec = BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
	if got, set := spec.Env["SLACK_BRIDGE_URL"]; set {
		t.Errorf("tenant SLACK_BRIDGE_URL = %q with no land listener, want unset", got)
	}
	cfg.LandsBridgeURL = "http://gardenio-slack-bridge:8091"
	spec = BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "plain", "gd-crew-1", map[string]string{})
	if got := spec.Env["SLACK_BRIDGE_URL"]; got != cfg.SlackBridgeURL {
		t.Errorf("operator SLACK_BRIDGE_URL = %q, want the full bridge %q", got, cfg.SlackBridgeURL)
	}
}
