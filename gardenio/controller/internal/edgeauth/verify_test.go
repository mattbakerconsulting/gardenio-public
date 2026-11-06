package edgeauth

import (
	"encoding/base64"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const verifyTestKey = "test-shared-key"

func TestVerify_Defaults_BurnInMode(t *testing.T) {
	// enforce=false (default), no key configured: outcome=no_key but
	// Trusted=true because we are in burn-in mode.
	cfg := VerifyConfig{}
	req := httptest.NewRequest("GET", "/v1/beads", nil)
	req.Header.Set(VerifyHeaderOperator, "alice@example.com")
	got := cfg.Verify(req)
	if got.Outcome != OutcomeNoKey {
		t.Errorf("outcome=%s, want no_key", got.Outcome)
	}
	if !got.Trusted {
		t.Error("burn-in mode must trust X-Operator even on no_key")
	}
	if got.Operator != "alice@example.com" {
		t.Errorf("operator=%q, want alice@example.com", got.Operator)
	}
}

func TestVerify_Enforce_DropsUnsigned(t *testing.T) {
	cfg := VerifyConfig{Enforce: true}
	req := httptest.NewRequest("GET", "/v1/beads", nil)
	req.Header.Set(VerifyHeaderOperator, "alice@example.com")
	got := cfg.Verify(req)
	if got.Outcome != OutcomeNoKey {
		t.Errorf("outcome=%s, want no_key", got.Outcome)
	}
	if got.Trusted {
		t.Error("enforce=true must drop X-Operator on no_key")
	}
}

func TestVerify_HappyPath_Trusted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cfg := VerifyConfig{
		HMACKey: []byte(verifyTestKey),
		Enforce: true,
		Now:     func() time.Time { return now },
	}
	op := "alice@example.com"
	auditID := "01HXXXXXXXXXXXXXXXXXXXXXXX"
	path := "/v1/beads"
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := base64.StdEncoding.EncodeToString(canonicalHMAC([]byte(verifyTestKey), op, auditID, path, ts))

	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set(VerifyHeaderOperator, op)
	req.Header.Set(VerifyHeaderAuditID, auditID)
	req.Header.Set(VerifyHeaderTimestamp, ts)
	req.Header.Set(VerifyHeaderSignature, sig)

	got := cfg.Verify(req)
	if got.Outcome != OutcomeOK {
		t.Errorf("outcome=%s, want ok", got.Outcome)
	}
	if !got.Trusted {
		t.Error("happy path must be Trusted=true")
	}
	if got.AuditID != auditID {
		t.Errorf("auditID=%q, want %q", got.AuditID, auditID)
	}
}

func TestVerify_BadSig_Outcomes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cfgEnforce := VerifyConfig{
		HMACKey: []byte(verifyTestKey),
		Enforce: true,
		Now:     func() time.Time { return now },
	}
	cfgBurnIn := cfgEnforce
	cfgBurnIn.Enforce = false

	cases := []struct {
		name        string
		mutate      func(r map[string]string)
		wantOutcome VerifyOutcome
	}{
		{
			name:        "no operator",
			mutate:      func(r map[string]string) { delete(r, VerifyHeaderOperator) },
			wantOutcome: OutcomeNoOperator,
		},
		{
			name:        "no sig",
			mutate:      func(r map[string]string) { delete(r, VerifyHeaderSignature) },
			wantOutcome: OutcomeNoSig,
		},
		{
			name:        "no ts",
			mutate:      func(r map[string]string) { delete(r, VerifyHeaderTimestamp) },
			wantOutcome: OutcomeNoSig,
		},
		{
			name:        "stale ts",
			mutate:      func(r map[string]string) { r[VerifyHeaderTimestamp] = "1" },
			wantOutcome: OutcomeStale,
		},
		{
			name:        "non-numeric ts",
			mutate:      func(r map[string]string) { r[VerifyHeaderTimestamp] = "abc" },
			wantOutcome: OutcomeStale,
		},
		{
			name:        "tampered sig",
			mutate:      func(r map[string]string) { r[VerifyHeaderSignature] = base64.StdEncoding.EncodeToString([]byte("nope")) },
			wantOutcome: OutcomeMismatch,
		},
		{
			name:        "non-base64 sig",
			mutate:      func(r map[string]string) { r[VerifyHeaderSignature] = "!!!not-base64!!!" },
			wantOutcome: OutcomeMismatch,
		},
	}

	op := "alice@example.com"
	auditID := "AUDIT"
	path := "/v1/beads"
	ts := strconv.FormatInt(now.Unix(), 10)
	goodSig := base64.StdEncoding.EncodeToString(canonicalHMAC([]byte(verifyTestKey), op, auditID, path, ts))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{
				VerifyHeaderOperator:  op,
				VerifyHeaderAuditID:   auditID,
				VerifyHeaderTimestamp: ts,
				VerifyHeaderSignature: goodSig,
			}
			tc.mutate(headers)
			req := httptest.NewRequest("GET", path, nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			gotEnforce := cfgEnforce.Verify(req)
			if gotEnforce.Outcome != tc.wantOutcome {
				t.Errorf("enforce outcome=%s, want %s", gotEnforce.Outcome, tc.wantOutcome)
			}
			gotBurn := cfgBurnIn.Verify(req)
			if gotBurn.Outcome != tc.wantOutcome {
				t.Errorf("burn-in outcome=%s, want %s", gotBurn.Outcome, tc.wantOutcome)
			}
			// Enforce drops trust on any non-ok outcome (including
			// no-operator where there is nothing to trust anyway).
			if gotEnforce.Trusted {
				t.Error("enforce mode trusted a non-ok outcome")
			}
			// Burn-in mode trusts the asserted X-Operator on any
			// outcome other than OutcomeNoOperator (nothing to trust).
			wantBurnTrust := tc.wantOutcome != OutcomeNoOperator
			if gotBurn.Trusted != wantBurnTrust {
				t.Errorf("burn-in trusted=%v, want %v", gotBurn.Trusted, wantBurnTrust)
			}
		})
	}
}

func TestVerifyConfigFromEnv(t *testing.T) {
	t.Setenv("DAEMON_HMAC_KEY", "abc")
	t.Setenv("DAEMON_HMAC_VERIFY_ENFORCE", "true")
	cfg := VerifyConfigFromEnv()
	if string(cfg.HMACKey) != "abc" {
		t.Errorf("key=%q, want abc", cfg.HMACKey)
	}
	if !cfg.Enforce {
		t.Error("enforce=false, want true")
	}

	t.Setenv("DAEMON_HMAC_KEY", "")
	t.Setenv("DAEMON_HMAC_VERIFY_ENFORCE", "")
	cfg = VerifyConfigFromEnv()
	if len(cfg.HMACKey) != 0 {
		t.Errorf("default key=%q, want empty", cfg.HMACKey)
	}
	if cfg.Enforce {
		t.Error("default enforce=true, want false (safe-deploy default)")
	}
}

func TestEnvFlag_Truthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		t.Setenv("X_VERIFY_FLAG", v)
		if !envFlag("X_VERIFY_FLAG") {
			t.Errorf("%q must be truthy", v)
		}
	}
	for _, v := range []string{"", "0", "false", "bogus"} {
		t.Setenv("X_VERIFY_FLAG", v)
		if envFlag("X_VERIFY_FLAG") {
			t.Errorf("%q must be falsy", v)
		}
	}
}
