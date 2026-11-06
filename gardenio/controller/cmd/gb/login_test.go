package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
)

// The device grant, session cache and refresh are oidclogin's, tested through
// kd (kbeads/cmd/kd/login_test.go). These tests cover gb's surface over it:
// the session is bound to gb's --http-url and shared with kd.

const (
	testGBDaemonURL  = "https://api.clonk.tv/kd"
	testGBDaemonHost = "api.clonk.tv"
)

func useTempGBLoginFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "oidc-login.json")
	old := oidclogin.FilePath
	oidclogin.FilePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { oidclogin.FilePath = old })
	return path
}

// newFakeDeviceIssuer is a Keycloak stand-in that approves the device code on
// the first poll.
func newFakeDeviceIssuer(t *testing.T, access string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                        srv.URL,
			"token_endpoint":                srv.URL + "/token",
			"device_authorization_endpoint": srv.URL + "/device",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-code",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          srv.URL + "/device-verify",
			"verification_uri_complete": srv.URL + "/device-verify?user_code=ABCD-EFGH",
			"expires_in":                600,
			"interval":                  5,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.PostForm.Get("client_id") != "gardenio-cli" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"` + access + `","refresh_token":"refresh-1","expires_in":300}`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// setLoginFlags points the login command at issuer and httpURL for one test.
func setLoginFlags(t *testing.T, issuer, daemon string, noBrowser bool) {
	t.Helper()
	oldIssuer, oldClient, oldHTTP, oldNoBrowser, oldTimeout, oldSleep, oldStatus :=
		loginIssuer, loginClientID, httpURL, loginNoBrowser, loginTimeout, loginPollSleep, loginStatus
	loginIssuer, loginClientID, httpURL, loginNoBrowser, loginTimeout, loginStatus = issuer, "gardenio-cli", daemon, noBrowser, time.Minute, false
	loginPollSleep = func(time.Duration) {}
	t.Cleanup(func() {
		loginIssuer, loginClientID, httpURL, loginNoBrowser, loginTimeout, loginPollSleep, loginStatus =
			oldIssuer, oldClient, oldHTTP, oldNoBrowser, oldTimeout, oldSleep, oldStatus
	})
}

func stubBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	old := openBrowserHook
	openBrowserHook = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openBrowserHook = old })
	return &opened
}

func runLoginCmd(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	loginCmd.SetOut(&out)
	loginCmd.SetErr(&errOut)
	loginCmd.SetContext(context.Background())
	t.Cleanup(func() { loginCmd.SetOut(nil); loginCmd.SetErr(nil) })
	err = runLogin(loginCmd, nil)
	return out.String(), errOut.String(), err
}

func TestGBLogin_DeviceGrantWritesSessionBoundToHTTPURL(t *testing.T) {
	path := useTempGBLoginFile(t)
	opened := stubBrowser(t)
	issuer := newFakeDeviceIssuer(t, "access-1")
	setLoginFlags(t, issuer.URL, "https://API.clonk.tv:443/kd", false)

	stdout, stderr, err := runLoginCmd(t)
	if err != nil {
		t.Fatalf("gb login: %v (stderr %q)", err, stderr)
	}
	sess, err := oidclogin.Load()
	if err != nil || sess == nil {
		t.Fatalf("session not saved: %+v, %v", sess, err)
	}
	if sess.Daemon != testGBDaemonHost || sess.AccessToken != "access-1" || sess.RefreshToken != "refresh-1" || sess.Issuer != issuer.URL || sess.ClientID != "gardenio-cli" {
		t.Errorf("session = %+v", sess)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("session file mode: %v, %v", st, err)
	}
	if len(*opened) != 1 || !strings.Contains((*opened)[0], "user_code=ABCD-EFGH") {
		t.Errorf("browser opened %v, want the verification link with the code", *opened)
	}
	if !strings.Contains(stderr, "ABCD-EFGH") || !strings.Contains(stderr, "Only enter this code") {
		t.Errorf("prompt lacks the code or the warning: %q", stderr)
	}
	if strings.Contains(stdout+stderr, "access-1") || strings.Contains(stdout+stderr, "refresh-1") || strings.Contains(stdout+stderr, "dev-code") {
		t.Errorf("output leaks credentials: %q / %q", stdout, stderr)
	}
	if !strings.Contains(stdout, "Signed in") || !strings.Contains(stdout, testGBDaemonHost) {
		t.Errorf("stdout = %q", stdout)
	}
	// The viewer is never contacted: the old cookie-handoff paths are gone.
	if strings.Contains(stderr, "cli-handoff") || strings.Contains(stderr, "cli-exchange") {
		t.Errorf("gb login still mentions the cookie handoff: %q", stderr)
	}
}

func TestGBLogin_NoBrowserOnlyPrints(t *testing.T) {
	useTempGBLoginFile(t)
	opened := stubBrowser(t)
	issuer := newFakeDeviceIssuer(t, "access-1")
	setLoginFlags(t, issuer.URL, testGBDaemonURL, true)

	if _, stderr, err := runLoginCmd(t); err != nil {
		t.Fatalf("gb login: %v (stderr %q)", err, stderr)
	}
	if len(*opened) != 0 {
		t.Errorf("--no-browser opened %v", *opened)
	}
}

func TestGBLogin_NeedsADaemonHost(t *testing.T) {
	useTempGBLoginFile(t)
	setLoginFlags(t, "https://issuer.invalid", "", true)
	if _, _, err := runLoginCmd(t); err == nil || !strings.Contains(err.Error(), "--http-url") {
		t.Fatalf("err = %v, want a --http-url binding error", err)
	}
}

// The retired cookie-handoff flags still parse, so old scripts keep working.
func TestGBLogin_RetiredFlagsStillParse(t *testing.T) {
	for _, name := range []string{"viewer-url", "cli-exchange-url", "context"} {
		f := loginCmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("--%s is no longer registered", name)
		}
		if f.Deprecated == "" {
			t.Errorf("--%s is not marked deprecated", name)
		}
	}
}

// A session written by `kd login` (or `gb login`) for the daemon gb talks to is
// gb's bearer; one for another host is never sent, and the hint names gb login.
func TestGBLoginBearer_SharedSessionBoundToDaemon(t *testing.T) {
	useTempGBLoginFile(t)
	now := time.Now()
	if err := oidclogin.Save(&oidclogin.Session{Daemon: testGBDaemonHost, AccessToken: "live", Expiry: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := loginBearer(context.Background(), http.DefaultClient, now, testGBDaemonURL, &bytes.Buffer{}); got != "live" {
		t.Errorf("got %q, want live", got)
	}
	var warn bytes.Buffer
	if got := loginBearer(context.Background(), http.DefaultClient, now, "https://other.example/kd", &warn); got != "" {
		t.Errorf("token sent to another daemon: %q", got)
	}
	if !strings.Contains(warn.String(), "gb: ") || !strings.Contains(warn.String(), "'gb login'") || strings.Contains(warn.String(), "live") {
		t.Errorf("warn = %q", warn.String())
	}
}

func TestGBLogout_ForgetsSharedSession(t *testing.T) {
	path := useTempGBLoginFile(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	old := logoutContext
	logoutContext = ""
	t.Cleanup(func() { logoutContext = old })
	if err := oidclogin.Save(&oidclogin.Session{Daemon: testGBDaemonHost, AccessToken: "a", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := logoutCmd.RunE(logoutCmd, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("session file still present: %v", err)
	}
}

func TestGBLoginStatus_ShowsSessionWithoutTokens(t *testing.T) {
	useTempGBLoginFile(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BEADS_OPERATED_BY", "")
	resetOperatedByBeadMemo()
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	setLoginFlags(t, "https://issuer.invalid", testGBDaemonURL, true)

	var out bytes.Buffer
	if err := showLoginStatus(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Not logged in") {
		t.Errorf("signed-out status = %q", out.String())
	}

	if err := oidclogin.Save(&oidclogin.Session{Issuer: "https://issuer.invalid", Daemon: testGBDaemonHost, AccessToken: "secret-access", RefreshToken: "secret-refresh", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := showLoginStatus(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, testGBDaemonHost) || !strings.Contains(got, "valid") {
		t.Errorf("status = %q", got)
	}
	if strings.Contains(got, "secret-") {
		t.Errorf("status leaks token material: %q", got)
	}
}
