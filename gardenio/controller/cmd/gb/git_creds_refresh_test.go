package main

import (
	"context"
	"encoding/pem"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// gd-uPJ6AEsfyn: in-pod refresh of git credentials and gh hosts.yml for
// rotating (GitHub App installation) tokens. Every test uses obviously fake
// tokens and asserts they never leak into errors or logs.

const (
	tokOld = "ghs_OLDOLDOLDOLDOLDOLDOLDOLDOLDOLDOLD01"
	tokNew = "ghs_NEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEW02"
)

// projectGitHub writes the files the kubelet projects for the git Secret.
func projectGitHub(t *testing.T, dir, user, token string) {
	t.Helper()
	gh := filepath.Join(dir, "github")
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	if token != "" {
		if err := os.WriteFile(filepath.Join(gh, "token"), []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if user != "" {
		if err := os.WriteFile(filepath.Join(gh, "username"), []byte(user), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func refreshEnv(t *testing.T) (home, ghDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	// Isolate git from the ambient machine: the global config is a file in the
	// temp HOME (installProjectedGitHelper writes it), there is no system
	// config, and discovery cannot climb out of the temp tree into a checkout.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	ghDir = filepath.Join(home, ".config", "gh")
	t.Setenv("GH_CONFIG_DIR", ghDir)
	return home, ghDir
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestRefreshGitHubCreds_RotationReachesStoreAndHosts(t *testing.T) {
	home, ghDir := refreshEnv(t)
	creds := t.TempDir()
	projectGitHub(t, creds, "x-access-token", tokOld)

	// The agent container's store as writeGitCredentials leaves it: a stale
	// github line plus a GitLab line that must survive untouched.
	storePath := filepath.Join(home, ".git-credentials")
	store := "https://oauth2:gl-secret@gitlab.com\nhttps://x-access-token:" + tokOld + "@github.com\n"
	if err := os.WriteFile(storePath, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := refreshGitHubCreds(creds, home, ghDir)
	if err != nil || !changed {
		t.Fatalf("first refresh: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(readFile(t, filepath.Join(ghDir, "hosts.yml")), tokOld) {
		t.Fatal("hosts.yml must carry the current token")
	}

	// Rotate: the kubelet swaps the projected files.
	projectGitHub(t, creds, "x-access-token", tokNew)
	changed, err = refreshGitHubCreds(creds, home, ghDir)
	if err != nil || !changed {
		t.Fatalf("refresh after rotation: changed=%v err=%v", changed, err)
	}

	hosts := readFile(t, filepath.Join(ghDir, "hosts.yml"))
	if !strings.Contains(hosts, tokNew) || strings.Contains(hosts, tokOld) {
		t.Errorf("hosts.yml not rotated:\n%s", hosts)
	}
	got := readFile(t, storePath)
	if !strings.Contains(got, "x-access-token:"+tokNew+"@github.com") || strings.Contains(got, tokOld) {
		t.Errorf("store not rotated:\n%s", got)
	}
	if !strings.Contains(got, "oauth2:gl-secret@gitlab.com") {
		t.Errorf("GitLab line must be preserved:\n%s", got)
	}
	if strings.Count(got, "github.com") != 1 {
		t.Errorf("exactly one github.com line expected:\n%s", got)
	}
	for _, p := range []string{storePath, filepath.Join(ghDir, "hosts.yml")} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
	// No temp files left behind.
	for _, d := range []string{home, ghDir} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if strings.Contains(e.Name(), ".tmp-") {
				t.Errorf("leftover temp file %s/%s", d, e.Name())
			}
		}
	}
}

// writeHosts seeds $ghDir/hosts.yml.
func writeHosts(t *testing.T, ghDir, content string) string {
	t.Helper()
	if err := os.MkdirAll(ghDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(ghDir, "hosts.yml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A refresh must merge into the existing hosts.yml, not replace it (gd-AYftO5Gubz):
// other hosts and gh's own keys under github.com survive, the token rotates.
func TestWriteGhHosts_MergesIntoExistingFile(t *testing.T) {
	_, ghDir := refreshEnv(t)
	p := writeHosts(t, ghDir, `# managed by gh
ghe.example.com:
    user: octo
    oauth_token: ghe_SECRET
    git_protocol: ssh
github.com:
    users:
        alice:
            oauth_token: `+tokOld+`
    user: alice
    oauth_token: `+tokOld+`
    git_protocol: ssh
    editor: vim
`)
	changed, err := writeGhHosts(ghDir, githubCreds{User: "x-access-token", Token: tokNew})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var got map[string]map[string]any
	raw := readFile(t, p)
	if err := yaml.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("hosts.yml is not valid yaml: %v", err)
	}
	ghe, gh := got["ghe.example.com"], got["github.com"]
	if ghe["oauth_token"] != "ghe_SECRET" || ghe["git_protocol"] != "ssh" || ghe["user"] != "octo" {
		t.Errorf("other host clobbered: %v", ghe)
	}
	if gh["oauth_token"] != tokNew || gh["user"] != "x-access-token" || gh["git_protocol"] != "https" {
		t.Errorf("github.com not updated: %v", gh)
	}
	if gh["editor"] != "vim" || gh["users"] == nil {
		t.Errorf("unrelated github.com keys (editor, users) must survive: %v", gh)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("hosts.yml mode = %v, want 0600", fi.Mode().Perm())
	}
	// Same credential again: no rewrite.
	if changed, err := writeGhHosts(ghDir, githubCreds{User: "x-access-token", Token: tokNew}); err != nil || changed {
		t.Errorf("idempotent refresh: changed=%v err=%v", changed, err)
	}
}

// An absent, empty or unparsable hosts.yml is replaced with a fresh github.com
// entry (and the parse error, which could quote file content, is not surfaced).
func TestWriteGhHosts_AbsentOrCorruptFileIsReplaced(t *testing.T) {
	for name, seed := range map[string]string{
		"absent":  "",
		"empty":   "\n",
		"corrupt": "github.com: [unterminated\n  oauth_token: " + tokOld + "\n",
		"scalar":  "just a string\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, ghDir := refreshEnv(t)
			if name != "absent" {
				writeHosts(t, ghDir, seed)
			}
			if _, err := writeGhHosts(ghDir, githubCreds{User: "u", Token: tokNew}); err != nil {
				t.Fatalf("write: %v", err)
			}
			var got map[string]ghEntryForTest
			if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(ghDir, "hosts.yml"))), &got); err != nil {
				t.Fatal(err)
			}
			if e := got["github.com"]; e.OAuthToken != tokNew || e.User != "u" || e.GitProtocol != "https" {
				t.Errorf("github.com entry = %+v", e)
			}
		})
	}
}

type ghEntryForTest struct {
	User        string `yaml:"user"`
	OAuthToken  string `yaml:"oauth_token"`
	GitProtocol string `yaml:"git_protocol"`
}

func TestRefreshGitHubCreds_UnchangedTokenIsNoop(t *testing.T) {
	home, ghDir := refreshEnv(t)
	creds := t.TempDir()
	projectGitHub(t, creds, "", tokOld) // username defaults to x-access-token
	if err := os.WriteFile(filepath.Join(home, ".git-credentials"), []byte("https://x:y@gitlab.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := refreshGitHubCreds(creds, home, ghDir); err != nil || !changed {
		t.Fatalf("seed refresh: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(readFile(t, filepath.Join(ghDir, "hosts.yml")), "x-access-token") {
		t.Error("missing username file must default to x-access-token")
	}
	before, _ := os.Stat(filepath.Join(ghDir, "hosts.yml"))
	changed, err := refreshGitHubCreds(creds, home, ghDir)
	if err != nil || changed {
		t.Fatalf("second refresh must be a no-op: changed=%v err=%v", changed, err)
	}
	after, _ := os.Stat(filepath.Join(ghDir, "hosts.yml"))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("unchanged token must not rewrite hosts.yml")
	}
}

func TestRefreshGitHubCreds_NothingMounted(t *testing.T) {
	home, ghDir := refreshEnv(t)
	for _, creds := range []string{t.TempDir(), func() string { d := t.TempDir(); projectGitHub(t, d, "u", ""); return d }()} {
		changed, err := refreshGitHubCreds(creds, home, ghDir)
		if err != nil || changed {
			t.Fatalf("no token mounted must be a silent no-op: changed=%v err=%v", changed, err)
		}
	}
	if _, err := os.Stat(ghDir); !os.IsNotExist(err) {
		t.Errorf("gh config dir must not be created, stat err = %v", err)
	}
}

func TestRefreshGitHubCreds_DoesNotCreateAbsentStore(t *testing.T) {
	home, ghDir := refreshEnv(t)
	creds := t.TempDir()
	projectGitHub(t, creds, "x-access-token", tokOld)
	if _, err := refreshGitHubCreds(creds, home, ghDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".git-credentials")); !os.IsNotExist(err) {
		t.Errorf("no store helper reads it, so it must not be created (stat err = %v)", err)
	}
}

func TestRefreshGitHubCreds_ErrorsNeverContainTokens(t *testing.T) {
	home, _ := refreshEnv(t)
	creds := t.TempDir()
	projectGitHub(t, creds, "x-access-token", tokNew)

	// A store line that fails url.Parse (whose error would quote the line,
	// token and all) and a gh config dir that cannot be created.
	bad := "https://x-access-token:" + tokOld + "@[bad-host\n"
	if err := os.WriteFile(filepath.Join(home, ".git-credentials"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := refreshGitHubCreds(creds, home, filepath.Join(blocker, "gh"))
	if err == nil {
		t.Fatal("expected an error when the gh config dir cannot be created")
	}
	for _, tok := range []string{tokOld, tokNew} {
		if strings.Contains(err.Error(), tok) {
			t.Errorf("error leaks a token: %v", err)
		}
	}
	// The malformed foreign line is kept, not echoed.
	if !strings.Contains(readFile(t, filepath.Join(home, ".git-credentials")), "[bad-host") {
		t.Error("unparseable lines must be preserved verbatim")
	}
}

type syncLog struct {
	mu sync.Mutex
	sb strings.Builder
}

func (l *syncLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sb.WriteString(strings.TrimSpace(format))
	for range args {
		l.sb.WriteString(" ARG")
	}
}

func (l *syncLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.sb.String() }

func TestGitCredsRefreshLoop_PicksUpRotationWithoutLeaking(t *testing.T) {
	home, ghDir := refreshEnv(t)
	creds := t.TempDir()
	projectGitHub(t, creds, "x-access-token", tokOld)

	ctx, cancel := context.WithCancel(context.Background())
	var lg syncLog
	done := make(chan struct{})
	go func() { gitCredsRefreshLoop(ctx, creds, 10*time.Millisecond, lg.logf); close(done) }()

	waitForCond(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(ghDir, "hosts.yml"))
		return strings.Contains(string(b), tokOld)
	})
	projectGitHub(t, creds, "x-access-token", tokNew)
	waitForCond(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(ghDir, "hosts.yml"))
		return strings.Contains(string(b), tokNew)
	})
	cancel()
	<-done
	_ = home

	out := lg.String()
	if !strings.Contains(out, "refreshed") {
		t.Errorf("expected a refresh log line, got %q", out)
	}
	for _, tok := range []string{tokOld, tokNew} {
		if strings.Contains(out, tok) {
			t.Errorf("log leaks a token: %q", out)
		}
	}
}

func TestGitCredsRefreshLoop_FailureLoggedOncePerStreakWithoutToken(t *testing.T) {
	home, _ := refreshEnv(t)
	creds := t.TempDir()
	projectGitHub(t, creds, "x-access-token", tokNew)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", filepath.Join(blocker, "gh")) // MkdirAll always fails

	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	logf := func(format string, args ...any) {
		calls.Add(1)
		for _, a := range args {
			if s, ok := a.(error); ok && strings.Contains(s.Error(), tokNew) {
				t.Errorf("logged error leaks token: %v", s)
			}
		}
	}
	done := make(chan struct{})
	go func() { gitCredsRefreshLoop(ctx, creds, 5*time.Millisecond, logf); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	if n := calls.Load(); n != 1 {
		t.Errorf("failure streak must log exactly once, logged %d times", n)
	}
}

func waitForCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}

func TestGitCredentialProjectedResponse(t *testing.T) {
	creds := t.TempDir()
	attrs := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}

	if got := gitCredentialProjectedResponse(attrs("protocol", "https", "host", "github.com"), creds, "github.com"); got != "" {
		t.Errorf("nothing mounted must serve nothing, got %q", got)
	}
	projectGitHub(t, creds, "", tokNew)
	if got := gitCredentialProjectedResponse(attrs("protocol", "https", "host", "github.com"), creds, "github.com"); got !=
		"username=x-access-token\npassword="+tokNew+"\n" {
		t.Errorf("unexpected response %q", got)
	}
	if got := gitCredentialProjectedResponse(attrs("host", "github.com"), creds, "github.com"); got == "" {
		t.Error("missing protocol attr must still be served")
	}
	for name, a := range map[string]map[string]string{
		"other host":   attrs("protocol", "https", "host", "gitlab.com"),
		"lookalike":    attrs("protocol", "https", "host", "github.com.evil.test"),
		"plain http":   attrs("protocol", "http", "host", "github.com"),
		"no host":      attrs("protocol", "https"),
		"ssh":          attrs("protocol", "ssh", "host", "github.com"),
		"port in host": attrs("protocol", "https", "host", "github.com:8443"),
	} {
		if got := gitCredentialProjectedResponse(a, creds, "github.com"); got != "" {
			t.Errorf("%s: token must not be offered, got %q", name, got)
		}
	}
}

// fillCredential runs the real `git credential fill` and returns its stdout.
func fillCredential(t *testing.T, host string) string {
	t.Helper()
	fill := exec.Command("git", "credential", "fill")
	fill.Dir = os.Getenv("HOME") // never the checkout under test: its .git/config would apply
	fill.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
	fill.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
	out, err := fill.Output()
	if err != nil {
		t.Fatalf("git credential fill: %v", err)
	}
	return string(out)
}

func gitHelpers(t *testing.T) []string {
	t.Helper()
	cfg := exec.Command("git", "config", "--global", "--get-all", "credential.helper")
	cfg.Dir = os.Getenv("HOME")
	cfg.Env = os.Environ()
	out, err := cfg.Output()
	if err != nil {
		t.Fatalf("git config --get-all: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func TestInstallProjectedGitHelper_FirstInChainIdempotentAndTokenFree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	refreshEnv(t)
	creds := t.TempDir()

	// Nothing mounted: chain untouched.
	if err := installProjectedGitHelper(creds); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(os.Getenv("GIT_CONFIG_GLOBAL")); !os.IsNotExist(err) {
		t.Fatalf("no GitHub credential mounted: git config must not be touched (stat err = %v)", err)
	}

	projectGitHub(t, creds, "x-access-token", tokNew)
	// The pre-existing chain writeGitCredentials leaves behind: store, env.
	if err := writeGitCredentials("x-access-token", tokOld, "", []string{"github.com"}); err != nil {
		t.Fatalf("writeGitCredentials: %v", err)
	}
	for i := 0; i < 3; i++ { // pod restart / resume re-runs it
		if err := installProjectedGitHelper(creds); err != nil {
			t.Fatalf("install run %d: %v", i, err)
		}
	}
	h := gitHelpers(t)
	if len(h) != 3 {
		t.Fatalf("want [projected, store, env], got %d helpers: %v", len(h), h)
	}
	if !strings.Contains(h[0], projectedHelperName) {
		t.Errorf("projected helper must be first: %v", h)
	}
	if !strings.HasPrefix(h[1], "store --file=") || !strings.Contains(h[2], "git-credential-env") {
		t.Errorf("existing chain must be preserved in order after it: %v", h)
	}
	if cfg := readFile(t, os.Getenv("GIT_CONFIG_GLOBAL")); strings.Contains(cfg, tokNew) || strings.Contains(cfg, tokOld) {
		t.Errorf("a token reached the git config:\n%s", cfg)
	}
}

// TestProjectedHelperServesRotatedTokenToGit drives the REAL git binary: the
// store holds a stale token, the projected Secret rotates, and git must offer
// the new one with no refresh in between.
func TestProjectedHelperServesRotatedTokenToGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home, _ := refreshEnv(t)
	creds := t.TempDir()
	t.Setenv("GB_TEST_PROJECTED_DIR", creds) // read by TestMain's helper stand-in
	t.Setenv("GB_TEST_PROJECTED_HOST", "github.com")
	projectGitHub(t, creds, "x-access-token", tokOld)
	if err := writeGitCredentials("x-access-token", tokOld, "", []string{"github.com"}); err != nil {
		t.Fatal(err)
	}
	if err := installProjectedGitHelper(creds); err != nil {
		t.Fatal(err)
	}

	if out := fillCredential(t, "github.com"); !strings.Contains(out, "password="+tokOld+"\n") {
		t.Fatalf("pre-rotation fill: %q", out)
	}

	projectGitHub(t, creds, "x-access-token", tokNew) // rotation, store untouched
	if !strings.Contains(readFile(t, filepath.Join(home, ".git-credentials")), tokOld) {
		t.Fatal("precondition: store must still be stale")
	}
	out := fillCredential(t, "github.com")
	if !strings.Contains(out, "password="+tokNew+"\n") || strings.Contains(out, tokOld) {
		t.Fatalf("git must see the rotated token, got %q", out)
	}
}

// The credential tests must not inherit the config of the checkout they run in
// (gd-AYftO5Gubz): a repo-local `credential.helper =` resets the helper chain,
// which used to starve the projected helper when `go test` ran in such a
// worktree.
func TestProjectedHelperIgnoresAmbientRepoConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home, _ := refreshEnv(t)
	repo := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "--local", "credential.helper", ""}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES=") // allow discovery in repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(repo) // the test process now sits inside the poisoned checkout
	creds := t.TempDir()
	t.Setenv("GB_TEST_PROJECTED_DIR", creds)
	t.Setenv("GB_TEST_PROJECTED_HOST", "github.com")
	projectGitHub(t, creds, "x-access-token", tokOld)
	if err := installProjectedGitHelper(creds); err != nil {
		t.Fatal(err)
	}
	if out := fillCredential(t, "github.com"); !strings.Contains(out, "password="+tokOld+"\n") {
		t.Fatalf("ambient repo config leaked into the test: %q (home %s)", out, home)
	}
}

// TestRotatedTokenIsUsedByNextRemoteOperation is the acceptance test: a token
// rotated mid-session is the one git sends on the next remote operation. A fake
// smart-HTTP remote enforces Basic auth against the *currently valid* token;
// git is the real binary running the real helper chain. ls-remote shares the
// credential fill/approve/reject path with push (the receive-pack leg of a
// push differs only in the service name).
func TestRotatedTokenIsUsedByNextRemoteOperation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	_, _ = refreshEnv(t)

	var valid atomic.Value
	valid.Store(tokOld)
	var sawStale atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || pass != valid.Load().(string) {
			if ok && pass == tokOld {
				sawStale.Store(true)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		// Empty-repo smart-HTTP advertisement.
		_, _ = w.Write([]byte(pktLine("# service=git-upload-pack\n") + "0000" +
			pktLine("0000000000000000000000000000000000000000 capabilities^{}\x00agent=fake\n") + "0000"))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	creds := t.TempDir()
	t.Setenv("GB_TEST_PROJECTED_DIR", creds)
	t.Setenv("GB_TEST_PROJECTED_HOST", host)
	projectGitHub(t, creds, "x-access-token", tokOld)
	// Stale store line for the same host: what the pre-fix agent was stuck with.
	if err := writeGitCredentials("x-access-token", tokOld, "", []string{host}); err != nil {
		t.Fatal(err)
	}
	// installProjectedGitHelper keys on the mounted credential, not the host.
	if err := installProjectedGitHelper(creds); err != nil {
		t.Fatal(err)
	}

	ls := func() (string, error) {
		c := exec.Command("git", "ls-remote", srv.URL+"/r.git")
		c.Dir = os.Getenv("HOME")
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GIT_SSL_CAINFO="+caPath)
		out, err := c.CombinedOutput()
		return string(out), err
	}

	if out, err := ls(); err != nil {
		t.Fatalf("pre-rotation ls-remote failed: %v\n%s", err, out)
	}

	// Rotation: the remote now accepts only tokNew; the kubelet re-projects it.
	valid.Store(tokNew)
	projectGitHub(t, creds, "x-access-token", tokNew)
	sawStale.Store(false)
	out, err := ls()
	if err != nil {
		t.Fatalf("post-rotation ls-remote failed: %v\n%s", err, out)
	}
	if sawStale.Load() {
		t.Error("git offered the stale token after rotation")
	}
	for _, tok := range []string{tokOld, tokNew} {
		if strings.Contains(out, tok) {
			t.Errorf("git output leaks a token: %q", out)
		}
	}

	// Sanity: the fake really enforces auth — a remote that has rotated again
	// while the projected files lag must reject.
	valid.Store("ghs_SOMETHING_ELSE")
	if _, err := ls(); err == nil {
		t.Error("remote must reject a token that is not the currently valid one")
	}
}

// pktLine frames payload as a git pkt-line.
func pktLine(payload string) string { return fmt.Sprintf("%04x%s", len(payload)+4, payload) }

// TestPodSupervisorsStartGitCredsRefresh: both pod supervisors start the
// refresh loop. Agent pods run `gb init` as PID 1, and only `gb agent start`
// started it, so hosts.yml went stale and gh got 401 after ~60 min
// (gd-jfo7R99vgj). runInitLoop launches coop, mux and mail and cannot run in a
// unit test, so this reads the call from the source instead.
func TestPodSupervisorsStartGitCredsRefresh(t *testing.T) {
	for file, fn := range map[string]string{
		"init_agent_loop.go": "runInitLoop",
		"agent_start_k8s.go": "runAgentStartK8s",
	} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found, calls := false, false
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != fn {
				continue
			}
			found = true
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "startGitCredsRefresh" {
						calls = true
					}
				}
				return true
			})
		}
		if !found {
			t.Errorf("%s: func %s not found", file, fn)
		} else if !calls {
			t.Errorf("%s must call startGitCredsRefresh, or gh's hosts.yml goes stale once the token rotates", fn)
		}
	}
}
