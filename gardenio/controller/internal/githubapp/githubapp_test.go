package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeInstallToken = "ghs_FAKEinstallationTOKENvalue0123456789" // gitleaks:allow: fake credential

var testNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func genKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// fakeGitHub is a minimal GitHub App API. It records each request and verifies
// the App JWT signature on the endpoints that require one.
type fakeGitHub struct {
	t   *testing.T
	pub *rsa.PublicKey
	srv *httptest.Server

	mu         sync.Mutex
	mintBodies []map[string]any
	revoked    []string // bearer tokens presented to DELETE /installation/token
	jwts       []string

	selection  string   // repository_selection in the mint response
	repos      []string // repositories echoed in the mint response (nil = echo request)
	mintCode   int
	revokeCode int
	// repoFullName overrides full_name in GET /repos answers.
	repoFullName string
}

func newFakeGitHub(t *testing.T, pub *rsa.PublicKey) *fakeGitHub {
	f := &fakeGitHub{t: t, pub: pub, selection: "selected", mintCode: http.StatusCreated, revokeCode: http.StatusNoContent}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) verifyJWT(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig) == nil
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		if !f.verifyJWT(bearer) {
			http.Error(w, `{"message":"bad jwt"}`, http.StatusUnauthorized)
			return
		}
		f.jwts = append(f.jwts, bearer)
		_, _ = w.Write([]byte(`{"id":170295668}`))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		if !f.verifyJWT(bearer) {
			http.Error(w, `{"message":"bad jwt"}`, http.StatusUnauthorized)
			return
		}
		f.jwts = append(f.jwts, bearer)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mintBodies = append(f.mintBodies, body)
		if f.mintCode != http.StatusCreated {
			w.WriteHeader(f.mintCode)
			_, _ = w.Write([]byte(`{"message":"Validation Failed"}`))
			return
		}
		repos := f.repos
		if repos == nil {
			for _, n := range body["repositories"].([]any) {
				repos = append(repos, n.(string))
			}
		}
		var rs []map[string]string
		for _, n := range repos {
			rs = append(rs, map[string]string{"name": n})
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":                fakeInstallToken,
			"expires_at":           testNow.Add(time.Hour).Format(time.RFC3339),
			"repository_selection": f.selection,
			"repositories":         rs,
		})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") && strings.Contains(r.URL.Path, "/pulls/"):
		if bearer != fakeInstallToken {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		// #1 open, #2 closed, #3 merged, #5 answers for another PR; anything
		// else does not exist.
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "1":
			_, _ = w.Write([]byte(`{"number":1,"state":"open","merged":false}`))
		case "2":
			_, _ = w.Write([]byte(`{"number":2,"state":"closed","merged":false}`))
		case "3":
			_, _ = w.Write([]byte(`{"number":3,"state":"closed","merged":true}`))
		case "5":
			_, _ = w.Write([]byte(`{"number":6,"state":"open","merged":false}`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/"):
		if bearer != fakeInstallToken {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		full := strings.TrimPrefix(r.URL.Path, "/repos/")
		if f.repoFullName != "" {
			full = f.repoFullName
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": full, "default_branch": "trunk", "size": 2048})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/"):
		if bearer != fakeInstallToken {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch login := strings.TrimPrefix(r.URL.Path, "/users/"); strings.ToLower(login) {
		case "octocat":
			_, _ = w.Write([]byte(`{"id":583231,"login":"Octocat","type":"User"}`))
		case "renamed":
			_, _ = w.Write([]byte(`{"id":1,"login":"someone-else","type":"User"}`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
		f.revoked = append(f.revoked, bearer)
		w.WriteHeader(f.revokeCode)
	default:
		http.NotFound(w, r)
	}
}

func newTestClient(t *testing.T) (*Client, *fakeGitHub, []byte) {
	t.Helper()
	k, pemBytes := genKey(t)
	f := newFakeGitHub(t, &k.PublicKey)
	c, err := New(Config{AppID: "5029022", PrivateKey: pemBytes, BaseURL: f.srv.URL, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return c, f, pemBytes
}

func TestJWTClaimsAndSignature(t *testing.T) {
	c, f, _ := newTestClient(t)
	jwt, err := c.JWT()
	if err != nil {
		t.Fatal(err)
	}
	if !f.verifyJWT(jwt) {
		t.Fatal("JWT signature does not verify against the App public key")
	}
	parts := strings.Split(jwt, ".")
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if string(hdr) != `{"alg":"RS256","typ":"JWT"}` {
		t.Errorf("header = %s", hdr)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var cl struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &cl); err != nil {
		t.Fatal(err)
	}
	if cl.Iss != "5029022" {
		t.Errorf("iss = %q", cl.Iss)
	}
	if cl.Exp-testNow.Unix() > 600 || cl.Exp <= testNow.Unix() {
		t.Errorf("exp must be in (now, now+10m], got now%+ds", cl.Exp-testNow.Unix())
	}
	if cl.Iat >= testNow.Unix() {
		t.Errorf("iat must be backdated, got now%+ds", cl.Iat-testNow.Unix())
	}
}

func TestMintScopesToRepositoriesAndUsesJWT(t *testing.T) {
	c, f, _ := newTestClient(t)
	tok, err := c.MintInstallationToken(context.Background(), 170295668, []string{"gardenio"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Reveal() != fakeInstallToken {
		t.Fatal("token value not returned")
	}
	if !tok.ExpiresAt.Equal(testNow.Add(time.Hour)) {
		t.Errorf("expires_at = %v", tok.ExpiresAt)
	}
	if len(f.mintBodies) != 1 {
		t.Fatalf("mint calls = %d", len(f.mintBodies))
	}
	repos, _ := f.mintBodies[0]["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "gardenio" {
		t.Errorf("repositories = %v", f.mintBodies[0])
	}
}

func TestMintRequestsMinimalPermissions(t *testing.T) {
	c, f, _ := newTestClient(t)
	if _, err := c.MintInstallationToken(context.Background(), 1, []string{"gardenio"}); err != nil {
		t.Fatal(err)
	}
	perms, ok := f.mintBodies[0]["permissions"].(map[string]any)
	if !ok || len(perms) == 0 {
		t.Fatalf("mint body must carry an explicit permissions map (else the token inherits every App permission): %v", f.mintBodies[0])
	}
	want := map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write",
		"actions": "write", "workflows": "write", "checks": "read", "statuses": "read",
		"packages": "read",
	}
	if len(perms) != len(want) {
		t.Errorf("permissions = %v, want exactly %v", perms, want)
	}
	for k, v := range want {
		if perms[k] != v {
			t.Errorf("permission %s = %v, want %s", k, perms[k], v)
		}
	}
	for _, forbidden := range []string{"administration", "secrets", "members", "organization_administration", "environments"} {
		if _, has := perms[forbidden]; has {
			t.Errorf("permission %s must never be requested for an agent token", forbidden)
		}
	}
}

func TestMintRefusesUnscoped(t *testing.T) {
	c, f, _ := newTestClient(t)
	if _, err := c.MintInstallationToken(context.Background(), 1, nil); err == nil {
		t.Fatal("expected refusal for empty repositories")
	}
	if len(f.mintBodies) != 0 {
		t.Fatal("must not call GitHub for an unscoped mint")
	}
}

func TestMintRevokesTokenWiderThanRequested(t *testing.T) {
	for name, mutate := range map[string]func(*fakeGitHub){
		"all-repos selection": func(f *fakeGitHub) { f.selection = "all" },
		"extra repo":          func(f *fakeGitHub) { f.repos = []string{"gardenio", "other"} },
		"different repo":      func(f *fakeGitHub) { f.repos = []string{"other"} },
	} {
		t.Run(name, func(t *testing.T) {
			c, f, _ := newTestClient(t)
			mutate(f)
			tok, err := c.MintInstallationToken(context.Background(), 1, []string{"gardenio"})
			if err == nil {
				t.Fatal("expected error")
			}
			if tok.Reveal() != "" {
				t.Fatal("must not return the wide token")
			}
			if len(f.revoked) != 1 || f.revoked[0] != fakeInstallToken {
				t.Fatalf("wide token must be revoked, revoked=%v", f.revoked)
			}
			if strings.Contains(err.Error(), fakeInstallToken) {
				t.Fatal("error leaks token")
			}
		})
	}
}

func TestRevokeUsesTokenAsBearerAndIsIdempotent(t *testing.T) {
	c, f, _ := newTestClient(t)
	tok := NewToken(fakeInstallToken, testNow)
	if err := c.Revoke(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if len(f.revoked) != 1 || f.revoked[0] != fakeInstallToken {
		t.Fatalf("revoked = %v", f.revoked)
	}
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		f.revokeCode = code
		if err := c.Revoke(context.Background(), tok); err != nil {
			t.Errorf("%d must count as already revoked: %v", code, err)
		}
	}
	f.revokeCode = http.StatusInternalServerError
	if err := c.Revoke(context.Background(), tok); err == nil {
		t.Error("5xx must surface")
	}
	if err := c.Revoke(context.Background(), Token{}); err != nil {
		t.Error("empty token is a no-op")
	}
}

func TestInstallationForCaches(t *testing.T) {
	c, f, _ := newTestClient(t)
	for i := 0; i < 2; i++ {
		id, err := c.InstallationFor(context.Background(), RepoRef{"joyousaversion", "gardenio"})
		if err != nil || id != 170295668 {
			t.Fatalf("id=%d err=%v", id, err)
		}
	}
	if len(f.jwts) != 1 {
		t.Errorf("lookup should be cached, JWT-authenticated calls = %d", len(f.jwts))
	}
}

// RepoInstallation asks GitHub every time: it backs the Lands check that a
// pinned installation still includes the repo.
func TestRepoInstallationIsNotCached(t *testing.T) {
	c, f, _ := newTestClient(t)
	for i := 0; i < 2; i++ {
		id, err := c.RepoInstallation(context.Background(), RepoRef{"joyousaversion", "gardenio"})
		if err != nil || id != 170295668 {
			t.Fatalf("id=%d err=%v", id, err)
		}
	}
	if len(f.jwts) != 2 {
		t.Errorf("RepoInstallation must not cache, JWT-authenticated calls = %d", len(f.jwts))
	}
}

func TestParseRepoURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want RepoRef
		ok   bool
	}{
		{"https://github.com/joyousaversion/gardenio", RepoRef{"joyousaversion", "gardenio"}, true},
		{"https://github.com/joyousaversion/gardenio.git", RepoRef{"joyousaversion", "gardenio"}, true},
		{"https://GitHub.com/o/r/", RepoRef{"o", "r"}, true},
		{"https://gitlab.com/o/r", RepoRef{}, false},
		{"git@github.com:o/r.git", RepoRef{}, false},
		{"http://github.com/o/r", RepoRef{}, false},
		{"https://github.com/o", RepoRef{}, false},
		{"https://github.com/o/r/extra", RepoRef{}, false},
		{"", RepoRef{}, false},
	} {
		got, ok := ParseRepoURL(tc.in, "github.com")
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseRepoURL(%q) = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestNoSecretInFormattingOrErrors is the credential-hygiene contract: the
// private key, App JWT and installation token must not surface through any
// formatter, slog handler, JSON encoder or error string.
func TestNoSecretInFormattingOrErrors(t *testing.T) {
	c, f, pemBytes := newTestClient(t)
	tok, err := c.MintInstallationToken(context.Background(), 1, []string{"gardenio"})
	if err != nil {
		t.Fatal(err)
	}
	jwt, _ := c.JWT()
	secrets := []string{fakeInstallToken, jwt, string(pemBytes), base64.StdEncoding.EncodeToString(pemBytes)}

	var logBuf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&logBuf, nil))
	lg.Info("minted", "token", tok, "tok_ptr", &tok)
	jsonOut, _ := json.Marshal(map[string]any{"t": tok, "p": &tok})
	outputs := []string{
		fmt.Sprintf("%v|%+v|%#v|%s|%q", tok, tok, tok, tok, tok),
		fmt.Sprint(&tok), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c),
		logBuf.String(), string(jsonOut),
	}

	// Error paths: GitHub 4xx/5xx, transport failure, bad key.
	f.mintCode = http.StatusUnprocessableEntity
	_, e1 := c.MintInstallationToken(context.Background(), 1, []string{"gardenio"})
	f.mintCode = http.StatusInternalServerError
	_, e2 := c.MintInstallationToken(context.Background(), 1, []string{"gardenio"})
	f.revokeCode = http.StatusInternalServerError
	e3 := c.Revoke(context.Background(), tok)
	f.srv.Close()
	_, e4 := c.MintInstallationToken(context.Background(), 1, []string{"gardenio"})
	e5 := c.Revoke(context.Background(), tok)
	_, e6 := New(Config{AppID: "1", PrivateKey: pemBytes[:len(pemBytes)/2]})
	_, e7 := New(Config{AppID: "1", PrivateKey: []byte("-----BEGIN RSA PRIVATE KEY-----\nQUJDREVGRw==\n-----END RSA PRIVATE KEY-----")}) // gitleaks:allow: fake credential
	for i, e := range []error{e1, e2, e3, e4, e5, e6, e7} {
		if e == nil {
			t.Fatalf("error %d: expected failure", i+1)
		}
		outputs = append(outputs, e.Error())
	}

	for _, out := range outputs {
		for _, s := range secrets {
			if s != "" && strings.Contains(out, s) {
				t.Errorf("secret material leaked in %q", truncate(out))
			}
		}
		// The key body itself (any 40-char run of the PEM payload).
		if strings.Contains(out, "MIIE") {
			t.Errorf("PEM payload leaked in %q", truncate(out))
		}
	}
	if !strings.Contains(fmt.Sprint(tok), Redacted) {
		t.Error("Token must print a redaction marker")
	}
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

func TestNewValidation(t *testing.T) {
	_, pemBytes := genKey(t)
	for name, cfg := range map[string]Config{
		"empty app id": {PrivateKey: pemBytes},
		"alpha app id": {AppID: "abc", PrivateKey: pemBytes},
		"no key":       {AppID: "1"},
		"not pem":      {AppID: "1", PrivateKey: []byte("hello")},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// PKCS#8 is accepted too.
	k, _ := genKey(t)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if _, err := New(Config{AppID: "1", PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}); err != nil {
		t.Errorf("pkcs8: %v", err)
	}
}

func TestMintWithPermissionsPinsCallerSet(t *testing.T) {
	c, f, _ := newTestClient(t)
	if _, err := c.MintInstallationTokenWithPermissions(context.Background(), 1, []string{"gardenio"}, BridgeTokenPermissions()); err != nil {
		t.Fatal(err)
	}
	perms, _ := f.mintBodies[0]["permissions"].(map[string]any)
	if perms["contents"] != "read" || perms["pull_requests"] != "write" {
		t.Errorf("permissions = %v", perms)
	}
	for _, forbidden := range []string{"workflows", "packages", "administration"} {
		if _, has := perms[forbidden]; has {
			t.Errorf("bridge token must not request %s", forbidden)
		}
	}
	if perms["contents"] == "write" {
		t.Error("bridge token must never be able to push code")
	}
}

func TestReleaseDeployerTokenRequestsExactlyReadOnly(t *testing.T) {
	c, f, _ := newTestClient(t)
	if _, err := c.MintInstallationTokenWithPermissions(context.Background(), 1, []string{"gardenio"}, ReleaseDeployerTokenPermissions()); err != nil {
		t.Fatal(err)
	}
	perms, _ := f.mintBodies[0]["permissions"].(map[string]any)
	want := map[string]any{"contents": "read", "actions": "read", "metadata": "read"}
	if len(perms) != len(want) {
		t.Fatalf("permissions = %v, want exactly %v", perms, want)
	}
	for k, v := range want {
		if perms[k] != v {
			t.Errorf("permission %s = %v, want %v", k, perms[k], v)
		}
	}
	ReleaseDeployerTokenPermissions()["contents"] = "write"
	if ReleaseDeployerTokenPermissions()["contents"] != "read" {
		t.Error("a caller mutated the shared release-deployer permission set")
	}
}

func TestMintWithPermissionsRefusesEmptySet(t *testing.T) {
	c, f, _ := newTestClient(t)
	if _, err := c.MintInstallationTokenWithPermissions(context.Background(), 1, []string{"gardenio"}, nil); err == nil {
		t.Fatal("empty permission set must be refused (would inherit every App permission)")
	}
	if len(f.mintBodies) != 0 {
		t.Error("no request may be sent for a refused mint")
	}
}

func TestDefaultBranchUsesAMetadataTokenAndRevokesIt(t *testing.T) {
	c, f, _ := newTestClient(t)
	branch, err := c.DefaultBranch(context.Background(), 170295668, RepoRef{Owner: "octo", Name: "acme-site"})
	if err != nil || branch != "trunk" {
		t.Fatalf("DefaultBranch = %q, %v", branch, err)
	}
	if len(f.mintBodies) != 1 {
		t.Fatalf("mint calls = %d", len(f.mintBodies))
	}
	perms, _ := f.mintBodies[0]["permissions"].(map[string]any)
	if len(perms) != 1 || perms["metadata"] != "read" {
		t.Errorf("permissions = %v, want metadata:read only", perms)
	}
	if len(f.revoked) != 1 || f.revoked[0] != fakeInstallToken {
		t.Errorf("revoked = %v", f.revoked)
	}

	f.repoFullName = "someone/else"
	if _, err := c.DefaultBranch(context.Background(), 170295668, RepoRef{Owner: "octo", Name: "acme-site"}); err == nil {
		t.Fatal("an answer for another repository must be refused")
	}
	if _, err := c.DefaultBranch(context.Background(), 170295668, RepoRef{Name: "x"}); err == nil {
		t.Fatal("a repository without an owner must be refused")
	}
}

// RepoSizeKB reads GitHub's size the way DefaultBranch reads the branch: a
// metadata-only token, revoked once read, and only for the repository asked.
func TestRepoSizeKBUsesAMetadataTokenAndRevokesIt(t *testing.T) {
	c, f, _ := newTestClient(t)
	size, err := c.RepoSizeKB(context.Background(), 170295668, RepoRef{Owner: "octo", Name: "acme-site"})
	if err != nil || size != 2048 {
		t.Fatalf("RepoSizeKB = %d, %v", size, err)
	}
	perms, _ := f.mintBodies[0]["permissions"].(map[string]any)
	if len(perms) != 1 || perms["metadata"] != "read" {
		t.Errorf("permissions = %v, want metadata:read only", perms)
	}
	if len(f.revoked) != 1 {
		t.Errorf("revoked = %v", f.revoked)
	}
	f.repoFullName = "someone/else"
	if _, err := c.RepoSizeKB(context.Background(), 170295668, RepoRef{Owner: "octo", Name: "acme-site"}); err == nil {
		t.Fatal("an answer for another repository must be refused")
	}
}

func TestMaxRepoSizeMB(t *testing.T) {
	for v, want := range map[string]int64{"": MaxRepoSizeDefaultMB, "0": 0, "50": 50, " 7 ": 7, "-1": MaxRepoSizeDefaultMB, "big": MaxRepoSizeDefaultMB} {
		if got := MaxRepoSizeMB(func(string) string { return v }); got != want {
			t.Errorf("%s=%q: %d, want %d", MaxRepoSizeEnv, v, got, want)
		}
	}
}

func TestPullRequestStateUsesAPullRequestsReadTokenAndRevokesIt(t *testing.T) {
	c, f, _ := newTestClient(t)
	repo := RepoRef{Owner: "octo", Name: "acme-site"}
	for n, want := range map[int]string{1: "open", 2: "closed", 3: "merged"} {
		got, err := c.PullRequestState(context.Background(), 170295668, repo, n)
		if err != nil || got != want {
			t.Errorf("PullRequestState(#%d) = %q, %v; want %q", n, got, err, want)
		}
	}
	perms, _ := f.mintBodies[0]["permissions"].(map[string]any)
	if len(perms) != 1 || perms["pull_requests"] != "read" {
		t.Errorf("permissions = %v, want pull_requests:read only", perms)
	}
	if len(f.revoked) != 3 {
		t.Errorf("revoked = %d tokens, want one per read", len(f.revoked))
	}
	if _, err := c.PullRequestState(context.Background(), 170295668, repo, 4); err == nil {
		t.Error("a missing pull request must be an error, not a state")
	}
	if _, err := c.PullRequestState(context.Background(), 170295668, repo, 5); err == nil {
		t.Error("an answer for another pull request must be refused")
	}
	if _, err := c.PullRequestState(context.Background(), 170295668, RepoRef{Name: "x"}, 1); err == nil {
		t.Error("a repository without an owner must be refused")
	}
	if _, err := c.PullRequestState(context.Background(), 170295668, repo, 0); err == nil {
		t.Error("a zero pull request number must be refused")
	}
}

func TestParsePullRequestURL(t *testing.T) {
	repo, n, ok := ParsePullRequestURL("https://github.com/mattbakerconsulting/gardenio/pull/504", "github.com")
	if !ok || repo != (RepoRef{Owner: "mattbakerconsulting", Name: "gardenio"}) || n != 504 {
		t.Errorf("parse = %+v, %d, %v", repo, n, ok)
	}
	for _, bad := range []string{
		"http://github.com/o/r/pull/1",
		"https://gitlab.com/o/r/pull/1",
		"https://github.com/o/r/issues/1",
		"https://github.com/o/r/pull/x",
		"https://github.com/o/r/pull/0",
		"https://github.com/o/r/pull/1/files",
		"https://github.com/o/r",
	} {
		if _, _, ok := ParsePullRequestURL(bad, "github.com"); ok {
			t.Errorf("ParsePullRequestURL(%q) accepted", bad)
		}
	}
}

func TestUserByLoginResolvesTheIDWithAMetadataToken(t *testing.T) {
	c, f, _ := newTestClient(t)
	repo := RepoRef{Owner: "octo", Name: "acme-site"}
	u, err := c.UserByLogin(context.Background(), 170295668, repo, "octocat")
	if err != nil || u.ID != 583231 || u.Login != "Octocat" || u.Type != "User" {
		t.Fatalf("UserByLogin = %+v, %v", u, err)
	}
	perms, _ := f.mintBodies[0]["permissions"].(map[string]any)
	if len(perms) != 1 || perms["metadata"] != "read" {
		t.Errorf("permissions = %v, want metadata:read only", perms)
	}
	if len(f.revoked) != 1 {
		t.Errorf("revoked = %v", f.revoked)
	}
	if _, err := c.UserByLogin(context.Background(), 170295668, repo, "nobody-here"); !errors.Is(err, ErrNoGitHubUser) {
		t.Errorf("unknown login: %v", err)
	}
	if _, err := c.UserByLogin(context.Background(), 170295668, repo, "renamed"); err == nil || errors.Is(err, ErrNoGitHubUser) {
		t.Errorf("an answer for another login must be refused: %v", err)
	}
	if _, err := c.UserByLogin(context.Background(), 170295668, repo, "../repos/x"); !errors.Is(err, ErrNoGitHubUser) {
		t.Errorf("a login with a path in it: %v", err)
	}
}

func TestConflictResolverTokenRequestsOnlyThePush(t *testing.T) {
	c, f, _ := newTestClient(t)
	if _, err := c.MintInstallationTokenWithPermissions(context.Background(), 1, []string{"gardenio"}, ConflictResolverTokenPermissions()); err != nil {
		t.Fatal(err)
	}
	perms, _ := f.mintBodies[0]["permissions"].(map[string]any)
	want := map[string]any{"contents": "write", "workflows": "write"}
	if len(perms) != len(want) {
		t.Fatalf("permissions = %v, want exactly %v", perms, want)
	}
	for k, v := range want {
		if perms[k] != v {
			t.Errorf("permission %s = %v, want %v", k, perms[k], v)
		}
	}
}
