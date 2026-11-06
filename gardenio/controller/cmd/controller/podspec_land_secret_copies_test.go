package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/landtoken"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// gd-gokoYMl5px, gd-ApgYe740ps: a tenant pod in land-<land> reads two Secrets
// the doors do not mint there: its land's coopmux token, which the controller
// mints into the land namespace, and the Claude credential its lands-<land>
// account names (the fleet's, which stays by owner ruling gd-NXTrUXD1Sq, or
// the land's own lands-<land>), copied from the release namespace with the one
// key it is read by. The static coopmux token, a NATS token and another land's
// credential are never copied in.

func wantTenantSecretCopies() []podmanager.LandSecretCopy {
	return []podmanager.LandSecretCopy{{SourceNamespace: "gardenio", Name: "lands-acme", Keys: []string{"token"}}}
}

func TestTenantSecretCopies_OnlyTheLandsOwnCredential(t *testing.T) {
	for _, fill := range []bool{false, true} {
		for _, role := range []string{"plain", "escape"} {
			cfg := allowlistCfg(fill)
			specs := map[string]podmanager.AgentPodSpec{
				"event": buildAgentPodSpec(cfg, subscriber.Event{
					Project: "acme", Role: role, AgentName: "acme-crew-1", Mode: "crew", BeadID: allowlistBead, Metadata: allowlistPins(),
				}),
				"reconciler": BuildSpecFromBeadInfo(cfg, "acme", "crew", role, "acme-crew-1", allowlistPins()),
			}
			for door, spec := range specs {
				if !reflect.DeepEqual(spec.LandSecretCopies, wantTenantSecretCopies()) {
					t.Errorf("fill=%v role=%s %s: LandSecretCopies = %+v, want %+v", fill, role, door, spec.LandSecretCopies, wantTenantSecretCopies())
				}
				if !spec.LandCoopmuxToken || spec.SpawnBlocked {
					t.Errorf("fill=%v role=%s %s: LandCoopmuxToken=%v SpawnBlocked=%v (%s), want the land token and no block",
						fill, role, door, spec.LandCoopmuxToken, spec.SpawnBlocked, spec.SpawnBlockReason)
				}
			}
		}
	}
}

// Without the land's own account the credential is fenced off, so it is not
// copied either; an operator land's pod copies nothing.
func TestTenantSecretCopies_OnlyWhatTheFenceKeeps(t *testing.T) {
	cfg := allowlistCfg(false)
	delete(cfg.AccountCache, "lands-acme")
	spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
	if len(spec.LandSecretCopies) != 0 {
		t.Errorf("copies %+v without the land account, want none", spec.LandSecretCopies)
	}

	op := BuildSpecFromBeadInfo(allowlistCfg(false), "gardenio", "crew", "escape", "gd-crew-1", map[string]string{})
	if len(op.LandSecretCopies) != 0 || op.LandCoopmuxToken {
		t.Errorf("operator pod copies %+v, land token %v; want neither", op.LandSecretCopies, op.LandCoopmuxToken)
	}
}

// A land account that reads the template account's credential (the fleet
// claude-oauth) spawns with it: the Secret it names is read as the Claude
// token and copied into the land namespace (owner ruling gd-NXTrUXD1Sq).
func TestTenantCredential_FleetSecretIsCopied(t *testing.T) {
	cfg := allowlistCfg(false)
	acct := cfg.AccountCache["lands-acme"]
	acct.Credential = "secret:claude-oauth"
	cfg.AccountCache["lands-acme"] = acct
	spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
	if spec.SpawnBlocked {
		t.Fatalf("fleet credential: SpawnBlocked %q, want a spawn", spec.SpawnBlockReason)
	}
	var read []string
	for _, se := range spec.SecretEnv {
		if se.EnvName == claudeOAuthEnvName {
			read = append(read, se.SecretName+"/"+se.SecretKey)
		}
	}
	if !reflect.DeepEqual(read, []string{"claude-oauth/token"}) {
		t.Errorf("tenant pod reads %s from %v, want claude-oauth/token", claudeOAuthEnvName, read)
	}
	want := []podmanager.LandSecretCopy{{SourceNamespace: "gardenio", Name: "claude-oauth", Keys: []string{"token"}}}
	if !reflect.DeepEqual(spec.LandSecretCopies, want) {
		t.Errorf("copies %+v, want %+v", spec.LandSecretCopies, want)
	}
	assertTenantPodAllowed(t, cfg, renderTenantPod(spec))
}

// A land account that reads any Secret but its own or the template's (the
// fleet coopmux or daemon token, another land's, a forbidden name), or a
// credential that is not a Secret, blocks the spawn: the tenant pod gets no
// credential, and nothing is copied. With no template account in the cache
// only the land's own passes.
func TestTenantCredential_OtherSecretBlocksTheSpawn(t *testing.T) {
	base := allowlistCfg(false)
	creds := []string{"secret:" + base.CoopmuxTokenSecret, "secret:" + base.BeadsTokenSecret, "secret:" + base.ClaudeOAuthSecret,
		"secret:lands-beta", "secret:lands-acme-extra", "secret:lands-github-app-key", "env:TOKEN", "secret:",
		"secret:claude-oauth|no-template", "secret:claude-oauth|template-unset"}
	for _, cred := range creds {
		cfg := allowlistCfg(false)
		if strings.HasSuffix(cred, "|no-template") {
			cred = strings.TrimSuffix(cred, "|no-template")
			delete(cfg.AccountCache, cfg.LandsAccountTemplate)
		}
		if strings.HasSuffix(cred, "|template-unset") {
			cred = strings.TrimSuffix(cred, "|template-unset")
			cfg.LandsAccountTemplate = ""
		}
		acct := cfg.AccountCache["lands-acme"]
		acct.Credential = cred
		cfg.AccountCache["lands-acme"] = acct
		spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
		if !spec.SpawnBlocked || !strings.Contains(spec.SpawnBlockReason, "secret:lands-acme") {
			t.Errorf("%s: SpawnBlocked=%v reason %q, want a block naming secret:lands-acme", cred, spec.SpawnBlocked, spec.SpawnBlockReason)
		}
		for _, se := range spec.SecretEnv {
			if se.EnvName == claudeOAuthEnvName {
				t.Errorf("%s: tenant pod reads %s from %s", cred, se.EnvName, se.SecretName)
			}
		}
		if len(spec.LandSecretCopies) != 0 {
			t.Errorf("%s: copies %+v, want none", cred, spec.LandSecretCopies)
		}
	}

	// No credential at all: the land bills through its LiteLLM key alone.
	cfg := allowlistCfg(false)
	acct := cfg.AccountCache["lands-acme"]
	acct.Credential = ""
	cfg.AccountCache["lands-acme"] = acct
	spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
	if spec.SpawnBlocked || len(spec.LandSecretCopies) != 0 {
		t.Errorf("keyless land: SpawnBlocked=%v copies=%+v, want a spawn and no copy", spec.SpawnBlocked, spec.LandSecretCopies)
	}
}

// With no land token key the tenant pod gets no coopmux token at all, never
// the fleet's, and no coopmux URL to register at. The chart default falls the
// fleet token back to the shared daemon token, which must not reach it
// either.
func TestTenantCoopmux_NoKeyNoToken(t *testing.T) {
	cfg := allowlistCfg(false)
	cfg.CoopmuxLandTokenKeyFile = ""
	cfg.CoopmuxTokenSecret = cfg.BeadsTokenSecret
	spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
	if spec.LandCoopmuxToken {
		t.Error("LandCoopmuxToken with no key")
	}
	for _, se := range spec.SecretEnv {
		if strings.HasPrefix(se.EnvName, "COOP_") || se.SecretName == cfg.BeadsTokenSecret {
			t.Errorf("tenant pod reads %s from %s with no land key", se.EnvName, se.SecretName)
		}
	}
	for _, k := range []string{"COOP_MUX_URL", "COOP_BROKER_URL"} {
		if v, set := spec.Env[k]; set {
			t.Errorf("tenant pod has %s=%q with no land token", k, v)
		}
	}
	assertTenantPodAllowed(t, cfg, renderTenantPod(spec))
}

// A lands-<land> Secret is readable only as the credential of the account
// of the same name: a project or role secret, or another account, naming it
// is refused even when the allow-list would pass it.
func TestRefuseBeadSecret_LandCredential(t *testing.T) {
	cfg := &config.Config{AgentSecretAllowlist: []string{"lands-*", "claude-oauth"}}
	cases := []struct {
		source, owner, secret string
		refuse                bool
	}{
		{"account", "lands-acme", "lands-acme", false},
		{"account", "lands-beta", "lands-acme", true},
		{"account", "fleet-acct", "lands-acme", true},
		{"project", "acme", "lands-acme", true},
		{"role", "crew", "lands-acme", true},
		{"account", "fleet-acct", "claude-oauth", false},
	}
	for _, c := range cases {
		if got := refuseBeadSecret(cfg, c.source, c.owner, c.secret, claudeOAuthEnvName); got != c.refuse {
			t.Errorf("refuseBeadSecret(%s %s, %s) = %v, want %v", c.source, c.owner, c.secret, got, c.refuse)
		}
	}
	// The land's own needs no allow-list entry.
	if refuseBeadSecret(&config.Config{AgentSecretAllowlist: []string{"claude-oauth"}}, "account", "lands-acme", "lands-acme", claudeOAuthEnvName) {
		t.Error("lands-acme refused as lands-acme's own credential under an allow-list that does not name it")
	}
}

// The admission path end to end: with the land's credential only in the
// release namespace and the doors' per-agent Secrets in land-acme,
// CreateAgentPod mints the land's coopmux token and NATS password, copies the
// credential's read key alone, and creates the pod.
func TestTenantPodCreate_WritesLandSecrets(t *testing.T) {
	cfg := allowlistCfg(false)
	cfg.NatsLandCalloutIssuerSeedFile = "/etc/nats-land-callout/seed"
	spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
	spec.BeadID = allowlistBead
	spec.DaemonTokenSecret = agentTokenSecretName(allowlistBead)
	spec.GitCredentialsSecret = githubTokenSecretName(allowlistBead)

	secret := func(ns, name string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string][]byte{"token": []byte(name + "-value"), "fleet-extra": []byte("fleet")},
		}
	}
	client := fake.NewSimpleClientset(
		secret("gardenio", cfg.CoopmuxTokenSecret),
		secret("gardenio", cfg.NatsAgentUserSecret),
		secret("gardenio", "lands-acme"),
		secret("land-acme", spec.DaemonTokenSecret),
		secret("land-acme", spec.GitCredentialsSecret),
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := podmanager.New(client, logger)
	m.MintLandCoopmuxToken = func(land string) (string, error) { return "coopmux-land.v1." + land + ".mac", nil }
	m.MintLandNatsToken = func(land string) (string, error) { return "nats-land.v1." + land + ".mac", nil }
	ctx := context.Background()

	if r := evaluateSpawnCredentials(ctx, client, logger, spec); r.refuse {
		t.Fatalf("credential guard refused the tenant spawn: %s", r.reason)
	}
	if err := m.CreateAgentPod(ctx, spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}
	if _, err := client.CoreV1().Pods("land-acme").Get(ctx, spec.PodName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("pod not created in land-acme: %v", err)
	}
	want := map[string]map[string][]byte{
		"lands-acme":                      {"token": []byte("lands-acme-value")},
		podmanager.LandCoopmuxTokenSecret: {"token": []byte("coopmux-land.v1.acme.mac")},
		podmanager.LandNatsTokenSecret:    {"token": []byte("nats-land.v1.acme.mac")},
	}
	for name, data := range want {
		got, err := client.CoreV1().Secrets("land-acme").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(got.Data, data) {
			t.Errorf("%s data = %q, want %q", name, got.Data, data)
		}
	}
	for _, name := range []string{cfg.CoopmuxTokenSecret, cfg.NatsAgentUserSecret} {
		if _, err := client.CoreV1().Secrets("land-acme").Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("fleet Secret %s copied into land-acme: %v", name, err)
		}
	}
}

// The sweep keeps a land to what landSecretsFor allows: the Secret its
// account names, the land's own or the fleet's; once the account names
// another land's, the copy goes.
func TestLandSecretsFor(t *testing.T) {
	cfg := allowlistCfg(false)
	mint := func(land string) (string, error) { return "tok-" + land, nil }
	got, err := landSecretsFor(cfg, "acme", mint)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, podmanager.LandSecrets{Copies: wantTenantSecretCopies(), CoopmuxToken: "tok-acme"}) {
		t.Errorf("landSecretsFor = %+v", got)
	}
	// With land NATS users on, the land's NATS password under the key.
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	natsCfg := allowlistCfg(false)
	natsCfg.CoopmuxLandTokenKeyFile = keyFile
	natsCfg.NatsLandCalloutIssuerSeedFile = "/etc/nats-land-callout/seed"
	if got, err := landSecretsFor(natsCfg, "acme", mint); err != nil || got.NatsToken == "" || got.NatsTokenUnknown {
		t.Errorf("land NATS on: landSecretsFor = %+v, %v; want the land's NATS password", got, err)
	} else if err := landtoken.VerifyNATS([]byte("test-key"), got.NatsToken, "acme", 0); err != nil {
		t.Errorf("NATS password %q is not acme's at epoch 0: %v", got.NatsToken, err)
	}
	// A bumped nats_epoch mints the new epoch's password; one that does not
	// parse mints none, and the sweep leaves the Secret as it is.
	entry := natsCfg.ProjectCache["acme"]
	entry.NatsEpoch = "2"
	natsCfg.ProjectCache["acme"] = entry
	if got, err := landSecretsFor(natsCfg, "acme", mint); err != nil {
		t.Errorf("nats_epoch 2: %v", err)
	} else if err := landtoken.VerifyNATS([]byte("test-key"), got.NatsToken, "acme", 2); err != nil {
		t.Errorf("nats_epoch 2: password %q is not acme's at epoch 2: %v", got.NatsToken, err)
	}
	entry.NatsEpoch = "two"
	natsCfg.ProjectCache["acme"] = entry
	if got, err := landSecretsFor(natsCfg, "acme", mint); err == nil || !got.NatsTokenUnknown || got.NatsToken != "" {
		t.Errorf("nats_epoch two: landSecretsFor = %+v, %v; want the NATS password unknown", got, err)
	}
	acct := cfg.AccountCache["lands-acme"]
	acct.Credential = "secret:claude-oauth"
	cfg.AccountCache["lands-acme"] = acct
	fleet := []podmanager.LandSecretCopy{{SourceNamespace: "gardenio", Name: "claude-oauth", Keys: []string{"token"}}}
	if got, _ := landSecretsFor(cfg, "acme", nil); !reflect.DeepEqual(got.Copies, fleet) || got.CoopmuxToken != "" {
		t.Errorf("fleet credential, no minter: landSecretsFor = %+v, want the fleet copy and no token", got)
	}
	for _, cred := range []string{"secret:lands-beta", "secret:" + cfg.CoopmuxTokenSecret} {
		acct.Credential = cred
		cfg.AccountCache["lands-acme"] = acct
		if got, _ := landSecretsFor(cfg, "acme", nil); len(got.Copies) != 0 || got.CopiesUnknown {
			t.Errorf("%s: landSecretsFor = %+v, want no copies", cred, got)
		}
	}
}

// The sweep never deletes on what it cannot tell: with the land's account
// missing from the cache the copies are unknown, and a failed mint leaves the
// token unknown while the copies are still reconciled.
func TestLandSecretsFor_UnknownHalves(t *testing.T) {
	cfg := allowlistCfg(false)
	delete(cfg.AccountCache, "lands-acme")
	got, err := landSecretsFor(cfg, "acme", nil)
	if err != nil || !got.CopiesUnknown || len(got.Copies) != 0 {
		t.Errorf("no account cache entry: landSecretsFor = %+v, %v; want unknown copies", got, err)
	}
	cfg = allowlistCfg(false)
	delete(cfg.AccountCache, cfg.LandsAccountTemplate)
	got, err = landSecretsFor(cfg, "acme", nil)
	if err != nil || !got.CopiesUnknown || len(got.Copies) != 0 {
		t.Errorf("no template account cache entry: landSecretsFor = %+v, %v; want unknown copies", got, err)
	}
	// LANDS_ACCOUNT_TEMPLATE unset (the provisioner is off, the sweep still
	// runs): a land on the fleet credential keeps its copy.
	cfg = allowlistCfg(false)
	cfg.LandsAccountTemplate = ""
	acct := cfg.AccountCache["lands-acme"]
	acct.Credential = "secret:claude-oauth"
	cfg.AccountCache["lands-acme"] = acct
	got, err = landSecretsFor(cfg, "acme", nil)
	if err != nil || !got.CopiesUnknown || len(got.Copies) != 0 {
		t.Errorf("template unset: landSecretsFor = %+v, %v; want unknown copies", got, err)
	}

	// Land NATS users on, the key unreadable: both tokens are unknown, the
	// copies still known.
	cfg = allowlistCfg(false)
	cfg.NatsLandCalloutIssuerSeedFile = "/etc/nats-land-callout/seed"
	cfg.CoopmuxLandTokenKeyFile = filepath.Join(t.TempDir(), "missing")
	got, err = landSecretsFor(cfg, "acme", landCoopmuxTokenMinter(cfg))
	if err == nil || !got.CoopmuxTokenUnknown || !got.NatsTokenUnknown || got.CopiesUnknown {
		t.Errorf("unreadable key: landSecretsFor = %+v, %v; want both tokens unknown and the known copies", got, err)
	}

	cfg = allowlistCfg(false)
	failing := func(string) (string, error) { return "", errors.New("key unreadable") }
	got, err = landSecretsFor(cfg, "acme", failing)
	if err == nil || !got.CoopmuxTokenUnknown || got.CopiesUnknown || !reflect.DeepEqual(got.Copies, wantTenantSecretCopies()) {
		t.Errorf("failed mint: landSecretsFor = %+v, %v; want an unknown token and the known copies", got, err)
	}
}

// End to end through syncLandSecrets: a failed mint does not stop a stale
// credential copy going, and an empty account cache does not delete the
// copy running pods read.
func TestSyncLandSecrets_SweepRobustness(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	setup := func() (*config.Config, *fake.Clientset) {
		cfg := allowlistCfg(false)
		client := fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "lands-acme", Namespace: "gardenio"},
			Data:       map[string][]byte{"token": []byte("o1")},
		})
		if _, err := podmanager.CopyLandSecret(ctx, client, "land-acme", wantTenantSecretCopies()[0]); err != nil {
			t.Fatal(err)
		}
		if err := podmanager.WriteLandCoopmuxToken(ctx, client, "land-acme", "acme", "tok-1"); err != nil {
			t.Fatal(err)
		}
		return cfg, client
	}
	exists := func(client *fake.Clientset, name string) bool {
		_, err := client.CoreV1().Secrets("land-acme").Get(ctx, name, metav1.GetOptions{})
		return err == nil
	}

	cfg, client := setup()
	acct := cfg.AccountCache["lands-acme"]
	acct.Credential = "secret:claude-oauth"
	cfg.AccountCache["lands-acme"] = acct
	syncLandSecrets(ctx, cfg, client, func(string) (string, error) { return "", errors.New("key unreadable") }, logger)
	if exists(client, "lands-acme") {
		t.Error("failed mint: the copy the land may no longer hold was kept")
	}
	if !exists(client, podmanager.LandCoopmuxTokenSecret) {
		t.Error("failed mint: the land token was deleted")
	}

	cfg, client = setup()
	cfg.AccountCache = map[string]config.AccountCacheEntry{}
	syncLandSecrets(ctx, cfg, client, nil, logger)
	if !exists(client, "lands-acme") {
		t.Error("empty account cache: the credential copy was deleted")
	}
}
