package main

import (
	"fmt"
	"log/slog"
	"slices"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/podmanager"
)

// What a tenant pod holds (Lands Track B spec §5.3, step 10.3): a per-agent
// daemon token bound to its project, the land's LiteLLM key, a Lands App
// installation token for the one repo, its coop credentials and its land's
// own NATS user (fenceTenantNats, gd-sS7bSXOdGs). Nothing
// else: no AWS role, no fleet secrets, no Slack, Jira or GitHub bridge tokens,
// no internal service URLs. fenceTenantPodContents keeps only that, and
// TestTenantPodAllowlist renders a tenant pod and fails on anything else.
//
// The per-agent daemon token and the Lands App token are mounted at the spawn
// doors after the spec is built (provisionAgentToken, githubAppIssuer), so the
// fence clears the shared daemon token and every git credential the builders
// set; the doors put back only the agent's own.

// tenantPodEnv is the allowlist of plain env names a tenant pod's spec may
// carry. The names podmanager adds itself (HOME, POD_IP, the identity vars,
// GOMEMLIMIT, the daemon token vars) are not in spec.Env and not listed here.
var tenantPodEnv = map[string]bool{
	// The kbeads daemon (default-deny egress lets it through).
	"BEADS_GRPC_ADDR": true,
	"BEADS_HTTP_ADDR": true,
	// coop: session registration with coopmux. No NATS here: the fleet's
	// agent user may subscribe to every relay session's input (gd-ApgYe740ps),
	// so the builders' COOP_NATS_* always goes and fenceTenantNats sets the
	// land's own.
	"COOP_HOST":       true,
	"COOP_BROKER_URL": true,
	"COOP_MUX_URL":    true,
	// The land's account: LiteLLM base URL and key, model and harness.
	"BOAT_ACCOUNT":                  true,
	"ANTHROPIC_BASE_URL":            true,
	"ANTHROPIC_CUSTOM_HEADERS":      true,
	"ANTHROPIC_MODEL":               true,
	"CLAUDE_MODEL":                  true,
	"ANTHROPIC_DEFAULT_FABLE_MODEL": true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL":  true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL": true,
	EnvModelPinned:                  true,
	EnvHarness:                      true,
	envCodexHome:                    true,
	envCodexModel:                   true,
	"GEMINI_MODEL":                  true,
	envGeminiAuthMode:               true,
	geminiTrustWorkspaceEnv:         true,
	EnvUltracode:                    true,
	"CLAUDE_TEAMS_ENABLED":          true,
	"CLAUDE_TEAMMATE_MODE":          true,
	"CLAUDE_TEAMS_MAX_TEAMMATES":    true,
	// gb say (the bridge's land listener authenticates the per-agent token;
	// fenceTenantEnv sets it) and public links.
	"SLACK_BRIDGE_URL":      true,
	"BEADS_VIEWER_URL":      true,
	"GARDENIO_DEFAULT_HOST": true,
	"GARDENIO_CHANNEL":      true,
	// The agent's turn: its prompt, wake reason, thread and standby state.
	"BOAT_PROMPT":          true,
	"BOAT_WAKE_REASON":     true,
	"SLACK_THREAD_CHANNEL": true,
	"SLACK_THREAD_TS":      true,
	"BOAT_AGENT_STATE":     true,
	"BOAT_STANDBY":         true,
	"BOAT_STANDBY_POLL":    true,
}

// tenantAccountEnv are the allowlisted names that carry the account's
// credential or routing. They stay only when the pod resolved the land's own
// lands-<land> account.
var tenantAccountEnv = []string{
	"BOAT_ACCOUNT", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", envCodexHome, envGeminiAuthMode,
}

// fenceTenantPodContents strips a tenant land's pod spec down to the §5.3
// allowlist. acct is the land's lands-<land> account from the cache (ok false
// when the cache has none). Called by applyLandFence, so both spec builders
// apply it, after every layer that can add env, secrets or volumes.
func fenceTenantPodContents(cfg *config.Config, spec *podmanager.AgentPodSpec, acct config.AccountCacheEntry, ok bool) {
	landAccount := ok && spec.ResolvedAccount == purse.AccountName(spec.Project)
	var dropped []string
	drop := func(what string) { dropped = append(dropped, what) }

	fenceTenantEnv(cfg, spec, acct, landAccount, drop)
	fenceTenantMounts(cfg, spec, drop)
	fenceTenantNats(cfg, spec)
	spec.LandFenced = true
	spec.LandSecretCopies = tenantSecretCopies(cfg, spec)

	if len(dropped) > 0 {
		if _, seen := warnedForbiddenSecret.LoadOrStore("land|"+spec.Project+"|"+spec.Role, struct{}{}); !seen {
			slog.Info("land fence kept a tenant pod to its allowlist",
				"project", spec.Project, "role", spec.Role, "agent", spec.AgentName, "dropped", dropped)
		}
	}
}

// fenceTenantEnv sets the secret-sourced env to the land's own coopmux token
// and its account's Claude credential, and keeps the plain env to
// tenantPodEnv (gd-ApgYe740ps). Project and role secrets, the bridge tokens,
// the Grafana bundle, every git variable and the fleet's coopmux token go,
// whatever names them. The coopmux token is the land's
// (podmanager.LandCoopmuxTokenSecret, minted at pod create) when the
// controller can mint one; without it the pod gets no coopmux token and no
// coopmux URL, so coop does not register. The credential is the Secret the
// land's account names, if it is one of the two landCredential allows: the
// template account's (the fleet Claude credential, which stays by owner
// ruling gd-NXTrUXD1Sq) or the land's own lands-<land>. An account that names
// any other Secret, a forbidden name or no Secret at all blocks the spawn
// rather than hand the tenant what it may not read. landAccount reports that
// the pod resolved the land's account; without it the account's credential
// and routing go too.
func fenceTenantEnv(cfg *config.Config, spec *podmanager.AgentPodSpec, acct config.AccountCacheEntry, landAccount bool, drop func(string)) {
	var want []podmanager.SecretEnvSource
	spec.LandCoopmuxToken = cfg.CoopmuxLandTokenKeyFile != "" && cfg.CoopmuxURL != ""
	if spec.LandCoopmuxToken {
		for _, env := range []string{"COOP_BROKER_TOKEN", "COOP_MUX_TOKEN"} {
			want = append(want, podmanager.SecretEnvSource{EnvName: env, SecretName: podmanager.LandCoopmuxTokenSecret, SecretKey: "token"})
		}
	} else {
		for _, k := range []string{"COOP_BROKER_URL", "COOP_MUX_URL"} {
			if _, set := spec.Env[k]; set {
				drop("env " + k + " (no land coopmux token)")
				delete(spec.Env, k)
			}
		}
	}
	if landAccount {
		tmplCred, tmplKnown := landTemplateCredential(cfg)
		name, allowed := landCredential(spec.Project, tmplCred, acct)
		switch {
		case !allowed:
			template := "the " + cfg.LandsAccountTemplate + " account's credential"
			if !tmplKnown {
				template = "the LANDS_ACCOUNT_TEMPLATE account's credential (unset or not loaded on this controller)"
			}
			spec.SpawnBlocked = true
			spec.SpawnBlockReason = fmt.Sprintf("land %s's account %s reads its credential from %q, which a tenant pod may not: "+
				"set its credential to secret:%s (this land's own) or to %s, "+
				"or clear it to bill through the land's LiteLLM key alone (gd-ApgYe740ps)",
				spec.Project, spec.ResolvedAccount, acct.Credential, landCredentialSecret(spec.Project), template)
		case name != "":
			want = append(want, podmanager.SecretEnvSource{EnvName: claudeOAuthEnvName, SecretName: name, SecretKey: "token"})
		}
	}
	for _, se := range spec.SecretEnv {
		if !slices.Contains(want, se) {
			drop("secret env " + se.EnvName + " from " + se.SecretName)
		}
	}
	spec.SecretEnv = want

	if !landAccount {
		for _, k := range tenantAccountEnv {
			if _, set := spec.Env[k]; set {
				drop("env " + k)
				delete(spec.Env, k)
			}
		}
		unsetSpecField(&spec.CodexAuthSecret, "codex auth secret", drop)
		unsetSpecField(&spec.GeminiOAuthSecret, "gemini auth secret", drop)
	}
	for k := range spec.Env {
		if !tenantPodEnv[k] {
			drop("env " + k)
			delete(spec.Env, k)
		}
	}
	// gb say goes to the bridge's land listener, never its full mux
	// (cfg.LandsBridgeURL, gd-pXik57wZA9).
	if cfg.LandsBridgeURL != "" {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env["SLACK_BRIDGE_URL"] = cfg.LandsBridgeURL
	} else if _, set := spec.Env["SLACK_BRIDGE_URL"]; set {
		drop("env SLACK_BRIDGE_URL (no land listener)")
		delete(spec.Env, "SLACK_BRIDGE_URL")
	}
}

// fenceTenantMounts clears every Secret, ConfigMap, sidecar and overlay the
// builders mount. The doors then mount the per-agent daemon token and the
// Lands App token, and fenceTenantNats the land's NATS password.
func fenceTenantMounts(cfg *config.Config, spec *podmanager.AgentPodSpec, drop func(string)) {
	// The fleet's NATS token never stays. The agent user is shared by every
	// pod and may subscribe to coop.mux.session.*.input (gd-ApgYe740ps), and
	// the legacy token is wider still; fenceTenantEnv drops the NATS env.
	unsetSpecField(&spec.NatsTokenSecret, "fleet nats token", drop)
	unsetSpecField(&spec.DaemonTokenSecret, "shared daemon token", drop)
	unsetSpecField(&spec.GitCredentialsSecret, "shared git credential", drop)
	unsetSpecField(&spec.GitlabTokenSecret, "gitlab token", drop)
	unsetSpecField(&spec.CredentialsSecret, "fleet claude credentials", drop)
	unsetSpecField(&spec.ObjectStoreCredentialsSecret, "object store credentials", drop)
	unsetSpecField(&spec.SlackViewSessionSecret, "slack viewer session", drop)
	unsetSpecField(&spec.MachineClientSecret, "machine client secret", drop)
	unsetSpecField(&spec.ConfigMapName, "configmap", drop)
	if len(spec.ReferenceRepos) > 0 {
		drop("reference repos")
		spec.ReferenceRepos = nil
	}
	if len(spec.SshKeys) > 0 {
		drop("ssh keys")
		spec.SshKeys = nil
	}
	if len(spec.ImagePullSecrets) > 0 {
		drop("image pull secrets")
		spec.ImagePullSecrets = nil
	}
	if spec.ZitiAccess != nil {
		drop("ziti access")
		spec.ZitiAccess = nil
	}
	if spec.Docker || spec.Postgres {
		drop("sidecars")
		spec.Docker, spec.Postgres = false, false
	}
}

// tenantSecretCopies lists the release-namespace Secrets the fenced spec
// still references, with the key each is read by: only the credential its
// account names (landCredential), read as CLAUDE_CODE_OAUTH_TOKEN. Run after
// the fence, so nothing off the allowlist is ever copied into a land
// namespace. The coopmux
// token is minted in the land namespace, and the per-agent daemon token and
// the Lands App token by the doors; none is copied. podmanager copies these at
// pod create (gd-gokoYMl5px).
func tenantSecretCopies(cfg *config.Config, spec *podmanager.AgentPodSpec) []podmanager.LandSecretCopy {
	if cfg.Namespace == "" || spec.Namespace == "" || cfg.Namespace == spec.Namespace {
		return nil
	}
	for _, se := range spec.SecretEnv {
		if se.EnvName == claudeOAuthEnvName && se.SecretName != "" {
			return []podmanager.LandSecretCopy{{SourceNamespace: cfg.Namespace, Name: se.SecretName, Keys: []string{se.SecretKey}}}
		}
	}
	return nil
}

// unsetSpecField empties a spec field naming a Secret or ConfigMap, noting
// what went.
func unsetSpecField(field *string, what string, drop func(string)) {
	if *field != "" {
		drop(what)
		*field = ""
	}
}
