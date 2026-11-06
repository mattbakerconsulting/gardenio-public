package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
)

// fakeKeycloak serves discovery, the device authorization endpoint and the
// token endpoint. tokenReplies are returned in order from the device_code
// grant; the last one repeats.
type fakeKeycloak struct {
	srv          *httptest.Server
	tokenReplies []string // JSON bodies; prefixed "400:" for a 400 response
	tokenCalls   atomic.Int32
	refreshCalls atomic.Int32
	refreshReply string
	deviceClient atomic.Value // client_id seen at the device endpoint
	deviceScope  atomic.Value // scope seen at the device endpoint
	// refreshOutages is how many upcoming refresh calls answer the way the
	// public edge does while Keycloak restarts: a 502 with an nginx HTML body,
	// no OAuth error.
	refreshOutages atomic.Int32
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	f := &fakeKeycloak{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                        f.srv.URL,
			"token_endpoint":                f.srv.URL + "/token",
			"device_authorization_endpoint": f.srv.URL + "/device",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.deviceClient.Store(r.PostForm.Get("client_id"))
		f.deviceScope.Store(r.PostForm.Get("scope"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-code",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          f.srv.URL + "/device-verify",
			"verification_uri_complete": f.srv.URL + "/device-verify?user_code=ABCD-EFGH",
			"expires_in":                600,
			"interval":                  5,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("grant_type") == "refresh_token" {
			f.refreshCalls.Add(1)
			if f.refreshOutages.Load() > 0 {
				f.refreshOutages.Add(-1)
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("<html><body><h1>502 Bad Gateway</h1><hr>nginx</body></html>"))
				return
			}
			if r.PostForm.Get("refresh_token") != "refresh-1" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token is not active"}`))
				return
			}
			_, _ = w.Write([]byte(f.refreshReply))
			return
		}
		i := int(f.tokenCalls.Add(1)) - 1
		if i >= len(f.tokenReplies) {
			i = len(f.tokenReplies) - 1
		}
		body := f.tokenReplies[i]
		if strings.HasPrefix(body, "400:") {
			w.WriteHeader(http.StatusBadRequest)
			body = strings.TrimPrefix(body, "400:")
		}
		_, _ = w.Write([]byte(body))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func fakeJWT(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

// noRetrySleep makes the refresh backoff instant and records how often it ran.
func noRetrySleep(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := oidclogin.RetrySleep
	oidclogin.RetrySleep = func(time.Duration) { n.Add(1) }
	t.Cleanup(func() { oidclogin.RetrySleep = old })
	return &n
}

func useTempLoginFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "oidc-login.json")
	old := oidclogin.FilePath
	oidclogin.FilePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { oidclogin.FilePath = old })
	return path
}

func TestRunDeviceLogin_PollsUntilApproved(t *testing.T) {
	access := fakeJWT(map[string]any{
		"email":       "owner@example.com",
		"realm_roles": []string{"gardenio-admin", "offline_access"},
	})
	f := newFakeKeycloak(t)
	f.tokenReplies = []string{
		`400:{"error":"authorization_pending"}`,
		`400:{"error":"slow_down"}`,
		`{"access_token":"` + access + `","refresh_token":"refresh-1","expires_in":300}`,
	}

	var out bytes.Buffer
	var sleeps []time.Duration
	sess, err := runDeviceLoginForLand(context.Background(), f.srv.Client(), f.srv.URL, "gardenio-cli", "", &out,
		func(d time.Duration) { sleeps = append(sleeps, d) })
	if err != nil {
		t.Fatalf("runDeviceLoginForLand: %v", err)
	}
	if sess.AccessToken != access || sess.RefreshToken != "refresh-1" || sess.ClientID != "gardenio-cli" {
		t.Errorf("unexpected session: %+v", sess)
	}
	if got := f.deviceClient.Load(); got != "gardenio-cli" {
		t.Errorf("device endpoint saw client_id %v", got)
	}
	// 5s, 5s (pending), then +5s after slow_down.
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Errorf("sleeps = %v, want %v", sleeps, want)
			break
		}
	}
	prompt := out.String()
	for _, s := range []string{"/device-verify", "ABCD-EFGH"} {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing %q:\n%s", s, prompt)
		}
	}
	// The prompt must never echo credentials.
	for _, s := range []string{access, "refresh-1", "dev-code"} {
		if strings.Contains(prompt, s) {
			t.Errorf("prompt leaks %q", s)
		}
	}
}

func TestRunDeviceLogin_Denied(t *testing.T) {
	f := newFakeKeycloak(t)
	f.tokenReplies = []string{`400:{"error":"access_denied"}`}
	_, err := runDeviceLoginForLand(context.Background(), f.srv.Client(), f.srv.URL, "gardenio-cli", "", &bytes.Buffer{}, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v, want denied", err)
	}
}

func TestRunDeviceLogin_ExpiredCode(t *testing.T) {
	f := newFakeKeycloak(t)
	f.tokenReplies = []string{`400:{"error":"expired_token"}`}
	_, err := runDeviceLoginForLand(context.Background(), f.srv.Client(), f.srv.URL, "gardenio-cli", "", &bytes.Buffer{}, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want expired", err)
	}
}

func TestDiscoverEndpoints_RejectsIssuerMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"https://evil.example","token_endpoint":"https://evil.example/token"}`))
	}))
	defer srv.Close()
	if _, err := oidclogin.Discover(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("expected issuer mismatch error")
	}
}

func TestSaveLoginSession_Perms(t *testing.T) {
	path := useTempLoginFile(t)
	s := &oidclogin.Session{Issuer: "i", ClientID: "c", AccessToken: "a", Expiry: time.Now().Add(time.Hour)}
	if err := oidclogin.Save(s); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
	dst, _ := os.Stat(filepath.Dir(path))
	if perm := dst.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
	// Overwrite keeps 0600 and leaves no temp files behind.
	if err := oidclogin.Save(s); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("state dir has %d entries, want 1", len(entries))
	}
	got, err := oidclogin.Load()
	if err != nil || got == nil || got.AccessToken != "a" {
		t.Errorf("round trip: %+v, %v", got, err)
	}
}

func TestLoginBearer_NotSignedIn(t *testing.T) {
	useTempLoginFile(t)
	var warn bytes.Buffer
	if got := loginBearer(context.Background(), http.DefaultClient, time.Now(), testDaemonURL, &warn); got != "" || warn.Len() != 0 {
		t.Errorf("got %q, warn %q; want silent empty", got, warn.String())
	}
}

func TestLoginBearer_FreshTokenUsedAsIs(t *testing.T) {
	useTempLoginFile(t)
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{Daemon: testDaemonHost, AccessToken: "live", Expiry: now.Add(time.Hour)})
	if got := loginBearer(context.Background(), http.DefaultClient, now, testDaemonURL, &bytes.Buffer{}); got != "live" {
		t.Errorf("got %q, want live", got)
	}
}

func TestLoginBearer_RefreshesExpiredAndPersists(t *testing.T) {
	useTempLoginFile(t)
	f := newFakeKeycloak(t)
	f.refreshReply = `{"access_token":"new-access","refresh_token":"refresh-2","expires_in":300}`
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: f.srv.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "old", RefreshToken: "refresh-1", Expiry: now.Add(-time.Minute),
	})

	if got := loginBearer(context.Background(), f.srv.Client(), now, testDaemonURL, &bytes.Buffer{}); got != "new-access" {
		t.Fatalf("got %q, want new-access", got)
	}
	saved, _ := oidclogin.Load()
	if saved.AccessToken != "new-access" || saved.RefreshToken != "refresh-2" {
		t.Errorf("refreshed session not persisted: %+v", saved)
	}
	// A second call within the TTL does not hit Keycloak again.
	if got := loginBearer(context.Background(), f.srv.Client(), now.Add(time.Minute), testDaemonURL, &bytes.Buffer{}); got != "new-access" {
		t.Errorf("second call got %q", got)
	}
	if n := f.refreshCalls.Load(); n != 1 {
		t.Errorf("refresh calls = %d, want 1", n)
	}
}

func TestLoginBearer_RefreshFailureFallsBackWithHint(t *testing.T) {
	useTempLoginFile(t)
	f := newFakeKeycloak(t)
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: f.srv.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "old", RefreshToken: "revoked", Expiry: now.Add(-time.Minute),
	})
	var warn bytes.Buffer
	if got := loginBearer(context.Background(), f.srv.Client(), now, testDaemonURL, &warn); got != "" {
		t.Errorf("got %q, want empty so the caller falls back", got)
	}
	if !strings.Contains(warn.String(), "kd login") || !strings.Contains(warn.String(), "ended the login session") {
		t.Errorf("warning lacks the re-login hint: %q", warn.String())
	}
	if strings.Contains(warn.String(), "revoked") || strings.Contains(warn.String(), "old") {
		t.Errorf("warning leaks token material: %q", warn.String())
	}
	// A refusal is final: no retry.
	if n := f.refreshCalls.Load(); n != 1 {
		t.Errorf("refresh calls = %d, want 1 (a refused refresh is not retried)", n)
	}
}

// gd-NrzwHXUxV0: a refresh that fails because Keycloak is restarting (the edge
// answers 502, no OAuth error) is retried, and succeeds once Keycloak is back.
func TestLoginBearer_TransientRefreshFailureRetries(t *testing.T) {
	useTempLoginFile(t)
	slept := noRetrySleep(t)
	f := newFakeKeycloak(t)
	f.refreshOutages.Store(1)
	f.refreshReply = `{"access_token":"new-access","refresh_token":"refresh-2","expires_in":300}`
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: f.srv.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "old", RefreshToken: "refresh-1", Expiry: now.Add(-time.Minute),
	})
	var warn bytes.Buffer
	if got := loginBearer(context.Background(), f.srv.Client(), now, testDaemonURL, &warn); got != "new-access" {
		t.Fatalf("got %q, want new-access after one retry (warn %q)", got, warn.String())
	}
	if n := f.refreshCalls.Load(); n != 2 {
		t.Errorf("refresh calls = %d, want 2", n)
	}
	if slept.Load() != 1 {
		t.Errorf("backoff sleeps = %d, want 1", slept.Load())
	}
	if warn.Len() != 0 {
		t.Errorf("a recovered refresh should be silent, got %q", warn.String())
	}
}

// gd-NrzwHXUxV0: when Keycloak stays unreachable the session is NOT expired.
// kd keeps it on disk, does not send a token, and says to retry rather than to
// sign in again.
func TestLoginBearer_TransientRefreshFailureKeepsSession(t *testing.T) {
	useTempLoginFile(t)
	noRetrySleep(t)
	f := newFakeKeycloak(t)
	f.refreshOutages.Store(100)
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: f.srv.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "old", RefreshToken: "refresh-1", Expiry: now.Add(-time.Minute),
	})
	var warn bytes.Buffer
	if got := loginBearer(context.Background(), f.srv.Client(), now, testDaemonURL, &warn); got != "" {
		t.Fatalf("got %q, want empty so the caller falls back", got)
	}
	if n := f.refreshCalls.Load(); n != oidclogin.RefreshAttempts {
		t.Errorf("refresh calls = %d, want %d", n, oidclogin.RefreshAttempts)
	}
	msg := warn.String()
	if strings.Contains(msg, "expired") || strings.Contains(msg, "run 'kd login'") {
		t.Errorf("a transient failure must not read as an expired session: %q", msg)
	}
	if !strings.Contains(msg, "retry") || !strings.Contains(msg, "session is kept") {
		t.Errorf("warning should say the session is kept and to retry: %q", msg)
	}
	if strings.Contains(msg, "refresh-1") || strings.Contains(msg, "old") {
		t.Errorf("warning leaks token material: %q", msg)
	}
	saved, err := oidclogin.Load()
	if err != nil || saved == nil || saved.RefreshToken != "refresh-1" || saved.AccessToken != "old" {
		t.Errorf("session must be kept intact for the next run: %+v, %v", saved, err)
	}
}

// Inside the early-refresh window the cached token is still valid, and the
// daemon verifies it without Keycloak: send it rather than nothing.
func TestLoginBearer_TransientRefreshFailureWithinSkewUsesCachedToken(t *testing.T) {
	useTempLoginFile(t)
	noRetrySleep(t)
	f := newFakeKeycloak(t)
	f.refreshOutages.Store(100)
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: f.srv.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "still-good", RefreshToken: "refresh-1", Expiry: now.Add(10 * time.Second),
	})
	var warn bytes.Buffer
	if got := loginBearer(context.Background(), f.srv.Client(), now, testDaemonURL, &warn); got != "still-good" {
		t.Fatalf("got %q, want the cached token (warn %q)", got, warn.String())
	}
	if strings.Contains(warn.String(), "expired") || !strings.Contains(warn.String(), "cached token") {
		t.Errorf("warning should explain the cached token is used: %q", warn.String())
	}
}

// Discovery itself failing (the edge 502s everything while Keycloak boots) is
// transient too.
func TestLoginBearer_DiscoveryDownKeepsSession(t *testing.T) {
	useTempLoginFile(t)
	noRetrySleep(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway nginx</html>"))
	}))
	t.Cleanup(down.Close)
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: down.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "old", RefreshToken: "refresh-1", Expiry: now.Add(-time.Minute),
	})
	var warn bytes.Buffer
	if got := loginBearer(context.Background(), down.Client(), now, testDaemonURL, &warn); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if strings.Contains(warn.String(), "expired") || !strings.Contains(warn.String(), "session is kept") {
		t.Errorf("warning: %q", warn.String())
	}
	if saved, _ := oidclogin.Load(); saved == nil || saved.RefreshToken != "refresh-1" {
		t.Errorf("session not kept: %+v", saved)
	}
}

// A discovery document for the wrong issuer is a misconfiguration: no retry,
// and the session is still not declared expired.
func TestLoginBearer_DiscoveryIssuerMismatchNotRetried(t *testing.T) {
	useTempLoginFile(t)
	slept := noRetrySleep(t)
	var hits atomic.Int32
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"issuer":"https://evil.example","token_endpoint":"https://evil.example/token"}`))
	}))
	t.Cleanup(wrong.Close)
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: wrong.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "old", RefreshToken: "refresh-1", Expiry: now.Add(-time.Minute),
	})
	var warn bytes.Buffer
	if got := loginBearer(context.Background(), wrong.Client(), now, testDaemonURL, &warn); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if hits.Load() != 1 || slept.Load() != 0 {
		t.Errorf("discovery hits = %d, sleeps = %d; want 1 and 0", hits.Load(), slept.Load())
	}
	if !strings.Contains(warn.String(), "does not match") {
		t.Errorf("warn: %q", warn.String())
	}
}

func TestRefreshRefused(t *testing.T) {
	for code, want := range map[string]bool{
		"invalid_grant": true, "invalid_client": true, "unauthorized_client": true,
		"temporarily_unavailable": false, "server_error": false, "": false,
	} {
		if got := oidclogin.RefreshRefused(&oidclogin.OAuthError{Code: code}); got != want {
			t.Errorf("refreshRefused(%q) = %v, want %v", code, got, want)
		}
	}
	if oidclogin.RefreshRefused(errors.New("oidc discovery: x returned 502 Bad Gateway")) {
		t.Error("a non-OAuth error must be transient")
	}
}

// gd-NrzwHXUxV0 fix shape (a): a 401 for a token kd believed fresh is retried
// once with a token refreshed through Keycloak.
func TestLoginSessionRefresher_RefreshesAndPersists(t *testing.T) {
	useTempLoginFile(t)
	noRetrySleep(t)
	f := newFakeKeycloak(t)
	f.refreshReply = `{"access_token":"new-access","refresh_token":"refresh-2","expires_in":300}`
	now := time.Now()
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: f.srv.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "rejected-but-unexpired", RefreshToken: "refresh-1", Expiry: now.Add(4 * time.Minute),
	})
	var warn bytes.Buffer
	tok, err := loginSessionRefresher(f.srv.Client(), testDaemonURL, &warn)(context.Background())
	if err != nil || tok != "new-access" {
		t.Fatalf("got %q, %v; want new-access", tok, err)
	}
	if saved, _ := oidclogin.Load(); saved == nil || saved.AccessToken != "new-access" || saved.RefreshToken != "refresh-2" {
		t.Errorf("refreshed session not persisted: %+v", saved)
	}
	if !strings.Contains(warn.String(), "retrying once") {
		t.Errorf("warn: %q", warn.String())
	}
}

func TestLoginSessionRefresher_RefusedHintsReLogin(t *testing.T) {
	useTempLoginFile(t)
	noRetrySleep(t)
	f := newFakeKeycloak(t)
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: f.srv.URL, ClientID: "gardenio-cli", Daemon: testDaemonHost,
		AccessToken: "x", RefreshToken: "revoked", Expiry: time.Now().Add(4 * time.Minute),
	})
	var warn bytes.Buffer
	if tok, err := loginSessionRefresher(f.srv.Client(), testDaemonURL, &warn)(context.Background()); err == nil || tok != "" {
		t.Fatalf("got %q, %v; want failure", tok, err)
	}
	if !strings.Contains(warn.String(), "kd login") || strings.Contains(warn.String(), "revoked") {
		t.Errorf("warn: %q", warn.String())
	}
}

func TestLoginSessionRefresher_OtherDaemonRefused(t *testing.T) {
	useTempLoginFile(t)
	f := newFakeKeycloak(t)
	_ = oidclogin.Save(&oidclogin.Session{
		Issuer: f.srv.URL, ClientID: "gardenio-cli", Daemon: "other.example",
		AccessToken: "x", RefreshToken: "refresh-1", Expiry: time.Now().Add(4 * time.Minute),
	})
	if tok, err := loginSessionRefresher(f.srv.Client(), testDaemonURL, &bytes.Buffer{})(context.Background()); err == nil || tok != "" {
		t.Fatalf("got %q, %v; want refusal", tok, err)
	}
	if f.refreshCalls.Load() != 0 {
		t.Error("must not touch Keycloak for a session bound to another daemon")
	}
}

func TestDescribeIdentity(t *testing.T) {
	tok := fakeJWT(map[string]any{
		"email":        "owner@example.com",
		"realm_roles":  []string{"gardenio-admin", "default-roles"},
		"realm_access": map[string]any{"roles": []string{"gardenio-admin", "gardenio-viewer"}},
	})
	if got, want := oidclogin.DescribeIdentity(tok), " as owner@example.com (gardenio-admin, gardenio-viewer)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := oidclogin.DescribeIdentity("not-a-jwt"); got != "" {
		t.Errorf("got %q for a non-JWT", got)
	}
}

func TestLogoutCmd(t *testing.T) {
	path := useTempLoginFile(t)
	_ = oidclogin.Save(&oidclogin.Session{AccessToken: "a", Expiry: time.Now().Add(time.Hour)})
	var out bytes.Buffer
	logoutCmd.SetOut(&out)
	t.Cleanup(func() { logoutCmd.SetOut(nil) })
	if err := logoutCmd.RunE(logoutCmd, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("session file still present: %v", err)
	}
	if err := logoutCmd.RunE(logoutCmd, nil); err != nil || !strings.Contains(out.String(), "Not signed in") {
		t.Errorf("second logout: err=%v out=%q", err, out.String())
	}
}

const (
	testDaemonURL  = "https://beads.example.com/kd"
	testDaemonHost = "beads.example.com"
)

func TestDaemonHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://Beads.Example.com/kd": "beads.example.com",
		"http://localhost:8080":        "localhost",
		"localhost:9090":               "localhost",
		"beads.example.com":            "beads.example.com",
		"":                             "",
		"https://[::1]:8080/x":         "::1",
	} {
		if got := oidclogin.DaemonHost(in); got != want {
			t.Errorf("daemonHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// The cached device-grant token is sent only to the daemon it was minted for:
// a different --http-url / `kd remote` / --server host never receives it, and
// nothing is refreshed over the network for the wrong daemon.
func TestLoginBearer_BoundToDaemonHost(t *testing.T) {
	useTempLoginFile(t)
	f := newFakeKeycloak(t)
	now := time.Now()
	for _, sess := range []*oidclogin.Session{
		{Issuer: f.srv.URL, Daemon: testDaemonHost, AccessToken: "live", Expiry: now.Add(time.Hour)},
		{Issuer: f.srv.URL, Daemon: testDaemonHost, AccessToken: "old", RefreshToken: "r", Expiry: now.Add(-time.Minute)},
	} {
		_ = oidclogin.Save(sess)
		for _, other := range []string{"https://other.example.org/kd", "other.example.org:9090", "http://localhost:8080", ""} {
			var warn bytes.Buffer
			if got := loginBearer(context.Background(), f.srv.Client(), now, other, &warn); got != "" {
				t.Errorf("target %q: token sent to a different daemon: %q", other, got)
			}
			if !strings.Contains(warn.String(), "kd login") {
				t.Errorf("target %q: no re-login hint: %q", other, warn.String())
			}
			if strings.Contains(warn.String(), "live") || strings.Contains(warn.String(), "old") {
				t.Errorf("warning leaks token material: %q", warn.String())
			}
		}
	}
	if n := f.refreshCalls.Load(); n != 0 {
		t.Errorf("refresh calls = %d for a mismatched daemon, want 0", n)
	}
	// Same host, different scheme/port/path/case: still the bound daemon.
	_ = oidclogin.Save(&oidclogin.Session{Daemon: testDaemonHost, AccessToken: "live", Expiry: now.Add(time.Hour)})
	if got := loginBearer(context.Background(), f.srv.Client(), now, "http://BEADS.example.com:8080/other", &bytes.Buffer{}); got != "live" {
		t.Errorf("same-host target got %q, want live", got)
	}
}

// A session with no recorded daemon (unbound) is never sent.
func TestLoginBearer_UnboundSessionRefused(t *testing.T) {
	useTempLoginFile(t)
	_ = oidclogin.Save(&oidclogin.Session{AccessToken: "live", Expiry: time.Now().Add(time.Hour)})
	if got := loginBearer(context.Background(), http.DefaultClient, time.Now(), testDaemonURL, &bytes.Buffer{}); got != "" {
		t.Errorf("unbound session sent: %q", got)
	}
}

// TestSaveLoginSession_JSONFieldNames pins the six field names of the file kd
// login writes. kd and gb share the writer (gardenio/clicreds/oidclogin), but
// kd and gb binaries of older versions still read and write this file, so the
// names are a compatibility contract.
func TestSaveLoginSession_JSONFieldNames(t *testing.T) {
	path := useTempLoginFile(t)
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := oidclogin.Save(&oidclogin.Session{
		Issuer: "https://i", ClientID: "c", Daemon: "api.clonk.tv",
		AccessToken: "a", RefreshToken: "r", Expiry: exp,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"issuer":        "https://i",
		"client_id":     "c",
		"daemon":        "api.clonk.tv",
		"access_token":  "a",
		"refresh_token": "r",
		"expiry":        "2030-01-02T03:04:05Z",
	}
	if len(got) != len(want) {
		t.Errorf("session has %d fields, want exactly %d: %s", len(got), len(want), data)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q = %v, want %v (gb's reader depends on these names)", k, got[k], v)
		}
	}
}

// A sign-in for a land (a daemon URL through the land door) requests the
// land's client scope, so the token carries aud land-<name> (gd-eAfptUBh9Z),
// and its session lands in its own file, next to the ordinary one.
func TestRunDeviceLogin_LandRequestsLandScope(t *testing.T) {
	path := useTempLoginFile(t)
	f := newFakeKeycloak(t)
	f.tokenReplies = []string{`{"access_token":"land-tok","refresh_token":"refresh-1","expires_in":300}`}
	sess, err := runDeviceLoginForLand(context.Background(), f.srv.Client(), f.srv.URL, "gardenio-cli", "acme", io.Discard, func(time.Duration) {})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.deviceScope.Load(); got != "openid email profile land-acme" {
		t.Errorf("device endpoint saw scope %v", got)
	}
	if sess.Land != "acme" {
		t.Errorf("session land = %q, want acme", sess.Land)
	}
	if _, err := runDeviceLoginForLand(context.Background(), f.srv.Client(), f.srv.URL, "gardenio-cli", "", io.Discard, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	if got := f.deviceScope.Load(); got != "openid email profile" {
		t.Errorf("ordinary sign-in saw scope %v", got)
	}
	sess.Daemon = "api.example.com"
	if err := oidclogin.Save(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a land session overwrote the ordinary session file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "oidc-login-land-acme.json")); err != nil {
		t.Errorf("land session file: %v", err)
	}
}

// Each land door gets its own land's session and nothing else: the ordinary
// session never goes through a land door, a land's session never goes
// anywhere but its own door.
func TestLoginBearer_LandSessionsPerDoor(t *testing.T) {
	useTempLoginFile(t)
	exp := time.Now().Add(time.Hour)
	for _, s := range []*oidclogin.Session{
		{Daemon: "api.example.com", AccessToken: "plain", Expiry: exp},
		{Daemon: "api.example.com", Land: "acme", AccessToken: "acme-tok", Expiry: exp},
	} {
		if err := oidclogin.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	for target, want := range map[string]string{
		"https://api.example.com/kd":             "plain",
		"https://api.example.com/land/acme":      "acme-tok",
		"https://api.example.com/land/acme/":     "acme-tok",
		"https://api.example.com/land/beta":      "",
		"https://api.example.com/landing/acme":   "plain",
		"https://api.example.com/land/Acme":      "plain",
		"https://api.example.com/land/acme%2fx/": "plain",
	} {
		if got := loginBearer(context.Background(), http.DefaultClient, time.Now(), target, io.Discard); got != want {
			t.Errorf("%s: bearer %q, want %q", target, got, want)
		}
	}
	// A land session file whose recorded land disagrees with its door is
	// never sent.
	if err := oidclogin.Save(&oidclogin.Session{Daemon: "api.example.com", Land: "beta", AccessToken: "beta-tok", Expiry: exp}); err != nil {
		t.Fatal(err)
	}
	p, _ := oidclogin.SessionPath("beta")
	data, _ := os.ReadFile(p)
	acme, _ := oidclogin.SessionPath("acme")
	_ = os.WriteFile(acme, data, 0o600)
	if got := loginBearer(context.Background(), http.DefaultClient, time.Now(), "https://api.example.com/land/acme", io.Discard); got != "" {
		t.Errorf("mislabelled land session sent: %q", got)
	}
}

func TestLandFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.clonk.tv/land/acme":      "acme",
		"https://api.clonk.tv/land/acme/v1/x": "acme",
		"https://api.clonk.tv/kd":             "",
		"https://api.clonk.tv/land/":          "",
		"https://api.clonk.tv/land/9acme":     "",
		"https://api.clonk.tv/x/land/acme":    "",
		"api.clonk.tv/land/acme":              "",
		"https://api.clonk.tv/land/acme%2F..": "",
		"https://api.clonk.tv/land/a-b-c?x=y": "a-b-c",
	} {
		if got := oidclogin.LandFromURL(in); got != want {
			t.Errorf("LandFromURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := oidclogin.SessionPath("../x"); err == nil {
		t.Error("SessionPath accepted a path-traversing land")
	}
}
