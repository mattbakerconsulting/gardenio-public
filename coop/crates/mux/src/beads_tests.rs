// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

#[test]
fn fields_map_parses_strings() -> anyhow::Result<()> {
    let bead = BeadJson {
        id: "test".into(),
        title: "test".into(),
        description: None,
        fields: Some(serde_json::json!({
            "provider": "anthropic",
            "model": "claude-sonnet-4-20250514"
        })),
        labels: vec![],
    };
    let m = bead.fields_map();
    assert_eq!(m.get("provider").map(String::as_str), Some("anthropic"));
    assert_eq!(m.get("model").map(String::as_str), Some("claude-sonnet-4-20250514"));
    Ok(())
}

#[test]
fn fields_map_handles_non_string_values() -> anyhow::Result<()> {
    let bead = BeadJson {
        id: "test".into(),
        title: "test".into(),
        description: None,
        fields: Some(serde_json::json!({
            "enabled": true,
            "count": 42
        })),
        labels: vec![],
    };
    let m = bead.fields_map();
    assert_eq!(m.get("enabled").map(String::as_str), Some("true"));
    assert_eq!(m.get("count").map(String::as_str), Some("42"));
    Ok(())
}

#[test]
fn fields_map_handles_null_fields() -> anyhow::Result<()> {
    let bead = BeadJson {
        id: "test".into(),
        title: "test".into(),
        description: None,
        fields: None,
        labels: vec![],
    };
    let m = bead.fields_map();
    assert!(m.is_empty());
    Ok(())
}

#[test]
fn encode_path_preserves_safe_chars() -> anyhow::Result<()> {
    assert_eq!(encode_path("ka-abc-123"), "ka-abc-123");
    assert_eq!(encode_path("a/b"), "a%2Fb");
    Ok(())
}

#[test]
fn parse_error_message_extracts_field() -> anyhow::Result<()> {
    let body = r#"{"error":"not found"}"#;
    assert_eq!(parse_error_message(body), Some("not found".into()));
    assert_eq!(parse_error_message("not json"), None);
    Ok(())
}

#[test]
fn auth_config_desc_deserializes_minimal() -> anyhow::Result<()> {
    let json = r#"{"account":"anthropic-main"}"#;
    let desc: AuthConfigDesc = serde_json::from_str(json)?;
    assert_eq!(desc.account, "anthropic-main");
    assert!(desc.primary_account.is_empty());
    assert!(desc.failed_over_at.is_empty());
    Ok(())
}

#[test]
fn auth_config_desc_deserializes_full() -> anyhow::Result<()> {
    let json = r#"{
            "account": "anthropic-fallback",
            "primary_account": "anthropic-main",
            "failed_over_at": "2026-03-23T10:00:00Z",
            "failover_reason": "budget exhausted"
        }"#;
    let desc: AuthConfigDesc = serde_json::from_str(json)?;
    assert_eq!(desc.account, "anthropic-fallback");
    assert_eq!(desc.primary_account, "anthropic-main");
    assert_eq!(desc.failed_over_at, "2026-03-23T10:00:00Z");
    assert_eq!(desc.failover_reason, "budget exhausted");
    Ok(())
}

/// Spawn a one-shot TCP listener that captures the first HTTP request line
/// and replies with the given JSON body. Returns the bound URL and a
/// JoinHandle that resolves to the captured request string.
async fn one_shot_capture(
    body: &'static str,
) -> anyhow::Result<(String, tokio::task::JoinHandle<String>)> {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let handle = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.expect("accept");
        let mut buf = vec![0u8; 8192];
        let n = stream.read(&mut buf).await.expect("read");
        let req = String::from_utf8_lossy(&buf[..n]).to_string();
        let resp = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                body.len(),
                body
            );
        let _ = stream.write_all(resp.as_bytes()).await;
        let _ = stream.shutdown().await;
        req
    });
    Ok((format!("http://{}", addr), handle))
}

#[tokio::test]
async fn list_account_beads_sends_filter_query() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (url, handle) = one_shot_capture(r#"{"beads":[]}"#).await?;
    let client = BeadsClient::new(url, None)?;
    let _ = client.list_account_beads().await?;
    let req = handle.await?;
    let first_line = req.lines().next().unwrap_or("");
    assert!(
        first_line.contains("/v1/beads?type=account&status=open"),
        "expected query preserved, got: {first_line}"
    );
    Ok(())
}

#[tokio::test]
async fn list_auth_configs_sends_filter_query() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (url, handle) = one_shot_capture(r#"{"beads":[]}"#).await?;
    let client = BeadsClient::new(url, None)?;
    let _ = client.list_auth_configs().await?;
    let req = handle.await?;
    let first_line = req.lines().next().unwrap_or("");
    assert!(
        first_line.contains("/v1/beads?type=config&status=open"),
        "expected query preserved, got: {first_line}"
    );
    Ok(())
}

/// Regression: get_beads_batch previously sent `/v1/beads?id=...`, which
/// `handleListBeads` silently ignores (no `id` parser), causing the server
/// to scan and return the entire bead table — root cause of ka-0000004415.
/// The fix routes through the dedicated `/v1/batch/beads?ids=` endpoint.
#[tokio::test]
async fn get_beads_batch_uses_batch_endpoint_with_ids_param() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (url, handle) = one_shot_capture(r#"{"results":[]}"#).await?;
    let client = BeadsClient::new(url, None)?;
    let _ = client.get_beads_batch(&["ka-aaa", "ka-bbb"]).await?;
    let req = handle.await?;
    let first_line = req.lines().next().unwrap_or("");
    assert!(
        first_line.contains("/v1/batch/beads?ids=ka-aaa,ka-bbb"),
        "expected batch endpoint with ids= param, got: {first_line}"
    );
    Ok(())
}

#[tokio::test]
async fn get_beads_batch_parses_results_envelope() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let body = r#"{"results":[
            {"id":"ka-aaa","bead":{"id":"ka-aaa","title":"Hello","fields":{"k":"v"}}},
            {"id":"ka-missing","error":"not found"}
        ]}"#;
    let (url, handle) = one_shot_capture(body).await?;
    let client = BeadsClient::new(url, None)?;
    let map = client.get_beads_batch(&["ka-aaa", "ka-missing"]).await?;
    let _ = handle.await?;
    assert_eq!(map.len(), 1);
    let summary = map.get("ka-aaa").expect("ka-aaa present");
    assert_eq!(summary.title, "Hello");
    assert_eq!(summary.fields.get("k").map(String::as_str), Some("v"));
    Ok(())
}

#[test]
fn client_from_env_defaults() -> anyhow::Result<()> {
    // Install crypto provider (tests don't go through main()).
    let _ = rustls::crypto::ring::default_provider().install_default();
    // Clear env to test defaults.
    std::env::remove_var("BEADS_HTTP_ADDR");
    std::env::remove_var("BEADS_TOKEN");
    let client = BeadsClient::from_env()?;
    assert_eq!(client.base_url, "http://localhost:8080");
    assert!(matches!(client.auth, BeadsAuth::None));
    Ok(())
}

// --- machine principal (gd-Wcu0DOkHfN) ----------------------------------------

/// A one-shot token endpoint returning `token` as a client-credentials grant.
async fn one_shot_token_endpoint(token: &str) -> anyhow::Result<String> {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let body = format!(r#"{{"access_token":"{token}","expires_in":1800}}"#);
    tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.expect("accept");
        let mut buf = vec![0u8; 8192];
        let _ = stream.read(&mut buf).await.expect("read");
        let resp = format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        );
        let _ = stream.write_all(resp.as_bytes()).await;
        let _ = stream.shutdown().await;
    });
    Ok(format!("http://{addr}/token"))
}

#[tokio::test]
async fn machine_client_sends_service_account_bearer() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = dir.path().join("clientSecret");
    std::fs::write(&secret, "s3cr3t\n")?;
    let token_url = one_shot_token_endpoint("machine-jwt").await?;
    let source = crate::machine_token::MachineTokenSource::new(
        token_url,
        "gardenio-coopmux".into(),
        &secret,
    )?;
    let (daemon_url, handle) = one_shot_capture(r#"{"id":"ka-1"}"#).await?;
    let client = BeadsClient::new_machine(daemon_url, std::sync::Arc::new(source))?;
    assert!(client.is_machine());

    let mut fields = HashMap::new();
    fields.insert("status".to_owned(), "exhausted".to_owned());
    client.update_bead_fields("ka-1", &fields).await?;

    let req = handle.await?;
    let lower = req.to_lowercase();
    assert!(lower.starts_with("patch /v1/beads/ka-1 "), "got: {req}");
    assert!(lower.contains("authorization: bearer machine-jwt"), "got: {req}");
    assert!(!req.contains("s3cr3t"), "client secret must never reach the daemon: {req}");
    Ok(())
}

#[tokio::test]
async fn machine_client_fails_closed_when_token_unavailable() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    // Secret file absent: the token source cannot fetch, so the write must
    // fail BEFORE any daemon request (the daemon here never answers).
    let source = crate::machine_token::MachineTokenSource::new(
        "http://127.0.0.1:9/token".into(),
        "gardenio-coopmux".into(),
        dir.path().join("missing"),
    )?;
    let (daemon_url, handle) = one_shot_capture(r#"{}"#).await?;
    let client = BeadsClient::new_machine(daemon_url, std::sync::Arc::new(source))?;
    let err = match client.update_bead_fields("ka-1", &HashMap::new()).await {
        Ok(()) => anyhow::bail!("write must fail closed"),
        Err(e) => e.to_string(),
    };
    assert!(err.contains(crate::machine_token::AUTH_UNAVAILABLE), "got: {err}");
    assert!(!handle.is_finished(), "daemon must not have been contacted");
    handle.abort();
    Ok(())
}

#[tokio::test]
async fn static_client_is_not_machine() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    assert!(!BeadsClient::new("http://127.0.0.1:9".into(), Some("tok".into()))?.is_machine());
    assert!(!BeadsClient::new("http://127.0.0.1:9".into(), None)?.is_machine());
    Ok(())
}
