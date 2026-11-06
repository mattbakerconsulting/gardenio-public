// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Tests for the edge-auth HMAC verification middleware. Mirrors
//! `kbeads/internal/edgeauth/verify_test.go`.

use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::Result;
use axum::http::{HeaderMap, HeaderValue};
use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine as _;
use ring::hmac;

use super::*;

const REF_EPOCH: u64 = 1_700_000_000;

fn at_epoch(secs: u64) -> SystemTime {
    UNIX_EPOCH + Duration::from_secs(secs)
}

fn ref_now() -> SystemTime {
    at_epoch(REF_EPOCH)
}

fn sign_bytes(key: &[u8], op: &str, audit: &str, path: &str, ts: &str) -> Vec<u8> {
    let k = hmac::Key::new(hmac::HMAC_SHA256, key);
    let msg = format!("{op}\n{audit}\n{path}\n{ts}");
    hmac::sign(&k, msg.as_bytes()).as_ref().to_vec()
}

fn sign_b64(key: &[u8], op: &str, audit: &str, path: &str, ts: &str) -> String {
    B64.encode(sign_bytes(key, op, audit, path, ts))
}

#[allow(clippy::too_many_arguments)]
fn sign_v2_b64(
    key: &[u8],
    op: &str,
    auth_mode: &str,
    roles: &str,
    audit: &str,
    method: &str,
    path: &str,
    ts: &str,
) -> String {
    let k = hmac::Key::new(hmac::HMAC_SHA256, key);
    let msg = format!(
        "v2\n{op}\n{auth_mode}\n{roles}\n{audit}\n{}\n{path}\n{ts}",
        method.to_ascii_uppercase()
    );
    B64.encode(hmac::sign(&k, msg.as_bytes()).as_ref())
}

fn build_headers(op: &str, audit: &str, ts: &str, sig: Option<&str>) -> Result<HeaderMap> {
    let mut h = HeaderMap::new();
    if !op.is_empty() {
        h.insert(HEADER_OPERATOR, HeaderValue::from_str(op)?);
    }
    if !audit.is_empty() {
        h.insert(HEADER_AUDIT_ID, HeaderValue::from_str(audit)?);
    }
    if !ts.is_empty() {
        h.insert(HEADER_TIMESTAMP, HeaderValue::from_str(ts)?);
    }
    if let Some(s) = sig {
        h.insert(HEADER_SIGNATURE, HeaderValue::from_str(s)?);
    }
    Ok(h)
}

fn ref_cfg(key: &[u8]) -> Config {
    Config { hmac_key: key.to_vec(), enforce: false, now: Some(ref_now), ..Config::default() }
}

#[allow(clippy::too_many_arguments)]
fn build_v2_headers(
    key: &[u8],
    op: &str,
    auth_mode: &str,
    roles: &str,
    audit: &str,
    method: &str,
    path: &str,
    ts: &str,
) -> Result<HeaderMap> {
    let sig = sign_v2_b64(key, op, auth_mode, roles, audit, method, path, ts);
    let legacy_sig = sign_b64(key, op, audit, path, ts);
    let mut headers = build_headers(op, audit, ts, Some(&legacy_sig))?;
    headers.insert(HEADER_VERSION, HeaderValue::from_static("2"));
    headers.insert(HEADER_AUTH_MODE, HeaderValue::from_str(auth_mode)?);
    headers.insert(HEADER_ROLES, HeaderValue::from_str(roles)?);
    headers.insert(HEADER_SIGNATURE_V2, HeaderValue::from_str(&sig)?);
    Ok(headers)
}

#[test]
fn verify_ok() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    let sig = sign_b64(key, "alice@example.com", "audit-1", "/v1/beads", &ts);
    let h = build_headers("alice@example.com", "audit-1", &ts, Some(&sig))?;

    let got = ref_cfg(key).verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Ok);
    assert!(got.trusted);
    assert_eq!(got.operator, "alice@example.com");
    assert_eq!(got.audit_id, "audit-1");
    Ok(())
}

#[test]
fn verify_v2_ok_and_method_bound() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    let h = build_v2_headers(
        key,
        "alice@example.com",
        "entra",
        "gardenio-operator,gardenio-viewer",
        "audit-2",
        "POST",
        "/api/v1/sessions/s1/input",
        &ts,
    )?;

    let got = ref_cfg(key).verify(&h, "POST", "/api/v1/sessions/s1/input");
    assert_eq!(got.outcome, Outcome::Ok);
    assert!(got.verified_v2);
    assert_eq!(got.version, 2);
    assert_eq!(got.auth_mode, "entra");
    assert_eq!(got.roles, vec!["gardenio-operator", "gardenio-viewer"]);

    let replay = ref_cfg(key).verify(&h, "GET", "/api/v1/sessions/s1/input");
    assert_eq!(replay.outcome, Outcome::Mismatch);
    assert!(!replay.verified_v2);
    assert!(replay.roles.is_empty(), "unverified roles must be discarded");
    Ok(())
}

#[test]
fn verify_v2_rejects_noncanonical_roles() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    let h = build_v2_headers(
        key,
        "alice@example.com",
        "entra",
        "gardenio-viewer,gardenio-operator",
        "audit-2",
        "GET",
        "/api/v1/sessions",
        &ts,
    )?;
    let got = ref_cfg(key).verify(&h, "GET", "/api/v1/sessions");
    assert_eq!(got.outcome, Outcome::Malformed);
    assert!(!got.verified_v2);
    Ok(())
}

#[test]
fn verify_v2_does_not_fall_back_to_legacy_signature() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    let mut h = build_v2_headers(
        key,
        "alice@example.com",
        "entra",
        "gardenio-admin",
        "audit-2",
        "POST",
        "/api/v1/sessions/s1/input",
        &ts,
    )?;
    h.remove(HEADER_SIGNATURE_V2);
    let got = ref_cfg(key).verify(&h, "POST", "/api/v1/sessions/s1/input");
    assert_eq!(got.outcome, Outcome::NoSig);
    assert!(!got.verified_v2);
    assert!(got.roles.is_empty());
    Ok(())
}

#[test]
fn v2_cross_language_known_vector() {
    let got = sign_v2_b64(
        b"shared-secret",
        "operator@acme.example",
        "entra",
        "gardenio-operator,gardenio-viewer",
        "audit-123",
        "POST",
        "/v1/beads",
        "1700000000",
    );
    assert_eq!(got, "KNAEp9aRG8u2NQXkBgNsyD/R3X7l6RdnkI+CFL7mkBY=");
}

#[test]
fn trusted_operator_role_hierarchy_and_legacy_floor() {
    let viewer = TrustedOperator {
        operator: "viewer@example.com".to_owned(),
        audit_id: "a".to_owned(),
        version: 2,
        auth_mode: "entra".to_owned(),
        roles: vec!["gardenio-viewer".to_owned()],
        verified_v2: true,
        verified_v1: false,
        relay: false,
    };
    assert!(viewer.has_minimum_role("gardenio-viewer"));
    assert!(!viewer.has_minimum_role("gardenio-operator"));

    let admin = TrustedOperator { roles: vec!["gardenio-admin".to_owned()], ..viewer.clone() };
    assert!(admin.has_minimum_role("gardenio-operator"));

    // Additive enforcement (ka-0000001641): a cryptographically verified v1
    // envelope keeps pre-MR authority — it satisfies every role, including
    // admin, because before this MR any verified envelope skipped the role
    // gate entirely.
    let verified_v1 = TrustedOperator {
        version: 1,
        auth_mode: String::new(),
        roles: Vec::new(),
        verified_v2: false,
        verified_v1: true,
        ..viewer.clone()
    };
    assert!(verified_v1.has_minimum_role("gardenio-viewer"));
    assert!(verified_v1.has_minimum_role("gardenio-operator"));
    assert!(verified_v1.has_minimum_role("gardenio-admin"));

    // A burn-in-trusted but cryptographically UNVERIFIED envelope (neither v1
    // nor v2) is pinned to the read-only viewer floor and can never mutate.
    let burn_in = TrustedOperator {
        version: 1,
        auth_mode: String::new(),
        roles: Vec::new(),
        verified_v2: false,
        verified_v1: false,
        ..viewer
    };
    assert!(burn_in.has_minimum_role("gardenio-viewer"));
    assert!(!burn_in.has_minimum_role("gardenio-operator"));
}

// ── Additive-enforcement acceptance suite (operator decision ka-0000001641,
// task ka-0000006597). Mirrors the four kbeads tests. Each drives verify() and
// then the has_minimum_role() gate the same way auth_layer does, with
// enforce=true (the live-prod posture). ──

fn enforce_cfg(key: &[u8]) -> Config {
    Config { hmac_key: key.to_vec(), enforce: true, now: Some(ref_now), ..Config::default() }
}

fn trusted_from(result: &VerifyResult) -> TrustedOperator {
    TrustedOperator {
        operator: result.operator.clone(),
        audit_id: result.audit_id.clone(),
        version: result.version,
        auth_mode: result.auth_mode.clone(),
        roles: result.roles.clone(),
        verified_v2: result.verified_v2,
        verified_v1: result.verified_v1,
        relay: result.relay,
    }
}

// 1. valid v1 mutation ALLOWED (with enforce=true).
#[test]
fn additive_valid_v1_keeps_mutation_authority() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    let sig = sign_b64(key, "alice@example.com", "audit-1", "/api/v1/sessions/s1/input", &ts);
    let h = build_headers("alice@example.com", "audit-1", &ts, Some(&sig))?;

    let got = enforce_cfg(key).verify(&h, "POST", "/api/v1/sessions/s1/input");
    assert_eq!(got.outcome, Outcome::Ok);
    assert!(got.verified_v1);
    assert!(!got.verified_v2);
    // A verified v1 envelope clears an operator-level route (pre-MR authority).
    let trusted = trusted_from(&got);
    assert!(trusted.has_minimum_role("gardenio-operator"));
    assert!(trusted.has_minimum_role("gardenio-admin"));
    Ok(())
}

// 2. invalid v1 REJECTED under enforce; and burn-in invalid still cannot mutate.
#[test]
fn additive_invalid_v1_never_mutates() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    // Wrong signature.
    let h = build_headers("alice@example.com", "audit-1", &ts, Some("YmFk"))?;

    let enforced = enforce_cfg(key).verify(&h, "POST", "/api/v1/sessions/s1/input");
    assert_eq!(enforced.outcome, Outcome::Mismatch);
    assert!(!enforced.trusted, "enforce: invalid v1 must not be trusted");
    assert!(!enforced.verified_v1);

    // Burn-in (enforce=false): trusted for attribution, but NOT verified_v1, so
    // the viewer floor denies the mutation.
    let burn_in = ref_cfg(key).verify(&h, "POST", "/api/v1/sessions/s1/input");
    assert_eq!(burn_in.outcome, Outcome::Mismatch);
    assert!(burn_in.trusted, "burn-in: trusted for attribution");
    assert!(!burn_in.verified_v1, "burn-in invalid must not be verified_v1");
    let trusted = trusted_from(&burn_in);
    assert!(!trusted.has_minimum_role("gardenio-operator"), "burn-in invalid must not mutate");
    Ok(())
}

// 3. invalid v2 does NOT downgrade to v1 legacy authority.
#[test]
fn additive_invalid_v2_does_not_downgrade_to_v1() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    // A well-formed v1 legacy signature is present (build_v2_headers stamps it),
    // but the v2 signature is forged.
    let mut h = build_v2_headers(
        key,
        "alice@example.com",
        "entra",
        "gardenio-admin",
        "audit-2",
        "POST",
        "/api/v1/sessions/s1/input",
        &ts,
    )?;
    h.insert(HEADER_SIGNATURE_V2, HeaderValue::from_static("Zm9yZ2Vk"));

    let got = enforce_cfg(key).verify(&h, "POST", "/api/v1/sessions/s1/input");
    assert_eq!(got.outcome, Outcome::Mismatch, "forged v2 must not verify");
    assert!(!got.verified_v2);
    assert!(!got.verified_v1, "version:2 must never be treated as verified v1");
    assert!(!got.trusted, "enforce: forged v2 rejected outright");
    let trusted = trusted_from(&got);
    assert!(!trusted.has_minimum_role("gardenio-operator"), "no v1 downgrade authority");
    Ok(())
}

// 4. v2 viewer DENIED a mutation (v2 role enforcement intact, additive).
#[test]
fn additive_v2_viewer_denied_mutation() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    let h = build_v2_headers(
        key,
        "alice@example.com",
        "entra",
        "gardenio-viewer",
        "audit-2",
        "POST",
        "/api/v1/sessions/s1/input",
        &ts,
    )?;

    let got = enforce_cfg(key).verify(&h, "POST", "/api/v1/sessions/s1/input");
    assert_eq!(got.outcome, Outcome::Ok);
    assert!(got.verified_v2);
    assert!(!got.verified_v1);
    let trusted = trusted_from(&got);
    assert!(trusted.has_minimum_role("gardenio-viewer"));
    assert!(!trusted.has_minimum_role("gardenio-operator"), "v2 viewer must not mutate");
    Ok(())
}

#[test]
fn verify_no_operator() {
    let cfg = Config { hmac_key: b"k".to_vec(), ..Config::default() };
    let h = HeaderMap::new();
    let got = cfg.verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::NoOperator);
    assert!(!got.trusted, "nothing to trust either way");
}

#[test]
fn verify_no_key_burn_in() {
    let cfg = Config::default();
    let mut h = HeaderMap::new();
    h.insert(HEADER_OPERATOR, HeaderValue::from_static("alice@example.com"));
    let got = cfg.verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::NoKey);
    assert!(!got.trusted, "no key: an unverifiable X-Operator is never trusted, even in burn-in");
}

#[test]
fn verify_no_key_enforce() {
    let cfg = Config { enforce: true, ..Config::default() };
    let mut h = HeaderMap::new();
    h.insert(HEADER_OPERATOR, HeaderValue::from_static("alice@example.com"));
    let got = cfg.verify(&h, "GET", "/v1/beads");
    assert!(!got.trusted, "enforce: untrusted when no key configured");
}

#[test]
fn verify_no_sig_burn_in() -> Result<()> {
    let cfg = Config { hmac_key: b"k".to_vec(), ..Config::default() };
    let h = build_headers("alice@example.com", "audit-1", "", None)?;
    let got = cfg.verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::NoSig);
    assert!(got.trusted);
    Ok(())
}

#[test]
fn verify_no_sig_enforce() -> Result<()> {
    let cfg = Config { hmac_key: b"k".to_vec(), enforce: true, ..Config::default() };
    let h = build_headers("alice@example.com", "audit-1", "", None)?;
    let got = cfg.verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::NoSig);
    assert!(!got.trusted);
    Ok(())
}

#[test]
fn verify_stale_past() -> Result<()> {
    let key = b"secret-key";
    let stale_ts = (REF_EPOCH - 10 * 60).to_string();
    let sig = sign_b64(key, "alice@example.com", "audit-1", "/v1/beads", &stale_ts);
    let h = build_headers("alice@example.com", "audit-1", &stale_ts, Some(&sig))?;
    let got = ref_cfg(key).verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Stale);
    Ok(())
}

#[test]
fn verify_stale_future() -> Result<()> {
    let key = b"secret-key";
    let future_ts = (REF_EPOCH + 10 * 60).to_string();
    let sig = sign_b64(key, "alice@example.com", "audit-1", "/v1/beads", &future_ts);
    let h = build_headers("alice@example.com", "audit-1", &future_ts, Some(&sig))?;
    let got = ref_cfg(key).verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Stale, "positive skew should also be stale");
    Ok(())
}

#[test]
fn verify_stale_bad_timestamp() -> Result<()> {
    let cfg = Config { hmac_key: b"k".to_vec(), ..Config::default() };
    let h = build_headers("alice@example.com", "audit-1", "not-a-number", Some("sig"))?;
    let got = cfg.verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Stale);
    Ok(())
}

#[test]
fn verify_mismatch_wrong_key() -> Result<()> {
    let ts = REF_EPOCH.to_string();
    let sig = sign_b64(b"attacker-key", "alice@example.com", "audit-1", "/v1/beads", &ts);
    let h = build_headers("alice@example.com", "audit-1", &ts, Some(&sig))?;
    let got = ref_cfg(b"real-key").verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Mismatch);
    Ok(())
}

#[test]
fn verify_mismatch_tampered_path() -> Result<()> {
    let key = b"secret-key";
    let ts = REF_EPOCH.to_string();
    let sig = sign_b64(key, "alice@example.com", "audit-1", "/v1/beads", &ts);
    let h = build_headers("alice@example.com", "audit-1", &ts, Some(&sig))?;
    let got = ref_cfg(key).verify(&h, "GET", "/admin");
    assert_eq!(got.outcome, Outcome::Mismatch, "path swap must invalidate sig");
    Ok(())
}

#[test]
fn verify_mismatch_bad_base64() -> Result<()> {
    let ts = REF_EPOCH.to_string();
    let h = build_headers("alice@example.com", "audit-1", &ts, Some("!!!not-base64!!!"))?;
    let got = ref_cfg(b"k").verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Mismatch);
    Ok(())
}

#[test]
fn verify_burn_in_vs_enforce() -> Result<()> {
    let key = b"real-key";
    let ts = REF_EPOCH.to_string();
    let sig = sign_b64(b"wrong-key", "alice@example.com", "audit-1", "/v1/beads", &ts);
    let h = build_headers("alice@example.com", "audit-1", &ts, Some(&sig))?;

    let burn_in = ref_cfg(key);
    let got = burn_in.verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Mismatch);
    assert!(got.trusted, "burn-in: mismatch but still trusted");

    let mut enforce = ref_cfg(key);
    enforce.enforce = true;
    let got = enforce.verify(&h, "GET", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Mismatch);
    assert!(!got.trusted, "enforce: mismatch rejected");
    Ok(())
}

#[test]
fn verify_max_skew_custom() -> Result<()> {
    let key = b"k";
    let ts = (REF_EPOCH - 30).to_string();
    let sig = sign_b64(key, "alice@example.com", "audit-1", "/v1/beads", &ts);
    let h = build_headers("alice@example.com", "audit-1", &ts, Some(&sig))?;

    let mut cfg = ref_cfg(key);
    cfg.max_skew = Duration::from_secs(10);
    assert_eq!(cfg.verify(&h, "GET", "/v1/beads").outcome, Outcome::Stale);

    cfg.max_skew = Duration::from_secs(60);
    assert_eq!(cfg.verify(&h, "GET", "/v1/beads").outcome, Outcome::Ok);
    Ok(())
}

#[test]
fn config_enabled() {
    assert!(!Config::default().enabled());
    let cfg = Config { hmac_key: b"k".to_vec(), ..Config::default() };
    assert!(cfg.enabled());
}

#[test]
fn env_flag_values() {
    let cases: &[(&str, bool)] = &[
        ("1", true),
        ("true", true),
        ("TRUE", true),
        ("yes", true),
        ("on", true),
        ("0", false),
        ("false", false),
        ("no", false),
        ("off", false),
        ("", false),
    ];
    // Serialize to avoid env-var races across parallel tests.
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    for (v, want) in cases {
        let _g = LOCK.lock().unwrap_or_else(|p| p.into_inner());
        std::env::set_var("EDGE_AUTH_TEST_FLAG", v);
        let got = env_flag("EDGE_AUTH_TEST_FLAG");
        std::env::remove_var("EDGE_AUTH_TEST_FLAG");
        assert_eq!(got, *want, "env_flag({v:?})");
    }
}

#[test]
fn metrics_render_prometheus() {
    let m = EdgeAuthMetrics::new();
    m.inc(Outcome::Ok, false);
    m.inc(Outcome::Ok, false);
    m.inc(Outcome::Mismatch, false);
    m.inc(Outcome::Mismatch, true);

    let mut buf = String::new();
    m.render_prometheus(&mut buf);
    assert!(buf.contains("# TYPE coopmux_edge_auth_verify_total counter"));
    assert!(
        buf.contains("coopmux_edge_auth_verify_total{outcome=\"ok\",enforce=\"false\"} 2"),
        "buf was:\n{buf}"
    );
    assert!(
        buf.contains("coopmux_edge_auth_verify_total{outcome=\"mismatch\",enforce=\"false\"} 1")
    );
    assert!(buf.contains("coopmux_edge_auth_verify_total{outcome=\"mismatch\",enforce=\"true\"} 1"));
}

/// With `hmac_key_v2` set, a v2 envelope verifies only under the v2 key: the
/// shared v1 key alone cannot mint one (gd-dMof1PW1oJ). v1 keeps the shared
/// key, and an unset `hmac_key_v2` falls back to it for v2 as well.
#[test]
fn verify_v2_split_key() -> Result<()> {
    let (v1_key, v2_key) = (b"v1-key", b"v2-key");
    let ts = REF_EPOCH.to_string();
    let split = Config { hmac_key_v2: v2_key.to_vec(), ..ref_cfg(v1_key) };
    let v2 = |key: &[u8]| {
        build_v2_headers(
            key,
            "alice@example.com",
            "entra",
            "gardenio-admin",
            "audit-split",
            "POST",
            "/v1/beads",
            &ts,
        )
    };

    let got = split.verify(&v2(v2_key)?, "POST", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Ok);
    assert!(got.verified_v2);

    let got = split.verify(&v2(v1_key)?, "POST", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Mismatch);
    assert!(!got.verified_v2);
    assert!(got.roles.is_empty());

    let sig = sign_b64(v1_key, "alice@example.com", "audit-split", "/v1/beads", &ts);
    let v1 = build_headers("alice@example.com", "audit-split", &ts, Some(&sig))?;
    assert_eq!(split.verify(&v1, "GET", "/v1/beads").outcome, Outcome::Ok);

    let got = ref_cfg(v1_key).verify(&v2(v1_key)?, "POST", "/v1/beads");
    assert_eq!(got.outcome, Outcome::Ok);
    assert!(got.verified_v2);
    Ok(())
}

/// The Glass relay key (gd-9ZGh01W5ub) verifies a v2 envelope only on the
/// relay's `GET /ws/{session}` and `GET /ws/mux` upgrades, marks the identity
/// `relay`, and is never a v1 key or a key for any other route.
#[test]
fn verify_relay_key_only_on_watch_upgrade() -> Result<()> {
    let (v1_key, v2_key, relay_key) = (b"v1-key", b"v2-key", b"relay-key");
    let ts = REF_EPOCH.to_string();
    let cfg = Config {
        hmac_key_v2: v2_key.to_vec(),
        hmac_key_relay: relay_key.to_vec(),
        ..enforce_cfg(v1_key)
    };
    let v2 = |key: &[u8], method: &str, path: &str| {
        build_v2_headers(
            key,
            "alice@example.com",
            "cookie",
            "gardenio-operator,gardenio-viewer",
            "audit-relay",
            method,
            path,
            &ts,
        )
    };

    for path in ["/ws/sess-1", "/ws/mux"] {
        let got = cfg.verify(&v2(relay_key, "GET", path)?, "GET", path);
        assert_eq!(got.outcome, Outcome::Ok, "{path}");
        assert!(got.trusted && got.verified_v2 && got.relay, "{path}: {got:?}");
        assert_eq!(got.roles, vec!["gardenio-operator", "gardenio-viewer"]);
    }

    // The v2 key on the same upgrade is an ordinary v2 identity.
    let got = cfg.verify(&v2(v2_key, "GET", "/ws/sess-1")?, "GET", "/ws/sess-1");
    assert_eq!(got.outcome, Outcome::Ok);
    assert!(got.verified_v2 && !got.relay);

    for (method, path) in [
        ("POST", "/ws/sess-1"),
        ("GET", "/ws/"),
        ("GET", "/ws/sess-1/extra"),
        ("GET", "/sse/mux"),
        ("POST", "/sse/session/sess-1/send"),
        ("GET", "/api/v1/sessions/sess-1/screen"),
        ("POST", "/api/v1/sessions/sess-1/input"),
        ("GET", "/api/v1/credentials"),
    ] {
        let got = cfg.verify(&v2(relay_key, method, path)?, method, path);
        assert_eq!(got.outcome, Outcome::Mismatch, "{method} {path}");
        assert!(!got.trusted && !got.verified_v2 && !got.relay, "{method} {path}: {got:?}");
        assert!(got.roles.is_empty(), "{method} {path}: unverified roles must be discarded");
    }

    // A v1 envelope signed with the relay key is not trusted anywhere.
    let sig = sign_b64(relay_key, "alice@example.com", "audit-relay", "/ws/sess-1", &ts);
    let v1 = build_headers("alice@example.com", "audit-relay", &ts, Some(&sig))?;
    let got = cfg.verify(&v1, "GET", "/ws/sess-1");
    assert_eq!(got.outcome, Outcome::Mismatch);
    assert!(!got.trusted && !got.verified_v1 && !got.relay);
    Ok(())
}

/// A coopmux holding only the relay key verifies the relay's upgrade and
/// nothing else: every other envelope is still `NoKey`, never trusted.
#[test]
fn verify_relay_key_alone() -> Result<()> {
    let relay_key = b"relay-key";
    let ts = REF_EPOCH.to_string();
    let cfg =
        Config { hmac_key_relay: relay_key.to_vec(), now: Some(ref_now), ..Config::default() };
    assert!(cfg.enabled());
    let v2 = |path: &str| {
        build_v2_headers(
            relay_key,
            "alice@example.com",
            "cookie",
            "gardenio-viewer",
            "audit-relay",
            "GET",
            path,
            &ts,
        )
    };

    let got = cfg.verify(&v2("/ws/sess-1")?, "GET", "/ws/sess-1");
    assert_eq!(got.outcome, Outcome::Ok);
    assert!(got.trusted && got.relay);

    let got = cfg.verify(&v2("/api/v1/sessions")?, "GET", "/api/v1/sessions");
    assert_eq!(got.outcome, Outcome::NoKey);
    assert!(!got.trusted && !got.relay);
    Ok(())
}

/// A relay identity passes the upgrade's operator gate on its roles, but may
/// only send viewer-tier frames upstream: it watches, it never types.
#[test]
fn relay_identity_may_send_viewer_frames_only() {
    let relay = TrustedOperator {
        operator: "alice@example.com".to_owned(),
        audit_id: "a".to_owned(),
        version: 2,
        auth_mode: "cookie".to_owned(),
        roles: vec!["gardenio-admin".to_owned()],
        verified_v2: true,
        verified_v1: false,
        relay: true,
    };
    assert!(relay.has_minimum_role("gardenio-operator"));
    assert!(relay.may_send("gardenio-viewer"));
    assert!(!relay.may_send("gardenio-operator"));
    assert!(!relay.may_send("gardenio-admin"));

    let direct = TrustedOperator { relay: false, ..relay };
    assert!(direct.may_send("gardenio-admin"));
}
