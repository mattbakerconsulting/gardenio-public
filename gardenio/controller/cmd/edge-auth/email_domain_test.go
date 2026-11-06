package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestProxy_EmailDomainGate exercises the optional email-domain allowlist
// applied AFTER the group-membership gate (ka-0000000533). The authenticated
// PAT "good-pat" resolves to alice@example.com and IS in the gardenio group, so
// the only variable under test is whether her email domain clears the gate.
func TestProxy_EmailDomainGate(t *testing.T) {
	tests := []struct {
		name           string
		allowedDomains []string
		wantStatus     int
		wantReachUp    bool
	}{
		{
			// Default / unconfigured: gate OFF — group membership is the sole
			// authority, preserving the shim's historical behaviour.
			name:           "gate off admits any domain",
			allowedDomains: nil,
			wantStatus:     http.StatusOK,
			wantReachUp:    true,
		},
		{
			name:           "gate on, domain in allowlist admitted",
			allowedDomains: []string{"example.com"},
			wantStatus:     http.StatusOK,
			wantReachUp:    true,
		},
		{
			// alice@example.com is in-group but example.com is not in the
			// allowlist → denied with 403 before reaching the upstream.
			name:           "gate on, domain not in allowlist denied",
			allowedDomains: []string{"acme.example", "corp.example"},
			wantStatus:     http.StatusForbidden,
			wantReachUp:    false,
		},
		{
			name:           "gate on, allowlist is case-insensitive",
			allowedDomains: []string{"EXAMPLE.COM"},
			wantStatus:     http.StatusOK,
			wantReachUp:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base, upstream, fx := newProxyTestServerWithCapture(t, "test-hmac-key", func(c *config) {
				c.allowedEmailDomains = tc.allowedDomains
			})
			defer fx.close()

			req, _ := http.NewRequest("GET", base+"/kd/v1/beads", strings.NewReader(""))
			req.Header.Set("Authorization", "Bearer good-pat")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status=%d, want %d (body=%s)", resp.StatusCode, tc.wantStatus, body)
			}
			if reached := upstream.gotPath != ""; reached != tc.wantReachUp {
				t.Errorf("upstream reached=%v, want %v", reached, tc.wantReachUp)
			}
			if tc.wantStatus == http.StatusForbidden && !strings.Contains(string(body), "email_domain_not_allowed") {
				t.Errorf("403 body = %q, want it to mention email_domain_not_allowed", body)
			}
		})
	}
}

// TestEmailDomainAllowed unit-tests the pure allowlist helper, including the
// off-by-default (empty allowlist) and malformed-email edge cases.
func TestEmailDomainAllowed(t *testing.T) {
	tests := []struct {
		name    string
		email   string
		allowed []string
		want    bool
	}{
		{"empty allowlist allows anything", "x@anywhere.io", nil, true},
		{"empty allowlist allows malformed", "no-at-sign", nil, true},
		{"match", "a@acme.example", []string{"acme.example"}, true},
		{"no match", "a@evil.com", []string{"acme.example"}, false},
		{"case-insensitive email domain", "a@ACME.Example", []string{"acme.example"}, true},
		{"case-insensitive allowlist entry", "a@acme.example", []string{"ACME.Example"}, true},
		{"second entry matches", "a@corp.example", []string{"acme.example", "corp.example"}, true},
		{"malformed denied when gate on", "no-at-sign", []string{"acme.example"}, false},
		{"empty email denied when gate on", "", []string{"acme.example"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := emailDomainAllowed(tc.email, tc.allowed); got != tc.want {
				t.Errorf("emailDomainAllowed(%q, %v) = %v, want %v", tc.email, tc.allowed, got, tc.want)
			}
		})
	}
}

// TestParseCommaSeparated covers the env-parsing helper that feeds the gate:
// trimming, lower-casing, empty-segment dropping, and nil-on-empty.
func TestParseCommaSeparated(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"ACME.Example", []string{"acme.example"}},
		{"acme.example,CORP.example", []string{"acme.example", "corp.example"}},
		{" acme.example , corp.example ", []string{"acme.example", "corp.example"}},
		{"a,,b,", []string{"a", "b"}},
	}
	for _, tc := range tests {
		got := parseCommaSeparated(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseCommaSeparated(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseCommaSeparated(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
