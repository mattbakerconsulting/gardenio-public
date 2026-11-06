// Per-agent GitHub App installation tokens (task gd-fzNBBfmdEO, epic
// gd-BHLDnjX1Hw, plan gardenio/plans/github-app-onboarding.md §4.2/§4.3).
//
// Modelled on microvm_git_credentials.go (mint → deliver → revoke) and
// agenttoken.go (per-agent K8s Secret). Behind agents.githubApp.enabled
// (GITHUB_APP_ENABLED), default off.
//
// Flow, for an agent whose project GitURL is on github.com:
//  1. Resolve owner/repo (and same-owner reference repos) from the AgentPodSpec.
//  2. Mint an installation token narrowed to exactly those repositories.
//  3. Write it to Secret agent-github-token-<beadID> (keys token + username
//     "x-access-token") in the pod's namespace, and point
//     spec.GitCredentialsSecret at it. The pod already projects that Secret at
//     /etc/git-creds/github/{username,token}; the agent side needs no change.
//  4. The sweep re-mints before expiry (kubelet re-projects the file; the
//     in-pod rewrite of ~/.git-credentials is gd-uPJ6AEsfyn) and revokes
//     tokens whose bead closed.
//  5. On done/kill/stop/stuck the token is revoked at GitHub and the Secret
//     deleted.
//
// FAIL CLOSED: when enabled, a GitHub-hosted project whose token cannot be
// minted is NOT spawned with the shared credential. The spawn is refused
// (retried by the reconciler) and an escalation is filed. Projects whose
// GitURL is not on github.com (GitLab etc.) are not this provider's concern
// and keep their configured credentials; that is not a fallback.
//
// TWO APPS (Lands Track B spec §3.2/§7, step 8.7): a project's github_app
// field names the App that mints its tokens. "fleet" (the default) is the
// operator's App above, scoped to the repo plus same-owner reference repos.
// "lands" is the Lands App a tenant installed on its one repo: the token is
// minted from the project's pinned github_installation_id (never looked up)
// and covers that one repo only, never reference repos. Before each Lands
// mint and refresh the installation is read back and must be on the account
// that owns the repo, and GitHub must place the repo in that installation
// (verifyLandsInstallation), and neither may be an operator org named in
// LANDS_OPERATOR_GITHUB_ORGS (the daemon refuses that pin at write time too,
// gd-72m3STiqya); the daemon lets only an admin or the Lands
// provisioner write github_app, github_installation_id, or a tenant land's
// git_url and repos (kbeads land_switch_authz.go). A tenant land
// (land_kind other than operator) can only ever mint with the Lands App: a
// tenant project naming the fleet App, or no App, is refused, as is an
// unknown github_app value. The Lands App is read from LANDS_GITHUB_APP_ID and
// LANDS_GITHUB_APP_PRIVATE_KEY_PATH (the variables the Lands provisioner
// reads); unconfigured, every Lands-minted spawn is refused.
//
// Credential hygiene: the private key is read from a mounted file (never an
// env var); no token, JWT or key appears in a log line, error string, bead
// field, label or annotation. The only things recorded are non-secret: the
// Secret name, bead id, repo list, and expiry.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/reconciler"
)

const (
	tokenProviderGithub = "github"

	githubHost = "github.com"

	// githubTokenUsername is GitHub's documented HTTPS username for
	// installation-token auth; agent_k8s_workspace.go already defaults to it.
	githubTokenUsername = "x-access-token"

	// githubTokenRefreshWindow: re-mint when less than this remains. Tokens
	// live 60 min and the sweep runs every ≤5 min, so a token is replaced
	// between ~40 and ~45 min in, leaving the kubelet re-projection (~60s) and
	// the in-pod rewrite ample time before expiry.
	githubTokenRefreshWindow = 20 * time.Minute

	annotationGithubExpiresAt = "gardenio.io/github-token-expires-at"
	annotationGithubRepos     = "gardenio.io/github-repos"
	// annotationGithubApp records which App minted the token (absent on
	// Secrets written before step 8.7: those are the fleet App's), and
	// annotationGithubInstallation the installation it was minted from.
	annotationGithubApp          = "gardenio.io/github-app"
	annotationGithubInstallation = "gardenio.io/github-installation-id"

	githubOpTimeout = 30 * time.Second
)

// githubAppAPI is the slice of *githubapp.Client the issuer uses; faked in tests.
type githubAppAPI interface {
	InstallationFor(ctx context.Context, repo githubapp.RepoRef) (int64, error)
	MintInstallationToken(ctx context.Context, installationID int64, repoNames []string) (githubapp.Token, error)
	Revoke(ctx context.Context, tok githubapp.Token) error
	Installation(ctx context.Context, installationID int64) (githubapp.Installation, error)
	RepoInstallation(ctx context.Context, repo githubapp.RepoRef) (int64, error)
	RepoSizeKB(ctx context.Context, installationID int64, repo githubapp.RepoRef) (int64, error)
}

// githubAppIssuer mints, refreshes and revokes per-agent installation tokens.
// A nil *githubAppIssuer means the feature is disabled.
type githubAppIssuer struct {
	app            githubAppAPI // fleet App; nil when initErr != nil or fleetOff
	initErr        error        // non-nil: enabled but unusable → every GitHub spawn is refused
	installationID int64        // 0 = look up per repo
	// fleetOff: GITHUB_APP_ENABLED is false and the issuer exists only for the
	// Lands App. Projects on the fleet App are then not this provider's
	// concern and keep their configured credentials, as with no issuer at all.
	fleetOff bool

	lands    githubAppAPI // Lands App; nil when landsErr != nil
	landsErr error        // non-nil: every Lands-minted spawn is refused

	// operatorOrgs are the operator's GitHub accounts, lower-cased
	// (LANDS_OPERATOR_GITHUB_ORGS, chart agents.lands.operatorGithubOrgs). A
	// Lands installation on one of them is never minted from, so the Lands
	// App can never hand a land a token for an operator repo.
	operatorOrgs map[string]bool

	// maxRepoSizeKB is the largest repository a Lands token is minted for
	// (githubapp.MaxRepoSizeEnv, the limit the setup check applied); 0 is no
	// limit.
	maxRepoSizeKB int64

	// project reads a project's cache entry (land_kind, github_app,
	// github_installation_id). A project it does not know is refused, never
	// assumed to be the operator's.
	project func(name string) (config.ProjectCacheEntry, bool)

	// landHalt reads a land's kill switches live (land_frozen, the
	// lands-pause gate, a land gone or unreadable: wall.Halted). A refused
	// land gets no token at mint, reuse or refresh. Nil checks nothing.
	landHalt func(ctx context.Context, project string) wall.Refusal

	k8s    kubernetes.Interface
	logger *slog.Logger
	tm     *tokenMetrics
	now    func() time.Time

	// created records beads whose per-agent Secret THIS process created in the
	// latest provision (value: struct{}). Only such a token may be revoked when
	// CreateAgentPod then fails (revokeIfCreated): a duplicate spawn that reused
	// a live agent's Secret, or merely refreshed one, must not cut the running
	// pod's git access because its own create hit a transient error. Process-
	// local on purpose: after a restart nothing is revoked, which is the safe
	// direction (the token lapses within the hour or the sweep retires it).
	created sync.Map
}

// newGithubAppIssuer returns nil when the feature is off: GITHUB_APP_ENABLED
// is false and no LANDS_GITHUB_APP_* variable is set. When on but an App's
// credentials cannot be loaded it returns a non-nil issuer that refuses every
// spawn that App would mint for (fail closed) rather than crashing the
// controller and taking other projects down with it.
func newGithubAppIssuer(cfg *config.Config, k8s kubernetes.Interface, logger *slog.Logger, tm *tokenMetrics) *githubAppIssuer {
	return newGithubAppIssuerFrom(cfg, k8s, logger, tm, os.Getenv, os.ReadFile)
}

func newGithubAppIssuerFrom(cfg *config.Config, k8s kubernetes.Interface, logger *slog.Logger, tm *tokenMetrics,
	getenv func(string) string, readFile func(string) ([]byte, error)) *githubAppIssuer {
	if cfg == nil {
		return nil
	}
	landsCfg, landsOK, landsErr := githubapp.ConfigFromEnv(githubapp.Lands, landsGitHubAppEnvPrefix, false, getenv, readFile)
	if !cfg.GithubAppEnabled && !landsOK && landsErr == nil {
		return nil
	}
	iss := &githubAppIssuer{k8s: k8s, logger: logger, tm: tm, now: time.Now, project: projectCacheLookup(cfg),
		operatorOrgs:  parseOperatorOrgs(getenv(landsOperatorGitHubOrgsEnv)),
		maxRepoSizeKB: githubapp.MaxRepoSizeMB(getenv) * 1024}
	switch {
	case landsErr != nil:
		iss.landsErr = landsErr
		logger.Error("lands github app misconfigured; spawns minted by the Lands App will be refused", "error", landsErr)
	case !landsOK:
		iss.landsErr = fmt.Errorf("%w: %s (set %s_ID and %s_PRIVATE_KEY_PATH)", githubapp.ErrNotConfigured, githubapp.Lands, landsGitHubAppEnvPrefix, landsGitHubAppEnvPrefix)
	default:
		if client, err := githubapp.New(landsCfg.Config); err != nil {
			iss.landsErr = fmt.Errorf("lands github app client: %w", err)
			logger.Error("lands github app misconfigured; spawns minted by the Lands App will be refused", "error", iss.landsErr)
		} else {
			iss.lands = client
			logger.Info("lands github app per-agent tokens enabled", "app_id", landsCfg.AppID)
		}
	}
	if !cfg.GithubAppEnabled {
		iss.fleetOff = true
		return iss
	}
	pemBytes, err := os.ReadFile(cfg.GithubAppPrivateKeyFile)
	if err != nil {
		// Do not echo the path's error text beyond the OS reason; it holds no secret.
		iss.initErr = fmt.Errorf("github app private key unreadable at %q: %w", cfg.GithubAppPrivateKeyFile, errors.Unwrap(err))
		logger.Error("github app enabled but private key cannot be read; GitHub-hosted spawns will be refused", "error", iss.initErr)
		return iss
	}
	client, err := githubapp.New(githubapp.Config{AppID: cfg.GithubAppID, PrivateKey: pemBytes, BaseURL: cfg.GithubAppAPIURL})
	if err != nil {
		iss.initErr = fmt.Errorf("github app client: %w", err)
		logger.Error("github app enabled but misconfigured; GitHub-hosted spawns will be refused", "error", iss.initErr)
		return iss
	}
	iss.app = client
	iss.installationID = cfg.GithubAppInstallationID
	logger.Info("github app per-agent tokens enabled", "app_id", cfg.GithubAppID, "installation_id", cfg.GithubAppInstallationID)
	return iss
}

// parseOperatorOrgs reads a comma-separated LANDS_OPERATOR_GITHUB_ORGS into a
// lower-cased set (GitHub logins are case-insensitive).
func parseOperatorOrgs(v string) map[string]bool {
	orgs := map[string]bool{}
	for _, o := range strings.Split(v, ",") {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" {
			orgs[o] = true
		}
	}
	return orgs
}

// projectCacheLookup reads cfg.ProjectCache under its lock.
func projectCacheLookup(cfg *config.Config) func(string) (config.ProjectCacheEntry, bool) {
	return func(name string) (config.ProjectCacheEntry, bool) {
		cfg.ProjectCacheMu.RLock()
		defer cfg.ProjectCacheMu.RUnlock()
		e, ok := cfg.ProjectCache[name]
		return e, ok
	}
}

// githubMint is the App and installation a project's tokens are minted from.
// installationID 0 means "look up per repo", which only the fleet App does.
type githubMint struct {
	app            githubapp.AppName
	installationID int64
}

// resolveGithubApp decides which App mints project's tokens, from its cache
// entry. It never falls back from one App to the other: a tenant land must
// name the Lands App and pin its installation, an unknown github_app is
// refused, and a project the cache does not know is refused rather than
// guessed to be the operator's.
func (g *githubAppIssuer) resolveGithubApp(project string) (githubMint, error) {
	var entry config.ProjectCacheEntry
	ok := false
	if g.project != nil {
		entry, ok = g.project(project)
	}
	if !ok {
		return githubMint{}, fmt.Errorf("project %q is not in the project cache, so the GitHub App that mints its tokens is unknown", project)
	}
	app := githubapp.AppName(strings.ToLower(strings.TrimSpace(entry.GithubApp)))
	if app == "" {
		app = githubapp.Fleet
	}
	if app != githubapp.Fleet && app != githubapp.Lands {
		return githubMint{}, fmt.Errorf("project %q names unknown github_app %q (want %s or %s)", project, entry.GithubApp, githubapp.Fleet, githubapp.Lands)
	}
	if !entry.IsOperatorLand() && app != githubapp.Lands {
		return githubMint{}, fmt.Errorf("tenant land %q must mint with the %s GitHub App, not %s", project, githubapp.Lands, app)
	}
	if app == githubapp.Fleet {
		return githubMint{app: githubapp.Fleet, installationID: g.installationID}, nil
	}
	if entry.GithubInstallationID <= 0 {
		return githubMint{}, fmt.Errorf("project %q uses the %s GitHub App but has no pinned github_installation_id", project, githubapp.Lands)
	}
	return githubMint{app: githubapp.Lands, installationID: entry.GithubInstallationID}, nil
}

// verifyLandsInstallation refuses a Lands mint unless the pinned installation
// is one of the App's, is not suspended, is on the account that owns the repo
// the token is for, and is the installation GitHub itself places that repo in
// (uncached: a repo dropped from a selected-repositories installation is
// refused at once). The pin and the repo (git_url, or repos' primary entry)
// are all project-bead fields, so without this a pin naming another account's
// installation, or a repo that installation was never granted, would reach
// the mint. Neither the repo's owner nor the installation's account may be
// one of operatorOrgs: a Lands App installed on an operator org must never
// mint for a land, even when an admin pinned it by mistake. The repo must
// also still be within maxRepoSizeKB (0 is no limit): the setup check read
// its size once, and a tenant can push gigabytes after approval
// (gd-i74esUHCTg).
func verifyLandsInstallation(ctx context.Context, app githubAppAPI, installID int64, repo githubapp.RepoRef, operatorOrgs map[string]bool, maxRepoSizeKB int64) error {
	owner := repo.Owner
	if operatorOrgs[strings.ToLower(owner)] {
		return fmt.Errorf("repo %s is on operator GitHub account %q; the Lands App never mints for an operator org", repo, owner)
	}
	inst, err := app.Installation(ctx, installID)
	if err != nil {
		return fmt.Errorf("verify pinned installation %d: %w", installID, err)
	}
	if inst.ID != installID {
		return fmt.Errorf("verify pinned installation %d: GitHub returned installation %d", installID, inst.ID)
	}
	if operatorOrgs[strings.ToLower(inst.Account.Login)] {
		return fmt.Errorf("pinned installation %d is on operator GitHub account %q; the Lands App never mints for an operator org", installID, inst.Account.Login)
	}
	if !strings.EqualFold(inst.Account.Login, owner) {
		return fmt.Errorf("pinned installation %d is on GitHub account %q, not %q (the owner of the project's git_url)", installID, inst.Account.Login, owner)
	}
	if inst.SuspendedAt != nil {
		return fmt.Errorf("pinned installation %d is suspended", installID)
	}
	got, err := app.RepoInstallation(ctx, repo)
	if err != nil {
		return fmt.Errorf("pinned installation %d: check that it includes %s: %w", installID, repo, err)
	}
	if got != installID {
		return fmt.Errorf("repo %s is in installation %d, not the pinned installation %d", repo, got, installID)
	}
	if maxRepoSizeKB > 0 {
		size, err := app.RepoSizeKB(ctx, installID, repo)
		if err != nil {
			return fmt.Errorf("pinned installation %d: read the size of %s: %w", installID, repo, err)
		}
		if size > maxRepoSizeKB {
			return fmt.Errorf("repo %s is %d KB, over the %d KB a land takes (%s)", repo, size, maxRepoSizeKB, githubapp.MaxRepoSizeEnv)
		}
	}
	return nil
}

// client returns the App client for name, or the reason it cannot mint.
func (g *githubAppIssuer) client(name githubapp.AppName) (githubAppAPI, error) {
	if name == githubapp.Lands {
		if g.lands == nil {
			return nil, g.landsErr
		}
		return g.lands, nil
	}
	if g.initErr != nil {
		return nil, g.initErr
	}
	if g.app == nil {
		return nil, fmt.Errorf("%w: %s", githubapp.ErrNotConfigured, githubapp.Fleet)
	}
	return g.app, nil
}

// secretApp is the App that minted a per-agent Secret's token.
func secretApp(sec *corev1.Secret) githubapp.AppName {
	if a := githubapp.AppName(sec.Annotations[annotationGithubApp]); a != "" {
		return a
	}
	return githubapp.Fleet
}

// urlUserinfoRE matches the userinfo of any scheme://user[:pass]@ authority.
// The class stops at '/' so a path '@' is untouched; greedy matching reaches
// the LAST '@' of the authority, so a password containing '@' is fully covered.
var urlUserinfoRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s]*@`)

// redactURLUserinfo strips credentials ("user:pass@") from every URL in s. A
// project git_url may carry an embedded token; it must never reach a log line,
// error string or bead field (refusal log, escalation spec/context).
func redactURLUserinfo(s string) string {
	return urlUserinfoRE.ReplaceAllString(s, "${1}")
}

// githubTokenSecretName is lowercased like agentTokenSecretName: bead ids are
// mixed case (gd-OUtD8KYmIk) and a Secret name must be an RFC 1123 subdomain.
// Without it every mint failed and, fail-closed, every GitHub spawn was
// refused (rev 269, 2026-11-02, gd-gCzUTAg2a1).
func githubTokenSecretName(beadID string) string {
	return "agent-github-token-" + strings.ToLower(beadID)
}

// githubScope derives the repositories a token must cover from the pod spec:
// the primary repo plus reference repos on github.com under the same owner (an
// installation token cannot span owners). applicable is false when the primary
// repo is not on github.com. Order is stable; duplicates removed.
func githubScope(spec podmanager.AgentPodSpec) (owner string, names []string, applicable bool) {
	return githubScopeFor(spec, githubapp.Fleet)
}

// githubScopeFor is githubScope for the App that will mint the token. A Lands
// App token covers the land's one repo and never its reference repos.
func githubScopeFor(spec podmanager.AgentPodSpec, app githubapp.AppName) (owner string, names []string, applicable bool) {
	primary, ok := githubapp.ParseRepoURL(spec.GitURL, githubHost)
	if !ok {
		return "", nil, false
	}
	seen := map[string]bool{strings.ToLower(primary.Name): true}
	names = []string{primary.Name}
	if app == githubapp.Lands {
		return primary.Owner, names, true
	}
	for _, r := range spec.ReferenceRepos {
		ref, ok := githubapp.ParseRepoURL(r.URL, githubHost)
		if !ok || !strings.EqualFold(ref.Owner, primary.Owner) || seen[strings.ToLower(ref.Name)] {
			continue
		}
		seen[strings.ToLower(ref.Name)] = true
		names = append(names, ref.Name)
	}
	return primary.Owner, names, true
}

// provision mints a token for the spec's repos and writes the per-agent
// Secret. applicable=false means the project is not GitHub-hosted (nothing
// done). A non-nil err with applicable=true means the spawn must be refused.
func (g *githubAppIssuer) provision(ctx context.Context, spec podmanager.AgentPodSpec, beadID string) (secretName string, applicable bool, err error) {
	if _, _, applicable := githubScope(spec); !applicable {
		return "", false, nil
	}
	fail := func(e error) (string, bool, error) {
		g.tm.recordProvision(tokenProviderGithub, tokenProvisionError)
		return "", true, e
	}
	mint, rerr := g.resolveGithubApp(spec.Project)
	if rerr != nil {
		return fail(rerr)
	}
	if mint.app == githubapp.Fleet && g.fleetOff {
		return "", false, nil // the fleet App is off: not this provider's concern
	}
	if r := g.halted(ctx, spec.Project); r.Refused() {
		return fail(fmt.Errorf("%s: %s", r.Code, r.Reason))
	}
	app, cerr := g.client(mint.app)
	if cerr != nil {
		return fail(cerr)
	}
	owner, repos, _ := githubScopeFor(spec, mint.app)
	ctx, cancel := context.WithTimeout(ctx, githubOpTimeout)
	defer cancel()

	// A Lands token, reused or new, is only handed out after the pinned
	// installation is read back for this repo.
	if mint.app == githubapp.Lands {
		if verr := verifyLandsInstallation(ctx, app, mint.installationID, githubapp.RepoRef{Owner: owner, Name: repos[0]}, g.operatorOrgs, g.maxRepoSizeKB); verr != nil {
			return fail(verr)
		}
	}

	name := githubTokenSecretName(beadID)
	reposAnno := owner + "/" + strings.Join(repos, ",")
	secrets := g.k8s.CoreV1().Secrets(spec.Namespace)

	// Idempotent per live pod: a duplicate/redelivered spawn event (decision
	// resolve respawns the requesting actor; CreateAgentPod then adopts the
	// existing pod) must NOT retire the token the running agent is holding.
	// A Secret whose token still has more than the refresh window left and
	// covers the same repos (and, for the Lands App, comes from the pinned
	// installation) is reused as-is.
	existing, gerr := secrets.Get(ctx, name, metav1.GetOptions{})
	if gerr != nil && !apierrors.IsNotFound(gerr) {
		return fail(fmt.Errorf("read github token secret %s: %w", name, gerr))
	}
	if gerr == nil && g.reusable(existing, reposAnno, mint) {
		g.created.Delete(beadID)
		g.tm.recordProvision(tokenProviderGithub, tokenProvisionSuccess)
		g.logger.Info("reusing live per-agent github installation token",
			"bead", beadID, "secret", name, "repos", reposAnno,
			"expires_at", existing.Annotations[annotationGithubExpiresAt])
		return name, true, nil
	}

	installID := mint.installationID
	if installID == 0 { // fleet only: resolveGithubApp pins every Lands mint
		var lerr error
		installID, lerr = app.InstallationFor(ctx, githubapp.RepoRef{Owner: owner, Name: repos[0]})
		if lerr != nil {
			return fail(lerr)
		}
	}
	tok, merr := app.MintInstallationToken(ctx, installID, repos)
	if merr != nil {
		return fail(merr)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: spec.Namespace,
			Labels: map[string]string{
				podmanager.LabelApp:     podmanager.LabelAppValue,
				"gardenio.io/type":      "agent-token",
				"gardenio.io/provider":  tokenProviderGithub,
				"gardenio.io/bead":      beadID,
				podmanager.LabelProject: spec.Project,
			},
			Annotations: map[string]string{
				annotationGithubExpiresAt:    tok.ExpiresAt.UTC().Format(time.RFC3339),
				annotationGithubRepos:        reposAnno,
				annotationGithubApp:          string(mint.app),
				annotationGithubInstallation: fmt.Sprint(installID),
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": tok.Reveal(), "username": githubTokenUsername},
	}
	if gerr != nil { // NotFound: first provision for this bead
		if _, cerr := secrets.Create(ctx, secret, metav1.CreateOptions{}); cerr != nil {
			g.revokeQuietly(ctx, app, tok)
			// A concurrent provision for the same bead (two sync paths handling
			// one spawn) wrote the Secret between the Get above and this Create.
			// Its token is as fresh as ours, so the agent uses it.
			if apierrors.IsAlreadyExists(cerr) {
				if won, werr := secrets.Get(ctx, name, metav1.GetOptions{}); werr == nil && g.reusable(won, reposAnno, mint) {
					g.tm.recordProvision(tokenProviderGithub, tokenProvisionSuccess)
					g.logger.Info("reusing per-agent github installation token written by a concurrent provision",
						"bead", beadID, "secret", name, "repos", reposAnno,
						"expires_at", won.Annotations[annotationGithubExpiresAt])
					return name, true, nil
				}
			}
			return fail(fmt.Errorf("create github token secret %s: %w", name, cerr))
		}
		g.created.Store(beadID, struct{}{})
	} else {
		g.created.Delete(beadID)
		// Stale Secret from an earlier incarnation of this bead (stuck restart,
		// dropped despawn, token inside the refresh window, or a different repo
		// scope). Retire the old token only when it cannot be the working
		// token of the same scope; one merely nearing expiry is left to lapse
		// (the live pod may still hold it until the kubelet re-projects).
		if existing.Annotations[annotationGithubRepos] != reposAnno || secretApp(existing) != mint.app || !sameInstallation(existing, mint) {
			old, _ := g.client(secretApp(existing))
			g.revokeQuietly(ctx, old, githubapp.NewToken(string(existing.Data["token"]), time.Time{}))
		}
		secret.ResourceVersion = existing.ResourceVersion
		if _, uerr := secrets.Update(ctx, secret, metav1.UpdateOptions{}); uerr != nil {
			g.revokeQuietly(ctx, app, tok)
			return fail(fmt.Errorf("update github token secret %s: %w", name, uerr))
		}
	}
	g.tm.recordProvision(tokenProviderGithub, tokenProvisionSuccess)
	g.logger.Info("provisioned per-agent github installation token",
		"bead", beadID, "secret", name, "repos", reposAnno, "github_app", mint.app,
		"expires_at", tok.ExpiresAt.UTC().Format(time.RFC3339))
	return name, true, nil
}

// reusable reports whether an existing per-agent Secret still holds a usable
// token for the wanted repo scope: it has a token, was minted by mint's App
// (and installation, for the Lands App), covers exactly reposAnno, and more
// than the refresh window remains.
func (g *githubAppIssuer) reusable(sec *corev1.Secret, reposAnno string, mint githubMint) bool {
	if len(sec.Data["token"]) == 0 && sec.StringData["token"] == "" {
		return false
	}
	if sec.Annotations[annotationGithubRepos] != reposAnno || secretApp(sec) != mint.app || !sameInstallation(sec, mint) {
		return false
	}
	exp, err := time.Parse(time.RFC3339, sec.Annotations[annotationGithubExpiresAt])
	return err == nil && exp.Sub(g.now()) > githubTokenRefreshWindow
}

// sameInstallation reports whether sec's token came from mint's installation.
// Only a Lands mint is compared: its installation is pinned on the project
// bead, so a repin must not keep handing out the old installation's token.
// Fleet Secrets minted before step 8.7 carry no installation annotation, and
// the fleet installation is the operator's own, so they are not compared.
func sameInstallation(sec *corev1.Secret, mint githubMint) bool {
	return mint.app != githubapp.Lands || sec.Annotations[annotationGithubInstallation] == fmt.Sprint(mint.installationID)
}

// halted is the land's kill-switch refusal (landHalt), the zero Refusal when
// the land may hold a token or nothing is wired to check.
func (g *githubAppIssuer) halted(ctx context.Context, project string) wall.Refusal {
	if g.landHalt == nil {
		return wall.Refusal{}
	}
	return g.landHalt(ctx, project)
}

// revokeQuietly revokes tok with app, the App that minted it. A nil app (that
// App is unusable here) leaves the token to its one-hour expiry.
func (g *githubAppIssuer) revokeQuietly(ctx context.Context, app githubAppAPI, tok githubapp.Token) {
	if app == nil || tok.Reveal() == "" {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubOpTimeout)
	defer cancel()
	if err := app.Revoke(rctx, tok); err != nil {
		g.logger.Warn("github installation token revoke failed (1h expiry backstops)", "error", err)
	}
}

// applyToSpec is the spawn-door hook. It returns refuse=true (with a
// non-secret reason) when a GitHub-hosted spawn must not proceed.
func (g *githubAppIssuer) applyToSpec(ctx context.Context, spec *podmanager.AgentPodSpec, beadID string) (refuse bool, reason string) {
	if g == nil {
		return false, ""
	}
	if beadID == "" {
		if _, _, app := githubScope(*spec); app {
			return true, "github app tokens require an agent bead id; refusing to fall back to a shared git credential"
		}
		return false, ""
	}
	secretName, applicable, err := g.provision(ctx, *spec, beadID)
	if !applicable {
		return false, ""
	}
	if err != nil {
		return true, redactURLUserinfo(fmt.Sprintf("GitHub App installation token could not be minted for %s (%v); refusing to fall back to a shared git credential", spec.GitURL, err))
	}
	dropSharedGitEnv(spec, secretName)
	spec.GitCredentialsSecret = secretName
	return false, ""
}

// dropSharedGitEnv removes every secret-sourced env var that would hand the
// agent pod a git credential other than the per-agent Secret. A project bead's
// secrets[] mapping of GIT_TOKEN/GIT_USERNAME (from the shared PAT Secret) is
// turned into spec.SecretEnv by the spec builder; left in place, `gb agent
// start` and the env credential helper would write and serve that PAT for
// in-session git and the App token would only be used by init-clone and `gh`.
// The vars are dropped, not repointed: an env var sourced from a Secret is
// resolved once at container start and never refreshes, so it would go stale
// after an hour. The pod's git credential is the projected per-agent file.
func dropSharedGitEnv(spec *podmanager.AgentPodSpec, perAgentSecret string) {
	if len(spec.SecretEnv) == 0 {
		return
	}
	shared := spec.GitCredentialsSecret
	kept := spec.SecretEnv[:0:0]
	for _, se := range spec.SecretEnv {
		gitEnv := se.EnvName == "GIT_TOKEN" || se.EnvName == "GIT_USERNAME"
		fromShared := shared != "" && shared != perAgentSecret && se.SecretName == shared
		if (gitEnv || fromShared) && se.SecretName != perAgentSecret {
			continue
		}
		kept = append(kept, se)
	}
	spec.SecretEnv = kept
}

// reconcilerApplier adapts the issuer to the reconciler's pre-create seam. A
// refusal is recorded exactly as at the event door (error log + deduplicated
// escalation bead) so a bead the reconciler cannot spawn is visible.
func (g *githubAppIssuer) reconcilerApplier(logger *slog.Logger, daemon *beadsapi.Client) reconciler.GitCredentialApplier {
	return reconciler.GitCredentialApplier{
		Apply: func(ctx context.Context, spec *podmanager.AgentPodSpec, beadID string) (bool, string) {
			refuse, reason := g.applyToSpec(ctx, spec, beadID)
			if refuse {
				recordGithubAppRefusal(ctx, logger, daemon, *spec, beadID, reason)
			}
			return refuse, reason
		},
		Release: func(ctx context.Context, spec podmanager.AgentPodSpec, beadID string) {
			g.revokeIfCreated(ctx, beadID, spec.Namespace, revocationTypeDespawn)
		},
	}
}

// revokeIfCreated is the cleanup for a pod create that failed after
// applyToSpec: it revokes the token only if this spawn created the Secret.
// A reused or refreshed Secret may belong to a live agent (duplicate or
// redelivered spawn whose CreateAgentPod hit a transient error such as a
// missing Secret), and revoking it would cut that agent's git access. No-op on
// a nil issuer.
func (g *githubAppIssuer) revokeIfCreated(ctx context.Context, beadID, namespace, revType string) {
	if g == nil || beadID == "" {
		return
	}
	if _, ok := g.created.LoadAndDelete(beadID); !ok {
		g.logger.Info("pod create failed; leaving reused github installation token for the live agent",
			"bead", beadID, "secret", githubTokenSecretName(beadID))
		return
	}
	g.revoke(ctx, beadID, namespace, revType, time.Time{})
}

// revoke revokes the bead's token at GitHub and deletes its Secret. No-op on a
// nil issuer. Best-effort: a failure leaves the Secret for the next sweep.
func (g *githubAppIssuer) revoke(ctx context.Context, beadID, namespace, revType string, doneAt time.Time) {
	if g == nil || beadID == "" {
		return
	}
	g.created.Delete(beadID)
	name := githubTokenSecretName(beadID)
	secrets := g.k8s.CoreV1().Secrets(namespace)
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return // nothing minted for this bead
	}
	if err != nil {
		g.logger.Warn("github token revoke: cannot read secret (sweep retries)", "secret", name, "error", err)
		return
	}
	if app, _ := g.client(secretApp(sec)); app != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubOpTimeout)
		rerr := app.Revoke(rctx, githubapp.NewToken(string(sec.Data["token"]), time.Time{}))
		cancel()
		if rerr != nil {
			// Keep the Secret so the sweep can retry the revoke; the token
			// still expires within the hour.
			g.logger.Warn("github token revoke failed (sweep retries, 1h expiry backstops)", "bead", beadID, "secret", name, "error", rerr)
			return
		}
	}
	if derr := secrets.Delete(ctx, name, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
		g.logger.Warn("failed to delete github token secret", "secret", name, "error", derr)
	}
	g.tm.recordRevocation(tokenProviderGithub, revType, doneAt)
	g.logger.Info("revoked per-agent github installation token", "bead", beadID, "secret", name, "revocation_type", revType)
}

// githubSweepDaemon is the slice of the beads client the sweep needs.
type githubSweepDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

type githubSweepStats struct{ Scanned, Refreshed, Revoked int }

// sweep walks github-provider token Secrets in namespaces: closed beads are
// revoked (async_janitor), open beads that still have an agent pod are
// re-minted when within the refresh window; open beads with no pod are left to
// lapse. Best-effort per Secret.
func (g *githubAppIssuer) sweep(ctx context.Context, daemon githubSweepDaemon, namespaces []string) githubSweepStats {
	var st githubSweepStats
	if g == nil || (g.app == nil && g.lands == nil) || daemon == nil {
		return st
	}
	for _, ns := range namespaces {
		if ns == "" {
			continue
		}
		list, err := g.k8s.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "gardenio.io/type=agent-token,gardenio.io/provider=" + tokenProviderGithub,
		})
		if err != nil {
			g.logger.Warn("github token sweep: list failed", "namespace", ns, "error", err)
			continue
		}
		// Beads that currently have an agent pod in this namespace. nil means
		// the pod list failed: refreshes are skipped this sweep (closed-bead
		// revocation, which needs no pod, still runs).
		livePods := g.liveAgentBeads(ctx, ns)
		for i := range list.Items {
			sec := &list.Items[i]
			beadID := sec.Labels["gardenio.io/bead"]
			if beadID == "" {
				continue
			}
			st.Scanned++
			bead, err := daemon.GetBead(ctx, beadID)
			if err != nil || bead == nil {
				continue
			}
			if bead.Status == "closed" {
				g.revoke(ctx, beadID, ns, revocationTypeAsyncJanitor, parseBeadCloseTime(bead.ClosedAt))
				st.Revoked++
				continue
			}
			// An open bead with no pod (deferred/refused spawn, pod gone before
			// the bead closed) has nobody to use a fresh token: re-minting it
			// burns the installation's GitHub rate limit and leaves a live
			// token nobody holds. Let it lapse; the next spawn mints afresh.
			if livePods == nil || !livePods[beadID] {
				continue
			}
			if g.refreshIfDue(ctx, sec) {
				st.Refreshed++
			}
		}
	}
	return st
}

// liveAgentBeads returns the set of bead ids that have an agent pod in ns
// (pod annotation gardenio.io/bead-id), or nil when the pods cannot be listed.
func (g *githubAppIssuer) liveAgentBeads(ctx context.Context, ns string) map[string]bool {
	pods, err := g.k8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: podmanager.LabelAgent})
	if err != nil {
		g.logger.Warn("github token sweep: pod list failed; skipping refreshes this sweep", "namespace", ns, "error", err)
		return nil
	}
	live := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		if id := pods.Items[i].Annotations[podmanager.AnnotationBeadID]; id != "" {
			live[id] = true
		}
	}
	return live
}

// refreshIfDue re-mints the token for a live agent when it is inside the
// refresh window, rewriting the Secret in place (RBAC: secrets update). The
// previous token is left to expire rather than revoked: the running agent may
// still be holding it until the kubelet re-projects the file.
//
// The App is resolved afresh from the project (gardenio.io/project label), as
// at spawn: a Secret minted by a different App or installation than the
// project now names is not refreshed but left to lapse, so a land that moved
// to the Lands App never gets another fleet token. So is the token of a land
// that is frozen, paused, gone or cannot be read (landHalt): under a kill
// switch the land's last token lapses at its expiry, within the hour.
func (g *githubAppIssuer) refreshIfDue(ctx context.Context, sec *corev1.Secret) bool {
	exp, err := time.Parse(time.RFC3339, sec.Annotations[annotationGithubExpiresAt])
	if err != nil || exp.Sub(g.now()) > githubTokenRefreshWindow {
		return false
	}
	owner, repos, ok := splitGithubRepos(sec.Annotations[annotationGithubRepos])
	if !ok {
		g.logger.Warn("github token refresh: malformed repos annotation", "secret", sec.Name)
		return false
	}
	mint, err := g.resolveGithubApp(sec.Labels[podmanager.LabelProject])
	if err != nil {
		g.logger.Warn("github token refresh: no GitHub App for the project; leaving the token to lapse", "secret", sec.Name, "error", err)
		return false
	}
	if secretApp(sec) != mint.app || !sameInstallation(sec, mint) {
		g.logger.Warn("github token refresh: project now mints with another App or installation; leaving the token to lapse",
			"secret", sec.Name, "minted_by", secretApp(sec), "project_app", mint.app)
		return false
	}
	if mint.app == githubapp.Lands && len(repos) != 1 {
		g.logger.Warn("github token refresh: a Lands App token covers one repo; leaving the token to lapse", "secret", sec.Name)
		return false
	}
	if r := g.halted(ctx, sec.Labels[podmanager.LabelProject]); r.Refused() {
		g.logger.Warn("github token refresh: the land is halted; leaving the token to lapse",
			"secret", sec.Name, "code", r.Code, "reason", r.Reason)
		return false
	}
	app, err := g.client(mint.app)
	if err != nil {
		g.logger.Warn("github token refresh: App unavailable (current token still valid until expiry)", "secret", sec.Name, "error", err)
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, githubOpTimeout)
	defer cancel()
	installID := mint.installationID
	if installID == 0 {
		if installID, err = app.InstallationFor(ctx, githubapp.RepoRef{Owner: owner, Name: repos[0]}); err != nil {
			g.logger.Warn("github token refresh: installation lookup failed", "secret", sec.Name, "error", err)
			return false
		}
	}
	if mint.app == githubapp.Lands {
		if err := verifyLandsInstallation(ctx, app, installID, githubapp.RepoRef{Owner: owner, Name: repos[0]}, g.operatorOrgs, g.maxRepoSizeKB); err != nil {
			g.tm.recordProvision(tokenProviderGithub, tokenProvisionError)
			g.logger.Warn("github token refresh: pinned installation does not cover the repo; leaving the token to lapse", "secret", sec.Name, "error", err)
			return false
		}
	}
	tok, err := app.MintInstallationToken(ctx, installID, repos)
	if err != nil {
		g.tm.recordProvision(tokenProviderGithub, tokenProvisionError)
		g.logger.Warn("github token refresh: mint failed (current token still valid until expiry)", "secret", sec.Name, "error", err)
		return false
	}
	updated := sec.DeepCopy()
	updated.Data = nil
	updated.StringData = map[string]string{"token": tok.Reveal(), "username": githubTokenUsername}
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[annotationGithubExpiresAt] = tok.ExpiresAt.UTC().Format(time.RFC3339)
	updated.Annotations[annotationGithubApp] = string(mint.app)
	updated.Annotations[annotationGithubInstallation] = fmt.Sprint(installID)
	if _, err := g.k8s.CoreV1().Secrets(sec.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		g.revokeQuietly(ctx, app, tok)
		g.tm.recordProvision(tokenProviderGithub, tokenProvisionError)
		g.logger.Warn("github token refresh: secret update failed", "secret", sec.Name, "error", err)
		return false
	}
	g.tm.recordProvision(tokenProviderGithub, tokenProvisionSuccess)
	g.logger.Info("refreshed per-agent github installation token", "secret", sec.Name,
		"expires_at", tok.ExpiresAt.UTC().Format(time.RFC3339))
	return true
}

func splitGithubRepos(v string) (owner string, repos []string, ok bool) {
	o, rest, found := strings.Cut(v, "/")
	if !found || o == "" || rest == "" {
		return "", nil, false
	}
	return o, strings.Split(rest, ","), true
}

// refusalBeadIDPattern matches bead ids (gd-OUtD8KYmIk, or lowercased inside a
// Secret name) so refusals that differ only by agent share one cause.
var refusalBeadIDPattern = regexp.MustCompile(`(?i)\b[a-z]{2}-[a-z0-9]{10}\b`)

// githubAppRefusalKey is the escalation idempotency key for a refused spawn:
// one per project, per cause, per UTC hour. A cluster-wide failure (bad key,
// bad App id, the rev 269 Secret-name bug) refuses every agent, and every
// retry is a new agent bead, so a per-agent key filed one escalation per
// refused agent (14 for one 20-minute outage, 2026-11-02). The hour bucket
// lets a cause that persists or comes back after its escalation was closed
// file again, since the idempotency lookup also matches closed beads.
func githubAppRefusalKey(spec podmanager.AgentPodSpec, reason string, now time.Time) string {
	cause := strings.ReplaceAll(reason, spec.AgentName, "<agent>")
	cause = refusalBeadIDPattern.ReplaceAllString(cause, "<bead>")
	sum := sha256.Sum256([]byte(cause))
	return fmt.Sprintf("github-app-mint:%s:%s:%s", spec.Project, hex.EncodeToString(sum[:6]),
		now.UTC().Format("2006010215"))
}

// buildGithubAppRefusalEscalation records a refused GitHub-hosted spawn for the
// operator queue. Reason text is non-secret by construction (see applyToSpec).
// Refusals with the same cause in the same project and hour share one bead
// (githubAppRefusalKey); the controller log names every refused agent.
func buildGithubAppRefusalEscalation(spec podmanager.AgentPodSpec, agentBeadID, reason string, now time.Time) beadsapi.CreateBeadRequest {
	reason = redactURLUserinfo(reason)
	fields := map[string]any{
		"operation_type": "credential_provisioning",
		"operation_spec": map[string]any{
			"agent": spec.AgentName, "project": spec.Project, "role": spec.Role,
			"git_url": redactURLUserinfo(spec.GitURL), "reason": reason,
		},
		"context": fmt.Sprintf("Controller refused to spawn agent %q: %s. Fix the GitHub App "+
			"(installed on the repo? private key mounted? App id right?) and the reconciler will retry. "+
			"The shared git credential is deliberately NOT used as a fallback. Other agents refused for "+
			"the same cause this hour are folded into this escalation; the controller log "+
			"(\"refusing agent spawn\") names each one.", spec.AgentName, reason),
	}
	if agentBeadID != "" {
		fields["requesting_agent"] = agentBeadID
	}
	fieldsJSON, _ := json.Marshal(fields)
	labels := []string{"guard:github-app-mint"}
	if spec.Project != "" {
		labels = append(labels, "project:"+spec.Project)
	}
	req := beadsapi.CreateBeadRequest{
		Title:     fmt.Sprintf("Refused spawns, GitHub App token mint failed (project %s)", spec.Project),
		Type:      "escalation",
		Kind:      "data",
		Fields:    json.RawMessage(fieldsJSON),
		Labels:    labels,
		Priority:  1,
		CreatedBy: "gardenio-controller",
	}
	req.IdempotencyKey = githubAppRefusalKey(spec, reason, now)
	return req
}

// recordGithubAppRefusal surfaces a refused spawn: structured error log plus a
// (deduplicated) escalation bead. The refusal itself holds regardless of the
// daemon writes.
func recordGithubAppRefusal(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, spec podmanager.AgentPodSpec, agentBeadID, reason string) {
	logger.Error("refusing agent spawn — GitHub App token unavailable (fail closed, no shared-token fallback)",
		"agent", spec.AgentName, "project", spec.Project, "role", spec.Role, "bead", agentBeadID, "reason", redactURLUserinfo(reason))
	if daemon == nil {
		return
	}
	if _, err := daemon.CreateBead(ctx, buildGithubAppRefusalEscalation(spec, agentBeadID, reason, time.Now())); err != nil {
		logger.Warn("github app refusal: failed to file escalation bead", "bead", agentBeadID, "error", err)
	}
}
