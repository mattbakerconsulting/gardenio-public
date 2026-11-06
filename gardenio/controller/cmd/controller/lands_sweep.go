package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/landns"
	"gardenio/controller/internal/lands/landtoken"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/podmanager"
)

// landNamespacesEnabled reports whether the deployment turned land namespaces
// on (LANDS_NAMESPACES_ENABLED, with the chart's landNamespaces.enabled RBAC).
func landNamespacesEnabled(getenv func(string) string) bool {
	return strings.EqualFold(strings.TrimSpace(getenv(landsNamespacesEnabledEnv)), "true")
}

// tenantLandNamespaces is the namespaces of the tenant lands in the project
// cache whose namespace step has run: the project bead names the land's own
// land-<land>, the check the spawn wall makes before a tenant pod may spawn
// (landFenceRefusal). Those are the namespaces the land fence places tenant pods
// in, so the status reporter sweeps them every SyncAll (gd-Vuu1J7y4J0); without
// it a tenant agent's bead never leaves spawning. Sorted, so a sweep's order is
// stable.
func tenantLandNamespaces(cfg *config.Config) []string {
	lands := tenantLands(cfg)
	out := make([]string, 0, len(lands))
	for _, land := range lands {
		ns, _ := landns.Name(land)
		out = append(out, ns)
	}
	slices.Sort(out)
	return out
}

// tenantLands is the lands of tenantLandNamespaces, sorted.
func tenantLands(cfg *config.Config) []string {
	cfg.ProjectCacheMu.RLock()
	defer cfg.ProjectCacheMu.RUnlock()
	var out []string
	for project, entry := range cfg.ProjectCache {
		if isTenantLandEntry(project, entry) {
			out = append(out, project)
		}
	}
	slices.Sort(out)
	return out
}

// isTenantLandEntry is tenantLands' test for one project cache entry.
func isTenantLandEntry(project string, entry config.ProjectCacheEntry) bool {
	if !entry.IsTenantLand() {
		return false
	}
	ns, err := landns.Name(project)
	return err == nil && entry.Namespace == ns
}

// landCoopmuxTokenMinter mints a land's coopmux token under
// cfg.CoopmuxLandTokenKeyFile, read at every mint so a rotated key reaches the
// next pod and the next sweep. nil when no key file is set: tenant pods then
// get no coopmux token (fenceTenantEnv).
func landCoopmuxTokenMinter(cfg *config.Config) podmanager.LandCoopmuxTokenMinter {
	path := cfg.CoopmuxLandTokenKeyFile
	if path == "" {
		return nil
	}
	return func(land string) (string, error) {
		key, err := landtoken.LoadKey(path)
		if err != nil {
			return "", err
		}
		return landtoken.Mint(key, land)
	}
}

// landCredentialSecret is the Secret that is a tenant land's own Claude
// credential: lands-<land>, named for its account, in the release namespace
// (gd-ApgYe740ps). A land account may read it, or the fleet credential
// lands-template carries (owner ruling gd-NXTrUXD1Sq: the Claude credential
// stays); never another land's.
func landCredentialSecret(land string) string {
	return purse.AccountName(land)
}

// landTemplateCredential is the Secret the LANDS_ACCOUNT_TEMPLATE account's
// credential names, the fleet Claude credential every lands-<land> account
// copies, or "" when the template has none or it is not a Secret.
// templateKnown is false when no template is set (the chart sets it only
// with the provisioner, and the sweep runs whenever land namespaces are on)
// or it is not in the account cache (a failed first account load, say), so a
// caller can tell "no template credential" from "cannot tell" and the sweep
// leaves the copies running pods read alone. Takes cfg.ProjectCacheMu.
func landTemplateCredential(cfg *config.Config) (name string, templateKnown bool) {
	if cfg.LandsAccountTemplate == "" {
		return "", false
	}
	cfg.ProjectCacheMu.RLock()
	tmpl, ok := cfg.AccountCache[cfg.LandsAccountTemplate]
	cfg.ProjectCacheMu.RUnlock()
	if !ok {
		return "", false
	}
	name, _ = strings.CutPrefix(tmpl.Credential, "secret:")
	return name, true
}

// landCredential reports the Secret acct, land's lands-<land> account, reads
// its credential from, and whether the land's pods may read it. Only two
// Secrets may: the land's own, lands-<land> (landCredentialSecret), and the
// template account's credential (templateCredential, landTemplateCredential),
// the fleet Claude credential the owner ruled must stay (gd-NXTrUXD1Sq);
// neither may be a forbidden name. Every other Secret, the fleet coopmux or
// daemon token say, and every non-Secret credential is refused, so the fence
// fails closed. An account with no credential (LiteLLM key only) reports
// "", true.
func landCredential(land, templateCredential string, acct config.AccountCacheEntry) (string, bool) {
	if acct.Credential == "" {
		return "", true
	}
	kind, name, ok := strings.Cut(acct.Credential, ":")
	allowed := name == landCredentialSecret(land) || (templateCredential != "" && name == templateCredential)
	return name, ok && kind == "secret" && allowed && !isForbiddenPodSecret(name)
}

// landSecretsFor is what land's namespace may hold (podmanager.LandSecrets):
// a copy of its account's credential (landCredential), when it reads one, its
// coopmux token, when mint is set, and its NATS password, when
// landNatsEnabled. The same rule fenceTenantEnv applies to a
// pod, so a sweep never keeps what no new pod would get. With no cache entry
// for the land's account or the template account (a failed first account
// load, say) the copies are unknown, and a failed mint leaves that token
// unknown and is returned, so a sweep leaves that part alone rather than
// delete what running pods read.
func landSecretsFor(cfg *config.Config, land string, mint podmanager.LandCoopmuxTokenMinter) (podmanager.LandSecrets, error) {
	var want podmanager.LandSecrets
	cfg.ProjectCacheMu.RLock()
	acct, ok := cfg.AccountCache[purse.AccountName(land)]
	cfg.ProjectCacheMu.RUnlock()
	tmplCred, tmplKnown := landTemplateCredential(cfg)
	switch name, allowed := landCredential(land, tmplCred, acct); {
	case !ok || !tmplKnown:
		want.CopiesUnknown = true
	case allowed && name != "" && cfg.Namespace != "":
		want.Copies = []podmanager.LandSecretCopy{{SourceNamespace: cfg.Namespace, Name: name, Keys: []string{"token"}}}
	}
	var errs []error
	if mint != nil && cfg.CoopmuxURL != "" {
		token, err := mint(land)
		if err != nil {
			want.CoopmuxTokenUnknown = true
			errs = append(errs, fmt.Errorf("coopmux token: %w", err))
		}
		want.CoopmuxToken = token
	}
	if natsMint := landNatsTokenMinter(cfg); natsMint != nil {
		token, err := natsMint(land)
		if err != nil {
			want.NatsTokenUnknown = true
			errs = append(errs, fmt.Errorf("nats password: %w", err))
		}
		want.NatsToken = token
	}
	return want, errors.Join(errs...)
}

// landSecretSyncInterval is how often runLandSecretSync brings the tenant
// lands' Secrets in line.
const landSecretSyncInterval = 5 * time.Minute

// runLandSecretSync keeps each tenant land namespace's land Secrets
// (podmanager.SyncLandSecrets, gd-gokoYMl5px) in line with landSecretsFor: a
// rotated credential or land token key reaches them within interval, and a
// copy the land may no longer hold, or whose source is gone, is deleted. A pod
// create writes them afresh as well, so a new pod never waits for this sweep.
// They go with the namespace when gb lands close deletes it.
func runLandSecretSync(ctx context.Context, cfg *config.Config, k8s kubernetes.Interface, logger *slog.Logger, interval time.Duration) {
	logger = logger.With("component", "land-secret-sync")
	mint := landCoopmuxTokenMinter(cfg)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		syncLandSecrets(ctx, cfg, k8s, mint, logger)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncLandSecrets is one runLandSecretSync pass over the tenant lands in the
// project cache. A land whose token cannot be minted keeps that token until
// the next pass; the rest is synced all the same.
func syncLandSecrets(ctx context.Context, cfg *config.Config, k8s kubernetes.Interface, mint podmanager.LandCoopmuxTokenMinter, logger *slog.Logger) {
	for _, land := range tenantLands(cfg) {
		ns, _ := landns.Name(land)
		want, err := landSecretsFor(cfg, land, mint)
		if err != nil {
			logger.Warn("land token not minted; left as it is", "land", land, "error", err)
		}
		if err := podmanager.SyncLandSecrets(ctx, k8s, ns, want); err != nil {
			logger.Warn("land secrets not synced", "namespace", ns, "error", err)
		}
	}
}
