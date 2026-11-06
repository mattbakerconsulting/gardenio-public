package edgeauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func makeRequest(t *testing.T, op, auditID, path, ts string, sig []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if op != "" {
		r.Header.Set(HeaderOperator, op)
	}
	if auditID != "" {
		r.Header.Set(HeaderAuditID, auditID)
	}
	if ts != "" {
		r.Header.Set(HeaderTimestamp, ts)
	}
	if sig != nil {
		r.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(sig))
	}
	return r
}

func sign(key []byte, op, auditID, path, ts string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(op + "\n" + auditID + "\n" + path + "\n" + ts))
	return m.Sum(nil)
}

func signV2(
	key []byte,
	op, authMode, roles, auditID, method, path, ts string,
) []byte {
	return canonicalHMACV2(key, op, authMode, roles, auditID, method, path, ts)
}

func makeV2Request(
	t *testing.T,
	key []byte,
	op, authMode, roles, auditID, method, path, ts string,
) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set(HeaderOperator, op)
	r.Header.Set(HeaderAuditID, auditID)
	r.Header.Set(HeaderTimestamp, ts)
	r.Header.Set(HeaderVersion, "2")
	r.Header.Set(HeaderAuthMode, authMode)
	r.Header.Set(HeaderRoles, roles)
	r.Header.Set(
		HeaderSignature,
		base64.StdEncoding.EncodeToString(sign(key, op, auditID, path, ts)),
	)
	r.Header.Set(
		HeaderSignatureV2,
		base64.StdEncoding.EncodeToString(
			signV2(key, op, authMode, roles, auditID, method, path, ts),
		),
	)
	return r
}

func TestVerify_OK(t *testing.T) {
	key := []byte("secret-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := sign(key, "alice@example.com", "audit-1", "/v1/beads", ts)

	cfg := Config{HMACKey: key, Now: func() time.Time { return now }}
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", ts, sig)

	got := cfg.Verify(r)
	if got.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeOK)
	}
	if !got.Trusted {
		t.Fatal("Trusted = false, want true")
	}
	if got.Operator != "alice@example.com" {
		t.Errorf("Operator = %q", got.Operator)
	}
	if got.AuditID != "audit-1" {
		t.Errorf("AuditID = %q", got.AuditID)
	}
}

func TestVerifyV2_OKAndMethodBound(t *testing.T) {
	key := []byte("secret-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	r := makeV2Request(
		t,
		key,
		"alice@example.com",
		"entra",
		"gardenio-operator,gardenio-viewer",
		"audit-2",
		http.MethodPost,
		"/v1/beads",
		ts,
	)
	cfg := Config{HMACKey: key, Now: func() time.Time { return now }}

	got := cfg.Verify(r)
	if got.Outcome != OutcomeOK || !got.VerifiedV2 {
		t.Fatalf("outcome=%q verified_v2=%v, want ok+true", got.Outcome, got.VerifiedV2)
	}
	if got.AuthMode != "entra" {
		t.Errorf("AuthMode=%q, want entra", got.AuthMode)
	}
	if strings.Join(got.Roles, ",") != "gardenio-operator,gardenio-viewer" {
		t.Errorf("Roles=%v", got.Roles)
	}

	r.Method = http.MethodGet
	got = cfg.Verify(r)
	if got.Outcome != OutcomeMismatch || got.VerifiedV2 {
		t.Fatalf("method replay outcome=%q verified_v2=%v, want mismatch+false", got.Outcome, got.VerifiedV2)
	}
	if len(got.Roles) != 0 {
		t.Fatalf("unverified roles escaped verification: %v", got.Roles)
	}
}

// The slack-bridge signs a v2 envelope with auth_mode "slack" for the
// Keycloak-resolved person behind a Slack user (gd-7CDM9gdog0). An unknown
// mode must still be refused.
func TestVerifyV2_SlackAuthMode(t *testing.T) {
	key := []byte("secret-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	cfg := Config{HMACKey: key, Now: func() time.Time { return now }}

	r := makeV2Request(t, key, "oidc:owner@example.com", "slack",
		"gardenio-admin", "audit-slack", http.MethodPost, "/v1/beads", ts)
	got := cfg.Verify(r)
	if got.Outcome != OutcomeOK || !got.VerifiedV2 || got.AuthMode != "slack" {
		t.Fatalf("outcome=%q verified_v2=%v mode=%q, want ok+true+slack", got.Outcome, got.VerifiedV2, got.AuthMode)
	}

	r = makeV2Request(t, key, "oidc:owner@example.com", "carrier-pigeon",
		"gardenio-admin", "audit-bad", http.MethodPost, "/v1/beads", ts)
	if got := cfg.Verify(r); got.Outcome != OutcomeMalformed || got.VerifiedV2 {
		t.Fatalf("unknown mode outcome=%q verified_v2=%v, want malformed+false", got.Outcome, got.VerifiedV2)
	}
}

func TestVerifyV2_RejectsNonCanonicalRoles(t *testing.T) {
	key := []byte("secret-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	r := makeV2Request(
		t,
		key,
		"alice@example.com",
		"entra",
		"gardenio-viewer,gardenio-operator",
		"audit-2",
		http.MethodGet,
		"/v1/beads",
		ts,
	)
	got := (Config{HMACKey: key, Now: func() time.Time { return now }}).Verify(r)
	if got.Outcome != OutcomeMalformed || got.VerifiedV2 {
		t.Fatalf("outcome=%q verified_v2=%v, want malformed+false", got.Outcome, got.VerifiedV2)
	}
}

func TestVerifyV2_DoesNotFallBackToLegacySignature(t *testing.T) {
	key := []byte("secret-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	r := makeV2Request(
		t,
		key,
		"alice@example.com",
		"entra",
		"gardenio-admin",
		"audit-2",
		http.MethodPost,
		"/v1/beads",
		ts,
	)
	// The dual envelope still contains a valid v1 signature for rolling read
	// compatibility. Removing the v2 signature must not let verification
	// downgrade to that legacy field.
	r.Header.Del(HeaderSignatureV2)
	got := (Config{HMACKey: key, Now: func() time.Time { return now }}).Verify(r)
	if got.Outcome != OutcomeNoSig || got.VerifiedV2 {
		t.Fatalf("outcome=%q verified_v2=%v, want no_sig+false", got.Outcome, got.VerifiedV2)
	}
	if len(got.Roles) != 0 {
		t.Fatalf("downgraded v1 signature retained roles: %v", got.Roles)
	}
}

func TestCanonicalHMACV2_CrossLanguageKnownVector(t *testing.T) {
	got := base64.StdEncoding.EncodeToString(canonicalHMACV2(
		[]byte("shared-secret"),
		"operator@acme.example",
		"entra",
		"gardenio-operator,gardenio-viewer",
		"audit-123",
		http.MethodPost,
		"/v1/beads",
		"1700000000",
	))
	const want = "KNAEp9aRG8u2NQXkBgNsyD/R3X7l6RdnkI+CFL7mkBY="
	if got != want {
		t.Fatalf("v2 wire vector=%q, want %q", got, want)
	}
}

func TestVerify_NoOperator(t *testing.T) {
	cfg := Config{HMACKey: []byte("k")}
	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	got := cfg.Verify(r)
	if got.Outcome != OutcomeNoOperator {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeNoOperator)
	}
	if got.Trusted {
		t.Fatal("Trusted = true, want false (nothing to trust)")
	}
}

func TestVerify_NoKey(t *testing.T) {
	cfg := Config{} // no HMAC key
	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	r.Header.Set(HeaderOperator, "alice@example.com")
	got := cfg.Verify(r)
	if got.Outcome != OutcomeNoKey {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeNoKey)
	}
	if !got.Trusted {
		t.Fatal("burn-in: Trusted should be true when Enforce=false")
	}
}

func TestVerify_NoKey_Enforce(t *testing.T) {
	cfg := Config{Enforce: true}
	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	r.Header.Set(HeaderOperator, "alice@example.com")
	got := cfg.Verify(r)
	if got.Trusted {
		t.Fatal("enforce: Trusted should be false when no key configured")
	}
}

func TestVerify_NoSig(t *testing.T) {
	cfg := Config{HMACKey: []byte("k")}
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", "", nil)
	got := cfg.Verify(r)
	if got.Outcome != OutcomeNoSig {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeNoSig)
	}
	if !got.Trusted {
		t.Fatal("burn-in: Trusted should be true")
	}
}

func TestVerify_NoSig_Enforce(t *testing.T) {
	cfg := Config{HMACKey: []byte("k"), Enforce: true}
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", "", nil)
	got := cfg.Verify(r)
	if got.Outcome != OutcomeNoSig {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeNoSig)
	}
	if got.Trusted {
		t.Fatal("enforce: Trusted should be false on missing sig")
	}
}

func TestVerify_Stale(t *testing.T) {
	key := []byte("secret-key")
	now := time.Unix(1_700_000_000, 0)
	staleTs := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
	sig := sign(key, "alice@example.com", "audit-1", "/v1/beads", staleTs)

	cfg := Config{HMACKey: key, Now: func() time.Time { return now }}
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", staleTs, sig)
	got := cfg.Verify(r)
	if got.Outcome != OutcomeStale {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeStale)
	}
}

func TestVerify_Stale_FutureTs(t *testing.T) {
	key := []byte("secret-key")
	now := time.Unix(1_700_000_000, 0)
	futureTs := strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)
	sig := sign(key, "alice@example.com", "audit-1", "/v1/beads", futureTs)

	cfg := Config{HMACKey: key, Now: func() time.Time { return now }}
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", futureTs, sig)
	got := cfg.Verify(r)
	if got.Outcome != OutcomeStale {
		t.Fatalf("outcome = %q, want %q (positive skew should also be stale)", got.Outcome, OutcomeStale)
	}
}

func TestVerify_Stale_BadTimestamp(t *testing.T) {
	cfg := Config{HMACKey: []byte("k")}
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", "not-a-number", []byte("sig"))
	got := cfg.Verify(r)
	if got.Outcome != OutcomeStale {
		t.Fatalf("outcome = %q, want %q (non-numeric ts must be stale)", got.Outcome, OutcomeStale)
	}
}

func TestVerify_Mismatch_WrongKey(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := sign([]byte("attacker-key"), "alice@example.com", "audit-1", "/v1/beads", ts)

	cfg := Config{HMACKey: []byte("real-key"), Now: func() time.Time { return now }}
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", ts, sig)
	got := cfg.Verify(r)
	if got.Outcome != OutcomeMismatch {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeMismatch)
	}
}

func TestVerify_Mismatch_TamperedPath(t *testing.T) {
	key := []byte("secret-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := sign(key, "alice@example.com", "audit-1", "/v1/beads", ts)

	cfg := Config{HMACKey: key, Now: func() time.Time { return now }}
	// request hits /admin but sig was for /v1/beads
	r := makeRequest(t, "alice@example.com", "audit-1", "/admin", ts, sig)
	got := cfg.Verify(r)
	if got.Outcome != OutcomeMismatch {
		t.Fatalf("outcome = %q, want %q (path swap must invalidate sig)", got.Outcome, OutcomeMismatch)
	}
}

func TestVerify_Mismatch_BadBase64(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	cfg := Config{HMACKey: []byte("k"), Now: func() time.Time { return now }}
	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	r.Header.Set(HeaderOperator, "alice@example.com")
	r.Header.Set(HeaderAuditID, "audit-1")
	r.Header.Set(HeaderTimestamp, ts)
	r.Header.Set(HeaderSignature, "!!!not-base64!!!")
	got := cfg.Verify(r)
	if got.Outcome != OutcomeMismatch {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeMismatch)
	}
}

func TestVerify_BurnInVsEnforce(t *testing.T) {
	key := []byte("real-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	// sign with wrong key → Mismatch
	sig := sign([]byte("wrong-key"), "alice@example.com", "audit-1", "/v1/beads", ts)
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", ts, sig)

	burnIn := Config{HMACKey: key, Enforce: false, Now: func() time.Time { return now }}
	if got := burnIn.Verify(r); got.Outcome != OutcomeMismatch || !got.Trusted {
		t.Fatalf("burn-in: outcome=%q trusted=%v, want mismatch+trusted=true", got.Outcome, got.Trusted)
	}

	enforce := Config{HMACKey: key, Enforce: true, Now: func() time.Time { return now }}
	if got := enforce.Verify(r); got.Outcome != OutcomeMismatch || got.Trusted {
		t.Fatalf("enforce: outcome=%q trusted=%v, want mismatch+trusted=false", got.Outcome, got.Trusted)
	}
}

func TestVerify_MaxSkew_Custom(t *testing.T) {
	key := []byte("k")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Add(-30*time.Second).Unix(), 10)
	sig := sign(key, "alice@example.com", "audit-1", "/v1/beads", ts)

	// 10s window → 30s drift is stale
	cfg := Config{HMACKey: key, MaxSkew: 10 * time.Second, Now: func() time.Time { return now }}
	r := makeRequest(t, "alice@example.com", "audit-1", "/v1/beads", ts, sig)
	if got := cfg.Verify(r); got.Outcome != OutcomeStale {
		t.Fatalf("outcome = %q, want stale with 10s skew", got.Outcome)
	}

	// 1min window → same drift accepted
	cfg.MaxSkew = time.Minute
	if got := cfg.Verify(r); got.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q, want ok with 1m skew", got.Outcome)
	}
}

func TestConfig_Enabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("empty Config.Enabled() = true, want false")
	}
	if !(Config{HMACKey: []byte("k")}).Enabled() {
		t.Error("Config{HMACKey: nonempty}.Enabled() = false, want true")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("DAEMON_HMAC_KEY", "shared")
	t.Setenv("DAEMON_HMAC_VERIFY_ENFORCE", "true")
	cfg := ConfigFromEnv()
	if string(cfg.HMACKey) != "shared" {
		t.Errorf("HMACKey = %q", string(cfg.HMACKey))
	}
	if !cfg.Enforce {
		t.Error("Enforce = false, want true")
	}
}

func TestEnvFlag(t *testing.T) {
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, "yes": true, "on": true,
		"0": false, "false": false, "no": false, "off": false, "": false,
	}
	for v, want := range cases {
		t.Setenv("X_FLAG", v)
		if got := envFlag("X_FLAG"); got != want {
			t.Errorf("envFlag(%q) = %v, want %v", v, got, want)
		}
	}
}

// With HMACKeyV2 set, a v2 envelope verifies only under the v2 key: the
// shared v1 key alone cannot mint one (gd-dMof1PW1oJ). v1 envelopes keep the
// shared key, and an unset HMACKeyV2 falls back to it for v2 as well.
func TestVerifyV2_SplitKey(t *testing.T) {
	v1Key, v2Key := []byte("v1-key"), []byte("v2-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	split := Config{HMACKey: v1Key, HMACKeyV2: v2Key, Now: func() time.Time { return now }}
	v2 := func(key []byte) *http.Request {
		return makeV2Request(t, key, "alice@example.com", "entra", "gardenio-admin",
			"audit-split", http.MethodPost, "/v1/beads", ts)
	}

	if got := split.Verify(v2(v2Key)); got.Outcome != OutcomeOK || !got.VerifiedV2 {
		t.Errorf("v2 signed with the v2 key: outcome=%q verified_v2=%v, want ok", got.Outcome, got.VerifiedV2)
	}
	if got := split.Verify(v2(v1Key)); got.Outcome != OutcomeMismatch || got.VerifiedV2 || len(got.Roles) != 0 {
		t.Errorf("v2 signed with the v1 key: outcome=%q verified_v2=%v roles=%v, want mismatch", got.Outcome, got.VerifiedV2, got.Roles)
	}
	v1 := makeRequest(t, "alice@example.com", "audit-split", "/v1/beads", ts, sign(v1Key, "alice@example.com", "audit-split", "/v1/beads", ts))
	if got := split.Verify(v1); got.Outcome != OutcomeOK {
		t.Errorf("v1 signed with the v1 key: outcome=%q, want ok", got.Outcome)
	}

	fallback := Config{HMACKey: v1Key, Now: func() time.Time { return now }}
	if got := fallback.Verify(v2(v1Key)); got.Outcome != OutcomeOK || !got.VerifiedV2 {
		t.Errorf("no v2 key: v2 under the shared key outcome=%q, want ok (pre-split behaviour)", got.Outcome)
	}
	if !(Config{HMACKeyV2: v2Key}).Enabled() {
		t.Error("Config{HMACKeyV2: nonempty}.Enabled() = false, want true")
	}
}

func TestConfigFromEnv_V2Key(t *testing.T) {
	t.Setenv("DAEMON_HMAC_KEY", "shared")
	t.Setenv("DAEMON_HMAC_KEY_V2", "v2")
	if cfg := ConfigFromEnv(); string(cfg.HMACKeyV2) != "v2" {
		t.Errorf("HMACKeyV2 = %q, want v2", string(cfg.HMACKeyV2))
	}
}
