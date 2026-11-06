package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/githubapp"
)

// fakeDeployerApp is a releaseDeployerTokenAPI that mints "ghs_DEPLOY_n".
type fakeDeployerApp struct {
	perms    []map[string]string
	repos    [][]string
	installs []int64
	lookups  int
	revoked  []string
	n        int
	expiry   time.Time
	mintErr  error
}

func (f *fakeDeployerApp) InstallationFor(context.Context, githubapp.RepoRef) (int64, error) {
	f.lookups++
	return 170295668, nil
}

func (f *fakeDeployerApp) MintInstallationTokenWithPermissions(_ context.Context, id int64, repos []string, perms map[string]string) (githubapp.Token, error) {
	if f.mintErr != nil {
		return githubapp.Token{}, f.mintErr
	}
	f.n++
	f.perms = append(f.perms, perms)
	f.repos = append(f.repos, repos)
	f.installs = append(f.installs, id)
	return githubapp.NewToken(fmt.Sprintf("ghs_DEPLOY_%d", f.n), f.expiry), nil
}

func (f *fakeDeployerApp) Revoke(_ context.Context, tok githubapp.Token) error {
	f.revoked = append(f.revoked, tok.Reveal())
	return nil
}

const deployerSecret = "release-deployer-github-token"

func newDeployerTokenEnv(t *testing.T, objs ...runtime.Object) (*releaseDeployerToken, *fakeDeployerApp, *fake.Clientset, *bytes.Buffer) {
	t.Helper()
	now := time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)
	app := &fakeDeployerApp{expiry: now.Add(time.Hour)}
	k := fake.NewSimpleClientset(objs...)
	buf := &bytes.Buffer{}
	r := &releaseDeployerToken{app: app, repo: githubapp.RepoRef{Owner: "mattbakerconsulting", Name: "gardenio"},
		k8s: k, namespace: "gardenio", secretName: deployerSecret,
		logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now:    func() time.Time { return now }}
	return r, app, k, buf
}

func TestReleaseDeployerToken_CreatesReadOnlySecret(t *testing.T) {
	r, app, k, logs := newDeployerTokenEnv(t)
	minted, err := r.refresh(context.Background())
	if err != nil || !minted {
		t.Fatalf("refresh = %v, %v", minted, err)
	}
	want := map[string]string{"contents": "read", "actions": "read", "metadata": "read"}
	if got := app.perms[0]; len(got) != len(want) || got["contents"] != "read" || got["actions"] != "read" || got["metadata"] != "read" {
		t.Errorf("permissions = %v, want exactly %v", got, want)
	}
	if len(app.repos[0]) != 1 || app.repos[0][0] != "gardenio" {
		t.Errorf("repos = %v, want [gardenio]", app.repos[0])
	}
	if app.lookups != 1 || app.installs[0] != 170295668 {
		t.Errorf("installation lookups = %d, installs = %v", app.lookups, app.installs)
	}
	sec := getSecret(t, k, "gardenio", deployerSecret)
	if sec.StringData["username"] != "x-access-token" || sec.StringData["token"] != "ghs_DEPLOY_1" {
		t.Errorf("secret data = %v", sec.StringData)
	}
	if sec.Labels["gardenio.io/type"] == "agent-token" {
		t.Error("the deployer Secret must not carry type=agent-token, or the per-agent sweeps would revoke it")
	}
	if sec.Annotations[annotationGithubRepos] != "mattbakerconsulting/gardenio" {
		t.Errorf("repos annotation = %q", sec.Annotations[annotationGithubRepos])
	}
	if strings.Contains(logs.String(), "ghs_DEPLOY") {
		t.Errorf("token value reached the log: %s", logs.String())
	}
}

func TestReleaseDeployerToken_FreshSecretIsLeftAlone(t *testing.T) {
	r, app, _, _ := newDeployerTokenEnv(t, deployerSecretObj("ghs_OLD", "mattbakerconsulting/gardenio", time.Date(2026, 11, 3, 7, 50, 0, 0, time.UTC)))
	minted, err := r.refresh(context.Background())
	if err != nil || minted || app.n != 0 {
		t.Fatalf("a token with 50m left must not be re-minted: minted=%v err=%v mints=%d", minted, err, app.n)
	}
}

func TestReleaseDeployerToken_DueSecretIsRemintedInPlace(t *testing.T) {
	old := deployerSecretObj("ghs_OLD", "mattbakerconsulting/gardenio", time.Date(2026, 11, 3, 7, 30, 0, 0, time.UTC))
	r, app, k, _ := newDeployerTokenEnv(t, old)
	r.installationID = 42
	minted, err := r.refresh(context.Background())
	if err != nil || !minted {
		t.Fatalf("refresh = %v, %v", minted, err)
	}
	if app.lookups != 0 || app.installs[0] != 42 {
		t.Errorf("a pinned installation must skip the lookup: lookups=%d installs=%v", app.lookups, app.installs)
	}
	sec := getSecret(t, k, "gardenio", deployerSecret)
	if sec.StringData["token"] != "ghs_DEPLOY_1" {
		t.Errorf("token = %q, want the fresh one", sec.StringData["token"])
	}
	if exp := sec.Annotations[annotationGithubExpiresAt]; exp != "2026-11-03T08:00:00Z" {
		t.Errorf("expires-at = %q", exp)
	}
	if len(app.revoked) != 0 {
		t.Errorf("the replaced token must lapse, not be revoked (a running Job may hold it): %v", app.revoked)
	}
}

func TestReleaseDeployerToken_OtherRepoIsReminted(t *testing.T) {
	r, app, _, _ := newDeployerTokenEnv(t, deployerSecretObj("ghs_OLD", "joyousaversion/gardenio", time.Date(2026, 11, 3, 7, 59, 0, 0, time.UTC)))
	if minted, err := r.refresh(context.Background()); err != nil || !minted || app.n != 1 {
		t.Fatalf("a Secret for another repo must be re-minted: minted=%v err=%v mints=%d", minted, err, app.n)
	}
}

func TestReleaseDeployerToken_WriteFailureRevokesNewToken(t *testing.T) {
	r, app, k, _ := newDeployerTokenEnv(t)
	k.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := r.refresh(context.Background()); err == nil {
		t.Fatal("a failed Secret write must be an error")
	}
	if len(app.revoked) != 1 || app.revoked[0] != "ghs_DEPLOY_1" {
		t.Errorf("revoked = %v, want the unwritten token", app.revoked)
	}
}

func TestReleaseDeployerToken_MintFailureKeepsSecret(t *testing.T) {
	r, app, k, _ := newDeployerTokenEnv(t, deployerSecretObj("ghs_OLD", "mattbakerconsulting/gardenio", time.Date(2026, 11, 3, 7, 20, 0, 0, time.UTC)))
	app.mintErr = errors.New("HTTP 502")
	if _, err := r.refresh(context.Background()); err == nil {
		t.Fatal("a failed mint must be an error")
	}
	if got := string(getSecret(t, k, "gardenio", deployerSecret).Data["token"]); got != "ghs_OLD" {
		t.Errorf("token = %q, the current token must stay until it expires", got)
	}
}

func TestReleaseDeployerTokenFor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	iss := &githubAppIssuer{app: &fakeGithubApp{}, installationID: 7}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	on := map[string]string{releaseDeployerTokenSecretEnv: deployerSecret, releaseDeployerTokenRepoEnv: "https://github.com/mattbakerconsulting/gardenio"}

	if releaseDeployerTokenFor(iss, nil, "gardenio", env(nil), logger) != nil {
		t.Error("no Secret configured must mean no loop")
	}
	if releaseDeployerTokenFor(nil, nil, "gardenio", env(on), logger) != nil {
		t.Error("App off must mean no loop")
	}
	if releaseDeployerTokenFor(&githubAppIssuer{fleetOff: true}, nil, "gardenio", env(on), logger) != nil {
		t.Error("fleet App off (Lands only) must mean no loop")
	}
	bad := map[string]string{releaseDeployerTokenSecretEnv: deployerSecret, releaseDeployerTokenRepoEnv: "git@github.com:mattbakerconsulting/gardenio.git"}
	if releaseDeployerTokenFor(iss, nil, "gardenio", env(bad), logger) != nil {
		t.Error("a non-https repo must mean no loop")
	}
	// fakeGithubApp has no MintInstallationTokenWithPermissions; the real client does.
	if releaseDeployerTokenFor(iss, nil, "gardenio", env(on), logger) != nil {
		t.Error("an App client without a permission-set mint must mean no loop")
	}
	iss.app = &deployerCapableApp{fakeGithubApp: &fakeGithubApp{}}
	r := releaseDeployerTokenFor(iss, nil, "gardenio", env(on), logger)
	if r == nil || r.repo.String() != "mattbakerconsulting/gardenio" || r.installationID != 7 || r.secretName != deployerSecret {
		t.Fatalf("loop = %+v", r)
	}
}

// deployerCapableApp is a githubAppAPI that also mints with a permission set.
type deployerCapableApp struct {
	*fakeGithubApp
}

func (deployerCapableApp) MintInstallationTokenWithPermissions(context.Context, int64, []string, map[string]string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func deployerSecretObj(token, repos string, expires time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: deployerSecret, Namespace: "gardenio", ResourceVersion: "1",
			Annotations: map[string]string{annotationGithubRepos: repos, annotationGithubExpiresAt: expires.Format(time.RFC3339)}},
		Data: map[string][]byte{"token": []byte(token), "username": []byte("x-access-token")},
	}
}
