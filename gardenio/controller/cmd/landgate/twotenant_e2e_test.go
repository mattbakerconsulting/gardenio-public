//go:build e2e

package main

// The two-tenant mutual-invisibility e2e (Lands boundary gd-uYNbyW4a9Z §6
// step 11, the live twin of 10.6's cross-land matrix): two lands, one tenant
// each, a landgate per land in front of a real `kd serve` on Postgres that
// trusts a Keycloak-shaped test issuer. Each tenant works their own land
// through their own landgate, and neither ever reads, changes or lands a
// bead in the other's, through either gate or straight at the daemon.
//
// Needs LANDGATE_E2E_KD (a kd binary) and LANDGATE_E2E_DATABASE_URL (a
// Postgres with the vector extension). Without them it skips, unless
// LANDGATE_E2E_REQUIRED=1, which e2e.yml's `landgate` job sets so a missing
// rig fails instead of passing silently.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tenant struct {
	land  string
	token string
	gate  *httptest.Server
}

func TestTwoTenantMutualInvisibility(t *testing.T) {
	kd, dbURL := os.Getenv("LANDGATE_E2E_KD"), os.Getenv("LANDGATE_E2E_DATABASE_URL")
	if kd == "" || dbURL == "" {
		if os.Getenv("LANDGATE_E2E_REQUIRED") == "1" {
			t.Fatal("LANDGATE_E2E_REQUIRED=1 but LANDGATE_E2E_KD or LANDGATE_E2E_DATABASE_URL is unset")
		}
		t.Skip("set LANDGATE_E2E_KD and LANDGATE_E2E_DATABASE_URL to run the two-tenant e2e")
	}

	iss := newTestIssuer(t)
	daemon := startKdServe(t, kd, dbURL, iss)
	auth := newTestAuth(t, iss)
	up, err := url.Parse(daemon)
	if err != nil {
		t.Fatal(err)
	}
	suffix := randHex(t)
	gateFor := func(land string) *httptest.Server {
		gw := httptest.NewServer(newGate(land, up, auth.Middleware, slog.New(slog.DiscardHandler)).handler(auth.Ready))
		t.Cleanup(gw.Close)
		return gw
	}
	// Distinct land names per run: the database outlives a local re-run.
	member := []string{"gardenio-project-member"}
	mk := func(land, email string) tenant {
		return tenant{land: land, gate: gateFor(land), token: iss.mint(t, map[string]any{
			"email":        email,
			"realm_access": map[string]any{"roles": member},
			"groups":       []string{"/lands-tenants", "/project-" + land},
			"aud":          []string{testAudience, landAudiencePrefix + land},
		})}
	}
	a := mk("landa-"+suffix, "ana-"+suffix+"@example.com")
	b := mk("landb-"+suffix, "bo-"+suffix+"@example.com")

	// Each tenant opens a bead in their own land through their own gate.
	secret := map[string]string{a.land: "a-secret-" + suffix, b.land: "b-secret-" + suffix}
	ids := map[string]string{}
	for _, tn := range []tenant{a, b} {
		code, body := call(t, tn.gate.URL, tn.token, http.MethodPost, "/v1/beads", map[string]any{
			"title": secret[tn.land], "type": "task", "labels": []string{"project:" + tn.land},
		})
		if code/100 != 2 {
			t.Fatalf("%s: create own bead: %d %s", tn.land, code, body)
		}
		var bead struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(body), &bead); err != nil || bead.ID == "" {
			t.Fatalf("%s: create response has no id: %s", tn.land, body)
		}
		ids[tn.land] = bead.ID
	}

	for _, pair := range [][2]tenant{{a, b}, {b, a}} {
		me, other := pair[0], pair[1]
		theirs, theirSecret := ids[other.land], secret[other.land]
		t.Run(me.land+" sees own land", func(t *testing.T) {
			code, body := call(t, me.gate.URL, me.token, http.MethodGet, "/v1/beads/"+ids[me.land], nil)
			if code != http.StatusOK || !strings.Contains(body, secret[me.land]) {
				t.Errorf("get own bead: %d %s", code, body)
			}
			code, body = call(t, me.gate.URL, me.token, http.MethodGet, "/v1/beads?limit=500", nil)
			if code != http.StatusOK || !strings.Contains(body, ids[me.land]) {
				t.Errorf("list: %d, own bead missing", code)
			}
			if strings.Contains(body, theirs) || strings.Contains(body, theirSecret) {
				t.Errorf("list through own gate shows the other land's bead")
			}
		})

		// Through my own gate (wall one passes me; kbeads must refuse) and
		// straight at the daemon (no landgate at all): the other land's bead
		// is never read, changed or reached through a sub-resource.
		for _, door := range []struct{ name, base string }{{"own gate", me.gate.URL}, {"daemon direct", daemon}} {
			t.Run(me.land+" cannot reach the other land via "+door.name, func(t *testing.T) {
				for _, req := range []struct{ method, path string }{
					{http.MethodGet, "/v1/beads/" + theirs},
					{http.MethodGet, "/v1/beads/" + theirs + "/comments"},
					{http.MethodGet, "/v1/beads/" + theirs + "/events"},
					{http.MethodGet, "/v1/beads?limit=500&labels=project:" + other.land},
				} {
					code, body := call(t, door.base, me.token, req.method, req.path, nil)
					if strings.Contains(body, theirSecret) {
						t.Errorf("%s %s: %d leaks the other land's title", req.method, req.path, code)
					}
					if strings.Contains(req.path, theirs) && code/100 == 2 {
						t.Errorf("%s %s: %d, want a refusal", req.method, req.path, code)
					}
				}
				for _, w := range []struct {
					method, path string
					body         any
				}{
					{http.MethodPatch, "/v1/beads/" + theirs, map[string]any{"title": "pwned"}},
					{http.MethodPost, "/v1/beads/" + theirs + "/close", map[string]any{}},
					{http.MethodPost, "/v1/beads/" + theirs + "/comments", map[string]any{"text": "pwned"}},
					{http.MethodPost, "/v1/beads", map[string]any{"title": "pwned", "type": "task", "labels": []string{"project:" + other.land}}},
				} {
					if code, body := call(t, door.base, me.token, w.method, w.path, w.body); code/100 == 2 {
						t.Errorf("%s %s: %d %s, want a refusal", w.method, w.path, code, body)
					}
				}
			})
		}

		t.Run(me.land+" is refused at the other land's gate", func(t *testing.T) {
			for _, p := range []string{"/v1/beads/" + theirs, "/v1/beads/" + ids[me.land], "/v1/beads?limit=500"} {
				if code, body := call(t, other.gate.URL, me.token, http.MethodGet, p, nil); code != http.StatusForbidden {
					t.Errorf("GET %s at the other gate: %d %s, want 403", p, code, body)
				}
			}
		})
	}

	// A user in both lands (gd-eAfptUBh9Z): a token minted for land A reaches
	// A and nothing of B, through A's gate, at B's gate or straight at the
	// daemon, and a token minted for B is refused at A's gate.
	t.Run("a two-land user's token reaches only the land it was minted for", func(t *testing.T) {
		both := func(land string) string {
			return iss.mint(t, map[string]any{
				"email":        "two-" + suffix + "@example.com",
				"realm_access": map[string]any{"roles": member},
				"groups":       []string{"/lands-tenants", "/project-" + a.land, "/project-" + b.land},
				"aud":          []string{testAudience, landAudiencePrefix + land},
			})
		}
		forA, forB := both(a.land), both(b.land)
		if code, body := call(t, a.gate.URL, forA, http.MethodGet, "/v1/beads/"+ids[a.land], nil); code != http.StatusOK {
			t.Errorf("A token at A's gate, A's bead: %d %s", code, body)
		}
		for _, base := range []string{a.gate.URL, daemon} {
			for _, p := range []string{
				"/v1/beads/" + ids[b.land],
				"/v1/beads?limit=500&project=" + b.land,
				"/v1/beads?limit=500&labels=project:" + b.land,
				"/v1/lands/" + b.land + "/members",
			} {
				code, body := call(t, base, forA, http.MethodGet, p, nil)
				if strings.Contains(body, secret[b.land]) || (strings.Contains(p, ids[b.land]) && code/100 == 2) || (strings.Contains(p, "/lands/") && code/100 == 2) {
					t.Errorf("A token GET %s via %s: %d reaches land B", p, base, code)
				}
			}
			if code, body := call(t, base, forA, http.MethodPost, "/v1/beads", map[string]any{"title": "pwned", "type": "task", "labels": []string{"project:" + b.land}}); code/100 == 2 {
				t.Errorf("A token create in B via %s: %d %s", base, code, body)
			}
		}
		if code, _ := call(t, b.gate.URL, forA, http.MethodGet, "/v1/beads/"+ids[b.land], nil); code != http.StatusForbidden {
			t.Errorf("A token at B's gate: %d, want 403", code)
		}
		if code, _ := call(t, a.gate.URL, forB, http.MethodGet, "/v1/beads/"+ids[a.land], nil); code != http.StatusForbidden {
			t.Errorf("B token at A's gate: %d, want 403", code)
		}
	})

	t.Run("the other land's bead is untouched", func(t *testing.T) {
		for _, tn := range []tenant{a, b} {
			code, body := call(t, tn.gate.URL, tn.token, http.MethodGet, "/v1/beads/"+ids[tn.land], nil)
			if code != http.StatusOK || !strings.Contains(body, secret[tn.land]) || strings.Contains(body, "pwned") {
				t.Errorf("%s: own bead after the other tenant's attempts: %d %s", tn.land, code, body)
			}
			if strings.Contains(body, `"status":"closed"`) {
				t.Errorf("%s: own bead was closed by the other tenant", tn.land)
			}
			code, body = call(t, tn.gate.URL, tn.token, http.MethodGet, "/v1/beads/"+ids[tn.land]+"/comments", nil)
			if code == http.StatusOK && strings.Contains(body, "pwned") {
				t.Errorf("%s: the other tenant commented on own bead", tn.land)
			}
		}
	})

	t.Run("the land door's closed routes stay closed", func(t *testing.T) {
		for _, p := range []string{"/v1/events/stream", "/v1/bus/events", "/v1/bridge-tokens", "/v1/admin/revoke-token"} {
			if code, _ := call(t, a.gate.URL, a.token, http.MethodGet, p, nil); code != http.StatusNotFound {
				t.Errorf("GET %s through a gate: %d, want 404", p, code)
			}
		}
	})
}

// startKdServe runs `kd serve` on free loopback ports against dbURL, trusting
// the test issuer's certificate, and returns its HTTP base URL once live with
// OIDC enabled.
func startKdServe(t *testing.T, kd, dbURL string, iss *testIssuer) string {
	t.Helper()
	dir := t.TempDir()
	caFile := filepath.Join(dir, "issuer-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: iss.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	httpAddr := freeAddr(t)
	logFile := filepath.Join(dir, "kd-serve.log")
	logOut, err := os.Create(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, kd, "serve")
	// A clean environment: no NATS (kd serve fails closed on an unreachable
	// one) and nothing from the runner that names a daemon or a token.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"BEADS_DATABASE_URL=" + dbURL,
		"BEADS_HTTP_ADDR=" + httpAddr,
		"BEADS_GRPC_ADDR=" + freeAddr(t),
		"BEADS_METRICS_ADDR=" + freeAddr(t),
		"BEADS_RATE_LIMIT_ENABLED=false",
		"BEADS_AUTH_TOKEN=landgate-e2e-" + randHex(t),
		"BEADS_OIDC_ISSUER_URL=" + iss.url(),
		"BEADS_OIDC_AUDIENCE=" + testAudience,
		"SSL_CERT_FILE=" + caFile,
	}
	cmd.Stdout, cmd.Stderr = logOut, logOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	dumpLog := func() {
		b, _ := os.ReadFile(logFile)
		t.Logf("kd serve log:\n%s", b)
	}
	t.Cleanup(func() {
		cancel()
		<-exited
		if t.Failed() {
			dumpLog()
		}
	})

	base := "http://" + httpAddr
	deadline := time.Now().Add(90 * time.Second)
	for {
		select {
		case <-exited:
			dumpLog()
			t.Fatal("kd serve exited before it was live")
		default:
		}
		if resp, err := http.Get(base + "/v1/health/live"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			dumpLog()
			t.Fatal("kd serve never became live")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if b, _ := os.ReadFile(logFile); !bytes.Contains(b, []byte("OIDC auth enabled")) {
		dumpLog()
		t.Fatal("kd serve did not enable OIDC against the test issuer")
	}
	return base
}

func call(t *testing.T, base, token, method, path string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
