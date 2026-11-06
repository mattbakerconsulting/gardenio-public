// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::HashMap;

use serial_test::serial;

use super::*;

#[tokio::test]
async fn probe_skips_empty_credentials() -> anyhow::Result<()> {
    let creds = HashMap::new();
    let result = probe_credentials("test", &creds).await;
    assert_eq!(result.status, ProbeStatus::Skipped);
    assert_eq!(result.profile, "test");
    assert!(result.credential_type.is_none());
    Ok(())
}

#[tokio::test]
async fn probe_skips_empty_token_values() -> anyhow::Result<()> {
    let creds = HashMap::from([
        ("CLAUDE_CODE_OAUTH_TOKEN".to_owned(), String::new()),
        ("ANTHROPIC_API_KEY".to_owned(), String::new()),
    ]);
    let result = probe_credentials("test", &creds).await;
    assert_eq!(result.status, ProbeStatus::Skipped);
    Ok(())
}

#[tokio::test]
async fn probe_prefers_oauth_over_api_key() -> anyhow::Result<()> {
    // When both are present, OAuth should be tried first.
    // We check the credential_type to verify OAuth was attempted.
    let creds = HashMap::from([
        ("CLAUDE_CODE_OAUTH_TOKEN".to_owned(), "invalid-token".to_owned()),
        ("ANTHROPIC_API_KEY".to_owned(), "invalid-key".to_owned()),
    ]);
    let result = probe_credentials("test", &creds).await;
    assert_eq!(result.credential_type.as_deref(), Some("oauth"));
    Ok(())
}

#[tokio::test]
async fn probe_falls_back_to_api_key() -> anyhow::Result<()> {
    let creds = HashMap::from([("ANTHROPIC_API_KEY".to_owned(), "invalid-key".to_owned())]);
    let result = probe_credentials("test", &creds).await;
    assert_eq!(result.credential_type.as_deref(), Some("api_key"));
    Ok(())
}

#[test]
#[serial]
fn api_base_url_defaults_to_anthropic() {
    std::env::remove_var("ANTHROPIC_BASE_URL");
    assert_eq!(api_base_url(), "https://api.anthropic.com");
}

#[test]
#[serial]
fn api_base_url_respects_override() {
    std::env::set_var("ANTHROPIC_BASE_URL", "http://localhost:4000");
    assert_eq!(api_base_url(), "http://localhost:4000");
    std::env::remove_var("ANTHROPIC_BASE_URL");
}

#[test]
#[serial]
fn oauth_base_url_defaults_to_anthropic() {
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");
    assert_eq!(oauth_base_url(), "https://api.anthropic.com");
}

#[test]
#[serial]
fn oauth_base_url_ignores_anthropic_base_url() {
    // OAuth probing must NOT use ANTHROPIC_BASE_URL (which is often a LiteLLM proxy).
    std::env::set_var("ANTHROPIC_BASE_URL", "http://litellm-proxy:4000");
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");
    assert_eq!(oauth_base_url(), "https://api.anthropic.com");
    std::env::remove_var("ANTHROPIC_BASE_URL");
}

#[test]
#[serial]
fn oauth_base_url_respects_own_override() {
    std::env::set_var("ANTHROPIC_OAUTH_BASE_URL", "http://localhost:9999");
    assert_eq!(oauth_base_url(), "http://localhost:9999");
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");
}

#[test]
fn probe_result_serialization() -> anyhow::Result<()> {
    let result = ProbeResult {
        profile: "prod".to_owned(),
        status: ProbeStatus::Valid,
        credential_type: Some("oauth".to_owned()),
        detail: None,
        usage: None,
    };
    let json = serde_json::to_value(&result)?;
    assert_eq!(json["profile"], "prod");
    assert_eq!(json["status"], "valid");
    assert_eq!(json["credential_type"], "oauth");
    // detail is None, should not be present due to skip_serializing_if.
    assert!(json.get("detail").is_none());
    Ok(())
}

#[test]
fn probe_result_serialization_with_detail() -> anyhow::Result<()> {
    let result = ProbeResult {
        profile: "staging".to_owned(),
        status: ProbeStatus::Invalid,
        credential_type: Some("api_key".to_owned()),
        detail: Some("authentication failed (HTTP 401)".to_owned()),
        usage: None,
    };
    let json = serde_json::to_value(&result)?;
    assert_eq!(json["status"], "invalid");
    assert_eq!(json["detail"], "authentication failed (HTTP 401)");
    Ok(())
}

#[test]
fn probe_status_variants() -> anyhow::Result<()> {
    // Verify all variants serialize correctly.
    let cases = [
        (ProbeStatus::Valid, "valid"),
        (ProbeStatus::Invalid, "invalid"),
        (ProbeStatus::Error, "error"),
        (ProbeStatus::Skipped, "skipped"),
    ];
    for (status, expected) in cases {
        let json = serde_json::to_value(&status)?;
        assert_eq!(json.as_str(), Some(expected), "status={status:?}");
    }
    Ok(())
}

/// Spawn a mock HTTP server that returns a fixed status code for any request.
async fn mock_server(status_code: u16) -> anyhow::Result<std::net::SocketAddr> {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let status_text = match status_code {
        200 => "OK",
        401 => "Unauthorized",
        403 => "Forbidden",
        429 => "Too Many Requests",
        500 => "Internal Server Error",
        _ => "Unknown",
    };

    tokio::spawn(async move {
        loop {
            let Ok((stream, _)) = listener.accept().await else {
                break;
            };
            let status_line = format!("HTTP/1.1 {status_code} {status_text}");
            tokio::spawn(async move {
                use tokio::io::{AsyncReadExt, AsyncWriteExt};
                let (mut r, mut w) = stream.into_split();
                // Read request (drain headers).
                let mut buf = vec![0u8; 4096];
                let _ = r.read(&mut buf).await;
                let resp = format!("{status_line}\r\nContent-Length: 2\r\n\r\n{{}}");
                let _ = w.write_all(resp.as_bytes()).await;
                let _ = w.shutdown().await;
            });
        }
    });

    Ok(addr)
}

/// Test OAuth probing against a local mock server that returns 200.
/// Uses ANTHROPIC_OAUTH_BASE_URL (not ANTHROPIC_BASE_URL).
#[tokio::test]
#[serial]
async fn probe_oauth_against_mock_200() -> anyhow::Result<()> {
    let addr = mock_server(200).await?;
    std::env::set_var("ANTHROPIC_OAUTH_BASE_URL", format!("http://{addr}"));
    let result = probe_oauth_token("mock-profile", "test-token").await;
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");

    assert_eq!(result.status, ProbeStatus::Valid);
    assert_eq!(result.credential_type.as_deref(), Some("oauth"));
    Ok(())
}

/// Test OAuth probing against a local mock server that returns 401.
#[tokio::test]
#[serial]
async fn probe_oauth_against_mock_401() -> anyhow::Result<()> {
    let addr = mock_server(401).await?;
    std::env::set_var("ANTHROPIC_OAUTH_BASE_URL", format!("http://{addr}"));
    let result = probe_oauth_token("mock-profile", "bad-token").await;
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");

    assert_eq!(result.status, ProbeStatus::Invalid);
    assert!(result.detail.as_deref().unwrap_or("").contains("401"));
    Ok(())
}

/// Test API key probing against a local mock server that returns 429.
#[tokio::test]
#[serial]
async fn probe_api_key_against_mock_429() -> anyhow::Result<()> {
    let addr = mock_server(429).await?;
    std::env::set_var("ANTHROPIC_BASE_URL", format!("http://{addr}"));
    let result = probe_api_key("mock-profile", "valid-key").await;
    std::env::remove_var("ANTHROPIC_BASE_URL");

    // 429 should be treated as valid (credential works, just rate-limited).
    assert_eq!(result.status, ProbeStatus::Valid);
    assert!(result.detail.as_deref().unwrap_or("").contains("rate limited"));
    Ok(())
}

/// Test probing against a local mock server that returns 500.
#[tokio::test]
#[serial]
async fn probe_against_mock_500() -> anyhow::Result<()> {
    let addr = mock_server(500).await?;
    std::env::set_var("ANTHROPIC_OAUTH_BASE_URL", format!("http://{addr}"));
    let result = probe_oauth_token("mock-profile", "token").await;
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");

    assert_eq!(result.status, ProbeStatus::Error);
    assert!(result.detail.as_deref().unwrap_or("").contains("500"));
    Ok(())
}

/// Test connection error (nothing listening on an unreachable port).
#[tokio::test]
#[serial]
async fn probe_connection_refused() -> anyhow::Result<()> {
    // Bind and immediately drop to get a port that nothing listens on.
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    drop(listener);

    std::env::set_var("ANTHROPIC_OAUTH_BASE_URL", format!("http://{addr}"));
    let result = probe_oauth_token("bad-host", "token").await;
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");

    assert_eq!(result.status, ProbeStatus::Error);
    assert_eq!(result.credential_type.as_deref(), Some("oauth"));
    Ok(())
}

/// OAuth probing uses a different URL than API key probing when
/// ANTHROPIC_BASE_URL points to a LiteLLM proxy.
#[tokio::test]
#[serial]
async fn probe_oauth_bypasses_litellm_proxy() -> anyhow::Result<()> {
    // Set up two mock servers: one for LiteLLM (API key), one for Anthropic (OAuth).
    let litellm_addr = mock_server(200).await?;
    let anthropic_addr = mock_server(200).await?;

    std::env::set_var("ANTHROPIC_BASE_URL", format!("http://{litellm_addr}"));
    std::env::set_var("ANTHROPIC_OAUTH_BASE_URL", format!("http://{anthropic_addr}"));

    // OAuth probing should hit the Anthropic mock (ANTHROPIC_OAUTH_BASE_URL).
    let oauth_result = probe_oauth_token("claude-max", "oauth-token").await;
    assert_eq!(oauth_result.status, ProbeStatus::Valid);
    assert_eq!(oauth_result.credential_type.as_deref(), Some("oauth"));

    // API key probing should hit the LiteLLM mock (ANTHROPIC_BASE_URL).
    let api_result = probe_api_key("litellm", "api-key").await;
    assert_eq!(api_result.status, ProbeStatus::Valid);
    assert_eq!(api_result.credential_type.as_deref(), Some("api_key"));

    std::env::remove_var("ANTHROPIC_BASE_URL");
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");
    Ok(())
}

/// When ANTHROPIC_BASE_URL is set to a proxy but ANTHROPIC_OAUTH_BASE_URL
/// is not set, OAuth probing should still go to the real Anthropic API,
/// not the proxy.
#[tokio::test]
#[serial]
async fn probe_oauth_defaults_to_real_api_not_proxy() -> anyhow::Result<()> {
    std::env::set_var("ANTHROPIC_BASE_URL", "http://litellm-proxy:4000");
    std::env::remove_var("ANTHROPIC_OAUTH_BASE_URL");

    // Verify the URLs are different.
    assert_eq!(api_base_url(), "http://litellm-proxy:4000");
    assert_eq!(oauth_base_url(), "https://api.anthropic.com");

    std::env::remove_var("ANTHROPIC_BASE_URL");
    Ok(())
}

// -- Codex wham/usage ---------------------------------------------------------

fn codex_doc(access: &str) -> String {
    serde_json::json!({
        "auth_mode": "chatgpt",
        "tokens": {"access_token": access, "refresh_token": "ph", "account_id": "acct-1"},
    })
    .to_string()
}

#[test]
fn wham_usage_maps_windows_onto_bead_fields() {
    let body = serde_json::json!({
        "plan_type": "pro",
        "rate_limit": {
            "allowed": true,
            "limit_reached": false,
            "primary_window": {"used_percent": 42, "limit_window_seconds": 18000, "reset_at": 1_800_000_000u64},
            "secondary_window": {"used_percent": 10.5, "limit_window_seconds": 604800, "reset_after_seconds": 3600},
        }
    });
    let u = parse_wham_usage(&body, 1_000).unwrap();
    assert_eq!(u.utilization_5h, Some(0.42));
    assert_eq!(u.utilization_7d, Some(0.105));
    assert_eq!(u.throttle_reset_5h_at, Some(1_800_000_000));
    assert_eq!(u.throttle_reset_7d_at, Some(4_600));
    assert_eq!(u.representative_claim.as_deref(), Some("five_hour"));
    assert_eq!(u.throttle_status, "allowed");
}

#[test]
fn wham_usage_limit_reached_and_weekly_binding() {
    let body = serde_json::json!({
        "limit_reached": true,
        "primary_window": {"used_percent": 30},
        "secondary_window": {"used_percent": 100},
    });
    let u = parse_wham_usage(&body, 0).unwrap();
    assert_eq!(u.throttle_status, "rejected");
    assert_eq!(u.representative_claim.as_deref(), Some("seven_day"));
    assert_eq!(u.utilization_7d, Some(1.0));
}

#[test]
fn wham_usage_unrecognised_body_fails_open() {
    assert_eq!(parse_wham_usage(&serde_json::json!({"hello": "world"}), 0), None);
    assert_eq!(parse_wham_usage(&serde_json::json!("nope"), 0), None);
    assert_eq!(parse_wham_usage(&serde_json::json!({"rate_limit": {}}), 0), None);
}

#[tokio::test]
async fn codex_probe_skips_non_documents() {
    let creds = HashMap::from([(AUTH_ENV.to_owned(), "not json".to_owned())]);
    assert_eq!(probe_credentials("c", &creds).await.status, ProbeStatus::Skipped);
    let creds = HashMap::from([(AUTH_ENV.to_owned(), r#"{"tokens":{}}"#.to_owned())]);
    assert_eq!(probe_credentials("c", &creds).await.status, ProbeStatus::Skipped);
}

/// Serve `status`/`body` for every request; returns the base URL and the last
/// request's headers.
async fn fake_usage_server(
    status: u16,
    body: &'static str,
) -> (String, std::sync::Arc<std::sync::Mutex<Option<axum::http::HeaderMap>>>) {
    use axum::http::{HeaderMap, StatusCode};
    let seen = std::sync::Arc::new(std::sync::Mutex::new(None));
    let seen_h = std::sync::Arc::clone(&seen);
    let app = axum::Router::new().route(
        "/wham/usage",
        axum::routing::get(move |headers: HeaderMap| {
            let seen = std::sync::Arc::clone(&seen_h);
            async move {
                *seen.lock().unwrap() = Some(headers);
                (StatusCode::from_u16(status).unwrap(), body)
            }
        }),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (format!("http://{addr}/wham/usage"), seen)
}

#[tokio::test]
#[serial]
async fn codex_probe_reads_usage_with_bearer_and_account_id() {
    let (url, seen) = fake_usage_server(
        200,
        r#"{"rate_limit":{"primary_window":{"used_percent":95},"limit_reached":false}}"#,
    )
    .await;
    std::env::set_var("CODEX_USAGE_URL", &url);
    let creds = HashMap::from([(AUTH_ENV.to_owned(), codex_doc("at-123"))]);
    let r = probe_credentials("codex-a", &creds).await;
    std::env::remove_var("CODEX_USAGE_URL");

    assert_eq!(r.status, ProbeStatus::Valid);
    assert_eq!(r.credential_type.as_deref(), Some("codex_chatgpt"));
    assert_eq!(r.usage.unwrap().utilization_5h, Some(0.95));
    let headers = seen.lock().unwrap().clone().unwrap();
    assert_eq!(headers["authorization"], "Bearer at-123");
    assert_eq!(headers["chatgpt-account-id"], "acct-1");
}

#[tokio::test]
#[serial]
async fn codex_probe_classifies_auth_rate_limit_and_outage() {
    for (status, expect) in [
        (401u16, ProbeStatus::Invalid),
        (403, ProbeStatus::Invalid),
        (429, ProbeStatus::Valid),
        (503, ProbeStatus::Error),
    ] {
        let (url, _) = fake_usage_server(status, "{}").await;
        std::env::set_var("CODEX_USAGE_URL", &url);
        let creds = HashMap::from([(AUTH_ENV.to_owned(), codex_doc("at"))]);
        let r = probe_credentials("c", &creds).await;
        std::env::remove_var("CODEX_USAGE_URL");
        assert_eq!(r.status, expect, "HTTP {status}");
        assert!(r.usage.is_none(), "no usage on HTTP {status}");
    }
}

#[tokio::test]
#[serial]
async fn codex_probe_fails_open_on_unreadable_body() {
    let (url, _) = fake_usage_server(200, "<html>moved</html>").await;
    std::env::set_var("CODEX_USAGE_URL", &url);
    let creds = HashMap::from([(AUTH_ENV.to_owned(), codex_doc("at"))]);
    let r = probe_credentials("c", &creds).await;
    std::env::remove_var("CODEX_USAGE_URL");
    assert_eq!(r.status, ProbeStatus::Valid);
    assert!(r.usage.is_none());
}
