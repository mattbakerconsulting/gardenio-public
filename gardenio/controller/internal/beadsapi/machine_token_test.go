package beadsapi

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixtureJWT builds an unsigned JWT-shaped token whose payload carries the
// claims the first-fetch log line reports. kbeads verifies signatures; this
// package only decodes the payload for metadata.
func fixtureJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return hdr + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fixture-signature"
}

// tokenEndpoint is a recording Keycloak token-endpoint stub.
type tokenEndpoint struct {
	t         *testing.T
	mu        sync.Mutex
	calls     int
	secrets   []string // client secrets seen, in order
	token     string
	expiresIn int
	fail      atomic.Bool
	// reject answers 401 invalid_client: what Keycloak returns while the
	// KeycloakClient has not been reconciled yet (gd-pj5hT3V1la).
	reject atomic.Bool
}

func (e *tokenEndpoint) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.fail.Load() {
			http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
			return
		}
		if e.reject.Load() {
			http.Error(w, `{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "form", http.StatusBadRequest)
			return
		}
		if got := r.PostForm.Get("grant_type"); got != "client_credentials" {
			http.Error(w, "grant_type="+got, http.StatusBadRequest)
			return
		}
		id, secret, ok := r.BasicAuth()
		if !ok {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if id != "gardenio-controller" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		e.mu.Lock()
		e.calls++
		e.secrets = append(e.secrets, secret)
		tok, exp := e.token, e.expiresIn
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": tok,
			"token_type":   "Bearer",
			"expires_in":   exp,
		})
	})
}

func (e *tokenEndpoint) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *tokenEndpoint) seenSecrets() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.secrets...)
}

const (
	fixtureSecret  = "s3cr3t-fixture-value-do-not-log"
	fixtureSecret2 = "r0tated-fixture-value-do-not-log"
)

func writeSecretFile(t *testing.T, dir, value string) string {
	t.Helper()
	p := filepath.Join(dir, "clientSecret")
	if err := os.WriteFile(p, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func setMachineEnv(t *testing.T, url, id, file string) {
	t.Helper()
	t.Setenv(envMachineTokenURL, url)
	t.Setenv(envMachineClientID, id)
	t.Setenv(envMachineClientSecretFile, file)
}

func newFixtureEndpoint(t *testing.T, expiresIn int) (*tokenEndpoint, *httptest.Server, string) {
	t.Helper()
	tok := fixtureJWT(t, map[string]any{
		"azp":                "gardenio-controller",
		"preferred_username": "service-account-gardenio-controller",
		"aud":                []string{"gardenio-traefik-forwardauth", "account"},
		"realm_roles":        []string{"gardenio-operator", "gardenio-machine-accounts"},
		"realm_access":       map[string]any{"roles": []string{"gardenio-operator", "gardenio-machine-token-mint"}},
		"iat":                1700000000,
		"exp":                1700000000 + expiresIn,
	})
	ep := &tokenEndpoint{t: t, token: tok, expiresIn: expiresIn}
	srv := httptest.NewServer(ep.handler())
	t.Cleanup(srv.Close)
	return ep, srv, tok
}

func TestMachineTokenSource_NoneSetIsDisabled(t *testing.T) {
	setMachineEnv(t, "", "", "")
	ts, ok, err := MachineTokenSourceFromEnv(context.Background(), slog.Default())
	if err != nil || ok || ts != nil {
		t.Fatalf("want (nil,false,nil), got (%v,%v,%v)", ts, ok, err)
	}
}

func TestMachineTokenSource_PartialEnvIsAnError(t *testing.T) {
	cases := map[string][3]string{
		"url only":       {"http://kc/token", "", ""},
		"id only":        {"", "gardenio-controller", ""},
		"file only":      {"", "", "/var/run/secrets/x"},
		"url+id no file": {"http://kc/token", "gardenio-controller", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			setMachineEnv(t, c[0], c[1], c[2])
			ts, ok, err := MachineTokenSourceFromEnv(context.Background(), slog.Default())
			if err == nil || ok || ts != nil {
				t.Fatalf("want error, got (%v,%v,%v)", ts, ok, err)
			}
		})
	}
}

func TestMachineTokenSource_FetchCacheAndHalfLifeRefresh(t *testing.T) {
	ep, srv, fixtureTok := newFixtureEndpoint(t, 1800)
	dir := t.TempDir()
	file := writeSecretFile(t, dir, fixtureSecret)
	setMachineEnv(t, srv.URL, "gardenio-controller", file)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	ts, ok, err := MachineTokenSourceFromEnv(context.Background(), logger)
	if err != nil || !ok {
		t.Fatalf("want enabled, got ok=%v err=%v", ok, err)
	}
	src := ts.(*machineTokenSource)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	clock := base
	src.now = func() time.Time { return clock }

	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != fixtureTok {
		t.Fatalf("token mismatch")
	}
	if ep.count() != 1 {
		t.Fatalf("want 1 fetch, got %d", ep.count())
	}

	// Well inside the half-life: cached, no new fetch.
	clock = base.Add(10 * time.Minute)
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ep.count() != 1 {
		t.Fatalf("token must be cached inside half-life; fetches=%d", ep.count())
	}

	// Past half-life (900s of 1800s): refreshed, and the secret file is
	// re-read so a rotated secret reaches the IdP without a restart.
	writeSecretFile(t, dir, fixtureSecret2)
	clock = base.Add(901 * time.Second)
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ep.count() != 2 {
		t.Fatalf("token must refresh after half-life; fetches=%d", ep.count())
	}
	if s := ep.seenSecrets(); len(s) != 2 || s[0] != fixtureSecret || s[1] != fixtureSecret2 {
		t.Fatalf("secret must be re-read on each fetch, saw %q", s)
	}

	// The first-fetch metadata line: claims yes, token/secret bytes never.
	logs := logBuf.String()
	for _, want := range []string{
		"machine token acquired",
		"azp=gardenio-controller",
		"preferred_username=service-account-gardenio-controller",
		"gardenio-traefik-forwardauth",
		"gardenio-machine-accounts",
		"gardenio-machine-token-mint",
		"exp_minus_iat_seconds=1800",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q:\n%s", want, logs)
		}
	}
	if strings.Count(logs, "machine token acquired") != 1 {
		t.Errorf("metadata line must be logged exactly once:\n%s", logs)
	}
	for _, never := range []string{fixtureTok, fixtureSecret, fixtureSecret2, "fixture-signature"} {
		if strings.Contains(logs, never) {
			t.Errorf("log leaks credential material %q:\n%s", never, logs)
		}
	}
}

// recordingDaemon captures every request a beadsapi client sends.
type recordingDaemon struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (d *recordingDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.reqs = append(d.reqs, r.Clone(context.Background()))
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

func (d *recordingDaemon) requests() []*http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*http.Request(nil), d.reqs...)
}

func TestMachineTokenSource_EndpointDownFailsClosed(t *testing.T) {
	ep, srv, _ := newFixtureEndpoint(t, 1800)
	ep.fail.Store(true)
	file := writeSecretFile(t, t.TempDir(), fixtureSecret)
	setMachineEnv(t, srv.URL, "gardenio-controller", file)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	ts, ok, err := MachineTokenSourceFromEnv(context.Background(), logger)
	if err != nil || !ok {
		t.Fatalf("want enabled, got ok=%v err=%v", ok, err)
	}

	daemon := &recordingDaemon{}
	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)
	// A static token is ALSO configured: it must never be used as a fallback.
	client, err := New(Config{HTTPAddr: daemonSrv.URL, Token: "static-fallback-token", TokenSource: ts})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	err = client.UpdateBeadFields(context.Background(), "gd-acct", map[string]string{"total_cost_usd": "1"})
	if err == nil {
		t.Fatal("write must fail when the token endpoint is down")
	}
	if !strings.Contains(err.Error(), "auth_unavailable") {
		t.Fatalf("error must carry auth_unavailable, got: %v", err)
	}
	if reqs := daemon.requests(); len(reqs) != 0 {
		for _, r := range reqs {
			t.Errorf("daemon received %s %s Authorization=%q — no request may be sent without a machine token", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
	}
	if strings.Contains(logBuf.String(), fixtureSecret) {
		t.Errorf("log leaks the secret:\n%s", logBuf.String())
	}

	// Endpoint recovers: the next write goes through with the machine token
	// and not the static one.
	ep.fail.Store(false)
	if err := client.UpdateBeadFields(context.Background(), "gd-acct", map[string]string{"total_cost_usd": "1"}); err != nil {
		t.Fatalf("write after recovery: %v", err)
	}
	reqs := daemon.requests()
	if len(reqs) == 0 {
		t.Fatal("expected the write to reach the daemon after recovery")
	}
	for _, r := range reqs {
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, "static-fallback-token") {
			t.Errorf("static token sent on %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(auth, "Bearer ") || !strings.Contains(auth, ep.token) {
			t.Errorf("machine token not sent on %s %s", r.Method, r.URL.Path)
		}
	}
}

func TestMachineTokenSource_MissingSecretFileIsAuthUnavailable(t *testing.T) {
	_, srv, _ := newFixtureEndpoint(t, 1800)
	setMachineEnv(t, srv.URL, "gardenio-controller", filepath.Join(t.TempDir(), "absent"))
	ts, _, err := MachineTokenSourceFromEnv(context.Background(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auth_unavailable") {
		t.Fatalf("want auth_unavailable, got %v", err)
	}
}

// TestMachineTokenSource_CAFileTrustsPrivateCA: with BEADS_OIDC_CA_FILE the
// token fetch trusts that CA beside the system roots (the keycloak-runtime
// in-cluster CA, gd-lYPZlslOM7). Before the file exists (the optional CA
// volume, cert-manager not yet issued) the fetch fails closed, and it recovers
// once the file appears, without a new source.
func TestMachineTokenSource_CAFileTrustsPrivateCA(t *testing.T) {
	ep := &tokenEndpoint{t: t, token: fixtureJWT(t, map[string]any{"azp": "gardenio-controller"}), expiresIn: 1800}
	srv := httptest.NewTLSServer(ep.handler())
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	setMachineEnv(t, srv.URL, "gardenio-controller", writeSecretFile(t, dir, fixtureSecret))
	t.Setenv(envMachineCAFile, caFile)

	ts, ok, err := MachineTokenSourceFromEnv(context.Background(), slog.Default())
	if err != nil || !ok {
		t.Fatalf("MachineTokenSourceFromEnv = (%v, %v)", ok, err)
	}
	if _, err := ts.Token(context.Background()); err == nil || !IsAuthUnavailable(err) || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("missing CA file: want auth_unavailable naming the CA, got %v", err)
	}
	if err := os.WriteFile(caFile, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Token(context.Background()); err == nil || !IsAuthUnavailable(err) || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("non-PEM CA file: want auth_unavailable, got %v", err)
	}
	if ep.count() != 0 {
		t.Fatalf("token endpoint called %d times without a trusted CA", ep.count())
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := ts.Token(context.Background()); err != nil || tok != ep.token {
		t.Fatalf("with the CA: got (%q, %v)", tok, err)
	}
	if ep.count() != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", ep.count())
	}
}

// TestMachineTokenSource_NoCAFileDistrustsPrivateCA: without the CA file the
// system roots alone do not trust a private CA, so the fetch fails closed.
func TestMachineTokenSource_NoCAFileDistrustsPrivateCA(t *testing.T) {
	ep := &tokenEndpoint{t: t, token: fixtureJWT(t, map[string]any{"azp": "gardenio-controller"}), expiresIn: 1800}
	srv := httptest.NewTLSServer(ep.handler())
	t.Cleanup(srv.Close)
	setMachineEnv(t, srv.URL, "gardenio-controller", writeSecretFile(t, t.TempDir(), fixtureSecret))
	t.Setenv(envMachineCAFile, "")
	ts, _, err := MachineTokenSourceFromEnv(context.Background(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Token(context.Background()); err == nil || !IsAuthUnavailable(err) {
		t.Fatalf("want auth_unavailable (untrusted cert), got %v", err)
	}
	if ep.count() != 0 {
		t.Fatalf("token endpoint answered %d times over an untrusted cert", ep.count())
	}
}

func TestMachineTokenSource_RefreshFailureServesCachedUntilExpiry(t *testing.T) {
	ep, srv, fixtureTok := newFixtureEndpoint(t, 1800)
	file := writeSecretFile(t, t.TempDir(), fixtureSecret)
	ts := NewMachineTokenSource(context.Background(), slog.Default(), srv.URL, "gardenio-controller", file)
	src := ts.(*machineTokenSource)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	clock := base
	src.now = func() time.Time { return clock }
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	ep.fail.Store(true)
	clock = base.Add(1000 * time.Second) // past half-life, before expiry
	got, err := ts.Token(context.Background())
	if err != nil || got != fixtureTok {
		t.Fatalf("stale-but-valid token must be served, got %q err=%v", got, err)
	}
	clock = base.Add(1801 * time.Second) // past expiry: fail closed
	if _, err := ts.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "auth_unavailable") {
		t.Fatalf("expired token must not be served, got err=%v", err)
	}
}

func TestParseJWTClaims_AudienceStringOrArray(t *testing.T) {
	one := parseJWTClaims(fixtureJWT(t, map[string]any{"aud": "x"}))
	if a := one.audience(); len(a) != 1 || a[0] != "x" {
		t.Fatalf("string aud: %v", a)
	}
	many := parseJWTClaims(fixtureJWT(t, map[string]any{"aud": []string{"x", "y"}}))
	if a := many.audience(); len(a) != 2 {
		t.Fatalf("array aud: %v", a)
	}
	if c := parseJWTClaims("not-a-jwt"); c.AZP != "" || c.audience() != nil {
		t.Fatalf("garbage must decode to zero claims: %+v", c)
	}
}

// Warm (gd-pj5hT3V1la): the first fetch on an enabling roll races the
// Keycloak operator creating the client, so the token endpoint answers
// invalid_client for a few seconds. Warm retries with 1s, 2s, 4s... backoff
// (capped at 30s) until the first token is held, then Token serves from the
// cache without another fetch.
func TestMachineTokenSource_WarmRetriesInvalidClientUntilReconciled(t *testing.T) {
	ep, srv, fixtureTok := newFixtureEndpoint(t, 1800)
	ep.reject.Store(true)
	file := writeSecretFile(t, t.TempDir(), fixtureSecret)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	ts := NewMachineTokenSource(context.Background(), logger, srv.URL, "gardenio-controller", file)
	src := ts.(*machineTokenSource)

	var delays []time.Duration
	src.sleep = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		if len(delays) == 3 {
			ep.reject.Store(false) // the operator reconciles the client
		}
		return nil
	}
	if err := ts.(MachineTokenWarmer).Warm(context.Background()); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("backoff delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delay[%d] = %v, want %v", i, delays[i], want[i])
		}
	}
	if ep.count() != 1 {
		t.Errorf("successful fetches = %d, want 1", ep.count())
	}
	// Warm populated the cache: Token needs no fetch.
	got, err := ts.Token(context.Background())
	if err != nil || got != fixtureTok {
		t.Fatalf("Token after Warm = %q err=%v", got, err)
	}
	if ep.count() != 1 {
		t.Errorf("Token after Warm fetched again: %d", ep.count())
	}
	log := logBuf.String()
	if !strings.Contains(log, "machine token warm-up failed") || !strings.Contains(log, "invalid_client") {
		t.Errorf("expected a warn per failed attempt naming invalid_client:\n%s", log)
	}
	if !strings.Contains(log, "attempts=4") {
		t.Errorf("expected the success line to count the attempts:\n%s", log)
	}
	if strings.Contains(log, fixtureSecret) || strings.Contains(log, fixtureTok) {
		t.Errorf("log leaks the secret or the token:\n%s", log)
	}
}

func TestMachineTokenSource_WarmBackoffCapsAt30s(t *testing.T) {
	ep, srv, _ := newFixtureEndpoint(t, 1800)
	ep.fail.Store(true)
	file := writeSecretFile(t, t.TempDir(), fixtureSecret)
	ts := NewMachineTokenSource(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), srv.URL, "gardenio-controller", file)
	src := ts.(*machineTokenSource)
	var delays []time.Duration
	src.sleep = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		if len(delays) == 8 {
			ep.fail.Store(false)
		}
		return nil
	}
	if err := src.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 1,2,4,8,16,30,30,30
	for i, d := range delays {
		if d > machineWarmBackoffMax {
			t.Errorf("delay[%d] = %v exceeds cap %v", i, d, machineWarmBackoffMax)
		}
	}
	if last := delays[len(delays)-1]; last != machineWarmBackoffMax {
		t.Errorf("delay did not reach the cap: %v", delays)
	}
}

func TestMachineTokenSource_WarmStopsOnContextAndIsAuthUnavailable(t *testing.T) {
	ep, srv, _ := newFixtureEndpoint(t, 1800)
	ep.reject.Store(true)
	file := writeSecretFile(t, t.TempDir(), fixtureSecret)
	ts := NewMachineTokenSource(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), srv.URL, "gardenio-controller", file)
	src := ts.(*machineTokenSource)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	src.sleep = func(ctx context.Context, _ time.Duration) error {
		attempts++
		if attempts == 2 {
			cancel() // the pod is told to shut down mid-wait
		}
		return ctx.Err()
	}
	err := src.Warm(ctx)
	if err == nil {
		t.Fatal("Warm must return once ctx ends")
	}
	if !IsAuthUnavailable(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("want auth_unavailable wrapping context.Canceled, got %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestMachineTokenSource_WarmIsNoOpWhenCached(t *testing.T) {
	ep, srv, _ := newFixtureEndpoint(t, 1800)
	file := writeSecretFile(t, t.TempDir(), fixtureSecret)
	ts := NewMachineTokenSource(context.Background(), slog.Default(), srv.URL, "gardenio-controller", file)
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	ep.fail.Store(true) // would fail if Warm fetched
	if err := WarmMachineToken(context.Background(), ts); err != nil {
		t.Fatalf("Warm with a cached token must not fetch: %v", err)
	}
	if ep.count() != 1 {
		t.Errorf("fetches = %d, want 1", ep.count())
	}
}

func TestWarmMachineToken_NilAndNonWarmerAreNoOps(t *testing.T) {
	if err := WarmMachineToken(context.Background(), nil); err != nil {
		t.Fatalf("nil source: %v", err)
	}
	if err := WarmMachineToken(context.Background(), staticTokenSource("x")); err != nil {
		t.Fatalf("non-warmer: %v", err)
	}
}

func TestIsAuthUnavailable(t *testing.T) {
	_, srv, _ := newFixtureEndpoint(t, 1800)
	ts := NewMachineTokenSource(context.Background(), slog.Default(), srv.URL, "gardenio-controller", filepath.Join(t.TempDir(), "absent"))
	_, err := ts.Token(context.Background())
	if !IsAuthUnavailable(err) {
		t.Errorf("secret-file failure must be auth_unavailable: %v", err)
	}
	if !errors.Is(err, ErrMachineAuthUnavailable) {
		t.Errorf("secret-file failure must wrap the sentinel: %v", err)
	}
	if !IsAuthUnavailable(fmt.Errorf("wrapped: %w", err)) {
		t.Error("wrapping must preserve the classification")
	}
	if !IsAuthUnavailable(errors.New("flattened: auth_unavailable: machine token fetch failed")) {
		t.Error("the text marker alone must still classify")
	}
	if IsAuthUnavailable(nil) || IsAuthUnavailable(errors.New("403 forbidden")) {
		t.Error("nil and unrelated errors are not auth_unavailable")
	}
}
