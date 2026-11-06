package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/podmanager"
)

// fakeGithubApp is a githubAppAPI that mints tokens "ghs_FAKE_n" and records
// every call. mintErr/revokeErr inject failures.
type fakeGithubApp struct {
	minted   [][]string // repo names per mint call
	installs []int64    // installation id per mint call
	lookups  int
	revoked  []string
	n        int
	expiry   time.Time
	// account is the GitHub login every installation is on, unless accounts
	// names the installation's own; instReads counts Installation calls.
	account   string
	accounts  map[int64]string
	instReads int
	suspended bool
	// repoInstalls maps "owner/name" (lower case) to the installation
	// RepoInstallation reports; a repo not in it is GitHub's 404. repoReads
	// counts RepoInstallation calls.
	repoInstalls map[string]int64
	repoReads    int
	// sizeKB is the size RepoSizeKB reports for every repo; sizeReads counts
	// its calls and sizeErr fails them.
	sizeKB    int64
	sizeReads int
	sizeErr   error

	mintErr, revokeErr, lookupErr error
}

func (f *fakeGithubApp) InstallationFor(context.Context, githubapp.RepoRef) (int64, error) {
	f.lookups++
	if f.lookupErr != nil {
		return 0, f.lookupErr
	}
	return 170295668, nil
}

func (f *fakeGithubApp) MintInstallationToken(_ context.Context, id int64, repos []string) (githubapp.Token, error) {
	if f.mintErr != nil {
		return githubapp.Token{}, f.mintErr
	}
	f.n++
	f.minted = append(f.minted, repos)
	f.installs = append(f.installs, id)
	return githubapp.NewToken(fmt.Sprintf("ghs_FAKE_%d", f.n), f.expiry), nil
}

func (f *fakeGithubApp) Installation(_ context.Context, id int64) (githubapp.Installation, error) {
	f.instReads++
	inst := githubapp.Installation{ID: id}
	inst.Account.Login = f.account
	if a, ok := f.accounts[id]; ok {
		inst.Account.Login = a
	}
	if f.suspended {
		at := time.Now()
		inst.SuspendedAt = &at
	}
	return inst, nil
}

func (f *fakeGithubApp) RepoInstallation(_ context.Context, repo githubapp.RepoRef) (int64, error) {
	f.repoReads++
	if id, ok := f.repoInstalls[strings.ToLower(repo.String())]; ok {
		return id, nil
	}
	return 0, &githubapp.HTTPError{Status: http.StatusNotFound, Message: "Not Found"}
}

func (f *fakeGithubApp) RepoSizeKB(context.Context, int64, githubapp.RepoRef) (int64, error) {
	f.sizeReads++
	return f.sizeKB, f.sizeErr
}

func (f *fakeGithubApp) Revoke(_ context.Context, tok githubapp.Token) error {
	f.revoked = append(f.revoked, tok.Reveal())
	return f.revokeErr
}

type ghTestEnv struct {
	iss *githubAppIssuer
	app *fakeGithubApp
	k8s *fake.Clientset
	log *bytes.Buffer
	now time.Time
}

func newGHTestEnv(t *testing.T, objs ...runtime.Object) *ghTestEnv {
	t.Helper()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	app := &fakeGithubApp{expiry: now.Add(time.Hour)}
	k := fake.NewSimpleClientset(objs...)
	buf := &bytes.Buffer{}
	return &ghTestEnv{
		iss: &githubAppIssuer{app: app, k8s: k, logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), now: func() time.Time { return now },
			project: ghProjects(map[string]config.ProjectCacheEntry{"gardenio": {}})},
		app: app, k8s: k, log: buf, now: now,
	}
}

// ghProjects is a project-cache lookup over a fixed map.
func ghProjects(m map[string]config.ProjectCacheEntry) func(string) (config.ProjectCacheEntry, bool) {
	return func(name string) (config.ProjectCacheEntry, bool) {
		e, ok := m[name]
		return e, ok
	}
}

func ghSpec() podmanager.AgentPodSpec {
	return podmanager.AgentPodSpec{
		Project: "gardenio", Role: "crew", AgentName: "c1", Namespace: "gardenio",
		GitURL:               "https://github.com/joyousaversion/gardenio.git",
		GitCredentialsSecret: "gardenio-git-credentials", // the shared PAT
	}
}

func getSecret(t *testing.T, k *fake.Clientset, ns, name string) *corev1.Secret {
	t.Helper()
	s, err := k.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret %s/%s: %v", ns, name, err)
	}
	return s
}

func TestGithubScope(t *testing.T) {
	spec := ghSpec()
	spec.ReferenceRepos = []podmanager.RepoRef{
		{URL: "https://github.com/joyousaversion/gascity-hq"},
		{URL: "https://github.com/joyousaversion/gardenio"},    // dup of primary
		{URL: "https://github.com/someone-else/secret-repo"},   // other owner: cannot be on this token
		{URL: "https://gitlab.com/acme/platform/gardenio.git"}, // not GitHub
	}
	owner, names, ok := githubScope(spec)
	if !ok || owner != "joyousaversion" || strings.Join(names, ",") != "gardenio,gascity-hq" {
		t.Fatalf("scope = %q %v %v", owner, names, ok)
	}
	spec.GitURL = "https://gitlab.com/acme/platform/gardenio.git"
	if _, _, ok := githubScope(spec); ok {
		t.Fatal("gitlab project must not be GitHub-scoped")
	}
}

func TestProvision_WritesScopedSecretAndRepointsSpec(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	spec.ReferenceRepos = []podmanager.RepoRef{{URL: "https://github.com/joyousaversion/gascity-hq"}}

	refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-bead1")
	if refuse {
		t.Fatalf("unexpected refusal: %s", reason)
	}
	if spec.GitCredentialsSecret != "agent-github-token-gd-bead1" {
		t.Fatalf("spec must point at the per-agent Secret, got %q", spec.GitCredentialsSecret)
	}
	if len(e.app.minted) != 1 || strings.Join(e.app.minted[0], ",") != "gardenio,gascity-hq" {
		t.Fatalf("mint scope = %v", e.app.minted)
	}
	sec := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-bead1")
	if sec.StringData["token"] != "ghs_FAKE_1" || sec.StringData["username"] != "x-access-token" {
		t.Fatalf("secret keys wrong: %v", sec.StringData)
	}
	for k, v := range map[string]string{"gardenio.io/type": "agent-token", "gardenio.io/provider": "github", "gardenio.io/bead": "gd-bead1"} {
		if sec.Labels[k] != v {
			t.Errorf("label %s = %q want %q", k, sec.Labels[k], v)
		}
	}
	if sec.Annotations[annotationGithubExpiresAt] != e.now.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("expiry annotation = %q", sec.Annotations[annotationGithubExpiresAt])
	}
	if e.app.lookups != 1 {
		t.Errorf("installation should be looked up when not pinned, lookups=%d", e.app.lookups)
	}
}

func TestProvision_PinnedInstallationSkipsLookup(t *testing.T) {
	e := newGHTestEnv(t)
	e.iss.installationID = 42
	spec := ghSpec()
	if refuse, r := e.iss.applyToSpec(context.Background(), &spec, "gd-b"); refuse {
		t.Fatal(r)
	}
	if e.app.lookups != 0 || e.app.installs[0] != 42 {
		t.Fatalf("lookups=%d installs=%v", e.app.lookups, e.app.installs)
	}
}

func TestApplyToSpec_NonGithubProjectUntouched(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	spec.GitURL = "https://gitlab.com/acme/platform/gardenio.git"
	if refuse, _ := e.iss.applyToSpec(context.Background(), &spec, "gd-b"); refuse {
		t.Fatal("a GitLab project is not this provider's concern and must not be refused")
	}
	if spec.GitCredentialsSecret != "gardenio-git-credentials" || len(e.app.minted) != 0 {
		t.Fatal("non-GitHub spec must be untouched")
	}
}

func TestApplyToSpec_NilIssuerIsNoop(t *testing.T) {
	var g *githubAppIssuer
	spec := ghSpec()
	if refuse, _ := g.applyToSpec(context.Background(), &spec, "gd-b"); refuse || spec.GitCredentialsSecret != "gardenio-git-credentials" {
		t.Fatal("disabled feature must leave the spec alone")
	}
	g.revoke(context.Background(), "gd-b", "gardenio", revocationTypeDespawn, time.Time{}) // must not panic
}

// Fail closed: every failure on a GitHub-hosted project refuses the spawn and
// leaves no per-agent Secret. The shared credential must never be substituted.
func TestApplyToSpec_FailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*ghTestEnv){
		"mint error":       func(e *ghTestEnv) { e.app.mintErr = errors.New("HTTP 422: Validation Failed") },
		"lookup error":     func(e *ghTestEnv) { e.app.lookupErr = errors.New("HTTP 404: Not Found") },
		"app init failure": func(e *ghTestEnv) { e.iss.app = nil; e.iss.initErr = errors.New("github app private key unreadable") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newGHTestEnv(t)
			mutate(e)
			spec := ghSpec()
			refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-b")
			if !refuse || reason == "" {
				t.Fatal("spawn must be refused")
			}
			if !strings.Contains(reason, "refusing to fall back") {
				t.Errorf("reason should state there is no fallback: %q", reason)
			}
			secs, _ := e.k8s.CoreV1().Secrets("gardenio").List(context.Background(), metav1.ListOptions{})
			if len(secs.Items) != 0 {
				t.Fatal("no Secret may be left behind")
			}
		})
	}
}

func TestApplyToSpec_NoBeadIDRefusesGithubProject(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	if refuse, _ := e.iss.applyToSpec(context.Background(), &spec, ""); !refuse {
		t.Fatal("cannot mint without a bead id; must refuse, not fall back")
	}
}

func TestProvision_SecretCreateFailureRevokesMintedToken(t *testing.T) {
	e := newGHTestEnv(t)
	e.k8s.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcd unavailable")
	})
	spec := ghSpec()
	if refuse, _ := e.iss.applyToSpec(context.Background(), &spec, "gd-b"); !refuse {
		t.Fatal("must refuse")
	}
	if len(e.app.revoked) != 1 || e.app.revoked[0] != "ghs_FAKE_1" {
		t.Fatalf("minted token must be revoked when it cannot be delivered, revoked=%v", e.app.revoked)
	}
}

func TestProvision_ConcurrentProvisionWinsCreateRaceReusesItsToken(t *testing.T) {
	e := newGHTestEnv(t)
	// Another provision for the same bead writes the Secret between this
	// provision's Get (NotFound) and its Create.
	e.k8s.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		won := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret).DeepCopy()
		won.StringData = map[string]string{"token": "ghs_WINNER", "username": githubTokenUsername}
		if err := e.k8s.Tracker().Add(won); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), won.Name)
	})
	spec := ghSpec()
	if refuse, r := e.iss.applyToSpec(context.Background(), &spec, "gd-race"); refuse {
		t.Fatalf("losing the create race to a concurrent provision must not refuse the spawn: %s", r)
	}
	if spec.GitCredentialsSecret != "agent-github-token-gd-race" {
		t.Fatalf("spec must point at the Secret the concurrent provision wrote, got %q", spec.GitCredentialsSecret)
	}
	if len(e.app.revoked) != 1 || e.app.revoked[0] != "ghs_FAKE_1" {
		t.Fatalf("the losing provision's own token must be revoked, revoked=%v", e.app.revoked)
	}
	if got := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-race"); got.StringData["token"] != "ghs_WINNER" {
		t.Fatalf("the winner's token must be left in place: %v", got.StringData)
	}
}

func TestProvision_AlreadyExistsRetiresOldTokenAndOverwrites(t *testing.T) {
	old := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-github-token-gd-b", Namespace: "gardenio"},
		Data:       map[string][]byte{"token": []byte("ghs_OLD")},
	}
	e := newGHTestEnv(t, old)
	spec := ghSpec()
	if refuse, r := e.iss.applyToSpec(context.Background(), &spec, "gd-b"); refuse {
		t.Fatal(r)
	}
	if len(e.app.revoked) != 1 || e.app.revoked[0] != "ghs_OLD" {
		t.Fatalf("stale token must be revoked, got %v", e.app.revoked)
	}
	if got := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-b"); got.StringData["token"] != "ghs_FAKE_1" {
		t.Fatalf("secret not overwritten: %v", got.StringData)
	}
}

func TestRevoke_RevokesAtGithubAndDeletesSecret(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	_, _ = e.iss.applyToSpec(context.Background(), &spec, "gd-b")
	// fake clientset keeps StringData; mimic the API server's conversion.
	sec := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-b")
	sec.Data = map[string][]byte{"token": []byte(sec.StringData["token"])}
	_, _ = e.k8s.CoreV1().Secrets("gardenio").Update(context.Background(), sec, metav1.UpdateOptions{})

	e.iss.revoke(context.Background(), "gd-b", "gardenio", revocationTypeDespawn, e.now)
	if len(e.app.revoked) != 1 || e.app.revoked[0] != "ghs_FAKE_1" {
		t.Fatalf("revoked = %v", e.app.revoked)
	}
	if _, err := e.k8s.CoreV1().Secrets("gardenio").Get(context.Background(), "agent-github-token-gd-b", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("secret should be deleted, err=%v", err)
	}
	// Idempotent: second call finds nothing and does nothing.
	e.iss.revoke(context.Background(), "gd-b", "gardenio", revocationTypeDespawn, e.now)
	if len(e.app.revoked) != 1 {
		t.Fatal("second revoke must be a no-op")
	}
}

func TestRevoke_GithubFailureKeepsSecretForRetry(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-github-token-gd-b", Namespace: "gardenio"},
		Data:       map[string][]byte{"token": []byte("ghs_X")},
	}
	e := newGHTestEnv(t, sec)
	e.app.revokeErr = errors.New("HTTP 500")
	e.iss.revoke(context.Background(), "gd-b", "gardenio", revocationTypeDespawn, e.now)
	getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-b") // still there → the sweep retries
}

func liveGithubSecret(bead string, expires time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: githubTokenSecretName(bead), Namespace: "gardenio",
			Labels: map[string]string{"gardenio.io/type": "agent-token", "gardenio.io/provider": "github", "gardenio.io/bead": bead, podmanager.LabelProject: "gardenio"},
			Annotations: map[string]string{
				annotationGithubExpiresAt: expires.UTC().Format(time.RFC3339),
				annotationGithubRepos:     "joyousaversion/gardenio,gascity-hq",
			},
		},
		Data: map[string][]byte{"token": []byte("ghs_OLD_" + bead), "username": []byte("x-access-token")},
	}
}

func TestSweep_RefreshesDueRevokesClosedLeavesFresh(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e := newGHTestEnv(t,
		liveGithubSecret("gd-due", now.Add(10*time.Minute)),    // inside the 20m window
		liveGithubSecret("gd-fresh", now.Add(50*time.Minute)),  // not due
		liveGithubSecret("gd-closed", now.Add(10*time.Minute)), // bead closed
	)
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-due": {Status: "in_progress"}, "gd-fresh": {Status: "in_progress"}, "gd-closed": {Status: "closed"},
	}}
	addAgentPod(t, e, "gd-due", "gd-fresh")
	st := e.iss.sweep(context.Background(), d, []string{"gardenio", ""})
	if st.Scanned != 3 || st.Refreshed != 1 || st.Revoked != 1 {
		t.Fatalf("stats = %+v", st)
	}
	due := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-due")
	if due.StringData["token"] != "ghs_FAKE_1" || due.Annotations[annotationGithubExpiresAt] != now.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("due secret not refreshed: %v %v", due.StringData, due.Annotations)
	}
	if strings.Join(e.app.minted[0], ",") != "gardenio,gascity-hq" {
		t.Errorf("refresh must re-scope to the same repos, got %v", e.app.minted[0])
	}
	if _, err := e.k8s.CoreV1().Secrets("gardenio").Get(context.Background(), "agent-github-token-gd-closed", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("closed bead's secret should be deleted")
	}
	if len(e.app.revoked) != 1 || e.app.revoked[0] != "ghs_OLD_gd-closed" {
		t.Errorf("closed bead's token must be revoked at GitHub: %v", e.app.revoked)
	}
	if got := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-fresh"); got.StringData["token"] != "" {
		t.Error("fresh secret must not be rewritten")
	}
}

func TestSweep_RefreshMintFailureKeepsCurrentToken(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e := newGHTestEnv(t, liveGithubSecret("gd-due", now.Add(5*time.Minute)))
	e.app.mintErr = errors.New("HTTP 503")
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-due": {Status: "in_progress"}}}
	addAgentPod(t, e, "gd-due")
	if st := e.iss.sweep(context.Background(), d, []string{"gardenio"}); st.Refreshed != 0 {
		t.Fatal("must not report a refresh")
	}
	if got := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-due"); string(got.Data["token"]) != "ghs_OLD_gd-due" {
		t.Fatal("current token must be left in place")
	}
}

// addAgentPod creates an agent pod in "gardenio" for each bead, shaped like the
// real ones (agent label + canonical bead-id annotation).
func addAgentPod(t *testing.T, e *ghTestEnv, beads ...string) {
	t.Helper()
	for _, b := range beads {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "crew-gardenio-" + b, Namespace: "gardenio",
			Labels:      map[string]string{podmanager.LabelAgent: "a-" + b},
			Annotations: map[string]string{podmanager.AnnotationBeadID: b},
		}}
		if _, err := e.k8s.CoreV1().Pods("gardenio").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// An open bead whose pod is gone (refused/deferred spawn, pod deleted before
// the bead closed) must not have its token re-minted by the sweep: that burns
// the installation's rate limit for a token nobody holds (gd-g2ztahU4Iq).
func TestSweep_DoesNotRemintOpenBeadWithNoPod(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e := newGHTestEnv(t,
		liveGithubSecret("gd-nopod", now.Add(5*time.Minute)),
		liveGithubSecret("gd-haspod", now.Add(5*time.Minute)),
	)
	addAgentPod(t, e, "gd-haspod")
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-nopod": {Status: "in_progress"}, "gd-haspod": {Status: "in_progress"},
	}}
	st := e.iss.sweep(context.Background(), d, []string{"gardenio"})
	if st.Refreshed != 1 || len(e.app.minted) != 1 {
		t.Fatalf("only the bead with a pod may be re-minted: stats=%+v mints=%d", st, len(e.app.minted))
	}
	if got := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-nopod"); string(got.Data["token"]) != "ghs_OLD_gd-nopod" || got.StringData["token"] != "" {
		t.Fatal("pod-less bead's Secret must be left untouched")
	}
}

// A failed pod list must not be read as "no pods" for revocation, and must not
// trigger re-mints either: closed beads are still retired, open ones skipped.
func TestSweep_PodListFailureSkipsRefreshButStillRevokesClosed(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e := newGHTestEnv(t,
		liveGithubSecret("gd-due", now.Add(5*time.Minute)),
		liveGithubSecret("gd-closed", now.Add(5*time.Minute)),
	)
	addAgentPod(t, e, "gd-due")
	e.k8s.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-due": {Status: "in_progress"}, "gd-closed": {Status: "closed"},
	}}
	st := e.iss.sweep(context.Background(), d, []string{"gardenio"})
	if st.Refreshed != 0 || len(e.app.minted) != 0 {
		t.Fatalf("no refresh when pods cannot be listed: stats=%+v", st)
	}
	if st.Revoked != 1 || len(e.app.revoked) != 1 {
		t.Fatalf("closed bead must still be revoked: stats=%+v revoked=%v", st, e.app.revoked)
	}
}

// The beads janitor must not touch GitHub token Secrets (it would call the
// beads RevokeBridgeToken and miss the GitHub revoke).
func TestBeadsJanitorIgnoresGithubSecrets(t *testing.T) {
	gh := liveGithubSecret("gd-closed", time.Now())
	k := fake.NewSimpleClientset(gh, agentTokenSecret("gd-closed", "gardenio", true))
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-closed": {Status: "closed"}}}
	j := newTokenJanitor(d, k, "gardenio", slog.Default(), nil)
	st := j.sweep(context.Background())
	if st.Scanned != 1 || st.Revoked != 1 {
		t.Fatalf("beads sweep should see only the beads secret: %+v", st)
	}
	if _, err := k.CoreV1().Secrets("gardenio").Get(context.Background(), gh.Name, metav1.GetOptions{}); err != nil {
		t.Fatal("github secret must be left for the github sweep")
	}
}

func TestJanitorGithubOnlyModeSkipsBeadsSweep(t *testing.T) {
	now := time.Now()
	e := newGHTestEnv(t, liveGithubSecret("gd-closed", now), agentTokenSecret("gd-other", "gardenio", true))
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-closed": {Status: "closed"}, "gd-other": {Status: "closed"}}}
	j := newTokenJanitor(d, e.k8s, "gardenio", slog.Default(), nil)
	j.beadsTokens = false // PER_AGENT_TOKENS off, GitHub App on
	j.github = e.iss
	j.githubNamespaces = []string{"gardenio"}
	j.sweep(context.Background())
	if len(d.revoked) != 0 {
		t.Fatalf("beads fleet tokens must not be swept when PER_AGENT_TOKENS is off: %v", d.revoked)
	}
	if len(e.app.revoked) != 1 {
		t.Fatalf("github token should be revoked: %v", e.app.revoked)
	}
}

// --- configuration / construction ---

func testKeyFile(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "private-key")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewGithubAppIssuer(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if newGithubAppIssuer(&config.Config{}, fake.NewSimpleClientset(), log, nil) != nil {
		t.Fatal("disabled by default → nil issuer")
	}
	if newGithubAppIssuer(nil, nil, log, nil) != nil {
		t.Fatal("nil config → nil issuer")
	}

	ok := newGithubAppIssuer(&config.Config{GithubAppEnabled: true, GithubAppID: "5029022", GithubAppPrivateKeyFile: testKeyFile(t), GithubAppInstallationID: 170295668}, fake.NewSimpleClientset(), log, nil)
	if ok == nil || ok.initErr != nil || ok.app == nil || ok.installationID != 170295668 {
		t.Fatalf("healthy issuer = %+v", ok)
	}

	// Enabled but unusable: a non-nil issuer that refuses (fail closed), not nil
	// (which would silently mean "use the shared token").
	for name, cfg := range map[string]*config.Config{
		"missing key file": {GithubAppEnabled: true, GithubAppID: "1", GithubAppPrivateKeyFile: "/nonexistent/key"},
		"bad app id":       {GithubAppEnabled: true, GithubAppID: "", GithubAppPrivateKeyFile: testKeyFile(t)},
	} {
		iss := newGithubAppIssuer(cfg, fake.NewSimpleClientset(), log, nil)
		if iss == nil || iss.initErr == nil {
			t.Fatalf("%s: want fail-closed issuer, got %+v", name, iss)
		}
		spec := ghSpec()
		if refuse, _ := iss.applyToSpec(context.Background(), &spec, "gd-b"); !refuse {
			t.Fatalf("%s: GitHub spawn must be refused", name)
		}
	}
}

func TestConfigGithubAppDefaultsOff(t *testing.T) {
	t.Setenv("GITHUB_APP_ENABLED", "")
	cfg := config.Parse()
	if cfg.GithubAppEnabled {
		t.Fatal("GITHUB_APP_ENABLED must default to false")
	}
	if cfg.GithubAppPrivateKeyFile != "/etc/gardenio/github-app/private-key" {
		t.Errorf("key file default = %q", cfg.GithubAppPrivateKeyFile)
	}
	t.Setenv("GITHUB_APP_ENABLED", "true")
	t.Setenv("GITHUB_APP_ID", " 5029022 ")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "170295668")
	cfg = config.Parse()
	if !cfg.GithubAppEnabled || cfg.GithubAppID != "5029022" || cfg.GithubAppInstallationID != 170295668 {
		t.Errorf("parsed cfg = %+v", cfg)
	}
}

// --- credential hygiene ---

// TestNoCredentialMaterialLeaks drives the whole lifecycle (provision, refresh,
// revoke, every failure mode, the refusal escalation) with a real private key
// and an httptest GitHub, then scans every log line, error string, reason,
// escalation payload, label and annotation for token / JWT / key material.
func TestNoCredentialMaterialLeaks(t *testing.T) {
	keyFile := testKeyFile(t)
	keyPEM, _ := os.ReadFile(keyFile)

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	app := &fakeGithubApp{expiry: now.Add(5 * time.Minute)} // due immediately
	k := fake.NewSimpleClientset()
	iss := &githubAppIssuer{app: app, k8s: k, logger: logger, now: func() time.Time { return now },
		project: ghProjects(map[string]config.ProjectCacheEntry{"gardenio": {}})}

	var surfaces []string
	spec := ghSpec()
	refuse, reason := iss.applyToSpec(context.Background(), &spec, "gd-b")
	if refuse {
		t.Fatal(reason)
	}
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-b": {Status: "in_progress"}}}
	iss.sweep(context.Background(), d, []string{"gardenio"}) // refresh
	iss.revoke(context.Background(), "gd-b", "gardenio", revocationTypeDespawn, now)

	// Failure modes.
	app.mintErr = errors.New("HTTP 422: Validation Failed")
	_, r1 := iss.applyToSpec(context.Background(), &spec, "gd-c")
	app.mintErr = nil
	iss.initErr = errors.New("github app client: githubapp: private key is not valid PEM")
	_, r2 := iss.applyToSpec(context.Background(), &spec, "gd-d")
	iss.initErr = nil
	app.revokeErr = errors.New("HTTP 500")
	iss.revokeQuietly(context.Background(), app, githubapp.NewToken("ghs_FAKE_1", time.Time{}))
	surfaces = append(surfaces, r1, r2)

	esc := buildGithubAppRefusalEscalation(spec, "gd-c", r1, now)
	escJSON, _ := json.Marshal(esc)
	surfaces = append(surfaces, string(escJSON), logs.String())

	// Everything the cluster/bead store ever sees about the live secret.
	sec := liveGithubSecret("gd-x", now)
	meta, _ := json.Marshal(map[string]any{"labels": sec.Labels, "annotations": sec.Annotations})
	surfaces = append(surfaces, string(meta))
	if got := getSecretOrNil(k, "gardenio", "agent-github-token-gd-b"); got != nil {
		m2, _ := json.Marshal(map[string]any{"labels": got.Labels, "annotations": got.Annotations})
		surfaces = append(surfaces, string(m2))
	}

	secrets := []string{"ghs_FAKE_", "ghs_OLD", "x-access-token-secret", "BEGIN RSA PRIVATE KEY", "MIIE", string(keyPEM)}
	for _, out := range surfaces {
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("credential material %q leaked in: %.120s", s, out)
			}
		}
		if strings.Count(out, ".") >= 2 && strings.Contains(out, "eyJ") {
			t.Errorf("JWT-shaped string leaked in: %.120s", out)
		}
	}
	if logs.Len() == 0 {
		t.Fatal("expected log output to scan")
	}
}

func getSecretOrNil(k *fake.Clientset, ns, name string) *corev1.Secret {
	s, err := k.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return s
}

func TestRefusalEscalationShape(t *testing.T) {
	req := buildGithubAppRefusalEscalation(ghSpec(), "gd-b", "mint failed", time.Now())
	if req.Type != "escalation" || !strings.HasPrefix(req.IdempotencyKey, "github-app-mint:") {
		t.Fatalf("req = %+v", req)
	}
	if !strings.Contains(string(req.Fields), "NOT used as a fallback") {
		t.Error("context must state there is no fallback")
	}
}

// A git_url with embedded userinfo must not be echoed into the refusal reason,
// the structured refusal log, or the escalation bead (gd-5w5r7rNTMO).
func TestRefusal_RedactsGitURLUserinfo(t *testing.T) {
	const fakePass = "FAKEPASS_s3cr3t@x" // includes '@' to exercise the greedy match
	e := newGHTestEnv(t)
	e.app.mintErr = errors.New("mint failed for https://oauth2:" + fakePass + "@github.com/o/r.git: 502")
	spec := ghSpec()
	spec.GitURL = "https://user:" + fakePass + "@github.com/joyousaversion/gardenio.git"

	refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-b")
	if !refuse {
		t.Fatal("mint failure must refuse")
	}
	logger := slog.New(slog.NewTextHandler(e.log, nil))
	recordGithubAppRefusal(context.Background(), logger, nil, spec, "gd-b", reason)
	req := buildGithubAppRefusalEscalation(spec, "gd-b", reason, time.Now())
	// Even a reason that was not redacted upstream must be scrubbed at the sinks.
	raw := buildGithubAppRefusalEscalation(spec, "gd-b", "bad "+spec.GitURL, time.Now())

	for name, got := range map[string]string{
		"reason":          reason,
		"log":             e.log.String(),
		"escalation":      string(req.Fields) + req.Title,
		"escalation(raw)": string(raw.Fields) + raw.Title,
	} {
		if strings.Contains(got, "FAKEPASS") || strings.Contains(got, "s3cr3t") {
			t.Errorf("%s leaks git_url credential: %s", name, got)
		}
	}
	if !strings.Contains(string(req.Fields), "github.com/joyousaversion/gardenio.git") {
		t.Error("redaction must keep the host/path so the escalation stays actionable")
	}
}

func TestRedactURLUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r.git":             "https://github.com/o/r.git",
		"https://tok@github.com/o/r.git":         "https://github.com/o/r.git",
		"https://u:p@ss@github.com/o/r.git":      "https://github.com/o/r.git",
		"a https://u:p@h/x and ssh://git@h2/y z": "a https://h/x and ssh://h2/y z",
		"https://github.com/o/user@name.git":     "https://github.com/o/user@name.git",
	} {
		if got := redactURLUserinfo(in); got != want {
			t.Errorf("redactURLUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

// The App private key Secret must never be mountable into an agent pod, even
// when a project bead names it with a valid {project}- prefix (plan §6).
func TestPrivateKeySecretIsForbiddenPodSecret(t *testing.T) {
	for _, name := range []string{"gardenio-github-app-private-key", "Gardenio-GitHub-App-Private-Key", "crew-github-app-private-key"} {
		if !isForbiddenPodSecret(name) {
			t.Errorf("%q must be on the agent-pod mount deny-list", name)
		}
	}
	if isForbiddenPodSecret("gardenio-git-credentials") || isForbiddenPodSecret("agent-github-token-gd-b") {
		t.Error("ordinary git credential Secrets must stay mountable")
	}
}

// toAPIServerShape mimics the API server's StringData→Data conversion that the
// fake clientset does not perform.
func toAPIServerShape(t *testing.T, e *ghTestEnv, ns, name string) {
	t.Helper()
	sec := getSecret(t, e.k8s, ns, name)
	if tok, ok := sec.StringData["token"]; ok {
		sec.Data = map[string][]byte{"token": []byte(tok), "username": []byte(sec.StringData["username"])}
		sec.StringData = nil
	}
	if _, err := e.k8s.CoreV1().Secrets(ns).Update(context.Background(), sec, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// B2: a duplicate/redelivered spawn event for a live agent must not retire the
// token the running pod is holding.
func TestProvision_DuplicateSpawnForLiveAgentReusesTokenAndRevokesNothing(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	if refuse, r := e.iss.applyToSpec(context.Background(), &spec, "gd-live"); refuse {
		t.Fatal(r)
	}
	toAPIServerShape(t, e, "gardenio", "agent-github-token-gd-live")

	dup := ghSpec()
	if refuse, r := e.iss.applyToSpec(context.Background(), &dup, "gd-live"); refuse {
		t.Fatal(r)
	}
	if dup.GitCredentialsSecret != "agent-github-token-gd-live" {
		t.Fatalf("duplicate spawn must repoint at the live Secret, got %q", dup.GitCredentialsSecret)
	}
	if len(e.app.revoked) != 0 {
		t.Fatalf("duplicate spawn revoked the live agent's token: %v", e.app.revoked)
	}
	if len(e.app.minted) != 1 {
		t.Fatalf("duplicate spawn must not mint again, mints=%d", len(e.app.minted))
	}
	sec := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-live")
	if string(sec.Data["token"]) != "ghs_FAKE_1" {
		t.Fatalf("live token was overwritten: %q", sec.Data["token"])
	}
}

func TestProvision_NearExpiryTokenRemintedWithoutRevokingLiveToken(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	_, _ = e.iss.applyToSpec(context.Background(), &spec, "gd-live")
	toAPIServerShape(t, e, "gardenio", "agent-github-token-gd-live")
	// Time passes: only 5 minutes (< refresh window) of the first token remain.
	e.iss.now = func() time.Time { return e.now.Add(55 * time.Minute) }
	e.app.expiry = e.now.Add(115 * time.Minute)

	dup := ghSpec()
	if refuse, r := e.iss.applyToSpec(context.Background(), &dup, "gd-live"); refuse {
		t.Fatal(r)
	}
	if len(e.app.minted) != 2 {
		t.Fatalf("stale token must be re-minted, mints=%d", len(e.app.minted))
	}
	if len(e.app.revoked) != 0 {
		t.Fatalf("a still-valid token of the same scope is left to lapse, not revoked: %v", e.app.revoked)
	}
	if got := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-live"); got.StringData["token"] != "ghs_FAKE_2" {
		t.Fatalf("secret not refreshed: %v", got.StringData)
	}
}

func TestProvision_DifferentRepoScopeRetiresOldToken(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	_, _ = e.iss.applyToSpec(context.Background(), &spec, "gd-live")
	toAPIServerShape(t, e, "gardenio", "agent-github-token-gd-live")

	wider := ghSpec()
	wider.ReferenceRepos = []podmanager.RepoRef{{URL: "https://github.com/joyousaversion/gascity-hq"}}
	if refuse, r := e.iss.applyToSpec(context.Background(), &wider, "gd-live"); refuse {
		t.Fatal(r)
	}
	if len(e.app.revoked) != 1 || e.app.revoked[0] != "ghs_FAKE_1" {
		t.Fatalf("token of a different scope must be revoked, got %v", e.app.revoked)
	}
}

// The stuck-restart path revokes first (Secret deleted), so the next spawn
// mints fresh rather than reusing.
func TestProvision_AfterRevokeMintsFresh(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	_, _ = e.iss.applyToSpec(context.Background(), &spec, "gd-live")
	toAPIServerShape(t, e, "gardenio", "agent-github-token-gd-live")
	e.iss.revoke(context.Background(), "gd-live", "gardenio", revocationTypeStuckRestart, e.now)

	again := ghSpec()
	if refuse, r := e.iss.applyToSpec(context.Background(), &again, "gd-live"); refuse {
		t.Fatal(r)
	}
	if len(e.app.minted) != 2 {
		t.Fatalf("restart after revoke must mint a fresh token, mints=%d", len(e.app.minted))
	}
}

// B3: the shared PAT must not reach the pod as env. A project bead's secrets[]
// mapping of GIT_TOKEN/GIT_USERNAME is dropped (not repointed: env never
// refreshes), and nothing may reference the shared Secret.
func TestApplyToSpec_DropsSharedPATFromSecretEnv(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	spec.SecretEnv = []podmanager.SecretEnvSource{
		{EnvName: "GIT_TOKEN", SecretName: "gardenio-git-credentials", SecretKey: "token"},
		{EnvName: "GIT_USERNAME", SecretName: "gardenio-git-credentials", SecretKey: "username"},
		{EnvName: "GH_TOKEN", SecretName: "gardenio-git-credentials", SecretKey: "token"},
		{EnvName: "CLAUDE_CODE_OAUTH_TOKEN", SecretName: "gardenio-claude-oauth", SecretKey: "token"},
		{EnvName: "GITLAB_TOKEN", SecretName: "gardenio-gitlab", SecretKey: "token"},
	}
	if refuse, r := e.iss.applyToSpec(context.Background(), &spec, "gd-b"); refuse {
		t.Fatal(r)
	}
	for _, se := range spec.SecretEnv {
		if se.SecretName == "gardenio-git-credentials" || se.EnvName == "GIT_TOKEN" || se.EnvName == "GIT_USERNAME" {
			t.Errorf("shared git credential still reaches the pod as env: %+v", se)
		}
	}
	got := map[string]bool{}
	for _, se := range spec.SecretEnv {
		got[se.EnvName] = true
	}
	if !got["CLAUDE_CODE_OAUTH_TOKEN"] || !got["GITLAB_TOKEN"] || len(spec.SecretEnv) != 2 {
		t.Errorf("unrelated secret env must be preserved, got %+v", spec.SecretEnv)
	}
}

// A refused spawn leaves the spec's env untouched, but it is never created, so
// the PAT still cannot reach a pod.
func TestApplyToSpec_NonGithubProjectKeepsGitEnv(t *testing.T) {
	e := newGHTestEnv(t)
	spec := ghSpec()
	spec.GitURL = "https://gitlab.com/acme/platform/gardenio.git"
	spec.SecretEnv = []podmanager.SecretEnvSource{{EnvName: "GIT_TOKEN", SecretName: "gardenio-git-credentials", SecretKey: "token"}}
	if refuse, _ := e.iss.applyToSpec(context.Background(), &spec, "gd-b"); refuse {
		t.Fatal("non-github project must not be refused")
	}
	if len(spec.SecretEnv) != 1 {
		t.Fatalf("non-github project keeps its configured env, got %+v", spec.SecretEnv)
	}
}

// B1: the reconciler door, wired through the real issuer.
func TestReconcilerApplier_GithubProjectGetsPerAgentSecretOnly(t *testing.T) {
	e := newGHTestEnv(t)
	a := e.iss.reconcilerApplier(slog.New(slog.NewTextHandler(e.log, nil)), nil)
	spec := ghSpec()
	spec.SecretEnv = []podmanager.SecretEnvSource{{EnvName: "GIT_TOKEN", SecretName: "gardenio-git-credentials", SecretKey: "token"}}
	if refuse, r := a.Apply(context.Background(), &spec, "gd-rec"); refuse {
		t.Fatal(r)
	}
	if spec.GitCredentialsSecret != "agent-github-token-gd-rec" || len(spec.SecretEnv) != 0 {
		t.Fatalf("reconciler-created pod must carry agent-github-token-<bead> and never the project secret: %+v", spec)
	}
}

func TestReconcilerApplier_MintFailureRefusesAndNeverFallsBackToPAT(t *testing.T) {
	e := newGHTestEnv(t)
	e.app.mintErr = errors.New("github 503")
	a := e.iss.reconcilerApplier(slog.New(slog.NewTextHandler(e.log, nil)), nil)
	spec := ghSpec()
	refuse, reason := a.Apply(context.Background(), &spec, "gd-rec")
	if !refuse || reason == "" {
		t.Fatalf("mint failure must refuse, got refuse=%v", refuse)
	}
	if strings.Contains(e.log.String(), "ghs_") || strings.Contains(reason, "ghs_") {
		t.Fatal("token material leaked")
	}
}

func TestReconcilerApplier_ReleaseRevokesToken(t *testing.T) {
	e := newGHTestEnv(t)
	a := e.iss.reconcilerApplier(slog.New(slog.NewTextHandler(e.log, nil)), nil)
	spec := ghSpec()
	_, _ = a.Apply(context.Background(), &spec, "gd-rec")
	toAPIServerShape(t, e, "gardenio", "agent-github-token-gd-rec")
	a.Release(context.Background(), spec, "gd-rec")
	if len(e.app.revoked) != 1 || e.app.revoked[0] != "ghs_FAKE_1" {
		t.Fatalf("revoked = %v", e.app.revoked)
	}
}

// A duplicate spawn for a live agent reuses its Secret; when that spawn's own
// CreateAgentPod then fails transiently (e.g. a missing Secret) the cleanup must
// not revoke or delete the token the running pod is using.
func TestReconcilerApplier_ReleaseKeepsReusedLiveAgentToken(t *testing.T) {
	e := newGHTestEnv(t)
	a := e.iss.reconcilerApplier(slog.New(slog.NewTextHandler(e.log, nil)), nil)
	live := ghSpec()
	if refuse, r := a.Apply(context.Background(), &live, "gd-live"); refuse {
		t.Fatal(r)
	}
	toAPIServerShape(t, e, "gardenio", "agent-github-token-gd-live")

	dup := ghSpec()
	if refuse, r := a.Apply(context.Background(), &dup, "gd-live"); refuse {
		t.Fatal(r)
	}
	a.Release(context.Background(), dup, "gd-live") // dup's CreateAgentPod failed
	if len(e.app.revoked) != 0 {
		t.Fatalf("duplicate spawn's failed create revoked the live agent's token: %v", e.app.revoked)
	}
	sec := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-live")
	if string(sec.Data["token"]) != "ghs_FAKE_1" {
		t.Fatalf("live Secret was changed: %q", sec.Data["token"])
	}
}

// Same, for the event door's cleanup (event_handler.go), and for a refresh-in-place:
// only the spawn that created the Secret may revoke it on a failed create.
func TestRevokeIfCreated_OnlyRevokesWhatThisSpawnCreated(t *testing.T) {
	ctx := context.Background()
	e := newGHTestEnv(t)
	spec := ghSpec()
	_, _ = e.iss.applyToSpec(ctx, &spec, "gd-live")
	toAPIServerShape(t, e, "gardenio", "agent-github-token-gd-live")

	// Duplicate spawn reuses the Secret; its failed create must leave it alone.
	dup := ghSpec()
	_, _ = e.iss.applyToSpec(ctx, &dup, "gd-live")
	e.iss.revokeIfCreated(ctx, "gd-live", "gardenio", revocationTypeDespawn)
	if len(e.app.revoked) != 0 || getSecretOrNil(e.k8s, "gardenio", "agent-github-token-gd-live") == nil {
		t.Fatalf("reused token was revoked/deleted: revoked=%v", e.app.revoked)
	}

	// Token inside the refresh window: this spawn re-mints into the existing
	// Secret. That is not a first provision, so a failed create must not
	// delete the Secret out from under the live pod either.
	e.iss.now = func() time.Time { return e.now.Add(45 * time.Minute) }
	near := ghSpec()
	_, _ = e.iss.applyToSpec(ctx, &near, "gd-live")
	e.iss.revokeIfCreated(ctx, "gd-live", "gardenio", revocationTypeDespawn)
	if len(e.app.revoked) != 0 || getSecretOrNil(e.k8s, "gardenio", "agent-github-token-gd-live") == nil {
		t.Fatalf("refreshed Secret was revoked/deleted: revoked=%v", e.app.revoked)
	}

	// A first spawn that created the Secret owns it: failed create revokes it.
	first := ghSpec()
	_, _ = e.iss.applyToSpec(ctx, &first, "gd-new")
	toAPIServerShape(t, e, "gardenio", "agent-github-token-gd-new")
	e.iss.revokeIfCreated(ctx, "gd-new", "gardenio", revocationTypeDespawn)
	if len(e.app.revoked) != 1 || getSecretOrNil(e.k8s, "gardenio", "agent-github-token-gd-new") != nil {
		t.Fatalf("created token not retired: revoked=%v", e.app.revoked)
	}
}

// Every CreateAgentPod call site outside podmanager must be accounted for here.
// A new pod-creation door that skips the git-credential hook silently falls
// back to the shared PAT when the GitHub App flag is on, so adding one must
// fail this test until it is routed through the issuer.
func TestEveryPodCreationDoorIsRoutedThroughIssuer(t *testing.T) {
	want := map[string]int{
		"cmd/controller/event_handler.go":        2, // spawn + stuck-restart: ghApp.applyToSpec
		"internal/reconciler/reconcile_steps.go": 1, // gitCreds.Apply (SetGitCredentialApplier)
	}
	got := map[string]int{}
	root := filepath.Join("..", "..")
	for _, dir := range []string{"cmd", "internal"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/podmanager/") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			if n := strings.Count(string(b), ".CreateAgentPod("); n > 0 {
				got[rel] = n
			}
			return nil
		})
	}
	for f, n := range want {
		if got[f] != n {
			t.Errorf("%s: %d CreateAgentPod call sites, want %d", f, got[f], n)
		}
	}
	for f := range got {
		if _, ok := want[f]; !ok {
			t.Errorf("%s calls CreateAgentPod but is not routed through the GitHub App issuer; wire it and list it here", f)
		}
	}
	// And each door really invokes the issuer.
	mainSrc, _ := os.ReadFile("event_handler.go")
	if c := strings.Count(string(mainSrc), "ghApp.applyToSpec("); c != 2 {
		t.Errorf("event_handler.go: %d ghApp.applyToSpec calls, want 2", c)
	}
	recSrc, _ := os.ReadFile(filepath.Join(root, "internal/reconciler/reconcile_steps.go"))
	if !strings.Contains(string(recSrc), "r.gitCreds.Apply(") {
		t.Error("reconcile_steps.go does not invoke the git credential hook")
	}
}

// --- two Apps: fleet and Lands (step 8.7) ---

// ghTenant is a tenant land as the Lands provisioner writes it (provision.go):
// github_app=lands and the installation pinned from the setup callback.
func ghTenant() config.ProjectCacheEntry {
	return config.ProjectCacheEntry{LandKind: "tenant", GithubApp: "lands", GithubInstallationID: 9001}
}

// newGHTwoAppEnv is newGHTestEnv plus a separate fake Lands App and a tenant
// land "tomato-patch" next to the operator's "gardenio".
func newGHTwoAppEnv(t *testing.T, objs ...runtime.Object) (*ghTestEnv, *fakeGithubApp) {
	t.Helper()
	e := newGHTestEnv(t, objs...)
	e.iss.installationID = 42 // the fleet pin must never reach a Lands mint
	lands := &fakeGithubApp{expiry: e.now.Add(time.Hour), account: "applicant", repoInstalls: map[string]int64{"applicant/tomatoes": 9001}}
	e.iss.lands = lands
	e.iss.project = ghProjects(map[string]config.ProjectCacheEntry{"gardenio": {}, "tomato-patch": ghTenant()})
	return e, lands
}

func tenantSpec() podmanager.AgentPodSpec {
	return podmanager.AgentPodSpec{
		Project: "tomato-patch", Role: "crew", AgentName: "t1", Namespace: "land-tomato-patch",
		GitURL: "https://github.com/applicant/tomatoes.git",
		ReferenceRepos: []podmanager.RepoRef{
			{URL: "https://github.com/applicant/private-notes"}, // same owner: still never on a Lands token
			{URL: "https://github.com/joyousaversion/gardenio"},
		},
	}
}

func TestResolveGithubApp(t *testing.T) {
	iss := &githubAppIssuer{installationID: 42, project: ghProjects(map[string]config.ProjectCacheEntry{
		"operator-default":  {},
		"operator-fleet":    {LandKind: "operator", GithubApp: "fleet"},
		"operator-lands":    {GithubApp: "lands", GithubInstallationID: 7},
		"tenant":            ghTenant(),
		"tenant-mixed-case": {LandKind: "tenant", GithubApp: " Lands ", GithubInstallationID: 9001},
		"tenant-no-app":     {LandKind: "tenant", GithubInstallationID: 9001},
		"tenant-fleet":      {LandKind: "tenant", GithubApp: "fleet", GithubInstallationID: 9001},
		"tenant-fleet-caps": {LandKind: "tenant", GithubApp: "FLEET"},
		"tenant-no-pin":     {LandKind: "tenant", GithubApp: "lands"},
		"odd-kind-fleet":    {LandKind: "tennant", GithubApp: "fleet"}, // not operator → tenant rules
		"unknown-app":       {GithubApp: "gardenio-io"},
	})}
	for name, want := range map[string]githubMint{
		"operator-default":  {app: githubapp.Fleet, installationID: 42},
		"operator-fleet":    {app: githubapp.Fleet, installationID: 42},
		"operator-lands":    {app: githubapp.Lands, installationID: 7},
		"tenant":            {app: githubapp.Lands, installationID: 9001},
		"tenant-mixed-case": {app: githubapp.Lands, installationID: 9001},
	} {
		got, err := iss.resolveGithubApp(name)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", name, got, err, want)
		}
	}
	for _, name := range []string{"tenant-no-app", "tenant-fleet", "tenant-fleet-caps", "tenant-no-pin", "odd-kind-fleet", "unknown-app", "not-cached", ""} {
		if got, err := iss.resolveGithubApp(name); err == nil {
			t.Errorf("%s: want refusal, got %+v", name, got)
		}
	}
	if _, err := (&githubAppIssuer{}).resolveGithubApp("gardenio"); err == nil {
		t.Error("no project lookup: a project must be refused, never assumed to be the operator's")
	}
}

func TestGithubScopeFor_LandsAppIsOneRepo(t *testing.T) {
	owner, names, ok := githubScopeFor(tenantSpec(), githubapp.Lands)
	if !ok || owner != "applicant" || strings.Join(names, ",") != "tomatoes" {
		t.Fatalf("lands scope = %q %v %v", owner, names, ok)
	}
	if _, names, _ := githubScopeFor(tenantSpec(), githubapp.Fleet); strings.Join(names, ",") != "tomatoes,private-notes" {
		t.Fatalf("fleet scope keeps same-owner reference repos, got %v", names)
	}
}

func TestProvision_TenantLandMintsWithLandsAppPinnedInstallationOneRepo(t *testing.T) {
	e, lands := newGHTwoAppEnv(t)
	spec := tenantSpec()
	if refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-t1"); refuse {
		t.Fatalf("unexpected refusal: %s", reason)
	}
	if e.app.n != 0 || e.app.lookups != 0 {
		t.Fatalf("the fleet App must not be touched for a tenant: mints=%d lookups=%d", e.app.n, e.app.lookups)
	}
	if len(lands.minted) != 1 || strings.Join(lands.minted[0], ",") != "tomatoes" || lands.installs[0] != 9001 || lands.lookups != 0 {
		t.Fatalf("lands mint = repos %v installs %v lookups %d; want [tomatoes] from pinned 9001, no lookup", lands.minted, lands.installs, lands.lookups)
	}
	if spec.GitCredentialsSecret != "agent-github-token-gd-t1" {
		t.Fatalf("spec secret = %q", spec.GitCredentialsSecret)
	}
	sec := getSecret(t, e.k8s, "land-tomato-patch", "agent-github-token-gd-t1")
	for k, v := range map[string]string{annotationGithubApp: "lands", annotationGithubInstallation: "9001", annotationGithubRepos: "applicant/tomatoes"} {
		if sec.Annotations[k] != v {
			t.Errorf("annotation %s = %q want %q", k, sec.Annotations[k], v)
		}
	}
	if sec.Labels[podmanager.LabelProject] != "tomato-patch" {
		t.Errorf("project label = %q", sec.Labels[podmanager.LabelProject])
	}
}

func TestProvision_OperatorProjectStillMintsWithFleetApp(t *testing.T) {
	e, lands := newGHTwoAppEnv(t)
	spec := ghSpec()
	spec.ReferenceRepos = []podmanager.RepoRef{{URL: "https://github.com/joyousaversion/gascity-hq"}}
	if refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-op"); refuse {
		t.Fatal(reason)
	}
	if lands.n != 0 || e.app.n != 1 || e.app.installs[0] != 42 || strings.Join(e.app.minted[0], ",") != "gardenio,gascity-hq" {
		t.Fatalf("fleet mint = %v %v, lands mints = %d", e.app.minted, e.app.installs, lands.n)
	}
	if a := getSecret(t, e.k8s, "gardenio", "agent-github-token-gd-op").Annotations[annotationGithubApp]; a != "fleet" {
		t.Errorf("app annotation = %q", a)
	}
}

// A tenant project can never select the fleet App: whatever its github_app
// says, the fleet App mints nothing and no Secret is written.
func TestApplyToSpec_TenantProjectNeverSelectsFleetApp(t *testing.T) {
	for _, entry := range []config.ProjectCacheEntry{
		{LandKind: "tenant"},
		{LandKind: "tenant", GithubApp: "fleet", GithubInstallationID: 9001},
		{LandKind: "tenant", GithubApp: " Fleet "},
		{LandKind: "tenant", GithubApp: "lands"}, // no pinned installation: no lookup either
		{LandKind: "tenant", GithubApp: "gardenio-io", GithubInstallationID: 9001},
	} {
		e, lands := newGHTwoAppEnv(t)
		e.iss.project = ghProjects(map[string]config.ProjectCacheEntry{"tomato-patch": entry})
		spec := tenantSpec()
		refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-t1")
		if !refuse {
			t.Fatalf("%+v: tenant spawn must be refused", entry)
		}
		if e.app.n+e.app.lookups+lands.n+lands.lookups != 0 {
			t.Fatalf("%+v: nothing may be minted or looked up (fleet %d/%d, lands %d/%d)", entry, e.app.n, e.app.lookups, lands.n, lands.lookups)
		}
		if spec.GitCredentialsSecret != "" || getSecretOrNil(e.k8s, "land-tomato-patch", "agent-github-token-gd-t1") != nil {
			t.Fatalf("%+v: no credential may be handed out", entry)
		}
		if strings.Contains(reason, "ghs_") {
			t.Fatalf("reason leaks a token: %s", reason)
		}
	}
}

func TestApplyToSpec_LandsAppUnavailableRefusesTenantOnly(t *testing.T) {
	e, _ := newGHTwoAppEnv(t)
	e.iss.lands, e.iss.landsErr = nil, fmt.Errorf("%w: lands", githubapp.ErrNotConfigured)
	spec := tenantSpec()
	if refuse, _ := e.iss.applyToSpec(context.Background(), &spec, "gd-t1"); !refuse {
		t.Fatal("tenant spawn must be refused when the Lands App is unavailable")
	}
	if e.app.n != 0 {
		t.Fatal("no fallback to the fleet App")
	}
	op := ghSpec()
	if refuse, reason := e.iss.applyToSpec(context.Background(), &op, "gd-op"); refuse {
		t.Fatalf("operator project must be unaffected: %s", reason)
	}
}

func TestApplyToSpec_FleetOffLeavesOperatorAloneButMintsTenant(t *testing.T) {
	e, lands := newGHTwoAppEnv(t)
	e.iss.app, e.iss.fleetOff = nil, true
	op := ghSpec()
	if refuse, _ := e.iss.applyToSpec(context.Background(), &op, "gd-op"); refuse || op.GitCredentialsSecret != "gardenio-git-credentials" {
		t.Fatalf("fleet App off: an operator project keeps its configured credential, got refuse=%v secret=%q", refuse, op.GitCredentialsSecret)
	}
	spec := tenantSpec()
	if refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-t1"); refuse || lands.n != 1 {
		t.Fatalf("tenant must still mint with the Lands App: refuse=%v (%s) mints=%d", refuse, reason, lands.n)
	}
}

// A live fleet token is not reused for a project that now mints with the
// Lands App, even over the same one-repo scope; it is revoked with the App
// that minted it.
func TestProvision_FleetSecretNotReusedForLandsProject(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	old := liveGithubSecret("gd-t1", now.Add(50*time.Minute))
	old.Namespace = "land-tomato-patch"
	old.Labels[podmanager.LabelProject] = "tomato-patch"
	old.Annotations[annotationGithubRepos] = "applicant/tomatoes" // no app annotation: a fleet token
	e, lands := newGHTwoAppEnv(t, old)
	spec := tenantSpec()
	if refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-t1"); refuse {
		t.Fatal(reason)
	}
	if lands.n != 1 {
		t.Fatalf("lands mints = %d, want a fresh Lands token", lands.n)
	}
	if len(e.app.revoked) != 1 || e.app.revoked[0] != "ghs_OLD_gd-t1" || len(lands.revoked) != 0 {
		t.Fatalf("old fleet token must be revoked by the fleet App: fleet %v lands %v", e.app.revoked, lands.revoked)
	}
	if a := getSecret(t, e.k8s, "land-tomato-patch", "agent-github-token-gd-t1").Annotations[annotationGithubApp]; a != "lands" {
		t.Errorf("app annotation = %q", a)
	}
}

func tenantGithubSecret(bead string, expires time.Time, app string) *corev1.Secret {
	sec := liveGithubSecret(bead, expires)
	sec.Namespace = "land-tomato-patch"
	sec.Labels[podmanager.LabelProject] = "tomato-patch"
	sec.Annotations[annotationGithubRepos] = "applicant/tomatoes"
	if app != "" {
		sec.Annotations[annotationGithubApp] = app
		sec.Annotations[annotationGithubInstallation] = "9001"
	}
	return sec
}

func TestSweep_TenantRefreshUsesLandsAppAndLapsesAFleetToken(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, lands := newGHTwoAppEnv(t,
		tenantGithubSecret("gd-lands", now.Add(10*time.Minute), "lands"),
		tenantGithubSecret("gd-fleet", now.Add(10*time.Minute), ""), // minted by the fleet App before 8.7
	)
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-lands": {Status: "in_progress"}, "gd-fleet": {Status: "in_progress"}}}
	for _, b := range []string{"gd-lands", "gd-fleet"} {
		if _, err := e.k8s.CoreV1().Pods("land-tomato-patch").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "pod-" + b, Namespace: "land-tomato-patch",
			Labels:      map[string]string{podmanager.LabelAgent: "x"},
			Annotations: map[string]string{podmanager.AnnotationBeadID: b},
		}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"})
	if st.Refreshed != 1 {
		t.Fatalf("stats = %+v, want only the Lands token refreshed", st)
	}
	if e.app.n != 0 || e.app.lookups != 0 {
		t.Fatalf("the fleet App must not re-mint for a tenant: mints=%d lookups=%d", e.app.n, e.app.lookups)
	}
	if len(lands.minted) != 1 || strings.Join(lands.minted[0], ",") != "tomatoes" || lands.installs[0] != 9001 {
		t.Fatalf("lands refresh = %v %v", lands.minted, lands.installs)
	}
	if got := getSecret(t, e.k8s, "land-tomato-patch", "agent-github-token-gd-fleet"); got.StringData["token"] != "" {
		t.Error("the fleet-minted tenant Secret must be left to lapse, not refreshed")
	}
}

func TestSweep_RunsWithOnlyTheLandsApp(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, lands := newGHTwoAppEnv(t, tenantGithubSecret("gd-closed", now.Add(30*time.Minute), "lands"))
	e.iss.app, e.iss.fleetOff = nil, true
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-closed": {Status: "closed"}}}
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Revoked != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if len(lands.revoked) != 1 || lands.revoked[0] != "ghs_OLD_gd-closed" {
		t.Fatalf("closed tenant token must be revoked with the Lands App: %v", lands.revoked)
	}
}

func TestNewGithubAppIssuer_LandsApp(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	key := testKeyFile(t)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	landsEnv := map[string]string{"LANDS_GITHUB_APP_ID": "6100100", "LANDS_GITHUB_APP_PRIVATE_KEY_PATH": key}

	if newGithubAppIssuerFrom(&config.Config{}, fake.NewSimpleClientset(), log, nil, env(nil), os.ReadFile) != nil {
		t.Fatal("neither App configured → nil issuer")
	}

	only := newGithubAppIssuerFrom(&config.Config{}, fake.NewSimpleClientset(), log, nil, env(landsEnv), os.ReadFile)
	if only == nil || !only.fleetOff || only.app != nil || only.lands == nil || only.landsErr != nil || only.project == nil {
		t.Fatalf("lands-only issuer = %+v", only)
	}
	if len(only.operatorOrgs) != 0 {
		t.Fatalf("no LANDS_OPERATOR_GITHUB_ORGS → empty denylist, got %v", only.operatorOrgs)
	}
	if only.maxRepoSizeKB != githubapp.MaxRepoSizeDefaultMB*1024 {
		t.Fatalf("no %s → the setup check's default limit, got %d KB", githubapp.MaxRepoSizeEnv, only.maxRepoSizeKB)
	}
	withOrgs := map[string]string{"LANDS_OPERATOR_GITHUB_ORGS": "MattBakerConsulting, claimlayer", githubapp.MaxRepoSizeEnv: "5"}
	for k, v := range landsEnv {
		withOrgs[k] = v
	}
	denied := newGithubAppIssuerFrom(&config.Config{}, fake.NewSimpleClientset(), log, nil, env(withOrgs), os.ReadFile)
	if denied == nil || len(denied.operatorOrgs) != 2 || !denied.operatorOrgs["mattbakerconsulting"] || !denied.operatorOrgs["claimlayer"] {
		t.Fatalf("operator orgs from env = %v", denied.operatorOrgs)
	}
	if denied.maxRepoSizeKB != 5*1024 {
		t.Fatalf("%s=5 → 5120 KB, got %d", githubapp.MaxRepoSizeEnv, denied.maxRepoSizeKB)
	}

	both := newGithubAppIssuerFrom(&config.Config{GithubAppEnabled: true, GithubAppID: "5029022", GithubAppPrivateKeyFile: key}, fake.NewSimpleClientset(), log, nil, env(landsEnv), os.ReadFile)
	if both == nil || both.fleetOff || both.app == nil || both.lands == nil {
		t.Fatalf("two-App issuer = %+v", both)
	}

	partial := newGithubAppIssuerFrom(&config.Config{}, fake.NewSimpleClientset(), log, nil, env(map[string]string{"LANDS_GITHUB_APP_ID": "6100100"}), os.ReadFile)
	if partial == nil || partial.lands != nil || partial.landsErr == nil {
		t.Fatalf("partly configured Lands App must fail closed, got %+v", partial)
	}

	fleetOnly := newGithubAppIssuerFrom(&config.Config{GithubAppEnabled: true, GithubAppID: "5029022", GithubAppPrivateKeyFile: key}, fake.NewSimpleClientset(), log, nil, env(nil), os.ReadFile)
	if fleetOnly == nil || fleetOnly.lands != nil || !errors.Is(fleetOnly.landsErr, githubapp.ErrNotConfigured) {
		t.Fatalf("fleet-only issuer must refuse Lands mints, got %+v", fleetOnly)
	}

	// The issuer reads the controller's live project cache.
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{"tomato-patch": ghTenant()}}
	iss := newGithubAppIssuerFrom(cfg, fake.NewSimpleClientset(), log, nil, env(landsEnv), os.ReadFile)
	if got, err := iss.resolveGithubApp("tomato-patch"); err != nil || got.app != githubapp.Lands || got.installationID != 9001 {
		t.Fatalf("resolve from cache = %+v, %v", got, err)
	}
}

// TestTokenScopePerApp drives the real githubapp clients against an httptest
// GitHub: a Lands token asks for exactly the Lands permission set and the one
// repo from the pinned installation; a fleet token keeps the fleet's set.
func TestTokenScopePerApp(t *testing.T) {
	type mintReq struct {
		path        string
		Repos       []string          `json:"repositories"`
		Permissions map[string]string `json:"permissions"`
	}
	var got []mintReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations/9001" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001, "account": map[string]any{"login": "Applicant"}, "repository_selection": "selected"})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/repos/applicant/tomatoes/installation" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req mintReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		req.path = r.URL.Path
		got = append(got, req)
		repos := make([]map[string]string, 0, len(req.Repos))
		for _, n := range req.Repos {
			repos = append(repos, map[string]string{"name": n})
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fmt.Sprintf("ghs_FAKE_%d", len(got)), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"repository_selection": "selected", "repositories": repos,
		})
	}))
	defer srv.Close()
	keyPEM, _ := os.ReadFile(testKeyFile(t))
	fleet, err := githubapp.New(githubapp.Config{Name: githubapp.Fleet, AppID: "5029022", PrivateKey: keyPEM, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	lands, err := githubapp.New(githubapp.Config{Name: githubapp.Lands, AppID: "6100100", PrivateKey: keyPEM, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := newGHTestEnv(t)
	e.iss.app, e.iss.lands, e.iss.installationID = fleet, lands, 42
	e.iss.project = ghProjects(map[string]config.ProjectCacheEntry{"gardenio": {}, "tomato-patch": ghTenant()})

	tspec := tenantSpec()
	if refuse, reason := e.iss.applyToSpec(context.Background(), &tspec, "gd-t1"); refuse {
		t.Fatal(reason)
	}
	ospec := ghSpec()
	ospec.ReferenceRepos = []podmanager.RepoRef{{URL: "https://github.com/joyousaversion/gascity-hq"}}
	if refuse, reason := e.iss.applyToSpec(context.Background(), &ospec, "gd-op"); refuse {
		t.Fatal(reason)
	}
	if len(got) != 2 {
		t.Fatalf("mint requests = %d", len(got))
	}
	if got[0].path != "/app/installations/9001/access_tokens" || strings.Join(got[0].Repos, ",") != "tomatoes" {
		t.Errorf("lands mint = %s %v", got[0].path, got[0].Repos)
	}
	if fmt.Sprint(got[0].Permissions) != fmt.Sprint(githubapp.LandsTokenPermissions()) {
		t.Errorf("lands permissions = %v, want %v", got[0].Permissions, githubapp.LandsTokenPermissions())
	}
	if got[1].path != "/app/installations/42/access_tokens" || strings.Join(got[1].Repos, ",") != "gardenio,gascity-hq" {
		t.Errorf("fleet mint = %s %v", got[1].path, got[1].Repos)
	}
	if fmt.Sprint(got[1].Permissions) != fmt.Sprint(githubapp.AgentTokenPermissions()) {
		t.Errorf("fleet permissions = %v, want %v", got[1].Permissions, githubapp.AgentTokenPermissions())
	}
	for _, p := range []string{"actions", "workflows", "packages", "administration"} {
		if _, ok := got[0].Permissions[p]; ok {
			t.Errorf("a Lands token must not ask for %s", p)
		}
	}
}
