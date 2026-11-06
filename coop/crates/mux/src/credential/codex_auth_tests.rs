// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use base64::Engine;

use super::*;

fn jwt(exp: u64) -> String {
    let enc = |s: &str| base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(s);
    format!("{}.{}.sig", enc(r#"{"alg":"none"}"#), enc(&format!(r#"{{"exp":{exp}}}"#)))
}

fn doc(access: &str, refresh: &str) -> String {
    serde_json::json!({
        "auth_mode": "chatgpt",
        "tokens": {
            "id_token": "id",
            "access_token": access,
            "refresh_token": refresh,
            "account_id": "acct-1",
        },
        "last_refresh": "2026-10-15T00:00:00Z",
    })
    .to_string()
}

#[test]
fn normalize_replaces_real_refresh_token() {
    let auth = normalize(&doc(&jwt(2_000_000_000), "rt-real-secret")).unwrap();
    assert!(!auth.document.contains("rt-real-secret"));
    assert!(auth.document.contains(PLACEHOLDER_REFRESH_TOKEN));
    assert_eq!(auth.account_id.as_deref(), Some("acct-1"));
    assert_eq!(auth.access_expires_at, Some(2_000_000_000));
}

#[test]
fn normalize_is_idempotent_across_raw_and_session_forms() {
    let raw = normalize(&doc("a.b.c", "rt-real")).unwrap();
    let session = normalize(&doc("a.b.c", PLACEHOLDER_REFRESH_TOKEN)).unwrap();
    assert_eq!(raw.document, session.document);
}

#[test]
fn normalize_tolerates_opaque_access_token() {
    let auth = normalize(&doc("not-a-jwt", "rt")).unwrap();
    assert_eq!(auth.access_expires_at, None);
}

#[test]
fn normalize_rejects_non_documents() {
    assert!(normalize("sk-proj-plain-api-key").is_err());
    assert!(normalize("[]").is_err());
    assert!(normalize(r#"{"auth_mode":"chatgpt"}"#).is_err());
    assert!(normalize(r#"{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}"#).is_err());
}

#[test]
fn normalize_rejects_api_key_mode() {
    let raw = r#"{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x","tokens":{"access_token":"a"}}"#;
    assert!(normalize(raw).unwrap_err().contains("apikey"));
}

#[test]
fn normalize_strips_top_level_api_key() {
    // auth_mode absent (older file) + tokens + a populated API key must not
    // carry the key through to every pod's auth.json.
    let raw = r#"{"OPENAI_API_KEY":"sk-leak","tokens":{"access_token":"a.b.c"}}"#;
    let auth = normalize(raw).unwrap();
    assert!(!auth.document.contains("sk-leak"));
    assert!(!auth.document.contains("OPENAI_API_KEY"));
    // Same with an explicit chatgpt mode and a null key (what `codex login` writes).
    let raw = r#"{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":"a.b.c"}}"#;
    assert!(!normalize(raw).unwrap().document.contains("OPENAI_API_KEY"));
}

#[test]
fn access_token_reads_back() {
    let auth = normalize(&doc("tok.en.x", "rt")).unwrap();
    assert_eq!(access_token(&auth.document).as_deref(), Some("tok.en.x"));
}
