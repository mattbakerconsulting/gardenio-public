// Release-deployer GitHub token (gd-q48ldwLspl).
//
// The release-deployer CronJob lists this repo's tags, clones a tag and reads
// its commit and workflow runs. With agents.githubApp.enabled the chart points
// releaseDeployer.gitSecret at RELEASE_DEPLOYER_GITHUB_TOKEN_SECRET, and this
// loop keeps that Secret (keys username=x-access-token, token) holding a
// read-only installation token of the fleet App for the one repo
// RELEASE_DEPLOYER_GITHUB_REPO, so the deployer needs no PAT and the App key
// stays in the controller.
//
// The token is re-minted at startup and whenever less than
// releaseDeployerTokenRefreshWindow of its hour remains, so a Job that reads
// the Secret at start holds a token with at least ~30 minutes left. The token
// it replaces is left to lapse, not revoked: a running Job may still hold it.
//
// Credential hygiene as in github_app_credentials.go: the token value goes to
// the Secret's data and nowhere else.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/podmanager"
)

const (
	releaseDeployerTokenSecretEnv = "RELEASE_DEPLOYER_GITHUB_TOKEN_SECRET"
	releaseDeployerTokenRepoEnv   = "RELEASE_DEPLOYER_GITHUB_REPO"

	// releaseDeployerTokenRefreshWindow: re-mint when less than this remains.
	// With a check every releaseDeployerTokenInterval the token is replaced
	// 20-25 min into its hour.
	releaseDeployerTokenRefreshWindow = 40 * time.Minute
	releaseDeployerTokenInterval      = 5 * time.Minute

	// releaseDeployerTokenType labels the Secret. It is not "agent-token", so
	// neither the per-agent GitHub sweep nor the beads token janitor touches it.
	releaseDeployerTokenType = "release-deployer-token"
)

// releaseDeployerTokenAPI is the slice of *githubapp.Client the loop uses.
type releaseDeployerTokenAPI interface {
	InstallationFor(ctx context.Context, repo githubapp.RepoRef) (int64, error)
	MintInstallationTokenWithPermissions(ctx context.Context, installationID int64, repoNames []string, permissions map[string]string) (githubapp.Token, error)
	Revoke(ctx context.Context, tok githubapp.Token) error
}

// releaseDeployerToken keeps the release deployer's token Secret fresh.
type releaseDeployerToken struct {
	app            releaseDeployerTokenAPI
	installationID int64 // 0 = look up for repo
	repo           githubapp.RepoRef
	k8s            kubernetes.Interface
	namespace      string
	secretName     string
	logger         *slog.Logger
	now            func() time.Time
}

// releaseDeployerTokenFor returns the loop for the fleet App of g, or nil when
// RELEASE_DEPLOYER_GITHUB_TOKEN_SECRET is unset or the fleet App cannot mint.
// A set Secret with an unusable repo or App is logged as an error, since the
// deployer then has no credential.
func releaseDeployerTokenFor(g *githubAppIssuer, k8s kubernetes.Interface, namespace string, getenv func(string) string, logger *slog.Logger) *releaseDeployerToken {
	secretName := getenv(releaseDeployerTokenSecretEnv)
	if secretName == "" {
		return nil
	}
	repoURL := getenv(releaseDeployerTokenRepoEnv)
	repo, ok := githubapp.ParseRepoURL(repoURL, githubHost)
	if !ok {
		logger.Error("release-deployer github token: repo is not an https github.com URL; no token will be minted",
			"env", releaseDeployerTokenRepoEnv, "repo", redactURLUserinfo(repoURL))
		return nil
	}
	if g == nil || g.fleetOff || g.initErr != nil || g.app == nil {
		logger.Error("release-deployer github token: the fleet GitHub App is not usable; no token will be minted", "secret", secretName)
		return nil
	}
	app, ok := g.app.(releaseDeployerTokenAPI)
	if !ok {
		return nil
	}
	return &releaseDeployerToken{app: app, installationID: g.installationID, repo: repo, k8s: k8s,
		namespace: namespace, secretName: secretName, logger: logger, now: time.Now}
}

// RunLoop refreshes the Secret now and every interval until ctx ends.
func (r *releaseDeployerToken) RunLoop(ctx context.Context, interval time.Duration) {
	if r == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := r.refresh(ctx); err != nil {
			r.logger.Warn("release-deployer github token refresh failed (current token, if any, valid until expiry)",
				"secret", r.secretName, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refresh mints a token and writes it to the Secret unless the Secret already
// holds one for the repo with more than the refresh window left. minted
// reports whether the Secret was written.
func (r *releaseDeployerToken) refresh(ctx context.Context) (minted bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, githubOpTimeout)
	defer cancel()
	secrets := r.k8s.CoreV1().Secrets(r.namespace)
	existing, gerr := secrets.Get(ctx, r.secretName, metav1.GetOptions{})
	if gerr != nil && !apierrors.IsNotFound(gerr) {
		return false, fmt.Errorf("read secret %s: %w", r.secretName, gerr)
	}
	if gerr == nil && r.fresh(existing) {
		return false, nil
	}

	installID := r.installationID
	if installID == 0 {
		if installID, err = r.app.InstallationFor(ctx, r.repo); err != nil {
			return false, err
		}
	}
	tok, err := r.app.MintInstallationTokenWithPermissions(ctx, installID, []string{r.repo.Name}, githubapp.ReleaseDeployerTokenPermissions())
	if err != nil {
		return false, err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.secretName,
			Namespace: r.namespace,
			Labels: map[string]string{
				podmanager.LabelApp:    podmanager.LabelAppValue,
				"gardenio.io/type":     releaseDeployerTokenType,
				"gardenio.io/provider": tokenProviderGithub,
			},
			Annotations: map[string]string{
				annotationGithubExpiresAt:    tok.ExpiresAt.UTC().Format(time.RFC3339),
				annotationGithubRepos:        r.repo.String(),
				annotationGithubApp:          string(githubapp.Fleet),
				annotationGithubInstallation: fmt.Sprint(installID),
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": tok.Reveal(), "username": githubTokenUsername},
	}
	if gerr != nil { // NotFound
		_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
	} else {
		secret.ResourceVersion = existing.ResourceVersion
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
	}
	if err != nil {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), githubOpTimeout)
		defer rcancel()
		_ = r.app.Revoke(rctx, tok)
		return false, fmt.Errorf("write secret %s: %w", r.secretName, err)
	}
	r.logger.Info("refreshed release-deployer github token", "secret", r.secretName, "repo", r.repo.String(),
		"expires_at", tok.ExpiresAt.UTC().Format(time.RFC3339))
	return true, nil
}

// fresh reports whether sec holds a token for r.repo with more than the
// refresh window left.
func (r *releaseDeployerToken) fresh(sec *corev1.Secret) bool {
	if len(sec.Data["token"]) == 0 || sec.Annotations[annotationGithubRepos] != r.repo.String() {
		return false
	}
	exp, err := time.Parse(time.RFC3339, sec.Annotations[annotationGithubExpiresAt])
	return err == nil && exp.Sub(r.now()) > releaseDeployerTokenRefreshWindow
}

var _ releaseDeployerTokenAPI = (*githubapp.Client)(nil)
