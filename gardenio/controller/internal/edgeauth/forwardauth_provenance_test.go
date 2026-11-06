package edgeauth

import (
	"encoding/base64"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// fixedNow returns a Now func pinned to t, for deterministic skew checks.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestSignVerifyRoundTrip(t *testing.T) {
	key := []byte("shared-edge-auth-secret")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	email, groups, auditID := "approver@acme.example", "grp-a,grp-b", "audit-123"

	sig := SignForwardAuth(key, email, groups, auditID, ts)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, email)
	r.Header.Set(ForwardAuthHeaderGroups, groups)
	r.Header.Set(ForwardAuthHeaderAuditID, auditID)
	r.Header.Set(ForwardAuthHeaderSig, sig)
	r.Header.Set(ForwardAuthHeaderTs, ts)

	cfg := ForwardAuthVerifyConfig{HMACKey: key, Enforce: true, Now: fixedNow(now)}
	got := cfg.Verify(r)
	if got.Outcome != FwdOutcomeOK {
		t.Fatalf("outcome = %q, want ok", got.Outcome)
	}
	if !got.Trusted {
		t.Fatalf("trusted = false, want true for a valid envelope in enforce mode")
	}
	if got.Email != email || got.AuditID != auditID {
		t.Fatalf("echoed identity = (%q,%q), want (%q,%q)", got.Email, got.AuditID, email, auditID)
	}
}

func TestVerifyEmptyGroupsRoundTrip(t *testing.T) {
	// oauth2-proxy omits the groups header when the user is in no groups, so the
	// signed groups string is "" and the header is absent. Both sides must agree.
	key := []byte("k")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	email, auditID := "a@b.c", "aud"

	sig := SignForwardAuth(key, email, "", auditID, ts)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, email)
	// no groups header set
	r.Header.Set(ForwardAuthHeaderAuditID, auditID)
	r.Header.Set(ForwardAuthHeaderSig, sig)
	r.Header.Set(ForwardAuthHeaderTs, ts)

	cfg := ForwardAuthVerifyConfig{HMACKey: key, Enforce: true, Now: fixedNow(now)}
	if got := cfg.Verify(r); got.Outcome != FwdOutcomeOK || !got.Trusted {
		t.Fatalf("empty-groups roundtrip: outcome=%q trusted=%v, want ok/true", got.Outcome, got.Trusted)
	}
}

func TestVerifyTamperedIdentityMismatch(t *testing.T) {
	// A caller that forges a different email but replays a real signature must
	// fail: the HMAC binds the identity, so the recompute won't match.
	key := []byte("k")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)

	sig := SignForwardAuth(key, "real@acme.example", "", "aud", ts)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, "attacker@evil.example") // swapped
	r.Header.Set(ForwardAuthHeaderAuditID, "aud")
	r.Header.Set(ForwardAuthHeaderSig, sig)
	r.Header.Set(ForwardAuthHeaderTs, ts)

	enforce := ForwardAuthVerifyConfig{HMACKey: key, Enforce: true, Now: fixedNow(now)}
	if got := enforce.Verify(r); got.Outcome != FwdOutcomeMismatch || got.Trusted {
		t.Fatalf("tampered identity: outcome=%q trusted=%v, want mismatch/false", got.Outcome, got.Trusted)
	}
}

func TestVerifyWrongKeyMismatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := SignForwardAuth([]byte("edge-key"), "a@b.c", "", "aud", ts)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, "a@b.c")
	r.Header.Set(ForwardAuthHeaderAuditID, "aud")
	r.Header.Set(ForwardAuthHeaderSig, sig)
	r.Header.Set(ForwardAuthHeaderTs, ts)

	cfg := ForwardAuthVerifyConfig{HMACKey: []byte("portal-has-a-different-key"), Enforce: true, Now: fixedNow(now)}
	if got := cfg.Verify(r); got.Outcome != FwdOutcomeMismatch || got.Trusted {
		t.Fatalf("wrong key: outcome=%q trusted=%v, want mismatch/false", got.Outcome, got.Trusted)
	}
}

func TestVerifyStaleTimestamp(t *testing.T) {
	key := []byte("k")
	signedAt := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(signedAt.Unix(), 10)
	sig := SignForwardAuth(key, "a@b.c", "", "aud", ts)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, "a@b.c")
	r.Header.Set(ForwardAuthHeaderAuditID, "aud")
	r.Header.Set(ForwardAuthHeaderSig, sig)
	r.Header.Set(ForwardAuthHeaderTs, ts)

	// Verify 10 minutes later with the default 5-minute skew.
	cfg := ForwardAuthVerifyConfig{HMACKey: key, Enforce: true, Now: fixedNow(signedAt.Add(10 * time.Minute))}
	if got := cfg.Verify(r); got.Outcome != FwdOutcomeStale || got.Trusted {
		t.Fatalf("stale ts: outcome=%q trusted=%v, want stale/false", got.Outcome, got.Trusted)
	}
}

func TestVerifyUnparseableTimestampStale(t *testing.T) {
	key := []byte("k")
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, "a@b.c")
	r.Header.Set(ForwardAuthHeaderAuditID, "aud")
	r.Header.Set(ForwardAuthHeaderSig, "AAAA")
	r.Header.Set(ForwardAuthHeaderTs, "not-a-number")

	cfg := ForwardAuthVerifyConfig{HMACKey: key, Enforce: true, Now: fixedNow(time.Unix(1_700_000_000, 0))}
	if got := cfg.Verify(r); got.Outcome != FwdOutcomeStale {
		t.Fatalf("unparseable ts: outcome=%q, want stale", got.Outcome)
	}
}

func TestVerifyNoIdentityNeverTrusted(t *testing.T) {
	// No email at all: there is nothing to vouch for. Never trusted, in EITHER
	// mode — even burn-in must not manufacture trust from an empty identity.
	key := []byte("k")
	r := httptest.NewRequest("GET", "/", nil)

	for _, enforce := range []bool{false, true} {
		cfg := ForwardAuthVerifyConfig{HMACKey: key, Enforce: enforce, Now: fixedNow(time.Unix(1_700_000_000, 0))}
		got := cfg.Verify(r)
		if got.Outcome != FwdOutcomeNoIdentity {
			t.Fatalf("enforce=%v: outcome=%q, want no_identity", enforce, got.Outcome)
		}
		if got.Trusted {
			t.Fatalf("enforce=%v: trusted=true for no identity, want false", enforce)
		}
	}
}

func TestVerifyNoKeyBurnInVsEnforce(t *testing.T) {
	// With no HMAC key the verifier cannot run. Burn-in trusts a present
	// identity (pre-envelope behaviour); enforce fails closed.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, "a@b.c")

	burn := ForwardAuthVerifyConfig{HMACKey: nil, Enforce: false}
	if got := burn.Verify(r); got.Outcome != FwdOutcomeNoKey || !got.Trusted {
		t.Fatalf("no-key burn-in: outcome=%q trusted=%v, want no_key/true", got.Outcome, got.Trusted)
	}

	enforce := ForwardAuthVerifyConfig{HMACKey: nil, Enforce: true}
	if got := enforce.Verify(r); got.Outcome != FwdOutcomeNoKey || got.Trusted {
		t.Fatalf("no-key enforce: outcome=%q trusted=%v, want no_key/false", got.Outcome, got.Trusted)
	}
}

func TestVerifyNoSigBurnInVsEnforce(t *testing.T) {
	// Identity present, key configured, but no envelope (sig/ts absent). This is
	// BOTH the forged-in-cluster shape and the pre-cutover shape (edge not yet
	// signing) — burn-in trusts, enforce refuses.
	key := []byte("k")
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, "a@b.c")

	burn := ForwardAuthVerifyConfig{HMACKey: key, Enforce: false}
	if got := burn.Verify(r); got.Outcome != FwdOutcomeNoSig || !got.Trusted {
		t.Fatalf("no-sig burn-in: outcome=%q trusted=%v, want no_sig/true", got.Outcome, got.Trusted)
	}

	enforce := ForwardAuthVerifyConfig{HMACKey: key, Enforce: true}
	if got := enforce.Verify(r); got.Outcome != FwdOutcomeNoSig || got.Trusted {
		t.Fatalf("no-sig enforce: outcome=%q trusted=%v, want no_sig/false", got.Outcome, got.Trusted)
	}
}

func TestVerifyMismatchBurnInStillTrusts(t *testing.T) {
	// Even a hard mismatch is trusted in burn-in — the whole point is that
	// flipping enforce is what changes behaviour, so a soak can surface
	// mismatches without breaking anyone.
	key := []byte("k")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, "a@b.c")
	r.Header.Set(ForwardAuthHeaderAuditID, "aud")
	r.Header.Set(ForwardAuthHeaderSig, "d29uZy1zaWc=") // "wong-sig"
	r.Header.Set(ForwardAuthHeaderTs, ts)

	burn := ForwardAuthVerifyConfig{HMACKey: key, Enforce: false, Now: fixedNow(now)}
	if got := burn.Verify(r); got.Outcome != FwdOutcomeMismatch || !got.Trusted {
		t.Fatalf("mismatch burn-in: outcome=%q trusted=%v, want mismatch/true", got.Outcome, got.Trusted)
	}
}

func TestDomainSeparationFromDaemonEnvelope(t *testing.T) {
	// A daemon-envelope signature (signEdgeAuth, verify.go) must NOT verify as a
	// forwardauth-provenance signature even with the same key/identity/ts — the
	// "fwdauth-v1" prefix domain-separates the two schemes.
	key := []byte("shared-key")
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	email, auditID := "a@b.c", "aud"

	// A daemon-envelope signature over the same fields (canonicalHMAC is the
	// in-package shape cmd/edge-auth/proxy.go signEdgeAuth mirrors).
	daemonSig := base64.StdEncoding.EncodeToString(canonicalHMAC(key, email, auditID, "/some/path", ts))

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, email)
	r.Header.Set(ForwardAuthHeaderAuditID, auditID)
	r.Header.Set(ForwardAuthHeaderSig, daemonSig)
	r.Header.Set(ForwardAuthHeaderTs, ts)

	cfg := ForwardAuthVerifyConfig{HMACKey: key, Enforce: true, Now: fixedNow(now)}
	if got := cfg.Verify(r); got.Outcome == FwdOutcomeOK {
		t.Fatal("daemon-envelope signature verified as forwardauth provenance — domain separation broken")
	}
}

func TestCustomMaxSkew(t *testing.T) {
	key := []byte("k")
	signedAt := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(signedAt.Unix(), 10)
	sig := SignForwardAuth(key, "a@b.c", "", "aud", ts)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ForwardAuthHeaderEmail, "a@b.c")
	r.Header.Set(ForwardAuthHeaderAuditID, "aud")
	r.Header.Set(ForwardAuthHeaderSig, sig)
	r.Header.Set(ForwardAuthHeaderTs, ts)

	// 2 minutes later, but MaxSkew is only 30s => stale.
	cfg := ForwardAuthVerifyConfig{HMACKey: key, Enforce: true, MaxSkew: 30 * time.Second, Now: fixedNow(signedAt.Add(2 * time.Minute))}
	if got := cfg.Verify(r); got.Outcome != FwdOutcomeStale {
		t.Fatalf("custom skew: outcome=%q, want stale", got.Outcome)
	}
}
