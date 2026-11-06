// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

use base64::Engine;
use serde_json::json;
use tempfile::TempDir;

use super::*;

/// Build a JWT with the given claims payload. The header/signature are dummies
/// — the gate never verifies the signature, only decodes the payload.
fn make_jwt(payload: serde_json::Value) -> String {
    let b64 = |bytes: &[u8]| base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes);
    let header = b64(br#"{"alg":"RS256","typ":"JWT"}"#);
    let body = b64(serde_json::to_string(&payload).expect("json").as_bytes());
    format!("{header}.{body}.c2ln") // "sig"
}

/// A bound-SA-token-shaped payload: SA sub, pod uid, aud, iat, exp.
fn sa_payload(pod_uid: &str, aud: &str, iat: i64, exp: i64) -> serde_json::Value {
    json!({
        "sub": "system:serviceaccount:gardenio:gardenio-agents-sa",
        "aud": [aud],
        "iat": iat,
        "exp": exp,
        "kubernetes.io": { "pod": { "name": "agent-b", "uid": pod_uid } }
    })
}

// ===========================================================================
// decode_jwt_claims
// ===========================================================================

#[test]
fn decode_extracts_claims() -> anyhow::Result<()> {
    let jwt = make_jwt(sa_payload("pod-b-uid", AWS_STS_AUDIENCE, 1000, 5000));
    let c = decode_jwt_claims(&jwt)?;
    assert_eq!(c.sub.as_deref(), Some("system:serviceaccount:gardenio:gardenio-agents-sa"));
    assert_eq!(c.aud, vec![AWS_STS_AUDIENCE.to_string()]);
    assert_eq!(c.iat, Some(1000));
    assert_eq!(c.exp, Some(5000));
    assert_eq!(c.pod_uid.as_deref(), Some("pod-b-uid"));
    Ok(())
}

#[test]
fn decode_accepts_scalar_aud() -> anyhow::Result<()> {
    let jwt = make_jwt(json!({ "aud": "sts.amazonaws.com", "iat": 1, "exp": 2 }));
    let c = decode_jwt_claims(&jwt)?;
    assert_eq!(c.aud, vec!["sts.amazonaws.com".to_string()]);
    Ok(())
}

#[test]
fn decode_rejects_non_jwt() {
    assert!(matches!(decode_jwt_claims("not-a-jwt"), Err(FreshnessError::Malformed(_))));
    assert!(matches!(decode_jwt_claims("only.two"), Err(FreshnessError::Malformed(_))));
    assert!(matches!(decode_jwt_claims("a.b.c.d"), Err(FreshnessError::Malformed(_))));
    // Bad base64 in the payload segment.
    assert!(matches!(decode_jwt_claims("aaa.!!!.sig"), Err(FreshnessError::Malformed(_))));
}

// ===========================================================================
// freshness_gate — the fail-closed identity check (headline tests)
// ===========================================================================

fn expect_b(frozen_at: i64) -> Expectations {
    Expectations {
        frozen_at: Some(frozen_at),
        expected_pod_uid: Some("pod-b-uid".into()),
        expected_aud: Some(AWS_STS_AUDIENCE.into()),
    }
}

#[test]
fn gate_passes_fresh_pod_b_token() {
    // Issued after freeze, Pod-B uid, correct aud, not expired.
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![AWS_STS_AUDIENCE.into()],
        iat: Some(2000),
        exp: Some(9999),
        pod_uid: Some("pod-b-uid".into()),
    };
    assert_eq!(freshness_gate(&claims, &expect_b(1000), 3000), Ok(()));
}

#[test]
fn gate_fails_closed_on_stale_pod_a_iat() {
    // Token issued *before* the freeze — a Pod-A token still in RAM. Even
    // though it carries the right aud and is not yet expired, it must fail.
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![AWS_STS_AUDIENCE.into()],
        iat: Some(500), // <= frozen_at (1000)
        exp: Some(9999),
        pod_uid: None,
    };
    assert_eq!(
        freshness_gate(&claims, &expect_b(1000), 1500),
        Err(FreshnessError::StaleIssuedAt { iat: 500, frozen_at: 1000 })
    );
}

#[test]
fn gate_fails_closed_on_pod_a_uid() {
    // Token bound to Pod A's uid under Pod B — the identity-confusion hazard.
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![AWS_STS_AUDIENCE.into()],
        iat: Some(2000),
        exp: Some(9999),
        pod_uid: Some("pod-a-uid".into()),
    };
    assert_eq!(
        freshness_gate(&claims, &expect_b(1000), 3000),
        Err(FreshnessError::PodAIdentity { want: "pod-b-uid".into(), got: "pod-a-uid".into() })
    );
}

#[test]
fn gate_fails_closed_on_expired() {
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![AWS_STS_AUDIENCE.into()],
        iat: Some(2000),
        exp: Some(2500),
        pod_uid: Some("pod-b-uid".into()),
    };
    // now (3000) is past exp (2500).
    assert_eq!(
        freshness_gate(&claims, &expect_b(1000), 3000),
        Err(FreshnessError::Expired { exp: 2500, now: 3000 })
    );
}

#[test]
fn gate_fails_closed_on_wrong_audience() {
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec!["some-other-aud".into()],
        iat: Some(2000),
        exp: Some(9999),
        pod_uid: Some("pod-b-uid".into()),
    };
    assert!(matches!(
        freshness_gate(&claims, &expect_b(1000), 3000),
        Err(FreshnessError::WrongAudience { .. })
    ));
}

#[test]
fn gate_fails_closed_when_indeterminate() {
    // No pod-uid claim, and no frozen_at to compare iat against, and no
    // expected pod-uid → freshness cannot be proven → fail closed.
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![AWS_STS_AUDIENCE.into()],
        iat: Some(2000),
        exp: Some(9999),
        pod_uid: None,
    };
    let expect = Expectations {
        frozen_at: None,
        expected_pod_uid: None,
        expected_aud: Some(AWS_STS_AUDIENCE.into()),
    };
    assert_eq!(freshness_gate(&claims, &expect, 3000), Err(FreshnessError::Indeterminate));
}

#[test]
fn gate_passes_on_pod_uid_proof_alone() {
    // No frozen_at / iat signal, but the pod-uid matches Pod B → proven fresh.
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![],
        iat: None,
        exp: None,
        pod_uid: Some("pod-b-uid".into()),
    };
    let expect = Expectations {
        frozen_at: None,
        expected_pod_uid: Some("pod-b-uid".into()),
        expected_aud: None,
    };
    assert_eq!(freshness_gate(&claims, &expect, 3000), Ok(()));
}

#[test]
fn gate_passes_on_post_freeze_iat_alone() {
    // No pod-uid claim, but iat is after the freeze → proven fresh.
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![],
        iat: Some(2000),
        exp: None,
        pod_uid: None,
    };
    let expect = Expectations { frozen_at: Some(1000), expected_pod_uid: None, expected_aud: None };
    assert_eq!(freshness_gate(&claims, &expect, 3000), Ok(()));
}

#[test]
fn gate_disqualifier_beats_positive_proof() {
    // iat is fresh (post-freeze), but the pod-uid is Pod A's. The hard
    // disqualifier must win — a fresh iat must not rescue a Pod-A binding.
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![AWS_STS_AUDIENCE.into()],
        iat: Some(2000),
        exp: Some(9999),
        pod_uid: Some("pod-a-uid".into()),
    };
    assert!(matches!(
        freshness_gate(&claims, &expect_b(1000), 3000),
        Err(FreshnessError::PodAIdentity { .. })
    ));
}

#[test]
fn gate_pod_uid_match_suppresses_stale_iat() {
    // ka-0000002492 / ka-0000004784 — the warm-graft hot path. Pod B's honest
    // SA token carries Pod B's kubelet uid (proven fresh, unforgeable) BUT its
    // `iat` is <= frozen_at, because the warm-pool kubelet minted it before the
    // agent freeze and does not re-mint on graft. A proven pod-uid must SUPPRESS
    // the StaleIssuedAt disqualifier, so this token passes. RED on unfixed coop
    // (returns Err(StaleIssuedAt)); GREEN once the iat branch is gated on
    // !proven_fresh.
    let claims = JwtClaims {
        sub: Some("sa".into()),
        aud: vec![AWS_STS_AUDIENCE.into()],
        iat: Some(500), // <= frozen_at (1000): pre-freeze, warm-pool-minted
        exp: Some(9999),
        pod_uid: Some("pod-b-uid".into()), // kubelet-bound proof this is Pod B
    };
    assert_eq!(freshness_gate(&claims, &expect_b(1000), 1500), Ok(()));
}

// ===========================================================================
// CredentialReminter handler — end-to-end through the trait
// ===========================================================================

fn write_token(dir: &std::path::Path, name: &str, jwt: &str) -> PathBuf {
    let p = dir.join(name);
    std::fs::write(&p, jwt).expect("write token");
    p
}

/// Marker fixture: thawed into Pod B at a known freeze stamp (coop `@`-form).
fn marker_for(pod_uid: &str, frozen_at: i64) -> Marker {
    Marker {
        epoch: 1,
        pod_uid: pod_uid.into(),
        node: "node-b".into(),
        ts: Some("@2000".into()),
        frozen_at: Some(format!("@{frozen_at}")),
        deadline_ms: 0,
    }
}

/// Marker fixture whose `frozen_at` is a Go RFC3339 string (the real on-disk
/// form: thawepoch.Marker.FrozenAt marshals as RFC3339). Exercises the
/// ka-0000004956 parse_rfc3339_unix path end-to-end through expectations().
fn marker_rfc3339(pod_uid: &str, frozen_at_rfc3339: &str) -> Marker {
    Marker {
        epoch: 1,
        pod_uid: pod_uid.into(),
        node: "node-b".into(),
        ts: Some("2024-01-01T00:00:00Z".into()),
        frozen_at: Some(frozen_at_rfc3339.into()),
        deadline_ms: 0,
    }
}

#[tokio::test]
async fn handler_acks_ok_on_fresh_tokens_and_poisons_aws() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    // Fresh Pod-B tokens: iat (2000) > frozen_at (1000), Pod-B uid, valid aud.
    let aws = write_token(
        dir.path(),
        "aws-token",
        &make_jwt(sa_payload("pod-b-uid", AWS_STS_AUDIENCE, 2000, 9_999_999)),
    );
    let sa = write_token(
        dir.path(),
        "sa-token",
        &make_jwt(sa_payload("pod-b-uid", "https://kubernetes.default.svc", 2000, 9_999_999)),
    );

    let reset_calls = Arc::new(AtomicUsize::new(0));
    let rc = Arc::clone(&reset_calls);
    let aws_reset: AwsReset = Arc::new(move || {
        let rc = Arc::clone(&rc);
        Box::pin(async move {
            rc.fetch_add(1, Ordering::SeqCst);
        })
    });

    let reminter = CredentialReminter::new()
        .with_web_identity_token(aws)
        .with_sa_token(sa)
        .with_aws_reset(aws_reset)
        .with_now_fn(|| 3000);

    let outcome = reminter.rehydrate(&marker_for("pod-b-uid", 1000)).await;
    assert!(outcome.ok, "expected OK, got {outcome:?}");
    assert_eq!(outcome.error, None);
    assert_eq!(outcome.stages.get("aws_sts").map(String::as_str), Some("ok"));
    assert_eq!(outcome.stages.get("aws_token").map(String::as_str), Some("ok"));
    assert_eq!(outcome.stages.get("sa_token").map(String::as_str), Some("ok"));
    assert_eq!(reset_calls.load(Ordering::SeqCst), 1, "aws reset must fire exactly once");
    Ok(())
}

#[tokio::test]
async fn handler_fails_closed_on_stale_pod_a_token() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    // SA token still presents Pod A's uid and a pre-freeze iat — the agent is
    // still living under Pod A's identity. The thaw MUST fail closed.
    let sa = write_token(
        dir.path(),
        "sa-token",
        &make_jwt(sa_payload("pod-a-uid", "https://kubernetes.default.svc", 500, 9_999_999)),
    );

    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 3000);

    let outcome = reminter.rehydrate(&marker_for("pod-b-uid", 1000)).await;
    assert!(!outcome.ok, "stale Pod-A token must fail closed");
    assert!(outcome.error.is_some());
    assert!(
        outcome.stages.get("sa_token").map(|s| s.starts_with("failed")).unwrap_or(false),
        "sa_token stage should be marked failed: {:?}",
        outcome.stages
    );
    Ok(())
}

#[tokio::test]
async fn handler_fails_closed_on_blank_token_file() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let sa = write_token(dir.path(), "sa-token", "   \n");
    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 3000);
    let outcome = reminter.rehydrate(&marker_for("pod-b-uid", 1000)).await;
    assert!(!outcome.ok, "blank token file must fail closed");
    Ok(())
}

// ===========================================================================
// ka-0000008074 — absent token file is a DISTINCT, legible failure (not a
// misleading "malformed JWT"). The prod canary (2026.180.23) failed with
// `aws_token: malformed JWT: read .../token: No such file or directory` because
// the freeze warm-copy dropped the kubelet atomic-writer token symlink, so coop
// read an ENOENT. Classifying ENOENT as Malformed buried a missing-file mount
// bug under a JWT-shaped error. These tests pin the distinct classification.
// ===========================================================================

#[tokio::test]
async fn gate_token_missing_file_is_missing_not_malformed() -> anyhow::Result<()> {
    // A configured token source whose file does not exist (the prod ENOENT after
    // the warm-copy dropped the atomic-writer token symlink). The read error must
    // classify as FreshnessError::Missing — NOT Malformed (which reads as "the
    // JWT bytes are bad", sending forensics down the wrong path).
    let dir = TempDir::new()?;
    let absent = dir.path().join("does-not-exist").join("token");
    let reminter = CredentialReminter::new().with_sa_token(absent).with_now_fn(|| 3000);
    let outcome = reminter.rehydrate(&marker_for("pod-b-uid", 1000)).await;
    assert!(!outcome.ok, "absent token file must fail closed");
    let sa = outcome.stages.get("sa_token").cloned().unwrap_or_default();
    assert!(
        sa.contains("token file not found") || sa.contains("missing"),
        "absent token should be reported as a missing file, got: {sa:?}"
    );
    // And it must NOT masquerade as a malformed JWT — that classification cost the
    // canary forensics a cycle.
    assert!(
        !sa.contains("malformed JWT"),
        "an absent token file must not be reported as a malformed JWT: {sa:?}"
    );
    Ok(())
}

/// `FreshnessError::Missing` is its own fail-closed variant, distinct from
/// `Malformed`, and its Display names the absent path. Pins the diagnostic shape
/// that the prod ENOENT should have produced.
#[test]
fn freshness_error_missing_displays_distinctly() {
    let miss =
        FreshnessError::Missing("/var/run/secrets/kubernetes.io/serviceaccount/token".into());
    let s = format!("{miss}");
    assert!(s.contains("token file not found"), "Missing display: {s}");
    assert!(s.contains("/var/run/secrets/kubernetes.io/serviceaccount/token"), "names path: {s}");
    // Distinct from Malformed's wording.
    assert!(!s.contains("malformed JWT"), "Missing must not read as malformed: {s}");
}

#[tokio::test]
async fn handler_is_idempotent() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let sa = write_token(
        dir.path(),
        "sa-token",
        &make_jwt(sa_payload("pod-b-uid", "https://kubernetes.default.svc", 2000, 9_999_999)),
    );
    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 3000);
    let m = marker_for("pod-b-uid", 1000);

    let first = reminter.rehydrate(&m).await;
    let second = reminter.rehydrate(&m).await;
    assert!(first.ok && second.ok, "re-running rehydration must stay OK");
    assert_eq!(first.stages, second.stages);
    Ok(())
}

#[tokio::test]
async fn handler_with_no_aws_reset_marks_skipped() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let sa = write_token(
        dir.path(),
        "sa-token",
        &make_jwt(sa_payload("pod-b-uid", "https://kubernetes.default.svc", 2000, 9_999_999)),
    );
    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 3000);
    let outcome = reminter.rehydrate(&marker_for("pod-b-uid", 1000)).await;
    assert!(outcome.ok);
    assert_eq!(outcome.stages.get("aws_sts").map(String::as_str), Some("skipped"));
    assert_eq!(outcome.stages.get("account").map(String::as_str), Some("skipped"));
    Ok(())
}

// ===========================================================================
// ka-0000004956 — the credential-gate overlay fix, end-to-end through coop
// ===========================================================================

/// (b) POSITIVE: after the controller overlay, the token file coop re-reads
/// carries Pod B's uid. With a Go RFC3339 `frozen_at` marker (the real on-disk
/// form), the handler acks ok=true. This is the post-fix steady state the canary
/// must reach instead of the PodAIdentity fail-close.
#[tokio::test]
async fn handler_ok_when_overlaid_token_carries_pod_b_uid() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    // frozen_at 2026-06-29T12:00:00Z == 1782734400; token issued 100s later.
    let sa = write_token(
        dir.path(),
        "sa-token",
        &make_jwt(sa_payload(
            "pod-b-uid",
            "https://kubernetes.default.svc",
            1782734500,
            9_999_999_999,
        )),
    );
    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 1782734600);
    let outcome = reminter.rehydrate(&marker_rfc3339("pod-b-uid", "2026-06-29T12:00:00Z")).await;
    assert!(outcome.ok, "fresh overlaid Pod-B token must ack ok: {outcome:?}");
    assert_eq!(outcome.error, None);
    assert_eq!(outcome.stages.get("sa_token").map(String::as_str), Some("ok"));
    Ok(())
}

/// (b') POSITIVE — the WARM-GRAFT HOT PATH (ka-0000002492 / ka-0000004784).
/// Sibling of (b) but with a PRE-freeze `iat`: Pod B is grafted from the warm
/// pool, so its overlaid SA token was minted by the kubelet at pool-fill time,
/// BEFORE the agent freeze (no re-mint on graft). The token carries Pod B's uid
/// (proven fresh) yet `iat <= frozen_at`. The proven pod-uid must SUPPRESS the
/// StaleIssuedAt disqualifier so the handler still acks ok=true. RED on the
/// unfixed coop (gate returns StaleIssuedAt → ok=false, sa_token "failed");
/// GREEN once the iat branch is gated on !proven_fresh. This is the case MR
/// !4985's hardcoded post-freeze fixture did NOT cover.
#[tokio::test]
async fn handler_ok_when_overlaid_pod_b_token_has_pre_freeze_iat() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    // frozen_at 2026-06-29T12:00:00Z == 1782734400; token issued 100s EARLIER
    // (warm-pool mint, pre-freeze). Pod-B uid is the kubelet-bound proof.
    let sa = write_token(
        dir.path(),
        "sa-token",
        &make_jwt(sa_payload(
            "pod-b-uid",
            "https://kubernetes.default.svc",
            1782734300, // <= frozen_at: pre-freeze warm-pool mint
            9_999_999_999,
        )),
    );
    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 1782734600);
    let outcome = reminter.rehydrate(&marker_rfc3339("pod-b-uid", "2026-06-29T12:00:00Z")).await;
    assert!(outcome.ok, "warm-graft Pod-B token (pre-freeze iat) must ack ok: {outcome:?}");
    assert_eq!(outcome.error, None);
    assert_eq!(outcome.stages.get("sa_token").map(String::as_str), Some("ok"));
    Ok(())
}

/// (c') NEGATIVE — the suppression keys strictly on a pod-uid MATCH, never on a
/// stale iat. Same pre-freeze `iat` as (b'), but Pod-A's uid: the StaleIssuedAt
/// suppression must NOT fire (proven_fresh stays false), and the Pod-A pod-uid
/// disqualifier must fail it closed (PodAIdentity). Stays red-bar fail-closed in
/// BOTH the unfixed and fixed states — proving the !proven_fresh gate is not a
/// gate-loosen.
#[tokio::test]
async fn handler_fails_closed_on_pod_a_token_with_pre_freeze_iat_rfc3339() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let sa = write_token(
        dir.path(),
        "sa-token",
        &make_jwt(sa_payload(
            "pod-a-uid",
            "https://kubernetes.default.svc",
            1782734300, // pre-freeze, same as (b') — only the uid differs
            9_999_999_999,
        )),
    );
    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 1782734600);
    let outcome = reminter.rehydrate(&marker_rfc3339("pod-b-uid", "2026-06-29T12:00:00Z")).await;
    assert!(!outcome.ok, "Pod-A token must fail closed regardless of iat");
    assert!(outcome.error.is_some());
    assert!(
        outcome.stages.get("sa_token").map(|s| s.starts_with("failed")).unwrap_or(false),
        "sa_token stage must be marked failed: {:?}",
        outcome.stages
    );
    Ok(())
}

/// (b, complement) The RFC3339 `frozen_at` parse is LOAD-BEARING: a token with
/// NO pod_uid claim is proven fresh ONLY by its post-freeze `iat` compared
/// against the RFC3339-parsed `frozen_at`. Before the parse_unix_ts fix this
/// marker parsed to None, the iat line was inert, and the gate returned
/// Indeterminate (fail-closed) — so this test fails RED on the unfixed coop and
/// passes only once the RFC3339 form is accepted. It proves the complement fix
/// is not dead code.
#[tokio::test]
async fn handler_ok_on_post_freeze_iat_with_rfc3339_marker() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    // No pod_uid claim → the only freshness signal is iat (1782734500) vs the
    // RFC3339 frozen_at (1782734400). Post-freeze → proven fresh.
    let payload = json!({
        "sub": "system:serviceaccount:gardenio:gardenio-agents-sa",
        "aud": ["https://kubernetes.default.svc"],
        "iat": 1782734500i64,
        "exp": 9_999_999_999i64,
    });
    let sa = write_token(dir.path(), "sa-token", &make_jwt(payload));
    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 1782734600);
    let outcome = reminter.rehydrate(&marker_rfc3339("pod-b-uid", "2026-06-29T12:00:00Z")).await;
    assert!(outcome.ok, "post-freeze iat vs RFC3339 frozen_at must prove fresh: {outcome:?}");
    assert_eq!(outcome.stages.get("sa_token").map(String::as_str), Some("ok"));
    Ok(())
}

/// (c) NEGATIVE — the fail-closed invariant the overlay must NOT break. Without
/// the overlay, the token coop reads is the warm-copied FROZEN Pod-A token
/// (Pod-A uid). Even with the now-live RFC3339 `iat` signal saying "fresh", the
/// pod_uid disqualifier must still win: ok=false and sa_token starts_with
/// "failed". This proves the fix does NOT vacuously pass and a genuine
/// foreign/stale identity is still rejected (PodAIdentity), per the security
/// invariant.
#[tokio::test]
async fn handler_fails_closed_on_pod_a_token_even_with_fresh_iat_rfc3339() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    // Pod-A uid, but a POST-freeze iat (so iat alone would say "fresh"). The
    // pod_uid disqualifier must beat the positive iat proof.
    let sa = write_token(
        dir.path(),
        "sa-token",
        &make_jwt(sa_payload(
            "pod-a-uid",
            "https://kubernetes.default.svc",
            1782734500,
            9_999_999_999,
        )),
    );
    let reminter = CredentialReminter::new().with_sa_token(sa).with_now_fn(|| 1782734600);
    let outcome = reminter.rehydrate(&marker_rfc3339("pod-b-uid", "2026-06-29T12:00:00Z")).await;
    assert!(!outcome.ok, "un-overlaid Pod-A token must fail closed even with a fresh iat");
    assert!(outcome.error.is_some());
    assert!(
        outcome.stages.get("sa_token").map(|s| s.starts_with("failed")).unwrap_or(false),
        "sa_token stage must be marked failed: {:?}",
        outcome.stages
    );
    Ok(())
}

// ===========================================================================
// parse_unix_ts
// ===========================================================================

#[test]
fn parse_unix_ts_handles_coop_and_numeric_forms() {
    assert_eq!(parse_unix_ts("@1700000000"), Some(1700000000));
    assert_eq!(parse_unix_ts("1700000000"), Some(1700000000));
    assert_eq!(parse_unix_ts(""), None);
}

/// ka-0000004956 red-bar: parse_unix_ts MUST accept the Go RFC3339 `frozen_at`
/// (thawepoch.Marker.FrozenAt marshals as RFC3339 via time.Time). This inverts
/// the former line-~362 `== None` assertion. Without this, a Go-written
/// frozen_at parsed to None and the `iat` freshness line of the gate was inert.
#[test]
fn parse_unix_ts_accepts_go_rfc3339() {
    // The exact shape Go's time.Time JSON marshaller emits (UTC `Z`).
    // 2026-06-04T00:00:00Z == 1780531200 (was asserted None before the fix).
    assert_eq!(parse_unix_ts("2026-06-04T00:00:00Z"), Some(1780531200));
    // A well-known anchor: 2024-01-01T00:00:00Z == 1704067200.
    assert_eq!(parse_unix_ts("2024-01-01T00:00:00Z"), Some(1704067200));
    // Noon on the cycle date: 2026-06-29T12:00:00Z == 1782734400.
    assert_eq!(parse_unix_ts("2026-06-29T12:00:00Z"), Some(1782734400));
    // Fractional seconds are truncated to second precision (same epoch).
    assert_eq!(parse_unix_ts("2026-06-29T12:00:00.123456789Z"), Some(1782734400));
    // A non-UTC offset is normalized to UTC: +02:00 noon == 10:00Z == 1782727200.
    assert_eq!(parse_unix_ts("2026-06-29T12:00:00+02:00"), Some(1782727200));
    // Whitespace is trimmed.
    assert_eq!(parse_unix_ts("  2024-01-01T00:00:00Z  "), Some(1704067200));
}

/// Malformed RFC3339-ish strings still fail to None (fail closed to the pod-uid
/// signal) — the new parser must not vacuously accept garbage.
#[test]
fn parse_unix_ts_rejects_malformed_rfc3339() {
    assert_eq!(parse_unix_ts("2026-13-01T00:00:00Z"), None); // month 13
    assert_eq!(parse_unix_ts("2026-06-32T00:00:00Z"), None); // day 32
    assert_eq!(parse_unix_ts("2026-06-04T25:00:00Z"), None); // hour 25
    assert_eq!(parse_unix_ts("2026-06-04T00:00:00"), None); // no offset/Z
    assert_eq!(parse_unix_ts("2026-06-04"), None); // date only
    assert_eq!(parse_unix_ts("not-a-timestamp"), None);
}

/// ka-0000007623 red-bar: impossible civil dates that pass the (1..=31) bound
/// must NOT silently roll forward through `days_from_civil` — they must return
/// None (fail closed to the pod-uid signal), while real calendar days, including
/// genuine leap days, must still parse.
#[test]
fn parse_unix_ts_rejects_invalid_civil_dates() {
    // Feb 29 in a non-leap year (2023 is not divisible by 4).
    assert_eq!(parse_unix_ts("2023-02-29T00:00:00Z"), None);
    // Feb 29 in a non-leap century (2100 % 100 == 0 but % 400 != 0).
    assert_eq!(parse_unix_ts("2100-02-29T00:00:00Z"), None);
    // April has only 30 days.
    assert_eq!(parse_unix_ts("2026-04-31T00:00:00Z"), None);
    // February never has 30 days.
    assert_eq!(parse_unix_ts("2026-02-30T00:00:00Z"), None);

    // Positive: a genuine leap day (2024 is divisible by 4, not a century)
    // still parses. 2024-02-29T00:00:00Z == 1709164800.
    assert_eq!(parse_unix_ts("2024-02-29T00:00:00Z"), Some(1709164800));
    // Positive: a leap day in a leap century (2000 % 400 == 0) parses too.
    // 2000-02-29T00:00:00Z == 951782400.
    assert_eq!(parse_unix_ts("2000-02-29T00:00:00Z"), Some(951782400));
    // Positive: last day of a 30-day month parses. 2026-04-30T00:00:00Z.
    assert_eq!(parse_unix_ts("2026-04-30T00:00:00Z"), Some(1777507200));
}

/// Unit coverage for the leap-year helper directly: the 4/100/400 rule and the
/// 30- vs 31-day months.
#[test]
fn days_in_month_applies_leap_rule() {
    // 31-day months.
    for m in [1, 3, 5, 7, 8, 10, 12] {
        assert_eq!(days_in_month(2026, m), 31, "month {m}");
    }
    // 30-day months.
    for m in [4, 6, 9, 11] {
        assert_eq!(days_in_month(2026, m), 30, "month {m}");
    }
    // February: leap-year rule.
    assert_eq!(days_in_month(2024, 2), 29); // divisible by 4
    assert_eq!(days_in_month(2023, 2), 28); // not divisible by 4
    assert_eq!(days_in_month(2100, 2), 28); // century, not divisible by 400
    assert_eq!(days_in_month(2000, 2), 29); // century divisible by 400
}
