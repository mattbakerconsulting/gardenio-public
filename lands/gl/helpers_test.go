package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/lands/contract/landsv1"
)

// fixture reads one of the contract's golden fixtures, so the fake land
// answers exactly what the contract says a land answers.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "contract", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seen is one request the fake land received.
type seen struct {
	method, path, query string
	header              http.Header
	body                string
}

// fakeLand is a land door: it routes /land/<land>/v1/lands/v1/lands/<land>/...
// to canned answers and records every request.
type fakeLand struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	reqs     []seen
	routes   map[string]http.HandlerFunc // "METHOD /rest" after the land's prefix
	contract string                      // X-Lands-Contract; "" sends ContractVersion
}

func newFakeLand(t *testing.T, land string) *fakeLand {
	t.Helper()
	f := &fakeLand{t: t, routes: map[string]http.HandlerFunc{}}
	prefix := "/land/" + land + "/v1/lands/v1/lands/" + land
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
		f.mu.Unlock()
		v := f.contract
		if v == "" {
			v = landsv1.ContractVersion
		}
		w.Header().Set(landsv1.HeaderContract, v)
		rest, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			problem(w, t, 404, "Problem/not_found.json")
			return
		}
		h := f.routes[r.Method+" "+rest]
		if h == nil {
			problem(w, t, 404, "Problem/not_found.json")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLand) on(route string, h http.HandlerFunc) { f.routes[route] = h }

// answer serves a fixture with a status.
func (f *fakeLand) answer(route string, status int, name string) {
	b := fixture(f.t, name)
	f.on(route, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	})
}

func (f *fakeLand) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.reqs...)
}

func problem(w http.ResponseWriter, t *testing.T, status int, name string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(fixture(t, name))
}

// useTempState moves the session files and gl's config into a temp dir.
func useTempState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldF, oldC := oidclogin.FilePath, configPath
	oidclogin.FilePath = func() (string, error) { return filepath.Join(dir, "state", "oidc-login.json"), nil }
	configPath = func() (string, error) { return filepath.Join(dir, "config", "gl.json"), nil }
	oldSleep := oidclogin.RetrySleep
	oidclogin.RetrySleep = func(time.Duration) {}
	t.Cleanup(func() { oidclogin.FilePath, configPath, oidclogin.RetrySleep = oldF, oldC, oldSleep })
	return dir
}

// signIn caches a land session bound to host.
func signIn(t *testing.T, land, host string, s oidclogin.Session) {
	t.Helper()
	s.Land, s.Daemon = land, host
	if s.AccessToken == "" {
		s.AccessToken = "tok"
	}
	if s.Expiry.IsZero() {
		s.Expiry = time.Now().Add(time.Hour)
	}
	if err := oidclogin.Save(&s); err != nil {
		t.Fatal(err)
	}
}

// result is one gl invocation.
type result struct {
	out, err string
	e        error
	sleeps   []time.Duration
}

type runOpts struct {
	env    map[string]string
	stream *http.Client
	// stopAfter > 0 ends the command (as Ctrl-C would) once it has printed
	// that many lines.
	stopAfter int
}

// lineStopper cancels once n lines have been written through it.
type lineStopper struct {
	w      io.Writer
	n      int
	cancel context.CancelFunc
}

func (l *lineStopper) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if l.n -= bytes.Count(p, []byte("\n")); l.n <= 0 {
		l.cancel()
	}
	return n, err
}

func run(t *testing.T, o runOpts, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var w io.Writer = &out
	if o.stopAfter > 0 {
		w = &lineStopper{w: &out, n: o.stopAfter, cancel: cancel}
	}
	// gl writes errors while gb's stderr is copied into the same buffer.
	a := newApp(w, &lockedWriter{w: &errOut})
	var res result
	a.sleep = func(d time.Duration) { res.sleeps = append(res.sleeps, d) }
	a.openBrowser = func(string) error { return nil }
	a.getenv = func(k string) string { return o.env[k] }
	if o.stream != nil {
		a.streamHC = o.stream
	}
	root := newRoot(a)
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	res.e = root.ExecuteContext(ctx)
	res.out, res.err = out.String(), errOut.String()
	return res
}

// lockedWriter serializes writes and, having no ReadFrom, keeps io.Copy from
// reading straight into the buffer under another goroutine's Write.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// fakeJWT is an unsigned token carrying claims; gl only ever displays them.
func fakeJWT(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

// fakeKeycloak serves discovery, the device grant and refresh.
type fakeKeycloak struct {
	srv          *httptest.Server
	mu           sync.Mutex
	deviceScope  string
	refreshes    int
	accessToken  string
	refreshReply func(w http.ResponseWriter)
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	k := &fakeKeycloak{accessToken: fakeJWT(map[string]any{"email": "ada@example.com"})}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                        k.srv.URL,
			"token_endpoint":                k.srv.URL + "/token",
			"device_authorization_endpoint": k.srv.URL + "/device",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		k.mu.Lock()
		k.deviceScope = r.PostForm.Get("scope")
		k.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev", "user_code": "ABCD-EFGH",
			"verification_uri": k.srv.URL + "/verify", "expires_in": 600, "interval": 1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("grant_type") == "refresh_token" {
			k.mu.Lock()
			k.refreshes++
			reply := k.refreshReply
			k.mu.Unlock()
			if reply != nil {
				reply(w)
				return
			}
			if r.PostForm.Get("refresh_token") != "refresh-1" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "renewed", "refresh_token": "refresh-2", "expires_in": 300})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": k.accessToken, "refresh_token": "refresh-1", "expires_in": 300})
	})
	k.srv = httptest.NewServer(mux)
	t.Cleanup(k.srv.Close)
	return k
}
