package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
)

// --- gitlabProjectPath -------------------------------------------------------

func TestGitlabProjectPath(t *testing.T) {
	cases := []struct {
		url, host, want string
	}{
		{"https://gitlab.com/acme/platform/gardenio.git", "gitlab.com", "acme/platform/gardenio"},
		{"https://gitlab.com/group/repo", "gitlab.com", "group/repo"},
		{"https://gitlab.example.com:8443/g/r.git", "gitlab.example.com:8443", "g/r"},
		{"https://gitlab.com/onlygroup", "gitlab.com", ""},          // bare group is not a project
		{"https://github.com/o/r.git", "gitlab.com", ""},            // wrong host
		{"git@gitlab.com:g/r.git", "gitlab.com", ""},                // not https
		{"https://gitlab.com/g/r.git", "", ""},                      // no host to match
		{"  https://gitlab.com/g/r.git  ", "gitlab.com", "g/r"},     // trimmed
		{"https://gitlab.com/a/b/c/d.git", "gitlab.com", "a/b/c/d"}, // deep subgroups
	}
	for _, tc := range cases {
		if got := gitlabProjectPath(tc.url, tc.host); got != tc.want {
			t.Errorf("gitlabProjectPath(%q, %q) = %q, want %q", tc.url, tc.host, got, tc.want)
		}
	}
}

// --- minter ------------------------------------------------------------------

// newTestMinter points a minter at a httptest GitLab stub.
func newTestMinter(t *testing.T, handler http.Handler) *microvmGitMinter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m := newMicrovmGitMinter(&config.Config{
		MicrovmGitFactoryToken: "factory-secret",
		MicrovmGitLabBaseURL:   srv.URL,
	}, discardLogger())
	if m == nil {
		t.Fatal("minter should be constructed when the factory token is set")
	}
	return m
}

func TestMicrovmGitMinterDisabledWithoutFactory(t *testing.T) {
	if m := newMicrovmGitMinter(&config.Config{MicrovmGitLabBaseURL: "https://gitlab.com"}, discardLogger()); m != nil {
		t.Fatal("minter must be nil when MICROVM_GIT_FACTORY_TOKEN is unset")
	}
	if m := newMicrovmGitMinter(nil, discardLogger()); m != nil {
		t.Fatal("minter must be nil for nil config")
	}
}

func TestMicrovmGitMinterMint(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	m := newTestMinter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("PRIVATE-TOKEN")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 4242, "token": "glpat-minted-value"}`)
	}))

	minted, err := m.Mint(context.Background(), "acme/platform/gardenio", "gd-BEAD1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if minted.Token != "glpat-minted-value" || minted.TokenID != 4242 || minted.ProjectPath != "acme/platform/gardenio" {
		t.Errorf("minted = %+v, want token/id/path", minted)
	}
	if want := "/api/v4/projects/acme%2Fplatform%2Fgardenio/access_tokens"; gotPath != want {
		t.Errorf("POST path = %q, want %q", gotPath, want)
	}
	if gotAuth != "factory-secret" {
		t.Errorf("PRIVATE-TOKEN = %q, want the factory token", gotAuth)
	}
	if gotBody["name"] != "gardenio-microvm-gd-BEAD1" {
		t.Errorf("token name = %v, want gardenio-microvm-gd-BEAD1", gotBody["name"])
	}
	if gotBody["access_level"] != float64(microvmGitTokenAccessLevel) {
		t.Errorf("access_level = %v, want %d (Developer)", gotBody["access_level"], microvmGitTokenAccessLevel)
	}
	// api is included so an in-VM agent can finalize its own MR (set the
	// description/PROOF block, un-draft, merge — ka-0000007908). It stays narrow
	// on every other axis: project-scoped, Developer level, +2d TTL, eager
	// revoke. GitLab has no MR-only project-token scope, so api is the minimum.
	scopes, _ := gotBody["scopes"].([]any)
	if len(scopes) != 3 || scopes[0] != "read_repository" || scopes[1] != "write_repository" || scopes[2] != "api" {
		t.Errorf("scopes = %v, want [read_repository write_repository api]", gotBody["scopes"])
	}
	if exp, _ := gotBody["expires_at"].(string); len(exp) != len("2026-01-02") {
		t.Errorf("expires_at = %q, want a YYYY-MM-DD date backstop", exp)
	}
}

func TestMicrovmGitMinterMintHTTPError(t *testing.T) {
	m := newTestMinter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
	}))
	if _, err := m.Mint(context.Background(), "g/r", "gd-X"); err == nil {
		t.Fatal("Mint should surface a non-201 response as an error")
	}
}

func TestMicrovmGitMinterRevoke(t *testing.T) {
	var gotMethod, gotPath string
	status := http.StatusNoContent
	m := newTestMinter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		w.WriteHeader(status)
	}))

	if err := m.Revoke(context.Background(), "g/r", 4242); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v4/projects/g%2Fr/access_tokens/4242" {
		t.Errorf("revoke call = %s %s, want DELETE with encoded path + id", gotMethod, gotPath)
	}

	// Idempotency: an already-revoked token (404) is success — every exit path
	// may re-revoke freely.
	status = http.StatusNotFound
	if err := m.Revoke(context.Background(), "g/r", 4242); err != nil {
		t.Fatalf("Revoke of an absent token should be success, got %v", err)
	}

	status = http.StatusInternalServerError
	if err := m.Revoke(context.Background(), "g/r", 4242); err == nil {
		t.Fatal("Revoke should surface a 5xx as an error (expiry backstops, but callers log it)")
	}
}

// --- resolveMicrovmGitCredentials ---------------------------------------------

func TestResolveMicrovmGitCredentialsMintingMode(t *testing.T) {
	m := newTestMinter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 7, "token": "glpat-run-token"}`)
	}))
	host := m.host()
	spec := podmanager.AgentPodSpec{
		BeadID: "gd-M1", Project: "gardenio", Namespace: "gardenio",
		GitURL: "https://" + host + "/acme/platform/gardenio.git",
	}
	cred := resolveMicrovmGitCredentials(context.Background(), discardLogger(), m, fake.NewSimpleClientset(), spec)
	if cred.GitlabToken != "glpat-run-token" || cred.TokenID != 7 || cred.ProjectPath != "acme/platform/gardenio" {
		t.Errorf("cred = %+v, want the minted token + coordinates", cred)
	}
}

func TestResolveMicrovmGitCredentialsMintFailureDeliversNothing(t *testing.T) {
	// A mint outage must NOT silently widen to the shared credential.
	m := newTestMinter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	sharedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gardenio-gitlab-token", Namespace: "gardenio"},
		Data:       map[string][]byte{"token": []byte("shared-gitlab-token")},
	}
	spec := podmanager.AgentPodSpec{
		BeadID: "gd-M2", Project: "gardenio", Namespace: "gardenio",
		GitURL:            "https://" + m.host() + "/g/r.git",
		GitlabTokenSecret: "gardenio-gitlab-token",
	}
	cred := resolveMicrovmGitCredentials(context.Background(), discardLogger(), m, fake.NewSimpleClientset(sharedSecret), spec)
	if cred.GitlabToken != "" || cred.TokenID != 0 {
		t.Errorf("cred = %+v, want no GitLab token after a mint failure (no silent fallback)", cred)
	}
}

func TestResolveMicrovmGitCredentialsSharedFallback(t *testing.T) {
	gitlabSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gardenio-gitlab-token", Namespace: "gardenio"},
		Data:       map[string][]byte{"token": []byte("shared-gitlab-token")},
	}
	githubSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gardenio-git-credentials", Namespace: "gardenio"},
		Data:       map[string][]byte{"username": []byte("bot-user"), "token": []byte("ghp-shared")},
	}
	spec := podmanager.AgentPodSpec{
		BeadID: "gd-S1", Project: "gardenio", Namespace: "gardenio",
		GitURL:               "https://gitlab.com/g/r.git",
		GitlabTokenSecret:    "gardenio-gitlab-token",
		GitCredentialsSecret: "gardenio-git-credentials",
	}
	cred := resolveMicrovmGitCredentials(context.Background(), discardLogger(), nil, fake.NewSimpleClientset(gitlabSecret, githubSecret), spec)
	if cred.GitlabToken != "shared-gitlab-token" {
		t.Errorf("GitlabToken = %q, want the shared secret value", cred.GitlabToken)
	}
	if cred.GitUsername != "bot-user" || cred.GitToken != "ghp-shared" {
		t.Errorf("github creds = %q/%q, want bot-user/ghp-shared", cred.GitUsername, cred.GitToken)
	}
	if cred.TokenID != 0 {
		t.Errorf("TokenID = %d, want 0 (nothing minted in shared mode)", cred.TokenID)
	}
}

func TestResolveMicrovmGitCredentialsNoSecretsNoCreds(t *testing.T) {
	spec := podmanager.AgentPodSpec{BeadID: "gd-N1", Project: "gardenio", Namespace: "gardenio"}
	cred := resolveMicrovmGitCredentials(context.Background(), discardLogger(), nil, fake.NewSimpleClientset(), spec)
	if cred.GitlabToken != "" || cred.GitUsername != "" || cred.GitToken != "" || cred.TokenID != 0 {
		t.Errorf("cred = %+v, want zero credential set", cred)
	}
}

// --- applyMicrovmGitCredentials -----------------------------------------------

func TestApplyMicrovmGitCredentials(t *testing.T) {
	spec := &microvm.AgentSpec{}
	applyMicrovmGitCredentials(spec, microvmGitCredential{
		GitlabToken: "glpat-x", GitUsername: "u", GitToken: "t",
	})
	for k, want := range map[string]string{"GITLAB_TOKEN": "glpat-x", "GIT_USERNAME": "u", "GIT_TOKEN": "t"} {
		if got := spec.Environment[k]; got != want {
			t.Errorf("env[%q] = %q, want %q", k, got, want)
		}
	}

	// Empty credential set stamps nothing (absent must not look authoritative).
	empty := &microvm.AgentSpec{}
	applyMicrovmGitCredentials(empty, microvmGitCredential{})
	for _, k := range []string{"GITLAB_TOKEN", "GIT_USERNAME", "GIT_TOKEN"} {
		if _, present := empty.Environment[k]; present {
			t.Errorf("env[%q] present with no credential; want omitted", k)
		}
	}

	// A username without a token (or vice versa) is not a usable pair.
	half := &microvm.AgentSpec{}
	applyMicrovmGitCredentials(half, microvmGitCredential{GitUsername: "u"})
	if _, present := half.Environment["GIT_USERNAME"]; present {
		t.Error("GIT_USERNAME present without GIT_TOKEN; want the pair omitted")
	}
}

// --- revoker ------------------------------------------------------------------

// fakeBeadReader serves BeadDetail fixtures for revoker tests.
type fakeBeadReader struct {
	beads map[string]*beadsapi.BeadDetail
	err   error
}

func (f *fakeBeadReader) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.beads[beadID], nil
}

func TestMicrovmCredentialRevokerRevokesGitToken(t *testing.T) {
	var deletes []string
	m := newTestMinter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes = append(deletes, r.URL.EscapedPath())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := &microvmCredentialRevoker{
		logger: discardLogger(),
		cfg:    &config.Config{}, // PerAgentTokens off → beads revoke is a no-op
		git:    m,
		beads: &fakeBeadReader{beads: map[string]*beadsapi.BeadDetail{
			"gd-R1": {ID: "gd-R1", Fields: map[string]string{
				fieldMicrovmGitTokenID: "99",
				fieldMicrovmGitProject: "g/r",
			}},
		}},
	}
	r.RevokeAgentCredentials(context.Background(), "gd-R1", "terminate: AgentStop")
	if len(deletes) != 1 || deletes[0] != "/api/v4/projects/g%2Fr/access_tokens/99" {
		t.Fatalf("deletes = %v, want the persisted token coordinates", deletes)
	}
}

func TestMicrovmCredentialRevokerNoCoordinatesNoCall(t *testing.T) {
	var calls int
	m := newTestMinter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	r := &microvmCredentialRevoker{
		logger: discardLogger(),
		cfg:    &config.Config{},
		git:    m,
		beads: &fakeBeadReader{beads: map[string]*beadsapi.BeadDetail{
			"gd-R2": {ID: "gd-R2", Fields: map[string]string{}}, // nothing minted
		}},
	}
	r.RevokeAgentCredentials(context.Background(), "gd-R2", "leak_gc: owning bead gone")
	if calls != 0 {
		t.Fatalf("GitLab called %d times for a bead with no minted token; want 0", calls)
	}
}

func TestMicrovmCredentialRevokerNoMinterIsInert(t *testing.T) {
	// Shared-fallback deployments have no minter; revocation must be a quiet
	// no-op (the per-run document deletion is the whole revocation).
	r := &microvmCredentialRevoker{logger: discardLogger(), cfg: &config.Config{}, beads: &fakeBeadReader{}}
	r.RevokeAgentCredentials(context.Background(), "gd-R3", "terminate: AgentDone")
}

// TestMicrovmGitCredentialValueNeverLogged is the redaction guard for the
// launch + revoke paths this file owns (pairs with the CloudWatch redaction
// work in ka-0000002120): exercise the mint/resolve/persist-fail/revoke flows
// against a capturing logger and assert no credential VALUE ever reaches a log
// line. Coordinates (token id, project path) are allowed — they are not
// secrets.
func TestMicrovmGitCredentialValueNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	m := newTestMinter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 11, "token": "glpat-SUPER-SECRET-VALUE"}`)
	}))
	spec := podmanager.AgentPodSpec{
		BeadID: "gd-L1", Project: "gardenio", Namespace: "gardenio",
		GitURL: "https://" + m.host() + "/g/r.git",
	}
	// Full happy path: mint via resolve, then revoke via the revoker.
	cred := resolveMicrovmGitCredentials(context.Background(), logger, m, fake.NewSimpleClientset(), spec)
	if cred.GitlabToken != "glpat-SUPER-SECRET-VALUE" {
		t.Fatalf("resolve did not mint: %+v", cred)
	}
	r := &microvmCredentialRevoker{
		logger: logger,
		cfg:    &config.Config{},
		git:    m,
		beads: &fakeBeadReader{beads: map[string]*beadsapi.BeadDetail{
			"gd-L1": {ID: "gd-L1", Fields: map[string]string{
				fieldMicrovmGitTokenID: "11",
				fieldMicrovmGitProject: "g/r",
			}},
		}},
	}
	r.RevokeAgentCredentials(context.Background(), "gd-L1", "terminate: AgentDone")

	if logged := buf.String(); strings.Contains(logged, "glpat-SUPER-SECRET-VALUE") {
		t.Fatalf("git credential VALUE leaked into logs: %s", logged)
	}
	if !strings.Contains(buf.String(), "token_id=11") {
		t.Errorf("expected the non-secret token id in logs for auditability, got: %s", buf.String())
	}
}
