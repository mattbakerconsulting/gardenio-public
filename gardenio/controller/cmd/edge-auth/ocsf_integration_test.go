// Integration test for gardenio-edge-auth OCSF Access Activity audit
// logs (W-B-10, ka-0000004490). Pairs the audit sink in audit.go with
// the live HTTP surface in server.go + proxy.go so the wire-shape that
// CloudWatch + Mezmo ingest is exercised by every status_detail
// outcome the shim can emit.
//
// What this file adds on top of audit_test.go:
//
//  1. Schema validator (validateOCSFAccessActivity). Audit unit tests
//     check individual fields; this validator enforces the whole
//     contract — class, required metadata, status enum, actor
//     presence rules per status. A future refactor that drops a
//     required field or invents a new status_detail without
//     registering it here fails loudly.
//  2. Live-emission table that drives every status_detail value
//     through real HTTP into the shim and validates the captured
//     record against the schema validator.
//  3. Checked-in fixtures under testdata/ocsf/. One per
//     (endpoint, status_detail) pair, normalized so they round-trip
//     deterministically. These are the canonical examples the Mezmo
//     dashboard work (parented under epic ka-0000003384) references
//     when building filters and saved searches.
//
// Run `go test -run TestOCSF_LiveEmission_AllOutcomes -update
// ./gardenio/controller/cmd/edge-auth/...` to regenerate fixtures
// after an intentional schema change.
//
// No build tag — this runs every `go test ./...` cycle so a
// regression in the audit shape catches before the next deploy.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var ocsfUpdate = flag.Bool("update", false, "regenerate testdata/ocsf/* fixtures from live emission")

// validateOCSFAccessActivity enforces the OCSF Access Activity (uid
// 3006) subset documented in gardenio/plans/stage3-ava-cli-eb-rollout.md
// §"Audit log schema". It accepts a generic decoded JSON map so it
// catches missing-field regressions the typed auditRecord would
// silently default away.
func validateOCSFAccessActivity(rec map[string]any) error {
	if got, _ := rec["class"].(string); got != ocsfClassAccessActivity {
		return fmt.Errorf("class=%q, want %q", got, ocsfClassAccessActivity)
	}
	ts, _ := rec["time"].(string)
	if ts == "" {
		return fmt.Errorf("time empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		return fmt.Errorf("time=%q is not RFC3339Nano: %w", ts, err)
	}

	// status_id JSON unmarshals to float64.
	statusID, ok := rec["status_id"].(float64)
	if !ok {
		return fmt.Errorf("status_id missing or not numeric: %v", rec["status_id"])
	}
	if int(statusID) != ocsfStatusSuccess && int(statusID) != ocsfStatusFailure {
		return fmt.Errorf("status_id=%v not in {%d,%d}", statusID, ocsfStatusSuccess, ocsfStatusFailure)
	}

	detail, _ := rec["status_detail"].(string)
	if detail == "" {
		return fmt.Errorf("status_detail empty")
	}
	if _, known := knownStatusDetails()[detail]; !known {
		return fmt.Errorf("status_detail=%q is not a registered value", detail)
	}

	md, _ := rec["metadata"].(map[string]any)
	if md == nil {
		return fmt.Errorf("metadata object missing")
	}
	if id, _ := md["audit_id"].(string); id == "" {
		return fmt.Errorf("metadata.audit_id empty")
	}
	if sv, _ := md["shim_version"].(string); sv == "" {
		return fmt.Errorf("metadata.shim_version empty")
	}
	endpoint, _ := md["endpoint"].(string)
	switch endpoint {
	case auditEndpointAuthCheck, auditEndpointProxy, auditEndpointForwardauth, auditEndpointEntraForwardauth:
		// ok
	default:
		return fmt.Errorf("metadata.endpoint=%q must be %q, %q, %q or %q",
			endpoint, auditEndpointAuthCheck, auditEndpointProxy, auditEndpointForwardauth, auditEndpointEntraForwardauth)
	}
	if up, _ := md["upstream_service"].(string); up != "" {
		if _, ok := knownUpstreams()[up]; !ok {
			return fmt.Errorf("metadata.upstream_service=%q is not a known upstream", up)
		}
	}

	req, _ := rec["http_request"].(map[string]any)
	if req == nil {
		return fmt.Errorf("http_request object missing")
	}
	if m, _ := req["http_method"].(string); m == "" {
		return fmt.Errorf("http_request.http_method empty")
	}
	url, _ := req["url"].(map[string]any)
	if url == nil {
		return fmt.Errorf("http_request.url object missing")
	}
	if p, _ := url["path"].(string); p == "" {
		return fmt.Errorf("http_request.url.path empty")
	}

	if int(statusID) == ocsfStatusSuccess {
		actor, _ := rec["actor"].(map[string]any)
		if actor == nil {
			return fmt.Errorf("status_id=success but actor is null")
		}
		user, _ := actor["user"].(map[string]any)
		if user == nil {
			return fmt.Errorf("status_id=success but actor.user missing")
		}
		email, _ := user["email_addr"].(string)
		name, _ := user["name"].(string)
		if email == "" && name == "" {
			return fmt.Errorf("status_id=success but actor.user has neither email_addr nor name")
		}
	}
	// On failure, actor may be nil (PAT-invalid) or carry a partial
	// identity (deny:not-in-gardenio). Both are valid per plan and the
	// AVA reference behaviour.

	if src, ok := rec["src_endpoint"].(map[string]any); ok {
		if ip, _ := src["ip"].(string); ip != "" {
			if net.ParseIP(ip) == nil {
				return fmt.Errorf("src_endpoint.ip=%q is not a valid IP", ip)
			}
		}
	}
	return nil
}

// knownStatusDetails is the registry of every status_detail value the
// shim is allowed to emit. Validator rejects anything not present so a
// new outcome that ships without registration fails the test pyramid.
func knownStatusDetails() map[string]struct{} {
	return map[string]struct{}{
		auditDetailOK:                      {},
		auditDetailDenyInvalidPAT:          {},
		auditDetailDenyMissingToken:        {},
		auditDetailDenyNotInGroup:          {},
		auditDetailDenyUnknownPrefix:       {},
		auditDetailDenyValidatorOff:        {},
		auditDetailDenyProxyOff:            {},
		auditDetailDenyGitLabDown:          {},
		auditDetailDenyInvalidAVA:          {},
		auditDetailDenyMissingAVAHeader:    {},
		auditDetailDenyForwardauthOff:      {},
		auditDetailDenyVerifierUnavailable: {},
		auditDetailDenyInvalidEntra:        {},
		auditDetailDenyEntraForbidden:      {},
		auditDetailDenyEntraUnavailable:    {},
		auditDetailDenyEntraForwardauthOff: {},
	}
}

// knownUpstreams mirrors the upstream registry in buildUpstreams.
func knownUpstreams() map[string]struct{} {
	return map[string]struct{}{"kbeads": {}, "coopmux": {}}
}

// ocsfFixture binds a request the integration test fires at the shim
// to the expected (endpoint, status_detail) pair and a fixture
// filename. The filename also doubles as the test sub-name.
type ocsfFixture struct {
	name     string // testdata/ocsf/<name>.json
	endpoint string
	detail   string
	prepare  func(t *testing.T) (req *http.Request, cleanup func())
}

// fixedClock is the same epoch hmac_integration_test.go uses so audit
// timestamps are predictable across both integration suites.
const ocsfFixedUnix = int64(1_700_000_000)

func ocsfFixedTime() time.Time { return time.Unix(ocsfFixedUnix, 0).UTC() }

func TestOCSF_LiveEmission_AllOutcomes(t *testing.T) {
	fixtures := []ocsfFixture{
		{
			name:     "auth_check__ok",
			endpoint: auditEndpointAuthCheck,
			detail:   auditDetailOK,
			prepare:  prepareAuthCheck(authCheckCfg{token: "Bearer good-pat"}),
		},
		{
			name:     "auth_check__deny_missing_token",
			endpoint: auditEndpointAuthCheck,
			detail:   auditDetailDenyMissingToken,
			prepare:  prepareAuthCheck(authCheckCfg{token: ""}),
		},
		{
			name:     "auth_check__deny_invalid_pat",
			endpoint: auditEndpointAuthCheck,
			detail:   auditDetailDenyInvalidPAT,
			prepare:  prepareAuthCheck(authCheckCfg{token: "Bearer bogus-pat"}),
		},
		{
			name:     "auth_check__deny_not_in_group",
			endpoint: auditEndpointAuthCheck,
			detail:   auditDetailDenyNotInGroup,
			prepare:  prepareAuthCheck(authCheckCfg{token: "Bearer mallory-pat"}),
		},
		{
			name:     "auth_check__deny_validator_off",
			endpoint: auditEndpointAuthCheck,
			detail:   auditDetailDenyValidatorOff,
			prepare:  prepareAuthCheck(authCheckCfg{token: "Bearer anything", noValidator: true}),
		},
		{
			name:     "auth_check__deny_gitlab_unavailable",
			endpoint: auditEndpointAuthCheck,
			detail:   auditDetailDenyGitLabDown,
			prepare:  prepareAuthCheck(authCheckCfg{token: "Bearer good-pat", gitlabDown: true}),
		},
		{
			name:     "proxy__ok",
			endpoint: auditEndpointProxy,
			detail:   auditDetailOK,
			prepare:  prepareProxy(proxyCfg{token: "Bearer good-pat", path: "/kd/v1/beads"}),
		},
		{
			name:     "proxy__deny_missing_token",
			endpoint: auditEndpointProxy,
			detail:   auditDetailDenyMissingToken,
			prepare:  prepareProxy(proxyCfg{token: "", path: "/kd/v1/beads"}),
		},
		{
			name:     "proxy__deny_invalid_pat",
			endpoint: auditEndpointProxy,
			detail:   auditDetailDenyInvalidPAT,
			prepare:  prepareProxy(proxyCfg{token: "Bearer bogus-pat", path: "/kd/v1/beads"}),
		},
		{
			name:     "proxy__deny_not_in_group",
			endpoint: auditEndpointProxy,
			detail:   auditDetailDenyNotInGroup,
			prepare:  prepareProxy(proxyCfg{token: "Bearer mallory-pat", path: "/kd/v1/beads"}),
		},
		{
			name:     "proxy__deny_proxy_off",
			endpoint: auditEndpointProxy,
			detail:   auditDetailDenyProxyOff,
			prepare:  prepareProxy(proxyCfg{token: "Bearer good-pat", path: "/kd/v1/beads", noHMAC: true}),
		},
		{
			name:     "proxy__deny_gitlab_unavailable",
			endpoint: auditEndpointProxy,
			detail:   auditDetailDenyGitLabDown,
			prepare:  prepareProxy(proxyCfg{token: "Bearer good-pat", path: "/kd/v1/beads", gitlabDown: true}),
		},
		{
			name:     "proxy__deny_invalid_ava",
			endpoint: auditEndpointProxy,
			detail:   auditDetailDenyInvalidAVA,
			prepare:  prepareProxyAVA(proxyAVACfg{header: "tampered-jwt", path: "/kd/v1/beads", verifyErr: errAVAInvalid}),
		},
		{
			name:     "forwardauth__ok",
			endpoint: auditEndpointForwardauth,
			detail:   auditDetailOK,
			prepare: prepareForwardauth(forwardauthCfg{
				header:   "valid-jwt",
				identity: avaIdentity{Email: "ava-user@acme.example", Groups: []string{"gardenio"}},
			}),
		},
		{
			name:     "forwardauth__deny_invalid_ava",
			endpoint: auditEndpointForwardauth,
			detail:   auditDetailDenyInvalidAVA,
			prepare: prepareForwardauth(forwardauthCfg{
				header:    "tampered-jwt",
				verifyErr: errAVAInvalid,
			}),
		},
		{
			name:     "forwardauth__deny_missing_ava_header",
			endpoint: auditEndpointForwardauth,
			detail:   auditDetailDenyMissingAVAHeader,
			prepare:  prepareForwardauth(forwardauthCfg{header: ""}),
		},
		{
			name:     "forwardauth__deny_forwardauth_off",
			endpoint: auditEndpointForwardauth,
			detail:   auditDetailDenyForwardauthOff,
			prepare:  prepareForwardauth(forwardauthCfg{header: "anything", noAVA: true}),
		},
		{
			name:     "forwardauth__deny_verifier_unavailable",
			endpoint: auditEndpointForwardauth,
			detail:   auditDetailDenyVerifierUnavailable,
			prepare: prepareForwardauth(forwardauthCfg{
				header:    "valid-jwt",
				verifyErr: errors.New("jwks fetch timeout"),
			}),
		},
		{
			name:     "entra_forwardauth__ok",
			endpoint: auditEndpointEntraForwardauth,
			detail:   auditDetailOK,
			prepare: prepareEntraForwardauth(entraForwardauthCfg{
				token:    "valid-jwt",
				identity: entraIdentity{Email: "operator@acme.example", Roles: []string{"gardenio-operator"}},
			}),
		},
		{
			name:     "entra_forwardauth__deny_invalid_entra",
			endpoint: auditEndpointEntraForwardauth,
			detail:   auditDetailDenyInvalidEntra,
			prepare: prepareEntraForwardauth(entraForwardauthCfg{
				token:     "tampered-jwt",
				verifyErr: errEntraInvalid,
			}),
		},
		{
			name:     "entra_forwardauth__deny_entra_forbidden",
			endpoint: auditEndpointEntraForwardauth,
			detail:   auditDetailDenyEntraForbidden,
			prepare: prepareEntraForwardauth(entraForwardauthCfg{
				token:     "wrong-scope-jwt",
				verifyErr: errEntraForbidden,
			}),
		},
		{
			name:     "entra_forwardauth__deny_entra_unavailable",
			endpoint: auditEndpointEntraForwardauth,
			detail:   auditDetailDenyEntraUnavailable,
			prepare: prepareEntraForwardauth(entraForwardauthCfg{
				token:     "valid-jwt",
				verifyErr: errEntraUnavailable,
			}),
		},
		{
			name:     "entra_forwardauth__deny_not_configured",
			endpoint: auditEndpointEntraForwardauth,
			detail:   auditDetailDenyEntraForwardauthOff,
			prepare:  prepareEntraForwardauth(entraForwardauthCfg{token: "anything", noEntra: true}),
		},
		{
			// /mux/ matches a registered prefix but the upstream URL is
			// blank in this fixture so coopmux is NOT in s.upstreams.
			// We register a custom catch-all on /mux/ to drive the
			// matchUpstream miss; cleaner than reaching for the
			// unregistered-prefix path which the ServeMux 404s before
			// the handler fires (see audit_test.go DenyModes
			// "unknown_prefix").
			name:     "proxy__deny_unknown_upstream",
			endpoint: auditEndpointProxy,
			detail:   auditDetailDenyUnknownPrefix,
			prepare:  prepareProxy(proxyCfg{token: "Bearer good-pat", path: "/mux/sessions", registerMuxCatchAll: true}),
		},
	}

	// Coverage assertion: the integration suite must exercise every
	// known status_detail at least once. Catches the case where someone
	// adds a new detail to audit.go without registering a fixture.
	seen := map[string]struct{}{}
	for _, f := range fixtures {
		seen[f.detail] = struct{}{}
	}
	for d := range knownStatusDetails() {
		if _, ok := seen[d]; !ok {
			t.Fatalf("integration coverage gap: status_detail %q has no fixture; add one to TestOCSF_LiveEmission_AllOutcomes", d)
		}
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			req, cleanup := fx.prepare(t)
			defer cleanup()
			req.RemoteAddr = "203.0.113.7:54321" // fixed for fixture stability (TEST-NET-3)

			rec := fireAndCapture(t, req)

			// Schema validation against the OCSF Access Activity subset
			// documented in the plan. Decode through map[string]any so
			// missing fields surface — the typed auditRecord would
			// silently default.
			var generic map[string]any
			if err := json.Unmarshal(rec, &generic); err != nil {
				t.Fatalf("decode audit JSON: %v\n%s", err, rec)
			}
			if err := validateOCSFAccessActivity(generic); err != nil {
				t.Fatalf("OCSF schema validation failed: %v\nrecord: %s", err, rec)
			}

			// Cross-check that the (endpoint, status_detail) pair the
			// fixture claims to exercise is what actually landed in the
			// record. Guards against a refactor that, say, changes the
			// proxy off-switch to emit auditEndpointAuthCheck.
			gotDetail, _ := generic["status_detail"].(string)
			if gotDetail != fx.detail {
				t.Errorf("status_detail=%q, want %q", gotDetail, fx.detail)
			}
			md := generic["metadata"].(map[string]any)
			gotEndpoint, _ := md["endpoint"].(string)
			if gotEndpoint != fx.endpoint {
				t.Errorf("metadata.endpoint=%q, want %q", gotEndpoint, fx.endpoint)
			}

			// Normalise volatile fields for fixture stability. The live
			// audit_id is ULID-shaped (timestamp + random); we pin it to
			// FIXTURE_AUDIT_ID. The time is already fixed because every
			// fixture wires s.now to ocsfFixedTime.
			md["audit_id"] = "01H00000000000000000000000"

			normalised, err := json.MarshalIndent(generic, "", "  ")
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			normalised = append(normalised, '\n')

			path := filepath.Join("testdata", "ocsf", fx.name+".json")
			if *ocsfUpdate {
				if err := os.WriteFile(path, normalised, 0o644); err != nil {
					t.Fatalf("write fixture %s: %v", path, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture %s: %v (run with -update to regenerate)", path, err)
			}
			if !bytes.Equal(want, normalised) {
				t.Errorf("fixture %s drifted from live emission. Re-run with -update if intended.\n--- want\n%s\n--- got\n%s", path, want, normalised)
			}
		})
	}
}

// TestOCSF_Fixtures_SchemaValid loads every checked-in fixture and
// re-runs the schema validator. Decoupled from the live-emission test
// so that a future refactor that changes how records are emitted
// (e.g. moves to a logging library) is still caught by the schema
// pinned to disk.
func TestOCSF_Fixtures_SchemaValid(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "ocsf"))
	if err != nil {
		t.Fatalf("list testdata/ocsf: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no fixtures in testdata/ocsf; run TestOCSF_LiveEmission_AllOutcomes with -update")
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "ocsf", n))
			if err != nil {
				t.Fatal(err)
			}
			var rec map[string]any
			if err := json.Unmarshal(b, &rec); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := validateOCSFAccessActivity(rec); err != nil {
				t.Errorf("fixture %s fails OCSF schema: %v", n, err)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Fixture prep helpers
// ---------------------------------------------------------------------

type authCheckCfg struct {
	token       string
	noValidator bool // build server without GitLab config -> deny:validator-not-configured
	gitlabDown  bool // point validator at a closed listener -> deny:gitlab-unavailable
}

func prepareAuthCheck(c authCheckCfg) func(t *testing.T) (*http.Request, func()) {
	return func(t *testing.T) (*http.Request, func()) {
		t.Helper()
		var cleanup []func()
		cfg := &config{}
		if !c.noValidator {
			if c.gitlabDown {
				cfg.gitlabBaseURL = unreachableURL(t)
				cfg.gitlabAuthGroup = "gardenio"
			} else {
				gitlab := newStubGitLab(t)
				cleanup = append(cleanup, gitlab.Close)
				cfg.gitlabBaseURL = gitlab.URL
				cfg.gitlabAuthGroup = "gardenio"
			}
		}
		ts := newSharedShim(t, cfg, captureSink(t))
		cleanup = append(cleanup, ts.shutdown)

		req, err := http.NewRequest(http.MethodGet, ts.url+"/auth/check", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.token != "" {
			req.Header.Set("Authorization", c.token)
		}
		bind(t, req, ts)
		return req, func() {
			for _, f := range cleanup {
				f()
			}
		}
	}
}

type proxyCfg struct {
	token               string
	path                string
	noHMAC              bool // disable HMAC -> deny:proxy-not-configured
	gitlabDown          bool // gitlab unreachable -> deny:gitlab-unavailable
	registerMuxCatchAll bool // wire /mux/ on the mux without a coopmux upstream -> deny:unknown-upstream
}

func prepareProxy(c proxyCfg) func(t *testing.T) (*http.Request, func()) {
	return func(t *testing.T) (*http.Request, func()) {
		t.Helper()
		var cleanup []func()
		cfg := &config{gitlabAuthGroup: "gardenio", hmacKey: "test-hmac-key"}
		if c.noHMAC {
			cfg.hmacKey = ""
		}
		if c.gitlabDown {
			cfg.gitlabBaseURL = unreachableURL(t)
		} else {
			gitlab := newStubGitLab(t)
			cleanup = append(cleanup, gitlab.Close)
			cfg.gitlabBaseURL = gitlab.URL
		}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		cleanup = append(cleanup, upstream.Close)
		cfg.upstreamKbeads = upstream.URL

		var extra []routeRegistrar
		if c.registerMuxCatchAll {
			// Wire /mux/ on the ServeMux directly so the request
			// reaches handleProxy, then matchUpstream returns nil
			// because coopmux is absent from cfg.upstreams. This is
			// the precise condition that emits
			// auditDetailDenyUnknownPrefix.
			extra = append(extra, func(mux *http.ServeMux, s *server) {
				mux.HandleFunc("/mux/", s.handleProxy)
			})
		}
		ts := newSharedShim(t, cfg, captureSink(t), extra...)
		cleanup = append(cleanup, ts.shutdown)

		req, err := http.NewRequest(http.MethodGet, ts.url+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.token != "" {
			req.Header.Set("Authorization", c.token)
		}
		bind(t, req, ts)
		return req, func() {
			for _, f := range cleanup {
				f()
			}
		}
	}
}

// proxyAVACfg drives the AVA-mode fixture. We do not stand up a real JWKS
// server here — that lives in ava_integration_test.go. The OCSF fixture
// only needs to confirm that an AVA-verification failure produces the
// expected audit record shape, so we inject a stub verifier that returns
// the configured outcome.
type proxyAVACfg struct {
	header    string // value to set on X-Amzn-Ava-User-Context
	path      string
	identity  avaIdentity // returned on success
	verifyErr error       // returned when set; nil = success
}

func prepareProxyAVA(c proxyAVACfg) func(t *testing.T) (*http.Request, func()) {
	return func(t *testing.T) (*http.Request, func()) {
		t.Helper()
		var cleanup []func()
		// Stub GitLab is still wired so newServer builds a PAT validator —
		// otherwise proxyEnabled() returns false and the AVA branch never
		// fires. The stub is harmless: the AVA path does not call it.
		gitlab := newStubGitLab(t)
		cleanup = append(cleanup, gitlab.Close)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		cleanup = append(cleanup, upstream.Close)

		cfg := &config{
			gitlabBaseURL:   gitlab.URL,
			gitlabAuthGroup: "gardenio",
			hmacKey:         "test-hmac-key",
			upstreamKbeads:  upstream.URL,
		}
		ts := newSharedShim(t, cfg, captureSink(t), func(_ *http.ServeMux, s *server) {
			s.avaVerifier = &stubVerifier{id: c.identity, err: c.verifyErr}
		})
		cleanup = append(cleanup, ts.shutdown)

		req, err := http.NewRequest(http.MethodGet, ts.url+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.header != "" {
			req.Header.Set("X-Amzn-Ava-User-Context", c.header)
		}
		bind(t, req, ts)
		return req, func() {
			for _, f := range cleanup {
				f()
			}
		}
	}
}

// forwardauthCfg drives the /forwardauth/verify fixture. Like proxyAVACfg
// it stubs the verifier rather than standing up a JWKS server.
type forwardauthCfg struct {
	header    string      // value to set on X-Amzn-Ava-User-Context; "" omits
	identity  avaIdentity // returned on success
	verifyErr error       // returned when set; nil = success
	noAVA     bool        // build the shim with no AVA verifier wired
}

func prepareForwardauth(c forwardauthCfg) func(t *testing.T) (*http.Request, func()) {
	return func(t *testing.T) (*http.Request, func()) {
		t.Helper()
		var cleanup []func()
		// Stub GitLab is still wired so newServer builds a PAT validator.
		// handleForwardauth does not call the validator, but the shared
		// shim helper expects readiness to be plausible.
		gitlab := newStubGitLab(t)
		cleanup = append(cleanup, gitlab.Close)

		cfg := &config{
			gitlabBaseURL:   gitlab.URL,
			gitlabAuthGroup: "gardenio",
			hmacKey:         "test-hmac-key",
		}
		ts := newSharedShim(t, cfg, captureSink(t), func(_ *http.ServeMux, s *server) {
			if c.noAVA {
				s.avaVerifier = nil
			} else {
				s.avaVerifier = &stubVerifier{id: c.identity, err: c.verifyErr}
			}
		})
		cleanup = append(cleanup, ts.shutdown)

		req, err := http.NewRequest(http.MethodGet, ts.url+"/forwardauth/verify", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.header != "" {
			req.Header.Set("X-Amzn-Ava-User-Context", c.header)
		}
		bind(t, req, ts)
		return req, func() {
			for _, f := range cleanup {
				f()
			}
		}
	}
}

// entraForwardauthCfg drives the dedicated delegated-token ForwardAuth
// endpoint with a stub verifier. Signature and claims verification are covered
// by oidcauth tests; these fixtures pin the edge's audit contract.
type entraForwardauthCfg struct {
	token     string
	identity  entraIdentity
	verifyErr error
	noEntra   bool
}

func prepareEntraForwardauth(c entraForwardauthCfg) func(t *testing.T) (*http.Request, func()) {
	return func(t *testing.T) (*http.Request, func()) {
		t.Helper()
		gitlab := newStubGitLab(t)
		cfg := &config{
			gitlabBaseURL:   gitlab.URL,
			gitlabAuthGroup: "gardenio",
			hmacKey:         "test-hmac-key",
		}
		ts := newSharedShim(t, cfg, captureSink(t), func(_ *http.ServeMux, s *server) {
			if c.noEntra {
				s.entraVerifier = nil
			} else {
				s.entraVerifier = &stubEntraVerifier{id: c.identity, err: c.verifyErr, isReady: c.verifyErr == nil}
			}
		})

		req, err := http.NewRequest(http.MethodGet, ts.url+"/forwardauth/entra", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		bind(t, req, ts)
		return req, func() {
			ts.shutdown()
			gitlab.Close()
		}
	}
}

// routeRegistrar lets a fixture install an extra route on the shim's
// ServeMux *after* RegisterRoutes ran. Used for the unknown-upstream
// fixture, which needs a path that reaches handleProxy without a
// matching upstream.
type routeRegistrar func(mux *http.ServeMux, s *server)

// ---------------------------------------------------------------------
// Shim + sink plumbing
// ---------------------------------------------------------------------

type shimFixture struct {
	url      string
	buf      *bytes.Buffer
	shutdown func()
}

// captureSink returns a buffer the caller installs onto the shim via
// newSharedShim. The buffer survives the per-request lifecycle so the
// test can read it after firing.
func captureSink(t *testing.T) *bytes.Buffer {
	t.Helper()
	return &bytes.Buffer{}
}

func newSharedShim(t *testing.T, cfg *config, buf *bytes.Buffer, extra ...routeRegistrar) *shimFixture {
	t.Helper()
	srv := newServer(cfg, "ocsf-int-test-1.0", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv.now = func() time.Time { return ocsfFixedTime() }
	srv.audit = newAuditSink(buf, srv.now)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	for _, fn := range extra {
		fn(mux, srv)
	}
	ts := httptest.NewServer(mux)
	return &shimFixture{url: ts.URL, buf: buf, shutdown: ts.Close}
}

// bind stitches the request and the shim so the firing helper can find
// the buffer after the request completes. Avoids a package-global.
func bind(t *testing.T, req *http.Request, ts *shimFixture) {
	t.Helper()
	req.Header.Set("X-Test-Shim-URL", ts.url)
	// Stash the buffer pointer on the request via a private header key
	// won't work — we cannot put pointers in headers. Use a registry
	// keyed off URL instead. Single-use per fixture is fine because
	// each call to prepare() builds its own shim.
	registerSink(ts.url, ts.buf)
}

var sinkRegistry = map[string]*bytes.Buffer{}

func registerSink(url string, buf *bytes.Buffer) {
	sinkRegistry[url] = buf
}

func sinkFor(req *http.Request) *bytes.Buffer {
	url := req.Header.Get("X-Test-Shim-URL")
	return sinkRegistry[url]
}

// fireAndCapture issues the request, asserts a response landed, and
// returns the single audit JSON line emitted. Fails the test if zero
// or more-than-one record were emitted — the shim emits exactly one
// per terminated request, so any other count is a regression.
func fireAndCapture(t *testing.T, req *http.Request) []byte {
	t.Helper()
	buf := sinkFor(req)
	if buf == nil {
		t.Fatalf("no audit buffer registered for %q", req.URL.String())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte{'\n'})
	if len(lines) != 1 || len(lines[0]) == 0 {
		t.Fatalf("want 1 audit line, got %d: %s", len(lines), buf.String())
	}
	return lines[0]
}

// ---------------------------------------------------------------------
// Stub GitLab + unreachable upstream
// ---------------------------------------------------------------------

// newStubGitLab returns an httptest server that resolves:
//   - good-pat                     -> alice@example.com, member of gardenio
//   - mallory-pat, notin-group-pat -> mallory@example.com, NOT in gardenio
//
// Any other token returns 401, mapped to deny:invalid-pat by the shim.
func newStubGitLab(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/user":
			switch bearerPAT(r) {
			case "good-pat":
				_, _ = w.Write([]byte(`{"id":42,"username":"alice","email":"alice@example.com","name":"Alice"}`))
			case "mallory-pat", "notin-group-pat":
				_, _ = w.Write([]byte(`{"id":99,"username":"mallory","email":"mallory@example.com","name":"Mallory"}`))
			default:
				http.Error(w, "401", http.StatusUnauthorized)
			}
		case "/api/v4/groups/gardenio/members/all/42":
			_, _ = w.Write([]byte(`{"id":42,"username":"alice"}`))
		case "/api/v4/groups/gardenio/members/all/99":
			http.Error(w, "404", http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
}

// unreachableURL returns an http URL whose port is closed. Hitting it
// triggers the GitLab-down branch in the validator and emits
// auditDetailDenyGitLabDown.
func unreachableURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // immediately free the port so dials fail with connection-refused
	return "http://" + addr
}
