package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureAudit replaces a server's audit sink with one that writes to an
// in-memory buffer so tests can decode every emitted OCSF record.
func captureAudit(s *server) *bytes.Buffer {
	buf := &bytes.Buffer{}
	s.audit = newAuditSink(&lockedWriter{w: buf}, func() time.Time { return s.now() })
	return buf
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func decodeAudit(t *testing.T, buf *bytes.Buffer) []auditRecord {
	t.Helper()
	var out []auditRecord
	for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var rec auditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("decode audit line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestAudit_AuthCheck_HappyPath(t *testing.T) {
	gitlab := newStubGitLab(t)
	defer gitlab.Close()

	cfg := &config{gitlabBaseURL: gitlab.URL, gitlabAuthGroup: "gardenio"}
	srv := newServer(cfg, "test-shim-1.0", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	buf := captureAudit(srv)

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/auth/check", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}

	recs := decodeAudit(t, buf)
	if len(recs) != 1 {
		t.Fatalf("len(records)=%d, want 1: %s", len(recs), buf.String())
	}
	rec := recs[0]
	if rec.Class != ocsfClassAccessActivity {
		t.Errorf("class=%q, want %q", rec.Class, ocsfClassAccessActivity)
	}
	if rec.StatusID != ocsfStatusSuccess {
		t.Errorf("status_id=%d, want %d", rec.StatusID, ocsfStatusSuccess)
	}
	if rec.StatusDetail != auditDetailOK {
		t.Errorf("status_detail=%q, want %q", rec.StatusDetail, auditDetailOK)
	}
	if rec.Actor == nil || rec.Actor.User.EmailAddr != "alice@example.com" {
		t.Errorf("actor email=%+v, want alice@example.com", rec.Actor)
	}
	if rec.Metadata.AuditID == "" {
		t.Error("audit_id empty")
	}
	if rec.Metadata.ShimVersion != "test-shim-1.0" {
		t.Errorf("shim_version=%q, want test-shim-1.0", rec.Metadata.ShimVersion)
	}
	if rec.Metadata.Endpoint != auditEndpointAuthCheck {
		t.Errorf("endpoint=%q, want %q", rec.Metadata.Endpoint, auditEndpointAuthCheck)
	}
	if rec.HTTPRequest.URL.Path != "/auth/check" {
		t.Errorf("http_request.url.path=%q, want /auth/check", rec.HTTPRequest.URL.Path)
	}
	if rec.HTTPRequest.Method != "GET" {
		t.Errorf("http_request.http_method=%q, want GET", rec.HTTPRequest.Method)
	}
	// time should parse as RFC3339Nano.
	if _, err := time.Parse(time.RFC3339Nano, rec.Time); err != nil {
		t.Errorf("time=%q is not RFC3339Nano: %v", rec.Time, err)
	}
}

func TestAudit_AuthCheck_FailureModes(t *testing.T) {
	gitlab := newStubGitLab(t)
	defer gitlab.Close()

	cases := []struct {
		name       string
		token      string
		wantStatus int
		wantDetail string
		wantActor  string
	}{
		{"missing_token", "", http.StatusUnauthorized, auditDetailDenyMissingToken, ""},
		{"invalid_token", "Bearer bogus", http.StatusUnauthorized, auditDetailDenyInvalidPAT, ""},
		// Validate returns the partial identity from /user on
		// ErrNotInGroup, so the deny record carries the actor email.
		{"not_in_group", "Bearer mallory-pat", http.StatusForbidden, auditDetailDenyNotInGroup, "mallory@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config{gitlabBaseURL: gitlab.URL, gitlabAuthGroup: "gardenio"}
			srv := newServer(cfg, "v", slog.New(slog.NewJSONHandler(io.Discard, nil)))
			buf := captureAudit(srv)
			mux := http.NewServeMux()
			srv.RegisterRoutes(mux)
			ts := httptest.NewServer(mux)
			defer ts.Close()

			req, _ := http.NewRequest("GET", ts.URL+"/auth/check", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", tc.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status=%d, want %d", resp.StatusCode, tc.wantStatus)
			}
			recs := decodeAudit(t, buf)
			if len(recs) != 1 {
				t.Fatalf("len(records)=%d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.StatusID != ocsfStatusFailure {
				t.Errorf("status_id=%d, want %d", rec.StatusID, ocsfStatusFailure)
			}
			if rec.StatusDetail != tc.wantDetail {
				t.Errorf("status_detail=%q, want %q", rec.StatusDetail, tc.wantDetail)
			}
			if tc.wantActor == "" {
				if rec.Actor != nil {
					t.Errorf("actor=%+v, want null", rec.Actor)
				}
			} else {
				if rec.Actor == nil || rec.Actor.User.EmailAddr != tc.wantActor {
					t.Errorf("actor=%+v, want %q", rec.Actor, tc.wantActor)
				}
			}
		})
	}
}

func TestAudit_AuthCheck_ValidatorNotConfigured(t *testing.T) {
	cfg := &config{} // no validator
	srv := newServer(cfg, "v", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	buf := captureAudit(srv)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/auth/check")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
	recs := decodeAudit(t, buf)
	if len(recs) != 1 {
		t.Fatalf("len(records)=%d", len(recs))
	}
	if recs[0].StatusDetail != auditDetailDenyValidatorOff {
		t.Errorf("status_detail=%q, want %q", recs[0].StatusDetail, auditDetailDenyValidatorOff)
	}
}

func TestAudit_Proxy_HappyPath(t *testing.T) {
	base, _, cleanup := newProxyTestServerWithCapture(t, "hmac-key")
	defer cleanup.close()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	recs := decodeAudit(t, cleanup.buf)
	if len(recs) != 1 {
		t.Fatalf("len(records)=%d: %s", len(recs), cleanup.buf.String())
	}
	rec := recs[0]
	if rec.StatusID != ocsfStatusSuccess || rec.StatusDetail != auditDetailOK {
		t.Errorf("status=%d detail=%q, want %d ok", rec.StatusID, rec.StatusDetail, ocsfStatusSuccess)
	}
	if rec.Metadata.UpstreamService != "kbeads" {
		t.Errorf("upstream_service=%q, want kbeads", rec.Metadata.UpstreamService)
	}
	if rec.Metadata.Endpoint != auditEndpointProxy {
		t.Errorf("endpoint=%q, want %q", rec.Metadata.Endpoint, auditEndpointProxy)
	}
	if rec.HTTPRequest.URL.Path != "/kd/v1/beads" {
		t.Errorf("path=%q, want /kd/v1/beads", rec.HTTPRequest.URL.Path)
	}
	if rec.Actor == nil || rec.Actor.User.EmailAddr != "alice@example.com" {
		t.Errorf("actor=%+v, want alice@example.com", rec.Actor)
	}
	if rec.Metadata.DryRun {
		t.Error("dry_run=true on non-dry-run request")
	}
	if rec.SrcEndpoint == nil || rec.SrcEndpoint.IP == "" {
		t.Errorf("src_endpoint missing: %+v", rec.SrcEndpoint)
	}
}

func TestAudit_Proxy_DryRunFlagSet(t *testing.T) {
	const key = "hmac-key"
	gitlab := newStubGitLab(t)
	defer gitlab.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream must not be reached when dryRun=true")
	}))
	defer upstream.Close()

	cfg := &config{
		gitlabBaseURL:   gitlab.URL,
		gitlabAuthGroup: "gardenio",
		hmacKey:         key,
		upstreamKbeads:  upstream.URL,
		dryRun:          true,
	}
	srv := newServer(cfg, "v", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	buf := captureAudit(srv)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	recs := decodeAudit(t, buf)
	if len(recs) != 1 {
		t.Fatalf("len=%d", len(recs))
	}
	if !recs[0].Metadata.DryRun {
		t.Error("dry_run=false in audit record, want true")
	}
	if recs[0].StatusID != ocsfStatusSuccess {
		t.Errorf("status_id=%d, want %d", recs[0].StatusID, ocsfStatusSuccess)
	}
}

func TestAudit_Proxy_DenyModes(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		token      string
		wantStatus int
		wantDetail string
		wantActor  string
		// wantUpstream is the metadata.upstream_service value the test
		// expects; "" means the record was emitted before an upstream
		// match (proxy disabled, unknown prefix).
		wantUpstream string
	}{
		{"missing_token", "/kd/v1/beads", "", http.StatusUnauthorized, auditDetailDenyMissingToken, "", "kbeads"},
		{"invalid_pat", "/kd/v1/beads", "Bearer bogus", http.StatusUnauthorized, auditDetailDenyInvalidPAT, "", "kbeads"},
		{"not_in_group", "/kd/v1/beads", "Bearer notin-group-pat", http.StatusForbidden, auditDetailDenyNotInGroup, "mallory@example.com", "kbeads"},
		{"unknown_prefix", "/mux/sessions", "Bearer good-pat", http.StatusNotFound, auditDetailDenyUnknownPrefix, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, _, cleanup := newProxyTestServerWithCapture(t, "k")
			defer cleanup.close()
			req, _ := http.NewRequest("GET", base+tc.path, nil)
			if tc.token != "" {
				req.Header.Set("Authorization", tc.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status=%d, want %d", resp.StatusCode, tc.wantStatus)
			}
			recs := decodeAudit(t, cleanup.buf)
			if tc.name == "unknown_prefix" {
				// /mux not registered means the ServeMux returns 404
				// without invoking our handler; no audit record fires.
				// That matches the daemon-not-aware-of-this-call shape;
				// document it here so a future change that adds a
				// catch-all handler notices.
				if len(recs) != 0 {
					t.Errorf("len(records)=%d, want 0 for unregistered prefix", len(recs))
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("len(records)=%d: %s", len(recs), cleanup.buf.String())
			}
			rec := recs[0]
			if rec.StatusID != ocsfStatusFailure {
				t.Errorf("status_id=%d, want %d", rec.StatusID, ocsfStatusFailure)
			}
			if rec.StatusDetail != tc.wantDetail {
				t.Errorf("status_detail=%q, want %q", rec.StatusDetail, tc.wantDetail)
			}
			if tc.wantActor == "" {
				if rec.Actor != nil {
					t.Errorf("actor=%+v, want null", rec.Actor)
				}
			} else if rec.Actor == nil || rec.Actor.User.EmailAddr != tc.wantActor {
				t.Errorf("actor=%+v, want %q", rec.Actor, tc.wantActor)
			}
			if rec.Metadata.UpstreamService != tc.wantUpstream {
				t.Errorf("upstream_service=%q, want %q", rec.Metadata.UpstreamService, tc.wantUpstream)
			}
		})
	}
}

func TestAudit_Proxy_ProxyNotConfigured(t *testing.T) {
	// HMAC key empty -> proxy disabled even if validator is good.
	gitlab := newStubGitLab(t)
	defer gitlab.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream must not be reached")
	}))
	defer upstream.Close()

	cfg := &config{
		gitlabBaseURL:   gitlab.URL,
		gitlabAuthGroup: "gardenio",
		hmacKey:         "",
		upstreamKbeads:  upstream.URL,
	}
	srv := newServer(cfg, "v", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	buf := captureAudit(srv)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	recs := decodeAudit(t, buf)
	if len(recs) != 1 {
		t.Fatalf("len=%d", len(recs))
	}
	if recs[0].StatusDetail != auditDetailDenyProxyOff {
		t.Errorf("detail=%q, want %q", recs[0].StatusDetail, auditDetailDenyProxyOff)
	}
}

// proxyCaptureCleanup bundles the tear-down + audit buffer of a proxy
// test server fixture. s is the live *server so AVA-mode tests can swap
// in a stub avaVerifier after construction (newServer wires the real
// verifier from cfg.avaTrustSigner; tests inject for branching coverage).
type proxyCaptureCleanup struct {
	close func()
	buf   *bytes.Buffer
	s     *server
}

// newProxyTestServerWithCapture wires newStubGitLab, a stub upstream, and an
// edge-auth server with a fixed clock and an audit-capture buffer into one
// fixture callers can hit with a real *http.Client. opts adjust the config
// before the server is built (e.g. cfg.allowedEmailDomains).
func newProxyTestServerWithCapture(t *testing.T, hmacKey string, opts ...func(*config)) (string, *upstreamRecorder, proxyCaptureCleanup) {
	t.Helper()
	gitlab := newStubGitLab(t)
	upstream := &upstreamRecorder{body: `{"ok":true}`}
	upstreamServer := httptest.NewServer(upstream.handler())

	cfg := &config{
		gitlabBaseURL:   gitlab.URL,
		gitlabAuthGroup: "gardenio",
		hmacKey:         hmacKey,
		upstreamKbeads:  upstreamServer.URL,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	srv := newServer(cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	buf := captureAudit(srv)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)

	return ts.URL, upstream, proxyCaptureCleanup{
		close: func() {
			ts.Close()
			upstreamServer.Close()
			gitlab.Close()
		},
		buf: buf,
		s:   srv,
	}
}

func TestRemoteAddrIP(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"1.2.3.4:5678", "1.2.3.4"},
		{"[::1]:80", "::1"},
		{"plain-no-port", "plain-no-port"},
		{"", ""},
	}
	for _, tc := range cases {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.in
		if got := remoteAddrIP(r); got != tc.want {
			t.Errorf("remoteAddrIP(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAuditSink_NilSafe(t *testing.T) {
	// Calling emit on a nil sink must be a no-op (used in default-zero
	// server values during test scaffolding).
	var s *auditSink
	s.emit(auditRecord{})
}

func TestAuditSink_SerialisesWrites(t *testing.T) {
	// Concurrent calls to emit should produce a buffer where every line
	// parses as a complete JSON object. A naive non-mutex sink would
	// interleave bytes from concurrent records and break decoding.
	buf := &bytes.Buffer{}
	sink := newAuditSink(buf, func() time.Time { return time.Unix(0, 0) })
	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink.emit(auditRecord{
				Metadata:     auditMetadata{AuditID: "id"},
				StatusID:     ocsfStatusSuccess,
				StatusDetail: "ok",
				HTTPRequest:  auditRequest{Method: "GET", URL: auditURL{Path: "/x"}},
			})
		}()
	}
	wg.Wait()

	count := 0
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec auditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("interleaved write detected: %q: %v", line, err)
		}
		count++
	}
	if count != n {
		t.Errorf("count=%d, want %d", count, n)
	}
}

// bearerPAT extracts the token from the Authorization: Bearer header the shim
// now sends to GitLab (it previously used PRIVATE-TOKEN). Test mock servers use
// this to read the presented token.
func bearerPAT(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
