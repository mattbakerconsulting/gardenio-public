//go:build conformance

package main

// The conformance suite (Door D.11, task gd-oQdHmmwZFW; contract design
// gd-EWSKPH7pa5 §8 step 9; boundary gd-uYNbyW4a9Z §6 and §7): the contract's
// request fixtures and gl's own commands, replayed through the land door
// against a real kbeads with two lands, acorn and birch, asserting the
// cross-land matrix: a member of one land never sees, or changes, the other.
//
// The pieces, all started here:
//
//   - an OIDC issuer standing in for Keycloak: discovery and a JWKS, and
//     Keycloak-shaped access tokens minted in-process (groups, realm roles and
//     the per-land land-<name> audience, owner decision gd-zf6eceweED);
//   - `kd serve` itself (LANDS_CONFORMANCE_KD) on a real Postgres
//     (LANDS_CONFORMANCE_DATABASE_URL), with OIDC on, a static token and no
//     NATS, so every request takes the daemon's whole HTTP chain;
//   - the land door: what public-edge.yaml's `location ^~ /land/` does,
//     rule for rule (doorDenied, the allowed_groups check, the prefix
//     rewrite, X-Land-Door and the stripped identity headers). It records
//     every response it forwards, gl's included.
//
// Each land's member then replays every contract request fixture
// (../contract/fixtures/<Schema>/) on its verb and runs gl's commands
// in-process. Every answer must be the contract's own type, round-tripped
// strictly, and every recorded response must pass the contract's hygiene
// denylist and carry nothing of the other land.
//
// Run (CI: ci.yml job lands-conformance):
//
//	LANDS_CONFORMANCE_KD=/path/to/kd \
//	LANDS_CONFORMANCE_DATABASE_URL=postgres://... \
//	  GOWORK=off go test -tags conformance -run TestConformance -count=1 -v .
//
// The database must be empty and have the vector extension available; the
// daemon migrates it. LANDS_CONFORMANCE_LOG_DIR, when set, keeps the
// daemon's log there.

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/lands/contract/landsv1"
)

const (
	idempotencyHeader = "Idempotency-Key"

	confAudience = "gardenio-conformance"
	confRealm    = "/realms/conformance"
	confAZP      = "gardenio-cli" // gl's device grant
	roleMember   = "gardenio-project-member"

	landA = "acorn"
	landB = "birch"
)

var confLands = []string{landA, landB}

// ── The issuer ──────────────────────────────────────────────────────────────

// issuer is a Keycloak stand-in: discovery and a JWKS over plain http (the
// daemon's verifier takes any scheme), and RS256 tokens signed here.
type issuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	iss string
}

const issuerKID = "conformance-1"

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+confRealm+"/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                                is.iss,
			"authorization_endpoint":                is.iss + "/protocol/openid-connect/auth",
			"token_endpoint":                        is.iss + "/protocol/openid-connect/token",
			"jwks_uri":                              is.iss + "/protocol/openid-connect/certs",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET "+confRealm+"/protocol/openid-connect/certs", func(w http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		writeTestJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": issuerKID, "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	is.iss = is.srv.URL + confRealm
	return is
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// person is who a token is for.
type person struct {
	email  string
	roles  []string
	groups []string
	lands  []string // land-<name> audiences
}

func (is *issuer) mint(p person) string {
	now := time.Now().Unix()
	aud := []string{confAudience, "account"}
	for _, l := range p.lands {
		aud = append(aud, "land-"+l)
	}
	claims := map[string]any{
		"iss": is.iss, "sub": "sub-" + p.email, "aud": aud, "azp": confAZP, "typ": "Bearer",
		"iat": now, "nbf": now - 5, "exp": now + 3600,
		"scope": "openid email profile", "email": p.email, "email_verified": true,
		"preferred_username": p.email, "realm_access": map[string]any{"roles": p.roles},
		"groups": p.groups,
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	in := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": issuerKID}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, is.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// verify is the edge's bearer-JWT check (oauth2-proxy
// --skip-jwt-bearer-tokens): a token this issuer signed, not expired. It
// returns the token's email and groups.
func (is *issuer) verify(tok string) (email string, groups []string, ok bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", nil, false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&is.key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return "", nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", nil, false
	}
	var c struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
		Exp    int64    `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp < time.Now().Unix() {
		return "", nil, false
	}
	return c.Email, c.Groups, true
}

// ── The daemon ──────────────────────────────────────────────────────────────

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startDaemon runs `kd serve` on dsn with OIDC against is, and returns its
// base URL once it is live and verifying is's tokens.
func startDaemon(t *testing.T, kd, dsn string, is *issuer, adminTok string) string {
	t.Helper()
	logDir := os.Getenv("LANDS_CONFORMANCE_LOG_DIR")
	if logDir == "" {
		logDir = t.TempDir()
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "kd-serve.log")
	logf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	httpAddr := freeAddr(t)
	cmd := exec.Command(kd, "serve")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"BEADS_DATABASE_URL=" + dsn,
		"BEADS_HTTP_ADDR=" + httpAddr,
		"BEADS_GRPC_ADDR=" + freeAddr(t),
		"BEADS_METRICS_ADDR=" + freeAddr(t),
		"BEADS_RATE_LIMIT_ENABLED=false",
		"BEADS_AUTH_TOKEN=conformance-static-" + randHex(8),
		"BEADS_OIDC_ISSUER_URL=" + is.iss,
		"BEADS_OIDC_AUDIENCE=" + confAudience,
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("kd serve: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		_ = logf.Close()
		if t.Failed() {
			t.Logf("kd serve log (%s), last lines:\n%s", logPath, tail(logPath, 80))
		}
	})

	base := "http://" + httpAddr
	ready := func(path, tok string) bool {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	// Live first (migrations run before the listener), then the OIDC
	// verifier, which a retry loop may install after the listener is up.
	for _, step := range []struct{ path, tok, what string }{
		{"/v1/health/live", "", "live"},
		{"/v1/me", adminTok, "verifying the issuer's tokens"},
	} {
		deadline := time.Now().Add(120 * time.Second)
		for !ready(step.path, step.tok) {
			select {
			case err := <-exited:
				t.Fatalf("kd serve exited (%v) before it was %s; log:\n%s", err, step.what, tail(logPath, 80))
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("kd serve was not %s after 120s; log:\n%s", step.what, tail(logPath, 80))
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	return base
}

func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── The land door ───────────────────────────────────────────────────────────

// The edge's land door, public-edge.yaml `location ^~ /land/` (Door D.2,
// gd-h8nQExxbkD; X-Land-Door gd-eAfptUBh9Z). Kept rule for rule: a change
// there should change this too.
var (
	// map $request_uri $land_door_name: read off the raw request line.
	doorName = regexp.MustCompile(`^/land/([a-z][a-z0-9-]{0,62})/v1/`)
	// The daemon routes the door never forwards, matched on the decoded path.
	doorDenied = []*regexp.Regexp{
		regexp.MustCompile(`^/land/[a-z][a-z0-9-]{0,62}/v1/(bridge-tokens|fleet-tokens|auth|admin|hooks|embeddings)(/|$)`),
		regexp.MustCompile(`^/land/[a-z][a-z0-9-]{0,62}/v1/(events/stream|bus/events)(/|$)`),
	}
	doorForwards = regexp.MustCompile(`^/land/[a-z][a-z0-9-]{0,62}/v1/`)
	// proxy_set_header <name> "": nothing from the internet may name these.
	doorClearedHeaders = []string{
		"Cookie", "X-Auth-Request-Access-Token", "X-Actor", "X-Fleet", "X-System-Operator",
		"X-Owner-Override", "X-Operated-By", "X-Edge-Auth-Ts", "X-Edge-Auth-Sig",
		"X-Edge-Auth-Version", "X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups",
	}
)

// exchange is one response the door forwarded.
type exchange struct {
	door, email, method, uri string
	status                   int
	header                   http.Header
	body                     string
}

type door struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []exchange
}

func newDoor(t *testing.T, upstream string, is *issuer) *door {
	t.Helper()
	up, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	d := &door{}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = pr.In.Host
		},
		FlushInterval: -1, // the land feed: every frame as it is written
	}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/land/") {
			http.NotFound(w, r)
			return
		}
		for _, re := range doorDenied {
			if re.MatchString(path) {
				http.NotFound(w, r)
				return
			}
		}
		m := doorName.FindStringSubmatch(r.RequestURI)
		if !doorForwards.MatchString(path) || m == nil || !strings.HasPrefix(path, "/land/"+m[1]+"/") {
			http.NotFound(w, r)
			return
		}
		land := m[1]
		// auth_request /oauth2/auth-land: a valid bearer in the land's
		// members, owners or guests group, nobody else.
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		email, groups, ok := is.verify(tok)
		if !ok {
			http.Error(w, "401", http.StatusUnauthorized)
			return
		}
		g := "/project-" + land
		if !slices.Contains(groups, g) && !slices.Contains(groups, g+"/owners") && !slices.Contains(groups, g+"/guests") {
			http.Error(w, "403", http.StatusForbidden)
			return
		}
		// rewrite ^/land/[^/]+(/v1/.*)$ $1 break;
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(path, "/land/"+land)
		r2.URL.RawPath = ""
		for _, h := range doorClearedHeaders {
			r2.Header.Del(h)
		}
		r2.Header.Set("X-Land-Door", land)
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		proxy.ServeHTTP(rec, r2)
		d.mu.Lock()
		d.seen = append(d.seen, exchange{land, email, r.Method, r.RequestURI, rec.status, w.Header().Clone(), rec.body.String()})
		d.mu.Unlock()
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *door) exchanges() []exchange {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]exchange(nil), d.seen...)
}

// recorder tees a response's status and body.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ── The suite ───────────────────────────────────────────────────────────────

// land is one test land: what was seeded into it and what its member made.
type land struct {
	name   string
	marker string // in every seeded title: the other land's responses must never carry it
	member string // the member's bearer: groups /project-<name>, aud land-<name>
	owner  string // the owner's bearer: groups /project-<name>/owners, aud land-<name>
	email  string
	ticket string // seeded, stays open
	plan   string // seeded epic with the plan label
	// release is a seeded release, tag v0.1.0, running nowhere.
	release string
	// Seeded open decisions, by what they are for.
	decisions map[string]string
	mu        sync.Mutex
	own       map[string]bool // every id the land's members may see
}

func (l *land) owns(ids ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if id != "" {
			l.own[id] = true
		}
	}
}

func (l *land) ids() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.own))
	for id := range l.own {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

type suite struct {
	t      *testing.T
	is     *issuer
	daemon string
	door   *door
	admin  string
	lands  map[string]*land
}

// response is one answer.
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) String() string { return fmt.Sprintf("%d %s", r.status, bytes.TrimSpace(r.body)) }

func do(t *testing.T, method, target, tok, body string, hdr map[string]string) response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPost {
		req.Header.Set(idempotencyHeader, "conf-"+randHex(12))
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := confClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, b}
}

// confClient bounds every plain request, so a route that streams where it
// should refuse fails the suite instead of hanging it.
var confClient = &http.Client{Timeout: 30 * time.Second}

// via calls the daemon route path (from /v1) through door with tok.
func (s *suite) via(tok, doorLand, method, path, body string) response {
	s.t.Helper()
	return do(s.t, method, s.door.srv.URL+"/land/"+doorLand+path, tok, body, nil)
}

// api calls the land's contract route rest (after /lands/v1/lands/<land>)
// through the land's own door with its member's bearer.
func (s *suite) api(l *land, method, rest, body string) response {
	s.t.Helper()
	return s.via(l.member, l.name, method, "/v1/lands/v1/lands/"+l.name+rest, body)
}

// direct calls the daemon as the admin, past the door: seeding and checks.
func (s *suite) direct(method, path string, body any) response {
	s.t.Helper()
	var raw string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		raw = string(b)
	}
	return do(s.t, method, s.daemon+path, s.admin, raw, nil)
}

func (s *suite) createBead(body map[string]any) string {
	s.t.Helper()
	r := s.direct(http.MethodPost, "/v1/beads", body)
	if r.status != http.StatusCreated {
		s.t.Fatalf("seed %v: %s", body["title"], r)
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.body, &b); err != nil || b.ID == "" {
		s.t.Fatalf("seed %v: no id in %s", body["title"], r)
	}
	return b.ID
}

// beadStatus reads a bead's status as the admin.
func (s *suite) beadStatus(id string) string {
	s.t.Helper()
	r := s.direct(http.MethodGet, "/v1/beads/"+id, nil)
	var b struct {
		Status string `json:"status"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &b) != nil {
		s.t.Fatalf("read %s: %s", id, r)
	}
	return b.Status
}

// seedDecisions are the open decisions each land starts with, by use.
var seedDecisions = []string{"resolve-minimal", "resolve-with_note", "cancel", "comment", "plan", "gl"}

// decisionOptions carry the option ids the contract's request fixtures
// answer with (DecisionResolve/*.json, PlanAnswer/answer.json).
var decisionOptions = []map[string]string{
	{"id": "b", "label": "Keep the old logo"},
	{"id": "or-Opt0000001", "short": "Draw a new logo"},
}

func (s *suite) seed(name string) *land {
	s.t.Helper()
	l := &land{name: name, marker: "only-in-" + name + "-" + randHex(4), decisions: map[string]string{}, own: map[string]bool{}}
	l.email = "member@" + name + ".example"
	l.member = s.is.mint(person{email: l.email, roles: []string{roleMember}, groups: []string{"/project-" + name}, lands: []string{name}})
	l.owner = s.is.mint(person{email: "owner@" + name + ".example", roles: []string{roleMember}, groups: []string{"/project-" + name + "/owners"}, lands: []string{name}})
	// A provisioned land's release source (releases-design.md §4): semver
	// tags, one prod target.
	s.createBead(map[string]any{"title": name, "type": "project", "kind": "config", "fields": map[string]any{
		"release_tag_pattern": `^v\d+\.\d+\.\d+$`, "release_targets": []map[string]string{{"name": "prod", "kind": "land"}},
	}})
	labels := []string{"project:" + name}
	l.ticket = s.createBead(map[string]any{
		"title": "Seed ticket " + l.marker, "short_title": "Seed ticket", "type": "task", "kind": "issue",
		"priority": 2, "labels": labels, "description": "Seeded for the conformance suite, " + l.marker,
	})
	l.plan = s.createBead(map[string]any{
		"title": "Seed plan " + l.marker, "short_title": "Seed plan", "type": "epic", "kind": "issue",
		"priority": 2, "labels": append(slices.Clone(labels), "plan"),
	})
	for _, use := range seedDecisions {
		fields := map[string]any{"prompt": "Which logo for " + use + ", " + l.marker + "?", "options": decisionOptions}
		if use == "plan" {
			fields["requesting_plan"] = l.plan
		}
		l.decisions[use] = s.createBead(map[string]any{
			"title": "Which logo (" + use + ") " + l.marker, "short_title": "Which logo", "type": "decision", "kind": "data",
			"labels": labels, "fields": fields, "force": true,
		})
	}
	l.release = s.createRelease(name, "v0.1.0", "2026-10-18T09:00:00Z", []map[string]any{
		{"pr": 1, "title": "First page " + l.marker, "url": "https://github.com/" + name + "/shop/pull/1"},
	})
	l.owns(l.ticket, l.plan, l.release)
	for _, id := range l.decisions {
		l.owns(id)
	}
	return l
}

// createRelease records a release of land as github-bridge does when a tag
// is pushed: as a release writer (the admin), with its contents.
func (s *suite) createRelease(land, tag, cutAt string, contents []map[string]any) string {
	s.t.Helper()
	r := s.direct(http.MethodPost, "/v1/releases", map[string]any{
		"project": land, "tag": tag, "cut_at": cutAt, "source": "manual",
		"sha": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "contents": contents,
	})
	var b struct {
		ID string `json:"id"`
	}
	if r.status != http.StatusCreated || json.Unmarshal(r.body, &b) != nil || b.ID == "" {
		s.t.Fatalf("seed release %s %s: %s", land, tag, r)
	}
	return b.ID
}

func (s *suite) other(l *land) *land {
	for _, n := range confLands {
		if n != l.name {
			return s.lands[n]
		}
	}
	panic("one land")
}

// reports sends each DeploymentReport fixture as the land's owner; a member
// is refused. It runs after the scenarios: a tenant's report is stamped now,
// and the release_deployed scenario backdates a report on prod, which stays
// open only while nothing later has run there (deployRelease closes it at the
// since of the next row on the target). The full fixture's revision is one no
// scenario records: a helm revision on a target is one roll of one tag, so a
// revision recorded for another release (v2.1.0 holds 8) is a 409.
func (s *suite) reports(l *land) {
	names, reports := requestFixtures(s.t, "DeploymentReport")
	s.refused("reportDeployment as a member", s.api(l, "POST", "/releases/v0.1.0/deployments", reports[names[0]]), 403)
	for _, n := range names {
		var rel landsv1.Release
		if s.ok("reportDeployment "+n, s.via(l.owner, l.name, "POST", "/v1/lands/v1/lands/"+l.name+"/releases/v0.1.0/deployments", reports[n]), 200, &rel) && rel.Status != landsv1.Live {
			s.t.Errorf("reportDeployment %s: status %q, want live", n, rel.Status)
		}
	}
	s.refused("reportDeployment (undeclared target)", s.via(l.owner, l.name, "POST", "/v1/lands/v1/lands/"+l.name+"/releases/v0.1.0/deployments", `{"target":"staging"}`), 409)
}

// requestFixtures returns the contract's request fixtures for schema, by
// case name, in name order.
func requestFixtures(t *testing.T, schema string) ([]string, map[string]string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "contract", "fixtures", schema, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no contract fixtures for %s: %v", schema, err)
	}
	sort.Strings(paths)
	names := []string{}
	out := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		n := strings.TrimSuffix(filepath.Base(p), ".json")
		names = append(names, n)
		out[n] = string(b)
	}
	return names, out
}

// strictRoundTrip decodes raw into dst refusing unknown fields, and fails
// unless re-encoding gives the same document back: a field the contract does
// not have, or a required one missing, both show.
func strictRoundTrip(dst any, raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	out, err := json.Marshal(dst)
	if err != nil {
		return err
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		return err
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return err
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("round-trip changed the document:\n want %s\n  got %s", bytes.TrimSpace(raw), out)
	}
	return nil
}

// ok asserts r is a contract answer with status want, decoded into dst.
func (s *suite) ok(op string, r response, want int, dst any) bool {
	s.t.Helper()
	if r.status != want {
		s.t.Errorf("%s: %s, want %d", op, r, want)
		return false
	}
	if got := r.header.Get(landsv1.HeaderContract); got != landsv1.ContractVersion {
		s.t.Errorf("%s: %s = %q, want %q", op, landsv1.HeaderContract, got, landsv1.ContractVersion)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		s.t.Errorf("%s: Content-Type %q", op, ct)
	}
	if err := strictRoundTrip(dst, r.body); err != nil {
		s.t.Errorf("%s: not the contract's type: %v", op, err)
		return false
	}
	return true
}

// refused asserts r is a contract problem: one of the statuses, problem+json,
// the Problem schema.
func (s *suite) refused(what string, r response, statuses ...int) {
	s.t.Helper()
	if !slices.Contains(statuses, r.status) {
		s.t.Errorf("%s: %s, want one of %v", what, r, statuses)
		return
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		s.t.Errorf("%s: Content-Type %q, want application/problem+json: %s", what, ct, r)
		return
	}
	var p landsv1.Problem
	if err := strictRoundTrip(&p, r.body); err != nil {
		s.t.Errorf("%s: not a contract Problem: %v", what, err)
	}
}

// either accepts a contract answer (decoded into dst) or a contract problem:
// for verbs whose answer depends on what the land has (a budget, threads).
func (s *suite) either(op string, r response, want int, dst any) {
	s.t.Helper()
	if r.status >= 200 && r.status < 300 {
		s.ok(op, r, want, dst)
		return
	}
	s.refused(op, r, r.status)
}

func idsOf[T any](items []T, id func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, id(it))
	}
	return out
}

// onlyOwn asserts every id a list returned is the land's own.
func (s *suite) onlyOwn(op string, l *land, ids []string) {
	s.t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if !l.own[id] {
			s.t.Errorf("%s in %s lists %s, which is not the land's", op, l.name, id)
		}
	}
}

func TestConformance(t *testing.T) {
	kd, dsn := os.Getenv("LANDS_CONFORMANCE_KD"), os.Getenv("LANDS_CONFORMANCE_DATABASE_URL")
	if kd == "" || dsn == "" {
		t.Fatal("the conformance build tag needs LANDS_CONFORMANCE_KD (a kd binary) and LANDS_CONFORMANCE_DATABASE_URL (an empty Postgres with pgvector)")
	}
	useTempState(t)
	is := newIssuer(t)
	s := &suite{t: t, is: is, lands: map[string]*land{}}
	s.admin = is.mint(person{email: "admin@conformance.example", roles: []string{"gardenio-viewer", "gardenio-operator", "gardenio-admin"}})
	s.daemon = startDaemon(t, kd, dsn, is, s.admin)
	s.door = newDoor(t, s.daemon, is)
	for _, n := range confLands {
		s.lands[n] = s.seed(n)
	}

	for _, n := range confLands {
		t.Run("fixtures/"+n, func(t *testing.T) { s.sub(t).fixtures(s.lands[n]) })
	}
	for _, n := range confLands {
		t.Run("gl/"+n, func(t *testing.T) { s.sub(t).gl(s.lands[n]) })
	}
	for _, n := range confLands {
		t.Run("cross-land/"+n, func(t *testing.T) { s.sub(t).crossLand(s.lands[n]) })
	}
	t.Run("door", func(t *testing.T) { s.sub(t).doorTokens() })
	t.Run("feed", func(t *testing.T) { s.sub(t).feed() })
	// The scenario fixtures (scenarios_test.go), each seeded in each land.
	for _, n := range confLands {
		t.Run("scenarios/"+n, func(t *testing.T) { s.sub(t).scenarios(s.lands[n]) })
	}
	for _, n := range confLands {
		t.Run("reports/"+n, func(t *testing.T) { s.sub(t).reports(s.lands[n]) })
	}
	// Last: it reads every response the door forwarded above.
	t.Run("hygiene", func(t *testing.T) { s.sub(t).hygiene() })
}

// sub is s reporting to a subtest.
func (s *suite) sub(t *testing.T) *suite {
	c := *s
	c.t = t
	return &c
}

// fixtures replays every contract request fixture on its verb, as l's member
// through l's door, and checks every answer against the contract's types.
func (s *suite) fixtures(l *land) {
	var (
		lnd     landsv1.Land
		me      landsv1.Member
		plots   landsv1.PlotPage
		crew    landsv1.MemberPage
		budget  landsv1.Budget
		history landsv1.BudgetHistory
	)
	s.ok("getLand", s.api(l, "GET", "", ""), 200, &lnd)
	if lnd.Name != l.name {
		s.t.Errorf("getLand: name %q, want %q", lnd.Name, l.name)
	}
	s.ok("whoami", s.api(l, "GET", "/whoami", ""), 200, &me)
	s.ok("listPlots", s.api(l, "GET", "/plots", ""), 200, &plots)
	s.refused("getPlot (none)", s.api(l, "GET", "/plots/x", ""), 404)
	s.ok("listCrew", s.api(l, "GET", "/crew", ""), 200, &crew)
	s.either("getBudget", s.api(l, "GET", "/budget", ""), 200, &budget)
	s.either("getBudgetHistory", s.api(l, "GET", "/budget/history?days=7", ""), 200, &history)
	s.refused("getBudgetHistory (days out of range)", s.api(l, "GET", "/budget/history?days=31", ""), 400)
	// A member's own-machine agents (D.17): the routes reach the map through
	// the door. A fresh land has none.
	var agents landsv1.AgentPage
	s.ok("listAgents", s.api(l, "GET", "/agents", ""), 200, &agents)
	s.refused("getAgent (none)", s.api(l, "GET", "/agents/x", ""), 404)

	// Tickets: one per TicketCreate fixture, then every other ticket verb on them.
	// Tickets bear no arms yet (heraldry spec R6), so a fixture giving some is
	// refused, and makes nothing.
	names, creates := requestFixtures(s.t, "TicketCreate")
	var made []string
	for _, n := range names {
		if strings.Contains(creates[n], `"heraldry"`) {
			s.refused("createTicket "+n+" (arms on a ticket)", s.api(l, "POST", "/tickets", creates[n]), 400)
			continue
		}
		var tk landsv1.Ticket
		if s.ok("createTicket "+n, s.api(l, "POST", "/tickets", creates[n]), 201, &tk) {
			l.owns(tk.Id)
			made = append(made, tk.Id)
		}
	}
	if len(made) < 2 {
		s.t.Fatalf("createTicket made %d tickets, the rest of the replay needs 2", len(made))
	}
	// The same Idempotency-Key twice is one ticket.
	key := map[string]string{idempotencyHeader: "conf-once-" + randHex(8)}
	base := s.door.srv.URL + "/land/" + l.name + "/v1/lands/v1/lands/" + l.name
	var once, again landsv1.Ticket
	s.ok("createTicket (key)", do(s.t, "POST", base+"/tickets", l.member, creates[names[0]], key), 201, &once)
	r := do(s.t, "POST", base+"/tickets", l.member, creates[names[0]], key)
	if r.status >= 200 && r.status < 300 {
		_ = json.Unmarshal(r.body, &again)
	}
	if once.Id == "" || again.Id != once.Id {
		s.t.Errorf("createTicket twice with one Idempotency-Key: %q then %s", once.Id, r)
	}
	l.owns(once.Id)

	var page landsv1.TicketPage
	if s.ok("listTickets", s.api(l, "GET", "/tickets", ""), 200, &page) {
		got := idsOf(page.Items, func(t landsv1.Ticket) string { return t.Id })
		s.onlyOwn("listTickets", l, got)
		for _, id := range append([]string{l.ticket}, made...) {
			if !slices.Contains(got, id) {
				s.t.Errorf("listTickets does not list %s: %v", id, got)
			}
		}
	}
	s.ok("listTickets (filtered)", s.api(l, "GET", "/tickets?status=open&limit=1", ""), 200, &landsv1.TicketPage{})
	var found landsv1.TicketPage
	if s.ok("searchTickets", s.api(l, "GET", "/search/tickets?q="+url.QueryEscape(l.marker), ""), 200, &found) {
		s.onlyOwn("searchTickets", l, idsOf(found.Items, func(t landsv1.Ticket) string { return t.Id }))
	}
	var seeded landsv1.Ticket
	if s.ok("getTicket", s.api(l, "GET", "/tickets/"+l.ticket, ""), 200, &seeded) && seeded.Id != l.ticket {
		s.t.Errorf("getTicket: id %q, want %q", seeded.Id, l.ticket)
	}
	names, updates := requestFixtures(s.t, "TicketUpdate")
	for _, n := range names {
		s.ok("updateTicket "+n, s.api(l, "PATCH", "/tickets/"+made[0], updates[n]), 200, &landsv1.Ticket{})
	}
	_, comments := requestFixtures(s.t, "CommentCreate")
	for n, body := range comments {
		s.ok("commentTicket "+n, s.api(l, "POST", "/tickets/"+made[0]+"/comments", body), 201, &landsv1.Comment{})
	}
	names, closes := requestFixtures(s.t, "TicketClose")
	for i, n := range names {
		var tk landsv1.Ticket
		if s.ok("closeTicket "+n, s.api(l, "POST", "/tickets/"+made[i%len(made)]+"/close", closes[n]), 200, &tk) && tk.Status != landsv1.TicketStatusDone {
			s.t.Errorf("closeTicket %s: status %q", n, tk.Status)
		}
	}
	var reopened landsv1.Ticket
	if s.ok("reopenTicket", s.api(l, "POST", "/tickets/"+made[0]+"/reopen", ""), 200, &reopened) && reopened.Status == landsv1.TicketStatusDone {
		s.t.Errorf("reopenTicket: still %q", reopened.Status)
	}

	// Decisions.
	var dpage landsv1.DecisionPage
	if s.ok("listDecisions", s.api(l, "GET", "/decisions", ""), 200, &dpage) {
		got := idsOf(dpage.Items, func(d landsv1.Decision) string { return d.Id })
		s.onlyOwn("listDecisions", l, got)
		if !slices.Contains(got, l.decisions["comment"]) {
			s.t.Errorf("listDecisions does not list %s: %v", l.decisions["comment"], got)
		}
	}
	s.ok("getDecision", s.api(l, "GET", "/decisions/"+l.decisions["comment"], ""), 200, &landsv1.Decision{})
	// An evidence picture (1.0.0-beta.5): this daemon has no picture bucket
	// (COOP_S3_BUCKET), so the route reaches the map through the door and is
	// upstream_unavailable.
	s.refused("getDecisionEvidence (no picture bucket)", s.api(l, "GET", "/decisions/"+l.decisions["comment"]+"/evidence/"+strings.Repeat("0", 32), ""), 503)
	names, resolves := requestFixtures(s.t, "DecisionResolve")
	for _, n := range names {
		var d landsv1.Decision
		var in landsv1.DecisionResolve
		_ = json.Unmarshal([]byte(resolves[n]), &in)
		id := l.decisions["resolve-"+n]
		if id == "" {
			s.t.Fatalf("no seeded decision for DecisionResolve/%s.json: add it to seedDecisions", n)
		}
		if s.ok("resolveDecision "+n, s.api(l, "POST", "/decisions/"+id+"/resolve", resolves[n]), 200, &d) {
			if d.Status != landsv1.DecisionStatusResolved || d.Chosen == nil || *d.Chosen != in.OptionId {
				s.t.Errorf("resolveDecision %s: status %q chosen %v, want resolved %q", n, d.Status, d.Chosen, in.OptionId)
			}
		}
	}
	_, cancels := requestFixtures(s.t, "DecisionCancel")
	for n, body := range cancels {
		var d landsv1.Decision
		if s.ok("cancelDecision "+n, s.api(l, "POST", "/decisions/"+l.decisions["cancel"]+"/cancel", body), 200, &d) && d.Status != landsv1.DecisionStatusCancelled {
			s.t.Errorf("cancelDecision: status %q", d.Status)
		}
	}
	for n, body := range comments {
		s.ok("commentDecision "+n, s.api(l, "POST", "/decisions/"+l.decisions["comment"]+"/comments", body), 201, &landsv1.Comment{})
	}

	// Escalations.
	_, escs := requestFixtures(s.t, "EscalationCreate")
	for n, body := range escs {
		var e landsv1.Escalation
		if s.ok("createEscalation "+n, s.api(l, "POST", "/escalations", body), 201, &e) {
			l.owns(e.Id)
			s.ok("getEscalation", s.api(l, "GET", "/escalations/"+e.Id, ""), 200, &landsv1.Escalation{})
		}
	}
	var epage landsv1.EscalationPage
	if s.ok("listEscalations", s.api(l, "GET", "/escalations", ""), 200, &epage) {
		s.onlyOwn("listEscalations", l, idsOf(epage.Items, func(e landsv1.Escalation) string { return e.Id }))
	}

	// Plans: the PlanAnswer fixture's decision_id is rebound to the seeded
	// plan's question; everything else is sent as the fixture has it.
	var ppage landsv1.PlanPage
	if s.ok("listPlans", s.api(l, "GET", "/plans", ""), 200, &ppage) {
		got := idsOf(ppage.Items, func(p landsv1.Plan) string { return p.Id })
		s.onlyOwn("listPlans", l, got)
		if !slices.Contains(got, l.plan) {
			s.t.Errorf("listPlans does not list %s: %v", l.plan, got)
		}
	}
	s.ok("getPlan", s.api(l, "GET", "/plans/"+l.plan, ""), 200, &landsv1.Plan{})
	_, answers := requestFixtures(s.t, "PlanAnswer")
	for n, body := range answers {
		var in map[string]any
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			s.t.Fatal(err)
		}
		in["decision_id"] = l.decisions["plan"]
		b, _ := json.Marshal(in)
		var d landsv1.Decision
		if s.ok("answerPlan "+n, s.api(l, "POST", "/plans/"+l.plan+"/answer", string(b)), 200, &d) && d.Status != landsv1.DecisionStatusResolved {
			s.t.Errorf("answerPlan: status %q", d.Status)
		}
	}

	// Releases: read by tag and by id; only the land's owner reports a
	// deployment, each DeploymentReport fixture in turn.
	var rpage landsv1.ReleasePage
	if s.ok("listReleases", s.api(l, "GET", "/releases", ""), 200, &rpage) {
		got := idsOf(rpage.Items, func(r landsv1.Release) string { return r.Id })
		s.onlyOwn("listReleases", l, got)
		if !slices.Contains(got, l.release) {
			s.t.Errorf("listReleases does not list %s: %v", l.release, got)
		}
	}
	var byTag, byID landsv1.Release
	if s.ok("getRelease (tag)", s.api(l, "GET", "/releases/v0.1.0", ""), 200, &byTag) && byTag.Id != l.release {
		s.t.Errorf("getRelease v0.1.0: id %q, want %q", byTag.Id, l.release)
	}
	s.ok("getRelease (id)", s.api(l, "GET", "/releases/"+l.release, ""), 200, &byID)
	s.refused("getRelease (none)", s.api(l, "GET", "/releases/v9.9.9", ""), 404)
	// The owner's reports run in reports, after the scenarios.

	// Threads wait on D.9's owner question; the answer must still be the
	// contract's, whichever it is.
	s.either("listThreads", s.api(l, "GET", "/threads", ""), 200, &landsv1.ThreadPage{})
	_, msgs := requestFixtures(s.t, "MessageCreate")
	for n, body := range msgs {
		r := s.api(l, "POST", "/threads/x/messages", body)
		s.refused("postMessage "+n+" (no such thread)", r, 404, 403)
	}
}

// gl drives the tenant CLI, in-process, through l's door.
func (s *suite) gl(l *land) {
	signIn(s.t, l.name, host, oidclogin.Session{AccessToken: l.member, Expiry: time.Now().Add(time.Hour)})
	edge := s.door.srv.URL
	gl := func(dst any, args ...string) string {
		s.t.Helper()
		full := append([]string{"--edge", edge, "--land", l.name}, args...)
		if dst != nil {
			full = append(full, "--json")
		}
		r := run(s.t, runOpts{}, full...)
		if r.e != nil {
			s.t.Fatalf("gl %s: %v\n%s", strings.Join(args, " "), r.e, r.err)
		}
		if strings.Contains(r.err, "this gl was built for") {
			s.t.Errorf("gl %s warned of contract skew: %s", strings.Join(args, " "), r.err)
		}
		if dst != nil {
			if err := json.Unmarshal([]byte(r.out), dst); err != nil {
				s.t.Fatalf("gl %s --json: %v\n%s", strings.Join(args, " "), err, r.out)
			}
		}
		return r.out
	}

	var me landsv1.Member
	gl(&me, "whoami")
	var tk landsv1.Ticket
	gl(&tk, "ticket", "open", "--kind", "bug", "--body", "Opened by gl, "+l.marker, "gl", "ticket", l.marker)
	if tk.Id == "" {
		s.t.Fatal("gl ticket open: no id")
	}
	l.owns(tk.Id)
	var page landsv1.TicketPage
	gl(&page, "ticket", "list")
	got := idsOf(page.Items, func(t landsv1.Ticket) string { return t.Id })
	s.onlyOwn("gl ticket list", l, got)
	if !slices.Contains(got, tk.Id) {
		s.t.Errorf("gl ticket list does not list %s", tk.Id)
	}
	if out := gl(nil, "ticket", "show", tk.Id); !strings.Contains(out, l.marker) {
		s.t.Errorf("gl ticket show does not show the title:\n%s", out)
	}
	gl(nil, "ticket", "comment", tk.Id, "A", "comment", "from", "gl")
	var closed landsv1.Ticket
	if gl(&closed, "ticket", "close", tk.Id, "--reason", "Done by gl"); closed.Status != landsv1.TicketStatusDone {
		s.t.Errorf("gl ticket close: status %q", closed.Status)
	}
	gl(nil, "ticket", "reopen", tk.Id)

	var dpage landsv1.DecisionPage
	gl(&dpage, "decision", "list")
	s.onlyOwn("gl decision list", l, idsOf(dpage.Items, func(d landsv1.Decision) string { return d.Id }))
	var d landsv1.Decision
	gl(&d, "decision", "resolve", l.decisions["gl"], "--option", decisionOptions[0]["label"], "--note", "From gl")
	if d.Status != landsv1.DecisionStatusResolved || d.Chosen == nil || *d.Chosen != decisionOptions[0]["id"] {
		s.t.Errorf("gl decision resolve: status %q chosen %v", d.Status, d.Chosen)
	}

	var e landsv1.Escalation
	gl(&e, "escalation", "create", "--title", "Connect the payment account", "--reason", "Checkout needs it, "+l.marker)
	l.owns(e.Id)
	var epage landsv1.EscalationPage
	gl(&epage, "escalation", "list")
	s.onlyOwn("gl escalation list", l, idsOf(epage.Items, func(e landsv1.Escalation) string { return e.Id }))

	var ppage landsv1.PlanPage
	gl(&ppage, "plan", "list")
	s.onlyOwn("gl plan list", l, idsOf(ppage.Items, func(p landsv1.Plan) string { return p.Id }))
	gl(nil, "plan", "show", l.plan)

	// A sign-in for the other land is never sent through this one's door.
	o := s.other(l)
	r := run(s.t, runOpts{}, "--edge", edge, "--land", o.name, "ticket", "show", l.ticket)
	if r.e == nil {
		s.t.Errorf("gl --land %s ticket show %s (a ticket of %s) succeeded:\n%s", o.name, l.ticket, l.name, r.out)
	}
}

// crossLand: l's member, through l's door and every other way the door
// leaves open, never reads or changes the other land.
func (s *suite) crossLand(l *land) {
	o := s.other(l)
	mine := "/v1/lands/v1/lands/" + l.name
	theirs := "/v1/lands/v1/lands/" + o.name
	notFound := []int{403, 404}

	// The other land by name, through this land's door.
	for _, rest := range []string{"", "/tickets", "/tickets/" + o.ticket, "/decisions", "/decisions/" + o.decisions["comment"] + "/evidence/" + strings.Repeat("0", 32), "/plans", "/escalations", "/crew", "/whoami", "/events"} {
		s.refused("GET "+theirs+rest+" via "+l.name, s.via(l.member, l.name, "GET", theirs+rest, ""), notFound...)
	}
	// The other land's objects under this land's name.
	for what, id := range map[string]string{"tickets": o.ticket, "decisions": o.decisions["comment"], "plans": o.plan} {
		s.refused("GET "+what+"/"+id+" of "+o.name+" in "+l.name, s.via(l.member, l.name, "GET", mine+"/"+what+"/"+id, ""), notFound...)
	}
	_, comments := requestFixtures(s.t, "CommentCreate")
	_, resolves := requestFixtures(s.t, "DecisionResolve")
	_, closes := requestFixtures(s.t, "TicketClose")
	for _, body := range comments {
		s.refused("comment on "+o.name+"'s ticket", s.via(l.member, l.name, "POST", mine+"/tickets/"+o.ticket+"/comments", body), notFound...)
		s.refused("comment on "+o.name+"'s ticket under its name", s.via(l.member, l.name, "POST", theirs+"/tickets/"+o.ticket+"/comments", body), notFound...)
	}
	for _, body := range closes {
		s.refused("close "+o.name+"'s ticket", s.via(l.member, l.name, "POST", mine+"/tickets/"+o.ticket+"/close", body), notFound...)
	}
	for _, body := range resolves {
		s.refused("resolve "+o.name+"'s decision", s.via(l.member, l.name, "POST", mine+"/decisions/"+o.decisions["comment"]+"/resolve", body), notFound...)
	}
	s.refused("update "+o.name+"'s ticket", s.via(l.member, l.name, "PATCH", mine+"/tickets/"+o.ticket, `{"title":"taken"}`), notFound...)
	s.refused("read "+o.name+"'s release in "+l.name, s.via(l.member, l.name, "GET", mine+"/releases/"+o.release, ""), notFound...)
	s.refused("report "+o.name+"'s release as "+l.name+"'s owner", s.via(l.owner, l.name, "POST", mine+"/releases/"+o.release+"/deployments", `{"target":"prod"}`), notFound...)
	s.refused("report "+o.name+"'s release under its name", s.via(l.owner, l.name, "POST", theirs+"/releases/v0.1.0/deployments", `{"target":"prod"}`), notFound...)
	s.refused("open a ticket in "+o.name, s.via(l.member, l.name, "POST", theirs+"/tickets", `{"kind":"task","title":"in the other land"}`), notFound...)

	// The door forwards the daemon's own /v1 routes too (that is how kd and
	// gb reach a land): they must be just as narrow.
	for _, q := range []string{"labels=project:" + o.name, "project=" + o.name, "q=" + o.marker} {
		r := s.via(l.member, l.name, "GET", "/v1/beads?limit=200&"+q, "")
		if r.status == 200 && (strings.Contains(string(r.body), o.marker) || strings.Contains(string(r.body), o.ticket)) {
			s.t.Errorf("GET /v1/beads?%s via %s shows %s's beads", q, l.name, o.name)
		}
	}
	for _, id := range []string{o.ticket, o.plan, o.decisions["comment"]} {
		r := s.via(l.member, l.name, "GET", "/v1/beads/"+id, "")
		if r.status == 200 || strings.Contains(string(r.body), o.marker) {
			s.t.Errorf("GET /v1/beads/%s (%s's) via %s: %d", id, o.name, l.name, r.status)
		}
		if r := s.via(l.member, l.name, "POST", "/v1/beads/"+id+"/comments", `{"text":"from the other land"}`); r.status < 400 {
			s.t.Errorf("POST /v1/beads/%s/comments (%s's) via %s: %d", id, o.name, l.name, r.status)
		}
	}
	if r := s.via(l.member, l.name, "POST", "/v1/decisions/"+o.decisions["comment"]+"/resolve", `{"selected_option":"b"}`); r.status < 400 {
		s.t.Errorf("POST /v1/decisions/%s/resolve (%s's) via %s: %d", o.decisions["comment"], o.name, l.name, r.status)
	}

	// This land's member at the other land's door: the edge's group check.
	if r := s.via(l.member, o.name, "GET", theirs+"/tickets", ""); r.status != 403 {
		s.t.Errorf("%s's member at %s's door: %d, want 403", l.name, o.name, r.status)
	}

	// A member of both lands, signed in for this one: through this door it
	// is this land only; at the other land's door kbeads refuses the token
	// (minted for another land), though the edge's group check admits it.
	both := s.is.mint(person{email: "both-" + l.name + "@conformance.example", roles: []string{roleMember},
		groups: []string{"/project-" + l.name, "/project-" + o.name}, lands: []string{l.name}})
	var page landsv1.TicketPage
	if s.ok("listTickets as a member of both", s.via(both, l.name, "GET", mine+"/tickets", ""), 200, &page) {
		s.onlyOwn("listTickets as a member of both", l, idsOf(page.Items, func(t landsv1.Ticket) string { return t.Id }))
	}
	s.refused("a member of both reads "+o.name+" via "+l.name, s.via(both, l.name, "GET", theirs+"/tickets", ""), notFound...)
	r := s.via(both, l.name, "GET", "/v1/beads?limit=200&labels=project:"+o.name, "")
	if r.status == 200 && strings.Contains(string(r.body), o.marker) {
		s.t.Errorf("a member of both, signed in for %s, lists %s's beads through %s's door", l.name, o.name, l.name)
	}
	if r := s.via(both, o.name, "GET", theirs+"/tickets", ""); r.status != 403 {
		s.t.Errorf("a token minted for %s at %s's door: %d, want 403: %s", l.name, o.name, r.status, r)
	}

	// Nothing above changed the other land.
	if st := s.beadStatus(o.ticket); st != "open" {
		s.t.Errorf("%s's seeded ticket is %q after %s's attempts", o.name, st, l.name)
	}
	if st := s.beadStatus(o.decisions["comment"]); st != "open" {
		s.t.Errorf("%s's decision is %q after %s's attempts", o.name, st, l.name)
	}
	r = s.direct("GET", "/v1/beads/"+o.ticket+"/comments", nil)
	if strings.Contains(string(r.body), "other land") {
		s.t.Errorf("%s's ticket carries a comment from %s: %s", o.name, l.name, r)
	}
}

// doorTokens: what the land door turns away whatever the path.
func (s *suite) doorTokens() {
	l := s.lands[landA]
	path := "/v1/lands/v1/lands/" + l.name + "/tickets"
	for _, c := range []struct {
		what string
		tok  string
		want int
	}{
		{"no bearer", "", 401},
		{"a token from another issuer", fakeJWT(map[string]any{"email": l.email, "groups": []string{"/project-" + l.name}}), 401},
		{"a token minted for two lands", s.is.mint(person{email: "two@conformance.example", roles: []string{roleMember},
			groups: []string{"/project-" + landA, "/project-" + landB}, lands: []string{landA, landB}}), 403},
		{"a member's token minted for no land", s.is.mint(person{email: "noaud@conformance.example", roles: []string{roleMember},
			groups: []string{"/project-" + l.name}}), 403},
		{"the admin's token (no land group)", s.admin, 403},
	} {
		if r := s.via(c.tok, l.name, "GET", path, ""); r.status != c.want {
			s.t.Errorf("%s at %s's door: %s, want %d", c.what, l.name, r, c.want)
		}
	}
	// The daemon routes the door never forwards.
	for _, p := range []string{"/v1/admin/x", "/v1/auth/token", "/v1/fleet-tokens", "/v1/bridge-tokens", "/v1/hooks/x", "/v1/embeddings", "/v1/events/stream", "/v1/bus/events"} {
		if r := s.via(l.member, l.name, "GET", p, ""); r.status != 404 {
			s.t.Errorf("GET /land/%s%s: %d, want 404", l.name, p, r.status)
		}
	}
	// X-Land-Door is the edge's: one the client sends is replaced.
	r := do(s.t, "GET", s.door.srv.URL+"/land/"+l.name+path, l.member, "", map[string]string{"X-Land-Door": landB})
	s.ok("listTickets with a forged X-Land-Door", r, 200, &landsv1.TicketPage{})
}

// feed: each land's event stream carries its own land's changes and never
// the other's.
func (s *suite) feed() {
	type stream struct {
		mu    sync.Mutex
		buf   strings.Builder
		resp  *http.Response
		ready chan struct{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streams := map[string]*stream{}
	for _, n := range confLands {
		l := s.lands[n]
		req, _ := http.NewRequestWithContext(ctx, "GET", s.door.srv.URL+"/land/"+n+"/v1/lands/v1/lands/"+n+"/events", nil)
		req.Header.Set("Authorization", "Bearer "+l.member)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			s.t.Fatalf("feed %s: %v", n, err)
		}
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			s.t.Fatalf("feed %s: %d %s %s", n, resp.StatusCode, resp.Header.Get("Content-Type"), b)
		}
		st := &stream{resp: resp}
		streams[n] = st
		go func() {
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				st.mu.Lock()
				st.buf.WriteString(sc.Text() + "\n")
				st.mu.Unlock()
			}
		}()
	}
	// One new ticket in each land, by its member.
	made := map[string]string{}
	for _, n := range confLands {
		l := s.lands[n]
		var tk landsv1.Ticket
		if !s.ok("createTicket (feed)", s.api(l, "POST", "/tickets", `{"kind":"task","title":"Feed ticket `+l.marker+`"}`), 201, &tk) {
			s.t.FailNow()
		}
		l.owns(tk.Id)
		made[n] = tk.Id
	}
	deadline := time.Now().Add(30 * time.Second)
	for _, n := range confLands {
		st := streams[n]
		for {
			st.mu.Lock()
			got := st.buf.String()
			st.mu.Unlock()
			if strings.Contains(got, made[n]) {
				break
			}
			if time.Now().After(deadline) {
				s.t.Errorf("feed %s never carried its own new ticket %s; it read:\n%s", n, made[n], got)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// Give a stray frame for the other land time to arrive, then look.
	time.Sleep(2 * time.Second)
	cancel()
	for _, n := range confLands {
		st := streams[n]
		o := s.other(s.lands[n])
		st.mu.Lock()
		got := st.buf.String()
		st.mu.Unlock()
		if strings.Contains(got, made[o.name]) || strings.Contains(got, o.marker) {
			s.t.Errorf("feed %s carried %s's ticket:\n%s", n, o.name, got)
		}
		for _, line := range strings.Split(got, "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok && !json.Valid([]byte(data)) {
				s.t.Errorf("feed %s: data is not JSON: %s", n, data)
			}
			if typ, ok := strings.CutPrefix(line, "event: "); ok && !landsv1.EventType(typ).Valid() {
				s.t.Errorf("feed %s: event type %q is not the contract's", n, typ)
			}
		}
	}
}

// ── Hygiene ─────────────────────────────────────────────────────────────────

// mintedCorrelationID is a correlation_id as the map mints it, 24 random hex
// digits, which can spell "bead" by chance; kbeads/internal/landmap's
// hygiene_test.go sets the same shape aside. Any other value is checked.
var mintedCorrelationID = regexp.MustCompile(`"correlation_id":"[0-9a-f]{24}"`)

// hygiene runs over every response the door forwarded: the contract's
// denylist on bodies and headers, and nothing of the other land.
func (s *suite) hygiene() {
	rules := loadDenylist(s.t)
	byEmail := map[string]*land{}
	for _, l := range s.lands {
		byEmail[l.email] = l
	}
	seen := s.door.exchanges()
	if len(seen) < 50 {
		s.t.Fatalf("the door forwarded %d responses; the suite above should have sent far more", len(seen))
	}
	checked := 0
	for _, x := range seen {
		viewer := s.lands[x.door]
		if l, ok := byEmail[x.email]; ok {
			viewer = l
		}
		allowed := map[string]bool{}
		for _, id := range viewer.ids() {
			allowed[id] = true
		}
		// An id the caller put in the request may come back in its answer.
		for _, r := range rules {
			if r.re != nil {
				for _, m := range r.re.FindAllString(x.uri, -1) {
					allowed[m] = true
				}
			}
		}
		o := s.other(viewer)
		where := fmt.Sprintf("%s %s (as %s, %d)", x.method, x.uri, x.email, x.status)
		var hdr strings.Builder
		for k, vs := range x.header {
			fmt.Fprintf(&hdr, "%s: %s\n", k, strings.Join(vs, ", "))
		}
		// The daemon's own /v1 routes are kbeads' vocabulary by design (kd
		// and gb speak it); only the Lands API is held to the denylist.
		if strings.Contains(x.uri, "/v1/lands/v1/") {
			checked++
			if f := findings(rules, mintedCorrelationID.ReplaceAllString(x.body, `"correlation_id":"minted"`), allowed); len(f) > 0 {
				s.t.Errorf("%s: body breaks the denylist: %v\n%s", where, f, x.body)
			}
			if f := findings(rules, hdr.String(), allowed); len(f) > 0 {
				s.t.Errorf("%s: headers break the denylist: %v\n%s", where, f, hdr.String())
			}
		}
		if strings.Contains(x.body, o.marker) {
			s.t.Errorf("%s: carries %s's marker\n%s", where, o.name, x.body)
		}
		for _, id := range o.ids() {
			if strings.Contains(x.body, id) && !strings.Contains(x.uri, id) {
				s.t.Errorf("%s: carries %s's id %s\n%s", where, o.name, id, x.body)
			}
		}
	}
	if checked == 0 {
		s.t.Fatal("no Lands API response reached the hygiene check")
	}
	s.t.Logf("hygiene: %d responses through the door, %d Lands API answers held to %d denylist rules", len(seen), checked, len(rules))
}
