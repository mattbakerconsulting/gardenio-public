//go:build integration

// Integration tests for gardenio-edge-auth /auth/check (W-B-8, ka-0000005404).
//
// These tests exercise the /auth/check endpoint end-to-end against a real
// GitLab instance using real (ephemeral) PATs. They cover the four PAT
// dispositions called out in the Stage 3 test pyramid (ka-0000008114):
//
//   - valid, in-group PAT          -> HTTP 200
//   - expired PAT                  -> HTTP 401
//   - revoked PAT                  -> HTTP 401
//   - wrong-scope / not-in-group   -> HTTP 403
//
// They are dormant under the default `go test` invocation — guarded by
// both:
//
//  1. The `integration` build tag, so plain `go test ./...` (and ordinary
//     CI) never compiles this file. The other unit tests in this package
//     still run.
//  2. A runtime skip when GITLAB_TEST_URL is unset, so the file compiles
//     cleanly under `-tags=integration` without spuriously failing when
//     no GitLab fixture is available (e.g. in IDE / pre-fixture CI).
//
// # Running locally
//
// Provision an ephemeral GitLab project + group + four PATs (the
// fixture-provisioner script is out of scope here; see ka-0000008114),
// then:
//
//	GITLAB_TEST_URL=https://gitlab.example.com \
//	GITLAB_TEST_GROUP=my-group \
//	GITLAB_TEST_VALID_PAT=glpat-xxxxxxxxxxxxxxxxxxxx \
//	GITLAB_TEST_EXPIRED_PAT=glpat-yyyyyyyyyyyyyyyyyyyy \
//	GITLAB_TEST_REVOKED_PAT=glpat-zzzzzzzzzzzzzzzzzzzz \
//	GITLAB_TEST_WRONG_SCOPE_PAT=glpat-wwwwwwwwwwwwwwwwwwww \
//	go test -tags=integration -v -run TestPATValidation_Integration \
//	    ./gardenio/controller/cmd/edge-auth/...
//
// Any PAT env var that is empty causes its sub-test to be skipped, so a
// partial fixture (e.g. valid + expired only) still exercises what it
// can. GITLAB_TEST_URL + GITLAB_TEST_GROUP are required.
//
// The "wrong scope" case is implemented as a PAT whose owner is not a
// member of GITLAB_TEST_GROUP — that is the only disposition the
// edge-auth shim can map to HTTP 403 (ErrNotInGroup). A scoped-token
// without `read_user` would 401 at the GitLab /user endpoint, which the
// shim correctly maps to 401, not 403; the fixture script must therefore
// mint the wrong-scope PAT under an account outside the auth group.

package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const (
	envGitLabURL          = "GITLAB_TEST_URL"
	envGitLabGroup        = "GITLAB_TEST_GROUP"
	envValidPAT           = "GITLAB_TEST_VALID_PAT"
	envExpiredPAT         = "GITLAB_TEST_EXPIRED_PAT"
	envRevokedPAT         = "GITLAB_TEST_REVOKED_PAT"
	envWrongScopePAT      = "GITLAB_TEST_WRONG_SCOPE_PAT"
)

func TestPATValidation_Integration(t *testing.T) {
	baseURL := os.Getenv(envGitLabURL)
	group := os.Getenv(envGitLabGroup)
	if baseURL == "" || group == "" {
		t.Skipf("integration test dormant: set %s and %s to run against a real GitLab instance",
			envGitLabURL, envGitLabGroup)
	}

	cfg := &config{
		gitlabBaseURL:   strings.TrimRight(baseURL, "/"),
		gitlabAuthGroup: group,
	}
	srv := newServer(cfg, "integration-test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if ok, reason := srv.ready(); !ok {
		t.Fatalf("server not ready: %s (GITLAB_TEST_URL=%q GITLAB_TEST_GROUP=%q)",
			reason, baseURL, group)
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	cases := []struct {
		name   string
		envVar string
		want   int
	}{
		{"valid_PAT_in_group_returns_200", envValidPAT, http.StatusOK},
		{"expired_PAT_returns_401", envExpiredPAT, http.StatusUnauthorized},
		{"revoked_PAT_returns_401", envRevokedPAT, http.StatusUnauthorized},
		{"wrong_scope_or_out_of_group_PAT_returns_403", envWrongScopePAT, http.StatusForbidden},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pat := os.Getenv(tc.envVar)
			if pat == "" {
				t.Skipf("set %s to exercise this case", tc.envVar)
			}
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/auth/check", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+pat)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("call /auth/check: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
			if resp.StatusCode != tc.want {
				t.Errorf("/auth/check status=%d body=%q, want %d",
					resp.StatusCode, body, tc.want)
			}
		})
	}
}
