package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/lands/landns"
	"gardenio/controller/internal/lands/landscope"
	"gardenio/controller/internal/lands/provision"
	"gardenio/controller/internal/mailer"
	"gardenio/controller/internal/steward"
)

// The Lands provisioner (internal/lands/provision, Track B spec §3.3, step
// 8.6): once a minute it turns approved plot applications into lands and
// closes the lands gb lands close asks it to. It is off unless
// LANDS_PROVISION_ENABLED is true, and then it needs every one of its
// credentials: a provisioner missing one stays off and says which, because a
// half-built land is worse than an approval that waits.
//
//   - Keycloak: the confidential client gardenio-lands-provisioner (step 8.5,
//     gardenio/docs/lands/keycloak-provisioner.md): LANDS_KEYCLOAK_URL,
//     LANDS_KEYCLOAK_REALM (default gardenio), LANDS_KEYCLOAK_CLIENT_ID
//     (default gardenio-lands-provisioner), LANDS_KEYCLOAK_CLIENT_SECRET_PATH
//     (a mounted Secret key, never the environment) and
//     LANDS_KEYCLOAK_RESERVED_GROUPS (comma-separated groups the chart renders
//     besides keycloakadmin.ReservedGroups). LANDS_KEYCLOAK_CA_FILE is a PEM
//     CA bundle trusted besides the system roots: the in-cluster
//     keycloak-runtime Service serves https on the chart's own CA
//     (gd-XRsosVFAau).
//   - The Lands GitHub App (step 7.6), LANDS_GITHUB_APP_ID and
//     LANDS_GITHUB_APP_PRIVATE_KEY_PATH, for each repository's default branch.
//   - LiteLLM: LITELLM_URL and LITELLM_MASTER_KEY, which spend sync already
//     uses.
//   - LANDS_ACCOUNT_TEMPLATE: the account bead whose provider, base URL,
//     credential and model a land's lands-<land> account copies; never its key.
//
// The demo land (gd-c6LzZkk0qy): LANDS_DEMO_LAND names the land whose guest
// passes the viewer's /lands/demo door hands out (set it on both), and
// LANDS_DEMO_GUEST_TTL (a Go duration, at least 1h, default 24h) is how long
// a pass lasts after the join. Runbook: gardenio/docs/lands/demo-land.md.
//
// Its daemon writes go out as the controller's machine principal, which must
// hold gardenio-machine-lands-provisioner (kbeads machine_grants.go). The
// steward step (internal/steward ProvisionHook, step 9.3) writes the land's steward
// trigger and its wake post on the same principal.
//
// Land namespaces (internal/lands/landns, step 10.2) are on with
// LANDS_NAMESPACES_ENABLED, and then need LANDS_CONTROLLER_SERVICE_ACCOUNT
// (the controller's own ServiceAccount, which each land's RoleBinding names)
// and the controller RBAC the chart renders with landNamespaces.enabled.
//
// Per-land audience scopes (internal/lands/landscope, gd-eAfptUBh9Z) are
// KeycloakClientScope resources in the release namespace, made for the
// KeycloakRealm resource LANDS_KEYCLOAK_REALM_RESOURCE names; the chart
// renders it with the RBAC and admission policy that hold those writes.
// LANDS_RUNTIME_CLASS (landns.RuntimeClassEnv) is the RuntimeClass tenant
// pods run under and the namespace step stamps; unset, the node's default.
// LANDS_NAMESPACE_EGRESS_PEERS (a JSON list of landns.Peer) replaces the
// default in-cluster egress peers, and LANDS_NAMESPACE_EXTRA_EXCEPT_CIDRS
// (comma-separated) cuts more ranges from the internet egress rule. With them
// off, the provisioner fails every land at its namespace step: a tenant land
// never opens without its fence.
const (
	landsProvisionEnabledEnv       = "LANDS_PROVISION_ENABLED"
	landsKeycloakURLEnv            = "LANDS_KEYCLOAK_URL"
	landsKeycloakRealmEnv          = "LANDS_KEYCLOAK_REALM"
	landsKeycloakClientIDEnv       = "LANDS_KEYCLOAK_CLIENT_ID"
	landsKeycloakClientSecretEnv   = "LANDS_KEYCLOAK_CLIENT_SECRET_PATH"
	landsKeycloakReservedGroupsEnv = "LANDS_KEYCLOAK_RESERVED_GROUPS"
	landsKeycloakCAFileEnv         = "LANDS_KEYCLOAK_CA_FILE"
	landsAccountTemplateEnv        = "LANDS_ACCOUNT_TEMPLATE"
	landsGitHubAppEnvPrefix        = "LANDS_GITHUB_APP"
	landsOperatorGitHubOrgsEnv     = "LANDS_OPERATOR_GITHUB_ORGS"
	landsProvisionerMachineRole    = "gardenio-machine-lands-provisioner"
	landsKeycloakRealmDefault      = "gardenio"
	landsKeycloakClientIDDefault   = "gardenio-lands-provisioner"

	// landsKeycloakRealmResourceEnv names the KeycloakRealm resource (in the
	// release namespace) a land's KeycloakClientScope resource belongs to
	// (internal/lands/landscope, gd-eAfptUBh9Z). Unset, the provisioner makes
	// no land scopes and every land door refuses every token.
	landsKeycloakRealmResourceEnv = "LANDS_KEYCLOAK_REALM_RESOURCE"

	landsNamespacesEnabledEnv     = "LANDS_NAMESPACES_ENABLED"
	landsControllerSAEnv          = "LANDS_CONTROLLER_SERVICE_ACCOUNT"
	landsNamespaceEgressPeersEnv  = "LANDS_NAMESPACE_EGRESS_PEERS"
	landsNamespaceExtraExceptsEnv = "LANDS_NAMESPACE_EXTRA_EXCEPT_CIDRS"

	landsDemoLandEnv     = "LANDS_DEMO_LAND"
	landsDemoGuestTTLEnv = "LANDS_DEMO_GUEST_TTL"

	// landsGatehouseLandEnv names the gatehouse (step 9.12): the land every
	// signed-in person with no land is a guest of. Unset, there is none.
	landsGatehouseLandEnv = "LANDS_GATEHOUSE_LAND"
)

// landsProvisionEnabled reports whether the deployment turned the provisioner
// on.
func landsProvisionEnabled(getenv func(string) string) bool {
	return strings.EqualFold(strings.TrimSpace(getenv(landsProvisionEnabledEnv)), "true")
}

// controllerMachineRolesFor is controllerMachineRoles plus the provisioner's
// role when it or the purge sweep (lands_purge.go) is on, so a token without
// it is re-fetched rather than kept.
func controllerMachineRolesFor(getenv func(string) string) []string {
	roles := append([]string(nil), controllerMachineRoles...)
	if landsProvisionEnabled(getenv) || landsPurgeEnabled(getenv) {
		roles = append(roles, landsProvisionerMachineRole)
	}
	return roles
}

// landsRepos reads default branches with the Lands App.
type landsRepos struct{ app *githubapp.Client }

func (r landsRepos) DefaultBranch(ctx context.Context, installationID int64, repo string) (string, error) {
	owner, name, _ := strings.Cut(repo, "/")
	return r.app.DefaultBranch(ctx, installationID, githubapp.RepoRef{Owner: owner, Name: name})
}

// newLandsProvisioner builds the provisioner from the environment. It returns
// nil and no error when the provisioner is off, and an error naming what is
// missing when it is on but cannot run.
func newLandsProvisioner(ctx context.Context, daemon *beadsapi.Client, litellmURL, litellmMasterKey string,
	getenv func(string) string, readFile func(string) ([]byte, error), logger *slog.Logger) (*provision.Provisioner, error) {
	if !landsProvisionEnabled(getenv) {
		return nil, nil
	}
	logger = logger.With("component", "lands-provisioner")
	var missing []string
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }

	kcURL, secretPath := get(landsKeycloakURLEnv), get(landsKeycloakClientSecretEnv)
	if kcURL == "" {
		missing = append(missing, landsKeycloakURLEnv)
	}
	if secretPath == "" {
		missing = append(missing, landsKeycloakClientSecretEnv)
	}
	if litellmURL == "" || litellmMasterKey == "" {
		missing = append(missing, "LITELLM_URL and LITELLM_MASTER_KEY")
	}
	tmpl := get(landsAccountTemplateEnv)
	if tmpl == "" {
		missing = append(missing, landsAccountTemplateEnv)
	}
	appCfg, appOK, appErr := githubapp.ConfigFromEnv(githubapp.Lands, landsGitHubAppEnvPrefix, false, getenv, readFile)
	if appErr != nil {
		return nil, appErr
	}
	if !appOK {
		missing = append(missing, landsGitHubAppEnvPrefix+"_ID and "+landsGitHubAppEnvPrefix+"_PRIVATE_KEY_PATH")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s is set but %s is not", landsProvisionEnabledEnv, strings.Join(missing, ", "))
	}
	demoLand := get(landsDemoLandEnv)
	if demoLand != "" {
		if _, err := keycloakadmin.GroupName(demoLand); err != nil {
			return nil, fmt.Errorf("%s=%q: %w", landsDemoLandEnv, demoLand, err)
		}
	}
	gatehouse := get(landsGatehouseLandEnv)
	if gatehouse != "" {
		if _, err := keycloakadmin.GroupName(gatehouse); err != nil {
			return nil, fmt.Errorf("%s=%q: %w", landsGatehouseLandEnv, gatehouse, err)
		}
		if gatehouse == demoLand {
			// A demo pass ends by taking the guest out of the land, which
			// would take them out of the gatehouse too.
			return nil, fmt.Errorf("%s and %s both name %q; the gatehouse is not the demo land", landsGatehouseLandEnv, landsDemoLandEnv, gatehouse)
		}
	}
	var demoTTL time.Duration
	if v := get(landsDemoGuestTTLEnv); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Hour {
			return nil, fmt.Errorf("%s=%q: want a duration of at least 1h", landsDemoGuestTTLEnv, v)
		}
		demoTTL = d
	}

	secret, err := readFile(secretPath)
	if err != nil {
		return nil, fmt.Errorf("reading the Keycloak client secret: %w", err)
	}
	realm := get(landsKeycloakRealmEnv)
	if realm == "" {
		realm = landsKeycloakRealmDefault
	}
	clientID := get(landsKeycloakClientIDEnv)
	if clientID == "" {
		clientID = landsKeycloakClientIDDefault
	}
	var reserved []string
	for _, g := range strings.Split(get(landsKeycloakReservedGroupsEnv), ",") {
		if g = strings.TrimSpace(g); g != "" {
			reserved = append(reserved, g)
		}
	}
	hc, err := landsKeycloakHTTPClient(get(landsKeycloakCAFileEnv), readFile)
	if err != nil {
		return nil, err
	}
	kc, err := keycloakadmin.New(keycloakadmin.Config{
		BaseURL: kcURL, Realm: realm, ClientID: clientID,
		ClientSecret: strings.TrimSpace(string(secret)), ReservedGroups: reserved,
		HTTPClient: hc,
	}, logger)
	if err != nil {
		return nil, err
	}
	app, err := githubapp.New(appCfg.Config)
	if err != nil {
		return nil, err
	}

	mailCfg := mailer.ConfigFromEnv()
	m, err := mailer.New(ctx, mailCfg)
	if err != nil {
		logger.Warn("lands provisioner: mailer unavailable, land mail is off", "error", err)
		m = mailer.Disabled{}
	}
	return &provision.Provisioner{
		Daemon:          daemon,
		Keys:            &provision.LiteLLM{BaseURL: litellmURL, MasterKey: litellmMasterKey},
		Groups:          kc,
		Repos:           landsRepos{app: app},
		Stewards:        steward.ProvisionHook{Client: daemon, Spec: steward.BindSpec{CreatedBy: provision.Author}},
		Mailer:          m,
		Templates:       mailer.NewTemplates(mailCfg),
		Logger:          logger,
		AccountTemplate: tmpl,
		DemoLand:        demoLand,
		DemoGuestTTL:    demoTTL,
		Gatehouse:       gatehouse,
		RuntimeClass:    get(landns.RuntimeClassEnv),
	}, nil
}

// landsKeycloakHTTPClient returns nil (keycloakadmin's default client) when
// caPath is empty, and otherwise a client that trusts the PEM CAs in caPath
// besides the system roots. A CA file that is unreadable or holds no
// certificate is an error, so the provisioner stays off and says why rather
// than failing every land at its keycloak step.
func landsKeycloakHTTPClient(caPath string, readFile func(string) ([]byte, error)) (*http.Client, error) {
	if caPath == "" {
		return nil, nil
	}
	pem, err := readFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("reading the Keycloak CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: %s holds no PEM certificate", landsKeycloakCAFileEnv, caPath)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: 10 * time.Second, Transport: tr}, nil
}

// newLandNamespaces builds the land namespace applier from the environment.
// It returns nil and no error when land namespaces are off, and an error
// naming what is wrong when they are on but cannot render.
func newLandNamespaces(k8s kubernetes.Interface, releaseNamespace string, getenv func(string) string) (*landns.Applier, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	if !landNamespacesEnabled(getenv) {
		return nil, nil
	}
	if k8s == nil {
		return nil, fmt.Errorf("%s is set but the controller has no Kubernetes client", landsNamespacesEnabledEnv)
	}
	cfg := landns.Config{ReleaseNamespace: releaseNamespace, ControllerServiceAccount: get(landsControllerSAEnv)}
	if raw := get(landsNamespaceEgressPeersEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Peers); err != nil {
			return nil, fmt.Errorf("%s: %w", landsNamespaceEgressPeersEnv, err)
		}
	}
	for _, c := range strings.Split(get(landsNamespaceExtraExceptsEnv), ",") {
		if c = strings.TrimSpace(c); c != "" {
			cfg.ExtraExceptCIDRs = append(cfg.ExtraExceptCIDRs, c)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s is set but %w", landsNamespacesEnabledEnv, err)
	}
	return &landns.Applier{Client: k8s, Config: cfg}, nil
}

// landsDynamicClient is the land scope writer's Kubernetes client, or nil
// (logged) when land scopes are off or the client cannot be built.
func landsDynamicClient(kubeConfig string, logger *slog.Logger) dynamic.Interface {
	if strings.TrimSpace(os.Getenv(landsKeycloakRealmResourceEnv)) == "" {
		return nil
	}
	cfg, err := buildK8sConfig(kubeConfig)
	if err == nil {
		var dyn dynamic.Interface
		if dyn, err = dynamic.NewForConfig(cfg); err == nil {
			return dyn
		}
	}
	logger.Error("lands provisioner: no Kubernetes client for land scopes", "error", err)
	return nil
}

// newLandScopes builds the land scope writer from the environment. It returns
// nil when LANDS_KEYCLOAK_REALM_RESOURCE is unset or there is no Kubernetes
// client: the land scope is not a land's fence, only its door.
func newLandScopes(dyn dynamic.Interface, releaseNamespace string, getenv func(string) string) *landscope.Applier {
	realm := strings.TrimSpace(getenv(landsKeycloakRealmResourceEnv))
	if realm == "" || dyn == nil {
		return nil
	}
	return &landscope.Applier{Client: dyn, Namespace: releaseNamespace, Realm: realm}
}

// startLandsProvisioner runs the provisioner when it is on and fully
// configured; otherwise it logs why it is not running.
func startLandsProvisioner(ctx context.Context, daemon *beadsapi.Client, k8s kubernetes.Interface, dyn dynamic.Interface, releaseNamespace, litellmURL, litellmMasterKey string, logger *slog.Logger) {
	p, err := newLandsProvisioner(ctx, daemon, litellmURL, litellmMasterKey, os.Getenv, os.ReadFile, logger)
	if err == nil && p != nil {
		if ls := newLandScopes(dyn, releaseNamespace, os.Getenv); ls != nil {
			p.LandScopes = ls
		} else {
			logger.Warn("lands provisioner: no land scopes (" + landsKeycloakRealmResourceEnv + " unset); land doors refuse every token")
		}
		var ns *landns.Applier
		ns, err = newLandNamespaces(k8s, releaseNamespace, os.Getenv)
		switch {
		case err != nil:
			p = nil
		case ns != nil:
			p.Namespaces = ns
		default:
			logger.Warn("lands provisioner: land namespaces are off; every land fails at its namespace step until " + landsNamespacesEnabledEnv + " is on")
		}
	}
	switch {
	case err != nil:
		logger.Error("lands provisioner: not started; approved plot applications wait", "error", err)
	case p != nil:
		logger.Info("lands provisioner: started", "land_namespaces", p.Namespaces != nil, "land_scopes", p.LandScopes != nil)
		go p.Run(ctx, provision.Interval)
	}
}
